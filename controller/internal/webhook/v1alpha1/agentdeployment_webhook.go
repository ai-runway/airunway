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
	"math"

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
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

// agentDeploymentMaxNameLength caps AgentDeployment names so every derived
// workload label value (which uses the name verbatim) stays within
// Kubernetes' 63-character label-value limit. Without this, an otherwise
// valid long name is admitted but its rendered Deployment/Job fails to apply.
const agentDeploymentMaxNameLength = 63

// ValidateCreate validates AgentDeployment on create.
func (v *AgentDeploymentCustomValidator) ValidateCreate(_ context.Context, obj *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	allErrs := validateAgentProviderOverrides(obj.Spec.Provider, field.NewPath("spec", "provider", "overrides"))
	allErrs = append(allErrs, validateAgentDeploymentName(obj)...)
	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	return nil, nil
}

// validateAgentDeploymentName rejects names too long to be reused verbatim as a
// label value on the rendered workloads.
func validateAgentDeploymentName(obj *airunwayv1alpha1.AgentDeployment) field.ErrorList {
	if len(obj.Name) > agentDeploymentMaxNameLength {
		return field.ErrorList{field.Invalid(
			field.NewPath("metadata", "name"),
			obj.Name,
			fmt.Sprintf("name must be at most %d characters so derived workload labels stay within Kubernetes' 63-character label-value limit", agentDeploymentMaxNameLength),
		)}
	}
	return nil
}

// ValidateUpdate validates AgentDeployment on update.
func (v *AgentDeploymentCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	return v.validateUpdate(oldObj, newObj)
}

func (v *AgentDeploymentCustomValidator) validateUpdate(oldObj, newObj *airunwayv1alpha1.AgentDeployment) (admission.Warnings, error) {
	allErrs := validateAgentProviderOverrides(newObj.Spec.Provider, field.NewPath("spec", "provider", "overrides"))
	if oldObj != nil && oldObj.Spec.Framework.Name != newObj.Spec.Framework.Name {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "framework", "name"),
			"framework selection is immutable; create a new AgentDeployment to switch frameworks",
		))
	}
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
			allErrs = append(allErrs, validatePodSecurityContextValues(obj, sectionPath.Child(key))...)
			if seccompVal, found := obj["seccompProfile"]; found {
				allErrs = append(allErrs, validateSeccompProfile(seccompVal, sectionPath.Child(key, "seccompProfile"))...)
			}
		case "securityContext":
			allErrs = append(allErrs, validateAllowedObjectKeys(obj, sectionPath.Child(key), allowedContainerSecurityContextKeys,
				"unsupported securityContext key")...)
			allErrs = append(allErrs, validateContainerSecurityContextValues(obj, sectionPath.Child(key))...)
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
	allErrs := validateAllowedObjectKeys(obj, path, allowedCapabilitiesKeys, "unsupported capabilities key")
	dropVal, hasDrop := obj["drop"]
	if !hasDrop {
		return allErrs
	}
	dropList, ok := dropVal.([]interface{})
	if !ok {
		allErrs = append(allErrs, field.Invalid(path.Child("drop"), dropVal, "drop must be an array of capability names"))
		return allErrs
	}
	if len(dropList) == 0 {
		allErrs = append(allErrs, field.Invalid(path.Child("drop"), dropVal, "drop must include \"ALL\""))
		return allErrs
	}
	hasAll := false
	for i, item := range dropList {
		capName, ok := item.(string)
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("drop").Index(i), item, "capability name must be a string"))
			continue
		}
		if capName == "ALL" {
			hasAll = true
		}
	}
	if !hasAll {
		allErrs = append(allErrs, field.Invalid(path.Child("drop"), dropVal, "drop must include \"ALL\""))
	}
	return allErrs
}

func validateSeccompProfile(value interface{}, path *field.Path) field.ErrorList {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return field.ErrorList{field.Invalid(path, value, "seccompProfile override must be a JSON object")}
	}
	allErrs := validateAllowedObjectKeys(obj, path, allowedSeccompProfileKeys, "unsupported seccompProfile key")
	typeVal, found := obj["type"]
	if !found {
		allErrs = append(allErrs, field.Required(path.Child("type"), "seccompProfile.type is required"))
		return allErrs
	}
	typeName, ok := typeVal.(string)
	if !ok || typeName == "" {
		allErrs = append(allErrs, field.Invalid(path.Child("type"), typeVal, "seccompProfile.type must be a non-empty string"))
		return allErrs
	}

	localhostProfileVal, hasLocalhostProfile := obj["localhostProfile"]
	switch typeName {
	case "RuntimeDefault":
		if hasLocalhostProfile {
			allErrs = append(allErrs, field.Forbidden(path.Child("localhostProfile"), "localhostProfile is only valid when seccompProfile.type is Localhost"))
		}
	case "Localhost":
		if !hasLocalhostProfile {
			allErrs = append(allErrs, field.Required(path.Child("localhostProfile"), "localhostProfile is required when seccompProfile.type is Localhost"))
			return allErrs
		}
		profile, ok := localhostProfileVal.(string)
		if !ok || profile == "" {
			allErrs = append(allErrs, field.Invalid(path.Child("localhostProfile"), localhostProfileVal, "localhostProfile must be a non-empty string"))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(path.Child("type"), typeVal, []string{"RuntimeDefault", "Localhost"}))
	}
	return allErrs
}

