import type {
  DeploymentMode,
  RecipeProvenance,
  VllmRecipeRawPayload,
  VllmRecipeResolveRequest,
  VllmRecipeResolveResult,
} from '@airunway/shared';
import { vllmRecipesClient, type VllmRecipesClient } from './vllmRecipesClient';

const GENERATED_BY_ANNOTATION = 'airunway.ai/generated-by';
const RECIPE_SOURCE_ANNOTATION = 'airunway.ai/recipe.source';
const RECIPE_ID_ANNOTATION = 'airunway.ai/recipe.id';
const RECIPE_STRATEGY_ANNOTATION = 'airunway.ai/recipe.strategy';
const RECIPE_HARDWARE_ANNOTATION = 'airunway.ai/recipe.hardware';
const RECIPE_VARIANT_ANNOTATION = 'airunway.ai/recipe.variant';
const RECIPE_PRECISION_ANNOTATION = 'airunway.ai/recipe.precision';
const RECIPE_FEATURES_ANNOTATION = 'airunway.ai/recipe.features';
const RECIPE_REVISION_ANNOTATION = 'airunway.ai/recipe.revision';

const ENGINE_ARG_KEY_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_-]*$/;

type JsonRecord = Record<string, unknown>;

interface CommandSource {
  command: JsonRecord;
  sourceLabel: string;
}

interface ArgParseResult {
  engineArgs: Record<string, string>;
  engineExtraArgs: string[];
}

interface AlternativeCandidate {
  record: JsonRecord;
  key?: string;
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 ? value : undefined;
}

function asStringArray(value: unknown): string[] | undefined {
  return Array.isArray(value) && value.every((item) => typeof item === 'string')
    ? value
    : undefined;
}

function asNumber(value: unknown): number | undefined {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value;
  }

  if (typeof value === 'string' && value.trim().length > 0) {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : undefined;
  }

  return undefined;
}

function normalizeMatchText(value: string): string {
  return value
    .toLowerCase()
    .replace(/\.json$/, '')
    .replace(/[^a-z0-9_-]+/g, '_')
    .replace(/^_+|_+$/g, '');
}

function textMatches(candidate: unknown, requested: string): boolean {
  if (typeof candidate !== 'string') {
    return false;
  }

  const normalizedCandidate = normalizeMatchText(candidate);
  const normalizedRequested = normalizeMatchText(requested);

  return (
    normalizedCandidate === normalizedRequested ||
    normalizedCandidate.endsWith(`_${normalizedRequested}`) ||
    normalizedCandidate.includes(normalizedRequested)
  );
}

function toStringRecord(value: unknown, warnings: string[], label: string): Record<string, string> {
  if (value === undefined) {
    return {};
  }

  if (!isRecord(value)) {
    warnings.push(`Skipped ${label}: expected an object of string values.`);
    return {};
  }

  const result: Record<string, string> = {};
  for (const [key, item] of Object.entries(value)) {
    if (typeof item === 'string') {
      result[key] = item;
    } else {
      warnings.push(`Skipped ${label}.${key}: expected a string value.`);
    }
  }

  return result;
}

function findStringAtPaths(root: JsonRecord, paths: string[][]): string | undefined {
  for (const path of paths) {
    let current: unknown = root;
    for (const segment of path) {
      if (!isRecord(current)) {
        current = undefined;
        break;
      }
      current = current[segment];
    }

    const value = asString(current);
    if (value) {
      return value;
    }
  }

  return undefined;
}

function getRecommendedCommand(recipe: JsonRecord): JsonRecord | undefined {
  const recommendedCommand = recipe.recommended_command;
  return isRecord(recommendedCommand) ? recommendedCommand : undefined;
}

function selectCommandFromPayload(payload: JsonRecord, sourceLabel: string): CommandSource | undefined {
  if (
    asStringArray(payload.argv) ||
    asStringArray(payload.head_argv) ||
    asStringArray(payload.worker_argv)
  ) {
    return { command: payload, sourceLabel };
  }

  const recommendedCommand = getRecommendedCommand(payload);
  if (recommendedCommand) {
    return { command: recommendedCommand, sourceLabel };
  }

  const strategySpec = payload.strategy_spec;
  if (isRecord(strategySpec)) {
    return selectCommandFromPayload(strategySpec, `${sourceLabel}.strategy_spec`);
  }

  return undefined;
}

