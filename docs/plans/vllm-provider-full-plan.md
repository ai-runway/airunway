# Full Plan: First-Class vLLM Provider, Image Provenance, Launch Tags, and Recipes

## Summary

Implement issue #238 as a full feature, not an MVP: add a first-class direct **vLLM provider** for day-0 model support, including image selection/provenance, launch image support, vLLM Recipes integration, UI support, safety warnings, and documentation.

The feature will let users deploy a Hugging Face model through direct vLLM with:

- **Stable / Nightly / Launch image** choices.
- Default **`vllm/vllm-openai:cu130-nightly`** for bleeding-edge deployments.
- **Digest-pinned image status** for reproducibility.
- **`spec.engine.image`** override for launch tags and custom images.
- **vLLM Recipes import** to auto-fill model-specific image, flags, env vars, hardware guidance, and recipe provenance.
- Clear UI warnings for unstable, frozen, unsigned, unsupported, or recipe-derived images.
- A direct Kubernetes `Deployment` + `Service` reconciler for aggregated vLLM serving.

## Public API / Type Changes

### `ModelDeployment.spec.engine`

Add engine-scoped image support and keep vLLM-specific tuning as pass-through CLI args, not a typed `engine.vllm` API:

```yaml
spec:
  model:
    id: deepseek-ai/DeepSeek-V4-Flash
  engine:
    type: vllm
    image: vllm/vllm-openai:deepseekv4-cu130
    contextLength: 16384
    args:
      tokenizer-mode: deepseek_v4
      reasoning-parser: deepseek_v4
      tool-call-parser: deepseek_v4
      enable-auto-tool-choice: ""
      kv-cache-dtype: fp8
      block-size: "256"
      max-num-batched-tokens: "16384"
      max-num-seqs: "256"
      tensor-parallel-size: "4"
      pipeline-parallel-size: "1"
      data-parallel-size: "4"
      trust-remote-code: ""
      enable-prefix-caching: ""
    extraArgs:
      - --enable-expert-parallel
      - --compilation-config
      - '{"cudagraph_mode":"FULL_AND_PIECEWISE","custom_ops":["all"]}'
      - --kv-transfer-config
      - '{"kv_connector":"PyNcclConnector","kv_role":"kv_producer"}'
```

Rules:

- `spec.engine.image` is the preferred new image override.
- Existing `spec.image` remains supported for backward compatibility.
- If both are set:
  - same value: accepted.
  - different values: reject with `ImageResolved=False`.
- Keep only portable engine fields as first-class API: `type`, `image`, `contextLength`, `args`, and `extraArgs`.
- `contextLength` is a normalized Air Runway field; the vLLM provider renders it as the equivalent vLLM max-context flag.
- Do **not** introduce a new first-class remote-code trust field as part of the vLLM provider. If an existing model-source API already exposes this concept, the provider may honor it; otherwise represent vLLM's `--trust-remote-code` as `engine.args.trust-remote-code: ""`.
- Do **not** add first-class engine fields for vLLM booleans such as `enablePrefixCaching` or `enforceEager`. Recipe-derived or user-provided values for those belong in `engine.args` / `engine.extraArgs`.
- Do **not** add a typed `engine.vllm` / `VLLMEngineSpec`; vLLM adds CLI flags too frequently for the CRD to model each one.
- Use `engine.args` for deterministic flag-name/value pass-through. Keys are flag names without leading `--`; values are strings; an empty string means a valueless flag.
- Use `engine.extraArgs` for ordered, repeated, or raw argv tokens that do not fit a map shape.
- Generated normalized args are emitted first, sorted `engine.args` next, and `engine.extraArgs` last. Reject known conflicting duplicates between normalized fields and `engine.args` when they would make the manifest ambiguous.
- `provider.overrides` is reserved for direct provider/pod/rendering behavior such as probes, deployment patches, env merge behavior, and image verification policy — not normal vLLM CLI flags.

### Provider vs engine boundary

In this feature, vLLM appears in two places intentionally:

- `spec.provider.name: vllm` selects the direct vLLM provider/reconciler: Kubernetes resource rendering, provider-specific safety policy, and operational behavior.
- `spec.engine.type: vllm` describes the model-server runtime process: image, normalized engine settings, and vLLM server argv.

