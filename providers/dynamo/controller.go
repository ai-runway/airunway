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

package dynamo

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	"github.com/kaito-project/airunway/controller/pkg/storage"
)

const (
	// ProviderName is the name of this provider
	ProviderName = "dynamo"

	// FinalizerName is the finalizer used by this controller
	FinalizerName = "airunway.ai/dynamo-provider"

	// FieldManager is the server-side apply field manager name
	FieldManager = "dynamo-provider"

	// RequeueInterval is the default requeue interval for periodic reconciliation
	RequeueInterval = 30 * time.Second

	// FinalizerTimeout is the timeout for finalizer cleanup
	FinalizerTimeout = 5 * time.Minute
)

// DynamoProviderReconciler reconciles ModelDeployment resources for the Dynamo provider
type DynamoProviderReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Transformer      *Transformer
	StatusTranslator *StatusTranslator
	DownloadJobImage string
}

// NewDynamoProviderReconciler creates a new Dynamo provider reconciler
func NewDynamoProviderReconciler(client client.Client, scheme *runtime.Scheme, downloadJobImage string) *DynamoProviderReconciler {
	if downloadJobImage == "" {
		downloadJobImage = storage.DefaultDownloadJobImage
	}
	return &DynamoProviderReconciler{
		Client:           client,
		Scheme:           scheme,
		Transformer:      NewTransformer(),
		StatusTranslator: NewStatusTranslator(),
		DownloadJobImage: downloadJobImage,
	}
}

// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=airunway.ai,resources=inferenceproviderconfigs,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=inferenceproviderconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=dynamographdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// Reconcile handles the reconciliation loop for ModelDeployments assigned to the Dynamo provider
func (r *DynamoProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	logger.Info("Reconciling ModelDeployment for Dynamo provider", "name", md.Name, "namespace", md.Namespace)

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
	r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderCompatible, metav1.ConditionTrue, "CompatibilityVerified", "Configuration compatible with Dynamo")

	// --- Phase 1: Ensure PVCs ---
	if storage.HasStorageVolumes(&md) {
		allReady, err := storage.EnsurePVCs(ctx, r.Client, &md)
		if err != nil {
			logger.Error(err, "Failed to ensure PVCs", "name", md.Name)
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeStorageReady, metav1.ConditionFalse, "PVCFailed", err.Error())
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = fmt.Sprintf("Failed to ensure PVCs: %s", err.Error())
			return ctrl.Result{}, r.Status().Update(ctx, &md)
		}
		if !allReady {
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeStorageReady, metav1.ConditionFalse, "PVCsPending", "Waiting for PVCs to be bound")
			md.Status.Phase = airunwayv1alpha1.DeploymentPhasePending
			md.Status.Message = "Waiting for PVCs to be bound"
			if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeStorageReady, metav1.ConditionTrue, "PVCsBound", "All managed PVCs are bound")
	}

	// --- Phase 2: Ensure model download ---
	if storage.NeedsDownloadJob(&md) {
		completed, err := storage.EnsureDownloadJob(ctx, r.Client, &md, r.DownloadJobImage)
		if err != nil {
			logger.Error(err, "Failed to ensure download Job", "name", md.Name)
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeModelDownloaded, metav1.ConditionFalse, "DownloadFailed", err.Error())
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = fmt.Sprintf("Model download failed: %s", err.Error())
			return ctrl.Result{}, r.Status().Update(ctx, &md)
		}
		if !completed {
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeModelDownloaded, metav1.ConditionFalse, "DownloadInProgress", "Model download in progress")
			md.Status.Phase = airunwayv1alpha1.DeploymentPhasePending
			md.Status.Message = "Model download in progress"
			if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeModelDownloaded, metav1.ConditionTrue, "DownloadComplete", "Model download completed")
	}

	// --- Phase 3: Create/update DGD ---

	// Transform ModelDeployment to DynamoGraphDeployment
	resources, err := r.Transformer.Transform(ctx, &md)
	if err != nil {
		logger.Error(err, "Failed to transform ModelDeployment", "name", md.Name)
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "TransformFailed", err.Error())
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = fmt.Sprintf("Failed to generate Dynamo resources: %s", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &md)
	}

	// Create or update the DynamoGraphDeployment
	for _, resource := range resources {
		if err := r.createOrUpdateResource(ctx, resource, &md); err != nil {
			logger.Error(err, "Failed to create/update resource", "name", resource.GetName(), "kind", resource.GetKind())
			// requeue to retry with the latest version rather than marking
			// the deployment as Failed to prevent triggering gateway resource cleanup and
			// invalidate the EPP pod's ServiceAccount token.
			if errors.IsConflict(err) {
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "ResourceConflict", err.Error())
				if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			reason := "CreateFailed"
			if isResourceConflict(err) {
				reason = "ResourceConflict"
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "ResourceConflict", err.Error())
			}
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, err.Error())
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = fmt.Sprintf("Failed to create DynamoGraphDeployment: %s", err.Error())
			return ctrl.Result{}, r.Status().Update(ctx, &md)
		}
	}

	r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionTrue, "ResourceCreated", "DynamoGraphDeployment created successfully")

	// Update provider status
	md.Status.Provider.ResourceName = md.Name
	md.Status.Provider.ResourceKind = DynamoGraphDeploymentKind

	// Sync status from upstream resource
	if len(resources) > 0 {
		if err := r.syncStatus(ctx, &md, resources[0]); err != nil {
			logger.Error(err, "Failed to sync status", "name", md.Name)
			// Don't fail the reconciliation, just log the error
		}
	}

	// Set phase to Deploying if not already Running or Failed
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning &&
		md.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
		md.Status.Message = "DynamoGraphDeployment created, waiting for pods to be ready"
	}

	if err := r.Status().Update(ctx, &md); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Reconciliation complete", "name", md.Name, "phase", md.Status.Phase)

	// Requeue to periodically sync status
	return ctrl.Result{RequeueAfter: RequeueInterval}, nil
}

