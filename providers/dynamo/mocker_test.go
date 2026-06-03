package dynamo

import (
	"context"
	"strings"
	"testing"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// newMockerMD builds an aggregated mocker-annotated ModelDeployment with no GPU.
func newMockerMD(name string) *airunwayv1alpha1.ModelDeployment {
	md := newTestMD(name, "default")
	md.Annotations = map[string]string{AnnotationDynamoTestBackend: DynamoTestBackendMocker}
	md.Spec.Model.ID = "Qwen/Qwen3-0.6B"
	md.Spec.Resources = nil
	return md
}

// dgdService extracts a service map from a transformed DGD.
func dgdService(t *testing.T, dgd *unstructured.Unstructured, name string) map[string]interface{} {
	t.Helper()
	services, found, err := unstructured.NestedMap(dgd.Object, "spec", "services")
	if err != nil || !found {
		t.Fatalf("spec.services not found: err=%v found=%v", err, found)
	}
	svc, ok := services[name].(map[string]interface{})
	if !ok {
		t.Fatalf("service %q not found in services (have %v)", name, keysOf(services))
	}
	return svc
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// mainContainer returns the worker/frontend main container map.
func mainContainer(t *testing.T, svc map[string]interface{}) map[string]interface{} {
	t.Helper()
	pod, ok := svc["extraPodSpec"].(map[string]interface{})
	if !ok {
		t.Fatalf("extraPodSpec missing")
	}
	c, ok := pod["mainContainer"].(map[string]interface{})
	if !ok {
		t.Fatalf("mainContainer missing")
	}
	return c
}

func toStringSlice(t *testing.T, v interface{}) []string {
	t.Helper()
	raw, ok := v.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", v)
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("expected string element, got %T", e)
		}
		out = append(out, s)
	}
	return out
}

func TestIsMockerMode(t *testing.T) {
	if isMockerMode(nil) {
		t.Error("nil md should not be mocker mode")
	}
	md := newTestMD("x", "default")
	if isMockerMode(md) {
		t.Error("md without annotation should not be mocker mode")
	}
	md.Annotations = map[string]string{AnnotationDynamoTestBackend: "something-else"}
	if isMockerMode(md) {
		t.Error("md with other annotation value should not be mocker mode")
	}
	md.Annotations[AnnotationDynamoTestBackend] = DynamoTestBackendMocker
	if !isMockerMode(md) {
		t.Error("md with mocker annotation should be mocker mode")
	}
}

func TestValidateCompatibilityMocker(t *testing.T) {
	r := &DynamoProviderReconciler{}

	t.Run("aggregated CPU-only mocker is compatible", func(t *testing.T) {
		md := newMockerMD("agg")
		if err := r.validateCompatibility(md); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("disaggregated CPU-only mocker with scaling is compatible", func(t *testing.T) {
		md := newMockerMD("disagg")
		md.Spec.Serving = &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated}
		md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
			Prefill: &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
			Decode:  &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
		}
		if err := r.validateCompatibility(md); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("disaggregated mocker missing scaling is rejected", func(t *testing.T) {
		md := newMockerMD("disagg-bad")
		md.Spec.Serving = &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated}
		err := r.validateCompatibility(md)
		if err == nil || !strings.Contains(err.Error(), "prefill and spec.scaling.decode") {
			t.Fatalf("expected prefill/decode error, got %v", err)
		}
	})

	t.Run("non-vllm mocker is rejected", func(t *testing.T) {
		md := newMockerMD("sglang")
		md.Spec.Engine.Type = airunwayv1alpha1.EngineTypeSGLang
		err := r.validateCompatibility(md)
		if err == nil || !strings.Contains(err.Error(), "only supports the vllm engine") {
			t.Fatalf("expected vllm-only error, got %v", err)
		}
	})

	t.Run("non-mocker CPU-only still rejected", func(t *testing.T) {
		md := newTestMD("nogpu", "default")
		md.Spec.Resources = nil
		err := r.validateCompatibility(md)
		if err == nil || !strings.Contains(err.Error(), "Dynamo requires GPU") {
			t.Fatalf("expected GPU-required error, got %v", err)
		}
	})
}

func TestTransformMockerAggregated(t *testing.T) {
	tr := NewTransformer()
	md := newMockerMD("mock-agg")

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dgd := resources[0]

	// Standalone Frontend, no EPP (gateway forced off in mocker mode).
	if _, found, _ := unstructured.NestedMap(dgd.Object, "spec", "services"); !found {
		t.Fatal("spec.services missing")
	}
	frontend := dgdService(t, dgd, "Frontend")
	fImage, _ := mainContainer(t, frontend)["image"].(string)
	if !strings.Contains(fImage, "dynamo-planner") {
		t.Errorf("frontend image=%q, expected dynamo-planner", fImage)
	}

	worker := dgdService(t, dgd, "VllmWorker")
	c := mainContainer(t, worker)

	image, _ := c["image"].(string)
	if !strings.Contains(image, "dynamo-planner") {
		t.Errorf("worker image=%q, expected dynamo-planner", image)
	}

	command := toStringSlice(t, c["command"])
	if strings.Join(command, " ") != "python3 -m dynamo.mocker" {
		t.Errorf("worker command=%v, expected python3 -m dynamo.mocker", command)
	}

	args := strings.Join(toStringSlice(t, c["args"]), " ")
	for _, want := range []string{
		"--model-path Qwen/Qwen3-0.6B",
		"--model-name Qwen/Qwen3-0.6B",
		"--speedup-ratio 1000.0",
		"--num-gpu-blocks-override 4096",
		"--block-size 16",
		"--max-num-seqs 64",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("worker args %q missing %q", args, want)
		}
	}

	// No GPU, but small CPU/memory requests+limits. Equal requests and limits
	// make the pod Guaranteed QoS (the point is only to avoid BestEffort).
	res, _ := worker["resources"].(map[string]interface{})
	requests, _ := res["requests"].(map[string]interface{})
	limits, _ := res["limits"].(map[string]interface{})
	if _, hasGPU := requests["gpu"]; hasGPU {
		t.Errorf("expected no GPU request in mocker mode, got requests=%v", requests)
	}
	if _, hasGPU := limits["gpu"]; hasGPU {
		t.Errorf("expected no GPU limit in mocker mode, got limits=%v", limits)
	}
	if requests["cpu"] != MockerWorkerCPU || requests["memory"] != MockerWorkerMemory {
		t.Errorf("expected CPU/memory requests %s/%s, got %v", MockerWorkerCPU, MockerWorkerMemory, requests)
	}
	if limits["cpu"] != MockerWorkerCPU || limits["memory"] != MockerWorkerMemory {
		t.Errorf("expected CPU/memory limits %s/%s, got %v", MockerWorkerCPU, MockerWorkerMemory, limits)
	}
}