Even when both values are `vllm`, classify settings by what they control:

| Setting controls | Use |
|---|---|
| vLLM server command line, serving behavior, engine runtime tuning | `spec.engine.args` / `spec.engine.extraArgs` |
| portable model-server concepts, such as context length | normalized `spec.engine.*` fields |
| model artifact/source identity and existing source-level config | `spec.model.*` |
| env vars consumed by the model server | `spec.env` |
| provider/controller rendering behavior, such as probes, pod/deployment patches, Service shape, rollout behavior, image verification policy, or env merge policy | `spec.provider.overrides` |

Do not put vLLM CLI flags in `provider.overrides` just because direct vLLM is also the selected provider. Keeping CLI settings under `engine` preserves a clean boundary and leaves room for future providers that can run a vLLM engine behind a different reconciler.

### `ModelDeployment.spec.model`

Keep model-artifact settings separate from engine runtime tuning:

```yaml
spec:
  model:
    id: deepseek-ai/DeepSeek-V4-Flash
```

Rules:

- This plan should not add new vLLM-oriented booleans under `spec.model`.
- `spec.model` should carry model identity and existing model-source fields only.
- If the platform already has a model-source remote-code trust field, the vLLM renderer may translate it to the appropriate runtime flag with a security warning.
- If the value comes from a vLLM recipe and there is no existing portable/source-level field, preserve it as a vLLM CLI flag in `spec.engine.args` / `spec.engine.extraArgs`.

### Recipe provenance annotations

The UI/backend recipe resolver sets the actual deployment fields: `spec.engine.image`, `spec.engine.args`, `spec.engine.extraArgs`, `spec.env`, and resource defaults. The controller must not use a recipe reference to look up or infer vLLM flags during reconciliation.

Do **not** add `spec.recipe` or `spec.recipes` in the initial CRD. Recipe provenance is optional, non-authoritative metadata stored in annotations:

```yaml
metadata:
  annotations:
    airunway.ai/generated-by: vllm-recipe-resolver
    airunway.ai/recipe.source: vllm-recipes
    airunway.ai/recipe.id: deepseek-ai/DeepSeek-V4-Flash
    airunway.ai/recipe.strategy: single_node_dep
    airunway.ai/recipe.hardware: h200
    airunway.ai/recipe.variant: default
    airunway.ai/recipe.precision: fp8
    airunway.ai/recipe.features: '["reasoning"]'
    airunway.ai/recipe.revision: abc1234
```

Annotation contract:

- annotations are audit/UI provenance only;
- reconciliation behavior comes from materialized `spec` fields only;
- editing recipe annotations alone must not change the rendered vLLM command line;
- if useful, the controller may copy selected recipe annotations to owned resources for traceability, but must not derive runtime flags from them.

### `ModelDeployment.status`

Add image status only; recipe provenance stays in `metadata.annotations` unless a later API revision needs a typed status field:

```yaml
status:
  image:
    requested: vllm/vllm-openai:cu130-nightly
    resolved: vllm/vllm-openai@sha256:...
    repository: vllm/vllm-openai
    tag: cu130-nightly
    digest: sha256:...
    source: nightly
    createdAt: "2026-05-02T..."
    revision: abc1234
    age: 14h
    inNightly: true
    verified: false
    verificationMessage: Image signature not found
    message: Resolved and pinned nightly image
```

Add conditions:

- `ImageResolved`
- `ImageVerified`
- `UnsupportedImage`
- `RecipeResolved`
- `NightlyAvailable`
- `HardwareCompatible`

Regenerate CRDs and generated Go code after editing API types:

```bash
cd controller
make manifests generate
```

## Provider Implementation

### New provider package

Create `providers/vllm/` with the same structure as the existing provider packages:

```text
providers/vllm/
  Dockerfile
  Makefile
  go.mod
  go.sum
  cmd/main.go
  config.go
  controller.go
  transformer.go
  image_resolver.go
  image_verifier.go
  recipe_annotations.go
  status.go
  *_test.go
  config/
  deploy/vllm.yaml
```

### Provider registration

Register an `InferenceProviderConfig` named `vllm`:

