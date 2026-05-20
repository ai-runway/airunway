package dynamo

import (
	"context"
	"encoding/json"
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

	dgd := resources[0]
	if dgd.GetKind() != DynamoGraphDeploymentKind {
		t.Errorf("expected kind %s, got %s", DynamoGraphDeploymentKind, dgd.GetKind())
	}
	expectedName := "test-model"
	if dgd.GetName() != expectedName {
		t.Errorf("expected name %q, got %s", expectedName, dgd.GetName())
	}
	if dgd.GetAPIVersion() != "nvidia.com/v1alpha1" {
		t.Errorf("expected apiVersion 'nvidia.com/v1alpha1', got %s", dgd.GetAPIVersion())
	}

	// Check namespace — DGD should be in the same namespace as the ModelDeployment
	if dgd.GetNamespace() != "default" {
		t.Errorf("expected namespace %q, got %q", "default", dgd.GetNamespace())
	}

	// Check labels
	labels := dgd.GetLabels()
	if labels[airunwayv1alpha1.LabelManagedBy] != "airunway" {
		t.Errorf("expected managed-by label 'airunway'")
	}
	if labels["airunway.ai/engine-type"] != "vllm" {
		t.Errorf("expected engine-type label 'vllm'")
	}

	// Check OwnerReference
	ownerRefs := dgd.GetOwnerReferences()
	if len(ownerRefs) != 1 {
		t.Fatalf("expected 1 OwnerReference, got %d", len(ownerRefs))
	}
	if ownerRefs[0].UID != md.UID {
		t.Errorf("expected OwnerReference UID %q, got %q", md.UID, ownerRefs[0].UID)
	}
	if ownerRefs[0].Kind != "ModelDeployment" {
		t.Errorf("expected OwnerReference Kind 'ModelDeployment', got %q", ownerRefs[0].Kind)
	}
	if ownerRefs[0].Name != md.Name {
		t.Errorf("expected OwnerReference Name %q, got %q", md.Name, ownerRefs[0].Name)
	}
	if ownerRefs[0].APIVersion != airunwayv1alpha1.GroupVersion.String() {
		t.Errorf("expected OwnerReference APIVersion %q, got %q", airunwayv1alpha1.GroupVersion.String(), ownerRefs[0].APIVersion)
	}
	if ownerRefs[0].Controller == nil || !*ownerRefs[0].Controller {
		t.Errorf("expected OwnerReference Controller to be true")
	}
	if ownerRefs[0].BlockOwnerDeletion == nil || !*ownerRefs[0].BlockOwnerDeletion {
		t.Errorf("expected OwnerReference BlockOwnerDeletion to be true")
	}

	// Check spec
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	if spec["backendFramework"] != "vllm" {
		t.Errorf("expected backendFramework 'vllm', got %v", spec["backendFramework"])
	}

	services, _ := spec["services"].(map[string]interface{})
	if _, ok := services["Frontend"]; ok {
		t.Error("did not expect Frontend service when gateway is enabled (default)")
	}
	if _, ok := services["Epp"]; !ok {
		t.Error("expected Epp service when gateway is enabled (default)")
	}
	if _, ok := services["VllmWorker"]; !ok {
		t.Error("expected VllmWorker service in aggregated mode")
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
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
			Memory:   "32Gi",
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})

	// Disaggregated mode should have prefill and decode workers, not VllmWorker
	if _, ok := services["VllmWorker"]; ok {
		t.Error("did not expect VllmWorker in disaggregated mode")
	}
	if _, ok := services["VllmPrefillWorker"]; !ok {
		t.Error("expected VllmPrefillWorker in disaggregated mode")
	}
	if _, ok := services["VllmDecodeWorker"]; !ok {
		t.Error("expected VllmDecodeWorker in disaggregated mode")
	}

	// Check prefill worker
	prefill, _ := services["VllmPrefillWorker"].(map[string]interface{})
	if prefill["replicas"] != int64(2) {
		t.Errorf("expected prefill replicas 2, got %v", prefill["replicas"])
	}
	if prefill["subComponentType"] != SubComponentTypePrefill {
		t.Errorf("expected subComponentType '%s', got %v", SubComponentTypePrefill, prefill["subComponentType"])
	}

	// Check decode worker
	decode, _ := services["VllmDecodeWorker"].(map[string]interface{})
	if decode["replicas"] != int64(3) {
		t.Errorf("expected decode replicas 3, got %v", decode["replicas"])
	}
}

func TestMapEngineType(t *testing.T) {
	tr := NewTransformer()

	tests := []struct {
		input    airunwayv1alpha1.EngineType
		expected string
	}{
		{airunwayv1alpha1.EngineTypeVLLM, "vllm"},
		{airunwayv1alpha1.EngineTypeSGLang, "sglang"},
		{airunwayv1alpha1.EngineTypeTRTLLM, "trtllm"},
		{airunwayv1alpha1.EngineType("unknown"), "unknown"},
	}

	for _, tt := range tests {
		result := tr.mapEngineType(tt.input)
		if result != tt.expected {
			t.Errorf("mapEngineType(%s) = %s, expected %s", tt.input, result, tt.expected)
		}
	}
}

