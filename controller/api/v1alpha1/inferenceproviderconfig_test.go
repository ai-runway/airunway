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
	"testing"
)

func TestHasEngine(t *testing.T) {
	caps := &ProviderCapabilities{
		Engines: []EngineCapability{
			{Name: EngineTypeVLLM, GPUSupport: true},
			{Name: EngineTypeLlamaCpp, GPUSupport: true, CPUSupport: true},
		},
	}

	if !caps.HasEngine(EngineTypeVLLM) {
		t.Error("expected HasEngine(vllm) to be true")
	}
	if !caps.HasEngine(EngineTypeLlamaCpp) {
		t.Error("expected HasEngine(llamacpp) to be true")
	}
	if caps.HasEngine(EngineTypeSGLang) {
		t.Error("expected HasEngine(sglang) to be false")
	}
	if caps.HasEngine(EngineTypeTRTLLM) {
		t.Error("expected HasEngine(trtllm) to be false")
	}
}

func TestHasEngine_Empty(t *testing.T) {
	caps := &ProviderCapabilities{}
	if caps.HasEngine(EngineTypeVLLM) {
		t.Error("expected HasEngine to be false on empty capabilities")
	}
}

func TestHasEngine_Nil(t *testing.T) {
	var caps *ProviderCapabilities
	if caps.HasEngine(EngineTypeVLLM) {
		t.Error("expected HasEngine to be false on nil capabilities")
	}
}

func TestGetEngineCapability(t *testing.T) {
	caps := &ProviderCapabilities{
		Engines: []EngineCapability{
			{Name: EngineTypeVLLM, GPUSupport: true, ServingModes: []ServingMode{ServingModeAggregated}},
			{Name: EngineTypeLlamaCpp, GPUSupport: true, CPUSupport: true, ServingModes: []ServingMode{ServingModeAggregated}},
		},
	}

	ec := caps.GetEngineCapability(EngineTypeVLLM)
	if ec == nil {
		t.Fatal("expected non-nil engine capability for vllm")
	}
	if ec.Name != EngineTypeVLLM {
		t.Errorf("expected name vllm, got %s", ec.Name)
	}
	if !ec.GPUSupport {
		t.Error("expected GPU support for vllm")
	}
	if ec.CPUSupport {
		t.Error("expected no CPU support for vllm")
	}

	ec = caps.GetEngineCapability(EngineTypeLlamaCpp)
	if ec == nil {
		t.Fatal("expected non-nil engine capability for llamacpp")
	}
	if !ec.CPUSupport {
		t.Error("expected CPU support for llamacpp")
	}

	ec = caps.GetEngineCapability(EngineTypeSGLang)
	if ec != nil {
		t.Error("expected nil engine capability for sglang")
	}
}

func TestGetEngineCapability_Nil(t *testing.T) {
	var caps *ProviderCapabilities
	ec := caps.GetEngineCapability(EngineTypeVLLM)
	if ec != nil {
		t.Error("expected nil on nil receiver")
	}
}

func TestSupportsServingMode(t *testing.T) {
	caps := &ProviderCapabilities{
		Engines: []EngineCapability{
			{
				Name:         EngineTypeVLLM,
				ServingModes: []ServingMode{ServingModeAggregated, ServingModeDisaggregated},
				GPUSupport:   true,
			},
			{
				Name:         EngineTypeTRTLLM,
				ServingModes: []ServingMode{ServingModeAggregated},
				GPUSupport:   true,
			},
		},
	}

	if !caps.SupportsServingMode(EngineTypeVLLM, ServingModeAggregated) {
		t.Error("expected vllm to support aggregated mode")
	}
	if !caps.SupportsServingMode(EngineTypeVLLM, ServingModeDisaggregated) {
		t.Error("expected vllm to support disaggregated mode")
	}
	if !caps.SupportsServingMode(EngineTypeTRTLLM, ServingModeAggregated) {
		t.Error("expected trtllm to support aggregated mode")
	}
	if caps.SupportsServingMode(EngineTypeTRTLLM, ServingModeDisaggregated) {
		t.Error("expected trtllm to NOT support disaggregated mode")
	}
	if caps.SupportsServingMode(EngineTypeSGLang, ServingModeAggregated) {
		t.Error("expected missing engine to not support any mode")
	}
}

func TestEngineCapability_SupportsServingMode(t *testing.T) {
	ec := &EngineCapability{
		Name:         EngineTypeVLLM,
		ServingModes: []ServingMode{ServingModeAggregated},
	}
	if !ec.SupportsServingMode(ServingModeAggregated) {
		t.Error("expected aggregated to be supported")
	}
	if ec.SupportsServingMode(ServingModeDisaggregated) {
		t.Error("expected disaggregated to NOT be supported")
	}
	var nilEC *EngineCapability
	if nilEC.SupportsServingMode(ServingModeAggregated) {
		t.Error("expected nil receiver to return false")
	}
}

func TestSupportsGPU(t *testing.T) {
	caps := &ProviderCapabilities{
		Engines: []EngineCapability{
			{Name: EngineTypeVLLM, GPUSupport: true},
			{Name: EngineTypeLlamaCpp, GPUSupport: true, CPUSupport: true},
		},
	}

	if !caps.SupportsGPU(EngineTypeVLLM) {
		t.Error("expected vllm to support GPU")
	}
	if !caps.SupportsGPU(EngineTypeLlamaCpp) {
		t.Error("expected llamacpp to support GPU")
	}
	if caps.SupportsGPU(EngineTypeSGLang) {
		t.Error("expected missing engine to not support GPU")
	}
}

func TestSupportsCPU(t *testing.T) {
	caps := &ProviderCapabilities{
		Engines: []EngineCapability{
			{Name: EngineTypeVLLM, GPUSupport: true},
			{Name: EngineTypeLlamaCpp, GPUSupport: true, CPUSupport: true},
		},
	}

	if caps.SupportsCPU(EngineTypeVLLM) {
		t.Error("expected vllm to NOT support CPU")
	}
	if !caps.SupportsCPU(EngineTypeLlamaCpp) {
		t.Error("expected llamacpp to support CPU")
	}
	if caps.SupportsCPU(EngineTypeSGLang) {
		t.Error("expected missing engine to not support CPU")
	}
}

func TestEngineNames(t *testing.T) {
	caps := &ProviderCapabilities{
		Engines: []EngineCapability{
			{Name: EngineTypeVLLM},
			{Name: EngineTypeSGLang},
			{Name: EngineTypeTRTLLM},
		},
	}

	names := caps.EngineNames()
	if len(names) != 3 {
		t.Fatalf("expected 3 engine names, got %d", len(names))
	}
	if names[0] != EngineTypeVLLM {
		t.Errorf("expected first engine name to be vllm, got %s", names[0])
	}
	if names[1] != EngineTypeSGLang {
		t.Errorf("expected second engine name to be sglang, got %s", names[1])
	}
	if names[2] != EngineTypeTRTLLM {
		t.Errorf("expected third engine name to be trtllm, got %s", names[2])
	}
}

func TestEngineNames_Empty(t *testing.T) {
	caps := &ProviderCapabilities{}
	names := caps.EngineNames()
	if len(names) != 0 {
		t.Fatalf("expected 0 engine names, got %d", len(names))
	}
}

func TestEngineNames_Nil(t *testing.T) {
	var caps *ProviderCapabilities
	names := caps.EngineNames()
	if names != nil {
		t.Fatalf("expected nil engine names on nil receiver, got %v", names)
	}
}
