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
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"syscall"
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
	utilnet "k8s.io/apimachinery/pkg/util/net"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/pkg/dynamointent"
	"github.com/ai-runway/airunway/controller/pkg/storage"
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

	// ExternalRecoveryInterval retries failures that require an out-of-band fix without
	// hot-looping while the installed upstream or resource ownership remains unchanged.
	ExternalRecoveryInterval = 5 * time.Minute

	// FinalizerTimeout is the timeout for finalizer cleanup
	FinalizerTimeout = 5 * time.Minute

	dynamoDGDRNameLabel      = "dgdr.nvidia.com/name"
	dynamoDGDRNamespaceLabel = "dgdr.nvidia.com/namespace"
)

var errIntentResourceReplacing = stderrors.New("intent resource replacement in progress")

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
// +kubebuilder:rbac:groups=nvidia.com,resources=dynamocomponentdeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=nvidia.com,resources=dynamographdeploymentrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=inference.networking.k8s.io,resources=inferencepools,verbs=get;list;watch
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
	resources, err := r.renderResources(ctx, &md)
	if err != nil {
		logger.Error(err, "Failed to transform ModelDeployment", "name", md.Name)
		var runtimeErr *runtimeDiscoveryError
		if stderrors.As(err, &runtimeErr) {
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "RuntimeVersionUndetermined", err.Error())
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionUnknown, "RuntimeVersionUndetermined", err.Error())
			md.Status.Phase = airunwayv1alpha1.DeploymentPhasePending
			md.Status.Message = err.Error()
			return ctrl.Result{RequeueAfter: ExternalRecoveryInterval}, r.Status().Update(ctx, &md)
		}

		if isResourceConflict(err) || isRetryableUpstreamWriteError(err) {
			reason := "CreateFailed"
			interval := RequeueInterval
			if isResourceConflict(err) {
				reason = "ResourceConflict"
				interval = ExternalRecoveryInterval
			}
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, err.Error())
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, reason, err.Error())
			md.Status.Endpoint = nil
			md.Status.Replicas = nil
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = err.Error()
			return ctrl.Result{RequeueAfter: interval}, r.Status().Update(ctx, &md)
		}

		// Same treatment as the upstream-rejection path below: force Ready False and drop
		// the stale endpoint/replica counts. Otherwise a previously-Running deployment whose
		// spec is edited into something unrenderable reports Failed while still advertising
		// a live endpoint and "1/1 ready" — the contradiction strict validation exists to surface.
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "TransformFailed", err.Error())
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "TransformFailed", err.Error())
		md.Status.Endpoint = nil
		md.Status.Replicas = nil
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = fmt.Sprintf("Failed to generate Dynamo resources: %s", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, &md)
	}

	// Create or update the rendered Dynamo resource.
	for _, resource := range resources {
		transitioning, transitionErr := r.ensureDeploymentModeTransition(ctx, resource, &md)
		if transitionErr != nil {
			var locked *intentLockedError
			if stderrors.As(transitionErr, &locked) {
				md.Status.Message = locked.Error()
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "IntentInputsLocked", locked.Error())
				return ctrl.Result{RequeueAfter: ExternalRecoveryInterval}, r.Status().Update(ctx, &md)
			}
			return ctrl.Result{}, transitionErr
		}
		if transitioning {
			if err := r.Status().Update(ctx, &md); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if err := r.createOrUpdateResource(ctx, resource, &md); err != nil {
			if stderrors.Is(err, errIntentResourceReplacing) {
				if err := r.Status().Update(ctx, &md); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			var locked *intentLockedError
			if stderrors.As(err, &locked) {
				// The desired edit failed, but the existing workload may still be healthy.
				if md.Status.Provider.RequestRef != nil {
					if syncErr := r.syncStatus(ctx, &md, referenceResource(md.Status.Provider.RequestRef)); syncErr != nil {
						return ctrl.Result{}, syncErr
					}
				}
				md.Status.Message = locked.Error()
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "IntentInputsLocked", locked.Error())
				return ctrl.Result{RequeueAfter: ExternalRecoveryInterval}, r.Status().Update(ctx, &md)
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
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "IncompatibleUpstream", err.Error())
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "IncompatibleUpstream", err.Error())
				// Clear the Running-era endpoint and replica counts. This branch returns
				// before syncStatus, so on an update rejection they would otherwise keep
				// their previous values and the object would report Failed alongside a live
				// endpoint and "1/1 ready" — the same contradiction described above.
				md.Status.Endpoint = nil
				md.Status.Replicas = nil
				md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
				md.Status.Message = fmt.Sprintf("Incompatible with the installed upstream: the installed Dynamo CRD does not declare a field this provider renders. This usually means the cluster's Dynamo is older than this provider requires, or that spec.provider.overrides sets a key it does not support. %s", err.Error())
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
			if errors.IsConflict(err) {
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
			md.Status.Message = fmt.Sprintf("Failed to create DynamoGraphDeployment: %s", err.Error())
			if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	resourceKind := resources[0].GetKind()
	r.setCondition(
		&md,
		airunwayv1alpha1.ConditionTypeResourceCreated,
		metav1.ConditionTrue,
		"ResourceCreated",
		fmt.Sprintf("%s created successfully", resourceKind),
	)

	// Update provider status
	md.Status.Provider.ResourceName = resources[0].GetName()
	md.Status.Provider.ResourceKind = resourceKind

	// Sync status from upstream resource
	if len(resources) > 0 {
		if err := r.syncStatus(ctx, &md, resources[0]); err != nil {
			logger.Error(err, "Failed to sync status", "name", md.Name)
			md.Status.Endpoint = nil
			md.Status.Replicas = nil
			md.Status.Provider.InferencePoolRef = nil
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
			md.Status.Message = fmt.Sprintf("Unable to read Dynamo serving status: %s", err)
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionUnknown, "StatusUnavailable", md.Status.Message)
		}
	}

	// Set phase to Deploying if not already Running or Failed
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning &&
		md.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
		if md.Status.Message == "" {
			md.Status.Message = fmt.Sprintf("%s created, waiting for pods to be ready", resourceKind)
		}
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

	// Mocker mode (test-only): the python3 -m dynamo.mocker backend simulates
	// serving without GPUs, so the GPU requirement is waived. It only supports
	// the vLLM engine, and disaggregated mode still needs prefill+decode scaling
	// blocks so the transformer can build both workers.
	if isMockerMode(md) {
		if md.ResolvedEngineType() != airunwayv1alpha1.EngineTypeVLLM {
			return fmt.Errorf("Dynamo mocker mode only supports the vllm engine")
		}
		if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
			if md.Spec.Scaling == nil || md.Spec.Scaling.Prefill == nil || md.Spec.Scaling.Decode == nil {
				return fmt.Errorf("Dynamo mocker disaggregated mode requires spec.scaling.prefill and spec.scaling.decode")
			}
		}
		return nil
	}

	if err := dynamointent.Validate(md); err != nil {
		return err
	}
	if intent, err := dynamointent.Parse(md); err != nil {
		return err
	} else if intent != nil {
		return nil
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
func (r *DynamoProviderReconciler) createOrUpdateResource(
	ctx context.Context,
	resource *unstructured.Unstructured,
	md *airunwayv1alpha1.ModelDeployment,
) error {
	logger := log.FromContext(ctx)

	if resource.GetKind() == DynamoGraphDeploymentRequestKind {
		return r.reconcileIntent(ctx, resource, md)
	}
	annotations := resource.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[manualInputHashAnnotation] = manualFingerprint(md)
	if annotations[runtimeVersionAnnotation] == "" {
		annotations[runtimeVersionAnnotation] = existingRuntimeVersion(resource)
	}
	resource.SetAnnotations(annotations)

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
		return wrapResourceWriteError(r.Create(ctx, resource, strictFieldValidation), false)
	}
	if err != nil {
		return fmt.Errorf("failed to get existing resource: %w", err)
	}

	// Verify ownership before updating
	if err := verifyDynamoOwnership(existing, md.UID); err != nil {
		return err
	}
	if existing.GetAnnotations()[manualInputHashAnnotation] == annotations[manualInputHashAnnotation] {
		return nil
	}
	// Retain operator/user metadata on spec updates.
	for key, value := range existing.GetAnnotations() {
		if _, ok := annotations[key]; !ok {
			annotations[key] = value
		}
	}
	resource.SetAnnotations(annotations)
	resourceWasOwnedActiveAndServing := false
	if existing.GetDeletionTimestamp() == nil && r.StatusTranslator != nil {
		statusResult, statusErr := r.StatusTranslator.TranslateStatus(existing)
		resourceWasOwnedActiveAndServing = statusErr == nil && statusResult.Phase == airunwayv1alpha1.DeploymentPhaseRunning
	}

	// Update existing resource if spec has changed.
	// The Dynamo CRD API server adds zero-value defaults (e.g. name: "",
	// resources: {}) that the provider never sets. Comparing raw specs would
	// trigger an infinite update loop. Strip server-added zero-values
	// from the existing spec before comparing.
	existingSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	newSpec, _, _ := unstructured.NestedMap(resource.Object, "spec")

	// Normalize server-added zero values on both sides for the ordinary comparison, then
	// compare paths explicitly supplied through provider.overrides.spec with presence-aware
	// semantics. Without the second check an empty unknown override such as futureField: {}
	// disappears during normalization, so no strict update is attempted and the incompatible
	// override appears to succeed.
	if !equality.Semantic.DeepEqual(stripEmptyDefaults(existingSpec), stripEmptyDefaults(newSpec)) ||
		overrideSpecDiffers(md, existingSpec, newSpec) {
		logger.Info("Updating resource", "kind", resource.GetKind(), "name", resource.GetName())
		// Keep upstream finalizers, defaults in metadata, and other owners. A
		// rendered spec update must not remove the operator's cleanup machinery.
		next := existing.DeepCopy()
		for key, value := range resource.Object {
			if key != "metadata" && key != "status" {
				next.Object[key] = value
			}
		}
		next.SetAnnotations(annotations)
		labels := next.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for key, value := range resource.GetLabels() {
			labels[key] = value
		}
		next.SetLabels(labels)
		return wrapResourceWriteError(r.Update(ctx, next, strictFieldValidation), resourceWasOwnedActiveAndServing)
	}

	if existing.GetAnnotations()[manualInputHashAnnotation] != annotations[manualInputHashAnnotation] {
		next := existing.DeepCopy()
		next.SetAnnotations(annotations)
		return r.Patch(ctx, next, client.MergeFromWithOptions(existing, client.MergeFromWithOptimisticLock{}), strictFieldValidation)
	}

	return nil
}

