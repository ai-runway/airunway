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
  engine:
    type: vllm                   # vllm, sglang, trtllm, llamacpp (optional, auto-selected)
    contextLength: 32768
    trustRemoteCode: false
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
  gateway:
    enabled: true                # Optional: defaults to true when Gateway detected
    modelName: ""                # Optional: override model name for routing
  model:
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
```

> **Note:** If `gateway.enabled` is explicitly set to `true` but the Gateway API Inference Extension CRDs are not installed, the controller sets a `GatewayReady=False` condition with reason `CRDsNotAvailable`. This surfaces as a status warning on the `ModelDeployment`.

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

Cluster-scoped resource for provider registration. Each provider controller self-registers its `InferenceProviderConfig` at startup, declaring capabilities and selection rules in `spec`, and installation/documentation metadata in `metadata.annotations`:

```yaml
apiVersion: airunway.ai/v1alpha1
kind: InferenceProviderConfig
metadata:
  name: dynamo
  annotations:
    airunway.ai/documentation: "https://github.com/kaito-project/dynamo-provider"
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

### Annotations

| Annotation | Type | Description |
|---|---|---|
| `airunway.ai/documentation` | string | URL to provider documentation |
| `airunway.ai/installation` | JSON string | Installation metadata (description, defaultNamespace, helmRepos, helmCharts, steps). The backend parses this JSON to show installation commands and steps in the UI. |

## AgentProviderConfig

Cluster-scoped resource for agent framework registration. Capabilities stay in `spec`, while marketplace metadata and install guidance are carried in annotations.

```yaml
apiVersion: airunway.ai/v1alpha1
kind: AgentProviderConfig
metadata:
  name: kagent
  annotations:
    airunway.ai/agent-catalog: |
      [
        {
          "name": "kagent-k8s-sre",
          "title": "Kubernetes SRE (Kagent)",
          "description": "Diagnose deployments, pods, and networking.",
          "tags": ["devops", "observability"]
        }
      ]
    airunway.ai/install-instructions: "Install the Kagent operator before deploying agents with this framework."
spec:
  capabilities:
    backend: crd
    requiresOperator: true
    operatorAPIGroup: kagent.dev
    modelBindingModes: [deploymentRef, gatewayEndpoint, externalAPI]
    protocols: [mcp, a2a, openaiTools]
status:
  ready: true
  version: "kagent-provider:v0.1.0"
```

### AgentProviderConfig annotations

| Annotation | Type | Description |
|---|---|---|
| `airunway.ai/agent-catalog` | JSON string | Catalog entries shown in the agent marketplace UI. Value must be a JSON array of items with unique `name` and non-empty `title`. |
| `airunway.ai/install-instructions` | string | Plain-text install guidance shown when `status.conditions[type=Ready].reason` is `OperatorNotInstalled`. |

## AgentDeployment model binding behavior

For `spec.model.deploymentRef`, the core controller resolves the model binding in this order:

1. If `ModelDeployment.status.gateway.gatewayName` and `gatewayNamespace` are present, use the in-cluster gateway service URL `http://<gatewayName>.<gatewayNamespace>.svc.cluster.local/v1`.
2. Else if `ModelDeployment.status.gateway.endpoint` is present, use that endpoint (normalized to an OpenAI-compatible `/v1` base URL).
3. Else fall back to the model Service endpoint from `ModelDeployment.status.endpoint`.

The resolved `status.modelBinding.modelName` prefers `status.gateway.modelName`, then `spec.model.servedName`, then `spec.model.id`.

For keyless in-cluster `deploymentRef` bindings, core leaves `status.modelBinding.credentialsRef` empty. Container backends inject `OPENAI_API_KEY=not-required` directly, while CRD backends provision an Airunway-managed per-agent no-auth Secret and reference it in their rendered CRs.

`spec.provider.overrides` is an escape hatch for validated security-context overrides. Supported sections are `workload` and `container`, each allowing only `podSecurityContext` and `securityContext` keys with allow-listed security fields.

## See also

- [Architecture Overview](architecture.md)
- [Controller Architecture](controller-architecture.md)
