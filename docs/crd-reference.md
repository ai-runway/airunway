# CRD Reference

## ModelDeployment

Unified API for deploying ML models.

```yaml
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: my-model
  namespace: default
spec:
  model:
    id: "Qwen/Qwen3-0.6B"       # HuggingFace model ID
    source: huggingface          # huggingface or custom
    storage:
      volumes:
        - name: model-cache      # DNS label, unique per deployment
          purpose: modelCache    # modelCache, compilationCache, or custom
          # Option A: reference a pre-existing PVC
          claimName: pvc-claim
          # readOnly: false         # optional, default false
          # Option B: let the controller create a PVC (omit claimName, set size)
          # size: 100Gi
          # storageClassName: azurelustre-static   # omit to use cluster default
          # accessMode: ReadWriteMany              # default when size is set
          mountPath: /model-cache  # required when purpose is custom; defaults for cache purposes
  engine:
    type: vllm                   # vllm, sglang, trtllm, llamacpp (optional, auto-selected)
    image: ""                    # Engine-specific image override; preferred for Direct vLLM/custom vLLM images
    contextLength: 32768
    trustRemoteCode: false
    enablePrefixCaching: true
    enforceEager: false
    args: {}                     # Engine-specific named flags, passed through by providers
    extraArgs: []                # Additional raw engine flags
  provider:
    name: ""                     # Optional: explicit provider selection
  serving:
    mode: aggregated             # aggregated or disaggregated
  resources:
    gpu:
      count: 1
      type: "nvidia.com/gpu"
  scaling:
    replicas: 1
  image: ""                      # Legacy provider-level image override; prefer spec.engine.image for Direct vLLM
  gateway:
    enabled: true                # Optional: defaults to true when Gateway detected
    modelName: ""                # Optional: override model name for routing
```

> **Note:** If `gateway.enabled` is explicitly set to `true` but the Gateway API Inference Extension CRDs are not installed, the controller sets a `GatewayReady=False` condition with reason `CRDsNotAvailable`. This surfaces as a status warning on the `ModelDeployment`.

### spec.engine

`spec.engine` defines the model-server runtime and engine-level launch settings.

| Field | Type | Required | Description |
|---|---|---|---|
| `type` | string | no | Engine type: `vllm`, `sglang`, `trtllm`, or `llamacpp`. If omitted, the controller auto-selects from provider capabilities. |
| `image` | string | no | Engine-specific container image override. This is the preferred field for Direct vLLM and custom vLLM OpenAI-compatible server images. |
| `contextLength` | int | no | Maximum context length. Providers map this to engine-specific flags such as vLLM `--max-model-len`. |
| `trustRemoteCode` | bool | no | Allows remote HuggingFace model code execution when supported by the engine. |
| `enablePrefixCaching` | bool | no | Enables prefix caching when supported by the engine. |
| `enforceEager` | bool | no | Forces eager execution when supported by the engine. |
| `args` | map[string]string | no | Engine-specific named arguments. Providers pass these through to the engine; for boolean-style flags, use an empty string value when supported by the provider. |
| `extraArgs` | []string | no | Additional raw engine flags for arguments that do not have a structured field or map representation yet. |

### spec.image (legacy)

Top-level `spec.image` remains supported for backward compatibility as a provider-level custom image override. For Direct vLLM and custom vLLM launch images, prefer `spec.engine.image`.

### Direct vLLM image example

Use explicit provider/runtime selection and put the vLLM server image under `spec.engine.image`:

```yaml
spec:
  provider:
    name: vllm
  engine:
    type: vllm
    image: vllm/vllm-openai:cu130-nightly
    args:
      trust-remote-code: ""
```

### spec.model.storage.volumes[]

Each entry is a `StorageVolume`. Maximum 8 volumes per deployment.

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Unique volume identifier. DNS label format (`[a-z0-9-]`, max 63 chars). |
| `purpose` | string | no | `modelCache`, `compilationCache`, or `custom` (default). Controls mount path defaults and engine behavior. Only one volume of each cache purpose is allowed. |
| `claimName` | string | conditional | Name of a pre-existing PVC in the same namespace. Required when `size` is not set. When `size` is set and `claimName` is empty, defaults to `<deployment-name>-<volume-name>`. |
| `mountPath` | string | conditional | Absolute path inside the container. Required when `purpose` is `custom`. Defaults: `/model-cache` for `modelCache`, `/compilation-cache` for `compilationCache`. |
| `readOnly` | bool | no | Mount the volume read-only. Default: `false`. |
| `size` | string | no | Requested storage size (e.g. `100Gi`). When set, the controller creates a PVC automatically. When omitted, `claimName` must reference a pre-existing PVC. |
| `storageClassName` | string | no | StorageClass for controller-created PVCs. Omit to use the cluster default. Set to `""` to disable dynamic provisioning. Only used when `size` is set. |
| `accessMode` | string | no | PVC access mode for controller-created PVCs. One of `ReadWriteOnce`, `ReadWriteMany`, `ReadOnlyMany`, `ReadWriteOncePod`. Default: `ReadWriteMany`. Only used when `size` is set. |

