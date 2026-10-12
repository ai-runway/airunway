//go:build integration

package kaito

import (
	"context"
	"errors"
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	selectorTestExternalManager = "external-selector"
	selectorTestSuccess         = "success"
)

// KAITO v0.10.0 config/crd/bases/kaito.sh_workspaces.yaml: labelSelector is
// atomic, as are its matchExpressions and values lists. Descriptions omitted.
func migrationSelectorSchema() apiextensionsv1.JSONSchemaProps {
	atomic := "atomic"
	return apiextensionsv1.JSONSchemaProps{
		Type: "object", XMapType: &atomic,
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"matchLabels": {Type: "object", AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
				Allows: true, Schema: &apiextensionsv1.JSONSchemaProps{Type: "string"},
			}},
			"matchExpressions": {Type: "array", XListType: &atomic, Items: &apiextensionsv1.JSONSchemaPropsOrArray{
				Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", Required: []string{"key", "operator"},
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"key": {Type: "string"}, "operator": {Type: "string"},
						"values": {Type: "array", XListType: &atomic, Items: &apiextensionsv1.JSONSchemaPropsOrArray{
							Schema: &apiextensionsv1.JSONSchemaProps{Type: "string"},
						}},
					},
				},
			}},
		},
	}
}

func newSelectorEnvtestClient(t *testing.T) client.WithWatch {
	t.Helper()
	crd := migrationWorkspaceCRD()
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	resource := schema.Properties["resource"]
	resource.Required = []string{"labelSelector"}
	schema.Properties["resource"] = resource
	return newMigrationEnvtestClientForCRD(t, crd)
}

func migrationSelectorForTest(stale bool) map[string]any {
	selector := map[string]any{"matchLabels": map[string]any{"kubernetes.io/os": testLinuxOS}}
	if stale {
		selector["matchLabels"].(map[string]any)["example.com/old-pool"] = "retired"
		selector["matchExpressions"] = []any{map[string]any{
			"key": "example.com/old-zone", "operator": "In", "values": []any{"retired"},
		}}
	}
	return selector
}

func setMigrationSelectorForTest(t *testing.T, workspace *unstructured.Unstructured, selector map[string]any) {
	t.Helper()
	if err := unstructured.SetNestedMap(workspace.Object, selector, "resource", "labelSelector"); err != nil {
		t.Fatal(err)
	}
}

func newSelectorMigrationWorkspace(t *testing.T, c client.WithWatch, fingerprint bool) *unstructured.Unstructured {
	t.Helper()
	original := newSSAWorkspaceForTest("")
	original.SetLabels(map[string]string{"airunway.ai/managed-by": "airunway", "airunway.ai/model-deployment": "test"})
	setMigrationSelectorForTest(t, original, migrationSelectorForTest(true))
	if fingerprint {
		if err := setLastAppliedManagedFields(original); err != nil {
			t.Fatal(err)
		}
	}
	if err := unstructured.SetNestedField(original.Object, migrationTestInstanceType, "resource", "instanceType"); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), original, client.FieldOwner(migrationTestLegacyManager)); err != nil {
		t.Fatal(err)
	}
	cleanupCaptureWorkspace(t, c, original)
	base := original.DeepCopy()
	annotations := copyStringMap(original.GetAnnotations())
	annotations["external.example/keep"] = migrationTestUntouched
	original.SetAnnotations(annotations)
	if err := c.Patch(t.Context(), original, client.MergeFrom(base), client.FieldOwner("external-actor")); err != nil {
		t.Fatal(err)
	}
	assertAtomicSelectorForTest(t, original, migrationTestLegacyManager, metav1.ManagedFieldsOperationUpdate, migrationSelectorForTest(true))
	return original
}

func assertAtomicSelectorForTest(t *testing.T, live *unstructured.Unstructured, manager string, operation metav1.ManagedFieldsOperationType, want map[string]any) {
	t.Helper()
	selector, found, err := unstructured.NestedMap(live.Object, "resource", "labelSelector")
	if err != nil || !found || !reflect.DeepEqual(selector, want) {
		t.Fatalf("selector pruned or changed: got=%v want=%v err=%v", selector, want, err)
	}
	fields, err := managedFieldsForManager(live, manager, operation)
	if err != nil {
		t.Fatal(err)
	}
	owned, found, err := unstructured.NestedMap(fields, "f:resource", "f:labelSelector")
	if err != nil || !found || len(owned) != 0 {
		t.Fatalf("%s does not own one atomic selector: %v found=%v err=%v", manager, owned, found, err)
	}
}

