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

package v1alpha1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"github.com/ai-runway/airunway/controller/internal/validation"
)

const (
	// MaxGPUCount is the maximum GPU count allowed per component
	MaxGPUCount = 64
	// MaxReplicas is the maximum replica count allowed per component
	MaxReplicas = 32
	// MaxCPU is the maximum CPU request allowed (in cores)
	MaxCPU = "512"
	// MaxMemory is the maximum memory request allowed
	MaxMemory = "4Ti"
)

// nolint:unused
// log is for logging in this package.
var modeldeploymentlog = logf.Log.WithName("modeldeployment-resource")

// SetupModelDeploymentWebhookWithManager registers the webhook for ModelDeployment in the manager.
func SetupModelDeploymentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &airunwayv1alpha1.ModelDeployment{}).
		WithValidator(&ModelDeploymentCustomValidator{
			// Reader is the cached client — every admission request used to
			// hit the API server via mgr.GetAPIReader(), which on a busy
			// cluster turns admission into a synchronous round-trip and a
			// load multiplier on apiserver. The reconciler already watches
			// InferenceProviderConfig, so the cache is warm by the time
			// admission starts serving traffic.
			Reader: mgr.GetClient(),
			// APIReader is a non-cached fallback used only when the cached
			// Reader returns NotFound, to disambiguate "truly absent" from
			// "informer hasn't yet observed a freshly-created provider".
			// In steady state it is never called.
			APIReader: mgr.GetAPIReader(),
		}).
		WithDefaulter(&ModelDeploymentCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-airunway-ai-v1alpha1-modeldeployment,mutating=true,failurePolicy=fail,sideEffects=None,groups=airunway.ai,resources=modeldeployments,verbs=create;update,versions=v1alpha1,name=mmodeldeployment-v1alpha1.kb.io,admissionReviewVersions=v1

// ModelDeploymentCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind ModelDeployment when those are created or updated.
type ModelDeploymentCustomDefaulter struct{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind ModelDeployment.
func (d *ModelDeploymentCustomDefaulter) Default(_ context.Context, obj *airunwayv1alpha1.ModelDeployment) error {
	modeldeploymentlog.Info("Defaulting for ModelDeployment", "name", obj.GetName())

	spec := &obj.Spec

	// Default model source to huggingface
	if spec.Model.Source == "" {
		spec.Model.Source = airunwayv1alpha1.ModelSourceHuggingFace
	}

	// Default serving mode to aggregated
	if spec.Serving == nil {
		spec.Serving = &airunwayv1alpha1.ServingSpec{
			Mode: airunwayv1alpha1.ServingModeAggregated,
		}
	} else if spec.Serving.Mode == "" {
		spec.Serving.Mode = airunwayv1alpha1.ServingModeAggregated
	}

	// Default scaling replicas to 1 for aggregated mode
	if spec.Serving.Mode == airunwayv1alpha1.ServingModeAggregated {
		if spec.Scaling == nil {
			spec.Scaling = &airunwayv1alpha1.ScalingSpec{
				Replicas: 1,
			}
		} else if spec.Scaling.Replicas == 0 {
			// Allow 0 for scale-to-zero, but default to 1 if not explicitly set
			// This is handled by the kubebuilder default tag
		}
	}

	// Default GPU to 1 in aggregated mode when resources are unspecified
	// and an engine type is explicitly set. Skip the default when:
	// - engine is not specified (auto-selection will determine GPU requirements)
	// - engine is llamacpp (supports CPU-only inference)
	// - the user provided a custom image (may not need GPU)
	if spec.Serving.Mode == airunwayv1alpha1.ServingModeAggregated && spec.Resources == nil &&
		spec.Engine.Type != "" && spec.Engine.Type != airunwayv1alpha1.EngineTypeLlamaCpp &&
		spec.Image == "" {
		spec.Resources = &airunwayv1alpha1.ResourceSpec{
			GPU: &airunwayv1alpha1.GPUSpec{
				Count: 1,
				Type:  "nvidia.com/gpu",
			},
		}
	}

	// Default GPU type if GPU is specified but type is empty
	if spec.Resources != nil && spec.Resources.GPU != nil && spec.Resources.GPU.Type == "" {
		spec.Resources.GPU.Type = "nvidia.com/gpu"
	}

	// Default GPU type for disaggregated mode components
	if spec.Scaling != nil {
		if spec.Scaling.Prefill != nil && spec.Scaling.Prefill.GPU != nil && spec.Scaling.Prefill.GPU.Type == "" {
			spec.Scaling.Prefill.GPU.Type = "nvidia.com/gpu"
		}
		if spec.Scaling.Decode != nil && spec.Scaling.Decode.GPU != nil && spec.Scaling.Decode.GPU.Type == "" {
			spec.Scaling.Decode.GPU.Type = "nvidia.com/gpu"
		}
	}

	// Default storage volume fields
	if spec.Model.Storage != nil {
		for i := range spec.Model.Storage.Volumes {
			vol := &spec.Model.Storage.Volumes[i]
			// Default purpose to custom if empty
			if vol.Purpose == "" {
				vol.Purpose = airunwayv1alpha1.VolumePurposeCustom
			}
			// Default mountPath based on purpose
			if vol.MountPath == "" {
				switch vol.Purpose {
				case airunwayv1alpha1.VolumePurposeModelCache:
					vol.MountPath = "/model-cache"
				case airunwayv1alpha1.VolumePurposeCompilationCache:
					vol.MountPath = "/compilation-cache"
				}
			}
			// When size is set (controller-created PVC mode):
			if vol.Size != nil {
				// Default claimName to <md-name>-<volume-name>
				if vol.ClaimName == "" {
					vol.ClaimName = fmt.Sprintf("%s-%s", obj.Name, vol.Name)
				}
				// Default accessMode to ReadWriteMany
				if vol.AccessMode == "" {
					vol.AccessMode = corev1.ReadWriteMany
				}
			}
		}
	}

	return nil
}

// +kubebuilder:webhook:path=/validate-airunway-ai-v1alpha1-modeldeployment,mutating=false,failurePolicy=fail,sideEffects=None,groups=airunway.ai,resources=modeldeployments,verbs=create;update,versions=v1alpha1,name=vmodeldeployment-v1alpha1.kb.io,admissionReviewVersions=v1

// ModelDeploymentCustomValidator struct is responsible for validating the ModelDeployment resource
// when it is created, updated, or deleted.
type ModelDeploymentCustomValidator struct {
	// Reader is used to look up InferenceProviderConfig resources for
	// provider compatibility validation at admission time. In production
	// this is the manager's cached client so admission does not synchronously
	// hit the API server on every request.
	Reader client.Reader

	// APIReader is an optional uncached fallback consulted only when Reader
	// returns NotFound, so we can distinguish a missing provider from an
	// informer cache that has not yet observed a freshly-created one. May be
	// nil in tests; in that case a Reader NotFound is treated as authoritative.
	APIReader client.Reader
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type ModelDeployment.
func (v *ModelDeploymentCustomValidator) ValidateCreate(ctx context.Context, obj *airunwayv1alpha1.ModelDeployment) (admission.Warnings, error) {
	modeldeploymentlog.Info("Validation for ModelDeployment upon creation", "name", obj.GetName())

	var warnings admission.Warnings
	var allErrs field.ErrorList

	// Validate name does not contain dots (derived volume/service names prohibit dots)
	if strings.Contains(obj.Name, ".") {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("metadata", "name"),
			obj.Name,
			"name must not contain dots (dots are invalid in derived Kubernetes volume and service names)",
		))
	}

	// Validate the spec
	specWarnings, specErrs := v.validateSpec(ctx, obj)
	warnings = append(warnings, specWarnings...)
	allErrs = append(allErrs, specErrs...)

	// Check for warnings
	warnings = append(warnings, v.checkWarnings(obj)...)

	if len(allErrs) > 0 {
		return warnings, allErrs.ToAggregate()
	}
	return warnings, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type ModelDeployment.
func (v *ModelDeploymentCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *airunwayv1alpha1.ModelDeployment) (admission.Warnings, error) {
	modeldeploymentlog.Info("Validation for ModelDeployment upon update", "name", newObj.GetName())

	var warnings admission.Warnings
	var allErrs field.ErrorList

	// Validate the spec
	specWarnings, specErrs := v.validateSpec(ctx, newObj)
	warnings = append(warnings, specWarnings...)
	allErrs = append(allErrs, specErrs...)

	// Validate immutable fields (identity fields that trigger delete+recreate)
	allErrs = append(allErrs, v.validateImmutableFields(oldObj, newObj)...)

	// Check for warnings
	warnings = append(warnings, v.checkWarnings(newObj)...)

	if len(allErrs) > 0 {
		return warnings, allErrs.ToAggregate()
	}
	return warnings, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type ModelDeployment.
func (v *ModelDeploymentCustomValidator) ValidateDelete(_ context.Context, obj *airunwayv1alpha1.ModelDeployment) (admission.Warnings, error) {
	modeldeploymentlog.Info("Validation for ModelDeployment upon deletion", "name", obj.GetName())

	// No validation on delete
	return nil, nil
}

// validateSpec validates the ModelDeployment spec
func (v *ModelDeploymentCustomValidator) validateSpec(ctx context.Context, obj *airunwayv1alpha1.ModelDeployment) (admission.Warnings, field.ErrorList) {
	var warnings admission.Warnings
	var allErrs field.ErrorList
	spec := &obj.Spec
	specPath := field.NewPath("spec")

	// Validate image override fields are not conflicting.
	if err := spec.ValidateImageFields(); err != nil {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("engine", "image"),
			spec.Engine.Image,
			err.Error(),
		))
	}

	// Reject a launch flag set in both spec.engine.args and spec.engine.extraArgs
	// at admission, so the conflict fails the apply/patch synchronously instead of
	// being admitted and then surfacing asynchronously as a Failed reconcile. The
	// provider transforms re-check this as a backstop. Provider-agnostic: the
	// provider is frequently auto-selected and unknown at admission time.
	if err := spec.ValidateEngineArgs(); err != nil {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("engine", "extraArgs"),
			spec.Engine.ExtraArgs,
			err.Error(),
		))
	}

	// Validate model.id is required for huggingface source
	if spec.Model.Source == airunwayv1alpha1.ModelSourceHuggingFace || spec.Model.Source == "" {
		if spec.Model.ID == "" {
			allErrs = append(allErrs, field.Required(
				specPath.Child("model", "id"),
				"model.id is required when source is huggingface",
			))
		}
	}

	// Validate engine type if set (empty is allowed - controller will auto-select)
	if spec.Engine.Type != "" {
		// Validation of engine type value is handled by the Enum marker on EngineType
	}

	// Validate provider overrides don't contain dangerous fields
	allErrs = append(allErrs, v.validateOverrides(spec, specPath)...)

	// Resolve serving mode for validation checks below
	servingMode := airunwayv1alpha1.ServingModeAggregated
	if spec.Serving != nil && spec.Serving.Mode != "" {
		servingMode = spec.Serving.Mode
	}

	// Validate provider compatibility when both provider and engine are specified.
	// Uses the cached Reader to avoid a synchronous apiserver round-trip per
	// admission; falls back to the uncached APIReader only when the cache
	// reports NotFound, to absorb the race where a brand-new
	// InferenceProviderConfig hasn't yet propagated to informers.
	//
	// Mocker mode escape hatch: a ModelDeployment annotated with
	// airunway.ai/dynamo-test-backend=mocker targeting the dynamo provider runs
	// the GPU-less python3 -m dynamo.mocker backend, so the provider's GPU
	// capability check must not reject it at admission. This is a test-only path
	// (the dynamo provider re-validates compatibility during reconciliation).
	// The annotation key is kept as a literal here to avoid importing the
	// provider module from the controller webhook (see
	// providers/dynamo/mocker.go AnnotationDynamoTestBackend / DynamoTestBackendMocker).
	isDynamoMocker := obj.Annotations["airunway.ai/dynamo-test-backend"] == "mocker" &&
		spec.Provider != nil && spec.Provider.Name == "dynamo"

	// The Dynamo mocker backend only simulates the vLLM engine. Enforce the
	// vLLM-only constraint at admission so a non-vllm engine + mocker annotation
	// is rejected here rather than admitted and failing later during provider
	// reconciliation (the dynamo provider re-validates this too). An empty engine
	// type is allowed — the provider defaults it to vllm.
	if isDynamoMocker && spec.Engine.Type != "" && spec.Engine.Type != airunwayv1alpha1.EngineTypeVLLM {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("engine", "type"),
			spec.Engine.Type,
			"the dynamo mocker test backend only supports the vllm engine",
		))
	}

	if !isDynamoMocker && spec.Provider != nil && spec.Provider.Name != "" && spec.Engine.Type != "" && v.Reader != nil {
		var providerConfig airunwayv1alpha1.InferenceProviderConfig
		err := v.Reader.Get(ctx, client.ObjectKey{Name: spec.Provider.Name}, &providerConfig)
		if apierrors.IsNotFound(err) && v.APIReader != nil {
			// Cache may be stale for a just-created provider; confirm against
			// the API server before we tell the user the provider doesn't
			// exist. Any error from the fallback is preserved verbatim so
			// the existing switch below classifies it the same way it would
			// have under the old all-APIReader path.
			err = v.APIReader.Get(ctx, client.ObjectKey{Name: spec.Provider.Name}, &providerConfig)
		}
		switch {
		case apierrors.IsNotFound(err):
			// Reject obviously-bogus provider names at admission time so the
			// user gets immediate feedback rather than waiting for reconcile.
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("provider", "name"),
				spec.Provider.Name,
				fmt.Sprintf("InferenceProviderConfig %q not found", spec.Provider.Name),
			))
		case meta.IsNoMatchError(err):
			// CRD is not installed (cluster mid-bootstrap). Skip — the
			// controller will catch this during reconciliation.
		case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
			// Webhook RBAC is misconfigured (e.g. ServiceAccount missing
			// `get` on InferenceProviderConfig). Do NOT silently skip
			// validation — that would mask a serious misconfiguration and
			// disable admission-time enforcement cluster-wide. Surface it
			// as an InternalError so the apiserver rejects admission with
			// an actionable diagnostic.
			allErrs = append(allErrs, field.InternalError(
				specPath.Child("provider", "name"),
				fmt.Errorf("cannot verify provider %q: %w", spec.Provider.Name, err),
			))
		case err != nil:
			// Transient API error (timeout, connection refused, etc.). Do not
			// block admission on infra flakes — log and skip so the controller
			// can re-validate later.
			logf.FromContext(ctx).Info(
				"failed to look up InferenceProviderConfig for webhook validation; skipping provider/engine compatibility check",
				"provider", spec.Provider.Name,
				"error", err.Error(),
			)
			warnings = append(warnings, fmt.Sprintf(
				"could not verify provider %q compatibility at admission time (%v); the controller will re-validate during reconciliation",
				spec.Provider.Name, err,
			))
		case providerConfig.Spec.Capabilities != nil:
			gpuCount := int32(0)
			if spec.Resources != nil && spec.Resources.GPU != nil {
				gpuCount = spec.Resources.GPU.Count
			}
			for _, ce := range validation.CheckProviderCompatibility(
				spec.Provider.Name,
				&providerConfig,
				nil,
				spec.Engine.Type,
				servingMode,
				gpuCount,
			) {
				fp := specPath
				for _, seg := range ce.FieldPath {
					fp = fp.Child(seg)
				}
				allErrs = append(allErrs, field.Invalid(fp, ce.BadValue, ce.Message))
			}
		}
	}

	// Validate disaggregated mode configuration
	if servingMode == airunwayv1alpha1.ServingModeDisaggregated {
		// Cannot specify resources.gpu in disaggregated mode
		if spec.Resources != nil && spec.Resources.GPU != nil && spec.Resources.GPU.Count > 0 {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("resources", "gpu"),
				spec.Resources.GPU,
				"cannot specify both resources.gpu and scaling.prefill/decode in disaggregated mode",
			))
		}

		// Must specify prefill and decode
		if spec.Scaling == nil {
			allErrs = append(allErrs, field.Required(
				specPath.Child("scaling"),
				"disaggregated mode requires scaling configuration",
			))
		} else {
			if spec.Scaling.Prefill == nil {
				allErrs = append(allErrs, field.Required(
					specPath.Child("scaling", "prefill"),
					"disaggregated mode requires scaling.prefill",
				))
			} else if !isDynamoMocker {
				// Mocker mode runs the GPU-less python3 -m dynamo.mocker backend,
				// so a CPU-only disaggregated mocker deployment legitimately omits
				// scaling.prefill.gpu.count. The prefill block itself is still
				// required (above) so the dynamo transformer can build the worker.
				if spec.Scaling.Prefill.GPU == nil || spec.Scaling.Prefill.GPU.Count == 0 {
					allErrs = append(allErrs, field.Required(
						specPath.Child("scaling", "prefill", "gpu", "count"),
						"disaggregated mode requires scaling.prefill.gpu.count > 0",
					))
				}
			}

			if spec.Scaling.Decode == nil {
				allErrs = append(allErrs, field.Required(
					specPath.Child("scaling", "decode"),
					"disaggregated mode requires scaling.decode",
				))
			} else if !isDynamoMocker {
				// See the prefill note above: mocker mode waives the GPU-count
				// requirement while still requiring the decode block.
				if spec.Scaling.Decode.GPU == nil || spec.Scaling.Decode.GPU.Count == 0 {
					allErrs = append(allErrs, field.Required(
						specPath.Child("scaling", "decode", "gpu", "count"),
						"disaggregated mode requires scaling.decode.gpu.count > 0",
					))
				}
			}
		}
	}

	// Validate storage configuration
	allErrs = append(allErrs, v.validateStorage(obj)...)

	// Enforce resource ceilings to prevent runaway resource requests at admission time.
	allErrs = append(allErrs, validateResourceCeilings(spec, specPath)...)

	return warnings, allErrs
}

