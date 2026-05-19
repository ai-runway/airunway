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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AgentProviderBackend describes the implementation strategy a
// framework provider uses to render AgentDeployments.
// +kubebuilder:validation:Enum=crd;container
type AgentProviderBackend string

const (
	// AgentProviderBackendCRD means the provider renders the
	// AgentDeployment into a framework-native custom resource (e.g.
	// Kagent renders to kagent.dev/Agent + ModelConfig).
	AgentProviderBackendCRD AgentProviderBackend = "crd"

	// AgentProviderBackendContainer means the provider renders the
	// AgentDeployment into plain Kubernetes workloads (Deployment +
	// Service + ConfigMap) using an image reference supplied by the
	// catalog entry. Used for non-Kubernetes-native agent frameworks
	// such as OpenClaw, CrewAI, LangGraph, and Hermes.
	AgentProviderBackendContainer AgentProviderBackend = "container"
)

// AgentToolProtocol identifies a tool-calling protocol the framework
// can natively consume.
// +kubebuilder:validation:Enum=mcp;a2a;openaiTools
type AgentToolProtocol string

const (
	// AgentToolProtocolMCP indicates Model Context Protocol support.
	AgentToolProtocolMCP AgentToolProtocol = "mcp"
	// AgentToolProtocolA2A indicates Google's Agent-to-Agent protocol support.
	AgentToolProtocolA2A AgentToolProtocol = "a2a"
	// AgentToolProtocolOpenAITools indicates OpenAI tool/function calling support.
	AgentToolProtocolOpenAITools AgentToolProtocol = "openaiTools"
)

// AgentProviderCapabilities declares what an agent framework can do.
type AgentProviderCapabilities struct {
	// modelBindingModes is the set of model-binding modes the framework
	// implementation natively supports. The core controller refuses
	// AgentDeployments whose binding mode is not in this set.
	// +optional
	ModelBindingModes []ModelBindingMode `json:"modelBindingModes,omitempty"`

	// protocols is the set of tool/agent protocols the framework
	// natively understands.
	// +optional
	Protocols []AgentToolProtocol `json:"protocols,omitempty"`

	// backend identifies the rendering strategy this provider uses. See
	// AgentProviderBackend for values.
	// +optional
	Backend AgentProviderBackend `json:"backend,omitempty"`

	// requiresOperator indicates the framework relies on an upstream
	// Kubernetes operator/CRD being installed in the cluster (e.g.
	// Kagent). The dashboard uses this to gate "install upstream"
	// flows. Mirrors InferenceProviderConfig.requiresCRD semantics.
	// +optional
	RequiresOperator *bool `json:"requiresOperator,omitempty"`
}

// AgentCatalogItem is a curated, one-click deployable recipe. The
// dashboard renders these on the marketplace browse page; selecting
// one prefills the deploy wizard with the bundled template.
//
// Inspired by vLLM production-stack recipes: shipping known-good
// combinations of model + framework + config eliminates the empty-form
// experience and matches the Ollama-style "one-line launch" UX target.
type AgentCatalogItem struct {
	// name is a stable, machine-readable identifier within the catalog
	// (e.g. "kagent-k8s-sre", "openclaw-personal-assistant").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// title is the human-facing recipe name shown in the UI.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Title string `json:"title"`

	// description explains what the recipe does, in plain language.
	// +optional
	Description string `json:"description,omitempty"`

	// icon is an optional URL to an icon shown in the catalog tile.
	// +optional
	Icon string `json:"icon,omitempty"`

	// tags categorise the recipe (e.g. ["devops", "observability"]).
	// +optional
	Tags []string `json:"tags,omitempty"`

	// image is the container image the catalog item runs when the
	// provider's backend is "container". Ignored for CRD-backed
	// providers, where the framework operator owns image selection.
	//
	// Carrying the image at the catalog level lets a single container-
	// based provider serve many frameworks (OpenClaw, CrewAI,
	// LangGraph, Hermes) by varying the catalog entry rather than the
	// provider code.
	// +optional
	Image string `json:"image,omitempty"`

	// recommendedSecurity ships sensible security defaults for this
	// recipe. The user can override them per AgentDeployment via
	// spec.security, but a known-good baseline here means most users
	// never have to. Per-recipe defaults are required because agent
	// frameworks have legitimately different requirements (e.g.
	// OpenClaw writes the local file system, so it cannot run with
	// readOnlyRootFilesystem=true).
	// +optional
	RecommendedSecurity *AgentSecuritySpec `json:"recommendedSecurity,omitempty"`

	// template is a partial AgentDeployment spec the dashboard
	// prefills into the deploy wizard when the user selects this
	// recipe. Stored as RawExtension so catalog authors can ship
	// framework-specific config without the core controller learning
	// every framework's schema.
	// +optional
	Template *runtime.RawExtension `json:"template,omitempty"`
}