function collectAlternativeCandidates(value: unknown, key?: string): AlternativeCandidate[] {
  if (typeof value === 'string' && key) {
    return [{ record: { json: value, strategy: key }, key }];
  }

  if (Array.isArray(value)) {
    return value.flatMap((item) => collectAlternativeCandidates(item));
  }

  if (!isRecord(value)) {
    return [];
  }

  const candidates: AlternativeCandidate[] = [];
  const looksLikeCandidate =
    value.argv !== undefined ||
    value.json !== undefined ||
    value.url !== undefined ||
    value.path !== undefined ||
    value.strategy !== undefined ||
    value.name !== undefined ||
    value.id !== undefined;

  if (looksLikeCandidate) {
    candidates.push({ record: value, key });
  }

  for (const [childKey, childValue] of Object.entries(value)) {
    if (childKey === 'argv' || childKey === 'head_argv' || childKey === 'worker_argv') {
      continue;
    }
    candidates.push(...collectAlternativeCandidates(childValue, childKey));
  }

  return candidates;
}

function scoreAlternative(candidate: AlternativeCandidate, request: VllmRecipeResolveRequest): number {
  const { record, key } = candidate;
  let score = 0;

  const textFields: unknown[] = [
    key,
    record.strategy,
    record.name,
    record.id,
    record.title,
    record.url,
    record.json,
    record.path,
  ];

  if (request.strategy && textFields.some((field) => textMatches(field, request.strategy as string))) {
    score += 10;
  }

  if (request.hardware && textFields.concat(record.hardware, record.gpu, record.accelerator).some((field) => textMatches(field, request.hardware as string))) {
    score += 3;
  }

  if (request.variant && textFields.concat(record.variant).some((field) => textMatches(field, request.variant as string))) {
    score += 2;
  }

  return score;
}

async function maybeResolveAlternative(
  recipe: JsonRecord,
  request: VllmRecipeResolveRequest,
  client: VllmRecipesClient,
  warnings: string[]
): Promise<CommandSource | undefined> {
  if (!request.strategy && !request.hardware && !request.variant) {
    return undefined;
  }

  const recommendedCommand = getRecommendedCommand(recipe);
  const alternatives = recommendedCommand?.alternatives ?? recipe.alternatives;
  const candidates = collectAlternativeCandidates(alternatives);
  if (candidates.length === 0) {
    if (request.strategy) {
      warnings.push(`Requested recipe strategy "${request.strategy}" but no structured alternatives were listed.`);
    }
    return undefined;
  }

  const scored = candidates
    .map((candidate) => ({ candidate, score: scoreAlternative(candidate, request) }))
    .filter((item) => item.score > 0)
    .sort((a, b) => b.score - a.score);

  const best = scored[0]?.candidate;
  if (!best) {
    if (request.strategy) {
      warnings.push(`Requested recipe strategy "${request.strategy}" was not found in structured alternatives.`);
    }
    return undefined;
  }

  const inlineCommand = selectCommandFromPayload(best.record, 'alternative');
  if (inlineCommand) {
    return inlineCommand;
  }

  const reference = asString(best.record.json) ?? asString(best.record.url) ?? asString(best.record.path);
  if (!reference) {
    warnings.push('Matched recipe alternative did not contain a structured JSON reference or argv array.');
    return undefined;
  }

  try {
    const payload = await client.fetchReference(reference);
    const command = selectCommandFromPayload(payload, client.resolveReference(reference));
    if (!command) {
      warnings.push(`Fetched recipe alternative ${reference} but it did not contain a structured argv array.`);
      return undefined;
    }
    return command;
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    warnings.push(`Failed to fetch recipe alternative ${reference}: ${message}`);
    return undefined;
  }
}

