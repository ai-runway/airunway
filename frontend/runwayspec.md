# AI Runway — Projects + AKS Overlay (Headlamp)

**Date:** 2026-05-29
**Status:** Draft for stakeholder review
**Home repo:** `kaito-project/airunway` (controller + plugins live here)

---

## TL;DR

AI Runway today deploys ML models well but does not give operators an
*application* view. v1 of this work adds three things to the Headlamp
plugin: a **Project** CRD (label-selector grouping of the resources
that make up one ML app, with an opt-in secondary-membership label),
a **Project-scoped Observability tab** that unifies inference-server
metrics and Inspektor Gadget kernel diagnostics, and an **AKS overlay**
that wires KAITO, Workload Identity, ACR, Gateway API, and an Azure
OpenAI fallback — *without any cloud-provider SDK dependency, ever*.
Azure-specific facts come from K8s labels, CR status, and Portal
deep-links. Everything optional fails open with a call-to-action.
v1 is scoped at ~6–10 engineer-weeks (one engineer working
sequentially; calendar depends on staffing). A real topology view of
each Project is a v1.0 stretch goal.

---

## 1. Problem & goals

### Problem

AI Runway today gives operators a unified `ModelDeployment` CRD across
several inference providers, but a real ML application is almost never *one*
ModelDeployment. It is a model **plus** an app pod, **plus** a PVC for
weights or RAG indexes, **plus** a Service / HTTPRoute, **plus** secrets.
Headlamp shows these as scattered resources with no grouping, no
application-level health, and no AI-aware diagnostics. Two pain points
follow:

1. **No application identity.** Operators can't answer "what does the
   *Customer-Support RAG* app look like, and is it healthy?" without a
   mental join across kinds.
2. **No AI-aware observability.** Inference-server metrics (TTFT, KV-cache,
   tokens/s) live behind raw `/metrics` endpoints; kernel-level signals
   (DNS to HuggingFace, OOM-kills, model-weight I/O) are not surfaced at
   all. Operators reach for kubectl, Grafana, and Inspektor Gadget
   manually.

### Goals (v1)

- **Project as a first-class grouping** so an operator can see one ML
  application end-to-end and bulk-act on it.
- **Two-pane observability** scoped to a Project: app metrics from the
  inference engine, kernel diagnostics from Inspektor Gadget.
- **AKS-aware deploy glue** that removes the most common Azure
  paper-cuts (KAITO preset, Workload Identity, ACR, HTTPRoute, AOAI
  fallback) without any cloud-provider SDK dep.

(Graceful degradation across all optional dependencies is treated as a
design principle, not a goal — see §3.)

### Non-goals (v1)

Replacing AI Runway's deploy wizard · generic IG browser · alerting /
on-call · GitOps integration · multi-cluster Project views · *any* cloud
SDK use (Azure ARM, AWS, GCP) · long-term metric / gadget-result
persistence.

### Success criteria

- An operator can go from "no Project" to "RAG app deployed, labeled,
  visible in Projects list with green status, TTFT panel rendering" in
  under 10 minutes on a fresh AKS cluster with KAITO + IG installed.
- On a cluster with the AI Runway backend installed (a required
  install-time dep — see §3 / §6) but **none** of the other optional
  pieces (no Prometheus, no IG, no KAITO, no Gateway API), every
  Project page still renders without errors and shows actionable CTAs.
- The AKS overlay can be pulled out into its own repo later as a config
  change, not a refactor (enforced by the import-direction rule, §4,
  and the cloud-neutral data-path rule, §3).

---

## 2. Personas

| Persona | Primary job | What they need from this work |
|---|---|---|
| **Platform engineer** (primary) | Runs the cluster; installs AI Runway, KAITO, IG; troubleshoots when ML eng pings them | Cluster-wide Projects view, deep diagnostics (IG), AKS glue that cuts ticket volume |
| **ML engineer** (secondary) | Owns one app (a Project); pushes new model versions; reads metrics | Project URL they can bookmark; TTFT / throughput / queue depth; "is my model healthy" at a glance |
| **Eng manager** (tertiary, read-only) | Wants a status overview before standup | Projects list with Ready / Degraded chips; no need to touch IG or AKS detail |

UX detailing optimizes for the platform engineer; ML-engineer access is via
shareable Project URLs (no separate role-restricted view in v1).

---

## 3. Key design principles

1. **Two data planes, treated independently.** AI-workload signal comes
   from two distinct sources: the **application-metrics plane** (TTFT,
   TPOT, tokens/s, KV-cache — emitted by the inference server on its
   Prometheus endpoint, served to the plugin through the AI Runway
   backend) and the **kernel/eBPF plane** (DNS, TCP, OOM, file-I/O —
   emitted by Inspektor Gadget). They have independent availability and
   never substitute for each other. IG cannot produce TTFT; an
   inference server cannot tell you why DNS to HuggingFace is slow.
2. **Inspektor Gadget is provider-neutral.** IG lives in the **core**
   plugin, scoped to a Project, on any Kubernetes distribution. The AKS
   overlay only adds AKS-*flavored enrichers* on top (GPU-SKU fit,
   NCCL flagging on Azure GPU pools).
3. **Project is a CRD with label-selector membership** — not
   namespace-as-Project. The CRD gives the UI a real object to watch,
   supports multiple-Projects-per-namespace, and leaves room for status
   aggregation and (later) ownerRef-style cascades. Primary membership
   is one label; opt-in secondary membership lets a resource belong to
   additional Projects without creating ambiguity in the common case
   (§5).
4. **Graceful degradation is a hard requirement.** Every optional
   dependency (IG, Prometheus, KAITO, Gateway API, AKS itself) is
   detected; missing = inline CTA; never a crash. The AI Runway
   backend is the one exception: it is a required install-time
   component (see §6), and its absence is surfaced as "Install or
   repair AI Runway backend" rather than as silent partial
   functionality.
