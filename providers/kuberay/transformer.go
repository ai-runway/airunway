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

package kuberay

import (
	"context"
	"fmt"
	"sort"
	"strings"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const (
	// RayAPIGroup is the API group for KubeRay CRDs
	RayAPIGroup = "ray.io"
	// RayAPIVersion is the current API version for KubeRay CRDs
	RayAPIVersion = "v1"
	// RayServiceKind is the kind for RayService
	RayServiceKind = "RayService"

	// DefaultImage is the default Ray image for vLLM workloads. This default uses
	// the CUDA-specific cu128 variant and may require compatible NVIDIA
	// driver/CUDA support in the target cluster. Users can override this via
	// spec.image if their environment requires a different image or CUDA variant.
	DefaultImage = "rayproject/ray-llm:2.55.0-py311-cu128"

	// DefaultHeadCPU is the default CPU limit for the head node
	DefaultHeadCPU = "4"
	// DefaultHeadMemory is the default memory limit for the head node
	DefaultHeadMemory = "16Gi"
	// DefaultWorkerMemory is the default memory limit for worker nodes
	DefaultWorkerMemory = "32Gi"
)

// Transformer handles transformation of ModelDeployment to RayService
type Transformer struct{}

// Application represents a single RayServe application
type Application struct {
	Name        string          `json:"name" yaml:"name"`
	RoutePrefix string          `json:"route_prefix" yaml:"route_prefix"`
	ImportPath  string          `json:"import_path" yaml:"import_path"`
	Args        ApplicationArgs `json:"args" yaml:"args"`
}

// ServeConfig represents the top-level RayServe configuration
type ServeConfig struct {
	Applications []Application `json:"applications" yaml:"applications"`
}

// ModelLoadingConfig represents the configuration for loading a model in the RayService spec
type ModelLoadingConfig struct {
	ModelID     string `json:"model_id" yaml:"model_id"`
	ModelSource string `json:"model_source" yaml:"model_source"`
}

// AutoscalingConfig holds autoscaling settings for a deployment
type AutoscalingConfig struct {
	MinReplicas int64 `json:"min_replicas" yaml:"min_replicas"`
	MaxReplicas int64 `json:"max_replicas" yaml:"max_replicas"`
}

// DeploymentConfig holds deployment configuration including autoscaling
type DeploymentConfig struct {
	AutoscalingConfig AutoscalingConfig `json:"autoscaling_config" yaml:"autoscaling_config"`
}

// LLMConfig represents the configuration for loading a model in the RayService spec
type LLMConfig struct {
	ModelLoadingConfig ModelLoadingConfig     `json:"model_loading_config" yaml:"model_loading_config"`
	EngineArgs         map[string]interface{} `json:"engine_kwargs,omitempty" yaml:"engine_kwargs,omitempty"`
	DeploymentConfig   DeploymentConfig       `json:"deployment_config" yaml:"deployment_config"`
}

// ApplicationArgs holds the args for a RayServe application
type ApplicationArgs struct {
	LLMConfigs []LLMConfig `json:"llm_configs" yaml:"llm_configs"`
}

// NewTransformer creates a new KubeRay transformer
func NewTransformer() *Transformer {
	return &Transformer{}
}

// Transform converts a ModelDeployment to a RayService
func (t *Transformer) Transform(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) ([]*unstructured.Unstructured, error) {
	rs := &unstructured.Unstructured{}
	rs.SetAPIVersion(fmt.Sprintf("%s/%s", RayAPIGroup, RayAPIVersion))
	rs.SetKind(RayServiceKind)
	rs.SetName(md.Name)
	rs.SetNamespace(md.Namespace)

	// Set owner reference
	rs.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         md.APIVersion,
			Kind:               md.Kind,
			Name:               md.Name,
			UID:                md.UID,
			Controller:         boolPtr(true),
			BlockOwnerDeletion: boolPtr(true),
		},
	})

	// Set labels
	labels := map[string]string{
		"airunway.ai/managed-by":   "airunway",
		"airunway.ai/deployment":   md.Name,
		"airunway.ai/model-source": string(md.Spec.Model.Source),
		"airunway.ai/engine-type":  string(md.ResolvedEngineType()),
	}
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil {
		for k, v := range md.Spec.PodTemplate.Metadata.Labels {
			labels[k] = v
		}
	}
	rs.SetLabels(labels)

	// Merge podTemplate annotations onto the RayService
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil && len(md.Spec.PodTemplate.Metadata.Annotations) > 0 {
		rs.SetAnnotations(md.Spec.PodTemplate.Metadata.Annotations)
	}

	// Build the spec
	spec, err := t.buildSpec(md)
	if err != nil {
		return nil, fmt.Errorf("failed to build RayService spec: %w", err)
	}

	if err := unstructured.SetNestedField(rs.Object, spec, "spec"); err != nil {
		return nil, fmt.Errorf("failed to set spec: %w", err)
	}

	return []*unstructured.Unstructured{rs}, nil
}