```yaml
spec:
  capabilities:
    engines:
      - vllm
    servingModes:
      - aggregated
    gpuSupport: true
    cpuSupport: false
  selectionRules:
    - condition: "has(spec.resources.gpu) && spec.resources.gpu.count > 0 && spec.engine.type == 'vllm'"
      priority: 10
```

Priority `10` keeps direct vLLM below Dynamo/KubeRay for normal auto-selection, while still allowing explicit selection through:

```yaml
spec:
  provider:
    name: vllm
```

### Reconciler behavior

The vLLM reconciler processes only `ModelDeployment` resources where:

```yaml
status.provider.name: vllm
```

For each deployment:

1. Validate provider compatibility.
2. Resolve effective image.
3. Preserve/copy recipe provenance annotations if present; do not fetch or resolve recipes during reconciliation.
4. Generate vLLM CLI args from normalized model/engine fields plus `engine.args` / `engine.extraArgs`.
5. Create/update one `apps/v1.Deployment`.
6. Create/update one `core/v1.Service`.
7. Update provider, endpoint, replica, image, and condition status.

### Aggregated serving

Initial required reconciler output:

- one `Deployment`
- one `Service`
- one vLLM container
- port `8000`

Generated pod includes:

- image from resolver
- vLLM command/args
- GPU limits/requests from `spec.resources.gpu`
- CPU/memory from `spec.resources`
- `HF_TOKEN` from `spec.secrets.huggingFaceToken`
- user env from `spec.env`
- recipe env from materialized spec
- `/dev/shm` `emptyDir`
- HF/cache PVC mounts from `spec.model.storage`
- `nodeSelector`
- `tolerations`
- pod labels/annotations
- startup probe with long timeout
- readiness probe
- liveness probe

Reject direct vLLM disaggregated mode in the first provider pass unless the recipe resolver has materialized a supported prefill/decode/router topology. For unsupported disaggregated requests, set a clear failure message.

## Image Resolution and Provenance

### Effective image selection

Controller resolution order:

1. `spec.engine.image`
2. `spec.image`
3. provider default: `vllm/vllm-openai:cu130-nightly`

For known recipe models, the backend/UI resolver may recommend a launch image. The UI should show it as a recommendation, but the final deployment spec must still contain the selected image explicitly. The controller must not read recipe annotations to choose an image.

### Resolver behavior

Implement using `go-containerregistry`.

For tag-based images:

- resolve tag to digest
- record digest in `status.image.digest`
- record resolved digest image in `status.image.resolved`
- record tag age and revision if available
- classify image source:
  - `stable`
  - `nightly`
  - `launch`
  - `custom`

For digest-pinned images:

- preserve image exactly
- parse digest
- record status

For default nightly:

- generated pod image should use digest-pinned form when resolution succeeds:

```text
vllm/vllm-openai@sha256:...
```

For user-specified launch/custom images:

- preserve the user’s requested image in the pod by default.
- still record digest if resolvable.
- surface `UnsupportedImage=True` for unknown/custom images.

### Failure behavior

If image resolution fails:

- set `ImageResolved=False`
- include the resolver error in `status.image.message`
- continue only for user-supplied images when safe
- for default nightly, fail reconciliation because reproducibility depends on digest resolution

## Image Verification

Add cosign verification on the resolved digest.

Default policy: `Warn`.

Supported policies:

- `Disabled`
- `Warn`
- `Enforce`

Behavior:

- `ImageVerified=True`: signature verified.
- `ImageVerified=False`: unsigned or verification failed.
- `ImageVerified=Unknown`: verification skipped or unsupported.

With policy `Warn`, unsigned launch images are allowed but visible in status/UI.

With policy `Enforce`, unsigned images block deployment.

## Image Catalog and Launch Tag Discovery

Add backend services:

```text
backend/src/services/registryClient.ts
backend/src/services/vllmImageCatalog.ts
backend/src/routes/vllmImages.ts
```

Expose:

```http
GET /api/vllm/images/tags?repository=vllm/vllm-openai
GET /api/vllm/images/resolve?image=vllm/vllm-openai:cu130-nightly
GET /api/vllm/images/recommend?model=deepseek-ai/DeepSeek-V4-Flash
```

