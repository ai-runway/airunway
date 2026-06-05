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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// mockerAnnotationKey/Value are the ModelDeployment annotation that switches the
// Dynamo provider into its GPU-less mocker test backend. They must match
// providers/dynamo/mocker.go (AnnotationDynamoTestBackend / DynamoTestBackendMocker).
const (
	mockerAnnotationKey   = "airunway.ai/dynamo-test-backend"
	mockerAnnotationValue = "mocker"
)

// mockerPlannerImageSubstr is the image substring expected on mocker workers.
const mockerPlannerImageSubstr = "dynamo-planner"

// mockerCase parametrizes a mocker E2E run so the aggregated and disaggregated
// lanes can share one subtest sequence.
type mockerCase struct {
	// name is the ModelDeployment / DGD name.
	name string
	// fixture is the testdata YAML filename to apply (with the mocker
	// annotation injected at apply-time).
	fixture string
	// frontendSvc is the generated standalone Frontend service: <name>-frontend.
	frontendSvc string
	// workerServices are the DGD service keys whose container we assert on.
	workerServices []string
	// disaggregated indicates the prefill/decode worker assertions should run.
	disaggregated bool
}

// TestDynamoMockerE2E verifies the full CPU-only Dynamo mocker pipeline for
// aggregated serving:
//
//	ModelDeployment (mocker annotation) → Airunway controller → dynamo-provider
//	→ DynamoGraphDeployment (mocker workers) → Dynamo operator
//	→ mocker-backed OpenAI-compatible /v1/chat/completions
//
// It reuses the existing aggregated fixture (testdata/dynamo-modeldeployment.yaml)
// with the mocker annotation injected, so the GPU values in that fixture are
// ignored and the worker runs python3 -m dynamo.mocker on CPU.
//
// Gated by DYNAMO_MOCKER=true (distinct from DYNAMO_INSTALLED so the GPU and
// CPU suites never both run on a single invocation).
func TestDynamoMockerE2E(t *testing.T) {
	if os.Getenv("DYNAMO_MOCKER") != "true" {
		t.Skip("skipping: DYNAMO_MOCKER is not set to true")
	}

	runMockerCase(t, mockerCase{
		name:           "qwen3-0-6b",
		fixture:        "dynamo-modeldeployment.yaml",
		frontendSvc:    "qwen3-0-6b-frontend",
		workerServices: []string{"VllmWorker"},
		disaggregated:  false,
	})
}

// TestDynamoMockerDisaggE2E verifies the CPU-only Dynamo mocker pipeline for
// disaggregated serving (standalone Frontend + prefill + decode mocker workers).
//
// Gated by DYNAMO_MOCKER=true.
func TestDynamoMockerDisaggE2E(t *testing.T) {
	if os.Getenv("DYNAMO_MOCKER") != "true" {
		t.Skip("skipping: DYNAMO_MOCKER is not set to true")
	}

	runMockerCase(t, mockerCase{
		name:           "qwen3-0-6b-disagg",
		fixture:        "dynamo-disagg-modeldeployment.yaml",
		frontendSvc:    "qwen3-0-6b-disagg-frontend",
		workerServices: []string{"VllmPrefillWorker", "VllmDecodeWorker"},
		disaggregated:  true,
	})
}

// runMockerCase executes the shared mocker subtest sequence for a case.
func runMockerCase(t *testing.T, tc mockerCase) {
	t.Cleanup(func() {
		if t.Failed() {
			collectDebugInfo(t, tc.name, mdNamespace)
		}
		// Best-effort teardown so a failed run does not leak resources.
		deleteModelDeployment(t, tc.name)
	})

	t.Run("ProviderReady", func(t *testing.T) {
		testProviderReady(t)
	})

	t.Run("CreateModelDeployment", func(t *testing.T) {
		testCreateMockerModelDeployment(t, tc)
	})

	t.Run("DGDCreated", func(t *testing.T) {
		testMockerDGDCreated(t, tc)
	})

	t.Run("DGDSuccessful", func(t *testing.T) {
		testMockerDGDSuccessful(t, tc)
	})

	t.Run("PhaseRunning", func(t *testing.T) {
		testMockerPhaseRunning(t, tc)
	})

	t.Run("InferenceServing", func(t *testing.T) {
		testMockerInferenceServing(t, tc)
	})

	// Skip the explicit Cleanup subtest if an earlier step failed: the registered
	// t.Cleanup above still deletes the ModelDeployment best-effort, but leaving
	// the in-cluster state intact (and collectDebugInfo's dump) is more useful for
	// debugging than tearing it down here.
	if t.Failed() {
		t.Log("skipping explicit Cleanup subtest due to earlier failure; relying on t.Cleanup teardown")
		return
	}

	t.Run("Cleanup", func(t *testing.T) {
		testMockerCleanup(t, tc)
	})
}

