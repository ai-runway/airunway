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

package kaito

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

const (
	// ProviderName is the name of this provider
	ProviderName = "kaito"

	// FinalizerName is the finalizer used by this controller
	FinalizerName = "airunway.ai/kaito-provider"

	// FieldManager is the server-side apply field manager name
	FieldManager = "kaito-provider"

	// RequeueInterval is the default requeue interval for periodic reconciliation
	RequeueInterval = 30 * time.Second

	// FinalizerTimeout is the timeout for finalizer cleanup
	FinalizerTimeout = 5 * time.Minute
)

// KaitoProviderReconciler reconciles ModelDeployment resources for the KAITO provider
type KaitoProviderReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Transformer      *Transformer
	StatusTranslator *StatusTranslator
}

// NewKaitoProviderReconciler creates a new KAITO provider reconciler
func NewKaitoProviderReconciler(client client.Client, scheme *runtime.Scheme) *KaitoProviderReconciler {
	return &KaitoProviderReconciler{
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
// +kubebuilder:rbac:groups=kaito.sh,resources=workspaces,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles the reconciliation loop for ModelDeployments assigned to the KAITO provider
func (r *KaitoProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	logger.Info("Reconciling ModelDeployment for KAITO provider", "name", md.Name, "namespace", md.Namespace)

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
	r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderCompatible, metav1.ConditionTrue, "CompatibilityVerified", "Configuration compatible with KAITO")

	// Transform ModelDeployment to KAITO Workspace
	resources, err := r.Transformer.Transform(ctx, &md)
	if err != nil {
		logger.Error(err, "Failed to transform ModelDeployment", "name", md.Name)
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "TransformFailed", err.Error())
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = fmt.Sprintf("Failed to generate KAITO resources: %s", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &md)
	}

	// Create or update the Workspace
	for _, resource := range resources {
		if err := r.createOrUpdateResource(ctx, resource, &md); err != nil {
			// API conflict errors (stale resourceVersion) are transient — requeue to retry
			if errors.IsConflict(err) {
				logger.Info("Resource version conflict, requeuing", "name", resource.GetName(), "kind", resource.GetKind())
				return ctrl.Result{Requeue: true}, nil
			}

			logger.Error(err, "Failed to create/update resource", "name", resource.GetName(), "kind", resource.GetKind())
			reason := "CreateFailed"
			if isResourceConflict(err) {
				reason = "ResourceConflict"
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "ResourceConflict", err.Error())
			}
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, err.Error())
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = fmt.Sprintf("Failed to create Workspace: %s", err.Error())
			return ctrl.Result{}, r.Status().Update(ctx, &md)
		}
	}

	r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionTrue, "ResourceCreated", "Workspace created successfully")

	// Update provider status
	md.Status.Provider.ResourceName = md.Name
	md.Status.Provider.ResourceKind = WorkspaceKind

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
		md.Status.Message = "Workspace created, waiting for pods to be ready"
	}

	if err := r.Status().Update(ctx, &md); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Reconciliation complete", "name", md.Name, "phase", md.Status.Phase)

	// Requeue to periodically sync status
	return ctrl.Result{RequeueAfter: RequeueInterval}, nil
}

