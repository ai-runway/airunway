/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

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
