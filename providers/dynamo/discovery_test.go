package dynamo

import (
	"context"
	"strings"
	"testing"

	api "github.com/ai-runway/airunway/controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func dynamoMapper(versions ...string) meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, version := range versions {
		mapper.Add(schema.GroupVersionKind{Group: DynamoAPIGroup, Version: version, Kind: DynamoGraphDeploymentKind}, meta.RESTScopeNamespace)
	}
	return mapper
}

func TestDiscoverySelectsDGDContractWithoutDGDR(t *testing.T) {
	for _, version := range []string{DynamoAPIVersion, dynamoBetaVersion} {
		t.Run(version, func(t *testing.T) {
			runtimeVersion := "1.1.1"
			if version == dynamoBetaVersion {
				runtimeVersion = "1.5.0"
			}
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(operatorRuntimeFixtures(runtimeVersion, "")...).WithRESTMapper(dynamoMapper(version)).Build()
			r := NewDynamoProviderReconciler(c, newScheme(), "")
			md := newMDForController("test", "default")
			rendered, err := r.renderResources(context.Background(), md)
			if err != nil {
				t.Fatal(err)
			}
			if rendered[0].GroupVersionKind().Version != version {
				t.Fatalf("wrong API %s", rendered[0].GetAPIVersion())
			}

			images := []string{}
			collectImages(rendered[0].Object["spec"], &images)
			foundRuntime := false
			for _, image := range images {
				if strings.Contains(image, "/vllm-runtime:") {
					foundRuntime = true
					if !strings.HasSuffix(image, ":"+runtimeVersion) {
						t.Fatalf("wrong runtime %s", image)
					}
				}
			}
			if !foundRuntime {
				t.Fatal("runtime image missing")
			}
		})
	}
}

func TestExistingManualDeploymentKeepsAPIAndRuntime(t *testing.T) {
	ctx := context.Background()
	md := newMDForController("test", "default")
	old, err := NewTransformer().TransformForVersion(ctx, md, DynamoAPIVersion, "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	old[0].SetUID("existing-uid")
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRESTMapper(dynamoMapper(dynamoBetaVersion, DynamoAPIVersion)).WithObjects(old[0]).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	rendered, err := r.renderResources(ctx, md)
	if err != nil {
		t.Fatal(err)
	}
	if rendered[0].GetAPIVersion() != old[0].GetAPIVersion() {
		t.Fatal("existing alpha object migrated automatically")
	}
	if existingRuntimeVersion(rendered[0]) != "1.1.1" {
		t.Fatal("Runway upgrade changed runtime version")
	}
	if err := r.createOrUpdateResource(ctx, rendered[0], md); err != nil {
		t.Fatal(err)
	}
	got := old[0].DeepCopy()
	_ = c.Get(ctx, client.ObjectKeyFromObject(got), got)
	before := got.GetResourceVersion()
	// Subsequent provider reconciliation may add metadata but cannot roll out new defaults.
	rendered, err = r.renderResources(ctx, md)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.createOrUpdateResource(ctx, rendered[0], md); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(got), got)
	if got.GetResourceVersion() != before || got.GetUID() != "existing-uid" {
		t.Fatal("unchanged manual deployment was mutated")
	}
}

