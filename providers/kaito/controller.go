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
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/csaupgrade"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

const (
	// ProviderName is the name of this provider
	ProviderName = "kaito"

	// FinalizerName is the finalizer used by this controller
	FinalizerName = "airunway.ai/kaito-provider"

	// FieldManager is the server-side apply field manager name
	FieldManager = "kaito-provider"

	// createFieldsManager is used only for the atomic Create collision boundary.
	// Its temporary Update ownership is migrated before FieldManager records the
	// final rendered configuration.
	createFieldsManager = "kaito-provider-create"

	// preservedFieldsManager holds only non-rendered fields discovered while
	// migrating legacy Create/Update ownership. Keeping it separate ensures the
	// stable FieldManager declaratively owns only the rendered configuration.
	preservedFieldsManager = "kaito-provider-preserved-fields"

	// lastAppliedWorkspaceAnnotation stores the Workspace fields rendered by this controller.
	// It provides a stable no-op fingerprint and a migration record for pre-SSA Workspaces.
	lastAppliedWorkspaceAnnotation = "airunway.ai/kaito-last-applied"

	// migrationManagersAnnotation temporarily records the exact legacy Update
	// managers captured before first SSA adoption so an interrupted migration can
	// resume without guessing from unrelated managers' managedFields entries.
	migrationManagersAnnotation = "airunway.ai/kaito-migration-managers"

	// migrationPreviousFieldsAnnotation keeps the original last-applied
	// fingerprint until preservation ownership has been released successfully.
	migrationPreviousFieldsAnnotation = "airunway.ai/kaito-migration-previous-fields"

	// RequeueInterval is the default requeue interval for periodic reconciliation
	RequeueInterval = 30 * time.Second

	// ExternalRecoveryInterval retries failures that require an out-of-band fix without
	// hot-looping while the installed upstream or resource ownership remains unchanged.
	ExternalRecoveryInterval = 5 * time.Minute

	// FinalizerTimeout is the timeout for finalizer cleanup
	FinalizerTimeout = 5 * time.Minute
)

// strictFieldValidation makes the API server reject fields the installed upstream does
// not declare, instead of silently pruning them — see issue #308 and the "Upstream
// compatibility" section of docs/providers.md. kubectl sends strict validation by default;
// Go clients do not, so it must be set explicitly on every upstream write.
var strictFieldValidation = client.FieldValidation(metav1.FieldValidationStrict)

// strictUnknownFieldRejection matches the terminal diagnostic emitted by apimachinery's
// strict decoder. Anchoring the diagnostic at the end keeps an ordinary validation error
// from matching when its echoed user value contains the same words.
var strictUnknownFieldRejection = regexp.MustCompile(
	`(^|: )strict decoding error: unknown field "(\\.|[^"\\])*"(, unknown field "(\\.|[^"\\])*")*$`,
)

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

	// Custom-resource paths: match the complete terminal diagnostic, not independent
	// substrings. An Invalid status echoes the offending value back, so a user-supplied
	// string may itself contain both phrases and must not be misclassified as a version
	// mismatch and retried forever.
	if strictUnknownFieldRejection.MatchString(msg) {
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

// isRetryableUpstreamWriteError reports failures that can recover without changing the
// ModelDeployment or cluster configuration. These must not erase last-known serving status:
// a failed or ambiguous API response does not mean the existing workload stopped serving.
func isRetryableUpstreamWriteError(err error) bool {
	if err == nil {
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

// resourceWriteError records whether the target was observed as owned, active, and serving
// before its write. A transient update failure can retain last-known serving status only after
// that safe observation; create failures, terminating or unready resources, and unverified
// read failures cannot.
type resourceWriteError struct {
	err                              error
	resourceWasOwnedActiveAndServing bool
}

func (e *resourceWriteError) Error() string { return e.err.Error() }
func (e *resourceWriteError) Unwrap() error { return e.err }

func wrapResourceWriteError(err error, resourceWasOwnedActiveAndServing bool) error {
	if err == nil {
		return nil
	}
	return &resourceWriteError{err: err, resourceWasOwnedActiveAndServing: resourceWasOwnedActiveAndServing}
}

func canPreserveLastKnownStatus(err error) bool {
	var writeErr *resourceWriteError
	return stderrors.As(err, &writeErr) && writeErr.resourceWasOwnedActiveAndServing
}

// KaitoProviderReconciler reconciles ModelDeployment resources for the KAITO provider
type KaitoProviderReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Transformer      *Transformer
	StatusTranslator *StatusTranslator
	DirectClient     client.Client
	Recorder         record.EventRecorder
}

// NewKaitoProviderReconciler creates a new KAITO provider reconciler
func NewKaitoProviderReconciler(c client.Client, scheme *runtime.Scheme, direct client.Client, recorder record.EventRecorder) *KaitoProviderReconciler {
	return &KaitoProviderReconciler{
		Client:           c,
		Scheme:           scheme,
		Transformer:      NewTransformer(),
		StatusTranslator: NewStatusTranslator(),
		DirectClient:     direct,
		Recorder:         recorder,
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

	// Upstream health probe — refuse-fast before transform if the real KAITO
	// workspace controller is not running.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 10*time.Second)
	health := probeUpstreamController(probeCtx, r.DirectClient)
	cancelProbe()
	if !health.Healthy {
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, health.Reason, health.Message)
		r.Recorder.Event(&md, corev1.EventTypeWarning, health.Reason, health.Message)
		if err := r.Status().Update(ctx, &md); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: RequeueInterval}, nil
	}

	// Transform ModelDeployment to KAITO Workspace
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
		md.Status.Message = fmt.Sprintf("Failed to generate KAITO resources: %s", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &md)
	}

	// Create or update the Workspace
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
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "IncompatibleUpstream", err.Error())
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "IncompatibleUpstream", err.Error())
				// Clear the Running-era endpoint and replica counts. This branch returns
				// before syncStatus, so on an update rejection they would otherwise keep
				// their previous values and the object would report Failed alongside a live
				// endpoint and "1/1 ready" — the same contradiction described above.
				md.Status.Endpoint = nil
				md.Status.Replicas = nil
				md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
				md.Status.Message = fmt.Sprintf("Incompatible with the installed upstream: the installed KAITO CRD does not declare a field this provider renders. This usually means the cluster's KAITO is older than this provider requires, or that spec.provider.overrides sets a key it does not support. %s", err.Error())
				if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				return ctrl.Result{RequeueAfter: ExternalRecoveryInterval}, nil
			}
			// Conflicts, throttling, server failures, and transport interruptions say nothing
			// about whether the existing workload is still serving. Record the failed write,
			// but preserve last-known Phase/Ready/Endpoint/Replicas until a successful read can
			// replace them.
			fieldManagerConflict := isFieldManagerConflict(err)
			retryableWriteError := isRetryableUpstreamWriteError(err) && !fieldManagerConflict
			if retryableWriteError && canPreserveLastKnownStatus(err) {
				reason := "CreateFailed"
				if errors.IsConflict(err) || errors.IsAlreadyExists(err) {
					reason = "ResourceConflict"
				}
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, err.Error())
				if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				requeueAfter := RequeueInterval
				if errors.IsConflict(err) {
					// A fresh read resolves a resourceVersion race; do not delay desired
					// spec convergence by the normal transient-error interval.
					requeueAfter = time.Second
				}
				return ctrl.Result{RequeueAfter: requeueAfter}, nil
			}
			reason := "CreateFailed"
			requeueAfter := ExternalRecoveryInterval
			if errors.IsConflict(err) && !fieldManagerConflict {
				requeueAfter = time.Second
			} else if errors.IsNotFound(err) || retryableWriteError {
				// A definite 404 means the write did not reach an existing upstream
				// object. Fail closed, but retry on the normal recovery cadence because
				// discovery or admission ordering can make this short-lived.
				requeueAfter = RequeueInterval
			}
			if isResourceConflict(err) {
				reason = "ResourceConflict"
			}
			// Definite write failures fail closed. Validation/admission and ownership
			// failures use a slower retry because an out-of-band policy, CRD, or ownership
			// change can make the same ModelDeployment valid without changing its spec.
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, err.Error())
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, reason, err.Error())
			md.Status.Endpoint = nil
			md.Status.Replicas = nil
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = fmt.Sprintf("Failed to create Workspace: %s", err.Error())
			if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			if fieldManagerConflict {
				// Changing or relinquishing the conflicting field manager will update
				// the Workspace and trigger another reconcile. Do not hot-loop a
				// deterministic ownership conflict in the meantime.
				return ctrl.Result{}, nil
			}
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
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
	return stderrors.As(err, &conflict) || isFieldManagerConflict(err) || errors.IsAlreadyExists(err)
}

