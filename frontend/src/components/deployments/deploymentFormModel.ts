import type { DeploymentConfig } from '@/hooks/useDeployments'
import type { AIConfiguratorResult, Engine, KaitoResourceType, Model } from '@/lib/api'
import { calculateMultiNode, type MultiNodeRecommendation } from '@/lib/gpu-recommendations'
import { generateDeploymentName } from '@/lib/utils'

// Subset of Engine type for traditional GPU inference engines (excludes llamacpp which is KAITO-only)
export type TraditionalEngine = 'vllm' | 'sglang' | 'trtllm'
export type RouterMode = 'default' | 'kv' | 'round-robin'
export type DeploymentMode = 'aggregated' | 'disaggregated'
export type RuntimeId = 'dynamo' | 'kuberay' | 'kaito' | 'llmd' | 'vllm'
export type KaitoComputeType = 'cpu' | 'gpu'
export type GgufRunMode = 'build' | 'direct'

export const TENSOR_PARALLEL_SIZE_ARG = 'tensor-parallel-size'
export const PIPELINE_PARALLEL_SIZE_ARG = 'pipeline-parallel-size'

// FP8 precision engine flags (vLLM / SGLang). Only emitted when FP8 is selected
// on FP8-capable hardware; FP16/BF16 is the engine default and needs no flag.
export const QUANTIZATION_ARG = 'quantization'
export const KV_CACHE_DTYPE_ARG = 'kv-cache-dtype'

// Runtime metadata for display
export const RUNTIME_INFO: Record<RuntimeId, { name: string; description: string; defaultNamespace: string }> = {
  dynamo: {
    name: 'NVIDIA Dynamo',
    description: 'High-performance inference with KV-cache routing and disaggregated serving',
    defaultNamespace: 'dynamo-system',
  },
  kuberay: {
    name: 'KubeRay',
    description: 'Ray-based serving with autoscaling and distributed inference',
    defaultNamespace: 'kuberay-system',
  },
  kaito: {
    name: 'KAITO',
    description: 'Flexible inference with GGUF (llama.cpp) and vLLM support',
    defaultNamespace: 'kaito-workspace',
  },
  llmd: {
    name: 'llm-d',
    description: 'GPU-accelerated vLLM inference with disaggregated prefill/decode support',
    defaultNamespace: 'default',
  },
  vllm: {
    name: 'vLLM',
    description: 'High-throughput inference with the native vLLM provider',
    defaultNamespace: 'default',
  },
}

// Engine support by runtime (only traditional GPU engines, not llamacpp)
export const RUNTIME_ENGINES: Record<RuntimeId, TraditionalEngine[]> = {
  dynamo: ['vllm', 'sglang', 'trtllm'],
  kuberay: ['vllm'], // KubeRay only supports vLLM currently
  kaito: ['vllm'], // KAITO exposes vLLM in the engine picker; single-engine llama.cpp models bypass it
  llmd: ['vllm'], // llm-d uses vLLM exclusively
  vllm: ['vllm'], // Native vLLM provider uses vLLM exclusively
}

// Engines that accept the generic --quantization / --kv-cache-dtype flags.
// TRT-LLM uses a different mechanism and KAITO ignores generic engine args.
export const FP8_ARG_ENGINES: TraditionalEngine[] = ['vllm', 'sglang']

export function normalizeGatewayAvailability(
  config: DeploymentConfig,
  gatewayAvailable: boolean | undefined
): DeploymentConfig {
  if (gatewayAvailable !== false || !('gatewayEnabled' in config)) {
    return config
  }

  const nextConfig = { ...config }
  delete nextConfig.gatewayEnabled
  return nextConfig
}

// Check if a runtime is compatible with a model based on engine support
export function isRuntimeCompatible(runtimeId: RuntimeId, modelEngines: Engine[]): boolean {
  // KAITO supports llamacpp (GGUF) AND vllm models
  if (runtimeId === 'kaito') {
    return modelEngines.includes('llamacpp') || modelEngines.includes('vllm')
  }
  // Other models need at least one matching engine with the runtime
  const runtimeEngines = RUNTIME_ENGINES[runtimeId]
  return modelEngines.some(e => runtimeEngines.includes(e as TraditionalEngine))
}

