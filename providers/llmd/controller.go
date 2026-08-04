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

package llmd

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

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

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const (
	// ProviderName is the name of this provider
	ProviderName = "llmd"

	// FinalizerName is the finalizer used by this controller
	FinalizerName = "airunway.ai/llmd-provider"

	// FieldManager is the server-side apply field manager name
	FieldManager = "llmd-provider"

	// RequeueInterval is the default requeue interval for periodic reconciliation
	RequeueInterval = 30 * time.Second

	// FinalizerTimeout is the timeout for finalizer cleanup
	FinalizerTimeout = 5 * time.Minute
)

// strictFieldValidation makes the API server reject fields the target schema does not
// declare, instead of silently pruning them — see issue #308 and the "Upstream
// compatibility" section of docs/providers.md.
//
// This provider renders built-in apps/v1 and v1 types via server-side apply, where the
// field manager ALREADY rejects unknown fields during typed conversion regardless of this
// option (verified: an SSA apply with validation explicitly ignored still fails with
// "field not declared in schema"). So here this mainly adds duplicate-key detection and
// keeps one uniform rule across all five providers; the providers that write third-party
// CRDs are the ones it genuinely protects.
var strictFieldValidation = client.FieldValidation(metav1.FieldValidationStrict)

// statusSafeRejectionDetail returns a form of err that is stable across identical calls, for
// storing in status.
//
// Server-side apply reports only the FIRST unknown field it encounters, and with more than
// one it picks a different field each time. Two independent confirmations:
//   - mechanism: structured-merge-diff's typed/validate.go appends one error and returns out
//     of the map walk, and value/mapunstructured.go iterates a plain Go map, so the field
//     chosen is whichever the randomised iteration reached first;
//   - observed: against a live API server, three unknown fields on one Deployment produced
//     three different messages across twelve identical apply calls. Putting that straight into
//
// status.message — or into a condition Message, which is status too — would rewrite status on
// every reconcile, and since the ModelDeployment watch has no GenerationChangedPredicate each
// write re-enqueues the object: an unbounded loop.
//
// Custom-resource strict decoding does not have this problem: apimachinery sorts the unknown
// field paths and lists them all, so those messages pass through unchanged.
//
// Applied on the generic-failure path too: a server-side apply TYPE mismatch carries the same
// wrapper without the unknown-field needle, so it lands there rather than in the rejection
// branch — and structured-merge-diff accumulates type errors without sorting them, so their
// concatenation order follows map iteration and is just as volatile.
//
// The full error is always logged; only the stored copy is normalised.
func statusSafeRejectionDetail(err error) string {
	msg := err.Error()
	// These are the two wrappers apimachinery's structuredmerge can produce on the APPLY
	// path. It has two more ("failed to convert new/live object … to smd typed") carrying the
	// same payload on the non-apply Update path, which this provider never takes — add them
	// here if that ever changes, or the volatile detail gets through and the loop returns.
	if strings.Contains(msg, "failed to create typed patch object") ||
		strings.Contains(msg, "failed to create typed live object") {
		return "the offending field and the exact reason are in the controller logs"
	}
	return msg
}

// isUpstreamSchemaRejection reports whether err is the API server refusing a field the
// installed upstream does not declare, as opposed to any other rejection.
//
// Matching on the message rather than the status class is deliberate, because the class
// varies by write path:
//   - custom resource create/update -> 400 BadRequest, "strict decoding error: unknown field"
//   - custom resource merge patch   -> 422 Invalid,    same prefix (verified live)
//   - server-side apply on built-in types -> 500, "field not declared in schema"
//     (verified against a live cluster: the error is a plain error from the field manager,
//     so it is neither IsBadRequest nor IsInvalid)
//
// Gating on IsInvalid alone would also swallow every CEL and OpenAPI type violation, which
// are user configuration errors that no upstream upgrade would fix — reporting those as an
// upstream version mismatch would send operators down the wrong path entirely.
//
// The needle is the "strict decoding error" prefix rather than the bare "unknown field"
// cause it wraps, because an Invalid status echoes the offending value back and a
// user-supplied string (a model id, an image, an engine arg) could otherwise contain the
// bare phrase and be misclassified.
func isUpstreamSchemaRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()

	// Custom-resource paths: apimachinery wraps the unknown-field cause in a
	// "strict decoding error" prefix. Require BOTH parts. An Invalid status echoes the
	// offending value back, so a user-supplied string (a model id, an image, an engine arg)
	// containing either phrase on its own must not be misclassified as a version mismatch
	// and retried forever.
	if strings.Contains(msg, "strict decoding error") && strings.Contains(msg, "unknown field") {
		return true
	}

	// Server-side apply on built-in types: the rejection comes from the field manager's
	// typed conversion, not from field validation, so it carries a different wrapper and a
	// different status class. Bind the diagnostic to that wrapper for the same reason.
	if strings.Contains(msg, "failed to create typed patch object") ||
		strings.Contains(msg, "failed to create typed live object") {
		return strings.Contains(msg, "field not declared in schema")
	}

	return false
}