func isFieldManagerConflict(err error) bool {
	var statusError errors.APIStatus
	if !stderrors.As(err, &statusError) {
		return false
	}
	details := statusError.Status().Details
	if details == nil {
		return false
	}
	for _, cause := range details.Causes {
		if cause.Type == metav1.CauseTypeFieldManagerConflict {
			return true
		}
	}
	return false
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

// createOrUpdateResource creates, adopts, or updates a Workspace with server-side apply.
// A preliminary Get protects resources owned by a different ModelDeployment. Once
// adopted, FieldManager's managedFields entry is the source of truth for ownership:
// omitting a previously applied field deletes it while fields owned by KAITO survive.
func (r *KaitoProviderReconciler) createOrUpdateResource(ctx context.Context, resource *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	if err := setLastAppliedManagedFields(resource); err != nil {
		return err
	}

	// Check if resource exists
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(resource.GroupVersionKind())

	err := r.Get(ctx, types.NamespacedName{
		Name:      resource.GetName(),
		Namespace: resource.GetNamespace(),
	}, existing)

	if errors.IsNotFound(err) {
		// Apply is an upsert, so it cannot safely preserve the create collision
		// boundary after this non-atomic existence check. Create atomically first;
		// a concurrent creator then gets AlreadyExists without this controller
		// mutating or adopting its object. After Create succeeds, migrate its
		// Update ownership through the same preservation handoff used for legacy
		// objects before recording the stable Apply ownership.
		logger.Info("Creating resource", "kind", resource.GetKind(), "name", resource.GetName())
		created := resource.DeepCopy()
		if err := setPendingMigrationManagers(created, map[string]struct{}{createFieldsManager: struct{}{}}); err != nil {
			return err
		}
		if err := r.Create(ctx, created, client.FieldOwner(createFieldsManager), strictFieldValidation); err != nil {
			if !errors.IsAlreadyExists(err) {
				return wrapResourceWriteError(
					fmt.Errorf("failed to create Workspace %s/%s: %w", resource.GetNamespace(), resource.GetName(), err),
					false,
				)
			}

			// A parallel reconcile or a stale cached Get can race with Create.
			// Read directly from the API server and let the normal ownership check
			// below distinguish a same-owner winner from a foreign collision.
			reader := r.DirectClient
			if reader == nil {
				reader = r.Client
			}
			if err := reader.Get(ctx, types.NamespacedName{
				Name:      resource.GetName(),
				Namespace: resource.GetNamespace(),
			}, existing); err != nil {
				return fmt.Errorf("failed to get Workspace %s/%s after create collision: %w", resource.GetNamespace(), resource.GetName(), err)
			}
		} else {
			applied, err := r.applyWorkspace(ctx, withoutLastAppliedWorkspaceAnnotation(resource), created.GetResourceVersion())
			if err != nil {
				return wrapResourceWriteError(err, false)
			}
			_, err = r.completeOwnershipMigration(ctx, applied, resource, map[string]struct{}{createFieldsManager: struct{}{}})
			return wrapResourceWriteError(err, false)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get existing resource: %w", err)
	}

	// Verify ownership before updating
	if err := verifyOwnerReference(existing, md.UID); err != nil {
		return err
	}
	resourceWasOwnedActiveAndServing := false
	if existing.GetDeletionTimestamp() == nil && r.StatusTranslator != nil {
		statusResult, statusErr := r.StatusTranslator.TranslateStatus(existing)
		resourceWasOwnedActiveAndServing = statusErr == nil && statusResult.Phase == airunwayv1alpha1.DeploymentPhaseRunning
	}
	return wrapResourceWriteError(
		r.reconcileExistingWorkspace(ctx, existing, resource, md),
		resourceWasOwnedActiveAndServing,
	)
}

func (r *KaitoProviderReconciler) reconcileExistingWorkspace(ctx context.Context, existing, resource *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) error {
	logger := log.FromContext(ctx)

	if hasApplyManagedFields(existing) {
		migrationManagers, migrationPending, err := pendingMigrationManagers(existing)
		if err != nil {
			return err
		}
		updateManagers, err := updateManagersOwningMigrationState(existing)
		if err != nil {
			return err
		}
		_, controllerUpdatePending := updateManagers[FieldManager]
		if controllerUpdatePending {
			migrationManagers[FieldManager] = struct{}{}
		}
		preservedOverlap, err := managerOwnsAnyDesired(existing, preservedFieldsManager, resource)
		if err != nil {
			return err
		}
		if migrationPending || controllerUpdatePending || preservedOverlap {
			stableApplied, err := r.completeOwnershipMigration(ctx, existing, resource, migrationManagers)
			if err != nil {
				return err
			}
			if stableApplied {
				return nil
			}
		}
	}

	// Avoid an API write when our previously applied state is still present.
	// The last-applied annotation detects desired-field removals; the subset
	// comparison detects drift while ignoring fields defaulted by KAITO.
	matches, err := workspaceMatchesDesired(existing, resource)
	if err != nil {
		return err
	}
	if matches {
		return nil
	}

	if !hasApplyManagedFields(existing) {
		// Existing Workspaces were historically written with Create/Update. Record
		// the exact managers selected for migration, then let the managedFields
		// handoff move only their fields through SSA. The bridge patch changes no
		// rendered value, so unrelated Apply ownership still produces a conflict.
		logger.Info("Adopting resource with server-side apply", "kind", resource.GetKind(), "name", resource.GetName())
		migrationManagers, err := legacyUpdateManagers(existing, md.UID)
		if err != nil {
			return err
		}
		migrationManagers[FieldManager] = struct{}{}
		migrated, err := r.markLegacyWorkspaceMigration(ctx, existing, migrationManagers)
		if err != nil {
			return err
		}
		_, err = r.completeOwnershipMigration(ctx, migrated, resource, migrationManagers)
		return err
	}

	logger.Info("Updating resource with server-side apply", "kind", resource.GetKind(), "name", resource.GetName())
	_, err = r.applyWorkspace(ctx, resource, existing.GetResourceVersion())
	return err
}

func (r *KaitoProviderReconciler) applyWorkspace(ctx context.Context, resource *unstructured.Unstructured, resourceVersion string) (*unstructured.Unstructured, error) {
	return r.applyWorkspaceAs(ctx, resource, FieldManager, resourceVersion)
}

func (r *KaitoProviderReconciler) applyWorkspaceAs(ctx context.Context, resource *unstructured.Unstructured, manager, resourceVersion string) (*unstructured.Unstructured, error) {
	// The apply patch must receive the freshly rendered configuration, never a
	// live object containing status or server metadata.
	// resourceVersion makes the verified Workspace identity part of the write,
	// so a delete/recreate between Get and Apply returns Conflict rather than
	// allowing this controller to adopt the replacement.
	if resourceVersion == "" {
		return nil, fmt.Errorf("cannot server-side apply Workspace %s/%s without resourceVersion", resource.GetNamespace(), resource.GetName())
	}
	applied := resource.DeepCopy()
	applied.SetResourceVersion(resourceVersion)
	// controller-runtime's Client.Apply options do not expose fieldValidation.
	// Patch with client.Apply is the equivalent SSA request and lets every KAITO
	// write retain the repository-wide Strict validation guarantee.
	if err := r.Patch(ctx, applied, client.Apply, client.FieldOwner(manager), strictFieldValidation); err != nil {
		return nil, fmt.Errorf("failed to server-side apply Workspace %s/%s: %w", resource.GetNamespace(), resource.GetName(), err)
	}
	return applied, nil
}

// updateManagersOwningLastApplied returns Update managers that own AI Runway's
// reserved migration annotation. This is the same ownership signal used by
// Kubernetes' client-side-to-server-side apply migration; manager names alone
// are insufficient because clients may choose identical or changing names.
func updateManagersOwningLastApplied(resource *unstructured.Unstructured) (map[string]struct{}, error) {
	return updateManagersOwningAnyField(resource, [][]string{
		{"f:metadata", "f:annotations", "f:" + lastAppliedWorkspaceAnnotation},
	})
}

func updateManagersOwningMigrationState(resource *unstructured.Unstructured) (map[string]struct{}, error) {
	return updateManagersOwningAnyField(resource, [][]string{
		{"f:metadata", "f:annotations", "f:" + lastAppliedWorkspaceAnnotation},
		{"f:metadata", "f:annotations", "f:" + migrationManagersAnnotation},
	})
}

func pendingMigrationManagers(resource *unstructured.Unstructured) (map[string]struct{}, bool, error) {
	annotation, found := resource.GetAnnotations()[migrationManagersAnnotation]
	if !found {
		return map[string]struct{}{}, false, nil
	}
	var managerNames []string
	if err := json.Unmarshal([]byte(annotation), &managerNames); err != nil {
		return nil, true, fmt.Errorf("failed to decode Workspace %s/%s migration managers annotation: %w", resource.GetNamespace(), resource.GetName(), err)
	}
	managers := make(map[string]struct{}, len(managerNames))
	for _, manager := range managerNames {
		if manager == "" {
			return nil, true, fmt.Errorf("failed to decode Workspace %s/%s migration managers annotation: manager name is empty", resource.GetNamespace(), resource.GetName())
		}
		managers[manager] = struct{}{}
	}
	return managers, true, nil
}

// legacyUpdateManagers identifies the pre-SSA manager before any migration
// patch can transfer the last-applied annotation. Workspaces predating that
// annotation fall back to controller-specific identity fields, but only after
// their ModelDeployment owner reference has already been verified by the caller.
func legacyUpdateManagers(resource *unstructured.Unstructured, ownerUID types.UID) (map[string]struct{}, error) {
	managers, err := updateManagersOwningLastApplied(resource)
	if err != nil || len(managers) > 0 {
		return managers, err
	}
	labelManagers, err := updateManagersOwningAnyField(resource, [][]string{
		{"f:metadata", "f:labels", "f:airunway.ai/managed-by"},
		{"f:metadata", "f:labels", "f:airunway.ai/model-deployment"},
	})
	if err != nil || len(labelManagers) == 1 {
		return labelManagers, err
	}
	ownerManagers, err := updateManagersOwningOwnerReferenceUID(resource, ownerUID)
	if err != nil {
		return nil, err
	}
	if len(labelManagers) == 0 {
		if len(ownerManagers) == 1 {
			return ownerManagers, nil
		}
		return map[string]struct{}{}, nil
	}
	intersection := map[string]struct{}{}
	for manager := range labelManagers {
		if _, ownsVerifiedReference := ownerManagers[manager]; ownsVerifiedReference {
			intersection[manager] = struct{}{}
		}
	}
	if len(intersection) == 1 {
		return intersection, nil
	}
	// Ambiguous identity ownership is safer to leave in place than to transfer
	// an unrelated manager's entire Update field set.
	return map[string]struct{}{}, nil
}

func updateManagersOwningOwnerReferenceUID(resource *unstructured.Unstructured, ownerUID types.UID) (map[string]struct{}, error) {
	managers := map[string]struct{}{}
	if ownerUID == "" {
		return managers, nil
	}
	for _, entry := range resource.GetManagedFields() {
		if entry.Operation != metav1.ManagedFieldsOperationUpdate || entry.Subresource != "" || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			return nil, fmt.Errorf("failed to decode Workspace managedFields for manager %q: %w", entry.Manager, err)
		}
		ownerReferences, found, err := unstructured.NestedMap(fields, "f:metadata", "f:ownerReferences")
		if err != nil {
			return nil, fmt.Errorf("failed to inspect Workspace ownerReference managedFields for manager %q: %w", entry.Manager, err)
		}
		if !found {
			continue
		}
		for fieldKey := range ownerReferences {
			if len(fieldKey) > 2 && fieldKey[:2] == "k:" && jsonKeyContainsUID(fieldKey[2:], string(ownerUID)) {
				managers[entry.Manager] = struct{}{}
				break
			}
		}
	}
	return managers, nil
}

func updateManagersOwningAnyField(resource *unstructured.Unstructured, fieldPaths [][]string) (map[string]struct{}, error) {
	managers := map[string]struct{}{}
	for _, entry := range resource.GetManagedFields() {
		if entry.Operation != metav1.ManagedFieldsOperationUpdate || entry.Subresource != "" || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			return nil, fmt.Errorf("failed to decode Workspace managedFields for manager %q: %w", entry.Manager, err)
		}
		for _, fieldPath := range fieldPaths {
			if _, found, err := unstructured.NestedFieldNoCopy(fields, fieldPath...); err != nil {
				return nil, fmt.Errorf("failed to inspect Workspace managedFields for manager %q: %w", entry.Manager, err)
			} else if found {
				managers[entry.Manager] = struct{}{}
				break
			}
		}
	}
	return managers, nil
}

