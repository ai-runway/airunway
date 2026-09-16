//go:build integration

package kaito

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	migrationTestFinalFingerprint = "final-fingerprint"
	migrationTestPreparation      = "preparation"
	migrationTestLegacyManager    = "legacy-kaito-provider"
	migrationTestSeed             = "seed"
	migrationTestStableApply      = "stable-apply"
	migrationTestInstanceType     = "preserved-instance-type"
	migrationTestUntouched        = "untouched"
	migrationTestMove             = "move"
	migrationTestFinish           = "preservation-finish"
)

// TestEnvtestMigrationRetry exercises real SSA managedFields. Only the selected
// failed request is intercepted; successful writes and all reads use the API server.
func TestEnvtestMigrationRetry(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, boundary := range []string{migrationTestSeed, "managed-fields", migrationTestStableApply} {
		t.Run(boundary, func(t *testing.T) {
			testMigrationRetryAtBoundary(t, c, boundary, nil)
		})
	}
}

// Legacy podTemplate annotations must never become trusted migration state,
// even when their contents look like legitimate manager names or field history.
func TestEnvtestLegacyMigrationAnnotationCollisions(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, tt := range []struct {
		name        string
		annotations map[string]string
	}{
		{"malformed-managers", map[string]string{migrationManagersAnnotation: "foo"}},
		{"malformed-fields", map[string]string{migrationPreviousFieldsAnnotation: "foo"}},
		{"both-malformed", map[string]string{
			migrationManagersAnnotation: "foo", migrationPreviousFieldsAnnotation: "foo",
		}},
		{"forged-managers", map[string]string{migrationManagersAnnotation: `["external-actor"]`}},
		{"forged-fields", map[string]string{
			migrationPreviousFieldsAnnotation: `{"resource":{"instanceType":"preserved-instance-type"}}`,
		}},
		{"both-forged", map[string]string{
			migrationManagersAnnotation:       `["external-actor"]`,
			migrationPreviousFieldsAnnotation: `{"annotations":{"external.example/keep":"untouched"}}`,
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, boundary := range []string{migrationTestSeed, "managed-fields", migrationTestStableApply} {
				t.Run(boundary, func(t *testing.T) {
					testMigrationRetryAtBoundary(t, c, boundary, tt.annotations)
				})
			}
		})
	}
}

// Once marking removes last-applied, corrupt protocol state must still fail
// closed. It cannot be treated as a legacy user annotation and rediscovered.
func TestEnvtestCorruptPendingMigration(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, key := range []string{migrationManagersAnnotation, migrationPreviousFieldsAnnotation} {
		t.Run(key, func(t *testing.T) {
			testCorruptPendingMigration(t, c, key)
		})
	}
}

func testCorruptPendingMigration(t *testing.T, c client.WithWatch, key string) {
	t.Helper()
	existing := createSplitOwnershipWorkspace(t, c, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Delete(ctx, existing); err != nil {
			t.Errorf("delete test Workspace: %v", err)
		}
	})
	r := &KaitoProviderReconciler{Client: c}
	marked, err := r.markLegacyWorkspaceMigration(t.Context(), existing, newSSADeploymentForTest().UID, map[string]struct{}{FieldManager: {}, migrationTestLegacyManager: {}})
	if err != nil {
		t.Fatal(err)
	}
	assertPendingMigration(t, marked)
	base := marked.DeepCopy()
	annotations := marked.GetAnnotations()
	annotations[key] = "foo"
	marked.SetAnnotations(annotations)
	if err := c.Patch(t.Context(), marked, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	writes := 0
	observed := interceptor.NewClient(c, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object,
			patch client.Patch, opts ...client.PatchOption) error {
			writes++
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	for range 2 {
		r = &KaitoProviderReconciler{Client: observed}
		err := r.createOrUpdateResource(t.Context(), newSSAWorkspaceForTest(""), newSSADeploymentForTest())
		if err == nil || !strings.Contains(err.Error(), "annotation") {
			t.Fatalf("corrupt pending state did not fail closed: %v", err)
		}
	}
	if writes != 0 || !reflect.DeepEqual(getWorkspaceForTest(t, c).Object, marked.Object) {
		t.Fatal("corrupt pending state triggered mutation")
	}
}

func testMigrationRetryAtBoundary(t *testing.T, c client.WithWatch, boundary string, annotations map[string]string) {
	t.Helper()
	existing := createSplitOwnershipWorkspace(t, c, annotations)
	originalFingerprint := existing.GetAnnotations()[lastAppliedWorkspaceAnnotation]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Delete(ctx, existing); err != nil {
			t.Errorf("delete test Workspace: %v", err)
		}
	})
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(existing.GetLabels())
	wantErr := errors.New("injected " + boundary + " failure")
	failures := 0
	faulty := interceptor.NewClient(c, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object,
			patch client.Patch, opts ...client.PatchOption) error {
			if migrationRequestMatchesBoundary(boundary, patch, opts) {
				failures++
				return wantErr
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})

	var recordedManagers, recordedFields string
	for attempt := range 2 {
		// A new reconciler on each retry rules out in-memory recovery state.
		r := &KaitoProviderReconciler{Client: faulty}
		err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest())
		if !errors.Is(err, wantErr) || failures != attempt+1 {
			t.Fatalf("attempt %d did not reach %s: failures=%d err=%v", attempt, boundary, failures, err)
		}
		live := getWorkspaceForTest(t, c)
		annotations := assertPendingMigration(t, live)
		if annotations[migrationPreviousFieldsAnnotation] != originalFingerprint {
			t.Fatal("mark did not preserve the original legacy fingerprint")
		}
		if attempt == 0 {
			recordedManagers = annotations[migrationManagersAnnotation]
			recordedFields = annotations[migrationPreviousFieldsAnnotation]
		} else if annotations[migrationManagersAnnotation] != recordedManagers ||
			annotations[migrationPreviousFieldsAnnotation] != recordedFields {
			t.Fatal("retry replaced persisted migration state")
		}
		assertMigrationBoundary(t, live, boundary)
	}

	r := &KaitoProviderReconciler{Client: c}
	if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("resume migration: %v", err)
	}
	completed := getWorkspaceForTest(t, c)
	assertCompletedMigration(t, completed)
	for range 2 {
		if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
			t.Fatalf("reconcile completed migration: %v", err)
		}
		if live := getWorkspaceForTest(t, c); live.GetResourceVersion() != completed.GetResourceVersion() {
			t.Fatal("completed migration performed another API write")
		}
	}
	t.Logf("%s: captured managers survived two failures; stale field removed; external ownership retained; no-op verified",
		boundary)
}

