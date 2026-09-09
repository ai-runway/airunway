# Web UI Architecture

The Web UI is an **optional, swappable** layer. The backend REST API is the integration boundary — any frontend that speaks HTTP/JSON can replace the bundled React UI without changes to the backend or controller.

## API Boundary

```
  Any Frontend                     Backend API                  Kubernetes
 ┌────────────┐    HTTP/JSON     ┌─────────────┐   K8s API   ┌────────────┐
 │ React UI   │───────────────▶  │  Hono/Bun   │────────────▶│  API       │
 │ Headlamp   │  Bearer token    │             │             │  Server    │
 │ Custom UI  │◀──────────────── │  /api/*     │◀────────────│            │
 └────────────┘                  └──────┬──────┘             └────────────┘
 └────────────┘                         │
                                        │ TokenReview
                                        ▼
                                  ┌─────────────┐
                                  │ Auth        │
                                  │ Middleware  │
                                  └─────────────┘
```

**Key API endpoints:**

| Endpoint                    | Purpose                              |
| --------------------------- | ------------------------------------ |
| `GET /api/health`           | Cluster connectivity check           |
| `GET/POST /api/deployments` | ModelDeployment CRUD                 |
| `GET /api/models`           | Model catalog (HuggingFace + custom) |
| `GET /api/runtimes`         | Runtime/provider status              |
| `POST /api/installation/*`  | Helm-based runtime installation      |
| `GET /api/settings`         | Cluster configuration                |

## Supported Frontends

The same backend API supports multiple frontends simultaneously:

| Frontend                   | How it connects                 | Notes                                 |
| -------------------------- | ------------------------------- | ------------------------------------- |
| **Bundled React UI**       | Same-origin (served by backend) | Default, ships in container           |
| **Headlamp Plugin**        | In-cluster service URL          | Uses `@airunway/shared` API client |
| **Custom UI / CLI**        | Any HTTP client                 | Backend is a standard REST API        |
| **kubectl**                | Bypasses backend entirely       | Works directly with CRDs on K8s API   |

## Authentication Flow

When `AUTH_ENABLED=true`, the system uses Kubernetes OIDC tokens:

```
┌──────────┐    airunway login    ┌─────────────┐
│   CLI    │ ───────────────────────▶│  kubeconfig │
│          │◀───────────────────────│  (OIDC)     │
└────┬─────┘    extract token        └─────────────┘
     │
     │ open browser with #token=...
     ▼
┌──────────┐    save to localStorage  ┌─────────────┐
│ Browser  │ ────────────────────────▶│  Frontend   │
│          │                          │  (React)    │
└──────────┘                          └──────┬──────┘
                                             │
              Authorization: Bearer <token>  │
                                             ▼
                                      ┌─────────────┐
                                      │  Backend    │
                                      │  (Hono)     │
                                      └──────┬──────┘
                                             │
                          TokenReview API    │
                                             ▼
                                      ┌─────────────┐
                                      │  Kubernetes │
                                      │  API Server │
                                      └─────────────┘
```

## Data Models

### Model (Catalog Entry)
```typescript
interface Model {
  id: string;                    // HuggingFace model ID
  name: string;                  // Display name
  description: string;
  size: string;                  // Parameter count (e.g., "0.6B")
  task: 'text-generation' | 'chat';
  contextLength?: number;
  supportedEngines: Engine[];
  minGpuMemory?: string;
  gated?: boolean;               // Requires HuggingFace auth
  // Fields from HuggingFace search
  estimatedGpuMemory?: string;   // Estimated GPU memory (e.g., "16GB")
  estimatedGpuMemoryGb?: number; // Numeric GPU memory for comparisons
  parameterCount?: number;       // Parameter count from safetensors
  fromHfSearch?: boolean;        // True if from HF search (not curated)
}
```

### DeploymentConfig
```typescript
interface DeploymentConfig {
  name: string;                  // K8s resource name
  namespace: string;
  modelId: string;
  engine: 'vllm' | 'sglang' | 'trtllm';
  mode: 'aggregated' | 'disaggregated';
  replicas: number;
  hfTokenSecret: string;
  enforceEager: boolean;
  enablePrefixCaching: boolean;
  trustRemoteCode: boolean;
}
```

### DeploymentStatus
```typescript
interface DeploymentStatus {
  name: string;
  namespace: string;
  modelId: string;
  engine: Engine;
  phase: 'Pending' | 'Deploying' | 'Running' | 'Failed' | 'Terminating';
  replicas: { desired: number; ready: number; available: number; };
  pods: PodStatus[];
  createdAt: string;
}
```

## Configuration Storage

Settings are persisted in a Kubernetes ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: airunway-config
  namespace: airunway-system
data:
  config.json: |
    {
      "defaultNamespace": "airunway-system"
    }
