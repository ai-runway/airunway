# Providers

## Engine & Provider Selection

When `spec.engine.type` is omitted, the controller auto-selects the engine from provider capabilities. When `spec.provider.name` is omitted, the controller auto-selects a provider using CEL-based selection rules from `InferenceProviderConfig` resources. Each provider declares rules with priorities; the highest-priority match wins.

### Engine Auto-Selection

The controller selects the engine by scanning per-engine capabilities from all ready `InferenceProviderConfig` resources:

1. **Filter engines** by per-engine compatibility with the deployment:
   - GPU/CPU: each engine declares `gpuSupport` and `cpuSupport` independently
   - Serving mode: each engine declares its supported `servingModes`
2. **Rank available engines** by preference: `vllm` > `sglang` > `trtllm` > `llamacpp`
3. **Pick the first available** engine by preference

The selected engine is stored in `status.engine.type` with a reason in `status.engine.selectedReason`.

### Provider Auto-Selection

With the engine resolved, provider selection evaluates CEL rules from each `InferenceProviderConfig`:

**Default selection behavior** depends on the `InferenceProviderConfig` resources installed in the cluster. With the provider configs bundled in this repository, the shipped rules are:

```
IF gpu.count == 0 OR resources.gpu is omitted:
    → KAITO (CPU-capable provider), engine auto-selected to llamacpp when needed

IF engine == "trtllm" OR engine == "sglang":
    → Dynamo

IF engine == "llamacpp":
    → KAITO

IF mode == "disaggregated":
    → Dynamo

IF gpu.count > 1 AND engine == "vllm":
    → KubeRay

IF gpu.count > 0 AND engine == "vllm":
    → Dynamo
```

**Note:** Provider auto-selection is driven by registered `InferenceProviderConfig.selectionRules`; the core selector does not hard-code KubeRay, llm-d, or Direct vLLM. Providers with empty or no matching rules are explicit-only unless their installed config makes them selectable.

The selection reason is recorded in `status.provider.selectedReason` for observability.

### Provider Capability Matrix

| Criteria                   | KAITO   | Dynamo        | KubeRay                | llm-d              | Direct vLLM                    |
| -------------------------- | ------- | ------------- | ---------------------- | ------------------ | ------------------------------ |
| CPU inference              | **Yes** | No            | No                     | No                 | No                             |
| GPU inference              | Yes     | **Yes**       | Yes                    | Yes                | Yes                            |
| vLLM engine                | Yes     | **Yes**       | Yes                    | Yes                | Yes                            |
| sglang engine              | No      | **Yes**       | No                     | No                 | No                             |
| trtllm engine              | No      | **Yes**       | No                     | No                 | No                             |
| llamacpp engine            | **Yes** | No            | No                     | No                 | No                             |
| Disaggregated P/D          | No      | **Yes**       | Yes                    | Yes                | No                             |
| Self-managed InferencePool | No      | **Yes**       | No                     | No                 | No                             |
| Self-managed EPP           | No      | **Yes**       | No                     | No                 | No                             |
| Customizable EPP image/config | No   | No            | No                     | **Yes**            | No                             |
| Auto-selection             | Yes     | Yes           | Via selection rules    | Explicit/config rules only | Explicit only                 |

## Provider Abstraction

AI Runway supports two deployment methods, both using the provider abstraction pattern:

### CRD-Based Deployment (Recommended)
Users create `ModelDeployment` CRs, and the controller + provider controllers handle the rest:
- Automatic provider selection based on capabilities
- Unified status reporting
- Provider-agnostic lifecycle management

### Web UI Deployment
The Web UI backend reads provider information (capabilities, installation steps, Helm charts) from `InferenceProviderConfig` CRDs in the cluster. These CRDs are created by **provider shims** — each provider shim must be installed (e.g., `kubectl apply -f providers/kaito/deploy/kaito.yaml`) before its provider appears in the UI. Once visible, the UI can trigger Helm-based upstream provider installation and creates `ModelDeployment` CRs for model deployment, which are then handled by the controller and provider controllers.

### Supported Providers

