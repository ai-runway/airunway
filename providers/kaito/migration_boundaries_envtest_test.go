//go:build integration

package kaito

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var captureTestBoundaries = []string{migrationTestPreparation, "capture", migrationTestMove, migrationTestSeed, "managed-fields", "preservation-handoff", migrationTestStableApply, "preservation-release", "cleanup", migrationTestFinish, migrationTestFinalFingerprint}

func captureRequestBoundary(live *unstructured.Unstructured, obj client.Object, patch client.Patch, opts []client.PatchOption) string {
	options := (&client.PatchOptions{}).ApplyOptions(opts)
	annotations := obj.GetAnnotations()
	_, current := annotations[lastAppliedWorkspaceAnnotation]
	_, record := annotations[migrationManagersAnnotation]
	state, _ := readWorkspaceMigrationState(live)
	switch patch.Type() {
	case types.JSONPatchType:
		return "managed-fields"
	case types.MergePatchType:
		if state == nil {
			return migrationTestPreparation
		}
		if record {
			return migrationTestMove
		}
		return "cleanup"
	case types.ApplyPatchType:
		switch options.FieldManager {
		case migrationStateManager:
			return "capture"
		case FieldManager:
			if current {
				return migrationTestFinalFingerprint
			}
			return migrationTestStableApply
		case preservedFieldsManager:
			return capturePreservationBoundary(live, state)
		}
	}
	return ""
}

func capturePreservationBoundary(live *unstructured.Unstructured, state *workspaceMigrationState) string {
	fields, _ := managedFieldsForManager(live, preservedFieldsManager, metav1.ManagedFieldsOperationApply)
	if fields == nil {
		return migrationTestSeed
	}
	if !hasApplyManagedFields(live) {
		return "preservation-handoff"
	}
	if state != nil {
		return "preservation-release"
	}
	return migrationTestFinish
}

func newCaptureBoundaryWorkspace(t *testing.T, c client.WithWatch) *unstructured.Unstructured {
	t.Helper()
	live := createSplitOwnershipWorkspace(t, c, map[string]string{
		migrationStateAnnotation:          migrationStateVersion,
		migrationManagersAnnotation:       `{"managers":["external-actor"]}`,
		migrationPreviousFieldsAnnotation: `{"resource":{"instanceType":"preserved-instance-type"}}`,
	})
	cleanupCaptureWorkspace(t, c, live)
	return live
}

func assertCaptureExternalState(t *testing.T, live *unstructured.Unstructured) {
	t.Helper()
	value, _, err := unstructured.NestedString(live.Object, "resource", "instanceType")
	if err != nil || value != migrationTestInstanceType {
		t.Fatalf("unrelated value lost: %q %v", value, err)
	}
	owners, err := updateManagersOwningAnyField(live, [][]string{{"f:metadata", "f:annotations", "f:external.example/keep"}})
	if err != nil || !reflect.DeepEqual(owners, map[string]struct{}{"external-actor": {}}) {
		t.Fatalf("foreign ownership changed: %v %v", owners, err)
	}
}

func assertCaptureCompleted(t *testing.T, c client.WithWatch, desired *unstructured.Unstructured) {
	t.Helper()
	r := &KaitoProviderReconciler{Client: c}
	if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
		t.Fatalf("resume capture: %v", err)
	}
	completed := getWorkspaceForTest(t, c)
	assertCompletedMigration(t, completed)
	if _, present := completed.GetAnnotations()[migrationStateAnnotation]; present {
		t.Fatal("capture anchor survived cleanup")
	}
	fields, err := managedFieldsForManager(completed, migrationStateManager, metav1.ManagedFieldsOperationApply)
	if err != nil || fields != nil {
		t.Fatalf("capture ownership survived cleanup: %v %v", fields, err)
	}
	for range 2 {
		r = &KaitoProviderReconciler{Client: c}
		if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); err != nil {
			t.Fatal(err)
		}
		if getWorkspaceForTest(t, c).GetResourceVersion() != completed.GetResourceVersion() {
			t.Fatal("completion was not a stable no-op")
		}
	}
}

