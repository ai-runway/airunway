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
	"io"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/storage"
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

	// ExternalRecoveryInterval retries failures that require an out-of-band fix without
	// hot-looping while the installed upstream or resource ownership remains unchanged.
	ExternalRecoveryInterval = 5 * time.Minute

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
// This provider renders only built-in types — apps/v1 Deployment and v1 Service — and writes
// them through server-side apply, so that is the only rejection shape it can receive. It
// arrives as a plain error from the field manager's typed conversion
// (structured-merge-diff, typed/validate.go), neither IsBadRequest nor IsInvalid, which is
// why the match is on the message rather than the status class. Gating on IsInvalid would
// additionally swallow every CEL and OpenAPI type violation — user configuration errors no
// upstream upgrade would fix.
//
// The custom-resource shape ("strict decoding error: unknown field", 400 on create/update and
// 422 on merge patch) is deliberately NOT matched here: it is unreachable for this provider,
// and matching it would only create a way to misclassify an ordinary validation error whose
// echoed value happens to contain that phrasing — reported as a version mismatch and retried
// forever. The providers that do write custom resources match it in their own copy.
func isUpstreamSchemaRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()

	// Bind the diagnostic to the wrapper, so an error that merely echoes the phrase back
	// cannot match on the phrase alone.
	if strings.Contains(msg, "failed to create typed patch object") ||
		strings.Contains(msg, "failed to create typed live object") {
		return strings.Contains(msg, "field not declared in schema")
	}

	return false
}

// isRetryableUpstreamWriteError reports failures that can recover without changing the
// ModelDeployment or cluster configuration. These must not erase last-known serving status:
// a failed or ambiguous API response does not mean the existing workload stopped serving.
func isRetryableUpstreamWriteError(err error) bool {
	if err == nil {
		return false
	}
	// The API server may wrap deterministic structured-merge-diff conversion failures in
	// an HTTP 500. Retrying the identical rendered object cannot make those succeed; schema
	// unknown-field errors are classified separately before this helper is called.
	msg := err.Error()
	if strings.Contains(msg, "failed to create typed patch object") ||
		strings.Contains(msg, "failed to create typed live object") {
		return false
	}
	if errors.IsConflict(err) || errors.IsAlreadyExists(err) ||
		errors.IsTimeout(err) || errors.IsServerTimeout(err) || errors.IsTooManyRequests(err) ||
		errors.IsServiceUnavailable(err) || errors.IsInternalError(err) {
		return true
	}

	var status errors.APIStatus
	if stderrors.As(err, &status) && status.Status().Code >= 500 {
		return true
	}

	return stderrors.Is(err, context.DeadlineExceeded) ||
		stderrors.Is(err, io.EOF) || stderrors.Is(err, io.ErrUnexpectedEOF) ||
		stderrors.Is(err, syscall.EPIPE) ||
		utilnet.IsTimeout(err) || utilnet.IsProbableEOF(err) ||
		utilnet.IsConnectionReset(err) || utilnet.IsConnectionRefused(err) ||
		utilnet.IsHTTP2ConnectionLost(err)
}

// resourceWriteError records whether the target was observed before its write. A transient
// update failure can retain last-known serving status; a transient create failure cannot,
// because the controller had just observed that the required resource was absent.
type resourceWriteError struct {
	err             error
	resourceExisted bool
}

func (e *resourceWriteError) Error() string { return e.err.Error() }
func (e *resourceWriteError) Unwrap() error { return e.err }

func wrapResourceWriteError(err error, resourceExisted bool) error {
	if err == nil {
		return nil
	}
	return &resourceWriteError{err: err, resourceExisted: resourceExisted}
}