func TestGetImage(t *testing.T) {
	tr := NewTransformer()

	// Custom image
	md := newTestMD("test", "default")
	md.Spec.Image = "custom-image:v1"
	if img := tr.getImage(md); img != "custom-image:v1" {
		t.Errorf("expected custom image, got %s", img)
	}

	// Default vLLM image
	md.Spec.Image = ""
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
	if img := tr.getImage(md); img != defaultVLLMRuntimeImage {
		t.Errorf("expected default vllm image, got %s", img)
	}

	// Default SGLang image
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang
	if img := tr.getImage(md); img != defaultSGLangRuntimeImage {
		t.Errorf("expected default sglang image, got %s", img)
	}

	// Default TRT-LLM image
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeTRTLLM
	if img := tr.getImage(md); img != defaultTRTLLMRuntimeImage {
		t.Errorf("expected default trtllm image, got %s", img)
	}

	// Unknown engine → fallback
	md.Spec.Engine.Type = airunwayv1alpha1.EngineType("unknown")
	if img := tr.getImage(md); img != defaultVLLMRuntimeImage {
		t.Errorf("expected fallback to vllm image, got %s", img)
	}
}

func TestBuildEngineArgs(t *testing.T) {
	tr := NewTransformer()

	// Basic vLLM - args no longer include engine command
	md := newTestMD("test", "default")
	args, err := tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"--model", "meta-llama/Llama-2-7b-chat-hf"}
	if !sliceEqual(args, expected) {
		t.Errorf("unexpected args: %v, expected %v", args, expected)
	}

	// SGLang with context length
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang
	ctxLen := int32(4096)
	md.Spec.Engine.ContextLength = &ctxLen
	args, err = tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected = []string{"--model-path", "meta-llama/Llama-2-7b-chat-hf", "--context-length", "4096"}
	if !sliceEqual(args, expected) {
		t.Errorf("unexpected args: %v, expected %v", args, expected)
	}

	// vLLM with context length
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
	args, err = tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected = []string{"--model", "meta-llama/Llama-2-7b-chat-hf", "--max-model-len", "4096"}
	if !sliceEqual(args, expected) {
		t.Errorf("unexpected args: %v, expected %v", args, expected)
	}

	// TRT-LLM
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeTRTLLM
	md.Spec.Engine.ContextLength = nil
	args, err = tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected = []string{"--model-path", "meta-llama/Llama-2-7b-chat-hf"}
	if !sliceEqual(args, expected) {
		t.Errorf("unexpected args: %v, expected %v", args, expected)
	}

	// With served name and trust remote code
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeVLLM
	md.Spec.Model.ServedName = "my-model"
	md.Spec.Engine.TrustRemoteCode = true
	args, err = tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedParts := []string{"--served-model-name", "my-model", "--trust-remote-code"}
	for _, part := range expectedParts {
		if !sliceContainsStr(args, part) {
			t.Errorf("expected args to contain '%s', got: %v", part, args)
		}
	}

	// With enable prefix caching
	md.Spec.Engine.TrustRemoteCode = false
	md.Spec.Model.ServedName = ""
	md.Spec.Engine.EnablePrefixCaching = true
	args, err = tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sliceContainsStr(args, "--enable-prefix-caching") {
		t.Errorf("expected args to contain '--enable-prefix-caching', got: %v", args)
	}

	// With enforce eager
	md.Spec.Engine.EnablePrefixCaching = false
	md.Spec.Engine.EnforceEager = true
	args, err = tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sliceContainsStr(args, "--enforce-eager") {
		t.Errorf("expected args to contain '--enforce-eager', got: %v", args)
	}

	// Prefix caching and enforce eager not added for TRT-LLM
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeTRTLLM
	md.Spec.Engine.EnablePrefixCaching = true
	md.Spec.Engine.EnforceEager = true
	args, err = tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sliceContainsStr(args, "--enable-prefix-caching") {
		t.Errorf("--enable-prefix-caching should not be added for trtllm, got: %v", args)
	}
	if sliceContainsStr(args, "--enforce-eager") {
		t.Errorf("--enforce-eager should not be added for trtllm, got: %v", args)
	}
}

func TestEngineCommand(t *testing.T) {
	tr := NewTransformer()

	tests := []struct {
		input    airunwayv1alpha1.EngineType
		expected []string
	}{
		{airunwayv1alpha1.EngineTypeVLLM, []string{"python3", "-m", "dynamo.vllm"}},
		{airunwayv1alpha1.EngineTypeSGLang, []string{"python3", "-m", "dynamo.sglang"}},
		{airunwayv1alpha1.EngineTypeTRTLLM, []string{"python3", "-m", "dynamo.trtllm"}},
	}

	for _, tt := range tests {
		result := tr.engineCommand(tt.input)
		if !sliceEqual(result, tt.expected) {
			t.Errorf("engineCommand(%s) = %v, expected %v", tt.input, result, tt.expected)
		}
	}
}

