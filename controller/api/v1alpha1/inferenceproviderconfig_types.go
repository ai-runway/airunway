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

const (
	// AnnotationInstallation is the annotation key for provider installation metadata (JSON-encoded InstallationInfo).
	AnnotationInstallation = "airunway.ai/installation"

	// AnnotationDocumentation is the annotation key for the provider documentation URL.
	AnnotationDocumentation = "airunway.ai/documentation"
)

// EngineCapability defines per-engine capability metadata
type EngineCapability struct {
	// name is the inference engine type
	// +kubebuilder:validation:Required
	Name EngineType `json:"name"`

	// servingModes is the list of serving modes this engine supports
	// +optional
	ServingModes []ServingMode `json:"servingModes,omitempty"`

	// gpuSupport indicates if this engine supports GPU inference
	// +optional
	GPUSupport bool `json:"gpuSupport,omitempty"`

	// cpuSupport indicates if this engine supports CPU-only inference
	// +optional
	CPUSupport bool `json:"cpuSupport,omitempty"`

	// requiresCRD indicates if this engine needs an upstream CRD/operator installation.
	// When omitted, clients should treat this as true for backward compatibility.
	// +optional
	RequiresCRD *bool `json:"requiresCRD,omitempty"`

	// gateway defines this engine's gateway-related capabilities.
	// +optional
	Gateway *GatewayCapabilities `json:"gateway,omitempty"`
}

// ProviderCapabilities defines what a provider supports.
//
// NOTE: the legacy-schema migration in
// controller/internal/controller/migration.go unconditionally strips the
// top-level keys listed in `legacyFlatKeys` (servingModes, gpuSupport,
// cpuSupport, requiresCRD, gateway) from spec.capabilities. When adding a
// new top-level field here, avoid those JSON tag names — or update the
// migration to preserve the new field — otherwise it will be silently
// dropped at controller startup.
type ProviderCapabilities struct {
	// engines is the list of supported inference engines with per-engine capabilities
	// +listType=map
	// +listMapKey=name
	// +optional
	Engines []EngineCapability `json:"engines,omitempty"`
}

// GatewayCapabilities defines gateway-related capabilities for a specific engine.
type GatewayCapabilities struct {
	// managesInferencePool indicates that the provider's operator creates and
	// owns the GAIE InferencePool (and EPP) for ModelDeployments using this
	// engine. When true, the airunway controller will not create an
	// InferencePool itself: it waits for the provider-managed one to appear
	// and uses it as the HTTPRoute backend. When false (the default), the
	// controller creates and manages the InferencePool/EPP, even if other
	// fields on this struct (e.g. ignoresServedName) are set.
	// +optional
	ManagesInferencePool bool `json:"managesInferencePool,omitempty"`

	// inferencePoolNamePattern is the naming pattern for provider-created pools.
	// Supports {name} and {namespace} placeholders. Only consulted when
	// managesInferencePool is true.
	// +optional
	InferencePoolNamePattern string `json:"inferencePoolNamePattern,omitempty"`

	// inferencePoolNamespace is the namespace where the provider creates its InferencePool.
	// Supports {name} and {namespace} placeholders (resolved from the ModelDeployment).
	// When the resolved namespace differs from the ModelDeployment namespace, the
	// controller creates a ReferenceGrant for cross-namespace HTTPRoute routing.
	// +optional
	InferencePoolNamespace string `json:"inferencePoolNamespace,omitempty"`

	// ignoresServedName indicates that gateway routing for this provider+engine
	// pair does not honor spec.model.servedName, so the controller should fall
	// back to auto-discovery / spec.model.id when computing the route model
	// name. Set this when the provider's serving mode for this engine does not
	// expose the OpenAI-style served name (e.g. KAITO's llama.cpp deployment).
	// +optional
	IgnoresServedName bool `json:"ignoresServedName,omitempty"`
}

// HasEngine returns true if the provider supports the given engine type
func (c *ProviderCapabilities) HasEngine(engine EngineType) bool {
	return c.GetEngineCapability(engine) != nil
}

// GetEngineCapability returns the capability for the given engine type, or nil if not found
func (c *ProviderCapabilities) GetEngineCapability(engine EngineType) *EngineCapability {
	if c == nil {
		return nil
	}
	for i := range c.Engines {
		if c.Engines[i].Name == engine {
			return &c.Engines[i]
		}
	}
	return nil
}

// SupportsServingMode returns true if this engine supports the specified serving mode.
func (e *EngineCapability) SupportsServingMode(mode ServingMode) bool {
	if e == nil {
		return false
	}
	for _, sm := range e.ServingModes {
		if sm == mode {
			return true
		}
	}
	return false
}

// SupportsGPU returns true if this engine supports GPU inference.
func (e *EngineCapability) SupportsGPU() bool {
	if e == nil {
		return false
	}
	return e.GPUSupport
}

// SupportsCPU returns true if this engine supports CPU-only inference.
func (e *EngineCapability) SupportsCPU() bool {
	if e == nil {
		return false
	}
	return e.CPUSupport
}

// SupportsServingMode returns true if the given engine supports the specified serving mode
func (c *ProviderCapabilities) SupportsServingMode(engine EngineType, mode ServingMode) bool {
	return c.GetEngineCapability(engine).SupportsServingMode(mode)
}

// SupportsGPU returns true if the given engine supports GPU inference
func (c *ProviderCapabilities) SupportsGPU(engine EngineType) bool {
	return c.GetEngineCapability(engine).SupportsGPU()
}

