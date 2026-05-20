# Contributing to AIRunway

Thank you for your interest in contributing to AIRunway! This guide covers development setup, project structure, and contribution guidelines.

## Development Setup

### Prerequisites

- [Bun](https://bun.sh) 1.0+
- Access to a Kubernetes cluster
- Helm CLI
- kubectl configured with cluster access

### Quick Start

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

### Build Commands

```bash
# Run all tests (frontend + backend)
bun run test

# Run frontend e2e tests (Playwright)
bun run test:e2e

# Build single binary (includes frontend)
make compile

# Lint all packages
bun run lint
```

### Running E2E Tests Locally

The frontend has end-to-end tests using [Playwright](https://playwright.dev) that exercise full user flows in a real browser. No backend or Kubernetes cluster is required — all API calls are intercepted and mocked.

```bash
# One-time setup: install Playwright browsers
cd frontend && bunx playwright install --with-deps chromium

# Build frontend (required before running e2e tests)
bun run build:frontend

# Run e2e tests
bun run test:e2e

# Run with UI mode for debugging
cd frontend && bunx playwright test --ui

# Run a specific test file
cd frontend && bunx playwright test e2e/navigation.spec.ts

# View the HTML report after a test run
cd frontend && bunx playwright show-report
```

E2e tests live in `frontend/e2e/` and use `page.route()` to intercept API requests with mock data defined in `frontend/e2e/fixtures.ts`.

### Individual Package Commands

**Frontend:**
```bash
bun run dev:frontend    # Start Vite dev server
bun run build:frontend  # Build for production
```

**Backend:**
```bash
bun run dev:backend     # Start with watch mode
```

## Project Structure

```
airunway/
├── frontend/          # React frontend application
│   ├── src/
│   │   ├── components/  # UI components
│   │   ├── pages/       # Page components
│   │   ├── hooks/       # React hooks
│   │   └── lib/         # Utilities and API client
│   └── ...
├── backend/           # Hono backend API (runs on Bun)
│   ├── src/
│   │   ├── hono-app.ts  # All API routes consolidated
│   │   ├── index.ts     # Bun.serve() entry point
│   │   ├── routes/      # API route handlers
│   │   │   ├── installation.ts # Provider installation (reads from CRDs)
│   │   │   ├── deployments.ts  # Deployment management
│   │   │   └── ...
│   │   ├── services/    # Core services
│   │   │   ├── kubernetes.ts # K8s client
│   │   │   ├── config.ts     # ConfigMap persistence
│   │   │   ├── helm.ts       # Helm CLI integration
│   │   │   ├── metrics.ts    # Prometheus metrics fetching
│   │   │   ├── autoscaler.ts # Cluster autoscaler detection
│   │   │   ├── buildkit.ts   # BuildKit builder management
│   │   │   └── registry.ts   # In-cluster registry management
│   │   ├── lib/         # Utility libraries
│   │   │   ├── k8s-errors.ts # K8s error handling
│   │   │   ├── prometheus-parser.ts # Prometheus text parser
│   │   │   └── retry.ts      # Retry logic for K8s calls
│   │   └── data/        # Static model catalog
│   └── ...
├── shared/            # Shared TypeScript types
├── controller/        # Go-based Kubernetes controller (kubebuilder)
│   ├── api/v1alpha1/  # CRD type definitions
│   ├── internal/      # Reconciliation logic
│   ├── cmd/main.go    # Controller entrypoint
│   └── config/        # Kustomize manifests
├── providers/         # Out-of-tree provider operators (Go)
│   ├── kaito/         # KAITO provider
│   ├── dynamo/        # NVIDIA Dynamo provider
│   └── kuberay/       # KubeRay provider
│   └── llmd/          # llm-d provider
└── docs/              # Documentation
```

## Architecture

### Provider Pattern

AI Runway uses a two-tier provider architecture. The core controller handles `ModelDeployment` validation and provider selection, while independent out-of-tree provider controllers (in `providers/`) handle provider-specific resource creation:

- **Core controller**: Validates specs, selects providers via `InferenceProviderConfig` CRDs, updates status
- **Provider controllers**: Watch `ModelDeployment` resources, create provider-specific resources (KAITO Workspace, DynamoGraphDeployment, RayService)
- **Web UI backend**: Reads provider info (capabilities, installation steps, Helm charts) from `InferenceProviderConfig` CRDs — no hardcoded provider registry

### Configuration Storage

Settings are stored in a Kubernetes ConfigMap (`airunway-config`) in the `airunway-system` namespace:

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

## Adding a New Provider

Providers are independent Go operators in `providers/<name>/`. See existing providers for reference.

1. **Create provider directory:**
   ```
   providers/<name>/
   ├── cmd/main.go          # Provider entrypoint
   ├── controller.go        # Reconciliation logic
   ├── transformer.go       # ModelDeployment → provider resource conversion
   ├── status.go            # Provider resource → ModelDeployment status mapping
   ├── config.go            # InferenceProviderConfig self-registration
   ├── config/              # Kustomize deployment manifests
   ├── Dockerfile           # Container image
   └── go.mod               # Independent Go module
   ```

2. **Implement the provider controller:**
   - Watch `ModelDeployment` resources where `status.provider.name` matches your provider
   - Transform `ModelDeployment` spec into provider-specific resources
   - Map provider resource status back to `ModelDeployment` status
   - Self-register an `InferenceProviderConfig` with capabilities, selection rules, and installation info

3. **Add a Makefile** in the provider directory (`providers/<name>/Makefile`):
   ```bash
   cd providers/<name>
   make build                       # Build provider binary
   make docker-build                # Build Docker image
   make deploy                      # Deploy to cluster
   make generate-deploy-manifests   # Generate deploy YAML
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

# Create deployment
curl -X POST http://localhost:3001/api/deployments \
  -H "Content-Type: application/json" \
  -d '{
    "name": "test-deployment",
    "namespace": "airunway-system",
    "modelId": "Qwen/Qwen3-0.6B",
    "engine": "vllm",
    "mode": "aggregated",
    "replicas": 1,
    "hfTokenSecret": "hf-token-secret",
    "enforceEager": true
  }'
```

## API Endpoints

### Health & Cluster
- `GET /api/health` - Health check
- `GET /api/health/version` - Build version information
- `GET /api/cluster/status` - Kubernetes cluster status
- `GET /api/cluster/nodes` - List cluster nodes with GPU info

### Settings
- `GET /api/settings` - Get current settings and provider list
- `PUT /api/settings` - Update settings

### Runtimes
- `GET /api/runtimes/status` - Get all runtimes installation status

### Installation
- `GET /api/installation/helm/status` - Check Helm CLI availability
- `GET /api/installation/gpu-operator/status` - GPU Operator status
- `GET /api/installation/gpu-capacity` - Cluster GPU capacity
- `GET /api/installation/gpu-capacity/detailed` - Detailed GPU capacity with node pools
- `POST /api/installation/gpu-operator/install` - Install GPU Operator
- `GET /api/installation/providers/:id/status` - Get provider installation status
- `GET /api/installation/providers/:id/commands` - Get manual installation commands
- `POST /api/installation/providers/:id/install` - Install provider via Helm
- `POST /api/installation/providers/:id/upgrade` - Upgrade provider
- `POST /api/installation/providers/:id/uninstall` - Uninstall provider
- `POST /api/installation/providers/:id/uninstall-crds` - Uninstall provider CRDs

### Deployments
- `GET /api/deployments` - List all deployments
- `POST /api/deployments` - Create a new deployment
- `GET /api/deployments/:name` - Get deployment details
- `DELETE /api/deployments/:name` - Delete a deployment
- `GET /api/deployments/:name/pods` - Get deployment pods
- `GET /api/deployments/:name/logs` - Get deployment logs
- `GET /api/deployments/:name/metrics` - Get deployment metrics
- `GET /api/deployments/:name/pending-reasons` - Get pending pod reasons

### Models
- `GET /api/models` - Get model catalog
- `GET /api/models/search` - Search HuggingFace models

### Autoscaler
- `GET /api/autoscaler/detection` - Detect autoscaler type
- `GET /api/autoscaler/status` - Get autoscaler status

### AIKit (KAITO)
- `GET /api/aikit/models` - List pre-made GGUF models
- `GET /api/aikit/models/:id` - Get pre-made model details
- `POST /api/aikit/build` - Build AIKit image
- `POST /api/aikit/build/preview` - Preview image build
- `GET /api/aikit/infrastructure/status` - Check build infrastructure
- `POST /api/aikit/infrastructure/setup` - Setup build infrastructure

### OAuth & Secrets
- `GET /api/oauth/huggingface/config` - Get HuggingFace OAuth config
- `POST /api/oauth/huggingface/token` - Exchange OAuth code for token
- `GET /api/secrets/huggingface/status` - HuggingFace secret status
- `POST /api/secrets/huggingface` - Save HuggingFace token
- `DELETE /api/secrets/huggingface` - Delete HuggingFace token

## Troubleshooting

### Backend can't connect to cluster
- Verify kubectl is configured: `kubectl cluster-info`
- Check KUBECONFIG environment variable
- Ensure proper RBAC permissions

### Provider not detected as installed
- Check CRD exists: `kubectl get crd dynamographdeployments.nvidia.com`
- Check operator deployment: `kubectl get deployments -n airunway`

### Frontend can't reach backend
- Check CORS_ORIGIN matches frontend URL
- Verify backend is running on correct port
- Check browser console for errors

## Code Standards

Please refer to [docs/standards.md](docs/standards.md) for coding standards and conventions.

## Pull Request Process

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Make your changes
4. Run tests (`bun run test`)
5. Run linting (`bun run lint`)
6. Commit your changes (`git commit -m 'Add amazing feature'`)
7. Push to the branch (`git push origin feature/amazing-feature`)
8. Open a Pull Request
9. **Share AI Prompts** — If you used AI assistance, include the prompt in your PR (see below)

## AI-Assisted Contributions & Prompt Requests

We embrace AI-assisted development! Whether you use GitHub Copilot, Claude, Cursor, or other AI tools, we welcome contributions that leverage these capabilities.

### What is a Prompt Request?

A **prompt request** is a contribution where you share the AI prompt that generates code, rather than (or in addition to) the code itself. This approach:

- **Captures intent** — The prompt often explains *why* better than a code diff
- **Enables review before implementation** — Maintainers can validate the approach
- **Supports iteration** — Prompts can be refined before code is generated
- **Improves reproducibility** — Anyone can run the prompt to verify results

### Contributing with AI Assistance

#### Option 1: Traditional PR with AI Prompt Disclosure

Submit code as usual, but include the AI prompt in the PR template's "AI Prompt" section. This helps reviewers understand your approach and intent.

#### Option 2: Prompt Request (Prompt-Only)

Create an issue using the **Prompt Request** template if you:
- Have a well-crafted prompt but haven't run it yet
- Want feedback on your approach before implementation
- Prefer maintainers to run and merge the prompt themselves

### Best Practices for AI Prompts

1. **Be specific** — Include file paths, function names, and concrete requirements
2. **Reference project conventions** — Mention agents.md and relevant patterns
3. **Define acceptance criteria** — How will we know it worked?
4. **Include context** — Link to issues, docs, or examples
5. **Test locally when possible** — Verify the prompt produces working code

### Example Prompt

```
Add GPU memory utilization to the deployment metrics page.

Requirements:
- Fetch GPU memory from Prometheus metrics via backend/src/services/metrics.ts
- Display as a progress bar in frontend/src/components/DeploymentMetrics.tsx
- Follow existing patterns for Prometheus metric parsing
- Add unit tests in backend/src/services/metrics.test.ts
- Ensure TypeScript types are properly defined in shared/types/metrics.ts

Reference: backend/src/lib/prometheus-parser.ts for existing metric parsing patterns
```

## Questions?

Feel free to open an issue for questions or discussions about contributing.