func TestIsValidArgKey(t *testing.T) {
	valid := []string{"tensor-parallel-size", "enable_feature", "maxBatchSize", "abc123"}
	for _, k := range valid {
		if !isValidArgKey(k) {
			t.Errorf("expected %q to be valid", k)
		}
	}
	invalid := []string{"", "key;drop", "a b", "foo$bar", "x&y", "a|b", "a`b"}
	for _, k := range invalid {
		if isValidArgKey(k) {
			t.Errorf("expected %q to be invalid", k)
		}
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func sliceContainsStr(ss []string, item string) bool {
	for _, s := range ss {
		if s == item {
			return true
		}
	}
	return false
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containerEnvValue(container map[string]interface{}, name string) (string, bool) {
	envList, ok := container["env"].([]interface{})
	if !ok {
		return "", false
	}

	for _, entry := range envList {
		envMap, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if envMap["name"] == name {
			value, ok := envMap["value"].(string)
			return value, ok
		}
	}

	return "", false
}

func TestBuildResourceLimits(t *testing.T) {
	tr := NewTransformer()

	// Nil spec
	result := tr.buildResourceLimits(nil)
	limits, _ := result["limits"].(map[string]interface{})
	if len(limits) != 0 {
		t.Errorf("expected empty limits for nil spec")
	}

	// With GPU
	result = tr.buildResourceLimits(&airunwayv1alpha1.ResourceSpec{
		GPU: &airunwayv1alpha1.GPUSpec{Count: 4, Type: "nvidia.com/gpu"},
	})
	limits, _ = result["limits"].(map[string]interface{})
	if limits["gpu"] != "4" {
		t.Errorf("expected gpu limit 4, got %v", limits["gpu"])
	}
	requests, _ := result["requests"].(map[string]interface{})
	if requests["gpu"] != "4" {
		t.Errorf("expected gpu request 4, got %v", requests["gpu"])
	}

	// With custom GPU type (Dynamo always uses 'gpu' key)
	result = tr.buildResourceLimits(&airunwayv1alpha1.ResourceSpec{
		GPU: &airunwayv1alpha1.GPUSpec{Count: 2, Type: "amd.com/gpu"},
	})
	limits, _ = result["limits"].(map[string]interface{})
	if limits["gpu"] != "2" {
		t.Errorf("expected gpu limit 2, got %v", limits["gpu"])
	}

	// With memory and CPU
	result = tr.buildResourceLimits(&airunwayv1alpha1.ResourceSpec{
		Memory: "32Gi",
		CPU:    "8",
	})
	limits, _ = result["limits"].(map[string]interface{})
	if limits["memory"] != "32Gi" {
		t.Errorf("expected memory 32Gi, got %v", limits["memory"])
	}
	if limits["cpu"] != "8" {
		t.Errorf("expected cpu 8, got %v", limits["cpu"])
	}
}

func TestParseOverrides(t *testing.T) {
	tr := NewTransformer()

	// No overrides
	md := newTestMD("test", "default")
	overrides, err := tr.parseOverrides(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides.RouterMode != "" {
		t.Errorf("expected empty router mode, got %s", overrides.RouterMode)
	}

	// With overrides
	overrideData := DynamoOverrides{
		RouterMode: "kv",
		Frontend: &FrontendOverrides{
			Replicas: int32Ptr(3),
			Resources: &ResourceOverrides{
				CPU:    "4",
				Memory: "8Gi",
			},
		},
	}
	raw, _ := json.Marshal(overrideData)
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name:      "dynamo",
		Overrides: &runtime.RawExtension{Raw: raw},
	}

	overrides, err = tr.parseOverrides(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides.RouterMode != "kv" {
		t.Errorf("expected router mode 'kv', got %s", overrides.RouterMode)
	}
	if *overrides.Frontend.Replicas != 3 {
		t.Errorf("expected frontend replicas 3, got %d", *overrides.Frontend.Replicas)
	}

	// Invalid overrides
	md.Spec.Provider.Overrides = &runtime.RawExtension{Raw: []byte("invalid json")}
	_, err = tr.parseOverrides(md)
	if err == nil {
		t.Fatal("expected error for invalid overrides")
	}
}

func int32Ptr(i int32) *int32 {
	return &i
}

func TestBuildAggregatedWorker(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{Replicas: 2}

	worker, err := tr.buildAggregatedWorker(md, "test-image:v1", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if worker["replicas"] != int64(2) {
		t.Errorf("expected replicas 2, got %v", worker["replicas"])
	}
	if worker["componentType"] != ComponentTypeWorker {
		t.Errorf("expected componentType '%s', got %v", ComponentTypeWorker, worker["componentType"])
	}

	extraPodSpec, _ := worker["extraPodSpec"].(map[string]interface{})
	mainContainer, _ := extraPodSpec["mainContainer"].(map[string]interface{})
	if mainContainer["image"] != "test-image:v1" {
		t.Errorf("expected image 'test-image:v1', got %v", mainContainer["image"])
	}
	// Verify no shell execution
	cmd, _ := mainContainer["command"].([]interface{})
	if len(cmd) < 1 || cmd[0] != "python3" {
		t.Errorf("expected command to start with python3, got %v", cmd)
	}
}

func TestBuildAggregatedWorkerWithSecret(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Secrets = &airunwayv1alpha1.SecretsSpec{HuggingFaceToken: "hf-secret"}

	worker, err := tr.buildAggregatedWorker(md, "img", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if worker["envFromSecret"] != "hf-secret" {
		t.Errorf("expected envFromSecret, got %v", worker["envFromSecret"])
	}
}

func TestAddSchedulingConfig(t *testing.T) {
	tr := NewTransformer()

	// With node selector
	md := newTestMD("test", "default")
	md.Spec.NodeSelector = map[string]string{"gpu": "a100"}
	service := map[string]interface{}{
		"extraPodSpec": map[string]interface{}{},
	}
	tr.addSchedulingConfig(service, md)
	eps, _ := service["extraPodSpec"].(map[string]interface{})
	ns, _ := eps["nodeSelector"].(map[string]interface{})
	if ns["gpu"] != "a100" {
		t.Errorf("expected nodeSelector gpu=a100")
	}

	// Verify nodeSelector is a copy (safe for unstructured deep copy)
	md.Spec.NodeSelector["gpu"] = "changed"
	if ns["gpu"] != "a100" {
		t.Errorf("nodeSelector should be a copy, not a reference to the original map")
	}

	// With tolerations
	md.Spec.Tolerations = []corev1.Toleration{
		{
			Key:      "nvidia.com/gpu",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		},
	}
	service = map[string]interface{}{}
	tr.addSchedulingConfig(service, md)
	eps, _ = service["extraPodSpec"].(map[string]interface{})
	tolerations, _ := eps["tolerations"].([]interface{})
	if len(tolerations) != 1 {
		t.Fatalf("expected 1 toleration, got %d", len(tolerations))
	}

	// With toleration value and tolerationSeconds
	secs := int64(300)
	md.Spec.Tolerations = []corev1.Toleration{
		{
			Key:               "node.kubernetes.io/not-ready",
			Operator:          corev1.TolerationOpEqual,
			Value:             "true",
			Effect:            corev1.TaintEffectNoExecute,
			TolerationSeconds: &secs,
		},
	}
	service = map[string]interface{}{}
	tr.addSchedulingConfig(service, md)
	eps, _ = service["extraPodSpec"].(map[string]interface{})
	tolerations, _ = eps["tolerations"].([]interface{})
	tol, _ := tolerations[0].(map[string]interface{})
	if tol["value"] != "true" {
		t.Errorf("expected toleration value 'true', got %v", tol["value"])
	}
	if tol["tolerationSeconds"] != int64(300) {
		t.Errorf("expected tolerationSeconds 300, got %v", tol["tolerationSeconds"])
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

func TestBoolPtr(t *testing.T) {
	p := boolPtr(true)
	if *p != true {
		t.Error("expected true")
	}
}

func TestBuildPrefillWorkerWithSecret(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Secrets = &airunwayv1alpha1.SecretsSpec{HuggingFaceToken: "hf-secret"}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
		},
	}

	worker, err := tr.buildPrefillWorker(md, "img", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if worker["envFromSecret"] != "hf-secret" {
		t.Errorf("expected envFromSecret, got %v", worker["envFromSecret"])
	}
	// Check explicit disaggregation mode in args
	eps, _ := worker["extraPodSpec"].(map[string]interface{})
	mc, _ := eps["mainContainer"].(map[string]interface{})
	args, _ := mc["args"].([]interface{})
	foundMode := false
	foundKVTransfer := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--disaggregation-mode" && args[i+1] == SubComponentTypePrefill {
			foundMode = true
		}
		if args[i] == "--kv-transfer-config" && args[i+1] == VLLMKVTransferConfig {
			foundKVTransfer = true
		}
	}
	if !foundMode {
		t.Errorf("expected --disaggregation-mode %s in args: %v", SubComponentTypePrefill, args)
	}
	if !foundKVTransfer {
		t.Errorf("expected --kv-transfer-config %s in args: %v", VLLMKVTransferConfig, args)
	}
}

func TestBuildDecodeWorkerWithSecret(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Secrets = &airunwayv1alpha1.SecretsSpec{HuggingFaceToken: "hf-secret"}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 2,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1, Type: "custom.gpu"},
			Memory:   "64Gi",
		},
	}

	worker, err := tr.buildDecodeWorker(md, "img", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if worker["envFromSecret"] != "hf-secret" {
		t.Errorf("expected envFromSecret")
	}
	if worker["replicas"] != int64(2) {
		t.Errorf("expected replicas 2")
	}
	eps, _ := worker["extraPodSpec"].(map[string]interface{})
	mc, _ := eps["mainContainer"].(map[string]interface{})
	args, _ := mc["args"].([]interface{})
	foundMode := false
	foundKVTransfer := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--disaggregation-mode" && args[i+1] == SubComponentTypeDecode {
			foundMode = true
		}
		if args[i] == "--kv-transfer-config" && args[i+1] == VLLMKVTransferConfig {
			foundKVTransfer = true
		}
	}
	if !foundMode {
		t.Errorf("expected --disaggregation-mode %s in args: %v", SubComponentTypeDecode, args)
	}
	if !foundKVTransfer {
		t.Errorf("expected --kv-transfer-config %s in args: %v", VLLMKVTransferConfig, args)
	}
}

func TestBuildEngineArgsWithCustomArgs(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Engine.Args = map[string]string{
		"tensor-parallel-size":  "4",
		"enable-prefix-caching": "",
	}

	args, err := tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sliceContainsStr(args, "--tensor-parallel-size") {
		t.Errorf("expected --tensor-parallel-size in args: %v", args)
	}
}

func TestBuildEngineArgsAggregatedVLLMOmitsConnector(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")

	args, err := tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertNoArg(t, args, "--connector")
}

func TestBuildEngineArgsStripsConnectorFromVLLMArgs(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Engine.Args = map[string]string{
		"connector": "nixl",
	}

	args, err := tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertNoArg(t, args, "--connector")
}

func TestBuildEngineArgsDisaggregatedLeavesConnectorToRuntimeDefault(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}

	args, err := tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertNoArg(t, args, "--connector")
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
	first, err := tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 20; i++ {
		result, err := tr.buildEngineArgs(md)
		if err != nil {
			t.Fatalf("unexpected error on iteration %d: %v", i, err)
		}
		if !sliceEqual(result, first) {
			t.Fatalf("non-deterministic output on iteration %d:\n  first: %v\n  got:   %v", i, first, result)
		}
	}

	// Verify alphabetical key order of custom args
	joined := strings.Join(first, " ")
	alphaIdx := strings.Index(joined, "--alpha-param")
	betaIdx := strings.Index(joined, "--beta-param")
	dataIdx := strings.Index(joined, "--data-path")
	enableIdx := strings.Index(joined, "--enable-some-feature")
	middleIdx := strings.Index(joined, "--middle-param")
	zebraIdx := strings.Index(joined, "--zebra-param")

	if alphaIdx > betaIdx || betaIdx > dataIdx || dataIdx > enableIdx || enableIdx > middleIdx || middleIdx > zebraIdx {
		t.Errorf("custom args not in alphabetical order: %v", first)
	}
}

