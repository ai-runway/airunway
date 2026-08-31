package kuberay

import (
	"context"
	"fmt"
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func newTestMD(name, namespace string) *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID:     "meta-llama/Llama-2-7b-chat-hf",
				Source: airunwayv1alpha1.ModelSourceHuggingFace,
			},
			Engine: airunwayv1alpha1.EngineSpec{
				Type: airunwayv1alpha1.EngineTypeVLLM,
			},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{
					Count: 1,
				},
			},
		},
	}
}

func TestTransformAggregated(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}

	rs := resources[0]
	if rs.GetKind() != RayServiceKind {
		t.Errorf("expected kind %s, got %s", RayServiceKind, rs.GetKind())
	}
	if rs.GetName() != "test-model" {
		t.Errorf("expected name 'test-model', got %s", rs.GetName())
	}
	if rs.GetAPIVersion() != fmt.Sprintf("%s/%s", RayAPIGroup, RayAPIVersion) {
		t.Errorf("expected apiVersion 'ray.io/v1', got %s", rs.GetAPIVersion())
	}

	// Check owner references
	ownerRefs := rs.GetOwnerReferences()
	if len(ownerRefs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(ownerRefs))
	}

	// Check labels
	labels := rs.GetLabels()
	if labels["airunway.ai/managed-by"] != "airunway" {
		t.Errorf("expected managed-by label 'airunway'")
	}
	if labels["airunway.ai/engine-type"] != "vllm" {
		t.Errorf("expected engine-type label 'vllm'")
	}

	// Check spec
	spec, _, _ := unstructured.NestedMap(rs.Object, "spec")
	serveConfig, _ := spec["serveConfigV2"].(string)
	if !strings.Contains(serveConfig, "num_replicas: 1") {
		t.Errorf("expected num_replicas: 1 in serveConfig, got: %s", serveConfig)
	}

	// Check rayClusterConfig exists
	rayCluster, _, _ := unstructured.NestedMap(rs.Object, "spec", "rayClusterConfig")
	if rayCluster == nil {
		t.Fatal("expected rayClusterConfig")
	}

	// Check head group spec
	headGroup, _, _ := unstructured.NestedMap(rs.Object, "spec", "rayClusterConfig", "headGroupSpec")
	if headGroup == nil {
		t.Fatal("expected headGroupSpec")
	}

	// Check worker group specs
	workerGroups, _, _ := unstructured.NestedSlice(rs.Object, "spec", "rayClusterConfig", "workerGroupSpecs")
	if len(workerGroups) != 1 {
		t.Fatalf("expected 1 worker group, got %d", len(workerGroups))
	}
}

func TestTransformWithScaling(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{Replicas: 3}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rs := resources[0]
	spec, _, _ := unstructured.NestedMap(rs.Object, "spec")
	serveConfig, _ := spec["serveConfigV2"].(string)
	if !strings.Contains(serveConfig, "num_replicas: 3") {
		t.Errorf("expected num_replicas: 3 in serveConfig")
	}
}

func TestTransformDisaggregated(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 2,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 2, Type: "nvidia.com/gpu"},
			Memory:   "64Gi",
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 3,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
			Memory:   "32Gi",
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rs := resources[0]
	workerGroups, _, _ := unstructured.NestedSlice(rs.Object, "spec", "rayClusterConfig", "workerGroupSpecs")
	if len(workerGroups) != 2 {
		t.Fatalf("expected 2 worker groups for disaggregated, got %d", len(workerGroups))
	}

	// Check prefill group
	prefill, _ := workerGroups[0].(map[string]interface{})
	if prefill["groupName"] != "prefill-workers" {
		t.Errorf("expected prefill-workers group name, got %v", prefill["groupName"])
	}
	if prefill["replicas"] != int64(2) {
		t.Errorf("expected prefill replicas 2, got %v", prefill["replicas"])
	}

	// Check decode group
	decode, _ := workerGroups[1].(map[string]interface{})
	if decode["groupName"] != "decode-workers" {
		t.Errorf("expected decode-workers group name, got %v", decode["groupName"])
	}
	if decode["replicas"] != int64(3) {
		t.Errorf("expected decode replicas 3, got %v", decode["replicas"])
	}
}

