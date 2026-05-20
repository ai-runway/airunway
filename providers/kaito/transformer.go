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

package kaito

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// KaitoAPIGroup is the API group for KAITO CRDs
	KaitoAPIGroup = "kaito.sh"
	// KaitoAPIVersion is the current API version for KAITO CRDs
	KaitoAPIVersion = "v1beta1"
	// WorkspaceKind is the kind for KAITO Workspace
	WorkspaceKind = "Workspace"

	// defaultLlamaCppPort is the default serving port for llamacpp containers
	defaultLlamaCppPort = 5000
	// DefaultPresetPort is the default serving port for KAITO preset models
	DefaultPresetPort = 80

	// nodeAutoProvisioningEnv enables KAITO node auto-provisioning when set to a truthy value.
	nodeAutoProvisioningEnv = "AIRUNWAY_KAITO_NODE_AUTO_PROVISIONING"
	// cpuInstanceTypeEnv supplies the KAITO instanceType for CPU-only deployments.
	cpuInstanceTypeEnv = "AIRUNWAY_KAITO_CPU_INSTANCE_TYPE"
	// gpuInstanceTypeEnv supplies the KAITO instanceType for GPU deployments.
	gpuInstanceTypeEnv = "AIRUNWAY_KAITO_GPU_INSTANCE_TYPE"
)

// Transformer handles transformation of ModelDeployment to KAITO Workspace
type Transformer struct{}

// NewTransformer creates a new KAITO transformer
func NewTransformer() *Transformer {
	return &Transformer{}
}

