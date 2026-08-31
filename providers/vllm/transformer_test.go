package vllm

import (
	"context"
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func newTestMD(name, namespace string) *airunwayv1alpha1.ModelDeployment {
	return &airunwayv1alpha1.ModelDeployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "airunway.ai/v1alpha1",
			Kind:       "ModelDeployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("test-uid"),
		},
		Spec: airunwayv1alpha1.ModelDeploymentSpec{
			Model: airunwayv1alpha1.ModelSpec{
				ID:     "meta-llama/Llama-3.1-8B-Instruct",
				Source: airunwayv1alpha1.ModelSourceHuggingFace,
			},
			Engine: airunwayv1alpha1.EngineSpec{
				Type: airunwayv1alpha1.EngineTypeVLLM,
			},
			Resources: &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: 1},
			},
		},
	}
}

func TestTransformAggregatedBasic(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should return Deployment + Service
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(resources))
	}

	deploy := resources[0]
	svc := resources[1]

	// Deployment checks
	if deploy.GetKind() != "Deployment" {
		t.Errorf("expected Deployment, got %s", deploy.GetKind())
	}
	if deploy.GetAPIVersion() != "apps/v1" {
		t.Errorf("expected apps/v1, got %s", deploy.GetAPIVersion())
	}
	if deploy.GetName() != "test-model" {
		t.Errorf("expected name 'test-model', got %s", deploy.GetName())
	}
	if deploy.GetNamespace() != "default" {
		t.Errorf("expected namespace 'default', got %s", deploy.GetNamespace())
	}

	// Service checks
	if svc.GetKind() != "Service" {
		t.Errorf("expected Service, got %s", svc.GetKind())
	}
	if svc.GetName() != "test-model" {
		t.Errorf("expected service name 'test-model', got %s", svc.GetName())
	}
}

func TestTransformAggregatedOwnerReference(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	ownerRefs := deploy.GetOwnerReferences()
	if len(ownerRefs) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(ownerRefs))
	}
	if ownerRefs[0].Name != "test-model" {
		t.Errorf("expected owner ref name 'test-model', got %s", ownerRefs[0].Name)
	}
	if ownerRefs[0].UID != "test-uid" {
		t.Errorf("expected owner ref UID 'test-uid', got %s", ownerRefs[0].UID)
	}
	if *ownerRefs[0].Controller != true {
		t.Error("expected controller=true on owner ref")
	}
}

func TestTransformAggregatedLabels(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	labels := deploy.GetLabels()
	if labels["airunway.ai/managed-by"] != "airunway" {
		t.Errorf("expected managed-by label 'airunway', got %s", labels["airunway.ai/managed-by"])
	}
	if labels["airunway.ai/deployment"] != "test-model" {
		t.Errorf("expected deployment label 'test-model', got %s", labels["airunway.ai/deployment"])
	}
	if labels["airunway.ai/engine-type"] != "vllm" {
		t.Errorf("expected engine-type label 'vllm', got %s", labels["airunway.ai/engine-type"])
	}
}

func TestTransformAggregatedReplicas(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{Replicas: 3}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	replicas, found, _ := unstructured.NestedInt64(deploy.Object, "spec", "replicas")
	if !found || replicas != 3 {
		t.Errorf("expected 3 replicas, got %v (found=%v)", replicas, found)
	}
}

func TestTransformAggregatedDefaultReplicas(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	// No scaling spec

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	replicas, found, _ := unstructured.NestedInt64(deploy.Object, "spec", "replicas")
	if !found || replicas != 1 {
		t.Errorf("expected default 1 replica, got %v (found=%v)", replicas, found)
	}
}

func TestTransformAggregatedVLLMArgs(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Model.ServedName = "my-alias"
	contextLen := int32(4096)
	md.Spec.Engine.ContextLength = &contextLen
	md.Spec.Engine.TrustRemoteCode = true

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, found, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if !found || len(containers) == 0 {
		t.Fatal("expected containers")
	}
	container := containers[0].(map[string]interface{})
	args := argsToStrings(container["args"].([]interface{}))

	assertArg(t, args, "--model", "meta-llama/Llama-3.1-8B-Instruct")
	assertArg(t, args, "--served-model-name", "my-alias")
	assertArg(t, args, "--max-model-len", "4096")
	assertFlag(t, args, "--trust-remote-code")
}

