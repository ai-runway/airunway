# AI Runway — Projects + AKS Overlay (Headlamp)

**Date:** 2026-05-28
**Status:** Draft for stakeholder review
**Home repo:** `kaito-project/airunway` (controller + plugins live here)

---

## TL;DR

AI Runway today deploys ML models well but does not give operators an
*application* view. v1 of this work adds three things to the Headlamp
plugin: a **Project** CRD (label-selector grouping of the resources
that make up one ML app), a **Project-scoped Observability tab** that
unifies inference-server metrics and Inspektor Gadget kernel
diagnostics, and an **AKS overlay** that wires KAITO, Workload Identity,
ACR, Gateway API, and an Azure OpenAI fallback — without taking on the
Azure ARM control plane. Everything optional fails open with a
call-to-action. v1 is scoped at ~6–10 engineering weeks; v2 covers
historical metrics and ARM. The IG-maintainer items in §8 are the only
external blockers.

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
  fallback) without taking on the Azure ARM control plane.

(Graceful degradation across all optional dependencies is treated as a
design principle, not a goal — see §3.)

### Non-goals (v1)

Replacing AI Runway's deploy wizard · generic IG browser · alerting /
on-call · GitOps integration · multi-cluster Project views · Azure ARM
control plane · long-term metric / gadget-result persistence.

### Success criteria

- An operator can go from "no Project" to "RAG app deployed, labeled,
  visible in Projects list with green status, TTFT panel rendering" in
  under 10 minutes on a fresh AKS cluster with KAITO + IG installed.
- On a cluster with **none** of the optional deps (no Prometheus, no IG,
  no KAITO, no Gateway API), every Project page still renders without
  errors and shows actionable CTAs.
- Pulling the AKS overlay out into its own repo later is a config
  change, not a refactor (enforced by the import-direction rule, §4).

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
   Prometheus endpoint) and the **kernel/eBPF plane** (DNS, TCP, OOM,
   file-I/O — emitted by Inspektor Gadget). They have independent
   availability and never substitute for each other. IG cannot produce
   TTFT; an inference server cannot tell you why DNS to HuggingFace is
   slow.
2. **Inspektor Gadget is provider-neutral.** IG lives in the **core**
   plugin, scoped to a Project, on any Kubernetes distribution. The AKS
   overlay only adds AKS-*flavored enrichers* on top (GPU-SKU fit,
   NCCL flagging on Azure GPU pools).
3. **Project is a CRD with label-selector membership** — not
   namespace-as-Project. The CRD gives the UI a real object to watch,
   supports multiple-Projects-per-namespace, and leaves room for status
   aggregation and (later) ownerRef-style cascades.
4. **Graceful degradation is a hard requirement.** Every optional
   dependency (IG, Prometheus, KAITO, Gateway API, AKS itself) is
   detected; missing = inline CTA; never a crash.

---

## 4. Three layers

```
┌──────────────────────────── Headlamp (browser) ─────────────────────────┐
│                                                                          │
│  Layer A — CORE airunway plugin  (provider-neutral, runs everywhere)     │
│    • Existing: Deployments, Models, Runtimes(RO), Gateway, Settings      │
│    • NEW: Projects (list, detail, create wizard, "Add to project")       │
│    • NEW: Project → Observability tab                                    │
│         ├─ App-metrics panel  (inference server Prometheus)              │
│         └─ Diagnostics panel  (Inspektor Gadget, when installed)         │
│                                                                          │
│  Layer B — AKS overlay plugin  (loads ONLY when cluster is AKS)          │
│    • augments core views via registerDetailsViewSection etc.             │
│    • Azure tab on ModelDeployment; AKS deploy-wizard step                │
│    • KAITO (SKU,model)→preset table; WI/ACR/Gateway glue                 │
│    • AKS-flavored IG enrichers (GPU-SKU fit, NCCL silent-replica)        │
└──────────────────────────────────────────────────────────────────────────┘
                                   │  Headlamp K8s proxy is the only data path
                                   ▼
   Kubernetes cluster: Project CRD + controller · ModelDeployments ·
   inference-server /metrics · Inspektor Gadget (optional) · KAITO/KubeRay/…
```

