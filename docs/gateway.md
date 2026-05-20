# Gateway API Inference Extension Integration

## Overview

AI Runway integrates with the [Gateway API Inference Extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension) to provide a unified inference gateway. Instead of accessing each model's Service individually, you deploy a single Gateway and call **all** models through one endpoint using the standard OpenAI-compatible API. The Gateway routes requests to the correct model based on the `model` field in the request body.

When gateway integration is active, AI Runway automatically creates an **InferencePool**, **Endpoint Picker (EPP)**, and an **HTTPRoute** for each `ModelDeployment`. You only need to provide the Gateway itself.

## Architecture

```
                     ┌───────────────────────────────────────────────┐
                     │              Kubernetes Cluster               │
                     │                                               │
 ┌────────┐         │  ┌─────────┐       ┌───────────┐              │
 │ Client  │────────▶│  │ Gateway │──────▶│ HTTPRoute │              │
 │ (curl/  │         │  │  + BBR  │       │           │              │
 │ openai) │         │  └─────────┘       └─────┬─────┘              │
 └────────┘         │                          │                     │
                     │                          ▼                     │
                     │                  ┌───────────────┐             │
                     │                  │ InferencePool │             │
                     │                  │ (auto-created)│             │
                     │                  └───────┬───────┘             │
                     │                          │                     │
                     │                          ▼                     │
                     │                  ┌───────────────┐             │
                     │                  │  EPP (Endpoint│             │
                     │                  │  Picker Proxy)│             │
                     │                  │ (auto-created)│             │
                     │                  └───────┬───────┘             │
                     │                          │                     │
                     │                          ▼                     │
                     │                  ┌───────────────┐             │
                     │                  │  Model Server  │             │
                     │                  │  Pod (vLLM,    │             │
                     │                  │  sglang, etc.) │             │
                     │                  └───────────────┘             │
                     └───────────────────────────────────────────────┘
```

**Request flow:** Client → Gateway (+BBR) → HTTPRoute → InferencePool → Endpoint Picker (EPP) → Model Server Pod

**What AI Runway creates automatically** (when `gateway.enabled` is `true` or omitted, and Gateway CRDs are detected):
- `InferencePool` — selects pods labeled with `airunway.ai/model-deployment: <name>` on the model's serving port
- `HTTPRoute` — routes from the Gateway to the InferencePool (unless `httpRouteRef` is set)
- `EPP` — Endpoint Picker Proxy for intelligent endpoint selection

**What you provide:**
- A Gateway resource (with any compatible implementation)

## Prerequisites