// validateCompatibility checks if the ModelDeployment configuration is compatible with Dynamo
func (r *DynamoProviderReconciler) validateCompatibility(md *airunwayv1alpha1.ModelDeployment) error {
	// Dynamo doesn't support llamacpp
	if md.ResolvedEngineType() == airunwayv1alpha1.EngineTypeLlamaCpp {
		return fmt.Errorf("Dynamo does not support llamacpp engine")
	}

	// Dynamo requires GPU
	hasGPU := false
	if md.Spec.Resources != nil && md.Spec.Resources.GPU != nil && md.Spec.Resources.GPU.Count > 0 {
		hasGPU = true
	}
	if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		// Disaggregated mode always has GPU in prefill/decode
		if md.Spec.Scaling != nil {
			if md.Spec.Scaling.Prefill != nil && md.Spec.Scaling.Prefill.GPU != nil && md.Spec.Scaling.Prefill.GPU.Count > 0 {
				hasGPU = true
			}
		}
	}

	if !hasGPU {
		return fmt.Errorf("Dynamo requires GPU (set resources.gpu.count > 0)")
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

// verifyDynamoOwnership checks that the existing resource is managed by this specific ModelDeployment.
func verifyDynamoOwnership(existing *unstructured.Unstructured, mdUID types.UID) error {
	for _, ref := range existing.GetOwnerReferences() {
		if ref.UID == mdUID {
			return nil
		}
	}
	return &resourceConflictError{namespace: existing.GetNamespace(), name: existing.GetName()}
}

// createOrUpdateResource creates or updates an unstructured resource
func (r *DynamoProviderReconciler) createOrUpdateResource(ctx context.Context, resource *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) error {
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
	if err := verifyDynamoOwnership(existing, md.UID); err != nil {
		return err
	}

	// Update existing resource if spec has changed.
	// The Dynamo CRD API server adds zero-value defaults (e.g. name: "",
	// resources: {}) that the provider never sets. Comparing raw specs would
	// trigger an infinite update loop. Strip server-added zero-values
	// from the existing spec before comparing.
	existingSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	newSpec, _, _ := unstructured.NestedMap(resource.Object, "spec")

	if !equality.Semantic.DeepEqual(stripEmptyDefaults(existingSpec), stripEmptyDefaults(newSpec)) {
		logger.Info("Updating resource", "kind", resource.GetKind(), "name", resource.GetName())
		resource.SetResourceVersion(existing.GetResourceVersion())
		return r.Update(ctx, resource)
	}

	return nil
}

// stripEmptyDefaults recursively removes zero-value fields (empty strings,
// empty maps) that the Kubernetes API server adds as defaults. This prevents
// diffs when comparing the provider's desired spec against the
// server-persisted spec.
func stripEmptyDefaults(obj map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(obj))
	for k, v := range obj {
		switch val := v.(type) {
		case string:
			if val != "" {
				result[k] = val
			}
		case map[string]interface{}:
			stripped := stripEmptyDefaults(val)
			if len(stripped) > 0 {
				result[k] = stripped
			}
		case []interface{}:
			result[k] = stripEmptyDefaultsSlice(val)
		default:
			result[k] = v
		}
	}
	return result
}