Catalog behavior:

- discover recent `vllm/vllm-openai` tags
- classify stable/nightly/launch/custom tags
- resolve digest
- track first-seen time
- expose image age
- expose “in nightly?” as `yes`, `no`, or `unknown`
- cache responses with TTL
- use stale cache on registry failure
- avoid adding an `ImageCatalog` CRD for this feature

## Nightly Recovery Hint

When a deployment uses a launch image, backend/controller should detect when nightly likely includes the needed model support.

Set:

```yaml
conditions:
  - type: NightlyAvailable
    status: "True"
    reason: LaunchTagLikelyObsolete
    message: A newer nightly image appears to support this model. Consider switching back to nightly for ongoing fixes.
```

UI banner:

> This launch image is a frozen snapshot. A newer nightly appears to support this model now. Switch back to nightly for ongoing fixes.

## vLLM Recipes Integration

### Backend services

Add:

```text
backend/src/services/vllmRecipesClient.ts
backend/src/services/vllmRecipeResolver.ts
backend/src/routes/vllmRecipes.ts
shared/types/vllmRecipes.ts
```

Expose:

```http
GET /api/vllm/recipes
GET /api/vllm/recipes/:org/:model
POST /api/vllm/recipes/resolve
```

### Recipe discovery

Use the public vLLM Recipes catalog as the source of truth.

Lookup flow:

1. Load recipe index.
2. Match by Hugging Face model id / model name selected in the UI.
3. Prefer exact `org/model` matches; use normalized aliases or fuzzy/name-only matches only as suggestions that require user confirmation.
4. Prefer structured JSON/YAML recipe data.
5. Cache recipe index and model recipe payload.
6. Record recipe revision/etag/commit when available.
7. If recipe is missing, fall back to manual image/args flow.

The UI/backend recipe resolver is the layer that should translate matched recipes into `engine.args`, `engine.extraArgs`, env vars, image, and resource defaults. The controller should receive a fully materialized `ModelDeployment` and should not fetch recipes or infer model-specific vLLM flags during reconciliation.

### Recipe resolver

Input:

```json
{
  "modelId": "deepseek-ai/DeepSeek-V4-Flash",
  "mode": "aggregated",
  "hardware": "h200",
  "strategy": "single_node_dep",
  "variant": "default",
  "features": ["reasoning"],
  "imageChoice": "launch"
}
```

Notes for DeepSeek-V4-Flash:

- `aggregated` is the Air Runway serving mode, not a vLLM recipe strategy.
- Valid vLLM recipe strategies for this model include `single_node_tep`, `single_node_dep`, `single_node_tp`, `multi_node_dep`, and `pd_cluster`; `single_node_tep` is the recipe default.
- The recipe variant key is `default`; `fp8` is precision metadata, not the variant id.
- The model-specific structured recipe does not expose `model.docker_image`; image recommendations may come from guide examples, an image catalog, provider defaults, or user override.

Output:

```json
{
  "provider": "vllm",
  "engine": "vllm",
  "mode": "aggregated",
  "imageRef": "vllm/vllm-openai:deepseekv4-cu130",
  "resources": {
    "gpu": 4,
    "memory": "..."
  },
  "engineArgs": {
    "reasoning-parser": "deepseek_v4",
    "kv-cache-dtype": "fp8",
    "block-size": "256",
    "data-parallel-size": "4",
    "trust-remote-code": ""
  },
  "engineExtraArgs": [
    "--enable-expert-parallel",
    "--compilation-config",
    "{\"cudagraph_mode\":\"FULL_AND_PIECEWISE\",\"custom_ops\":[\"all\"]}"
  ],
  "env": {
    "VLLM_ENGINE_READY_TIMEOUT_S": "3600"
  },
  "annotations": {
    "airunway.ai/generated-by": "vllm-recipe-resolver",
    "airunway.ai/recipe.source": "vllm-recipes",
    "airunway.ai/recipe.id": "deepseek-ai/DeepSeek-V4-Flash",
    "airunway.ai/recipe.strategy": "single_node_dep",
    "airunway.ai/recipe.hardware": "h200",
    "airunway.ai/recipe.variant": "default",
    "airunway.ai/recipe.precision": "fp8",
    "airunway.ai/recipe.features": "[\"reasoning\"]",
    "airunway.ai/recipe.revision": "..."
  },
  "warnings": []
}
```

