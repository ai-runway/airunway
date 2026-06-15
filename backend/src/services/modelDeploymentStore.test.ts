import { describe, expect, test } from 'bun:test';
import type {
  DeploymentConfig,
  DeploymentStatus,
  ModelDeployment,
  PodStatus,
} from '@airunway/shared';

import {
  createDeployment,
  deleteDeployment,
  getDeployment,
  getDeploymentManifest,
  listDeployments,
  type ModelDeploymentStoreAdapter,
} from './modelDeploymentStore';

function modelDeployment(input: {
  name: string;
  namespace?: string;
  createdAt: string;
  modelId?: string;
}): ModelDeployment {
  return {
    apiVersion: 'airunway.ai/v1alpha1',
    kind: 'ModelDeployment',
    metadata: {
      name: input.name,
      namespace: input.namespace,
      creationTimestamp: input.createdAt,
    },
    spec: {
      model: { id: input.modelId || `${input.name}-model` },
      engine: { type: 'vllm' },
      provider: { name: 'dynamo' },
    },
    status: {
      phase: 'Running',
      provider: { name: 'dynamo' },
      endpoint: { service: `${input.name}-frontend`, port: 8000 },
    },
  } as ModelDeployment;
}

function adapter(overrides: Partial<ModelDeploymentStoreAdapter>): ModelDeploymentStoreAdapter {
  return {
    listClusterModelDeployments: async () => ({ items: [] }),
    listNamespacedModelDeployments: async () => ({ items: [] }),
    getModelDeployment: async () => { throw { statusCode: 404 }; },
    createModelDeployment: async () => undefined,
    deleteModelDeployment: async () => undefined,
    listNamespaces: async () => [],
    canListModelDeployments: async () => false,
    getDeploymentPods: async () => [],
    getDeploymentStatus: async () => null,
    logDebug: () => undefined,
    logInfo: () => undefined,
    logError: () => undefined,
    getK8sStatusCode: error => (error as { statusCode?: number })?.statusCode,
    getK8sErrorMessage: error => (error as Error)?.message || String(error),
    ...overrides,
  };
}

describe('modelDeploymentStore Module', () => {
  test('lists cluster-wide deployments with pods and sorts newest first', async () => {
    const podCalls: Array<{ name: string; namespace: string }> = [];
    const deployments = await listDeployments(adapter({
      listClusterModelDeployments: async () => ({
        items: [
          modelDeployment({ name: 'old', namespace: 'team-a', createdAt: '2026-01-01T00:00:00.000Z' }),
          modelDeployment({ name: 'new', namespace: 'team-b', createdAt: '2026-02-01T00:00:00.000Z' }),
        ],
      }),
      getDeploymentPods: async (name, namespace) => {
        podCalls.push({ name, namespace });
        return [{ name: `${name}-pod`, phase: 'Running', ready: true, restarts: 0 } as PodStatus];
      },
    }));

    expect(deployments.map(item => item.name)).toEqual(['new', 'old']);
    expect(deployments[0].pods.map(pod => pod.name)).toEqual(['new-pod']);
    expect(podCalls).toEqual([
      { name: 'old', namespace: 'team-a' },
      { name: 'new', namespace: 'team-b' },
    ]);
  });

  test('falls back to allowed namespace listing after cluster-wide RBAC denial', async () => {
    const listedNamespaces: string[] = [];
    const deployments = await listDeployments(adapter({
      listClusterModelDeployments: async () => { throw { statusCode: 403, message: 'forbidden' }; },
      listNamespaces: async () => ['team-a', 'team-b', 'team-c'],
      canListModelDeployments: async namespace => namespace !== 'team-b',
      listNamespacedModelDeployments: async (namespace) => {
        listedNamespaces.push(namespace);
        return { items: [modelDeployment({ name: namespace, namespace, createdAt: `2026-01-0${namespace === 'team-a' ? '1' : '3'}T00:00:00.000Z` })] };
      },
    }), undefined, 'user-token');

    expect(listedNamespaces.sort()).toEqual(['team-a', 'team-c']);
    expect(deployments.map(item => item.name)).toEqual(['team-c', 'team-a']);
  });

  test('gets deployment status with pods and returns null for 404s', async () => {
    const found = await getDeployment(adapter({
      getModelDeployment: async () => modelDeployment({ name: 'demo', namespace: 'default', createdAt: '2026-01-01T00:00:00.000Z' }),
      getDeploymentPods: async () => [{ name: 'demo-pod', phase: 'Running', ready: true, restarts: 0 } as PodStatus],
    }), 'demo', 'default');

    expect(found?.name).toBe('demo');
    expect(found?.pods.map(pod => pod.name)).toEqual(['demo-pod']);

    const missing = await getDeployment(adapter({
      getModelDeployment: async () => { throw { statusCode: 404 }; },
    }), 'missing', 'default');
    expect(missing).toBeNull();
  });

  test('gets raw manifests and maps missing manifests to null', async () => {
    const manifest = await getDeploymentManifest(adapter({
      getModelDeployment: async () => ({ kind: 'ModelDeployment', metadata: { name: 'demo' } }),
    }), 'demo', 'default');

    expect(manifest).toEqual({ kind: 'ModelDeployment', metadata: { name: 'demo' } });

    const missing = await getDeploymentManifest(adapter({
      getModelDeployment: async () => { throw { statusCode: 404 }; },
    }), 'missing', 'default');
    expect(missing).toBeNull();
  });

  test('creates manifests from deployment config and deletes only after status lookup succeeds', async () => {
    let created: { namespace: string; manifest: Record<string, unknown> } | undefined;
    let deleted: { name: string; namespace: string } | undefined;
    const config: DeploymentConfig = {
      name: 'demo',
      namespace: 'default',
      modelId: 'Qwen/Qwen3-0.6B',
      engine: 'vllm',
      mode: 'aggregated',
      routerMode: 'default',
      replicas: 1,
      enforceEager: false,
      enablePrefixCaching: true,
      trustRemoteCode: false,
    };

    const store = adapter({
      createModelDeployment: async (namespace, manifest) => {
        created = { namespace, manifest };
      },
      getDeploymentStatus: async () => ({ name: 'demo' } as DeploymentStatus),
      deleteModelDeployment: async (name, namespace) => {
        deleted = { name, namespace };
      },
    });

    await createDeployment(store, config);
    expect(created?.namespace).toBe('default');
    expect(created?.manifest.metadata).toMatchObject({ name: 'demo', namespace: 'default' });
    expect((created?.manifest.spec as { model: { id: string } }).model.id).toBe('Qwen/Qwen3-0.6B');

    await deleteDeployment(store, 'demo', 'default');
    expect(deleted).toEqual({ name: 'demo', namespace: 'default' });

    await expect(deleteDeployment(adapter({
      getDeploymentStatus: async () => null,
    }), 'missing', 'default')).rejects.toThrow("Deployment 'missing' not found in namespace 'default'");
  });
});