func TestBuildEngineArgsTrustRemoteCodeSGLang(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang
	md.Spec.Engine.TrustRemoteCode = true

	args, err := tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sliceContainsStr(args, "--trust-remote-code") {
		t.Errorf("expected --trust-remote-code for sglang: %v", args)
	}
}

func TestBuildEngineArgsTRTLLMContextLength(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeTRTLLM
	ctxLen := int32(8192)
	md.Spec.Engine.ContextLength = &ctxLen

	args, err := tr.buildEngineArgs(md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// TRT-LLM doesn't use context length at runtime
	if sliceContainsStr(args, "8192") {
		t.Errorf("TRT-LLM should not include context length: %v", args)
	}
}

func TestBuildPrefillWorkerWithCustomGPUType(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test", "default")
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 2, Type: "amd.com/gpu"},
			Memory:   "32Gi",
		},
	}

	worker, err := tr.buildPrefillWorker(md, "img", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resources, _ := worker["resources"].(map[string]interface{})
	limits, _ := resources["limits"].(map[string]interface{})
	if limits["gpu"] != "2" {
		t.Errorf("expected gpu=2, got %v", limits["gpu"])
	}
	if limits["memory"] != "32Gi" {
		t.Errorf("expected memory=32Gi, got %v", limits["memory"])
	}
}

