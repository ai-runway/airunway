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
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func catalogJSON(t *testing.T, items []AgentCatalogItem) string {
	t.Helper()
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	return string(raw)
}

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

func TestAgentProviderConfig_CatalogItems(t *testing.T) {
	t.Run("nil receiver returns nil", func(t *testing.T) {
		var apc *AgentProviderConfig
		got, err := apc.CatalogItems()
		if err != nil {
			t.Fatalf("CatalogItems() error = %v", err)
		}
		if got != nil {
			t.Errorf("expected nil from nil receiver, got %v", got)
		}
	})

	t.Run("missing annotation returns nil", func(t *testing.T) {
		apc := &AgentProviderConfig{}
		got, err := apc.CatalogItems()
		if err != nil {
			t.Fatalf("CatalogItems() error = %v", err)
		}
		if got != nil {
			t.Errorf("expected nil from missing annotation, got %v", got)
		}
	})

	t.Run("annotation parses", func(t *testing.T) {
		apc := &AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					AgentProviderCatalogAnnotation: catalogJSON(t, []AgentCatalogItem{
						{Name: "kagent-k8s-sre", Title: "Kubernetes SRE"},
						{Name: "openclaw-personal-assistant", Title: "Personal Assistant"},
					}),
				},
			},
		}
		got, err := apc.CatalogItems()
		if err != nil {
			t.Fatalf("CatalogItems() error = %v", err)
		}
		if len(got) != 2 || got[1].Title != "Personal Assistant" {
			t.Errorf("unexpected parsed catalog: %+v", got)
		}
	})

	t.Run("invalid json returns error", func(t *testing.T) {
		apc := &AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					AgentProviderCatalogAnnotation: "{not-json",
				},
			},
		}
		_, err := apc.CatalogItems()
		if err == nil {
			t.Fatal("expected parse error, got nil")
		}
		if !strings.Contains(err.Error(), AgentProviderCatalogAnnotation) {
			t.Errorf("expected error mentioning annotation key, got %v", err)
		}
	})

	t.Run("duplicate names returns error", func(t *testing.T) {
		apc := &AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					AgentProviderCatalogAnnotation: catalogJSON(t, []AgentCatalogItem{
						{Name: "dup", Title: "One"},
						{Name: "dup", Title: "Two"},
					}),
				},
			},
		}
		_, err := apc.CatalogItems()
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}
		if !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("expected duplicate error, got %v", err)
		}
	})
}

func TestAgentProviderConfig_GetCatalogItem(t *testing.T) {
	apc := &AgentProviderConfig{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AgentProviderCatalogAnnotation: catalogJSON(t, []AgentCatalogItem{
					{Name: "kagent-k8s-sre", Title: "Kubernetes SRE"},
					{Name: "openclaw-personal-assistant", Title: "Personal Assistant"},
				}),
			},
		},
	}

	item, ok, err := apc.GetCatalogItem("openclaw-personal-assistant")
	if err != nil {
		t.Fatalf("GetCatalogItem() error = %v", err)
	}
	if !ok {
		t.Fatal("expected catalog hit, got miss")
	}
	if item.Title != "Personal Assistant" {
		t.Errorf("unexpected item title: %q", item.Title)
	}

	_, ok, err = apc.GetCatalogItem("does-not-exist")
	if err != nil {
		t.Fatalf("GetCatalogItem() miss error = %v", err)
	}
	if ok {
		t.Fatal("expected miss, got hit")
	}
}

func TestAgentProviderConfig_CatalogItemNames(t *testing.T) {
	t.Run("nil receiver returns nil", func(t *testing.T) {
		var apc *AgentProviderConfig
		got, err := apc.CatalogItemNames()
		if err != nil {
			t.Fatalf("CatalogItemNames() error = %v", err)
		}
		if got != nil {
			t.Errorf("expected nil from nil receiver, got %v", got)
		}
	})

	t.Run("returns names in declaration order", func(t *testing.T) {
		apc := &AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					AgentProviderCatalogAnnotation: catalogJSON(t, []AgentCatalogItem{
						{Name: "a", Title: "A"},
						{Name: "b", Title: "B"},
						{Name: "c", Title: "C"},
					}),
				},
			},
		}
		want := []string{"a", "b", "c"}
		got, err := apc.CatalogItemNames()
		if err != nil {
			t.Fatalf("CatalogItemNames() error = %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("CatalogItemNames() = %v, want %v", got, want)
		}
	})
}

func TestAgentProviderConfig_InstallInstructions(t *testing.T) {
	t.Run("nil receiver returns empty", func(t *testing.T) {
		var apc *AgentProviderConfig
		if got := apc.InstallInstructions(); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("trims and returns annotation value", func(t *testing.T) {
		apc := &AgentProviderConfig{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					AgentProviderInstallInstructionsAnnotation: "  kubectl apply -f https://example.com/install.yaml  ",
				},
			},
		}
		if got := apc.InstallInstructions(); got != "kubectl apply -f https://example.com/install.yaml" {
			t.Errorf("InstallInstructions() = %q", got)
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
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AgentProviderCatalogAnnotation: catalogJSON(t, []AgentCatalogItem{
					{
						Name:        "openclaw-personal-assistant",
						Title:       "Personal Assistant",
						Description: "OpenClaw-powered personal automation",
						Tags:        []string{"personal", "automation"},
						Image:       "ghcr.io/openclaw/openclaw:latest",
						Template:    &runtime.RawExtension{Raw: []byte(`{"systemPrompt":"hi"}`)},
					},
				}),
			},
		},
		Spec: AgentProviderConfigSpec{
			Capabilities: &AgentProviderCapabilities{
				Backend:           AgentProviderBackendContainer,
				RequiresOperator:  &requiresOp,
				ModelBindingModes: []ModelBindingMode{ModelBindingModeDeploymentRef, ModelBindingModeExternalAPI},
				Protocols:         []AgentToolProtocol{AgentToolProtocolMCP, AgentToolProtocolOpenAITools},
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
	if cp.Annotations == nil {
		t.Fatal("Annotations should be copied")
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
	cp.Annotations[AgentProviderCatalogAnnotation] = "changed"
	if orig.Annotations[AgentProviderCatalogAnnotation] == "changed" {
		t.Error("mutating copy annotation leaked into original")
	}
}

// TestAgentProviderConfig_DeepCopyObject confirms the runtime.Object
// interface is satisfied (so the type can be registered with a scheme).
func TestAgentProviderConfig_DeepCopyObject(t *testing.T) {
	var _ runtime.Object = (*AgentProviderConfig)(nil)
	var _ runtime.Object = (*AgentProviderConfigList)(nil)
}
