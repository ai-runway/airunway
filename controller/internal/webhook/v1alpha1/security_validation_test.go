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
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func requireValidationErrorField(t *testing.T, errs field.ErrorList, fieldName string) {
	t.Helper()
	for _, err := range errs {
		if err.Field == fieldName {
			return
		}
	}
	t.Fatalf("expected validation error for %s, got %v", fieldName, errs)
}

func requireValidationErrorDetail(t *testing.T, errs field.ErrorList, detail string) {
	t.Helper()
	for _, err := range errs {
		if err.Detail == detail {
			return
		}
	}
	t.Fatalf("expected validation error detail %q, got %v", detail, errs)
}

func TestValidateOverrides_BlocksSecurityContext(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"securityContext": map[string]interface{}{
			"privileged": true,
		},
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) == 0 {
		t.Fatal("expected error for securityContext override")
	}
}

func TestValidateOverrides_BlocksHostNetwork(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"hostNetwork": true,
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) == 0 {
		t.Fatal("expected error for hostNetwork override")
	}
}

func TestValidateOverrides_BlocksServiceAccountName(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"serviceAccountName": "admin",
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) == 0 {
		t.Fatal("expected error for serviceAccountName override")
	}
}

func TestValidateOverrides_BlocksNestedSecurityContext(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"spec": map[string]interface{}{
			"securityContext": map[string]interface{}{
				"runAsRoot": true,
			},
		},
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) == 0 {
		t.Fatal("expected error for nested securityContext override")
	}
}

func TestValidateOverrides_AllowsSafeFields(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"labels": map[string]interface{}{
			"team": "ml",
		},
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) != 0 {
		t.Fatalf("expected no errors for safe fields, got %v", errs)
	}
}

func TestValidateOverrides_BlocksNestedReplicas(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"frontend": map[string]interface{}{
			"replicas": MaxReplicas + 1,
		},
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.frontend.replicas")
}

func TestValidateOverrides_BlocksNestedResources(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"frontend": map[string]interface{}{
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{
					"cpu": "1024",
				},
			},
		},
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.frontend.resources")
}

func TestValidateOverrides_BlocksSizingKeysInsideArray(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"services": []interface{}{
			map[string]interface{}{
				"name":     "frontend",
				"replicas": MaxReplicas + 1,
			},
		},
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.services[0].replicas")
}

func TestValidateOverrides_NilOverrides(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	spec := &airunwayv1alpha1.ModelDeploymentSpec{}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) != 0 {
		t.Fatalf("expected no errors for nil overrides, got %v", errs)
	}
}

func TestValidateOverrides_InvalidJSON(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: []byte(`{invalid`)},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) == 0 {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateOverrides_RejectsNonObjectJSON(t *testing.T) {
	testCases := []struct {
		name string
		raw  []byte
	}{
		{name: "array", raw: []byte(`[{}]`)},
		{name: "string", raw: []byte(`"override"`)},
		{name: "number", raw: []byte(`42`)},
		{name: "null", raw: []byte(`null`)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			v := &ModelDeploymentCustomValidator{}
			spec := &airunwayv1alpha1.ModelDeploymentSpec{
				Provider: &airunwayv1alpha1.ProviderSpec{
					Overrides: &runtime.RawExtension{Raw: tc.raw},
				},
			}
			errs := v.validateOverrides(spec, field.NewPath("spec"))
			requireValidationErrorDetail(t, errs, "overrides must be a JSON object")
		})
	}
}

func TestValidateResourceQuantity_WithinLimits(t *testing.T) {
	errs := validateResourceQuantity("4", MaxCPU, field.NewPath("cpu"))
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateResourceQuantity_ExceedsLimit(t *testing.T) {
	errs := validateResourceQuantity("1024", MaxCPU, field.NewPath("cpu"))
	if len(errs) == 0 {
		t.Fatal("expected error for exceeding CPU limit")
	}
}

func TestValidateResourceQuantity_ExceedsMemoryLimit(t *testing.T) {
	errs := validateResourceQuantity("8Ti", MaxMemory, field.NewPath("memory"))
	if len(errs) == 0 {
		t.Fatal("expected error for exceeding memory limit")
	}
}

func TestValidateResourceQuantity_ValidMemory(t *testing.T) {
	errs := validateResourceQuantity("256Gi", MaxMemory, field.NewPath("memory"))
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateResourceQuantity_EmptyString(t *testing.T) {
	errs := validateResourceQuantity("", MaxCPU, field.NewPath("cpu"))
	if len(errs) != 0 {
		t.Fatalf("expected no errors for empty string, got %v", errs)
	}
}

func TestValidateResourceQuantity_InvalidFormat(t *testing.T) {
	errs := validateResourceQuantity("notanumber", MaxCPU, field.NewPath("cpu"))
	if len(errs) == 0 {
		t.Fatal("expected error for invalid resource format")
	}
}

func TestResourceCeilings_GPUCount(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	md := &airunwayv1alpha1.ModelDeployment{
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{ID: "test/model"},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: MaxGPUCount + 1},
			},
		},
	}
	_, errs := v.validateSpec(context.Background(), md)
	found := false
	for _, e := range errs {
		if e.Field == "spec.resources.gpu.count" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected GPU count ceiling error")
	}
}