The annotation object in resolver output is provenance for the UI/manifest preview. It is not a compact substitute for `engineArgs`; the resolver must still materialize every image, arg, env var, and resource default that should affect deployment.

If `tool_calling` is selected for DeepSeek-V4-Flash, materialize all tool-calling args together: `tokenizer-mode: deepseek_v4`, `tool-call-parser: deepseek_v4`, and `enable-auto-tool-choice: ""`. Reasoning-only selection should add only `reasoning-parser: deepseek_v4`.

Guide-only args, such as `--disable-uvicorn-access-log`, may be offered as optional recommendations or provider defaults, but should not be treated as authoritative structured recipe output.

### Translation rules

The recipe resolver returns normal deployment-form fields; the UI applies them to the manifest preview before the user deploys. Recipe-derived settings are not hidden provider overrides.

Map recipe fields into Air Runway fields:

| Recipe data | Air Runway target |
|---|---|
| model id | `spec.model.id` |
| existing Air Runway model-source options | `spec.model.*` |
| docker image if structured in recipe, otherwise selected recommendation/default/override | `spec.engine.image` |
| base args | `spec.engine.args` and `spec.engine.extraArgs` |
| feature args | `spec.engine.args` and `spec.engine.extraArgs` |
| env vars | `spec.env` |
| GPU count | `spec.resources.gpu.count` |
| storage/cache hints | `spec.model.storage` |
| hardware profile | optional provenance annotation: `metadata.annotations["airunway.ai/recipe.hardware"]` |
| strategy | optional provenance annotation: `metadata.annotations["airunway.ai/recipe.strategy"]` |
| variant/precision | optional provenance annotations: `metadata.annotations["airunway.ai/recipe.variant"]` / `metadata.annotations["airunway.ai/recipe.precision"]` |
| recipe revision | optional provenance annotation: `metadata.annotations["airunway.ai/recipe.revision"]` |

Recipe fields that correspond to vLLM CLI flags, including `--trust-remote-code` when no existing source-level field is available, should be materialized as `spec.engine.args` / `spec.engine.extraArgs`, not as new ad hoc model/provider fields. Recipe provenance must not be the source of truth for rendering.

Safety rules:

- Do not execute shell snippets.
- Do not run arbitrary setup or dependency commands from recipes. Surface dependency notes/warnings instead.
- Parse recipe args into safe argv tokens; do not execute recipe shell snippets.
- vLLM flags go to `engine.args`; ordered, repeated, or raw tokens go to `engine.extraArgs`.
- Unsupported recipe operations become warnings.
- Recipe output must be visible before deploy.

## Disaggregated Recipe Support

Support two phases in one architecture.

### Aggregated

Required path:

```yaml
serving:
  mode: aggregated
```

Creates one vLLM Deployment + Service.

### Disaggregated

Recipe-driven only at first.

For recipes with prefill/decode/router strategy:

- create prefill Deployment
- create decode Deployment
- create router Deployment
- create router Service
- map recipe env and KV transfer config
- map prefill/decode GPU counts into `spec.scaling`
- mark unsupported strategies with clear warnings

If the recipe requires Mooncake/NIXL features not supported by the direct provider, block deployment with a plain message unless the user selects an explicitly supported recipe strategy.

## Hardware and CUDA Compatibility

Add compatibility checks using:

- recipe hardware profile
- cluster GPU capacity data
- known GPU labels when available
- image tag CUDA suffix, such as `cu129` or `cu130`
- selected image architecture metadata when available

Behavior:

- warn when selected recipe hardware does not match detected cluster hardware.
- warn when CUDA tag appears incompatible.
- do not force node affinity unless the cluster exposes reliable labels.
- allow users to set node selectors/tolerations manually.

Status:

```yaml
conditions:
  - type: HardwareCompatible
    status: Unknown
    reason: HardwareNotDetected
    message: Could not confirm whether this image matches your GPU type.
```

## Backend Runtime Status

Update runtime discovery so `vllm` appears in the UI.

