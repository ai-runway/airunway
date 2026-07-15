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

package llmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// DefaultVLLMImage is the default container image for llm-d vLLM deployments
	DefaultVLLMImage = "vllm/vllm-openai:v0.9.1"

	// DefaultVLLMPort is the default serving port for vLLM
	DefaultVLLMPort = int64(8000)

	// GPUResourceKey is the Kubernetes resource key for NVIDIA GPUs
	GPUResourceKey = "nvidia.com/gpu"

	// KVTransferConfigPrefill is the vLLM KV transfer config for prefill workers
	KVTransferConfigPrefill = `{"kv_connector":"PyNcclConnector","kv_role":"kv_producer"}`

	// KVTransferConfigDecode is the vLLM KV transfer config for decode workers
	KVTransferConfigDecode = `{"kv_connector":"PyNcclConnector","kv_role":"kv_consumer"}`
)

// Transformer handles transformation of ModelDeployment to llm-d Deployments and Services
type Transformer struct{}

// NewTransformer creates a new llm-d transformer
func NewTransformer() *Transformer {
	return &Transformer{}
}

// Transform converts a ModelDeployment to llm-d Deployments and Services.
//
// Aggregated mode returns [Deployment, Service].
// Disaggregated mode returns [decode Deployment, prefill Deployment, decode Service, prefill Service].
// resources[0] is always the primary resource used for status tracking.
func (t *Transformer) Transform(ctx context.Context, md *airunwayv1alpha1.ModelDeployment) ([]*unstructured.Unstructured, error) {
	if md.ResolvedEngineType() != airunwayv1alpha1.EngineTypeVLLM {
		return nil, fmt.Errorf("llm-d provider only supports vllm engine, got %s", md.ResolvedEngineType())
	}

	servingMode := airunwayv1alpha1.ServingModeAggregated
	if md.Spec.Serving != nil && md.Spec.Serving.Mode != "" {
		servingMode = md.Spec.Serving.Mode
	}

	if servingMode == airunwayv1alpha1.ServingModeDisaggregated {
		return t.transformDisaggregated(md)
	}
	return t.transformAggregated(md)
}

// transformAggregated creates a single Deployment + Service for aggregated serving.
func (t *Transformer) transformAggregated(md *airunwayv1alpha1.ModelDeployment) ([]*unstructured.Unstructured, error) {
	replicas := int64(1)
	if md.Spec.Scaling != nil && md.Spec.Scaling.Replicas > 0 {
		replicas = int64(md.Spec.Scaling.Replicas)
	}

	args, err := t.buildVLLMArgs(md, "", 0)
	if err != nil {
		return nil, fmt.Errorf("failed to build vLLM args: %w", err)
	}

	deployment, err := t.buildDeployment(md, md.Name, replicas, md.Spec.Resources, args)
	if err != nil {
		return nil, fmt.Errorf("failed to build Deployment: %w", err)
	}

	svc := t.buildService(md, md.Name, md.Name)

	return []*unstructured.Unstructured{deployment, svc}, nil
}

// transformDisaggregated creates separate decode + prefill Deployments and Services.
func (t *Transformer) transformDisaggregated(md *airunwayv1alpha1.ModelDeployment) ([]*unstructured.Unstructured, error) {
	if md.Spec.Scaling == nil {
		return nil, fmt.Errorf("spec.scaling is required for disaggregated serving mode")
	}
	if md.Spec.Scaling.Decode == nil {
		return nil, fmt.Errorf("spec.scaling.decode is required for disaggregated serving mode")
	}
	if md.Spec.Scaling.Prefill == nil {
		return nil, fmt.Errorf("spec.scaling.prefill is required for disaggregated serving mode")
	}

	decodeResources := componentToResourceSpec(md.Spec.Scaling.Decode)
	prefillResources := componentToResourceSpec(md.Spec.Scaling.Prefill)

	decodeArgs, err := t.buildVLLMArgs(md, KVTransferConfigDecode, md.Spec.Scaling.Decode.GPU.Count)
	if err != nil {
		return nil, fmt.Errorf("failed to build decode vLLM args: %w", err)
	}

	prefillArgs, err := t.buildVLLMArgs(md, KVTransferConfigPrefill, md.Spec.Scaling.Prefill.GPU.Count)
	if err != nil {
		return nil, fmt.Errorf("failed to build prefill vLLM args: %w", err)
	}

	decodeName := md.Name + "-decode"
	prefillName := md.Name + "-prefill"

	decodeDeployment, err := t.buildDeployment(md, decodeName, int64(md.Spec.Scaling.Decode.Replicas), decodeResources, decodeArgs)
	if err != nil {
		return nil, fmt.Errorf("failed to build decode Deployment: %w", err)
	}

	prefillDeployment, err := t.buildDeployment(md, prefillName, int64(md.Spec.Scaling.Prefill.Replicas), prefillResources, prefillArgs)
	if err != nil {
		return nil, fmt.Errorf("failed to build prefill Deployment: %w", err)
	}

	// Decode service is the main serving endpoint; prefill service is internal
	decodeSvc := t.buildService(md, decodeName, decodeName)
	prefillSvc := t.buildService(md, prefillName, prefillName)

	// decode Deployment is resources[0] (primary for status tracking)
	return []*unstructured.Unstructured{decodeDeployment, prefillDeployment, decodeSvc, prefillSvc}, nil
}