func TestResourceCeilings_GPUCountValid(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	md := &airunwayv1alpha1.ModelDeployment{
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model:  airunwayv1alpha1.ModelSpec{ID: "test/model"},
			Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: 8},
			},
		},
	}
	_, errs := v.validateSpec(context.Background(), md)
	for _, e := range errs {
		if e.Field == "spec.resources.gpu.count" {
			t.Fatalf("unexpected GPU count error: %v", e)
		}
	}
}

// TestValidateOverrides_BlocksKeysInsideArray covers the array-recursion bug:
// previously checkBlockedKeys only walked nested objects, so blocked keys
// nested inside list-valued overrides (e.g. containers[].securityContext)
// could bypass validation.
func TestValidateOverrides_BlocksKeysInsideArray(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"containers": []interface{}{
			map[string]interface{}{
				"name": "main",
				"securityContext": map[string]interface{}{
					"privileged": true,
				},
			},
		},
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) == 0 {
		t.Fatal("expected error for securityContext nested inside an array")
	}
}

// TestValidateOverrides_BlocksKeysInsideNestedArray covers arrays inside arrays
// (e.g. spec.template.spec.containers[].volumeMounts) so deeply nested blocked
// keys can't slip through either.
func TestValidateOverrides_BlocksKeysInsideNestedArray(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":        "main",
							"hostNetwork": true,
						},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) == 0 {
		t.Fatal("expected error for hostNetwork nested inside spec.template.spec.containers[]")
	}
}

// TestValidateOverrides_NoDuplicateErrorsForNestedForbiddenKey ensures that a
// forbidden key whose value contains another forbidden key is reported once
// (for the outer path) and not also reported for inner paths, because the
// whole subtree is already rejected.
func TestValidateOverrides_NoDuplicateErrorsForNestedForbiddenKey(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	overrides := map[string]interface{}{
		"securityContext": map[string]interface{}{
			// Nested forbidden key inside an already-forbidden subtree.
			"securityContext": map[string]interface{}{
				"privileged": true,
			},
			"hostNetwork": true,
		},
	}
	raw, _ := json.Marshal(overrides)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: raw},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) != 1 {
		t.Fatalf("expected exactly one error for the outer securityContext, got %d: %v", len(errs), errs)
	}
	if got := errs[0].Field; got != "spec.provider.overrides.securityContext" {
		t.Fatalf("expected error on outer securityContext path, got %q", got)
	}
}

func TestResourceCeilings_AggregatedReplicas(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	md := &airunwayv1alpha1.ModelDeployment{
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model:   airunwayv1alpha1.ModelSpec{ID: "test/model"},
			Scaling: &airunwayv1alpha1.ScalingSpec{Replicas: MaxReplicas + 1},
		},
	}
	_, errs := v.validateSpec(context.Background(), md)
	found := false
	for _, e := range errs {
		if e.Field == "spec.scaling.replicas" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected aggregated-mode replicas ceiling error")
	}
}

func TestResourceCeilings_PrefillMemory(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	md := &airunwayv1alpha1.ModelDeployment{
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{ID: "test/model"},
			Scaling: &airunwayv1alpha1.ScalingSpec{
				Prefill: &airunwayv1alpha1.ComponentScalingSpec{Memory: "8Ti"},
			},
		},
	}
	_, errs := v.validateSpec(context.Background(), md)
	found := false
	for _, e := range errs {
		if e.Field == "spec.scaling.prefill.memory" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected prefill memory ceiling error")
	}
}

func TestResourceCeilings_DecodeMemory(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	md := &airunwayv1alpha1.ModelDeployment{
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{ID: "test/model"},
			Scaling: &airunwayv1alpha1.ScalingSpec{
				Decode: &airunwayv1alpha1.ComponentScalingSpec{Memory: "8Ti"},
			},
		},
	}
	_, errs := v.validateSpec(context.Background(), md)
	found := false
	for _, e := range errs {
		if e.Field == "spec.scaling.decode.memory" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected decode memory ceiling error")
	}
}

// TestValidateOverrides_InvalidJSONRedactsRawPayload ensures admission errors
// don't echo the user's full raw payload back: it can be large and may contain
// content the user didn't intend to surface in admission errors/logs.
func TestValidateOverrides_InvalidJSONRedactsRawPayload(t *testing.T) {
	v := &ModelDeploymentCustomValidator{}
	rawPayload := []byte(`{secret-token: "supersecret-should-not-leak"`)
	spec := &airunwayv1alpha1.ModelDeploymentSpec{
		Provider: &airunwayv1alpha1.ProviderSpec{
			Overrides: &runtime.RawExtension{Raw: rawPayload},
		},
	}
	errs := v.validateOverrides(spec, field.NewPath("spec"))
	if len(errs) == 0 {
		t.Fatal("expected error for invalid JSON")
	}
	for _, e := range errs {
		bv, ok := e.BadValue.(string)
		if !ok {
			continue
		}
		if bv != "" && bv == string(rawPayload) {
			t.Fatalf("admission error echoed raw payload back instead of redacting: %q", bv)
		}
	}
}
