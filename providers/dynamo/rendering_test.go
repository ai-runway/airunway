package dynamo

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func renderedBeta(t *testing.T, md *api.ModelDeployment) *unstructured.Unstructured {
	t.Helper()
	objects, err := NewTransformer().TransformForVersion(context.Background(), md, "nvidia.com/v1beta1", "1.5.0")
	if err != nil {
		t.Fatal(err)
	}
	return objects[0]
}
func betaComponent(t *testing.T, obj *unstructured.Unstructured, name string) map[string]any {
	t.Helper()
	components, _, err := unstructured.NestedSlice(obj.Object, "spec", "components")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range components {
		c := raw.(map[string]any)
		if c["name"] == name {
			return c
		}
	}
	t.Fatalf("component %s not found", name)
	return nil
}
func betaContainer(t *testing.T, component map[string]any, name string) map[string]any {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(component, "podTemplate", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range containers {
		c := raw.(map[string]any)
		if c["name"] == name {
			return c
		}
	}
	t.Fatalf("container %s not found", name)
	return nil
}
func containerEnv(t *testing.T, container map[string]any, name string) string {
	t.Helper()
	env, _, err := unstructured.NestedSlice(container, "env")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range env {
		e := raw.(map[string]any)
		if e["name"] == name {
			v, _ := e["value"].(string)
			return v
		}
	}
	return ""
}
func TestNativeBetaEPPAndWorker(t *testing.T) {
	md := newTestMD("model", "models")
	md.Spec.Secrets = &api.SecretsSpec{HuggingFaceToken: "model-token"}
	md.Spec.Engine.ExtraArgs = []string{"--block-size=128"}
	obj := renderedBeta(t, md)
	epp := betaComponent(t, obj, "Epp")
	if _, exists := epp["eppConfig"]; exists {
		t.Fatal("native Rust EPP must not have eppConfig")
	}
	main := betaContainer(t, epp, "main")
	if main["image"] != "nvcr.io/nvidia/ai-dynamo/dynamo-frontend:1.5.0" {
		t.Fatalf("incorrect EPP image: %v", main["image"])
	}
	if _, exists := main["command"]; exists {
		t.Fatal("Rust EPP must use operator entrypoint defaults")
	}
	if containerEnv(t, main, "DYN_MODEL_NAME") != md.Spec.Model.ID || containerEnv(t, main, "DYN_KV_CACHE_BLOCK_SIZE") != "128" {
		t.Fatalf("missing EPP configuration: %v", main)
	}
	if containerEnv(t, main, "DYN_ENFORCE_DISAGG") != "false" {
		t.Fatal("wrong disaggregation setting")
	}
	if envFrom, _, _ := unstructured.NestedSlice(main, "envFrom"); len(envFrom) != 1 {
		t.Fatal("EPP token not propagated")
	}
	worker := betaComponent(t, obj, "VllmWorker")
	if worker["frontendSidecar"] != "sidecar-frontend" {
		t.Fatalf("bad frontend sidecar reference: %v", worker["frontendSidecar"])
	}
	container := betaContainer(t, worker, "main")
	command, _, _ := unstructured.NestedStringSlice(container, "command")
	if !reflect.DeepEqual(command, []string{"python3", "-m", "dynamo.vllm"}) {
		t.Fatalf("wrong worker command: %v", command)
	}
	args, _, _ := unstructured.NestedStringSlice(container, "args")
	if flagValue(args, "--block-size") != "128" || flagValue(args, "--kv-events-config") != `{"enable_kv_cache_events":true}` {
		t.Fatalf("incorrect KV events contract: %v", args)
	}
	if gpu, _, _ := unstructured.NestedString(container, "resources", "limits", "nvidia.com/gpu"); gpu != "1" {
		t.Fatal("missing native GPU resource")
	}
	sidecar := betaContainer(t, worker, "sidecar-frontend")
	if sidecar["image"] != "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.5.0" {
		t.Fatal("sidecar image version drift")
	}
}

func TestNativeBetaStorageAndPlacement(t *testing.T) {
	md := newTestMD("model", "models")
	md.Spec.Gateway = &api.GatewaySpec{Enabled: boolPtr(false)}
	md.Spec.Model.Storage = &api.StorageSpec{Volumes: []api.StorageVolume{
		{Name: "weights", ClaimName: "model-cache", MountPath: "/models", Purpose: api.VolumePurposeModelCache, ReadOnly: true},
		{Name: "compile", ClaimName: "compile-cache", MountPath: "/compile", Purpose: api.VolumePurposeCompilationCache},
	}}
	md.Spec.NodeSelector = map[string]string{"pool": "gpu"}
	md.Spec.Tolerations = []corev1.Toleration{{Key: "gpu", Operator: corev1.TolerationOpExists}}
	md.Spec.Env = []corev1.EnvVar{{Name: "CUSTOM", Value: "set"}}
	md.Spec.PodTemplate = &api.PodTemplateSpec{Metadata: &api.PodTemplateMetadata{Labels: map[string]string{"team": "ml"}}}
	obj := renderedBeta(t, md)
	assertContract(t, readReleasedContract(t, "v1.5.0", "dynamographdeployments", "v1beta1"), obj)
	c := betaComponent(t, obj, "VllmWorker")
	main := betaContainer(t, c, "main")
	if containerEnv(t, main, "HF_HOME") != "/models" {
		t.Fatal("HF_HOME lost")
	}
	if selector, _, _ := unstructured.NestedString(c, "podTemplate", "spec", "nodeSelector", "pool"); selector != "gpu" {
		t.Fatal("placement lost")
	}
	if cache, _, _ := unstructured.NestedString(c, "compilationCache", "pvcName"); cache != "compile-cache" {
		t.Fatal("compilation cache lost")
	}
	mounts, _, _ := unstructured.NestedSlice(main, "volumeMounts")
	if len(mounts) != 2 || mounts[0].(map[string]any)["readOnly"] != true {
		t.Fatalf("mounts lost: %v", mounts)
	}
	volumes, _, _ := unstructured.NestedSlice(c, "podTemplate", "spec", "volumes")
	if len(volumes) != 2 {
		t.Fatal("PVC volumes lost")
	}
	if label, _, _ := unstructured.NestedString(obj.Object, "spec", "labels", "team"); label != "ml" {
		t.Fatal("pod metadata lost")
	}
	env, _, _ := unstructured.NestedSlice(obj.Object, "spec", "env")
	if len(env) != 1 {
		t.Fatal("global env lost")
	}
}

func TestBetaLegacyOverridesPreserveEPPAndCustomContainer(t *testing.T) {
	md := newTestMD("model", "models")
	setRenderingOverrides(t, md, map[string]any{"spec": map[string]any{"services": map[string]any{
		"Epp":        map[string]any{"eppConfig": map[string]any{"configMapRef": map[string]any{"name": "legacy", "key": "config.yaml"}}, "extraPodSpec": map[string]any{"mainContainer": map[string]any{"image": "registry/legacy-epp:1.1.1"}}},
		"VllmWorker": map[string]any{"extraPodSpec": map[string]any{"mainContainer": map[string]any{"env": []any{map[string]any{"name": "CUSTOM", "value": "value"}}, "image": "registry/custom:tag"}}},
	}}})
	obj := renderedBeta(t, md)
	assertContract(t, readReleasedContract(t, "v1.5.0", "dynamographdeployments", "v1beta1"), obj)
	epp := betaComponent(t, obj, "Epp")
	if _, ok := epp["eppConfig"]; !ok {
		t.Fatal("legacy EPP config lost")
	}
	if betaContainer(t, epp, "main")["image"] != "registry/legacy-epp:1.1.1" {
		t.Fatal("explicit legacy EPP image changed")
	}
	worker := betaContainer(t, betaComponent(t, obj, "VllmWorker"), "main")
	if worker["image"] != "registry/custom:tag" || containerEnv(t, worker, "CUSTOM") != "value" {
		t.Fatal("custom worker settings lost")
	}
}

func TestNativeOverridesMergeNamedPodFields(t *testing.T) {
	md := newTestMD("model", "models")
	setRenderingOverrides(t, md, map[string]any{"spec": map[string]any{"components": []any{map[string]any{
		"name": "VllmWorker", "podTemplate": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "main", "image": "registry/custom:v2", "env": []any{map[string]any{"name": "CUSTOM", "value": "yes"}}}}}},
	}}}})
	obj := renderedBeta(t, md)
	assertContract(t, readReleasedContract(t, "v1.5.0", "dynamographdeployments", "v1beta1"), obj)
	worker := betaComponent(t, obj, "VllmWorker")
	main := betaContainer(t, worker, "main")
	if main["image"] != "registry/custom:v2" || containerEnv(t, main, "CUSTOM") != "yes" {
		t.Fatal("override not merged")
	}
	if args, _, _ := unstructured.NestedSlice(main, "args"); len(args) == 0 {
		t.Fatal("partial override lost generated command arguments")
	}
	betaContainer(t, worker, "sidecar-frontend")
	betaComponent(t, obj, "Epp")
}