// validateResourceCeilings enforces the Max* limits on resource and scaling fields.
func validateResourceCeilings(spec *airunwayv1alpha1.ModelDeploymentSpec, specPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if spec.Resources != nil {
		resPath := specPath.Child("resources")
		if spec.Resources.GPU != nil && spec.Resources.GPU.Count > MaxGPUCount {
			allErrs = append(allErrs, field.Invalid(
				resPath.Child("gpu", "count"),
				spec.Resources.GPU.Count,
				fmt.Sprintf("exceeds maximum allowed (%d)", MaxGPUCount),
			))
		}
		allErrs = append(allErrs, validateResourceQuantity(spec.Resources.CPU, MaxCPU, resPath.Child("cpu"))...)
		allErrs = append(allErrs, validateResourceQuantity(spec.Resources.Memory, MaxMemory, resPath.Child("memory"))...)
	}

	if spec.Scaling != nil {
		scalingPath := specPath.Child("scaling")
		if spec.Scaling.Replicas > MaxReplicas {
			allErrs = append(allErrs, field.Invalid(
				scalingPath.Child("replicas"),
				spec.Scaling.Replicas,
				fmt.Sprintf("exceeds maximum allowed (%d)", MaxReplicas),
			))
		}
		allErrs = append(allErrs, validateComponentCeilings(spec.Scaling.Prefill, scalingPath.Child("prefill"))...)
		allErrs = append(allErrs, validateComponentCeilings(spec.Scaling.Decode, scalingPath.Child("decode"))...)
	}

	return allErrs
}

