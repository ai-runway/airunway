//go:build e2e

package gpu

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ai-runway/airunway/test/e2e/gpu/e2eutil"
)

// forceCascade releases an MD's GPU when graceful delete times out. It deletes
// owner-first then pods so the operator cannot recreate a pod between steps,
// then strips the MD finalizer so the object can leave even with its provider
// controller mid-reconcile. Every step is best-effort (--ignore-not-found) and
// never fails the test — its only job is to free the GPU for the rest of the
// batch.
func forceCascade(t *testing.T, tc testCase) {
	t.Helper()

	// 1. Delete the upstream CR so the operator stops reconciling/recreating pods.
	forceDelete(t, tc.upstreamCR, tc.mdName, tc.namespace)
	stripFinalizers(t, tc.upstreamCR, tc.mdName, tc.namespace)

	// 2. KAITO's Workspace owns a StatefulSet which owns the pod; delete it too,
	//    or the StatefulSet recreates the pod and re-grabs the GPU.
	if tc.provider == "kaito" {
		forceDelete(t, "statefulset", tc.mdName, tc.namespace)
	}

	// 3. Force-delete the GPU-holding pods by selector.
	_, _ = e2eutil.KubectlMayFail(t, "delete", "pod", "-n", tc.namespace,
		"-l", tc.podSelector, "--force", "--grace-period=0", "--ignore-not-found")

	// 4. Strip the MD finalizer so the object leaves even if the controller is busy.
	stripFinalizers(t, "modeldeployment", tc.mdName, tc.namespace)
}

func forceDelete(t *testing.T, resource, name, namespace string) {
	t.Helper()
	_, _ = e2eutil.KubectlMayFail(t, "delete", resource, name, "-n", namespace,
		"--force", "--grace-period=0", "--ignore-not-found")
}

func stripFinalizers(t *testing.T, resource, name, namespace string) {
	t.Helper()
	_, _ = e2eutil.KubectlMayFail(t, "patch", resource, name, "-n", namespace,
		"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`)
}

// collectDebug dumps diagnostics for a failed case to its log and to a
// per-case debug.txt artifact. It is filtered to the case's own resources where
// possible so concurrent cases don't pollute each other's bundle; cluster-wide
// dumps (events, pods) are included per-case.
func collectDebug(t *testing.T, tc testCase) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "=== DEBUG %s ===\n", tc.name)

	dump := func(label string, args ...string) {
		out, err := e2eutil.KubectlMayFail(t, args...)
		if err == nil && out != "" {
			fmt.Fprintf(&b, "\n--- %s ---\n%s\n", label, out)
		}
	}

	dump("ModelDeployment",
		"get", "modeldeployment", tc.mdName, "-n", tc.namespace, "-o", "yaml")
	dump("Upstream CR "+tc.upstreamCR,
		"get", tc.upstreamCR, tc.mdName, "-n", tc.namespace, "-o", "yaml")
	dump("Pods (case)",
		"get", "pods", "-n", tc.namespace, "-l", tc.podSelector, "-o", "wide")
	dump("Provider logs",
		"logs", "-n", "airunway-system", "-l",
		fmt.Sprintf("control-plane=%s-provider", tc.provider), "--tail=100")
	dump("Events",
		"get", "events", "-n", tc.namespace, "--sort-by=.lastTimestamp")
	dump("Pods (all namespaces)", "get", "pods", "-A")

	t.Logf("%s", b.String())
	writeArtifact(t, tc, "debug.txt", b.String())
}