// completeOwnershipMigration uses Kubernetes' supported client-side-apply
// managedFields migration to convert legacy Update ownership into a dedicated
// Apply manager. That manager then applies only the live fields absent from the
// rendered configuration, relinquishing rendered fields without deleting
// webhook defaults or other preserved values.
func (r *KaitoProviderReconciler) completeOwnershipMigration(ctx context.Context, live, desired *unstructured.Unstructured, capturedManagers map[string]struct{}) (bool, error) {
	stableApplied := false
	previouslyRendered, err := lastAppliedWorkspaceConfiguration(live)
	if err != nil {
		return false, err
	}
	migrationDesired := desired
	annotations := live.GetAnnotations()
	_, hasManagerMarker := annotations[migrationManagersAnnotation]
	_, hasPreviousFields := annotations[migrationPreviousFieldsAnnotation]
	migrationPending := hasManagerMarker || hasPreviousFields
	if migrationPending {
		// Keep only the old fingerprint while migration is pending. Applying the
		// new fingerprint alongside it can exceed Kubernetes' annotation-size
		// limit. The full desired configuration is applied after cleanup removes
		// the old fingerprint.
		migrationDesired = withoutLastAppliedWorkspaceAnnotation(desired)
	}
	managers := map[string]struct{}{}
	for manager := range capturedManagers {
		managers[manager] = struct{}{}
	}
	if len(managers) > 0 {
		preservedFields, err := managedFieldsForManager(live, preservedFieldsManager, metav1.ManagedFieldsOperationApply)
		if err != nil {
			return false, err
		}
		if preservedFields == nil {
			seed, found, err := capturedUpdateFieldsConfiguration(live, managers)
			if err != nil {
				return false, err
			}
			if found {
				live, err = r.applyWorkspaceAs(ctx, seed, preservedFieldsManager, live.GetResourceVersion())
				if err != nil {
					return false, fmt.Errorf("failed to seed preserved Workspace %s/%s fields: %w", desired.GetNamespace(), desired.GetName(), err)
				}
			}
		}
		managerNames := sets.New[string]()
		for manager := range managers {
			managerNames.Insert(manager)
		}
		patchData, err := csaupgrade.UpgradeManagedFieldsPatch(live, managerNames, preservedFieldsManager)
		if err != nil {
			return false, fmt.Errorf("failed to prepare Workspace %s/%s managedFields migration: %w", live.GetNamespace(), live.GetName(), err)
		}
		if patchData != nil {
			if err := r.Patch(ctx, live, client.RawPatch(types.JSONPatchType, patchData), strictFieldValidation); err != nil {
				return false, fmt.Errorf("failed to migrate Workspace %s/%s managedFields: %w", live.GetNamespace(), live.GetName(), err)
			}
		}
	}
	preservedOverlap, err := managerOwnsAnyDesired(live, preservedFieldsManager, migrationDesired)
	if err != nil {
		return false, err
	}
	stableOwnsDesired, err := applyManagerOwnsDesired(live, migrationDesired)
	if err != nil {
		return false, err
	}
	stableHasDesiredValues := desiredSubsetMatches(migrationDesired.Object, live.Object)
	stableReady := stableOwnsDesired && stableHasDesiredValues
	if preservedOverlap && !stableReady {
		// Move overlapping fields to their desired values while the
		// preservation manager still owns them. The stable Apply below then
		// shares ownership before the preservation manager omits them.
		handoff, err := preservedFieldsHandoffConfiguration(live, migrationDesired)
		if err != nil {
			return false, err
		}
		live, err = r.applyWorkspaceAs(ctx, handoff, preservedFieldsManager, live.GetResourceVersion())
		if err != nil {
			return false, fmt.Errorf("failed to prepare preserved Workspace %s/%s fields for ownership handoff: %w", desired.GetNamespace(), desired.GetName(), err)
		}
	}
	if !stableReady {
		live, err = r.applyWorkspace(ctx, migrationDesired, live.GetResourceVersion())
		if err != nil {
			return false, fmt.Errorf("failed to claim Workspace %s/%s fields: %w", desired.GetNamespace(), desired.GetName(), err)
		}
		stableApplied = true
	}
	preserved, err := preservedFieldsConfiguration(live, migrationDesired, previouslyRendered, true)
	if err != nil {
		return false, err
	}
	released, err := r.applyWorkspaceAs(ctx, preserved, preservedFieldsManager, live.GetResourceVersion())
	if err != nil {
		return false, fmt.Errorf("failed to preserve non-rendered Workspace %s/%s fields: %w", live.GetNamespace(), live.GetName(), err)
	}
	cleaned := released
	if previouslyRendered != nil {
		cleaned, err = r.removeUnownedPreviouslyRenderedFields(ctx, released, migrationDesired, previouslyRendered)
		if err != nil {
			return false, err
		}
	}
	annotations = cleaned.GetAnnotations()
	_, hasManagerMarker = annotations[migrationManagersAnnotation]
	_, hasPreviousFields = annotations[migrationPreviousFieldsAnnotation]
	if hasManagerMarker || hasPreviousFields {
		finalPreserved, err := preservedFieldsConfiguration(cleaned, migrationDesired, previouslyRendered, false)
		if err != nil {
			return false, err
		}
		cleaned, err = r.applyWorkspaceAs(ctx, finalPreserved, preservedFieldsManager, cleaned.GetResourceVersion())
		if err != nil {
			return false, fmt.Errorf("failed to finish Workspace %s/%s ownership migration: %w", live.GetNamespace(), live.GetName(), err)
		}
	}
	if migrationPending {
		if _, err := r.applyWorkspace(ctx, desired, cleaned.GetResourceVersion()); err != nil {
			return false, fmt.Errorf("failed to record Workspace %s/%s applied configuration: %w", live.GetNamespace(), live.GetName(), err)
		}
		stableApplied = true
	}
	return stableApplied, nil
}

