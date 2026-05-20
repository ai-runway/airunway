# API Reference

AI Runway provides two APIs for managing deployments:

1. **CRD API** (Recommended) - Create `ModelDeployment` custom resources directly via kubectl
2. **REST API** - Web UI backend API for browser-based management

## CRD API (Kubernetes Native)

The preferred way to deploy models is via the `ModelDeployment` CRD:

```bash
# Create a deployment
kubectl apply -f - <<EOF
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: qwen-demo
  namespace: default
spec:
  model:
    id: "Qwen/Qwen3-0.6B"
    source: huggingface
  engine:
    type: vllm
  resources:
    gpu:
      count: 1
  scaling:
    replicas: 1
EOF

# List deployments
kubectl get modeldeployments

# Check status
kubectl describe modeldeployment qwen-demo

# Delete deployment
kubectl delete modeldeployment qwen-demo
```

See [controller-architecture.md](controller-architecture.md) for controller internals and [providers.md](providers.md) for provider selection.

### ModelDeployment Spec Reference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `model.id` | string | Yes (when source=huggingface) | — | HuggingFace model ID |
| `model.source` | string | No | `huggingface` | `huggingface` or `custom` |
| `model.servedName` | string | No | Model ID basename | API-facing model name |
| `engine.type` | string | No | Auto-selected | `vllm`, `sglang`, `trtllm`, or `llamacpp`. If omitted, auto-selected from provider capabilities |
| `engine.contextLength` | int | No | Model default | Max context length |
| `engine.trustRemoteCode` | bool | No | `false` | Allow remote code (vLLM/SGLang only) |
| `engine.args` | map[string]string | No | `{}` | Engine-specific CLI flags |
| `provider.name` | string | No | Auto-selected | `dynamo`, `kaito`, or `kuberay`, or `llmd` |
| `provider.overrides` | object | No | `{}` | Provider-specific escape hatch |
| `serving.mode` | string | No | `aggregated` | `aggregated` or `disaggregated` |
| `scaling.replicas` | int | No | `1` | Replicas (aggregated mode) |
| `scaling.prefill` | object | No | — | Prefill scaling (disaggregated mode) |
| `scaling.decode` | object | No | — | Decode scaling (disaggregated mode) |
| `resources.gpu.count` | int | No | `0` | GPU count |
| `resources.gpu.type` | string | No | `nvidia.com/gpu` | GPU resource name |
| `resources.memory` | string | No | — | Memory request |
| `resources.cpu` | string | No | — | CPU request |
| `image` | string | No | Provider default | Custom container image |
| `env` | []EnvVar | No | `[]` | Environment variables |
| `podTemplate.metadata.labels` | map | No | `{}` | Labels for pods |
| `podTemplate.metadata.annotations` | map | No | `{}` | Annotations for pods |
| `secrets.huggingFaceToken` | string | No | — | K8s secret name for HF token |
| `nodeSelector` | map | No | `{}` | Node selector |
| `tolerations` | []Toleration | No | `[]` | Tolerations |
| `gateway.enabled` | *bool | No | `true` (when Gateway detected) | Enable/disable gateway integration |
| `gateway.modelName` | string | No | Model served name or ID | Override model name for gateway routing |

### Update Semantics

When updating a `ModelDeployment`, changes are handled based on field type:

**Identity fields** — changing these triggers delete + recreate (brief downtime):
- `model.id`, `model.source`, `engine.type` (once set), `provider.name`, `serving.mode`

**Config fields** — changed in-place without recreation:
- `model.servedName`, `scaling.*`, `env`, `resources`, `engine.args`, `engine.contextLength`, `image`, `secrets.*`, `podTemplate.metadata`, `nodeSelector`, `tolerations`, `provider.overrides`

### API Versioning

| Version | Status | Stability |
|---------|--------|-----------|
| `v1alpha1` | Current | Experimental — breaking changes allowed |
| `v1beta1` | Planned | Feature complete — breaking changes with deprecation warnings |
| `v1` | Future | Stable — no breaking changes, long-term support |

### Engine-Specific Parameters

Common concepts are abstracted via `engine.contextLength` and `engine.trustRemoteCode`. For engine-specific flags, use `engine.args`:

**Context length mapping:**

| Engine | CLI flag | Default |
|--------|----------|---------|
| vLLM | `--max-model-len` | Model default |
| SGLang | `--context-length` | Model default |
| TensorRT-LLM | Build-time config | — |
| llama.cpp | `--ctx-size` | Model max |

**Quantization** (via `engine.args`):
```yaml
engine:
  type: vllm
  args:
    quantization: "awq"    # awq, gptq, squeezellm, fp8
```

**GPU memory utilization** (via `engine.args`):

| Engine | Arg key | Default |
|--------|---------|---------|
| vLLM | `gpu-memory-utilization` | `0.9` |
| SGLang | `mem-fraction-static` | `0.88` |

### Example Transformations

#### GPU Deployment → Dynamo (auto-selected)

```yaml
# User creates:
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: llama-8b
spec:
  model:
    id: "meta-llama/Llama-3.1-8B-Instruct"
    source: huggingface
  engine:
    type: vllm
    contextLength: 8192
  scaling:
    replicas: 1
  resources:
    gpu:
      count: 1
    memory: "32Gi"
  secrets:
    huggingFaceToken: "hf-token"

# Controller creates DynamoGraphDeployment with:
#   - Frontend service (router)
#   - VllmWorker with 1 GPU, 32Gi memory
#   - vLLM runtime image
# Status:
#   provider.name: dynamo
#   provider.selectedReason: "default → dynamo (GPU inference default)"
#   endpoint.service: llama-8b-frontend, port: 8000
```

