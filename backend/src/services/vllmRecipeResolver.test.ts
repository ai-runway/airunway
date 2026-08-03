import { describe, expect, test } from 'bun:test';
import type { VllmRecipeRawResponse } from '@airunway/shared';
import { VllmRecipeResolver } from './vllmRecipeResolver';
import type { VllmRecipesClient } from './vllmRecipesClient';

function createMockClient(recipe: Record<string, unknown>): VllmRecipesClient {
  return {
    sourceBaseUrl: 'https://recipes.vllm.ai',
    list: async () => ({ recipes: [], total: 0, source: 'https://recipes.vllm.ai/models.json' }),
    get: async (org: string, model: string) => ({
      modelId: `${org}/${model}`,
      source: `https://recipes.vllm.ai/${org}/${model}.json`,
      recipe,
    }) as VllmRecipeRawResponse,
    getByModelId: async (modelId: string) => ({
      modelId,
      source: `https://recipes.vllm.ai/${modelId}.json`,
      recipe,
    }) as VllmRecipeRawResponse,
    fetchReference: async () => recipe,
    resolveReference: (reference: string) => reference,
  } as unknown as VllmRecipesClient;
}

const phi4MiniRecipe = {
  hf_id: 'microsoft/Phi-4-mini-instruct',
  meta: {
    title: 'Phi-4',
    provider: 'Microsoft',
    date_updated: '2026-04-17',
    hardware: { h100: 'verified' },
  },
  recommended_command: {
    hardware: 'h200',
    strategy: 'single_node_tp',
    variant: 'default',
    node_count: 1,
    deploy_type: 'single_node',
    env: {},
    docker_image: 'vllm/vllm-openai:latest',
    argv: [
      'vllm',
      'serve',
      'microsoft/Phi-4-mini-instruct',
      '--tensor-parallel-size',
      '1',
    ],
    strategy_spec: {
      name: 'single_node_tp',
      deploy_type: 'single_node',
      hardware_match: {
        min_gpus: 1,
        max_gpus: 8,
        multi_node: false,
      },
    },
    hardware_profile: {
      brand: 'NVIDIA',
      display_name: 'H200',
      gpu_count: 8,
      vram_gb: 1128,
      multi_node: false,
    },
  },
  model: {
    model_id: 'microsoft/Phi-4-mini-instruct',
    min_vllm_version: '0.7.0',
    parameter_count: '4B',
    context_length: 131072,
  },
  variants: {
    default: {
      precision: 'bf16',
      vram_minimum_gb: 10,
      description: 'Phi-4-mini-instruct, conversational instruction-tuned',
    },
  },
  hardware_overrides: {},
};

