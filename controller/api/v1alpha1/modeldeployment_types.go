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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ModelSource defines where the model comes from
// +kubebuilder:validation:Enum=huggingface;custom
type ModelSource string

const (
	// ModelSourceHuggingFace indicates the model is from HuggingFace
	ModelSourceHuggingFace ModelSource = "huggingface"
	// ModelSourceCustom indicates a custom model pre-loaded in the image
	ModelSourceCustom ModelSource = "custom"
)

// EngineType defines the inference engine type
// +kubebuilder:validation:Enum=vllm;sglang;trtllm;llamacpp
type EngineType string

const (
	EngineTypeVLLM     EngineType = "vllm"
	EngineTypeSGLang   EngineType = "sglang"
	EngineTypeTRTLLM   EngineType = "trtllm"
	EngineTypeLlamaCpp EngineType = "llamacpp"
)

// ServingMode defines the serving mode
// +kubebuilder:validation:Enum=aggregated;disaggregated
type ServingMode string

const (
	ServingModeAggregated    ServingMode = "aggregated"
	ServingModeDisaggregated ServingMode = "disaggregated"
)

// DeploymentPhase defines the phase of the deployment
// +kubebuilder:validation:Enum=Pending;Deploying;Running;Failed;Terminating
type DeploymentPhase string

const (
	DeploymentPhasePending     DeploymentPhase = "Pending"
	DeploymentPhaseDeploying   DeploymentPhase = "Deploying"
	DeploymentPhaseRunning     DeploymentPhase = "Running"
	DeploymentPhaseFailed      DeploymentPhase = "Failed"
	DeploymentPhaseTerminating DeploymentPhase = "Terminating"
)

// VolumePurpose defines the intended purpose of a storage volume
// +kubebuilder:validation:Enum=modelCache;compilationCache;custom
type VolumePurpose string

const (
	// VolumePurposeModelCache indicates the volume is used for caching model weights
	VolumePurposeModelCache VolumePurpose = "modelCache"
	// VolumePurposeCompilationCache indicates the volume is used for caching compiled artifacts
	VolumePurposeCompilationCache VolumePurpose = "compilationCache"
	// VolumePurposeCustom indicates a general-purpose volume with no special behavior
	VolumePurposeCustom VolumePurpose = "custom"
)

// StorageVolume defines a persistent volume claim reference for model storage
type StorageVolume struct {
	// name is a unique identifier for this volume (DNS label format)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// claimName is the name of a PersistentVolumeClaim in the same namespace.
	// When size is set and claimName is empty, it defaults to <md-name>-<volume-name>.
	// When size is NOT set, claimName is required (references a pre-existing PVC).
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// mountPath is the absolute path where the volume will be mounted in the container
	// Defaults based on purpose: /model-cache for modelCache, /compilation-cache for compilationCache
	// Required when purpose is custom
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// purpose defines the intended use of this volume, enabling engine-aware defaults
	// +kubebuilder:default=custom
	// +optional
	Purpose VolumePurpose `json:"purpose,omitempty"`

	// readOnly mounts the volume as read-only when true
	// +kubebuilder:default=false
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`

	// size is the requested storage size (e.g., "100Gi").
	// When set, the controller creates a PVC automatically.
	// When not set, claimName must reference a pre-existing PVC.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// storageClassName is the StorageClass to use for controller-created PVCs.
	// When nil (omitted), the cluster's default StorageClass is used.
	// When set to empty string (""), no StorageClass is applied (disables dynamic provisioning).
	// Only applicable when size is set.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// accessMode is the PVC access mode for controller-created PVCs.
	// Defaults to ReadWriteMany when size is set.
	// Only applicable when size is set.
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany;ReadOnlyMany;ReadWriteOncePod
	// +optional
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
}

// ResolvedClaimName returns the PVC claim name, either explicitly set or auto-generated
// from the ModelDeployment name and volume name.
// Note: The mutating webhook defaults ClaimName for managed PVCs (when Size is set),
// so the auto-generated fallback here serves as a safety net for contexts where the
// webhook is bypassed (e.g., unit tests, direct API access without admission webhooks).
func (v *StorageVolume) ResolvedClaimName(mdName string) string {
	if v.ClaimName != "" {
		return v.ClaimName
	}
	return fmt.Sprintf("%s-%s", mdName, v.Name)
}

// StorageSpec defines persistent storage configuration for model data
type StorageSpec struct {
	// volumes is a list of PVC references to mount into inference containers
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Volumes []StorageVolume `json:"volumes,omitempty"`
}

// ModelSpec defines the model specification
type ModelSpec struct {
	// id is the model identifier (e.g., HuggingFace model ID)
	// Required when source is huggingface
	// +optional
	ID string `json:"id,omitempty"`

	// servedName is the API-facing model name
	// Defaults to model ID basename if not specified
	// Not applicable for source=custom
	// +optional
	ServedName string `json:"servedName,omitempty"`

	// source indicates where the model comes from
	// +kubebuilder:default=huggingface
	// +optional
	Source ModelSource `json:"source,omitempty"`

	// storage defines persistent storage for model data (e.g., model weights, compilation caches)
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`
}