**Import direction:** B → A only, never A → B. Core never learns about
Azure. Splitting B out later stays a config change, not a refactor.

**Controller** (`controller/project/`): one `controller-runtime` reconciler
that watches `Project`, lists resources matching its selector, writes
`status.resourceCounts` + aggregated `status.conditions`. v1: no
ownerRefs, no cascade delete, no finalizer, no quota.

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

Membership = label `airunway.ai/project=<name>`. The CRD gives the UI a
real object to watch and room to grow.

### Scope rules (decisions)

- **Namespaced.** A Project is namespaced and its selector matches
  *namespaced* resources in **its own namespace** only. Cross-namespace
  membership is explicitly out of v1.
- **Cluster-scoped resources excluded.** ClusterRoles, CRDs, Nodes etc.
  are intentionally not Project members in v1. (If a Project needs
  "context" from a cluster-scoped resource — e.g., the GatewayClass it
  routes through — it's surfaced as a *reference*, not membership.)
- **Selector collisions.** Two Projects in the same namespace may select
  the same resource. v1 behavior: **allowed** (a resource can appear in
  multiple Projects). The Project detail page shows a "Also in: N other
  Projects" hint when this happens. A validating webhook is *not* added
  in v1 — overlap is a legitimate use case (e.g., a shared embedding
  model serving two RAG apps).
- **Label tampering.** Any user with patch rights on a resource can add
  or remove the `airunway.ai/project` label. v1 treats this as an RBAC
  question for the cluster admin; the plugin does not enforce
  Project-scoped ACLs. See [§13 Security](#13-security).

### UI (core plugin)

- **Projects list** — sidebar entry; status chips (Ready / Degraded / Empty).
- **Project detail** — resources grouped by kind (ModelDeployments,
  Deployments, Services, PVCs, Secrets, HTTPRoutes), quick actions, plus
  the **Observability tab** (§6).
- **Create wizard** — three TS-defined templates: *Chat / single-model*,
  *RAG app* (ModelDeployment + PVC + app-pod placeholder), *Empty*
  (creates the `Project` CR alone, no resources — users grow it later
  via "Add to project"). Templates live in TS, not in-cluster.
- **"Add to project"** affordance on existing ModelDeployment / Deployment
  detail pages — patches the label. **This is the primary day-2 path**;
  the wizard is the greenfield path.

### Not in v1

ownerRef cascade delete · Project-scoped RBAC/quota UI · in-cluster
template storage · multi-cluster Project views · resource move across
namespaces · Project rename (label rewrite is invasive; pushed to v1.1).

---

## 6. Observability — the MVP

Lives as a **tab on Project detail** — always *of* a Project. Two
independent panels, two data planes.

### Data-source priority (per plane, fail soft)

| Plane | Source order | Transport | If unavailable |
|---|---|---|---|
| App metrics | (1) AI Runway backend → (2) inference-server `/metrics` via Headlamp proxy → (3) Prometheus via Headlamp proxy | All reads go through Headlamp's K8s API proxy. No in-browser scrape (avoids CORS, avoids per-engine TLS pain). | panel shows inline hint, page still renders |
| Kernel/eBPF | Inspektor Gadget (on-demand) | Trace CRD via Headlamp proxy | "Install IG" CTA, rest works |
| K8s-native | always (pod status, restarts, Events, container resources) | Headlamp K8s client | n/a |

> **Principle:** a missing optional source is a UI state, not a crash.
> Detection fails open → assume not-installed → show CTA.

### Panel 1 — App metrics (inference server)

Per-ModelDeployment, sourced from the serving engine's own Prometheus
output. Provider-specific schemas already standardized in AI Runway's
existing `MetricsPanel` / `useMetrics`:

- **Latency:** TTFT (p50/p95/p99), TPOT / inter-token, end-to-end.
- **Throughput:** tokens/s, requests/s.
- **Saturation:** queue depth / pending, KV-cache utilization, error rate.
- Time-series sparklines (extends current `MetricsPanel`, which is mostly
  point-in-time today).

> Engine coverage note: vLLM and Ray Serve expose these natively; TRT-LLM
> and SGLang vary. The panel renders whatever the engine emits and labels
> unknowns "not reported by this engine" rather than showing zeros.

### Panel 2 — Diagnostics (Inspektor Gadget)

On-demand, scoped to the Project's pods. See §8 for the IG client.

### Overview strip (K8s-native, always on)

`Pods: 6 (5 running, 1 pending) · Restarts(1h): 0 · OOMs(24h): 1 ·
GPU pods: 4 · Recent events: 3`

### Not in v1

Grafana embedding · alerting / thresholds / on-call · cost rollups ·
historical storage of gadget results · long-term metric retention (read
live; persistence is a v2 question).

---

## 7. AI-workload detection (well-known frameworks)

Detection drives engine-specific metric schemas and per-kind icons in the
Project view. It is **independent of Project membership** — a Pod is
detected as "vLLM" purely from its image/env; that detection does *not*
add it to any Project. Membership requires the
`airunway.ai/project=<name>` label, always.

Detect by image + env + label, extensible via ConfigMap:

| Framework | Signal |
|---|---|
| vLLM | image `*vllm*` or env `VLLM_*` |
| Triton | image `*tritonserver*` |
| SGLang | image `*sglang*` |
| TorchServe | image `*torchserve*` |
| Ray Serve / KubeRay | RayService/RayCluster CRs |
| KServe | label `serving.kserve.io/inferenceservice` |
| ONNX Runtime | image `*onnxruntime-server*` |
| Ollama | image `*ollama*` |
| Generic GPU (fallback) | `nvidia.com/gpu` request > 0 |

---

## 8. Inspektor Gadget integration (Layer A core; `ig-client.ts`)

> Items flagged **[IG-CONFIRM]** below are assumptions that still need
> verification against the current IG catalog and roadmap before we
> commit UI copy.

### Design

- **Surface:** prefer the `Trace` CRD (create → watch → delete) wrapped
  in `useGadgetTrace(spec)` that cleans up on unmount. Goes through
  Headlamp's K8s proxy, no extra endpoint or auth. Keep the
  gadget-manager HTTP API as fallback.
- **Dynamic discovery + curated subset.** On mount, query IG for
  available gadgets + param schemas (no hardcoded list). Surface a
  "Recommended for AI workloads" group (~8) with the full catalog under
  "All gadgets". Curated list in `ig-recommended.json`, overridable in
  settings.
- **Scoping:** every run auto-fills namespace = Project; user may narrow
  by pod label / container, never widen. Per-user rate limit to avoid
  DOSing the `gadget` DaemonSet.
- **Run model:** on-demand only, user-picked duration (15s / 30s / 60s /
  5min). No always-on streams in v1.
- **Rendering (`GadgetRunner.tsx`):** generic table from the gadget's
  column schema; optional *enrichers* for curated gadgets (e.g.
  `trace_dns` groups by host + flags `huggingface.co`, `*.azurecr.io`,
  `openai.azure.com`; network gadgets annotate pod/container per PID).
  Absent enricher = raw table. Export JSON / CSV.
- **Results:** last ~5 per Project in Headlamp plugin-storage;
  ephemeral, documented as such. No server-side persistence.
- **Permissions:** IG needs cluster privileges; plugin never elevates.
  If the user's token can't reach IG, show a clear permission message
  (see [§13 Security](#13-security) for PII handling).

### Open questions to verify against the IG catalog

1. **[IG-CONFIRM] Surface choice.** Trace CRD vs gadget-manager HTTP
   API — confirm CRD is the supported path going forward.
2. **[IG-CONFIRM] GPU coverage.** IG is eBPF-based; GPU *utilization %*
   is conventionally DCGM-exporter → Prometheus, not eBPF. We do not
   want to assert gadgets like `profile_cuda` / `top_gpu` /
   `top_cuda_memory` exist without confirmation. Which GPU signals can
   IG actually produce today, and which must come from DCGM?
3. **[IG-CONFIRM] Curated catalog.** Confirm real names for the
   candidate set: `trace_dns`, `trace_tcp`, `top_tcp`, `trace_oomkill`,
   `trace_open` / `trace_fsslower` (model-weight load), `top_file`,
   plus GPU gadget(s) iff they exist.
4. **[IG-CONFIRM] Version floor.** Minimum IG release containing all
   gadgets used; we pin in detection and CTA on mismatch.

---

## 9. AKS overlay

Loads only on AKS. Gate: node label `kubernetes.azure.com/cluster`.
When absent, none of this code runs — core stays clean for non-AKS
users.

### Detection → `useAksContext()` (all fail open)

| Signal | Source | Used for |
|---|---|---|
| Is AKS | `kubernetes.azure.com/cluster` | activate overlay |
| GPU pools | `agentpool` + `accelerator=nvidia` + SKU label | KAITO preset / warn |
| GPU SKU (H100/A100/V100/T4) | `node.kubernetes.io/instance-type` | preset, model-fit |
| KAITO installed | `kaito.sh` CRDs | Workspace shortcuts |
| Workload Identity | OIDC issuer + `azure.workload.identity/use` | one-click SA wiring |
| ACR attached | pull-secret / kubelet identity patterns | image-pull config |
| Gateway API | `gateway.networking.k8s.io` CRDs | HTTPRoute template |
| IG installed | IG CRDs | enable AKS IG enrichers |

### Deploy glue — in-cluster writes only

No Azure ARM calls in v1. Adds an "Azure" step to the deploy wizard +
"Azure" tab on ModelDeployment:

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

### Where ARM is needed → copy-paste, not API calls

Node pool creation, ACR attach, Workload-Identity enablement, AOAI
provisioning — all require Azure ARM access we are explicitly deferring
to v2. UX surface in v1:

- A **right-side drawer** opens when an Azure-out-of-band step is
  needed.
- Each step renders as: 1-line description, a code block with the
  exact `az` command pre-filled with detected values (subscription,
  resource group, cluster name), a **Copy** button, and a **"Open in
  Azure Portal"** deep link as a secondary action.
- The drawer is dismissible and re-openable; nothing in the cluster
  state depends on the user having actually run the command — the next
  detection pass picks up the new state.

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
not calendar commitments. **S** ≈ days, **M** ≈ 1–2 weeks, **L** ≈ 3+
weeks (per engineer).

| Phase | Size | Scope |
|---|---|---|
| **v1.0** | L | Project CRD + controller (status aggregation); Projects list/detail/wizard + "Add to project"; **Observability tab — app-metrics panel + IG diagnostics panel** (MVP); framework detection; AKS overlay: `useAksContext`, Azure tab, KAITO preset step, WI SA / ACR / HTTPRoute / AOAI-fallback glue, AKS IG enrichers; graceful-degradation CTAs everywhere. |
| **v1.1** | M | Project rename; cascade-delete finalizer; more curated gadgets + enrichers from feedback; more KAITO presets; AMD GPU in IG panel; automated AKS E2E in CI. |
| **v2.0** | L | Azure ARM control plane (separate auth design); Prometheus / Azure Monitor historical metrics; gadget-result persistence; cross-Project ops view; vector-DB / training-shaped Projects. |
| **Not planned** | — | Replacing AI Runway's deploy wizard; generic IG browser; alerting; GitOps integration. |

### Suggested build order

| # | Step | Size |
|---|---|---|
| 1 | Project CRD + controller (unblocks UI) | M |
| 2 | Projects list / detail / wizard + "Add to project" in core | M |
| 3 | Observability tab — app-metrics panel first (reuses existing `useMetrics`), then IG diagnostics panel | M |
| 4 | AKS overlay scaffold + `useAksContext` + Azure tab placeholder | S |
| 5 | KAITO preset step; WI / ACR / HTTPRoute / AOAI glue | M |
| 6 | AKS IG enrichers; polish; docs; E2E checklist | S |

---

## 11. Cross-cutting: data flow, errors, testing

- **One data path:** Headlamp's K8s API client. Project CRD reads, IG
  Trace writes, ModelDeployment writes, detection probes — all standard
  K8s calls. App-metrics is the one non-K8s read, **also routed through
  the Headlamp K8s proxy** (via Service / Pod port-forward semantics) to
  avoid CORS and to inherit the user's token. No new plugin-owned
  backend.
- **Caching:** detection + context in React context (`useRunwayContext`,
  `useAksContext`), invalidated on cluster switch.
- **Errors:** missing dependency = inline CTA, never a throw; detection
  fails open; K8s writes are per-resource (wizards surface
  partial-failure + "retry failed step", no auto-rollback — see §9);
  IG trace failures non-fatal.
- **Testing:** unit (vitest/RTL) is the bulk — detection, KAITO table,
  CRD transforms, `ig-client` with mocked IG, hook lifecycles; component
  tests per major view against fixtures using Headlamp's harness;
  controller via controller-runtime envtest; IG stubbed via fake Trace
  CRD in envtest, real IG verified manually until AKS E2E lands (v1.1).
  Gate: `bun run lint && bun run tsc && bun run test`.

---

## 12. Non-functional requirements

| Area | Target |
|---|---|
| Headlamp version | ≥ current LTS at v1.0 ship date; pinned in plugin manifest. Breaking-API drift treated as a release blocker. |
| Kubernetes version | ≥ 1.27 (matches AI Runway controller floor). |
| Browser support | Latest 2 versions of Chrome, Edge, Firefox, Safari. |
| Plugin bundle size | Core plugin **≤ 1.5 MB** gzipped; AKS overlay **≤ 750 KB** gzipped. CI fails on regression. |
| Project-detail TTI | **≤ 2.5 s** on a cluster with 50 Projects / 500 labeled resources, on a warm Headlamp session (measured against fixtures in CI). |
| Observability tab first-paint | **≤ 1.5 s** to render the K8s-native overview strip; app-metrics and IG panels stream in independently. |
| Detection latency | All `useAksContext` / `useRunwayContext` probes complete in **≤ 1 s** or fail open. |
| Accessibility | Keyboard-navigable; respects Headlamp's existing a11y baseline. No new keyboard traps. |

---

## 13. Security

- **RBAC for Projects.** The plugin issues all calls with the
  end-user's token; it never elevates. Creating, listing, or modifying a
  `Project` CR requires standard K8s RBAC on `projects.airunway.ai` in
  the target namespace. Cluster admins are responsible for granting
  this — the plugin offers no per-Project ACL of its own in v1.
- **Label tampering.** Any user with `patch` on a resource can add or
  remove the `airunway.ai/project` label, moving it in or out of a
  Project. This is the same trust model as any K8s label-based grouping
  (Services, NetworkPolicies). v1 does not validate or webhook-enforce
  membership — cluster admins who care should restrict `patch` on the
  label key via RBAC or an OPA / Kyverno policy. Documented, not
  prevented.
- **Selector overlap is allowed** (§5); UI surfaces it so users notice
  if a resource is unexpectedly in multiple Projects.
- **IG privileges.** IG itself runs privileged on every node; the plugin
  inherits no extra privilege. If the user's token cannot create
  `Trace` CRs, the Diagnostics panel shows a permission CTA rather than
  failing silently.
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
  user-supplied client IDs, and gadget output are all rendered as text
  (React default-escapes); no `dangerouslySetInnerHTML`. Backend Zod
  validation enforced on every route per project security rules.
- **Secrets.** The AOAI fallback step writes a `Secret`; the plugin
  never logs the value, never sends it to telemetry, and clears form
  fields on navigation.

---

## 14. Open questions

These need decisions from named owners before or during v1
implementation.

1. **[IG-CONFIRM] IG surface, GPU coverage, curated catalog, version
   floor.** See §8 sub-bullets.
2. **App-metrics fallback.** Can we rely on the AI Runway backend being
   present, or must direct `/metrics` (via Headlamp proxy) be a
   first-class path? Affects whether observability works backend-less.
3. **KAITO preset table.** Which `(SKU, model-family)` pairs ship in
   v1? Needs a pass with the KAITO catalog owner.
4. **Sidebar placement.** Projects as a new top-level group, or nested
   under the existing "AIRunway" group?
5. **Naming.** Core Project work needs no special name; AKS overlay
   working name `airunway-aks` — final name TBD.

---

## 15. Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| IG gadget catalog doesn't include the GPU signals assumed in the curated set | Medium | Medium — Diagnostics panel feels thin on GPU | Confirm with IG maintainer ([§8](#8-inspektor-gadget-integration-layer-a-core-ig-clientts)) before committing UI copy; fall back to DCGM-via-Prometheus + clear "GPU = via DCGM" label |
| KAITO presets churn between releases | Medium | Low — wrong preset surfaced | Keep preset table in TS, version it against KAITO release; CTA "report stale preset" |
| Inference-engine metric schemas drift (vLLM rename, SGLang adds/removes) over a 12-month horizon | High | Low — panel labels go stale | Engine-coverage matrix in tests; renders "not reported by this engine" gracefully |
| Headlamp plugin API breaks between releases | Low–Medium | High — plugin doesn't load | Pin tested Headlamp version range; CI runs plugin against Headlamp-main weekly |
| Selector overlap confuses users in practice | Medium | Low | "Also in: N Projects" hint in UI; revisit at v1.1 if support load is high |
| AKS detection false positives on non-AKS clusters using the same node label out of habit | Low | Medium — overlay activates wrongly | Detection requires *both* the cluster-wide label and at least one Azure-specific CRD/identity signal before activating |
| Wizard partial-failure cleanup is misunderstood as auto-rollback | Medium | Medium — orphaned resources | UI is explicit: "no automatic rollback, use Clean up partial resources to remove what this wizard created" |
| ARM-deferred steps (copy-paste `az`) feel like a regression vs. fully automated tooling stakeholders may have seen elsewhere | Medium | Low | Frame as deliberate v1 scope cut in stakeholder review; v2 owns the ARM control plane with proper auth design |

---

## 16. Glossary

| Term | Meaning |
|---|---|
| **ACR** | Azure Container Registry — Azure's image registry; attached to AKS for image pulls |
| **AGIC** | Application Gateway Ingress Controller — Azure's Gateway API / Ingress implementation |
| **AKS** | Azure Kubernetes Service — Microsoft's managed Kubernetes offering |
| **AOAI** | Azure OpenAI — Azure-hosted OpenAI-compatible model endpoints |
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
| **SKU** | Stock-Keeping Unit — Azure's name for VM/instance types (e.g., `Standard_NC24ads_A100_v4`) |
| **TPOT** | Time Per Output Token — average latency between successive generated tokens; a key streaming-perf metric |
| **TTFT** | Time To First Token — latency from request submission to the first generated token reaching the client; a key user-experience metric |
| **TRT-LLM** | NVIDIA TensorRT-LLM — high-perf inference engine for NVIDIA GPUs |
| **vLLM** | High-throughput LLM inference engine with PagedAttention; the most common engine in AI Runway today |
| **WI** | Workload Identity — AKS feature that lets pods authenticate to Azure without long-lived secrets, via an OIDC-issued ServiceAccount token |

---

