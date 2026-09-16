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
	"github.com/ai-runway/airunway/controller/pkg/storage"
)

const (
	// HuggingFaceTokenSecretName is the namespace-level Secret name required by Dynamo DGDR workloads.
	HuggingFaceTokenSecretName = "hf-token-secret"
	huggingFaceTokenSecretKey  = "HF_TOKEN"
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

// dgdrIntentImmutableError reports a ModelDeployment spec generation that cannot
// be applied to an existing DGDR after Dynamo has started profiling it.
type dgdrIntentImmutableError struct {
	existingGeneration string
	desiredGeneration  string
}

func (e *dgdrIntentImmutableError) Error() string {
	return fmt.Sprintf(
		"DynamoGraphDeploymentRequest intent is immutable after creation "+
			"(existing ModelDeployment generation %s, desired generation %s); "+
			"delete and recreate the ModelDeployment to apply the new intent",
		e.existingGeneration, e.desiredGeneration,
	)
}

func isDGDRIntentImmutable(err error) bool {
	var immutableErr *dgdrIntentImmutableError
	return stderrors.As(err, &immutableErr)
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
	APIReader        client.Reader
	Scheme           *runtime.Scheme
	Transformer      *Transformer
	StatusTranslator *StatusTranslator
	DownloadJobImage string
}

// NewDynamoProviderReconciler creates a new Dynamo provider reconciler
func NewDynamoProviderReconciler(
	kubeClient client.Client,
	scheme *runtime.Scheme,
	downloadJobImage string,
	apiReaders ...client.Reader,
) *DynamoProviderReconciler {
	if downloadJobImage == "" {
		downloadJobImage = storage.DefaultDownloadJobImage
	}
	apiReader := client.Reader(kubeClient)
	if len(apiReaders) > 0 && apiReaders[0] != nil {
		apiReader = apiReaders[0]
	}
	return &DynamoProviderReconciler{
		Client:           kubeClient,
		APIReader:        apiReader,
		Scheme:           scheme,
		Transformer:      NewTransformer(),
		StatusTranslator: NewStatusTranslator(),
		DownloadJobImage: downloadJobImage,
	}
}

// ensureDGDRHuggingFaceSecret creates Dynamo's required empty placeholder for
// public models, but never creates or changes a Secret declared by the user.
func (r *DynamoProviderReconciler) ensureDGDRHuggingFaceSecret(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) error {
	key := types.NamespacedName{Name: HuggingFaceTokenSecretName, Namespace: md.Namespace}
	secret := &corev1.Secret{}
	// Bypass the shared cache so this exact-name read does not start a
	// cluster-wide Secret informer that would require list/watch privileges.
	if err := r.APIReader.Get(ctx, key, secret); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" {
		return fmt.Errorf(
			"secret %s/%s declared by spec.secrets.huggingFaceToken does not exist",
			md.Namespace, HuggingFaceTokenSecretName,
		)
	}

	// The empty token is valid for public models and avoids mutating a token that
	// may be managed independently by the dashboard or a cluster administrator.
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HuggingFaceTokenSecretName,
			Namespace: md.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "airunway",
				"airunway.ai/secret-type":      "huggingface-token",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			huggingFaceTokenSecretKey: {},
		},
	}
	if err := r.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=airunway.ai,resources=inferenceproviderconfigs,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=airunway.ai,resources=inferenceproviderconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=dynamographdeployments,verbs=get;list;watch;create;update;patch;delete
