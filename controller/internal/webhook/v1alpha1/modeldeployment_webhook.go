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
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
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
		WithValidator(&ModelDeploymentCustomValidator{}).
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
	if spec.Serving.Mode == airunwayv1alpha1.ServingModeAggregated && spec.Resources == nil {
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
type ModelDeploymentCustomValidator struct{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type ModelDeployment.
func (v *ModelDeploymentCustomValidator) ValidateCreate(_ context.Context, obj *airunwayv1alpha1.ModelDeployment) (admission.Warnings, error) {
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
	allErrs = append(allErrs, v.validateSpec(obj)...)

	// Check for warnings
	warnings = append(warnings, v.checkWarnings(obj)...)

	if len(allErrs) > 0 {
		return warnings, allErrs.ToAggregate()
	}
	return warnings, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type ModelDeployment.
func (v *ModelDeploymentCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *airunwayv1alpha1.ModelDeployment) (admission.Warnings, error) {
	modeldeploymentlog.Info("Validation for ModelDeployment upon update", "name", newObj.GetName())

	var warnings admission.Warnings
	var allErrs field.ErrorList

	// Validate the spec
	allErrs = append(allErrs, v.validateSpec(newObj)...)

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
func (v *ModelDeploymentCustomValidator) validateSpec(obj *airunwayv1alpha1.ModelDeployment) field.ErrorList {
	var allErrs field.ErrorList
	spec := &obj.Spec
	specPath := field.NewPath("spec")

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

	// Validate GPU requirements for certain engines (only when engine is specified)
	gpuCount := int32(0)
	if spec.Resources != nil && spec.Resources.GPU != nil {
		gpuCount = spec.Resources.GPU.Count
	}

	// Resource ceilings
	if gpuCount > MaxGPUCount {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("resources", "gpu", "count"),
			gpuCount,
			fmt.Sprintf("GPU count exceeds maximum allowed (%d)", MaxGPUCount),
		))
	}
	if spec.Resources != nil {
		allErrs = append(allErrs, validateResourceQuantity(spec.Resources.CPU, MaxCPU, specPath.Child("resources", "cpu"))...)
		allErrs = append(allErrs, validateResourceQuantity(spec.Resources.Memory, MaxMemory, specPath.Child("resources", "memory"))...)
	}
	if spec.Scaling != nil {
		if spec.Scaling.Replicas > MaxReplicas {
			allErrs = append(allErrs, field.Invalid(specPath.Child("scaling", "replicas"), spec.Scaling.Replicas, fmt.Sprintf("exceeds maximum replicas (%d)", MaxReplicas)))
		}
		if spec.Scaling.Prefill != nil {
			if spec.Scaling.Prefill.Replicas > MaxReplicas {
				allErrs = append(allErrs, field.Invalid(specPath.Child("scaling", "prefill", "replicas"), spec.Scaling.Prefill.Replicas, fmt.Sprintf("exceeds maximum replicas (%d)", MaxReplicas)))
			}
			if spec.Scaling.Prefill.GPU != nil && spec.Scaling.Prefill.GPU.Count > MaxGPUCount {
				allErrs = append(allErrs, field.Invalid(specPath.Child("scaling", "prefill", "gpu", "count"), spec.Scaling.Prefill.GPU.Count, fmt.Sprintf("exceeds maximum GPU count (%d)", MaxGPUCount)))
			}
			allErrs = append(allErrs, validateResourceQuantity(spec.Scaling.Prefill.Memory, MaxMemory, specPath.Child("scaling", "prefill", "memory"))...)
		}
		if spec.Scaling.Decode != nil {
			if spec.Scaling.Decode.Replicas > MaxReplicas {
				allErrs = append(allErrs, field.Invalid(specPath.Child("scaling", "decode", "replicas"), spec.Scaling.Decode.Replicas, fmt.Sprintf("exceeds maximum replicas (%d)", MaxReplicas)))
			}
			if spec.Scaling.Decode.GPU != nil && spec.Scaling.Decode.GPU.Count > MaxGPUCount {
				allErrs = append(allErrs, field.Invalid(specPath.Child("scaling", "decode", "gpu", "count"), spec.Scaling.Decode.GPU.Count, fmt.Sprintf("exceeds maximum GPU count (%d)", MaxGPUCount)))
			}
			allErrs = append(allErrs, validateResourceQuantity(spec.Scaling.Decode.Memory, MaxMemory, specPath.Child("scaling", "decode", "memory"))...)
		}
	}

	// Validate provider overrides don't contain dangerous fields
	allErrs = append(allErrs, v.validateOverrides(spec, specPath)...)

	servingMode := airunwayv1alpha1.ServingModeAggregated
	if spec.Serving != nil && spec.Serving.Mode != "" {
		servingMode = spec.Serving.Mode
	}

	switch spec.Engine.Type {
	case airunwayv1alpha1.EngineTypeVLLM, airunwayv1alpha1.EngineTypeSGLang, airunwayv1alpha1.EngineTypeTRTLLM:
		// These engines require GPU (unless in disaggregated mode with component-level GPUs)
		if servingMode == airunwayv1alpha1.ServingModeAggregated && gpuCount == 0 {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("resources", "gpu", "count"),
				gpuCount,
				fmt.Sprintf("%s engine requires GPU (set resources.gpu.count > 0)", spec.Engine.Type),
			))
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
			} else {
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
			} else {
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

	// provider.name is an identity field (once set)
	oldProvider := ""
	newProvider := ""
	if oldSpec.Provider != nil {
		oldProvider = oldSpec.Provider.Name
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
				if !reflect.DeepEqual(oldVol, newVol) {
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
		for _, forbidden := range forbiddenKeys {
			if key == forbidden {
				allErrs = append(allErrs, field.Forbidden(
					fldPath.Child(key),
					detailFor(key),
				))
			}
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
