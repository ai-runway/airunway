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
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// dynamoProvider returns an InferenceProviderConfig that mimics Dynamo:
// vllm (gpu, aggregated+disaggregated), sglang (gpu, aggregated+disaggregated), trtllm (gpu, aggregated only).
func dynamoProvider() airunwayv1alpha1.InferenceProviderConfig {
	return airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "dynamo"},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.ProviderCapabilities{
				Engines: []airunwayv1alpha1.EngineCapability{
					{
						Name:         airunwayv1alpha1.EngineTypeVLLM,
						GPUSupport:   true,
						ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated, airunwayv1alpha1.ServingModeDisaggregated},
					},
					{
						Name:         airunwayv1alpha1.EngineTypeSGLang,
						GPUSupport:   true,
						ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated, airunwayv1alpha1.ServingModeDisaggregated},
					},
					{
						Name:         airunwayv1alpha1.EngineTypeTRTLLM,
						GPUSupport:   true,
						ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated},
					},
				},
			},
		},
		Status: airunwayv1alpha1.InferenceProviderConfigStatus{Ready: true},
	}
}

// kaitoProvider returns an InferenceProviderConfig that mimics KAITO:
// vllm (gpu, aggregated), llamacpp (gpu+cpu, aggregated).
func kaitoProvider() airunwayv1alpha1.InferenceProviderConfig {
	return airunwayv1alpha1.InferenceProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "kaito"},
		Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
			Capabilities: &airunwayv1alpha1.ProviderCapabilities{
				Engines: []airunwayv1alpha1.EngineCapability{
					{
						Name:         airunwayv1alpha1.EngineTypeVLLM,
						GPUSupport:   true,
						ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated},
					},
					{
						Name:         airunwayv1alpha1.EngineTypeLlamaCpp,
						GPUSupport:   true,
						CPUSupport:   true,
						ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated},
					},
				},
			},
		},
		Status: airunwayv1alpha1.InferenceProviderConfigStatus{Ready: true},
	}
}

func allProviders() []airunwayv1alpha1.InferenceProviderConfig {
	return []airunwayv1alpha1.InferenceProviderConfig{dynamoProvider(), kaitoProvider()}
}