func migrationRequestMatchesBoundary(boundary string, patch client.Patch, opts []client.PatchOption) bool {
	options := (&client.PatchOptions{}).ApplyOptions(opts)
	fail := boundary == migrationTestSeed && patch.Type() == types.ApplyPatchType &&
		options.FieldManager == preservedFieldsManager
	fail = fail || boundary == "managed-fields" && patch.Type() == types.JSONPatchType
	fail = fail || boundary == migrationTestStableApply && patch.Type() == types.ApplyPatchType &&
		options.FieldManager == FieldManager
	return fail
}

func assertPendingMigration(t *testing.T, live *unstructured.Unstructured) map[string]string {
	t.Helper()
	if hasApplyManagedFields(live) {
		t.Fatal("failure must precede stable Apply ownership")
	}
	annotations := live.GetAnnotations()
	if annotations[migrationPreviousFieldsAnnotation] == "" {
		t.Fatal("mark must preserve the old rendered fingerprint")
	}
	if _, found := annotations[lastAppliedWorkspaceAnnotation]; found {
		t.Fatal("mark must move the old last-applied annotation before the injected failure")
	}
	managers, pending, err := pendingMigrationManagers(live)
	wantManagers := map[string]struct{}{FieldManager: {}, migrationTestLegacyManager: {}}
	if err != nil || !pending || !reflect.DeepEqual(managers, wantManagers) {
		t.Fatalf("lost captured managers: got %v pending=%v err=%v", managers, pending, err)
	}
	rediscovered, err := legacyUpdateManagers(live, newSSADeploymentForTest().UID)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := rediscovered[migrationTestLegacyManager]; found {
		t.Fatal("fixture must make the original manager undiscoverable after marking")
	}
	return annotations
}

func assertMigrationBoundary(t *testing.T, live *unstructured.Unstructured, boundary string) {
	t.Helper()
	preserved, err := managedFieldsForManager(live, preservedFieldsManager, metav1.ManagedFieldsOperationApply)
	if err != nil {
		t.Fatal(err)
	}
	if (preserved != nil) != (boundary != migrationTestSeed) {
		t.Fatalf("unexpected preservation ownership at %s: %v", boundary, preserved)
	}
	legacyPresent := false
	for _, entry := range live.GetManagedFields() {
		if entry.Manager == migrationTestLegacyManager && entry.Operation == metav1.ManagedFieldsOperationUpdate {
			legacyPresent = true
		}
	}
	if legacyPresent != (boundary != migrationTestStableApply) {
		t.Fatalf("unexpected legacy Update ownership at %s: present=%v", boundary, legacyPresent)
	}
}