func TestTransformDisaggregatedDefaultMemory(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
			// No memory → should default
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
			// No memory → should default
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rs := resources[0]
	workerGroups, _, _ := unstructured.NestedSlice(rs.Object, "spec", "rayClusterConfig", "workerGroupSpecs")
	for _, wg := range workerGroups {
		group, _ := wg.(map[string]interface{})
		template, _ := group["template"].(map[string]interface{})
		spec, _ := template["spec"].(map[string]interface{})
		containers, _ := spec["containers"].([]interface{})
		container, _ := containers[0].(map[string]interface{})
		res, _ := container["resources"].(map[string]interface{})
		limits, _ := res["limits"].(map[string]interface{})
		if limits["memory"] != DefaultWorkerMemory {
			t.Errorf("expected default worker memory %s, got %v", DefaultWorkerMemory, limits["memory"])
		}
	}
}

func TestTransformWithPodTemplateLabels(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.PodTemplate = &airunwayv1alpha1.PodTemplateSpec{
		Metadata: &airunwayv1alpha1.PodTemplateMetadata{
			Labels:      map[string]string{"custom": "label"},
			Annotations: map[string]string{"custom": "annotation"},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rs := resources[0]
	if rs.GetLabels()["custom"] != "label" {
		t.Error("expected custom label")
	}
	if rs.GetAnnotations()["custom"] != "annotation" {
		t.Error("expected custom annotation")
	}
}

func TestBuildEngineArgs(t *testing.T) {
	tr := NewTransformer()

	// Basic
	md := newTestMD("test", "default")
	args := tr.buildEngineArgs(md)
	if !strings.Contains(args, "--model meta-llama/Llama-2-7b-chat-hf") {
		t.Errorf("expected --model in args: %s", args)
	}

	// With context length
	ctxLen := int32(4096)
	md.Spec.Engine.ContextLength = &ctxLen
	args = tr.buildEngineArgs(md)
	if !strings.Contains(args, "--max-model-len 4096") {
		t.Errorf("expected --max-model-len in args: %s", args)
	}

	// With served name
	md.Spec.Model.ServedName = "my-model"
	args = tr.buildEngineArgs(md)
	if !strings.Contains(args, "--served-model-name my-model") {
		t.Errorf("expected --served-model-name in args: %s", args)
	}

	// With trust remote code
	md.Spec.Engine.TrustRemoteCode = true
	args = tr.buildEngineArgs(md)
	if !strings.Contains(args, "--trust-remote-code") {
		t.Errorf("expected --trust-remote-code in args: %s", args)
	}
}

func TestBuildEngineArgsWithCustomArgs(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Engine.Args = map[string]string{
		"tensor-parallel-size": "4",
	}
	args := tr.buildEngineArgs(md)
	if !strings.Contains(args, "--tensor-parallel-size") {
		t.Errorf("expected --tensor-parallel-size in args: %s", args)
	}
}

func TestBuildEngineArgsDeterministicOrder(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Engine.Args = map[string]string{
		"zebra-param":         "z",
		"alpha-param":         "a",
		"middle-param":        "m",
		"beta-param":          "b",
		"enable-some-feature": "",
		"data-path":           "/data",
	}

	// Run multiple times and verify identical output
	first := tr.buildEngineArgs(md)
	for i := 0; i < 20; i++ {
		result := tr.buildEngineArgs(md)
		if result != first {
			t.Fatalf("non-deterministic output on iteration %d:\n  first: %s\n  got:   %s", i, first, result)
		}
	}

	// Verify alphabetical key order of custom args
	alphaIdx := strings.Index(first, "--alpha-param")
	betaIdx := strings.Index(first, "--beta-param")
	dataIdx := strings.Index(first, "--data-path")
	enableIdx := strings.Index(first, "--enable-some-feature")
	middleIdx := strings.Index(first, "--middle-param")
	zebraIdx := strings.Index(first, "--zebra-param")

	if alphaIdx > betaIdx || betaIdx > dataIdx || dataIdx > enableIdx || enableIdx > middleIdx || middleIdx > zebraIdx {
		t.Errorf("custom args not in alphabetical order: %s", first)
	}
}

func TestBuildEnvVars(t *testing.T) {
	tr := NewTransformer()

	// Empty
	md := newTestMD("test", "default")
	envVars := tr.buildEnvVars(md)
	if len(envVars) != 0 {
		t.Errorf("expected empty env vars, got %d", len(envVars))
	}

	// With user env
	md.Spec.Env = []corev1.EnvVar{
		{Name: "FOO", Value: "bar"},
	}
	envVars = tr.buildEnvVars(md)
	if len(envVars) != 1 {
		t.Fatalf("expected 1 env var, got %d", len(envVars))
	}
	env, _ := envVars[0].(map[string]interface{})
	if env["name"] != "FOO" || env["value"] != "bar" {
		t.Errorf("expected FOO=bar, got %v", env)
	}

	// With HF token
	md.Spec.Secrets = &airunwayv1alpha1.SecretsSpec{HuggingFaceToken: "my-secret"}
	envVars = tr.buildEnvVars(md)
	if len(envVars) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(envVars))
	}
	hfEnv, _ := envVars[1].(map[string]interface{})
	if hfEnv["name"] != "HF_TOKEN" {
		t.Errorf("expected HF_TOKEN, got %v", hfEnv)
	}

	// With secret ref
	md.Spec.Env = []corev1.EnvVar{
		{
			Name: "SECRET_VAR",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
					Key:                  "my-key",
				},
			},
		},
	}
	md.Spec.Secrets = nil
	envVars = tr.buildEnvVars(md)
	if len(envVars) != 1 {
		t.Fatalf("expected 1 env var, got %d", len(envVars))
	}
	secretEnv, _ := envVars[0].(map[string]interface{})
	valueFrom, _ := secretEnv["valueFrom"].(map[string]interface{})
	secretRef, _ := valueFrom["secretKeyRef"].(map[string]interface{})
	if secretRef["name"] != "my-secret" || secretRef["key"] != "my-key" {
		t.Errorf("expected secret ref, got %v", secretRef)
	}
}

