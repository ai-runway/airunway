import { describe, expect, it } from 'vitest'
import type { DeploymentConfig } from '@/hooks/useDeployments'
import type { AIConfiguratorResult } from '@/lib/api'

import {
  PIPELINE_PARALLEL_SIZE_ARG,
  TENSOR_PARALLEL_SIZE_ARG,
  applyAIConfiguratorResultToConfig,
  buildDeploymentFormConfig,
  createInitialDeploymentConfig,
  calculateSelectedGpus,
  applyRuntimeChangeToConfig,
  getAIConfigRecommendedValues,
  getAIConfiguratorAppliedToastDescription,
  getAvailableEnginesForRuntime,
  getCurrentMultiNode,
  getDefaultEngineForRuntime,
  getMaxGpusPerPod,
  getDefaultRuntimeForModel,
  getDeploymentModelFacts,
  getNodeCountFromOverrides,
  isKaitoConfigValid,
  selectPreferredGgufFile,
} from './deploymentFormModel'

function baseConfig(overrides: Partial<DeploymentConfig> = {}): DeploymentConfig {
  return {
    name: 'demo',
    namespace: 'dynamo-system',
    modelId: 'Qwen/Qwen3-0.6B',
    engine: 'vllm',
    mode: 'aggregated',
    provider: 'dynamo',
    routerMode: 'kv',
    replicas: 1,
    enforceEager: true,
    enablePrefixCaching: true,
    trustRemoteCode: false,
    resources: { gpu: 1 },
    ...overrides,
  }
}