describe('VllmRecipeResolver', () => {
  test('materializes the Phi-4 mini recipe for a single A100 GPU through the resolver path', async () => {
    const resolver = new VllmRecipeResolver(createMockClient(phi4MiniRecipe));

    const resolved = await resolver.resolve({
      modelId: 'microsoft/Phi-4-mini-instruct',
      mode: 'aggregated',
      hardware: 'a100',
      imageChoice: { type: 'recipe' },
    });

    expect(resolved.provider).toBe('vllm');
    expect(resolved.engine).toBe('vllm');
    expect(resolved.mode).toBe('aggregated');
    expect(resolved.imageRef).toBe('vllm/vllm-openai:latest');
    expect(resolved.resources).toEqual({ gpu: 1 });
    expect(resolved.engineArgs).toEqual({ 'tensor-parallel-size': '1' });
    expect(resolved.engineExtraArgs).toEqual([]);
    expect(resolved.env).toEqual({});
    expect(resolved.recipeProvenance).toMatchObject({
      id: 'microsoft/Phi-4-mini-instruct',
      strategy: 'single_node_tp',
      hardware: 'a100',
      variant: 'default',
    });
    expect(resolved.annotations).toMatchObject({
      'airunway.ai/generated-by': 'vllm-recipe-resolver',
      'airunway.ai/recipe.id': 'microsoft/Phi-4-mini-instruct',
      'airunway.ai/recipe.strategy': 'single_node_tp',
      'airunway.ai/recipe.hardware': 'a100',
      'airunway.ai/recipe.variant': 'default',
    });
    expect(resolved.warnings).toEqual([]);
  });

  test('uses the hardware profile as a fallback when the recipe has no explicit parallelism', async () => {
    const resolver = new VllmRecipeResolver(createMockClient({
      hf_id: 'example/no-parallelism',
      recommended_command: {
        docker_image: 'example/vllm:latest',
        argv: ['vllm', 'serve', 'example/no-parallelism'],
        hardware_profile: {
          gpu_count: 4,
        },
      },
    }));

    const resolved = await resolver.resolve({
      modelId: 'example/no-parallelism',
      imageChoice: { type: 'recipe' },
    });

    expect(resolved.resources).toEqual({ gpu: 4 });
  });

  test('preserves an explicit custom image choice over the recipe image', async () => {
    const resolver = new VllmRecipeResolver(createMockClient(phi4MiniRecipe));

    const resolved = await resolver.resolve({
      modelId: 'microsoft/Phi-4-mini-instruct',
      imageChoice: {
        type: 'custom',
        imageRef: 'registry.example.com/vllm-openai:launch-phi4',
      },
    });

    expect(resolved.imageRef).toBe('registry.example.com/vllm-openai:launch-phi4');
  });

  test('derives GPUs-per-pod as tensor-parallel × pipeline-parallel, ignoring data-parallel', async () => {
    const resolver = new VllmRecipeResolver(createMockClient({
      hf_id: 'example/multi-gpu',
      recommended_command: {
        docker_image: 'example/vllm:latest',
        argv: [
          'vllm',
          'serve',
          'example/multi-gpu',
          '--tensor-parallel-size',
          '4',
          '--pipeline-parallel-size',
          '2',
          '--data-parallel-size',
          '4',
        ],
      },
    }));

    const resolved = await resolver.resolve({
      modelId: 'example/multi-gpu',
      imageChoice: { type: 'recipe' },
    });

    // 4 (TP) × 2 (PP) = 8 GPUs per pod. data-parallel-size scales replicas, not
    // GPUs-per-pod, so it must NOT inflate this to 32.
    expect(resolved.resources).toEqual({ gpu: 8 });
  });

  test('does not strip a leading --model flag after "vllm serve"', async () => {
    const resolver = new VllmRecipeResolver(createMockClient({
      hf_id: 'example/flag-model',
      recommended_command: {
        docker_image: 'example/vllm:latest',
        // Model passed as a flag rather than a positional argument.
        argv: ['vllm', 'serve', '--model', 'example/flag-model', '--tensor-parallel-size', '2'],
      },
    }));

    const resolved = await resolver.resolve({
      modelId: 'example/flag-model',
      imageChoice: { type: 'recipe' },
    });

    // The --model flag must survive (not be swallowed as a positional model id).
    expect(resolved.engineArgs).toMatchObject({ model: 'example/flag-model', 'tensor-parallel-size': '2' });
  });

  test('detects disaggregated mode from the recipe deploy_type', async () => {
    const resolver = new VllmRecipeResolver(createMockClient({
      hf_id: 'example/disagg',
      recommended_command: {
        docker_image: 'example/vllm:latest',
        deploy_type: 'disaggregated',
        argv: ['vllm', 'serve', 'example/disagg'],
      },
    }));

    const resolved = await resolver.resolve({
      modelId: 'example/disagg',
      imageChoice: { type: 'recipe' },
    });

    expect(resolved.mode).toBe('disaggregated');
  });

  test('an explicit requested mode overrides the recipe deploy_type', async () => {
    const resolver = new VllmRecipeResolver(createMockClient({
      hf_id: 'example/disagg',
      recommended_command: {
        docker_image: 'example/vllm:latest',
        deploy_type: 'disaggregated',
        argv: ['vllm', 'serve', 'example/disagg'],
      },
    }));

    const resolved = await resolver.resolve({
      modelId: 'example/disagg',
      mode: 'aggregated',
      imageChoice: { type: 'recipe' },
    });

    expect(resolved.mode).toBe('aggregated');
  });
});