func (r *DynamoProviderReconciler) ensureDeploymentModeTransition(ctx context.Context, desired *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) (bool, error) {
	p := ensureProviderStatus(md)
	if desired.GetKind() == DynamoGraphDeploymentRequestKind {
		if p.RequestRef != nil {
			return false, nil
		}
		name := md.Name
		if p.WorkloadRef != nil {
			name = p.WorkloadRef.Name
		}
		dgd, err := r.findDGD(ctx, md.Namespace, name, p.WorkloadRef)
		if err != nil {
			return false, err
		}
		if dgd == nil {
			if p.Intent == nil {
				p.WorkloadRef = nil
			}
			return false, nil
		}
		if err := verifyDynamoOwnership(dgd, md.UID); err != nil {
			return false, nil
		}
		ready := meta.FindStatusCondition(md.Status.Conditions, airunwayv1alpha1.ConditionTypeReady)
		if p.WorkloadRef == nil || ready == nil || ready.Reason != "Reconfiguring" || md.Status.Phase != airunwayv1alpha1.DeploymentPhaseDeploying {
			p.WorkloadRef = resourceReference(dgd)
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
			md.Status.Endpoint = nil
			md.Status.Replicas = nil
			p.InferencePoolRef = nil
			r.setCondition(md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "Reconfiguring", "Switching to automatic configuration")
			return true, nil
		}
		pending, err := r.deleteRecordedWorkload(ctx, md)
		return pending, err
	}
	request, err := r.findRequest(ctx, md)
	if err != nil {
		return false, err
	}
	if request == nil && p.RequestRef == nil {
		return false, nil
	}
	attempt := ""
	if p.Intent != nil {
		attempt = p.Intent.Attempt
	} else if request != nil {
		attempt = request.GetAnnotations()[dynamointent.AttemptAnnotation]
	}
	if md.Annotations[dynamointent.AttemptAnnotation] == attempt {
		return false, &intentLockedError{message: "Change airunway.ai/dynamo-attempt before replacing automatic configuration with a manual deployment"}
	}
	needsCheckpoint := p.Intent == nil || p.Intent.Phase != "Replacing"
	if request != nil {
		previous := p.WorkloadRef
		p.RequestRef = resourceReference(request)
		if _, err := r.resolveGeneratedDGD(ctx, md, request); err != nil {
			return false, err
		}
		if previous == nil && p.WorkloadRef != nil {
			needsCheckpoint = true
		}
	}
	if p.Intent == nil {
		p.Intent = &airunwayv1alpha1.ProviderIntentStatus{Attempt: attempt}
	}
	p.Intent.Phase = "Replacing"
	md.Status.Endpoint = nil
	md.Status.Replicas = nil
	p.InferencePoolRef = nil
	md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
	r.setCondition(md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "Reconfiguring", "Switching to manual configuration")
	if needsCheckpoint {
		return true, nil
	}
	pending, err := r.deleteIntentResource(ctx, md, request)
	if err == nil && !pending {
		p.RequestRef = nil
		p.WorkloadRef = nil
		p.Intent = nil
	}
	return pending, err
}