func TestApplyOverridesEscapeHatch(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	// Set overrides with both typed fields and arbitrary escape hatch fields
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name: "dynamo",
		Overrides: &runtime.RawExtension{
			Raw: []byte(`{
				"routerMode": "kv",
				"spec": {
					"customField": "customValue"
				}
			}`),
		},
	}

	results, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dgd := results[0]

	// Verify the escape hatch field was merged into the output
	customField, found, _ := unstructured.NestedString(dgd.Object, "spec", "customField")
	if !found || customField != "customValue" {
		t.Errorf("expected customField 'customValue', got %q (found=%v)", customField, found)
	}

	// Verify existing spec fields are preserved (backendFramework should still be set)
	framework, found, _ := unstructured.NestedString(dgd.Object, "spec", "backendFramework")
	if !found || framework == "" {
		t.Error("expected backendFramework to be preserved after override merge")
	}
}

func TestTransformAggregatedNoGPU(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
		Memory: "16Gi",
		CPU:    "4",
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	res, _ := worker["resources"].(map[string]interface{})
	limits, _ := res["limits"].(map[string]interface{})
	requests, _ := res["requests"].(map[string]interface{})

	if _, ok := limits["gpu"]; ok {
		t.Error("expected no gpu in limits when GPU not specified")
	}
	if _, ok := requests["gpu"]; ok {
		t.Error("expected no gpu in requests when GPU not specified")
	}
	if limits["memory"] != "16Gi" {
		t.Errorf("expected memory=16Gi, got %v", limits["memory"])
	}
	if limits["cpu"] != "4" {
		t.Errorf("expected cpu=4, got %v", limits["cpu"])
	}
}

func TestTransformAggregatedNilResources(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Resources = nil

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	res, _ := worker["resources"].(map[string]interface{})
	limits, _ := res["limits"].(map[string]interface{})
	if len(limits) != 0 {
		t.Errorf("expected empty limits for nil resources, got %v", limits)
	}
}

func TestTransformAggregatedGPUCount0(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Resources = &airunwayv1alpha1.ResourceSpec{
		GPU: &airunwayv1alpha1.GPUSpec{Count: 0},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	res, _ := worker["resources"].(map[string]interface{})
	limits, _ := res["limits"].(map[string]interface{})
	if _, ok := limits["gpu"]; ok {
		t.Error("expected no gpu in limits when count is 0")
	}
}

func TestTransformSGLangEngine(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	if spec["backendFramework"] != "sglang" {
		t.Errorf("expected backendFramework 'sglang', got %v", spec["backendFramework"])
	}

	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	eps, _ := worker["extraPodSpec"].(map[string]interface{})
	mc, _ := eps["mainContainer"].(map[string]interface{})
	cmdSlice, _ := mc["command"].([]interface{})
	if len(cmdSlice) < 3 {
		t.Fatal("expected engine command with at least 3 elements")
	}
	if cmdSlice[2] != "dynamo.sglang" {
		t.Errorf("expected sglang runner in command, got %v", cmdSlice)
	}
}

func TestTransformTRTLLMEngine(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeTRTLLM

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	if spec["backendFramework"] != "trtllm" {
		t.Errorf("expected backendFramework 'trtllm', got %v", spec["backendFramework"])
	}

	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	eps, _ := worker["extraPodSpec"].(map[string]interface{})
	mc, _ := eps["mainContainer"].(map[string]interface{})
	cmdSlice, _ := mc["command"].([]interface{})
	if len(cmdSlice) < 3 {
		t.Fatal("expected engine command with at least 3 elements")
	}
	if cmdSlice[2] != "dynamo.trtllm" {
		t.Errorf("expected trtllm runner in command, got %v", cmdSlice)
	}

	argsSlice, _ := mc["args"].([]interface{})
	if len(argsSlice) < 2 {
		t.Fatal("expected engine args with at least 2 elements")
	}
	if argsSlice[0] != "--model-path" || argsSlice[1] != "meta-llama/Llama-2-7b-chat-hf" {
		t.Errorf("expected TRT-LLM args to start with --model-path and model ID, got %v", argsSlice)
	}
}

func TestTransformWithCustomScalingReplicas(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Replicas: 5,
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	if worker["replicas"] != int64(5) {
		t.Errorf("expected replicas 5, got %v", worker["replicas"])
	}
}

func TestTransformDisaggregatedGPURequests(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 4},
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

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})

	// Check prefill has both limits and requests
	prefill, _ := services["VllmPrefillWorker"].(map[string]interface{})
	prefillRes, _ := prefill["resources"].(map[string]interface{})
	prefillLimits, _ := prefillRes["limits"].(map[string]interface{})
	prefillRequests, _ := prefillRes["requests"].(map[string]interface{})
	if prefillLimits["gpu"] != "4" {
		t.Errorf("expected prefill gpu limit 4, got %v", prefillLimits["gpu"])
	}
	if prefillRequests["gpu"] != "4" {
		t.Errorf("expected prefill gpu request 4, got %v", prefillRequests["gpu"])
	}

	// Check decode has both limits and requests
	decode, _ := services["VllmDecodeWorker"].(map[string]interface{})
	decodeRes, _ := decode["resources"].(map[string]interface{})
	decodeLimits, _ := decodeRes["limits"].(map[string]interface{})
	decodeRequests, _ := decodeRes["requests"].(map[string]interface{})
	if decodeLimits["gpu"] != "2" {
		t.Errorf("expected decode gpu limit 2, got %v", decodeLimits["gpu"])
	}
	if decodeRequests["gpu"] != "2" {
		t.Errorf("expected decode gpu request 2, got %v", decodeRequests["gpu"])
	}
}

