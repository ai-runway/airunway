# Providers

## Engine & Provider Selection

When `spec.engine.type` is omitted, the controller auto-selects the engine from provider capabilities. When `spec.provider.name` is omitted, the controller auto-selects a provider using CEL-based selection rules from `InferenceProviderConfig` resources. Each provider declares rules with priorities; the highest-priority match wins.

### Engine Auto-Selection

The controller selects the engine in two passes:

1. **Filter providers** by compatibility with the deployment:
   - GPU/CPU: GPU deployments need `gpuSupport`, CPU deployments need `cpuSupport`
   - Serving mode: provider must support the requested mode (aggregated/disaggregated)
2. **Filter engines** from compatible providers:
   - CPU deployments skip GPU-requiring engines (`vllm`, `sglang`, `trtllm`)
   - Remaining engines are ranked by preference: `vllm` > `sglang` > `trtllm` > `llamacpp`
3. **Pick the first available** engine by preference

The selected engine is stored in `status.engine.type` with a reason in `status.engine.selectedReason`.

### Provider Auto-Selection

With the engine resolved, provider selection evaluates CEL rules from each `InferenceProviderConfig`:

**Default selection behavior** (with built-in providers):

```
IF gpu.count == 0 OR resources.gpu is omitted:
    → KAITO (only CPU provider), engine auto-selected to llamacpp

IF engine == "trtllm" OR engine == "sglang":
    → Dynamo (only provider supporting these engines)

IF engine == "llamacpp":
    → KAITO (only llamacpp provider)

IF mode == "disaggregated":
    → Dynamo (best disaggregated support)

DEFAULT (GPU + vllm + aggregated):
    → Dynamo (GPU inference default)
```

**Note:** KubeRay is never auto-selected. Users must explicitly set `provider.name: kuberay`.

The selection reason is recorded in `status.provider.selectedReason` for observability.

### Provider Capability Matrix

| Criteria              | KAITO   | Dynamo        | KubeRay            | llm-d              |
| --------------------- | ------- | ------------- | ------------------ | ------------------ |
| CPU inference         | **Yes** | No            | No                 | No                 |
| GPU inference         | Yes     | **Yes**       | Yes                | Yes                |
| vLLM engine           | Yes     | **Yes**       | Yes                | Yes                |
| sglang engine         | No      | **Yes**       | No                 | No                 |
| trtllm engine         | No      | **Yes**       | No                 | No                 |
| llamacpp engine       | **Yes** | No            | No                 | No                 |
| Disaggregated P/D     | No      | **Yes**       | Yes                | Yes                |
| Self-managed InferencePool | No | **Yes**       | No                 | No                 |
| Self-managed EPP      | No      | **Yes**       | No                 | No                 |
| Auto-selection        | Yes     | Yes (default) | No (explicit only) | No (explicit only) |

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
| NVIDIA Dynamo | DynamoGraphDeployment | ✅ Available | [dynamo.yaml](../providers/dynamo/deploy/dynamo.yaml) | High-performance GPU inference with KV-cache routing and disaggregated serving |
| KubeRay       | RayService            | ✅ Available | [kuberay.yaml](../providers/kuberay/deploy/kuberay.yaml) | Ray-based distributed inference with autoscaling                               |
| KAITO         | Workspace             | ✅ Available | [kaito.yaml](../providers/kaito/deploy/kaito.yaml) | Flexible inference with vLLM (GPU) or llama.cpp (CPU/GPU)                      |
| llm-d         | none                  | ✅ Available | [llmd.yaml](../providers/llmd/deploy/llmd.yaml) | Flexible inference with vLLM (GPU) with KV-cache routing and disaggregated serving |

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

## See also

- [Architecture Overview](architecture.md)
- [Controller Architecture](controller-architecture.md)
- [CRD Reference](crd-reference.md)