func newDynamoResource(version, kind, name, namespace string) *unstructured.Unstructured {
	resource := &unstructured.Unstructured{}
	resource.SetGroupVersionKind(schema.GroupVersionKind{Group: DynamoAPIGroup, Version: version, Kind: kind})
	resource.SetName(name)
	resource.SetNamespace(namespace)
	return resource
}

func (r *DynamoProviderReconciler) deleteIntentResource(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
	dgdr *unstructured.Unstructured,
) (bool, error) {
	pending, err := r.deleteGeneratedDGDs(ctx, md, dgdr)
	if err != nil || pending {
		return pending, err
	}
	if dgdr == nil {
		return false, nil
	}
	if dgdr.GetDeletionTimestamp() != nil {
		return true, nil
	}
	if err := r.deleteWithIdentityPreconditions(ctx, dgdr); err != nil && !errors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

func (r *DynamoProviderReconciler) deleteGeneratedDGDs(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, dgdr *unstructured.Unstructured) (bool, error) {
	p := ensureProviderStatus(md)
	if p.WorkloadRef == nil && dgdr != nil {
		if _, err := r.resolveGeneratedDGD(ctx, md, dgdr); err != nil {
			return false, err
		}
		if p.WorkloadRef != nil {
			return true, nil
		} // caller must persist identity before cleanup
	}
	return r.deleteRecordedWorkload(ctx, md)
}

func (r *DynamoProviderReconciler) deleteWithIdentityPreconditions(
	ctx context.Context,
	resource client.Object,
) error {
	preconditions := &metav1.Preconditions{}
	if uid := resource.GetUID(); uid != "" {
		preconditions.UID = &uid
	}
	if resourceVersion := resource.GetResourceVersion(); resourceVersion != "" {
		preconditions.ResourceVersion = &resourceVersion
	}
	propagation := metav1.DeletePropagationForeground
	return r.Delete(ctx, resource, &client.DeleteOptions{Preconditions: preconditions, PropagationPolicy: &propagation})
}

// overrideSpecDiffers reports whether any path explicitly supplied through
// provider.overrides.spec is absent from, or differs between, the observed and desired
// specs. The override value acts as a path selector: provider-generated and server-defaulted
// siblings are deliberately ignored, while key presence is still significant for empty maps
// and strings that stripEmptyDefaults removes.
func overrideSpecDiffers(md *airunwayv1alpha1.ModelDeployment, existingSpec, desiredSpec map[string]interface{}) bool {
	if md.Spec.Provider == nil || md.Spec.Provider.Overrides == nil {
		return false
	}

	var overrides map[string]interface{}
	if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &overrides); err != nil {
		// Transform validates the same payload before this function is reached. Keep this
		// comparison side-effect free if a direct caller supplies malformed test data.
		return false
	}
	overrideSpec, ok := overrides["spec"].(map[string]interface{})
	if !ok {
		return false
	}

	return selectedOverrideValuesDiffer(existingSpec, desiredSpec, overrideSpec)
}