func stripEmptyDefaultsSlice(arr []interface{}) []interface{} {
	result := make([]interface{}, len(arr))
	for i, v := range arr {
		switch val := v.(type) {
		case map[string]interface{}:
			result[i] = stripEmptyDefaults(val)
		case []interface{}:
			result[i] = stripEmptyDefaultsSlice(val)
		default:
			result[i] = v
		}
	}
	return result
}

// syncStatus fetches the upstream resource and syncs its status to the ModelDeployment
func (r *DynamoProviderReconciler) syncStatus(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, desired *unstructured.Unstructured) error {
	// Fetch the current state of the upstream resource
	upstream := &unstructured.Unstructured{}
	upstream.SetGroupVersionKind(desired.GroupVersionKind())

	err := r.Get(ctx, types.NamespacedName{
		Name:      desired.GetName(),
		Namespace: desired.GetNamespace(),
	}, upstream)
	if err != nil {
		if errors.IsNotFound(err) {
			// Resource not created yet
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
func (r *DynamoProviderReconciler) handleDeletion(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) (ctrl.Result, error) {
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

	// Delete the DGD first so its Pods terminate before we remove PVCs/Jobs
	dgd := &unstructured.Unstructured{}
	dgd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   DynamoAPIGroup,
		Version: DynamoAPIVersion,
		Kind:    DynamoGraphDeploymentKind,
	})

	dgdName := md.Name
	err := r.Get(ctx, types.NamespacedName{
		Name:      dgdName,
		Namespace: md.Namespace,
	}, dgd)

	if err == nil {
		// Verify ownership before deleting
		if err := verifyDynamoOwnership(dgd, md.UID); err != nil {
			logger.Info("Resource exists but is not managed by this ModelDeployment, skipping deletion", "name", dgdName)
			controllerutil.RemoveFinalizer(md, FinalizerName)
			return ctrl.Result{}, r.Update(ctx, md)
		}

		// Resource exists and is owned by us, delete it
		logger.Info("Deleting DynamoGraphDeployment", "name", dgdName)
		if err := r.Delete(ctx, dgd); err != nil {
			if upstreamResourceUnavailable(err) {
				logger.Info("DynamoGraphDeployment unavailable during deletion, continuing managed cleanup", "name", dgdName)
			} else {
				logger.Error(err, "Failed to delete DynamoGraphDeployment")

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
			// Requeue to wait for DGD and its Pods to be fully terminated
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	if !upstreamResourceUnavailable(err) {
		// Unexpected error fetching DGD — check timeout before requeueing
		deletionTime := md.DeletionTimestamp.Time
		if time.Since(deletionTime) > FinalizerTimeout {
			logger.Info("Finalizer timeout reached, removing finalizer without cleanup")
			controllerutil.RemoveFinalizer(md, FinalizerName)
			return ctrl.Result{}, r.Update(ctx, md)
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// The upstream resource is already gone or its CRD is no longer installed,
	// so continue with managed Jobs/PVC cleanup and remove the finalizer.
	var cleanupErrs []error
	if err := storage.DeleteManagedJobs(ctx, r.Client, md); err != nil {
		logger.Error(err, "Failed to delete managed Jobs")
		cleanupErrs = append(cleanupErrs, err)
	}
	if err := storage.DeleteManagedPVCs(ctx, r.Client, md); err != nil {
		logger.Error(err, "Failed to delete managed PVCs")
		cleanupErrs = append(cleanupErrs, err)
	}
	if err := stderrors.Join(cleanupErrs...); err != nil {
		// Check if we should force-remove the finalizer
		deletionTime := md.DeletionTimestamp.Time
		if time.Since(deletionTime) > FinalizerTimeout {
			logger.Info("Finalizer timeout reached, removing finalizer without cleanup")
			controllerutil.RemoveFinalizer(md, FinalizerName)
			return ctrl.Result{}, r.Update(ctx, md)
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// All resources cleaned up, remove finalizer
	logger.Info("All resources deleted, removing finalizer", "name", md.Name)
	controllerutil.RemoveFinalizer(md, FinalizerName)
	return ctrl.Result{}, r.Update(ctx, md)
}

func upstreamResourceUnavailable(err error) bool {
	return errors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// setCondition updates a condition on the ModelDeployment
func (r *DynamoProviderReconciler) setCondition(md *airunwayv1alpha1.ModelDeployment, conditionType string, status metav1.ConditionStatus, reason, message string) {
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

// dynamoProviderPredicate returns true if the event should be processed by the dynamo controller.
// For ModelDeployment objects, it checks if the provider is "dynamo" or if the finalizer is present.
// For non-ModelDeployment objects (PVCs, Jobs, DGDs), it always returns true to allow
// Owns()/Watches() events through — the owner-reference handler will resolve them to the
// correct ModelDeployment.
func dynamoProviderPredicate(obj client.Object) bool {
	md, ok := obj.(*airunwayv1alpha1.ModelDeployment)
	if !ok {
		return true // Allow secondary watches (PVCs, Jobs, DGDs, provider configs) through.
	}
	// Process if provider is dynamo OR if being deleted (to handle finalizer)
	if md.Status.Provider != nil && md.Status.Provider.Name == ProviderName {
		return true
	}
	// Also process if spec explicitly requests dynamo
	if md.Spec.Provider != nil && md.Spec.Provider.Name == ProviderName {
		return true
	}
	// Process if we have our finalizer (for deletion handling)
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

func (r *DynamoProviderReconciler) mapProviderConfigToModelDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
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
func (r *DynamoProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.ModelDeployment{}).
		// Watch PVCs and Jobs owned by ModelDeployments (auto-reconcile on status changes)
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Watches(
			&airunwayv1alpha1.InferenceProviderConfig{},
			handler.EnqueueRequestsFromMapFunc(r.mapProviderConfigToModelDeployments),
			ctrlbuilder.WithPredicates(providerConfigChangePredicate()),
		).
		// Only watch ModelDeployments where provider.name == "dynamo"
		WithEventFilter(predicate.NewPredicateFuncs(dynamoProviderPredicate))

	// Only watch DynamoGraphDeployment resources if the CRD is installed.
	// Without this check, the manager crashes at startup when
	// the backend CRDs are not present (see #178).
	mapper := mgr.GetRESTMapper()
	if _, err := mapper.RESTMapping(schema.GroupKind{Group: DynamoAPIGroup, Kind: DynamoGraphDeploymentKind}, DynamoAPIVersion); err == nil {
		logger := mgr.GetLogger()
		logger.Info("DynamoGraphDeployment CRD detected, enabling event-driven watch")
		builder = builder.Watches(
			&unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", DynamoAPIGroup, DynamoAPIVersion),
				"kind":       DynamoGraphDeploymentKind,
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
				labels := obj.GetLabels()
				if labels[airunwayv1alpha1.LabelManagedBy] == "airunway" {
					if deployment := labels[airunwayv1alpha1.LabelModelDeployment]; deployment != "" {
						return []reconcile.Request{
							{
								NamespacedName: types.NamespacedName{
									Name:      deployment,
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
		Named("dynamo-provider").
		Complete(r)
}
