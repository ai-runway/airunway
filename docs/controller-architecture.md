# Controller Architecture

The AI Runway controller follows a **two-tier reconciliation model**, inspired by the Kubernetes Container Runtime Interface (CRI) and Cluster API provider patterns:

```
CRI Pattern:
   kubelet ──► CRI Interface ──► containerd/CRI-O/dockershim ──► containers

AI Runway Provider Pattern:
   core ──► Provider Interface ──► kaito-provider/dynamo-provider ──► provider CRs
```

Just as `dockershim` was an adapter that made Docker work with the CRI interface, `kaito-provider` is an adapter that makes KAITO (which doesn't know about AI Runway) work with the AI Runway provider interface.

### Lessons from Dockershim

Dockershim was deprecated and removed from Kubernetes in v1.24. Our design explicitly avoids these mistakes:

| Dockershim Problem | Our Solution |
|--------------------|--------------|
| **Embedded in kubelet** — tight coupling meant every kubelet release had to consider Docker compatibility | Provider controllers are **separate deployments** with independent release cycles |
| **Maintenance burden** — Docker bugs affected core Kubernetes releases | A bug in kaito-provider only affects KAITO users, not core |
| **Blocked innovation** — new features couldn't be adopted because dockershim couldn't translate them | Providers can be updated independently; `provider.overrides` provides escape hatch |
| **Special treatment** — Docker got different treatment than other runtimes | All providers go through the same InferenceProviderConfig interface |

## Core Controller
The core controller (`modeldeployment_controller.go`) is intentionally minimal:
1. **Validates** the `ModelDeployment` spec
2. **Selects a provider** (if `spec.provider.name` is empty) using `InferenceProviderConfig` resources
3. **Updates status conditions** (Validated, ProviderSelected)
4. **Does NOT create** provider-specific resources

## Provider Controllers (Out-of-Tree)
Provider controllers watch for `ModelDeployment` resources where `status.provider.name` matches their name:
1. Check compatibility with the deployment configuration
2. Create provider-specific resources (KAITO Workspace, DynamoGraphDeployment, RayService, etc.)
3. Update `ModelDeployment` status (phase, replicas, endpoint, conditions)

This separation allows:
- Clean separation of concerns
- Independent provider controller versioning
- Easy addition of new providers
- Provider-specific features via `spec.provider.overrides`

## Reconciliation Data Flow

```
                                    ┌─────────────────┐
                                    │ User applies    │
                                    │ ModelDeployment │
                                    └────────┬────────┘
                                             │
                                             ▼
                                    ┌─────────────────┐
                                    │ airunway-    │
                                    │ core webhooks   │
                                    │ (validation)    │
                                    └────────┬────────┘
                                             │
                          ┌──────────────────┴──────────────────┐
                          │                                     │
                          ▼                                     │
               ┌─────────────────────┐                         │
               │ engine specified?   │──── yes ────┐           │
               └─────────────────────┘             │           │
                          │                         │           │
                          no                        │           │
                          │                         │           │
                          ▼                         │           │
               ┌─────────────────────┐             │           │
               │ auto-select engine  │             │           │
               │ from provider       │             │           │
               │ capabilities        │             │           │
               │ (GPU/CPU + serving  │             │           │
               │  mode filtered)     │             │           │
               └─────────┬───────────┘             │           │
                         │                         │           │
                         │ sets status.engine.type  │           │
                         │                         │           │
                         └────────┬────────────────┘           │
                                  │                             │
                                  ▼                             │
               ┌─────────────────────┐                         │
               │ provider specified? │──── yes ───────────────►│
               └─────────────────────┘                         │
                          │                                     │
                          no                                    │
                          │                                     │
                          ▼                                     │
               ┌─────────────────────┐                         │
               │ built-in provider   │                         │
               │ selection algorithm │                         │
               │ (CEL-based)         │                         │
               └─────────┬───────────┘                         │
                         │                                     │
                         │ sets status.provider.name           │
                         │                                     │
                         └─────────────────────────────────────┤
                                                               │
                                                               ▼
                                                    ┌─────────────────────┐
                                                    │ Provider controller │
                                                    │ watches & creates   │
                                                    │ provider resource   │
                                                    │ (Kaito Workspace or │
                                                    │ Dynamo DGD, etc.)   │
                                                    └─────────┬───────────┘
                                                              │
                                                              ▼
                                                    ┌─────────────────────┐
                                                    │ Provider operator   │
                                                    │ reconciles pods     │
                                                    └─────────┬───────────┘
                                                              │
                                                              ▼
                                                    ┌─────────────────────┐
                                                    │ Provider syncs      │
                                                    │ status back to      │
                                                    │ ModelDeployment     │
                                                    └─────────────────────┘
```

## Status Ownership

Multiple controllers write to `ModelDeployment.status` using server-side apply with distinct field managers:

| Field                            | Owner               | Description                       |
| -------------------------------- | ------------------- | --------------------------------- |
| `status.engine.type`             | Core controller     | Resolved engine type              |
| `status.engine.selectedReason`   | Core controller     | Why this engine was chosen        |
| `status.provider.name`           | Core controller     | Selected provider name            |
| `status.provider.selectedReason` | Core controller     | Why this provider was chosen      |
| `status.phase`                   | Provider controller | Deploying / Running / Failed      |
| `status.provider.resourceName`   | Provider controller | Name of created upstream resource |
| `status.provider.resourceKind`   | Provider controller | Kind of created upstream resource |
| `status.replicas.*`              | Provider controller | Desired, ready, available counts  |
| `status.endpoint.*`              | Provider controller | Service name and port             |
| `conditions[Validated]`          | Core webhook        | Spec validation result            |
| `conditions[EngineSelected]`     | Core controller     | Engine selection result            |
| `conditions[ProviderSelected]`   | Core controller     | Provider selection result         |
| `conditions[ProviderCompatible]` | Provider controller | Engine/mode compatibility check   |
| `conditions[ResourceCreated]`    | Provider controller | Upstream resource creation status |
| `conditions[Ready]`              | Provider controller | Overall readiness                 |
| `status.gateway.*`               | Core controller     | Gateway endpoint, model name, readiness |
| `conditions[GatewayReady]`       | Core controller     | Gateway route active              |

## Drift Detection

The controller enforces the `ModelDeployment` spec on provider resources. If someone directly edits a provider resource (e.g., `kubectl edit workspace my-llm`), the controller overwrites those changes on the next reconciliation.

**Pause annotation** — to temporarily disable reconciliation for debugging:
```yaml
metadata:
  annotations:
    airunway.ai/reconcile-paused: "true"
```

## Owner References & Garbage Collection

The controller sets `ownerReferences` on created provider resources:

```yaml
metadata:
  ownerReferences:
    - apiVersion: airunway.ai/v1alpha1
      kind: ModelDeployment
      name: my-llm
      uid: abc-123
      controller: true
      blockOwnerDeletion: true
```

This ensures:
- Deleting `ModelDeployment` automatically deletes the provider resource
- Provider resources cannot be accidentally orphaned
- Clear ownership hierarchy in the cluster

**Namespace Requirement:** Provider resources are always created in the **same namespace** as the `ModelDeployment`. Cross-namespace ownership is not supported (Kubernetes owner references require same-namespace resources).

## Finalizer Handling

The controller uses finalizers to ensure cleanup. If the provider operator is unavailable:

1. Controller attempts cleanup for **5 minutes**
2. After timeout, controller removes finalizer with warning event
3. Orphaned provider resources may remain (logged for manual cleanup)

**Manual escape (immediate):**
```bash
kubectl patch modeldeployment my-llm --type=merge \
  -p '{"metadata":{"finalizers":[]}}'
```

## Update Semantics

When a user updates a `ModelDeployment` spec, changes are handled based on field type:

**Identity fields (trigger delete + recreate):**

| Field           | Reason                                                                 |
| --------------- | ---------------------------------------------------------------------- |
| `model.id`      | Changing the model fundamentally changes the deployment                |
| `model.source`  | Changing from huggingface to custom changes how model is loaded        |
| `engine.type`   | Changing inference engine requires new containers (immutable once set)  |
| `provider.name` | Changing provider requires different resource type                     |
| `serving.mode`  | Changing aggregated ↔ disaggregated restructures the entire deployment |

> **Warning:** Changing identity fields causes brief downtime as the provider resource is deleted and recreated.

**Config fields (in-place update):**

| Field                                   | Notes                                  |
| --------------------------------------- | -------------------------------------- |
| `model.servedName`                      | Changes API-facing model name argument |
| `scaling.replicas`                      | Can be updated without recreation      |
| `scaling.prefill.*`, `scaling.decode.*` | Worker scaling (disaggregated mode)    |
| `env`                                   | Environment variable changes           |
| `resources`                             | Memory/CPU/GPU adjustments             |
| `engine.args`                           | Engine parameter tuning                |
| `engine.contextLength`                  | Context length adjustment              |
| `image`                                 | Rolling update to new container image  |
| `secrets.huggingFaceToken`              | Updates secret reference               |
| `podTemplate.metadata`                  | Updates pod labels/annotations         |
| `nodeSelector`, `tolerations`           | Scheduling constraints                 |
| `provider.overrides`                    | Provider-specific configuration        |

## Status Mapping

The controller extracts meaningful error messages from provider status:

| Provider | Provider Status            | Unified Phase | Message Extraction           |
| -------- | -------------------------- | ------------- | ---------------------------- |
| KAITO    | `WorkspaceSucceeded: True` | Running       | —                            |
| KAITO    | `InferenceReady: False`    | Deploying     | Extract condition message    |
| KAITO    | Error condition            | Failed        | Extract error from condition |
| Dynamo   | `state: successful`        | Running       | —                            |
| Dynamo   | `state: deploying`         | Deploying     | —                            |
| Dynamo   | `state: failed`            | Failed        | Extract from status.message  |
| KubeRay  | `serviceStatus: Running`   | Running       | —                            |
| KubeRay  | `serviceStatus: Pending`   | Pending       | —                            |
| KubeRay  | `serviceStatus: Failed`    | Failed        | Extract from serveStatuses   |
| llmd     | `Available`                | Running       | —                            |
| llmd     | `Progressing`              | Deploying     | Extract condition message    |
| llmd.    | Error condition            | Failed        |  Extract error from condition  |

## Label Propagation

Labels from `ModelDeployment.metadata.labels` are selectively propagated:

- **To provider resource:** Only labels with `airunway.ai/` prefix are copied
- **To pods:** Use `spec.podTemplate.metadata.labels` for pod-level labels
- **Controller-managed:** The controller always adds `airunway.ai/managed-by: airunway`

## Provider Overrides

The `spec.provider.overrides` field provides an escape hatch for provider-specific configuration not covered by the unified API:

**Dynamo overrides:**
```yaml
provider:
  name: dynamo
  overrides:
    routerMode: "kv"          # kv | round-robin | none (default: round-robin)
    epp:
      image: "my-registry/epp:v1"
```

> The validating webhook rejects `replicas` and `resources` **anywhere** inside
> `provider.overrides` (including nested, e.g. `frontend.replicas`), because raw overrides
> are merged after admission has checked `spec.resources` / `spec.scaling` ceilings. Use
> `spec.resources` and `spec.scaling` for sizing. KAITO's equivalent replica field,
> `resource.count`, is rejected explicitly; use `spec.scaling.replicas` instead.

**KubeRay** does not read `spec.provider.overrides`. Overrides that pass the global webhook
do not affect the rendered `RayService`; the webhook still rejects globally blocked security
and sizing paths.

**KAITO** accepts `resource` and `inference`, which the Workspace places at the object root
rather than under `spec` — so unlike the other providers, `spec` is not an accepted key here.
Any other root key is rejected with an error naming it. The allowlist is deliberately narrower
than the Workspace schema, which also declares `tuning`: a `ModelDeployment` describes an
inference deployment and the status translator reads inference conditions, so a tuning
Workspace would report status the provider cannot interpret. Within `resource`, `count` is
managed by `spec.scaling.replicas` and cannot be overridden; fields such as `labelSelector`
and `instanceType` remain available.

Invalid types cause reconciliation failure.

## Validation Webhook

The controller includes a validating admission webhook for `ModelDeployment` resources. Webhook TLS uses self-signed certificates managed by [cert-controller](https://github.com/open-policy-agent/cert-controller) (in-process, no cert-manager dependency).

**Schema validation (core webhook):**

| Rule                                                                | Error Message                                                    |
| ------------------------------------------------------------------- | ---------------------------------------------------------------- |
| `engine: vllm` with `gpu.count: 0`                                  | "vLLM engine requires GPU (set resources.gpu.count > 0)"         |
| `engine: sglang` with `gpu.count: 0`                                | "SGLang engine requires GPU (set resources.gpu.count > 0)"       |
| `engine: trtllm` with `gpu.count: 0`                                | "TensorRT-LLM engine requires GPU (set resources.gpu.count > 0)" |
| `mode: disaggregated` with `spec.resources.gpu`                     | "Cannot specify both resources.gpu and scaling.prefill/decode"   |
| `mode: disaggregated` without `scaling.prefill` or `scaling.decode` | "Disaggregated mode requires scaling.prefill and scaling.decode" |
| `mode: disaggregated` without `scaling.prefill.gpu.count`           | "Disaggregated mode requires scaling.prefill.gpu.count"          |
| `mode: disaggregated` without `scaling.decode.gpu.count`            | "Disaggregated mode requires scaling.decode.gpu.count"           |
| Missing `engine.type`                                               | "engine.type is required"                                        |
| Missing `model.id` when `source: huggingface`                       | "model.id is required when source is huggingface"                |
| Provider CRD not installed                                          | "Provider '{name}' CRD not installed in cluster"                 |

**Provider compatibility (validated by provider controllers, not core):**

| Rule                                         | Error Message                                    |
| -------------------------------------------- | ------------------------------------------------ |
| `engine: sglang` with `provider: kaito`      | "KAITO does not support sglang engine"           |
| `engine: trtllm` with `provider: kaito`      | "KAITO does not support trtllm engine"           |
| `engine: llamacpp` with `provider: dynamo`   | "Dynamo does not support llamacpp engine"        |
| `engine: llamacpp` with `provider: kuberay`  | "KubeRay does not support llamacpp engine"       |
| `engine: sglang` with `provider: kuberay`    | "KubeRay does not support sglang engine"         |
| `engine: trtllm` with `provider: kuberay`    | "KubeRay does not support trtllm engine"         |
| `gpu.count: 0` with `provider: dynamo`       | "Dynamo requires GPU"                            |
| `gpu.count: 0` with `provider: kuberay`      | "KubeRay requires GPU"                           |
| `mode: disaggregated` with `provider: kaito` | "KAITO does not support disaggregated mode"      |
| `engine: sglang` with `provider: llmd`       | "llm-d only supports vllm engine"           | 
| `gpu.count: 0` with `provider: llmd`         | "llm-d requires GPU"                            |

Provider compatibility is surfaced in `ModelDeployment.status.conditions` with type `ProviderCompatible: False`, maintaining the "core has zero provider knowledge" principle.

**Webhook unavailability:** If the webhook is not available (e.g., during initial setup), schema validation occurs at reconciliation time. The controller will accept the resource and set `status.phase: Pending` with a descriptive message until validation passes.

## RBAC

### Controller ServiceAccount

The controller ServiceAccount requires permissions for ModelDeployments, InferenceProviderConfigs, and provider-specific CRDs:

```yaml
rules:
  - apiGroups: ["airunway.ai"]
    resources: ["modeldeployments", "modeldeployments/status"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["kaito.sh"]
    resources: ["workspaces"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["nvidia.com"]
    resources: ["dynamographdeployments"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["ray.io"]
    resources: ["rayservices"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["apiextensions.k8s.io"]
    resources: ["customresourcedefinitions"]
    verbs: ["get", "list"]  # For dynamic version detection
```

### User Permissions

**Users** only need RBAC for `ModelDeployment` resources — the controller acts as a privileged intermediary that creates provider resources on their behalf. This means a user who can create `ModelDeployment` in namespace X can effectively create provider resources in that namespace.

### Secret Handling

- Controller never reads secret contents
- Only passes secret references to provider resources
- HuggingFace tokens stay in Kubernetes secrets

---

## See Also

- [Architecture Overview](architecture.md)
- [CRD Reference](crd-reference.md)
- [Providers](providers.md)