func TestTransformAggregatedTensorParallelism(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
		GPU: &airunwayv1alpha1.GPUSpec{Count: 4},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	args := argsToStrings(container["args"].([]interface{}))

	assertArg(t, args, "--tensor-parallel-size", "4")
}

func TestTransformAggregatedNoTensorParallelForSingleGPU(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	// 1 GPU — no tensor-parallel-size needed

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	args := argsToStrings(container["args"].([]interface{}))

	for _, a := range args {
		if a == "--tensor-parallel-size" {
			t.Error("should not set --tensor-parallel-size for single GPU")
		}
	}
}

func TestTransformAggregatedDefaultImage(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	const expectedImage = "vllm/vllm-openai:cu130-nightly"
	if DefaultVLLMImage != expectedImage {
		t.Errorf("expected DefaultVLLMImage %s, got %s", expectedImage, DefaultVLLMImage)
	}
	if container["image"] != expectedImage {
		t.Errorf("expected default image %s, got %s", expectedImage, container["image"])
	}
}

func TestTransformAggregatedUsesResolvedDefaultImage(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Status.Image = &airunwayv1alpha1.ImageStatus{
		Requested: DefaultVLLMImage,
		Resolved:  "vllm/vllm-openai@sha256:default",
		Source:    "nightly",
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	if container["image"] != "vllm/vllm-openai@sha256:default" {
		t.Errorf("expected resolved default image, got %s", container["image"])
	}
}

func TestTransformAggregatedCustomImage(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Image = "my-custom-vllm:latest"

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	if container["image"] != "my-custom-vllm:latest" {
		t.Errorf("expected custom image, got %s", container["image"])
	}
}

func TestTransformAggregatedPreservesUserImageWhenStatusResolved(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.Image = "ghcr.io/acme/vllm-openai-custom:cuda12"
	md.Status.Image = &airunwayv1alpha1.ImageStatus{
		Requested: "ghcr.io/acme/vllm-openai-custom:cuda12",
		Resolved:  "ghcr.io/acme/vllm-openai-custom@sha256:custom",
		Source:    "custom",
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	if container["image"] != "ghcr.io/acme/vllm-openai-custom:cuda12" {
		t.Errorf("expected user-specified image to be preserved, got %s", container["image"])
	}
}

func TestTransformAggregatedEngineImagePreferredOverLegacyImage(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Image = "legacy-vllm:latest"
	md.Spec.Engine.Image = "engine-vllm:latest"

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	if container["image"] != "engine-vllm:latest" {
		t.Errorf("expected engine image to take precedence, got %s", container["image"])
	}
}

func TestTransformAggregatedGPUResources(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
		GPU:    &airunwayv1alpha1.GPUSpec{Count: 2},
		Memory: "32Gi",
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	resMap, ok := container["resources"].(map[string]interface{})
	if !ok {
		t.Fatal("expected resources map")
	}
	limits, _ := resMap["limits"].(map[string]interface{})
	if limits[GPUResourceKey] != "2" {
		t.Errorf("expected GPU limit 2, got %v", limits[GPUResourceKey])
	}
	if limits["memory"] != "32Gi" {
		t.Errorf("expected memory 32Gi, got %v", limits["memory"])
	}
}

func TestTransformAggregatedHFTokenSecret(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Secrets = &airunwayv1alpha1.SecretsSpec{
		HuggingFaceToken: "my-hf-secret",
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	envVars, _ := container["env"].([]interface{})

	foundHFToken := false
	for _, ev := range envVars {
		e := ev.(map[string]interface{})
		if e["name"] == "HF_TOKEN" {
			foundHFToken = true
			vf := e["valueFrom"].(map[string]interface{})
			skr := vf["secretKeyRef"].(map[string]interface{})
			if skr["name"] != "my-hf-secret" {
				t.Errorf("expected secret name 'my-hf-secret', got %v", skr["name"])
			}
			if skr["key"] != "HF_TOKEN" {
				t.Errorf("expected key 'HF_TOKEN', got %v", skr["key"])
			}
		}
	}
	if !foundHFToken {
		t.Error("expected HF_TOKEN env var")
	}
}

// A user-supplied HF_TOKEN in spec.env must not coexist with the secret-backed
// injection: the pod should carry exactly one HF_TOKEN, sourced from the secret.
func TestTransformHFTokenSecretDedupesUserEnv(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Secrets = &airunwayv1alpha1.SecretsSpec{HuggingFaceToken: "my-hf-secret"}
	md.Spec.Env = []corev1.EnvVar{
		{Name: "HF_TOKEN", Value: "plaintext-should-be-dropped"},
		{Name: "VLLM_USE_V1", Value: "1"},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	container := getContainer(t, resources[0])
	envVars, _ := container["env"].([]interface{})

	hfCount := 0
	for _, ev := range envVars {
		e := ev.(map[string]interface{})
		if e["name"] != "HF_TOKEN" {
			continue
		}
		hfCount++
		// The surviving entry must be the secret-backed one, not the plaintext value.
		if _, hasValueFrom := e["valueFrom"]; !hasValueFrom {
			t.Errorf("expected secret-backed HF_TOKEN, got plaintext entry %v", e)
		}
		if _, hasValue := e["value"]; hasValue {
			t.Errorf("expected no inline value on HF_TOKEN, got %v", e["value"])
		}
	}
	if hfCount != 1 {
		t.Fatalf("expected exactly one HF_TOKEN env var, got %d in %v", hfCount, envVars)
	}
}

func TestTransformAggregatedEnvVars(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Env = []corev1.EnvVar{
		{Name: "FOO", Value: "bar"},
		{
			Name: "SECRET_VAL",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
					Key:                  "my-key",
				},
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	envVars, _ := container["env"].([]interface{})

	if len(envVars) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(envVars))
	}
	e0 := envVars[0].(map[string]interface{})
	if e0["name"] != "FOO" || e0["value"] != "bar" {
		t.Errorf("expected FOO=bar, got %v", e0)
	}
	e1 := envVars[1].(map[string]interface{})
	if e1["name"] != "SECRET_VAL" {
		t.Errorf("expected SECRET_VAL, got %v", e1["name"])
	}
}

func TestTransformAggregatedEnvVarPrefersValueFromOverValue(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Env = []corev1.EnvVar{
		{
			Name:  "SECRET_VAL",
			Value: "inline-fallback",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
					Key:                  "my-key",
				},
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	envVars, _ := container["env"].([]interface{})
	if len(envVars) != 1 {
		t.Fatalf("expected 1 env var, got %d", len(envVars))
	}

	env := envVars[0].(map[string]interface{})
	if _, ok := env["value"]; ok {
		t.Fatalf("env var should not set both value and valueFrom: %#v", env)
	}
	valueFrom := env["valueFrom"].(map[string]interface{})
	secretKeyRef := valueFrom["secretKeyRef"].(map[string]interface{})
	if secretKeyRef["name"] != "my-secret" || secretKeyRef["key"] != "my-key" {
		t.Fatalf("unexpected secretKeyRef: %#v", secretKeyRef)
	}
}

func TestTransformAggregatedNodeSelector(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.NodeSelector = map[string]string{
		"gpu-type": "a100",
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	ns, found, _ := unstructured.NestedStringMap(deploy.Object, "spec", "template", "spec", "nodeSelector")
	if !found || ns["gpu-type"] != "a100" {
		t.Errorf("expected nodeSelector gpu-type=a100, got %v", ns)
	}
}

func TestTransformAggregatedPodTemplateLabels(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.PodTemplate = &airunwayv1alpha1.PodTemplateSpec{
		Metadata: &airunwayv1alpha1.PodTemplateMetadata{
			Labels: map[string]string{
				"custom-label": "custom-value",
			},
			Annotations: map[string]string{
				"custom-annotation": "annotation-value",
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	// Check Deployment labels
	if deploy.GetLabels()["custom-label"] != "custom-value" {
		t.Error("expected custom-label on Deployment")
	}
	// Check Deployment annotations
	if deploy.GetAnnotations()["custom-annotation"] != "annotation-value" {
		t.Error("expected custom-annotation on Deployment")
	}
	// Check pod template labels
	podLabels, _, _ := unstructured.NestedStringMap(deploy.Object, "spec", "template", "metadata", "labels")
	if podLabels["custom-label"] != "custom-value" {
		t.Error("expected custom-label in pod template labels")
	}
}

func TestTransformAggregatedServicePort(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	svc := resources[1]
	ports, found, _ := unstructured.NestedSlice(svc.Object, "spec", "ports")
	if !found || len(ports) == 0 {
		t.Fatal("expected service ports")
	}
	port := ports[0].(map[string]interface{})
	if port["port"] != DefaultVLLMPort {
		t.Errorf("expected port %d, got %v", DefaultVLLMPort, port["port"])
	}
}

func TestTransformAggregatedExplicitHostPortArgs(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := getContainerArgs(t, resources[0])
	assertArg(t, args, "--host", DefaultVLLMHost)
	assertArg(t, args, "--port", "8000")
}

func TestTransformAggregatedDeploymentProbes(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	container := getContainer(t, resources[0])
	assertHTTPProbe(t, container, "startupProbe", 15, 10, 60)
	assertHTTPProbe(t, container, "livenessProbe", 15, 10, 3)
	assertHTTPProbe(t, container, "readinessProbe", 15, 5, 3)
}

func TestTransformAggregatedSharedMemoryForTensorParallelism(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
		GPU: &airunwayv1alpha1.GPUSpec{Count: 4},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertSharedMemoryPresent(t, resources[0])
}

func TestTransformAggregatedNoSharedMemoryForSingleGPU(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertSharedMemoryAbsent(t, resources[0])
}

func TestTransformAggregatedCustomEngineArgs(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.Args = map[string]string{
		"gpu-memory-utilization": "0.9",
		"disable-log-requests":   "",
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	args := argsToStrings(container["args"].([]interface{}))

	assertArg(t, args, "--gpu-memory-utilization", "0.9")
	assertFlag(t, args, "--disable-log-requests")
}

func TestTransformAggregatedEngineExtraArgsAppendedAfterSortedEngineArgs(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.Args = map[string]string{
		"zeta":  "last",
		"alpha": "first",
	}
	md.Spec.Engine.ExtraArgs = []string{"--raw-flag", "raw-value", "--second-raw"}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	args := getContainerArgs(t, resources[0])
	expectedSuffix := []string{"--alpha", "first", "--zeta", "last", "--raw-flag", "raw-value", "--second-raw"}
	if len(args) < len(expectedSuffix) {
		t.Fatalf("expected at least %d args, got %d: %v", len(expectedSuffix), len(args), args)
	}
	suffix := args[len(args)-len(expectedSuffix):]
	for i := range expectedSuffix {
		if suffix[i] != expectedSuffix[i] {
			t.Fatalf("expected suffix %v, got %v (all args: %v)", expectedSuffix, suffix, args)
		}
	}
}

func TestTransformAggregatedInvalidEngineArgKey(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.Args = map[string]string{
		"-bad-key": "value",
	}

	_, err := tr.Transform(context.Background(), md)
	if err == nil {
		t.Fatal("expected error for invalid engine arg key starting with hyphen")
	}
}

func TestTransformAggregatedRejectsReservedEngineArgPort(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.Args = map[string]string{
		"port": "9000",
	}

	_, err := tr.Transform(context.Background(), md)
	if err == nil {
		t.Fatal("expected error for reserved port engine arg")
	}
	if !strings.Contains(err.Error(), `engine arg "port" conflicts`) {
		t.Fatalf("expected reserved port error, got %v", err)
	}
}

func TestTransformAggregatedRejectsReservedEngineExtraArgHost(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.ExtraArgs = []string{"--host=127.0.0.1"}

	_, err := tr.Transform(context.Background(), md)
	if err == nil {
		t.Fatal("expected error for reserved host extraArg")
	}
	if !strings.Contains(err.Error(), `engine extraArg "--host=127.0.0.1" conflicts`) {
		t.Fatalf("expected reserved host extraArg error, got %v", err)
	}
}

func TestTransformUnsupportedEngine(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang

	_, err := tr.Transform(context.Background(), md)
	if err == nil {
		t.Fatal("expected error for unsupported engine")
	}
}

func TestTransformDisaggregatedBasic(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 4,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 4},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// decode Deployment, prefill Deployment, decode Service, prefill Service
	if len(resources) != 4 {
		t.Fatalf("expected 4 resources for disaggregated mode, got %d", len(resources))
	}

	decodeDeployment := resources[0]
	prefillDeployment := resources[1]
	decodeSvc := resources[2]
	prefillSvc := resources[3]

	if decodeDeployment.GetName() != "test-model-decode" {
		t.Errorf("expected decode deployment name 'test-model-decode', got %s", decodeDeployment.GetName())
	}
	if prefillDeployment.GetName() != "test-model-prefill" {
		t.Errorf("expected prefill deployment name 'test-model-prefill', got %s", prefillDeployment.GetName())
	}
	if decodeSvc.GetName() != "test-model-decode" {
		t.Errorf("expected decode service name 'test-model-decode', got %s", decodeSvc.GetName())
	}
	if prefillSvc.GetName() != "test-model-prefill" {
		t.Errorf("expected prefill service name 'test-model-prefill', got %s", prefillSvc.GetName())
	}
}

func TestTransformDisaggregatedKVTransferConfig(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 2,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 2},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	decodeDeployment := resources[0]
	prefillDeployment := resources[1]

	decodeArgs := getContainerArgs(t, decodeDeployment)
	prefillArgs := getContainerArgs(t, prefillDeployment)

	assertArg(t, decodeArgs, "--kv-transfer-config", KVTransferConfigDecode)
	assertArg(t, prefillArgs, "--kv-transfer-config", KVTransferConfigPrefill)
}

func TestTransformDisaggregatedReplicas(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 4,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 2,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 2},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	decodeDeployment := resources[0]
	prefillDeployment := resources[1]

	decodeReplicas, _, _ := unstructured.NestedInt64(decodeDeployment.Object, "spec", "replicas")
	prefillReplicas, _, _ := unstructured.NestedInt64(prefillDeployment.Object, "spec", "replicas")

	if decodeReplicas != 2 {
		t.Errorf("expected 2 decode replicas, got %d", decodeReplicas)
	}
	if prefillReplicas != 4 {
		t.Errorf("expected 4 prefill replicas, got %d", prefillReplicas)
	}
}

func TestTransformDisaggregatedMissingScaling(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	// No scaling spec

	_, err := tr.Transform(context.Background(), md)
	if err == nil {
		t.Fatal("expected error for disaggregated mode without scaling spec")
	}
}

func TestTransformAggregatedOverride(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Overrides: &runtime.RawExtension{
			Raw: []byte(`{"spec": {"replicas": 5}}`),
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	// JSON-parsed numbers are float64; use NestedFloat64 for override values
	replicas, found, _ := unstructured.NestedFloat64(deploy.Object, "spec", "replicas")
	if !found || replicas != 5 {
		t.Errorf("expected overridden replicas 5, got %v (found=%v)", replicas, found)
	}
}

func TestTransformOverrideBlocksMetadata(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Overrides: &runtime.RawExtension{
			Raw: []byte(`{"metadata": {"name": "evil-name"}}`),
		},
	}

	_, err := tr.Transform(context.Background(), md)
	if err == nil {
		t.Fatal("expected error when overriding metadata")
	}
}

func TestBuildResourceLimits(t *testing.T) {
	tr := NewTransformer()

	// nil spec
	if r := tr.buildResourceLimits(nil); r != nil {
		t.Errorf("expected nil for nil spec, got %v", r)
	}

	// GPU only
	r := tr.buildResourceLimits(&airunwayv1alpha1.ResourceSpec{
		GPU: &airunwayv1alpha1.GPUSpec{Count: 2},
	})
	limits := r["limits"].(map[string]interface{})
	if limits[GPUResourceKey] != "2" {
		t.Errorf("expected GPU limit 2, got %v", limits[GPUResourceKey])
	}

	// Memory only
	r = tr.buildResourceLimits(&airunwayv1alpha1.ResourceSpec{Memory: "16Gi"})
	limits = r["limits"].(map[string]interface{})
	if limits["memory"] != "16Gi" {
		t.Errorf("expected memory 16Gi, got %v", limits["memory"])
	}
}

// --- helpers ---

func argsToStrings(args []interface{}) []string {
	result := make([]string, len(args))
	for i, a := range args {
		result[i] = a.(string)
	}
	return result
}

func assertArg(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return
		}
	}
	t.Errorf("expected arg %s %s in %v", flag, value, args)
}

func assertNoArg(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, a := range args {
		if a == flag {
			t.Errorf("unexpected arg %s in %v", flag, args)
			return
		}
	}
}

// Test that disaggregated mode uses per-component GPU counts for tensor parallelism
func TestTransformDisaggregatedTensorParallelism(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Resources = nil // no top-level resources
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 4,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 4},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check decode deployment has --tensor-parallel-size 4
	decodeContainers, _, _ := unstructured.NestedSlice(resources[0].Object, "spec", "template", "spec", "containers")
	decodeArgs := argsToStrings(decodeContainers[0].(map[string]interface{})["args"].([]interface{}))
	assertArg(t, decodeArgs, "--tensor-parallel-size", "4")

	// Check prefill deployment has NO --tensor-parallel-size (single GPU)
	prefillContainers, _, _ := unstructured.NestedSlice(resources[1].Object, "spec", "template", "spec", "containers")
	prefillArgs := argsToStrings(prefillContainers[0].(map[string]interface{})["args"].([]interface{}))
	assertNoArg(t, prefillArgs, "--tensor-parallel-size")
}

func TestTransformDisaggregatedSharedMemoryPerComponent(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Resources = nil
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 4,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 4},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertSharedMemoryPresent(t, resources[0])
	assertSharedMemoryAbsent(t, resources[1])
}

// Test that user-provided labels cannot overwrite selector-critical keys
func TestTransformUserLabelsCannotClobberSelectors(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.PodTemplate = &airunwayv1alpha1.PodTemplateSpec{
		Metadata: &airunwayv1alpha1.PodTemplateMetadata{
			Labels: map[string]string{
				"app":                    "my-custom-app",
				"airunway.ai/deployment": "my-custom-deployment",
				"my-label":               "my-value",
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deployment := resources[0]
	selectorLabels, _, _ := unstructured.NestedMap(deployment.Object, "spec", "selector", "matchLabels")
	podLabels, _, _ := unstructured.NestedMap(deployment.Object, "spec", "template", "metadata", "labels")

	// Selector-critical keys must match between selector and pod template
	if selectorLabels["app"] != podLabels["app"] {
		t.Errorf("selector app=%v but pod app=%v — selector won't match pods", selectorLabels["app"], podLabels["app"])
	}
	if selectorLabels["airunway.ai/deployment"] != podLabels["airunway.ai/deployment"] {
		t.Errorf("selector deployment=%v but pod deployment=%v", selectorLabels["airunway.ai/deployment"], podLabels["airunway.ai/deployment"])
	}

	// Custom labels should still be present
	if podLabels["my-label"] != "my-value" {
		t.Errorf("custom label my-label not preserved, got %v", podLabels["my-label"])
	}
}

func assertFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, a := range args {
		if a == flag {
			return
		}
	}
	t.Errorf("expected flag %s in %v", flag, args)
}

func getContainer(t *testing.T, deploy *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		t.Fatal("expected at least one container")
	}
	return containers[0].(map[string]interface{})
}

func getContainerArgs(t *testing.T, deploy *unstructured.Unstructured) []string {
	t.Helper()
	container := getContainer(t, deploy)
	return argsToStrings(container["args"].([]interface{}))
}

func assertHTTPProbe(t *testing.T, container map[string]interface{}, name string, initialDelaySeconds, periodSeconds, failureThreshold int64) {
	t.Helper()
	probe, ok := container[name].(map[string]interface{})
	if !ok {
		t.Fatalf("expected %s", name)
	}
	if probe["initialDelaySeconds"] != initialDelaySeconds {
		t.Errorf("expected %s initialDelaySeconds %d, got %v", name, initialDelaySeconds, probe["initialDelaySeconds"])
	}
	if probe["periodSeconds"] != periodSeconds {
		t.Errorf("expected %s periodSeconds %d, got %v", name, periodSeconds, probe["periodSeconds"])
	}
	if probe["failureThreshold"] != failureThreshold {
		t.Errorf("expected %s failureThreshold %d, got %v", name, failureThreshold, probe["failureThreshold"])
	}
	httpGet, ok := probe["httpGet"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected %s httpGet", name)
	}
	if httpGet["path"] != DefaultVLLMHealthPath {
		t.Errorf("expected %s path %s, got %v", name, DefaultVLLMHealthPath, httpGet["path"])
	}
	if httpGet["port"] != DefaultVLLMPort {
		t.Errorf("expected %s port %d, got %v", name, DefaultVLLMPort, httpGet["port"])
	}
}

func assertSharedMemoryPresent(t *testing.T, deploy *unstructured.Unstructured) {
	t.Helper()
	container := getContainer(t, deploy)
	volumeMounts, ok := container["volumeMounts"].([]interface{})
	if !ok || len(volumeMounts) != 1 {
		t.Fatalf("expected one volumeMount, got %v", container["volumeMounts"])
	}
	volumeMount := volumeMounts[0].(map[string]interface{})
	if volumeMount["name"] != VLLMShmVolumeName {
		t.Errorf("expected volumeMount name %s, got %v", VLLMShmVolumeName, volumeMount["name"])
	}
	if volumeMount["mountPath"] != "/dev/shm" {
		t.Errorf("expected volumeMount mountPath /dev/shm, got %v", volumeMount["mountPath"])
	}

	volumes, found, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "volumes")
	if !found || len(volumes) != 1 {
		t.Fatalf("expected one volume, got %v (found=%v)", volumes, found)
	}
	volume := volumes[0].(map[string]interface{})
	if volume["name"] != VLLMShmVolumeName {
		t.Errorf("expected volume name %s, got %v", VLLMShmVolumeName, volume["name"])
	}
	emptyDir, ok := volume["emptyDir"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected emptyDir volume, got %v", volume["emptyDir"])
	}
	if emptyDir["medium"] != "Memory" {
		t.Errorf("expected emptyDir medium Memory, got %v", emptyDir["medium"])
	}
	if emptyDir["sizeLimit"] != DefaultVLLMShmSize {
		t.Errorf("expected emptyDir sizeLimit %s, got %v", DefaultVLLMShmSize, emptyDir["sizeLimit"])
	}
}

func assertSharedMemoryAbsent(t *testing.T, deploy *unstructured.Unstructured) {
	t.Helper()
	container := getContainer(t, deploy)
	if _, exists := container["volumeMounts"]; exists {
		t.Fatalf("expected no volumeMounts, got %v", container["volumeMounts"])
	}
	if volumes, found, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "volumes"); found {
		t.Fatalf("expected no volumes, got %v", volumes)
	}
}

// countArg returns how many times a flag appears in the rendered args.
func countArg(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

// argValue returns the value following the first occurrence of flag.
func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// An explicit spec.engine.args entry must override the flag we derive from the
// GPU count, so editing GPUs after applying a recipe can never emit two
// conflicting --tensor-parallel-size flags. Regression for the TP-duplication bug.
func TestBuildVLLMArgsTensorParallelDedup(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
		GPU: &airunwayv1alpha1.GPUSpec{Count: 2},
	}
	md.Spec.Engine.Args = map[string]string{
		"tensor-parallel-size": "4",
	}

	args, err := tr.buildVLLMArgs(md, "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := countArg(args, "--tensor-parallel-size"); got != 1 {
		t.Fatalf("expected exactly one --tensor-parallel-size, got %d in %v", got, args)
	}
	if value, _ := argValue(args, "--tensor-parallel-size"); value != "4" {
		t.Errorf("expected explicit engine.args value 4 to win, got %q", value)
	}
}

// A derived flag supplied via spec.engine.extraArgs (raw "--key=value" or
// "--key value" tokens) must also suppress the structured-field form, so the
// rendered command never carries a conflicting duplicate that crashes vLLM.
func TestBuildVLLMArgsTensorParallelDedupViaExtraArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extraArgs []string
	}{
		{name: "inline value form", extraArgs: []string{"--tensor-parallel-size=4"}},
		{name: "two-token form", extraArgs: []string{"--tensor-parallel-size", "4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTransformer()
			md := newTestMD("test-model", "default")
			md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
				GPU: &airunwayv1alpha1.GPUSpec{Count: 2},
			}
			md.Spec.Engine.ExtraArgs = tc.extraArgs

			args, err := tr.buildVLLMArgs(md, "", 0)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Count every occurrence of the flag in either the bare ("--tensor-parallel-size")
			// or inline ("--tensor-parallel-size=…") form. The derived form must be
			// suppressed so only the user's single setting remains.
			tpCount := 0
			for _, a := range args {
				if a == "--tensor-parallel-size" || strings.HasPrefix(a, "--tensor-parallel-size=") {
					tpCount++
				}
			}
			if tpCount != 1 {
				t.Fatalf("expected exactly one --tensor-parallel-size setting, got %d in %v", tpCount, args)
			}
		})
	}
}