func TestEnvtestCaptureFailureBoundaries(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, boundary := range captureTestBoundaries {
		t.Run(boundary, func(t *testing.T) { testCaptureFailureBoundary(t, c, boundary) })
	}
}

func testCaptureFailureBoundary(t *testing.T, c client.WithWatch, boundary string) {
	t.Helper()
	original := newCaptureBoundaryWorkspace(t, c)
	fingerprint := original.GetAnnotations()[lastAppliedWorkspaceAnnotation]
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(original.GetLabels())
	wantErr := errors.New("injected " + boundary)
	failures := 0
	faulty := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		options := (&client.PatchOptions{}).ApplyOptions(opts)
		if options.Force != nil && *options.Force {
			t.Fatal("migration requested force ownership")
		}
		if captureRequestBoundary(getWorkspaceForTest(t, cl), obj, patch, opts) == boundary {
			failures++
			return wantErr
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	var saved string
	for attempt := range 2 {
		r := &KaitoProviderReconciler{Client: faulty}
		err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest())
		if boundary == migrationTestFinish && attempt == 1 && err == nil {
			break
		}
		if !errors.Is(err, wantErr) || failures != attempt+1 {
			t.Fatalf("boundary %s attempt %d: failures=%d err=%v", boundary, attempt, failures, err)
		}
		live := getWorkspaceForTest(t, c)
		assertCaptureExternalState(t, live)
		record := assertCaptureFailureState(t, live, boundary, fingerprint)
		if attempt == 0 {
			saved = record
		} else if saved != record {
			t.Fatal("fresh-process retry replaced captured record")
		}

	}
	assertCaptureCompleted(t, c, desired)
}

func assertCaptureFailureState(t *testing.T, live *unstructured.Unstructured, boundary, fingerprint string) string {
	t.Helper()
	state, err := readWorkspaceMigrationState(live)
	if err != nil {
		t.Fatal(err)
	}
	switch boundary {
	case migrationTestPreparation, "capture":
		if state != nil || live.GetAnnotations()[lastAppliedWorkspaceAnnotation] != fingerprint {
			t.Fatal("pre-capture failure discarded discovery evidence")
		}
		managers, err := legacyUpdateManagers(live, newSSADeploymentForTest().UID)
		if err != nil || !reflect.DeepEqual(managers, map[string]struct{}{migrationTestLegacyManager: {}}) {
			t.Fatalf("pre-capture rediscovery changed: %v %v", managers, err)
		}
		return ""
	case migrationTestFinish, migrationTestFinalFingerprint:
		if state != nil {
			t.Fatal("completed cleanup retained capture")
		}
		if _, stale, _ := unstructured.NestedString(live.Object, "inference", "preset", "accessMode"); stale {
			t.Fatal("record removed before stale cleanup completed")
		}
		return ""
	default:
		if state == nil || !reflect.DeepEqual(state.Managers, []string{FieldManager, migrationTestLegacyManager}) || state.PreviousFieldsSHA256 != workspaceFingerprintDigest(fingerprint) {
			t.Fatalf("captured managers/fingerprint changed: %#v", state)
		}
		if value, err := state.fingerprint(live); err != nil || value != fingerprint {
			t.Fatalf("fingerprint not recoverable: %v", err)
		}
		return live.GetAnnotations()[migrationManagersAnnotation]
	}
}

func TestEnvtestCaptureRejectsReplacement(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, boundary := range captureTestBoundaries {
		t.Run(boundary, func(t *testing.T) { testCaptureReplacement(t, c, boundary) })
	}
}