// Extract nodeCount from providerOverrides structure

const RUNTIME_PREFERENCE: RuntimeId[] = ['dynamo', 'kuberay', 'kaito', 'llmd', 'vllm']

export function getDefaultRuntimeForModel(
  modelEngines: Engine[],
  runtimes?: Array<{ id: string; installed?: boolean }>
): RuntimeId {
  if (!runtimes || runtimes.length === 0) {
    return modelEngines.includes('llamacpp') ? 'kaito' : 'dynamo'
  }

  // Find first compatible and installed runtime
  for (const rtId of RUNTIME_PREFERENCE) {
    const rt = runtimes.find(r => r.id === rtId)
    if (rt?.installed && isRuntimeCompatible(rtId, modelEngines)) {
      return rtId
    }
  }

  // If no compatible installed runtime, return the first compatible runtime that is available to select
  for (const rtId of RUNTIME_PREFERENCE) {
    const rt = runtimes.find(r => r.id === rtId)
    if (rt && isRuntimeCompatible(rtId, modelEngines)) {
      return rtId
    }
  }

  return 'dynamo'
}

export function getAvailableEnginesForRuntime(runtime: RuntimeId, modelEngines: Engine[]): TraditionalEngine[] {
  const runtimeEngines = RUNTIME_ENGINES[runtime]
  // Filter model engines to only those supported by the runtime (excluding llamacpp)
  return modelEngines.filter(
    (e): e is TraditionalEngine => runtimeEngines.includes(e as TraditionalEngine)
  )
}

export function getDefaultEngineForRuntime(runtime: RuntimeId, modelEngines: Engine[]): Engine {
  if (modelEngines.length === 1) {
    return modelEngines[0]
  }

  return getAvailableEnginesForRuntime(runtime, modelEngines)[0] || modelEngines[0] || 'vllm'
}

export interface DeploymentModelFacts {
  isHuggingFaceGgufModel: boolean
  isVllmModel: boolean
}

export function getDeploymentModelFacts(model: Pick<Model, 'id' | 'supportedEngines'>): DeploymentModelFacts {
  return {
    // GGUF models have only llama.cpp as supported engine and come from Hugging Face.
    isHuggingFaceGgufModel: model.supportedEngines.length === 1 &&
      model.supportedEngines[0] === 'llamacpp' &&
      !model.id.startsWith('kaito/'),
    // KAITO vLLM models expose vLLM without also being llama.cpp/GGUF models.
    isVllmModel: model.supportedEngines.includes('vllm') &&
      !model.supportedEngines.includes('llamacpp'),
  }
}

export interface InitialDeploymentConfigOptions {
  model: Pick<Model, 'id' | 'supportedEngines'>
  runtime: RuntimeId
  hfTokenSecret?: string
  name?: string
}

export function createInitialDeploymentConfig({
  model,
  runtime,
  hfTokenSecret = import.meta.env.VITE_DEFAULT_HF_SECRET || '',
  name = generateDeploymentName(model.id),
}: InitialDeploymentConfigOptions): DeploymentConfig {
  return {
    name,
    namespace: RUNTIME_INFO[runtime].defaultNamespace,
    modelId: model.id,
    engine: getDefaultEngineForRuntime(runtime, model.supportedEngines),
    mode: 'aggregated',
    provider: runtime,
    routerMode: 'default',
    replicas: 1,
    hfTokenSecret,
    enforceEager: true,
    enablePrefixCaching: true,
    trustRemoteCode: false,
    prefillReplicas: 1,
    decodeReplicas: 1,
    prefillGpus: 1,
    decodeGpus: 1,
    resources: {
      gpu: 0,
    },
  }
}