// validateComponentCeilings enforces ceilings on a prefill/decode component.
func validateComponentCeilings(comp *airunwayv1alpha1.ComponentScalingSpec, compPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if comp == nil {
		return allErrs
	}
	if comp.Replicas > MaxReplicas {
		allErrs = append(allErrs, field.Invalid(
			compPath.Child("replicas"),
			comp.Replicas,
			fmt.Sprintf("exceeds maximum allowed (%d)", MaxReplicas),
		))
	}
	if comp.GPU != nil && comp.GPU.Count > MaxGPUCount {
		allErrs = append(allErrs, field.Invalid(
			compPath.Child("gpu", "count"),
			comp.GPU.Count,
			fmt.Sprintf("exceeds maximum allowed (%d)", MaxGPUCount),
		))
	}
	allErrs = append(allErrs, validateResourceQuantity(comp.Memory, MaxMemory, compPath.Child("memory"))...)
	return allErrs
}

// validateImmutableFields checks if any immutable (identity) fields have been changed
// Changing these fields triggers a delete+recreate of the provider resource
func (v *ModelDeploymentCustomValidator) validateImmutableFields(oldObj, newObj *airunwayv1alpha1.ModelDeployment) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	oldSpec := &oldObj.Spec
	newSpec := &newObj.Spec

	// model.id is an identity field
	if oldSpec.Model.ID != newSpec.Model.ID {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("model", "id"),
			newSpec.Model.ID,
			"model.id is immutable (changing it requires delete and recreate)",
		))
	}

	// model.source is an identity field
	if oldSpec.Model.Source != newSpec.Model.Source {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("model", "source"),
			newSpec.Model.Source,
			"model.source is immutable (changing it requires delete and recreate)",
		))
	}

	// engine.type is an identity field (once set)
	if oldSpec.Engine.Type != "" && newSpec.Engine.Type != "" && oldSpec.Engine.Type != newSpec.Engine.Type {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("engine", "type"),
			newSpec.Engine.Type,
			"engine.type is immutable (changing it requires delete and recreate)",
		))
	}

	// provider.name is an identity field (once set). It can be pinned two ways:
	// explicitly in spec.provider.name, or recorded in status by auto-selection
	// (the controller writes its choice to status, never to the user's spec). When
	// spec.provider.name was empty, fall back to the status-recorded provider as
	// the "old" value, so overriding an auto-selected provider is rejected here at
	// admission instead of being admitted and then failing asynchronously during
	// reconciliation (the controller re-validates this too as a backstop).
	oldProvider := ""
	newProvider := ""
	if oldSpec.Provider != nil {
		oldProvider = oldSpec.Provider.Name
	}
	if oldProvider == "" && oldObj.Status.Provider != nil {
		oldProvider = oldObj.Status.Provider.Name
	}
	if newSpec.Provider != nil {
		newProvider = newSpec.Provider.Name
	}
	if oldProvider != "" && newProvider != "" && oldProvider != newProvider {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("provider", "name"),
			newProvider,
			"provider.name is immutable (changing it requires delete and recreate)",
		))
	}

	// serving.mode is an identity field
	oldMode := airunwayv1alpha1.ServingModeAggregated
	newMode := airunwayv1alpha1.ServingModeAggregated
	if oldSpec.Serving != nil && oldSpec.Serving.Mode != "" {
		oldMode = oldSpec.Serving.Mode
	}
	if newSpec.Serving != nil && newSpec.Serving.Mode != "" {
		newMode = newSpec.Serving.Mode
	}
	if oldMode != newMode {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("serving", "mode"),
			newMode,
			"serving.mode is immutable (changing it requires delete and recreate)",
		))
	}

	// Storage volumes are immutable once a managed PVC is created.
	// Only applies to managed volumes (size != nil) that existed in the old spec.
	// Two bypass scenarios are prevented:
	// 1. Dropping a managed volume from the list (would orphan its PVC)
	// 2. Setting model.storage to nil (would orphan all managed PVCs)
	oldManagedVolumes := make(map[string]airunwayv1alpha1.StorageVolume)
	if oldSpec.Model.Storage != nil {
		for _, vol := range oldSpec.Model.Storage.Volumes {
			if vol.Size != nil {
				oldManagedVolumes[vol.Name] = vol
			}
		}
	}

	if len(oldManagedVolumes) > 0 {
		storagePath := specPath.Child("model", "storage", "volumes")

		// Build a set of new volume names for quick lookup
		newVolumeNames := make(map[string]bool)
		if newSpec.Model.Storage != nil {
			for _, vol := range newSpec.Model.Storage.Volumes {
				newVolumeNames[vol.Name] = true
			}
		}

		// Pass 1 — detect removals: reject any old managed volume not present in the new spec
		for _, oldVol := range oldManagedVolumes {
			if !newVolumeNames[oldVol.Name] {
				allErrs = append(allErrs, field.Forbidden(
					storagePath,
					fmt.Sprintf("managed storage volume %q cannot be removed (it has an associated PVC; delete the ModelDeployment to clean up managed storage)", oldVol.Name),
				))
			}
		}

		// Pass 2 — detect modifications: reject any change to an existing managed volume
		if newSpec.Model.Storage != nil {
			for i, newVol := range newSpec.Model.Storage.Volumes {
				oldVol, exists := oldManagedVolumes[newVol.Name]
				if !exists {
					continue
				}
				if !storageVolumeEqual(&oldVol, &newVol) {
					volPath := storagePath.Index(i)
					allErrs = append(allErrs, field.Invalid(
						volPath,
						newVol.Name,
						"managed storage volume is immutable once created (delete the ModelDeployment to change managed storage configuration)",
					))
				}
			}
		}
	}

	return allErrs
}