// buildServeConfig constructs the structured ServeConfig for a ModelDeployment
func (t *Transformer) buildServeConfig(md *airunwayv1alpha1.ModelDeployment) ServeConfig {
	replicas := int64(1)
	if md.Spec.Scaling != nil && md.Spec.Scaling.Replicas > 0 {
		replicas = int64(md.Spec.Scaling.Replicas)
	}

	modelLoadingConfig := ModelLoadingConfig{
		ModelID:     md.Spec.Model.ID,
		ModelSource: md.Spec.Model.ID,
	}

	deploymentConfig := DeploymentConfig{
		AutoscalingConfig: AutoscalingConfig{
			MinReplicas: replicas,
			MaxReplicas: replicas,
		},
	}

	llmConfig := LLMConfig{
		ModelLoadingConfig: modelLoadingConfig,
		DeploymentConfig:   deploymentConfig,
	}

	if md.Spec.Engine.ContextLength != nil {
		llmConfig.EngineArgs = map[string]interface{}{
			"max_model_len": *md.Spec.Engine.ContextLength,
		}
	}

	if md.Spec.Engine.Args != nil {
		if llmConfig.EngineArgs == nil {
			llmConfig.EngineArgs = make(map[string]interface{})
		}
		for k, v := range md.Spec.Engine.Args {
			llmConfig.EngineArgs[k] = v
		}
	}

	return ServeConfig{
		Applications: []Application{
			{
				Name:        "llm",
				RoutePrefix: "/",
				ImportPath:  "ray.serve.llm:build_openai_app",
				Args: ApplicationArgs{
					LLMConfigs: []LLMConfig{llmConfig},
				},
			},
		},
	}
}

// buildSpec creates the spec for a RayService
func (t *Transformer) buildSpec(md *airunwayv1alpha1.ModelDeployment) (map[string]interface{}, error) {
	spec := map[string]interface{}{}

	// Build serveConfigV2 from structured data
	serveConfig := t.buildServeConfig(md)
	serveConfigYAML, err := yaml.Marshal(serveConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal serve config: %w", err)
	}
	spec["serveConfigV2"] = string(serveConfigYAML)

	// Build rayClusterConfig
	rayClusterConfig, err := t.buildRayClusterConfig(md)
	if err != nil {
		return nil, err
	}
	spec["rayClusterConfig"] = rayClusterConfig

	return spec, nil
}

// buildRayClusterConfig creates the rayClusterConfig section
func (t *Transformer) buildRayClusterConfig(md *airunwayv1alpha1.ModelDeployment) (map[string]interface{}, error) {
	config := map[string]interface{}{}

	// Build head group spec
	config["headGroupSpec"] = t.buildHeadGroupSpec(md)

	// Build worker group specs
	servingMode := airunwayv1alpha1.ServingModeAggregated
	if md.Spec.Serving != nil && md.Spec.Serving.Mode != "" {
		servingMode = md.Spec.Serving.Mode
	}

	if servingMode == airunwayv1alpha1.ServingModeDisaggregated {
		config["workerGroupSpecs"] = t.buildDisaggregatedWorkerGroups(md)
	} else {
		config["workerGroupSpecs"] = t.buildAggregatedWorkerGroup(md)
	}

	return config, nil
}