## InferenceProviderConfig

Cluster-scoped resource for provider registration. Each provider controller self-registers its `InferenceProviderConfig` at startup, declaring capabilities and selection rules in `spec`, and display, installation, health, and documentation metadata in `metadata.annotations`:

```yaml
apiVersion: airunway.ai/v1alpha1
kind: InferenceProviderConfig
metadata:
  name: dynamo
  annotations:
    airunway.ai/documentation: "https://github.com/ai-runway/airunway/tree/main/docs/providers/dynamo.md"
    airunway.ai/installation: |
      {
        "description": "NVIDIA Dynamo for high-performance GPU inference",
        "defaultNamespace": "dynamo-system",
        "helmRepos": [
          { "name": "nvidia-ai-dynamo", "url": "https://helm.ngc.nvidia.com/nvidia/ai-dynamo" }
        ],
        "helmCharts": [
          {
            "name": "dynamo-platform",
            "chart": "https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-1.1.1.tgz",
            "namespace": "dynamo-system",
            "createNamespace": true,
            "values": { "global.grove.install": true }
          }
        ],
        "steps": [
          {
            "title": "Install Dynamo Platform",
            "command": "helm upgrade --install dynamo-platform https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-1.1.1.tgz --namespace dynamo-system --create-namespace --set-json global.grove.install=true",
            "description": "Install the Dynamo platform operator with bundled Grove and CRDs"
          }
        ]
      }
spec:
  capabilities:
    engines:
      - name: vllm
        servingModes: [aggregated, disaggregated]
        gpuSupport: true
        requiresCRD: true                            # Optional; nil is treated as true for backward compatibility
        gateway:                                     # Optional: per-engine gateway capabilities
          managesInferencePool: true                 # Provider creates and owns the InferencePool/EPP
          inferencePoolNamePattern: "{name}-pool"    # Pool naming pattern ({name}, {namespace} accepted)
          inferencePoolNamespace: "{namespace}"      # Namespace for provider's InferencePool
      - name: sglang
        servingModes: [aggregated, disaggregated]
        gpuSupport: true
        gateway:
          managesInferencePool: true
          inferencePoolNamePattern: "{name}-pool"
          inferencePoolNamespace: "{namespace}"
      - name: trtllm
        servingModes: [aggregated]
        gpuSupport: true
        gateway:
          managesInferencePool: true
          inferencePoolNamePattern: "{name}-pool"
          inferencePoolNamespace: "{namespace}"
  selectionRules:
    - condition: "spec.serving.mode == 'disaggregated'"
      priority: 100
status:
  ready: true
  version: "dynamo-provider:v0.2.0"
```

### Provider Metadata and Capabilities Annotations

Providers should declare scheduling capabilities in `spec.capabilities`. They may also mirror display and discovery metadata in annotations for dashboard clients and older integrations.

| Annotation | Type | Description |
|---|---|---|
| `airunway.ai/display-name` | string | Human-friendly provider name shown in the UI. |
| `airunway.ai/description` | string | Short provider description shown in runtime/provider lists. |
| `airunway.ai/default-namespace` | string | Default namespace suggested by the UI for provider workloads or installation. |
| `airunway.ai/documentation-url` | string | Canonical URL to provider documentation. |
| `airunway.ai/documentation` | string | Backward-compatible documentation URL fallback. |
| `airunway.ai/capabilities` | JSON string | Optional compatibility mirror of provider capabilities. New controllers should keep `spec.capabilities` authoritative. |
| `airunway.ai/health` | JSON string | Optional CRD/operator/status probes used by the dashboard to check live provider health. |

### Installation Metadata

| Annotation | Type | Description |
|---|---|---|
| `airunway.ai/installation` | JSON string | Installation metadata (description, defaultNamespace, helmRepos, helmCharts, steps). The backend parses this JSON to show installation commands and steps in the UI. |

## See also

- [Architecture Overview](architecture.md)
- [Controller Architecture](controller-architecture.md)
