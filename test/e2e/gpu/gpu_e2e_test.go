//go:build e2e

// Package gpu contains the consolidated GPU end-to-end suite for the airunway
// inference providers (Dynamo, vLLM, KAITO) in aggregated serving mode.
//
// The suite is table-driven by test case (provider × scenario). A single
// TestGPUProviders runs every selected case as a parallel subtest through the
// uniform lifecycle in runCase. Filter to one provider with
// `-run TestGPUProviders/<provider>`.
//
// It assumes a pre-provisioned GPU cluster with the airunway controller and the
// selected providers already deployed, the NVIDIA GPU operator, an RWX-capable
// StorageClass, and the inference gateway. TestMain enforces the cheap
// preconditions and fails fast.
package gpu

import (
	"os"
	"testing"
	"time"

	"github.com/ai-runway/airunway/test/e2e/gpu/e2eutil"
)

// Timeouts for the case lifecycle.
const (
	// upstreamCRTimeout bounds the wait for the provider's rendered CR to appear.
	upstreamCRTimeout = 3 * time.Minute
	// runningTimeout bounds the wait for the MD to reach Running (image pull +
	// model load + server start). Multi-GB images make this generous.
	runningTimeout = 45 * time.Minute
	// gatewayTimeout bounds the wait for gateway reconciliation to publish the
	// endpoint after Running.
	gatewayTimeout = 5 * time.Minute
	// inferenceTimeout bounds the per-request HTTP timeout for the chat call.
	inferenceTimeout = 30 * time.Second
	// inferenceWindow bounds retries of the chat call (EPP can be cold for ~60s
	// after a new MD's pool is created).
	inferenceWindow = 3 * time.Minute
	// deleteTimeout bounds the graceful MD delete during cleanup. It exceeds the
	// providers' 5-minute finalizer self-timeout so the happy path cascades
	// fully before any force-cascade.
	deleteTimeout = 6 * time.Minute
)

// Inference gateway coordinates. assertInference port-forwards this Service
// rather than using its external LoadBalancer IP, so inference is reachable from
// any machine with kubectl access.
const (
	gatewayService   = "inference-gateway-istio"
	gatewayNamespace = "default"
	gatewayPort      = 80
)

// keepEnabled reports whether GPU_E2E_KEEP requests leaving MDs in place after
// the test (for inspection). The next run's pre-delete still clears them.
func keepEnabled() bool {
	switch os.Getenv("GPU_E2E_KEEP") {
	case "1", "true", "TRUE", "yes", "on":
		return true
	default:
		return false
	}
}

// storageClass returns the StorageClass the harness injects into the Dynamo
// fixture and asserts on. Must be RWX-capable (the model-cache volume defaults
// to ReadWriteMany).
func storageClass() string {
	if sc := os.Getenv("GPU_E2E_STORAGE_CLASS"); sc != "" {
		return sc
	}
	return "azurefile-premium"
}

// TestGPUProviders runs every case in the matrix as a parallel subtest.
func TestGPUProviders(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runCase(t, tc)
		})
	}
}

// runCase drives one case through the uniform lifecycle:
//
//	pre-delete → apply fixture → upstream CR exists → schedule → Running +
//	provider name → GatewayReady → extra assertions → inference via gateway.
//
// Teardown and debug collection are registered as t.Cleanup callbacks rather
// than inline steps: a leaf parallel subtest's own cleanups run as soon as that
// subtest finishes — on success OR on a mid-case t.Fatal — and promptly, not at
// the end of the batch. That frees this case's GPU for siblings even when an
// assertion fails. Cleanups run LIFO, so they are registered in reverse of the
// order they run: recordResult first (runs last, after teardown and debug),
// then cleanup, then debug capture (runs first, while state still exists).
func runCase(t *testing.T, tc testCase) {
	// Registered first → runs last: record the final PASS/FAIL/SKIP after
	// teardown and debug have run.
	t.Cleanup(func() { recordResult(t, tc) })
	// Frees the GPU regardless of outcome.
	t.Cleanup(func() { cleanup(t, tc) })
	// Captures diagnostics only on failure (runs before teardown removes state).
	t.Cleanup(func() {
		if t.Failed() {
			collectDebug(t, tc)
		}
	})

	// Clean slate: remove any leftover MD from a prior or crashed run so GPU
	// accounting and apply are deterministic.
	preDeleteMD(t, tc)

	applyFixture(t, tc)

	t.Run("UpstreamCRExists", func(t *testing.T) { assertUpstreamCRExists(t, tc) })

	// Phase-1 scheduling classification runs on the case-level t (not a subtest)
	// so a capacity Skip or a non-GPU Fatal terminates the whole case rather than
	// just one subtest, and the later stages do not run against a pod that will
	// never schedule.
	classifyScheduling(t, tc)

	t.Run("Running", func(t *testing.T) { assertRunning(t, tc) })
	t.Run("GatewayReady", func(t *testing.T) { assertGatewayReady(t, tc) })

	if tc.extraAssert != nil {
		t.Run("ProviderChecks", func(t *testing.T) { tc.extraAssert(t, tc) })
	}

	t.Run("InferenceServing", func(t *testing.T) { assertInference(t, tc) })
}