export function applyRuntimeChangeToConfig(
  prev: DeploymentConfig,
  options: {
    runtime: RuntimeId
    modelEngines: Engine[]
    recommendedGpus: number
    estimatedMemoryGb?: number
    gpuMemoryGb?: number
  }
): DeploymentConfig {
  const newAvailableEngines = getAvailableEnginesForRuntime(options.runtime, options.modelEngines)
  const currentEngineSupported = newAvailableEngines.includes(prev.engine as TraditionalEngine)
  const nextEngine = currentEngineSupported
    ? prev.engine
    : getDefaultEngineForRuntime(options.runtime, options.modelEngines)
  const shouldManageDynamoParallelism =
    options.runtime === 'dynamo' &&
    prev.mode === 'aggregated' &&
    nextEngine === 'vllm'

  let newEngineArgs = setDynamoParallelismEngineArgs(prev.engineArgs, null)
  let newProviderOverrides = shouldManageDynamoParallelism ? prev.providerOverrides : undefined

  // When switching TO Dynamo + vLLM, recalculate multi-node from current GPU config.
  if (shouldManageDynamoParallelism) {
    const currentGpu = prev.resources?.gpu || options.recommendedGpus || 1

    if (options.estimatedMemoryGb && options.gpuMemoryGb) {
      const multiNodeResult = calculateMultiNode(options.estimatedMemoryGb, options.gpuMemoryGb, currentGpu)
      if (multiNodeResult) {
        newProviderOverrides = buildDynamoMultiNodeOverrides(multiNodeResult.nodeCount)
        newEngineArgs = setDynamoParallelismEngineArgs(newEngineArgs, multiNodeResult)
      } else {
        newProviderOverrides = undefined
      }
    }
  }

  return {
    ...prev,
    provider: options.runtime,
    namespace: RUNTIME_INFO[options.runtime].defaultNamespace,
    // Reset engine if current one isn't supported by new runtime
    engine: nextEngine,
    // Reset router mode if switching away from Dynamo
    routerMode: options.runtime === 'dynamo' ? prev.routerMode : 'default',
    // Reset to aggregated mode if switching to KAITO (disaggregated not supported)
    mode: options.runtime === 'kaito' ? 'aggregated' : prev.mode,
    providerOverrides: newProviderOverrides,
    engineArgs: newEngineArgs,
  }
}


export function applyDeploymentModeChangeToConfig(
  prev: DeploymentConfig,
  mode: DeploymentMode
): DeploymentConfig {
  if (mode === 'disaggregated') {
    return {
      ...prev,
      mode,
      providerOverrides: undefined,
      engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, null),
    }
  }

  return {
    ...prev,
    mode,
  }
}

export function getNodeCountFromOverrides(overrides?: Record<string, unknown>): number {
  if (!overrides) return 1
  const spec = overrides.spec as Record<string, unknown> | undefined
  const services = spec?.services as Record<string, unknown> | undefined
  const vllmWorker = services?.VllmWorker as Record<string, unknown> | undefined
  const multinode = vllmWorker?.multinode as Record<string, unknown> | undefined
  const nodeCount = multinode?.nodeCount as number | undefined
  return nodeCount && nodeCount > 1 ? nodeCount : 1
}

export function getNumericEngineArg(engineArgs: Record<string, unknown> | undefined, key: string): number | undefined {
  const value = engineArgs?.[key]
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value === 'string') {
    const parsed = Number(value)
    if (Number.isFinite(parsed)) return parsed
  }
  return undefined
}

export function buildDynamoMultiNodeOverrides(nodeCount: number): Record<string, unknown> {
  return {
    spec: {
      services: {
        VllmWorker: {
          multinode: {
            nodeCount,
          },
        },
      },
    },
  }
}