func TestGetImage(t *testing.T) {
	tr := NewTransformer()

	// Custom image
	md := newTestMD("test", "default")
	md.Spec.Image = "custom:v1"
	if img := tr.getImage(md); img != "custom:v1" {
		t.Errorf("expected custom image, got %s", img)
	}

	// Default image
	md.Spec.Image = ""
	if img := tr.getImage(md); img != DefaultImage {
		t.Errorf("expected default image %s, got %s", DefaultImage, img)
	}
}

func TestBuildHeadGroupSpec(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")

	headSpec := tr.buildHeadGroupSpec(md)

	// Check template
	template, _ := headSpec["template"].(map[string]interface{})
	spec, _ := template["spec"].(map[string]interface{})
	containers, _ := spec["containers"].([]interface{})
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}

	container, _ := containers[0].(map[string]interface{})
	if container["name"] != "ray-head" {
		t.Errorf("expected name 'ray-head', got %v", container["name"])
	}

	// Check env vars
	envVars, _ := container["env"].([]interface{})
	foundModelID := false
	foundEngineArgs := false
	for _, ev := range envVars {
		e, _ := ev.(map[string]interface{})
		if e["name"] == "MODEL_ID" {
			foundModelID = true
			if e["value"] != "meta-llama/Llama-2-7b-chat-hf" {
				t.Errorf("expected MODEL_ID value, got %v", e["value"])
			}
		}
		if e["name"] == "VLLM_ENGINE_ARGS" {
			foundEngineArgs = true
		}
	}
	if !foundModelID {
		t.Error("expected MODEL_ID env var")
	}
	if !foundEngineArgs {
		t.Error("expected VLLM_ENGINE_ARGS env var")
	}
}

