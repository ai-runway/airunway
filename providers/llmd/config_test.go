package llmd

import (
	"testing"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
)

func TestGetProviderConfigSpec(t *testing.T) {
	spec := GetProviderConfigSpec()

	// Capabilities
	if spec.Capabilities == nil {
		t.Fatal("expected non-nil capabilities")
	}
	if !spec.Capabilities.GPUSupport {
		t.Error("expected GPU support")
	}
	if spec.Capabilities.CPUSupport {
		t.Error("expected no CPU support")
	}
	if spec.Capabilities.RequiresCRD == nil || *spec.Capabilities.RequiresCRD {
		t.Error("expected LLMD to not require CRDs")
	}

	// Engines
	engines := spec.Capabilities.Engines
	if len(engines) == 0 {
		t.Fatal("expected at least one engine")
	}
	hasVLLM := false
	for _, e := range engines {
		if e == airunwayv1alpha1.EngineTypeVLLM {
			hasVLLM = true
		}
	}
	if !hasVLLM {
		t.Error("expected vllm engine support")
	}

	// Serving modes
	modes := spec.Capabilities.ServingModes
	hasAggregated := false
	hasDisaggregated := false
	for _, m := range modes {
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
	if !hasDisaggregated {
		t.Error("expected disaggregated serving mode")
	}

	// No auto-selection rules
	if len(spec.SelectionRules) != 0 {
		t.Errorf("expected no selection rules (never auto-selected), got %d", len(spec.SelectionRules))
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
}

func TestProviderDocumentation(t *testing.T) {
	if ProviderDocumentation == "" {
		t.Error("expected documentation URL")
	}
}