| Provider      | Upstream CRD          | Status      | Shim YAML | Description                                                                    |
| ------------- | --------------------- | ----------- | --------- | ------------------------------------------------------------------------------ |
| NVIDIA Dynamo | DynamoGraphDeployment / DynamoGraphDeploymentRequest | ✅ Available | [dynamo.yaml](https://github.com/ai-runway/airunway/blob/main/providers/dynamo/deploy/dynamo.yaml) | High-performance GPU inference with KV-cache routing, intent-based profiling and disaggregated serving|
| KubeRay       | RayService            | ✅ Available | [kuberay.yaml](https://github.com/ai-runway/airunway/blob/main/providers/kuberay/deploy/kuberay.yaml) | Ray-based distributed inference with autoscaling                               |
| KAITO         | Workspace             | ✅ Available | [kaito.yaml](https://github.com/ai-runway/airunway/blob/main/providers/kaito/deploy/kaito.yaml) | Flexible inference with vLLM (GPU) or llama.cpp (CPU/GPU)                      |
| llm-d         | none                  | ✅ Available | [llmd.yaml](https://github.com/ai-runway/airunway/blob/main/providers/llmd/deploy/llmd.yaml) | Flexible inference with vLLM (GPU) with KV-cache routing and disaggregated serving |
| Direct vLLM   | Deployment            | ✅ Available | [vllm.yaml](https://github.com/ai-runway/airunway/blob/main/providers/vllm/deploy/vllm.yaml) | Direct vLLM OpenAI-compatible server deployments using `spec.engine.image`; see [Direct vLLM guide](providers/vllm.md) |

### Dynamo deployment modes

Dynamo supports manual configuration through `DynamoGraphDeployment` (DGD) and
automatic configuration through `DynamoGraphDeploymentRequest` (DGDR). Both are
managed through `ModelDeployment`. The web UI exposes the same two modes.

#### Automatic configuration

Specify the model, backend, total GPU budget, expected traffic, and optional
latency targets. Do not specify manual resources or replica counts: Dynamo chooses
the topology and allocation within the profiling budget.

```yaml
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: qwen-auto
  namespace: default
spec:
  model:
    id: Qwen/Qwen3-0.6B
    source: huggingface
  engine:
    type: vllm
  provider:
    name: dynamo
    overrides:
      deploymentMode: intent
      intent:
        hardware:
          totalGpus: 1
        searchStrategy: rapid
        workload:
          isl: 1024
          osl: 256
          requestRate: 1
        sla:
          ttft: 1000
          itl: 50
  gateway:
    enabled: true
```

The typed `intent` block is validated by admission and provider reconciliation:

- `hardware.totalGpus` is a required budget from 1 through 64. Optional `gpuSku`,
  `vramMb`, and `numGpusPerNode` supply hardware information when discovery is not
  available. This budget does not reserve GPUs in the cluster.
- `workload.isl` and `osl` are positive token counts. Use `requestRate` or
  `concurrency`, not both.
- `sla.ttft` and `itl` are millisecond targets. Alternatively specify `e2eLatency`.
  Targets must be positive and end-to-end latency cannot be combined with the
  other targets.
- The initial typed workflow supports `searchStrategy: rapid` and submits
  `autoApply: true`. Real-GPU thorough searches, Planner controls, and
  review-before-apply are not exposed by this workflow.
- The installed operator supplies its matching profiler image. Ensure GPU
  discovery is available, or supply complete hardware information. Upstream's
  default profiler expects a namespace-local `hf-token-secret` with an `HF_TOKEN`
  key.

Rapid profiling uses performance estimates and can fall back to a basic
configuration. A generated configuration or healthy deployment is not proof that
it meets the requested performance targets.

#### Request lifecycle and explicit reconfiguration

Once profiling starts, normal edits to profiling inputs are rejected. Gateway
changes do not restart profiling. Failed requests do not automatically rerun.
Use the web UI's Retry or Reconfigure action to explicitly create a new request.
Reconfiguration can interrupt service; it is not a zero-downtime migration.

For YAML workflows, changing the `airunway.ai/dynamo-attempt` annotation to a new
short token explicitly starts a fresh attempt. To change profiling inputs, update
the annotation and desired inputs together. Reapplying the same token is not a
retry. The controller records the accepted token and input hash durably so a
controller restart does not repeat profiling.

The request and actual serving workload are tracked separately in
`status.provider.requestRef` and `status.provider.workloadRef`, including their
UIDs. `status.provider.intent` reports the request phase and profiling progress.
Endpoints, serving health, pod discovery, and routing come from the generated DGD.
When the generated topology has a frontend Service rather than an inference pool,
the gateway routes to that Service.

Deleting a native DGDR leaves its DGD running. Deleting a Runway ModelDeployment
instead cleans up its managed request and serving workload, using their recorded
identities rather than treating a matching name as ownership.

#### Manual configuration and legacy overrides

Omit `deploymentMode`, or set it to `manual`, to render a DGD directly. Manual
configuration retains ordinary resource updates and scaling; DGDR input
immutability does not apply to manual deployments.

Existing `deploymentMode: intent` resources using `overrides.spec` retain the
legacy pass-through format. That format derives the GPU budget from normal
resource/replica fields and lets upstream validate additional fields. It must not
be combined with the typed `intent` block. Legacy profiler-only requests using
`autoApply: false` are not equivalent to a serving deployment.

Typed intent deliberately does not reuse `spec.engine.image` as a profiler image,
or reinterpret manual engine arguments as topology-independent overrides.
Custom engine images/arguments, served-model aliases, environment variables, pod
metadata, placement settings, custom token-secret names, and storage volumes are
not accepted by the initial typed workflow. Use manual mode for those settings,
or the legacy intent format for an explicit upstream model-cache snapshot path.
The existing API-defaulted `engine.enablePrefixCaching` value is not an optimizer
constraint; cache tuning belongs in manual mode. Unsupported customization fails
validation rather than being silently dropped.

#### Dynamo compatibility

New installation metadata defaults to Dynamo 1.5.0. The provider supports the
1.1.1 alpha DGD contract and the 1.5 beta DGD contract; DGDR uses `v1beta1` on both.
Installed APIs determine the supported path, not a blanket minimum-version gate.
An installation without DGDR support can still use manual DGD deployments.

Do not mix a new runtime image with an old launch contract. Existing manual
workloads retain their API/image choices across a Runway-only upgrade. New 1.5
workloads use the native Rust endpoint-picker contract, while the legacy contract
is preserved for existing deployments. Unsupported raw fields fail strict API
validation instead of being pruned.

Compatibility checks include released CRD schemas for 1.1.1 and 1.5.0. Real
profiling and end-to-end serving additionally require a GPU cluster and the
corresponding operator/runtime installation. Schema tests alone do not establish
engine performance or cluster-network compatibility.

### KAITO Provider

The KAITO provider enables flexible inference with multiple backends:

- **vLLM Mode**: GPU inference using vLLM engine with full HuggingFace model support
- **Pre-made GGUF**: Ready-to-deploy quantized models from `ghcr.io/kaito-project/aikit/*`
- **HuggingFace GGUF**: Run any GGUF model from HuggingFace directly (no build required)
- **CPU/GPU Flexibility**: llama.cpp models can run on CPU nodes (no GPU required) or GPU nodes

| Mode             | Engine    | Compute | Use Case                         |
| ---------------- | --------- | ------- | -------------------------------- |
| vLLM             | vLLM      | GPU     | High-performance GPU inference   |
| Pre-made GGUF    | llama.cpp | CPU/GPU | Ready-to-deploy quantized models |
| HuggingFace GGUF | llama.cpp | CPU/GPU | Run any HuggingFace GGUF model   |

#### Build Infrastructure

For HuggingFace GGUF models, KAITO uses in-cluster image building:

```
┌────────────────┐     ┌──────────────┐     ┌─────────────────┐
│  HuggingFace   │────▶│  BuildKit    │────▶│  In-Cluster     │
│  GGUF Model    │     │  (K8s Driver)│     │  Registry       │
└────────────────┘     └──────────────┘     └─────────────────┘
                                                    │
                                                    ▼
                                            ┌─────────────────┐
                                            │  KAITO Pod      │
                                            │  (llama.cpp)    │
                                            └─────────────────┘
```

#### Related Services

- **RegistryService** (`backend/src/services/registry.ts`): Manages in-cluster registry
- **BuildKitService** (`backend/src/services/buildkit.ts`): Manages BuildKit builder
- **AikitService** (`backend/src/services/aikit.ts`): Handles GGUF image building

---

## Upstream Compatibility

A provider shim renders manifests it does not own the schema for. For `dynamo`, `kaito` and
`kuberay` that means a third-party CRD installed separately; if the cluster's installed
upstream is older than the shim expects, it may not declare a field the shim emits. `dynamo`
and `kaito` pin their target version in `versions.env`. KubeRay's installation metadata pins
operator chart `1.3.0` in `providers/kuberay/config.go`, but unlike those two it has no
centralized `KUBERAY_VERSION` pin or version-sync check. `llmd` and `vllm` render only built-in
`apps/v1` and `v1` types, whose schemas ship with the API server, so their exposure is to
Kubernetes version skew rather than to a third-party operator.

Kubernetes CRDs with a structural schema **prune** undeclared fields by default: the write
succeeds, the field vanishes, and no error is raised. That produced a real failure
([#308](https://github.com/ai-runway/airunway/issues/308)) where a workload came up without
its HTTP frontend, the gateway returned 503, and `ModelDeployment.status.phase` still read
`Running`.

Provider writes to the upstream resource therefore set **`fieldValidation=Strict`**, so the
API server rejects unknown fields instead of dropping them.

This is what protects `dynamo`, `kaito` and `kuberay`. For `llmd` and `vllm` it changes
little: they render built-in types through server-side apply, where the field manager already
rejects unknown fields during typed conversion regardless of this option (verified — an SSA
apply with validation explicitly ignored still fails with `field not declared in schema`).
Setting it there adds duplicate-key detection and keeps one uniform rule across all five.

**How a rejection surfaces.** `ResourceCreated=False` and `Ready=False`, both with reason
`IncompatibleUpstream`, and phase `Failed`. For `dynamo`, `kaito` and `kuberay` the offending
field is named in `status.message`. For `llmd` and `vllm` it is not: server-side apply reports
only one arbitrarily-chosen unknown field and picks a different one per call, so storing it
would rewrite status on every reconcile and re-enqueue the object each time. Those two store a
stable summary and log the specific field instead.
The provider keeps requeueing, so upgrading the upstream recovers the deployment without
anyone touching the `ModelDeployment`. `Ready` is forced false deliberately: #308 was a
deployment that reported healthy while unable to serve.

**Detection is by error message, not status code**, because the API server's response varies
by write path — a custom resource create returns `400` with `strict decoding error: unknown
field`, a merge patch returns `422` with the same prefix, and server-side apply on a built-in
type returns **`500`** with `field not declared in schema`. The last one is not a field-validation
error at all: it originates in the field manager's typed conversion
(`structured-merge-diff`, `typed/validate.go`), which is why it has a different status class and
why SSA rejects unknown fields even when field validation is disabled. Matching on the status class alone would both miss the apply path and wrongly
capture ordinary CEL and type-validation failures, which no upstream upgrade would fix.

**Scope.** This covers writes to the upstream resource from the five provider shims. Three
writers are **not** covered and carry the same skew risk:

- provider self-registration to `InferenceProviderConfig` (`providers/*/config.go`), when a
  provider binary is newer than the installed AI Runway CRDs;
- the PVC and Job writes in `controller/pkg/storage`;
- **the gateway reconciler** (`controller/internal/controller/gateway_reconciler.go`), which
  writes `InferencePool`, `HTTPRoute`, an Istio `DestinationRule` and a Gateway API
  `ReferenceGrant` — third-party CRDs pinned by `GAIE_VERSION`, `GATEWAY_API_VERSION` and
  `ISTIO_VERSION`. This is the closest remaining instance of #308 in the codebase: an older
  upstream that does not declare a field the reconciler sets would prune it silently.
  Covering it is uneven: the `HTTPRoute` writes are a plain `Create`/`Update` and would take
  the option directly, while `InferencePool`, `DestinationRule` and `ReferenceGrant` go
  through `ctrl.CreateOrUpdate`, which accepts no field-validation option and would need
  restructuring.

**Rollback.** This is a fail-closed change: a mismatch that previously produced a silently
degraded workload now blocks the deployment. There is no runtime opt-out — to revert the
behaviour, pin the previous provider image.

`spec.provider.overrides` passes through **three** separate checks, and it is worth keeping them
apart:

1. **The AI Runway validating webhook checks every provider's override payload** before a
   `ModelDeployment` is admitted. Its recursive rules reject security-sensitive fields and
   workload-sizing fields that could bypass the unified resource and replica limits.
2. **Providers that consume overrides check the root keys** before rendering. Dynamo, KAITO,
   llm-d and Direct vLLM accept only the roots they know how to apply — `spec` plus Dynamo's
   transformer-specific keys, or `resource` and `inference` for KAITO, which places them at the
   object root. Anything else is rejected with an error naming the offending key, rather than
   being merged and silently pruned. KubeRay does not consume overrides that pass the global
   admission rules, so they do not change the rendered `RayService`.
3. **The target API server checks the rendered object.** Passed-through fields must be
   declared by the target CRD or built-in Kubernetes schema, otherwise the write is rejected.

The distinction matters because the transformer-specific keys are *not* declared upstream and
are never sent: Dynamo's `routerMode` and `epp`, for example, are decoded into the shim's own
config and stripped before the write. They are accepted despite being absent from the upstream
schema, but their documented structure is decoded strictly so a typo such as `epp.imag` is
rejected rather than silently discarded. KAITO similarly rejects its replica path
`resource.count`; replicas must be set through `spec.scaling.replicas`. See
[Provider Overrides](controller-architecture.md#provider-overrides).

---

## See also

- [Architecture Overview](architecture.md)
- [Controller Architecture](controller-architecture.md)
- [CRD Reference](crd-reference.md)