// storageVolumeEqual compares two StorageVolumes semantically. It uses
// resource.Quantity.Cmp for Size rather than reflect.DeepEqual, because
// Quantity carries unexported state (cached string form, format) that can
// differ between two semantically equivalent values (e.g. "1Gi" vs "1024Mi",
// or one Quantity that has had String() called and one that hasn't).
func storageVolumeEqual(a, b *airunwayv1alpha1.StorageVolume) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Name != b.Name ||
		a.ClaimName != b.ClaimName ||
		a.MountPath != b.MountPath ||
		a.Purpose != b.Purpose ||
		a.ReadOnly != b.ReadOnly ||
		a.AccessMode != b.AccessMode {
		return false
	}
	// StorageClassName is a *string
	switch {
	case a.StorageClassName == nil && b.StorageClassName == nil:
	case a.StorageClassName == nil || b.StorageClassName == nil:
		return false
	case *a.StorageClassName != *b.StorageClassName:
		return false
	}
	// Size is a *resource.Quantity — compare by value, not by DeepEqual.
	switch {
	case a.Size == nil && b.Size == nil:
	case a.Size == nil || b.Size == nil:
		return false
	case a.Size.Cmp(*b.Size) != 0:
		return false
	}
	return true
}

// checkWarnings returns non-fatal warnings for the spec
func (v *ModelDeploymentCustomValidator) checkWarnings(obj *airunwayv1alpha1.ModelDeployment) admission.Warnings {
	var warnings admission.Warnings
	spec := &obj.Spec

	// Warn if servedName is specified with custom source
	if spec.Model.Source == airunwayv1alpha1.ModelSourceCustom && spec.Model.ServedName != "" {
		warnings = append(warnings, "servedName is ignored for custom source (model name is defined by the container)")
	}

	// Warn if trustRemoteCode is true
	if spec.Engine.TrustRemoteCode {
		warnings = append(warnings, "trustRemoteCode=true allows execution of arbitrary code from HuggingFace")
	}

	// Warn if contextLength is set for trtllm
	if spec.Engine.Type == airunwayv1alpha1.EngineTypeTRTLLM && spec.Engine.ContextLength != nil {
		warnings = append(warnings, "contextLength is ignored for TensorRT-LLM (must be configured at engine build time)")
	}

	// Warn if readOnly is true on a compilationCache volume
	if spec.Model.Storage != nil {
		for _, vol := range spec.Model.Storage.Volumes {
			if vol.Purpose == airunwayv1alpha1.VolumePurposeCompilationCache && vol.ReadOnly {
				warnings = append(warnings, fmt.Sprintf(
					"storage volume %q has purpose=compilationCache with readOnly=true; compilation cache requires write access",
					vol.Name,
				))
			}
		}
	}

	// Warn if readOnly is true on a modelCache volume with huggingface source (download will be skipped)
	if spec.Model.Source == airunwayv1alpha1.ModelSourceHuggingFace && spec.Model.Storage != nil {
		for _, vol := range spec.Model.Storage.Volumes {
			if vol.Purpose == airunwayv1alpha1.VolumePurposeModelCache && vol.ReadOnly {
				warnings = append(warnings, fmt.Sprintf(
					"storage volume %q has purpose=modelCache with readOnly=true; model download will be skipped (ensure the PVC already contains the model data)",
					vol.Name,
				))
			}
		}
	}

	return warnings
}