For `vllm`:

- no external upstream operator CRD required
- installed when `InferenceProviderConfig/vllm.status.ready == true`
- healthy when the provider controller heartbeat is fresh
- display name: `vLLM`

Runtime order in UI:

1. NVIDIA Dynamo
2. KubeRay
3. KAITO
4. vLLM
5. llm-d

## Frontend UI

### Runtime selection

Add runtime id:

```ts
type RuntimeId = 'dynamo' | 'kuberay' | 'kaito' | 'llmd' | 'vllm'
```

Add runtime info:

```ts
vllm: {
  name: 'vLLM',
  description: 'Direct vLLM for newest model support and custom launch images',
  defaultNamespace: 'default',
}
```

Add engine support:

```ts
vllm: ['vllm']
```

### Bleeding-edge vLLM path

When `vllm` is selected, show a plain-language panel:

> Use direct vLLM when a model is too new for the managed runtimes. This gives you newer model support sooner, but images and settings may change quickly.

### Image chooser

Show three choices.

#### Stable

- pinned stable tag
- safest option
- use image catalog to populate latest stable tag
- disabled if no stable tag is available

#### Nightly

- default for vLLM path
- uses `vllm/vllm-openai:cu130-nightly`
- shows digest/age when resolved
- warning: “Nightly images change often.”

#### Launch image

- free-form image input
- dropdown of recent launch tags from image catalog
- show:
  - tag
  - first seen
  - digest
  - likely model association
  - in nightly: yes/no/unknown
- warning: “Launch images are frozen snapshots and may not receive fixes.”

### Recipe panel

When a recipe exists for the selected model:

- show “Official vLLM recipe found”
- show whether the recipe was matched by exact model id, alias, or fuzzy/name-only match
- show recipe source/revision
- show hardware selector
- show strategy selector
- show variant selector
- show recommended image
- show generated `engine.args`, `engine.extraArgs`, env, and resource preview
- show warnings, especially for non-exact matches
- let user apply recipe to populate the deployment form
- let user manually edit advanced args after applying recipe

If no recipe exists:

- show manual launch-image and args path
- optionally show a hint to upstream working settings to vLLM Recipes

### Deployment details page

Display:

- selected image
- resolved digest
- image source: Stable/Nightly/Launch/Custom
- image age
- signature status
- recipe provenance
- nightly recovery hint
- unsupported image warning

## Shared Type Updates

Update `shared/types/deployment.ts` for UI/backend form state and manifest generation:

```ts
export interface RecipeProvenance {
  source?: string
  id?: string
  strategy?: string
  hardware?: string
  variant?: string
  precision?: string
  features?: string[]
  revision?: string
}

export interface ImageStatus {
  requested?: string
  resolved?: string
  repository?: string
  tag?: string
  digest?: string
  source?: 'stable' | 'nightly' | 'launch' | 'custom'
  createdAt?: string
  revision?: string
  age?: string
  inNightly?: boolean
  verified?: boolean
  verificationMessage?: string
  message?: string
}
```

Add to `DeploymentConfig`:

```ts
imageChoice?: 'stable' | 'nightly' | 'launch'
// Optional UI/backend provenance only; rendered as metadata.annotations, not spec.recipe.
recipeProvenance?: RecipeProvenance
engineExtraArgs?: string[]
```

Continue using the existing `engineArgs?: Record<string, unknown>` field for flag-name/value pass-through.

Do not add a new model-source remote-code trust field for this vLLM feature. If such a field already exists elsewhere, preserve its current API and renderer behavior; recipe-import work should not create it.

Add to `EngineSpec`:

```ts
image?: string
contextLength?: number
args?: Record<string, string>
extraArgs?: string[]
```

Add to `ModelDeploymentStatus`:

```ts
image?: ImageStatus
```

Mapping:

- `config.imageRef` maps to `spec.engine.image` for provider `vllm`.
- existing non-vLLM flows may continue using `spec.image`.
- `config.engineArgs` maps to `spec.engine.args`.
- `config.engineExtraArgs` maps to `spec.engine.extraArgs`.
- `config.recipeProvenance` maps to `metadata.annotations["airunway.ai/recipe.*"]`.