func testCaptureReplacement(t *testing.T, c client.WithWatch, boundary string) {
	t.Helper()
	original := newCaptureBoundaryWorkspace(t, c)
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(original.GetLabels())
	replaced := false
	var replacement *unstructured.Unstructured
	observed := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if !replaced && captureRequestBoundary(getWorkspaceForTest(t, cl), obj, patch, opts) == boundary {
			replaced = true
			if err := cl.Delete(ctx, getWorkspaceForTest(t, cl)); err != nil {
				t.Fatal(err)
			}
			replacement = newSSAWorkspaceForTest("")
			references := replacement.GetOwnerReferences()
			references[0].UID = "foreign-replacement"
			replacement.SetOwnerReferences(references)
			if err := cl.Create(ctx, replacement, client.FieldOwner("external-actor")); err != nil {
				t.Fatal(err)
			}
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	r := &KaitoProviderReconciler{Client: observed}
	err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest())
	if !replaced || err == nil {
		t.Fatalf("replacement boundary missed or mutated: replaced=%v err=%v", replaced, err)
	}
	if !reflect.DeepEqual(getWorkspaceForTest(t, c).Object, replacement.Object) {
		t.Fatal("stale migration request changed the replacement")
	}
	if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); !isResourceConflict(err) {
		t.Fatalf("retry did not reject foreign owner: %v", err)
	}
}

func TestEnvtestCaptureCorruptionAndOldWriter(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, mutation := range []string{"record", "anchor", "missing-record", "missing-anchor", "both-overwritten", "both-deleted", "fingerprint", "old-writer-changed", "old-writer-same"} {
		t.Run(mutation, func(t *testing.T) { testCaptureMutation(t, c, mutation) })
	}
}

func testCaptureMutation(t *testing.T, c client.WithWatch, mutation string) {
	t.Helper()
	original := newCaptureBoundaryWorkspace(t, c)
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(original.GetLabels())
	r := &KaitoProviderReconciler{Client: c}
	marked, err := r.markLegacyWorkspaceMigration(t.Context(), original, newSSADeploymentForTest().UID, map[string]struct{}{FieldManager: {}, migrationTestLegacyManager: {}})
	if err != nil {
		t.Fatal(err)
	}
	mutateCaptureAnnotations(marked, original, mutation)
	// Old-main reconciliation uses a full Update, preserving unknown metadata.
	if err := c.Update(t.Context(), marked); err != nil {
		t.Fatal(err)
	}
	if mutation == "old-writer-same" {
		assertCaptureCompleted(t, c, desired)
		return
	}
	writes := 0
	observed := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		writes++
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	for range 2 {
		r = &KaitoProviderReconciler{Client: observed}
		if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); err == nil {
			t.Fatal("corrupt or changed state was accepted")
		}
	}
	if writes != 0 || !reflect.DeepEqual(getWorkspaceForTest(t, c).Object, marked.Object) {
		t.Fatal("corrupt recognized capture triggered writes")
	}
}

func mutateCaptureAnnotations(marked, original *unstructured.Unstructured, mutation string) {
	annotations := marked.GetAnnotations()
	switch mutation {
	case "record":
		annotations[migrationManagersAnnotation] = `{"managers":["external-actor"]}`
	case "anchor":
		annotations[migrationStateAnnotation] = "unknown-version"
	case "missing-record":
		delete(annotations, migrationManagersAnnotation)
	case "missing-anchor":
		delete(annotations, migrationStateAnnotation)
	case "both-overwritten":
		annotations[migrationManagersAnnotation] = original.GetAnnotations()[migrationManagersAnnotation]
		annotations[migrationStateAnnotation] = "old-user-value"
		annotations[lastAppliedWorkspaceAnnotation] = original.GetAnnotations()[lastAppliedWorkspaceAnnotation]
	case "both-deleted":
		delete(annotations, migrationManagersAnnotation)
		delete(annotations, migrationStateAnnotation)
	case "fingerprint":
		annotations[migrationPreviousFieldsAnnotation] = `{}`
	case "old-writer-changed":
		annotations[lastAppliedWorkspaceAnnotation] = `{}`
	case "old-writer-same":
		annotations[lastAppliedWorkspaceAnnotation] = original.GetAnnotations()[lastAppliedWorkspaceAnnotation]
	}
	marked.SetAnnotations(annotations)
}

