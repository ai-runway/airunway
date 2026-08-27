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
| Persistent model storage   | No      | **Yes**       | Yes                    | Yes                | Yes                            |
| Auto-selection             | Yes     | Yes           | Via selection rules    | Explicit/config rules only | Explicit only                 |

Persistent storage uses claims in the `ModelDeployment` namespace, not the provider installation namespace. Dynamo, KubeRay, and llm-d mount storage on every model-serving workload in each advertised serving mode; Direct vLLM mounts storage in its advertised aggregated mode. KAITO storage is rejected because preset workspaces do not expose a pod template and the portable API does not identify a concrete llama.cpp model file. See the [CRD storage reference](crd-reference.md#specmodelstoragevolumes) for lifecycle, cache, and multi-node constraints.

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
| NVIDIA Dynamo | DynamoGraphDeployment | ✅ Available | [dynamo.yaml](https://github.com/ai-runway/airunway/blob/main/providers/dynamo/deploy/dynamo.yaml) | High-performance GPU inference with KV-cache routing and disaggregated serving |
| KubeRay       | RayService            | ✅ Available | [kuberay.yaml](https://github.com/ai-runway/airunway/blob/main/providers/kuberay/deploy/kuberay.yaml) | Ray-based distributed inference with autoscaling                               |
| KAITO         | Workspace             | ✅ Available | [kaito.yaml](https://github.com/ai-runway/airunway/blob/main/providers/kaito/deploy/kaito.yaml) | Flexible inference with vLLM (GPU) or llama.cpp (CPU/GPU)                      |
| llm-d         | none                  | ✅ Available | [llmd.yaml](https://github.com/ai-runway/airunway/blob/main/providers/llmd/deploy/llmd.yaml) | Flexible inference with vLLM (GPU) with KV-cache routing and disaggregated serving |
| Direct vLLM   | Deployment            | ✅ Available | [vllm.yaml](https://github.com/ai-runway/airunway/blob/main/providers/vllm/deploy/vllm.yaml) | Direct vLLM OpenAI-compatible server deployments using `spec.engine.image`; see [Direct vLLM guide](providers/vllm.md) |

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
