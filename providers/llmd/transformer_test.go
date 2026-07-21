package llmd

import (
	"context"
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
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

func TestTransformAggregatedPrefixCachingFlag(t *testing.T) {
	tr := NewTransformer()

	// EnablePrefixCaching=true should emit --enable-prefix-caching (the CRD
	// default applied by the API server before reaching the provider).
	mdOn := newTestMD("test-model", "default")
	mdOn.Spec.Engine.EnablePrefixCaching = true
	resOn, err := tr.Transform(context.Background(), mdOn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	containersOn, _, _ := unstructured.NestedSlice(resOn[0].Object, "spec", "template", "spec", "containers")
	argsOn := argsToStrings(containersOn[0].(map[string]interface{})["args"].([]interface{}))
	assertFlag(t, argsOn, "--enable-prefix-caching")
	for _, a := range argsOn {
		if a == "--no-enable-prefix-caching" {
			t.Error("did not expect --no-enable-prefix-caching when EnablePrefixCaching=true")
		}
	}

	// Explicitly disabled should emit --no-enable-prefix-caching so the
	// provider's choice survives any vLLM-side default flips.
	mdOff := newTestMD("test-model", "default")
	mdOff.Spec.Engine.EnablePrefixCaching = false
	resOff, err := tr.Transform(context.Background(), mdOff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	containersOff, _, _ := unstructured.NestedSlice(resOff[0].Object, "spec", "template", "spec", "containers")
	argsOff := argsToStrings(containersOff[0].(map[string]interface{})["args"].([]interface{}))
	assertFlag(t, argsOff, "--no-enable-prefix-caching")
	for _, a := range argsOff {
		if a == "--enable-prefix-caching" {
			t.Error("did not expect --enable-prefix-caching when EnablePrefixCaching=false")
		}
	}
}

func TestTransformAggregatedEnforceEagerFlag(t *testing.T) {
	tr := NewTransformer()

	// EnforceEager=true should emit --enforce-eager.
	mdOn := newTestMD("test-model", "default")
	mdOn.Spec.Engine.EnforceEager = true
	resOn, err := tr.Transform(context.Background(), mdOn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	containersOn, _, _ := unstructured.NestedSlice(resOn[0].Object, "spec", "template", "spec", "containers")
	argsOn := argsToStrings(containersOn[0].(map[string]interface{})["args"].([]interface{}))
	assertFlag(t, argsOn, "--enforce-eager")

	// EnforceEager=false (the default) must NOT emit --enforce-eager: there
	// is no "off" form, so vLLM's CUDA-graph default is preserved by omission.
	mdOff := newTestMD("test-model", "default")
	mdOff.Spec.Engine.EnforceEager = false
	resOff, err := tr.Transform(context.Background(), mdOff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	containersOff, _, _ := unstructured.NestedSlice(resOff[0].Object, "spec", "template", "spec", "containers")
	argsOff := argsToStrings(containersOff[0].(map[string]interface{})["args"].([]interface{}))
	for _, a := range argsOff {
		if a == "--enforce-eager" {
			t.Errorf("did not expect --enforce-eager when EnforceEager=false, got: %v", argsOff)
		}
	}
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
	if container["image"] != DefaultVLLMImage {
		t.Errorf("expected default image %s, got %s", DefaultVLLMImage, container["image"])
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

func TestTransformOverrideBlocksPodSpec(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Overrides: &runtime.RawExtension{
			Raw: []byte(`{"spec": {"template": {"spec": {"hostNetwork": true}}}}`),
		},
	}

	_, err := tr.Transform(context.Background(), md)
	if err == nil {
		t.Fatal("expected error when overriding spec.template.spec")
	}
	if !strings.Contains(err.Error(), "spec.template.spec") {
		t.Fatalf("expected error to mention spec.template.spec, got %v", err)
	}
}

func TestHasNestedMapPath(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]interface{}
		path     []string
		expected bool
	}{
		{
			name: "path exists",
			input: map[string]interface{}{
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"spec": map[string]interface{}{},
					},
				},
			},
			path:     []string{"spec", "template", "spec"},
			expected: true,
		},
		{
			name: "path missing",
			input: map[string]interface{}{
				"spec": map[string]interface{}{},
			},
			path:     []string{"spec", "template", "spec"},
			expected: false,
		},
		{
			name: "path blocked by non-map",
			input: map[string]interface{}{
				"spec": "not-a-map",
			},
			path:     []string{"spec", "template", "spec"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasNestedMapPath(tt.input, tt.path...)
			if result != tt.expected {
				t.Fatalf("expected %v, got %v", tt.expected, result)
			}
		})
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

func TestSanitizeLabelValue(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"with spaces", "with-spaces"},
		{"with/slashes", "with-slashes"},
		{"-leading-dash", "leading-dash"},
		{"trailing-dash-", "trailing-dash"},
		{
			"this-is-a-very-long-label-value-that-exceeds-the-sixty-three-character-limit",
			"this-is-a-very-long-label-value-that-exceeds-the-sixty-three-ch",
		},
	}
	for _, tt := range tests {
		result := sanitizeLabelValue(tt.input)
		if result != tt.expected {
			t.Errorf("sanitizeLabelValue(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
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

func getContainerArgs(t *testing.T, deploy *unstructured.Unstructured) []string {
	t.Helper()
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		t.Fatal("expected at least one container")
	}
	container := containers[0].(map[string]interface{})
	return argsToStrings(container["args"].([]interface{}))
}