function selectArgv(command: JsonRecord, warnings: string[]): string[] {
  const argv = asStringArray(command.argv);
  if (argv) {
    return argv;
  }

  const headArgv = asStringArray(command.head_argv);
  if (headArgv) {
    warnings.push('Recipe uses multi-node head/worker argv; resolved head_argv only.');
    return headArgv;
  }

  if (asStringArray(command.worker_argv)) {
    warnings.push('Recipe only exposed worker_argv; worker-only recipes cannot be fully resolved into a Direct vLLM deployment.');
  } else if (command.command || command.docker_command || command.docker_argv) {
    warnings.push('Recipe did not expose a structured argv array; command and docker_command strings were intentionally not parsed.');
  } else {
    warnings.push('Recipe did not expose a structured argv array.');
  }

  return [];
}

function stripVllmServePrefix(argv: string[]): string[] {
  // Drop the "vllm serve <model>" / "serve <model>" prefix, but only skip the
  // positional model token when it is actually positional. A recipe that writes
  // the model as a flag ("vllm serve --model <id> …") has a real flag in that
  // slot, so dropping it blindly would corrupt the command line.
  if (argv.length >= 2 && argv[0] === 'vllm' && argv[1] === 'serve') {
    const rest = argv.slice(2);
    return rest.length > 0 && !rest[0].startsWith('--') ? rest.slice(1) : rest;
  }

  if (argv.length >= 1 && argv[0] === 'serve') {
    const rest = argv.slice(1);
    return rest.length > 0 && !rest[0].startsWith('--') ? rest.slice(1) : rest;
  }

  return argv;
}

function isValueToken(token: string | undefined): token is string {
  if (token === undefined) {
    return false;
  }

  return !token.startsWith('--') || /^-\d/.test(token);
}

function getFlagKey(token: string): string | undefined {
  if (!token.startsWith('--') || token.length <= 2) {
    return undefined;
  }

  const body = token.slice(2);
  const equalIndex = body.indexOf('=');
  const key = equalIndex >= 0 ? body.slice(0, equalIndex) : body;

  return ENGINE_ARG_KEY_PATTERN.test(key) ? key : undefined;
}

function parseArgTokens(tokens: string[], warnings: string[]): ArgParseResult {
  const engineArgs: Record<string, string> = {};
  const engineExtraArgs: string[] = [];
  const duplicateWarnings = new Set<string>();

  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (!token.startsWith('--')) {
      engineExtraArgs.push(token);
      continue;
    }

    const body = token.slice(2);
    const equalIndex = body.indexOf('=');
    const key = equalIndex >= 0 ? body.slice(0, equalIndex) : body;

    if (!ENGINE_ARG_KEY_PATTERN.test(key)) {
      engineExtraArgs.push(token);
      continue;
    }

    let value = '';
    const originalTokens = [token];

    if (equalIndex >= 0) {
      value = body.slice(equalIndex + 1);
    } else if (isValueToken(tokens[index + 1])) {
      value = tokens[index + 1];
      originalTokens.push(tokens[index + 1]);
      index += 1;
    }

    if (Object.prototype.hasOwnProperty.call(engineArgs, key)) {
      engineExtraArgs.push(...originalTokens);
      if (!duplicateWarnings.has(key)) {
        warnings.push(`Preserved duplicate vLLM argument "${key}" in engineExtraArgs.`);
        duplicateWarnings.add(key);
      }
      continue;
    }

    engineArgs[key] = value;
  }

  return { engineArgs, engineExtraArgs };
}

function extractFlagKeys(tokens: string[]): Set<string> {
  const keys = new Set<string>();
  for (const token of tokens) {
    const key = getFlagKey(token);
    if (key) {
      keys.add(key);
    }
  }
  return keys;
}

function removeArgsByKeys(tokens: string[], keysToRemove: Set<string>): string[] {
  const result: string[] = [];

  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    const key = getFlagKey(token);
    if (!key || !keysToRemove.has(key)) {
      result.push(token);
      continue;
    }

    if (!token.includes('=') && isValueToken(tokens[index + 1])) {
      index += 1;
    }
  }

  return result;
}

