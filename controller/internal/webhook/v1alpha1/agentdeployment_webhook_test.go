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
	"encoding/json"
	"testing"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func makeAgentProviderSpecWithOverrides(t *testing.T, overrides map[string]interface{}) *airunwayv1alpha1.AgentProviderSpec {
	t.Helper()
	raw, err := json.Marshal(overrides)
	if err != nil {
		t.Fatalf("marshal overrides: %v", err)
	}
	return &airunwayv1alpha1.AgentProviderSpec{
		Overrides: &runtime.RawExtension{Raw: raw},
	}
}

func TestValidateAgentProviderOverrides_AllowsSecurityContextOverrides(t *testing.T) {
	provider := makeAgentProviderSpecWithOverrides(t, map[string]interface{}{
		"workload": map[string]interface{}{
			"podSecurityContext": map[string]interface{}{
				"runAsUser":    1000,
				"runAsGroup":   1000,
				"runAsNonRoot": true,
				"fsGroup":      1000,
				"seccompProfile": map[string]interface{}{
					"type": "RuntimeDefault",
				},
			},
			"securityContext": map[string]interface{}{
				"allowPrivilegeEscalation": false,
				"readOnlyRootFilesystem":   true,
				"capabilities": map[string]interface{}{
					"drop": []interface{}{"ALL"},
				},
			},
		},
	})
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateAgentProviderOverrides_RejectsUnsupportedRootKey(t *testing.T) {
	provider := makeAgentProviderSpecWithOverrides(t, map[string]interface{}{
		"random": map[string]interface{}{},
	})
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.random")
}

func TestValidateAgentProviderOverrides_RejectsUnsupportedSecurityContextKey(t *testing.T) {
	provider := makeAgentProviderSpecWithOverrides(t, map[string]interface{}{
		"workload": map[string]interface{}{
			"securityContext": map[string]interface{}{
				"privileged": true,
			},
		},
	})
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.workload.securityContext.privileged")
}

func TestValidateAgentProviderOverrides_RejectsCapabilitiesAdd(t *testing.T) {
	provider := makeAgentProviderSpecWithOverrides(t, map[string]interface{}{
		"workload": map[string]interface{}{
			"securityContext": map[string]interface{}{
				"capabilities": map[string]interface{}{
					"add": []interface{}{"SYS_ADMIN"},
				},
			},
		},
	})
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.workload.securityContext.capabilities.add")
}

func TestValidateAgentProviderOverrides_RejectsInvalidJSON(t *testing.T) {
	provider := &airunwayv1alpha1.AgentProviderSpec{
		Overrides: &runtime.RawExtension{Raw: []byte(`{invalid`)},
	}
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	if len(errs) == 0 {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateAgentProviderOverrides_RejectsNonObjectJSON(t *testing.T) {
	provider := &airunwayv1alpha1.AgentProviderSpec{
		Overrides: &runtime.RawExtension{Raw: []byte(`["x"]`)},
	}
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	requireValidationErrorDetail(t, errs, "overrides must be a JSON object")
}