// validateStorage validates the model storage configuration
func (v *ModelDeploymentCustomValidator) validateStorage(obj *airunwayv1alpha1.ModelDeployment) field.ErrorList {
	var allErrs field.ErrorList
	storage := obj.Spec.Model.Storage

	if storage == nil || len(storage.Volumes) == 0 {
		return allErrs
	}

	storagePath := field.NewPath("spec", "model", "storage", "volumes")

	// System paths that cannot be used as mount points
	systemPaths := []string{"/dev", "/proc", "/sys", "/etc", "/var/run"}

	namesSeen := map[string]bool{}
	mountPathsSeen := map[string]bool{}
	claimNamesSeen := map[string]bool{}
	modelCacheCount := 0
	compilationCacheCount := 0
	hasManagedModelCache := false

	for i, vol := range storage.Volumes {
		volPath := storagePath.Index(i)

		// When size is NOT set, claimName is required (pre-existing PVC reference mode)
		if vol.Size == nil && vol.ClaimName == "" {
			allErrs = append(allErrs, field.Required(
				volPath.Child("claimName"),
				"claimName is required when size is not set (must reference a pre-existing PVC)",
			))
		}

		// Reject readOnly with size set (controller-created PVC shouldn't be read-only from the start)
		if vol.Size != nil && vol.ReadOnly {
			allErrs = append(allErrs, field.Invalid(
				volPath.Child("readOnly"),
				vol.ReadOnly,
				"readOnly must not be true when size is set (controller-created PVCs need write access)",
			))
		}

		// When size is set, claimName must match the auto-generated pattern <md-name>-<vol-name>.
		// The mutating webhook defaults claimName when empty, so by validation time it's always populated.
		// An arbitrary claimName with size could cause the reconciler to delete an unrelated PVC.
		if vol.Size != nil && vol.ClaimName != "" {
			expectedClaimName := fmt.Sprintf("%s-%s", obj.Name, vol.Name)
			if vol.ClaimName != expectedClaimName {
				allErrs = append(allErrs, field.Invalid(
					volPath.Child("claimName"),
					vol.ClaimName,
					fmt.Sprintf("claimName must not be set when size is set (auto-generated as %q)", expectedClaimName),
				))
			}
		}

		// Validate that the auto-generated claim name does not exceed the
		// Kubernetes DNS subdomain limit (253 chars).
		if vol.Size != nil {
			claimName := vol.ResolvedClaimName(obj.Name)
			if len(claimName) > 253 {
				allErrs = append(allErrs, field.Invalid(
					volPath.Child("name"),
					vol.Name,
					fmt.Sprintf(
						"auto-generated PVC claim name %q exceeds the 253-character Kubernetes name limit (got %d characters); use a shorter ModelDeployment or volume name",
						claimName, len(claimName)),
				))
			}
		}

		// Validate accessMode if set
		if vol.AccessMode != "" {
			switch vol.AccessMode {
			case corev1.ReadWriteOnce, corev1.ReadWriteMany, corev1.ReadOnlyMany, corev1.ReadWriteOncePod:
				// valid
			default:
				allErrs = append(allErrs, field.NotSupported(
					volPath.Child("accessMode"),
					vol.AccessMode,
					[]string{
						string(corev1.ReadWriteOnce),
						string(corev1.ReadWriteMany),
						string(corev1.ReadOnlyMany),
						string(corev1.ReadWriteOncePod),
					},
				))
			}

			// accessMode is only meaningful when size is set
			if vol.Size == nil {
				allErrs = append(allErrs, field.Invalid(
					volPath.Child("accessMode"),
					vol.AccessMode,
					"accessMode is only applicable when size is set (controller-created PVCs)",
				))
			}
		}

		// storageClassName is only meaningful when size is set
		if vol.StorageClassName != nil && vol.Size == nil {
			allErrs = append(allErrs, field.Invalid(
				volPath.Child("storageClassName"),
				*vol.StorageClassName,
				"storageClassName is only applicable when size is set (controller-created PVCs)",
			))
		}

		// Check duplicate names
		if namesSeen[vol.Name] {
			allErrs = append(allErrs, field.Invalid(
				volPath.Child("name"),
				vol.Name,
				"duplicate volume name",
			))
		}
		namesSeen[vol.Name] = true

		// Check duplicate mountPaths
		if vol.MountPath != "" {
			if mountPathsSeen[vol.MountPath] {
				allErrs = append(allErrs, field.Invalid(
					volPath.Child("mountPath"),
					vol.MountPath,
					"duplicate mount path",
				))
			}
			mountPathsSeen[vol.MountPath] = true
		}

		// Check duplicate claimNames (only if claimName is set)
		if vol.ClaimName != "" {
			if claimNamesSeen[vol.ClaimName] {
				allErrs = append(allErrs, field.Invalid(
					volPath.Child("claimName"),
					vol.ClaimName,
					"duplicate claim name",
				))
			}
			claimNamesSeen[vol.ClaimName] = true
		}

		// mountPath must be absolute
		if vol.MountPath != "" && !strings.HasPrefix(vol.MountPath, "/") {
			allErrs = append(allErrs, field.Invalid(
				volPath.Child("mountPath"),
				vol.MountPath,
				"mountPath must be an absolute path (start with /)",
			))
		}

		// custom purpose requires explicit mountPath
		if vol.Purpose == airunwayv1alpha1.VolumePurposeCustom && vol.MountPath == "" {
			allErrs = append(allErrs, field.Required(
				volPath.Child("mountPath"),
				"mountPath is required when purpose is custom",
			))
		}

		// Reject system paths
		for _, sysPath := range systemPaths {
			if vol.MountPath == sysPath || strings.HasPrefix(vol.MountPath, sysPath+"/") {
				allErrs = append(allErrs, field.Invalid(
					volPath.Child("mountPath"),
					vol.MountPath,
					fmt.Sprintf("mountPath must not overlap with system path %s", sysPath),
				))
				break
			}
		}

		// Count purposes
		switch vol.Purpose {
		case airunwayv1alpha1.VolumePurposeModelCache:
			modelCacheCount++
			if vol.Size != nil && !vol.ReadOnly {
				hasManagedModelCache = true
			}
		case airunwayv1alpha1.VolumePurposeCompilationCache:
			compilationCacheCount++
		}
	}

	// At most one modelCache volume
	if modelCacheCount > 1 {
		allErrs = append(allErrs, field.Invalid(
			storagePath,
			modelCacheCount,
			"at most one volume with purpose=modelCache is allowed",
		))
	}

	// At most one compilationCache volume
	if compilationCacheCount > 1 {
		allErrs = append(allErrs, field.Invalid(
			storagePath,
			compilationCacheCount,
			"at most one volume with purpose=compilationCache is allowed",
		))
	}

	// Validate that the auto-generated download job name fits within
	// the 253-character Kubernetes name limit.
	// The download job name is <md-name>-model-download (15-char suffix).
	downloadJobName := obj.Name + "-model-download"
	if hasManagedModelCache && len(downloadJobName) > 253 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("metadata", "name"),
			obj.Name,
			fmt.Sprintf(
				"auto-generated download Job name %q exceeds the 253-character Kubernetes name limit (got %d characters); use a shorter ModelDeployment name",
				downloadJobName, len(downloadJobName)),
		))
	}

	return allErrs
}

