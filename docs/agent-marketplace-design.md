# Agent Marketplace — Design Notes (post-review)

> Working design doc for the Agent Marketplace (issue #200, PR #287). Captures the decisions coming out of the review round and a design meeting, grounded against the code on `feat/200-agent-marketplace-api`. Not wired into the website sidebar — this is an internal ADR-style record, not user-facing product docs.

## Throughline

The model side (`ModelDeployment` + `InferenceProviderConfig` + `providers/*`) already solved most of the problems the agent side is now hitting. The consistent answer to the review/meeting feedback is **reuse the model side's patterns** rather than invent parallel ones:

- Model binding → prefer the OpenAI-native gateway path the model side already ships (base URL + served model name, routed by BBR).
- Metadata that isn't runtime state → annotations, not spec (the model side keeps UI/catalog concerns out of `spec`).
- Provider-specific logic → out-of-tree provider modules behind an interface, exactly like `providers/{dynamo,kaito,kuberay,llmd}/`.
- Escape hatch for the long tail → a documented `provider.overrides` `RawExtension`, exactly like `ModelDeployment.spec.provider.overrides`.

## Current state (grounded)

- Binding: `AgentDeployment.spec.model` with exactly one of three modes — `deploymentRef`, `gatewayEndpoint`, `externalAPI`. Resolved output is written as `status.modelBinding` (singular), so providers consume one normalized binding contract without a slot-key indirection.
- Catalog: `AgentProviderConfig.spec.catalog []AgentCatalogItem` — currently **spec-level** (`agentproviderconfig_types.go:106-177`).
- Providers: agent providers are **in-tree** (`controller/internal/controller/agentprovider_{kagent,orka,container}_*.go`), unlike model providers which are **out-of-tree own-module dirs** (`providers/*/` each with `go.mod` + `cmd/main.go` + `Dockerfile` + controller/transformer). `providers/README.md` states the intention is for *all* providers to live out-of-tree.
- Provider readiness: `AgentProviderConfigReconciler` now sets `status.ready` itself (added this review round) — no more hand-patching.
- Reusable model-side patterns: `ModelDeployment.spec.model.servedName` (`modeldeployment_types.go:164-168`) and `ModelDeployment.spec.provider.overrides *runtime.RawExtension` (`modeldeployment_types.go:191`).

## Decisions

### 1. Model binding — converge on the OpenAI-native gateway path

**Raised:** Sertac disliked `deploymentRef` exposing a Kubernetes-native reference as the primary binding UX; there was discussion of offering the served model name / model id instead, and of keeping `externalAPI` but letting it point at an *internal* OpenAI-compatible service. Suraj noted the served model name/id already lives on the `ModelDeployment`.

**Decision (recommended):**
- Make `gatewayEndpoint` (base URL + served model name, BBR-routed) the **canonical, recommended** binding. It is OpenAI-native and already implemented — nothing new to build for the happy path.
- **Keep `deploymentRef`** as ergonomic sugar so "airunway does everything" still holds, but change it to **resolve through the gateway**: the core reads `ModelDeployment.spec.model.servedName` (Suraj's point) and produces the same resolved endpoint + served name a `gatewayEndpoint` binding would, instead of pointing the agent straight at the backing Service. `deploymentRef` becomes a convenience that lowers to `gatewayEndpoint`, not a separate routing path.
- **Keep `externalAPI`** unchanged — its base URL is just a URL, so pointing it at an internal OpenAI-compatible Service already works today.
- **Collapse `spec.models[]` → a single `spec.model` block** now that `MaxItems=1`. This deletes the confusing `name: default` slot key; `spec.config` references the single binding implicitly. Alpha API, unreleased, on a feature branch → a breaking rename is acceptable here.

**Why this over the alternatives:** it satisfies the OpenAI-native ask *and* the "airunway owns the whole path" directive without breaking the working PoCs — they keep using `deploymentRef`, it just resolves via `servedName` + gateway underneath.

**Follow-up call:** whether/when to *deprecate or remove* `deploymentRef` entirely vs. keep it as sugar indefinitely. Current implementation keeps it as sugar.

**Trade-offs:** routing `deploymentRef` through the gateway makes the gateway a hard dependency for the in-cluster happy path; today the PoCs can hit the model Service directly. Mitigation: fall back to direct-Service resolution when no gateway is present, and surface that in `status`.

### 2. Catalog → annotation, not spec  **decided (meeting was explicit)**

**Raised:** "catalog should be an annotation; annotations should not be a spec-level thing." Catalog is curated UI metadata (tiles, icons, one-click recipes), not runtime state the controller reconciles.

**Decision:** move `AgentProviderConfig.spec.catalog` out of `spec` into an annotation (e.g. `airunway.io/agent-catalog`) carrying the catalog as a JSON document. Keep `GetCatalogItem` / `CatalogItemNames` helpers but source them from the parsed annotation.

**Trade-offs:** annotations are unstructured (no CRD schema validation) and capped at ~256KB total per object. Mitigate with (a) a documented JSON shape + a small typed parser, and (b) optional webhook validation of the annotation payload so authors still get errors early.

### 3. Lift agent providers out-of-tree behind a provider interface  **decided (meeting was explicit)**

**Raised:** "create a provider interface to keep [providers] out of tree just like model providers" — keeps provider-specific code out of the core and lets new providers ship independently.

**Decision:** extract the in-tree agent provider controllers into own-module directories mirroring `providers/*` (proposed: `providers/agent-kagent/`, `providers/agent-orka/`, `providers/agent-container/`), each behind a shared provider interface. The core controller resolves bindings + writes `status.modelBinding`; each provider module renders and reconciles its downstream CRs/workloads and reports readiness (the readiness reconciler added this round already establishes the self-reporting contract).

**Trade-offs:** multi-module repo overhead (separate `go.mod`, build/test/release per provider — see the existing `providers/dynamo` module boundary). Worth it for the same reasons the model side already accepted it.

### 4. Shims user-installed; install instructions in annotations  **decided (meeting was explicit)**

**Raised:** out-of-tree shims should be **user-installed**, with installation instructions carried in **annotations** (mentioned for orka and kagent; treat as the general rule).

**Decision:** when a provider's operator/shim isn't present, the provider reports a non-ready condition (e.g. `OperatorNotInstalled`), and the UI reads an install-instructions annotation off the `AgentProviderConfig` to tell the user how to install it. This dovetails with decision 3 (self-reporting readiness) and the "airunway does everything **except** operator install" directive — operator install stays a user/UI-triggered action.

### 5. `providerOverrides` escape hatch for security context

**Raised:** "take a look at provider overrides for security context. `providerOverrides` is how we handle it in ModelDeployments … steal the way we do it from there … make sure the provider shims can handle any security-context overrides needed for provider-specific stuff."

**Tension to resolve:** the review round (comment [8]) **removed** a user-facing `writableRootFilesystem` knob from `AgentDeployment.spec.config` and made the security posture **provider-owned** (a capability on `AgentProviderConfig`). Note 5 asks to add a user-facing override surface. These look contradictory.

**Decision (recommended) — secure-by-default, override-by-exception:**
- Provider **capabilities** keep owning the *default* security posture (comment [8] stands — `writableRootFilesystem` etc. remain provider-owned defaults, not a per-agent knob).
- Add `AgentDeployment.spec.provider.overrides *runtime.RawExtension` mirroring `ModelDeployment.spec.provider.overrides` — a **documented, limited-key** escape hatch for advanced users who must override a `securityContext` for a specific provider requirement.
- The webhook **validates** the override against the documented allowed keys and rejects unknown/dangerous keys, so the default path stays locked down and the escape hatch is explicit and auditable.

This reframes [8] and note 5 as consistent: [8] removed an *unstructured, always-on* per-field knob in favor of provider-owned defaults; note 5 adds a *structured, opt-in, validated* override path. We didn't forbid overrides — we moved the default to secure and made overriding explicit.

**Implemented guardrail:** user-facing security-context overrides are accepted only through a validated allow-list, and unknown/dangerous keys are rejected by webhook validation.

## Task breakdown

Ordered; blocked items are called out. Do **not** start blocked tasks until the sign-off items land.

| # | Task | Depends on | Status |
|---|------|-----------|--------|
| T1 | Catalog → annotation on `AgentProviderConfig` (parser + helpers + optional webhook validation + manifests regen) | — | done |
| T2 | Resolve `deploymentRef` via `servedName` + gateway endpoint (lower it to the `gatewayEndpoint` path; direct-Service fallback when no gateway) | — | done |
| T3 | Collapse `spec.models[]` → single `spec.model`; drop the `name: default` slot key; update config references, tests, demo manifests | T2 | done |
| T4 | Extract agent providers to `providers/agent-*` own-module dirs behind a shared provider interface (kagent, orka, container) | — (do after T1/T2 to avoid churn) | done |
| T5 | Install-instructions annotation + `OperatorNotInstalled` condition surfaced to UI | T4 | done |
| T6 | Add `spec.provider.overrides` `RawExtension` + webhook allow-list validation for security-context overrides | T4 | done |
| T7 | Docs: update `docs/crd-reference.md` + `docs/gateway.md` for the binding convergence and catalog move | T1–T3 | done |

## Out of scope / follow-ups

- PR #287 review comment [12] (full per-provider anthropic/azure `apiType` mapping) — separate follow-up; `apiType` is already preserved in `status.modelBinding`.
- PR #287 comments [2]/[4] (PR says "API only" but ships a PoC controller) — a scoping/PR-description call for @robert-cronin, not a code change.
- Removing/deprecating `deploymentRef` outright (decision 1) — deferred pending sign-off; kept as sugar for now.