## Documentation

Add:

```text
docs/providers/vllm.md
docs/recipes/vllm-recipes.md
docs/deployments/bleeding-edge-vllm.md
docs/api.md updates
docs/crd-reference.md updates
```

Documentation must cover:

- when to use direct vLLM
- when to prefer Dynamo/KAITO/KubeRay
- Stable vs Nightly vs Launch images
- digest pinning
- signature status
- unsupported image warnings
- recipe import workflow
- manual args fallback
- DeepSeek-V4-Flash example
- custom launch image example
- nightly recovery workflow
- security and reproducibility tradeoffs

## Example Manifests

### Default nightly

```yaml
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: qwen-nightly-vllm
spec:
  provider:
    name: vllm
  model:
    id: Qwen/Qwen3-0.6B
  engine:
    type: vllm
  resources:
    gpu:
      count: 1
```

### Launch image

```yaml
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: deepseek-v4-flash
spec:
  provider:
    name: vllm
  mode: aggregated
  model:
    id: deepseek-ai/DeepSeek-V4-Flash
  engine:
    type: vllm
    image: vllm/vllm-openai:deepseekv4-cu130
    args:
      tokenizer-mode: deepseek_v4
      reasoning-parser: deepseek_v4
      kv-cache-dtype: fp8
      block-size: "256"
      data-parallel-size: "4"
    extraArgs:
      - --enable-expert-parallel
  resources:
    gpu:
      count: 4
```

### Recipe-backed, materialized by UI

Recipe provenance is stored in annotations only. Deployment behavior comes from the materialized `engine`, `env`, and `resources` fields.

```yaml
apiVersion: airunway.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: deepseek-v4-flash-recipe
  annotations:
    airunway.ai/generated-by: vllm-recipe-resolver
    airunway.ai/recipe.source: vllm-recipes
    airunway.ai/recipe.id: deepseek-ai/DeepSeek-V4-Flash
    airunway.ai/recipe.strategy: single_node_dep
    airunway.ai/recipe.hardware: h200
    airunway.ai/recipe.variant: default
    airunway.ai/recipe.precision: fp8
    airunway.ai/recipe.features: '["reasoning"]'
    airunway.ai/recipe.revision: abc1234
spec:
  provider:
    name: vllm
  mode: aggregated
  model:
    id: deepseek-ai/DeepSeek-V4-Flash
  engine:
    type: vllm
    image: vllm/vllm-openai:deepseekv4-cu130
    args:
      reasoning-parser: deepseek_v4
      kv-cache-dtype: fp8
      block-size: "256"
      data-parallel-size: "4"
      trust-remote-code: ""
    extraArgs:
      - --enable-expert-parallel
      - --compilation-config
      - '{"cudagraph_mode":"FULL_AND_PIECEWISE","custom_ops":["all"]}'
  env:
    VLLM_ENGINE_READY_TIMEOUT_S: "3600"
  resources:
    gpu:
      count: 4
```

## Test Plan

### Controller/API tests

- CRD schema includes new fields.
- `spec.engine.image` accepted.
- `spec.image` still accepted.
- conflicting `spec.image` and `spec.engine.image` rejected.
- recipe provenance annotations are accepted/preserved as ordinary metadata.
- changing recipe provenance annotations alone does not change rendered CLI flags.
- CRD schema does not add `spec.recipe` or `spec.recipes`.
- `spec.engine.args` accepted and converted to CLI flags.
- provider selection keeps `vllm` below Dynamo/KubeRay.
- explicit provider selection chooses `vllm`.

### vLLM provider tests

- provider config registration.
- aggregated Deployment generated.
- Service generated.
- default nightly image selected.
- default nightly digest-pinned when resolved.
- custom launch image preserved.
- digest status populated.
- image resolver failure condition.
- cosign verified/unsigned/skipped conditions.
- `UnsupportedImage` condition for custom image.
- `contextLength` renders to the vLLM max-context flag.
- `engine.args.trust-remote-code: ""` renders to `--trust-remote-code` and triggers the same security warning as any remote-code trust path.
- the vLLM feature does not introduce a new first-class model-source remote-code trust field.
- `engine.args` forwarded as sorted CLI flag-name/value pairs.
- `engine.extraArgs` appended in caller-provided order.
- vLLM-specific flags such as prefix caching and enforce eager are accepted only through `engine.args` / `engine.extraArgs`, not as first-class engine fields.
- readiness/startup probes present.
- dshm volume present.
- HF token env present.
- GPU resources present.
- storage volume mounts present.