//nolint:lll // Kubebuilder RBAC markers cannot be split across lines.
// +kubebuilder:rbac:groups=nvidia.com,resources=dynamographdeploymentrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nvidia.com,resources=dynamographdeploymentrequests/status,verbs=get
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create

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

	// Create or update the DynamoGraphDeployment
	for _, resource := range resources {
		if resource.GetKind() == DynamoGraphDeploymentRequestKind {
			// Public Hugging Face models still need Dynamo's fixed Secret reference
			// to resolve before the profiler and generated serving pods can start.
			if err := r.ensureDGDRHuggingFaceSecret(ctx, &md); err != nil {
				return ctrl.Result{}, fmt.Errorf("ensure Dynamo Hugging Face Secret: %w", err)
			}
		}
		if err := r.createOrUpdateResource(ctx, resource, &md); err != nil {
			logger.Error(err, "Failed to create/update resource", "name", resource.GetName(), "kind", resource.GetKind())
			if isDGDRIntentImmutable(err) {
				// Preserve the serving upstream object but fail the desired generation
				// explicitly so users are not told an unapplied intent is healthy.
				r.setCondition(
					&md, airunwayv1alpha1.ConditionTypeResourceCreated,
					metav1.ConditionFalse, "ImmutableIntent", err.Error(),
				)
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "ImmutableIntent", err.Error())
				md.Status.Endpoint = nil
				md.Status.Replicas = nil
				md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
				md.Status.Message = err.Error()
				return ctrl.Result{}, r.Status().Update(ctx, &md)
			}
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
			md.Status.Message = fmt.Sprintf("Failed to create %s: %s", resource.GetKind(), err.Error())
			if statusErr := r.Status().Update(ctx, &md); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	primaryResource := resources[0]
	r.setCondition(
		&md, airunwayv1alpha1.ConditionTypeResourceCreated,
		metav1.ConditionTrue, "ResourceCreated",
		fmt.Sprintf("%s created successfully", primaryResource.GetKind()),
	)

	// Update provider status
	// Report the primary object Airunway owns; in intent mode the generated DGD
	// remains a Dynamo-managed child discovered through DGDR status and labels.
	md.Status.Provider.ResourceName = primaryResource.GetName()
	md.Status.Provider.ResourceKind = primaryResource.GetKind()

	// Sync status from upstream resource
	if len(resources) > 0 {
		// Discard text from a prior upstream state. syncStatus supplies current
		// upstream detail when available; the fallback below handles no status.
		md.Status.Message = ""
		if err := r.syncStatus(ctx, &md, resources[0]); err != nil {
			logger.Error(err, "Failed to sync status", "name", md.Name)
			// Don't fail the reconciliation, just log the error
		}
	}

	// Set phase to Deploying if not already Running or Failed
	if md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning &&
		md.Status.Phase != airunwayv1alpha1.DeploymentPhaseFailed {
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseDeploying
		if md.Status.Message == "" {
			md.Status.Message = fmt.Sprintf("%s created, waiting for deployment to be ready", primaryResource.GetKind())
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
		return wrapResourceWriteError(r.Create(ctx, resource, strictFieldValidation), false)
	}
	if err != nil {
		return fmt.Errorf("failed to get existing resource: %w", err)
	}

	// Verify ownership before updating
	if err := verifyDynamoOwnership(existing, md.UID); err != nil {
		return err
	}
	if resource.GetKind() == DynamoGraphDeploymentRequestKind {
		// DGDR intent becomes immutable once profiling begins. Compare the source
		// generation marker instead of live specs because the webhook defaults
		// fields such as the profiler image on creation.
		existingGeneration := existing.GetAnnotations()["airunway.ai/model-deployment-generation"]
		desiredGeneration := resource.GetAnnotations()["airunway.ai/model-deployment-generation"]
		if existingGeneration != desiredGeneration {
			return &dgdrIntentImmutableError{
				existingGeneration: existingGeneration,
				desiredGeneration:  desiredGeneration,
			}
		}
		return nil
	}
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
		resource.SetResourceVersion(existing.GetResourceVersion())
		return wrapResourceWriteError(r.Update(ctx, resource, strictFieldValidation), resourceWasOwnedActiveAndServing)
	}

	return nil
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
		// Replace stale progress text when either upstream kind reaches its healthy
		// terminal state and does not provide a more specific message.
		md.Status.Message = fmt.Sprintf("%s is ready", upstream.GetKind())
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

	// DGDR-created DGDs are independent resources, so collect their identity and
	// explicitly delete both upstream objects before releasing managed storage.
	dgdNames := map[string]struct{}{md.Name: {}}
	upstreamDeletionPending := false
	var cleanupErrs []error

	dgdr := &unstructured.Unstructured{}
	dgdr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   DynamoAPIGroup,
		Version: DynamoGraphDeploymentRequestAPIVersion,
		Kind:    DynamoGraphDeploymentRequestKind,
	})
	if err := r.Get(ctx, types.NamespacedName{Name: md.Name, Namespace: md.Namespace}, dgdr); err == nil {
		if ownershipErr := verifyDynamoOwnership(dgdr, md.UID); ownershipErr != nil {
			logger.Info(
				"DynamoGraphDeploymentRequest is not managed by this ModelDeployment, skipping deletion",
				"name", dgdr.GetName(),
			)
		} else {
			// Capture status.dgdName before deleting the DGDR because Dynamo does not
			// retain an owner reference from the generated DGD back to the request.
			if dgdName, found, _ := unstructured.NestedString(dgdr.Object, "status", "dgdName"); found && dgdName != "" {
				dgdNames[dgdName] = struct{}{}
			}
			if dgdr.GetDeletionTimestamp() == nil {
				logger.Info("Deleting DynamoGraphDeploymentRequest", "name", dgdr.GetName())
				if deleteErr := r.Delete(ctx, dgdr); deleteErr != nil && !upstreamResourceUnavailable(deleteErr) {
					cleanupErrs = append(cleanupErrs, deleteErr)
				}
			}
			upstreamDeletionPending = true
		}
	} else if !upstreamResourceUnavailable(err) {
		cleanupErrs = append(cleanupErrs, err)
	}

	linkedNames, err := r.findLinkedDGDNames(ctx, md)
	if err != nil && !upstreamResourceUnavailable(err) {
		cleanupErrs = append(cleanupErrs, err)
	}
	for _, name := range linkedNames {
		dgdNames[name] = struct{}{}
	}

	for dgdName := range dgdNames {
		dgd := &unstructured.Unstructured{}
		dgd.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   DynamoAPIGroup,
			Version: DynamoAPIVersion,
			Kind:    DynamoGraphDeploymentKind,
		})
		if err := r.Get(ctx, types.NamespacedName{Name: dgdName, Namespace: md.Namespace}, dgd); err != nil {
			if !upstreamResourceUnavailable(err) {
				cleanupErrs = append(cleanupErrs, err)
			}
			continue
		}
		if !dgdBelongsToModelDeployment(dgd, md) {
			logger.Info("DynamoGraphDeployment is not linked to this ModelDeployment, skipping deletion", "name", dgdName)
			continue
		}
		if dgd.GetDeletionTimestamp() == nil {
			logger.Info("Deleting DynamoGraphDeployment", "name", dgdName)
			if deleteErr := r.Delete(ctx, dgd); deleteErr != nil && !upstreamResourceUnavailable(deleteErr) {
				cleanupErrs = append(cleanupErrs, deleteErr)
			}
		}
		upstreamDeletionPending = true
	}

	if len(cleanupErrs) > 0 || upstreamDeletionPending {
		if time.Since(md.DeletionTimestamp.Time) > FinalizerTimeout {
			logger.Info("Finalizer timeout reached, removing finalizer without complete upstream cleanup")
			controllerutil.RemoveFinalizer(md, FinalizerName)
			return ctrl.Result{}, r.Update(ctx, md)
		}
		if len(cleanupErrs) > 0 {
			logger.Error(stderrors.Join(cleanupErrs...), "Failed to clean up Dynamo upstream resources")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// The upstream resource is already gone or its CRD is no longer installed,
	// so continue with managed Jobs/PVC cleanup and remove the finalizer.
	cleanupErrs = nil
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

// findLinkedDGDNames locates generated DGDs by Dynamo's stable DGDR label; the
// pinned same-name DGD is handled separately by the deletion caller.
func (r *DynamoProviderReconciler) findLinkedDGDNames(
	ctx context.Context,
	md *airunwayv1alpha1.ModelDeployment,
) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   DynamoAPIGroup,
		Version: DynamoAPIVersion,
		Kind:    DynamoGraphDeploymentKind + "List",
	})
	if err := r.List(ctx, list,
		client.InNamespace(md.Namespace),
		client.MatchingLabels{"dgdr.nvidia.com/name": md.Name},
	); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].GetName())
	}
	return names, nil
}

