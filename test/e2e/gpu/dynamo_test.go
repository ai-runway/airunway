//go:build e2e

package gpu

import (
	"testing"
	"time"

	"github.com/ai-runway/airunway/test/e2e/gpu/e2eutil"
)

// assertDynamoDeep runs the Dynamo-specific assertions that the uniform
// MD-ladder does not cover, preserving parity with the original
// TestDynamoProviderE2E: the model-cache PVC, the model-download Job, the
// DynamoGraphDeployment ownership, and the intermediate readiness conditions.
//
// It deliberately does NOT assert the PVC's requested size (Azure File CSI
// rounds small requests up to its minimum share size) and reaches the model
// through the gateway (the standalone frontend Service no longer exists), unlike
// the original test.
func assertDynamoDeep(t *testing.T, tc testCase) {
	assertDynamoPVC(t, tc)
	assertDynamoDownloadJob(t, tc)
	assertDynamoDGDOwnership(t, tc)
	assertDynamoConditions(t, tc)
}

// assertDynamoPVC verifies the model-cache PVC bound with the harness-selected
// StorageClass and the managed-by label. Size is intentionally not asserted.
func assertDynamoPVC(t *testing.T, tc testCase) {
	pvc := tc.mdName + "-model-cache"
	e2eutil.WaitFor(t, 5*time.Minute, 5*time.Second, desc(tc, "model-cache PVC bound"), func() error {
		phase, err := e2eutil.KubectlMayFail(t, "get", "pvc", pvc, "-n", tc.namespace,
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return errf("PVC %s not found: %v", pvc, err)
		}
		if phase != "Bound" {
			return errf("PVC %s phase=%q, want Bound", pvc, phase)
		}
		return nil
	})

	sc := e2eutil.Kubectl(t, "get", "pvc", pvc, "-n", tc.namespace,
		"-o", "jsonpath={.spec.storageClassName}")
	if sc != storageClass() {
		t.Fatalf("PVC %s storageClassName=%q, want %q", pvc, sc, storageClass())
	}

	managedBy := e2eutil.Kubectl(t, "get", "pvc", pvc, "-n", tc.namespace,
		`-o`, `jsonpath={.metadata.labels.airunway\.ai/managed-by}`)
	if managedBy != "airunway" {
		t.Fatalf("PVC %s managed-by label=%q, want airunway", pvc, managedBy)
	}
	t.Logf("model-cache PVC bound (storageClass=%s, managed-by=%s)", sc, managedBy)
}

// assertDynamoDownloadJob waits for the model-download Job to succeed and
// verifies its job-type label.
func assertDynamoDownloadJob(t *testing.T, tc testCase) {
	job := tc.mdName + "-model-download"
	e2eutil.WaitFor(t, 15*time.Minute, 10*time.Second, desc(tc, "download Job complete"), func() error {
		succeeded, err := e2eutil.KubectlMayFail(t, "get", "job", job, "-n", tc.namespace,
			"-o", "jsonpath={.status.succeeded}")
		if err != nil {
			return errf("Job %s not found: %v", job, err)
		}
		if succeeded != "1" {
			failed, _ := e2eutil.KubectlMayFail(t, "get", "job", job, "-n", tc.namespace,
				"-o", "jsonpath={.status.failed}")
			if failed != "" && failed != "0" {
				logs, _ := e2eutil.KubectlMayFail(t, "logs", "job/"+job, "-n", tc.namespace, "--tail=20")
				return errf("Job %s has %s failure(s):\n%s", job, failed, logs)
			}
			return errf("Job %s not yet succeeded", job)
		}
		return nil
	})

	jobType := e2eutil.Kubectl(t, "get", "job", job, "-n", tc.namespace,
		`-o`, `jsonpath={.metadata.labels.airunway\.ai/job-type}`)
	if jobType != "model-download" {
		t.Fatalf("Job %s job-type label=%q, want model-download", job, jobType)
	}
	t.Logf("model-download Job completed")
}

// assertDynamoDGDOwnership verifies the DynamoGraphDeployment is owned by the
// ModelDeployment (the cleanup cascade depends on this ownerReference).
func assertDynamoDGDOwnership(t *testing.T, tc testCase) {
	kind := e2eutil.Kubectl(t, "get", tc.upstreamCR, tc.mdName, "-n", tc.namespace,
		"-o", "jsonpath={.metadata.ownerReferences[0].kind}")
	if kind != "ModelDeployment" {
		t.Fatalf("DGD ownerReference kind=%q, want ModelDeployment", kind)
	}
	name := e2eutil.Kubectl(t, "get", tc.upstreamCR, tc.mdName, "-n", tc.namespace,
		"-o", "jsonpath={.metadata.ownerReferences[0].name}")
	if name != tc.mdName {
		t.Fatalf("DGD ownerReference name=%q, want %s", name, tc.mdName)
	}
	t.Logf("DGD owned by ModelDeployment/%s", tc.mdName)
}

// assertDynamoConditions verifies the intermediate readiness conditions the
// uniform ladder collapses into Running. They are polled because the controller
// may briefly flip them during optimistic-locking retries.
func assertDynamoConditions(t *testing.T, tc testCase) {
	for _, cond := range []string{"StorageReady", "ModelDownloaded", "ResourceCreated", "ProviderCompatible"} {
		c := cond
		e2eutil.WaitFor(t, 2*time.Minute, 5*time.Second, desc(tc, c+"=True"), func() error {
			status := e2eutil.MDJSONPath(t, tc.mdName, tc.namespace,
				`{.status.conditions[?(@.type=="`+c+`")].status}`)
			if status != "True" {
				return errf("%s=%q, want True", c, status)
			}
			return nil
		})
	}
	t.Logf("intermediate conditions True (StorageReady, ModelDownloaded, ResourceCreated, ProviderCompatible)")
}