export function setDynamoParallelismEngineArgs(
  engineArgs: Record<string, unknown> | undefined,
  multiNode: MultiNodeRecommendation | null
): Record<string, unknown> | undefined {
  const nextEngineArgs = { ...(engineArgs || {}) }

  if (multiNode) {
    nextEngineArgs[TENSOR_PARALLEL_SIZE_ARG] = String(multiNode.gpusPerNode)
    nextEngineArgs[PIPELINE_PARALLEL_SIZE_ARG] = String(multiNode.pipelineParallelSize)
  } else {
    delete nextEngineArgs[TENSOR_PARALLEL_SIZE_ARG]
    delete nextEngineArgs[PIPELINE_PARALLEL_SIZE_ARG]
  }

  return Object.keys(nextEngineArgs).length > 0 ? nextEngineArgs : undefined
}


export function applyGpuPerReplicaChangeToConfig(
  prev: DeploymentConfig,
  options: {
    selectedRuntime: RuntimeId
    gpuCount: number
    estimatedMemoryGb?: number
    gpuMemoryGb?: number
  }
): DeploymentConfig {
  if (
    options.selectedRuntime === 'dynamo' &&
    prev.engine === 'vllm' &&
    options.estimatedMemoryGb &&
    options.gpuMemoryGb
  ) {
    const multiNodeResult = calculateMultiNode(
      options.estimatedMemoryGb,
      options.gpuMemoryGb,
      options.gpuCount
    )

    if (multiNodeResult) {
      return {
        ...prev,
        resources: { ...prev.resources, gpu: options.gpuCount },
        providerOverrides: buildDynamoMultiNodeOverrides(multiNodeResult.nodeCount),
        engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, multiNodeResult),
      }
    }

    return {
      ...prev,
      resources: { ...prev.resources, gpu: options.gpuCount },
      providerOverrides: undefined,
      engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, null),
    }
  }

  return {
    ...prev,
    resources: { ...prev.resources, gpu: options.gpuCount },
  }
}

export function selectPreferredGgufFile(files: string[], currentSelection = ''): string {
  if (currentSelection || files.length === 0) {
    return currentSelection
  }

  return files.find(file => /q4_k_m/i.test(file)) || files[0]
}

export function calculateSelectedGpus(
  config: DeploymentConfig,
  recommendedGpus: number,
  nodeCount: number
): number {
  if (config.mode === 'disaggregated') {
    const prefillTotal = (config.prefillReplicas || 1) * (config.prefillGpus || 1)
    const decodeTotal = (config.decodeReplicas || 1) * (config.decodeGpus || 1)
    return prefillTotal + decodeTotal
  }

  const gpusPerReplica = config.resources?.gpu || recommendedGpus || 1
  const replicas = config.replicas || 1
  return gpusPerReplica * replicas * nodeCount
}

export function getCurrentMultiNode(
  config: DeploymentConfig,
  recommendedGpus: number,
  nodeCount: number,
  pipelineParallel?: number
): MultiNodeRecommendation | null {
  if (nodeCount <= 1) return null

  const gpusPerNode = config.resources?.gpu || recommendedGpus || 1
  return {
    nodeCount,
    gpusPerNode,
    totalGpus: nodeCount * gpusPerNode,
    pipelineParallelSize: pipelineParallel || nodeCount,
  }
}

export function getMaxGpusPerPod(config: DeploymentConfig, recommendedGpus: number): number {
  return config.mode === 'disaggregated'
    ? Math.max(config.prefillGpus || 1, config.decodeGpus || 1)
    : (config.resources?.gpu || recommendedGpus || 1)
}

export interface DeploymentResourceSummary {
  selectedGpus: number
  currentMultiNode: MultiNodeRecommendation | null
  maxGpusPerPod: number
}

export function getDeploymentResourceSummary(options: {
  config: DeploymentConfig
  recommendedGpus: number
  currentNodeCount: number
  currentPipelineParallel?: number
}): DeploymentResourceSummary {
  return {
    selectedGpus: calculateSelectedGpus(options.config, options.recommendedGpus, options.currentNodeCount),
    currentMultiNode: getCurrentMultiNode(
      options.config,
      options.recommendedGpus,
      options.currentNodeCount,
      options.currentPipelineParallel
    ),
    maxGpusPerPod: getMaxGpusPerPod(options.config, options.recommendedGpus),
  }
}