// ProviderSpec defines the provider selection
type ProviderSpec struct {
	// name is the provider name (e.g., dynamo, kaito, kuberay, llmd)
	// If not specified, the provider-selector will choose one
	// +optional
	Name string `json:"name,omitempty"`

	// overrides contains provider-specific configuration
	// This is an escape hatch for provider-specific features
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Overrides *runtime.RawExtension `json:"overrides,omitempty"`
}

// EngineSpec defines the inference engine configuration
type EngineSpec struct {
	// type is the inference engine type
	// If not specified, the controller will auto-select based on provider capabilities
	// +optional
	Type EngineType `json:"type,omitempty"`

	// contextLength is the maximum context length
	// Maps to engine-specific flags (--max-model-len for vllm, etc.)
	// +optional
	ContextLength *int32 `json:"contextLength,omitempty"`

	// trustRemoteCode allows execution of remote code from HuggingFace
	// Only applicable for vllm and sglang engines
	// +kubebuilder:default=false
	// +optional
	TrustRemoteCode bool `json:"trustRemoteCode,omitempty"`

	// enablePrefixCaching enables prefix caching for faster inference on repeated prompts
	// Required for KV-cache-aware routing to be effective
	// Only applicable for vllm and sglang engines
	// +kubebuilder:default=true
	// +optional
	EnablePrefixCaching bool `json:"enablePrefixCaching,omitempty"`

	// enforceEager forces eager execution mode (disables CUDA graphs)
	// Only applicable for vllm and sglang engines
	// +kubebuilder:default=false
	// +optional
	EnforceEager bool `json:"enforceEager,omitempty"`

	// args contains engine-specific arguments
	// These are passed directly to the engine and vary by type
	// +optional
	Args map[string]string `json:"args,omitempty"`
}

// ServingSpec defines the serving mode configuration
type ServingSpec struct {
	// mode is the serving mode (aggregated or disaggregated)
	// +kubebuilder:default=aggregated
	// +optional
	Mode ServingMode `json:"mode,omitempty"`
}

// GPUSpec defines GPU resource requirements
type GPUSpec struct {
	// count is the number of GPUs
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	Count int32 `json:"count,omitempty"`

	// type is the GPU resource name (defaults to nvidia.com/gpu)
	// Override for AMD/Intel GPUs
	// +kubebuilder:default="nvidia.com/gpu"
	// +optional
	Type string `json:"type,omitempty"`
}

// ResourceSpec defines resource requirements
type ResourceSpec struct {
	// gpu defines GPU requirements
	// +optional
	GPU *GPUSpec `json:"gpu,omitempty"`

	// memory is the memory requirement (e.g., "32Gi")
	// +optional
	Memory string `json:"memory,omitempty"`

	// cpu is the CPU requirement (e.g., "4")
	// +optional
	CPU string `json:"cpu,omitempty"`
}

// ComponentScalingSpec defines scaling for prefill/decode components
type ComponentScalingSpec struct {
	// replicas is the number of replicas for this component
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// gpu defines GPU requirements for this component
	// Required for disaggregated mode
	// +optional
	GPU *GPUSpec `json:"gpu,omitempty"`

	// memory is the memory requirement for this component
	// Required for disaggregated mode
	// +optional
	Memory string `json:"memory,omitempty"`
}