func (r *KaitoProviderReconciler) removeUnownedPreviouslyRenderedFields(ctx context.Context, live, desired, previouslyRendered *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	stale, keep := subtractDesiredJSON(previouslyRendered.Object, desired.Object, nil)
	if !keep {
		return live, nil
	}
	staleFields, ok := stale.(map[string]interface{})
	if !ok {
		return live, nil
	}
	ownedFields, err := managedFieldsOwnedConfiguration(live)
	if err != nil {
		return nil, err
	}
	unowned, keep := subtractDesiredJSON(staleFields, ownedFields, nil)
	if !keep {
		return live, nil
	}
	unownedFields, ok := unowned.(map[string]interface{})
	if !ok {
		return live, nil
	}
	cleaned, _ := subtractDesiredJSON(live.Object, unownedFields, nil)
	cleanedObject, ok := cleaned.(map[string]interface{})
	if !ok || equality.Semantic.DeepEqual(cleanedObject, live.Object) {
		return live, nil
	}
	updated := live.DeepCopy()
	updated.Object = cleanedObject
	if err := r.Patch(ctx, updated, client.MergeFromWithOptions(live.DeepCopy(), client.MergeFromWithOptimisticLock{}), client.FieldOwner(FieldManager), strictFieldValidation); err != nil {
		return nil, fmt.Errorf("failed to remove unowned legacy Workspace %s/%s fields: %w", live.GetNamespace(), live.GetName(), err)
	}
	return updated, nil
}

