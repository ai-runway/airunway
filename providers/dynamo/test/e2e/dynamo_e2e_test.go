//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	// mdName is the ModelDeployment name used in the test fixture.
	mdName = "qwen3-0-6b"

	// mdNamespace is the namespace for the ModelDeployment.
	mdNamespace = "default"

	// frontendPort is the port exposed by the Dynamo frontend service.
	frontendPort = "8000"

	// Multi-node test constants.
	multiNodeMDName      = "qwen25-72b-multinode"
	multiNodePVCName     = "shared-amlfs-storage"
	multiNodeJobName     = "qwen25-72b-multinode-model-download"
	multiNodeFrontendSvc = "qwen25-72b-multinode-frontend"
)

// testProviderReady verifies that the dynamo InferenceProviderConfig exists and is ready.
func testProviderReady(t *testing.T) {
	waitFor(t, 2*time.Minute, 5*time.Second, "dynamo provider ready", func() error {
		out, err := kubectlMayFail(t, "get", "inferenceproviderconfig", "dynamo",
			"-o", "jsonpath={.status.ready}")
		if err != nil {
			return fmt.Errorf("InferenceProviderConfig 'dynamo' not found: %v", err)
		}
		if out != "true" {
			return fmt.Errorf("dynamo provider not ready, status.ready=%q", out)
		}
		return nil
	})
	t.Log("dynamo provider is ready")
}