#### CPU Deployment → KAITO (auto-selected)

```yaml
# User creates:
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: gemma-cpu
spec:
  model:
    id: "google/gemma-3-1b-it-qat-q8_0-gguf"
    source: huggingface
  engine:
    type: llamacpp
  scaling:
    replicas: 1
  resources:
    gpu:
      count: 0
    memory: "16Gi"
    cpu: "8"
  image: "ghcr.io/sozercan/llama-cpp-runner:latest"

# Controller creates KAITO Workspace with:
#   - llama.cpp container, CPU-only
# Status:
#   provider.name: kaito
#   provider.selectedReason: "no GPU requested → kaito (only CPU provider)"
#   endpoint.service: gemma-cpu, port: 80
```

#### Disaggregated P/D → Dynamo (explicit)

```yaml
# User creates:
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: llama-70b-pd
spec:
  model:
    id: "meta-llama/Llama-3.1-70B-Instruct"
  provider:
    name: dynamo
    overrides:
      routerMode: "kv"
      frontend:
        replicas: 2
  engine:
    type: vllm
  serving:
    mode: disaggregated
  scaling:
    prefill:
      replicas: 2
      gpu:
        count: 4
      memory: "128Gi"
    decode:
      replicas: 4
      gpu:
        count: 2
      memory: "64Gi"
  secrets:
    huggingFaceToken: "hf-token"

# Controller creates DynamoGraphDeployment with:
#   - Frontend (2 replicas, KV routing)
#   - VllmPrefillWorker (2 replicas, 4 GPUs each)
#   - VllmDecodeWorker (4 replicas, 2 GPUs each)
# Status:
#   provider.name: dynamo
#   replicas.desired: 6 (2 prefill + 4 decode)
#   endpoint.service: llama-70b-pd-frontend, port: 8000
```

---

## REST API (Web UI)

Base URL: `http://localhost:3001/api`

## Health & Status

### GET /health
Health check endpoint.

**Response:**
```json
{
  "status": "healthy",
  "timestamp": "2025-01-15T10:30:00.000Z"
}
```

### GET /health/version
Get build version information.

**Response:**
```json
{
  "version": "v1.0.0",
  "buildTime": "2025-01-15T10:00:00.000Z",
  "gitCommit": "abc1234"
}
```

### GET /cluster/status
Get Kubernetes cluster connection status.

**Response:**
```json
{
  "connected": true,
  "namespace": "airunway-system",
  "providerId": "dynamo",
  "providerInstalled": true
}
```

### GET /cluster/nodes
Get list of cluster nodes with GPU information.

**Response:**
```json
{
  "nodes": [
    {
      "name": "gpu-node-1",
      "ready": true,
      "gpuCount": 2
    },
    {
      "name": "cpu-node-1",
      "ready": true,
      "gpuCount": 0
    }
  ]
}
```

## Settings

### GET /settings
Get current settings and available providers.

**Response:**
```json
{
  "config": {
    "defaultNamespace": "airunway-system"
  },
  "auth": {
    "enabled": false
  }
}
```

### PUT /settings
Update application settings.

**Request Body:**
```json
{
  "defaultNamespace": "my-namespace"
}
```

## Installation

### GET /installation/helm/status
Check if Helm CLI is available.

**Response:**
```json
{
  "available": true,
  "version": "v3.14.0"
}
```

### GET /installation/providers/:id/status
Get provider installation status.

**Response:**
```json
{
  "providerId": "dynamo",
  "providerName": "Dynamo",
  "installed": true,
  "crdFound": true,
  "operatorRunning": true,
  "version": "dynamo-provider:v0.2.0",
  "message": "Dynamo is installed and running"
}
```

### GET /installation/providers/:id/commands
Get manual installation commands for a provider.

**Response:**
```json
{
  "commands": [
    "helm repo add nvidia-ai-dynamo https://helm.ngc.nvidia.com/nvidia/ai-dynamo",
    "helm repo update",
    "helm install dynamo-platform https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-1.0.2.tgz --namespace dynamo-system --create-namespace --set-json global.grove.install=true"
  ]
}
```

### POST /installation/providers/:id/install
Install a provider via Helm.

**Response:**
```json
{
  "success": true,
  "message": "Provider installed successfully"
}
```

### POST /installation/providers/:id/uninstall
Uninstall a provider (preserves CRDs by default).

**Response:**
```json
{
  "success": true,
  "message": "Provider uninstalled (CRDs preserved - use 'Uninstall CRDs' for complete removal)",
  "installationStatus": {
    "installed": false,
    "crdFound": true,
    "operatorRunning": false
  },
  "results": [
    {
      "step": "Uninstall Helm chart: kaito-workspace",
      "success": true,
      "output": "release \"kaito-workspace\" uninstalled"
    }
  ]
}
```

### POST /installation/providers/:id/uninstall-crds
Delete CRDs for a provider (complete removal).

**Response:**
```json
{
  "success": true,
  "message": "Provider CRDs uninstalled",
  "installationStatus": {
    "installed": false,
    "crdFound": false,
    "operatorRunning": false
  },
  "results": [
    {
      "step": "Delete CRD: workspaces.kaito.sh",
      "success": true,
      "output": "CRD workspaces.kaito.sh deleted"
    }
  ]
}
```