func managedFieldsOwnedConfiguration(live *unstructured.Unstructured) (map[string]interface{}, error) {
	owned := map[string]interface{}{}
	for _, entry := range live.GetManagedFields() {
		if entry.Subresource != "" || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			return nil, fmt.Errorf("failed to decode Workspace managedFields for manager %q: %w", entry.Manager, err)
		}
		owned = mergeManagedFieldValues(owned, extractManagedFieldsMap(live.Object, fields))
	}
	return owned, nil
}

func capturedUpdateFieldsConfiguration(live *unstructured.Unstructured, managers map[string]struct{}) (*unstructured.Unstructured, bool, error) {
	object := map[string]interface{}{}
	found := false
	for _, entry := range live.GetManagedFields() {
		if _, captured := managers[entry.Manager]; !captured ||
			entry.Operation != metav1.ManagedFieldsOperationUpdate || entry.Subresource != "" || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			return nil, false, fmt.Errorf("failed to decode Workspace managedFields for manager %q: %w", entry.Manager, err)
		}
		object = mergeManagedFieldValues(object, extractManagedFieldsMap(live.Object, fields))
		found = true
	}
	if !found {
		return nil, false, nil
	}
	delete(object, "status")
	return workspaceConfigurationWithIdentity(object, live), true, nil
}

func mergeManagedFieldValues(target, source map[string]interface{}) map[string]interface{} {
	merged := runtime.DeepCopyJSON(target)
	for key, sourceValue := range source {
		targetValue, found := merged[key]
		if !found {
			merged[key] = runtime.DeepCopyJSONValue(sourceValue)
			continue
		}
		targetMap, targetIsMap := targetValue.(map[string]interface{})
		sourceMap, sourceIsMap := sourceValue.(map[string]interface{})
		if targetIsMap && sourceIsMap {
			merged[key] = mergeManagedFieldValues(targetMap, sourceMap)
			continue
		}
		targetList, targetIsList := targetValue.([]interface{})
		sourceList, sourceIsList := sourceValue.([]interface{})
		if targetIsList && sourceIsList {
			combined := runtime.DeepCopyJSONValue(targetList).([]interface{})
			for _, item := range sourceList {
				alreadyPresent := false
				for _, existingItem := range combined {
					if equality.Semantic.DeepEqual(existingItem, item) {
						alreadyPresent = true
						break
					}
				}
				if !alreadyPresent {
					combined = append(combined, runtime.DeepCopyJSONValue(item))
				}
			}
			merged[key] = combined
			continue
		}
		merged[key] = runtime.DeepCopyJSONValue(sourceValue)
	}
	return merged
}

func preservedFieldsHandoffConfiguration(live, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	fields, err := managedFieldsForManager(live, preservedFieldsManager, metav1.ManagedFieldsOperationApply)
	if err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("cannot hand off Workspace %s/%s fields: migration manager %q is missing", live.GetNamespace(), live.GetName(), preservedFieldsManager)
	}
	object := extractManagedFieldsMap(live.Object, fields)
	delete(object, "status")
	desiredOwnedFields := extractManagedFieldsMap(desired.Object, fields)
	object = migrateLegacyJSONMap(object, desiredOwnedFields, nil, nil)
	replaceAtomicDesiredFields(object, desiredOwnedFields)
	stableFields, err := managedFieldsForManager(live, FieldManager, metav1.ManagedFieldsOperationApply)
	if err != nil {
		return nil, err
	}
	if stableFields != nil {
		// Relinquish preservation ownership for fields the stable manager
		// already owns. Their old values remain present while the stable Apply
		// changes them; preservation-only fields stay in this handoff so they
		// can first move to the desired value without an intermediate deletion.
		stableOwned := extractManagedFieldsMap(live.Object, stableFields)
		remaining, _ := subtractDesiredJSON(object, stableOwned, nil)
		var ok bool
		object, ok = remaining.(map[string]interface{})
		if !ok {
			object = map[string]interface{}{}
		}
	}
	return workspaceConfigurationWithIdentity(object, live), nil
}

func replaceAtomicDesiredFields(target, desired map[string]interface{}) {
	targetResource, targetHasResource := target["resource"].(map[string]interface{})
	desiredResource, desiredHasResource := desired["resource"].(map[string]interface{})
	if !targetHasResource || !desiredHasResource {
		return
	}
	if selector, found := desiredResource["labelSelector"]; found {
		targetResource["labelSelector"] = runtime.DeepCopyJSONValue(selector)
	}
}

func managedFieldsForManager(resource *unstructured.Unstructured, manager string, operation metav1.ManagedFieldsOperationType) (map[string]interface{}, error) {
	for _, entry := range resource.GetManagedFields() {
		if entry.Manager != manager || entry.Operation != operation || entry.Subresource != "" || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			return nil, fmt.Errorf("failed to decode Workspace managedFields for manager %q: %w", entry.Manager, err)
		}
		return fields, nil
	}
	return nil, nil
}

func managerOwnsAnyDesired(existing *unstructured.Unstructured, manager string, desired *unstructured.Unstructured) (bool, error) {
	fields, err := managedFieldsForManager(existing, manager, metav1.ManagedFieldsOperationApply)
	if err != nil || fields == nil {
		return false, err
	}
	return managedFieldsOverlapDesired(desired.Object, fields, nil), nil
}

func managedFieldsOverlapDesired(desired map[string]interface{}, fields map[string]interface{}, path []string) bool {
	for key, desiredValue := range desired {
		if shouldIgnoreDesiredOwnershipPath(path, key) {
			continue
		}
		fieldValue, found := fields["f:"+key]
		if !found {
			continue
		}
		fieldChildren, ok := fieldValue.(map[string]interface{})
		if !ok || len(fieldChildren) == 0 {
			return true
		}
		nextPath := append(path, key)
		switch value := desiredValue.(type) {
		case map[string]interface{}:
			if managedFieldsOverlapDesired(value, fieldChildren, nextPath) {
				return true
			}
		case []interface{}:
			if !pathMatches(nextPath, "metadata", "ownerReferences") {
				return true
			}
			for _, item := range value {
				reference, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				uid, _ := reference["uid"].(string)
				for fieldKey := range fieldChildren {
					if len(fieldKey) > 2 && fieldKey[:2] == "k:" && jsonKeyContainsUID(fieldKey[2:], uid) {
						return true
					}
				}
			}
		default:
			return true
		}
	}
	return false
}

func jsonKeyContainsUID(encoded, uid string) bool {
	if uid == "" {
		return false
	}
	var keyFields map[string]interface{}
	return json.Unmarshal([]byte(encoded), &keyFields) == nil && keyFields["uid"] == uid
}