func selectedOverrideValuesDiffer(existing, desired, selected interface{}) bool {
	switch selectedValue := selected.(type) {
	case map[string]interface{}:
		existingMap, existingOK := existing.(map[string]interface{})
		desiredMap, desiredOK := desired.(map[string]interface{})
		if !existingOK || !desiredOK {
			return !equality.Semantic.DeepEqual(existing, desired)
		}
		for key, childSelection := range selectedValue {
			desiredChild, desiredFound := desiredMap[key]
			if !desiredFound {
				// A deep-merged override path should always be present in the desired
				// object. If it is not, there is nothing this update could validate.
				continue
			}
			existingChild, existingFound := existingMap[key]
			if !existingFound || selectedOverrideValuesDiffer(existingChild, desiredChild, childSelection) {
				return true
			}
		}
		return false
	case []interface{}:
		existingSlice, existingOK := existing.([]interface{})
		desiredSlice, desiredOK := desired.([]interface{})
		if !existingOK || !desiredOK || len(existingSlice) != len(desiredSlice) || len(selectedValue) != len(desiredSlice) {
			return !equality.Semantic.DeepEqual(existing, desired)
		}
		for i, childSelection := range selectedValue {
			if selectedOverrideValuesDiffer(existingSlice[i], desiredSlice[i], childSelection) {
				return true
			}
		}
		return false
	default:
		return !equality.Semantic.DeepEqual(existing, desired)
	}
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

	// Read the request and actual serving deployment separately.
	statusResult, err := r.readServingStatus(ctx, md, upstream)
	if err != nil {
		return fmt.Errorf("failed to resolve serving status: %w", err)
	}

	// Update ModelDeployment status
	md.Status.Phase = statusResult.Phase
	md.Status.Message = statusResult.Message
	if statusResult.Message != "" {
		md.Status.Message = statusResult.Message
	} else if statusResult.Phase == airunwayv1alpha1.DeploymentPhaseRunning {
		// The translator reports no message for a healthy DynamoGraphDeployment;
		// replace the stale "waiting for pods" message so status reflects Running.
		md.Status.Message = "DynamoGraphDeployment created, pods are ready"
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

	// Persist exact generated workload identity before initiating cleanup.
	p := ensureProviderStatus(md)
	request, err := r.findRequest(ctx, md)
	if err != nil {
		return r.cleanupRetryResult(ctx, md)
	}
	if request != nil {
		previous := p.WorkloadRef
		p.RequestRef = resourceReference(request)
		if _, err := r.resolveGeneratedDGD(ctx, md, request); err != nil {
			md.Status.Message = err.Error()
			_ = r.Status().Update(ctx, md)
			return r.cleanupRetryResult(ctx, md)
		}
		if previous == nil && p.WorkloadRef != nil {
			if err := r.Status().Update(ctx, md); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}
	if p.RequestRef != nil || request != nil {
		pending, err := r.deleteIntentResource(ctx, md, request)
		if err != nil {
			md.Status.Message = err.Error()
			_ = r.Status().Update(ctx, md)
			return r.cleanupRetryResult(ctx, md)
		}
		if pending {
			return r.cleanupRetryResult(ctx, md)
		}
	} else if p.ResourceKind == DynamoGraphDeploymentRequestKind && p.WorkloadRef == nil {
		md.Status.Message = "Dynamo request is missing and no workload UID was recorded; cleanup requires operator verification"
		_ = r.Status().Update(ctx, md)
		return r.cleanupRetryResult(ctx, md)
	}
	// Direct deployments retain their existing API representation on deletion.
	directName := md.Name
	var directRef *airunwayv1alpha1.ProviderResourceReference
	if p.RequestRef == nil && p.WorkloadRef != nil {
		directName = p.WorkloadRef.Name
		directRef = p.WorkloadRef
	}
	dgd, err := r.findDGD(ctx, md.Namespace, directName, directRef)
	if err != nil {
		return r.cleanupRetryResult(ctx, md)
	}
	if dgd != nil && verifyDynamoOwnership(dgd, md.UID) == nil {
		if p.RequestRef == nil && p.WorkloadRef != nil && p.WorkloadRef.UID != "" && p.WorkloadRef.UID != string(dgd.GetUID()) {
			return r.cleanupRetryResult(ctx, md)
		}
		if dgd.GetDeletionTimestamp() == nil {
			if err := r.deleteWithIdentityPreconditions(ctx, dgd); err != nil && !errors.IsNotFound(err) {
				return r.cleanupRetryResult(ctx, md)
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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

func (r *DynamoProviderReconciler) cleanupRetryResult(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
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
	for _, version := range []string{dynamoBetaVersion, DynamoAPIVersion} {
		if _, err := mapper.RESTMapping(schema.GroupKind{Group: DynamoAPIGroup, Kind: DynamoGraphDeploymentKind}, version); err != nil {
			continue
		}
		builder = builder.Watches(newDynamoResource(version, DynamoGraphDeploymentKind, "", ""), handler.EnqueueRequestsFromMapFunc(r.mapDynamoWorkload))
		// Watching both served views creates duplicate events for the same UID.
		// One preferred watch observes all storage changes; polling covers late CRDs.
		break
	}
	if _, err := mapper.RESTMapping(
		schema.GroupKind{Group: DynamoAPIGroup, Kind: DynamoGraphDeploymentRequestKind},
		DynamoGraphDeploymentRequestAPIVersion,
	); err == nil {
		mgr.GetLogger().Info("DynamoGraphDeploymentRequest CRD detected, enabling event-driven watch")
		builder = builder.Watches(
			&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", DynamoAPIGroup, DynamoGraphDeploymentRequestAPIVersion),
				"kind":       DynamoGraphDeploymentRequestKind,
			}},
			handler.EnqueueRequestForOwner(
				mgr.GetScheme(),
				mgr.GetRESTMapper(),
				&airunwayv1alpha1.ModelDeployment{},
				handler.OnlyControllerOwner(),
			),
		)
	}

	return builder.
		Named("dynamo-provider").
		Complete(r)
}
