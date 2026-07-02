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

package vllm

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
	ProviderName = "vllm"

	// imageVerificationNotImplementedMessage records the current verification boundary for Direct vLLM images.
	imageVerificationNotImplementedMessage = "Image attestation/signature verification is not implemented by this controller"

	// officialVLLMImageRepository is the upstream vLLM OpenAI-compatible image repository.
	officialVLLMImageRepository = "vllm/vllm-openai"

	// recipe provenance annotations added by the API resolver.
	recipeGeneratedByAnnotation = "airunway.ai/generated-by"
	recipeGeneratedByValue      = "vllm-recipe-resolver"
	recipeAnnotationPrefix      = "airunway.ai/recipe."

	// FinalizerName is the finalizer used by this controller
	FinalizerName = "airunway.ai/vllm-provider"

	// FieldManager is the server-side apply field manager name
	FieldManager = "vllm-provider"

	// RequeueInterval is the default requeue interval for periodic reconciliation
	RequeueInterval = 30 * time.Second

	// FinalizerTimeout is the timeout for finalizer cleanup
	FinalizerTimeout = 5 * time.Minute
)

var (
	deploymentGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	serviceGVK    = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}
)

// VLLMProviderReconciler reconciles ModelDeployment resources for the vLLM provider
type VLLMProviderReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Transformer      *Transformer
	StatusTranslator *StatusTranslator
	ImageResolver    ImageResolver
}

// NewVLLMProviderReconciler creates a new vLLM provider reconciler
func NewVLLMProviderReconciler(c client.Client, scheme *runtime.Scheme) *VLLMProviderReconciler {
	return &VLLMProviderReconciler{
		Client:           c,
		Scheme:           scheme,
		Transformer:      NewTransformer(),
		StatusTranslator: NewStatusTranslator(),
		ImageResolver:    NewRemoteImageResolver(),
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

// Reconcile handles the reconciliation loop for ModelDeployments assigned to the vLLM provider
func (r *VLLMProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	logger.Info("Reconciling ModelDeployment for vLLM provider", "name", md.Name, "namespace", md.Namespace)

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
	r.setCondition(&md, airunwayv1alpha1.ConditionTypeProviderCompatible, metav1.ConditionTrue, "CompatibilityVerified", "Configuration compatible with vLLM")
	if err := r.setImageResolutionStatus(ctx, &md); err != nil {
		logger.Error(err, "Failed to resolve vLLM image", "name", md.Name)
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = err.Error()
		if updateErr := r.Status().Update(ctx, &md); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, err
	}

	// Transform ModelDeployment to Deployments + Services
	resources, err := r.Transformer.Transform(ctx, &md)
	if err != nil {
		logger.Error(err, "Failed to transform ModelDeployment", "name", md.Name)
		r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, "TransformFailed", err.Error())
		md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
		md.Status.Message = fmt.Sprintf("Failed to generate vLLM resources: %s", err.Error())
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
			reason := "CreateFailed"
			if isResourceConflict(err) {
				reason = "ResourceConflict"
				r.setCondition(&md, airunwayv1alpha1.ConditionTypeReady, metav1.ConditionFalse, "ResourceConflict", err.Error())
			}
			r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionFalse, reason, err.Error())
			md.Status.Phase = airunwayv1alpha1.DeploymentPhaseFailed
			md.Status.Message = fmt.Sprintf("Failed to create/update resource %s: %s", resource.GetName(), err.Error())
			return ctrl.Result{}, r.Status().Update(ctx, &md)
		}
	}

	r.setCondition(&md, airunwayv1alpha1.ConditionTypeResourceCreated, metav1.ConditionTrue, "ResourceCreated", "Deployments and Services created successfully")

	// Update provider status — use the primary Deployment (resources[0]) for tracking
	if len(resources) > 0 {
		md.Status.Provider.ResourceName = resources[0].GetName()
		md.Status.Provider.ResourceKind = resources[0].GetKind()
	}

	// Sync status from the primary Deployment
	statusSynced := true
	if len(resources) > 0 {
		if err := r.syncStatus(ctx, &md, resources[0]); err != nil {
			logger.Error(err, "Failed to sync status", "name", md.Name)
			statusSynced = false
		}
	}

	// Set phase to Deploying if not already Running or Failed. Skip this when the
	// status sync failed: syncStatus is what promotes a deployment to Running, so
	// a transient API error reading the upstream Deployment must not downgrade a
	// previously-Running deployment back to Deploying on this pass.
	if statusSynced &&
		md.Status.Phase != airunwayv1alpha1.DeploymentPhaseRunning &&
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