func preservedFieldsConfiguration(live, desired, previouslyRendered *unstructured.Unstructured, keepMigrationState bool) (*unstructured.Unstructured, error) {
	fields, err := managedFieldsForManager(live, preservedFieldsManager, metav1.ManagedFieldsOperationApply)
	if err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("cannot preserve Workspace %s/%s fields: migration manager %q is missing", live.GetNamespace(), live.GetName(), preservedFieldsManager)
	}
	object := extractManagedFieldsMap(live.Object, fields)
	delete(object, "status")
	if !keepMigrationState {
		unstructured.RemoveNestedField(object, "metadata", "annotations", migrationManagersAnnotation)
		unstructured.RemoveNestedField(object, "metadata", "annotations", migrationPreviousFieldsAnnotation)
	}

	preserved, _ := subtractDesiredJSON(object, desired.Object, nil)
	preservedObject, ok := preserved.(map[string]interface{})
	if !ok {
		preservedObject = map[string]interface{}{}
	}
	if previouslyRendered != nil {
		preserved, _ = subtractDesiredJSON(preservedObject, previouslyRendered.Object, nil)
		preservedObject, ok = preserved.(map[string]interface{})
		if !ok {
			preservedObject = map[string]interface{}{}
		}
	}
	return workspaceConfigurationWithIdentity(preservedObject, live), nil
}

func workspaceConfigurationWithIdentity(object map[string]interface{}, live *unstructured.Unstructured) *unstructured.Unstructured {
	object["apiVersion"] = live.GetAPIVersion()
	object["kind"] = live.GetKind()
	metadata, _ := object["metadata"].(map[string]interface{})
	if metadata == nil {
		metadata = map[string]interface{}{}
		object["metadata"] = metadata
	}
	metadata["name"] = live.GetName()
	metadata["namespace"] = live.GetNamespace()
	return &unstructured.Unstructured{Object: object}
}

func extractManagedFieldsMap(live, fields map[string]interface{}) map[string]interface{} {
	extracted := map[string]interface{}{}
	for fieldKey, fieldValue := range fields {
		if len(fieldKey) < 3 || fieldKey[:2] != "f:" {
			continue
		}
		key := fieldKey[2:]
		liveValue, found := live[key]
		if !found {
			continue
		}
		fieldChildren, ok := fieldValue.(map[string]interface{})
		if !ok || len(fieldChildren) == 0 {
			extracted[key] = runtime.DeepCopyJSONValue(liveValue)
			continue
		}
		switch value := liveValue.(type) {
		case map[string]interface{}:
			children := extractManagedFieldsMap(value, fieldChildren)
			if len(children) > 0 {
				extracted[key] = children
			}
		case []interface{}:
			items := extractManagedFieldsList(value, fieldChildren)
			if len(items) > 0 {
				extracted[key] = items
			}
		default:
			extracted[key] = runtime.DeepCopyJSONValue(liveValue)
		}
	}
	return extracted
}

func extractManagedFieldsList(live []interface{}, fields map[string]interface{}) []interface{} {
	keyedFields := map[string]map[string]interface{}{}
	setValues := make([]interface{}, 0)
	for fieldKey, fieldValue := range fields {
		if len(fieldKey) < 3 {
			continue
		}
		switch fieldKey[:2] {
		case "k:":
			children, ok := fieldValue.(map[string]interface{})
			if ok {
				keyedFields[fieldKey[2:]] = children
			}
		case "v:":
			var value interface{}
			if json.Unmarshal([]byte(fieldKey[2:]), &value) == nil {
				setValues = append(setValues, value)
			}
		}
	}
	if len(keyedFields) == 0 && len(setValues) == 0 {
		return runtime.DeepCopyJSONValue(live).([]interface{})
	}

	extracted := make([]interface{}, 0, len(keyedFields)+len(setValues))
	for _, ownedValue := range setValues {
		for _, liveValue := range live {
			if equality.Semantic.DeepEqual(liveValue, ownedValue) {
				extracted = append(extracted, runtime.DeepCopyJSONValue(liveValue))
				break
			}
		}
	}
	for encodedKey, children := range keyedFields {
		var keyFields map[string]interface{}
		if json.Unmarshal([]byte(encodedKey), &keyFields) != nil {
			continue
		}
		for _, item := range live {
			itemMap, ok := item.(map[string]interface{})
			if !ok || !jsonMapContains(itemMap, keyFields) {
				continue
			}
			var extractedItem map[string]interface{}
			_, ownsItemNode := children["."]
			if len(children) == 0 || (len(children) == 1 && ownsItemNode) {
				extractedItem = runtime.DeepCopyJSON(itemMap)
			} else {
				extractedItem = extractManagedFieldsMap(itemMap, children)
			}
			for key, value := range keyFields {
				extractedItem[key] = runtime.DeepCopyJSONValue(value)
			}
			extracted = append(extracted, extractedItem)
			break
		}
	}
	return extracted
}

func jsonMapContains(values, expected map[string]interface{}) bool {
	for key, expectedValue := range expected {
		if !equality.Semantic.DeepEqual(values[key], expectedValue) {
			return false
		}
	}
	return true
}

func subtractDesiredJSON(live, desired interface{}, path []string) (interface{}, bool) {
	liveMap, liveIsMap := live.(map[string]interface{})
	desiredMap, desiredIsMap := desired.(map[string]interface{})
	if liveIsMap && desiredIsMap {
		remaining := runtime.DeepCopyJSON(liveMap)
		for key, desiredValue := range desiredMap {
			liveValue, found := remaining[key]
			if !found {
				continue
			}
			nextPath := append(path, key)
			if pathMatches(nextPath, "resource", "labelSelector") {
				// KAITO declares this map atomic. Any ownership overlap is for the
				// whole selector, never for an independently subtractable child.
				delete(remaining, key)
				continue
			}
			if pathMatches(nextPath, "metadata", "ownerReferences") {
				liveReferences, liveOK := liveValue.([]interface{})
				desiredReferences, desiredOK := desiredValue.([]interface{})
				if liveOK && desiredOK {
					filtered := filterDesiredOwnerReferences(liveReferences, desiredReferences)
					if len(filtered) == 0 {
						delete(remaining, key)
					} else {
						remaining[key] = filtered
					}
					continue
				}
			}
			if child, keep := subtractDesiredJSON(liveValue, desiredValue, nextPath); keep {
				remaining[key] = child
			} else {
				delete(remaining, key)
			}
		}
		return remaining, len(remaining) > 0
	}
	return nil, false
}

func filterDesiredOwnerReferences(live, desired []interface{}) []interface{} {
	desiredUIDs := map[string]struct{}{}
	for _, item := range desired {
		if reference, ok := item.(map[string]interface{}); ok {
			if uid, ok := reference["uid"].(string); ok {
				desiredUIDs[uid] = struct{}{}
			}
		}
	}
	filtered := make([]interface{}, 0, len(live))
	for _, item := range live {
		reference, ok := item.(map[string]interface{})
		uid, hasUID := reference["uid"].(string)
		if ok && hasUID {
			if _, rendered := desiredUIDs[uid]; rendered {
				continue
			}
		}
		filtered = append(filtered, runtime.DeepCopyJSONValue(item))
	}
	return filtered
}

func hasApplyManagedFields(resource *unstructured.Unstructured) bool {
	for _, entry := range resource.GetManagedFields() {
		if entry.Manager == FieldManager &&
			entry.Operation == metav1.ManagedFieldsOperationApply &&
			entry.Subresource == "" {
			return true
		}
	}
	return false
}