export type DeploymentSubmitStatus = 'idle' | 'validating' | 'submitting' | 'success' | string

export type DeploymentSubmitButtonKind =
  | 'hf-auth-required'
  | 'fp8-blocked'
  | 'runtime-not-ready'
  | 'runtime-not-installed'
  | 'select-kaito-model'
  | 'select-gguf-file'
  | 'configure-kaito-gpus'
  | 'validating'
  | 'submitting'
  | 'success'
  | 'ready'

export interface DeploymentSubmitButtonState {
  disabled: boolean
  label: string
  kind: DeploymentSubmitButtonKind
  kaitoConfigValid: boolean
}

export function getDeploymentSubmitButtonState(options: {
  isProcessing: boolean
  submitStatus: DeploymentSubmitStatus
  needsHfAuth: boolean
  fp8Blocked: boolean
  isRuntimeInstalled: boolean
  isSelectedCrdLessRuntimeNotReady: boolean
  selectedRuntime: RuntimeId
  isHuggingFaceGgufModel: boolean
  isVllmModel: boolean
  ggufFile: string
  gpuCount: number
  hasSelectedPremadeModel: boolean
}): DeploymentSubmitButtonState {
  const kaitoConfigValid = isKaitoConfigValid({
    selectedRuntime: options.selectedRuntime,
    isHuggingFaceGgufModel: options.isHuggingFaceGgufModel,
    isVllmModel: options.isVllmModel,
    ggufFile: options.ggufFile,
    gpuCount: options.gpuCount,
    hasSelectedPremadeModel: options.hasSelectedPremadeModel,
  })

  if (options.needsHfAuth) {
    return { disabled: true, label: 'HuggingFace Auth Required', kind: 'hf-auth-required', kaitoConfigValid }
  }

  if (options.fp8Blocked) {
    return { disabled: true, label: 'FP8 Not Supported on This GPU', kind: 'fp8-blocked', kaitoConfigValid }
  }

  if (!options.isRuntimeInstalled) {
    return {
      disabled: true,
      label: options.isSelectedCrdLessRuntimeNotReady ? 'Runtime Not Ready' : 'Runtime Not Installed',
      kind: options.isSelectedCrdLessRuntimeNotReady ? 'runtime-not-ready' : 'runtime-not-installed',
      kaitoConfigValid,
    }
  }

  if (options.selectedRuntime === 'kaito' && !options.isHuggingFaceGgufModel && !options.isVllmModel && !options.hasSelectedPremadeModel) {
    return { disabled: true, label: 'Select a Model', kind: 'select-kaito-model', kaitoConfigValid }
  }

  if (options.selectedRuntime === 'kaito' && options.isHuggingFaceGgufModel && !options.ggufFile.endsWith('.gguf')) {
    return { disabled: true, label: 'Select GGUF File', kind: 'select-gguf-file', kaitoConfigValid }
  }

  if (options.selectedRuntime === 'kaito' && options.isVllmModel && options.gpuCount < 1) {
    return { disabled: true, label: 'Configure GPUs', kind: 'configure-kaito-gpus', kaitoConfigValid }
  }

  switch (options.submitStatus) {
    case 'validating':
      return { disabled: options.isProcessing, label: 'Validating...', kind: 'validating', kaitoConfigValid }
    case 'submitting':
      return { disabled: options.isProcessing, label: 'Deploying...', kind: 'submitting', kaitoConfigValid }
    case 'success':
      return { disabled: options.isProcessing, label: 'Deployed!', kind: 'success', kaitoConfigValid }
    default:
      return { disabled: options.isProcessing, label: 'Deploy Model', kind: 'ready', kaitoConfigValid }
  }
}

export function isKaitoConfigValid(options: {
  selectedRuntime: RuntimeId
  isHuggingFaceGgufModel: boolean
  isVllmModel: boolean
  ggufFile: string
  gpuCount: number
  hasSelectedPremadeModel: boolean
}): boolean {
  if (options.selectedRuntime !== 'kaito') return true

  if (options.isHuggingFaceGgufModel) {
    return options.ggufFile.endsWith('.gguf')
  }

  if (options.isVllmModel) {
    return options.gpuCount >= 1
  }

  return options.hasSelectedPremadeModel
}