**Notes:**
- This is a destructive operation - existing workloads using the CRDs will be affected
- Use regular uninstall first to remove Helm releases while preserving CRDs
- Use this endpoint only when you want complete removal

### GET /installation/gpu-operator/status
Check NVIDIA GPU Operator installation status and GPU availability.

**Response:**
```json
{
  "installed": true,
  "crdFound": true,
  "operatorRunning": true,
  "gpusAvailable": true,
  "totalGPUs": 4,
  "gpuNodes": ["node-1", "node-2"],
  "message": "GPUs enabled: 4 GPU(s) on 2 node(s)",
  "helmCommands": [
    "helm repo add nvidia https://helm.ngc.nvidia.com/nvidia",
    "helm repo update",
    "helm install gpu-operator nvidia/gpu-operator --namespace gpu-operator --create-namespace"
  ]
}
```

### GET /installation/gpu-capacity
Get detailed GPU capacity information for the cluster.

**Response:**
```json
{
  "totalGpus": 4,
  "allocatedGpus": 1,
  "availableGpus": 3,
  "maxContiguousAvailable": 2,
  "totalMemoryGb": 80,
  "nodes": [
    {
      "nodeName": "gpu-node-1",
      "totalGpus": 2,
      "allocatedGpus": 1,
      "availableGpus": 1
    },
    {
      "nodeName": "gpu-node-2",
      "totalGpus": 2,
      "allocatedGpus": 0,
      "availableGpus": 2
    }
  ]
}
```

**Notes:**
- `totalMemoryGb` is detected from `nvidia.com/gpu.memory` node label (MiB converted to GB)
- Falls back to detecting memory from `nvidia.com/gpu.product` label if not available
- Used by frontend to show GPU fit indicators for HuggingFace search results

### POST /installation/gpu-operator/install
Install the NVIDIA GPU Operator via Helm.

**Response:**
```json
{
  "success": true,
  "message": "NVIDIA GPU Operator installed successfully",
  "status": {
    "installed": true,
    "crdFound": true,
    "operatorRunning": true,
    "gpusAvailable": false,
    "totalGPUs": 0,
    "gpuNodes": [],
    "message": "GPU Operator installed but no GPUs detected on nodes"
  }
}
```

### GET /installation/gpu-capacity/detailed
Get detailed GPU capacity with node pool breakdown.

**Response:**
```json
{
  "totalGpus": 4,
  "allocatedGpus": 1,
  "availableGpus": 3,
  "maxContiguousAvailable": 2,
  "maxNodeGpuCapacity": 2,
  "gpuNodeCount": 2,
  "totalMemoryGb": 80,
  "nodePools": [
    {
      "name": "gpu",
      "gpuCount": 4,
      "nodeCount": 2,
      "availableGpus": 3,
      "gpuModel": "NVIDIA-A100-SXM4-80GB"
    }
  ]
}
```

**Notes:**
- Groups nodes by node pool (agentpool, kubernetes.azure.com/agentpool, etc.)
- Shows per-pool GPU capacity and availability
- Used for capacity planning and autoscaler guidance

## Autoscaler

### GET /autoscaler/detection
Detect cluster autoscaler type and health status.

**Response:**
```json
{
  "detected": true,
  "type": "aks-managed",
  "healthy": true,
  "message": "Cluster Autoscaler running on 1 node group(s)",
  "nodeGroupCount": 1
}
```

**Autoscaler Types:**
- `aks-managed` - AKS managed cluster autoscaler (Azure)
- `cluster-autoscaler` - Self-managed cluster autoscaler (any cloud)
- `none` - No autoscaler detected

**Detection Logic:**
- Primary: Checks for `cluster-autoscaler-status` ConfigMap in `kube-system`
- Fallback: Checks for `cluster-autoscaler` Deployment
- Health: ConfigMap timestamp < 5 minutes = healthy

### GET /autoscaler/status
Get detailed autoscaler status from ConfigMap.

**Response:**
```json
{
  "health": "Healthy",
  "lastUpdated": "2025-01-15T10:30:00Z",
  "nodeGroups": [
    {
      "name": "gpu",
      "health": "Healthy",
      "minSize": 1,
      "maxSize": 10,
      "currentSize": 2
    }
  ]
}
```

## Models

### GET /models
Get the curated model catalog.

**Response:**
```json
{
  "models": [
    {
      "id": "Qwen/Qwen3-0.6B",
      "name": "Qwen3 0.6B",
      "description": "Small, efficient model ideal for development",
      "size": "0.6B",
      "task": "text-generation",
      "contextLength": 32768,
      "supportedEngines": ["vllm", "sglang", "trtllm"],
      "minGpuMemory": "4GB",
      "gated": false
    },
    {
      "id": "meta-llama/Llama-3.2-1B-Instruct",
      "name": "Llama 3.2 1B Instruct",
      "description": "Compact Llama model optimized for instruction following",
      "size": "1B",
      "task": "chat",
      "contextLength": 131072,
      "supportedEngines": ["vllm", "sglang", "trtllm"],
      "minGpuMemory": "4GB",
      "gated": true
    }
  ]
}
```