// blockedOverrideKeys are fields that cannot be set via spec.provider.overrides
// because they could escalate privileges or bypass security controls.
var blockedOverrideKeys = []string{
	"securityContext",
	"serviceAccountName",
	"serviceAccount",
	"hostNetwork",
	"hostPID",
	"hostIPC",
	"automountServiceAccountToken",
	"nodeName",
	"priorityClassName",
	"runtimeClassName",
}

// validateOverrides checks that provider overrides don't contain dangerous fields
func (v *ModelDeploymentCustomValidator) validateOverrides(spec *airunwayv1alpha1.ModelDeploymentSpec, specPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if spec.Provider == nil || spec.Provider.Overrides == nil || spec.Provider.Overrides.Raw == nil {
		return allErrs
	}

	var overrideValue interface{}
	if err := json.Unmarshal(spec.Provider.Overrides.Raw, &overrideValue); err != nil {
		// Don't echo the raw payload back: it can be large and may contain
		// data the user didn't expect to see in admission errors/logs.
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("provider", "overrides"),
			fmt.Sprintf("<redacted %d bytes>", len(spec.Provider.Overrides.Raw)),
			"overrides must be valid JSON",
		))
		return allErrs
	}

	overrideMap, ok := overrideValue.(map[string]interface{})
	if !ok {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("provider", "overrides"),
			fmt.Sprintf("<redacted %d bytes>", len(spec.Provider.Overrides.Raw)),
			"overrides must be a JSON object",
		))
		return allErrs
	}

	providerOverridesPath := specPath.Child("provider", "overrides")
	allErrs = append(allErrs, checkBlockedKeys(overrideMap, providerOverridesPath)...)
	allErrs = append(allErrs, checkSizingOverrideKeys(overrideMap, providerOverridesPath)...)

	return allErrs
}