// buildDeployment constructs an apps/v1 Deployment as unstructured.
func (t *Transformer) buildDeployment(md *airunwayv1alpha1.ModelDeployment, name string, replicas int64, resources *airunwayv1alpha1.ResourceSpec, args []string) (*unstructured.Unstructured, error) {
	d := &unstructured.Unstructured{}
	d.SetAPIVersion("apps/v1")
	d.SetKind("Deployment")
	d.SetName(name)
	d.SetNamespace(md.Namespace)

	// OwnerReference — same namespace, so OwnerReference is valid
	d.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         md.APIVersion,
			Kind:               md.Kind,
			Name:               md.Name,
			UID:                md.UID,
			Controller:         boolPtr(true),
			BlockOwnerDeletion: boolPtr(true),
		},
	})

	// Labels on Deployment metadata
	deployLabels := t.buildLabels(md)
	// Merge podTemplate labels
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil {
		for k, v := range md.Spec.PodTemplate.Metadata.Labels {
			deployLabels[k] = v
		}
	}
	d.SetLabels(deployLabels)

	// Annotations on Deployment metadata
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil && len(md.Spec.PodTemplate.Metadata.Annotations) > 0 {
		d.SetAnnotations(md.Spec.PodTemplate.Metadata.Annotations)
	}

	// Pod selector labels (must be a stable subset)
	selectorLabels := map[string]interface{}{
		"airunway.ai/deployment": md.Name,
		"app":                    name,
	}

	// Pod template labels (must include selector labels)
	podLabels := map[string]interface{}{
		"airunway.ai/deployment": md.Name,
		"app":                    name,
	}
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil {
		for k, v := range md.Spec.PodTemplate.Metadata.Labels {
			podLabels[k] = v
		}
	}
	// Re-apply selector labels to prevent user overrides from breaking selectors
	for k, v := range selectorLabels {
		podLabels[k] = v
	}

	image := t.getImage(md)

	container, err := t.buildContainer(md, image, args, resources)
	if err != nil {
		return nil, err
	}

	podSpec := map[string]interface{}{
		"containers": []interface{}{container},
	}

	if len(md.Spec.NodeSelector) > 0 {
		nodeSelector := make(map[string]interface{})
		for k, v := range md.Spec.NodeSelector {
			nodeSelector[k] = v
		}
		podSpec["nodeSelector"] = nodeSelector
	}

	if len(md.Spec.Tolerations) > 0 {
		podSpec["tolerations"] = t.buildTolerations(md)
	}

	podTemplateAnnotations := map[string]interface{}{}
	if md.Spec.PodTemplate != nil && md.Spec.PodTemplate.Metadata != nil {
		for k, v := range md.Spec.PodTemplate.Metadata.Annotations {
			podTemplateAnnotations[k] = v
		}
	}

	spec := map[string]interface{}{
		"replicas": replicas,
		"selector": map[string]interface{}{
			"matchLabels": selectorLabels,
		},
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels":      podLabels,
				"annotations": podTemplateAnnotations,
			},
			"spec": podSpec,
		},
	}

	if err := unstructured.SetNestedField(d.Object, spec, "spec"); err != nil {
		return nil, fmt.Errorf("failed to set deployment spec: %w", err)
	}

	// Apply escape hatch overrides last
	if err := applyOverrides(d, md); err != nil {
		return nil, fmt.Errorf("failed to apply provider overrides: %w", err)
	}

	return d, nil
}