func TestValidateSpec(t *testing.T) {
	r := &ModelDeploymentReconciler{}
	ctx := context.Background()

	tests := []struct {
		name            string
		md              airunwayv1alpha1.ModelDeployment
		providerConfigs []airunwayv1alpha1.InferenceProviderConfig
		wantErr         string // substring match; empty means no error expected
	}{
		{
			name: "valid: vllm aggregated with GPU on dynamo",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
					},
				},
			},
			providerConfigs: allProviders(),
		},
		{
			name: "valid: vllm disaggregated on dynamo",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
					Serving:  &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
						Decode:  &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
					},
				},
			},
			providerConfigs: allProviders(),
		},
		{
			name: "valid: CPU-only aggregated vllm on dynamo with mocker annotation",
			md: airunwayv1alpha1.ModelDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"airunway.ai/dynamo-test-backend": "mocker"},
				},
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
					// No resources.gpu: the GPU-less mocker backend waives it.
				},
			},
			providerConfigs: allProviders(),
		},
		{
			name: "valid: CPU-only disaggregated vllm on dynamo with mocker annotation",
			md: airunwayv1alpha1.ModelDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"airunway.ai/dynamo-test-backend": "mocker"},
				},
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
					Serving:  &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					// prefill/decode blocks present but no gpu.count.
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
						Decode:  &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
					},
				},
			},
			providerConfigs: allProviders(),
		},
		{
			name: "invalid: non-vllm engine on dynamo even with mocker annotation",
			md: airunwayv1alpha1.ModelDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"airunway.ai/dynamo-test-backend": "mocker"},
				},
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeSGLang},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "only supports the vllm engine",
		},
		{
			name: "invalid: CPU-only disaggregated vllm on dynamo WITHOUT mocker annotation",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
					Serving:  &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
						Decode:  &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
					},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "scaling.prefill.gpu.count > 0",
		},
		{
			name: "valid: llamacpp CPU-only on kaito",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeLlamaCpp},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "kaito"},
				},
			},
			providerConfigs: allProviders(),
		},
		{
			name: "invalid: trtllm disaggregated on dynamo",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeTRTLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
					Serving:  &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
						Decode:  &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
					},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "does not support disaggregated mode for engine trtllm",
		},
		{
			name: "invalid: provider does not support engine at all",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeSGLang},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "kaito"},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
					},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "does not support engine sglang",
		},
		{
			name: "invalid: llamacpp disaggregated on kaito",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeLlamaCpp},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "kaito"},
					Serving:  &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
						Decode:  &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
					},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "does not support disaggregated mode for engine llamacpp",
		},
		{
			name: "invalid: missing model.id for huggingface source",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:  airunwayv1alpha1.ModelSpec{Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
					},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "model.id is required",
		},
		{
			name: "invalid: no engine type",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model: airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
					},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "engine.type must be specified",
		},
		{
			name: "invalid: vllm without GPU requires GPU",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:  airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "vllm engine requires GPU",
		},
		{
			name: "invalid: disaggregated mode missing prefill/decode",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
					Serving:  &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "disaggregated mode requires scaling.prefill and scaling.decode",
		},
		{
			name: "invalid: disaggregated with resources.gpu set",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
					Serving:  &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 2},
					},
					Scaling: &airunwayv1alpha1.ScalingSpec{
						Prefill: &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
						Decode:  &airunwayv1alpha1.ComponentScalingSpec{GPU: &airunwayv1alpha1.GPUSpec{Count: 1}, Replicas: 1},
					},
				},
			},
			providerConfigs: allProviders(),
			wantErr:         "cannot specify both resources.gpu and scaling.prefill/decode",
		},
		{
			name: "valid: no provider specified — skips provider compatibility check",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:  airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
					},
				},
			},
			providerConfigs: allProviders(),
		},
		{
			name: "valid: provider not found in configs — no error (provider may not be registered yet)",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "unknown-provider"},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
					},
				},
			},
			providerConfigs: allProviders(),
		},
		{
			name: "valid: empty provider configs with GPU — no provider compat check",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "kaito"},
					Resources: &airunwayv1alpha1.ResourceSpec{
						GPU: &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
					},
				},
			},
			providerConfigs: nil,
		},
		{
			name: "valid: llamacpp without GPU and empty provider configs — skips GPU check",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:  airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeLlamaCpp},
				},
			},
			providerConfigs: nil,
		},
		{
			name: "invalid: pinned provider lacks CPU support even when another provider has it",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:    airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine:   airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
					Provider: &airunwayv1alpha1.ProviderSpec{Name: "dynamo"},
				},
			},
			providerConfigs: []airunwayv1alpha1.InferenceProviderConfig{
				dynamoProvider(), // vllm GPU-only
				{
					// Hypothetical provider advertising CPU vLLM. It should not
					// satisfy the GPU requirement for a deployment pinned to dynamo.
					ObjectMeta: metav1.ObjectMeta{Name: "cpu-vllm-prov"},
					Spec: airunwayv1alpha1.InferenceProviderConfigSpec{
						Capabilities: &airunwayv1alpha1.ProviderCapabilities{
							Engines: []airunwayv1alpha1.EngineCapability{
								{
									Name:         airunwayv1alpha1.EngineTypeVLLM,
									CPUSupport:   true,
									ServingModes: []airunwayv1alpha1.ServingMode{airunwayv1alpha1.ServingModeAggregated},
								},
							},
						},
					},
					Status: airunwayv1alpha1.InferenceProviderConfigStatus{Ready: true},
				},
			},
			wantErr: "vllm engine on provider dynamo requires GPU",
		},
		{
			name: "valid: vllm without GPU and empty provider configs — skips GPU check",
			md: airunwayv1alpha1.ModelDeployment{
				Spec: airunwayv1alpha1.ModelDeploymentSpec{
					Model:  airunwayv1alpha1.ModelSpec{ID: "Qwen/Qwen3-0.6B", Source: airunwayv1alpha1.ModelSourceHuggingFace},
					Engine: airunwayv1alpha1.EngineSpec{Type: airunwayv1alpha1.EngineTypeVLLM},
				},
			},
			providerConfigs: nil,
			// No error: with no provider capability data we cannot determine
			// hardware requirements; downstream reconciliation surfaces the
			// missing-provider condition with a clearer message.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.validateSpec(ctx, &tt.md, tt.providerConfigs, tt.md.ResolvedEngineType(), tt.md.ResolvedServingMode())
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			}
		})
	}
}