// testCreateMockerModelDeployment applies the fixture with the mocker annotation
// injected into metadata so it is present on the very first reconcile.
func testCreateMockerModelDeployment(t *testing.T, tc mockerCase) {
	path := testdataPath(t, tc.fixture)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read fixture %s: %v", path, err)
	}

	manifest := injectMockerAnnotation(t, string(raw))
	out, err := kubectlApplyLiteral(t, manifest)
	if err != nil {
		t.Fatalf("failed to apply mocker ModelDeployment: %v\nOutput: %s", err, out)
	}
	t.Logf("applied mocker ModelDeployment %s from %s", tc.name, tc.fixture)
}

// injectMockerAnnotation parses a single-document ModelDeployment manifest, sets
// the mocker annotation, and strips every GPU field so the applied spec is
// genuinely CPU-only. Both happen via the unstructured API, then it re-serializes.
//
// Setting the annotation handles all the fixture-evolution cases for free:
// SetAnnotations creates metadata.annotations if absent, merges into an existing
// block, and overwrites (no duplicate) if the key is already present.
//
// Stripping the GPU fields (spec.resources.gpu, spec.scaling.prefill.gpu,
// spec.scaling.decode.gpu) is what makes this test meaningful: the shared
// fixtures carry gpu.count so the GPU lane can reuse them, but the mocker backend
// is GPU-less. Removing the counts forces the request through the same CPU-only
// path real users hit, so the webhook AND reconciler mocker bypasses are actually
// exercised end-to-end — not satisfied trivially by a leftover gpu.count.
func injectMockerAnnotation(t *testing.T, manifest string) string {
	t.Helper()

	var obj unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(manifest), &obj.Object); err != nil {
		t.Fatalf("failed to parse fixture as YAML: %v", err)
	}

	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[mockerAnnotationKey] = mockerAnnotationValue
	obj.SetAnnotations(ann)

	// Drop GPU requests everywhere so the mocker spec is CPU-only.
	unstructured.RemoveNestedField(obj.Object, "spec", "resources", "gpu")
	unstructured.RemoveNestedField(obj.Object, "spec", "scaling", "prefill", "gpu")
	unstructured.RemoveNestedField(obj.Object, "spec", "scaling", "decode", "gpu")

	out, err := yaml.Marshal(obj.Object)
	if err != nil {
		t.Fatalf("failed to re-serialize annotated manifest: %v", err)
	}
	return string(out)
}

// testMockerDGDCreated waits for the DGD to exist and asserts the generated
// mocker shape: planner image, python3 -m dynamo.mocker command, and no GPU
// resource requests. For disaggregated cases it also checks the prefill/decode
// workers carry --disaggregation-mode and omit --kv-transfer-config.
func testMockerDGDCreated(t *testing.T, tc mockerCase) {
	waitFor(t, 3*time.Minute, 5*time.Second, "DGD created", func() error {
		_, err := kubectlMayFail(t, "get", "dynamographdeployments.nvidia.com", tc.name,
			"-n", mdNamespace)
		if err != nil {
			return fmt.Errorf("DynamoGraphDeployment %s not found: %v", tc.name, err)
		}
		return nil
	})

	for _, svc := range tc.workerServices {
		base := fmt.Sprintf("{.spec.services.%s.extraPodSpec.mainContainer.", svc)

		image := getDGDServiceField(t, tc.name, mdNamespace, svc, base+"image}")
		if !strings.Contains(image, mockerPlannerImageSubstr) {
			t.Fatalf("worker %s image=%q, expected to contain %q", svc, image, mockerPlannerImageSubstr)
		}

		command := getDGDServiceField(t, tc.name, mdNamespace, svc, base+"command}")
		if !strings.Contains(command, "dynamo.mocker") {
			t.Fatalf("worker %s command=%q, expected to contain dynamo.mocker", svc, command)
		}

		// Mocker workers must not request GPUs. Query kubectl directly (rather
		// than getDGDServiceField, which swallows errors and returns "") so a
		// failed jsonpath lookup is a hard failure instead of silently passing.
		gpu, err := kubectlMayFail(t, "get", "dynamographdeployments.nvidia.com", tc.name,
			"-n", mdNamespace,
			"-o", fmt.Sprintf("jsonpath={.spec.services.%s.resources.requests.gpu}", svc))
		if err != nil {
			t.Fatalf("worker %s: failed to read GPU request: %v", svc, err)
		}
		if gpu != "" {
			t.Fatalf("worker %s requests gpu=%q, expected no GPU request in mocker mode", svc, gpu)
		}

		args := getDGDServiceField(t, tc.name, mdNamespace, svc, base+"args}")
		if tc.disaggregated {
			if !strings.Contains(args, "--disaggregation-mode") {
				t.Fatalf("disagg worker %s args=%q, expected --disaggregation-mode", svc, args)
			}
			if strings.Contains(args, "--kv-transfer-config") {
				t.Fatalf("disagg mocker worker %s args=%q, must not contain --kv-transfer-config", svc, args)
			}
		}
	}

	t.Log("DynamoGraphDeployment created with mocker shape")
}

