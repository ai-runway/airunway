import { afterEach, describe, expect, test } from 'bun:test';
import app from '../hono-app';
import { kubernetesService } from '../services/kubernetes';
import { mockServiceMethod } from '../test/helpers';
import { defaultDynamoIntent, toDeploymentStatus, type ModelDeployment } from '@airunway/shared';

const restores: Array<() => void> = [];
afterEach(() => { restores.reverse().forEach(restore => restore()); restores.length = 0; });
const request = (path: string, body: unknown) => app.request(`/api/deployments${path}`, {
  method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
});
const body = () => ({ name: 'qwen-auto', namespace: 'default', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm', provider: 'dynamo',
  providerOverrides: { deploymentMode: 'intent', intent: defaultDynamoIntent() } });
const current = (): ModelDeployment => ({ apiVersion: 'airunway.ai/v1alpha1', kind: 'ModelDeployment',
  metadata: { name: 'qwen-auto', namespace: 'default', resourceVersion: '1', annotations: { keep: 'yes' } },
  spec: { model: { id: 'Qwen/Qwen3-0.6B', source: 'huggingface' }, engine: { type: 'vllm' }, provider: { name: 'dynamo', overrides: body().providerOverrides } } });

describe('Dynamo automatic API', () => {
  test('preview omits manual sizing and layout', async () => {
    restores.push(mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => { throw new Error('offline'); }));
    const response = await request('/preview', body());
    expect(response.status).toBe(200);
    const result = await response.json() as { resources: Array<{ manifest: ModelDeployment }> };
    const spec = result.resources[0].manifest.spec;
    expect(spec.provider.overrides).toEqual(body().providerOverrides);
    expect(spec.resources).toBeUndefined();
    expect(spec.scaling).toBeUndefined();
    expect(spec.serving).toBeUndefined();
  });

  test('rejects conflicting sizing, unknown fields, unsupported modes and search methods', async () => {
    for (const change of [
      { resources: { gpu: 1 } }, { replicas: 1 }, { scaling: { replicas: 1 } }, { podTemplate: { labels: { foo: 'bar' } } }, { nodeSelector: { gpu: 'h100' } }, { mode: 'disaggregated' }, { prefillGpus: 1 },
      { provider: 'vllm' }, { env: { FOO: 'bar' } }, { hfTokenSecret: 'other-secret' }, { storage: { volumes: [{ name: 'cache', claimName: 'cache' }] } }, { enforceEager: true }, { imageRef: 'custom/image' },
      { providerOverrides: { ...body().providerOverrides, autoApply: false } },
      { providerOverrides: { deploymentMode: 'manual', intent: defaultDynamoIntent() } },
      { providerOverrides: { ...body().providerOverrides, intent: { ...defaultDynamoIntent(), searchStrategy: 'thorough' } } },
    ]) expect((await request('/preview', { ...body(), ...change })).status).toBe(400);
  });

  test('reconfigure sends exactly one conditional update with fresh attempt and preserves metadata', async () => {
    const writes: ModelDeployment[] = [];
    restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => current() as unknown as Record<string, unknown>));
    restores.push(mockServiceMethod(kubernetesService, 'replaceDeployment', async (md: ModelDeployment) => { writes.push(md); }));
    const response = await request('/default/qwen-auto/reconfigure', { resourceVersion: '1', modelId: 'Qwen/Qwen3-8B', engine: 'sglang', intent: { hardware: { totalGpus: 2 } } });
    expect(response.status).toBe(200);
    expect(writes).toHaveLength(1);
    expect(writes[0].metadata.resourceVersion).toBe('1');
    expect(writes[0].metadata.annotations?.keep).toBe('yes');
    expect(writes[0].metadata.annotations?.['airunway.ai/dynamo-attempt']).toMatch(/^[a-f0-9-]{36}$/);
    expect(writes[0].spec.engine.type).toBe('sglang');
    expect(writes[0].spec.model.id).toBe('Qwen/Qwen3-8B');
    expect(writes[0].spec.model.source).toBe('huggingface');
  });

  test('rejects stale clients before write, and surfaces API-server conflicts without retry', async () => {
    let writes = 0;
    restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => current() as unknown as Record<string, unknown>));
    restores.push(mockServiceMethod(kubernetesService, 'replaceDeployment', async () => { writes++; throw { statusCode: 409, message: 'Conflict' }; }));
    expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: 'old' })).status).toBe(409);
    expect(writes).toBe(0);
    expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1' })).status).toBe(409);
    expect(writes).toBe(1);
  });

  test('rejects manual reconfiguration and arbitrary spec replacement', async () => {
    const md = current(); md.spec.provider!.overrides = { deploymentMode: 'manual' };
    restores.push(mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => md as unknown as Record<string, unknown>));
    expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1' })).status).toBe(422);
    expect((await request('/default/qwen-auto/reconfigure', { resourceVersion: '1', spec: {} })).status).toBe(400);
    expect((await request('/default/qwen-auto/reconfigure', {})).status).toBe(400);
  });
  test('chat discovers and calls the published Service in its actual namespace', async () => {
    const md = current();
    md.status = { phase: 'Running', endpoint: { service: 'chosen-frontend', port: 9000 }, replicas: { desired: 1, ready: 1, available: 1 }, provider: { workloadRef: { kind: 'DynamoGraphDeployment', name: 'chosen', namespace: 'serving' } } };
    const calls: unknown[][] = [];
    restores.push(mockServiceMethod(kubernetesService, 'getDeployment', async () => toDeploymentStatus(md)));
    restores.push(mockServiceMethod(kubernetesService, 'proxyServiceGet', async (...args: unknown[]) => { calls.push(args); return JSON.stringify({ data: [{ id: 'actual-model' }] }); }));
    restores.push(mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
      calls.push(args); return new Response('data: [DONE]\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
    }));
    const response = await request('/default/qwen-auto/chat', { messages: [{ role: 'user', content: 'Hello' }] });
    expect(response.status).toBe(200);
    await response.text();
    expect(calls).toHaveLength(2);
    expect(calls.every(call => call[0] === 'chosen-frontend' && call[1] === 'serving' && call[2] === 9000)).toBe(true);
    expect(calls[1][4]).toMatchObject({ model: 'actual-model' });
  });

});