function getRecipeFeatures(recipe: JsonRecord): JsonRecord {
  return isRecord(recipe.features) ? recipe.features : {};
}

function getHardwareOverrides(recipe: JsonRecord): JsonRecord {
  return isRecord(recipe.hardware_overrides) ? recipe.hardware_overrides : {};
}

function getHardwareOverride(recipe: JsonRecord, hardware: string | undefined): JsonRecord | undefined {
  if (!hardware) {
    return undefined;
  }

  const overrides = getHardwareOverrides(recipe);
  const direct = overrides[hardware];
  if (isRecord(direct)) {
    return direct;
  }

  const normalizedHardware = normalizeMatchText(hardware);
  for (const [key, value] of Object.entries(overrides)) {
    if (normalizeMatchText(key) === normalizedHardware && isRecord(value)) {
      return value;
    }
  }

  return undefined;
}

function collectFeatureArgSequences(recipe: JsonRecord, hardware: string | undefined, warnings: string[]): string[][] {
  const sequences: string[][] = [];
  const features = getRecipeFeatures(recipe);

  for (const [featureName, featureValue] of Object.entries(features)) {
    if (!isRecord(featureValue)) {
      continue;
    }

    const args = asStringArray(featureValue.args);
    if (args) {
      sequences.push(args);
    } else if (featureValue.args !== undefined) {
      warnings.push(`Skipped feature ${featureName} args: expected a string array.`);
    }

    const hardwareOverride = findFeatureHardwareOverride(featureValue, hardware);
    if (hardwareOverride?.args !== undefined) {
      const overrideArgs = asStringArray(hardwareOverride.args);
      if (overrideArgs) {
        sequences.push(overrideArgs);
      } else {
        warnings.push(`Skipped feature ${featureName} hardware override args: expected a string array.`);
      }
    }
  }

  return sequences;
}

function findFeatureHardwareOverride(feature: JsonRecord, hardware: string | undefined): JsonRecord | undefined {
  if (!hardware || !isRecord(feature.hardware_overrides)) {
    return undefined;
  }

  const direct = feature.hardware_overrides[hardware];
  if (isRecord(direct)) {
    return direct;
  }

  const normalizedHardware = normalizeMatchText(hardware);
  for (const [key, value] of Object.entries(feature.hardware_overrides)) {
    if (normalizeMatchText(key) === normalizedHardware && isRecord(value)) {
      return value;
    }
  }

  return undefined;
}

function resolveFeatureArgs(
  recipe: JsonRecord,
  featureName: string,
  hardware: string | undefined,
  warnings: string[]
): string[] {
  const features = getRecipeFeatures(recipe);
  const feature = features[featureName];

  if (!isRecord(feature)) {
    warnings.push(`Requested vLLM recipe feature "${featureName}" is not available for this recipe.`);
    return [];
  }

  const args = asStringArray(feature.args);
  const result = args ? [...args] : [];
  if (feature.args !== undefined && !args) {
    warnings.push(`Skipped feature ${featureName} args: expected a string array.`);
  }

  const hardwareOverride = findFeatureHardwareOverride(feature, hardware);
  if (hardwareOverride?.args !== undefined) {
    const overrideArgs = asStringArray(hardwareOverride.args);
    if (overrideArgs) {
      result.push(...overrideArgs);
    } else {
      warnings.push(`Skipped feature ${featureName} hardware override args: expected a string array.`);
    }
  }

  return result;
}

function isDeepSeekRecipe(recipe: JsonRecord, request: VllmRecipeResolveRequest): boolean {
  const modelIds = [request.modelId, asString(recipe.hf_id), findStringAtPaths(recipe, [['model', 'hf_id'], ['model', 'id']])]
    .filter((value): value is string => Boolean(value));

  return modelIds.some((modelId) => modelId.toLowerCase().includes('deepseek'));
}