**Model Fields:**
- `id` - HuggingFace model ID (e.g., "Qwen/Qwen3-0.6B")
- `name` - Display name
- `description` - Brief description
- `size` - Parameter count (e.g., "0.6B")
- `task` - Model task type ("text-generation", "chat", "fill-mask")
- `contextLength` - Maximum context length
- `supportedEngines` - Compatible inference engines
- `minGpuMemory` - Minimum GPU memory required
- `minGpus` - Minimum number of GPUs required (default: 1)
- `gated` - Whether model requires HuggingFace authentication (true for Llama, Mistral, etc.)
- `estimatedGpuMemory` - Estimated GPU memory from HF search (e.g., "16GB")
- `estimatedGpuMemoryGb` - Numeric GPU memory for capacity comparisons
- `parameterCount` - Parameter count from safetensors metadata
- `fromHfSearch` - True if model came from HuggingFace search

### GET /models/:modelId/gguf-files
Get available GGUF files for a HuggingFace model.

**Headers:**
- `X-HF-Token` (optional) - HuggingFace token for gated models

**Response:**
```json
{
  "files": [
    {
      "filename": "model-Q8_0.gguf",
      "size": 1340000000
    }
  ]
}
```

### GET /models/search
Search HuggingFace Hub for compatible models.

**Query Parameters:**
- `q` (required) - Search query
- `limit` (optional) - Number of results (default: 20, max: 50)
- `offset` (optional) - Pagination offset

**Headers:**
- `Authorization: Bearer <hf_token>` (optional) - For accessing gated models

**Response:**
```json
{
  "models": [
    {
      "id": "meta-llama/Llama-3.1-8B-Instruct",
      "name": "Llama-3.1-8B-Instruct",
      "author": "meta-llama",
      "downloads": 1500000,
      "likes": 2500,
      "pipelineTag": "text-generation",
      "gated": true,
      "supportedEngines": ["vllm", "sglang", "trtllm"],
      "estimatedGpuMemory": "19.2GB",
      "estimatedGpuMemoryGb": 19.2,
      "parameterCount": 8000000000
    }
  ],
  "total": 150,
  "offset": 0,
  "limit": 20
}
```

**Notes:**
- Only returns models with `text-generation` pipeline tag
- Filters out models with incompatible architectures
- GPU memory estimated as: `(params × 2GB) × 1.2` for FP16 inference
- Results cached client-side for 60 seconds

## Deployments

### GET /deployments
List all deployments for the active provider.

**Query Parameters:**
- `namespace` (optional) - Filter by namespace

**Response:**
```json
{
  "deployments": [
    {
      "name": "qwen-deployment",
      "namespace": "airunway-system",
      "modelId": "Qwen/Qwen3-0.6B",
      "engine": "vllm",
      "phase": "Running",
      "replicas": { "desired": 1, "ready": 1, "available": 1 },
      "createdAt": "2024-01-15T10:30:00Z"
    }
  ]
}
```

### POST /deployments
Create a new deployment.

**Request Body:**
```json
{
  "name": "qwen-deployment",
  "namespace": "airunway-system",
  "provider": "dynamo",
  "modelId": "Qwen/Qwen3-0.6B",
  "engine": "vllm",
  "mode": "aggregated",
  "replicas": 1,
  "hfTokenSecret": "hf-token-secret",
  "enforceEager": true,
  "enablePrefixCaching": false,
  "trustRemoteCode": false
}
```

**Required Fields:**
- `name` - Kubernetes resource name
- `namespace` - Target namespace
- `provider` - Runtime provider (`dynamo`, `kuberay`, or `kaito`)
- `modelId` - HuggingFace model ID
- `engine` - Inference engine (`vllm`, `sglang`, or `trtllm` for Dynamo; `vllm` for KubeRay; not used for KAITO)
- `hfTokenSecret` - Name of the Kubernetes secret containing HuggingFace token

**Response:**
```json
{
  "message": "Deployment created successfully",
  "name": "qwen-deployment",
  "namespace": "airunway-system",
  "provider": "dynamo"
}
```

### GET /deployments/:name
Get deployment details including pod status.

**Query Parameters:**
- `namespace` (required)

**Response:**
```json
{
  "name": "qwen-deployment",
  "namespace": "airunway-system",
  "modelId": "Qwen/Qwen3-0.6B",
  "engine": "vllm",
  "provider": "dynamo",
  "phase": "Running",
  "replicas": { "desired": 1, "ready": 1, "available": 1 },
  "pods": [
    {
      "name": "qwen-deployment-worker-0",
      "phase": "Running",
      "ready": true,
      "restarts": 0
    }
  ],
  "createdAt": "2024-01-15T10:30:00Z"
}
```

### GET /deployments/:name/manifest
Get the Kubernetes manifest resources for a deployment.

**Query Parameters:**
- `namespace` (optional)

**Response:**
```json
{
  "resources": [
    {
      "kind": "ModelDeployment",
      "apiVersion": "airunway.ai/v1alpha1",
      "name": "qwen-deployment",
      "manifest": { }
    }
  ],
  "primaryResource": {
    "kind": "ModelDeployment",
    "apiVersion": "airunway.ai/v1alpha1"
  }
}
```

## Runtimes

### GET /runtimes/status
Get installation and health status of all runtimes.

**Response:**
```json
{
  "runtimes": [
    {
      "id": "dynamo",
      "name": "Dynamo",
      "installed": true,
      "healthy": true,
      "version": "dynamo-provider:v0.2.0",
      "message": "Provider ready"
    },
    {
      "id": "kuberay",
      "name": "KubeRay",
      "installed": false,
      "healthy": false,
      "message": "CRD not found"
    },
    {
      "id": "kaito",
      "name": "KAITO",
      "installed": true,
      "healthy": true,
      "version": "0.6.0",
      "message": "KAITO is installed and running"
    }
  ]
}
```

