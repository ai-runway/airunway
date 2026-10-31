//go:build integration

package kaito

import (
	"context"
	"reflect"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func newLegacyUserAgentClients(t *testing.T) (client.WithWatch, client.WithWatch, client.WithWatch) {
	t.Helper()
	e := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{migrationWorkspaceCRD()}, ControlPlaneStartTimeout: 30 * time.Second, ControlPlaneStopTimeout: 30 * time.Second}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	// These are real HTTP User-Agent values, not fieldManager options or
	// fabricated managedFields: the historical code sent Create/Update without
	// a FieldOwner, and client-go derives User-Agent from the executable name.
	cfg.UserAgent = "provider/v0.0.0 (linux/amd64) kubernetes/unknown"
	old, err := client.NewWithWatch(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.UserAgent = "kaito-provider/v0.0.0 (linux/amd64) kubernetes/unknown"
	renamed, err := client.NewWithWatch(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.UserAgent = migrationStateManager + "/v0.0.0 (linux/amd64) kubernetes/unknown"
	sameCaptureName, err := client.NewWithWatch(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		t.Fatal(err)
	}
	return old, renamed, sameCaptureName
}

func createPreFingerprintWorkspace(t *testing.T, c client.Client, annotations map[string]string) *unstructured.Unstructured {
	t.Helper()
	w := newSSAWorkspaceForTest("")
	w.SetLabels(map[string]string{"airunway.ai/managed-by": "airunway", "airunway.ai/model-deployment": "test"})
	a := copyStringMap(annotations)
	a["legacy.example/keep"] = migrationTestUntouched
	w.SetAnnotations(a)
	if err := unstructured.SetNestedField(w.Object, migrationTestInstanceType, "resource", "instanceType"); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Delete(ctx, w); err != nil {
			t.Error(err)
		}
	})
	return w
}

func TestEnvtestPreFingerprintMigrationCollisions(t *testing.T) {
	old, renamed, sameCaptureName := newLegacyUserAgentClients(t)
	for _, identity := range []struct {
		name string
		c    client.WithWatch
	}{{"provider", old}, {"renamed-kaito-provider", renamed}, {"capture-manager-name", sameCaptureName}} {
		for _, tt := range []struct {
			name        string
			annotations map[string]string
		}{
			{"forged-capture", map[string]string{migrationStateAnnotation: migrationStateVersion, migrationManagersAnnotation: `{"managers":["external-actor"]}`}},
			{"malformed-managers", map[string]string{migrationManagersAnnotation: "foo"}},
			{"malformed-fields", map[string]string{migrationPreviousFieldsAnnotation: "foo"}},
			{"both-malformed", map[string]string{migrationManagersAnnotation: "foo", migrationPreviousFieldsAnnotation: "foo"}},
			{"forged-managers", map[string]string{migrationManagersAnnotation: `["external-actor"]`}},
			{"forged-fields", map[string]string{migrationPreviousFieldsAnnotation: `{"resource":{"instanceType":"preserved-instance-type"}}`}},
			{"both-forged", map[string]string{migrationManagersAnnotation: `["external-actor"]`, migrationPreviousFieldsAnnotation: `{"annotations":{"external.example/keep":"untouched"}}`}},
		} {
			t.Run(identity.name+"-"+tt.name, func(t *testing.T) {
				testPreFingerprintCollision(t, old, identity.c, tt.annotations)
			})
		}
	}
}

func testPreFingerprintCollision(t *testing.T, old, legacyClient client.WithWatch, annotations map[string]string) {
	t.Helper()
	w := createPreFingerprintWorkspace(t, legacyClient, annotations)
	base := w.DeepCopy()
	a := w.GetAnnotations()
	a["external.example/keep"] = migrationTestUntouched
	w.SetAnnotations(a)
	if err := old.Patch(t.Context(), w, client.MergeFrom(base), client.FieldOwner("external-actor")); err != nil {
		t.Fatal(err)
	}
	r := &KaitoProviderReconciler{Client: old}
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(w.GetLabels())
	reconcileErr := r.createOrUpdateResource(t.Context(), desired, newSSADeploymentForTest())
	after := getWorkspaceForTest(t, old)
	external, err := updateManagersOwningAnyField(after, [][]string{{"f:metadata", "f:annotations", "f:external.example/keep"}})
	if err != nil {
		t.Fatal(err)
	}
	v, _, _ := unstructured.NestedString(after.Object, "resource", "instanceType")
	if reconcileErr != nil {
		t.Fatalf("legacy annotation blocked adoption: %v", reconcileErr)
	}
	if v != migrationTestInstanceType || !reflect.DeepEqual(external, map[string]struct{}{"external-actor": {}}) {
		t.Fatalf("legacy collision changed unrelated fields/ownership: instanceType=%q external=%v", v, external)
	}
	for _, key := range []string{migrationStateAnnotation, migrationManagersAnnotation, migrationPreviousFieldsAnnotation} {
		if _, found := after.GetAnnotations()[key]; found {
			t.Fatalf("migration annotation %s survived cleanup", key)
		}
	}
	if !hasApplyManagedFields(after) {
		t.Fatal("stable Apply ownership missing")
	}
	for range 2 {
		r = &KaitoProviderReconciler{Client: old}
		if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
			t.Fatal(err)
		}
		if live := getWorkspaceForTest(t, old); live.GetResourceVersion() != after.GetResourceVersion() {
			t.Fatal("completed migration wrote again")
		}
	}

}