// buildService constructs a core/v1 Service as unstructured.
// selectorApp is the value of the "app" label used to target pods.
func (t *Transformer) buildService(md *airunwayv1alpha1.ModelDeployment, name, selectorApp string) *unstructured.Unstructured {
	svc := &unstructured.Unstructured{}
	svc.SetAPIVersion("v1")
	svc.SetKind("Service")
	svc.SetName(name)
	svc.SetNamespace(md.Namespace)

	svc.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         md.APIVersion,
			Kind:               md.Kind,
			Name:               md.Name,
			UID:                md.UID,
			Controller:         boolPtr(true),
			BlockOwnerDeletion: boolPtr(true),
		},
	})

	svc.SetLabels(t.buildLabels(md))

	spec := map[string]interface{}{
		"type": "ClusterIP",
		"selector": map[string]interface{}{
			"airunway.ai/deployment": md.Name,
			"app":                    selectorApp,
		},
		"ports": []interface{}{
			map[string]interface{}{
				"name":       "http",
				"port":       DefaultVLLMPort,
				"targetPort": DefaultVLLMPort,
				"protocol":   "TCP",
			},
		},
	}

	_ = unstructured.SetNestedField(svc.Object, spec, "spec")
	return svc
}

// buildContainer constructs the vLLM container map.
func (t *Transformer) buildContainer(md *airunwayv1alpha1.ModelDeployment, image string, args []string, resources *airunwayv1alpha1.ResourceSpec) (map[string]interface{}, error) {
	argsList := make([]interface{}, len(args))
	for i, a := range args {
		argsList[i] = a
	}

	ports := []interface{}{
		map[string]interface{}{
			"name":          "http",
			"containerPort": DefaultVLLMPort,
			"protocol":      "TCP",
		},
	}

	container := map[string]interface{}{
		"name":  "vllm",
		"image": image,
		"args":  argsList,
		"ports": ports,
	}

	// Resource limits/requests
	resMap := t.buildResourceLimits(resources)
	if len(resMap) > 0 {
		container["resources"] = resMap
	}

	// Environment variables
	envVars := t.buildEnvVars(md)
	if len(envVars) > 0 {
		container["env"] = envVars
	}

	return container, nil
}

// buildVLLMArgs constructs the vLLM command-line arguments.
// kvTransferConfig is optional; pass "" for aggregated mode.
// gpuCount overrides the GPU count used for tensor parallelism (0 means use top-level spec.resources).
func (t *Transformer) buildVLLMArgs(md *airunwayv1alpha1.ModelDeployment, kvTransferConfig string, gpuCount int32) ([]string, error) {
	var args []string

	// Model
	args = append(args, "--model", md.Spec.Model.ID)

	// Served model name
	if md.Spec.Model.ServedName != "" {
		args = append(args, "--served-model-name", md.Spec.Model.ServedName)
	}

	// Context length
	if md.Spec.Engine.ContextLength != nil {
		args = append(args, "--max-model-len", fmt.Sprintf("%d", *md.Spec.Engine.ContextLength))
	}

	// Trust remote code
	if md.Spec.Engine.TrustRemoteCode {
		args = append(args, "--trust-remote-code")
	}

	// Prefix caching. vLLM's prefix caching helps the EPP get real cache hits
	// when the llm-d Router routes requests to a pod that has previously seen
	// a similar prompt. Defaults to true via the CRD; explicitly map both
	// states so an override of false produces --no-enable-prefix-caching.
	if md.Spec.Engine.EnablePrefixCaching {
		args = append(args, "--enable-prefix-caching")
	} else {
		args = append(args, "--no-enable-prefix-caching")
	}

	// Eager execution (disables CUDA graphs). Off by default; only emit the
	// flag when explicitly requested.
	if md.Spec.Engine.EnforceEager {
		args = append(args, "--enforce-eager")
	}

	// Tensor parallelism from GPU count
	tpCount := gpuCount
	if tpCount == 0 && md.Spec.Resources != nil && md.Spec.Resources.GPU != nil {
		tpCount = md.Spec.Resources.GPU.Count
	}
	if tpCount > 1 {
		args = append(args, "--tensor-parallel-size", fmt.Sprintf("%d", tpCount))
	}

	// KV transfer config for disaggregated mode
	if kvTransferConfig != "" {
		args = append(args, "--kv-transfer-config", kvTransferConfig)
	}

	// Custom engine args (sorted for deterministic output)
	keys := make([]string, 0, len(md.Spec.Engine.Args))
	for k := range md.Spec.Engine.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !isValidArgKey(key) {
			return nil, fmt.Errorf("invalid engine arg key %q: must contain only alphanumeric characters, hyphens, and underscores", key)
		}
		value := md.Spec.Engine.Args[key]
		if value != "" {
			args = append(args, fmt.Sprintf("--%s", key), value)
		} else {
			args = append(args, fmt.Sprintf("--%s", key))
		}
	}

	if len(md.Spec.Engine.ExtraArgs) > 0 {
		args = append(args, md.Spec.Engine.ExtraArgs...)
	}

	return args, nil
}