5. **Don't re-implement Headlamp.** The plugin's job is to add what
   Headlamp doesn't know about: AI workloads, AI-specific metrics,
   kernel-level AI diagnostics, and Project-as-an-application grouping.
   Generic K8s status, events, logs, exec, YAML views, and per-resource
   aggregation are Headlamp's job — we link to them, not duplicate
   them. This shapes §5 (Project detail links to Headlamp pages for
   per-resource detail) and §6 (the overview strip only carries
   Project-specific items).
6. **Detection is explicit and overridable.** Every detector returns a
   `{ detected, confidence, source }` triple. Low-confidence
   detections (e.g., image-name regex) show an inline "override" link.
   Users can pin a definitive answer with the
   `airunway.ai/engine=<name>` label, which detectors treat as
   authoritative. Framework detection (§7) and AKS-context probes
   (§9) are entries in a single typed detection registry (§7.1).

---

## 4. Three layers + cloud-neutral data path

```
┌──────────────────────────── Headlamp (browser) ─────────────────────────┐
│                                                                          │
│  Layer A — CORE airunway plugin  (provider-neutral, runs everywhere)     │
│    • Existing: Deployments, Models, Runtimes(RO), Gateway, Settings      │
│    • NEW: Projects (list, detail [Topology | Resources | Observability], │
│           create wizard, "Add to project")                               │
│    • NEW: Project → Observability tab                                    │
│         ├─ App-metrics panel  (via AI Runway backend)                    │
│         └─ Diagnostics panel  (Inspektor Gadget, when installed)         │
│    • NEW: Detection registry (framework + cluster probes)                │
│                                                                          │
│  Layer B — AKS overlay plugin  (loads ONLY when cluster is AKS)          │
│    • augments core views via registerDetailsViewSection etc.             │
│    • Azure tab on ModelDeployment; AKS deploy-wizard step                │
│    • KAITO (SKU,model)→preset table; WI/ACR/Gateway glue                 │
│    • AKS-flavored IG enrichers (GPU-SKU fit, NCCL silent-replica)        │
└──────────────────────────────────────────────────────────────────────────┘
                                   │
                                   │  Two data paths, both K8s-only:
                                   │   (1) Headlamp's K8s API proxy
                                   │   (2) AI Runway backend (in-cluster Service,
                                   │       reached via that same K8s proxy)
                                   ▼
   Kubernetes cluster: Project CRD + controller · ModelDeployments ·
   AI Runway backend · Inspektor Gadget (optional) · KAITO/KubeRay/…
```

**Import direction:** B → A only, never A → B. Core never learns about
Azure. Splitting B out later stays a config change, not a refactor.