// sizingOverrideKeys are workload-sizing fields that cannot be set via
// spec.provider.overrides because provider-specific raw overrides are merged
// after admission validates spec.resources/spec.scaling ceilings. Denying
// these unstructured keys keeps resource limits enforceable.
var sizingOverrideKeys = []string{
	"replicas",
	"resources",
}

// checkBlockedKeys recursively walks an unmarshalled JSON value and reports
// any blocked keys found in nested objects, including those nested inside
// arrays (e.g. {"containers": [{"securityContext": ...}]}).
func checkBlockedKeys(m map[string]interface{}, fldPath *field.Path) field.ErrorList {
	return checkForbiddenOverrideKeys(m, fldPath, blockedOverrideKeys, func(key string) string {
		return fmt.Sprintf("overriding %q is not allowed for security reasons", key)
	})
}

// checkSizingOverrideKeys recursively walks provider overrides and rejects
// fields that would let raw provider overrides bypass resource/replica ceilings.
func checkSizingOverrideKeys(m map[string]interface{}, fldPath *field.Path) field.ErrorList {
	return checkForbiddenOverrideKeys(m, fldPath, sizingOverrideKeys, func(key string) string {
		return fmt.Sprintf("overriding %q is not allowed because it can bypass admission resource limits; use spec.resources / spec.scaling instead", key)
	})
}

