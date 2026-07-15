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

package dynamo

import (
	airunwayv1alpha1 "github.com/ai-runway/airunway/controller/api/v1alpha1"
)

// Mocker mode is an internal, test-only Dynamo backend that swaps the real
// engine runtime (vLLM/SGLang/TRT-LLM) for `python3 -m dynamo.mocker`. The
// mocker simulates LLM scheduling, KV-cache behavior, and OpenAI-compatible
// serving without any GPU, which lets the full Airunway → Dynamo control path
// run on GPU-less GitHub-hosted CI runners.
//
// It is opt-in via the `airunway.ai/dynamo-test-backend: mocker` annotation on
// the ModelDeployment and is not intended for production use.
const (
	// AnnotationDynamoTestBackend selects an internal test backend for the
	// Dynamo provider. The only supported value is "mocker".
	AnnotationDynamoTestBackend = "airunway.ai/dynamo-test-backend"

	// DynamoTestBackendMocker is the annotation value that enables mocker mode.
	DynamoTestBackendMocker = "mocker"

	// MockerWorkerCPU and MockerWorkerMemory are the CPU/memory requests and
	// limits applied to mocker workers, and the request values used by the mocker
	// Frontend (which sets requests only). On a worker, equal requests and limits
	// make the pod Guaranteed QoS; the goal is only to avoid BestEffort — which is
	// evicted first under memory pressure and rejected by namespace LimitRanges
	// that mandate requests/limits — while staying tiny enough to co-schedule on
	// small CPU-only CI nodes.
	MockerWorkerCPU    = "100m"
	MockerWorkerMemory = "256Mi"
)

// defaultMockerImage is the image used for mocker Frontend and worker
// containers. Upstream mocker examples use the dynamo-planner image because it
// bundles the ai-dynamo wheel, ai-dynamo-runtime, and planner/profiler
// dependencies that mocker needs. Declared as a var (not const) so a build-time
// ldflags override of DynamoVersion flows through automatically.
var defaultMockerImage = "nvcr.io/nvidia/ai-dynamo/dynamo-planner:" + DynamoVersion

// isMockerMode reports whether the ModelDeployment requests the internal mocker
// test backend via the airunway.ai/dynamo-test-backend annotation.
func isMockerMode(md *airunwayv1alpha1.ModelDeployment) bool {
	if md == nil || md.Annotations == nil {
		return false
	}
	return md.Annotations[AnnotationDynamoTestBackend] == DynamoTestBackendMocker
}

// buildMockerArgs returns the base argument list for `python3 -m dynamo.mocker`.
// Callers append role-specific flags (e.g. --disaggregation-mode) for
// disaggregated prefill/decode workers.
func buildMockerArgs(md *airunwayv1alpha1.ModelDeployment) []string {
	modelName := md.Spec.Model.ServedName
	if modelName == "" {
		modelName = md.Spec.Model.ID
	}

	return []string{
		"--model-path", md.Spec.Model.ID,
		"--model-name", modelName,
		// High speedup ratio keeps CI fast — the mocker compresses simulated
		// token timing by this factor.
		"--speedup-ratio", "1000.0",
		// Small, CI-friendly cache geometry.
		"--num-gpu-blocks-override", "4096",
		"--block-size", DefaultKVCacheBlockSize,
		"--max-num-seqs", "64",
	}
}

// mockerCommand returns the container command for a mocker worker.
func mockerCommand() []string {
	return []string{"python3", "-m", "dynamo.mocker"}
}

// mockerWorkerResources returns the resource block for mocker workers. It sets
// small CPU/memory requests and limits (and no GPU) so the worker schedules on
// CPU-only nodes. Equal requests and limits make the pod Guaranteed QoS; the
// intent is only to avoid BestEffort — which is evicted first under memory
// pressure and rejected outright in namespaces whose LimitRange mandates
// requests/limits, both of which would make the CPU-only E2E lane flaky. The
// values are ample for the lightweight python3 -m dynamo.mocker process.
func mockerWorkerResources() map[string]interface{} {
	return map[string]interface{}{
		"requests": map[string]interface{}{
			"cpu":    MockerWorkerCPU,
			"memory": MockerWorkerMemory,
		},
		"limits": map[string]interface{}{
			"cpu":    MockerWorkerCPU,
			"memory": MockerWorkerMemory,
		},
	}
}