**Fields:**
- `id` - Runtime identifier (`dynamo`, `kuberay`, or `kaito`)
- `name` - Display name
- `installed` - Whether the CRD is installed
- `healthy` - Whether the operator pods are running
- `version` - Detected version (if available)
- `message` - Status message

**Notes:**
- Used by the frontend to show available runtimes in the deployment wizard
- Checks CRD existence and operator pod status for each provider

### DELETE /deployments/:name
Delete a deployment.

**Query Parameters:**
- `namespace` (required)

**Response:**
```json
{
  "success": true,
  "message": "Deployment deleted"
}
```

### GET /deployments/:name/pods
Get pods for a deployment.

**Query Parameters:**
- `namespace` (optional)

**Response:**
```json
{
  "pods": [
    {
      "name": "qwen-deployment-worker-0",
      "phase": "Running",
      "ready": true,
      "restarts": 0,
      "node": "gpu-node-1"
    }
  ]
}
```

### GET /deployments/:name/logs
Get logs from a deployment's pods.

**Query Parameters:**
- `namespace` (optional) - Deployment namespace
- `podName` (optional) - Specific pod to get logs from (defaults to first pod)
- `container` (optional) - Specific container name
- `tailLines` (optional) - Number of lines to return (default: 100, max: 10000)
- `timestamps` (optional) - Include timestamps in log lines (true/false)

**Response:**
```json
{
  "logs": "[INFO] Model loaded successfully\n[INFO] Server started on port 8000\n...",
  "podName": "qwen-deployment-worker-0",
  "container": "model"
}
```

**Notes:**
- ANSI color codes are automatically stripped from logs
- If no pods exist for the deployment, returns empty logs with a message

### GET /deployments/:name/metrics
Get Prometheus metrics from a deployment's inference service.

**Query Parameters:**
- `namespace` (optional) - Deployment namespace

**Response (available):**
```json
{
  "available": true,
  "timestamp": "2025-01-15T10:30:00.000Z",
  "metrics": [
    {
      "name": "vllm:num_requests_running",
      "value": 5,
      "labels": { "model": "Qwen/Qwen3-0.6B" }
    },
    {
      "name": "vllm:gpu_cache_usage_perc",
      "value": 45.2,
      "labels": {}
    }
  ]
}
```

**Response (off-cluster):**
```json
{
  "available": false,
  "error": "Metrics are only available when AI Runway is deployed inside the Kubernetes cluster.",
  "timestamp": "2025-01-15T10:30:00.000Z",
  "metrics": [],
  "runningOffCluster": true
}
```

**Notes:**
- Metrics require AI Runway to be running inside the cluster
- Supports both vLLM and llama.cpp metric formats
- Returns `runningOffCluster: true` when running locally

### GET /deployments/:name/pending-reasons
Get reasons why deployment pods are pending (unschedulable).

**Query Parameters:**
- `namespace` (optional) - Deployment namespace

**Response:**
```json
{
  "reasons": [
    {
      "reason": "FailedScheduling",
      "message": "0/3 nodes are available: 3 Insufficient nvidia.com/gpu",
      "isResourceConstraint": true,
      "resourceType": "gpu",
      "canAutoscalerHelp": true
    }
  ]
}
```

**Resource Types:**
- `gpu` - Insufficient GPU resources
- `cpu` - Insufficient CPU resources
- `memory` - Insufficient memory

**Notes:**
- Only returns reasons for pending pods
- `canAutoscalerHelp` indicates if cluster autoscaler can provision resources
- Taint and node selector issues will have `canAutoscalerHelp: false`

## HuggingFace OAuth

AI Runway supports HuggingFace OAuth with PKCE for secure token acquisition. This enables access to gated models (e.g., Llama, Mistral) without manually managing tokens.

### GET /oauth/huggingface/config
Get OAuth configuration for initiating HuggingFace sign-in.

**Response:**
```json
{
  "clientId": "e05817a1-7053-4b9e-b292-29cd219fccf8",
  "authorizeUrl": "https://huggingface.co/oauth/authorize",
  "scopes": ["openid", "profile", "read-repos"]
}
```

### POST /oauth/huggingface/start
Start an OAuth flow with PKCE. Generates a code verifier and state parameter.

**Request Body:**
```json
{
  "redirectUri": "http://localhost:3000/oauth/callback/huggingface"
}
```

**Response:**
```json
{
  "authorizationUrl": "https://huggingface.co/oauth/authorize?client_id=...&state=...",
  "state": "random-state-string"
}
```

### GET /oauth/huggingface/verifier/:state
Retrieve the PKCE code verifier for a given OAuth state. One-time use — the verifier is deleted after retrieval.

**Response:**
```json
{
  "codeVerifier": "pkce_code_verifier_string",
  "redirectUri": "http://localhost:3000/oauth/callback/huggingface"
}
```

### POST /oauth/huggingface/token
Exchange OAuth authorization code for access token using PKCE.

**Request Body:**
```json
{
  "code": "authorization_code_from_callback",
  "codeVerifier": "pkce_code_verifier_min_43_chars",
  "redirectUri": "http://localhost:3000/oauth/callback/huggingface"
}
```