describe('deploymentFormModel', () => {
  it('selects the best default runtime from installed compatible runtimes', () => {
    expect(getDefaultRuntimeForModel(['llamacpp'], undefined)).toBe('kaito')
    expect(getDefaultRuntimeForModel(['vllm'], undefined)).toBe('dynamo')
    expect(getDefaultRuntimeForModel(['vllm'], [
      { id: 'kuberay', installed: false },
      { id: 'vllm', installed: true },
      { id: 'dynamo', installed: false },
    ])).toBe('vllm')
    expect(getDefaultRuntimeForModel(['vllm'], [
      { id: 'kuberay', installed: false },
      { id: 'vllm', installed: false },
    ])).toBe('kuberay')
  })

  it('filters available engines by runtime and chooses a default engine', () => {
    expect(getAvailableEnginesForRuntime('dynamo', ['vllm', 'trtllm', 'llamacpp'])).toEqual(['vllm', 'trtllm'])
    expect(getAvailableEnginesForRuntime('kuberay', ['vllm', 'sglang'])).toEqual(['vllm'])
    expect(getDefaultEngineForRuntime('dynamo', ['llamacpp'])).toBe('llamacpp')
    expect(getDefaultEngineForRuntime('kuberay', ['sglang', 'vllm'])).toBe('vllm')
  })

  it('classifies model facts used by runtime-specific form paths', () => {
    expect(getDeploymentModelFacts({ id: 'unsloth/Qwen3-GGUF', supportedEngines: ['llamacpp'] })).toEqual({
      isHuggingFaceGgufModel: true,
      isVllmModel: false,
    })
    expect(getDeploymentModelFacts({ id: 'kaito/llama3.2-1b', supportedEngines: ['llamacpp'] })).toEqual({
      isHuggingFaceGgufModel: false,
      isVllmModel: false,
    })
    expect(getDeploymentModelFacts({ id: 'meta-llama/Llama-3.1-8B-Instruct', supportedEngines: ['vllm', 'sglang'] })).toEqual({
      isHuggingFaceGgufModel: false,
      isVllmModel: true,
    })
    expect(getDeploymentModelFacts({ id: 'hybrid/model', supportedEngines: ['vllm', 'llamacpp'] })).toEqual({
      isHuggingFaceGgufModel: false,
      isVllmModel: false,
    })
  })

  it('creates the initial deployment config for a selected runtime', () => {
    expect(createInitialDeploymentConfig({
      model: { id: 'meta-llama/Llama-3.1-8B-Instruct', supportedEngines: ['vllm', 'sglang'] },
      runtime: 'kuberay',
      name: 'custom-name',
      hfTokenSecret: 'hf-token-secret',
    })).toEqual({
      name: 'custom-name',
      namespace: 'kuberay-system',
      modelId: 'meta-llama/Llama-3.1-8B-Instruct',
      engine: 'vllm',
      mode: 'aggregated',
      provider: 'kuberay',
      routerMode: 'default',
      replicas: 1,
      hfTokenSecret: 'hf-token-secret',
      enforceEager: true,
      enablePrefixCaching: true,
      trustRemoteCode: false,
      prefillReplicas: 1,
      decodeReplicas: 1,
      prefillGpus: 1,
      decodeGpus: 1,
      resources: { gpu: 0 },
    })
  })

  it('switches away from Dynamo by resetting runtime-specific fields and unsupported engines', () => {
    const next = applyRuntimeChangeToConfig(baseConfig({
      engine: 'sglang',
      providerOverrides: { spec: { services: { VllmWorker: { multinode: { nodeCount: 2 } } } } },
      engineArgs: {
        [TENSOR_PARALLEL_SIZE_ARG]: '4',
        [PIPELINE_PARALLEL_SIZE_ARG]: '2',
        custom: 'keep',
      },
    }), {
      runtime: 'kaito',
      modelEngines: ['vllm', 'sglang'],
      recommendedGpus: 1,
    })

    expect(next.provider).toBe('kaito')
    expect(next.namespace).toBe('kaito-workspace')
    expect(next.engine).toBe('vllm')
    expect(next.routerMode).toBe('default')
    expect(next.mode).toBe('aggregated')
    expect(next.providerOverrides).toBeUndefined()
    expect(next.engineArgs).toEqual({ custom: 'keep' })
  })

  it('recalculates Dynamo multi-node parallelism when switching to Dynamo vLLM', () => {
    const next = applyRuntimeChangeToConfig(baseConfig({
      namespace: 'kuberay-system',
      provider: 'kuberay',
      engine: 'vllm',
      resources: { gpu: 4 },
      engineArgs: { custom: 'keep' },
    }), {
      runtime: 'dynamo',
      modelEngines: ['vllm'],
      recommendedGpus: 4,
      estimatedMemoryGb: 900,
      gpuMemoryGb: 80,
    })

    expect(next.provider).toBe('dynamo')
    expect(next.namespace).toBe('dynamo-system')
    expect(getNodeCountFromOverrides(next.providerOverrides)).toBe(3)
    expect(next.engineArgs).toEqual({
      custom: 'keep',
      [TENSOR_PARALLEL_SIZE_ARG]: '4',
      [PIPELINE_PARALLEL_SIZE_ARG]: '3',
    })
  })

  it('applies aggregated AI Configurator recommendations including Dynamo parallelism', () => {
    const result: AIConfiguratorResult = {
      success: true,
      backend: 'vllm',
      supportedBackends: ['vllm'],
      mode: 'aggregated',
      replicas: 2,
      config: {
        tensorParallelDegree: 4,
        pipelineParallelDegree: 3,
        maxBatchSize: 8192,
        maxNumSeqs: 64,
        gpuMemoryUtilization: 0.92,
        maxModelLen: 16384,
      },
    }

    const next = applyAIConfiguratorResultToConfig(baseConfig({
      engine: 'sglang',
      engineArgs: { custom: 'keep' },
    }), result, 'dynamo')

    expect(next.engine).toBe('vllm')
    expect(next.mode).toBe('aggregated')
    expect(next.replicas).toBe(2)
    expect(next.contextLength).toBe(16384)
    expect(next.resources?.gpu).toBe(4)
    expect(getNodeCountFromOverrides(next.providerOverrides)).toBe(3)
    expect(next.engineArgs).toEqual({
      custom: 'keep',
      'max-num-batched-tokens': 8192,
      'gpu-memory-utilization': 0.92,
      'max-num-seqs': 64,
      [TENSOR_PARALLEL_SIZE_ARG]: '4',
      [PIPELINE_PARALLEL_SIZE_ARG]: '3',
    })
    expect(getAIConfiguratorAppliedToastDescription(result)).toBe(
      'AI Configurator recommendations applied. TP=4, PP=3, Context=16384, Engine=VLLM'
    )
  })

  it('applies disaggregated AI Configurator recommendations and exposes badge values', () => {
    const result: AIConfiguratorResult = {
      success: true,
      backend: 'sglang',
      mode: 'disaggregated',
      replicas: 1,
      config: {
        tensorParallelDegree: 2,
        maxBatchSize: 4096,
        gpuMemoryUtilization: 0.85,
        maxModelLen: 8192,
        prefillReplicas: 2,
        decodeReplicas: 3,
        prefillTensorParallel: 4,
        decodeTensorParallel: 1,
      },
    }

    const next = applyAIConfiguratorResultToConfig(baseConfig(), result, 'dynamo')

    expect(next.engine).toBe('sglang')
    expect(next.mode).toBe('disaggregated')
    expect(next.prefillReplicas).toBe(2)
    expect(next.decodeReplicas).toBe(3)
    expect(next.prefillGpus).toBe(4)
    expect(next.decodeGpus).toBe(1)
    expect(next.providerOverrides).toBeUndefined()
    expect(getAIConfigRecommendedValues(result)).toEqual({
      prefillReplicas: 2,
      decodeReplicas: 3,
      prefillGpus: 4,
      decodeGpus: 1,
      gpuPerReplica: 2,
    })
  })


  it('builds non-KAITO submit configs by only normalizing unavailable gateway routing', () => {
    const next = buildDeploymentFormConfig(baseConfig({ gatewayEnabled: true }), {
      selectedRuntime: 'dynamo',
      gatewayAvailable: false,
      kaitoResourceType: 'workspace',
      isHuggingFaceGgufModel: false,
      isVllmModel: false,
      modelId: 'Qwen/Qwen3-0.6B',
      ggufFile: '',
      ggufRunMode: 'direct',
      kaitoComputeType: 'gpu',
    })

    expect(next.gatewayEnabled).toBeUndefined()
    expect(next.provider).toBe('dynamo')
  })

  it('builds KAITO HuggingFace GGUF direct and built-image configs', () => {
    const direct = buildDeploymentFormConfig(baseConfig({ provider: 'kaito' }), {
      selectedRuntime: 'kaito',
      gatewayAvailable: true,
      kaitoResourceType: 'workspace',
      isHuggingFaceGgufModel: true,
      isVllmModel: false,
      modelId: 'org/model-gguf',
      ggufFile: 'model.Q4_K_M.gguf',
      ggufRunMode: 'direct',
      kaitoComputeType: 'cpu',
    })

    expect(direct).toMatchObject({
      kaitoResourceType: 'workspace',
      modelSource: 'huggingface',
      modelId: 'org/model-gguf',
      ggufFile: 'model.Q4_K_M.gguf',
      ggufRunMode: 'direct',
      computeType: 'cpu',
    })
    expect(direct.imageRef).toBeUndefined()

    const built = buildDeploymentFormConfig(baseConfig({ provider: 'kaito' }), {
      selectedRuntime: 'kaito',
      gatewayAvailable: true,
      kaitoResourceType: 'inferenceset',
      isHuggingFaceGgufModel: true,
      isVllmModel: false,
      modelId: 'org/model-gguf',
      ggufFile: 'model.Q4_K_M.gguf',
      ggufRunMode: 'build',
      kaitoComputeType: 'gpu',
      imageRef: 'registry.local/model:tag',
    })

    expect(built).toMatchObject({
      kaitoResourceType: 'inferenceset',
      modelSource: 'huggingface',
      ggufRunMode: 'build',
      computeType: 'gpu',
      imageRef: 'registry.local/model:tag',
    })
  })

  it('builds KAITO vLLM and premade configs', () => {
    const vllm = buildDeploymentFormConfig(baseConfig({
      provider: 'kaito',
      resources: { gpu: 2 },
      hfTokenSecret: 'hf-token-secret',
    }), {
      selectedRuntime: 'kaito',
      gatewayAvailable: true,
      kaitoResourceType: 'workspace',
      isHuggingFaceGgufModel: false,
      isVllmModel: true,
      modelId: 'meta-llama/Llama-3.1-8B-Instruct',
      ggufFile: '',
      ggufRunMode: 'direct',
      kaitoComputeType: 'gpu',
      maxModelLen: 8192,
    })

    expect(vllm).toMatchObject({
      modelSource: 'vllm',
      modelId: 'meta-llama/Llama-3.1-8B-Instruct',
      computeType: 'gpu',
      resources: { gpu: 2 },
      maxModelLen: 8192,
      hfTokenSecret: 'hf-token-secret',
    })

    const premade = buildDeploymentFormConfig(baseConfig({ provider: 'kaito' }), {
      selectedRuntime: 'kaito',
      gatewayAvailable: true,
      kaitoResourceType: 'workspace',
      isHuggingFaceGgufModel: false,
      isVllmModel: false,
      modelId: 'kaito/llama3.2-1b',
      ggufFile: '',
      ggufRunMode: 'direct',
      kaitoComputeType: 'cpu',
      selectedPremadeModelId: 'llama3.2:1b',
    })

    expect(premade).toMatchObject({
      modelSource: 'premade',
      computeType: 'cpu',
      premadeModel: 'llama3.2:1b',
    })
  })


  it('derives selected GPU totals, current multi-node state, and max GPUs per pod', () => {
    const aggregated = baseConfig({
      replicas: 2,
      resources: { gpu: 4 },
      providerOverrides: { spec: { services: { VllmWorker: { multinode: { nodeCount: 3 } } } } },
      engineArgs: { [PIPELINE_PARALLEL_SIZE_ARG]: '3' },
    })
    const nodeCount = getNodeCountFromOverrides(aggregated.providerOverrides)

    expect(calculateSelectedGpus(aggregated, 1, nodeCount)).toBe(24)
    expect(getCurrentMultiNode(aggregated, 1, nodeCount, 3)).toEqual({
      nodeCount: 3,
      gpusPerNode: 4,
      totalGpus: 12,
      pipelineParallelSize: 3,
    })
    expect(getMaxGpusPerPod(aggregated, 1)).toBe(4)

    const disaggregated = baseConfig({
      mode: 'disaggregated',
      prefillReplicas: 2,
      prefillGpus: 3,
      decodeReplicas: 4,
      decodeGpus: 1,
    })
    expect(calculateSelectedGpus(disaggregated, 1, 1)).toBe(10)
    expect(getMaxGpusPerPod(disaggregated, 1)).toBe(3)
  })

  it('validates KAITO source-specific requirements', () => {
    expect(isKaitoConfigValid({
      selectedRuntime: 'dynamo',
      isHuggingFaceGgufModel: false,
      isVllmModel: false,
      ggufFile: '',
      gpuCount: 0,
      hasSelectedPremadeModel: false,
    })).toBe(true)

    expect(isKaitoConfigValid({
      selectedRuntime: 'kaito',
      isHuggingFaceGgufModel: true,
      isVllmModel: false,
      ggufFile: 'model.gguf',
      gpuCount: 0,
      hasSelectedPremadeModel: false,
    })).toBe(true)
    expect(isKaitoConfigValid({
      selectedRuntime: 'kaito',
      isHuggingFaceGgufModel: true,
      isVllmModel: false,
      ggufFile: 'README.md',
      gpuCount: 0,
      hasSelectedPremadeModel: false,
    })).toBe(false)

    expect(isKaitoConfigValid({
      selectedRuntime: 'kaito',
      isHuggingFaceGgufModel: false,
      isVllmModel: true,
      ggufFile: '',
      gpuCount: 1,
      hasSelectedPremadeModel: false,
    })).toBe(true)
    expect(isKaitoConfigValid({
      selectedRuntime: 'kaito',
      isHuggingFaceGgufModel: false,
      isVllmModel: false,
      ggufFile: '',
      gpuCount: 0,
      hasSelectedPremadeModel: true,
    })).toBe(true)
  })


  it('selects the preferred GGUF file without overriding an existing choice', () => {
    expect(selectPreferredGgufFile([], '')).toBe('')
    expect(selectPreferredGgufFile(['model.Q5_K_M.gguf', 'model.Q4_K_M.gguf'], '')).toBe('model.Q4_K_M.gguf')
    expect(selectPreferredGgufFile(['model.Q5_K_M.gguf', 'model.Q8_0.gguf'], '')).toBe('model.Q5_K_M.gguf')
    expect(selectPreferredGgufFile(['model.Q4_K_M.gguf'], 'custom.gguf')).toBe('custom.gguf')
  })

})