func workspaceMatchesDesired(existing, desired *unstructured.Unstructured) (bool, error) {
	if !hasApplyManagedFields(existing) {
		return false, nil
	}
	owned, err := applyManagerOwnsDesired(existing, desired)
	if err != nil || !owned {
		return false, err
	}
	return desiredSubsetMatches(desired.Object, existing.Object), nil
}

// applyManagerOwnsDesired prevents the value-only no-op check from masking a
// rendered field that another manager force-took and restored to the same value.
// Re-applying in that case re-establishes shared declarative ownership without
// forcing the other manager away.
func applyManagerOwnsDesired(existing, desired *unstructured.Unstructured) (bool, error) {
	fields, err := managedFieldsForManager(existing, FieldManager, metav1.ManagedFieldsOperationApply)
	if err != nil || fields == nil {
		return false, err
	}
	return managedFieldsOwnDesired(desired.Object, fields, nil), nil
}

func managedFieldsOwnDesired(desired map[string]interface{}, fields map[string]interface{}, path []string) bool {
	for key, desiredValue := range desired {
		if shouldIgnoreDesiredOwnershipPath(path, key) {
			continue
		}
		fieldValue, found := fields["f:"+key]
		if !found {
			return false
		}
		fieldChildren, ok := fieldValue.(map[string]interface{})
		if !ok {
			return false
		}
		if len(fieldChildren) == 0 {
			continue
		}

		nextPath := append(path, key)
		switch value := desiredValue.(type) {
		case map[string]interface{}:
			if !managedFieldsOwnDesired(value, fieldChildren, nextPath) {
				return false
			}
		case []interface{}:
			if pathMatches(nextPath, "metadata", "ownerReferences") &&
				!managedFieldsOwnOwnerReferences(value, fieldChildren) {
				return false
			}
		}
	}
	return true
}