func TestVersionedRenderingDoesNotMutateDefaultsOrInput(t *testing.T) {
	tr := NewTransformer()
	md := newTestMD("model", "models")
	original := md.DeepCopy()
	before, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, target := range []struct{ api, version string }{{"v1alpha1", "1.1.1"}, {"v1beta1", "1.5.0"}} {
		wg.Add(1)
		go func(apiVersion, runtimeVersion string) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_, err := tr.TransformForVersion(context.Background(), md, apiVersion, runtimeVersion)
				if err != nil {
					t.Error(err)
				}
			}
		}(target.api, target.version)
	}
	wg.Wait()
	after, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(md, original) {
		t.Fatal("input ModelDeployment mutated")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("default rendering changed after versioned calls")
	}
	if tr.runtimeVersion != "" || tr.nativeBeta {
		t.Fatal("shared transformer mutated")
	}
	// Explicit images belong to the caller even when the selected runtime changes.
	md.Spec.Engine.Image = "registry/pinned@sha256:abcd"
	if got := betaContainer(t, betaComponent(t, renderedBeta(t, md), "VllmWorker"), "main")["image"]; got != md.Spec.Engine.Image {
		t.Fatalf("explicit image changed to %v", got)
	}
}

func TestBetaUnsupportedLegacyFieldsFailExplicitly(t *testing.T) {
	for _, spec := range []map[string]any{
		{"services": map[string]any{"VllmWorker": map[string]any{"ingress": map[string]any{"enabled": true}}}},
		{"services": map[string]any{"VllmWorker": map[string]any{"roles": []any{map[string]any{"name": "worker"}}}}},
		{"pvcs": []any{map[string]any{"name": "new-pvc", "create": true}}},
		{"components": []any{}, "services": map[string]any{}},
		{"components": []any{}, "envs": []any{}},
		{"services": map[string]any{"VllmWorker": map[string]any{"extraPodSpec": map[string]any{"containers": []any{map[string]any{"name": "main", "image": "unused"}}}}}},
	} {
		md := newTestMD("model", "models")
		setRenderingOverrides(t, md, map[string]any{"spec": spec})
		if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0"); err == nil {
			t.Fatalf("silently accepted unsupported override: %v", spec)
		}
	}
}

