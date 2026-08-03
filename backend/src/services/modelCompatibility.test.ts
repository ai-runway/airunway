import { describe, test, expect } from 'bun:test';
import {
  inferArchitectureFromModelId,
  getSupportedEngines,
  isPipelineTagCompatible,
  getIncompatibilityReason,
  extractParameterCount,
  processHfModel,
  filterCompatibleModels,
  getEngineArchitectures,
} from './modelCompatibility';
import { parseParameterCountFromName } from '@airunway/shared';
import type { HfApiModelResult } from '@airunway/shared';

describe('inferArchitectureFromModelId', () => {
  test('infers LlamaForCausalLM for llama models', () => {
    expect(inferArchitectureFromModelId('meta-llama/Llama-3.1-8B-Instruct')).toEqual(['LlamaForCausalLM']);
    expect(inferArchitectureFromModelId('meta-llama/Llama-2-7b-chat-hf')).toEqual(['LlamaForCausalLM']);
    expect(inferArchitectureFromModelId('TinyLlama/TinyLlama-1.1B-Chat-v1.0')).toEqual(['LlamaForCausalLM']);
    expect(inferArchitectureFromModelId('NousResearch/Llama-2-7b-chat-hf')).toEqual(['LlamaForCausalLM']);
  });

  test('infers MistralForCausalLM for mistral models', () => {
    expect(inferArchitectureFromModelId('mistralai/Mistral-7B-v0.1')).toEqual(['MistralForCausalLM']);
    expect(inferArchitectureFromModelId('mistralai/Mistral-7B-Instruct-v0.2')).toEqual(['MistralForCausalLM']);
  });

  test('infers MixtralForCausalLM for mixtral models', () => {
    expect(inferArchitectureFromModelId('mistralai/Mixtral-8x7B-v0.1')).toEqual(['MixtralForCausalLM']);
  });

  test('infers Qwen architectures', () => {
    expect(inferArchitectureFromModelId('Qwen/Qwen2-7B-Instruct')).toEqual(['Qwen2ForCausalLM']);
    expect(inferArchitectureFromModelId('Qwen/Qwen2.5-7B-Instruct')).toEqual(['Qwen2ForCausalLM']);
    expect(inferArchitectureFromModelId('Qwen/Qwen3-8B')).toEqual(['Qwen3ForCausalLM']);
  });

  test('infers GemmaForCausalLM for gemma models', () => {
    expect(inferArchitectureFromModelId('google/gemma-7b')).toEqual(['GemmaForCausalLM']);
    expect(inferArchitectureFromModelId('google/gemma-2-9b')).toEqual(['Gemma2ForCausalLM']);
    expect(inferArchitectureFromModelId('google/gemma2-9b')).toEqual(['Gemma2ForCausalLM']);
  });

  test('infers PhiForCausalLM for phi models', () => {
    expect(inferArchitectureFromModelId('microsoft/phi-2')).toEqual(['PhiForCausalLM']);
    expect(inferArchitectureFromModelId('microsoft/Phi-3-mini-4k-instruct')).toEqual(['Phi3ForCausalLM']);
  });

  test('returns empty array for unknown models', () => {
    expect(inferArchitectureFromModelId('unknown/model-name')).toEqual([]);
    expect(inferArchitectureFromModelId('some-random-model')).toEqual([]);
  });
});

describe('getSupportedEngines', () => {
  test('returns vllm, sglang, trtllm for LlamaForCausalLM', () => {
    const engines = getSupportedEngines(['LlamaForCausalLM']);
    expect(engines).toContain('vllm');
    expect(engines).toContain('sglang');
    expect(engines).toContain('trtllm');
  });

  test('returns engines for Qwen architecture', () => {
    const engines = getSupportedEngines(['Qwen2ForCausalLM']);
    expect(engines).toContain('vllm');
    expect(engines).toContain('sglang');
    expect(engines).toContain('trtllm');
  });

  test('returns limited engines for lesser supported architectures', () => {
    const engines = getSupportedEngines(['JambaForCausalLM']);
    expect(engines).toContain('vllm');
    expect(engines).not.toContain('trtllm');
  });

  test('returns only vllm for Laguna architecture', () => {
    const engines = getSupportedEngines(['LagunaForCausalLM']);
    expect(engines).toEqual(['vllm']);
  });

  test('returns empty array for unknown architecture', () => {
    const engines = getSupportedEngines(['UnknownArchitecture']);
    expect(engines).toHaveLength(0);
  });

  test('handles multiple architectures', () => {
    const engines = getSupportedEngines(['UnknownArch', 'LlamaForCausalLM']);
    expect(engines.length).toBeGreaterThan(0);
  });

  test('returns empty for empty input', () => {
    const engines = getSupportedEngines([]);
    expect(engines).toHaveLength(0);
  });
});