func canPreserveLastKnownStatus(err error) bool {
	var writeErr *resourceWriteError
	return !stderrors.As(err, &writeErr) || writeErr.resourceExisted
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

	// Releasing a terminating PVC takes precedence over validating the next
	// desired workload. An invalid spec must not keep older pods alive and
	// deadlock Kubernetes PVC protection.
	if storage.WorkloadTeardownRequired(&md) {
		workloads := []storage.ConsumerWorkload{
			{GroupVersionKind: deploymentGVK, Name: md.Name},
			{GroupVersionKind: deploymentGVK, Name: md.Name + "-decode"},
			{GroupVersionKind: deploymentGVK, Name: md.Name + "-prefill"},
		}
		if _, err := storage.EnsureConsumerWorkloadsAbsent(ctx, r.Client, &md, workloads...); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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

	if !storage.WorkloadReady(&md) {
		logger.Info("Waiting for model storage preparation", "name", md.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

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

	// Status may be preserved after an ambiguous update failure only when every required
	// resource existed before this write pass. Otherwise a successfully recreated earlier
	// resource followed by a failed later update could leave stale Ready=True status.
	preserveLastKnownStatusSafe := r.allRequiredResourcesAreOwnedActiveAndServing(ctx, resources, md.UID)

	// Create or update all resources
	for _, resource := range resources {
		if err := r.createOrUpdateResource(ctx, resource, &md); err != nil {
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
				return ctrl.Result{RequeueAfter: ExternalRecoveryInterval}, nil
			}
			// Conflicts, throttling, server failures, and transport interruptions say nothing
			// about whether the existing workload is still serving. Record the failed write,
			// but preserve last-known Phase/Ready/Endpoint/Replicas until a successful read can
			// replace them.
			retryableWriteError := isRetryableUpstreamWriteError(err)
			if retryableWriteError && preserveLastKnownStatusSafe && canPreserveLastKnownStatus(err) {
				reason := "CreateFailed"
				if errors.IsConflict(err) || errors.IsAlreadyExists(err) {
					reason = "ResourceConflict"
				}
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, statusSafeRejectionDetail(err))
				if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				requeueAfter := RequeueInterval
				if errors.IsConflict(err) {
					requeueAfter = time.Second
				}
				return ctrl.Result{RequeueAfter: requeueAfter}, nil
			}
			reason := "CreateFailed"
			// A definite NotFound means the prior workload cannot be assumed to still
			// exist, so fail closed but retry promptly. Other deterministic API-side
			// rejections may recover after an admission-policy or cluster change; retry
			// them at the slower external-recovery cadence.
			requeueAfter := ExternalRecoveryInterval
			if errors.IsConflict(err) {
				requeueAfter = time.Second
			} else if errors.IsNotFound(err) || retryableWriteError {
				requeueAfter = RequeueInterval
			}
			if isResourceConflict(err) {
				reason = "ResourceConflict"
			}
			// Validation/admission errors and ownership conflicts need an external change.
			// Fail closed and poll slowly so recovery does not depend on another watched event.
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, statusSafeRejectionDetail(err))
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, reason, statusSafeRejectionDetail(err))
			md.Status.Endpoint = nil
			md.Status.Replicas = nil
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = fmt.Sprintf("Failed to create/update resource %s: %s", resource.GetName(), statusSafeRejectionDetail(err))
			if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		// Once any earlier resource write succeeds, the pre-write serving snapshot no
		// longer proves the complete resource set is still serving. A later ambiguous
		// failure must therefore fail closed instead of retaining stale Ready status.
		preserveLastKnownStatusSafe = false
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
	resourceExisted := err == nil
	if resourceExisted {
		if err := verifyOwnerReference(existing, md.UID); err != nil {
			return err
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get existing resource: %w", err)
	}

	// Server-side apply: handles both create and update without needing resourceVersion.
	// ForceOwnership ensures our field manager wins over any conflicting field managers.
	logger.Info("Applying resource", "kind", resource.GetKind(), "name", resource.GetName())
	return wrapResourceWriteError(
		r.Patch(ctx, resource, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership, strictFieldValidation),
		resourceExisted,
	)
}

// allRequiredResourcesAreOwnedActiveAndServing snapshots whether the complete rendered
// resource set was present, owned by this ModelDeployment, and not terminating before
// reconciliation starts mutating it. Every Deployment must also be Running; otherwise stale
// serving status is unsafe to preserve if a later write fails ambiguously.
func (r *LLMDProviderReconciler) allRequiredResourcesAreOwnedActiveAndServing(
	ctx context.Context,
	resources []*unstructured.Unstructured,
	mdUID types.UID,
) bool {
	if r.StatusTranslator == nil {
		return false
	}
	foundDeployment := false
	for _, resource := range resources {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(resource.GroupVersionKind())
		if err := r.Get(ctx, types.NamespacedName{
			Name:      resource.GetName(),
			Namespace: resource.GetNamespace(),
		}, existing); err != nil {
			return false
		}
		if existing.GetDeletionTimestamp() != nil {
			return false
		}
		if err := verifyOwnerReference(existing, mdUID); err != nil {
			return false
		}
		if existing.GroupVersionKind() == deploymentGVK {
			foundDeployment = true
			statusResult, err := r.StatusTranslator.TranslateStatus(existing)
			if err != nil || statusResult.Phase != airunwayv1alpha1.DeploymentPhaseRunning {
				return false
			}
		}
	}
	return foundDeployment
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
	} else {
		// Do not retain a prior healthy message when current replica evidence
		// downgrades the Deployment to an in-progress state.
		md.Status.Message = "Deployments created, waiting for pods to be ready"
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