// checkForbiddenOverrideKeys recursively walks an unmarshalled JSON object and
// reports any forbidden keys found in nested objects, including those nested
// inside arrays.
func checkForbiddenOverrideKeys(m map[string]interface{}, fldPath *field.Path, forbiddenKeys []string, detailFor func(string) string) field.ErrorList {
	var allErrs field.ErrorList
	for key, val := range m {
		matched := false
		for _, forbidden := range forbiddenKeys {
			if key == forbidden {
				allErrs = append(allErrs, field.Forbidden(
					fldPath.Child(key),
					detailFor(key),
				))
				matched = true
				break
			}
		}
		// If this key is itself forbidden, the entire subtree is rejected.
		// Skip descending into it — otherwise a forbidden key whose value
		// contains another forbidden key (or the same key nested deeper)
		// produces redundant sibling errors for an already-rejected path.
		if matched {
			continue
		}
		allErrs = append(allErrs, checkForbiddenOverrideKeysInValue(val, fldPath.Child(key), forbiddenKeys, detailFor)...)
	}
	return allErrs
}

// checkForbiddenOverrideKeysInValue inspects an arbitrary JSON value and
// recurses into nested objects and arrays so forbidden keys can't bypass
// validation by being nested inside list-valued overrides.
func checkForbiddenOverrideKeysInValue(val interface{}, fldPath *field.Path, forbiddenKeys []string, detailFor func(string) string) field.ErrorList {
	switch v := val.(type) {
	case map[string]interface{}:
		return checkForbiddenOverrideKeys(v, fldPath, forbiddenKeys, detailFor)
	case []interface{}:
		var allErrs field.ErrorList
		for i, item := range v {
			allErrs = append(allErrs, checkForbiddenOverrideKeysInValue(item, fldPath.Index(i), forbiddenKeys, detailFor)...)
		}
		return allErrs
	}
	return nil
}

// validateResourceQuantity validates that a resource string doesn't exceed a maximum
func validateResourceQuantity(value string, max string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if value == "" {
		return allErrs
	}
	qty, err := resource.ParseQuantity(value)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, value, "invalid resource quantity"))
		return allErrs
	}
	maxQty := resource.MustParse(max)
	if qty.Cmp(maxQty) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, value, fmt.Sprintf("exceeds maximum allowed (%s)", max)))
	}
	return allErrs
}