export interface DeploymentFormConfigBuildOptions {
  selectedRuntime: RuntimeId
  gatewayAvailable?: boolean
  kaitoResourceType: KaitoResourceType
  isHuggingFaceGgufModel: boolean
  isVllmModel: boolean
  modelId: string
  ggufFile: string
  ggufRunMode: GgufRunMode
  kaitoComputeType: KaitoComputeType
  selectedPremadeModelId?: string
  maxModelLen?: number
  imageRef?: string
}

export function buildDeploymentFormConfig(
  config: DeploymentConfig,
  options: DeploymentFormConfigBuildOptions
): DeploymentConfig {
  let deploymentConfig = normalizeGatewayAvailability(config, options.gatewayAvailable)

  if (options.selectedRuntime !== 'kaito') {
    return deploymentConfig
  }

  deploymentConfig = { ...deploymentConfig, kaitoResourceType: options.kaitoResourceType }

  if (options.isHuggingFaceGgufModel) {
    return {
      ...deploymentConfig,
      modelSource: 'huggingface',
      modelId: options.modelId,
      ggufFile: options.ggufFile,
      ggufRunMode: options.ggufRunMode,
      computeType: options.kaitoComputeType,
      ...(options.imageRef ? { imageRef: options.imageRef } : {}),
    }
  }

  if (options.isVllmModel) {
    const gpuCount = config.resources?.gpu || 1
    return {
      ...deploymentConfig,
      modelSource: 'vllm',
      modelId: options.modelId,
      computeType: 'gpu',
      resources: { gpu: gpuCount },
      ...(options.maxModelLen && { maxModelLen: options.maxModelLen }),
      ...(config.hfTokenSecret && { hfTokenSecret: config.hfTokenSecret }),
    }
  }

  return {
    ...deploymentConfig,
    modelSource: 'premade',
    computeType: options.kaitoComputeType,
    premadeModel: options.selectedPremadeModelId,
  }
}

export interface AIConfigRecommendedValues {
  prefillReplicas?: number
  decodeReplicas?: number
  prefillGpus?: number
  decodeGpus?: number
  gpuPerReplica?: number
}

const AI_CONFIG_BACKEND_TO_ENGINE: Record<string, Engine> = {
  vllm: 'vllm',
  sglang: 'sglang',
  trtllm: 'trtllm',
}

export function getAIConfigRecommendedEngine(result: AIConfiguratorResult): Engine | undefined {
  return result.backend ? AI_CONFIG_BACKEND_TO_ENGINE[result.backend] : undefined
}

export function getAIConfigRecommendedValues(result: AIConfiguratorResult): AIConfigRecommendedValues {
  const cfg = result.config
  return {
    prefillReplicas: cfg.prefillReplicas,
    decodeReplicas: cfg.decodeReplicas,
    prefillGpus: cfg.prefillTensorParallel || cfg.tensorParallelDegree,
    decodeGpus: cfg.decodeTensorParallel || cfg.tensorParallelDegree,
    gpuPerReplica: cfg.tensorParallelDegree,
  }
}

