import { describe, expect, test } from 'bun:test';
import { defaultDynamoIntent, DYNAMO_ATTEMPT_ANNOTATION, type ModelDeployment } from '@airunway/shared';
import { dynamoIntentSchema, dynamoOverridesSchema, reconfigureDynamoDeployment } from './dynamo-intent';

export function automaticDeployment(): ModelDeployment {
  return {
    apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment',
    metadata: { name: 'test-auto', namespace: 'models', resourceVersion: '42', annotations: { keep: 'me' }, labels: { team: 'inference' } },
    spec: {
      model: { id: 'Qwen/Qwen3-0.6B', source: 'huggingface' },
      engine: { type: 'vllm' }, provider: { name: 'dynamo', overrides: { deploymentMode: 'intent', intent: defaultDynamoIntent() } },
      gateway: { enabled: true }, secrets: { huggingFaceToken: 'hf-token-secret' },
    },
    status: { phase: 'Running', provider: { intent: { phase: 'Deployed', attempt: 'old' } } },
  };
}

describe('Dynamo intent contract', () => {
  test('validates bounded rapid intents and exclusive alternatives', () => {
    expect(dynamoIntentSchema.safeParse(defaultDynamoIntent()).success).toBe(true);
    for (const invalid of [
      { ...defaultDynamoIntent(), hardware: { totalGpus: 0 } },
      { ...defaultDynamoIntent(), hardware: { totalGpus: 65 } },
      { ...defaultDynamoIntent(), hardware: { totalGpus: 1.5 } },
      { ...defaultDynamoIntent(), hardware: { totalGpus: 1, unknown: 1 } },
      { ...defaultDynamoIntent(), searchStrategy: 'thorough' },
      { ...defaultDynamoIntent(), autoApply: false },
      { ...defaultDynamoIntent(), workload: { requestRate: 2, concurrency: 4 } },
      { ...defaultDynamoIntent(), workload: { isl: -1 } },
      { ...defaultDynamoIntent(), workload: { requestRate: Infinity } },
      { ...defaultDynamoIntent(), sla: { ttft: 1, e2eLatency: 2 } },
    ]) expect(dynamoIntentSchema.safeParse(invalid).success).toBe(false);
    expect(dynamoOverridesSchema.safeParse({ deploymentMode: 'intent', intent: defaultDynamoIntent(), spec: {} }).success).toBe(false);
    expect(dynamoIntentSchema.safeParse({ hardware: { totalGpus: 64, gpuSku: 'H100' }, workload: { concurrency: 2 }, sla: { e2eLatency: 5000 } }).success).toBe(true);
  });

  test('atomically changes intent, model, engine, and attempt without losing unrelated values', () => {
    const original = automaticDeployment();
    const next = reconfigureDynamoDeployment(original, {
      resourceVersion: '42', intent: { hardware: { totalGpus: 2 }, searchStrategy: 'rapid' },
      modelId: 'Qwen/Qwen3-8B', engine: 'sglang',
    }, 'new-attempt');
    expect(next.metadata.annotations).toEqual({ keep: 'me', [DYNAMO_ATTEMPT_ANNOTATION]: 'new-attempt' });
    expect(next.metadata.resourceVersion).toBe('42');
    expect(next.metadata.labels).toEqual(original.metadata.labels);
    expect(next.spec.provider?.overrides?.intent).toEqual({ hardware: { totalGpus: 2 }, searchStrategy: 'rapid' });
    expect(next.spec.model).toEqual({ ...original.spec.model, id: 'Qwen/Qwen3-8B' });
    expect(next.spec.engine.type).toBe('sglang');
    expect(next.spec.gateway).toEqual(original.spec.gateway);
    expect(next.spec.secrets).toEqual(original.spec.secrets);
    expect(original.metadata.annotations).toEqual({ keep: 'me' });
  });

  test('retry preserves inputs and creates a unique attempt', () => {
    const current = automaticDeployment();
    const a = reconfigureDynamoDeployment(current, { resourceVersion: '42' });
    const b = reconfigureDynamoDeployment(current, { resourceVersion: '42' });
    expect(a.spec).toEqual(current.spec);
    expect(a.metadata.annotations?.[DYNAMO_ATTEMPT_ANNOTATION]).not.toBe(b.metadata.annotations?.[DYNAMO_ATTEMPT_ANNOTATION]);
  });

  test('refuses stale revisions, invalid stored intent, and manual deployments', () => {
    expect(() => reconfigureDynamoDeployment(automaticDeployment(), { resourceVersion: '41' })).toThrow('Deployment changed');
    const manual = automaticDeployment();
    manual.spec.provider!.overrides = {};
    expect(() => reconfigureDynamoDeployment(manual, { resourceVersion: '42' })).toThrow('Only automatic');
    const custom = automaticDeployment();
    custom.spec.model.storage = { volumes: [{ name: 'cache', claimName: 'cache' }] };
    expect(() => reconfigureDynamoDeployment(custom, { resourceVersion: '42' })).toThrow('require manual configuration');
    const invalid = automaticDeployment();
    invalid.spec.provider!.overrides!.intent = { hardware: { totalGpus: 80 } };
    expect(() => reconfigureDynamoDeployment(invalid, { resourceVersion: '42' })).toThrow('valid typed intent');
  });
});