**Response:**
```json
{
  "accessToken": "hf_xxxxx",
  "tokenType": "Bearer",
  "expiresIn": 3600,
  "scope": "openid profile read-repos",
  "user": {
    "id": "user123",
    "name": "username",
    "fullname": "Full Name",
    "email": "user@example.com",
    "avatarUrl": "https://huggingface.co/avatars/xxx.png"
  }
}
```

## HuggingFace Secrets

Manages HuggingFace tokens as Kubernetes secrets across provider namespaces.

### GET /secrets/huggingface/status
Get the status of HuggingFace token secrets across namespaces.

**Response:**
```json
{
  "configured": true,
  "namespaces": [
    { "name": "dynamo-system", "exists": true },
    { "name": "kuberay-system", "exists": true },
    { "name": "default", "exists": true }
  ],
  "user": {
    "id": "user123",
    "name": "username",
    "fullname": "Full Name"
  }
}
```

### POST /secrets/huggingface
Save HuggingFace access token as Kubernetes secrets in all required namespaces.

**Request Body:**
```json
{
  "accessToken": "hf_xxxxx"
}
```

**Response:**
```json
{
  "success": true,
  "message": "HuggingFace token saved successfully",
  "user": {
    "id": "user123",
    "name": "username",
    "fullname": "Full Name"
  },
  "results": [
    { "namespace": "dynamo-system", "success": true },
    { "namespace": "kuberay-system", "success": true },
    { "namespace": "default", "success": true }
  ]
}
```

### DELETE /secrets/huggingface
Delete HuggingFace token secrets from all namespaces.

**Response:**
```json
{
  "success": true,
  "message": "HuggingFace secrets deleted successfully",
  "results": [
    { "namespace": "dynamo-system", "success": true },
    { "namespace": "kuberay-system", "success": true },
    { "namespace": "default", "success": true }
  ]
}
```

## AIKit (KAITO Image Building)

Endpoints for building and managing KAITO/AIKit images for GGUF model deployment.

### GET /aikit/models
List available pre-made AIKit models.

**Response:**
```json
{
  "models": [
    {
      "id": "llama3.2-1b",
      "modelName": "Llama 3.2 1B",
      "image": "ghcr.io/kaito-project/aikit/llama3.2-1b:0.0.1",
      "license": "Llama"
    },
    {
      "id": "phi4-14b",
      "modelName": "Phi 4 14B",
      "image": "ghcr.io/kaito-project/aikit/phi4-14b:0.0.1",
      "license": "MIT"
    }
  ],
  "total": 15
}
```

### GET /aikit/models/:id
Get details for a specific pre-made model.

**Response:**
```json
{
  "id": "llama3.2-1b",
  "modelName": "Llama 3.2 1B",
  "image": "ghcr.io/kaito-project/aikit/llama3.2-1b:0.0.1",
  "license": "Llama"
}
```

### POST /aikit/build
Build an AIKit image from a HuggingFace GGUF model or get pre-made image reference.

**Request Body (Pre-made):**
```json
{
  "modelSource": "premade",
  "premadeModel": "llama3.2-1b"
}
```

**Request Body (HuggingFace GGUF):**
```json
{
  "modelSource": "huggingface",
  "modelId": "bartowski/gemma-3-1b-it-GGUF",
  "ggufFile": "gemma-3-1b-it-Q8_0.gguf",
  "imageName": "my-model",
  "imageTag": "v1"
}
```

**Response:**
```json
{
  "success": true,
  "imageRef": "registry.airunway-system.svc.cluster.local:5000/my-model:v1",
  "buildTime": 120,
  "wasPremade": false,
  "message": "AIKit image built successfully"
}
```

### POST /aikit/build/preview
Preview what image would be built (dry-run, no actual build).

**Response:**
```json
{
  "imageRef": "registry.airunway-system.svc.cluster.local:5000/my-model:v1",
  "wasPremade": false,
  "requiresBuild": true,
  "registryUrl": "registry.airunway-system.svc.cluster.local:5000"
}
```

### GET /aikit/infrastructure/status
Check build infrastructure (registry and BuildKit) status.

**Response:**
```json
{
  "ready": true,
  "registry": {
    "ready": true,
    "url": "registry.airunway-system.svc.cluster.local:5000",
    "message": "Registry is running"
  },
  "builder": {
    "exists": true,
    "ready": true,
    "running": true,
    "message": "BuildKit builder is ready"
  }
}
```

### POST /aikit/infrastructure/setup
Set up build infrastructure (deploy registry and BuildKit if needed).

**Response:**
```json
{
  "success": true,
  "message": "Build infrastructure is ready",
  "registry": {
    "url": "registry.airunway-system.svc.cluster.local:5000",
    "ready": true
  },
  "builder": {
    "name": "buildkit-airunway",
    "ready": true
  }
}
```

## AI Configurator

Endpoints for NVIDIA AI Configurator integration to get optimal inference configurations.

### GET /aiconfigurator/status
Check if AI Configurator CLI is available on the system.

**Response (available):**
```json
{
  "available": true,
  "version": "0.4.0"
}
```

**Response (unavailable):**
```json
{
  "available": false,
  "error": "AI Configurator CLI not found"
}
```

**Response (running in-cluster):**
```json
{
  "available": false,
  "runningInCluster": true,
  "error": "AI Configurator is only available when running AI Runway locally"
}
```