func TestTypedIntentRejectsUnrepresentableInputs(t *testing.T) {
	for name, change := range map[string]func(*api.ModelDeployment){
		"env":         func(md *api.ModelDeployment) { md.Spec.Env = []corev1.EnvVar{{Name: "X", Value: "Y"}} },
		"selector":    func(md *api.ModelDeployment) { md.Spec.NodeSelector = map[string]string{"pool": "gpu"} },
		"tolerations": func(md *api.ModelDeployment) { md.Spec.Tolerations = []corev1.Toleration{{Key: "gpu"}} },
		"secret":      func(md *api.ModelDeployment) { md.Spec.Secrets = &api.SecretsSpec{HuggingFaceToken: "custom"} },
		"cache": func(md *api.ModelDeployment) {
			md.Spec.Model.Storage = &api.StorageSpec{Volumes: []api.StorageVolume{{Name: "cache", MountPath: "/models", Purpose: api.VolumePurposeModelCache}}}
		},
		"mocker": func(md *api.ModelDeployment) {
			md.Annotations = map[string]string{AnnotationDynamoTestBackend: DynamoTestBackendMocker}
		},
		"metadata": func(md *api.ModelDeployment) {
			md.Spec.PodTemplate = &api.PodTemplateSpec{Metadata: &api.PodTemplateMetadata{Labels: map[string]string{"x": "y"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			md := typedRenderingMD(t)
			change(md)
			if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0"); err == nil {
				t.Fatal("unsupported input silently ignored")
			}
		})
	}
}

func TestTypedIntentStrictParsingAndLegacyCoexistence(t *testing.T) {
	for _, raw := range []string{
		`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":1}},"spec":{}}`,
		`{"intent":{"hardware":{"totalGpus":1}}}`,
		`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":1},"searchStrategy":"thorough"}}`,
		`{"deploymentMode":"intent","intent":{"hardware":{"totalGpus":1},"typo":true}}`,
		`{"deploymentMode":"intent","intent":null}`,
		`{"deploymentMode":"intent","Intent":{"hardware":{"totalGpus":0}}}`,
	} {
		md := typedRenderingMD(t)
		md.Spec.Provider.Overrides = &runtime.RawExtension{Raw: []byte(raw)}
		if _, err := NewTransformer().Transform(context.Background(), md); err == nil {
			t.Fatalf("accepted invalid input %s", raw)
		}
	}
	md := typedRenderingMD(t)
	md.Spec.Secrets = &api.SecretsSpec{HuggingFaceToken: "hf-token-secret"}
	objects, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if objects[0].GetKind() != DynamoGraphDeploymentRequestKind {
		t.Fatal("intent rendered direct DGD")
	}
	legacy := newTestMD("legacy", "models")
	setRenderingOverrides(t, legacy, map[string]any{"deploymentMode": "intent", "spec": map[string]any{"searchStrategy": "thorough", "modelCache": map[string]any{"pvcName": "weights", "pvcMountPath": "/weights", "pvcModelPath": "checkpoint"}}})
	objects, err = NewTransformer().TransformForVersion(context.Background(), legacy, "v1alpha1", "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if path, _, _ := unstructured.NestedString(objects[0].Object, "spec", "modelCache", "pvcModelPath"); path != "checkpoint" {
		t.Fatal("legacy cache override lost")
	}
}

func TestRenderingVersionErrors(t *testing.T) {
	for _, target := range [][2]string{{"v1", "1.5.0"}, {"v1beta1", "1.1.1"}, {"v1alpha1", "latest"}, {"v1alpha1", ""}} {
		if _, err := NewTransformer().TransformForVersion(context.Background(), newTestMD("model", "models"), target[0], target[1]); err == nil {
			t.Fatalf("accepted unsupported version pair %v", target)
		}
	}
	md := newTestMD("model", "models")
	setRenderingOverrides(t, md, map[string]any{"spec": map[string]any{"components": []any{}}})
	if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1alpha1", "1.1.1"); err == nil || !strings.Contains(err.Error(), "spec.components") {
		t.Fatalf("expected schema conflict error, got %v", err)
	}
}

