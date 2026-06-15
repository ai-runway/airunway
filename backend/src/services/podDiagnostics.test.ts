import { describe, expect, test } from 'bun:test';
import type * as k8s from '@kubernetes/client-node';

import {
  getDeploymentPods,
  getPodFailureReasons,
  getPodLogs,
  selectLogContainer,
  type PodDiagnosticsAdapter,
} from './podDiagnostics';

interface PodInput {
  name: string;
  labels?: Record<string, string>;
  containers?: string[];
  readyContainers?: string[];
  phase?: string;
  nodeName?: string;
}

function pod(input: PodInput): k8s.V1Pod {
  const containers = (input.containers || ['main']).map(name => ({ name }));
  const readyContainers = new Set(input.readyContainers || input.containers || ['main']);
  return {
    metadata: { name: input.name, labels: input.labels },
    spec: { nodeName: input.nodeName, containers },
    status: {
      phase: input.phase || 'Running',
      containerStatuses: containers.map(container => ({
        name: container.name,
        ready: readyContainers.has(container.name),
        restartCount: 0,
        state: { running: {} },
      })),
    },
  } as k8s.V1Pod;
}

function adapter(overrides: Partial<PodDiagnosticsAdapter>): PodDiagnosticsAdapter {
  return {
    listPodsByLabelSelector: async () => [],
    listPodsByFieldSelector: async () => [],
    listEvents: async () => [],
    readPodLog: async () => '',
    logDebug: () => undefined,
    logError: () => undefined,
    getK8sStatusCode: (error) => (error as { statusCode?: number })?.statusCode,
    getK8sErrorMessage: (error) => (error as Error)?.message || String(error),
    ...overrides,
  };
}

describe('podDiagnostics Module', () => {
  test('aggregates and de-duplicates deployment pods across exact selectors and KubeRay fallback', async () => {
    const selectors: string[] = [];
    const pods = await getDeploymentPods(adapter({
      listPodsByLabelSelector: async ({ labelSelector }) => {
        selectors.push(labelSelector);
        if (labelSelector === 'app.kubernetes.io/instance=demo') {
          return [pod({ name: 'demo-router' }), pod({ name: 'demo-shared' })];
        }
        if (labelSelector === 'airunway.ai/deployment=demo') {
          return [pod({ name: 'demo-shared' }), pod({ name: 'demo-worker' })];
        }
        if (labelSelector === 'nvidia.com/dynamo-graph-deployment-name=demo') {
          return [pod({ name: 'demo-epp' })];
        }
        if (labelSelector === 'ray.io/cluster') {
          return [
            pod({ name: 'demo-ray-head', labels: { 'ray.io/cluster': 'demo-raycluster' } }),
            pod({ name: 'demo2-ray-head', labels: { 'ray.io/cluster': 'demo2-raycluster' } }),
            pod({ name: 'demo-extra-ray-head', labels: { 'ray.io/cluster': 'demo-extra-raycluster' } }),
          ];
        }
        return [];
      },
    }), 'demo', 'default');

    expect(pods.map(item => item.name)).toEqual([
      'demo-epp',
      'demo-ray-head',
      'demo-router',
      'demo-shared',
      'demo-worker',
    ]);
    expect(new Set(pods.map(item => item.name)).size).toBe(pods.length);
    expect(selectors).toEqual([
      'app.kubernetes.io/instance=demo',
      'airunway.ai/deployment=demo',
      'airunway.ai/model-deployment=demo',
      'nvidia.com/dynamo-graph-deployment-name=demo',
      'kaito.sh/workspace=demo',
      'ray.io/cluster',
    ]);
  });

  test('uses broad app selector only as a last-resort fallback', async () => {
    const selectors: string[] = [];
    const pods = await getDeploymentPods(adapter({
      listPodsByLabelSelector: async ({ labelSelector }) => {
        selectors.push(labelSelector);
        return labelSelector === 'app=legacy-demo'
          ? [pod({ name: 'legacy-demo-pod' })]
          : [];
      },
    }), 'legacy-demo', 'default');

    expect(pods.map(item => item.name)).toEqual(['legacy-demo-pod']);
    expect(selectors).toEqual([
      'app.kubernetes.io/instance=legacy-demo',
      'airunway.ai/deployment=legacy-demo',
      'airunway.ai/model-deployment=legacy-demo',
      'nvidia.com/dynamo-graph-deployment-name=legacy-demo',
      'kaito.sh/workspace=legacy-demo',
      'ray.io/cluster',
      'app=legacy-demo',
    ]);
  });

  test('parses warning events into resource-aware pending reasons', async () => {
    const reasons = await getPodFailureReasons(adapter({
      listEvents: async () => [
        { type: 'Normal', reason: 'Scheduled', message: 'scheduled' },
        {
          type: 'Warning',
          reason: 'FailedScheduling',
          message: '0/3 nodes are available: 3 Insufficient nvidia.com/gpu.',
        },
        {
          type: 'Warning',
          reason: 'FailedScheduling',
          message: '0/3 nodes are available: 3 node(s) had taint {nvidia.com/gpu: true}.',
        },
      ] as k8s.CoreV1Event[],
    }), 'demo-pod', 'default');

    expect(reasons).toEqual([
      {
        reason: 'FailedScheduling',
        message: '0/3 nodes are available: 3 Insufficient nvidia.com/gpu.',
        isResourceConstraint: true,
        resourceType: 'gpu',
        canAutoscalerHelp: true,
      },
      {
        reason: 'FailedScheduling',
        message: '0/3 nodes are available: 3 node(s) had taint {nvidia.com/gpu: true}.',
        isResourceConstraint: true,
        resourceType: 'gpu',
        canAutoscalerHelp: false,
      },
    ]);
  });

  test('selects preferred model containers before ready sidecars', () => {
    expect(selectLogContainer(pod({
      name: 'demo-model',
      containers: ['istio-proxy', 'vllm'],
      readyContainers: ['istio-proxy'],
    }))).toBe('vllm');
    expect(selectLogContainer(pod({
      name: 'demo-worker',
      containers: ['frontend', 'main'],
    }))).toBe('main');
    expect(selectLogContainer(pod({
      name: 'demo-generic',
      containers: ['sidecar', 'app'],
      readyContainers: ['app'],
    }))).toBe('app');
  });

  test('reads logs from the resolved container, strips ANSI color codes, and maps 404s', async () => {
    const fieldSelectors: string[] = [];
    const readRequests: Array<{ container?: string; tailLines: number; timestamps: boolean }> = [];
    const logs = await getPodLogs(adapter({
      listPodsByFieldSelector: async ({ fieldSelector }) => {
        fieldSelectors.push(fieldSelector);
        return [pod({ name: 'demo-worker', containers: ['frontend', 'main'] })];
      },
      readPodLog: async ({ container, tailLines, timestamps }) => {
        readRequests.push({ container, tailLines, timestamps });
        return '\x1b[32mworker logs\x1b[0m';
      },
    }), 'demo-worker', 'default', { tailLines: 20, timestamps: true });

    expect(logs).toBe('worker logs');
    expect(fieldSelectors).toEqual(['metadata.name=demo-worker']);
    expect(readRequests).toEqual([{ container: 'main', tailLines: 20, timestamps: true }]);

    await expect(getPodLogs(adapter({
      readPodLog: async () => { throw { statusCode: 404 }; },
    }), 'missing', 'default')).rejects.toThrow("Pod 'missing' not found in namespace 'default'");
  });
});
