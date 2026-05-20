# Development Guide

## Prerequisites

- [Go](https://go.dev) 1.23+ (for controller development)
- [Bun](https://bun.sh) 1.0+ (for Web UI development)
- Access to a Kubernetes cluster
- Helm CLI (for provider installation)
- kubectl configured with cluster access

## Quick Start

### Web UI Development
```bash
# Install dependencies
bun install

# Start development servers (frontend + backend)
bun run dev

# Development mode:
#   Frontend: http://localhost:5173 (Vite dev server, proxies API to backend)
#   Backend:  http://localhost:3001
#
# Production mode (compiled binary):
#   Single server: http://localhost:3001 (frontend embedded in backend)
```

### Controller Development
```bash
# Build the controller binary
make controller-build

# Run controller tests
make controller-test

# Run controller locally (uses your kubeconfig)
make controller-run

# Regenerate CRDs and deepcopy code after editing *_types.go files
make controller-generate

# Build the docker container image
make controller-docker-build CONTROLLER_IMG=<YOUR IMAGE>

# Defaults: PUSH=false and PLATFORM=linux/amd64

# Optional: push instead of load, or target a different platform
make controller-docker-build CONTROLLER_IMG=<YOUR IMAGE> PUSH=true PLATFORM=linux/amd64,linux/arm64

# Install CRDs into the cluster
make controller-install

# Deploy controller to cluster
make controller-deploy CONTROLLER_IMG=<YOUR IMAGE>
```

**Important**: After editing `controller/api/v1alpha1/*_types.go` files, always run:
```bash
cd controller && make manifests generate
```

## Building a Single Binary

The project can be compiled to a standalone executable that includes both the backend API and embedded frontend assets:

```bash
# Compile to single binary (includes frontend)
bun run compile

# Run the binary (serves both API and frontend on port 3001)
./dist/airunway

# Check version info
curl http://localhost:3001/api/health/version
```

The compile process:
1. Builds the frontend with Vite
2. Generates native Bun file imports in `backend/src/embedded-assets.ts`
3. Injects build-time constants (version, git commit, build time) via `--define`
4. Compiles everything into a single executable using `bun build --compile --minify --sourcemap`

The binary is completely self-contained with zero-copy file serving. The backend uses Hono on Bun for optimal performance.

### Cross-Compilation

Build for multiple platforms:

```bash
# Build for all platforms
make compile-all

# Or individual targets
make compile-linux     # linux-x64, linux-arm64
make compile-darwin    # darwin-x64, darwin-arm64
make compile-windows   # windows-x64

# With explicit version
VERSION=v1.0.0 bun run compile
```

Supported targets:
- `linux-x64`, `linux-arm64`
- `darwin-x64`, `darwin-arm64`
- `windows-x64`

## Controller Development

The controller is a Go-based Kubernetes operator built with [Kubebuilder](https://kubebuilder.io/).

### Project Structure
```
controller/
├── api/v1alpha1/           # CRD type definitions
│   ├── modeldeployment_types.go
│   └── inferenceproviderconfig_types.go
├── cmd/                    # Main entrypoint
├── config/                 # Kustomize manifests
│   ├── crd/                # Generated CRD YAMLs
│   ├── rbac/               # RBAC manifests
│   └── manager/            # Controller deployment
├── internal/
│   ├── controller/         # Reconciliation logic
│   └── webhook/            # Validation webhooks
└── Makefile                # Build commands
```

### CRDs
AI Runway defines two CRDs:

1. **ModelDeployment** (namespaced) - User-facing API for deploying models
2. **InferenceProviderConfig** (cluster-scoped) - Provider registration

After editing `*_types.go` files, regenerate code:
```bash
cd controller && make manifests generate
```

### Reconciliation Flow

The core controller reconciliation follows these steps:

1. **Receive** ModelDeployment event
2. **Check** for pause annotation (`airunway.ai/reconcile-paused: "true"`) — skip if paused
3. **Select engine** — use explicit `spec.engine.type` or auto-select from provider capabilities (filtered by GPU/CPU, serving mode, and engine GPU requirements)
4. **Validate** spec (engine/resource compatibility, required fields)
5. **Select provider** — use explicit `spec.provider.name` or run auto-selection algorithm (CEL rules now see the resolved engine)
6. **Set status** — `status.engine`, `status.provider`, conditions

The core controller stops here. Provider controllers then:

6. **Filter** — only reconcile ModelDeployments where `status.provider.name` matches
7. **Validate compatibility** — check engine/mode support for this provider
8. **Transform** — convert ModelDeployment spec to provider-specific resource
9. **Create/Update** — apply provider resource with owner references
10. **Sync status** — map provider resource status back to ModelDeployment (phase, replicas, endpoint)
11. **Handle deletion** — clean up provider resources via finalizers (5-minute timeout)

### Observability

**Controller metrics:**
```
airunway_modeldeployment_total{namespace, phase}
airunway_reconciliation_duration_seconds{provider}
airunway_reconciliation_errors_total{provider, error_type}
airunway_provider_selection{provider, reason}
airunway_deployment_replicas{name, namespace, state}
airunway_deployment_phase{name, namespace, phase}
```

**Events emitted:**
```
Normal   ProviderSelected    Selected provider 'dynamo': default → dynamo (GPU inference default)
Normal   ResourceCreated     Created DynamoGraphDeployment 'my-llm'
Warning  SecretNotFound      Secret 'hf-token-secret' not found in namespace 'default'
Warning  ProviderError       Provider resource in error state: insufficient GPUs
Warning  DriftDetected       Provider resource was modified directly, reconciling
Warning  FinalizerTimeout    Finalizer removed after timeout, provider resource may be orphaned
```

### Running Locally
```bash
# Install CRDs first
make controller-install

# Run controller (uses your kubeconfig)
make controller-run
```

### Testing
```bash
# Run unit tests
make controller-test

# Run with verbose output
cd controller && go test -v ./...
```

**Test categories:**

- **Unit tests** — manifest transformation per provider, status mapping, provider selection algorithm, schema validation
- **Integration tests** — controller reconciliation with mock K8s API, owner references, finalizer behavior, drift detection, webhook validation
- **E2E tests** — full deployment lifecycle per provider, error recovery, controller restart resilience

### Version Compatibility Matrix

| AI Runway Controller | Kubernetes | KAITO Operator | Dynamo Operator | KubeRay Operator |
|------------------------|------------|----------------|-----------------|------------------|
| v0.1.x                 | 1.26-1.30  | v0.3.x         | v1.0.x          | v1.1.x           |

| Provider | Minimum Version | CRD API Version | Notes |
|----------|-----------------|-----------------|-------|
| KAITO    | v0.3.0          | kaito.sh/v1beta1 | Requires GPU operator for GPU workloads |
| Dynamo   | v1.0.0          | nvidia.com/v1alpha1 | Requires NVIDIA GPU operator; CRDs are bundled in the platform chart |
| KubeRay  | v1.1.0          | ray.io/v1       | Optional: KubeRay autoscaler for scaling |

### Finalizer Handling

The controller uses finalizers to ensure provider resource cleanup on deletion:

1. Controller attempts cleanup for **5 minutes**
2. After timeout, removes finalizer with warning event
3. Orphaned provider resources may remain (logged for manual cleanup)

**Manual escape (immediate — use when deletion is stuck):**
```bash
kubectl patch modeldeployment my-llm --type=merge \
  -p '{"metadata":{"finalizers":[]}}'
```

### Provider Development

Provider controllers are independent operators in `providers/<name>/`:

```bash
# Build a provider binary (from provider directory)
cd providers/kaito && make build
cd providers/dynamo && make build
cd providers/kuberay && make build
cd providers/llmd && make build

# Build provider Docker image
cd providers/kaito && make docker-build IMG=<YOUR IMAGE>
cd providers/llmd && make docker-build IMG=<YOUR IMAGE>

# Defaults: PUSH=false and PLATFORM=linux/amd64

# Optional: push instead of load, or target a different platform
cd providers/llmd && make docker-build IMG=<YOUR IMAGE> PUSH=true PLATFORM=linux/amd64,linux/arm64

# Deploy provider to cluster
cd providers/kaito && make deploy IMG=<YOUR IMAGE>
cd providers/llmd && make deploy IMG=<YOUR IMAGE>

# Generate deploy manifest
cd providers/kaito && make generate-deploy-manifests
```

## Environment Variables

### Frontend (.env)
```env
VITE_API_URL=http://localhost:3001
VITE_DEFAULT_NAMESPACE=airunway-system
VITE_DEFAULT_HF_SECRET=hf-token-secret
```

### Backend (.env)
```env
PORT=3001
DEFAULT_NAMESPACE=airunway-system
CORS_ORIGIN=http://localhost:5173
AUTH_ENABLED=false
```

## Authentication

AI Runway supports optional authentication using Kubernetes OIDC tokens from your kubeconfig.

### Enabling Authentication

Set the `AUTH_ENABLED` environment variable:

```bash
AUTH_ENABLED=true ./dist/airunway
```

### Login Flow

1. **Run the login command:**
   ```bash
   airunway login
   ```
   This extracts your OIDC token from kubeconfig and opens the browser with a magic link.

2. **Alternative: Specify server URL:**
   ```bash
   airunway login --server https://airunway.example.com
   ```

3. **Use a specific kubeconfig context:**
   ```bash
   airunway login --context my-cluster
   ```

### How It Works

- The CLI extracts the OIDC `id-token` from your kubeconfig
- Opens your browser with a URL containing the token in the fragment (`#token=...`)
- The frontend saves the token to localStorage
- All API requests include the token in the `Authorization: Bearer` header
- The backend validates tokens using Kubernetes `TokenReview` API

### Public Routes (No Auth Required)

These routes are accessible without authentication:
- `GET /api/health` - Health check
- `GET /api/cluster/status` - Cluster connection status
- `GET /api/settings` - Settings (includes `auth.enabled` for frontend)

### CLI Commands

```bash
airunway                    # Start server (default)
airunway serve              # Start server
airunway login              # Login with kubeconfig credentials
airunway login --server URL # Login to specific server
airunway login --context X  # Use specific kubeconfig context
airunway logout             # Clear stored credentials
airunway version            # Show version
airunway help               # Show help
```

## Project Commands

### Root
```bash
bun run dev           # Start both frontend and backend
bun run build         # Build all packages
bun run compile       # Build single binary (frontend + backend) to dist/airunway
bun run lint          # Lint all packages
```

### Controller (Go)
```bash
make controller-build       # Build Go controller binary
make controller-test        # Run controller tests
make controller-run         # Run controller locally
make controller-generate    # Regenerate CRDs and deepcopy code
make controller-install     # Install CRDs into cluster
make controller-deploy      # Deploy controller to cluster
```

### Frontend
```bash
bun run dev:frontend    # Start Vite dev server
bun run build:frontend  # Build for production
```

### Backend
```bash
bun run dev:backend     # Start with watch mode
bun run build:backend   # Compile TypeScript
bun run compile         # Build single binary executable
```

The backend pins TypeScript to `5.3.3` to keep Bun/import-meta compilation behavior stable.
Do not widen that version without validating `bun run build:backend` and `bun run compile`.

#### Backend Testing

```bash
cd backend
bun test                           # Run all backend tests
bun test src/routes/autoscaler.test.ts  # Run a specific test file
bun test --watch                   # Watch mode
bun test --coverage                # With coverage report
```

**Test organization:**
- `src/routes/*.test.ts` — Route-level tests using Hono's `app.request()` (exercises full middleware stack)
- `src/services/*.test.ts` — Service unit tests with mocked dependencies
- `src/lib/*.test.ts` — Utility/library unit tests
- `src/test/helpers.ts` — Shared test utilities (`mockServiceMethod`, `withTimeout`)
- `src/test/fixtures.ts` — Reusable mock data for K8s resources

**How mocking works:** Tests import the Hono `app` directly and use `app.request()` to invoke routes in-process (no HTTP server needed). K8s-dependent services are mocked via property replacement on singleton instances. Tests that may hit K8s use `withTimeout` to gracefully skip when no cluster is available.

**CI pipelines:** The `test.yml` workflow runs all tests in an environment without a Kubernetes cluster (K8s-dependent tests gracefully skip via timeout). The `e2e-backend.yml` workflow runs the same tests against a real Kind cluster with KAITO and the controller deployed, where K8s-dependent tests execute fully.

### Headlamp Plugin

```bash
cd plugins/headlamp
bun install             # Install plugin dependencies
bun run build           # Build plugin
bun run start           # Development mode with auto-rebuild
bun run test            # Run tests
bun run test:watch      # Watch mode for tests
bun run lint            # Lint code
bun run tsc             # Type check only
```

#### Makefile Commands

```bash
make setup              # Install deps, build, and deploy to Headlamp
make dev                # Build and deploy for development
make build              # Build only
make deploy             # Deploy to Headlamp plugins directory
make clean              # Remove build artifacts
```

#### Prerequisites for Headlamp Plugin

- Headlamp Desktop (v0.20+) or Headlamp running in-cluster
- AI Runway backend deployed or running locally

#### Configuring Backend URL

The plugin discovers the backend in this order:
1. **Plugin Settings**: Configure in Headlamp → Settings → Plugins → AIRunway
2. **In-Cluster**: Auto-discovers `airunway.<namespace>.svc`
3. **Default**: Falls back to `http://localhost:3001`

#### Testing with Headlamp Desktop

1. Build and deploy the plugin:
   ```bash
   cd plugins/headlamp
   make setup
   ```

2. Start AI Runway backend:
   ```bash
   cd ../..
   bun run dev:backend
   ```

3. Open Headlamp Desktop - the plugin should appear in the sidebar

## Kubernetes Setup

### Create HuggingFace Token Secret
```bash
kubectl create secret generic hf-token-secret \
  --from-literal=HF_TOKEN="your-token" \
  -n airunway
```

### Install NVIDIA Dynamo (via Helm)
```bash
export NAMESPACE=dynamo-system
export RELEASE_VERSION=1.0.2

# The Dynamo platform chart bundles its CRDs
helm upgrade --install dynamo-platform \
  https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-${RELEASE_VERSION}.tgz \
  --namespace ${NAMESPACE} \
  --create-namespace \
  --set-json global.grove.install=true
```

## Adding a New Provider

Providers are independent out-of-tree Go operators in `providers/<name>/`. Each provider watches `ModelDeployment` resources and creates provider-specific resources.

There are two provider patterns:

### Shim Providers (Adapter Pattern)

Use this when wrapping an existing inference operator that has its own CRD (e.g., KAITO Workspace, DynamoGraphDeployment, RayService). The provider translates `ModelDeployment` → upstream CRD and syncs status back.

```
ModelDeployment → Provider Controller → Upstream CRD → Upstream Operator → Pods/Services
                                             ↑ status sync
```

1. **Create provider directory:**
   ```
   providers/<name>/
   ├── cmd/main.go          # Provider entrypoint
   ├── controller.go        # Reconciliation logic
   ├── transformer.go       # ModelDeployment → upstream CRD conversion
   ├── status.go            # Upstream CRD → ModelDeployment status mapping
   ├── config.go            # InferenceProviderConfig self-registration
   ├── config/              # Kustomize deployment manifests
   ├── Dockerfile           # Container image
   ├── go.mod               # Independent Go module
   └── go.sum
   ```

2. **Implement the provider controller** (see existing providers for examples):
   - `controller.go`: Reconcile `ModelDeployment` resources where `status.provider.name` matches
   - `transformer.go`: Convert `ModelDeployment` spec to upstream CRD resources
   - `status.go`: Map upstream CRD status back to `ModelDeployment` status
   - `config.go`: Define `InferenceProviderConfigSpec` with capabilities and selection rules. Set `airunway.ai/installation` and `airunway.ai/documentation` annotations for UI metadata (see [CRD Reference](crd-reference.md#annotations))

### Native Providers (No Upstream CRD)

Use this when there is no upstream operator — the provider directly manages Kubernetes resources (Deployments, Services) from the `ModelDeployment` spec. No transformer or intermediate CRD is needed.

```
ModelDeployment → Provider Controller → Deployments/Services → Pods
                                             ↑ status sync
```

This works because the `status.provider.resourceKind` and `resourceName` fields are free-form strings — they can point at a `Deployment` just as easily as a `Workspace`. The core controller never inspects what the provider creates.

**When to use this pattern:**
- Building a new inference runtime with no pre-existing CRD
- A lightweight provider that runs vLLM/SGLang containers directly via Deployments
- A "generic" provider where an upstream CRD adds no value

**Directory structure** (no `transformer.go` needed):
```
providers/<name>/
├── cmd/main.go          # Provider entrypoint
├── controller.go        # Reconciliation logic (creates Deployments/Services directly)
├── status.go            # Deployment/Pod → ModelDeployment status mapping
├── config.go            # InferenceProviderConfig self-registration
├── config/              # Kustomize deployment manifests
├── Dockerfile
├── go.mod
└── go.sum
```

**Example reconciliation** (simplified):
```go
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    md := &v1alpha1.ModelDeployment{}
    r.Get(ctx, req.NamespacedName, md)

    // Build Deployment directly from ModelDeployment spec — no intermediate CRD
    deploy := r.buildDeployment(md)  // vllm container with model args
    svc := r.buildService(md)

    controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error { return nil })
    controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error { return nil })

    // Sync status from Deployment
    md.Status.Phase = phaseFromDeployment(deploy)
    md.Status.Provider.ResourceName = deploy.Name
    md.Status.Provider.ResourceKind = "Deployment"
    md.Status.Replicas = replicasFromDeployment(deploy)
    md.Status.Endpoint = endpointFromService(svc)
    r.Status().Update(ctx, md)

    return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
```

The `config.go` for a native provider can omit the `airunway.ai/installation` annotation (no upstream operator to install), or include it if the provider itself is installed via Helm. See [CRD Reference](crd-reference.md#annotations) for the annotation schema.

### Common Steps (Both Patterns)

3. **Add Makefile targets** in the root `Makefile`:
   ```bash
   make <name>-provider-build         # Build provider binary
   make <name>-provider-docker-build  # Build Docker image
   make <name>-provider-deploy        # Deploy to cluster
   ```

## Adding a New Model

Edit `backend/src/data/models.json`:

```json
{
  "models": [
    {
      "id": "org/model-name",
      "name": "Model Display Name",
      "description": "Brief description",
      "size": "7B",
      "task": "chat",
      "contextLength": 32768,
      "supportedEngines": ["vllm", "sglang"],
      "minGpuMemory": "16GB"
    }
  ]
}
```

## Testing API Endpoints

```bash
# Health check
curl http://localhost:3001/api/health

# Cluster status
curl http://localhost:3001/api/cluster/status

# List models
curl http://localhost:3001/api/models

# List deployments
curl http://localhost:3001/api/deployments

# Create deployment (Dynamo/KubeRay)
curl -X POST http://localhost:3001/api/deployments \
  -H "Content-Type: application/json" \
  -d '{
    "name": "test-deployment",
    "namespace": "airunway-system",
    "provider": "dynamo",
    "modelId": "Qwen/Qwen3-0.6B",
    "engine": "vllm",
    "mode": "aggregated",
    "replicas": 1,
    "hfTokenSecret": "hf-token-secret",
    "enforceEager": true
  }'

# Create deployment (KAITO with premade model)
curl -X POST http://localhost:3001/api/deployments \
  -H "Content-Type: application/json" \
  -d '{
    "name": "kaito-deployment",
    "namespace": "kaito-workspace",
    "provider": "kaito",
    "modelSource": "premade",
    "premadeModel": "llama3.2-1b",
    "computeType": "cpu"
  }'

# Create deployment (KAITO with HuggingFace GGUF - direct mode)
curl -X POST http://localhost:3001/api/deployments \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gemma-deployment",
    "namespace": "kaito-workspace",
    "provider": "kaito",
    "modelSource": "huggingface",
    "modelId": "bartowski/gemma-3-1b-it-GGUF",
    "ggufFile": "gemma-3-1b-it-Q8_0.gguf",
    "ggufRunMode": "direct",
    "computeType": "cpu"
  }'

# Create deployment (KAITO with vLLM for GPU inference)
curl -X POST http://localhost:3001/api/deployments \
  -H "Content-Type: application/json" \
  -d '{
    "name": "vllm-deployment",
    "namespace": "kaito-workspace",
    "provider": "kaito",
    "modelSource": "vllm",
    "modelId": "Qwen/Qwen3-0.6B",
    "hfTokenSecret": "hf-token-secret",
    "resources": { "gpu": 1 }
  }'
```

## Accessing Deployed Models

After deployment is running:

```bash
# Port-forward to the service (check deployment details for exact service name)
# Dynamo/KubeRay deployments expose port 8000
kubectl port-forward svc/<deployment>-frontend 8000:8000 -n airunway-system

# KAITO deployments with vLLM expose port 8000
kubectl port-forward svc/<deployment-name> 8000:8000 -n kaito-workspace

# KAITO deployments with llama.cpp (premade/GGUF) expose port 5000
kubectl port-forward svc/<deployment-name> 5000:5000 -n kaito-workspace

# Test the model (OpenAI-compatible API)
# For vLLM (port 8000):
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen/Qwen3-0.6B",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'

# For llama.cpp (port 5000):
curl http://localhost:5000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2-1b",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

## Troubleshooting

### Controller not reconciling
- Check controller logs: `kubectl logs -n airunway-system deploy/airunway-controller-manager`
- Verify CRDs are installed: `kubectl get crd modeldeployments.airunway.ai`
- Check RBAC permissions for the controller service account

### ModelDeployment stuck in Pending
- Check if any `InferenceProviderConfig` resources exist: `kubectl get inferenceproviderconfigs`
- Verify at least one provider has `status.ready: true`
- Check controller logs for provider selection errors

### Backend can't connect to cluster
- Verify kubectl is configured: `kubectl cluster-info`
- Check KUBECONFIG environment variable
- Ensure proper RBAC permissions

### Provider not detected as installed
- Check CRD exists:
  - Dynamo: `kubectl get crd dynamographdeployments.nvidia.com`
  - KubeRay: `kubectl get crd rayservices.ray.io`
  - KAITO: `kubectl get crd workspaces.kaito.sh`
- Check operator deployment:
  - Dynamo: `kubectl get deployments -n dynamo-system`
  - KubeRay: `kubectl get deployments -n ray-system`
  - KAITO: `kubectl get deployments -n kaito-workspace`

### KAITO deployment stuck in Pending
- Check KAITO workspace status: `kubectl describe workspace <name> -n kaito-workspace`
- Verify node labels match labelSelector (default: `kubernetes.io/os: linux`)
- For vLLM mode, ensure GPU nodes are available
- Check events: `kubectl get events -n kaito-workspace --sort-by=.lastTimestamp`

### Metrics not available
- Metrics require AI Runway to run in-cluster
- Check deployment pods are running: `kubectl get pods -n <namespace>`
- Verify metrics endpoint is exposed (port 8000 for vLLM, port 5000 for llama.cpp)

### Frontend can't reach backend
- Check CORS_ORIGIN matches frontend URL
- Verify backend is running on correct port
- Check browser console for errors

### Headlamp Plugin Issues

#### Plugin not appearing in Headlamp
- Verify plugin was built: `cd plugins/headlamp && bun run build`
- Check plugin deployment location:
  - macOS: `~/.config/Headlamp/plugins/airunway-headlamp-plugin`
  - Linux: `~/.config/Headlamp/plugins/airunway-headlamp-plugin`
  - Windows: `%APPDATA%/Headlamp/plugins/airunway-headlamp-plugin`
- Restart Headlamp after deploying the plugin

#### Plugin can't connect to backend
- Check backend URL in Headlamp → Settings → Plugins → AIRunway
- Verify backend is running: `curl http://localhost:3001/api/health`
- For in-cluster deployments, ensure the service is accessible
- Check browser dev tools (Network tab) for connection errors

#### Plugin shows "Connection Failed" banner
- The plugin auto-discovers the backend; ensure it's running
- In-cluster: Deploy AI Runway backend to `airunway-system` namespace
- Local development: Start backend with `bun run dev:backend`

#### Type errors after shared package changes
- Rebuild the shared package: `cd shared && bun run build`
- Rebuild the plugin: `cd plugins/headlamp && bun run build`
- Clear TypeScript cache: `rm -rf plugins/headlamp/node_modules/.cache`