// SupportsCPU returns true if the given engine supports CPU-only inference
func (c *ProviderCapabilities) SupportsCPU(engine EngineType) bool {
	return c.GetEngineCapability(engine).SupportsCPU()
}

// EngineNames returns a list of all engine types supported by this provider
func (c *ProviderCapabilities) EngineNames() []EngineType {
	if c == nil {
		return nil
	}
	names := make([]EngineType, len(c.Engines))
	for i, e := range c.Engines {
		names[i] = e.Name
	}
	return names
}

// HelmRepo defines a Helm repository needed for installation
type HelmRepo struct {
	// name is the local name for the Helm repository
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// url is the Helm repository URL
	// +kubebuilder:validation:Required
	URL string `json:"url"`
}

// HelmChart defines a Helm chart to install
type HelmChart struct {
	// name is the release name for the Helm chart
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// chart is the chart reference (e.g. "repo/chart" or a URL)
	// +kubebuilder:validation:Required
	Chart string `json:"chart"`

	// version is the chart version to install
	// +optional
	Version string `json:"version,omitempty"`

	// namespace is the Kubernetes namespace to install into
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// createNamespace indicates whether to create the namespace if it doesn't exist
	// +optional
	CreateNamespace bool `json:"createNamespace,omitempty"`

	// values is a JSON object of Helm --set-json overrides.
	// Each top-level key is the Helm values path to pass as the --set-json key,
	// and each top-level value is the JSON payload for that path. This is not
	// treated as a single arbitrary Helm values document.
	// +optional
	Values *runtime.RawExtension `json:"values,omitempty"`
}

// InstallationStep defines a step in the provider installation process
type InstallationStep struct {
	// title is a short description of this step
	// +kubebuilder:validation:Required
	Title string `json:"title"`

	// command is the shell command to run for this step
	// +optional
	Command string `json:"command,omitempty"`

	// description is a detailed explanation of what this step does
	// +kubebuilder:validation:Required
	Description string `json:"description"`
}

// InstallationInfo defines how to install the provider's upstream components
type InstallationInfo struct {
	// description is a human-readable description of the provider
	// +optional
	Description string `json:"description,omitempty"`

	// defaultNamespace is the default namespace for the provider's workloads
	// +optional
	DefaultNamespace string `json:"defaultNamespace,omitempty"`

	// helmRepos are the Helm repositories needed for installation
	// +optional
	HelmRepos []HelmRepo `json:"helmRepos,omitempty"`

	// helmCharts are the Helm charts to install
	// +optional
	HelmCharts []HelmChart `json:"helmCharts,omitempty"`

	// steps are the ordered installation steps with commands
	// +optional
	Steps []InstallationStep `json:"steps,omitempty"`
}

// SelectionRule defines a rule for auto-selecting this provider
type SelectionRule struct {
	// condition is a CEL expression that evaluates to true when this rule matches
	// The expression has access to the full ModelDeployment spec via `spec.*`
	// Examples:
	//   - "!has(spec.resources.gpu) || spec.resources.gpu.count == 0"
	//   - "spec.engine.type == 'llamacpp'"
	//   - "has(spec.resources.gpu) && spec.resources.gpu.count > 0"
	// +kubebuilder:validation:Required
	Condition string `json:"condition"`

	// priority is the priority of this rule (higher values = higher priority)
	// When multiple providers match, the one with the highest priority wins
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	Priority int32 `json:"priority,omitempty"`
}

// InferenceProviderConfigSpec defines the desired state of InferenceProviderConfig
type InferenceProviderConfigSpec struct {
	// capabilities defines what this provider supports
	// +optional
	Capabilities *ProviderCapabilities `json:"capabilities,omitempty"`

	// selectionRules defines rules for auto-selecting this provider
	// Conditions use CEL (Common Expression Language)
	// +optional
	SelectionRules []SelectionRule `json:"selectionRules,omitempty"`
}

// InferenceProviderConfigStatus defines the observed state of InferenceProviderConfig.
type InferenceProviderConfigStatus struct {
	// ready indicates if the provider is ready to accept workloads
	// +optional
	Ready bool `json:"ready,omitempty"`

	// version is the version of the provider controller
	// +optional
	Version string `json:"version,omitempty"`

	// lastHeartbeat is the last time the provider controller updated this status
	// +optional
	LastHeartbeat *metav1.Time `json:"lastHeartbeat,omitempty"`

	// upstreamCRDVersion is the API version of the upstream CRD this provider creates
	// +optional
	UpstreamCRDVersion string `json:"upstreamCRDVersion,omitempty"`

	// upstreamSchemaHash is a hash of the upstream CRD schema for version detection
	// +optional
	UpstreamSchemaHash string `json:"upstreamSchemaHash,omitempty"`

	// conditions represent the current state of the InferenceProviderConfig resource
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready",description="Provider ready"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.version",description="Provider version"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// InferenceProviderConfig is the Schema for the inferenceproviderconfigs API
// InferenceProviderConfig is a cluster-scoped resource that providers use to register themselves
type InferenceProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the provider capabilities and selection rules
	// +optional
	Spec InferenceProviderConfigSpec `json:"spec,omitempty"`

	// status is written by the provider controller
	// +optional
	Status InferenceProviderConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// InferenceProviderConfigList contains a list of InferenceProviderConfig
type InferenceProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferenceProviderConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InferenceProviderConfig{}, &InferenceProviderConfigList{})
}