func TestTransformMockerUsesServedName(t *testing.T) {
	tr := NewTransformer()
	md := newMockerMD("mock-served")
	md.Spec.Model.ServedName = "my-model"

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	worker := dgdService(t, resources[0], "VllmWorker")
	args := strings.Join(toStringSlice(t, mainContainer(t, worker)["args"]), " ")
	if !strings.Contains(args, "--model-name my-model") {
		t.Errorf("expected --model-name my-model, got %q", args)
	}
}

// TestTransformMockerImageWinsOverOverride asserts the planner image is used in
// mocker mode even when spec.Image is set: the dynamo.mocker module only exists
// in the planner image, so an arbitrary override would break the test backend.
func TestTransformMockerImageWinsOverOverride(t *testing.T) {
	tr := NewTransformer()
	md := newMockerMD("mock-img")
	md.Spec.Image = "example.com/some/custom-image:latest"

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	worker := dgdService(t, resources[0], "VllmWorker")
	image, _ := mainContainer(t, worker)["image"].(string)
	if !strings.Contains(image, "dynamo-planner") {
		t.Errorf("worker image=%q, expected dynamo-planner to win over spec.Image", image)
	}
	if strings.Contains(image, "custom-image") {
		t.Errorf("worker image=%q must not use the spec.Image override in mocker mode", image)
	}
}

func TestTransformMockerDisaggregated(t *testing.T) {
	tr := NewTransformer()
	md := newMockerMD("mock-disagg")
	md.Spec.Serving = &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated}
	md.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
		Decode:  &airunwayv1alpha1.ComponentScalingSpec{Replicas: 1},
	}

	resources, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dgd := resources[0]

	// Standalone Frontend present, no EPP.
	if _, ok := dgdServiceOK(dgd, "Epp"); ok {
		t.Error("did not expect Epp service in mocker mode")
	}
	if _, ok := dgdServiceOK(dgd, "Frontend"); !ok {
		t.Error("expected standalone Frontend in mocker mode")
	}

	cases := map[string]string{
		"VllmPrefillWorker": "prefill",
		"VllmDecodeWorker":  "decode",
	}
	for svcName, mode := range cases {
		worker := dgdService(t, dgd, svcName)
		c := mainContainer(t, worker)

		command := strings.Join(toStringSlice(t, c["command"]), " ")
		if command != "python3 -m dynamo.mocker" {
			t.Errorf("%s command=%q, expected python3 -m dynamo.mocker", svcName, command)
		}

		args := strings.Join(toStringSlice(t, c["args"]), " ")
		if !strings.Contains(args, "--disaggregation-mode "+mode) {
			t.Errorf("%s args %q missing --disaggregation-mode %s", svcName, args, mode)
		}
		// Mocker must NOT use the real-vLLM NIXL flag.
		if strings.Contains(args, "--kv-transfer-config") {
			t.Errorf("%s args %q must not contain --kv-transfer-config in mocker mode", svcName, args)
		}

		res, _ := worker["resources"].(map[string]interface{})
		requests, _ := res["requests"].(map[string]interface{})
		if _, hasGPU := requests["gpu"]; hasGPU {
			t.Errorf("%s expected no GPU request, got %v", svcName, requests)
		}
		if requests["cpu"] != MockerWorkerCPU || requests["memory"] != MockerWorkerMemory {
			t.Errorf("%s expected CPU/memory requests %s/%s, got %v", svcName, MockerWorkerCPU, MockerWorkerMemory, requests)
		}
	}
}

// dgdServiceOK reports whether a named service exists in the DGD.
func dgdServiceOK(dgd *unstructured.Unstructured, name string) (map[string]interface{}, bool) {
	services, found, err := unstructured.NestedMap(dgd.Object, "spec", "services")
	if err != nil || !found {
		return nil, false
	}
	svc, ok := services[name].(map[string]interface{})
	return svc, ok
}
