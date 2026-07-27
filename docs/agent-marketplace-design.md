# Agent Marketplace — Design Notes (post-review)

> Working design doc for the Agent Marketplace (issue #200, PR #287). Records the design decisions behind the implementation, grounded against the code on `feat/200-agent-marketplace-api`. Not wired into the website sidebar — this is an ADR-style engineering record, not user-facing product docs.

## Throughline

The model side (`ModelDeployment` + `InferenceProviderConfig` + `providers/*`) already solved most of the problems the agent side is now hitting. The consistent answer is **reuse the model side's patterns** rather than invent parallel ones:

- Model binding → prefer the OpenAI-native gateway path the model side already ships (base URL + served model name, routed by BBR).
- Metadata that isn't runtime state → annotations, not spec (the model side keeps UI/catalog concerns out of `spec`).
- Provider-specific logic → out-of-tree provider modules behind an interface, exactly like `providers/{dynamo,kaito,kuberay,llmd}/`.
- Escape hatch for the long tail → a documented `provider.overrides` `RawExtension`, exactly like `ModelDeployment.spec.provider.overrides`.

## Current state (grounded)

- Binding: `AgentDeployment.spec.model` with exactly one of three modes — `deploymentRef`, `gatewayEndpoint`, `externalAPI`. Resolved output is written as `status.modelBinding` (singular), so providers consume one normalized binding contract without a slot-key indirection.
- Catalog: `AgentProviderConfig` catalog is **annotation-level** (`airunway.ai/agent-catalog`), parsed by `CatalogItems()`. It is no longer a spec field — see decision 2. *(This bullet described the pre-T1 state; corrected after T1 landed.)*
- Providers: agent provider **reconcilers are still in-tree** (`controller/internal/controller/agentprovider_{kagent,orka,container}_*.go`) even though `providers/agent-*/` modules and binaries now exist — those shims re-export the in-tree reconcilers via `controller/pkg/agentproviders`. Model providers are genuinely **out-of-tree own-module dirs** (`providers/*/` each with `go.mod` + `cmd/main.go` + `Dockerfile` + controller/transformer). `providers/README.md` states the intention is for *all* providers to live out-of-tree.
- Provider readiness: `AgentProviderConfigReconciler` sets `status.ready` itself — no hand-patching required.
- Reusable model-side patterns: `ModelDeployment.spec.model.servedName` (`modeldeployment_types.go:164-168`) and `ModelDeployment.spec.provider.overrides *runtime.RawExtension` (`modeldeployment_types.go:191`).

## Decisions

### 1. Model binding — converge on the OpenAI-native gateway path

**Raised in review:** `deploymentRef` exposes a Kubernetes-native reference as the primary binding UX, which is the wrong altitude for the user-facing API. Alternatives considered: offering the served model name / model id instead, and keeping `externalAPI` but letting it point at an *internal* OpenAI-compatible service. Note that the served model name/id already lives on the `ModelDeployment`.

**Decision (recommended):**
- Make `gatewayEndpoint` (base URL + served model name, BBR-routed) the **canonical, recommended** binding. It is OpenAI-native and already implemented — nothing new to build for the happy path.
- **Keep `deploymentRef`** as ergonomic sugar so "airunway does everything" still holds, but change it to **resolve through the gateway**: the core reads `ModelDeployment.spec.model.servedName` and produces the same resolved endpoint + served name a `gatewayEndpoint` binding would, instead of pointing the agent straight at the backing Service. `deploymentRef` becomes a convenience that lowers to `gatewayEndpoint`, not a separate routing path.
- **Keep `externalAPI`** unchanged — its base URL is just a URL, so pointing it at an internal OpenAI-compatible Service already works today.
- **Collapse `spec.models[]` → a single `spec.model` block** now that `MaxItems=1`. This deletes the confusing `name: default` slot key; `spec.config` references the single binding implicitly. Alpha API, unreleased, on a feature branch → a breaking rename is acceptable here.

**Why this over the alternatives:** it is OpenAI-native *and* keeps AI Runway owning the whole path, without breaking the working PoCs — they keep using `deploymentRef`, it just resolves via `servedName` + gateway underneath.

**Follow-up call:** whether/when to *deprecate or remove* `deploymentRef` entirely vs. keep it as sugar indefinitely. Current implementation keeps it as sugar.

**Trade-offs:** routing `deploymentRef` through the gateway makes the gateway a hard dependency for the in-cluster happy path; today the PoCs can hit the model Service directly. Mitigation: fall back to direct-Service resolution when no gateway is present, and surface that in `status`.

### 2. Catalog → annotation, not spec  **decided**

**Raised:** the catalog belongs in an annotation rather than in `spec`. Catalog is curated UI metadata (tiles, icons, one-click recipes), not runtime state the controller reconciles.

**Decision:** move `AgentProviderConfig.spec.catalog` out of `spec` into an annotation (e.g. `airunway.ai/agent-catalog`) carrying the catalog as a JSON document. Keep `GetCatalogItem` / `CatalogItemNames` helpers but source them from the parsed annotation.

**Trade-offs:** annotations are unstructured (no CRD schema validation) and capped at ~256KB total per object. Mitigate with (a) a documented JSON shape + a small typed parser, and (b) optional webhook validation of the annotation payload so authors still get errors early. **(a) landed; (b) did not** — there is no `AgentProviderConfig` webhook, so a malformed catalog is admitted and only surfaces as a reconcile failure in the container provider. CRD-backed providers never validate it at all.

### 3. Lift agent providers out-of-tree behind a provider interface  **decided**

**Raised:** agent providers should sit behind a provider interface and live out-of-tree, exactly as the model providers do — keeping provider-specific code out of the core and lets new providers ship independently.

**Decision:** extract the in-tree agent provider controllers into own-module directories mirroring `providers/*` (proposed: `providers/agent-kagent/`, `providers/agent-orka/`, `providers/agent-container/`), each behind a shared provider interface. The core controller resolves bindings + writes `status.modelBinding`; each provider module renders and reconciles its downstream CRs/workloads and reports readiness (the readiness reconciler added this round already establishes the self-reporting contract).

**Trade-offs:** multi-module repo overhead (separate `go.mod`, build/test/release per provider — see the existing `providers/dynamo` module boundary). Worth it for the same reasons the model side already accepted it.

### 4. Shims user-installed; install instructions in annotations  **decided**

**Raised:** out-of-tree shims should be **user-installed**, with installation instructions carried in **annotations** (mentioned for orka and kagent; treat as the general rule).

**Decision:** when a provider's operator/shim isn't present, the provider reports a non-ready condition (e.g. `OperatorNotInstalled`), and the UI reads an install-instructions annotation off the `AgentProviderConfig` to tell the user how to install it. This dovetails with decision 3 (self-reporting readiness): AI Runway automates everything **except** operator install, which stays a user/UI-triggered action.

### 5. `providerOverrides` escape hatch for security context

**Raised:** security-context overrides should follow the same `provider.overrides` pattern `ModelDeployment` already uses, and the provider shims must be able to apply whatever security-context overrides their framework requires.

**Tension to resolve:** review feedback **removed** a user-facing `writableRootFilesystem` knob from `AgentDeployment.spec.config` and made the security posture **provider-owned** (a capability on `AgentProviderConfig`). Note 5 asks to add a user-facing override surface. These look contradictory.

**Confirmed, not a deviation:** the current formal design doc states **"no `spec.security`"**, so provider-owned posture plus a validated override allow-list is the intended shape, and this decision stands as written. (An earlier draft of the formal doc listed `spec.security` as a cross-framework field; that draft is superseded. Do not re-open on the basis of it.)

**Decision (recommended) — secure-by-default, override-by-exception:**
- Provider **capabilities** keep owning the *default* security posture — `writableRootFilesystem` etc. remain provider-owned defaults, not a per-agent knob.
- Add `AgentDeployment.spec.provider.overrides *runtime.RawExtension` mirroring `ModelDeployment.spec.provider.overrides` — a **documented, limited-key** escape hatch for advanced users who must override a `securityContext` for a specific provider requirement.
- The webhook **validates** the override against the documented allowed keys and rejects unknown/dangerous keys, so the default path stays locked down and the escape hatch is explicit and auditable.

This reframes [8] and note 5 as consistent: [8] removed an *unstructured, always-on* per-field knob in favor of provider-owned defaults; note 5 adds a *structured, opt-in, validated* override path. We didn't forbid overrides — we moved the default to secure and made overriding explicit.

**Implemented guardrail:** user-facing security-context overrides are accepted only through a validated allow-list, and unknown/dangerous keys are rejected by webhook validation.

## Task breakdown

Ordered; blocked items are called out. Do **not** start blocked tasks until their dependencies land.

| # | Task | Depends on | Status |
|---|------|-----------|--------|
| T1 | Catalog → annotation on `AgentProviderConfig` (parser + helpers + optional webhook validation + manifests regen) | — | **partial** — parser, helpers and manifests landed; the annotation-validating webhook did **not**. There is no `AgentProviderConfig` webhook, so a malformed catalog annotation is admitted and only surfaces later as a reconcile failure on every dependent agent. |
| T2 | Resolve `deploymentRef` via `servedName` + gateway endpoint (lower it to the `gatewayEndpoint` path; direct-Service fallback when no gateway) | — | done |
| T3 | Collapse `spec.models[]` → single `spec.model`; drop the `name: default` slot key; update config references, tests, demo manifests | T2 | done |
| T4 | Extract agent providers to `providers/agent-*` own-module dirs behind a shared provider interface (kagent, orka, container) | — (do after T1/T2 to avoid churn) | **packaging only** — `providers/agent-*` exist as separate modules and binaries, but the reconcilers still live in `controller/internal/controller/` and are re-exported through `controller/pkg/agentproviders` to work around Go's `internal` rule. Rendering has not moved out of core, core's ClusterRole still carries `kagent.dev` and `core.orka.ai`, and the shims ship no RBAC or deploy manifests. Decision 3's "each provider module renders and reconciles its downstream CRs/workloads" is **not** met. |
| T5 | Install-instructions annotation + `OperatorNotInstalled` condition surfaced to UI | T4 | done |
| T6 | Add `spec.provider.overrides` `RawExtension` + webhook allow-list validation for security-context overrides | T4 | done |
| T7 | Docs: update `docs/crd-reference.md` + `docs/gateway.md` for the binding convergence and catalog move | T1–T3 | done |

## Known gaps against the authoritative design doc

Recorded so they are decided rather than discovered. The design doc's target
frameworks are Kagent, OpenClaw, CrewAI and LangGraph.

| Gap | State |
|---|---|
| **Frameworks shipped ≠ frameworks designed** | Kagent ✅. **Orka** ships a full CRD-to-CRD provider but appears nowhere in the design — it was chosen to exercise the doc's own open question on Job-backed one-shot agents (`spec.lifecycle`). **CrewAI and LangGraph** are named targets with no `AgentProviderConfig`, no sample and no image. |
| **OpenClaw and Hermes samples are undeployable** | Both catalogs reference wrapper images (`ghcr.io/ai-runway/openclaw-agent`, `.../hermes-agent`) that have no Dockerfile, build target or source in this repo. The upstream OpenClaw image does not honour the container contract, which is why a wrapper is referenced — but the wrapper does not exist. |
| **Provider readiness is asserted by core, not reported by providers** | `AgentProviderConfigReconciler.evaluate` returns ready unconditionally for `backend: container`. That is true only while the container provider runs inside the core binary. Once the shims run standalone, core will report a framework ready whose provider is not running, and the readiness apply uses `ForceOwnership` so a shim could not correct it. Contrast the inference side, which self-registers and heartbeats from the provider process. |
| **Egress `NetworkPolicy` not built** | The formal design doc calls the auto-derived egress `NetworkPolicy` *"the one thing AI Runway actively materializes"*, with an `airunway.ai/egress: unrestricted` escape-hatch annotation. Neither exists — `grep -i networkpolicy` over all Go returns nothing. Listed as a follow-up in `controller/docs/agent-marketplace-poc.md`. This is the largest unbuilt item in the formal doc. |
| **`spec.protocols` absent; provider `protocols` unenforced** | `protocols` exists only as a declared provider capability with no enforcement — `HasProtocol` has no non-test caller, unlike its sibling `HasBindingMode` which *is* enforced. MCP deferral is explicitly sanctioned by a design-doc comment; A2A is not. (`spec.security` is **not** a gap — the current formal doc specifies no `spec.security`; see decision 5.) |
| **`InstallationInfo` not reused** | The design says to reuse the existing shape "where possible". Inference providers get structured `helmRepos`/`helmCharts`/`steps`; agent frameworks get a single plain-text sentence, so there is no UI-driven install flow for them. |
| **Marketplace metadata incomplete** | Per-catalog-item `title`/`description`/`icon`/`tags` exist, but there is no provider-level display name, description, icon or tags, and no docs URL at any level. `AgentCatalogItem.Template` — the design's "deployable templates that prefill the wizard" — has no consumer. |
| **Credential handling vs design-doc comment — no recorded rationale** | The design doc's inline comment flags `spec.model.externalAPI.credentialsRef` as carrying lateral-movement risk, and points at [kontxt](https://github.com/aramase/kontxt) / IETF Transaction Tokens: agents never hold credentials, an egress gateway exchanges the user's OAuth token for a short-lived, scope-narrowed TxToken per outbound call and injects the real upstream key itself. The implementation does the flagged thing on **every** backend — the key reaches the agent container as `OPENAI_API_KEY`/`AZURE_OPENAI_API_KEY` env (container), `apiKeySecret` (kagent), `secretRef` (orka). Nothing in the repo acknowledges the risk or reserves a seam for token exchange. **This is the one deviation with no rationale recorded anywhere** — it needs either an accepted-risk note or a forward-compatibility seam (e.g. a binding mode that resolves to a gateway URL with no credential, so the API does not have to change later). |

## Out of scope / follow-ups

- Full per-provider Anthropic/Azure `apiType` mapping — separate follow-up; `apiType` is already preserved in `status.modelBinding`.
- The PR description scopes this to "API only" while the branch also ships a PoC controller — a description/scoping correction, not a code change.
- Removing/deprecating `deploymentRef` outright (decision 1) — deferred; kept as sugar for now.
