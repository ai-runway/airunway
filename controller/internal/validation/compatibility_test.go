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

package validation

import (
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

func providerConfig(name string, engines ...airunwayv1alpha1.EngineCapability) *airunwayv1alpha1.InferenceProviderConfig {
	pc := &airunwayv1alpha1.InferenceProviderConfig{}
	pc.Name = name
	pc.Spec.Capabilities = &airunwayv1alpha1.ProviderCapabilities{Engines: engines}
	return pc
}

func TestCheckProviderCompatibility(t *testing.T) {
	vllmGPUOnly := airunwayv1alpha1.EngineCapability{
		Name:         airunwayv1alpha1.EngineTypeVLLM,
		ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated, airunwayv1alpha1.ServingModeDisaggregated},
		GPUSupport:   true,
	}
	llamaCpuGpu := airunwayv1alpha1.EngineCapability{
		Name:         airunwayv1alpha1.EngineTypeLlamaCpp,
		ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated},
		GPUSupport:   true,
		CPUSupport:   true,
	}

	tests := []struct {
		name         string
		providerName string
		config       *airunwayv1alpha1.InferenceProviderConfig
		allConfigs   []airunwayv1alpha1.InferenceProviderConfig
		engine       airunwayv1alpha1.EngineType
		mode         airunwayv1alpha1.ServingMode
		gpu          int32
		wantKinds    []CompatibilityErrorKind
	}{
		{
			name:         "no provider name, no configs: skip all checks",
			providerName: "",
			config:       nil,
			engine:       airunwayv1alpha1.EngineTypeVLLM,
			mode:         airunwayv1alpha1.ServingModeAggregated,
			gpu:          0,
			wantKinds:    nil,
		},
		{
			name:         "no provider name + configs without CPU support: GPU required",
			providerName: "",
			config:       nil,
			allConfigs:   []airunwayv1alpha1.InferenceProviderConfig{*providerConfig("kaito", vllmGPUOnly)},
			engine:       airunwayv1alpha1.EngineTypeVLLM,
			mode:         airunwayv1alpha1.ServingModeAggregated,
			gpu:          0,
			wantKinds:    []CompatibilityErrorKind{ErrGPURequired},
		},
		{
			name:         "no provider name + configs with CPU support: ok",
			providerName: "",
			config:       nil,
			allConfigs:   []airunwayv1alpha1.InferenceProviderConfig{*providerConfig("any", llamaCpuGpu)},
			engine:       airunwayv1alpha1.EngineTypeLlamaCpp,
			mode:         airunwayv1alpha1.ServingModeAggregated,
			gpu:          0,
			wantKinds:    nil,
		},
		{
			name:         "provider config nil: skip checks",
			providerName: "kaito",
			config:       nil,
			engine:       airunwayv1alpha1.EngineTypeVLLM,
			mode:         airunwayv1alpha1.ServingModeAggregated,
			gpu:          1,
			wantKinds:    nil,
		},
		{
			name:         "engine not supported by named provider",
			providerName: "kaito",
			config:       providerConfig("kaito", vllmGPUOnly),
			engine:       airunwayv1alpha1.EngineTypeLlamaCpp,
			mode:         airunwayv1alpha1.ServingModeAggregated,
			gpu:          1,
			wantKinds:    []CompatibilityErrorKind{ErrEngineUnsupported},
		},
		{
			name:         "serving mode not supported by engine",
			providerName: "kaito",
			config:       providerConfig("kaito", llamaCpuGpu),
			engine:       airunwayv1alpha1.EngineTypeLlamaCpp,
			mode:         airunwayv1alpha1.ServingModeDisaggregated,
			gpu:          1,
			wantKinds:    []CompatibilityErrorKind{ErrServingModeUnsupported},
		},
		{
			name:         "aggregated + gpu=0 + no CPU support: GPU required",
			providerName: "kaito",
			config:       providerConfig("kaito", vllmGPUOnly),
			engine:       airunwayv1alpha1.EngineTypeVLLM,
			mode:         airunwayv1alpha1.ServingModeAggregated,
			gpu:          0,
			wantKinds:    []CompatibilityErrorKind{ErrGPURequired},
		},
		{
			name:         "aggregated + gpu=0 + CPU support: ok",
			providerName: "kaito",
			config:       providerConfig("kaito", llamaCpuGpu),
			engine:       airunwayv1alpha1.EngineTypeLlamaCpp,
			mode:         airunwayv1alpha1.ServingModeAggregated,
			gpu:          0,
			wantKinds:    nil,
		},
		{
			name:         "disaggregated bypasses CPU check (no GPURequired even with gpu=0)",
			providerName: "kaito",
			config:       providerConfig("kaito", vllmGPUOnly),
			engine:       airunwayv1alpha1.EngineTypeVLLM,
			mode:         airunwayv1alpha1.ServingModeDisaggregated,
			gpu:          0,
			wantKinds:    nil,
		},
		{
			name:         "GPU set: no GPURequired even on GPU-only engine",
			providerName: "kaito",
			config:       providerConfig("kaito", vllmGPUOnly),
			engine:       airunwayv1alpha1.EngineTypeVLLM,
			mode:         airunwayv1alpha1.ServingModeAggregated,
			gpu:          2,
			wantKinds:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckProviderCompatibility(tc.providerName, tc.config, tc.allConfigs, tc.engine, tc.mode, tc.gpu)
			if len(got) != len(tc.wantKinds) {
				t.Fatalf("expected %d errors, got %d: %v", len(tc.wantKinds), len(got), got)
			}
			for i, want := range tc.wantKinds {
				if got[i].Kind != want {
					t.Errorf("error %d: kind = %q, want %q (msg=%q)", i, got[i].Kind, want, got[i].Message)
				}
			}
		})
	}
}
