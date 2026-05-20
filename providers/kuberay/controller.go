/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kuberay

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

const (
	// ProviderName is the name of this provider
	ProviderName = "kuberay"

	// FinalizerName is the finalizer used by this controller
	FinalizerName = "airunway.ai/kuberay-provider"

	// FieldManager is the server-side apply field manager name
	FieldManager = "kuberay-provider"

	// RequeueInterval is the default requeue interval for periodic reconciliation
	RequeueInterval = 30 * time.Second

	// FinalizerTimeout is the timeout for finalizer cleanup
	FinalizerTimeout = 5 * time.Minute
)

// KubeRayProviderReconciler reconciles ModelDeployment resources for the KubeRay provider
type KubeRayProviderReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Transformer      *Transformer
	StatusTranslator *StatusTranslator
}

// NewKubeRayProviderReconciler creates a new KubeRay provider reconciler
func NewKubeRayProviderReconciler(client client.Client, scheme *runtime.Scheme) *KubeRayProviderReconciler {
	return &KubeRayProviderReconciler{
		Client:           client,
		Scheme:           scheme,
		Transformer:      NewTransformer(),
		StatusTranslator: NewStatusTranslator(),
	}
}

// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=airunway.ai,resources=inferenceproviderconfigs,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=inferenceproviderconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ray.io,resources=rayservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ray.io,resources=rayservices/status,verbs=get

// Reconcile handles the reconciliation loop for ModelDeployments assigned to the KubeRay provider
func (r *KubeRayProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ModelDeployment
	var md airunwayv1alpha1.ModelDeployment
	if err := r.Get(ctx, req.NamespacedName, &md); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only process if this provider is selected
	if md.Status.Provider == nil || md.Status.Provider.Name != ProviderName {
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciling ModelDeployment for KubeRay provider", "name", md.Name, "namespace", md.Namespace)

	// Check for pause annotation
	if md.Annotations != nil && md.Annotations["airunway.ai/reconcile-paused"] == "true" {
		logger.Info("Reconciliation paused", "name", md.Name)
		return ctrl.Result{}, nil
	}

	// Handle deletion
	if !md.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &md)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&md, FinalizerName) {
		controllerutil.AddFinalizer(&md, FinalizerName)
		if err := r.Update(ctx, &md); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Validate provider compatibility
	if err := r.validateCompatibility(&md); err != nil {
		logger.Error(err, "Provider compatibility check failed", "name", md.Name)
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderCompatible, metav1.ConditionFalse, "IncompatibleConfiguration", err.Error())
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = err.Error()
		return ctrl.Result{}, r.Status().Update(ctx, &md)
	}
	r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderCompatible, metav1.ConditionTrue, "CompatibilityVerified", "Configuration compatible with KubeRay")

	// Transform ModelDeployment to RayService
	resources, err := r.Transformer.Transform(ctx, &md)
	if err != nil {
		logger.Error(err, "Failed to transform ModelDeployment", "name", md.Name)
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "TransformFailed", err.Error())
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = fmt.Sprintf("Failed to generate KubeRay resources: %s", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &md)
	}

	// Create or update the RayService
	for _, resource := range resources {
		if err := r.createOrUpdateResource(ctx, resource, &md); err != nil {
			logger.Error(err, "Failed to create/update resource", "name", resource.GetName(), "kind", resource.GetKind())
			reason := "CreateFailed"
			if isResourceConflict(err) {
				reason = "ResourceConflict"
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "ResourceConflict", err.Error())
			}
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, err.Error())
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = fmt.Sprintf("Failed to create RayService: %s", err.Error())
			return ctrl.Result{}, r.Status().Update(ctx, &md)
		}
	}

	r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionTrue, "ResourceCreated", "RayService created successfully")

	// Update provider status
	md.Status.Provider.ResourceName = md.Name
	md.Status.Provider.ResourceKind = RayServiceKind

	// Sync status from upstream resource
	if len(resources) > 0 {
		if err := r.syncStatus(ctx, &md, resources[0]); err != nil {
			logger.Error(err, "Failed to sync status", "name", md.Name)
		}
	}

	// Set phase to Deploying if not already Running or Failed
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning &&
		md.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
		md.Status.Message = "RayService created, waiting for pods to be ready"
	}

	if err := r.Status().Update(ctx, &md); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Reconciliation complete", "name", md.Name, "phase", md.Status.Phase)

	// Requeue to periodically sync status
	return ctrl.Result{RequeueAfter: RequeueInterval}, nil
}

// validateCompatibility checks if the ModelDeployment configuration is compatible with KubeRay
func (r *KubeRayProviderReconciler) validateCompatibility(md *airunwayv1alpha1.ModelDeployment) error {
	// KubeRay only supports vllm
	if md.ResolvedEngineType() != airunwayv1alpha1.EngineTypeVLLM {
		return fmt.Errorf("KubeRay only supports vllm engine, got %s", md.ResolvedEngineType())
	}

	// KubeRay requires GPU
	hasGPU := false
	if md.Spec.Resources != nil && md.Spec.Resources.GPU != nil && md.Spec.Resources.GPU.Count > 0 {
		hasGPU = true
	}
	if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		if md.Spec.Scaling != nil {
			if md.Spec.Scaling.Prefill != nil && md.Spec.Scaling.Prefill.GPU != nil && md.Spec.Scaling.Prefill.GPU.Count > 0 {
				hasGPU = true
			}
		}
	}

	if !hasGPU {
		return fmt.Errorf("KubeRay requires GPU (set resources.gpu.count > 0)")
	}

	return nil
}