function appendFlag(tokens: string[], key: string, value?: string): string[] {
  const withoutExisting = removeArgsByKeys(tokens, new Set([key]));
  withoutExisting.push(`--${key}`);
  if (value !== undefined) {
    withoutExisting.push(value);
  }
  return withoutExisting;
}

function applyDeepSeekFeatureRules(
  tokens: string[],
  selectedFeatures: Set<string>,
  recipe: JsonRecord,
  request: VllmRecipeResolveRequest
): string[] {
  if (!isDeepSeekRecipe(recipe, request)) {
    return tokens;
  }

  let result = [...tokens];
  if (selectedFeatures.has('tool_calling')) {
    result = appendFlag(result, 'tokenizer-mode', 'deepseek_v4');
    result = appendFlag(result, 'tool-call-parser', 'deepseek_v4');
    result = appendFlag(result, 'enable-auto-tool-choice');
  }

  if (selectedFeatures.has('reasoning')) {
    result = appendFlag(result, 'reasoning-parser', 'deepseek_v4');
  }

  return result;
}

function knownDeepSeekFeatureSequences(recipe: JsonRecord, request: VllmRecipeResolveRequest): string[][] {
  if (!isDeepSeekRecipe(recipe, request)) {
    return [];
  }

  return [
    ['--tokenizer-mode', 'deepseek_v4'],
    ['--tool-call-parser', 'deepseek_v4'],
    ['--enable-auto-tool-choice'],
    ['--reasoning-parser', 'deepseek_v4'],
  ];
}

function applyExplicitFeatureSelection(
  tokens: string[],
  recipe: JsonRecord,
  request: VllmRecipeResolveRequest,
  hardware: string | undefined,
  warnings: string[]
): string[] {
  if (request.features === undefined) {
    return tokens;
  }

  const selectedFeatures = new Set(request.features);
  const knownSequences = [
    ...collectFeatureArgSequences(recipe, hardware, warnings),
    ...knownDeepSeekFeatureSequences(recipe, request),
  ];
  const keysToRemove = new Set<string>();
  for (const sequence of knownSequences) {
    for (const key of extractFlagKeys(sequence)) {
      keysToRemove.add(key);
    }
  }

  const result = removeArgsByKeys(tokens, keysToRemove);
  let selectedFeatureTokens: string[] = [];

  const recipeFeatures = getRecipeFeatures(recipe);
  const deepSeekRecipe = isDeepSeekRecipe(recipe, request);
  for (const feature of selectedFeatures) {
    if (
      !isRecord(recipeFeatures[feature]) &&
      deepSeekRecipe &&
      (feature === 'tool_calling' || feature === 'reasoning')
    ) {
      continue;
    }

    selectedFeatureTokens.push(...resolveFeatureArgs(recipe, feature, hardware, warnings));
  }

  selectedFeatureTokens = applyDeepSeekFeatureRules(selectedFeatureTokens, selectedFeatures, recipe, request);
  result.push(...selectedFeatureTokens);

  return result;
}

function applyHardwareOverrides(
  tokens: string[],
  env: Record<string, string>,
  recipe: JsonRecord,
  hardware: string | undefined,
  warnings: string[]
): { tokens: string[]; env: Record<string, string> } {
  const override = getHardwareOverride(recipe, hardware);
  if (!override) {
    return { tokens, env };
  }

  const nextTokens = [...tokens];
  if (override.extra_args !== undefined) {
    const extraArgs = asStringArray(override.extra_args);
    if (extraArgs) {
      nextTokens.push(...extraArgs);
    } else {
      warnings.push(`Skipped hardware override extra_args for ${hardware}: expected a string array.`);
    }
  }

  const nextEnv = { ...env };
  Object.assign(nextEnv, toStringRecord(override.extra_env, warnings, `hardware_overrides.${hardware}.extra_env`));

  return { tokens: nextTokens, env: nextEnv };
}