func TestEnvtestAtomicSelectorMigration(t *testing.T) {
	c := newSelectorEnvtestClient(t)
	for _, fingerprint := range []bool{false, true} {
		name := "without-fingerprint"
		if fingerprint {
			name = "with-fingerprint"
		}
		t.Run(name, func(t *testing.T) {
			for _, boundary := range []string{selectorTestSuccess, migrationTestSeed, "managed-fields", migrationTestStableApply} {
				t.Run(boundary, func(t *testing.T) { testAtomicSelectorMigration(t, c, fingerprint, boundary) })
			}
		})
	}
}

func testAtomicSelectorMigration(t *testing.T, c client.WithWatch, fingerprint bool, boundary string) {
	t.Helper()
	original := newSelectorMigrationWorkspace(t, c, fingerprint)
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(original.GetLabels())
	setMigrationSelectorForTest(t, desired, migrationSelectorForTest(false))
	if boundary != selectorTestSuccess {
		failAtomicSelectorMigration(t, c, desired, boundary)
	}
	assertCaptureCompleted(t, c, desired)
	assertAtomicSelectorForTest(t, getWorkspaceForTest(t, c), FieldManager, metav1.ManagedFieldsOperationApply, migrationSelectorForTest(false))
	// The real CRD requires labelSelector. Remove all remaining constraints
	// by applying an empty selector, not by deleting the required field.
	setMigrationSelectorForTest(t, desired, map[string]any{})
	assertCaptureCompleted(t, c, desired)
	assertAtomicSelectorForTest(t, getWorkspaceForTest(t, c), FieldManager, metav1.ManagedFieldsOperationApply, map[string]any{})
}

func failAtomicSelectorMigration(t *testing.T, c client.WithWatch, desired *unstructured.Unstructured, boundary string) {
	t.Helper()
	wantErr := errors.New("interrupted atomic selector " + boundary)
	failures := 0
	faulty := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		options := (&client.PatchOptions{}).ApplyOptions(opts)
		if options.Force != nil && *options.Force {
			t.Fatal("selector migration requested force ownership")
		}
		if captureRequestBoundary(getWorkspaceForTest(t, cl), obj, patch, opts) == boundary {
			failures++
			return wantErr
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	var record string
	for attempt := range 2 {
		r := &KaitoProviderReconciler{Client: faulty}
		if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); !errors.Is(err, wantErr) || failures != attempt+1 {
			t.Fatalf("missed selector %s attempt%d: failures=%d err=%v", boundary, attempt, failures, err)
		}
		live := getWorkspaceForTest(t, c)
		assertCaptureExternalState(t, live)
		if attempt == 0 {
			record = live.GetAnnotations()[migrationManagersAnnotation]
		} else if record != live.GetAnnotations()[migrationManagersAnnotation] {
			t.Fatal("selector retry changed captured managers")
		}
	}
}

func TestEnvtestAtomicSelectorForeignOwnership(t *testing.T) {
	c := newSelectorEnvtestClient(t)
	original := newSelectorMigrationWorkspace(t, c, true)
	r := &KaitoProviderReconciler{Client: c}
	claim := workspaceConfigurationWithIdentity(map[string]any{}, original)
	setMigrationSelectorForTest(t, claim, migrationSelectorForTest(true))
	if _, err := r.applyWorkspaceAs(t.Context(), claim, selectorTestExternalManager, original.GetResourceVersion()); err != nil {
		t.Fatal(err)
	}
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(original.GetLabels())
	setMigrationSelectorForTest(t, desired, migrationSelectorForTest(false))
	if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); !isFieldManagerConflict(err) {
		t.Fatalf("expected atomic selector ownership conflict: %v", err)
	}
	assertAtomicSelectorForTest(t, getWorkspaceForTest(t, c), selectorTestExternalManager, metav1.ManagedFieldsOperationApply, migrationSelectorForTest(true))
	setMigrationSelectorForTest(t, desired, migrationSelectorForTest(true))
	assertCaptureCompleted(t, c, desired)
	before := getWorkspaceForTest(t, c)
	assertAtomicSelectorForTest(t, before, FieldManager, metav1.ManagedFieldsOperationApply, migrationSelectorForTest(true))
	assertAtomicSelectorForTest(t, before, selectorTestExternalManager, metav1.ManagedFieldsOperationApply, migrationSelectorForTest(true))
	setMigrationSelectorForTest(t, desired, map[string]any{})
	if err := r.createOrUpdateResource(t.Context(), desired, newSSADeploymentForTest()); !isFieldManagerConflict(err) {
		t.Fatalf("expected stable selector removal conflict: %v", err)
	}
	if !reflect.DeepEqual(getWorkspaceForTest(t, c).Object, before.Object) {
		t.Fatal("conflicting stable selector removal changed live state")
	}
}