// AgentProviderConfigSpec defines the registration for an agent
// framework provider.
type AgentProviderConfigSpec struct {
	// capabilities declares what this framework supports.
	// +optional
	Capabilities *AgentProviderCapabilities `json:"capabilities,omitempty"`

	// catalog lists deployable recipes (curated model+agent combos)
	// the dashboard surfaces under this framework on the marketplace
	// page. Recipes are not required; a provider can register
	// itself with no catalog and still accept hand-written
	// AgentDeployments.
	// +optional
	// +listType=map
	// +listMapKey=name
	Catalog []AgentCatalogItem `json:"catalog,omitempty"`
}

// AgentProviderConfigStatus is written by the framework provider.
type AgentProviderConfigStatus struct {
	// ready indicates whether the framework provider controller is
	// healthy and willing to accept AgentDeployments. The dashboard
	// uses this for the marketplace tile state.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// version is the running provider controller version. Useful for
	// surfacing shim drift between the dashboard and the provider.
	// +optional
	Version string `json:"version,omitempty"`

	// lastHeartbeat is the most recent provider status write. The
	// dashboard treats stale heartbeats as the provider being unhealthy.
	// +optional
	LastHeartbeat *metav1.Time `json:"lastHeartbeat,omitempty"`

	// conditions follow the standard Kubernetes condition pattern.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=apc
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=".spec.capabilities.backend"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.version"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// AgentProviderConfig registers an agent framework with AI Runway. It is
// the agent-marketplace analogue of InferenceProviderConfig.
type AgentProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentProviderConfigSpec   `json:"spec,omitempty"`
	Status AgentProviderConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentProviderConfigList contains a list of AgentProviderConfig.
type AgentProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentProviderConfig `json:"items"`
}

// HasBindingMode reports whether the provider declares native support
// for the given model-binding mode.
func (c *AgentProviderCapabilities) HasBindingMode(mode ModelBindingMode) bool {
	if c == nil {
		return false
	}
	for _, m := range c.ModelBindingModes {
		if m == mode {
			return true
		}
	}
	return false
}

// HasProtocol reports whether the provider declares native support for
// the given tool/agent protocol.
func (c *AgentProviderCapabilities) HasProtocol(p AgentToolProtocol) bool {
	if c == nil {
		return false
	}
	for _, x := range c.Protocols {
		if x == p {
			return true
		}
	}
	return false
}

// GetCatalogItem returns the catalog item with the given name, or nil
// when the spec has no matching entry.
func (s *AgentProviderConfigSpec) GetCatalogItem(name string) *AgentCatalogItem {
	if s == nil {
		return nil
	}
	for i := range s.Catalog {
		if s.Catalog[i].Name == name {
			return &s.Catalog[i]
		}
	}
	return nil
}

// CatalogItemNames returns the catalog item names in declaration order.
func (s *AgentProviderConfigSpec) CatalogItemNames() []string {
	if s == nil {
		return nil
	}
	names := make([]string, len(s.Catalog))
	for i := range s.Catalog {
		names[i] = s.Catalog[i].Name
	}
	return names
}

func init() {
	SchemeBuilder.Register(&AgentProviderConfig{}, &AgentProviderConfigList{})
}