func TestEnvtestCaptureForeignApplyConflict(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, key := range []string{migrationStateAnnotation, migrationManagersAnnotation, migrationPreviousFieldsAnnotation} {
		t.Run(key, func(t *testing.T) {
			original := newCaptureBoundaryWorkspace(t, c)
			// Sharing the existing value establishes genuine external Apply
			// ownership without using force or fabricated managedFields.
			claim := workspaceConfigurationWithIdentity(map[string]any{}, original)
			claim.SetAnnotations(map[string]string{key: original.GetAnnotations()[key]})
			r := &KaitoProviderReconciler{Client: c}
			live, err := r.applyWorkspaceAs(t.Context(), claim, "external-apply", original.GetResourceVersion())
			if err != nil {
				t.Fatal(err)
			}
			desired := newSSAWorkspaceForTest("")
			desired.SetLabels(original.GetLabels())
			if err := r.createOrUpdateResource(t.Context(), desired, newSSADeploymentForTest()); !isFieldManagerConflict(err) {
				t.Fatalf("expected Apply conflict: %v", err)
			}
			if !reflect.DeepEqual(getWorkspaceForTest(t, c).Object, live.Object) {
				t.Fatal("conflicting Apply ownership was changed")
			}
		})
	}
}

func TestEnvtestCaptureAnnotationSize(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	t.Run("near-limit", func(t *testing.T) {
		original := createSplitOwnershipWorkspace(t, c, map[string]string{"example.com/padding": strings.Repeat("x", 130000)})
		cleanupCaptureWorkspace(t, c, original)
		desired := newSSAWorkspaceForTest("")
		desired.SetLabels(original.GetLabels())
		assertCaptureCompleted(t, c, desired)
	})
	t.Run("no-space-preserves-original", func(t *testing.T) {
		original := createSplitOwnershipWorkspace(t, c, nil)
		cleanupCaptureWorkspace(t, c, original)
		annotations := original.GetAnnotations()
		used := len("example.com/padding")
		for key, value := range annotations {
			used += len(key) + len(value)
		}
		annotations["example.com/padding"] = strings.Repeat("x", 262144-used)
		original.SetAnnotations(annotations)
		if err := c.Update(t.Context(), original); err != nil {
			t.Fatal(err)
		}
		r := &KaitoProviderReconciler{Client: c}
		desired := newSSAWorkspaceForTest("")
		desired.SetLabels(original.GetLabels())
		if err := r.createOrUpdateResource(t.Context(), desired, newSSADeploymentForTest()); !apierrors.IsInvalid(err) {
			t.Fatalf("expected annotation limit rejection: %v", err)
		}
		if !reflect.DeepEqual(getWorkspaceForTest(t, c).Object, original.Object) {
			t.Fatal("failed capture lost original evidence")
		}
	})
}

func cleanupCaptureWorkspace(t *testing.T, c client.Client, live *unstructured.Unstructured) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Delete(ctx, live); err != nil && !apierrors.IsNotFound(err) {
			t.Error(err)
		}
	})
}

func TestEnvtestCaptureWithoutFingerprintRetries(t *testing.T) {
	c, _, _ := newLegacyUserAgentClients(t)
	for _, boundary := range captureTestBoundaries {
		if boundary == migrationTestMove {
			continue
		} // No old fingerprint exists to move.
		t.Run(boundary, func(t *testing.T) { testCaptureWithoutFingerprint(t, c, boundary) })
	}
}