function deriveGpuCount(command: JsonRecord, engineArgs: Record<string, string>): number {
  // GPUs-per-pod is the product of the parallelism dimensions that shard a SINGLE
  // model instance across GPUs within one pod: tensor-parallel × pipeline-parallel.
  // data-parallel-size and decode-context-parallel-size scale the number of
  // replicas/instances, not GPUs-per-pod, so including them over-counts (e.g.
  // TP=4, DP=4 would otherwise request 16 GPUs in one unschedulable pod).
  const perPodParallelismKeys = ['tensor-parallel-size', 'pipeline-parallel-size'];

  let product = 1;
  let found = false;
  for (const key of perPodParallelismKeys) {
    const value = asNumber(engineArgs[key]);
    if (value && value > 0) {
      product *= Math.trunc(value);
      found = true;
    }
  }

  if (found) {
    return Math.max(1, product);
  }

  const hardwareProfile = isRecord(command.hardware_profile) ? command.hardware_profile : undefined;
  const profileGpuCount =
    asNumber(command.gpu_count) ??
    asNumber(command.gpus) ??
    asNumber(command.gpuCount) ??
    asNumber(hardwareProfile?.gpu_count) ??
    asNumber(hardwareProfile?.gpus) ??
    asNumber(hardwareProfile?.gpuCount);

  if (profileGpuCount && profileGpuCount > 0) {
    return Math.max(1, Math.trunc(profileGpuCount));
  }

  return 1;
}

function deriveMode(command: JsonRecord, requestedMode: DeploymentMode | undefined): DeploymentMode {
  if (requestedMode) {
    return requestedMode;
  }

  const deployType = asString(command.deploy_type)?.toLowerCase() ?? '';
  if (deployType.includes('disaggregated')) {
    return 'disaggregated';
  }

  return 'aggregated';
}

function addDeploymentShapeWarnings(command: JsonRecord, warnings: string[]): void {
  const deployType = asString(command.deploy_type)?.toLowerCase();
  const nodeCount = asNumber(command.node_count);

  if (nodeCount && nodeCount > 1) {
    warnings.push(`Recipe targets ${nodeCount} nodes; Direct vLLM deployment fields were resolved as a best effort.`);
  }

  if (deployType && (deployType.includes('pd') || deployType.includes('cluster') || deployType.includes('multi'))) {
    warnings.push(`Recipe deploy_type "${deployType}" may require topology-specific orchestration beyond Direct vLLM.`);
  }

  if (command.worker_argv !== undefined && command.argv === undefined) {
    warnings.push('Recipe includes worker_argv; worker arguments were not merged into engineArgs.');
  }
}

function resolveImageRef(command: JsonRecord, request: VllmRecipeResolveRequest, warnings: string[]): string | undefined {
  const choice = request.imageChoice ?? { type: 'recipe' as const };

  if (choice.type === 'none') {
    return undefined;
  }

  if (choice.type === 'custom') {
    if (!choice.imageRef) {
      warnings.push('Custom imageChoice did not include imageRef; falling back to the recipe image.');
    } else {
      return choice.imageRef;
    }
  }

  return asString(command.docker_image);
}

function buildAnnotations(provenance: RecipeProvenance): Record<string, string> {
  const annotations: Record<string, string> = {
    [GENERATED_BY_ANNOTATION]: 'vllm-recipe-resolver',
  };

  if (provenance.source) annotations[RECIPE_SOURCE_ANNOTATION] = provenance.source;
  if (provenance.id) annotations[RECIPE_ID_ANNOTATION] = provenance.id;
  if (provenance.strategy) annotations[RECIPE_STRATEGY_ANNOTATION] = provenance.strategy;
  if (provenance.hardware) annotations[RECIPE_HARDWARE_ANNOTATION] = provenance.hardware;
  if (provenance.variant) annotations[RECIPE_VARIANT_ANNOTATION] = provenance.variant;
  if (provenance.precision) annotations[RECIPE_PRECISION_ANNOTATION] = provenance.precision;
  if (provenance.revision) annotations[RECIPE_REVISION_ANNOTATION] = provenance.revision;
  if (Array.isArray(provenance.features) && provenance.features.length > 0) {
    annotations[RECIPE_FEATURES_ANNOTATION] = JSON.stringify(provenance.features);
  }

  return annotations;
}