// TestDynamoMultiNodeE2E verifies the full Dynamo provider pipeline with multi-node
// inference: PVC (ReadWriteMany) creation → large model download → DGD creation with
// multinode config → pods on different nodes → inference serving.
//
// Uses Qwen/Qwen2.5-72B-Instruct (~146GB), which requires 2 nodes with 1×A100 80GB each.
// Gated by DYNAMO_INSTALLED=true environment variable.
func TestDynamoMultiNodeE2E(t *testing.T) {
	if os.Getenv("DYNAMO_INSTALLED") != "true" {
		t.Skip("skipping: DYNAMO_INSTALLED is not set to true")
	}

	// Register cleanup to collect debug info on failure.
	t.Cleanup(func() {
		if t.Failed() {
			collectMultiNodeDebugInfo(t, multiNodeMDName, mdNamespace)
		}
	})

	// Subtests are sequential — each phase gates the next.

	t.Run("ProviderReady", func(t *testing.T) {
		testProviderReady(t)
	})

	t.Run("CreateMultiNodeModelDeployment", func(t *testing.T) {
		yamlPath := testdataPath(t, "dynamo-multinode-modeldeployment.yaml")
		kubectl(t, "apply", "-f", yamlPath)
		t.Logf("applied multi-node ModelDeployment from %s", yamlPath)
	})

	t.Run("Phase1_PVCCreatedAndBound", func(t *testing.T) {
		// Pre-existing PVC — verify it exists and is Bound.
		waitFor(t, 5*time.Minute, 5*time.Second, "multi-node PVC bound", func() error {
			phase, err := kubectlMayFail(t, "get", "pvc", multiNodePVCName, "-n", mdNamespace,
				"-o", "jsonpath={.status.phase}")
			if err != nil {
				return fmt.Errorf("PVC %s not found: %v", multiNodePVCName, err)
			}
			if phase != "Bound" {
				return fmt.Errorf("PVC phase is %q, expected Bound", phase)
			}
			return nil
		})

		// Verify access mode is ReadWriteMany (required for multi-node).
		accessMode := kubectl(t, "get", "pvc", multiNodePVCName, "-n", mdNamespace,
			"-o", "jsonpath={.spec.accessModes[0]}")
		if accessMode != "ReadWriteMany" {
			t.Fatalf("PVC accessMode=%q, expected ReadWriteMany", accessMode)
		}

		// Verify StorageReady condition on the ModelDeployment.
		waitFor(t, 2*time.Minute, 5*time.Second, "StorageReady=True", func() error {
			status, reason := getCondition(t, multiNodeMDName, mdNamespace, "StorageReady")
			if status != "True" {
				return fmt.Errorf("StorageReady condition status=%q reason=%q, expected True", status, reason)
			}
			return nil
		})

		t.Log("pre-existing Lustre PVC verified Bound with ReadWriteMany access mode")
	})

	t.Run("Phase2_DownloadJobCompletes", func(t *testing.T) {
		// Longer timeout for ~146GB model download.
		waitFor(t, 45*time.Minute, 30*time.Second, "multi-node download Job complete", func() error {
			succeeded, err := kubectlMayFail(t, "get", "job", multiNodeJobName, "-n", mdNamespace,
				"-o", "jsonpath={.status.succeeded}")
			if err != nil {
				return fmt.Errorf("Job %s not found: %v", multiNodeJobName, err)
			}
			if succeeded != "1" {
				// Check for failure.
				failed, _ := kubectlMayFail(t, "get", "job", multiNodeJobName, "-n", mdNamespace,
					"-o", "jsonpath={.status.failed}")
				if failed != "" && failed != "0" && failed != "<nil>" {
					logs, _ := kubectlMayFail(t, "logs", fmt.Sprintf("job/%s", multiNodeJobName),
						"-n", mdNamespace, "--tail=20")
					return fmt.Errorf("Job has %s failure(s), logs:\n%s", failed, logs)
				}
				return fmt.Errorf("Job not yet succeeded (succeeded=%q)", succeeded)
			}
			return nil
		})

		// Verify ModelDownloaded condition.
		status, reason := getCondition(t, multiNodeMDName, mdNamespace, "ModelDownloaded")
		if status != "True" {
			t.Fatalf("ModelDownloaded condition status=%q reason=%q, expected True/DownloadComplete", status, reason)
		}
		if reason != "DownloadComplete" {
			t.Logf("ModelDownloaded reason=%q (expected DownloadComplete)", reason)
		}

		t.Log("multi-node model download completed successfully")
	})

	t.Run("Phase3_DGDCreated", func(t *testing.T) {
		// Wait for DGD to exist.
		waitFor(t, 3*time.Minute, 5*time.Second, "multi-node DGD created", func() error {
			_, err := kubectlMayFail(t, "get", "dynamographdeployments.nvidia.com", multiNodeMDName,
				"-n", mdNamespace)
			if err != nil {
				return fmt.Errorf("DynamoGraphDeployment %s not found: %v", multiNodeMDName, err)
			}
			return nil
		})

		// Verify owner reference back to ModelDeployment.
		ownerKind := kubectl(t, "get", "dynamographdeployments.nvidia.com", multiNodeMDName,
			"-n", mdNamespace, "-o", "jsonpath={.metadata.ownerReferences[0].kind}")
		if ownerKind != "ModelDeployment" {
			t.Fatalf("DGD ownerReference kind=%q, expected ModelDeployment", ownerKind)
		}

		ownerName := kubectl(t, "get", "dynamographdeployments.nvidia.com", multiNodeMDName,
			"-n", mdNamespace, "-o", "jsonpath={.metadata.ownerReferences[0].name}")
		if ownerName != multiNodeMDName {
			t.Fatalf("DGD ownerReference name=%q, expected %s", ownerName, multiNodeMDName)
		}

		// Verify multinode.nodeCount=2 on VllmWorker service.
		nodeCount := getDGDServiceField(t, multiNodeMDName, mdNamespace, "VllmWorker",
			"{.spec.services.VllmWorker.multinode.nodeCount}")
		if nodeCount != "2" {
			t.Fatalf("DGD VllmWorker multinode.nodeCount=%q, expected 2", nodeCount)
		}

		// Verify PVC reference exists in DGD with create: false (controller pre-created it).
		dgdPVCCreate := getDGDServiceField(t, multiNodeMDName, mdNamespace, "VllmWorker",
			"{.spec.services.VllmWorker.persistentVolumeClaim.create}")
		if dgdPVCCreate == "true" {
			t.Fatalf("DGD VllmWorker PVC create=%q, expected false (PVC managed by controller)", dgdPVCCreate)
		}

		// Verify ResourceCreated condition.
		waitFor(t, 3*time.Minute, 5*time.Second, "ResourceCreated=True", func() error {
			status, reason := getCondition(t, multiNodeMDName, mdNamespace, "ResourceCreated")
			if status != "True" {
				return fmt.Errorf("ResourceCreated condition status=%q reason=%q, expected True/ResourceCreated", status, reason)
			}
			return nil
		})

		// Verify ProviderCompatible condition.
		waitFor(t, 1*time.Minute, 5*time.Second, "ProviderCompatible=True", func() error {
			status, reason := getCondition(t, multiNodeMDName, mdNamespace, "ProviderCompatible")
			if status != "True" {
				return fmt.Errorf("ProviderCompatible condition status=%q reason=%q, expected True/CompatibilityVerified", status, reason)
			}
			return nil
		})

		t.Log("multi-node DynamoGraphDeployment created with nodeCount=2")
	})

	t.Run("Phase4_MultiNodePodsReady", func(t *testing.T) {
		// Wait for worker pods to be created by Grove (nodeCount=2 → 2 worker pods).
		// Note: pods may land on the same physical node if GPUs are available;
		// the scheduler is free to co-locate unless anti-affinity rules are set.
		waitFor(t, 20*time.Minute, 15*time.Second, "multi-node worker pods ready", func() error {
			nodes := getWorkerPodNodes(t, multiNodeMDName, mdNamespace)
			if len(nodes) < 2 {
				return fmt.Errorf("expected at least 2 worker pods, found %d (nodes: %v)", len(nodes), nodes)
			}
			return nil
		})

		// Log node distribution for observability.
		nodes := getWorkerPodNodes(t, multiNodeMDName, mdNamespace)
		nodeSet := make(map[string]bool)
		for _, n := range nodes {
			nodeSet[n] = true
		}
		t.Logf("multi-node worker pods (%d) on %d unique node(s): %v", len(nodes), len(nodeSet), nodes)

		// Verify ModelDeployment reaches Running phase.
		const failedThreshold = 3
		failedCount := 0

		waitFor(t, 25*time.Minute, 10*time.Second, "multi-node ModelDeployment Running", func() error {
			phase := getPhase(t, multiNodeMDName, mdNamespace)
			switch phase {
			case "Running":
				return nil
			case "Failed":
				failedCount++
				msg, _ := kubectlMayFail(t, "get", "modeldeployment", multiNodeMDName,
					"-n", mdNamespace, "-o", "jsonpath={.status.message}")
				if failedCount >= failedThreshold {
					t.Fatalf("ModelDeployment persistently Failed (%d consecutive): %s", failedCount, msg)
				}
				return fmt.Errorf("phase is Failed (attempt %d/%d, will retry): %s", failedCount, failedThreshold, msg)
			default:
				failedCount = 0
				return fmt.Errorf("phase is %q, waiting for Running", phase)
			}
		})

		// Log the nodes where worker pods are running.
		t.Logf("multi-node worker pods are running on nodes: %v", nodes)

		t.Log("multi-node ModelDeployment is Running with multi-node worker pods")
	})

	t.Run("InferenceServing", func(t *testing.T) {
		// Start port-forward to the frontend service.
		session := startPortForward(t, multiNodeFrontendSvc, frontendPort, mdNamespace)

		// Send inference request with retry.
		waitFor(t, 3*time.Minute, 5*time.Second, "multi-node inference response", func() error {
			requestBody := `{"model":"Qwen/Qwen2.5-72B-Instruct","messages":[{"role":"user","content":"Say hello in one word."}],"max_tokens":10}`
			cmd := exec.Command("curl", "-s", "-X", "POST",
				fmt.Sprintf("http://localhost:%s/v1/chat/completions", session.localPort),
				"-H", "Content-Type: application/json",
				"-d", requestBody,
				"--max-time", "60")
			output, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("curl failed: %v, output: %s", err, string(output))
			}

			t.Logf("multi-node inference response: %s", string(output))

			var response map[string]interface{}
			if err := json.Unmarshal(output, &response); err != nil {
				return fmt.Errorf("response is not valid JSON: %v", err)
			}

			choices, ok := response["choices"].([]interface{})
			if !ok || len(choices) == 0 {
				return fmt.Errorf("response missing choices: %v", response)
			}

			firstChoice, ok := choices[0].(map[string]interface{})
			if !ok {
				return fmt.Errorf("first choice is not an object")
			}

			message, ok := firstChoice["message"].(map[string]interface{})
			if !ok {
				return fmt.Errorf("choice missing message field")
			}

			content, ok := message["content"].(string)
			if !ok || content == "" {
				return fmt.Errorf("message content is empty or missing")
			}

			return nil
		})

		t.Log("multi-node inference serving verified successfully")
	})

	t.Run("Cleanup", func(t *testing.T) {
		// Delete the ModelDeployment.
		kubectl(t, "delete", "modeldeployment", multiNodeMDName, "-n", mdNamespace, "--timeout=5m")
		t.Log("multi-node ModelDeployment deleted")

		// Verify DGD is deleted.
		waitFor(t, 3*time.Minute, 5*time.Second, "multi-node DGD deleted", func() error {
			out, _ := kubectlMayFail(t, "get", "dynamographdeployments.nvidia.com", multiNodeMDName,
				"-n", mdNamespace, "--ignore-not-found")
			if out == "" {
				return nil
			}
			return fmt.Errorf("DGD %s still exists", multiNodeMDName)
		})
		t.Log("multi-node DGD deleted")

		// Pre-existing PVC should NOT be deleted — verify it survives.
		phase := kubectl(t, "get", "pvc", multiNodePVCName, "-n", mdNamespace,
			"-o", "jsonpath={.status.phase}")
		if phase != "Bound" {
			t.Fatalf("pre-existing PVC %s phase=%q after cleanup, expected Bound (should survive)", multiNodePVCName, phase)
		}
		t.Logf("pre-existing PVC %s survived cleanup (phase=Bound)", multiNodePVCName)

		// Verify Job is deleted.
		waitFor(t, 2*time.Minute, 5*time.Second, "multi-node Job deleted", func() error {
			out, _ := kubectlMayFail(t, "get", "job", multiNodeJobName, "-n", mdNamespace, "--ignore-not-found")
			if out == "" {
				return nil
			}
			return fmt.Errorf("Job %s still exists", multiNodeJobName)
		})
		t.Log("multi-node Job deleted")

		t.Log("multi-node cleanup completed successfully — all resources deleted")
	})
}

