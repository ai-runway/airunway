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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

// providerWithEngineRule returns a provider that supports the given engine
// (gpu, aggregated) and selects itself only via a CEL rule that keys on
// `spec.engine.type`. This is the scenario the review comment targeted:
// CEL must see the resolved engine type even when the user never set
// spec.engine.type (auto-selected, only in status).
func providerWithEngineRule(name string, engine airunwayv1alpha1.EngineType, priority int32) airunwayv1alpha1.InferenceProviderConfig {
	return airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.ProviderCapabilities{
				Engines: []airunwayv1alpha1.EngineCapability{
					{
						Name:         engine,
						GPUSupport:   true,
						ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated},
					},
				},
			},
			SelectionRules: []airunwayv1alpha1.SelectionRule{
				{
					Condition: "spec.engine.type == '" + string(engine) + "'",
					Priority:  priority,
				},
			},
		},
		Status: airunwayv1alpha1.InferenceProviderConfigStatus{Ready: true},
	}
}

// TestRunSelectionAlgorithm_AutoSelectedEngineVisibleToCEL ensures that CEL
// selection rules referencing `spec.engine.type` still match when the engine
// was auto-selected (lives only in status.engine.type) and the user never
// populated spec.engine.type. This is the behavior protected by the
// resolvedEngineType overlay in runSelectionAlgorithm — without it the CEL
// rule would silently evaluate false because the marshalled spec has an
// empty engine.type.
func TestRunSelectionAlgorithm_AutoSelectedEngineVisibleToCEL(t *testing.T) {
	r := &ModelDeploymentReconciler{}

	// User did NOT set spec.engine.type — the engine was auto-selected and is
	// only present in status.engine.type.
	md := &airunwayv1alpha1.ModelDeployment{
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
			},
		},
		Status: airunwayv1alpha1.ModelDeploymentStatus{
			Engine: &airunwayv1alpha1.EngineStatus{
				Type:           airunwayv1alpha1.EngineTypeVLLM,
				SelectedReason: "auto-selected",
			},
		},
	}

	providers := []airunwayv1alpha1.InferenceProviderConfig{
		providerWithEngineRule("vllm-provider", airunwayv1alpha1.EngineTypeVLLM, 10),
	}

	resolved := md.ResolvedEngineType()
	selected, _, err := r.runSelectionAlgorithm(md, providers, resolved, md.ResolvedServingMode())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected != "vllm-provider" {
		t.Errorf("expected vllm-provider to be selected via CEL rule, got %q", selected)
	}

	// Critical invariant: the selection algorithm must NOT mutate md.Spec.
	if md.Spec.Engine.Type != "" {
		t.Errorf("md.Spec.Engine.Type was mutated to %q; selection must not touch the spec",
			md.Spec.Engine.Type)
	}
}

// TestRunSelectionAlgorithm_ExplicitEngineUnchanged confirms the overlay logic
// does not clobber an engine type that the user explicitly set in spec.
func TestRunSelectionAlgorithm_ExplicitEngineUnchanged(t *testing.T) {
	r := &ModelDeploymentReconciler{}

	md := &airunwayv1alpha1.ModelDeployment{
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model:  airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
			Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
			},
		},
	}

	// Two providers: one for vllm (should win), one for sglang (must not match).
	providers := []airunwayv1alpha1.InferenceProviderConfig{
		providerWithEngineRule("vllm-provider", airunwayv1alpha1.EngineTypeVLLM, 10),
		providerWithEngineRule("sglang-provider", airunwayv1alpha1.EngineTypeSGLang, 100),
	}

	selected, _, err := r.runSelectionAlgorithm(md, providers, md.ResolvedEngineType(), md.ResolvedServingMode())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected != "vllm-provider" {
		t.Errorf("expected vllm-provider (explicit engine), got %q", selected)
	}
	if md.Spec.Engine.Type != airunwayv1alpha1.EngineTypeVLLM {
		t.Errorf("md.Spec.Engine.Type changed unexpectedly: got %q", md.Spec.Engine.Type)
	}
}

// TestValidateSpec_UsesProvidedEngineType verifies validateSpec relies on the
// engineType parameter rather than re-resolving from md, so callers control
// the source of truth.
func TestValidateSpec_UsesProvidedEngineType(t *testing.T) {
	r := &ModelDeploymentReconciler{}

	md := &airunwayv1alpha1.ModelDeployment{
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
			},
		},
	}

	// No engine in spec, no engine in status, and no engineType passed in:
	// validation must report the missing engine.
	if err := r.validateSpec(t.Context(), md, nil, "", md.ResolvedServingMode()); err == nil {
		t.Errorf("expected error when engine type is unresolved, got nil")
	}

	// When the caller resolved the engine (e.g. from status), validation should
	// succeed without the spec being mutated.
	if err := r.validateSpec(t.Context(), md, nil, airunwayv1alpha1.EngineTypeVLLM, md.ResolvedServingMode()); err != nil {
		t.Errorf("unexpected error with resolved engine type: %v", err)
	}
	if md.Spec.Engine.Type != "" {
		t.Errorf("validateSpec mutated md.Spec.Engine.Type to %q", md.Spec.Engine.Type)
	}
}