function effectiveHardware(recipe: JsonRecord, command: JsonRecord, request: VllmRecipeResolveRequest): string | undefined {
  return (
    request.hardware ??
    asString(command.hardware) ??
    findStringAtPaths(command, [['hardware_profile', 'name'], ['hardware_profile', 'id'], ['hardware_profile', 'gpu']]) ??
    findStringAtPaths(recipe, [['recommended_command', 'hardware']])
  );
}

function effectiveStrategy(command: JsonRecord, request: VllmRecipeResolveRequest): string | undefined {
  return request.strategy ?? asString(command.strategy) ?? asString(command.name) ?? asString(command.id);
}

function effectiveVariant(command: JsonRecord, request: VllmRecipeResolveRequest): string | undefined {
  return request.variant ?? asString(command.variant);
}

function effectivePrecision(recipe: JsonRecord, command: JsonRecord): string | undefined {
  return (
    findStringAtPaths(command, [['hardware_profile', 'precision'], ['strategy_spec', 'precision']]) ??
    findStringAtPaths(recipe, [['meta', 'precision'], ['model', 'precision'], ['precision']])
  );
}

function effectiveRevision(recipe: JsonRecord): string | undefined {
  return findStringAtPaths(recipe, [['meta', 'revision'], ['meta', 'sha'], ['revision'], ['git_revision']]);
}

export class VllmRecipeResolver {
  constructor(private readonly client: VllmRecipesClient = vllmRecipesClient) {}

  async resolve(request: VllmRecipeResolveRequest): Promise<VllmRecipeResolveResult> {
    const warnings: string[] = [];
    const raw = await this.client.getByModelId(request.modelId);
    const recipe = raw.recipe as VllmRecipeRawPayload;

    const recommendedCommand = getRecommendedCommand(recipe);
    if (!recommendedCommand) {
      warnings.push('Recipe did not include recommended_command; attempting to resolve from the top-level recipe object.');
    }

    const alternative = await maybeResolveAlternative(recipe, request, this.client, warnings);
    const selectedCommand = alternative ?? selectCommandFromPayload(recommendedCommand ?? recipe, raw.source);

    if (!selectedCommand) {
      warnings.push('No structured command source was available for this recipe.');
    }

    const command = selectedCommand?.command ?? {};
    addDeploymentShapeWarnings(command, warnings);

    const hardware = effectiveHardware(recipe, command, request);
    const argv = selectArgv(command, warnings);
    let tokens = stripVllmServePrefix(argv);
    tokens = applyExplicitFeatureSelection(tokens, recipe, request, hardware, warnings);

    let env = toStringRecord(command.env, warnings, 'recommended_command.env');
    const hardwareApplied = applyHardwareOverrides(tokens, env, recipe, hardware, warnings);
    tokens = hardwareApplied.tokens;
    env = hardwareApplied.env;

    const { engineArgs, engineExtraArgs } = parseArgTokens(tokens, warnings);
    const resources = {
      gpu: deriveGpuCount(command, engineArgs),
    };
    const mode = deriveMode(command, request.mode);
    const imageRef = resolveImageRef(command, request, warnings);

    if (!imageRef && (request.imageChoice === undefined || request.imageChoice.type === 'recipe')) {
      warnings.push('Recipe did not include docker_image; no imageRef was resolved.');
    }

    const provenance: RecipeProvenance = {
      source: selectedCommand?.sourceLabel ?? raw.source,
      id: asString(recipe.hf_id) ?? request.modelId,
      strategy: effectiveStrategy(command, request),
      hardware,
      variant: effectiveVariant(command, request),
      precision: effectivePrecision(recipe, command),
      features: request.features,
      revision: effectiveRevision(recipe),
    };

    return {
      provider: 'vllm',
      engine: 'vllm',
      mode,
      imageRef,
      resources,
      engineArgs,
      engineExtraArgs,
      env,
      annotations: buildAnnotations(provenance),
      recipeProvenance: provenance,
      warnings,
    };
  }
}

export const vllmRecipeResolver = new VllmRecipeResolver();