// validateCompatibility checks if the ModelDeployment configuration is compatible with vLLM
func (r *VLLMProviderReconciler) validateCompatibility(md *airunwayv1alpha1.ModelDeployment) error {
	// vLLM only supports vLLM
	if md.ResolvedEngineType() != airunwayv1alpha1.EngineTypeVLLM {
		return fmt.Errorf("vLLM provider only supports vllm engine, got %s", md.ResolvedEngineType())
	}

	// Direct vLLM advertises aggregated serving only (see GetProviderConfigSpec).
	// Reject disaggregated here so the provider does not accept a mode it does
	// not claim to support; the internal prefill/decode rendering remains
	// experimental and unadvertised.
	if md.Spec.Serving != nil && md.Spec.Serving.Mode == airunwayv1alpha1.ServingModeDisaggregated {
		return fmt.Errorf("vLLM provider does not support disaggregated serving mode; use spec.serving.mode: aggregated")
	}

	// Aggregated mode: require top-level GPU
	if md.Spec.Resources == nil || md.Spec.Resources.GPU == nil || md.Spec.Resources.GPU.Count == 0 {
		return fmt.Errorf("vLLM provider requires GPU resources (spec.resources.gpu.count > 0)")
	}

	return nil
}

// resourceConflictError is returned when a resource exists but is not managed by this ModelDeployment
type resourceConflictError struct {
	namespace string
	name      string
	owner     string
}

func (e *resourceConflictError) Error() string {
	if e.owner != "" {
		return fmt.Sprintf("resource %s/%s exists but is not managed by this ModelDeployment (owned by %s)", e.namespace, e.name, e.owner)
	}
	return fmt.Sprintf("resource %s/%s exists but is not managed by this ModelDeployment (no owner references)", e.namespace, e.name)
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
	return &resourceConflictError{
		namespace: existing.GetNamespace(),
		name:      existing.GetName(),
		owner:     describeOwnerReferences(existing.GetOwnerReferences()),
	}
}

// describeOwnerReferences renders a human-readable list of the owners of a
// resource so an ownership-conflict error names who actually holds it.
func describeOwnerReferences(refs []metav1.OwnerReference) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, fmt.Sprintf("%s/%s (uid %s)", ref.Kind, ref.Name, ref.UID))
	}
	return strings.Join(parts, ", ")
}

// createOrUpdateResource creates or updates an unstructured resource using server-side apply.
// Server-side apply avoids resourceVersion conflicts that occur when Kubernetes defaults
// fields between our Get and Update calls.
func (r *VLLMProviderReconciler) createOrUpdateResource(ctx context.Context, resource *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) error {
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
	return r.Patch(ctx, resource, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership)
}