func managedFieldsOwnOwnerReferences(desired []interface{}, fields map[string]interface{}) bool {
	if len(fields) == 0 {
		return true
	}
	for _, desiredItem := range desired {
		desiredReference, ok := desiredItem.(map[string]interface{})
		if !ok {
			return false
		}
		desiredUID, ok := desiredReference["uid"].(string)
		if !ok || desiredUID == "" {
			return false
		}
		matched := false
		for fieldKey, fieldValue := range fields {
			if len(fieldKey) < 2 || fieldKey[:2] != "k:" {
				continue
			}
			var keyFields map[string]interface{}
			if err := json.Unmarshal([]byte(fieldKey[2:]), &keyFields); err != nil || keyFields["uid"] != desiredUID {
				continue
			}
			fieldChildren, ok := fieldValue.(map[string]interface{})
			if !ok {
				return false
			}
			if _, ownsWholeItem := fieldChildren["."]; len(fieldChildren) == 0 || ownsWholeItem {
				matched = true
				break
			}
			if managedFieldsOwnDesired(desiredReference, fieldChildren, []string{"metadata", "ownerReferences"}) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func shouldIgnoreDesiredOwnershipPath(path []string, key string) bool {
	if len(path) == 0 && (key == "apiVersion" || key == "kind") {
		return true
	}
	if pathMatches(path, "metadata") {
		switch key {
		case "name", "namespace":
			return true
		}
	}
	if pathMatches(path, "metadata", "ownerReferences") && key == "uid" {
		// UID is the associative-list key and is represented by the surrounding
		// k:{"uid":...} FieldsV1 node rather than an f:uid child.
		return true
	}
	return false
}

// desiredSubsetMatches compares only rendered fields. Extra map fields may be
// KAITO defaults or fields owned by another SSA manager. The pinned KAITO CRD
// declares resource.labelSelector atomic, so that map is compared exactly;
// AI Runway owns the entire selector whenever it renders one. KAITO spec lists
// are likewise compared atomically because the CRD does not declare them
// associative; metadata.ownerReferences is compared by UID according to the
// Kubernetes metadata schema.
func desiredSubsetMatches(desired, existing interface{}, path ...string) bool {
	switch desiredValue := desired.(type) {
	case map[string]interface{}:
		existingValue, ok := existing.(map[string]interface{})
		if !ok {
			return false
		}
		if pathMatches(path, "resource", "labelSelector") {
			return equality.Semantic.DeepEqual(desiredValue, existingValue)
		}
		for key, value := range desiredValue {
			existingField, found := existingValue[key]
			if !found || !desiredSubsetMatches(value, existingField, append(path, key)...) {
				return false
			}
		}
		return true
	case []interface{}:
		existingValue, ok := existing.([]interface{})
		if !ok {
			return false
		}
		if pathMatches(path, "metadata", "ownerReferences") {
			return ownerReferencesSubsetMatches(desiredValue, existingValue)
		}
		return equality.Semantic.DeepEqual(desiredValue, existingValue)
	default:
		return equality.Semantic.DeepEqual(desired, existing)
	}
}

func ownerReferencesSubsetMatches(desired, existing []interface{}) bool {
	for _, desiredItem := range desired {
		desiredReference, ok := desiredItem.(map[string]interface{})
		if !ok {
			return false
		}
		desiredUID, ok := desiredReference["uid"].(string)
		if !ok || desiredUID == "" {
			return false
		}
		matched := false
		for _, existingItem := range existing {
			existingReference, ok := existingItem.(map[string]interface{})
			if !ok || existingReference["uid"] != desiredUID {
				continue
			}
			if desiredSubsetMatches(desiredReference, existingReference, "metadata", "ownerReferences") {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func pathMatches(path []string, segments ...string) bool {
	if len(path) != len(segments) {
		return false
	}
	for index, segment := range segments {
		if path[index] != segment {
			return false
		}
	}
	return true
}

// markLegacyWorkspaceMigration persists only the exact Update manager names
// selected for the SSA handoff. Rendered fields are deliberately untouched so
// a conflicting Apply owner is surfaced by the subsequent stable Apply.
func (r *KaitoProviderReconciler) markLegacyWorkspaceMigration(ctx context.Context, existing *unstructured.Unstructured, migrationManagers map[string]struct{}) (*unstructured.Unstructured, error) {
	base := existing.DeepCopy()
	updated := existing.DeepCopy()
	if err := setPendingMigrationManagers(updated, migrationManagers); err != nil {
		return nil, err
	}

	if equality.Semantic.DeepEqual(base.Object, updated.Object) {
		return existing, nil
	}
	if err := r.Patch(ctx, updated, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}), client.FieldOwner(FieldManager), strictFieldValidation); err != nil {
		return nil, fmt.Errorf("failed to mark legacy Workspace %s/%s migration: %w", existing.GetNamespace(), existing.GetName(), err)
	}
	return updated, nil
}

func migrateLegacyJSONMap(existing, desired, legacy map[string]interface{}, path []string) map[string]interface{} {
	merged := runtime.DeepCopyJSON(existing)
	if merged == nil {
		merged = map[string]interface{}{}
	}
	for key, desiredValue := range desired {
		nextPath := append(path, key)
		if pathMatches(nextPath, "metadata", "ownerReferences") {
			existingReferences, existingOK := merged[key].([]interface{})
			desiredReferences, desiredOK := desiredValue.([]interface{})
			if existingOK && desiredOK {
				merged[key] = mergeDesiredOwnerReferences(existingReferences, desiredReferences)
				continue
			}
		}
		existingMap, existingIsMap := merged[key].(map[string]interface{})
		desiredMap, desiredIsMap := desiredValue.(map[string]interface{})
		legacyMap, _ := legacy[key].(map[string]interface{})
		if existingIsMap && desiredIsMap {
			merged[key] = migrateLegacyJSONMap(existingMap, desiredMap, legacyMap, nextPath)
		} else {
			merged[key] = runtime.DeepCopyJSONValue(desiredValue)
		}
	}
	for key := range merged {
		if _, desiredHasKey := desired[key]; desiredHasKey {
			continue
		}
		if _, legacyOwnedKey := legacy[key]; legacyOwnedKey {
			delete(merged, key)
		}
	}
	return merged
}

func mergeDesiredOwnerReferences(existing, desired []interface{}) []interface{} {
	merged := runtime.DeepCopyJSONValue(existing).([]interface{})
	for _, desiredItem := range desired {
		desiredReference, desiredIsReference := desiredItem.(map[string]interface{})
		desiredUID, desiredHasUID := desiredReference["uid"].(string)
		replaced := false
		if desiredIsReference && desiredHasUID && desiredUID != "" {
			for index, existingItem := range merged {
				existingReference, existingIsReference := existingItem.(map[string]interface{})
				existingUID, existingHasUID := existingReference["uid"].(string)
				if existingIsReference && existingHasUID && existingUID == desiredUID {
					merged[index] = runtime.DeepCopyJSONValue(desiredItem)
					replaced = true
					break
				}
			}
		}
		if !replaced {
			merged = append(merged, runtime.DeepCopyJSONValue(desiredItem))
		}
	}
	return merged
}

func managedAnnotations(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	managed := make(map[string]string, len(annotations))
	for key, value := range annotations {
		if key == lastAppliedWorkspaceAnnotation || key == migrationManagersAnnotation || key == migrationPreviousFieldsAnnotation {
			continue
		}
		managed[key] = value
	}
	if len(managed) == 0 {
		return nil
	}
	return managed
}

func setPendingMigrationManagers(resource *unstructured.Unstructured, managers map[string]struct{}) error {
	managerSet := sets.New[string]()
	for manager := range managers {
		managerSet.Insert(manager)
	}
	data, err := json.Marshal(sets.List(managerSet))
	if err != nil {
		return fmt.Errorf("failed to marshal Workspace migration managers: %w", err)
	}
	annotations := copyStringMap(resource.GetAnnotations())
	if _, alreadyRecorded := annotations[migrationPreviousFieldsAnnotation]; !alreadyRecorded {
		if previous, found := annotations[lastAppliedWorkspaceAnnotation]; found {
			annotations[migrationPreviousFieldsAnnotation] = previous
			delete(annotations, lastAppliedWorkspaceAnnotation)
		}
	}
	annotations[migrationManagersAnnotation] = string(data)
	resource.SetAnnotations(annotations)
	return nil
}

func withoutLastAppliedWorkspaceAnnotation(resource *unstructured.Unstructured) *unstructured.Unstructured {
	configuration := resource.DeepCopy()
	annotations := copyStringMap(configuration.GetAnnotations())
	delete(annotations, lastAppliedWorkspaceAnnotation)
	configuration.SetAnnotations(annotations)
	return configuration
}

// setLastAppliedManagedFields records the desired Workspace fields rendered by
// AI Runway. SSA owns the annotation with the rest of the desired configuration;
// future reconciles use it to detect removals without comparing KAITO defaults.
func setLastAppliedManagedFields(resource *unstructured.Unstructured) error {
	managedFields := map[string]interface{}{
		"labels":      copyStringMap(resource.GetLabels()),
		"annotations": copyStringMap(managedAnnotations(resource.GetAnnotations())),
	}
	if resourceSpec, found, _ := unstructured.NestedMap(resource.Object, "resource"); found {
		managedFields["resource"] = resourceSpec
	}
	if inference, found, _ := unstructured.NestedMap(resource.Object, "inference"); found {
		managedFields["inference"] = inference
	}

	data, err := json.Marshal(managedFields)
	if err != nil {
		return fmt.Errorf("failed to marshal last-applied Workspace fields: %w", err)
	}

	annotations := copyStringMap(resource.GetAnnotations())
	annotations[lastAppliedWorkspaceAnnotation] = string(data)
	resource.SetAnnotations(annotations)
	return nil
}

// lastAppliedManagedFields returns the Workspace fields written by the legacy
// Create/Update implementation. It is used only to migrate those fields into
// SSA ownership; managedFields is authoritative after adoption.
func lastAppliedManagedFields(existing *unstructured.Unstructured) (map[string]interface{}, map[string]interface{}, map[string]string, map[string]string, error) {
	annotation := existing.GetAnnotations()[lastAppliedWorkspaceAnnotation]
	if annotation == "" {
		return nil, nil, nil, nil, nil
	}

	var managedFields map[string]interface{}
	if err := json.Unmarshal([]byte(annotation), &managedFields); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to decode legacy Workspace %s/%s last-applied annotation: %w", existing.GetNamespace(), existing.GetName(), err)
	}

	resourceSpec, _ := managedFields["resource"].(map[string]interface{})
	inference, _ := managedFields["inference"].(map[string]interface{})
	labels := stringMapFromInterface(managedFields["labels"])
	annotations := stringMapFromInterface(managedFields["annotations"])
	return resourceSpec, inference, labels, annotations, nil
}

func lastAppliedWorkspaceConfiguration(existing *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	annotation := existing.GetAnnotations()[migrationPreviousFieldsAnnotation]
	if annotation == "" {
		annotation = existing.GetAnnotations()[lastAppliedWorkspaceAnnotation]
	}
	if annotation == "" {
		return nil, nil
	}
	snapshot := existing.DeepCopy()
	snapshotAnnotations := copyStringMap(snapshot.GetAnnotations())
	snapshotAnnotations[lastAppliedWorkspaceAnnotation] = annotation
	snapshot.SetAnnotations(snapshotAnnotations)
	resourceSpec, inference, labels, annotations, err := lastAppliedManagedFields(snapshot)
	if err != nil {
		return nil, err
	}
	configuration := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if resourceSpec != nil {
		configuration.Object["resource"] = runtime.DeepCopyJSON(resourceSpec)
	}
	if inference != nil {
		configuration.Object["inference"] = runtime.DeepCopyJSON(inference)
	}
	if len(labels) > 0 {
		configuration.SetLabels(copyStringMap(labels))
	}
	if len(annotations) > 0 {
		configuration.SetAnnotations(copyStringMap(annotations))
	}
	return configuration, nil
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return map[string]string{}
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func stringMapFromInterface(value interface{}) map[string]string {
	values, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		stringValue, ok := value.(string)
		if !ok {
			return nil
		}
		result[key] = stringValue
	}
	return result
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
	} else if statusResult.Phase == airunwayv1alpha1.DeploymentPhaseRunning {
		// The translator reports no message for a healthy Workspace; replace the
		// stale "waiting for pods" message so status reflects the Running phase.
		md.Status.Message = "Workspace created, pods are ready"
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