// resourceConflictError is returned when a resource exists but is not managed by this ModelDeployment
type resourceConflictError struct {
	namespace string
	name      string
}

func (e *resourceConflictError) Error() string {
	return fmt.Sprintf("resource %s/%s exists but is not managed by this ModelDeployment", e.namespace, e.name)
}

// isResourceConflict checks whether the error is a resource ownership conflict
func isResourceConflict(err error) bool {
	var conflict *resourceConflictError
	return stderrors.As(err, &conflict)
}

// verifyOwnerReference checks that the existing resource has an OwnerReference pointing to the given ModelDeployment UID.
func verifyOwnerReference(existing *unstructured.Unstructured, mdUID types.UID) error {
	for _, ref := range existing.GetOwnerReferences() {
		if ref.UID == mdUID {
			return nil
		}
	}
	return &resourceConflictError{namespace: existing.GetNamespace(), name: existing.GetName()}
}

// createOrUpdateResource creates or updates an unstructured resource
func (r *KubeRayProviderReconciler) createOrUpdateResource(ctx context.Context, resource *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	// Check if resource exists
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(resource.GroupVersionKind())

	err := r.Get(ctx, types.NamespacedName{
		Name:      resource.GetName(),
		Namespace: resource.GetNamespace(),
	}, existing)

	if errors.IsNotFound(err) {
		// Create new resource
		logger.Info("Creating resource", "kind", resource.GetKind(), "name", resource.GetName())
		return r.Create(ctx, resource)
	}
	if err != nil {
		return fmt.Errorf("failed to get existing resource: %w", err)
	}

	// Verify ownership before updating
	if err := verifyOwnerReference(existing, md.UID); err != nil {
		return err
	}

	// Update existing resource if spec has changed
	existingSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	newSpec, _, _ := unstructured.NestedMap(resource.Object, "spec")

	if !equality.Semantic.DeepEqual(existingSpec, newSpec) {
		logger.Info("Updating resource", "kind", resource.GetKind(), "name", resource.GetName())
		resource.SetResourceVersion(existing.GetResourceVersion())
		return r.Update(ctx, resource)
	}

	return nil
}

// syncStatus fetches the upstream resource and syncs its status to the ModelDeployment
func (r *KubeRayProviderReconciler) syncStatus(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, desired *unstructured.Unstructured) error {
	// Fetch the current state of the upstream resource
	upstream := &unstructured.Unstructured{}
	upstream.SetGroupVersionKind(desired.GroupVersionKind())

	err := r.Get(ctx, types.NamespacedName{
		Name:      desired.GetName(),
		Namespace: desired.GetNamespace(),
	}, upstream)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get upstream resource: %w", err)
	}

	// Translate status
	statusResult, err := r.StatusTranslator.TranslateStatus(upstream)
	if err != nil {
		return fmt.Errorf("failed to translate status: %w", err)
	}

	// Update ModelDeployment status
	md.Status.Phase = statusResult.Phase
	if statusResult.Message != "" {
		md.Status.Message = statusResult.Message
	}
	md.Status.Replicas = statusResult.Replicas
	md.Status.Endpoint = statusResult.Endpoint

	// Update Ready condition based on phase
	if statusResult.Phase == airunwayv1alpha1.DeploymentPhaseRunning {
		r.setCondition(md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionTrue, "DeploymentReady", "All replicas are ready")
	} else if statusResult.Phase == airunwayv1alpha1.DeploymentPhaseFailed {
		r.setCondition(md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "DeploymentFailed", statusResult.Message)
	} else {
		r.setCondition(md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "DeploymentInProgress", "Deployment is in progress")
	}

	return nil
}

