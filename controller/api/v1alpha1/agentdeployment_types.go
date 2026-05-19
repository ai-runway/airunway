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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AgentPhase defines the phase of the agent deployment.
// +kubebuilder:validation:Enum=Pending;Deploying;Running;Failed;Terminating
type AgentPhase string

const (
	AgentPhasePending     AgentPhase = "Pending"
	AgentPhaseDeploying   AgentPhase = "Deploying"
	AgentPhaseRunning     AgentPhase = "Running"
	AgentPhaseFailed      AgentPhase = "Failed"
	AgentPhaseTerminating AgentPhase = "Terminating"
)

// ModelBindingMode identifies how the agent resolves its model endpoint.
// +kubebuilder:validation:Enum=deploymentRef;gatewayEndpoint;externalAPI
type ModelBindingMode string

const (
	// ModelBindingModeDeploymentRef binds the agent to an in-cluster ModelDeployment.
	ModelBindingModeDeploymentRef ModelBindingMode = "deploymentRef"
	// ModelBindingModeGatewayEndpoint binds the agent to a model exposed through Gateway API (GAIE).
	ModelBindingModeGatewayEndpoint ModelBindingMode = "gatewayEndpoint"
	// ModelBindingModeExternalAPI binds the agent to an external OpenAI-compatible endpoint
	// (e.g. OpenAI, Anthropic, Azure OpenAI, or any compatible third party).
	ModelBindingModeExternalAPI ModelBindingMode = "externalAPI"
)

// PodSecurityStandard mirrors the Kubernetes Pod Security Standards.
// +kubebuilder:validation:Enum=privileged;baseline;restricted
type PodSecurityStandard string

const (
	PodSecurityStandardPrivileged PodSecurityStandard = "privileged"
	PodSecurityStandardBaseline   PodSecurityStandard = "baseline"
	PodSecurityStandardRestricted PodSecurityStandard = "restricted"
)

// AgentFrameworkRef identifies which agent framework provider should
// reconcile this AgentDeployment. The name must match an
// AgentProviderConfig.metadata.name registered in the cluster.
type AgentFrameworkRef struct {
	// name is the framework identifier, e.g. "kagent", "openclaw",
	// "crewai", "langgraph". Must match an AgentProviderConfig.metadata.name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// version optionally pins a framework provider version. When empty,
	// providers use their default version. Format is provider-defined.
	// +optional
	Version string `json:"version,omitempty"`
}