// assertUpstreamCRExists waits for the provider's rendered upstream CR to appear
// with the MD's name.
func assertUpstreamCRExists(t *testing.T, tc testCase) {
	e2eutil.WaitFor(t, upstreamCRTimeout, 5*time.Second,
		desc(tc, "upstream CR "+tc.upstreamCR), func() error {
			_, err := e2eutil.KubectlMayFail(t, "get", tc.upstreamCR, tc.mdName, "-n", tc.namespace)
			return err
		})
	t.Logf("[%s] upstream CR %s/%s exists", tc.name, tc.upstreamCR, tc.mdName)
}

// assertRunning waits for the MD to reach phase=Running and verifies the
// resolved provider name.
func assertRunning(t *testing.T, tc testCase) {
	e2eutil.WaitFor(t, runningTimeout, 10*time.Second, desc(tc, "MD Running"), func() error {
		phase := e2eutil.MDJSONPath(t, tc.mdName, tc.namespace, "{.status.phase}")
		if phase != "Running" {
			return errf("phase is %q, want Running", phase)
		}
		return nil
	})
	got := e2eutil.MDJSONPath(t, tc.mdName, tc.namespace, "{.status.provider.name}")
	if got != tc.provider {
		t.Fatalf("status.provider.name = %q, want %q", got, tc.provider)
	}
	t.Logf("[%s] MD %s Running, provider=%s", tc.name, tc.mdName, tc.provider)
}

// assertGatewayReady waits for the GatewayReady condition and a published
// gateway endpoint.
func assertGatewayReady(t *testing.T, tc testCase) {
	e2eutil.WaitFor(t, gatewayTimeout, 5*time.Second, desc(tc, "GatewayReady"), func() error {
		status := e2eutil.MDJSONPath(t, tc.mdName, tc.namespace,
			`{.status.conditions[?(@.type=="GatewayReady")].status}`)
		if status != "True" {
			return errf("GatewayReady=%q", status)
		}
		ep := e2eutil.MDJSONPath(t, tc.mdName, tc.namespace, "{.status.gateway.endpoint}")
		if ep == "" {
			return errf("status.gateway.endpoint empty")
		}
		return nil
	})
	t.Logf("[%s] gateway ready, endpoint=%s model=%s", tc.name,
		e2eutil.MDJSONPath(t, tc.mdName, tc.namespace, "{.status.gateway.endpoint}"),
		e2eutil.MDJSONPath(t, tc.mdName, tc.namespace, "{.status.gateway.modelName}"))
}

// assertInference posts a chat-completion through the inference gateway and
// asserts a non-empty completion. It reaches the gateway via a port-forward to
// the gateway Service rather than its external LoadBalancer IP, so the check
// works from any machine with kubectl access (the external IP can be blocked by
// network policy). The model name is read from status.gateway.modelName — never
// hardcoded — and equals the backend served name (enforced by fixtures).
func assertInference(t *testing.T, tc testCase) {
	model := e2eutil.MDJSONPath(t, tc.mdName, tc.namespace, "{.status.gateway.modelName}")
	if model == "" {
		t.Fatalf("missing status.gateway.modelName")
	}

	pf := e2eutil.PortForwardService(t, gatewayService, gatewayNamespace, gatewayPort)

	e2eutil.WaitFor(t, inferenceWindow, 5*time.Second, desc(tc, "inference response"), func() error {
		// Re-establish the tunnel if it dropped, so a dead port-forward becomes
		// a one-tick retry instead of connection-refused for the whole window.
		pf.EnsureReady()
		content, err := e2eutil.GatewayChatCompletion(pf.BaseURL, model, inferenceTimeout)
		if err != nil {
			return err
		}
		if content == "" {
			return errf("empty completion content")
		}
		t.Logf("[%s] inference content: %q", tc.name, content)
		return nil
	})
}
