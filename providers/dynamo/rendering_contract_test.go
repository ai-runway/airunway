package dynamo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

type releasedContract struct {
	schema    *structuralschema.Structural
	validator apivalidation.SchemaValidator
	cel       *cel.Validator
}

func readReleasedContract(t *testing.T, release, resource, apiVersion string) releasedContract {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("testdata/compatibility/%s-%s-%s.json", release, resource, apiVersion))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Release    string                          `json:"release"`
		APIVersion string                          `json:"apiVersion"`
		Schema     apiextensionsv1.JSONSchemaProps `json:"schema"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Release != release || fixture.APIVersion != apiVersion {
		t.Fatal("wrong released contract fixture")
	}
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&fixture.Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	validator, _, err := apivalidation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	return releasedContract{structural, validator, cel.NewValidator(structural, true, 10000000)}
}

func (c releasedContract) errors(obj *unstructured.Unstructured) []string {
	errors := []string{}
	for _, e := range apivalidation.ValidateCustomResource(field.NewPath("resource"), obj.Object, c.validator) {
		errors = append(errors, e.Error())
	}
	if c.cel != nil {
		violations, _ := c.cel.Validate(context.Background(), field.NewPath("resource"), c.schema, obj.Object, nil, 10000000)
		for _, e := range violations {
			errors = append(errors, e.Error())
		}
	}
	// OpenAPI validation alone permits unknown properties which Kubernetes would
	// silently prune. Fail on every pruned field, including nested Pod fields.
	unknown := pruning.PruneWithOptions(obj.DeepCopy().Object, c.schema, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	for _, p := range unknown {
		errors = append(errors, "field would be pruned: "+p)
	}
	return errors
}
func assertContract(t *testing.T, c releasedContract, obj *unstructured.Unstructured) {
	t.Helper()
	if errs := c.errors(obj); len(errs) > 0 {
		t.Fatalf("released contract rejected rendering: %v", errs)
	}
}
func setRenderingOverrides(t *testing.T, md *api.ModelDeployment, overrides map[string]any) {
	t.Helper()
	data, err := json.Marshal(overrides)
	if err != nil {
		t.Fatal(err)
	}
	md.Spec.Provider = &api.ProviderSpec{Name: "dynamo", Overrides: &runtime.RawExtension{Raw: data}}
}
func typedRenderingMD(t *testing.T) *api.ModelDeployment {
	t.Helper()
	md := newTestMD("auto-model", "models")
	md.Spec.Resources = nil
	setRenderingOverrides(t, md, map[string]any{"deploymentMode": "intent", "intent": map[string]any{
		"hardware": map[string]any{"totalGpus": 4, "gpuSku": "h100_sxm", "vramMb": 80000, "numGpusPerNode": 4},
		"workload": map[string]any{"isl": 1024, "osl": 256, "requestRate": 1.5},
		"sla":      map[string]any{"ttft": 1000, "itl": 50},
	}})
	return md
}

func TestReleasedDynamoRenderingContracts(t *testing.T) {
	for _, target := range []struct{ release, api string }{{"v1.1.1", "v1alpha1"}, {"v1.5.0", "v1beta1"}} {
		t.Run(target.release, func(t *testing.T) {
			contract := readReleasedContract(t, target.release, "dynamographdeployments", target.api)
			for _, engine := range []api.EngineType{api.EngineTypeVLLM, api.EngineTypeSGLang, api.EngineTypeTRTLLM} {
				for _, disagg := range []bool{false, true} {
					for _, gateway := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/disagg=%v/gateway=%v", engine, disagg, gateway), func(t *testing.T) {
							md := newTestMD("model", "models")
							md.Spec.Engine.Type = engine
							md.Spec.Gateway = &api.GatewaySpec{Enabled: &gateway}
							md.Spec.Secrets = &api.SecretsSpec{HuggingFaceToken: "custom-token"}
							md.Spec.NodeSelector = map[string]string{"accelerator": "nvidia"}
							md.Spec.Tolerations = []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
							if disagg {
								md.Spec.Serving = &api.ServingSpec{Mode: api.ServingModeDisaggregated}
								md.Spec.Scaling = &api.ScalingSpec{
									Prefill: &api.ComponentScalingSpec{Replicas: 1, GPU: &api.GPUSpec{Count: 2}}, Decode: &api.ComponentScalingSpec{Replicas: 2, GPU: &api.GPUSpec{Count: 1}},
								}
							}
							objects, err := NewTransformer().TransformForVersion(context.Background(), md, target.api, target.release)
							if err != nil {
								t.Fatal(err)
							}
							assertContract(t, contract, objects[0])
						})
					}
				}
			}
			t.Run("legacy-multinode-overrides", func(t *testing.T) {
				md := newTestMD("multinode", "models")
				md.Spec.Engine.Type = api.EngineTypeVLLM
				md.Spec.Engine.Args = map[string]string{"tensor-parallel-size": "2", "pipeline-parallel-size": "2"}
				setRenderingOverrides(t, md, map[string]any{"spec": map[string]any{"services": map[string]any{"VllmWorker": map[string]any{"multinode": map[string]any{"nodeCount": int64(2)}}}}})
				objects, err := NewTransformer().TransformForVersion(context.Background(), md, target.api, target.release)
				if err != nil {
					t.Fatal(err)
				}
				assertContract(t, contract, objects[0])
				if target.api == "v1beta1" {
					components := objects[0].Object["spec"].(map[string]any)["components"].([]any)
					found := false
					for _, raw := range components {
						c := raw.(map[string]any)
						if c["name"] == "VllmWorker" {
							found = c["multinode"] != nil
						}
					}
					if !found {
						t.Fatal("multinode intent lost during alpha-to-beta conversion")
					}
				}
			})

			t.Run("contract-negative-controls", func(t *testing.T) {
				md := newTestMD("model", "models")
				objs, err := NewTransformer().TransformForVersion(context.Background(), md, target.api, target.release)
				if err != nil {
					t.Fatal(err)
				}
				bad := objs[0].DeepCopy()
				bad.Object["spec"].(map[string]any)["notARealField"] = true
				if len(contract.errors(bad)) == 0 {
					t.Fatal("unknown field was accepted")
				}
				bad = objs[0].DeepCopy()
				bad.Object["spec"].(map[string]any)["backendFramework"] = "invalid"
				if len(contract.errors(bad)) == 0 {
					t.Fatal("invalid backend enum was accepted")
				}
				if target.api == "v1beta1" {
					bad = objs[0].DeepCopy()
					components := bad.Object["spec"].(map[string]any)["components"].([]any)
					components = append(components, components[0])
					bad.Object["spec"].(map[string]any)["components"] = components
					if len(contract.errors(bad)) == 0 {
						t.Fatal("duplicate component escaped CEL validation")
					}
				}
			})
			requestContract := readReleasedContract(t, target.release, "dynamographdeploymentrequests", "v1beta1")
			md := typedRenderingMD(t)
			objects, err := NewTransformer().TransformForVersion(context.Background(), md, target.api, target.release)
			if err != nil {
				t.Fatal(err)
			}
			assertContract(t, requestContract, objects[0])
			spec := objects[0].Object["spec"].(map[string]any)
			if spec["autoApply"] != true || spec["searchStrategy"] != "rapid" {
				t.Fatalf("incorrect typed intent: %#v", spec)
			}
			if spec["image"] != "nvcr.io/nvidia/ai-dynamo/dynamo-planner:"+target.release[1:] {
				t.Fatalf("wrong profiler image: %v", spec["image"])
			}
			if _, ok := spec["overrides"]; ok {
				t.Fatal("typed intent must not guess generated topology")
			}
		})
	}
}