// ModelDeploymentBinding binds the agent to an in-cluster ModelDeployment.
type ModelDeploymentBinding struct {
	// name is the ModelDeployment name to bind to.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// namespace defaults to the AgentDeployment's namespace when empty.
	// Cross-namespace references require an AgentReferenceGrant (Phase 3).
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// GatewayEndpointBinding binds the agent to a model exposed via Gateway API
// (Gateway API Inference Extension).
type GatewayEndpointBinding struct {
	// gatewayRef points at the Gateway resource that serves the model.
	// +kubebuilder:validation:Required
	GatewayRef GatewayResourceRef `json:"gatewayRef"`

	// modelName is the served model name advertised through the gateway.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ModelName string `json:"modelName"`
}

// GatewayResourceRef references a Gateway resource by name/namespace.
type GatewayResourceRef struct {
	// name of the Gateway resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// namespace of the Gateway resource.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ExternalAPIType is the well-known shape of an external API endpoint.
// +kubebuilder:validation:Enum=openai;anthropic;azureOpenAI;custom
type ExternalAPIType string

const (
	ExternalAPITypeOpenAI      ExternalAPIType = "openai"
	ExternalAPITypeAnthropic   ExternalAPIType = "anthropic"
	ExternalAPITypeAzureOpenAI ExternalAPIType = "azureOpenAI"
	ExternalAPITypeCustom      ExternalAPIType = "custom"
)

// ExternalAPIBinding binds the agent to an external OpenAI-compatible API
// (e.g. OpenAI, Anthropic, Azure OpenAI, or a custom OpenAI-compatible host).
type ExternalAPIBinding struct {
	// type identifies the well-known API shape. Use "custom" for any
	// OpenAI-compatible endpoint not covered by the named types.
	// +kubebuilder:validation:Required
	Type ExternalAPIType `json:"type"`

	// baseURL is the OpenAI-compatible base URL, e.g. "https://api.openai.com/v1".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	BaseURL string `json:"baseURL"`

	// modelName is the model identifier the agent will request.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ModelName string `json:"modelName"`

	// credentialsRef points at a Secret holding the API credentials.
	// +optional
	CredentialsRef *SecretKeyRef `json:"credentialsRef,omitempty"`
}

// SecretKeyRef identifies a key within a Secret.
type SecretKeyRef struct {
	// name of the Secret in the AgentDeployment's namespace.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// key inside the Secret to read.
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// ModelBinding describes how the agent connects to its model. Exactly one
// of deploymentRef, gatewayEndpoint, or externalAPI must be set.
// +kubebuilder:validation:XValidation:rule="((has(self.deploymentRef)?1:0)+(has(self.gatewayEndpoint)?1:0)+(has(self.externalAPI)?1:0)) == 1",message="exactly one of deploymentRef, gatewayEndpoint, or externalAPI must be set"
type ModelBinding struct {
	// deploymentRef binds to an in-cluster ModelDeployment.
	// +optional
	DeploymentRef *ModelDeploymentBinding `json:"deploymentRef,omitempty"`

	// gatewayEndpoint binds to a model exposed through Gateway API (GAIE).
	// +optional
	GatewayEndpoint *GatewayEndpointBinding `json:"gatewayEndpoint,omitempty"`

	// externalAPI binds to an external OpenAI-compatible endpoint.
	// +optional
	ExternalAPI *ExternalAPIBinding `json:"externalAPI,omitempty"`
}

// AgentSecuritySpec captures security/isolation policy for the agent.
//
// The shape lives in the API contract from day one so that provider
// implementations can converge on a consistent model. Individual
// framework providers ship recommended defaults in their
// AgentProviderConfig catalog entries; users can override here per
// AgentDeployment.
type AgentSecuritySpec struct {
	// podSecurityStandard names the Kubernetes Pod Security Standard the
	// rendered pod should comply with. Providers translate this into
	// concrete defaults (e.g. dropped capabilities, no host namespaces).
	// +optional
	PodSecurityStandard PodSecurityStandard `json:"podSecurityStandard,omitempty"`

	// securityContext is a pod-level SecurityContext override that the
	// provider applies to the rendered agent workload. Provider defaults
	// (from the AgentProviderConfig catalog entry) are used when this is
	// unset; non-nil fields here override the provider defaults
	// field-by-field.
	// +optional
	SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
}

// AgentResourceSpec describes compute resources requested for the agent.
type AgentResourceSpec struct {
	// requests sets the minimum CPU and memory required for the agent.
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`

	// limits sets the maximum CPU and memory the agent may use.
	// +optional
	Limits corev1.ResourceList `json:"limits,omitempty"`
}

// AgentObservabilitySpec configures observability emission for the agent.
type AgentObservabilitySpec struct {
	// otlp configures OpenTelemetry export. When set, providers MUST
	// inject OTEL_EXPORTER_OTLP_* environment variables matching this
	// configuration into the rendered agent workload.
	// +optional
	OTLP *OTLPSpec `json:"otlp,omitempty"`
}

// OTLPSpec configures an OTLP exporter target.
type OTLPSpec struct {
	// endpoint is the OTLP collector endpoint URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// protocol identifies the OTLP transport (http/protobuf, grpc, ...).
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// AgentDeploymentSpec defines the desired state of an AgentDeployment.
type AgentDeploymentSpec struct {
	// framework selects which agent framework provider reconciles this
	// AgentDeployment. The provider must have a matching
	// AgentProviderConfig registered in the cluster.
	// +kubebuilder:validation:Required
	Framework AgentFrameworkRef `json:"framework"`

	// model describes how the agent resolves its model endpoint. Exactly
	// one binding mode must be set.
	// +kubebuilder:validation:Required
	Model ModelBinding `json:"model"`

	// config carries framework-specific configuration (e.g. system
	// prompt, skills, crew definition, graph definition).
	//
	// The shape is defined by the framework provider, validated against
	// its JSON schema during reconciliation. Use RawExtension here so
	// the core controller does not need to learn every framework's
	// schema; provider controllers parse and validate this field
	// themselves.
	// +optional
	Config *runtime.RawExtension `json:"config,omitempty"`

	// resources sets resource requests/limits for the rendered agent
	// workload. Providers translate this into native scheduling hints
	// (e.g. container resources on a Deployment, or framework-specific
	// fields on a native CR).
	// +optional
	Resources *AgentResourceSpec `json:"resources,omitempty"`

	// security captures security/isolation policy for the rendered
	// workload. See AgentSecuritySpec for layering semantics.
	// +optional
	Security *AgentSecuritySpec `json:"security,omitempty"`

	// observability configures observability emission (OTLP export).
	// +optional
	Observability *AgentObservabilitySpec `json:"observability,omitempty"`
}

// ModelBindingStatus is the resolved binding the provider should consume.
//
// Written exclusively by the core controller. Framework providers MUST
// read from this rather than re-resolving spec.model themselves, so that
// the resolution surface (cross-namespace grants, secret lookups,
// gateway endpoint discovery) lives in exactly one place.
type ModelBindingStatus struct {
	// bindingMode echoes which binding mode in spec.model was used.
	// +optional
	BindingMode ModelBindingMode `json:"bindingMode,omitempty"`

	// baseURL is the resolved OpenAI-compatible base URL for the model
	// endpoint (e.g. http://my-model.inference.svc.cluster.local/v1).
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// modelName is the model identifier the agent should request.
	// +optional
	ModelName string `json:"modelName,omitempty"`

	// credentialsRef points at a Secret with credentials the agent
	// should mount. Empty when no credentials are required.
	// +optional
	CredentialsRef *SecretKeyRef `json:"credentialsRef,omitempty"`

	// observedResourceUID is the UID of the underlying resource (e.g.
	// the ModelDeployment for deploymentRef bindings). The core
	// controller uses this to detect delete+recreate so providers
	// re-render agents when the upstream resource changes identity.
	// +optional
	ObservedResourceUID string `json:"observedResourceUID,omitempty"`
}

// AgentFrameworkStatus echoes resolved framework information for the
// agent. Written by the core controller after validating the framework
// reference against the registered AgentProviderConfig.
type AgentFrameworkStatus struct {
	// name is the resolved framework name (mirrors spec.framework.name).
	// +optional
	Name string `json:"name,omitempty"`

	// providerVersion is the version reported by the AgentProviderConfig
	// status at resolution time. Useful for debugging skew between the
	// AgentDeployment and the framework provider.
	// +optional
	ProviderVersion string `json:"providerVersion,omitempty"`
}

// AgentRuntimeStatus describes the running workload the framework
// provider rendered. Written exclusively by the framework provider.
type AgentRuntimeStatus struct {
	// workloadRef points at the framework-native resource the provider
	// created (e.g. Kagent Agent CR, plain Deployment).
	// +optional
	WorkloadRef *RuntimeWorkloadRef `json:"workloadRef,omitempty"`

	// address is the in-cluster service URL the agent is reachable at,
	// when applicable.
	// +optional
	Address string `json:"address,omitempty"`
}

// RuntimeWorkloadRef identifies the framework-native resource that
// backs an AgentDeployment.
type RuntimeWorkloadRef struct {
	// apiVersion of the backing resource (e.g. "kagent.dev/v1alpha1", "apps/v1").
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// kind of the backing resource (e.g. "Agent", "Deployment").
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// name of the backing resource.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// namespace of the backing resource. Empty for cluster-scoped resources.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// AgentReplicaStatus captures replica counts reported by the framework provider.
type AgentReplicaStatus struct {
	// total is the desired number of agent instances.
	// +optional
	Total int32 `json:"total,omitempty"`

	// ready is the count of instances reporting ready.
	// +optional
	Ready int32 `json:"ready,omitempty"`
}

// AgentDeployment condition types.
const (
	// AgentConditionTypeModelBound is True once the core controller has
	// resolved spec.model into status.modelBinding.
	AgentConditionTypeModelBound = "ModelBound"

	// AgentConditionTypeFrameworkReady is True once the core controller
	// has verified that spec.framework.name resolves to a ready
	// AgentProviderConfig.
	AgentConditionTypeFrameworkReady = "FrameworkReady"

	// AgentConditionTypeProviderReady is True when the framework
	// provider reports the underlying workload is ready.
	AgentConditionTypeProviderReady = "ProviderReady"

	// AgentConditionTypeReady is the aggregate readiness condition,
	// set by the core controller after the prior three are True.
	AgentConditionTypeReady = "Ready"
)

// AgentDeploymentStatus defines the observed state of an AgentDeployment.
//
// Status ownership is split between the core controller and the
// framework provider controller; the field-owner is shown in parens.
//
//   - framework, modelBinding                                     (core)
//   - conditions[ModelBound], conditions[FrameworkReady]          (core)
//   - conditions[Ready]                                           (core)
//   - phase, runtime, replicas, conditions[ProviderReady]         (provider)
//
// Both writers MUST use server-side apply with distinct field owners
// so the API server itself prevents cross-writes.
type AgentDeploymentStatus struct {
	// phase is the high-level lifecycle phase. Owned by the framework provider.
	// +optional
	Phase AgentPhase `json:"phase,omitempty"`

	// framework is the resolved framework metadata. Owned by core.
	// +optional
	Framework *AgentFrameworkStatus `json:"framework,omitempty"`

	// modelBinding is the resolved model binding contract for the
	// framework provider to consume. Owned by core.
	// +optional
	ModelBinding *ModelBindingStatus `json:"modelBinding,omitempty"`

	// runtime describes the rendered workload. Owned by the framework provider.
	// +optional
	Runtime *AgentRuntimeStatus `json:"runtime,omitempty"`

	// replicas summarises desired/ready instance counts. Owned by the
	// framework provider.
	// +optional
	Replicas *AgentReplicaStatus `json:"replicas,omitempty"`

	// observedGeneration is the AgentDeployment.metadata.generation
	// observed by the core controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions follow the standard Kubernetes condition pattern.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ad
// +kubebuilder:printcolumn:name="Framework",type=string,JSONPath=".spec.framework.name"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// AgentDeployment is the Schema for the agentdeployments API.
//
// An AgentDeployment describes one agent instance: which framework
// reconciles it, which model it talks to, and the framework-specific
// configuration that defines its behaviour.
type AgentDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentDeploymentSpec   `json:"spec,omitempty"`
	Status AgentDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentDeploymentList contains a list of AgentDeployment.
type AgentDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentDeployment{}, &AgentDeploymentList{})
}