func TestLegacyAlphaVersionedRendering(t *testing.T) {
	md := newTestMD("model", "models")
	tr := NewTransformer()
	objects, err := tr.TransformForVersion(context.Background(), md, "v1alpha1", "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	object := objects[0]
	if _, found, _ := unstructured.NestedMap(object.Object, "spec", "services", "Epp", "eppConfig"); !found {
		t.Fatal("1.1.1 legacy EPP contract changed")
	}
	image, _, _ := unstructured.NestedString(object.Object, "spec", "services", "VllmWorker", "extraPodSpec", "mainContainer", "image")
	if image != "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.1.1" {
		t.Fatalf("image drift: %s", image)
	}
	// Golden comparison with the existing Transform after explicitly supplying
	// identical images, independent of the executable's build-time defaults.
	setRenderingOverrides(t, md, map[string]any{"epp": map[string]any{"image": "registry/epp:pinned"}, "spec": map[string]any{"services": map[string]any{"VllmWorker": map[string]any{"frontendSidecar": map[string]any{"image": "registry/frontend:pinned"}}}}})
	md.Spec.Engine.Image = "registry/worker:pinned"
	a, err := tr.Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tr.TransformForVersion(context.Background(), md, "v1alpha1", "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	if string(left) != string(right) {
		t.Fatal("versioned alpha differs from legacy rendering despite explicit image pins")
	}
}

func TestBetaEPPRuntimeContract(t *testing.T) {
	for _, tc := range []struct {
		name, image, override string
		legacy, wantError     bool
	}{
		{name: "native", image: "registry/epp:1.5.0"},
		{name: "legacy", image: "registry/epp:1.1.1", legacy: true},
		{name: "mixed-native", image: "registry/epp:1.5.0", legacy: true, wantError: true},
		{name: "missing-legacy-config", image: "registry/epp:1.1.1", wantError: true},
		{name: "custom-tag-native", image: "registry/epp:custom"},
		{name: "custom-tag-legacy", image: "registry/epp:custom", legacy: true, override: "1.1.1"},
		{name: "custom-tag-unclaimed-legacy", image: "registry/epp:custom", legacy: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := newTestMD("model", "models")
			c := map[string]any{"name": "Epp", "podTemplate": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "main", "image": tc.image}}}}}
			if tc.legacy {
				c["eppConfig"] = map[string]any{"configMapRef": map[string]any{"name": "legacy", "key": "config.yaml"}}
			}
			if tc.override != "" {
				c["runtimeVersionOverride"] = tc.override
			}
			setRenderingOverrides(t, md, map[string]any{"spec": map[string]any{"components": []any{c}}})
			objs, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0")
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected runtime contract result: %v", err)
			}
			if !tc.wantError && strings.Contains(tc.image, ":custom") {
				component := betaComponent(t, objs[0], "Epp")
				want := tc.override
				if want == "" {
					want = "1.5.0"
				}
				if component["runtimeVersionOverride"] != want {
					t.Fatalf("custom image runtime not declared: %v", component)
				}
			}
		})
	}
}

func TestAlphaSchemaWithModernRuntimeUsesRustEPP(t *testing.T) {
	md := newTestMD("model", "models")
	objs, err := NewTransformer().TransformForVersion(context.Background(), md, "v1alpha1", "1.5.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, present, _ := unstructured.NestedFieldNoCopy(objs[0].Object, "spec", "services", "Epp", "eppConfig"); present {
		t.Fatal("1.5 runtime cannot use legacy EPP defaults")
	}
	assertContract(t, readReleasedContract(t, "v1.5.0", "dynamographdeployments", "v1alpha1"), objs[0])
}

func TestTypedIntentAcceptsAPIDefaultedPrefixCaching(t *testing.T) {
	md := typedRenderingMD(t)
	md.Spec.Engine.EnablePrefixCaching = true
	if _, err := NewTransformer().TransformForVersion(context.Background(), md, "v1beta1", "1.5.0"); err != nil {
		t.Fatal(err)
	}
}
