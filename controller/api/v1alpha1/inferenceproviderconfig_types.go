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

// ProviderCapabilities defines what a provider supports
type ProviderCapabilities struct {
	// engines is the list of supported inference engines
	// +optional
	Engines []EngineType `json:"engines,omitempty"`

	// servingModes is the list of supported serving modes
	// +optional
	ServingModes []ServingMode `json:"servingModes,omitempty"`

	// cpuSupport indicates if the provider supports CPU-only inference
	// +optional
	CPUSupport bool `json:"cpuSupport,omitempty"`

	// gpuSupport indicates if the provider supports GPU inference
	// +optional
	GPUSupport bool `json:"gpuSupport,omitempty"`

	// requiresCRD indicates if this provider needs an upstream CRD/operator installation.
	// When omitted, clients should treat this as true for backward compatibility.
	// +optional
	RequiresCRD *bool `json:"requiresCRD,omitempty"`

	// gateway defines the provider's gateway-related capabilities.
	// +optional
	Gateway *GatewayCapabilities `json:"gateway,omitempty"`
}

// GatewayCapabilities defines gateway-related capabilities for a specific provider.
type GatewayCapabilities struct {
	// inferencePoolNamePattern is the naming pattern for provider-created pools.
	// Supports {name} and {namespace} placeholders.
	// +optional
	InferencePoolNamePattern string `json:"inferencePoolNamePattern,omitempty"`

	// inferencePoolNamespace is the namespace where the provider creates its InferencePool.
	// Supports {name} and {namespace} placeholders (resolved from the ModelDeployment).
	// When the resolved namespace differs from the ModelDeployment namespace, the
	// controller creates a ReferenceGrant for cross-namespace HTTPRoute routing.
	// +optional
	InferencePoolNamespace string `json:"inferencePoolNamespace,omitempty"`
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