describe('isPipelineTagCompatible', () => {
  test('returns true for text-generation', () => {
    expect(isPipelineTagCompatible('text-generation')).toBe(true);
  });

  test('returns true for text2text-generation', () => {
    expect(isPipelineTagCompatible('text2text-generation')).toBe(true);
  });

  test('returns true for conversational', () => {
    expect(isPipelineTagCompatible('conversational')).toBe(true);
  });

  test('returns false for image-classification', () => {
    expect(isPipelineTagCompatible('image-classification')).toBe(false);
  });

  test('returns false for undefined', () => {
    expect(isPipelineTagCompatible(undefined)).toBe(false);
  });

  test('returns false for empty string', () => {
    expect(isPipelineTagCompatible('')).toBe(false);
  });
});

describe('getIncompatibilityReason', () => {
  test('returns reason for no pipeline tag', () => {
    const reason = getIncompatibilityReason(undefined);
    expect(reason).toBe('Model has no pipeline tag');
  });

  test('returns reason for unsupported pipeline tag', () => {
    const reason = getIncompatibilityReason('image-segmentation');
    expect(reason).toContain('not supported');
  });

  test('returns reason for unsupported library', () => {
    const reason = getIncompatibilityReason('text-generation', 'diffusers');
    expect(reason).toContain('diffusers');
  });

  test('returns reason for unknown architecture', () => {
    const reason = getIncompatibilityReason('text-generation', 'transformers', []);
    expect(reason).toContain('unknown');
  });

  test('returns reason for unsupported architecture', () => {
    const reason = getIncompatibilityReason('text-generation', 'transformers', ['UnknownArch'], []);
    expect(reason).toContain('not supported by any engine');
  });

  test('returns undefined for compatible model', () => {
    const reason = getIncompatibilityReason('text-generation', 'transformers', ['LlamaForCausalLM'], ['vllm']);
    expect(reason).toBeUndefined();
  });
});

describe('extractParameterCount', () => {
  test('extracts from safetensors total', () => {
    const model: HfApiModelResult = {
      id: 'test/model',
      safetensors: { total: 7_000_000_000 },
    };
    expect(extractParameterCount(model)).toBe(7_000_000_000);
  });

  test('extracts from safetensors parameters map', () => {
    const model: HfApiModelResult = {
      id: 'test/model',
      safetensors: {
        parameters: {
          'BF16': 3_500_000_000,
          'F16': 3_500_000_000,
        },
      },
    };
    expect(extractParameterCount(model)).toBe(7_000_000_000);
  });

  test('returns undefined when no parameter info', () => {
    const model: HfApiModelResult = {
      id: 'test/model',
    };
    expect(extractParameterCount(model)).toBeUndefined();
  });

  test('prefers total over parameters map', () => {
    const model: HfApiModelResult = {
      id: 'test/model',
      safetensors: {
        total: 10_000_000_000,
        parameters: { 'BF16': 5_000_000_000 },
      },
    };
    expect(extractParameterCount(model)).toBe(10_000_000_000);
  });
});

describe('processHfModel', () => {
  test('processes compatible model correctly', () => {
    const model: HfApiModelResult = {
      id: 'meta-llama/Llama-3.2-1B',
      downloads: 1000,
      likes: 50,
      pipeline_tag: 'text-generation',
      library_name: 'transformers',
      config: { architectures: ['LlamaForCausalLM'] },
      safetensors: { total: 1_000_000_000 },
      gated: false,
    };

    const result = processHfModel(model);
    expect(result.id).toBe('meta-llama/Llama-3.2-1B');
    expect(result.author).toBe('meta-llama');
    expect(result.name).toBe('Llama-3.2-1B');
    expect(result.compatible).toBe(true);
    expect(result.architectures).toEqual(['LlamaForCausalLM']);
    expect(result.supportedEngines.length).toBeGreaterThan(0);
    expect(result.parameterCount).toBe(1_000_000_000);
    expect(result.estimatedGpuMemory).toBeDefined();
    expect(result.incompatibilityReason).toBeUndefined();
    expect(result.gated).toBe(false);
  });

  test('processes incompatible model correctly', () => {
    const model: HfApiModelResult = {
      id: 'test/vision-model',
      downloads: 500,
      likes: 10,
      pipeline_tag: 'image-classification',
      library_name: 'transformers',
      config: { architectures: ['ViTForImageClassification'] },
    };

    const result = processHfModel(model);
    expect(result.compatible).toBe(false);
    expect(result.incompatibilityReason).toBeDefined();
    expect(result.supportedEngines).toHaveLength(0);
  });

  test('processes Laguna as vllm-only compatible model', () => {
    const model: HfApiModelResult = {
      id: 'poolside/Laguna-XS.2',
      downloads: 100,
      likes: 10,
      pipeline_tag: 'text-generation',
      library_name: 'transformers',
      config: { architectures: ['LagunaForCausalLM'] },
      gated: false,
    };

    const result = processHfModel(model);
    expect(result.compatible).toBe(true);
    expect(result.architectures).toEqual(['LagunaForCausalLM']);
    expect(result.supportedEngines).toEqual(['vllm']);
    expect(result.incompatibilityReason).toBeUndefined();
  });

  test('processes gated model without metadata by inferring architecture', () => {
    const model: HfApiModelResult = {
      id: 'meta-llama/Llama-3.1-8B-Instruct',
      downloads: 100000,
      likes: 5000,
      // Gated models return null for these fields without auth
      pipeline_tag: undefined,
      library_name: undefined,
      config: undefined,
      gated: true,
      safetensors: {
        total: 8030261248,
      },
    };

    const result = processHfModel(model);
    // Should be compatible because we infer LlamaForCausalLM from the model name
    expect(result.compatible).toBe(true);
    expect(result.architectures).toEqual(['LlamaForCausalLM']);
    expect(result.supportedEngines.length).toBeGreaterThan(0);
    expect(result.gated).toBe(true);
    expect(result.parameterCount).toBe(8030261248);
  });

  test('marks unknown model without metadata as incompatible', () => {
    const model: HfApiModelResult = {
      id: 'unknown/some-model',
      downloads: 10,
      likes: 1,
      pipeline_tag: undefined,
      library_name: undefined,
      config: undefined,
      gated: false,
    };

    const result = processHfModel(model);
    // Unknown model cannot have architecture inferred
    expect(result.compatible).toBe(false);
    expect(result.architectures).toEqual([]);
    expect(result.incompatibilityReason).toBeDefined();
  });

  test('handles auto-gated models', () => {
    const model: HfApiModelResult = {
      id: 'test/auto-gated',
      gated: 'auto',
    };

    const result = processHfModel(model);
    expect(result.gated).toBe(true);
  });

  test('handles model ID without slash', () => {
    const model: HfApiModelResult = {
      id: 'simple-model-name',
      pipeline_tag: 'text-generation',
    };

    const result = processHfModel(model);
    expect(result.author).toBe('simple-model-name');
    expect(result.name).toBe('simple-model-name');
  });
});