func TestTransformOverrideCanOverwriteServices(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Provider = &airunwayv1alpha1.ProviderSpec{
		Name: "dynamo",
		Overrides: &runtime.RawExtension{
			Raw: []byte(`{
				"spec": {
					"services": {
						"VllmWorker": {
							"replicas": 3
						}
					}
				}
			}`),
		},
	}

	results, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := results[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})

	// Deep merge should have replaced replicas but kept other fields
	if worker["replicas"] != float64(3) {
		t.Errorf("expected overridden replicas 3, got %v (type %T)", worker["replicas"], worker["replicas"])
	}
	// componentType should be preserved from the transformer
	if worker["componentType"] != ComponentTypeWorker {
		t.Errorf("expected componentType preserved, got %v", worker["componentType"])
	}
}

func TestTransformWithCustomImage(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Image = "my-registry.io/custom-vllm:v1"

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	eps, _ := worker["extraPodSpec"].(map[string]interface{})
	mc, _ := eps["mainContainer"].(map[string]interface{})
	if mc["image"] != "my-registry.io/custom-vllm:v1" {
		t.Errorf("expected custom image, got %v", mc["image"])
	}
}

func TestTransformAggregatedVLLMWorkersDoNotInjectNixlSideChannelHostByDefault(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})

	if env := findEnvVar(worker, "VLLM_NIXL_SIDE_CHANNEL_HOST"); env != nil {
		t.Fatalf("did not expect VLLM_NIXL_SIDE_CHANNEL_HOST for aggregated vLLM worker, got %v", env)
	}
}

func TestTransformDisaggregatedVLLMWorkersInjectNixlSideChannelHost(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1, Type: "nvidia.com/gpu"},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})

	for name, svcName := range map[string]string{
		"prefill": "VllmPrefillWorker",
		"decode":  "VllmDecodeWorker",
	} {
		worker, _ := services[svcName].(map[string]interface{})
		if worker == nil {
			t.Fatalf("expected %s service", svcName)
		}
		assertFieldRefEnvVar(t, worker, "VLLM_NIXL_SIDE_CHANNEL_HOST", "status.podIP")
		if testing.Verbose() {
			t.Logf("verified %s worker env injection", name)
		}
	}
}

func TestTransformNonVLLMWorkersDoNotInjectNixlSideChannelHost(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})

	if env := findEnvVar(worker, "VLLM_NIXL_SIDE_CHANNEL_HOST"); env != nil {
		t.Fatalf("did not expect VLLM_NIXL_SIDE_CHANNEL_HOST for non-vLLM worker, got %v", env)
	}
}

func TestBuildResourceLimitsWithAllFields(t *testing.T) {
	tr := NewTransformer()
	result := tr.buildResourceLimits(&airunwayv1alpha1.ResourceSpec{
		GPU:    &airunwayv1alpha1.GPUSpec{Count: 2},
		Memory: "64Gi",
		CPU:    "16",
	})
	limits, _ := result["limits"].(map[string]interface{})
	requests, _ := result["requests"].(map[string]interface{})

	if limits["gpu"] != "2" {
		t.Errorf("expected gpu limit 2, got %v", limits["gpu"])
	}
	if limits["memory"] != "64Gi" {
		t.Errorf("expected memory limit 64Gi, got %v", limits["memory"])
	}
	if limits["cpu"] != "16" {
		t.Errorf("expected cpu limit 16, got %v", limits["cpu"])
	}
	if requests["gpu"] != "2" {
		t.Errorf("expected gpu request 2, got %v", requests["gpu"])
	}
	// Memory and CPU should not be in requests (only gpu goes there)
	if _, ok := requests["memory"]; ok {
		t.Error("did not expect memory in requests")
	}
}

// --- Storage Tests ---

