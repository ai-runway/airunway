//go:build e2e

package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// resultsRoot is the per-run results directory, e.g.
// <module>/gpu-e2e-results/20260623-140512/. It is created once per `go test`
// process. Each case writes its artifacts under <resultsRoot>/<case-as-dir>/.
var (
	resultsOnce sync.Once
	resultsDir  string
)

// moduleDir returns the directory containing this test file (the gpu module
// root), so results land in a stable location regardless of the process cwd.
func moduleDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Dir(thisFile)
}

// runResultsDir returns (and lazily creates) the directory for this test run.
// An optional GPU_E2E_RESULTS_DIR env var overrides the location entirely (e.g.
// to share a root with an external caller's logs); otherwise it is
// <module>/gpu-e2e-results/<timestamp>/. The optional GPU_E2E_RUN_TS env var
// pins the timestamp. Neither is set by scripts/gpu-e2e.sh today.
func runResultsDir(t *testing.T) string {
	t.Helper()
	resultsOnce.Do(func() {
		if dir := os.Getenv("GPU_E2E_RESULTS_DIR"); dir != "" {
			resultsDir = dir
		} else {
			ts := os.Getenv("GPU_E2E_RUN_TS")
			if ts == "" {
				ts = time.Now().Format("20060102-150405")
			}
			resultsDir = filepath.Join(moduleDir(), "gpu-e2e-results", ts)
		}
		if err := os.MkdirAll(resultsDir, 0o755); err != nil {
			t.Logf("warning: could not create results dir %s: %v", resultsDir, err)
			resultsDir = ""
			return
		}
		t.Logf("results dir: %s", resultsDir)
	})
	return resultsDir
}

// caseDir returns the per-case artifact directory, creating it on first use.
// Slashes in the case name (e.g. "dynamo/agg") become dashes so each case maps
// to a single directory.
func caseDir(t *testing.T, tc testCase) string {
	t.Helper()
	root := runResultsDir(t)
	if root == "" {
		return ""
	}
	name := filepath.Join(root, sanitizeCaseName(tc.name))
	if err := os.MkdirAll(name, 0o755); err != nil {
		t.Logf("warning: could not create case dir %s: %v", name, err)
		return ""
	}
	return name
}

func sanitizeCaseName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r == '/' {
			r = '-'
		}
		out = append(out, r)
	}
	return string(out)
}

// writeArtifact writes content to <caseDir>/<filename>, best-effort. It never
// fails the test — result bundles are diagnostics, not assertions.
func writeArtifact(t *testing.T, tc testCase, filename, content string) {
	t.Helper()
	dir := caseDir(t, tc)
	if dir == "" {
		return
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Logf("warning: could not write %s: %v", path, err)
		return
	}
	t.Logf("wrote %s", path)
}

// recordResult writes a one-line PASS/FAIL/SKIP marker for the case. Called from
// a t.Cleanup so it reflects the final outcome.
func recordResult(t *testing.T, tc testCase) {
	t.Helper()
	status := "PASS"
	switch {
	case t.Skipped():
		status = "SKIP"
	case t.Failed():
		status = "FAIL"
	}
	writeArtifact(t, tc, "result", fmt.Sprintf("%s %s\n", status, tc.name))
}
