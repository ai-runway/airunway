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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	providerValidation := validateProviderCompatibility(ctx, obj, specPath, servingMode, v.Reader, v.APIReader)
	warnings = append(warnings, providerValidation.Warnings...)
	allErrs = append(allErrs, providerValidation.Errors...)
	isDynamoMocker := providerValidation.IsDynamoMocker

	// Validate serving/scaling configuration
	allErrs = append(allErrs, validateServingAndScaling(spec, specPath, servingMode, isDynamoMocker)...)

	// Validate storage configuration
	allErrs = append(allErrs, validateStorage(obj)...)

	// Enforce resource ceilings to prevent runaway resource requests at admission time.
	allErrs = append(allErrs, validateResourceCeilings(spec, specPath)...)

	return warnings, allErrs
}