// handleDeletion handles the deletion of a ModelDeployment
func (r *KubeRayProviderReconciler) handleDeletion(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(md, FinalizerName) {
		return ctrl.Result{}, nil
	}

	logger.Info("Handling deletion", "name", md.Name, "namespace", md.Namespace)

	// Update phase to Terminating
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseTerminating
	if err := r.Status().Update(ctx, md); err != nil {
		logger.Error(err, "Failed to update status to Terminating")
	}

	// Delete the upstream resource
	rs := &unstructured.Unstructured{}
	rs.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   RayAPIGroup,
		Version: RayAPIVersion,
		Kind:    RayServiceKind,
	})

	err := r.Get(ctx, types.NamespacedName{
		Name:      md.Name,
		Namespace: md.Namespace,
	}, rs)

	if err == nil {
		// Verify ownership before deleting
		if err := verifyOwnerReference(rs, md.UID); err != nil {
			logger.Info("Resource exists but is not managed by this ModelDeployment, skipping deletion", "name", md.Name)
			controllerutil.RemoveFinalizer(md, FinalizerName)
			return ctrl.Result{}, r.Update(ctx, md)
		}

		// Resource exists and is owned by us, delete it
		logger.Info("Deleting RayService", "name", md.Name)
		if err := r.Delete(ctx, rs); err != nil {
			if upstreamResourceUnavailable(err) {
				logger.Info("RayService unavailable during deletion, removing finalizer", "name", md.Name)
			} else {
				logger.Error(err, "Failed to delete RayService")

				// Check if we should force-remove the finalizer
				deletionTime := md.DeletionTimestamp.Time
				if time.Since(deletionTime) > FinalizerTimeout {
					logger.Info("Finalizer timeout reached, removing finalizer without cleanup")
					controllerutil.RemoveFinalizer(md, FinalizerName)
					return ctrl.Result{}, r.Update(ctx, md)
				}

				// Requeue to retry deletion
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
		} else {
			// Requeue to wait for deletion
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	if !upstreamResourceUnavailable(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get upstream resource: %w", err)
	}

	// The upstream resource is already gone or its CRD is no longer installed,
	// so cleanup can finish by removing the finalizer.
	logger.Info("Upstream resource unavailable or deleted, removing finalizer", "name", md.Name)
	controllerutil.RemoveFinalizer(md, FinalizerName)
	return ctrl.Result{}, r.Update(ctx, md)
}

func upstreamResourceUnavailable(err error) bool {
	return errors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// setCondition updates a condition on the ModelDeployment
func (r *KubeRayProviderReconciler) setCondition(md *airunwayv1alpha1.ModelDeployment, conditionType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: md.Generation,
	}
	meta.SetStatusCondition(&md.Status.Conditions, condition)
}

func kubeRayProviderPredicate(obj client.Object) bool {
	md, ok := obj.(*airunwayv1alpha1.ModelDeployment)
	if !ok {
		return true // Allow secondary watches (RayServices, provider configs) through.
	}
	if md.Status.Provider != nil && md.Status.Provider.Name == ProviderName {
		return true
	}
	if md.Spec.Provider != nil && md.Spec.Provider.Name == ProviderName {
		return true
	}
	return controllerutil.ContainsFinalizer(md, FinalizerName)
}

func providerConfigChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldConfig, okOld := e.ObjectOld.(*airunwayv1alpha1.InferenceProviderConfig)
			newConfig, okNew := e.ObjectNew.(*airunwayv1alpha1.InferenceProviderConfig)
			if !okOld || !okNew {
				return false
			}
			return oldConfig.Status.Ready != newConfig.Status.Ready ||
				!equality.Semantic.DeepEqual(oldConfig.Spec, newConfig.Spec)
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}
}

func (r *KubeRayProviderReconciler) mapProviderConfigToModelDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	providerConfig, ok := obj.(*airunwayv1alpha1.InferenceProviderConfig)
	if !ok || providerConfig.Name != ProviderName {
		return nil
	}

	var mdList airunwayv1alpha1.ModelDeploymentList
	if err := r.List(ctx, &mdList); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list ModelDeployments for provider config change", "provider", providerConfig.Name)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(mdList.Items))
	seen := make(map[types.NamespacedName]struct{}, len(mdList.Items))
	for i := range mdList.Items {
		md := &mdList.Items[i]
		if (md.Status.Provider == nil || md.Status.Provider.Name != ProviderName) &&
			(md.Spec.Provider == nil || md.Spec.Provider.Name != ProviderName) {
			continue
		}

		key := types.NamespacedName{Name: md.Name, Namespace: md.Namespace}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}

	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *KubeRayProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.ModelDeployment{}).
		Watches(
			&airunwayv1alpha1.InferenceProviderConfig{},
			handler.EnqueueRequestsFromMapFunc(r.mapProviderConfigToModelDeployments),
			ctrlbuilder.WithPredicates(providerConfigChangePredicate()),
		).
		// Only watch ModelDeployments where provider.name == "kuberay"
		WithEventFilter(predicate.NewPredicateFuncs(kubeRayProviderPredicate))

	// Only watch RayService resources if the CRD is installed.
	// Without this check, the manager crashes at startup when
	// the backend CRDs are not present (see #178).
	mapper := mgr.GetRESTMapper()
	if _, err := mapper.RESTMapping(schema.GroupKind{Group: RayAPIGroup, Kind: RayServiceKind}, RayAPIVersion); err == nil {
		logger := mgr.GetLogger()
		logger.Info("RayService CRD detected, enabling event-driven watch")
		builder = builder.Watches(
			&unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", RayAPIGroup, RayAPIVersion),
				"kind":       RayServiceKind,
			}},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				for _, ref := range obj.GetOwnerReferences() {
					if ref.APIVersion == airunwayv1alpha1.GroupVersion.String() &&
						ref.Kind == "ModelDeployment" {
						return []reconcile.Request{
							{
								NamespacedName: types.NamespacedName{
									Name:      ref.Name,
									Namespace: obj.GetNamespace(),
								},
							},
						}
					}
				}
				return nil
			}),
		)
	}

	return builder.
		Named("kuberay-provider").
		Complete(r)
}
