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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestAgentProviderCapabilities_HasBindingMode(t *testing.T) {
	tests := []struct {
		name string
		caps *AgentProviderCapabilities
		mode ModelBindingMode
		want bool
	}{
		{
			name: "nil receiver returns false",
			caps: nil,
			mode: ModelBindingModeDeploymentRef,
			want: false,
		},
		{
			name: "empty modes returns false",
			caps: &AgentProviderCapabilities{},
			mode: ModelBindingModeDeploymentRef,
			want: false,
		},
		{
			name: "matching mode returns true",
			caps: &AgentProviderCapabilities{
				ModelBindingModes: []ModelBindingMode{ModelBindingModeDeploymentRef, ModelBindingModeExternalAPI},
			},
			mode: ModelBindingModeDeploymentRef,
			want: true,
		},
		{
			name: "non-matching mode returns false",
			caps: &AgentProviderCapabilities{
				ModelBindingModes: []ModelBindingMode{ModelBindingModeDeploymentRef},
			},
			mode: ModelBindingModeGatewayEndpoint,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.caps.HasBindingMode(tt.mode); got != tt.want {
				t.Errorf("HasBindingMode(%q) = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}

func TestAgentProviderCapabilities_HasProtocol(t *testing.T) {
	tests := []struct {
		name     string
		caps     *AgentProviderCapabilities
		protocol AgentToolProtocol
		want     bool
	}{
		{
			name:     "nil receiver returns false",
			caps:     nil,
			protocol: AgentToolProtocolMCP,
			want:     false,
		},
		{
			name:     "empty protocols returns false",
			caps:     &AgentProviderCapabilities{},
			protocol: AgentToolProtocolMCP,
			want:     false,
		},
		{
			name: "matching protocol returns true",
			caps: &AgentProviderCapabilities{
				Protocols: []AgentToolProtocol{AgentToolProtocolMCP, AgentToolProtocolA2A},
			},
			protocol: AgentToolProtocolA2A,
			want:     true,
		},
		{
			name: "non-matching protocol returns false",
			caps: &AgentProviderCapabilities{
				Protocols: []AgentToolProtocol{AgentToolProtocolMCP},
			},
			protocol: AgentToolProtocolOpenAITools,
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.caps.HasProtocol(tt.protocol); got != tt.want {
				t.Errorf("HasProtocol(%q) = %v, want %v", tt.protocol, got, tt.want)
			}
		})
	}
}

func TestAgentProviderConfigSpec_GetCatalogItem(t *testing.T) {
	spec := &AgentProviderConfigSpec{
		Catalog: []AgentCatalogItem{
			{Name: "kagent-k8s-sre", Title: "Kubernetes SRE"},
			{Name: "openclaw-personal-assistant", Title: "Personal Assistant"},
		},
	}

	t.Run("nil receiver returns nil", func(t *testing.T) {
		var s *AgentProviderConfigSpec
		if got := s.GetCatalogItem("anything"); got != nil {
			t.Errorf("expected nil from nil receiver, got %+v", got)
		}
	})

	t.Run("empty catalog returns nil", func(t *testing.T) {
		empty := &AgentProviderConfigSpec{}
		if got := empty.GetCatalogItem("anything"); got != nil {
			t.Errorf("expected nil from empty catalog, got %+v", got)
		}
	})

	t.Run("hit returns pointer into the slice", func(t *testing.T) {
		got := spec.GetCatalogItem("openclaw-personal-assistant")
		if got == nil {
			t.Fatal("expected catalog item, got nil")
		}
		if got.Title != "Personal Assistant" {
			t.Errorf("unexpected title: %q", got.Title)
		}
		// Confirm it's a pointer into the underlying slice so callers
		// can compare by identity. (See GetCatalogItem godoc: callers
		// must not mutate through this pointer.)
		if got != &spec.Catalog[1] {
			t.Error("GetCatalogItem should return a pointer into the underlying slice")
		}
	})

	t.Run("miss returns nil", func(t *testing.T) {
		if got := spec.GetCatalogItem("does-not-exist"); got != nil {
			t.Errorf("expected nil for missing name, got %+v", got)
		}
	})
}

func TestAgentProviderConfigSpec_CatalogItemNames(t *testing.T) {
	t.Run("nil receiver returns nil", func(t *testing.T) {
		var s *AgentProviderConfigSpec
		if got := s.CatalogItemNames(); got != nil {
			t.Errorf("expected nil from nil receiver, got %v", got)
		}
	})

	t.Run("empty catalog returns empty slice", func(t *testing.T) {
		s := &AgentProviderConfigSpec{}
		got := s.CatalogItemNames()
		if len(got) != 0 {
			t.Errorf("expected empty slice, got %v", got)
		}
	})

	t.Run("returns names in declaration order", func(t *testing.T) {
		s := &AgentProviderConfigSpec{
			Catalog: []AgentCatalogItem{
				{Name: "a"},
				{Name: "b"},
				{Name: "c"},
			},
		}
		want := []string{"a", "b", "c"}
		got := s.CatalogItemNames()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("CatalogItemNames() = %v, want %v", got, want)
		}
	})
}

// TestAgentProviderConfig_DeepCopy is a smoke test that the generated
// DeepCopy methods produce an independent object. Catches accidental
// shallow copies introduced by hand-edited zz_generated files (mirrors
// TestAgentDeployment_DeepCopy for the AgentProviderConfig type).
func TestAgentProviderConfig_DeepCopy(t *testing.T) {
	ready := true
	requiresOp := true
	now := metav1.Now()
	orig := &AgentProviderConfig{
		Spec: AgentProviderConfigSpec{
			Capabilities: &AgentProviderCapabilities{
				Backend:           AgentProviderBackendContainer,
				RequiresOperator:  &requiresOp,
				ModelBindingModes: []ModelBindingMode{ModelBindingModeDeploymentRef, ModelBindingModeExternalAPI},
				Protocols:         []AgentToolProtocol{AgentToolProtocolMCP, AgentToolProtocolOpenAITools},
			},
			Catalog: []AgentCatalogItem{
				{
					Name:        "openclaw-personal-assistant",
					Title:       "Personal Assistant",
					Description: "OpenClaw-powered personal automation",
					Tags:        []string{"personal", "automation"},
					Image:       "ghcr.io/openclaw/openclaw:latest",
					Template:    &runtime.RawExtension{Raw: []byte(`{"systemPrompt":"hi"}`)},
				},
			},
		},
		Status: AgentProviderConfigStatus{
			Ready:         &ready,
			Version:       "v0.1.0",
			LastHeartbeat: &now,
		},
	}

	cp := orig.DeepCopy()
	if cp == orig {
		t.Fatal("DeepCopy returned the same pointer")
	}
	if cp.Spec.Capabilities == orig.Spec.Capabilities {
		t.Error("Capabilities should be a fresh allocation, not shared")
	}
	if cp.Spec.Capabilities.RequiresOperator == orig.Spec.Capabilities.RequiresOperator {
		t.Error("Capabilities.RequiresOperator *bool should be a fresh allocation, not shared")
	}
	if &cp.Spec.Capabilities.ModelBindingModes[0] == &orig.Spec.Capabilities.ModelBindingModes[0] {
		t.Error("Capabilities.ModelBindingModes slice should be a fresh allocation, not shared")
	}
	if &cp.Spec.Catalog[0] == &orig.Spec.Catalog[0] {
		t.Error("Catalog slice should be a fresh allocation, not shared")
	}
	if cp.Spec.Catalog[0].Template == orig.Spec.Catalog[0].Template {
		t.Error("Catalog[0].Template RawExtension should be a fresh allocation, not shared")
	}
	if cp.Status.Ready == orig.Status.Ready {
		t.Error("Status.Ready *bool should be a fresh allocation, not shared")
	}
	if cp.Status.LastHeartbeat == orig.Status.LastHeartbeat {
		t.Error("Status.LastHeartbeat *Time should be a fresh allocation, not shared")
	}

	// Mutating the copy must not affect the original.
	*cp.Status.Ready = false
	if *orig.Status.Ready != true {
		t.Error("mutating copy Ready leaked into original")
	}
	cp.Spec.Catalog[0].Title = "Changed"
	if orig.Spec.Catalog[0].Title != "Personal Assistant" {
		t.Errorf("mutating copy Catalog[0].Title leaked into original: %q", orig.Spec.Catalog[0].Title)
	}
}

// TestAgentProviderConfig_DeepCopyObject confirms the runtime.Object
// interface is satisfied (so the type can be registered with a scheme).
func TestAgentProviderConfig_DeepCopyObject(t *testing.T) {
	var _ runtime.Object = (*AgentProviderConfig)(nil)
	var _ runtime.Object = (*AgentProviderConfigList)(nil)
}