func assertCompletedMigration(t *testing.T, live *unstructured.Unstructured) {
	t.Helper()
	if _, found, err := unstructured.NestedString(live.Object, "inference", "preset", "accessMode"); err != nil || found {
		t.Fatalf("stale rendered field survived migration: found=%v err=%v", found, err)
	}
	for _, annotation := range []string{migrationManagersAnnotation, migrationPreviousFieldsAnnotation} {
		if _, found := live.GetAnnotations()[annotation]; found {
			t.Fatalf("migration did not clear %s", annotation)
		}
	}
	if !hasApplyManagedFields(live) || live.GetAnnotations()[lastAppliedWorkspaceAnnotation] == "" {
		t.Fatal("migration did not establish stable Apply and its fingerprint")
	}
	instanceType, _, err := unstructured.NestedString(live.Object, "resource", "instanceType")
	if err != nil || instanceType != migrationTestInstanceType {
		t.Fatalf("non-rendered legacy value lost: %q err=%v", instanceType, err)
	}
	if live.GetAnnotations()["external.example/keep"] != migrationTestUntouched {
		t.Fatal("external annotation changed")
	}
	owners, err := updateManagersOwningAnyField(live, [][]string{
		{"f:metadata", "f:annotations", "f:external.example/keep"},
	})
	if err != nil || !reflect.DeepEqual(owners, map[string]struct{}{"external-actor": {}}) {
		t.Fatalf("external annotation ownership changed: %v err=%v", owners, err)
	}
	for _, entry := range live.GetManagedFields() {
		if entry.Manager == migrationTestLegacyManager {
			t.Fatal("legacy manager retained ownership after completion")
		}
	}
}

func createSplitOwnershipWorkspace(
	t *testing.T, c client.Client, annotations map[string]string,
) *unstructured.Unstructured {
	t.Helper()
	existing := newSSAWorkspaceForTest(testPrivateAccessMode)
	existing.SetLabels(map[string]string{
		"airunway.ai/managed-by": "airunway", "airunway.ai/model-deployment": "test",
	})
	// Reproduce the legacy renderer/controller: both future reserved keys were
	// copied from podTemplate and included in its last-applied fingerprint.
	existing.SetAnnotations(copyStringMap(annotations))
	fingerprint, err := json.Marshal(map[string]any{
		"resource": existing.Object["resource"], "inference": existing.Object["inference"],
		"labels": existing.GetLabels(), "annotations": copyStringMap(annotations),
	})
	if err != nil {
		t.Fatal(err)
	}
	annotations = copyStringMap(annotations)
	annotations[lastAppliedWorkspaceAnnotation] = string(fingerprint)
	existing.SetAnnotations(annotations)
	// Model a non-rendered field included in Create ownership, as an admission
	// default would be. This test asserts preservation, not webhook execution.
	if err := unstructured.SetNestedField(existing.Object, migrationTestInstanceType, "resource", "instanceType"); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), existing, client.FieldOwner(migrationTestLegacyManager)); err != nil {
		t.Fatalf("create legacy Workspace: %v", err)
	}
	base := existing.DeepCopy()
	labels := existing.GetLabels()
	labels["airunway.ai/managed-by"] = "external"
	existing.SetLabels(labels)
	annotations = existing.GetAnnotations()
	annotations["external.example/keep"] = migrationTestUntouched
	existing.SetAnnotations(annotations)
	if err := c.Patch(t.Context(), existing, client.MergeFrom(base), client.FieldOwner("external-actor")); err != nil {
		t.Fatalf("split identity ownership: %v", err)
	}
	managers, err := legacyUpdateManagers(existing, newSSADeploymentForTest().UID)
	if err != nil || !reflect.DeepEqual(managers, map[string]struct{}{migrationTestLegacyManager: {}}) {
		t.Fatalf("initial last-applied owner discovery: %v err=%v", managers, err)
	}
	return existing
}

func newMigrationEnvtestClient(t *testing.T) client.WithWatch {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("integration tests require KUBEBUILDER_ASSETS pointing to envtest binaries")
	}
	testEnv := &envtest.Environment{
		CRDs:                     []*apiextensionsv1.CustomResourceDefinition{migrationWorkspaceCRD()},
		ControlPlaneStartTimeout: 30 * time.Second,
		ControlPlaneStopTimeout:  30 * time.Second,
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		t.Fatalf("create API client: %v", err)
	}
	return c
}

// Only the Workspace fields used by the migration scenarios are declared here.
// Granular objects and Kubernetes metadata use real structural-schema SSA rules;
// no KAITO webhook or inference/data-plane behavior is claimed by this fixture.
func migrationWorkspaceCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "workspaces.kaito.sh"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: KaitoAPIGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "workspaces", Singular: "workspace", Kind: WorkspaceKind, ListKind: "WorkspaceList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: KaitoAPIVersion, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"apiVersion": {Type: "string"}, "kind": {Type: "string"}, "metadata": {Type: "object"},
							"resource": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"count": {Type: "integer", Format: "int64"}, "instanceType": {Type: "string"},
							}},
							"inference": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"preset": {Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"name": {Type: "string"}, "accessMode": {Type: "string"},
								}},
							}},
						},
					},
				},
			}},
		},
	}
}