// Setting the SAME launch flag in both spec.engine.args (the structured map) and
// spec.engine.extraArgs (raw tokens) is a contradiction: engine.args renders
// first and extraArgs is appended verbatim, so both would reach vLLM and its
// last-wins argparse would silently honor the extraArgs value — defeating the
// engine.args one. The transformer must reject this instead of emitting two
// conflicting copies. Regression for the --tensor-parallel-size 4-vs-2 bug.
func TestBuildVLLMArgsRejectsArgsExtraArgsConflict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		extraArg string
	}{
		{name: "inline value form", extraArg: "--tensor-parallel-size=2"},
		{name: "two-token form", extraArg: "--tensor-parallel-size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTransformer()
			md := newTestMD("test-model", "default")
			md.Spec.Engine.Args = map[string]string{
				"tensor-parallel-size": "4",
			}
			md.Spec.Engine.ExtraArgs = []string{tc.extraArg}

			_, err := tr.buildVLLMArgs(md, "", 0)
			if err == nil {
				t.Fatalf("expected an error for a flag set in both engine.args and extraArgs")
			}
			for _, want := range []string{"tensor-parallel-size", "spec.engine.args", "spec.engine.extraArgs"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should mention %q", err.Error(), want)
				}
			}
		})
	}
}

// A flag that is present ONLY in extraArgs (not also in engine.args) is a valid,
// legitimately-repeatable override and must NOT be rejected by the duplicate
// guard — only the cross-source contradiction is an error.
func TestBuildVLLMArgsAllowsExtraArgsOnlyFlag(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.ExtraArgs = []string{"--tensor-parallel-size=2"}

	if _, err := tr.buildVLLMArgs(md, "", 0); err != nil {
		t.Fatalf("unexpected error for an extraArgs-only flag: %v", err)
	}
}

