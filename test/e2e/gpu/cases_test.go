//go:build e2e

package gpu

import "testing"

// testCase describes one GPU end-to-end scenario: a (provider × serving-mode)
// deployment driven through the uniform lifecycle in runCase. Adding a new
// scenario (e.g. dynamo disaggregated, trtllm) is a data-only change: append a
// case here and author its fixture under testdata/.
type testCase struct {
	// name is the subtest name, "<provider>/<scenario>" (e.g. "dynamo/agg").
	// The "<provider>" prefix lets `-run TestGPUProviders/<provider>` select all
	// of a provider's scenarios.
	name string

	// provider is the airunway provider name (status.provider.name).
	provider string

	// fixture is the testdata filename for this case.
	fixture string

	// mdName is the ModelDeployment metadata.name in the fixture.
	mdName string

	// namespace is always "default" (same namespace as the gateway — required;
	// cross-namespace gateway admission is currently broken upstream).
	namespace string

	// upstreamCR is the "resource" argument identifying the provider's rendered
	// upstream custom resource, used for the exists-check and debug dumps.
	upstreamCR string

	// podSelector selects the workload pods, for the timeout-only force-cascade
	// teardown. It may match more than the GPU-holding pod (e.g. Dynamo's
	// graph-deployment selector also matches the GPU-less frontend/EPP pod) —
	// that is fine for teardown, which deletes everything.
	podSelector string

	// workloadSelector optionally narrows the scheduling check to the
	// GPU-holding worker pod. When empty, the scheduling check uses podSelector.
	// Set it when podSelector also matches non-GPU pods (Dynamo), so the
	// capacity-SKIP path inspects the worker, not a frontend that schedules
	// instantly.
	workloadSelector string

	// extraAssert runs provider-specific assertions after GatewayReady and
	// before the inference check. nil for providers with no extra checks.
	extraAssert func(t *testing.T, tc testCase)
}

// cases is the v1 test matrix: three aggregated scenarios, all verified to serve
// through the gateway. Each upstreamCR/podSelector pair was confirmed against
// the live cluster.
var cases = []testCase{
	{
		name:        "vllm/agg",
		provider:    "vllm",
		fixture:     "vllm-modeldeployment.yaml",
		mdName:      "qwen3-0-6b-vllm",
		namespace:   "default",
		upstreamCR:  "deployments.apps",
		podSelector: "airunway.ai/deployment=qwen3-0-6b-vllm",
	},
	{
		name:        "kaito/agg",
		provider:    "kaito",
		fixture:     "kaito-modeldeployment.yaml",
		mdName:      "qwen3-0-6b-kaito",
		namespace:   "default",
		upstreamCR:  "workspaces.kaito.sh",
		podSelector: "kaito.sh/workspace=qwen3-0-6b-kaito",
	},
	{
		name:        "dynamo/agg",
		provider:    "dynamo",
		fixture:     "dynamo-modeldeployment.yaml",
		mdName:      "qwen3-0-6b-dynamo",
		namespace:   "default",
		upstreamCR:  "dynamographdeployments.nvidia.com",
		podSelector: "nvidia.com/dynamo-graph-deployment-name=qwen3-0-6b-dynamo",
		// The graph-deployment selector also matches the GPU-less frontend/EPP
		// pod; narrow the scheduling check to the GPU worker so capacity-SKIP
		// isn't masked by a frontend pod that schedules instantly.
		workloadSelector: "nvidia.com/dynamo-graph-deployment-name=qwen3-0-6b-dynamo,nvidia.com/dynamo-component-type=worker",
		extraAssert:      assertDynamoDeep,
	},
	// Disaggregated Dynamo serving is intentionally excluded: it serves correctly
	// in isolation but its shared-BBR restart races with concurrent aggregated
	// requests (ai-runway/airunway#334). Re-add as a data-only case once fixed.
}