**Cloud-neutral data path (new, hard rule):** the plugin makes calls
*only* to the Kubernetes API server (through Headlamp's proxy) and to
the AI Runway backend (also a K8s Service, reached the same way). Cloud
provider SDKs (Azure ARM, AWS, GCP) are explicitly out of scope at
every version. Cloud-specific facts surface via K8s labels, CR status,
or in-cluster exporters — never via a cloud SDK in the plugin. Where a
fact is only available outside K8s (e.g., AOAI provisioned-throughput
units), the UX is a Portal deep-link, not an API call. This rule
makes the AKS overlay an AKS-*detection-and-K8s-glue* layer rather
than an Azure-SDK layer, which is what keeps it fit-for-purpose in the
kaito-project repo and keeps the future "lift it out" path open.

**Controller** (`controller/project/`): one `controller-runtime` reconciler
that watches `Project`, computes the effective member set (primary
label union secondary labels — §5), writes `status.resourceCounts` +
aggregated `status.conditions`. v1: no ownerRefs, no cascade delete,
no finalizer, no quota.

---

## 5. Project model

### CRD (`airunway.ai/v1alpha1`, minimal)

```yaml
apiVersion: airunway.ai/v1alpha1
kind: Project
metadata: { name: my-rag-app, namespace: default }
spec:
  displayName: "Customer Support RAG"
  description: "Internal RAG over support docs"
  selector:
    matchLabels: { airunway.ai/project: my-rag-app }
status:
  resourceCounts: { modelDeployments: 1, deployments: 2, services: 3, pvcs: 1 }
  conditions: []   # aggregated readiness across labeled resources
```

### Membership model

- **Primary membership.** Label `airunway.ai/project=<name>`. This is
  the resource's *home* Project. The CRD `spec.selector` matches this
  label; users write exactly this in their YAML. One primary per
  resource.
- **Secondary membership (opt-in).** Label
  `airunway.ai/secondary-project.<name>=true`, one key per extra
  Project. The Project controller unions secondary members into the
  effective member set in code; users do not author secondary
  selectors. This keeps the common (single-Project) case clean while
  letting a shared resource (e.g., a common embedding model serving
  two RAG apps) declare itself in multiple Projects without ambiguity.
- **Lifecycle implication.** Actions that imply ownership (eventual
  cascade delete, eventual rename) only touch primary members.
  Secondaries are surfaced but never modified by Project-level
  operations.

### Scope rules

- **Namespaced.** A Project is namespaced and its members are in
  *its own namespace* only (whether primary or secondary). Cross-
  namespace membership is explicitly out of v1.
- **Cluster-scoped resources excluded.** ClusterRoles, CRDs, Nodes etc.
  are not Project members in v1. (Cluster-scoped context — e.g., the
  GatewayClass a Project routes through — is surfaced as a
  *reference*, not membership.)
- **Selector collisions on primary.** Two Projects in the same
  namespace whose primary selectors match the same resource: v1
  **allowed** for compatibility (and usually signals a label mistake).
  The Project detail page shows a "Also home in: N other Projects"
  warning when this happens; this is distinct from secondary
  membership.
- **Label tampering.** Any user with patch rights on a resource can
  add or remove either label. v1 treats this as an RBAC question for
  the cluster admin; the plugin does not enforce Project-scoped ACLs.
  See [§13 Security](#13-security).

### UI (core plugin)

- **Projects list** — sidebar entry; status chips (Ready / Degraded
  / Empty — see §6 for what feeds these).
- **Project detail** — three tabs:
  - **Topology** (default, v1.0 stretch — see §6a). Visual graph of
    the Project's primary members with edges by relationship.
    Falls back to the Resources tab if topology is deferred to v1.1.
  - **Resources.** The kind-grouped list (ModelDeployments,
    Deployments, Services, PVCs, Secrets, HTTPRoutes). Each entry
    links to Headlamp's existing per-resource detail page (per the
    "Don't re-implement Headlamp" principle, §3.5). Secondary
    members get a small "secondary" chip; resources with cross-
    primary collision get a warning marker.
  - **Observability** — §6.
- **Project-shaped quick actions only** — "Add to project", and (in
  v1.1) "Rename". Per-resource actions (delete, edit, scale, etc.)
  are not duplicated here — operators click into the Headlamp page.
- **Create wizard** — three TS-defined templates: *Chat /
  single-model*, *RAG app* (ModelDeployment + PVC + app-pod
  placeholder), *Empty* (creates the `Project` CR alone). Templates
  live in TS, not in-cluster.
- **"Add to project"** affordance on existing ModelDeployment /
  Deployment detail pages — patches the **primary** label by
  default. Holding a modifier (or selecting from a small menu) adds
  a secondary-project label instead. This is the primary day-2 path;
  the wizard is the greenfield path.
- **"Also in" hint.** On a resource's detail page (Headlamp), the
  plugin contributes a small "Project: my-rag-app · Also in:
  shared-embeddings, eval-harness" line so the dual-membership
  state is discoverable from either direction.

### Not in v1

ownerRef cascade delete · Project-scoped RBAC/quota UI · in-cluster
template storage · multi-cluster Project views · resource move across
namespaces · Project rename (v1.1; well-defined now because rename
touches only the primary label).

---

## 6. Observability — the MVP

Lives as a **tab on Project detail** — always *of* a Project. Two
independent panels, two data planes.

### Data-source priority (per plane, fail soft)

| Plane | Source | Transport | If unavailable |
|---|---|---|---|
| App metrics | **AI Runway backend** (required install-time dep) | Backend Service via Headlamp K8s proxy; inherits user's token | "Install or repair AI Runway backend" CTA; panel dark, rest of tab renders |
| Kernel/eBPF | Inspektor Gadget (on-demand) | IG `GadgetManager` gRPC via Headlamp K8s proxy | "Install IG" CTA; rest works |
| K8s-native | always (pod status, restarts, Events, container resources) | Headlamp K8s client | n/a — but the plugin **does not duplicate** Headlamp's per-resource detail; see §3.5 |

> **Principle:** a missing optional source is a UI state, not a crash.
> Detection fails open → assume not-installed → show CTA. The backend
> is the one *required* source; everything else is optional.

### Panel 1 — App metrics (inference server, via backend)

Per-ModelDeployment, sourced from the AI Runway backend, which in turn
reads the serving engine's Prometheus output. The backend schemas
already standardized in AI Runway's existing `MetricsPanel` /
`useMetrics` apply:

- **Latency:** TTFT (p50/p95/p99), TPOT / inter-token, end-to-end.
- **Throughput:** tokens/s, requests/s.
- **Saturation:** queue depth / pending, KV-cache utilization, error rate.
- **Time-series sparklines:** fixed window of **15 minutes**, polled
  every **15 seconds** (~60 points per series). The backend keeps an
  in-memory rolling ring buffer per ModelDeployment it knows about; no
  Prometheus dep, no on-disk persistence. Empty state "Collecting…"
  for the first 15 minutes after a new MD appears, and again after a
  backend restart (documented behaviour, acceptable for a live view).

> Engine coverage note: vLLM and Ray Serve expose these natively; TRT-LLM
> and SGLang vary. The panel renders whatever the engine emits and labels
> unknowns "not reported by this engine" rather than showing zeros.

### Panel 2 — Diagnostics (Inspektor Gadget)

On-demand, scoped to the Project's pods. See §8 for the IG client.

### Overview strip — Project-specific only

Per the "Don't re-implement Headlamp" principle (§3.5), the strip
carries only items Headlamp cannot compute because it doesn't know what
a Project or a ModelDeployment is:

`MDs: 3/3 ready · GPU pods: 4 · Engines: vLLM ×2, SGLang ×1 · Project status: Ready`

Generic K8s aggregations (Pods running, Restarts(1h), OOMs(24h),
Recent events) are not duplicated — they are one click away on any
member's Headlamp detail page.

### Ready / Degraded / Empty chip

Pure aggregation, no thresholds, no time windows, no app-metrics
signal:

- **Ready** = every primary Pod is Ready, every primary
  ModelDeployment has `status.ready=true`, every primary PVC is
  Bound, every primary Service has endpoints.
- **Degraded** = any of the above false.
- **Empty** = no primary members.

This stays deterministic — the chip never becomes a de-facto alert
(non-goal §1). Operators who want to see *why* something is wobbling
(CrashLoopBackOff counts, OOMs in the last hour, recent events) click
into the relevant member's Headlamp detail page, which already shows
all of that.

### Not in v1

Grafana embedding · alerting / thresholds / on-call · cost rollups ·
historical storage of gadget results · long-term metric retention
beyond the 15-minute live buffer (persistence is a v2 question, see
§10).

---

## 6a. Topology view (v1.0 stretch)

The default tab of Project detail. A real graph view of an
application's resources is the visually right answer for "what is this
Project" — a kind-grouped list is a table of contents, not a topology.
Marked **stretch** in v1.0: if the rest of v1.0 ships clean, topology
ships with it; if v1.0 runs hot, topology slides to v1.1 and Project
detail defaults to the Resources tab instead. Neither the Observability
tab nor the AKS overlay blocks on topology.

### Scope and rendering

- **Members shown:** primary members only. Secondary members render as
  faded chips at the edges with "see Project X" links — they are part
  of *another* Project's topology, not this one's.
- **Node kinds:** ModelDeployments, Deployments, Pods (collapsed by
  ReplicaSet when count > N, default N=3), Services, HTTPRoutes,
  Gateways, PVCs, and Secrets/ConfigMaps *only if referenced* by a
  shown resource. No generic "all resources" view.
- **Edges by relationship:** `routes-to`, `selects`, `owns`, `mounts`,
  `references`. Each edge clickable for a one-line tooltip.
- **Layout:** left-to-right, oriented around request entry —
  HTTPRoute / Gateway on the left, Services / Pods / MDs in the middle,
  storage / secrets on the right. Disaggregated serving
  (`spec.serving.mode: disaggregated`) gets prefill and decode lanes.
  The fixed direction and lane discipline kill the "graphs look
  chaotic" failure mode.
- **Renderer:** **react-flow** (~80 KB gz, BSD-3, actively maintained,
  accessible focus order, keyboard-navigable). Stays inside §12's
  1.5 MB core bundle budget with margin.
- **Node-count caps:**
  - At > 50 nodes: Pod groups auto-collapse to a single "N Pods"
    node (clickable for the expanded list).
  - At > 100 nodes: the topology pane falls back to the Resources tab
    with a banner "Project too large for topology view."
- **Interactions:** click a node → opens that resource in Headlamp's
  existing detail page (§3.5). No editing in the topology view.
- **Empty / degraded states:** a Project with one MD + one Service
  still renders meaningfully (2 nodes, 1 edge). An Empty Project
  shows the create-wizard CTA in the topology pane.

### Performance

Computed client-side from the same K8s reads the Project detail page
already does — no new API calls. First-paint target ≤ 2 s at 50
nodes (§12).

---

## 7. AI-workload detection (well-known frameworks)

Detection drives engine-specific metric schemas and per-kind icons in
the Project view. It is **independent of Project membership** — a Pod
is detected as "vLLM" purely from its image/env; that detection does
*not* add it to any Project. Membership requires the
`airunway.ai/project=<name>` label (primary) or
`airunway.ai/secondary-project.<name>=true` (secondary), always.

Detect by image + env + label, extensible via ConfigMap:

| Framework | Signal | Default confidence |
|---|---|---|
| vLLM | image `*vllm*` or env `VLLM_*` | low (image regex) → **high** if `airunway.ai/engine=vllm` is set |
| Triton | image `*tritonserver*` | low |
| SGLang | image `*sglang*` | low |
| TorchServe | image `*torchserve*` | low |
| Ray Serve / KubeRay | `RayService` / `RayCluster` CRs | **high** (CR presence) |
| KServe | label `serving.kserve.io/inferenceservice` | **high** (canonical label) |
| ONNX Runtime | image `*onnxruntime-server*` | low |
| Ollama | image `*ollama*` | low |
| Generic GPU (fallback) | `nvidia.com/gpu` request > 0 | informational only |

### 7.1 Detection contract

All detectors — framework detection here and the `useAksContext`
probes in §9 — return:

```ts
type Detection<T> = {
  detected: T;
  confidence: 'high' | 'low';
  source: 'crd' | 'label' | 'image' | 'env' | 'node-label' | 'oidc' | 'pull-secret';
};
```

- **High confidence** = canonical signal (CRD presence, dedicated
  label, OIDC issuer set). Rendered without an override link.
- **Low confidence** = inferred from a string match (image name, env
  prefix, pull-secret pattern). Rendered with a small "detected via
  image name — override?" link that points the user to set the
  authoritative label.
- **Override label:** `airunway.ai/engine=<name>` is the universal
  override for framework detection. When present on a Pod or its
  workload, all detectors treat it as authoritative and skip
  inference. (AKS-context overrides, if needed, follow the same
  pattern with their own well-known label keys; see §9.)

### Unified registry

Framework detection and the AKS-context probes (§9) are entries in a
single typed detection registry. Each probe declares:

- **Probe name** (e.g., `engine`, `aks.gpu-pool`, `aks.workload-identity`)
- **Signal type** (CRD / node label / pull-secret / etc.)
- **Cost** (cluster API hit, node label scan, etc.) and **cache TTL**
- **Fail-open default** (what to assume when the probe errors out)
- **"What's missing" CTA** (used by the UI when the probe returns
  "not detected" and a Project page wants to surface the gap)

The plugin exposes a single `useDetection(probeName)` hook. The §7
framework table and the §9 AKS-context table are both views over the
same registry.

---

## 8. Inspektor Gadget integration (Layer A core; `ig-client.ts`)

> Validated against the inspektor-gadget repo (image-based gadgets era,
> post-v0.31). The legacy `gadget.kinvolk.io/v1alpha1 Trace` CRD and its
> `gadgettracermanager` controller were removed in mid-2025 (commit
> `3ba4e5500`, "treewide: Remove built-in gadgets") and must not be used
> by the plugin. Any leftover CRD-era RBAC in IG's chart is stale.

### Design

- **Surface:** the IG gadget DaemonSet exposes a **gRPC API** (proto in
  `pkg/gadget-service/api/api.proto`) reached through Headlamp's K8s
  API proxy — no extra endpoint, no extra auth. Two services matter:
  - `GadgetManager.RunGadget` — bidi stream of `GadgetEvent`s for
    one-shot, user-scoped runs. Wrapped in `useGadgetRun(spec)` that
    cleans up on unmount.
  - `GadgetInstanceManager.{Create,List,Get,Remove}GadgetInstance` —
    named, durable runs that survive a browser refresh. Wrapped in
    `useGadgetInstance(spec)` for the 5-minute tier.
  There is no HTTP fallback; gRPC-through-the-K8s-proxy is the only
  supported path. (Helm-declared `GadgetInstance` ConfigMaps exist for
  cluster-admin pre-provisioning but are not a per-user run surface.)
- **Dynamic discovery + curated subset.** On mount, call
  `GadgetManager.GetGadgetInfo` to fetch each gadget's data sources,
  fields, and param schemas (no hardcoded list). Surface a "Recommended
  for AI workloads" group (~8) with the full catalog under "All
  gadgets". Curated list in `ig-recommended.json`, overridable in
  settings.
- **Scoping:** every run auto-fills namespace = Project; user may narrow
  by pod label / container, never widen. Per-user rate limit to avoid
  DOSing the `gadget` DaemonSet.
- **Run model:** on-demand only, user-picked duration (15s / 30s / 60s /
  5min). The 5min tier uses `GadgetInstance` so a refresh doesn't drop
  the stream; shorter tiers use `RunGadget` direct. No always-on
  streams in v1.
- **Rendering (`GadgetRunner.tsx`):** generic table from the gadget's
  column schema (from `GetGadgetInfo`); optional *enrichers* for
  curated gadgets (e.g. `trace_dns` groups by host + flags
  `huggingface.co`, `*.azurecr.io`, `openai.azure.com`; network gadgets
  annotate pod/container per PID). Absent enricher = raw table. Export
  JSON / CSV.
- **Results:** last ~5 per Project in Headlamp plugin-storage;
  ephemeral, documented as such. No server-side persistence.
- **Permissions:** IG needs cluster privileges; plugin never elevates.
  Reaching the gadget pod via apiserver proxy requires the user's token
  to have rights on the `gadget` namespace's pods (proxy/exec
  semantics). If the call fails, show a clear permission message (see
  [§13 Security](#13-security) for PII handling).

### Validated against the IG repo

1. **Surface choice — RESOLVED.** Use the gRPC `GadgetManager` /
   `GadgetInstanceManager` services. The `Trace` CRD is removed.
2. **GPU coverage — RESOLVED.** IG ships `profile_cuda` (CUDA memory
   allocation profile) and `top_cuda_memory` (per-process CUDA
   alloc/free activity); both are uprobe-on-CUDA-runtime, not
   DCGM/NVML. **`top_gpu` does not exist** — drop from any curated set
   or UI copy. GPU **utilization %, SM occupancy, temp, power**
   continue to come from DCGM-exporter → Prometheus.
3. **Curated catalog — RESOLVED.** Confirmed present in `gadgets/`:
   `trace_dns`, `trace_tcp`, `top_tcp`, `trace_oomkill`, `trace_open`,
   `trace_fsslower`, `top_file`, `profile_cuda`, `top_cuda_memory`.
   Adjacent gadgets worth considering for the AI-workloads preset:
   `trace_sni`, `trace_ssl`, `trace_tcpdrop`, `trace_tcpretrans`,
   `top_blockio`, `profile_blockio`, `trace_exec`, `traceloop`.
4. **Version floor — RESOLVED for v1.0.** Pin to the **latest stable
   IG release at v1.0 ship date**, documented as "minimum tested." A
   user on an older release sees an "Upgrade IG to ≥ vX.Y.Z" CTA; a
   user on a newer release gets forward-compatibility assumed, with
   CI smoke tests against IG-main (§11) catching breaks early. Exact
   tag is filled in at v1.0 ship. Per-gadget `GetGadgetInfo`
   degradation (probe each curated gadget individually, show "X of 8
   available") is deferred to v1.1.

---

## 9. AKS overlay

Loads only on AKS. Gate: node label `kubernetes.azure.com/cluster`.
When absent, none of this code runs — core stays clean for non-AKS
users. **No cloud-provider SDK calls at any version** (§3.5 / §4):
the overlay is an AKS-*detection-and-K8s-glue* layer.

### Detection → `useAksContext()` (all fail open, all entries in the §7.1 registry)

| Probe | Signal | Source | Used for |
|---|---|---|---|
| `aks.is-aks` | `kubernetes.azure.com/cluster` node label | node-label (high) | activate overlay |
| `aks.gpu-pool` | `agentpool` + `accelerator=nvidia` + SKU label | node-label (high) | KAITO preset / warn |
| `aks.gpu-sku` | `node.kubernetes.io/instance-type` (H100/A100/V100/T4) | node-label (high) | preset, model-fit |
| `aks.kaito` | `kaito.sh` CRDs | crd (high) | Workspace shortcuts |
| `aks.workload-identity` | OIDC issuer + `azure.workload.identity/use` | oidc + label (high) | one-click SA wiring |
| `aks.acr` | pull-secret / kubelet identity patterns | pull-secret (low → override via label) | image-pull config |
| `aks.gateway-api` | `gateway.networking.k8s.io` CRDs | crd (high) | HTTPRoute template |
| `aks.ig` | IG CRDs / gRPC reachability | crd (high) | enable AKS IG enrichers |

Each probe follows the §7.1 contract: probes whose source is `low`
confidence (`aks.acr` today) get an override affordance.

### Deploy glue — in-cluster writes only

Adds an "Azure" step to the deploy wizard + "Azure" tab on
ModelDeployment:

1. KAITO preset for detected SKU — `(SKU, model-family) → preset` table
   in TS. **Initial table needs a pass against the KAITO catalog**;
   shipping set TBD ([§14 Open questions](#14-open-questions)).
2. Model-fit estimate (model size vs GPU mem; R/Y/G).
3. ServiceAccount with `azure.workload.identity/client-id` (user supplies
   client ID) + patch `spec.serviceAccountName`.
4. ACR image-ref validation + inline "configure pull secret".
5. One-click HTTPRoute via detected Gateway class (default AGIC's).
6. Azure OpenAI fallback `Secret` + `ConfigMap` (OpenAI-compatible
   shape).

### Things the cloud-neutral rule punts to Portal deep-links

Node pool creation, ACR attach, Workload-Identity enablement, AOAI
provisioning, ARM-only infra details (pricing, quota, provisioned-
throughput units) — all of these require Azure ARM and are therefore
**permanently out of scope** for the plugin. UX surface:

- A **right-side drawer** opens when an Azure-out-of-band step is
  needed.
- Each step renders as: 1-line description, a code block with the
  exact `az` command pre-filled with detected values *from K8s*
  (cluster name from labels, etc.), a **Copy** button, and an **"Open
  in Azure Portal"** deep link as a secondary action.
- The drawer is dismissible and re-openable; nothing in the cluster
  state depends on the user having actually run the command — the
  next detection pass picks up the new state from K8s labels / CRs.

Where infra detail is genuinely missing (e.g., a fact the K8s API
doesn't surface), the right fix is **upstream**: KAITO exposes more in
CR status, AKS exposes more in node labels, or a Prometheus exporter
fills the gap. The plugin links to the gap, it does not work around it
with a cloud SDK. See §14 for the running list of such gaps.

### Partial-failure UX (no auto-rollback)

Multi-step wizards (Deploy, Add Azure glue) can leave the cluster with
some resources created and some not. v1 policy:

- Each step is a separate K8s write with its own success/failure pill in
  the wizard summary.
- A failure surfaces an inline error + **Retry this step** button.
- A **Clean up partial resources** action is offered at the wizard
  level — it lists what *would* be deleted (created in this session,
  tracked client-side) and requires explicit confirmation. Resources
  the wizard found pre-existing are never deleted.
- No automatic / hidden rollback. Stakeholders should expect "Wizard
  failed at step 4" to leave steps 1–3 in place until the user chooses.

### AKS-flavored IG enrichers

GPU-SKU-aware fit overlay on GPU diagnostics; NCCL inter-pod traffic
view that flags silent replicas in multi-replica deployments. These
*decorate* the core IG panel; they don't replace it.

---

## 10. Phasing

T-shirt sizes are engineering-effort estimates from current code state,
**per engineer**, not calendar commitments. **S** ≈ days, **M** ≈ 1–2
weeks, **L** ≈ 3+ weeks per engineer. The TL;DR's 6–10 engineer-weeks
figure is the total v1.0 sum assuming one engineer working
sequentially; calendar time depends on staffing.

| Phase | Per-engineer size | Scope |
|---|---|---|
| **v1.0** | ~6–10 eng-weeks total | Project CRD + controller (primary/secondary union + status aggregation); Projects list/detail-as-Resources/wizard + "Add to project"; **Observability tab — app-metrics panel (backend) + IG diagnostics panel (gRPC)** (MVP); framework detection (registry + override label); AKS overlay: `useAksContext`, Azure tab, KAITO preset step, WI SA / ACR / HTTPRoute / AOAI-fallback glue, AKS IG enrichers; graceful-degradation CTAs everywhere; IG smoke CI (§11). **Stretch:** Topology tab as the default Project view (§6a). |
| **v1.1** | M | Project rename (touches primary label only); cascade-delete finalizer (primaries only); more curated gadgets + enrichers from feedback; per-gadget IG version degradation; more KAITO presets; AMD GPU in IG panel; full AKS E2E in CI; if v1.0 deferred topology, ship it here. |
| **v2.0** | L | Prometheus / Azure-Monitor-as-Prometheus historical metrics (still K8s-cluster-local — no cloud SDK); gadget-result persistence; cross-Project ops view; vector-DB / training-shaped Projects. |
| **Not planned** | — | Replacing AI Runway's deploy wizard; generic IG browser; alerting; GitOps integration; **any cloud-provider SDK use (Azure ARM, AWS, GCP)**. |

### Suggested build order

| # | Step | Per-engineer size |
|---|---|---|
| 1 | Project CRD + controller (primary + secondary union; unblocks UI) | M |
| 2a | Projects list / Resources tab / wizard + "Add to project" in core | M |
| 2b | *(stretch)* Topology tab (react-flow, primary-only, caps, fallback) | M |
| 3 | Observability tab — app-metrics panel (reuses backend / `useMetrics`, adds sparkline ring buffer in backend), then IG diagnostics panel (gRPC `GadgetManager` + `GadgetInstanceManager`) + IG smoke CI | M |
| 4 | AKS overlay scaffold + `useAksContext` (probes in shared §7.1 registry) + Azure tab placeholder | S |
| 5 | KAITO preset step; WI / ACR / HTTPRoute / AOAI glue; Portal deep-link drawer | M |
| 6 | AKS IG enrichers; polish; docs; E2E checklist | S |

Steps 1 → 2a → 3 are the critical path. Step 2b can be cut at any
point if v1.0 is running hot; steps 4–6 can fork off in parallel
after step 2a if multiple engineers are available.

---

## 11. Cross-cutting: data flow, errors, testing

- **Two data paths, both K8s-only:** Headlamp's K8s API client for the
  CRD reads, IG gRPC, ModelDeployment writes, and detection probes;
  the AI Runway backend (also a K8s Service, reached the same way) for
  app metrics. No cloud-provider SDK dep (§4). No new plugin-owned
  backend; the existing AI Runway backend is reused and gets a small
  in-memory metric ring buffer for sparklines (§6).
- **Caching:** detection + context in React context (`useRunwayContext`,
  `useAksContext`), invalidated on cluster switch. Detection registry
  TTLs live with the probe definition (§7.1).
- **Errors:** missing optional dependency = inline CTA, never a throw;
  detection fails open; missing AI Runway backend = "Install/repair"
  CTA on the app-metrics panel; K8s writes are per-resource (wizards
  surface partial-failure + "retry failed step", no auto-rollback —
  see §9); IG failures non-fatal.
- **Testing:**
  - **Unit (vitest/RTL):** the bulk — detection registry & override
    label, KAITO table, CRD transforms (primary + secondary union),
    `ig-client` with mocked IG gRPC, hook lifecycles, sparkline ring
    buffer semantics.
  - **Component:** per major view against fixtures using Headlamp's
    harness — Projects list, Resources tab, Topology (if shipped),
    Observability with backend present/absent, IG panel with gRPC
    stubbed.
  - **Controller:** via controller-runtime envtest, including
    primary/secondary selector union and overlap warning.
  - **IG smoke (NEW in v1.0):** CI job spins up a `kind` cluster, Helm-
    installs IG, runs one curated gadget (`trace_dns`) end-to-end
    through the plugin's `ig-client` against the in-cluster gRPC
    endpoint. Triggered per-PR when `ig-client.ts` or
    `GadgetRunner.tsx` changes; nightly against IG-main to catch
    upstream churn.
  - **Full AKS E2E:** v1.1.
  - **Gate:** `bun run lint && bun run tsc && bun run test`.

---

## 12. Non-functional requirements

| Area | Target |
|---|---|
| Headlamp version | ≥ current LTS at v1.0 ship date; pinned in plugin manifest. Breaking-API drift treated as a release blocker. |
| Kubernetes version | ≥ 1.27 (matches AI Runway controller floor). |
| AI Runway backend | Required install-time dep; same version constraint as the controller. |
| Browser support | Latest 2 versions of Chrome, Edge, Firefox, Safari. |
| Plugin bundle size | Core plugin **≤ 1.5 MB** gzipped (includes react-flow ~80 KB gz for topology); AKS overlay **≤ 750 KB** gzipped. CI fails on regression. |
| Project-detail TTI | **≤ 2.5 s** on a cluster with 50 Projects / 500 labeled resources, on a warm Headlamp session (measured against fixtures in CI). |
| Topology tab first-paint | **≤ 2 s** at 50 nodes; falls back to Resources tab beyond the 100-node cap (§6a). |
| Observability tab first-paint | **≤ 1.5 s** to render the Project-specific overview strip; app-metrics and IG panels stream in independently. |
| Detection latency | All §7.1 registry probes complete in **≤ 1 s** or fail open. |
| Accessibility | Keyboard-navigable; respects Headlamp's existing a11y baseline. No new keyboard traps. Topology view has keyboard focus order across nodes/edges. |

---

## 13. Security

- **RBAC for Projects.** The plugin issues all calls with the
  end-user's token; it never elevates. Creating, listing, or modifying a
  `Project` CR requires standard K8s RBAC on `projects.airunway.ai` in
  the target namespace. Cluster admins are responsible for granting
  this — the plugin offers no per-Project ACL of its own in v1.
- **Label tampering.** Any user with `patch` on a resource can add or
  remove either the `airunway.ai/project` (primary) or
  `airunway.ai/secondary-project.<name>` (secondary) label. This is
  the same trust model as any K8s label-based grouping (Services,
  NetworkPolicies). v1 does not validate or webhook-enforce
  membership — cluster admins who care should restrict `patch` on
  the label keys via RBAC or an OPA / Kyverno policy. Documented, not
  prevented.
- **Selector collisions on primary** are allowed and surfaced (§5);
  secondary membership is the intended path for shared resources and
  does not collide.
- **IG privileges.** IG itself runs privileged on every node; the plugin
  inherits no extra privilege. If the user's token cannot reach IG via
  the apiserver proxy, the Diagnostics panel shows a permission CTA
  rather than failing silently.
- **PII in IG output.** Kernel-level traces can leak sensitive strings:
  DNS names (`*.openai.azure.com`, internal hostnames), file paths
  (model identifiers, user-data mounts), syscall arguments. Plugin
  policy:
  - IG output is rendered **as-is** in the table. Nothing is
    auto-uploaded anywhere; the last-5 ephemeral cache lives in
    Headlamp plugin-storage (browser-local), not the cluster.
    JSON/CSV export is explicit and user-initiated.
  - The Diagnostics panel header carries a one-line PII notice the
    first time it's opened per session.
- **Untrusted input.** Project `displayName` / `description`,
  user-supplied client IDs, override-label values, and gadget output
  are all rendered as text (React default-escapes); no
  `dangerouslySetInnerHTML`. Backend Zod validation enforced on every
  route per project security rules.
- **Secrets.** The AOAI fallback step writes a `Secret`; the plugin
  never logs the value, never sends it to telemetry, and clears form
  fields on navigation.

---

## 14. Open questions

These need decisions from named owners before or during v1
implementation.

1. **KAITO preset table.** Which `(SKU, model-family)` pairs ship in
   v1? Needs a pass with the KAITO catalog owner.
2. **Sidebar placement.** Projects as a new top-level group, or nested
   under the existing "AIRunway" group?
3. **Naming.** Core Project work needs no special name; AKS overlay
   working name `airunway-aks` — final name TBD.
4. **Upstream infra-metadata gaps.** With the cloud-neutral rule
   (§3/§4) holding firm, which infra facts that an operator would
   reasonably want to see in a Project are currently only available
   via ARM, and which of those should be fixed upstream (KAITO CR
   status, AKS node labels, a Prometheus exporter, etc.) rather than
   worked around in the plugin? Owner: KAITO maintainers + AI Runway.
   Track as a living list; do not block v1.
5. **Topology stretch decision.** Re-evaluate mid-v1.0: is the rest of
   v1.0 on track such that §6a topology ships in v1.0, or does it slip
   to v1.1? Owner: project lead, decision needed before step 2b
   starts.

> The IG version-floor tag (referenced in §8.4 and §15) is not an
> open question — it's a ship-time value filled in when v1.0 is cut,
> not a decision blocking design or implementation.

---

## 15. Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| AI Runway backend unavailable in a user's install (misconfig, version skew, RBAC) | Low | Medium — app-metrics panel dark | Required install-time dep; clear "Install/repair AI Runway backend" CTA on the panel; backend health probe in `useRunwayContext` |
| IG GPU coverage is alloc-events only (`profile_cuda`, `top_cuda_memory`) — no utilization%, SM occupancy, temp, power | Low | Low — known limit, clearly attributed | Validated against `gadgets/` (§8); UI labels CUDA-alloc panels as IG and routes utilization/temp/power to DCGM-via-Prometheus with a "GPU = via DCGM" badge |
| IG image-based gadget gRPC API churns (proto in `pkg/gadget-service/api/api.proto`) between releases | Medium | Medium — plugin run/instance calls break | Pin minimum IG release in detection and CTA on mismatch; **IG smoke CI in v1.0** (§11); keep curated set's required fields narrow |
| KAITO presets churn between releases | Medium | Low — wrong preset surfaced | Keep preset table in TS, version it against KAITO release; CTA "report stale preset" |
| Inference-engine metric schemas drift (vLLM rename, SGLang adds/removes) over a 12-month horizon | High | Low — panel labels go stale | Engine-coverage matrix in tests; renders "not reported by this engine" gracefully |
| Headlamp plugin API breaks between releases | Low–Medium | High — plugin doesn't load | Pin tested Headlamp version range; CI runs plugin against Headlamp-main weekly |
| Detection false positives confuse users (e.g., a fork named `myco-vllm-patched` mis-detected) | Medium | Low | Detection contract (§7.1) surfaces confidence; low-confidence detections offer the `airunway.ai/engine=<name>` override link inline |
| Secondary-membership UX confuses users ("why is this in two Projects?") | Medium | Low | Default action is primary-only; secondary requires an explicit modifier; "Also in" hint surfaces it from both directions; "secondary" chip in the Resources tab |
| AKS detection false positives on non-AKS clusters using the same node label out of habit | Low | Medium — overlay activates wrongly | Detection requires *both* the cluster-wide label and at least one Azure-specific CRD/identity signal before activating |
| Wizard partial-failure cleanup is misunderstood as auto-rollback | Medium | Medium — orphaned resources | UI is explicit: "no automatic rollback, use Clean up partial resources to remove what this wizard created" |
| Topology view becomes unreadable for medium Projects (20–50 nodes) | Medium | Low — view degrades, list still works | Fixed L→R direction + lane discipline; Pod-group collapse at 50 nodes; full fallback to Resources tab at 100 nodes (§6a) |
| Some Azure infra detail unavailable in K8s — looks like a regression vs. fully ARM-integrated tools | Medium | Low | Cloud-neutral rule is explicit and documented (§3/§4); Portal deep-links cover the "I need to see this in Azure" case; upstream-gaps list (§14.4) tracks where K8s should surface more |

---

## 16. Glossary

| Term | Meaning |
|---|---|
| **ACR** | Azure Container Registry — Azure's image registry; attached to AKS for image pulls |
| **AGIC** | Application Gateway Ingress Controller — Azure's Gateway API / Ingress implementation |
| **AKS** | Azure Kubernetes Service — Microsoft's managed Kubernetes offering |
| **AOAI** | Azure OpenAI — Azure-hosted OpenAI-compatible model endpoints |
| **ARM** | Azure Resource Manager — Azure's control-plane API. Explicitly out of scope for this plugin at every version (§3 / §4) |
| **CTA** | Call-to-action — a button or link prompting the user to take a next step (e.g., "Install IG") |
| **DCGM** | NVIDIA Data Center GPU Manager — exporter that produces GPU utilization, memory, temperature metrics for Prometheus |
| **eBPF** | extended Berkeley Packet Filter — Linux kernel mechanism for safely running sandboxed programs in kernel space; the basis for Inspektor Gadget |
| **Gateway API** | Kubernetes networking API (successor to Ingress) — `gateway.networking.k8s.io` |
| **IG / Inspektor Gadget** | A collection of eBPF-based tools for inspecting and debugging Kubernetes workloads |
| **KAITO** | Kubernetes AI Toolchain Operator — Azure-originated operator for deploying ML models; the "Workspace" CR is its deployment object |
| **KV-cache** | Key/Value cache in transformer inference — memory used to avoid recomputing attention over prior tokens; saturation kills throughput |
| **MD** | ModelDeployment — AI Runway's unified CRD for deploying ML models |
| **NCCL** | NVIDIA Collective Communications Library — used for inter-GPU / inter-pod tensor exchange in distributed inference; silent failures here cause "silent replicas" |
| **OIDC** | OpenID Connect — identity-token standard; needed for Workload Identity |
| **PII** | Personally Identifiable Information |
| **RAG** | Retrieval-Augmented Generation — LLM pattern that fetches documents and feeds them to the model as context |
| **R/Y/G** | Red / Yellow / Green status indicator |
| **react-flow** | React library for building node-based UIs; used for the topology view (§6a) |
| **SKU** | Stock-Keeping Unit — Azure's name for VM/instance types (e.g., `Standard_NC24ads_A100_v4`) |
| **TPOT** | Time Per Output Token — average latency between successive generated tokens; a key streaming-perf metric |
| **TTFT** | Time To First Token — latency from request submission to the first generated token reaching the client; a key user-experience metric |
| **TRT-LLM** | NVIDIA TensorRT-LLM — high-perf inference engine for NVIDIA GPUs |
| **vLLM** | High-throughput LLM inference engine with PagedAttention; the most common engine in AI Runway today |
| **WI** | Workload Identity — AKS feature that lets pods authenticate to Azure without long-lived secrets, via an OIDC-issued ServiceAccount token |
