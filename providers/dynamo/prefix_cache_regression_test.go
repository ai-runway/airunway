package dynamo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

func prefixCacheSchema(t *testing.T) *structuralschema.Structural {
	t.Helper()

	data, err := os.ReadFile("../../controller/config/crd/bases/airunway.ai_modeldeployments.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var external apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &external); err != nil {
		t.Fatal(err)
	}

	var internal apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&external, &internal, nil); err != nil {
		t.Fatal(err)
	}

	validation, err := apiextensions.GetSchemaForVersion(&internal, "v1alpha1")
	if err != nil {
		t.Fatal(err)
	}

	schema, err := structuralschema.NewStructural(validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}

	return schema
}

func prefixCacheFlags(t *testing.T, md *airunwayv1alpha1.ModelDeployment, worker string) (bool, bool) {
	t.Helper()

	resources, err := NewTransformer().Transform(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}

	raw, found, err := unstructured.NestedSlice(resources[0].Object, "spec", "services", worker, "extraPodSpec", "mainContainer", "args")
	if err != nil || !found {
		t.Fatalf("read %s args: found=%v err=%v", worker, found, err)
	}

	positive := false
	negative := false
	for _, value := range raw {
		s := fmt.Sprint(value)
		if s == "--enable-prefix-caching" {
			positive = true
		}
		if s == "--no-enable-prefix-caching" {
			negative = true
		}
	}

	return positive, negative
}

func disaggregatedCopy(md *airunwayv1alpha1.ModelDeployment) *airunwayv1alpha1.ModelDeployment {
	copy := md.DeepCopy()
	copy.Spec.Serving = &airunwayv1alpha1.ServingSpec{Mode: airunwayv1alpha1.ServingModeDisaggregated}
	copy.Spec.Scaling = &airunwayv1alpha1.ScalingSpec{
		Prefill: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
		},
		Decode: &airunwayv1alpha1.ComponentScalingSpec{
			Replicas: 1,
			GPU:      &airunwayv1alpha1.GPUSpec{Count: 1},
		},
	}
	return copy
}

func assertPrefixCacheFlags(t *testing.T, md *airunwayv1alpha1.ModelDeployment, wantPositive, wantNegative bool) {
	t.Helper()

	for _, worker := range []string{"VllmWorker", "VllmPrefillWorker", "VllmDecodeWorker"} {
		mdForWorker := md
		if worker != "VllmWorker" {
			mdForWorker = disaggregatedCopy(md)
		}

		gotPositive, gotNegative := prefixCacheFlags(t, mdForWorker, worker)
		if gotPositive != wantPositive || gotNegative != wantNegative {
			t.Fatalf("%s flags positive=%v negative=%v, want positive=%v negative=%v", worker, gotPositive, gotNegative, wantPositive, wantNegative)
		}
	}
}

func mustModelDeployment(t *testing.T, raw map[string]any) airunwayv1alpha1.ModelDeployment {
	t.Helper()
	var md airunwayv1alpha1.ModelDeployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &md); err != nil {
		t.Fatal(err)
	}
	return md
}

func assertPrefixCachingValue(t *testing.T, md *airunwayv1alpha1.ModelDeployment, want bool, where string) {
	t.Helper()
	if md.Spec.Engine.EnablePrefixCaching == nil || *md.Spec.Engine.EnablePrefixCaching != want {
		t.Fatalf("expected enablePrefixCaching=%v %s, got %#v", want, where, md.Spec.Engine.EnablePrefixCaching)
	}
}

func typedFinalizerUpdatePayload(t *testing.T, md *airunwayv1alpha1.ModelDeployment) map[string]any {
	t.Helper()
	md.Finalizers = []string{FinalizerName}
	wire, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	var update map[string]any
	if err := json.Unmarshal(wire, &update); err != nil {
		t.Fatal(err)
	}
	return update
}

func assertNestedPrefixCachingBool(t *testing.T, raw map[string]any, want bool, where string) {
	t.Helper()
	got, present, err := unstructured.NestedBool(raw, "spec", "engine", "enablePrefixCaching")
	if err != nil {
		t.Fatal(err)
	}
	if !present || got != want {
		t.Fatalf("expected serialized enablePrefixCaching=%v %s, present=%v value=%v", want, where, present, got)
	}
}

func TestPrefixCacheFalseSurvivesTypedUpdateAndDisablesAllWorkers(t *testing.T) {
	schema := prefixCacheSchema(t)

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newTestMD("prefix-cache-false", "default"))
	if err != nil {
		t.Fatal(err)
	}

	if err := unstructured.SetNestedField(raw, false, "spec", "engine", "enablePrefixCaching"); err != nil {
		t.Fatal(err)
	}
	structuraldefaulting.Default(raw, schema)

	before := mustModelDeployment(t, raw)
	assertPrefixCachingValue(t, &before, false, "before update")

	update := typedFinalizerUpdatePayload(t, &before)
	assertNestedPrefixCachingBool(t, update, false, "before re-defaulting")

	structuraldefaulting.Default(update, schema)
	assertNestedPrefixCachingBool(t, update, false, "after re-defaulting")

	after := mustModelDeployment(t, update)
	assertPrefixCachingValue(t, &after, false, "after update")

	assertPrefixCacheFlags(t, &before, false, true)
	assertPrefixCacheFlags(t, &after, false, true)
}

func TestPrefixCacheOmittedDefaultsTrueAndExplicitTrueStaysEnabled(t *testing.T) {
	schema := prefixCacheSchema(t)

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newTestMD("prefix-cache-defaulted", "default"))
	if err != nil {
		t.Fatal(err)
	}
	structuraldefaulting.Default(raw, schema)

	var defaulted airunwayv1alpha1.ModelDeployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &defaulted); err != nil {
		t.Fatal(err)
	}
	if defaulted.Spec.Engine.EnablePrefixCaching == nil || !*defaulted.Spec.Engine.EnablePrefixCaching {
		t.Fatalf("expected omitted field to default true, got %#v", defaulted.Spec.Engine.EnablePrefixCaching)
	}

	explicit := newTestMD("prefix-cache-true", "default")
	explicit.Spec.Engine.EnablePrefixCaching = boolPtr(true)

	assertPrefixCacheFlags(t, &defaulted, true, false)
	assertPrefixCacheFlags(t, explicit, true, false)
}