var (
	deploymentGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	serviceGVK    = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}
)

// LLMDProviderReconciler reconciles ModelDeployment resources for the llm-d provider
type LLMDProviderReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Transformer      *Transformer
	StatusTranslator *StatusTranslator
}

// NewLLMDProviderReconciler creates a new llm-d provider reconciler
func NewLLMDProviderReconciler(c client.Client, scheme *runtime.Scheme) *LLMDProviderReconciler {
	return &LLMDProviderReconciler{
		Client:           c,
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
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles the reconciliation loop for ModelDeployments assigned to the llm-d provider
func (r *LLMDProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	logger.Info("Reconciling ModelDeployment for llm-d provider", "name", md.Name, "namespace", md.Namespace)

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
	r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderCompatible, metav1.ConditionTrue, "CompatibilityVerified", "Configuration compatible with llm-d")

	// Transform ModelDeployment to Deployments + Services
	resources, err := r.Transformer.Transform(ctx, &md)
	if err != nil {
		logger.Error(err, "Failed to transform ModelDeployment", "name", md.Name)
		// Same treatment as the upstream-rejection path below: force Ready False and drop
		// the stale endpoint/replica counts. Otherwise a previously-Running deployment whose
		// spec is edited into something unrenderable reports Failed while still advertising
		// a live endpoint and "1/1 ready" — the contradiction strict validation exists to surface.
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "TransformFailed", err.Error())
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "TransformFailed", err.Error())
		md.Status.Endpoint = nil
		md.Status.Replicas = nil
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = fmt.Sprintf("Failed to generate llm-d resources: %s", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &md)
	}

	// Create or update all resources
	for _, resource := range resources {
		if err := r.createOrUpdateResource(ctx, resource, &md); err != nil {
			// Transient API conflict — requeue instead of marking as failed
			if errors.IsConflict(err) {
				logger.Info("Resource conflict during reconcile, requeueing", "name", resource.GetName())
				return ctrl.Result{Requeue: true}, nil
			}
			logger.Error(err, "Failed to create/update resource", "name", resource.GetName(), "kind", resource.GetKind())
			// Strict field validation rejected the write: the cluster does not accept a field
			// this provider renders. Give it its own reason so an operator can tell it apart
			// from a generic create failure, and keep requeueing — the remedy is
			// an out-of-band upstream upgrade, and nothing else would re-trigger this
			// reconcile. The provider-config watch fires only on Spec/Ready changes, and no
			// upstream object exists to watch, so without a requeue the deployment would sit
			// Failed until the ~10h resync even after the cluster is fixed.
			//
			// Ready is forced False here because the failure it catches is precisely a
			// deployment that reports healthy while being unable to serve. Note this deliberately does NOT
			// touch ProviderCompatible: that is set True earlier in this same reconcile, so
			// flipping it here would rewrite LastTransitionTime on every requeue and the
			// condition would never settle.
			if isUpstreamSchemaRejection(err) {
				detail := statusSafeRejectionDetail(err)
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "IncompatibleUpstream", detail)
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "IncompatibleUpstream", detail)
				// Clear the Running-era endpoint and replica counts. This branch returns
				// before syncStatus, so on an update rejection they would otherwise keep
				// their previous values and the object would report Failed alongside a live
				// endpoint and "1/1 ready" — the same contradiction described above.
				md.Status.Endpoint = nil
				md.Status.Replicas = nil
				md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
				md.Status.Message = fmt.Sprintf("Incompatible with the installed upstream: the cluster rejected a field in the rendered resource. This provider renders built-in Kubernetes types, so it usually means spec.provider.overrides sets a field that does not exist, or the cluster's Kubernetes version predates a field this provider uses. %s", detail)
				if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				return ctrl.Result{RequeueAfter: RequeueInterval}, nil
			}
			reason := "CreateFailed"
			if isResourceConflict(err) {
				// Ready is set below unconditionally with this same reason.
				reason = "ResourceConflict"
			}
			// Same treatment as the TransformFailed and IncompatibleUpstream branches: a
			// Failed deployment must not keep advertising a live endpoint and "1/1 ready".
			// Leaving this branch alone would make the most common failure the one that
			// still reports healthy — the same shape.
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, statusSafeRejectionDetail(err))
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, reason, statusSafeRejectionDetail(err))
			md.Status.Endpoint = nil
			md.Status.Replicas = nil
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = fmt.Sprintf("Failed to create/update resource %s: %s", resource.GetName(), statusSafeRejectionDetail(err))
			// Requeue. Unlike a schema rejection this is usually transient — a timeout, a
			// leader change, a momentary 500 — and it is the case that most needs a retry.
			// Without one the second pass writes identical status, which the API server
			// treats as a no-op, so nothing re-enqueues and the deployment sits Failed with
			// its endpoint cleared until the ~10h resync even though the workload may still
			// be serving.
			if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: RequeueInterval}, nil
		}
	}

	r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionTrue, "ResourceCreated", "Deployments and Services created successfully")

	// Update provider status — use the primary Deployment (resources[0]) for tracking
	if len(resources) > 0 {
		md.Status.Provider.ResourceName = resources[0].GetName()
		md.Status.Provider.ResourceKind = resources[0].GetKind()
	}

	// Sync status from the primary Deployment
	if len(resources) > 0 {
		if err := r.syncStatus(ctx, &md, resources[0]); err != nil {
			logger.Error(err, "Failed to sync status", "name", md.Name)
		}
	}

	// Set phase to Deploying if not already Running or Failed
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning &&
		md.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
		md.Status.Message = "Deployments created, waiting for pods to be ready"
	}

	if err := r.Status().Update(ctx, &md); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Reconciliation complete", "name", md.Name, "phase", md.Status.Phase)

	// Requeue to periodically sync status
	return ctrl.Result{RequeueAfter: RequeueInterval}, nil
}