**Notes:**
- Status is cached for 5 minutes to avoid repeated CLI calls
- AI Configurator must be installed locally: https://github.com/ai-dynamo/aiconfigurator
- When running inside Kubernetes, returns `runningInCluster: true` (AI Configurator is local-only)

### POST /aiconfigurator/analyze
Analyze a model + GPU combination and return optimal configuration.

**Request Body:**
```json
{
  "modelId": "Qwen/Qwen3-0.6B",
  "gpuType": "H100-80GB",
  "gpuCount": 2,
  "optimizeFor": "throughput",
  "maxLatencyMs": 100
}
```

**Required Fields:**
- `modelId` - HuggingFace model ID (validated format: `org/model-name` or `model-name`)
- `gpuType` - GPU type (e.g., "A100-80GB", "H100", "L40S")
- `gpuCount` - Number of GPUs available (minimum: 1)

**Optional Fields:**
- `optimizeFor` - Optimization target: `"throughput"` (default) or `"latency"`
- `maxLatencyMs` - Target time-to-first-token latency constraint in milliseconds

**Response (success):**
```json
{
  "success": true,
  "config": {
    "tensorParallelDegree": 1,
    "pipelineParallelDegree": 1,
    "maxBatchSize": 256,
    "maxNumSeqs": 256,
    "gpuMemoryUtilization": 0.8,
    "maxModelLen": 5000
  },
  "mode": "aggregated",
  "replicas": 1,
  "warnings": [],
  "estimatedPerformance": {
    "throughputTokensPerSec": 8901.5,
    "latencyP50Ms": 187.99,
    "latencyP99Ms": 281.98,
    "gpuUtilization": 0.8
  },
  "backend": "vllm",
  "supportedBackends": ["vllm", "sglang", "trtllm"]
}
```

**Response (disaggregated mode):**
```json
{
  "success": true,
  "config": {
    "tensorParallelDegree": 1,
    "pipelineParallelDegree": 1,
    "maxBatchSize": 256,
    "maxNumSeqs": 256,
    "gpuMemoryUtilization": 0.8,
    "maxModelLen": 5000,
    "prefillTensorParallel": 1,
    "decodeTensorParallel": 1,
    "prefillReplicas": 1,
    "decodeReplicas": 1
  },
  "mode": "disaggregated",
  "replicas": 1,
  "warnings": [],
  "estimatedPerformance": {
    "throughputTokensPerSec": 8405.12,
    "latencyP50Ms": 25.42,
    "latencyP99Ms": 38.13,
    "gpuUtilization": 0.8
  },
  "backend": "vllm",
  "supportedBackends": ["vllm", "sglang", "trtllm"]
}
```

**Response (CLI unavailable - returns defaults):**
```json
{
  "success": false,
  "config": {
    "tensorParallelDegree": 2,
    "maxBatchSize": 256,
    "gpuMemoryUtilization": 0.9,
    "maxModelLen": 4096
  },
  "mode": "aggregated",
  "replicas": 1,
  "error": "AI Configurator CLI not found",
  "warnings": ["AI Configurator not available, using default configuration"]
}
```

**Modes:**
- `aggregated` - Traditional serving where prefill and decode run on same GPUs
- `disaggregated` - Prefill and decode separated for lower latency (NVIDIA Dynamo feature)

**Supported Backends by GPU:**
- H100: vLLM, SGLang, TensorRT-LLM
- A100, H200, L40S, B200, GB200: TensorRT-LLM only (vLLM data not available in AI Configurator)

### POST /aiconfigurator/normalize-gpu
Normalize a GPU product string to AI Configurator format.

**Request Body:**
```json
{
  "gpuProduct": "nvidia-a100-sxm4-80gb"
}
```

**Response:**
```json
{
  "gpuProduct": "nvidia-a100-sxm4-80gb",
  "normalized": "A100-80GB"
}
```

**Notes:**
- Useful for converting Kubernetes node GPU labels to AI Configurator expected format
- Handles various formats: NVIDIA prefixes, SXM/PCIe variants, Tesla prefixes

## Cost Estimation

Endpoints for real-time cloud pricing and cost estimation for GPU node pools.

### POST /costs/estimate
Estimate deployment cost based on GPU configuration (static estimate).

**Request Body:**
```json
{
  "gpuType": "A100-80GB",
  "gpuCount": 1,
  "replicas": 1,
  "hoursPerMonth": 730
}
```

**Required Fields:**
- `gpuType` - GPU model name (e.g., "A100-80GB", "H100", "T4")
- `gpuCount` - Number of GPUs per replica (minimum: 1)
- `replicas` - Number of replicas (minimum: 1)

**Optional Fields:**
- `hoursPerMonth` - Hours per month for cost calculation (1-744, default: 730)

**Response:**
```json
{
  "success": true,
  "breakdown": {
    "totalGpus": 1,
    "gpuModel": "A100-80GB",
    "normalizedGpuModel": "A100-80GB",
    "perGpu": { "hourly": 0, "monthly": 0 },
    "estimate": {
      "hourly": 0,
      "monthly": 0,
      "currency": "USD",
      "source": "static",
      "confidence": "low"
    },
    "notes": ["Use real-time pricing via /costs/node-pools for accurate cloud pricing"]
  }
}
```

**Notes:**
- Static pricing is deprecated; use `/costs/node-pools` for real-time cloud pricing
- Returns `confidence: "low"` to indicate static estimates should not be relied upon

### GET /costs/node-pools
Get cost estimates for all node pools using real-time cloud pricing.

