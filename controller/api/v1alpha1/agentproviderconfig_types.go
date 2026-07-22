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
	"fmt"
	"strings"

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

const (
	// AgentProviderCatalogAnnotation stores the provider's marketplace catalog as
	// a JSON array of AgentCatalogItem values on AgentProviderConfig metadata.
	//
	// Catalog data is UI metadata, not reconciled runtime intent, so it lives on
	// annotations rather than spec.
	AgentProviderCatalogAnnotation = "airunway.ai/agent-catalog"

	// AgentProviderInstallInstructionsAnnotation carries plain-text install
	// instructions for the provider's upstream operator/shim. The readiness
	// controller surfaces this when a required operator is missing.
	AgentProviderInstallInstructionsAnnotation = "airunway.ai/install-instructions"
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

	// backend identifies the rendering strategy this provider uses
	// (crd-native vs container-based). It is required because the core
	// controller cannot determine how to render an agent workload
	// without it, and it backs the Backend printer column. It is immutable:
	// changing it would hand already-admitted agents to a different reconciler
	// while the original keeps filtering by framework name, so both would
	// render and force-own the same resources. See AgentProviderBackend for
	// values.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="backend is immutable"
	Backend AgentProviderBackend `json:"backend"`

	// requiresOperator indicates the framework relies on an upstream
	// Kubernetes operator/CRD being installed in the cluster (e.g.
	// Kagent). The dashboard uses this to gate "install upstream"
	// flows. Mirrors InferenceProviderConfig.requiresCRD semantics.
	// +optional
	RequiresOperator *bool `json:"requiresOperator,omitempty"`

	// operatorAPIGroup is the Kubernetes API group of the upstream
	// operator this framework renders into (e.g. "kagent.dev" or
	// "core.orka.ai"). When set for a crd backend, the controller marks
	// the provider ready only once this API group is served in the
	// cluster, so agents are never rendered before the operator is
	// installed. Ignored for container backends, which have no operator.
	// +optional
	OperatorAPIGroup string `json:"operatorAPIGroup,omitempty"`

	// writableRootFilesystem relaxes the container backend's hardened
	// read-only root filesystem for frameworks that legitimately need a
	// writable root (e.g. images that write outside /tmp). This is a
	// provider-owned property of the framework, NOT a user-facing knob:
	// it lives here on the framework's capabilities, not on an
	// AgentDeployment, so a deployment author cannot weaken the security
	// posture. A writable /tmp is always provided regardless. Ignored for
	// crd backends, whose upstream operator owns pod security.
	// +optional
	WritableRootFilesystem *bool `json:"writableRootFilesystem,omitempty"`
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
	// capabilities declares what this framework supports. Required
	// because the core controller cannot render an agent workload
	// without knowing the provider's backend (see
	// AgentProviderCapabilities.Backend).
	// +kubebuilder:validation:Required
	Capabilities *AgentProviderCapabilities `json:"capabilities"`
}

// AgentProviderConfigStatus is written by the framework provider.
type AgentProviderConfigStatus struct {
	// ready indicates whether the framework provider controller is
	// healthy and willing to accept AgentDeployments. The dashboard
	// uses this for the marketplace tile state.
	//
	// Pointer-to-bool so providers can distinguish "not yet reported"
	// (nil) from "explicitly not ready" (false). A plain bool with
	// omitempty would silently collapse those two states.
	// +optional
	Ready *bool `json:"ready,omitempty"`

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

// CatalogItems decodes the provider's catalog from
// metadata.annotations[airunway.ai/agent-catalog].
//
// A missing annotation returns (nil, nil). Invalid JSON or invalid item
// content returns an error.
func (c *AgentProviderConfig) CatalogItems() ([]AgentCatalogItem, error) {
	if c == nil || len(c.Annotations) == 0 {
		return nil, nil
	}
	raw := c.Annotations[AgentProviderCatalogAnnotation]
	if raw == "" {
		return nil, nil
	}
	var items []AgentCatalogItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("invalid %q annotation: %w", AgentProviderCatalogAnnotation, err)
	}
	if err := validateCatalogItems(items); err != nil {
		return nil, fmt.Errorf("invalid %q annotation: %w", AgentProviderCatalogAnnotation, err)
	}
	return items, nil
}

// GetCatalogItem returns one catalog item by name.
func (c *AgentProviderConfig) GetCatalogItem(name string) (AgentCatalogItem, bool, error) {
	items, err := c.CatalogItems()
	if err != nil {
		return AgentCatalogItem{}, false, err
	}
	for i := range items {
		if items[i].Name == name {
			return items[i], true, nil
		}
	}
	return AgentCatalogItem{}, false, nil
}

// CatalogItemNames returns the catalog item names in declaration order.
func (c *AgentProviderConfig) CatalogItemNames() ([]string, error) {
	items, err := c.CatalogItems()
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	names := make([]string, len(items))
	for i := range items {
		names[i] = items[i].Name
	}
	return names, nil
}

// InstallInstructions returns the provider's optional operator install hint.
func (c *AgentProviderConfig) InstallInstructions() string {
	if c == nil || len(c.Annotations) == 0 {
		return ""
	}
	return strings.TrimSpace(c.Annotations[AgentProviderInstallInstructionsAnnotation])
}

func validateCatalogItems(items []AgentCatalogItem) error {
	seen := make(map[string]struct{}, len(items))
	for i := range items {
		item := items[i]
		if item.Name == "" {
			return fmt.Errorf("catalog[%d].name: required", i)
		}
		if item.Title == "" {
			return fmt.Errorf("catalog[%d].title: required", i)
		}
		if _, ok := seen[item.Name]; ok {
			return fmt.Errorf("catalog[%d].name %q: duplicate", i, item.Name)
		}
		seen[item.Name] = struct{}{}
	}
	return nil
}

func init() {
	SchemeBuilder.Register(&AgentProviderConfig{}, &AgentProviderConfigList{})
}