- Kubernetes cluster with [Gateway API CRDs](https://gateway-api.sigs.k8s.io/guides/#installing-gateway-api) installed
- [Gateway API Inference Extension CRDs](https://github.com/kubernetes-sigs/gateway-api-inference-extension) installed (provides `InferencePool`)
- A compatible gateway implementation (see below)

## Gateway Implementations

AI Runway works with any Gateway API implementation that supports the [Inference Extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension). You are responsible for installing and managing your own gateway. Some known implementations:

| Implementation | `gatewayClassName` | Status | Docs |
|---|---|---|---|
| [Envoy Gateway](https://gateway.envoyproxy.io/) | `eg` | Not tested | [Inference Extension guide](https://gateway.envoyproxy.io/docs/tasks/ai-gateway/gateway-api-inference-extension/) |
| [Istio](https://istio.io/) | `istio` | Tested | [Inference Extension guide](https://istio.io/latest/docs/tasks/traffic-management/inference/) |
| [kgateway](https://kgateway.dev/) | `kgateway` | Tested (still requires the `X-Gateway-Model-Name` header) | [Inference Extension guide](https://kgateway.dev/docs/ai/gateway-api-inference-extension/) |
| [GKE Gateway](https://cloud.google.com/kubernetes-engine/docs/concepts/gateway-api) | `gke-l7-rilb` | Not tested | [GKE Inference guide](https://cloud.google.com/kubernetes-engine/docs/how-to/serve-llms-with-gateway-api) |

> **Note:** The only difference between implementations is the `gatewayClassName` in your Gateway resource. All AIRunway-managed resources (InferencePool, HTTPRoute) are identical regardless of which gateway you use.

## Setup

### Step 1: Install Gateway API CRDs

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml
```

### Step 2: Install Gateway API Inference Extension CRDs

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.3.1/manifests.yaml
```

### Step 3: Install a Gateway Implementation

Follow the installation guide for your chosen implementation:

- **Envoy Gateway:** [quickstart](https://gateway.envoyproxy.io/docs/tasks/quickstart/)
- **Istio:** [getting started](https://istio.io/latest/docs/setup/getting-started/)
- **kgateway:** [quickstart](https://kgateway.dev/docs/quickstart/)

> [!NOTE]
> **Istio:** Inference Extension support must be explicitly enabled by setting `ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true` on the `istiod` deployment (or passing `--set values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true` during `istioctl install`). Without this, Istio ignores InferencePool backend refs in HTTPRoutes. The `minimal` profile is sufficient — Istio auto-creates a gateway deployment and LoadBalancer Service when you create a Gateway resource. See the [Istio Inference Extension guide](https://istio.io/latest/docs/tasks/traffic-management/ingress/gateway-api-inference-extension/) for full details.

### Step 4: Create a Gateway Resource

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: inference-gateway
  namespace: default
spec:
  gatewayClassName: eg  # Change to match your implementation
  infrastructure:
    annotations:
      # Required on AKS with Istio. Azure otherwise probes GET / on port 80,
      # but the gateway returns 404 there and the public IP can time out.
      service.beta.kubernetes.io/port_80_health-probe_protocol: tcp
  listeners:
    - name: http
      protocol: HTTP
      port: 80
```

If you have multiple Gateways in the cluster, label the one to use for inference:

```yaml
metadata:
  labels:
    airunway.ai/inference-gateway: "true"
```

> [!NOTE]
> **AKS with Istio:** Keep the `spec.infrastructure.annotations.service.beta.kubernetes.io/port_80_health-probe_protocol: tcp`
> setting in your Gateway. Azure otherwise configures an HTTP health probe for `/` on port `80`, but Istio's generated
> gateway returns `404` on `/`. The result is a public IP that times out even though the gateway works through
> `kubectl port-forward` or from inside the cluster.

### Step 5: Deploy Models

Deploy models as usual. AI Runway automatically creates the InferencePool, EPP, and HTTPRoute:

```yaml
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: qwen3
  namespace: default
spec:
  model:
    id: "Qwen/Qwen3-0.6B"
  gateway:
    enabled: true  # Optional: enabled by default when Gateway is detected; set to false to explicitly disable
```

The `ModelDeployment` status will show gateway information once ready:

```bash
kubectl get modeldeployment qwen3 -o jsonpath='{.status.gateway}'
```

## Configuration

### Auto-detection

The controller auto-detects Gateway API Inference Extension CRDs at startup by querying the Kubernetes discovery API. If the CRDs (`InferencePool`, `HTTPRoute`, `Gateway`) are present, gateway integration is enabled. If not, it is silently disabled — no errors, no resources created.

### Explicit Gateway Selection

If you have multiple Gateways or want deterministic behavior, use controller flags:

```
--gateway-name=inference-gateway
--gateway-namespace=default
```

When set, the controller always uses the specified Gateway as the HTTPRoute parent instead of auto-detecting.

### Endpoint Picker (EPP) Configuration

The controller automatically deploys an EPP (Endpoint Picker Proxy) per ModelDeployment, named `<deployment-name>-epp`. The EPP handles intelligent request routing to model server pods.

```
--epp-service-port=9002               # EPP Service port (default: 9002)
--epp-image=<image>                   # EPP container image (default: upstream GAIE image)
--patch-gateway-allowed-routes=true   # Patch Gateway allowedRoutes for cross-namespace routing (default: true)
```

### Body-Based Routing (BBR)

When serving **multiple models** through a single Gateway, a Body-Based Router (BBR) is needed to extract the `model` field from the request body and route to the correct InferencePool. BBR is a separate component deployed via the upstream GAIE helm chart.

Install BBR using the upstream helm chart:

```bash
helm install body-based-router \
  --set provider.name=istio \
  --version v1.3.1 \
  oci://registry.k8s.io/gateway-api-inference-extension/charts/body-based-routing
```

> [!NOTE]
> It is recommended that BBR chart version to match the GAIE version used by AI Runway (currently v1.3.1). Check the [go.mod](https://github.com/kaito-project/airunway/blob/main/controller/go.mod) for the `sigs.k8s.io/gateway-api-inference-extension` dependency version.

Replace `provider.name` with your gateway implementation (`istio`, `gke`, or omit for others). The chart deploys the BBR container and any provider-specific resources (e.g. EnvoyFilter for Istio).

See the [upstream multi-model guide](https://gateway-api-inference-extension.sigs.k8s.io/guides/serving-multiple-inference-pools-latest/) for full details.

### Auto-detection with Multiple Gateways

When no explicit gateway is configured and multiple Gateway resources exist in the cluster, the controller looks for one labeled with:

```yaml
airunway.ai/inference-gateway: "true"
```

If no labeled Gateway is found, the controller skips gateway reconciliation and sets the `GatewayReady` condition to `False`.

### Cross-namespace Gateway

When the Gateway is in a different namespace than the ModelDeployment, the controller automatically patches each Gateway listener to allow HTTPRoutes from the ModelDeployment's namespace using a namespace selector:

```yaml
allowedRoutes:
  namespaces:
    from: Selector
    selector:
      matchLabels:
        kubernetes.io/metadata.name: <modeldeployment-namespace>
```

This is required because Gateway API uses `allowedRoutes` on the listener to control cross-namespace route binding. Without it, the Gateway will reject HTTPRoutes from other namespaces.

**Opting out of Gateway patching:** In security-conscious environments where a Gateway admin manages `allowedRoutes` independently, start the controller with `--patch-gateway-allowed-routes=false`. The controller will skip patching the Gateway globally, and the admin is responsible for configuring the listener to accept HTTPRoutes from ModelDeployment namespaces.

> [!NOTE]
> When `--patch-gateway-allowed-routes=false` is set and the Gateway does not allow routes from the ModelDeployment's namespace, the HTTPRoute will not be accepted by the Gateway and the model will not be reachable through the gateway endpoint.

### Per-deployment Configuration

Each `ModelDeployment` can override gateway behavior:

```yaml
spec:
  gateway:
    # Disable gateway integration for this specific deployment
    enabled: false
    # Override the model name used in routing (defaults to auto-discovered from /v1/models, or spec.model.id)
    modelName: "my-custom-model-name"
```

| Field | Default | Description |
|---|---|---|
| `spec.gateway.enabled` | `true` (when Gateway detected) | Set to `false` to skip InferencePool/HTTPRoute creation |
| `spec.gateway.modelName` | Auto-discovered or `spec.model.id` | Model name used for routing and in API requests |

## Provider-Managed Gateway Resources

Some inference providers (e.g., NVIDIA Dynamo, llm-d) have native Gateway API Inference Extension support with their own InferencePool and Endpoint Picker (EPP). These providers deploy specialized EPPs with capabilities beyond the generic upstream EPP — for example, Dynamo's EPP uses **KV-cache-aware scoring** to route requests to endpoints with the highest KV cache hit probability.

When a provider declares gateway capabilities in its `InferenceProviderConfig`, the controller **delegates** InferencePool and/or EPP management to the provider instead of creating its own.

### How It Works

Providers declare gateway capabilities in their `InferenceProviderConfig`:

```yaml
apiVersion: airunway.ai/v1alpha1
kind: InferenceProviderConfig
metadata:
  name: dynamo
spec:
  capabilities:
    engines: [vllm, sglang, trtllm]
    gateway:
      inferencePoolNamePattern: "{namespace}-{name}-pool"  # Pattern for the pool name
      inferencePoolNamespace: "dynamo-system"        # Namespace where the pool is created
```

The controller adapts its reconciliation based on these flags:

| Flag | `true` (provider-managed) | `false` / absent (controller-managed) |
|---|---|---|
| `managesInferencePool` | Controller waits for the provider's InferencePool to exist, then uses it as the HTTPRoute backend. Skips `reconcileInferencePool()` and `labelModelPods()`. | Controller creates and owns the InferencePool (default behavior). |
| `managesEPP` | Controller does nothing. | Controller deploys the generic upstream EPP. |

The HTTPRoute is **always** managed by the controller regardless of provider capabilities.

### Cross-Namespace Routing

Provider-managed resources often live in a different namespace than the ModelDeployment (e.g., Dynamo pods and InferencePool are in `dynamo-system`). The controller handles this by:

1. Setting the HTTPRoute backend ref with the provider pool's namespace
2. Creating a `ReferenceGrant` in the pool's namespace to allow cross-namespace HTTPRoute references

```
Single Gateway
  ├─ HTTPRoute "llama-70b" → Dynamo InferencePool (dynamo-system) → KV-aware EPP
  ├─ HTTPRoute "phi-4"     → Controller InferencePool (default)   → generic EPP → KAITO
  └─ HTTPRoute "mistral"   → Controller InferencePool (default)   → generic EPP → KubeRay
```

### Pool Name Resolution

The `inferencePoolNamePattern` supports `{name}` and `{namespace}` placeholders, substituted with the ModelDeployment's name and namespace:

| Pattern | ModelDeployment `default/llama-70b` | Resolved Pool Name |
|---|---|---|
| `{namespace}-{name}-pool` | `default/llama-70b` | `default-llama-70b-pool` |
| `{name}-pool` | `default/llama-70b` | `llama-70b-pool` |
| _(empty)_ | `default/llama-70b` | `llama-70b` (fallback to MD name) |

### Cleanup Behavior

When gateway resources are cleaned up (e.g., `gateway.enabled: false`):

- **Controller-managed** InferencePool and EPP resources are deleted normally
- **Provider-managed** InferencePool and EPP resources are **not deleted** — they are owned by the provider and cleaned up when the underlying provider CRD (e.g., DynamoGraphDeployment) is deleted
- The **HTTPRoute** is always deleted by the controller (it always owns the HTTPRoute)

### Dynamo Provider Gateway Support

The Dynamo provider registers full gateway capabilities. When a ModelDeployment uses Dynamo with gateway enabled:

1. The Dynamo operator creates a `DynamoGraphDeployment` with an `Epp` service configured for KV-cache-aware scoring
2. The Dynamo operator creates an InferencePool pointing at its managed EPP
3. The AIRunway controller detects the provider's gateway capabilities, waits for the InferencePool, creates the ReferenceGrant and HTTPRoute
4. Requests are routed through Dynamo's intelligent EPP instead of the generic EPP since that EPP creation has been skipped.

### Model Name Resolution

The controller resolves the gateway model name using this priority:

1. **`spec.gateway.modelName`** — explicit override, always wins
2. **`spec.model.servedName`** — user-specified served name
3. **Auto-discovered from `/v1/models`** — the controller probes the running model server's OpenAI-compatible `/v1/models` endpoint and uses the first model ID returned. This handles baked-in images where the served name differs from `spec.model.id`.
4. **`spec.model.id`** — final fallback

Auto-discovery runs only when the deployment reaches `Running` phase. If the probe fails (timeout, error, no models), it silently falls through to the next level.

## Using the Gateway

### Finding the Gateway Endpoint

```bash
# Get the Gateway address
kubectl get gateway inference-gateway -o jsonpath='{.status.addresses[0].value}'

# Or check the ModelDeployment status
kubectl get modeldeployment qwen3 -o jsonpath='{.status.gateway.endpoint}'
```

### Calling Models via curl

```bash
GATEWAY_IP=$(kubectl get gateway inference-gateway -o jsonpath='{.status.addresses[0].value}')

curl http://${GATEWAY_IP}/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen/Qwen3-0.6B",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### Calling Models via Python (OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url=f"http://{GATEWAY_IP}/v1",
    api_key="unused",  # No auth by default
)

response = client.chat.completions.create(
    model="Qwen/Qwen3-0.6B",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(response.choices[0].message.content)
```

### Multiple Models, One Endpoint

The gateway routes to the correct model based on the `model` field in the request body. Deploy multiple models and call them all through the same endpoint:

```bash
# Call model A
curl http://${GATEWAY_IP}/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "Qwen/Qwen3-0.6B", "messages": [{"role": "user", "content": "Hi"}]}'

# Call model B through the same endpoint
curl http://${GATEWAY_IP}/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "meta-llama/Llama-3.1-8B-Instruct", "messages": [{"role": "user", "content": "Hi"}]}'
```

## Troubleshooting

### Gateway integration is not activating

**Symptom:** No InferencePool or HTTPRoute created for deployments.

1. Check that CRDs are installed:
   ```bash
   kubectl api-resources | grep -E "inferencepools|httproutes|gateways"
   ```
2. Check controller logs for detection messages:
   ```bash
   kubectl logs -n airunway-system deploy/airunway-controller-manager | grep -i gateway
   ```
3. If CRDs were installed after the controller started, restart the controller to refresh detection.

### GatewayReady condition is False

**Symptom:** `ModelDeployment` has `GatewayReady=False`.

1. Check the condition message:
   ```bash
   kubectl get modeldeployment <name> -o jsonpath='{.status.conditions}' | jq '.[] | select(.type=="GatewayReady")'
   ```
2. Common reasons:
   - **NoGateway** — No Gateway resource found. Create one or set `--gateway-name`/`--gateway-namespace`.
   - **Multiple Gateways** — Multiple Gateways exist but none is labeled `airunway.ai/inference-gateway=true`.
   - **InferencePoolFailed** / **HTTPRouteFailed** — RBAC issue or CRD version mismatch.

### Requests return 404 or connection refused

1. Verify the Gateway has an address:
   ```bash
   kubectl get gateway inference-gateway -o jsonpath='{.status.addresses}'
   ```
2. Verify the HTTPRoute is accepted:
   ```bash
   kubectl get httproute <deployment-name> -o yaml
   ```
3. Verify the InferencePool matches running pods:
   ```bash
   kubectl get inferencepool <deployment-name> -o yaml
   kubectl get pods -l airunway.ai/model-deployment=<deployment-name>
   ```
4. If the Gateway has a public IP on AKS but requests to that IP time out, make sure the Gateway sets:
   ```yaml
   spec:
     infrastructure:
       annotations:
         service.beta.kubernetes.io/port_80_health-probe_protocol: tcp
   ```
   Azure can otherwise probe `GET /` on port `80`. Istio's gateway returns `404` there, so the load balancer marks the
   backend unhealthy even though requests succeed through `kubectl port-forward`.
