//go:build e2e

package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ai-runway/airunway/test/e2e/gpu/e2eutil"
)

// errf is a small wrapper so polling closures read tersely.
func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// desc prefixes a wait description with the case name so that interleaved
// parallel "waiting for ..." log lines identify which case they belong to.
func desc(tc testCase, what string) string {
	return "[" + tc.name + "] " + what
}

// testdataPath resolves a fixture filename to its absolute path under testdata/.
func testdataPath(t *testing.T, filename string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller for testdata path")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", filename)
}

// preDeleteMD removes any pre-existing MD of this case so each run starts from a
// clean slate (idempotent re-runs). It waits for the object to be gone.
func preDeleteMD(t *testing.T, tc testCase) {
	t.Helper()
	_, _ = e2eutil.KubectlMayFail(t, "delete", "modeldeployment", tc.mdName,
		"-n", tc.namespace, "--ignore-not-found", "--timeout=6m")
	e2eutil.WaitFor(t, 2*time.Minute, 5*time.Second, desc(tc, "pre-existing MD cleared"), func() error {
		out, err := e2eutil.KubectlMayFail(t, "get", "modeldeployment", tc.mdName,
			"-n", tc.namespace, "--ignore-not-found")
		if err != nil {
			return nil // not found counts as cleared
		}
		if strings.TrimSpace(out) != "" {
			return errf("MD still present")
		}
		return nil
	})
}

// applyFixture reads the case fixture, patches provider-specific values that the
// harness owns (currently the Dynamo StorageClass), and applies it via stdin so
// the on-disk fixture is never mutated.
func applyFixture(t *testing.T, tc testCase) {
	t.Helper()
	raw, err := os.ReadFile(testdataPath(t, tc.fixture))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", tc.fixture, err)
	}
	manifest := patchFixture(t, tc, raw)
	if out, err := e2eutil.KubectlApply(t, manifest); err != nil {
		t.Fatalf("applying fixture %s: %v\n%s", tc.fixture, err, out)
	}
	t.Logf("applied fixture %s", tc.fixture)
}

// patchFixture applies harness-owned overrides to a fixture before apply. For a
// Dynamo fixture that declares storage, it injects the chosen StorageClass so
// --storage-class retargets both the PVC and the storage assertion from a single
// source, and fails the test if the pinned literal is missing (a silent no-op
// would otherwise surface later as a confusing PVC mismatch). Storage-less
// fixtures (e.g. the disaggregated case) are returned unchanged.
func patchFixture(t *testing.T, tc testCase, raw []byte) []byte {
	t.Helper()
	out, ok := e2eutil.InjectStorageClass(tc.provider, raw, storageClass())
	if !ok {
		t.Fatalf("dynamo fixture %s declares storage but not %q; "+
			"the storage-class patch would silently no-op", tc.fixture, e2eutil.PinnedStorageClass)
	}
	return out
}

// cleanup runs as a t.Cleanup so a parallel case frees its GPU as soon as it
// finishes (on success or a mid-case t.Fatal), not at the end of the batch.
// After a graceful MD delete it asserts the upstream resources were actually
// garbage-collected (no orphans left holding a GPU) — a regression check a
// finalizer/ownerRef bug would otherwise slip past. On a graceful-delete
// timeout it force-cascades to release the GPU and skips the orphan check,
// since force-removal is the abnormal path. Skipped entirely under GPU_E2E_KEEP.
func cleanup(t *testing.T, tc testCase) {
	if keepEnabled() {
		t.Logf("GPU_E2E_KEEP set; leaving %s in place", tc.mdName)
		return
	}
	out, err := e2eutil.KubectlMayFail(t, "delete", "modeldeployment", tc.mdName,
		"-n", tc.namespace, "--ignore-not-found",
		fmt.Sprintf("--timeout=%ds", int(deleteTimeout.Seconds())))
	if err != nil {
		// Only a delete *timeout* means the resources are wedged and need a
		// force-cascade to free the GPU. Any other failure (RBAC, apiserver
		// down, missing CRD) is a real error we should surface, not silently
		// downgrade into a no-op cleanup that skips the orphan check.
		if strings.Contains(out, "timed out") || strings.Contains(err.Error(), "timed out") {
			t.Logf("graceful delete of %s timed out; force-cascading to free GPU", tc.mdName)
			forceCascade(t, tc)
			t.Logf("force-cascaded %s", tc.mdName)
			return
		}
		t.Fatalf("deleting %s failed (not a timeout): %v\n%s", tc.mdName, err, out)
	}
	assertNoOrphans(t, tc)
	t.Logf("cleaned up %s", tc.mdName)
}

// assertNoOrphans verifies that deleting the ModelDeployment cascaded to its
// rendered resources. It is read-only and best-effort-bounded: the upstream CR
// must be gone, and for Dynamo the model-cache PVC and download Job too. A
// controller bug that leaks these (a missing ownerRef or stuck finalizer) would
// leave GPU-holding workloads behind, which this catches.
func assertNoOrphans(t *testing.T, tc testCase) {
	gone := func(kind, name string) {
		e2eutil.WaitFor(t, 2*time.Minute, 5*time.Second,
			desc(tc, "orphan "+kind+" cleared"), func() error {
				out, _ := e2eutil.KubectlMayFail(t, "get", kind, name,
					"-n", tc.namespace, "--ignore-not-found")
				if out != "" {
					return errf("%s/%s still exists after MD delete", kind, name)
				}
				return nil
			})
	}

	gone(tc.upstreamCR, tc.mdName)
	if tc.provider == "dynamo" {
		gone("pvc", tc.mdName+"-model-cache")
		gone("job", tc.mdName+"-model-download")
	}
	t.Logf("[%s] no orphaned resources after delete", tc.name)
}