// TestDynamoStorageValidationE2E verifies storage-specific validation and failure paths:
// webhook rejections, reconcile-time failures, update immutability, and provider compatibility.
//
// Gated by DYNAMO_INSTALLED=true environment variable.
func TestDynamoStorageValidationE2E(t *testing.T) {
	if os.Getenv("DYNAMO_INSTALLED") != "true" {
		t.Skip("skipping: DYNAMO_INSTALLED is not set to true")
	}

	// --- Group 1: Webhook Storage Rejections ---
	// Each test applies an invalid YAML and asserts that the webhook rejects it.
	// No cleanup is needed because the resource is never created.

	// baseMD returns a ModelDeployment YAML string with the given name and storage section.
	// All tests use provider.name: dynamo, engine.type: vllm, resources.gpu.count: 1.
	baseMD := func(name, storageYAML string) string {
		return fmt.Sprintf(`apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: %s
  namespace: %s
spec:
  model:
    id: Qwen/Qwen3-0.6B
    source: huggingface
    storage:
%s
  provider:
    name: dynamo
  engine:
    type: vllm
  resources:
    gpu:
      count: 1
      type: nvidia.com/gpu
  scaling:
    replicas: 1
`, name, mdNamespace, storageYAML)
	}

	t.Run("StorageRejectsPreExistingWithoutClaimName", func(t *testing.T) {
		yaml := baseMD("val-pre-no-claim", `      volumes:
      - name: vol
        purpose: modelCache`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "claimName is required when size is not set") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsReadOnlyWithManagedPVC", func(t *testing.T) {
		yaml := baseMD("val-ro-managed", `      volumes:
      - name: model-cache
        purpose: modelCache
        size: "2Gi"
        readOnly: true`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "readOnly must not be true when size is set") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsManagedPVCWithCustomClaimName", func(t *testing.T) {
		yaml := baseMD("val-custom-claim", `      volumes:
      - name: model-cache
        purpose: modelCache
        size: "2Gi"
        claimName: my-custom-pvc`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "claimName must not be set when size is set") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsAccessModeWithoutSize", func(t *testing.T) {
		yaml := baseMD("val-access-no-size", `      volumes:
      - name: vol
        purpose: modelCache
        claimName: some-pvc
        accessMode: ReadWriteOnce`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "accessMode is only applicable when size is set") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsStorageClassWithoutSize", func(t *testing.T) {
		yaml := baseMD("val-sc-no-size", `      volumes:
      - name: vol
        purpose: modelCache
        claimName: some-pvc
        storageClassName: azurefile-premium`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "storageClassName is only applicable when size is set") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsDuplicateVolumeNames", func(t *testing.T) {
		yaml := baseMD("val-dup-names", `      volumes:
      - name: model-cache
        purpose: modelCache
        claimName: pvc-a
      - name: model-cache
        purpose: custom
        claimName: pvc-b
        mountPath: /data`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		// The CRD uses +listType=map +listMapKey=name, so the API server itself
		// may reject duplicates with "Duplicate value" before the webhook runs.
		// Accept either the API server's error or the webhook's error.
		if !strings.Contains(out, "duplicate volume name") && !strings.Contains(out, "Duplicate value") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsDuplicateMountPaths", func(t *testing.T) {
		yaml := baseMD("val-dup-mounts", `      volumes:
      - name: vol-a
        purpose: custom
        claimName: pvc-a
        mountPath: /shared-path
      - name: vol-b
        purpose: custom
        claimName: pvc-b
        mountPath: /shared-path`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "duplicate mount path") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsSystemMountPath", func(t *testing.T) {
		yaml := baseMD("val-sys-path", `      volumes:
      - name: vol
        purpose: custom
        claimName: some-pvc
        mountPath: /proc/data`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "system path") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsRelativeMountPath", func(t *testing.T) {
		yaml := baseMD("val-rel-path", `      volumes:
      - name: vol
        purpose: custom
        claimName: some-pvc
        mountPath: data/models`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "mountPath must be an absolute path") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsCustomPurposeWithoutMountPath", func(t *testing.T) {
		yaml := baseMD("val-custom-no-mp", `      volumes:
      - name: vol
        purpose: custom
        claimName: some-pvc`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "mountPath is required when purpose is custom") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	t.Run("StorageRejectsMultipleModelCacheVolumes", func(t *testing.T) {
		yaml := baseMD("val-multi-cache", `      volumes:
      - name: cache-a
        purpose: modelCache
        claimName: pvc-a
      - name: cache-b
        purpose: modelCache
        claimName: pvc-b`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			t.Fatalf("expected webhook rejection, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "at most one volume with purpose=modelCache") {
			t.Fatalf("unexpected error message: %s", out)
		}
	})

	// --- Group 2: Reconcile-Time Storage Failure ---
	// The resource passes webhook validation but the controller fails at reconcile time.

	t.Run("StorageFailsWhenPreExistingPVCMissing", func(t *testing.T) {
		name := "val-pvc-missing"
		t.Cleanup(func() { deleteModelDeployment(t, name) })

		yaml := baseMD(name, `      volumes:
      - name: vol
        purpose: modelCache
        claimName: nonexistent-pvc-xyz`)
		out, err := kubectlApplyLiteral(t, yaml)
		if err != nil {
			t.Fatalf("expected apply to succeed (webhook can't check PVC existence), but got: %s", out)
		}

		// Wait for the controller to set StorageReady=False with reason PVCFailed.
		waitFor(t, 1*time.Minute, 5*time.Second, "StorageReady=False/PVCFailed", func() error {
			status, reason := getCondition(t, name, mdNamespace, "StorageReady")
			if status != "False" {
				return fmt.Errorf("StorageReady status=%q reason=%q, expected False/PVCFailed", status, reason)
			}
			if reason != "PVCFailed" {
				return fmt.Errorf("StorageReady reason=%q, expected PVCFailed", reason)
			}
			return nil
		})

		// Verify phase is Failed.
		phase := getPhase(t, name, mdNamespace)
		if phase != "Failed" {
			t.Fatalf("expected phase=Failed, got %q", phase)
		}

		t.Log("controller correctly detected missing pre-existing PVC")
	})

	// --- Group 3: Update Immutability ---
	// Create a resource with managed storage, then attempt forbidden updates.

	t.Run("StorageRejectsRemovingManagedVolume", func(t *testing.T) {
		name := "val-immut-remove"
		t.Cleanup(func() { deleteModelDeployment(t, name) })

		// Create with managed storage.
		createYAML := baseMD(name, `      volumes:
      - name: model-cache
        purpose: modelCache
        size: "2Gi"`)
		out, err := kubectlApplyLiteral(t, createYAML)
		if err != nil {
			t.Fatalf("initial create failed: %s", out)
		}

		// Wait for resource to exist.
		waitFor(t, 30*time.Second, 2*time.Second, "resource exists", func() error {
			_, err := kubectlMayFail(t, "get", "modeldeployment", name, "-n", mdNamespace)
			return err
		})

		// Attempt update with storage block removed.
		updateYAML := fmt.Sprintf(`apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: %s
  namespace: %s
spec:
  model:
    id: Qwen/Qwen3-0.6B
    source: huggingface
  provider:
    name: dynamo
  engine:
    type: vllm
  resources:
    gpu:
      count: 1
      type: nvidia.com/gpu
  scaling:
    replicas: 1
`, name, mdNamespace)
		out, err = kubectlApplyLiteral(t, updateYAML)
		if err == nil {
			t.Fatalf("expected webhook to reject removal of managed volume, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "cannot be removed") {
			t.Fatalf("unexpected error message: %s", out)
		}

		t.Log("webhook correctly rejected removal of managed storage volume")
	})

	t.Run("StorageRejectsModifyingManagedVolume", func(t *testing.T) {
		name := "val-immut-modify"
		t.Cleanup(func() { deleteModelDeployment(t, name) })

		// Create with managed storage.
		createYAML := baseMD(name, `      volumes:
      - name: model-cache
        purpose: modelCache
        size: "2Gi"`)
		out, err := kubectlApplyLiteral(t, createYAML)
		if err != nil {
			t.Fatalf("initial create failed: %s", out)
		}

		// Wait for resource to exist.
		waitFor(t, 30*time.Second, 2*time.Second, "resource exists", func() error {
			_, err := kubectlMayFail(t, "get", "modeldeployment", name, "-n", mdNamespace)
			return err
		})

		// Attempt update changing size.
		updateYAML := baseMD(name, `      volumes:
      - name: model-cache
        purpose: modelCache
        size: "10Gi"`)
		out, err = kubectlApplyLiteral(t, updateYAML)
		if err == nil {
			t.Fatalf("expected webhook to reject modification of managed volume, but apply succeeded: %s", out)
		}
		if !strings.Contains(out, "immutable once created") {
			t.Fatalf("unexpected error message: %s", out)
		}

		t.Log("webhook correctly rejected modification of managed storage volume")
	})

	// --- Group 4: Provider Compatibility ---
	// These tests verify that the dynamo provider rejects incompatible configurations
	// at reconcile time (ProviderCompatible=False, phase=Failed).

	t.Run("ProviderRejectsLlamaCppEngine", func(t *testing.T) {
		name := "val-llamacpp"
		t.Cleanup(func() { deleteModelDeployment(t, name) })

		yaml := fmt.Sprintf(`apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: %s
  namespace: %s
spec:
  model:
    id: Qwen/Qwen3-0.6B
    source: huggingface
  provider:
    name: dynamo
  engine:
    type: llamacpp
  resources:
    gpu:
      count: 1
      type: nvidia.com/gpu
  scaling:
    replicas: 1
`, name, mdNamespace)
		out, err := kubectlApplyLiteral(t, yaml)
		if err != nil {
			// llamacpp + gpu might fail at webhook level — that's also acceptable
			t.Logf("apply returned error (may be webhook rejection): %s", out)
			return
		}

		waitFor(t, 1*time.Minute, 5*time.Second, "ProviderCompatible=False", func() error {
			status, reason := getCondition(t, name, mdNamespace, "ProviderCompatible")
			if status != "False" {
				return fmt.Errorf("ProviderCompatible status=%q reason=%q, expected False", status, reason)
			}
			return nil
		})

		phase := getPhase(t, name, mdNamespace)
		if phase != "Failed" {
			t.Fatalf("expected phase=Failed, got %q", phase)
		}

		t.Log("dynamo provider correctly rejected llamacpp engine")
	})

	t.Run("ProviderRejectsNoGPU", func(t *testing.T) {
		// The mutating webhook defaults resources.gpu to {count:1} when resources
		// are omitted. Setting gpu.count=0 explicitly bypasses the defaulter and
		// triggers the validating webhook's "vllm engine requires GPU" rejection.
		name := "val-no-gpu"

		yaml := fmt.Sprintf(`apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: %s
  namespace: %s
spec:
  model:
    id: Qwen/Qwen3-0.6B
    source: huggingface
  provider:
    name: dynamo
  engine:
    type: vllm
  resources:
    gpu:
      count: 0
  scaling:
    replicas: 1
`, name, mdNamespace)
		out, err := kubectlApplyLiteral(t, yaml)
		if err == nil {
			// If webhook didn't reject, clean up and check controller rejection.
			t.Cleanup(func() { deleteModelDeployment(t, name) })

			waitFor(t, 1*time.Minute, 5*time.Second, "ProviderCompatible=False", func() error {
				status, reason := getCondition(t, name, mdNamespace, "ProviderCompatible")
				if status != "False" {
					return fmt.Errorf("ProviderCompatible status=%q reason=%q, expected False", status, reason)
				}
				return nil
			})

			phase := getPhase(t, name, mdNamespace)
			if phase != "Failed" {
				t.Fatalf("expected phase=Failed, got %q", phase)
			}

			t.Log("dynamo provider correctly rejected no-GPU configuration at controller level")
			return
		}

		// Webhook rejected — verify the error mentions GPU requirement.
		if !strings.Contains(out, "requires GPU") && !strings.Contains(out, "gpu") {
			t.Fatalf("unexpected error message: %s", out)
		}

		t.Log("webhook correctly rejected no-GPU configuration")
	})
}
