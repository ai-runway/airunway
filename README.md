# AI Runway

<img src="./frontend/public/logo.png" alt="AI Runway Logo" width="200">

Deploy and manage large language models on Kubernetes — no YAML required.

> [!NOTE]
> AI Runway is still under heavy development and the APIs are not currently considered stable. Feedback is welcome! ❤️

AI Runway gives you a web UI and a unified Kubernetes CRD (`ModelDeployment`) to deploy models across multiple inference providers. Browse [HuggingFace](https://huggingface.co/), pick a model, click deploy.

## Demo

[![AI Runway Demo](https://img.youtube.com/vi/Pe0sLv7v2FM/maxresdefault.jpg)](https://youtu.be/Pe0sLv7v2FM)

## Highlights

- 🚀 **One-Click Deploy** — Browse models, check GPU fit, and deploy from the UI
- 🎯 **Unified CRD** — Single `ModelDeployment` API across all providers
- 🔧 **Multiple Engines** — [vLLM](https://github.com/vllm-project/vllm), [SGLang](https://github.com/sgl-project/sglang), [TensorRT-LLM](https://github.com/NVIDIA/TensorRT-LLM), [llama.cpp](https://github.com/ggml-org/llama.cpp)
- 📈 **Live Monitoring** — Real-time status, logs, and Prometheus metrics
- 💰 **Cost Estimation** — GPU pricing and capacity guidance
- 🌐 **Gateway API Integration** — Unified inference endpoint via [Gateway API Inference Extension](https://gateway-api.sigs.k8s.io/geps/gep-3567/) with auto-detected setup
- 🔌 **Headlamp Plugin** — Full-featured [Headlamp](https://headlamp.dev/) dashboard plugin

## Supported Providers

| Provider                                                 | Description                                                        | Provider Shim                                         |
| -------------------------------------------------------- | ------------------------------------------------------------------ | ----------------------------------------------------- |
| [**NVIDIA Dynamo**](https://github.com/ai-dynamo/dynamo) | GPU-accelerated inference with aggregated or disaggregated serving | [dynamo.yaml](providers/dynamo/deploy/dynamo.yaml)    |
| [**KubeRay**](https://github.com/ray-project/kuberay)    | Ray-based distributed inference                                    | [kuberay.yaml](providers/kuberay/deploy/kuberay.yaml) |
| [**KAITO**](https://github.com/kaito-project/kaito)      | vLLM (GPU) and llama.cpp (CPU/GPU) support                         | [kaito.yaml](providers/kaito/deploy/kaito.yaml)       |
| [**LLM-D**](https://github.com/llm-d/llm-d)              | vLLM (GPU) with aggregated or disaggregated serving                | [llmd.yaml](providers/llmd/deploy/llmd.yaml)          |
| [**Direct vLLM**](docs/providers/vllm.md)                 | Direct OpenAI-compatible vLLM Deployments for newest model support | [vllm.yaml](providers/vllm/deploy/vllm.yaml)          |

## Quick Start

### Prerequisites

- Kubernetes cluster with `kubectl` configured
- `helm` CLI installed
- GPU nodes with NVIDIA drivers (KAITO also supports CPU-only)

### Option A: Run Locally

Download the [latest release](https://github.com/ai-runway/airunway/releases) and run:

```bash
./airunway
```

Open **http://localhost:3001**

> **macOS:** Remove quarantine if needed: `xattr -dr com.apple.quarantine airunway`

### Option B: Deploy to Kubernetes

```bash
set -euo pipefail

# Block AgentDeployment CREATE/UPDATE before webhook versions can mix (phase 1)
kubectl apply -f https://raw.githubusercontent.com/ai-runway/airunway/main/deploy/agentdeployment-webhook-guard.yaml

# Install CRDs and roll out without AgentDeployment admission routes (phase 2)
kubectl apply -f https://raw.githubusercontent.com/ai-runway/airunway/main/deploy/controller.yaml

# Force a new ReplicaSet even when the image tag string is unchanged, then wait
# until every webhook Service endpoint runs the replacement pods.
kubectl rollout restart deployment/airunway-controller-manager -n airunway-system
kubectl rollout status deployment/airunway-controller-manager -n airunway-system --timeout=5m

# Activate the complete validator/mutator set, then unblock writes (phase 3).
# Wait until cert-controller injects the serving CA before deleting the guard.
kubectl apply -f https://raw.githubusercontent.com/ai-runway/airunway/main/deploy/agentdeployment-webhook.yaml
kubectl wait secret/airunway-webhook-server-cert -n airunway-system --for=jsonpath='{.data.ca\.crt}' --timeout=5m
AIRUNWAY_WEBHOOK_CA_BUNDLE="$(kubectl get secret/airunway-webhook-server-cert -n airunway-system -o jsonpath='{.data.ca\.crt}')"
test -n "${AIRUNWAY_WEBHOOK_CA_BUNDLE}"
kubectl wait mutatingwebhookconfiguration/airunway-mutating-webhook-configuration --for="jsonpath={.webhooks[?(@.name==\"magentdeployment-v1alpha1.kb.io\")].clientConfig.caBundle}=${AIRUNWAY_WEBHOOK_CA_BUNDLE}" --timeout=5m
kubectl wait validatingwebhookconfiguration/airunway-validating-webhook-configuration --for="jsonpath={.webhooks[?(@.name==\"vagentdeployment-v1alpha1.kb.io\")].clientConfig.caBundle}=${AIRUNWAY_WEBHOOK_CA_BUNDLE}" --timeout=5m
kubectl delete --ignore-not-found=true -f https://raw.githubusercontent.com/ai-runway/airunway/main/deploy/agentdeployment-webhook-guard.yaml

# Install dashboard UI (optional)
kubectl apply -f https://raw.githubusercontent.com/ai-runway/airunway/main/deploy/dashboard.yaml
kubectl port-forward -n airunway-system svc/airunway 3001:80
```

For production upgrades, use a versioned release and an immutable digest or a
new image tag. The explicit restart prevents an unchanged Deployment template
from making the rollout check succeed against old pods.

Open **http://localhost:3001** — see [deployment docs](deploy/README.md) for more options.

### Getting Started

1. **Install a provider shim** — Apply one or more provider shims to register providers with AI Runway. See [Supported Providers](#supported-providers) for available options.
2. **Install the provider** — Go to the Installation page and install the upstream provider via Helm
3. **Connect HuggingFace** — Sign in via Settings → HuggingFace (optional for non-gated models)
4. **Deploy a model** — Browse the catalog, pick a model, configure, and deploy
5. **Monitor** — Track status, stream logs, and view metrics on the Deployments page

### Access Your Model

Deployed models expose an OpenAI-compatible API:

```bash
kubectl port-forward svc/<deployment-name> 8000:8000 -n <namespace>
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "<model-name>", "messages": [{"role": "user", "content": "Hello!"}]}'
```

## ModelDeployment CRD

```yaml
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: my-model
spec:
  model:
    id: "Qwen/Qwen3-0.6B"
```

The controller automatically selects the best engine and provider, creates provider-specific resources, and reports unified status. See [CRD Reference](docs/crd-reference.md) for details.

## Documentation

📖 **Browse the docs at [ai-runway.github.io/airunway](https://ai-runway.github.io/airunway/)**

The same content also lives in [`docs/`](docs/) for in-repo browsing.

| Topic | Link |
| --- | --- |
| Architecture | [docs/architecture.md](docs/architecture.md) |
| CRD Reference | [docs/crd-reference.md](docs/crd-reference.md) |
| Providers | [docs/providers.md](docs/providers.md) |
| Observability | [docs/observability.md](docs/observability.md) |
| Development | [docs/development.md](docs/development.md) |
| Kubernetes Deployment | [deploy/README.md](deploy/README.md) |
| Gateway Integration | [docs/gateway.md](docs/gateway.md) |
| Headlamp Plugin | [docs/headlamp-plugin.md](docs/headlamp-plugin.md) |

## Community

- **Slack:** Join the [CNCF Slack](https://slack.cncf.io/) workspace and find us in `#airunway`
- **Issues:** Report bugs or request features in [GitHub Issues](https://github.com/ai-runway/airunway/issues)
- **Roadmap:** Follow planned work on the [AI Runway project board](https://github.com/orgs/ai-runway/projects/2)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup. We also accept [AI-assisted prompt requests](CONTRIBUTING.md#ai-assisted-contributions--prompt-requests).