func TestBuildHeadGroupSpecWithCustomMemory(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
		Memory: "64Gi",
		GPU:    &airunwayv1alpha1.GPUSpec{Count: 1},
	}

	headSpec := tr.buildHeadGroupSpec(md)
	template, _ := headSpec["template"].(map[string]interface{})
	spec, _ := template["spec"].(map[string]interface{})
	containers, _ := spec["containers"].([]interface{})
	container, _ := containers[0].(map[string]interface{})
	res, _ := container["resources"].(map[string]interface{})
	limits, _ := res["limits"].(map[string]interface{})
	if limits["memory"] != "64Gi" {
		t.Errorf("expected memory 64Gi, got %v", limits["memory"])
	}
}

func TestBuildAggregatedWorkerGroup(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{Replicas: 2}
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
		GPU: &airunwayv1alpha1.GPUSpec{Count: 4, Type: "nvidia.com/gpu"},
	}

	groups := tr.buildAggregatedWorkerGroup(md)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}

	group, _ := groups[0].(map[string]interface{})
	if group["replicas"] != int64(2) {
		t.Errorf("expected replicas 2, got %v", group["replicas"])
	}
	if group["groupName"] != "gpu-workers" {
		t.Errorf("expected groupName 'gpu-workers', got %v", group["groupName"])
	}
}

func TestBuildAggregatedWorkerGroupCustomGPU(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
		GPU:    &airunwayv1alpha1.GPUSpec{Count: 2, Type: "amd.com/gpu"},
		Memory: "64Gi",
	}

	groups := tr.buildAggregatedWorkerGroup(md)
	group, _ := groups[0].(map[string]interface{})
	template, _ := group["template"].(map[string]interface{})
	spec, _ := template["spec"].(map[string]interface{})
	containers, _ := spec["containers"].([]interface{})
	container, _ := containers[0].(map[string]interface{})
	res, _ := container["resources"].(map[string]interface{})
	limits, _ := res["limits"].(map[string]interface{})
	if limits["amd.com/gpu"] != "2" {
		t.Errorf("expected amd.com/gpu=2, got %v", limits["amd.com/gpu"])
	}
	if limits["memory"] != "64Gi" {
		t.Errorf("expected memory=64Gi, got %v", limits["memory"])
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"with/slashes", "with-slashes"},
		{"-leading", "leading"},
		{"trailing-", "trailing"},
		{"", ""},
	}

	for _, tt := range tests {
		result := sanitizeLabelValue(tt.input)
		if result != tt.expected {
			t.Errorf("sanitizeLabelValue(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
	}
}

func TestBoolPtr(t *testing.T) {
	p := boolPtr(true)
	if *p != true {
		t.Error("expected true")
	}
}

func TestBuildDisaggregatedWorkerGroupsWithCustomGPUType(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 2, Type: "amd.com/gpu"},
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1, Type: "amd.com/gpu"},
		},
	}

	groups := tr.buildDisaggregatedWorkerGroups(md)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// Check prefill has custom GPU type
	prefill, _ := groups[0].(map[string]interface{})
	pTemplate, _ := prefill["template"].(map[string]interface{})
	pSpec, _ := pTemplate["spec"].(map[string]interface{})
	pContainers, _ := pSpec["containers"].([]interface{})
	pContainer, _ := pContainers[0].(map[string]interface{})
	pRes, _ := pContainer["resources"].(map[string]interface{})
	pLimits, _ := pRes["limits"].(map[string]interface{})
	if pLimits["amd.com/gpu"] != "2" {
		t.Errorf("expected prefill amd.com/gpu=2, got %v", pLimits["amd.com/gpu"])
	}
}
