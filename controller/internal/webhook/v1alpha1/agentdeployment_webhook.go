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

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

var (
	allowedProviderOverrideRootKeys = map[string]struct{}{
		"workload":  {},
		"container": {},
	}
	allowedWorkloadOverrideKeys = map[string]struct{}{
		"podSecurityContext": {},
		"securityContext":    {},
	}
	allowedPodSecurityContextKeys = map[string]struct{}{
		"runAsUser":           {},
		"runAsGroup":          {},
		"runAsNonRoot":        {},
		"fsGroup":             {},
		"supplementalGroups":  {},
		"fsGroupChangePolicy": {},
		"seccompProfile":      {},
	}
	allowedContainerSecurityContextKeys = map[string]struct{}{
		"runAsUser":                {},
		"runAsGroup":               {},
		"runAsNonRoot":             {},
		"allowPrivilegeEscalation": {},
		"readOnlyRootFilesystem":   {},
		"capabilities":             {},
		"seccompProfile":           {},
	}
	allowedCapabilitiesKeys = map[string]struct{}{
		"drop": {},
	}
	allowedSeccompProfileKeys = map[string]struct{}{
		"type":             {},
		"localhostProfile": {},
	}
)

// SetupAgentDeploymentWebhookWithManager registers the validating webhook for AgentDeployment.
func SetupAgentDeploymentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &airunwayv1alpha1.AgentDeployment{}).
		WithValidator(&AgentDeploymentCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-airunway-ai-v1alpha1-agentdeployment,mutating=false,failurePolicy=fail,sideEffects=None,groups=airunway.ai,resources=agentdeployments,verbs=create;update,versions=v1alpha1,name=vagentdeployment-v1alpha1.kb.io,admissionReviewVersions=v1

// AgentDeploymentCustomValidator validates AgentDeployment resources.
type AgentDeploymentCustomValidator struct{}

// ValidateCreate validates AgentDeployment on create.
func (v *AgentDeploymentCustomValidator) ValidateCreate(_ context.Context, obj *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	allErrs := validateAgentProviderOverrides(obj.Spec.Provider, field.NewPath("spec", "provider", "overrides"))
	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	return nil, nil
}

// ValidateUpdate validates AgentDeployment on update.
func (v *AgentDeploymentCustomValidator) ValidateUpdate(_ context.Context, _, newObj *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	allErrs := validateAgentProviderOverrides(newObj.Spec.Provider, field.NewPath("spec", "provider", "overrides"))
	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	return nil, nil
}

// ValidateDelete performs no validation on delete.
func (v *AgentDeploymentCustomValidator) ValidateDelete(_ context.Context, _ *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	return nil, nil
}

func validateAgentProviderOverrides(provider *airunwayv1alpha1.AgentProviderSpec, overridesPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if provider == nil || provider.Overrides == nil || len(provider.Overrides.Raw) == 0 {
		return allErrs
	}

	var rawValue interface{}
	if err := json.Unmarshal(provider.Overrides.Raw, &rawValue); err != nil {
		allErrs = append(allErrs, field.Invalid(
			overridesPath,
			fmt.Sprintf("<redacted %d bytes>", len(provider.Overrides.Raw)),
			"overrides must be valid JSON",
		))
		return allErrs
	}

	root, ok := rawValue.(map[string]interface{})
	if !ok {
		allErrs = append(allErrs, field.Invalid(
			overridesPath,
			fmt.Sprintf("<redacted %d bytes>", len(provider.Overrides.Raw)),
			"overrides must be a JSON object",
		))
		return allErrs
	}

	for key, value := range root {
		if _, allowed := allowedProviderOverrideRootKeys[key]; !allowed {
			allErrs = append(allErrs, field.Forbidden(
				overridesPath.Child(key),
				"only workload/container override sections are supported",
			))
			continue
		}
		section, ok := value.(map[string]interface{})
		if !ok {
			allErrs = append(allErrs, field.Invalid(
				overridesPath.Child(key),
				value,
				"override section must be a JSON object",
			))
			continue
		}
		allErrs = append(allErrs, validateWorkloadSecurityOverrides(section, overridesPath.Child(key))...)
	}

	return allErrs
}

func validateWorkloadSecurityOverrides(section map[string]interface{}, sectionPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for key, value := range section {
		if _, allowed := allowedWorkloadOverrideKeys[key]; !allowed {
			allErrs = append(allErrs, field.Forbidden(
				sectionPath.Child(key),
				"only podSecurityContext and securityContext overrides are allowed",
			))
			continue
		}
		obj, ok := value.(map[string]interface{})
		if !ok {
			allErrs = append(allErrs, field.Invalid(
				sectionPath.Child(key),
				value,
				"security override must be a JSON object",
			))
			continue
		}

		switch key {
		case "podSecurityContext":
			allErrs = append(allErrs, validateAllowedObjectKeys(obj, sectionPath.Child(key), allowedPodSecurityContextKeys,
				"unsupported podSecurityContext key")...)
			if seccompVal, found := obj["seccompProfile"]; found {
				allErrs = append(allErrs, validateSeccompProfile(seccompVal, sectionPath.Child(key, "seccompProfile"))...)
			}
		case "securityContext":
			allErrs = append(allErrs, validateAllowedObjectKeys(obj, sectionPath.Child(key), allowedContainerSecurityContextKeys,
				"unsupported securityContext key")...)
			if capsVal, found := obj["capabilities"]; found {
				allErrs = append(allErrs, validateCapabilities(capsVal, sectionPath.Child(key, "capabilities"))...)
			}
			if seccompVal, found := obj["seccompProfile"]; found {
				allErrs = append(allErrs, validateSeccompProfile(seccompVal, sectionPath.Child(key, "seccompProfile"))...)
			}
		}
	}
	return allErrs
}

func validateAllowedObjectKeys(m map[string]interface{}, path *field.Path, allowed map[string]struct{}, detailPrefix string) field.ErrorList {
	var allErrs field.ErrorList
	for key := range m {
		if _, ok := allowed[key]; !ok {
			allErrs = append(allErrs, field.Forbidden(
				path.Child(key),
				fmt.Sprintf("%s %q", detailPrefix, key),
			))
		}
	}
	return allErrs
}

func validateCapabilities(value interface{}, path *field.Path) field.ErrorList {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return field.ErrorList{field.Invalid(path, value, "capabilities override must be a JSON object")}
	}
	return validateAllowedObjectKeys(obj, path, allowedCapabilitiesKeys, "unsupported capabilities key")
}

func validateSeccompProfile(value interface{}, path *field.Path) field.ErrorList {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return field.ErrorList{field.Invalid(path, value, "seccompProfile override must be a JSON object")}
	}
	return validateAllowedObjectKeys(obj, path, allowedSeccompProfileKeys, "unsupported seccompProfile key")
}
