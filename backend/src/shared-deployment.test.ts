import { describe, expect, test } from 'bun:test';
import { toModelDeploymentManifest, toModelDeploymentSpec, type DeploymentConfig } from '@airunway/shared';

const baseConfig: DeploymentConfig = {
  name: 'test-deployment',
  namespace: 'default',
  modelId: 'test/model',
  engine: 'vllm',
  mode: 'aggregated',
  provider: 'dynamo',
  routerMode: 'default',
  replicas: 1,
  enforceEager: true,
  enablePrefixCaching: true,
  trustRemoteCode: false,
  resources: {
    gpu: 1,
  },
};

describe('shared deployment manifest conversion', () => {
  test('maps direct vLLM imageRef to spec.engine.image', () => {
    const spec = toModelDeploymentSpec({
      ...baseConfig,
      provider: 'vllm',
      imageRef: 'vllm/vllm-openai:cu130-nightly',
    });

    expect(spec.engine.image).toBe('vllm/vllm-openai:cu130-nightly');
    expect(spec.image).toBeUndefined();
  });

  test('maps non-vLLM provider imageRef to legacy spec.image', () => {
    const spec = toModelDeploymentSpec({
      ...baseConfig,
      provider: 'dynamo',
      imageRef: 'nvcr.io/nvidia/dynamo:v1.0.0',
    });

    expect(spec.image).toBe('nvcr.io/nvidia/dynamo:v1.0.0');
    expect(spec.engine.image).toBeUndefined();
  });

  test('maps recipe provenance to metadata annotations only', () => {
    const manifest = toModelDeploymentManifest({
      ...baseConfig,
      provider: 'vllm',
      recipeProvenance: {
        source: 'vllm-recipes',
        id: 'deepseek-ai/DeepSeek-V3.2',
        strategy: 'single_node_dep',
        hardware: 'h200',
        variant: 'default',
        precision: 'fp8',
        features: ['chunkedPrefill', 'maxNumBatchedTokens'],
        revision: '2026-05-04',
      },
    });

    expect(manifest.metadata.annotations).toEqual({
      'airunway.ai/generated-by': 'vllm-recipe-resolver',
      'airunway.ai/recipe.source': 'vllm-recipes',
      'airunway.ai/recipe.id': 'deepseek-ai/DeepSeek-V3.2',
      'airunway.ai/recipe.strategy': 'single_node_dep',
      'airunway.ai/recipe.hardware': 'h200',
      'airunway.ai/recipe.variant': 'default',
      'airunway.ai/recipe.precision': 'fp8',
      'airunway.ai/recipe.features': JSON.stringify(['chunkedPrefill', 'maxNumBatchedTokens']),
      'airunway.ai/recipe.revision': '2026-05-04',
    });
    expect((manifest.spec as Record<string, unknown>).recipe).toBeUndefined();
    expect((manifest.spec as Record<string, unknown>).recipes).toBeUndefined();
    expect((manifest.status as Record<string, unknown> | undefined)?.recipe).toBeUndefined();
  });

  test('recipe provenance does not materialize engine args', () => {
    const manifest = toModelDeploymentManifest({
      ...baseConfig,
      provider: 'vllm',
      recipeProvenance: {
        id: 'deepseek-ai/DeepSeek-V3.2',
      },
    });

    expect(manifest.metadata.annotations).toEqual({
      'airunway.ai/generated-by': 'vllm-recipe-resolver',
      'airunway.ai/recipe.id': 'deepseek-ai/DeepSeek-V3.2',
    });
    expect(manifest.spec.engine.args).toBeUndefined();
    expect(manifest.spec.engine.extraArgs).toBeUndefined();
  });

  test('empty-string and empty-array provenance produce no annotations', () => {
    const manifest = toModelDeploymentManifest({
      ...baseConfig,
      provider: 'vllm',
      recipeProvenance: {
        source: '',
        id: '   ',
        features: [],
      },
    });

    // No meaningful provenance → no recipe annotations and no generated-by marker.
    expect(manifest.metadata.annotations).toBeUndefined();
  });

  test('trims provenance string values and skips blank ones', () => {
    const manifest = toModelDeploymentManifest({
      ...baseConfig,
      provider: 'vllm',
      recipeProvenance: {
        source: '  vllm-recipes  ',
        id: '',
        features: [],
      },
    });

    expect(manifest.metadata.annotations).toEqual({
      'airunway.ai/generated-by': 'vllm-recipe-resolver',
      'airunway.ai/recipe.source': 'vllm-recipes',
    });
  });

  test('maps env to spec.env', () => {
    const env = {
      VLLM_USE_V1: '1',
      NCCL_DEBUG: 'INFO',
    };

    const spec = toModelDeploymentSpec({
      ...baseConfig,
      provider: 'vllm',
      env,
    });

    expect(spec.env).toEqual([
      { name: 'VLLM_USE_V1', value: '1' },
      { name: 'NCCL_DEBUG', value: 'INFO' },
    ]);
  });

  test('omits empty env from spec.env', () => {
    const spec = toModelDeploymentSpec({
      ...baseConfig,
      provider: 'vllm',
      env: {},
    });

    expect(spec.env).toBeUndefined();
  });

  test('maps engineExtraArgs to spec.engine.extraArgs', () => {
    const engineExtraArgs = [
      '--enable-auto-tool-choice',
      '--tool-call-parser',
      'deepseek_v4',
    ];

    const spec = toModelDeploymentSpec({
      ...baseConfig,
      provider: 'vllm',
      engineExtraArgs,
    });

    expect(spec.engine.extraArgs).toEqual(engineExtraArgs);
  });
});
