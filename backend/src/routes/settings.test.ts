import { describe, test, expect, afterEach } from 'bun:test';
import app from '../hono-app';
import { kubernetesService } from '../services/kubernetes';
import { configService } from '../services/config';
import { mockServiceMethod } from '../test/helpers';
import { mockInferenceProviderConfig } from '../test/fixtures';
import { extractProviderInfo } from '../lib/providers';

describe('Settings Provider Routes', () => {
  const restores: Array<() => void> = [];

  afterEach(() => {
    restores.forEach((restore) => restore());
    restores.length = 0;
  });

  test('GET /api/settings includes providers', async () => {
    restores.push(
      mockServiceMethod(configService, 'getConfig', async () => ({ defaultNamespace: 'airunway-system' })),
      mockServiceMethod(kubernetesService, 'listInferenceProviderConfigs', async () => [mockInferenceProviderConfig]),
    );

    const res = await app.request('/api/settings');
    expect(res.status).toBe(200);

    const data = await res.json();
    expect(data.config).toBeDefined();
    expect(data.auth).toBeDefined();
    expect(data.providers).toHaveLength(1);
    expect(data.providers[0].id).toBe('kaito');
  });

  test('GET /api/settings/providers returns provider list', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'listInferenceProviderConfigs', async () => [mockInferenceProviderConfig]),
    );

    const res = await app.request('/api/settings/providers');
    expect(res.status).toBe(200);

    const data = await res.json();
    expect(data.providers).toHaveLength(1);
    expect(data.providers[0].id).toBe('kaito');
    expect(data.providers[0].name).toBe('KAITO');
    expect(data.providers[0].description).toBe('KAITO - Kubernetes AI Toolchain Operator');
  });


  test('extractProviderInfo reads provider capabilities from annotations', () => {
    const provider = extractProviderInfo({
      ...mockInferenceProviderConfig,
      spec: {
        capabilities: {
          engines: ['stale-engine'],
          servingModes: ['stale-mode'],
        },
      },
    });

    expect(provider.capabilities.engines).toEqual(['vllm', 'llamacpp']);
    expect(provider.capabilities.modes).toEqual(['aggregated']);
  });

  test('GET /api/settings/providers/:id returns provider details', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => mockInferenceProviderConfig),
    );

    const res = await app.request('/api/settings/providers/kaito');
    expect(res.status).toBe(200);

    const data = await res.json();
    expect(data.id).toBe('kaito');
    expect(data.name).toBe('KAITO');
    expect(data.crdConfig).toEqual({
      apiGroup: 'airunway.ai',
      apiVersion: 'v1alpha1',
      plural: 'inferenceproviderconfigs',
      kind: 'InferenceProviderConfig',
    });
    expect(data.helmCharts).toHaveLength(1);
    expect(data.installationSteps).toHaveLength(1);
  });

  test('GET /api/settings/providers/:id returns 404 for unknown provider', async () => {
    restores.push(
      mockServiceMethod(kubernetesService, 'getInferenceProviderConfig', async () => null),
    );

    const res = await app.request('/api/settings/providers/unknown');
    expect(res.status).toBe(404);
  });
});