// dgdBelongsToModelDeployment accepts either direct DGD ownership or the labels
// propagated through the DGDR override and added by Dynamo itself.
func dgdBelongsToModelDeployment(dgd *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) bool {
	for _, ref := range dgd.GetOwnerReferences() {
		if ref.UID == md.UID {
			return true
		}
	}
	labels := dgd.GetLabels()
	return labels[airunwayv1alpha1.LabelModelDeployment] == md.Name ||
		labels["dgdr.nvidia.com/name"] == md.Name
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

// mapDynamoResourceToModelDeployments maps owned DGDRs and both direct and
// generated DGDs back to the ModelDeployment reconciliation key.
func mapDynamoResourceToModelDeployments(_ context.Context, obj client.Object) []reconcile.Request {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == airunwayv1alpha1.GroupVersion.String() && ref.Kind == "ModelDeployment" {
			return []reconcile.Request{{
				NamespacedName: types.NamespacedName{Name: ref.Name, Namespace: obj.GetNamespace()},
			}}
		}
	}

	labels := obj.GetLabels()
	deploymentName := labels[airunwayv1alpha1.LabelModelDeployment]
	if deploymentName == "" {
		// Dynamo adds this label to generated DGDs even though it does not add an
		// owner reference back to the DGDR.
		deploymentName = labels["dgdr.nvidia.com/name"]
	}
	if deploymentName == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: deploymentName, Namespace: obj.GetNamespace()},
	}}
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
	if _, err := mapper.RESTMapping(
		schema.GroupKind{Group: DynamoAPIGroup, Kind: DynamoGraphDeploymentRequestKind},
		DynamoGraphDeploymentRequestAPIVersion,
	); err == nil {
		logger := mgr.GetLogger()
		logger.Info("DynamoGraphDeploymentRequest CRD detected, enabling event-driven watch")
		builder = builder.Watches(
			&unstructured.Unstructured{Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", DynamoAPIGroup, DynamoGraphDeploymentRequestAPIVersion),
				"kind":       DynamoGraphDeploymentRequestKind,
			}},
			handler.EnqueueRequestsFromMapFunc(mapDynamoResourceToModelDeployments),
		)
	}
	if _, err := mapper.RESTMapping(schema.GroupKind{Group: DynamoAPIGroup, Kind: DynamoGraphDeploymentKind}, DynamoAPIVersion); err == nil {
		logger := mgr.GetLogger()
		logger.Info("DynamoGraphDeployment CRD detected, enabling event-driven watch")
		builder = builder.Watches(
			&unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", DynamoAPIGroup, DynamoAPIVersion),
				"kind":       DynamoGraphDeploymentKind,
			}},
			handler.EnqueueRequestsFromMapFunc(mapDynamoResourceToModelDeployments),
		)
	}

	return builder.
		Named("dynamo-provider").
		Complete(r)
}