// Transform converts a ModelDeployment to a KAITO Workspace
func (t *Transformer) Transform(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) ([]*unstructured.Unstructured, error) {
	ws := &unstructured.Unstructured{}
	ws.SetAPIVersion(fmt.Sprintf("%s/%s", KaitoAPIGroup, KaitoAPIVersion))
	ws.SetKind(WorkspaceKind)
	ws.SetName(md.Name)
	ws.SetNamespace(md.Namespace)

	// Set owner reference
	ws.SetOwnerReferences([]metav1.OwnerReference{
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
		"airunway.ai/managed-by":       "airunway",
		"airunway.ai/deployment":       md.Name,
		"airunway.ai/model-source":     string(md.Spec.Model.Source),
		"airunway.ai/engine-type":      string(md.ResolvedEngineType()),
		"airunway.ai/model-deployment": md.Name,
	}
	// Merge podTemplate labels onto the Workspace
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil {
		for k, v := range md.Spec.PodTemplate.Metadata.Labels {
			labels[k] = v
		}
	}
	ws.SetLabels(labels)

	// Merge podTemplate annotations onto the Workspace
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil && len(md.Spec.PodTemplate.Metadata.Annotations) > 0 {
		ws.SetAnnotations(copyStringMap(md.Spec.PodTemplate.Metadata.Annotations))
	}

	// Build resource spec
	resource := t.buildResource(md)

	// Build inference spec based on engine type
	inference, err := t.buildInference(md)
	if err != nil {
		return nil, fmt.Errorf("failed to build inference spec: %w", err)
	}

	// KAITO Workspace CRD has resource and inference at root level, not under spec
	if err := unstructured.SetNestedField(ws.Object, resource, "resource"); err != nil {
		return nil, fmt.Errorf("failed to set resource: %w", err)
	}
	if err := unstructured.SetNestedField(ws.Object, inference, "inference"); err != nil {
		return nil, fmt.Errorf("failed to set inference: %w", err)
	}

	// Apply escape hatch overrides last so they can override any field.
	// Setting an override value to null deletes that field from the generated Workspace.
	if err := applyOverrides(ws, md); err != nil {
		return nil, fmt.Errorf("failed to apply provider overrides: %w", err)
	}

	return []*unstructured.Unstructured{ws}, nil
}

// buildResource creates the resource section of the Workspace spec
func (t *Transformer) buildResource(md *airunwayv1alpha1.ModelDeployment) map[string]interface{} {
	resource := map[string]interface{}{}

	// Map scaling.replicas → spec.resource.count
	count := int64(1)
	if md.Spec.Scaling != nil && md.Spec.Scaling.Replicas > 0 {
		count = int64(md.Spec.Scaling.Replicas)
	}
	resource["count"] = count

	// Node auto-provisioning mode: emit instanceType when it is explicitly
	// enabled and the matching instance type env var is set. Keep labelSelector
	// as well because the KAITO v1beta1 CRD requires resource.labelSelector even
	// when node auto-provisioning uses resource.instanceType.
	if kaitoNodeAutoProvisioningEnabled() {
		if instanceType := kaitoInstanceTypeForMD(md); instanceType != "" {
			resource["instanceType"] = instanceType
		}
	}

	// Always include a labelSelector. In BYO-node mode this is the scheduler
	// selector; in NAP mode it satisfies the Workspace schema and constrains
	// any existing nodes KAITO may reuse.
	matchLabels := map[string]interface{}{
		"kubernetes.io/os": "linux",
	}
	// Merge user-provided nodeSelector first so the forced GPU label below
	// always wins — preventing a user from accidentally disabling GPU node
	// targeting via spec.nodeSelector["nvidia.com/gpu.present"].
	for k, v := range md.Spec.NodeSelector {
		matchLabels[k] = v
	}
	// When GPUs are requested, force-target nodes with NVIDIA GPUs so KAITO's
	// webhook doesn't fail validating CPU nodes.
	//
	// Note: this assumes nodes are labeled with `nvidia.com/gpu.present=true`
	// (typically provided by NFD / gpu-feature-discovery). The default airunway
	// KAITO install disables those sub-charts (see config.go install command),
	// so operators using mixed CPU/GPU pools must either enable NFD or label
	// their GPU nodes manually. Users with a different GPU-presence label can use
	// spec.provider.overrides to delete this key and add their own selector.
	if kaitoHasGPU(md) {
		matchLabels["nvidia.com/gpu.present"] = "true"
	}
	resource["labelSelector"] = map[string]interface{}{
		"matchLabels": matchLabels,
	}

	return resource
}

func kaitoNodeAutoProvisioningEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(nodeAutoProvisioningEnv))) {
	case "1", "t", "true", "y", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

func kaitoHasGPU(md *airunwayv1alpha1.ModelDeployment) bool {
	return md.Spec.Resources != nil &&
		md.Spec.Resources.GPU != nil &&
		md.Spec.Resources.GPU.Count > 0
}

func kaitoInstanceTypeForMD(md *airunwayv1alpha1.ModelDeployment) string {
	if kaitoHasGPU(md) {
		return strings.TrimSpace(os.Getenv(gpuInstanceTypeEnv))
	}
	return strings.TrimSpace(os.Getenv(cpuInstanceTypeEnv))
}

// buildInference creates the inference section of the Workspace spec
func (t *Transformer) buildInference(md *airunwayv1alpha1.ModelDeployment) (map[string]interface{}, error) {
	inference := map[string]interface{}{}

	switch md.ResolvedEngineType() {
	case airunwayv1alpha1.EngineTypeVLLM:
		// vLLM preset path: KAITO manages the image
		inference["preset"] = map[string]interface{}{
			"name": md.Spec.Model.ID,
		}
	case airunwayv1alpha1.EngineTypeLlamaCpp:
		// llamacpp template path: user-provided image with pod template
		template, err := t.buildLlamaCppTemplate(md)
		if err != nil {
			return nil, err
		}
		inference["template"] = template
	default:
		return nil, fmt.Errorf("unsupported engine type for KAITO: %s", md.ResolvedEngineType())
	}

	return inference, nil
}

// buildLlamaCppTemplate creates the pod template spec for llamacpp inference
func (t *Transformer) buildLlamaCppTemplate(md *airunwayv1alpha1.ModelDeployment) (map[string]interface{}, error) {
	if md.Spec.Image == "" {
		return nil, fmt.Errorf("image is required for llamacpp engine type")
	}

	// Build container args
	args := []interface{}{
		"--address=:5000",
	}
	// Prefer the exact GGUF URL when the API populated one for direct-run deployments.
	if modelArg := resolveLlamaCppModelArg(md); modelArg != "" {
		args = append([]interface{}{modelArg}, args...)
	}

	// Build container ports
	ports := []interface{}{
		map[string]interface{}{
			"containerPort": int64(defaultLlamaCppPort),
		},
	}

	// Build container
	container := map[string]interface{}{
		"name":  "model",
		"image": md.Spec.Image,
		"args":  args,
		"ports": ports,
	}

	// Add resource requests
	resources := t.buildResourceRequests(md.Spec.Resources)
	if len(resources) > 0 {
		container["resources"] = resources
	}

	// Build env vars
	envVars := t.buildEnvVars(md)
	if len(envVars) > 0 {
		container["env"] = envVars
	}

	template := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				"airunway.ai/model-deployment": md.Name,
			},
		},
		"spec": map[string]interface{}{
			"containers": []interface{}{container},
		},
	}

	return template, nil
}