func TestTransformWithModelCacheStorage(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
		Volumes: []airunwayv1alpha1.StorageVolume{
			{
				Name:      "model-data",
				ClaimName: "model-pvc",
				MountPath: "/model-cache",
				Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")

	// Check pvcs
	pvcs, ok := spec["pvcs"].([]interface{})
	if !ok {
		t.Fatal("expected pvcs in spec")
	}
	if len(pvcs) != 1 {
		t.Fatalf("expected 1 pvc, got %d", len(pvcs))
	}
	pvc, _ := pvcs[0].(map[string]interface{})
	if pvc["name"] != "model-pvc" {
		t.Errorf("expected pvc name 'model-pvc', got %v", pvc["name"])
	}
	if pvc["create"] != false {
		t.Errorf("expected pvc create=false, got %v", pvc["create"])
	}

	// Check worker has volumeMounts
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	volumeMounts, ok := worker["volumeMounts"].([]interface{})
	if !ok {
		t.Fatal("expected volumeMounts on worker")
	}
	if len(volumeMounts) != 1 {
		t.Fatalf("expected 1 volumeMount, got %d", len(volumeMounts))
	}
	mount, _ := volumeMounts[0].(map[string]interface{})
	if mount["name"] != "model-pvc" {
		t.Errorf("expected mount name 'model-pvc', got %v", mount["name"])
	}
	if mount["mountPoint"] != "/model-cache" {
		t.Errorf("expected mountPoint '/model-cache', got %v", mount["mountPoint"])
	}
	// readOnly should not be present when ReadOnly is false (default)
	if _, ok := mount["readOnly"]; ok {
		t.Errorf("expected readOnly to be absent when ReadOnly is false, got %v", mount["readOnly"])
	}

	// Check HF_HOME auto-injected
	eps, _ := worker["extraPodSpec"].(map[string]interface{})
	mc, _ := eps["mainContainer"].(map[string]interface{})
	envList, _ := mc["env"].([]interface{})
	foundHFHome := false
	for _, e := range envList {
		envMap, _ := e.(map[string]interface{})
		if envMap["name"] == "HF_HOME" && envMap["value"] == "/model-cache" {
			foundHFHome = true
			break
		}
	}
	if !foundHFHome {
		t.Errorf("expected HF_HOME=/model-cache in env, got: %v", envList)
	}
}

func TestTransformWithCompilationCacheStorage(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
		Volumes: []airunwayv1alpha1.StorageVolume{
			{
				Name:      "compile-data",
				ClaimName: "compile-pvc",
				MountPath: "/compilation-cache",
				Purpose:   airunwayv1alpha1.VolumePurposeCompilationCache,
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	volumeMounts, _ := worker["volumeMounts"].([]interface{})
	if len(volumeMounts) != 1 {
		t.Fatalf("expected 1 volumeMount, got %d", len(volumeMounts))
	}
	mount, _ := volumeMounts[0].(map[string]interface{})
	if mount["useAsCompilationCache"] != true {
		t.Errorf("expected useAsCompilationCache=true, got %v", mount["useAsCompilationCache"])
	}
}

func TestTransformWithBothCaches(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
		Volumes: []airunwayv1alpha1.StorageVolume{
			{
				Name:      "model-data",
				ClaimName: "model-pvc",
				MountPath: "/model-cache",
				Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
			},
			{
				Name:      "compile-data",
				ClaimName: "compile-pvc",
				MountPath: "/compilation-cache",
				Purpose:   airunwayv1alpha1.VolumePurposeCompilationCache,
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")

	// Check pvcs
	pvcs, _ := spec["pvcs"].([]interface{})
	if len(pvcs) != 2 {
		t.Fatalf("expected 2 pvcs, got %d", len(pvcs))
	}

	// Check worker has 2 volumeMounts
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	volumeMounts, _ := worker["volumeMounts"].([]interface{})
	if len(volumeMounts) != 2 {
		t.Fatalf("expected 2 volumeMounts, got %d", len(volumeMounts))
	}

	// Verify one has useAsCompilationCache and the other doesn't
	foundModel := false
	foundCompile := false
	for _, vm := range volumeMounts {
		m, _ := vm.(map[string]interface{})
		if m["name"] == "model-pvc" && m["mountPoint"] == "/model-cache" {
			foundModel = true
			if _, hasKey := m["useAsCompilationCache"]; hasKey {
				t.Error("model-pvc should not have useAsCompilationCache")
			}
		}
		if m["name"] == "compile-pvc" && m["mountPoint"] == "/compilation-cache" {
			foundCompile = true
			if m["useAsCompilationCache"] != true {
				t.Error("compile-pvc should have useAsCompilationCache=true")
			}
		}
	}
	if !foundModel {
		t.Error("missing model-pvc volumeMount")
	}
	if !foundCompile {
		t.Error("missing compile-pvc volumeMount")
	}
}

func TestTransformNoStorageNoPVCs(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	// No storage configured

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")

	// Should not have pvcs key
	if _, ok := spec["pvcs"]; ok {
		t.Error("expected no pvcs key when storage is not configured")
	}

	// Worker should not have volumeMounts
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	if _, ok := worker["volumeMounts"]; ok {
		t.Error("expected no volumeMounts when storage is not configured")
	}
}

func TestTransformHFHomeNotInjectedWhenUserSetsIt(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
		Volumes: []airunwayv1alpha1.StorageVolume{
			{
				Name:      "model-data",
				ClaimName: "model-pvc",
				MountPath: "/model-cache",
				Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
			},
		},
	}
	// User explicitly sets HF_HOME
	md.Spec.Env = []corev1.EnvVar{
		{Name: "HF_HOME", Value: "/custom/hf/home"},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})

	// HF_HOME should NOT be auto-injected
	eps, _ := worker["extraPodSpec"].(map[string]interface{})
	mc, _ := eps["mainContainer"].(map[string]interface{})
	envList, _ := mc["env"].([]interface{})
	for _, e := range envList {
		envMap, _ := e.(map[string]interface{})
		if envMap["name"] == "HF_HOME" {
			t.Errorf("HF_HOME should not be auto-injected when user sets it, found: %v", envMap)
		}
	}
}

func TestTransformFrontendHasNoVolumeMounts(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
		Volumes: []airunwayv1alpha1.StorageVolume{
			{
				Name:      "model-data",
				ClaimName: "model-pvc",
				MountPath: "/model-cache",
				Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})
	frontend, _ := services["Frontend"].(map[string]interface{})

	// Frontend should NOT have volumeMounts (router doesn't load models)
	if _, ok := frontend["volumeMounts"]; ok {
		t.Error("frontend should not have volumeMounts")
	}
}

func TestTransformDisaggregatedBothWorkersGetVolumeMounts(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{
		Mode: airunwayv1alpha1.ServingModeDisaggregated,
	}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 2},
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
		},
	}
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
		Volumes: []airunwayv1alpha1.StorageVolume{
			{
				Name:      "model-data",
				ClaimName: "model-pvc",
				MountPath: "/model-cache",
				Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")
	services, _ := spec["services"].(map[string]interface{})

	// Check prefill worker has volumeMounts
	prefill, _ := services["VllmPrefillWorker"].(map[string]interface{})
	prefillMounts, ok := prefill["volumeMounts"].([]interface{})
	if !ok || len(prefillMounts) == 0 {
		t.Error("expected volumeMounts on prefill worker")
	}

	// Check decode worker has volumeMounts
	decode, _ := services["VllmDecodeWorker"].(map[string]interface{})
	decodeMounts, ok := decode["volumeMounts"].([]interface{})
	if !ok || len(decodeMounts) == 0 {
		t.Error("expected volumeMounts on decode worker")
	}

	// Check HF_HOME is injected on both workers
	for name, svc := range map[string]map[string]interface{}{"prefill": prefill, "decode": decode} {
		eps, _ := svc["extraPodSpec"].(map[string]interface{})
		mc, _ := eps["mainContainer"].(map[string]interface{})
		envList, _ := mc["env"].([]interface{})
		foundHFHome := false
		for _, e := range envList {
			envMap, _ := e.(map[string]interface{})
			if envMap["name"] == "HF_HOME" && envMap["value"] == "/model-cache" {
				foundHFHome = true
				break
			}
		}
		if !foundHFHome {
			t.Errorf("expected HF_HOME=/model-cache on %s worker", name)
		}
	}

	// Check frontend has NO volumeMounts
	frontend, _ := services["Frontend"].(map[string]interface{})
	if _, ok := frontend["volumeMounts"]; ok {
		t.Error("frontend should not have volumeMounts in disaggregated mode")
	}
}

func TestTransformWithReadOnlyVolume(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("test-model", "default")
	md.Spec.Model.Storage = &airunwayv1alpha1.StorageSpec{
		Volumes: []airunwayv1alpha1.StorageVolume{
			{
				Name:      "model-data",
				ClaimName: "model-pvc",
				MountPath: "/model-cache",
				Purpose:   airunwayv1alpha1.VolumePurposeModelCache,
				ReadOnly:  true,
			},
		},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dgd := resources[0]
	spec, _, _ := unstructured.NestedMap(dgd.Object, "spec")

	// Check worker has volumeMounts with readOnly
	services, _ := spec["services"].(map[string]interface{})
	worker, _ := services["VllmWorker"].(map[string]interface{})
	volumeMounts, ok := worker["volumeMounts"].([]interface{})
	if !ok {
		t.Fatal("expected volumeMounts on worker")
	}
	if len(volumeMounts) != 1 {
		t.Fatalf("expected 1 volumeMount, got %d", len(volumeMounts))
	}
	mount, _ := volumeMounts[0].(map[string]interface{})
	if mount["name"] != "model-pvc" {
		t.Errorf("expected mount name 'model-pvc', got %v", mount["name"])
	}
	if mount["mountPoint"] != "/model-cache" {
		t.Errorf("expected mountPoint '/model-cache', got %v", mount["mountPoint"])
	}
	if mount["readOnly"] != true {
		t.Errorf("expected readOnly=true, got %v", mount["readOnly"])
	}
}

func findEnvVar(service map[string]interface{}, name string) map[string]interface{} {
	eps, _ := service["extraPodSpec"].(map[string]interface{})
	mc, _ := eps["mainContainer"].(map[string]interface{})
	envList, _ := mc["env"].([]interface{})
	for _, e := range envList {
		envMap, _ := e.(map[string]interface{})
		if envMap["name"] == name {
			return envMap
		}
	}
	return nil
}

func assertFieldRefEnvVar(t *testing.T, service map[string]interface{}, name, fieldPath string) {
	t.Helper()

	env := findEnvVar(service, name)
	if env == nil {
		t.Fatalf("expected env var %s to be present", name)
	}

	valueFrom, _ := env["valueFrom"].(map[string]interface{})
	fieldRef, _ := valueFrom["fieldRef"].(map[string]interface{})
	if fieldRef["fieldPath"] != fieldPath {
		t.Fatalf("expected %s fieldPath %q, got %v", name, fieldPath, fieldRef["fieldPath"])
	}
}

func assertArg(t *testing.T, args []string, flag, value string) {
	t.Helper()

	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			if args[i+1] != value {
				t.Fatalf("expected %s value %q, got %q in %v", flag, value, args[i+1], args)
			}
			return
		}
	}

	t.Fatalf("expected %s %q in args: %v", flag, value, args)
}

func assertNoArg(t *testing.T, args []string, flag string) {
	t.Helper()

	for _, arg := range args {
		if arg == flag {
			t.Fatalf("did not expect %s in args: %v", flag, args)
		}
	}
}