// validateCompatibility checks if the ModelDeployment configuration is compatible with KAITO
func (r *KaitoProviderReconciler) validateCompatibility(md *airunwayv1alpha1.ModelDeployment) error {
	// KAITO doesn't support sglang
	if md.ResolvedEngineType() == airunwayv1alpha1.EngineTypeSGLang {
		return fmt.Errorf("KAITO does not support sglang engine")
	}

	// KAITO doesn't support trtllm
	if md.ResolvedEngineType() == airunwayv1alpha1.EngineTypeTRTLLM {
		return fmt.Errorf("KAITO does not support trtllm engine")
	}

	// KAITO doesn't support disaggregated serving
	if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		return fmt.Errorf("KAITO does not support disaggregated serving mode")
	}

	// llamacpp requires spec.image to be set
	if md.ResolvedEngineType() == airunwayv1alpha1.EngineTypeLlamaCpp && md.Spec.Image == "" {
		return fmt.Errorf("llamacpp engine requires spec.image to be set")
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
func (r *KaitoProviderReconciler) createOrUpdateResource(ctx context.Context, resource *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) error {
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

	// Update existing resource if resource or inference has changed
	// KAITO Workspace has resource/inference at root level, not under spec
	existingResource, _, _ := unstructured.NestedMap(existing.Object, "resource")
	newResource, _, _ := unstructured.NestedMap(resource.Object, "resource")
	existingInference, _, _ := unstructured.NestedMap(existing.Object, "inference")
	newInference, _, _ := unstructured.NestedMap(resource.Object, "inference")

	if !equality.Semantic.DeepEqual(existingResource, newResource) || !equality.Semantic.DeepEqual(existingInference, newInference) {
		logger.Info("Updating resource", "kind", resource.GetKind(), "name", resource.GetName())
		resource.SetResourceVersion(existing.GetResourceVersion())
		return r.Update(ctx, resource)
	}

	return nil
}

// syncStatus fetches the upstream resource and syncs its status to the ModelDeployment
func (r *KaitoProviderReconciler) syncStatus(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, desired *unstructured.Unstructured) error {
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
func (r *KaitoProviderReconciler) handleDeletion(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) (ctrl.Result, error) {
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
	ws := &unstructured.Unstructured{}
	ws.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   KaitoAPIGroup,
		Version: KaitoAPIVersion,
		Kind:    WorkspaceKind,
	})

	err := r.Get(ctx, types.NamespacedName{
		Name:      md.Name,
		Namespace: md.Namespace,
	}, ws)

	if err == nil {
		// Verify ownership before deleting
		if err := verifyOwnerReference(ws, md.UID); err != nil {
			logger.Info("Resource exists but is not managed by this ModelDeployment, skipping deletion", "name", md.Name)
			controllerutil.RemoveFinalizer(md, FinalizerName)
			return ctrl.Result{}, r.Update(ctx, md)
		}

		// Resource exists and is owned by us, delete it
		logger.Info("Deleting Workspace", "name", md.Name)
		if deleteErr := r.Delete(ctx, ws); deleteErr != nil {
			if upstreamResourceUnavailable(deleteErr) {
				logger.Info("Workspace unavailable during deletion, removing finalizer", "name", md.Name)
				controllerutil.RemoveFinalizer(md, FinalizerName)
				return ctrl.Result{}, r.Update(ctx, md)
			}

			logger.Error(deleteErr, "Failed to delete Workspace")

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

		// Requeue to wait for deletion
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if !upstreamResourceUnavailable(err) {
		// Unexpected error fetching the upstream resource (e.g. transient API
		// server failure). Honor the finalizer timeout so the ModelDeployment
		// can still be removed if the error persists, instead of requeueing
		// forever.
		deletionTime := md.DeletionTimestamp.Time
		if time.Since(deletionTime) > FinalizerTimeout {
			logger.Info("Finalizer timeout reached, removing finalizer without cleanup")
			controllerutil.RemoveFinalizer(md, FinalizerName)
			return ctrl.Result{}, r.Update(ctx, md)
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// The upstream resource is already gone or its CRD is no longer installed,
	// so cleanup can finish by removing the finalizer.
	logger.Info("Upstream resource unavailable or deleted, removing finalizer", "name", md.Name)
	controllerutil.RemoveFinalizer(md, FinalizerName)
	return ctrl.Result{}, r.Update(ctx, md)
}

// upstreamResourceUnavailable returns true when the error indicates the
// upstream resource is missing or its CRD is not installed in the cluster.
// This mirrors the helper used by the dynamo and kuberay providers so that
// finalizer cleanup completes even when KAITO has been uninstalled.
func upstreamResourceUnavailable(err error) bool {
	return errors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// setCondition updates a condition on the ModelDeployment
func (r *KaitoProviderReconciler) setCondition(md *airunwayv1alpha1.ModelDeployment, conditionType string, status metav1.ConditionStatus, reason, message string) {
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

// SetupWithManager sets up the controller with the Manager.
func (r *KaitoProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.ModelDeployment{}).
		// Only watch ModelDeployments where provider.name == "kaito"
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			md, ok := obj.(*airunwayv1alpha1.ModelDeployment)
			if !ok {
				return false
			}
			// Process if provider is kaito OR if being deleted (to handle finalizer)
			if md.Status.Provider != nil && md.Status.Provider.Name == ProviderName {
				return true
			}
			// Also process if spec explicitly requests kaito
			if md.Spec.Provider != nil && md.Spec.Provider.Name == ProviderName {
				return true
			}
			// Process if we have our finalizer (for deletion handling)
			return controllerutil.ContainsFinalizer(md, FinalizerName)
		})).
		Named("kaito-provider").
		Complete(r)
}