// buildHeadGroupSpec creates the head group spec
func (t *Transformer) buildHeadGroupSpec(md *airunwayv1alpha1.ModelDeployment) map[string]interface{} {
	image := t.getImage(md)
	headMemory := DefaultHeadMemory
	if md.Spec.Resources != nil && md.Spec.Resources.Memory != "" {
		headMemory = md.Spec.Resources.Memory
	}

	headGroupSpec := map[string]interface{}{
		"rayStartParams": map[string]interface{}{
			"dashboard-host": "0.0.0.0",
		},
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": map[string]interface{}{
					"airunway.ai/model-deployment": md.Name,
				},
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "ray-head",
						"image": image,
						"resources": map[string]interface{}{
							"limits": map[string]interface{}{
								"cpu":    DefaultHeadCPU,
								"memory": headMemory,
							},
						},
					},
				},
			},
		},
	}

	return headGroupSpec
}

// buildAggregatedWorkerGroup creates worker group specs for aggregated mode
func (t *Transformer) buildAggregatedWorkerGroup(md *airunwayv1alpha1.ModelDeployment) []interface{} {
	image := t.getImage(md)
	replicas := int64(1)
	if md.Spec.Scaling != nil && md.Spec.Scaling.Replicas > 0 {
		replicas = int64(md.Spec.Scaling.Replicas)
	}

	workerMemory := DefaultWorkerMemory
	if md.Spec.Resources != nil && md.Spec.Resources.Memory != "" {
		workerMemory = md.Spec.Resources.Memory
	}

	// Build resource limits
	limits := map[string]interface{}{
		"memory": workerMemory,
	}
	if md.Spec.Resources != nil && md.Spec.Resources.GPU != nil && md.Spec.Resources.GPU.Count > 0 {
		gpuType := "nvidia.com/gpu"
		if md.Spec.Resources.GPU.Type != "" {
			gpuType = md.Spec.Resources.GPU.Type
		}
		limits[gpuType] = fmt.Sprintf("%d", md.Spec.Resources.GPU.Count)
	}

	// Build env var with HF_TOKEN from secret if specified
	envVars := append([]interface{}{}, t.buildEnvVars(md)...)

	workerGroup := map[string]interface{}{
		"replicas":       replicas,
		"minReplicas":    replicas,
		"maxReplicas":    replicas,
		"groupName":      "gpu-workers",
		"rayStartParams": map[string]interface{}{},
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": map[string]interface{}{
					"airunway.ai/model-deployment": md.Name,
				},
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "ray-worker",
						"image": image,
						"resources": map[string]interface{}{
							"limits": limits,
						},
						"env": envVars,
					},
				},
			},
		},
	}

	return []interface{}{workerGroup}
}

// buildDisaggregatedWorkerGroups creates separate prefill and decode worker groups
func (t *Transformer) buildDisaggregatedWorkerGroups(md *airunwayv1alpha1.ModelDeployment) []interface{} {
	image := t.getImage(md)
	var workerGroups []interface{}

	// Build env var with HF_TOKEN from secret if specified
	envVars := append([]interface{}{}, t.buildEnvVars(md)...)

	// Build prefill worker group
	if md.Spec.Scaling != nil && md.Spec.Scaling.Prefill != nil {
		prefillSpec := md.Spec.Scaling.Prefill
		prefillLimits := map[string]interface{}{}
		if prefillSpec.GPU != nil && prefillSpec.GPU.Count > 0 {
			gpuType := "nvidia.com/gpu"
			if prefillSpec.GPU.Type != "" {
				gpuType = prefillSpec.GPU.Type
			}
			prefillLimits[gpuType] = fmt.Sprintf("%d", prefillSpec.GPU.Count)
		}
		if prefillSpec.Memory != "" {
			prefillLimits["memory"] = prefillSpec.Memory
		} else {
			prefillLimits["memory"] = DefaultWorkerMemory
		}

		prefillGroup := map[string]interface{}{
			"replicas":       int64(prefillSpec.Replicas),
			"minReplicas":    int64(prefillSpec.Replicas),
			"maxReplicas":    int64(prefillSpec.Replicas),
			"groupName":      "prefill-workers",
			"rayStartParams": map[string]interface{}{},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{
						"airunway.ai/model-deployment": md.Name,
					},
				},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "ray-worker",
							"image": image,
							"resources": map[string]interface{}{
								"limits": prefillLimits,
							},
							"env": envVars,
						},
					},
				},
			},
		}
		workerGroups = append(workerGroups, prefillGroup)
	}

	// Build decode worker group
	if md.Spec.Scaling != nil && md.Spec.Scaling.Decode != nil {
		decodeSpec := md.Spec.Scaling.Decode
		decodeLimits := map[string]interface{}{}
		if decodeSpec.GPU != nil && decodeSpec.GPU.Count > 0 {
			gpuType := "nvidia.com/gpu"
			if decodeSpec.GPU.Type != "" {
				gpuType = decodeSpec.GPU.Type
			}
			decodeLimits[gpuType] = fmt.Sprintf("%d", decodeSpec.GPU.Count)
		}
		if decodeSpec.Memory != "" {
			decodeLimits["memory"] = decodeSpec.Memory
		} else {
			decodeLimits["memory"] = DefaultWorkerMemory
		}

		decodeGroup := map[string]interface{}{
			"replicas":       int64(decodeSpec.Replicas),
			"minReplicas":    int64(decodeSpec.Replicas),
			"maxReplicas":    int64(decodeSpec.Replicas),
			"groupName":      "decode-workers",
			"rayStartParams": map[string]interface{}{},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{
						"airunway.ai/model-deployment": md.Name,
					},
				},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "ray-worker",
							"image": image,
							"resources": map[string]interface{}{
								"limits": decodeLimits,
							},
							"env": envVars,
						},
					},
				},
			},
		}
		workerGroups = append(workerGroups, decodeGroup)
	}

	return workerGroups
}