describe('filterCompatibleModels', () => {
  test('filters to only compatible models', () => {
    const models: HfApiModelResult[] = [
      {
        id: 'meta-llama/Llama-3.1-8B',
        gated: true,
      },
      {
        id: 'unknown/unsupported-model',
        gated: false,
      },
      {
        id: 'TinyLlama/TinyLlama-1.1B',
        pipeline_tag: 'text-generation',
        library_name: 'transformers',
        config: { architectures: ['LlamaForCausalLM'] },
        gated: false,
      },
    ];

    const compatible = filterCompatibleModels(models);
    expect(compatible.length).toBe(2);
    expect(compatible.map(m => m.id)).toContain('meta-llama/Llama-3.1-8B');
    expect(compatible.map(m => m.id)).toContain('TinyLlama/TinyLlama-1.1B');
    expect(compatible.map(m => m.id)).not.toContain('unknown/unsupported-model');
  });

  test('filters out incompatible models', () => {
    const models: HfApiModelResult[] = [
      {
        id: 'test/llm',
        pipeline_tag: 'text-generation',
        library_name: 'transformers',
        config: { architectures: ['LlamaForCausalLM'] },
      },
      {
        id: 'test/vision',
        pipeline_tag: 'image-classification',
        config: { architectures: ['ViTForImageClassification'] },
      },
    ];

    const result = filterCompatibleModels(models);
    expect(result).toHaveLength(1);
    expect(result[0].id).toBe('test/llm');
  });

  test('returns empty array for all incompatible', () => {
    const models: HfApiModelResult[] = [
      { id: 'test/1', pipeline_tag: 'image-classification' },
      { id: 'test/2', pipeline_tag: 'audio-classification' },
    ];

    const result = filterCompatibleModels(models);
    expect(result).toHaveLength(0);
  });
});

describe('parseParameterCountFromName', () => {
  test('parses billion parameters', () => {
    expect(parseParameterCountFromName('Llama-3.1-8B-Instruct')).toBe(8_000_000_000);
    expect(parseParameterCountFromName('Llama-2-70b-chat')).toBe(70_000_000_000);
    expect(parseParameterCountFromName('Qwen2-1.5B')).toBe(1_500_000_000);
  });

  test('parses million parameters', () => {
    expect(parseParameterCountFromName('model-125M')).toBe(125_000_000);
    expect(parseParameterCountFromName('gpt2-350m')).toBe(350_000_000);
  });

  test('returns undefined for unparseable names', () => {
    expect(parseParameterCountFromName('some-model')).toBeUndefined();
  });
});

describe('getEngineArchitectures', () => {
  test('returns architectures for vllm', () => {
    const archs = getEngineArchitectures('vllm');
    expect(archs).toContain('LlamaForCausalLM');
    expect(archs).toContain('MistralForCausalLM');
    expect(archs).toContain('LagunaForCausalLM');
    expect(archs.length).toBeGreaterThan(10);
  });

  test('returns architectures for trtllm', () => {
    const archs = getEngineArchitectures('trtllm');
    expect(archs).toContain('LlamaForCausalLM');
    // trtllm has fewer supported architectures
    expect(archs.length).toBeLessThan(getEngineArchitectures('vllm').length);
  });

  test('returns copy not reference', () => {
    const archs1 = getEngineArchitectures('vllm');
    const archs2 = getEngineArchitectures('vllm');
    archs1.push('TestArch');
    expect(archs2).not.toContain('TestArch');
  });
});