**Query Parameters:**
- `gpuCount` (optional) - Number of GPUs per deployment (default: 1)
- `replicas` (optional) - Number of replicas (default: 1)
- `realtime` (optional) - Enable real-time pricing, set to "false" for static (default: true)
- `computeType` (optional) - Filter by "gpu" or "cpu" (default: "gpu")

**Response:**
```json
{
  "success": true,
  "nodePoolCosts": [
    {
      "poolName": "gpu",
      "gpuModel": "A100-80GB",
      "availableGpus": 4,
      "costBreakdown": {
        "totalGpus": 1,
        "gpuModel": "A100-80GB",
        "normalizedGpuModel": "A100-80GB",
        "perGpu": { "hourly": 0, "monthly": 0 },
        "estimate": {
          "hourly": 3.5,
          "monthly": 2555,
          "currency": "USD",
          "source": "cloud-api",
          "confidence": "high"
        },
        "notes": ["Real-time pricing from AZURE"]
      },
      "realtimePricing": {
        "instanceType": "Standard_NC24ads_A100_v4",
        "hourlyPrice": 3.5,
        "monthlyPrice": 2555,
        "currency": "USD",
        "region": "eastus",
        "source": "realtime"
      }
    }
  ],
  "pricingSource": "realtime-with-fallback",
  "cacheStats": {
    "size": 5,
    "ttlMs": 3600000,
    "maxEntries": 1000
  }
}
```

**Notes:**
- Fetches real-time pricing from Azure Retail Prices API
- Falls back to static estimates if cloud pricing unavailable
- Pricing is cached for 1 hour to reduce API calls
- AWS and GCP pricing not yet implemented

### GET /costs/instance-price
Get real-time pricing for a specific instance type.

**Query Parameters:**
- `instanceType` (required) - Cloud instance type (e.g., "Standard_NC24ads_A100_v4")
- `region` (optional) - Cloud region (e.g., "eastus")

**Response (success):**
```json
{
  "success": true,
  "price": {
    "instanceType": "Standard_NC24ads_A100_v4",
    "provider": "azure",
    "region": "eastus",
    "hourlyPrice": 3.5,
    "currency": "USD",
    "priceType": "ondemand",
    "gpuCount": 1,
    "gpuModel": "A100-80GB",
    "lastUpdated": "2025-01-01T00:00:00.000Z"
  },
  "cached": false
}
```

**Response (provider not detected):**
```json
{
  "success": false,
  "error": "Could not detect cloud provider for instance type: unknown-instance"
}
```

**Response (pricing not found):**
```json
{
  "success": false,
  "error": "Price not found",
  "provider": "azure"
}
```

**Provider Detection:**
- Azure: Instance types starting with `Standard_` or `Basic_`
- AWS: Instance types with format like `p4d.24xlarge`, `g5.xlarge` (not yet implemented)
- GCP: Instance types like `n1-standard-4`, `a2-highgpu-1g` (not yet implemented)

### GET /costs/gpu-models
Get list of supported GPU models with specifications.

**Response:**
```json
{
  "success": true,
  "models": [
    {
      "model": "A100-80GB",
      "memoryGb": 80,
      "generation": "Ampere"
    },
    {
      "model": "H100-80GB",
      "memoryGb": 80,
      "generation": "Hopper"
    },
    {
      "model": "T4",
      "memoryGb": 16,
      "generation": "Turing"
    }
  ],
  "note": "For actual pricing, use /costs/node-pools or /costs/instance-price for real-time cloud provider pricing"
}
```

**Notes:**
- Returns GPU specifications only (memory, generation)
- For real-time pricing, use `/costs/node-pools` or `/costs/instance-price` endpoints
- GPU models are used for normalization and capacity planning

### GET /costs/normalize-gpu
Normalize a GPU label to a standard GPU model name.

**Query Parameters:**
- `label` (required) - GPU label from Kubernetes node (e.g., "NVIDIA-A100-SXM4-80GB")

**Response:**
```json
{
  "success": true,
  "originalLabel": "NVIDIA-A100-SXM4-80GB",
  "normalizedModel": "A100-80GB",
  "gpuInfo": {
    "memoryGb": 80,
    "generation": "Ampere"
  }
}
```

**Notes:**
- Handles various GPU label formats: NVIDIA prefixes, SXM/PCIe variants, Tesla prefixes
- Returns GPU specifications when available

## Gateway

> **Authentication:** Gateway endpoints require a valid Bearer token when authentication is enabled (same as all other non-public API routes). Access control is governed by Kubernetes TokenReview. When auth is disabled (default single-cluster mode), these endpoints are publicly accessible.

### GET /gateway/status
Get Gateway API Inference Extension availability and endpoint.

**Response:**
```json
{
  "available": true,
  "endpoint": "http://10.0.0.1"
}
```

### GET /gateway/models
List all models accessible through the unified gateway endpoint.

**Response:**
```json
[
  {
    "name": "llama-3-8b",
    "deploymentName": "my-llama",
    "provider": "kaito",
    "ready": true
  }
]
```

## Error Responses

All endpoints return errors in this format:

```json
{
  "error": "Error message",
  "code": "ERROR_CODE",
  "details": {}
}
```

Common error codes:
- `CLUSTER_UNAVAILABLE` - Cannot connect to Kubernetes
- `PROVIDER_NOT_INSTALLED` - Active provider not installed
- `VALIDATION_ERROR` - Invalid request body
- `NOT_FOUND` - Resource not found