// validateCompatibility checks if the ModelDeployment configuration is compatible with llm-d
func (r *LLMDProviderReconciler) validateCompatibility(md *airunwayv1alpha1.ModelDeployment) error {
	// llm-d only supports vLLM
	if md.ResolvedEngineType() != airunwayv1alpha1.EngineTypeVLLM {
		return fmt.Errorf("llm-d provider only supports vllm engine, got %s", md.ResolvedEngineType())
	}

	// Disaggregated mode: validate component-level GPUs
	if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		if md.Spec.Scaling == nil || md.Spec.Scaling.Prefill == nil {
			return fmt.Errorf("spec.scaling.prefill is required for disaggregated serving mode")
		}
		if md.Spec.Scaling.Decode == nil {
			return fmt.Errorf("spec.scaling.decode is required for disaggregated serving mode")
		}
		if md.Spec.Scaling.Prefill.GPU == nil || md.Spec.Scaling.Prefill.GPU.Count == 0 {
			return fmt.Errorf("llm-d provider requires GPU resources for prefill (spec.scaling.prefill.gpu.count > 0)")
		}
		if md.Spec.Scaling.Decode.GPU == nil || md.Spec.Scaling.Decode.GPU.Count == 0 {
			return fmt.Errorf("llm-d provider requires GPU resources for decode (spec.scaling.decode.gpu.count > 0)")
		}
	} else {
		// Aggregated mode: require top-level GPU
		if md.Spec.Resources == nil || md.Spec.Resources.GPU == nil || md.Spec.Resources.GPU.Count == 0 {
			return fmt.Errorf("llm-d provider requires GPU resources (spec.resources.gpu.count > 0)")
		}
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

// createOrUpdateResource creates or updates an unstructured resource using server-side apply.
// Server-side apply avoids resourceVersion conflicts that occur when Kubernetes defaults
// fields between our Get and Update calls.
func (r *LLMDProviderReconciler) createOrUpdateResource(ctx context.Context, resource *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	// For existing resources, verify ownership before applying
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(resource.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{
		Name:      resource.GetName(),
		Namespace: resource.GetNamespace(),
	}, existing)
	if err == nil {
		if err := verifyOwnerReference(existing, md.UID); err != nil {
			return err
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get existing resource: %w", err)
	}

	// Server-side apply: handles both create and update without needing resourceVersion.
	// ForceOwnership ensures our field manager wins over any conflicting field managers.
	logger.Info("Applying resource", "kind", resource.GetKind(), "name", resource.GetName())
	return r.Patch(ctx, resource, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership, strictFieldValidation)
}

// syncStatus fetches the primary Deployment and syncs its status to the ModelDeployment
func (r *LLMDProviderReconciler) syncStatus(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, desired *unstructured.Unstructured) error {
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

	statusResult, err := r.StatusTranslator.TranslateStatus(upstream)
	if err != nil {
		return fmt.Errorf("failed to translate status: %w", err)
	}

	md.Status.Phase = statusResult.Phase
	if statusResult.Message != "" {
		md.Status.Message = statusResult.Message
	} else if statusResult.Phase == airunwayv1alpha1.DeploymentPhaseRunning {
		// The translator reports no message for a healthy Deployment; replace the
		// stale "waiting for pods" message so status reflects the Running phase.
		md.Status.Message = "Deployments created, pods are ready"
	}
	md.Status.Replicas = statusResult.Replicas
	md.Status.Endpoint = statusResult.Endpoint

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
func (r *LLMDProviderReconciler) handleDeletion(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) (ctrl.Result, error) {
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

	// Determine primary Deployment name (decode suffix for disaggregated mode)
	primaryName := md.Name
	if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		primaryName = md.Name + "-decode"
	}

	// Delete the primary Deployment (other resources have OwnerReferences and will be GC'd)
	deploy := &unstructured.Unstructured{}
	deploy.SetGroupVersionKind(deploymentGVK)

	err := r.Get(ctx, types.NamespacedName{
		Name:      primaryName,
		Namespace: md.Namespace,
	}, deploy)

	if err == nil {
		// Verify ownership before deleting
		if err := verifyOwnerReference(deploy, md.UID); err != nil {
			logger.Info("Deployment exists but is not managed by this ModelDeployment, skipping deletion", "name", primaryName)
			controllerutil.RemoveFinalizer(md, FinalizerName)
			return ctrl.Result{}, r.Update(ctx, md)
		}

		logger.Info("Deleting primary Deployment", "name", primaryName)
		if err := r.Delete(ctx, deploy); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Failed to delete Deployment")

			if time.Since(md.DeletionTimestamp.Time) > FinalizerTimeout {
				logger.Info("Finalizer timeout reached, removing finalizer without cleanup")
				controllerutil.RemoveFinalizer(md, FinalizerName)
				return ctrl.Result{}, r.Update(ctx, md)
			}

			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		// For disaggregated mode, also delete the prefill Deployment explicitly
		if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
			prefillDeploy := &unstructured.Unstructured{}
			prefillDeploy.SetGroupVersionKind(deploymentGVK)
			prefillName := md.Name + "-prefill"

			if err := r.Get(ctx, types.NamespacedName{Name: prefillName, Namespace: md.Namespace}, prefillDeploy); err == nil {
				if verifyOwnerReference(prefillDeploy, md.UID) == nil {
					logger.Info("Deleting prefill Deployment", "name", prefillName)
					_ = r.Delete(ctx, prefillDeploy)
				}
			}
		}

		// Requeue to wait for deletion
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get Deployment: %w", err)
	}

	// Resource is gone, remove finalizer
	logger.Info("Deployment deleted, removing finalizer", "name", md.Name)
	controllerutil.RemoveFinalizer(md, FinalizerName)
	return ctrl.Result{}, r.Update(ctx, md)
}

// setCondition updates a condition on the ModelDeployment
func (r *LLMDProviderReconciler) setCondition(md *airunwayv1alpha1.ModelDeployment, conditionType string, status metav1.ConditionStatus, reason, message string) {
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
func (r *LLMDProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.ModelDeployment{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			md, ok := obj.(*airunwayv1alpha1.ModelDeployment)
			if !ok {
				return false
			}
			// Process if provider is llmd OR if being deleted (to handle finalizer)
			if md.Status.Provider != nil && md.Status.Provider.Name == ProviderName {
				return true
			}
			// Also process if spec explicitly requests llmd
			if md.Spec.Provider != nil && md.Spec.Provider.Name == ProviderName {
				return true
			}
			// Process if we have our finalizer (for deletion handling)
			return controllerutil.ContainsFinalizer(md, FinalizerName)
		})).
		Named("llmd-provider").
		Complete(r)
}