func testCaptureWithoutFingerprint(t *testing.T, c client.WithWatch, boundary string) {
	t.Helper()
	original := createPreFingerprintWorkspace(t, c, map[string]string{
		migrationManagersAnnotation:       `{"managers":["external-actor"]}`,
		migrationPreviousFieldsAnnotation: `{"resource":{"instanceType":"preserved-instance-type"}}`,
		migrationStateAnnotation:          migrationStateVersion,
	})
	base := original.DeepCopy()
	annotations := original.GetAnnotations()
	annotations["external.example/keep"] = migrationTestUntouched
	original.SetAnnotations(annotations)
	if err := c.Patch(t.Context(), original, client.MergeFrom(base), client.FieldOwner("external-actor")); err != nil {
		t.Fatal(err)
	}
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(original.GetLabels())
	wantErr := errors.New("interrupted " + boundary)
	failures := 0
	faulty := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		stage := captureRequestBoundary(getWorkspaceForTest(t, cl), obj, patch, opts)
		if stage == migrationTestMove {
			t.Fatal("absent fingerprint caused a move write")
		}
		if stage == boundary {
			failures++
			return wantErr
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	for attempt := range 2 {
		r := &KaitoProviderReconciler{Client: faulty}
		err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest())
		if boundary == migrationTestFinish && attempt == 1 && err == nil {
			break
		}
		if !errors.Is(err, wantErr) || failures != attempt+1 {
			t.Fatalf("%s attempt %d: failures=%d err=%v", boundary, attempt, failures, err)
		}
		live := getWorkspaceForTest(t, c)
		assertCaptureExternalState(t, live)
		assertAbsentFingerprintCapture(t, live)

	}
	assertCaptureCompleted(t, c, desired)
}

func TestEnvtestCaptureMalformedOwnedRecord(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, record := range []string{`{`, `{"managers":[]}`, `{"managers":[""]}`, `{"managers":["provider"],"previousFieldsSHA256":"bad"}`} {
		t.Run(record, func(t *testing.T) {
			original := newCaptureBoundaryWorkspace(t, c)
			r := &KaitoProviderReconciler{Client: c}
			live, err := r.markLegacyWorkspaceMigration(t.Context(), original, newSSADeploymentForTest().UID, map[string]struct{}{FieldManager: {}, migrationTestLegacyManager: {}})
			if err != nil {
				t.Fatal(err)
			}
			capture := workspaceConfigurationWithIdentity(map[string]any{}, live)
			capture.SetAnnotations(map[string]string{migrationStateAnnotation: migrationStateVersion, migrationManagersAnnotation: record})
			malformed, err := r.applyWorkspaceAs(t.Context(), capture, migrationStateManager, live.GetResourceVersion())
			if err != nil {
				t.Fatal(err)
			}
			writes := 0
			observed := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				writes++
				return cl.Patch(ctx, obj, patch, opts...)
			}})
			for range 2 {
				r = &KaitoProviderReconciler{Client: observed}
				if err := r.createOrUpdateResource(t.Context(), newSSAWorkspaceForTest(""), newSSADeploymentForTest()); err == nil {
					t.Fatal("malformed owned record accepted")
				}
			}
			if writes != 0 || !reflect.DeepEqual(getWorkspaceForTest(t, c).Object, malformed.Object) {
				t.Fatal("malformed owned record triggered writes")
			}
		})
	}
}

func assertAbsentFingerprintCapture(t *testing.T, live *unstructured.Unstructured) {
	t.Helper()
	state, err := readWorkspaceMigrationState(live)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		if state.PreviousFieldsSHA256 != "" || !reflect.DeepEqual(state.Managers, []string{FieldManager, "provider"}) {
			t.Fatalf("absent fingerprint/captured managers changed: %#v", state)
		}
		if _, found := live.GetAnnotations()[migrationPreviousFieldsAnnotation]; found {
			t.Fatal("forged fingerprint became protocol state")
		}
	}
}