// The same dedup must hold for the other derived flags so an explicit engine.args
// entry is never duplicated by the structured-field renderer.
func TestBuildVLLMArgsDerivedFlagDedup(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	ctxLen := int32(4096)
	md.Spec.Engine.ContextLength = &ctxLen
	md.Spec.Model.ServedName = "alias"
	md.Spec.Engine.Args = map[string]string{
		"model":             "override/model",
		"max-model-len":     "8192",
		"served-model-name": "override-alias",
	}

	args, err := tr.buildVLLMArgs(md, "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, flag := range []string{"--model", "--max-model-len", "--served-model-name"} {
		if got := countArg(args, flag); got != 1 {
			t.Errorf("expected exactly one %s, got %d in %v", flag, got, args)
		}
	}
	if value, _ := argValue(args, "--model"); value != "override/model" {
		t.Errorf("expected explicit model override to win, got %q", value)
	}
}

func TestBuildVLLMArgsEnforceEagerAndPrefixCaching(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.EnforceEager = true
	md.Spec.Engine.EnablePrefixCaching = true

	args, err := tr.buildVLLMArgs(md, "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertFlag(t, args, "--enforce-eager")
	assertFlag(t, args, "--enable-prefix-caching")
}

func TestBuildVLLMArgsOmitsEnforceEagerWhenDisabled(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	// EnforceEager / EnablePrefixCaching default to false

	args, err := tr.buildVLLMArgs(md, "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertNoArg(t, args, "--enforce-eager")
	assertNoArg(t, args, "--enable-prefix-caching")
}

// A user must not be able to slip a reserved host/port override past the guard
// using the flag form ("--port") or inline-value form ("--port=9000") as a map key.
func TestValidateReservedVLLMServerArgsRejectsDashAndEqualForms(t *testing.T) {
	cases := []map[string]string{
		{"--port": "9000"},
		{"--host": "127.0.0.1"},
		{"--port=9000": ""},
		{"port": "9000"},
	}
	for _, engineArgs := range cases {
		if err := validateReservedVLLMServerArgs(engineArgs, nil); err == nil {
			t.Errorf("expected reserved-arg rejection for %v", engineArgs)
		}
	}
}

func TestTransformMountsModelStorageVolumes(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
		Volumes: []airunwayv1alpha1.StorageVolume{
			{
				Name:      "model-cache",
				ClaimName: "shared-model-cache",
				MountPath: "/model-cache",
				ReadOnly:  true,
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deploy := resources[0]
	container := getContainer(t, deploy)
	mounts, ok := container["volumeMounts"].([]interface{})
	if !ok || len(mounts) != 1 {
		t.Fatalf("expected one volumeMount, got %v", container["volumeMounts"])
	}
	mount := mounts[0].(map[string]interface{})
	if mount["name"] != "model-cache" || mount["mountPath"] != "/model-cache" {
		t.Errorf("unexpected volumeMount %v", mount)
	}
	if mount["readOnly"] != true {
		t.Errorf("expected readOnly volumeMount, got %v", mount["readOnly"])
	}

	volumes, found, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "volumes")
	if !found || len(volumes) != 1 {
		t.Fatalf("expected one volume, got %v (found=%v)", volumes, found)
	}
	volume := volumes[0].(map[string]interface{})
	pvc, ok := volume["persistentVolumeClaim"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected persistentVolumeClaim volume, got %v", volume)
	}
	if pvc["claimName"] != "shared-model-cache" {
		t.Errorf("expected claimName shared-model-cache, got %v", pvc["claimName"])
	}
}
