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
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func makeMinimalAgentDeployment(framework string) *airunwayv1alpha1.AgentDeployment {
	return &airunwayv1alpha1.AgentDeployment{
		Spec: airunwayv1alpha1.AgentDeploymentSpec{
			Framework: airunwayv1alpha1.AgentFrameworkRef{Name: framework},
			Model: airunwayv1alpha1.ModelBinding{
				ExternalAPI: &airunwayv1alpha1.ExternalAPIBinding{
					Type:      airunwayv1alpha1.ExternalAPITypeOpenAI,
					BaseURL:   "https://api.openai.com/v1",
					ModelName: "gpt-4o-mini",
				},
			},
		},
	}
}

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

func TestValidateAgentProviderOverrides_RejectsAllowPrivilegeEscalationTrue(t *testing.T) {
	provider := makeAgentProviderSpecWithOverrides(t, map[string]interface{}{
		"workload": map[string]interface{}{
			"securityContext": map[string]interface{}{
				"allowPrivilegeEscalation": true,
			},
		},
	})
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.workload.securityContext.allowPrivilegeEscalation")
}

func TestValidateAgentProviderOverrides_RejectsRunAsNonRootFalse(t *testing.T) {
	provider := makeAgentProviderSpecWithOverrides(t, map[string]interface{}{
		"workload": map[string]interface{}{
			"podSecurityContext": map[string]interface{}{
				"runAsNonRoot": false,
			},
		},
	})
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.workload.podSecurityContext.runAsNonRoot")
}

func TestValidateAgentProviderOverrides_RejectsCapabilitiesDropWithoutALL(t *testing.T) {
	provider := makeAgentProviderSpecWithOverrides(t, map[string]interface{}{
		"workload": map[string]interface{}{
			"securityContext": map[string]interface{}{
				"capabilities": map[string]interface{}{
					"drop": []interface{}{"NET_RAW"},
				},
			},
		},
	})
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.workload.securityContext.capabilities.drop")
}

func TestValidateAgentProviderOverrides_RejectsLocalhostSeccompWithoutProfile(t *testing.T) {
	provider := makeAgentProviderSpecWithOverrides(t, map[string]interface{}{
		"workload": map[string]interface{}{
			"podSecurityContext": map[string]interface{}{
				"seccompProfile": map[string]interface{}{
					"type": "Localhost",
				},
			},
		},
	})
	errs := validateAgentProviderOverrides(provider, field.NewPath("spec", "provider", "overrides"))
	requireValidationErrorField(t, errs, "spec.provider.overrides.workload.podSecurityContext.seccompProfile.localhostProfile")
}

func TestAgentDeploymentCustomValidator_RejectsFrameworkChangeOnUpdate(t *testing.T) {
	validator := &AgentDeploymentCustomValidator{}
	oldObj := makeMinimalAgentDeployment("kagent")
	newObj := makeMinimalAgentDeployment("orka")

	_, err := validator.ValidateUpdate(context.Background(), oldObj, newObj)
	if err == nil {
		t.Fatal("expected framework immutability validation error, got nil")
	}
	if !strings.Contains(err.Error(), "spec.framework.name") {
		t.Fatalf("expected error to reference spec.framework.name, got: %v", err)
	}
}

func TestAgentDeploymentCustomValidator_AllowsUpdateWhenFrameworkUnchanged(t *testing.T) {
	validator := &AgentDeploymentCustomValidator{}
	oldObj := makeMinimalAgentDeployment("kagent")
	newObj := oldObj.DeepCopy()
	newObj.Spec.Provider = makeAgentProviderSpecWithOverrides(t, map[string]interface{}{
		"workload": map[string]interface{}{
			"securityContext": map[string]interface{}{
				"allowPrivilegeEscalation": false,
				"capabilities": map[string]interface{}{
					"drop": []interface{}{"ALL"},
				},
			},
		},
	})

	_, err := validator.ValidateUpdate(context.Background(), oldObj, newObj)
	if err != nil {
		t.Fatalf("expected update with unchanged framework to pass, got: %v", err)
	}
}