```

**Note:** Each deployment specifies its own runtime (`provider` field). There is no global "active provider" - users select the runtime when creating a deployment.

## Frontend Architecture

### Component Hierarchy
```
App
├── MainLayout
│   ├── Header (cluster status, warnings)
│   ├── Sidebar (navigation)
│   └── Page Content
│       ├── ModelsPage (tabs: Curated / HuggingFace Search)
│       │   ├── ModelGrid (curated models)
│       │   └── HfModelSearch (HuggingFace search with GPU fit indicators)
│       ├── DeployPage (GPU capacity warnings)
│       ├── DeploymentsPage
│       ├── DeploymentDetailsPage
│       ├── SettingsPage
```

### State Management
- **Server State**: TanStack Query for API data with caching
- **Local State**: React useState for UI state
- **Persistent State**: Browser localStorage for user preferences

## Backend Services

### KubernetesService
Handles all Kubernetes API interactions:
- List/create/delete custom resources for all providers
- Get pod status and logs
- Check cluster connectivity
- Namespace and node management
- Check GPU availability on nodes (`nvidia.com/gpu` resources)
- Detect GPU memory from node labels (`nvidia.com/gpu.memory` or `nvidia.com/gpu.product`)
- Get detailed GPU capacity with per-node and per-pool breakdown
- Check GPU Operator installation status (CRDs, pods)
- Get pod failure reasons from Kubernetes Events
- Regularly uninstall Helm releases while preserving namespaces, CRDs, and
  custom resources; the explicit complete-removal API deletes declared CRDs
  only after ownership and custom-resource safety checks pass

### MetricsService
Fetches and processes Prometheus metrics from inference deployments:
- Connects to deployment metrics endpoints (when running in-cluster)
- Parses Prometheus text format
- Supports vLLM and llama.cpp metric formats
- Handles provider-specific metric configurations

### AutoscalerService
Detects and monitors cluster autoscaler:
- Detects autoscaler type (AKS managed, self-managed Cluster Autoscaler)
- Parses autoscaler status from ConfigMap
- Reports node group health and scaling status

### HuggingFaceService
Handles HuggingFace Hub API interactions:
- Search models with text-generation pipeline
- Filter by architecture compatibility (vLLM, SGLang, TRT-LLM)
- Estimate GPU memory from parameter count (~2GB/billion params × 1.2 overhead)
- Extract parameter counts from safetensors metadata
- OAuth token exchange for gated model access

### ConfigService
Manages application configuration:
- Read/write ConfigMap
- Get active provider
- Persist provider settings

### HelmService
Handles Helm CLI operations:
- Check Helm availability and version
- Add/update repositories
- Install/upgrade/uninstall charts with real-time output
- Detect stuck/pending releases and handle cleanup
- Install NVIDIA GPU Operator (`gpu-operator` namespace)

### AuthService
Handles authentication when `AUTH_ENABLED=true`:
- Validate tokens via Kubernetes TokenReview API
- Extract OIDC tokens from kubeconfig (for CLI login)
- Generate magic link URLs for browser authentication
- Store/load/clear credentials locally (`~/.airunway/credentials.json`)

### RegistryService
Manages in-cluster container registry for KAITO image builds:
- Deploy and manage registry Deployment and Service
- Check registry readiness
- Generate registry URLs for in-cluster access

### BuildKitService
Manages BuildKit builder for KAITO custom images:
- Deploy BuildKit using Kubernetes driver
- Check builder status and readiness
- Build custom AIKit images from HuggingFace GGUF models

### AikitService
Handles KAITO/AIKit image operations:
- List available pre-made GGUF models
- Build custom images from HuggingFace GGUF files
- Generate image references for deployments

### AIConfiguratorService
Interfaces with NVIDIA AI Configurator for optimal inference configuration:
- Check if AI Configurator CLI is available locally (with 5-minute caching)
- Analyze model + GPU combinations to get optimal settings (tensor parallelism, batch size, etc.)
- Parse AI Configurator CSV output into deployment configuration
- Support aggregated and disaggregated serving modes
- Normalize GPU product labels to AI Configurator format
- Provide sensible defaults when AI Configurator is unavailable
- Input validation to prevent command injection attacks
- Automatic temp directory cleanup with try/finally pattern

### CloudPricingService
Fetches real-time pricing from cloud provider APIs:
- Azure Retail Prices API integration (no auth required)
- In-memory caching with 1-hour TTL and LRU eviction
- Provider detection from instance type naming conventions
- GPU info extraction for Azure GPU instance types
- Retry logic with exponential backoff and timeout handling
- AWS and GCP pricing API support (planned)

### CostEstimationService
Handles GPU cost estimation and normalization:
- GPU model normalization (e.g., "NVIDIA-A100-SXM4-80GB" → "A100-80GB")
- GPU info lookup (memory, generation)
- Node pool cost estimation with real-time pricing integration
- Fallback to static estimates when cloud pricing unavailable

---

## See also

- [Architecture Overview](architecture.md)
- [API Reference](api.md)