func TestNativeBetaPreservesImagesAndListShape(t *testing.T) {
	md := newMDForController("test", "default")
	rendered, err := NewTransformer().TransformForVersion(context.Background(), md, dynamoBetaVersion, "1.5.0")
	if err != nil {
		t.Fatal(err)
	}
	existing := rendered[0].DeepCopy()
	components, _, _ := unstructured.NestedSlice(existing.Object, "spec", "components")
	replaced := false
	for _, raw := range components {
		component := raw.(map[string]any)
		if component["type"] != "worker" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(component, "podTemplate", "spec", "containers")
		for _, raw := range containers {
			container := raw.(map[string]any)
			if container["name"] == "main" {
				container["image"] = "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.5.0@sha256:old-pinned"
				replaced = true
			}
		}
		_ = unstructured.SetNestedSlice(component, containers, "podTemplate", "spec", "containers")
	}
	_ = unstructured.SetNestedSlice(existing.Object, components, "spec", "components")
	if !replaced {
		t.Fatal("fixture did not find worker container")
	}
	preserveRuntimeImages(md, existing, rendered[0])
	if _, found, _ := unstructured.NestedSlice(rendered[0].Object, "spec", "components"); !found {
		t.Fatal("beta component list corrupted into map")
	}
	images := []string{}
	collectImages(rendered[0].Object["spec"], &images)
	found := false
	for _, image := range images {
		if strings.Contains(image, "sha256:old-pinned") {
			found = true
		}
	}
	if !found {
		t.Fatal("pinned runtime image overwritten")
	}
}

func TestReadDGDThroughAlternateAPIKeepsUID(t *testing.T) {
	md := newMDForController("test", "default")
	dgd := newDynamoResource(dynamoBetaVersion, DynamoGraphDeploymentKind, md.Name, md.Namespace)
	dgd.SetUID("workload-uid")
	ref := resourceReference(dgd)
	ref.APIVersion = DynamoAPIGroup + "/" + DynamoAPIVersion
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(dgd).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	found, err := r.findDGD(context.Background(), md.Namespace, md.Name, ref)
	if err != nil || found == nil || found.GetUID() != dgd.GetUID() {
		t.Fatalf("alternate API lookup failed: %v", err)
	}
}

func TestProviderReadyWithBetaOnlyAndNoDGDR(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	discovery := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	discovery.Resources = []*metav1.APIResourceList{{GroupVersion: DynamoAPIGroup + "/" + dynamoBetaVersion, APIResources: []metav1.APIResource{{Name: dynamoGraphDeploymentResource, Kind: DynamoGraphDeploymentKind}}}}
	manager := NewProviderConfigManager(c, discovery)
	if !manager.checkBackendCRDInstalled() {
		t.Fatal("missing alpha/DGDR disabled manual beta deployments")
	}
}

func TestServingConditionRejectsStaleGeneration(t *testing.T) {
	dgd := newDynamoResource(dynamoBetaVersion, DynamoGraphDeploymentKind, "test", "default")
	dgd.SetGeneration(3)
	dgd.Object["status"] = map[string]any{"state": "successful", "observedGeneration": int64(2), "components": map[string]any{"worker": map[string]any{"replicas": int64(1), "availableReplicas": int64(1)}}}
	result, err := NewStatusTranslator().TranslateStatus(dgd)
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase == api.DeploymentPhaseRunning {
		t.Fatal("stale status reported running")
	}
}

func TestManualSpecUpdatePreservesOperatorMetadata(t *testing.T) {
	md := newMDForController("test", "default")
	resources, err := NewTransformer().TransformForVersion(context.Background(), md, DynamoAPIVersion, "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	existing := resources[0]
	existing.SetUID("existing-uid")
	existing.SetFinalizers([]string{"nvidia.com/operator-cleanup"})
	labels := existing.GetLabels()
	labels["operator-added"] = "keep"
	existing.SetLabels(labels)
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(existing).Build()
	r := NewDynamoProviderReconciler(c, newScheme(), "")
	md.Spec.Resources.GPU.Count = 2
	desired, err := r.renderResources(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.createOrUpdateResource(context.Background(), desired[0], md); err != nil {
		t.Fatal(err)
	}
	got := existing.DeepCopy()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(got), got); err != nil {
		t.Fatal(err)
	}
	if len(got.GetFinalizers()) != 1 || got.GetFinalizers()[0] != "nvidia.com/operator-cleanup" || got.GetLabels()["operator-added"] != "keep" {
		t.Fatal("operator metadata was lost during spec update")
	}
}