func TestEnvtestCaptureOwnerAnchorIsNarrow(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	original := newCaptureBoundaryWorkspace(t, c)
	base := original.DeepCopy()
	original.SetOwnerReferences(append(original.GetOwnerReferences(), metav1.OwnerReference{
		APIVersion: "example.com/v1", Kind: "ExternalOwner", Name: "external", UID: "external-owner",
	}))
	if err := c.Patch(t.Context(), original, client.MergeFrom(base), client.FieldOwner("external-actor")); err != nil {
		t.Fatal(err)
	}
	r := &KaitoProviderReconciler{Client: c}
	captured, err := r.markLegacyWorkspaceMigration(t.Context(), original, newSSADeploymentForTest().UID,
		map[string]struct{}{FieldManager: {}, migrationTestLegacyManager: {}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(captured.GetOwnerReferences(), original.GetOwnerReferences()) {
		t.Fatal("capture changed owner references")
	}
	fields, err := managedFieldsForManager(captured, migrationStateManager, metav1.ManagedFieldsOperationApply)
	if err != nil {
		t.Fatal(err)
	}
	owned, found, err := unstructured.NestedMap(fields, "f:metadata", "f:ownerReferences")
	if err != nil || !found || len(owned) != 1 {
		t.Fatalf("capture owner anchor is not one keyed reference: %v %v", owned, err)
	}
	for key := range owned {
		if !jsonKeyContainsUID(strings.TrimPrefix(key, "k:"), string(newSSADeploymentForTest().UID)) {
			t.Fatalf("capture claimed unrelated reference %s", key)
		}
	}
	if _, exists := fields["f:resource"]; exists {
		t.Fatal("capture claimed live spec")
	}
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(original.GetLabels())
	assertCaptureCompleted(t, c, desired)
	completed := getWorkspaceForTest(t, c)
	if !reflect.DeepEqual(completed.GetOwnerReferences(), original.GetOwnerReferences()) {
		t.Fatal("cleanup changed owner references")
	}
	owners, err := updateManagersOwningOwnerReferenceUID(completed, "external-owner")
	if err != nil || !reflect.DeepEqual(owners, map[string]struct{}{"external-actor": {}}) {
		t.Fatalf("foreign owner-reference ownership changed: %v %v", owners, err)
	}
}

func TestEnvtestCaptureLegacyOwnerFlags(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	t.Run("success", func(t *testing.T) { testCaptureLegacyOwnerFlags(t, c, false) })
	t.Run("final-write-retry", func(t *testing.T) { testCaptureLegacyOwnerFlags(t, c, true) })
}

func testCaptureLegacyOwnerFlags(t *testing.T, c client.WithWatch, failFinalWrite bool) {
	t.Helper()
	original := newCaptureBoundaryWorkspace(t, c)
	references := original.GetOwnerReferences()
	references[0].Controller = nil
	references[0].BlockOwnerDeletion = nil
	original.SetOwnerReferences(references)
	if err := c.Update(t.Context(), original, client.FieldOwner(migrationTestLegacyManager)); err != nil {
		t.Fatal(err)
	}
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(original.GetLabels())
	blockDeletion := true
	references = desired.GetOwnerReferences()
	references[0].BlockOwnerDeletion = &blockDeletion
	desired.SetOwnerReferences(references)
	if failFinalWrite {
		failCaptureOwnerFinalWrite(t, c, original, desired)
	}
	assertCaptureCompleted(t, c, desired)
	if !reflect.DeepEqual(getWorkspaceForTest(t, c).GetOwnerReferences(), desired.GetOwnerReferences()) {
		t.Fatal("missing legacy owner flags were not adopted")
	}
}

func failCaptureOwnerFinalWrite(t *testing.T, c client.WithWatch, original, desired *unstructured.Unstructured) {
	t.Helper()
	wantErr := errors.New("injected owner-reference final write")
	failed := false
	faulty := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if captureRequestBoundary(getWorkspaceForTest(t, cl), obj, patch, opts) == migrationTestFinalFingerprint {
			failed = true
			return wantErr
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	r := &KaitoProviderReconciler{Client: faulty}
	if err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest()); !failed || !errors.Is(err, wantErr) {
		t.Fatalf("missed final owner-reference write: failed=%v err=%v", failed, err)
	}
	live := getWorkspaceForTest(t, c)
	state, err := readWorkspaceMigrationState(live)
	if err != nil || state != nil {
		t.Fatalf("final write preceded capture cleanup: %v %v", state, err)
	}
	if !reflect.DeepEqual(live.GetOwnerReferences(), original.GetOwnerReferences()) {
		t.Fatal("owner-reference changed before capture cleanup")
	}
}