### Backend tests

- image catalog tag listing.
- stable/nightly/launch classification.
- digest resolution.
- image age calculation.
- first-seen cache.
- stale cache fallback.
- “in nightly?” logic.
- recipe index fetch.
- recipe model lookup.
- recipe resolve for DeepSeek-V4-Flash.
- recipe missing fallback.
- unsupported recipe operations become warnings.
- runtime status includes `vllm`.

### Frontend tests

- vLLM runtime appears.
- vLLM runtime selectable only for vLLM-compatible models.
- Stable/Nightly/Launch image UI renders.
- Nightly is default.
- launch image required when launch option selected.
- launch dropdown uses backend catalog data.
- recipe panel appears when recipe exists.
- recipe apply populates image, args, env, resources, and recipe annotations.
- status page shows digest, image source, verification, and recipe provenance.
- warnings use plain language.

### End-to-end smoke tests

- deploy small model with default nightly.
- deploy small model with custom launch image.
- generate manifest from DeepSeek-V4-Flash recipe.
- verify `Deployment` and `Service` are created.
- verify `status.image.digest` is populated when registry access works.
- verify recipe provenance appears in metadata annotations.
- verify UI preview matches submitted manifest.

## Validation Commands

After API/controller changes:

```bash
cd controller
make manifests generate
go build ./...
go test ./...
```

After provider changes:

```bash
cd providers/vllm
go build ./...
go test ./...
```

After frontend/backend/shared changes:

```bash
bun run test
```

After all changes:

```bash
make compile
```

If controller API files changed, ensure generated CRDs and deepcopy files are committed.

## PR Breakdown

### PR 1 — CRD/API schema

- `spec.engine.image`
- `spec.engine.extraArgs`
- `status.image`
- no `spec.recipe`, `spec.recipes`, or `status.recipe` in the initial CRD; use metadata annotations for recipe provenance
- new conditions
- generated CRDs/deepcopy/shared TS types

### PR 2 — Direct vLLM provider

- `providers/vllm`
- low-priority provider config
- aggregated Deployment/Service reconciler
- basic status mapping
- unit tests

### PR 3 — Image resolver and verifier

- digest resolution
- image source classification
- status image fields
- cosign verification
- unsupported-image condition
- tests

### PR 4 — Image catalog backend

- registry client
- tag discovery
- first-seen cache
- Stable/Nightly/Launch classification
- “in nightly?” and recovery hint logic
- API routes
- tests

### PR 5 — vLLM Recipes backend

- recipes client/cache
- recipe resolver
- recipe-to-deployment translation
- DeepSeek-V4-Flash test fixture
- API routes
- tests

### PR 6 — Frontend vLLM path

- vLLM runtime card
- Bleeding-edge vLLM panel
- Stable/Nightly/Launch selector
- launch dropdown
- recipe panel
- manifest preview updates
- deployment status image/recipe display
- tests

### PR 7 — Disaggregated recipe support

- prefill/decode/router mapping
- recipe strategy support
- component status
- warnings for unsupported PD recipe features
- tests

### PR 8 — Docs and examples

- provider docs
- recipe docs
- CRD/API docs
- DeepSeek-V4-Flash example
- troubleshooting guide

## Assumptions

- Direct `vllm` is a separate provider named `vllm`; `llmd` remains unchanged.
- `spec.engine.image` is added because the issue explicitly calls for it.
- Existing `spec.image` remains supported for backward compatibility.
- Recipes are integrated through the backend/UI and materialized into `ModelDeployment`; the controller does not fetch recipes during reconciliation.
- Aggregated direct vLLM is required first; disaggregated support is recipe-driven and implemented after the aggregated path is stable.
- Backend image catalog is preferred over an `ImageCatalog` CRD.
- Cosign policy defaults to warn, not enforce, so launch-day workflows are not blocked by missing signatures.
