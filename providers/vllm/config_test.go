package vllm

import (
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

func TestGetProviderConfigSpec(t *testing.T) {
	spec := GetProviderConfigSpec()

	// Capabilities
	if spec.Capabilities == nil {
		t.Fatal("expected non-nil capabilities")
	}

	// Engines (per-engine capabilities)
	engines := spec.Capabilities.Engines
	if len(engines) == 0 {
		t.Fatal("expected at least one engine")
	}

	var vllmEngine *airunwayv1alpha1.EngineCapability
	for i := range engines {
		if engines[i].Name == airunwayv1alpha1.EngineTypeVLLM {
			vllmEngine = &engines[i]
		}
	}
	if vllmEngine == nil {
		t.Fatal("expected vllm engine support")
	}
	if !vllmEngine.GPUSupport {
		t.Error("expected GPU support")
	}
	if vllmEngine.CPUSupport {
		t.Error("expected no CPU support")
	}

	// API formats (per-engine)
	assertAPIFormats(t, "vllm", vllmEngine.APIFormats, []airunwayv1alpha1.APIFormat{
		airunwayv1alpha1.APIFormatOpenAIChat,
		airunwayv1alpha1.APIFormatOpenAIResponses,
		airunwayv1alpha1.APIFormatAnthropicMessages,
	})

	// Serving modes (aggregated only)
	hasAggregated := false
	hasDisaggregated := false
	for _, m := range vllmEngine.ServingModes {
		if m == airunwayv1alpha1.ServingModeAggregated {
			hasAggregated = true
		}
		if m == airunwayv1alpha1.ServingModeDisaggregated {
			hasDisaggregated = true
		}
	}
	if !hasAggregated {
		t.Error("expected aggregated serving mode")
	}
	if hasDisaggregated {
		t.Error("did not expect disaggregated serving mode to be advertised")
	}

	if len(spec.SelectionRules) != 0 {
		t.Fatalf("expected Direct vLLM to be explicit-only (no selection rules), got %d", len(spec.SelectionRules))
	}

}

func TestGetInstallationInfo(t *testing.T) {
	info := GetInstallationInfo()
	if info == nil {
		t.Fatal("expected non-nil installation info")
	}
	if info.Description == "" {
		t.Error("expected non-empty description")
	}
	if len(info.Steps) == 0 {
		t.Error("expected installation steps")
	}

	foundLowercaseSecretCommand := false
	for _, step := range info.Steps {
		if step.Command == "kubectl create secret generic vllm-hf-token --from-literal=HF_TOKEN=<your-token> -n <model-namespace>" {
			foundLowercaseSecretCommand = true
		}
	}
	if !foundLowercaseSecretCommand {
		t.Error("expected lowercase vllm-hf-token secret command")
	}
}

func TestProviderConstants(t *testing.T) {
	if ProviderConfigName != "vllm" {
		t.Errorf("expected provider config name 'vllm', got %s", ProviderConfigName)
	}
	if !strings.HasPrefix(ProviderVersion, "vllm-provider:") {
		t.Errorf("expected provider version to start with 'vllm-provider:', got %s", ProviderVersion)
	}
}

func TestProviderDocumentation(t *testing.T) {
	if ProviderDocumentation == "" {
		t.Error("expected documentation URL")
	}
}

func assertAPIFormats(t *testing.T, engine string, got, expected []airunwayv1alpha1.APIFormat) {
	t.Helper()
	if len(got) != len(expected) {
		t.Fatalf("expected %s to support %d API formats, got %d: %v", engine, len(expected), len(got), got)
	}
	for _, e := range expected {
		found := false
		for _, a := range got {
			if a == e {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s to support API format %s", engine, e)
		}
	}
}
