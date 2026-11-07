//go:build integration

package kaito

import (
	"context"
	"errors"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestEnvtestCreateCaptureFailures(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, boundary := range captureTestBoundaries {
		if boundary == migrationTestPreparation || boundary == "preservation-handoff" {
			continue
		}
		t.Run(boundary, func(t *testing.T) { testCreateCaptureFailure(t, c, boundary) })
	}
}

func createWithNonRenderedFields(t *testing.T, ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	t.Helper()
	workspace := obj.(*unstructured.Unstructured)
	// Model Create ownership including a default; no webhook execution claim.
	if err := unstructured.SetNestedField(workspace.Object, migrationTestInstanceType, "resource", "instanceType"); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, obj, opts...); err != nil {
		return err
	}
	base := workspace.DeepCopy()
	annotations := workspace.GetAnnotations()
	annotations["external.example/keep"] = migrationTestUntouched
	workspace.SetAnnotations(annotations)
	return c.Patch(ctx, workspace, client.MergeFrom(base), client.FieldOwner("external-actor"))
}

func testCreateCaptureFailure(t *testing.T, c client.WithWatch, boundary string) {
	t.Helper()
	cleanupCaptureWorkspace(t, c, newSSAWorkspaceForTest(""))
	wantErr := errors.New("interrupted Create " + boundary)
	failed := false
	faulty := interceptor.NewClient(c, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return createWithNonRenderedFields(t, ctx, cl, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if captureRequestBoundary(getWorkspaceForTest(t, cl), obj, patch, opts) == boundary {
				failed = true
				return wantErr
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	r := &KaitoProviderReconciler{Client: faulty}
	if err := r.createOrUpdateResource(t.Context(), newSSAWorkspaceForTest(testPrivateAccessMode), newSSADeploymentForTest()); !errors.Is(err, wantErr) || !failed {
		t.Fatalf("Create boundary %s missed: %v", boundary, err)
	}
	pending := getWorkspaceForTest(t, c)
	assertCaptureExternalState(t, pending)
	// Desired state changes while the process is stopped: the original
	// fingerprint must still allow removal of accessMode during recovery.
	assertCaptureCompleted(t, c, newSSAWorkspaceForTest(""))
}

func TestEnvtestCaptureResourceVersionConflicts(t *testing.T) {
	c := newMigrationEnvtestClient(t)
	for _, boundary := range captureTestBoundaries {
		t.Run(boundary, func(t *testing.T) { testCaptureResourceVersionConflict(t, c, boundary) })
	}
}

func testCaptureResourceVersionConflict(t *testing.T, c client.WithWatch, boundary string) {
	t.Helper()
	original := newCaptureBoundaryWorkspace(t, c)
	desired := newSSAWorkspaceForTest("")
	desired.SetLabels(original.GetLabels())
	raced := false
	var concurrent *unstructured.Unstructured
	observed := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if !raced && captureRequestBoundary(getWorkspaceForTest(t, cl), obj, patch, opts) == boundary {
			raced = true
			concurrent = getWorkspaceForTest(t, cl)
			base := concurrent.DeepCopy()
			annotations := concurrent.GetAnnotations()
			annotations["external.example/race"] = "retained"
			concurrent.SetAnnotations(annotations)
			if err := cl.Patch(ctx, concurrent, client.MergeFrom(base), client.FieldOwner("external-actor")); err != nil {
				t.Fatal(err)
			}
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	r := &KaitoProviderReconciler{Client: observed}
	err := r.createOrUpdateResource(t.Context(), desired.DeepCopy(), newSSADeploymentForTest())
	if !raced || (!apierrors.IsConflict(err) && !apierrors.IsInvalid(err)) {
		t.Fatalf("expected stale version rejection at %s: raced=%v err=%v", boundary, raced, err)
	}
	if !reflect.DeepEqual(getWorkspaceForTest(t, c).Object, concurrent.Object) {
		t.Fatal("stale request changed concurrently updated state")
	}
	assertCaptureCompleted(t, c, desired)
	if getWorkspaceForTest(t, c).GetAnnotations()["external.example/race"] != "retained" {
		t.Fatal("retry lost concurrent foreign annotation")
	}
}