export function applyAIConfiguratorResultToConfig(
  prev: DeploymentConfig,
  result: AIConfiguratorResult,
  selectedRuntime: RuntimeId
): DeploymentConfig {
  const cfg = result.config
  const recommendedEngine = getAIConfigRecommendedEngine(result)
  const nextEngine = recommendedEngine || prev.engine
  const pipelineParallelDegree = Math.max(1, cfg.pipelineParallelDegree || 1)
  const shouldApplyDynamoParallelism =
    selectedRuntime === 'dynamo' &&
    result.mode === 'aggregated' &&
    nextEngine === 'vllm' &&
    pipelineParallelDegree > 1

  const multiNodeConfig: MultiNodeRecommendation | null = shouldApplyDynamoParallelism
    ? {
        nodeCount: pipelineParallelDegree,
        gpusPerNode: cfg.tensorParallelDegree,
        totalGpus: pipelineParallelDegree * cfg.tensorParallelDegree,
        pipelineParallelSize: pipelineParallelDegree,
      }
    : null

  const engineArgs = setDynamoParallelismEngineArgs(
    {
      ...prev.engineArgs,
      'max-num-batched-tokens': cfg.maxBatchSize,
      'gpu-memory-utilization': cfg.gpuMemoryUtilization,
      ...(cfg.maxNumSeqs && { 'max-num-seqs': cfg.maxNumSeqs }),
    },
    multiNodeConfig
  )

  return {
    ...prev,
    mode: result.mode,
    replicas: result.replicas,
    contextLength: cfg.maxModelLen,
    // Set engine if AI Configurator recommended one
    ...(recommendedEngine && { engine: recommendedEngine }),
    resources: {
      ...prev.resources,
      gpu: cfg.tensorParallelDegree,
    },
    providerOverrides: multiNodeConfig ? buildDynamoMultiNodeOverrides(multiNodeConfig.nodeCount) : undefined,
    // Disaggregated mode settings
    ...(result.mode === 'disaggregated' && {
      prefillReplicas: cfg.prefillReplicas || 1,
      decodeReplicas: cfg.decodeReplicas || 1,
      prefillGpus: cfg.prefillTensorParallel || cfg.tensorParallelDegree,
      decodeGpus: cfg.decodeTensorParallel || cfg.tensorParallelDegree,
    }),
    // Engine args for advanced settings
    engineArgs,
  }
}

export function getAIConfiguratorAppliedToastDescription(result: AIConfiguratorResult): string {
  const cfg = result.config
  const recommendedEngine = getAIConfigRecommendedEngine(result)
  const engineInfo = recommendedEngine ? `, Engine=${recommendedEngine.toUpperCase()}` : ''
  const pipelineInfo = cfg.pipelineParallelDegree && cfg.pipelineParallelDegree > 1
    ? `, PP=${cfg.pipelineParallelDegree}`
    : ''
  return `AI Configurator recommendations applied. TP=${cfg.tensorParallelDegree}${pipelineInfo}, Context=${cfg.maxModelLen}${engineInfo}`
}

/**
 * Merge or strip the FP8 precision engine args (`quantization`,
 * `kv-cache-dtype`) without clobbering other args. Sets a key to `fp8` when the
 * corresponding precision is FP8 and the engine supports the flag; otherwise
 * removes ONLY a value this control owns (`fp8`). A user-provided non-fp8 value
 * (e.g. `awq`/`gptq` typed into the advanced engine-args editor) is preserved,
 * since FP16/BF16 is merely the engine default and shouldn't override an
 * explicit user choice.
 */
export function setFp8PrecisionEngineArgs(
  engineArgs: Record<string, unknown> | undefined,
  opts: { weightFp8: boolean; kvFp8: boolean }
): Record<string, unknown> | undefined {
  const nextEngineArgs = { ...(engineArgs || {}) }

  if (opts.weightFp8) {
    nextEngineArgs[QUANTIZATION_ARG] = 'fp8'
  } else if (nextEngineArgs[QUANTIZATION_ARG] === 'fp8') {
    // Only strip the value WE set. A user-provided non-fp8 quantization
    // (e.g. awq, gptq) from the advanced engine-args editor is preserved.
    delete nextEngineArgs[QUANTIZATION_ARG]
  }

  if (opts.kvFp8) {
    nextEngineArgs[KV_CACHE_DTYPE_ARG] = 'fp8'
  } else if (nextEngineArgs[KV_CACHE_DTYPE_ARG] === 'fp8') {
    delete nextEngineArgs[KV_CACHE_DTYPE_ARG]
  }

  return Object.keys(nextEngineArgs).length > 0 ? nextEngineArgs : undefined
}
