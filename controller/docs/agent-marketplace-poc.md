# Agent Marketplace — controller PoC

Proof-of-concept controller for the Agent Marketplace ([#200](https://github.com/kaito-project/airunway/issues/200)). It validates that the `AgentDeployment` / `AgentProviderConfig` API (PR #287) maps cleanly onto real agent frameworks across the two rendering backends, on top of an MVP of the controller logic. This is still a PoC: the manager binary currently registers providers in-tree, while the provider shim modules have already been split into separate out-of-tree packages for the runtime cutover path.

## Architecture

Two responsibilities, split exactly as the `AgentDeploymentStatus` ownership contract describes, with **server-side apply under distinct field owners** so the API server itself prevents the two writers from clobbering each other's status (the anti-thrash lesson from #264).

- **Core controller** (`AgentDeploymentReconciler`, field owner `airunway-agents-core`) — framework-neutral. It resolves `spec.framework.name` to a registered, ready `AgentProviderConfig` (`FrameworkReady`), resolves `spec.model` into a stable `status.modelBinding` contract (`ModelBound`), and aggregates `Ready`. It never renders workloads.
- **Framework providers** — each watches `AgentDeployment`, filters to its framework/backend, consumes `status.modelBinding` (never re-resolving), renders framework-native resources, and owns `phase`, `runtime`, `replicas`, and `ProviderReady` under its own field owner.

The shared `conditions` list is `listType=map` keyed by `type`, so SSA merges core-owned (`FrameworkReady`, `ModelBound`, `Ready`) and provider-owned (`ProviderReady`) conditions per key without either writer dropping the other's.

## Backends and providers

| Backend | Provider | Renders | Frameworks |
|---------|----------|---------|------------|
| `crd` | `KagentProviderReconciler` | `kagent.dev/v1alpha2` `Agent` + `ModelConfig` | kagent |
| `crd` | `OrkaProviderReconciler` | `core.orka.ai/v1alpha1` `Provider` + `Agent` | orka |
| `container` | `ContainerProviderReconciler` | `Deployment`/`Job` + `Service` + `ConfigMap` | openclaw, crewai, langgraph, hermes |

`crd` rendering is framework-specific (each operator has its own schema), so kagent and Orka are separate providers. The `container` provider is generic: a single provider serves every container framework because the image comes from `spec.config.image` or the framework's catalog entry — adding a container framework is catalog data, not code.

Both crd providers reflect the upstream operator's own readiness (`status.conditions[type=Ready]`) back into `ProviderReady`, rather than reporting ready the moment the CR is applied.

## Model binding modes

`externalAPI` (OpenAI/Anthropic/Azure OpenAI/custom), `deploymentRef` (an in-cluster `ModelDeployment`, resolved to its service or gateway endpoint), and `gatewayEndpoint`. The core controller validates each binding's mode against the provider's declared `capabilities.modelBindingModes` and refuses unsupported combinations. The resolved binding is injected into container agents as `OPENAI_BASE_URL` / `OPENAI_MODEL` / `OPENAI_API_KEY`, and into crd agents via the framework's native model config.

## `spec.config` — the framework contract

`spec.config` is an opaque `RawExtension`; each provider parses the shape it understands. The PoC confirms the system prompt maps cleanly but not uniformly: `systemPrompt` becomes kagent `declarative.systemMessage`, Orka `systemPrompt.inline`, or (for container agents) a key inside the `agent.json` mounted at `/etc/airunway/agent.json`. This is why `spec.config` must stay opaque rather than a fixed core schema.

## `spec.lifecycle`

`deployment` (default) runs a container agent as a long-running server; `job` runs it as a one-shot `Job` that executes to completion. Ignored by crd backends, whose operator owns the execution shape. Orka's coordinator/specialist swarm is the concrete driver for the one-shot case.

## Security

There is no `spec.security` field. Runtime hardening is provider-owned: the container provider bakes in `runAsNonRoot`, dropped capabilities, and a seccomp profile, relaxing `readOnlyRootFilesystem` only when a framework declares it needs a writable workdir. Cluster-wide guardrails are Pod Security Admission; egress containment is a follow-up `NetworkPolicy` derived from the resolved bindings. A hosted (pod-less) backend has no security context at all, which is why security cannot be a universal user-facing field.

## What the tests prove

Run the unit + envtest suite:

```bash
cd controller
make test        # or: KUBEBUILDER_ASSETS=$(pwd)/bin/k8s/<ver> go test ./internal/controller/...
```

- Pure render functions assert the exact rendered shapes (kagent Agent/ModelConfig, Orka Provider/Agent, container Deployment/Job/Service/ConfigMap) without a cluster.
- envtest specs (real API server) prove: framework/binding resolution and refusal of unsupported modes; the SSA field-ownership split (provider status survives a core reconcile and vice versa); crd providers rendering their CRs and reflecting upstream readiness; the container provider tracking Deployment/Job readiness; and `spec.lifecycle: job` rendering a `Job` (no Deployment/Service). Minimal kagent/Orka CRD stubs live under `internal/controller/testdata/crds/` so the crd providers' apply path is exercised without the real operators.

## Live cluster validation (manual, not automated)

envtest has no kubelet, so it cannot run the actual agents. End-to-end validation against a real cluster — install the framework operator, apply the samples in `config/samples/`, invoke the agent — is manual. The `crd`/kagent + Azure OpenAI path was validated by hand on a CPU cluster (recorded under `tmp/agent-poc-test/RESULTS.md` in this repo).

### Schema fidelity (automated, high-confidence)

The crd providers are tested against the **real** upstream CRDs, not permissive stubs: `internal/controller/testdata/crds/` vendors the actual `kagent.dev` (ModelConfig + Agent) and `core.orka.ai` (Provider + Agent) CRDs, so envtest enforces their real structural schemas and CEL rules when the providers apply their rendered resources. This is what caught the kagent `apiKeySecretRef`→`apiKeySecret` v1alpha2 rename. Re-vendor these files when bumping the target upstream version.

### crd backends

- **kagent**: install the operator (`helm install kagent-crds` + `kagent` from `oci://ghcr.io/kagent-dev/kagent/helm/`), apply the `sre-bot` sample, then invoke over A2A at `:8083/api/a2a/<ns>/<name>` (the `ask.sh` pattern). The rendered `ModelConfig` must be in the Agent's namespace (kagent rejects cross-namespace); the provider already renders it alongside the Agent.
- **Orka**: install the operator (`core.orka.ai`), apply `research-swarm`. Orka checks the referenced `Provider.status.ready` before the `Agent` goes Ready, so the Provider must reconcile first — the provider applies Provider before Agent and the requeue converges. A standalone Agent is a valid config object; create a `Task` referencing it to actually execute.

### container backend

No off-the-shelf framework image honors the mounted-`agent.json` + `OPENAI_*` contract as-is: OpenClaw serves `:18789` with its own config format, LangGraph serves `:8000` and needs `langgraph build`, and Hermes has retired `OPENAI_BASE_URL`. **CrewAI** is the easiest first real target — its library reads `OPENAI_BASE_URL`/`OPENAI_API_KEY` from env natively, so it needs only a thin FastAPI wrapper that reads `/etc/airunway/agent.json`. Use the `spec.config` `image`/`command`/`args`/`port` fields to adapt to a given image (e.g. `port: 8000` for LangGraph). To smoke-test the plumbing alone (ConfigMap mount + env injection + Service), point `spec.config.image` at a tiny non-root HTTP image that echoes `/etc/airunway/agent.json` and the `OPENAI_*` env, then curl the Service.

## Deferred follow-ups

- Complete the runtime cutover to load provider shims fully out-of-tree (the shim modules now exist, but the main manager still wires providers in-process).
- Advanced GAIE endpoint discovery for `gatewayEndpoint` bindings (listener/port/path selection beyond the current Gateway status address).
- MCP tool wiring, A2A dependencies, and the egress `NetworkPolicy`.
- Publish/point at real wrapped framework images (CrewAI wrapper first) so the container backend runs an actual agent end-to-end.