// ScalingSpec defines the scaling configuration
type ScalingSpec struct {
	// replicas is the number of replicas for aggregated mode
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// prefill defines prefill worker configuration for disaggregated mode
	// +optional
	Prefill *ComponentScalingSpec `json:"prefill,omitempty"`

	// decode defines decode worker configuration for disaggregated mode
	// +optional
	Decode *ComponentScalingSpec `json:"decode,omitempty"`
}

// PodTemplateMetadata defines metadata for created pods
type PodTemplateMetadata struct {
	// labels are labels to add to created pods
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// annotations are annotations to add to created pods
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PodTemplateSpec defines pod customization
type PodTemplateSpec struct {
	// metadata defines labels and annotations for created pods
	// +optional
	Metadata *PodTemplateMetadata `json:"metadata,omitempty"`
}

// SecretsSpec defines secret references
type SecretsSpec struct {
	// huggingFaceToken is the name of the Kubernetes secret containing HF_TOKEN
	// +optional
	HuggingFaceToken string `json:"huggingFaceToken,omitempty"`
}

// GatewaySpec defines the Gateway API integration configuration
type GatewaySpec struct {
	// enabled controls whether an InferencePool + HTTPRoute are created for this model.
	// Defaults to true when a Gateway is detected in the cluster.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// modelName overrides the model name used in HTTPRoute routing.
	// Defaults to spec.model.servedName or spec.model.id
	// +optional
	ModelName string `json:"modelName,omitempty"`
	// httpRouteRef references an existing HTTPRoute by name instead of auto-creating one.
	// When set, the controller skips HTTPRoute creation and uses the referenced route.
	// The HTTPRoute must be in the same namespace as the ModelDeployment.
	// +optional
	HTTPRouteRef string `json:"httpRouteRef,omitempty"`
}

// ModelDeploymentSpec defines the desired state of ModelDeployment
type ModelDeploymentSpec struct {
	// model defines the model specification
	// +kubebuilder:validation:Required
	Model ModelSpec `json:"model"`

	// provider defines the provider selection
	// +optional
	Provider *ProviderSpec `json:"provider,omitempty"`

	// engine defines the inference engine configuration
	// +optional
	Engine EngineSpec `json:"engine,omitempty"`

	// serving defines the serving mode configuration
	// +optional
	Serving *ServingSpec `json:"serving,omitempty"`

	// scaling defines the scaling configuration
	// +optional
	Scaling *ScalingSpec `json:"scaling,omitempty"`

	// resources defines the resource requirements
	// Not allowed in disaggregated mode (use scaling.prefill/decode instead)
	// +optional
	Resources *ResourceSpec `json:"resources,omitempty"`

	// image is a custom container image
	// +optional
	Image string `json:"image,omitempty"`

	// env defines environment variables for the container
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// podTemplate defines pod customization
	// +optional
	PodTemplate *PodTemplateSpec `json:"podTemplate,omitempty"`

	// secrets defines secret references
	// +optional
	Secrets *SecretsSpec `json:"secrets,omitempty"`

	// gateway defines the Gateway API integration configuration
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// nodeSelector constrains scheduling to nodes with specific labels
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations are tolerations for the pods
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// ProviderStatus contains information about the selected provider
type ProviderStatus struct {
	// name is the selected provider name
	// +optional
	Name string `json:"name,omitempty"`

	// resourceName is the name of the created provider resource
	// +optional
	ResourceName string `json:"resourceName,omitempty"`

	// resourceKind is the kind of the created provider resource
	// +optional
	ResourceKind string `json:"resourceKind,omitempty"`

	// selectedReason explains why this provider was selected
	// +optional
	SelectedReason string `json:"selectedReason,omitempty"`
}

// ReplicaStatus contains replica count information
type ReplicaStatus struct {
	// desired is the desired number of replicas
	// +optional
	Desired int32 `json:"desired,omitempty"`

	// ready is the number of ready replicas
	// +optional
	Ready int32 `json:"ready,omitempty"`

	// available is the number of available replicas
	// +optional
	Available int32 `json:"available,omitempty"`
}

// EndpointStatus contains service endpoint information
type EndpointStatus struct {
	// service is the name of the service
	// +optional
	Service string `json:"service,omitempty"`

	// port is the service port
	// +optional
	Port int32 `json:"port,omitempty"`
}

// EngineStatus contains information about the selected engine
type EngineStatus struct {
	// type is the resolved engine type
	// +optional
	Type EngineType `json:"type,omitempty"`

	// selectedReason explains why this engine was selected
	// +optional
	SelectedReason string `json:"selectedReason,omitempty"`
}

// GatewayStatus contains information about the gateway integration
type GatewayStatus struct {
	// endpoint is the unified gateway endpoint URL
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// modelName is the model name to use in API requests
	// +optional
	ModelName string `json:"modelName,omitempty"`
	// gatewayNamespace is the namespace of the Gateway resource used for routing.
	// +optional
	GatewayNamespace string `json:"gatewayNamespace,omitempty"`
}

// ModelDeploymentStatus defines the observed state of ModelDeployment.
type ModelDeploymentStatus struct {
	// phase is the current phase of the deployment
	// +optional
	Phase DeploymentPhase `json:"phase,omitempty"`

	// message is a human-readable message about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// provider contains information about the selected provider
	// +optional
	Provider *ProviderStatus `json:"provider,omitempty"`

	// engine contains information about the selected engine
	// +optional
	Engine *EngineStatus `json:"engine,omitempty"`

	// gateway contains information about the gateway integration
	// +optional
	Gateway *GatewayStatus `json:"gateway,omitempty"`

	// replicas contains replica count information
	// +optional
	Replicas *ReplicaStatus `json:"replicas,omitempty"`

	// endpoint contains the service endpoint information
	// +optional
	Endpoint *EndpointStatus `json:"endpoint,omitempty"`

	// conditions represent the current state of the ModelDeployment resource
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Current phase"
// +kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".status.provider.name",description="Selected provider"
// +kubebuilder:printcolumn:name="Engine",type="string",JSONPath=".status.engine.type",description="Inference engine"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.replicas.ready",description="Ready replicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ModelDeployment is the Schema for the modeldeployments API
type ModelDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of ModelDeployment
	// +kubebuilder:validation:Required
	Spec ModelDeploymentSpec `json:"spec"`

	// status defines the observed state of ModelDeployment
	// +optional
	Status ModelDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelDeploymentList contains a list of ModelDeployment
type ModelDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelDeployment{}, &ModelDeploymentList{})
}

// ResolvedEngineType returns the engine type from spec if set,
// otherwise falls back to the auto-selected engine from status.
func (md *ModelDeployment) ResolvedEngineType() EngineType {
	if md.Spec.Engine.Type != "" {
		return md.Spec.Engine.Type
	}
	if md.Status.Engine != nil {
		return md.Status.Engine.Type
	}
	return ""
}

// Condition types for ModelDeployment
const (
	// ConditionTypeValidated indicates the spec has been validated
	ConditionTypeValidated = "Validated"
	// ConditionTypeEngineSelected indicates an engine has been selected
	ConditionTypeEngineSelected = "EngineSelected"
	// ConditionTypeProviderSelected indicates a provider has been selected
	ConditionTypeProviderSelected = "ProviderSelected"
	// ConditionTypeProviderCompatible indicates the config is compatible with the provider
	ConditionTypeProviderCompatible = "ProviderCompatible"
	// ConditionTypeStorageReady indicates all managed PVCs are bound
	ConditionTypeStorageReady = "StorageReady"
	// ConditionTypeModelDownloaded indicates the model download job has completed
	ConditionTypeModelDownloaded = "ModelDownloaded"
	// ConditionTypeResourceCreated indicates the provider resource has been created
	ConditionTypeResourceCreated = "ResourceCreated"
	// ConditionTypeReady indicates the deployment is ready
	ConditionTypeReady = "Ready"
	// ConditionTypeGatewayReady indicates the gateway route is active
	ConditionTypeGatewayReady = "GatewayReady"
)

const (
	HTTPRouteCreated     = "airunway.ai/httproute-created"
	BBRRestarted         = "airunway.ai/bbr-restarted"
	LabelModelDeployment = "airunway.ai/model-deployment"
	LabelManagedBy       = "airunway.ai/managed-by"
	LabelJobType         = "airunway.ai/job-type"
)