// buildEngineArgs constructs the vLLM engine arguments string
func (t *Transformer) buildEngineArgs(md *airunwayv1alpha1.ModelDeployment) string {
	var args []string

	args = append(args, "--model", md.Spec.Model.ID)

	// Add context length
	if md.Spec.Engine.ContextLength != nil {
		args = append(args, "--max-model-len", fmt.Sprintf("%d", *md.Spec.Engine.ContextLength))
	}

	// Add served name if specified
	if md.Spec.Model.ServedName != "" {
		args = append(args, "--served-model-name", md.Spec.Model.ServedName)
	}

	// Add trust remote code
	if md.Spec.Engine.TrustRemoteCode {
		args = append(args, "--trust-remote-code")
	}

	// Add custom engine args (sorted for deterministic output)
	keys := make([]string, 0, len(md.Spec.Engine.Args))
	for k := range md.Spec.Engine.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := md.Spec.Engine.Args[key]
		if value != "" {
			args = append(args, fmt.Sprintf("--%s", key), value)
		} else {
			args = append(args, fmt.Sprintf("--%s", key))
		}
	}

	return strings.Join(args, " ")
}

// buildEnvVars constructs environment variables including HF_TOKEN from secrets
func (t *Transformer) buildEnvVars(md *airunwayv1alpha1.ModelDeployment) []interface{} {
	var envVars []interface{}

	// Add user-specified env vars
	for _, e := range md.Spec.Env {
		ev := map[string]interface{}{
			"name": e.Name,
		}
		if e.Value != "" {
			ev["value"] = e.Value
		}
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			ev["valueFrom"] = map[string]interface{}{
				"secretKeyRef": map[string]interface{}{
					"name": e.ValueFrom.SecretKeyRef.Name,
					"key":  e.ValueFrom.SecretKeyRef.Key,
				},
			}
		}
		envVars = append(envVars, ev)
	}

	// Add HF_TOKEN from secret if specified
	if md.Spec.Secrets != nil && md.Spec.Secrets.HuggingFaceToken != "" {
		envVars = append(envVars, map[string]interface{}{
			"name": "HF_TOKEN",
			"valueFrom": map[string]interface{}{
				"secretKeyRef": map[string]interface{}{
					"name": md.Spec.Secrets.HuggingFaceToken,
					"key":  "HF_TOKEN",
				},
			},
		})
	}

	return envVars
}

// getImage returns the container image to use
func (t *Transformer) getImage(md *airunwayv1alpha1.ModelDeployment) string {
	// Honor spec.engine.image (preferred) and the legacy spec.image via
	// ImageOverride(), matching the other providers so a user setting only
	// spec.engine.image is not silently ignored.
	if image := md.Spec.ImageOverride(); image != "" {
		return image
	}
	return DefaultImage
}

// sanitizeLabelValue ensures a value is valid for a Kubernetes label
func sanitizeLabelValue(value string) string {
	if len(value) > 63 {
		value = value[:63]
	}
	value = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, value)
	value = strings.Trim(value, "-_.")
	return value
}

// boolPtr returns a pointer to a bool
func boolPtr(b bool) *bool {
	return &b
}
