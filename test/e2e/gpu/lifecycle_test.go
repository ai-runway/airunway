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

	"github.com/kaito-project/airunway/test/e2e/gpu/e2eutil"
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
	manifest := patchFixture(tc, raw)
	if out, err := e2eutil.KubectlApply(t, manifest); err != nil {
		t.Fatalf("applying fixture %s: %v\n%s", tc.fixture, err, out)
	}
	t.Logf("applied fixture %s", tc.fixture)
}

// patchFixture applies harness-owned overrides to a fixture before apply. For
// Dynamo it injects the chosen StorageClass so --storage-class retargets both
// the PVC and the storage assertion from a single source.
func patchFixture(tc testCase, raw []byte) []byte {
	if tc.provider != "dynamo" {
		return raw
	}
	// The fixture pins azurefile-premium; rewrite it to the selected class.
	return []byte(strings.ReplaceAll(string(raw),
		"storageClassName: azurefile-premium",
		"storageClassName: "+storageClass()))
}

// cleanup deletes the MD inline (not via t.Cleanup) so a parallel case frees its
// GPU as soon as it finishes. On a graceful-delete timeout it force-cascades to
// release the GPU for the rest of the batch. Skipped under GPU_E2E_KEEP.
func cleanup(t *testing.T, tc testCase) {
	if keepEnabled() {
		t.Logf("GPU_E2E_KEEP set; leaving %s in place", tc.mdName)
		return
	}
	_, err := e2eutil.KubectlMayFail(t, "delete", "modeldeployment", tc.mdName,
		"-n", tc.namespace, "--ignore-not-found",
		fmt.Sprintf("--timeout=%ds", int(deleteTimeout.Seconds())))
	if err != nil {
		t.Logf("graceful delete of %s timed out (%v); force-cascading to free GPU", tc.mdName, err)
		forceCascade(t, tc)
	}
	t.Logf("cleaned up %s", tc.mdName)
}