func resolveLlamaCppModelArg(md *airunwayv1alpha1.ModelDeployment) string {
	if md.Spec.Model.Source == airunwayv1alpha1.ModelSourceCustom || md.Spec.Model.ID == "" {
		return ""
	}

	if md.Spec.Engine.Args != nil {
		if ggufURL := md.Spec.Engine.Args["ggufUrl"]; ggufURL != "" {
			return ggufURL
		}
	}

	return fmt.Sprintf("huggingface://%s", md.Spec.Model.ID)
}

// buildResourceRequests creates resource requests from ResourceSpec
func (t *Transformer) buildResourceRequests(spec *airunwayv1alpha1.ResourceSpec) map[string]interface{} {
	if spec == nil {
		return nil
	}

	requests := map[string]interface{}{}

	if spec.Memory != "" {
		requests["memory"] = spec.Memory
	}
	if spec.CPU != "" {
		requests["cpu"] = spec.CPU
	}

	if len(requests) == 0 {
		return nil
	}

	return map[string]interface{}{
		"requests": requests,
	}
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

// sanitizeLabelValue ensures a value is valid for a Kubernetes label
func sanitizeLabelValue(value string) string {
	// Labels must be 63 chars or less, start and end with alphanumeric
	if len(value) > 63 {
		value = value[:63]
	}
	// Replace invalid characters with dashes
	value = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, value)
	// Trim leading/trailing dashes
	value = strings.Trim(value, "-_.")
	return value
}

// boolPtr returns a pointer to a bool
func boolPtr(b bool) *bool {
	return &b
}

// applyOverrides deep-merges spec.provider.overrides into the unstructured object.
// This is the escape hatch that lets users set arbitrary fields on the provider CRD.
func applyOverrides(obj *unstructured.Unstructured, md *airunwayv1alpha1.ModelDeployment) error {
	if md.Spec.Provider == nil || md.Spec.Provider.Overrides == nil {
		return nil
	}

	var overrides map[string]interface{}
	if err := json.Unmarshal(md.Spec.Provider.Overrides.Raw, &overrides); err != nil {
		return fmt.Errorf("failed to unmarshal overrides: %w", err)
	}

	// Block dangerous top-level keys to prevent privilege escalation
	blockedKeys := []string{"apiVersion", "kind", "metadata", "status"}
	for _, key := range blockedKeys {
		if _, exists := overrides[key]; exists {
			return fmt.Errorf("overriding %q is not allowed", key)
		}
	}

	obj.Object = deepMerge(obj.Object, overrides)
	return nil
}

// deepMerge recursively merges src into dst.
// For maps, values are merged recursively. A nil src value deletes the field.
// For all other types, src overwrites dst.
func deepMerge(dst, src map[string]interface{}) map[string]interface{} {
	for key, srcVal := range src {
		if srcVal == nil {
			delete(dst, key)
			continue
		}
		if dstVal, exists := dst[key]; exists {
			srcMap, srcOk := srcVal.(map[string]interface{})
			dstMap, dstOk := dstVal.(map[string]interface{})
			if srcOk && dstOk {
				dst[key] = deepMerge(dstMap, srcMap)
				continue
			}
		}
		dst[key] = srcVal
	}
	return dst
}