// syncStatus fetches the primary Deployment and syncs its status to the ModelDeployment
func (r *VLLMProviderReconciler) syncStatus(ctx context.Context, md *airunwayv1alpha1.ModelDeployment, desired *unstructured.Unstructured) error {
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
func (r *VLLMProviderReconciler) handleDeletion(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) (ctrl.Result, error) {
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

		// The Deployment still exists. Enforce the finalizer timeout regardless of
		// why: a Deployment that is itself stuck Terminating (its own finalizers or
		// PDBs) means r.Delete returns nil while the object never disappears, so the
		// timeout must be checked here — not only on a Delete error — or the
		// ModelDeployment would requeue forever.
		if time.Since(md.DeletionTimestamp.Time) > FinalizerTimeout {
			logger.Info("Finalizer timeout reached, removing finalizer without waiting for deletion", "name", primaryName)
			controllerutil.RemoveFinalizer(md, FinalizerName)
			return ctrl.Result{}, r.Update(ctx, md)
		}

		logger.Info("Deleting primary Deployment", "name", primaryName)
		if err := r.Delete(ctx, deploy); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Failed to delete Deployment")
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

// setImageResolutionStatus records image resolution and provenance details for Direct vLLM deployments.
//
// This resolves tag-based image references to immutable digests. The built-in default
// nightly image must resolve successfully because generated pods use the digest-pinned
// result. User-specified images are preserved in generated pods by default, so their
// resolution failures are surfaced in status but do not fail reconciliation. The
// controller does not currently perform image attestation or signature verification,
// so verification remains Unknown and ImageStatus.Verified is intentionally left false.
func (r *VLLMProviderReconciler) setImageResolutionStatus(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) error {
	imageRef := md.Spec.ImageOverride()
	usingProviderDefault := imageRef == ""
	if usingProviderDefault {
		imageRef = DefaultVLLMImage
	}

	repository, tag, digest := parseImageReference(imageRef)
	source := directVLLMImageSource(repository, tag, imageRef)
	status := &airunwayv1alpha1.ImageStatus{
		Requested:           imageRef,
		Repository:          repository,
		Tag:                 tag,
		Digest:              digest,
		Source:              source,
		InNightly:           source == "nightly",
		VerificationMessage: imageVerificationNotImplementedMessage,
	}

	if shouldResolveImage(tag, digest) {
		if reuseResolvedImageStatus(status, md.Status.Image, imageRef) {
			message := fmt.Sprintf("Reused resolved Direct vLLM image %q to %q from %s selection.", imageRef, status.Resolved, source)
			message = r.appendRecipeProvenanceCondition(md, message)
			status.Message = message
			md.Status.Image = status

			r.setUnsupportedImageCondition(md, source)
			r.setCondition(md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionTrue, "ImageResolutionReused", message)
			r.setCondition(md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented", imageVerificationNotImplementedMessage)
			return nil
		}

		resolver := r.ImageResolver
		if resolver == nil {
			resolver = NewRemoteImageResolver()
		}

		resolved, err := resolver.Resolve(ctx, imageRef)
		if err == nil && (resolved == nil || strings.TrimSpace(resolved.Resolved) == "" || strings.TrimSpace(resolved.Digest) == "") {
			err = fmt.Errorf("resolver returned empty digest result")
		}
		if err != nil {
			message := fmt.Sprintf("Failed to resolve Direct vLLM image %q from %s selection: %s.", imageRef, source, err.Error())
			if !usingProviderDefault {
				message = fmt.Sprintf("%s Continuing with the user-specified image reference.", message)
			}
			message = r.appendRecipeProvenanceCondition(md, message)
			status.Message = message
			md.Status.Image = status

			r.setUnsupportedImageCondition(md, source)
			r.setCondition(md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionFalse, "ImageResolutionFailed", message)
			r.setCondition(md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented", imageVerificationNotImplementedMessage)

			if usingProviderDefault {
				return fmt.Errorf("failed to resolve default vLLM image %q: %w", imageRef, err)
			}
			return nil
		}

		applyResolvedImage(status, resolved)
	} else {
		status.Resolved = imageRef
	}

	message := fmt.Sprintf("Resolved Direct vLLM image %q", imageRef)
	if status.Resolved != "" && status.Resolved != imageRef {
		message = fmt.Sprintf("%s to %q", message, status.Resolved)
	}
	message = fmt.Sprintf("%s from %s selection.", message, source)
	message = r.appendRecipeProvenanceCondition(md, message)
	status.Message = message
	md.Status.Image = status

	r.setUnsupportedImageCondition(md, source)
	r.setCondition(md, airunwayv1alpha1.ConditionTypeImageResolved, metav1.ConditionTrue, "ImageResolved", message)
	r.setCondition(md, airunwayv1alpha1.ConditionTypeImageVerified, metav1.ConditionUnknown, "VerificationNotImplemented", imageVerificationNotImplementedMessage)
	return nil
}

func reuseResolvedImageStatus(status, current *airunwayv1alpha1.ImageStatus, requested string) bool {
	if current == nil {
		return false
	}
	if strings.TrimSpace(current.Requested) != strings.TrimSpace(requested) {
		return false
	}
	if strings.TrimSpace(current.Resolved) == "" || strings.TrimSpace(current.Digest) == "" {
		return false
	}

	status.Resolved = current.Resolved
	status.Digest = current.Digest
	status.CreatedAt = current.CreatedAt
	status.Revision = current.Revision
	status.Age = current.Age
	return true
}

func shouldResolveImage(tag, digest string) bool {
	return strings.TrimSpace(tag) != "" && strings.TrimSpace(digest) == ""
}

func applyResolvedImage(status *airunwayv1alpha1.ImageStatus, resolved *ResolvedImage) {
	if resolved == nil {
		return
	}
	if resolved.Requested != "" {
		status.Requested = resolved.Requested
	}
	if resolved.Resolved != "" {
		status.Resolved = resolved.Resolved
	}
	if resolved.Repository != "" {
		status.Repository = resolved.Repository
	}
	if resolved.Tag != "" {
		status.Tag = resolved.Tag
	}
	if resolved.Digest != "" {
		status.Digest = resolved.Digest
	}
	status.CreatedAt = strings.TrimSpace(resolved.CreatedAt)
	status.Revision = strings.TrimSpace(resolved.Revision)
	status.Age = imageAge(status.CreatedAt, time.Now())
}

func imageAge(createdAt string, now time.Time) string {
	createdAt = strings.TrimSpace(createdAt)
	if createdAt == "" {
		return ""
	}
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return ""
	}
	duration := now.UTC().Sub(created.UTC())
	if duration < 0 {
		duration = 0
	}
	days := int(duration.Hours() / 24)
	if days > 0 {
		return fmt.Sprintf("%dd", days)
	}
	hours := int(duration.Hours())
	if hours > 0 {
		return fmt.Sprintf("%dh", hours)
	}
	minutes := int(duration.Minutes())
	if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return "0m"
}

func (r *VLLMProviderReconciler) appendRecipeProvenanceCondition(md *airunwayv1alpha1.ModelDeployment, message string) string {
	if provenance := recipeProvenanceSummary(md.Annotations); provenance != "" {
		message = fmt.Sprintf("%s Recipe provenance: %s.", message, provenance)
		r.setCondition(md, airunwayv1alpha1.ConditionTypeRecipeResolved, metav1.ConditionTrue, "RecipeProvenanceResolved", provenance)
	}
	return message
}

func (r *VLLMProviderReconciler) setUnsupportedImageCondition(md *airunwayv1alpha1.ModelDeployment, source string) {
	if source == "custom" {
		r.setCondition(md, airunwayv1alpha1.ConditionTypeUnsupportedImage, metav1.ConditionTrue, "CustomImage", "User-specified custom vLLM image is not verified as a supported provider image")
		return
	}
	r.setCondition(md, airunwayv1alpha1.ConditionTypeUnsupportedImage, metav1.ConditionFalse, "SupportedImage", "Selected vLLM image is supported by provider image policy")
}

func parseImageReference(imageRef string) (repository, tag, digest string) {
	ref := strings.TrimSpace(imageRef)
	if ref == "" {
		return "", "", ""
	}

	nameAndTag := ref
	if digestIndex := strings.Index(ref, "@"); digestIndex >= 0 {
		nameAndTag = ref[:digestIndex]
		digest = ref[digestIndex+1:]
	}

	repository = nameAndTag
	lastSlash := strings.LastIndex(nameAndTag, "/")
	lastColon := strings.LastIndex(nameAndTag, ":")
	if lastColon > lastSlash {
		repository = nameAndTag[:lastColon]
		tag = nameAndTag[lastColon+1:]
	}

	return repository, tag, digest
}

func directVLLMImageSource(repository, tag, imageRef string) string {
	normalizedRepository := normalizeImageRepository(repository)
	lowerTag := strings.ToLower(tag)
	if strings.Contains(lowerTag, "launch") {
		return "launch"
	}
	if imageRef == DefaultVLLMImage || (normalizedRepository == officialVLLMImageRepository && strings.Contains(lowerTag, "nightly")) {
		return "nightly"
	}
	if normalizedRepository == officialVLLMImageRepository && tag == "latest" {
		return "stable"
	}
	return "custom"
}

func normalizeImageRepository(repository string) string {
	repository = strings.TrimSpace(repository)
	repository = strings.TrimPrefix(repository, "docker.io/")
	repository = strings.TrimPrefix(repository, "index.docker.io/")
	return repository
}

func recipeProvenanceSummary(annotations map[string]string) string {
	if len(annotations) == 0 {
		return ""
	}

	hasRecipeProvenance := annotations[recipeGeneratedByAnnotation] == recipeGeneratedByValue
	if !hasRecipeProvenance {
		for key := range annotations {
			if strings.HasPrefix(key, recipeAnnotationPrefix) {
				hasRecipeProvenance = true
				break
			}
		}
	}
	if !hasRecipeProvenance {
		return ""
	}

	fields := []struct {
		annotation string
		label      string
	}{
		{recipeGeneratedByAnnotation, "generated-by"},
		{recipeAnnotationPrefix + "source", "source"},
		{recipeAnnotationPrefix + "id", "id"},
		{recipeAnnotationPrefix + "strategy", "strategy"},
		{recipeAnnotationPrefix + "hardware", "hardware"},
		{recipeAnnotationPrefix + "variant", "variant"},
		{recipeAnnotationPrefix + "precision", "precision"},
		{recipeAnnotationPrefix + "revision", "revision"},
		{recipeAnnotationPrefix + "features", "features"},
	}

	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		if value := strings.TrimSpace(annotations[field.annotation]); value != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", field.label, value))
		}
	}

	return strings.Join(parts, ", ")
}

// setCondition updates a condition on the ModelDeployment
func (r *VLLMProviderReconciler) setCondition(md *airunwayv1alpha1.ModelDeployment, conditionType string, status metav1.ConditionStatus, reason, message string) {
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
func (r *VLLMProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&airunwayv1alpha1.ModelDeployment{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			md, ok := obj.(*airunwayv1alpha1.ModelDeployment)
			if !ok {
				return false
			}
			// Process if provider is vllm OR if being deleted (to handle finalizer)
			if md.Status.Provider != nil && md.Status.Provider.Name == ProviderName {
				return true
			}
			// Also process if spec explicitly requests vllm
			if md.Spec.Provider != nil && md.Spec.Provider.Name == ProviderName {
				return true
			}
			// Process if we have our finalizer (for deletion handling)
			return controllerutil.ContainsFinalizer(md, FinalizerName)
		})).
		Named("vllm-provider").
		Complete(r)
}