// testMockerDGDSuccessful waits for the DGD to reach status.state=successful.
func testMockerDGDSuccessful(t *testing.T, tc mockerCase) {
	waitFor(t, 15*time.Minute, 10*time.Second, "DGD successful", func() error {
		state, err := kubectlMayFail(t, "get", "dynamographdeployments.nvidia.com", tc.name,
			"-n", mdNamespace, "-o", "jsonpath={.status.state}")
		if err != nil {
			return fmt.Errorf("failed to read DGD state: %v", err)
		}
		if state != "successful" {
			return fmt.Errorf("DGD state=%q, waiting for successful", state)
		}
		return nil
	})
	t.Log("DynamoGraphDeployment reached state=successful")
}

// testMockerPhaseRunning waits for the ModelDeployment to reach Running phase.
func testMockerPhaseRunning(t *testing.T, tc mockerCase) {
	const failedThreshold = 3
	failedCount := 0

	waitFor(t, 10*time.Minute, 10*time.Second, "ModelDeployment Running", func() error {
		phase := getPhase(t, tc.name, mdNamespace)
		switch phase {
		case "Running":
			return nil
		case "Failed":
			failedCount++
			msg, _ := kubectlMayFail(t, "get", "modeldeployment", tc.name,
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

	providerName := kubectl(t, "get", "modeldeployment", tc.name,
		"-n", mdNamespace, "-o", "jsonpath={.status.provider.name}")
	if providerName != "dynamo" {
		t.Fatalf("status.provider.name=%q, expected dynamo", providerName)
	}
	t.Log("ModelDeployment is Running")
}

// testMockerInferenceServing port-forwards the standalone Frontend and sends a
// chat completion request, asserting a valid OpenAI-compatible response.
func testMockerInferenceServing(t *testing.T, tc mockerCase) {
	session := startPortForward(t, tc.frontendSvc, frontendPort, mdNamespace)

	waitFor(t, 2*time.Minute, 5*time.Second, "inference response", func() error {
		requestBody := `{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"Say hello in one word."}],"max_tokens":10}`
		cmd := exec.Command("curl", "-s", "-X", "POST",
			fmt.Sprintf("http://localhost:%s/v1/chat/completions", session.localPort),
			"-H", "Content-Type: application/json",
			"-d", requestBody,
			"--max-time", "30")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("curl failed: %v, output: %s", err, string(output))
		}

		t.Logf("inference response: %s", string(output))

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

	t.Log("mocker inference serving verified successfully")
}

// testMockerCleanup deletes the ModelDeployment and verifies the DGD cascades.
func testMockerCleanup(t *testing.T, tc mockerCase) {
	kubectl(t, "delete", "modeldeployment", tc.name, "-n", mdNamespace, "--timeout=5m")
	t.Log("ModelDeployment deleted")

	waitFor(t, 3*time.Minute, 5*time.Second, "DGD deleted", func() error {
		out, _ := kubectlMayFail(t, "get", "dynamographdeployments.nvidia.com", tc.name,
			"-n", mdNamespace, "--ignore-not-found")
		if out == "" {
			return nil
		}
		return fmt.Errorf("DGD %s still exists", tc.name)
	})
	t.Log("DynamoGraphDeployment deleted")
}