// buildResourceLimits creates resource limits and requests from ResourceSpec.
func (t *Transformer) buildResourceLimits(spec *airunwayv1alpha1.ResourceSpec) map[string]interface{} {
	if spec == nil {
		return nil
	}

	limits := map[string]interface{}{}
	requests := map[string]interface{}{}

	if spec.GPU != nil && spec.GPU.Count > 0 {
		gpuCount := fmt.Sprintf("%d", spec.GPU.Count)
		limits[GPUResourceKey] = gpuCount
		requests[GPUResourceKey] = gpuCount
	}
	if spec.Memory != "" {
		limits["memory"] = spec.Memory
		requests["memory"] = spec.Memory
	}
	if spec.CPU != "" {
		requests["cpu"] = spec.CPU
	}

	if len(limits) == 0 && len(requests) == 0 {
		return nil
	}

	result := map[string]interface{}{}
	if len(limits) > 0 {
		result["limits"] = limits
	}
	if len(requests) > 0 {
		result["requests"] = requests
	}
	return result
}

// buildEnvVars constructs environment variables including HF_TOKEN from secrets.
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

// buildTolerations converts tolerations from ModelDeployment to unstructured format.
func (t *Transformer) buildTolerations(md *airunwayv1alpha1.ModelDeployment) []interface{} {
	tolerations := make([]interface{}, len(md.Spec.Tolerations))
	for i, tol := range md.Spec.Tolerations {
		tolMap := map[string]interface{}{
			"key":      tol.Key,
			"operator": string(tol.Operator),
		}
		if tol.Value != "" {
			tolMap["value"] = tol.Value
		}
		if tol.Effect != "" {
			tolMap["effect"] = string(tol.Effect)
		}
		if tol.TolerationSeconds != nil {
			tolMap["tolerationSeconds"] = *tol.TolerationSeconds
		}
		tolerations[i] = tolMap
	}
	return tolerations
}

// buildLabels creates the standard set of labels for llm-d resources.
func (t *Transformer) buildLabels(md *airunwayv1alpha1.ModelDeployment) map[string]string {
	return map[string]string{
		"airunway.ai/managed-by":   "airunway",
		"airunway.ai/deployment":   md.Name,
		"airunway.ai/model-source": string(md.Spec.Model.Source),
		"airunway.ai/engine-type":  string(md.ResolvedEngineType()),
	}
}

// getImage returns the container image to use.
func (t *Transformer) getImage(md *airunwayv1alpha1.ModelDeployment) string {
	if image := md.Spec.ImageOverride(); image != "" {
		return image
	}
	return DefaultVLLMImage
}

// componentToResourceSpec converts a ComponentScalingSpec to a ResourceSpec
// for use in building container resources.
func componentToResourceSpec(comp *airunwayv1alpha1.ComponentScalingSpec) *airunwayv1alpha1.ResourceSpec {
	if comp == nil {
		return nil
	}
	spec := &airunwayv1alpha1.ResourceSpec{
		Memory: comp.Memory,
	}
	if comp.GPU != nil {
		spec.GPU = &airunwayv1alpha1.GPUSpec{
			Count: comp.GPU.Count,
		}
	}
	return spec
}

// isValidArgKey checks that an arg key contains only alphanumeric chars, hyphens, and underscores,
// and does not start with a hyphen.
func isValidArgKey(key string) bool {
	if len(key) == 0 {
		return false
	}
	if key[0] == '-' {
		return false
	}
	for _, r := range key {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// sanitizeLabelValue ensures a value is valid for a Kubernetes label.
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

// boolPtr returns a pointer to a bool.
func boolPtr(b bool) *bool {
	return &b
}

// applyOverrides deep-merges spec.provider.overrides into the unstructured object.
// This is the escape hatch that lets users set arbitrary fields on the provider resource.
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

	if hasNestedMapPath(overrides, "spec", "template", "spec") {
		return fmt.Errorf("overriding %q is not allowed", "spec.template.spec")
	}

	obj.Object = deepMerge(obj.Object, overrides)
	return nil
}

// hasNestedMapPath reports whether a nested map path exists in m.
func hasNestedMapPath(m map[string]interface{}, path ...string) bool {
	if len(path) == 0 {
		return false
	}

	current := m
	for i, key := range path {
		value, exists := current[key]
		if !exists {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		next, ok := value.(map[string]interface{})
		if !ok {
			return false
		}
		current = next
	}

	return false
}

// deepMerge recursively merges src into dst.
// For maps, values are merged recursively. For all other types, src overwrites dst.
func deepMerge(dst, src map[string]interface{}) map[string]interface{} {
	for key, srcVal := range src {
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