func validatePodSecurityContextValues(m map[string]interface{}, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if value, found := m["runAsNonRoot"]; found {
		b, ok := value.(bool)
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("runAsNonRoot"), value, "runAsNonRoot must be a boolean"))
		} else if !b {
			allErrs = append(allErrs, field.Forbidden(path.Child("runAsNonRoot"), "runAsNonRoot cannot be set to false"))
		}
	}
	if value, found := m["runAsUser"]; found {
		allErrs = append(allErrs, validateRunAsUser(path.Child("runAsUser"), value)...)
	}
	if value, found := m["runAsGroup"]; found {
		allErrs = append(allErrs, validateNonNegativeInt64(path.Child("runAsGroup"), value)...)
	}
	if value, found := m["fsGroup"]; found {
		allErrs = append(allErrs, validateNonNegativeInt64(path.Child("fsGroup"), value)...)
	}
	if value, found := m["supplementalGroups"]; found {
		groups, ok := value.([]interface{})
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("supplementalGroups"), value, "supplementalGroups must be an array of integers"))
		} else {
			for i, groupVal := range groups {
				allErrs = append(allErrs, validateNonNegativeInt64(path.Child("supplementalGroups").Index(i), groupVal)...)
			}
		}
	}
	if value, found := m["fsGroupChangePolicy"]; found {
		policy, ok := value.(string)
		if !ok || policy == "" {
			allErrs = append(allErrs, field.Invalid(path.Child("fsGroupChangePolicy"), value, "fsGroupChangePolicy must be a non-empty string"))
		} else if policy != "Always" && policy != "OnRootMismatch" {
			allErrs = append(allErrs, field.NotSupported(path.Child("fsGroupChangePolicy"), policy, []string{"Always", "OnRootMismatch"}))
		}
	}
	return allErrs
}

func validateContainerSecurityContextValues(m map[string]interface{}, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if value, found := m["runAsNonRoot"]; found {
		b, ok := value.(bool)
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("runAsNonRoot"), value, "runAsNonRoot must be a boolean"))
		} else if !b {
			allErrs = append(allErrs, field.Forbidden(path.Child("runAsNonRoot"), "runAsNonRoot cannot be set to false"))
		}
	}
	if value, found := m["allowPrivilegeEscalation"]; found {
		b, ok := value.(bool)
		if !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("allowPrivilegeEscalation"), value, "allowPrivilegeEscalation must be a boolean"))
		} else if b {
			allErrs = append(allErrs, field.Forbidden(path.Child("allowPrivilegeEscalation"), "allowPrivilegeEscalation cannot be set to true"))
		}
	}
	if value, found := m["readOnlyRootFilesystem"]; found {
		if _, ok := value.(bool); !ok {
			allErrs = append(allErrs, field.Invalid(path.Child("readOnlyRootFilesystem"), value, "readOnlyRootFilesystem must be a boolean"))
		}
	}
	if value, found := m["runAsUser"]; found {
		allErrs = append(allErrs, validateRunAsUser(path.Child("runAsUser"), value)...)
	}
	if value, found := m["runAsGroup"]; found {
		allErrs = append(allErrs, validateNonNegativeInt64(path.Child("runAsGroup"), value)...)
	}
	return allErrs
}

func validateNonNegativeInt64(path *field.Path, value interface{}) field.ErrorList {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < 0 {
		return field.ErrorList{field.Invalid(path, value, "must be a non-negative integer")}
	}
	return nil
}

// validateRunAsUser requires a strictly positive UID. Every rendered container
// keeps runAsNonRoot=true and overrides may not disable it, so runAsUser=0
// (root) would be admitted here but rejected by the kubelet at pod start.
func validateRunAsUser(path *field.Path, value interface{}) field.ErrorList {
	if errs := validateNonNegativeInt64(path, value); len(errs) > 0 {
		return errs
	}
	if number, ok := value.(float64); ok && number == 0 {
		return field.ErrorList{field.Forbidden(path, "runAsUser cannot be 0 (root); runAsNonRoot is always enforced")}
	}
	return nil
}
