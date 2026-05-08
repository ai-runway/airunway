import { describe, test, expect, afterEach } from 'bun:test';
import app from '../hono-app';
import { kubernetesService } from '../services/kubernetes';
import { configService } from '../services/config';
import { metricsService } from '../services/metrics';
import { mockServiceMethod } from '../test/helpers';
import type { MetricsResponse } from '@airunway/shared';
import {
  mockDeployment,
  mockDeploymentWithPendingPod,
  mockDeploymentManifest,
  mockPodFailureReasons,
} from '../test/fixtures';

// Base valid deployment body for reuse in tests
const validDeploymentBody = {
  name: 'test-deploy',
  modelId: 'meta-llama/Llama-3.1-8B-Instruct',
  engine: 'vllm',
  namespace: 'default',
  mode: 'aggregated',
  routerMode: 'default',
  replicas: 1,
  enforceEager: false,
  enablePrefixCaching: false,
  trustRemoteCode: false,
};

describe('Deployment Routes', () => {
  const restores: Array<() => void> = [];

  afterEach(() => {
    restores.forEach((r) => r());
    restores.length = 0;
  });

  describe('POST /api/deployments/preview', () => {
    test('returns manifest without storage', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(validDeploymentBody),
      });
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.resources).toBeArray();
      expect(data.resources.length).toBe(1);
      expect(data.resources[0].kind).toBe('ModelDeployment');
      expect(data.resources[0].name).toBe('test-deploy');
      expect(data.primaryResource.kind).toBe('ModelDeployment');
      expect(data.primaryResource.apiVersion).toBe('airunway.ai/v1alpha1');
      // No storage in manifest
      expect(data.resources[0].manifest.spec.model.storage).toBeUndefined();
    });

    test('returns manifest with storage volumes', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );

      const bodyWithStorage = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            {
              name: 'model-cache',
              purpose: 'modelCache',
              mountPath: '/model-cache',
              size: '100Gi',
              accessMode: 'ReadWriteMany',
            },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(bodyWithStorage),
      });
      expect(res.status).toBe(200);

      const data = await res.json();
      const spec = data.resources[0].manifest.spec;
      expect(spec.model.storage).toBeDefined();
      expect(spec.model.storage.volumes).toBeArray();
      expect(spec.model.storage.volumes.length).toBe(1);
      expect(spec.model.storage.volumes[0].name).toBe('model-cache');
      expect(spec.model.storage.volumes[0].size).toBe('100Gi');
    });

    test('normalizes KAITO GGUF deployments to llamacpp in preview manifests', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'kaito-workspace'),
      );

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          ...validDeploymentBody,
          namespace: 'kaito-workspace',
          provider: 'kaito',
          modelId: 'nvidia/Nemotron-3-Nano-4B-gguf',
          modelSource: 'huggingface',
          ggufFile: 'nvidia-nemotron-3-nano-4b.Q4_K_M.gguf',
          ggufRunMode: 'direct',
        }),
      });

      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.resources[0].manifest.spec.engine.type).toBe('llamacpp');
      expect(data.resources[0].manifest.spec.engine.args.ggufUrl).toBe(
        'https://huggingface.co/nvidia/Nemotron-3-Nano-4B-gguf/resolve/main/nvidia-nemotron-3-nano-4b.Q4_K_M.gguf'
      );
      expect(data.resources[0].manifest.spec.image).toBe('ghcr.io/kaito-project/aikit/runners/llama-cpp-cuda:latest');
      expect(data.resources[0].manifest.spec.provider.name).toBe('kaito');
    });

    test('normalizes KAITO premade deployments to llamacpp in preview manifests', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'kaito-workspace'),
      );

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          ...validDeploymentBody,
          namespace: 'kaito-workspace',
          provider: 'kaito',
          modelId: 'llama3.2:3b',
          modelSource: 'premade',
          premadeModel: 'llama3.2:3b',
        }),
      });

      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.resources[0].manifest.spec.engine.type).toBe('llamacpp');
      expect(data.resources[0].manifest.spec.image).toBe('ghcr.io/kaito-project/aikit/llama3.2:3b');
      expect(data.resources[0].manifest.spec.provider.name).toBe('kaito');
    });

    test('stringifies numeric engine args in preview manifests', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'dynamo-system'),
      );

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          ...validDeploymentBody,
          namespace: 'dynamo-system',
          provider: 'dynamo',
          engineArgs: {
            'tensor-parallel-size': 8,
            'pipeline-parallel-size': 3,
            'gpu-memory-utilization': 0.95,
            'enable-chunked-prefill': true,
          },
        }),
      });

      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.resources[0].manifest.spec.engine.args).toEqual({
        'tensor-parallel-size': '8',
        'pipeline-parallel-size': '3',
        'gpu-memory-utilization': '0.95',
        'enable-chunked-prefill': 'true',
      });
    });

    test('omits GPU resources for KAITO CPU preview manifests', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'kaito-workspace'),
      );

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          ...validDeploymentBody,
          namespace: 'kaito-workspace',
          provider: 'kaito',
          engine: 'llamacpp',
          modelId: 'unsloth/NVIDIA-Nemotron-3-Nano-4B-GGUF',
          modelSource: 'huggingface',
          ggufFile: 'NVIDIA-Nemotron-3-Nano-4B-Q4_K_M.gguf',
          ggufRunMode: 'direct',
          computeType: 'cpu',
          resources: { gpu: 1 },
        }),
      });

      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.resources[0].manifest.spec.resources).toBeUndefined();
      expect(data.resources[0].manifest.spec.engine.type).toBe('llamacpp');
    });
  });

  describe('POST /api/deployments - storage validation', () => {
    test('succeeds with valid storage config', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'createDeployment', async () => undefined),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getClusterGpuCapacity', async () => ({
          totalGpus: 8,
          availableGpus: 4,
          maxContiguousAvailable: 4,
          nodes: [],
        })),
      );

      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            {
              name: 'cache-vol',
              purpose: 'modelCache',
              mountPath: '/model-cache',
              size: '50Gi',
              accessMode: 'ReadWriteMany',
            },
          ],
        },
      };

      const res = await app.request('/api/deployments', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(201);
    });

    test('rejects duplicate volume names', async () => {
      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            { name: 'same-name', purpose: 'custom', mountPath: '/data1', claimName: 'pvc-1' },
            { name: 'same-name', purpose: 'custom', mountPath: '/data2', claimName: 'pvc-2' },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });

    test('rejects missing claimName when no size', async () => {
      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            { name: 'my-vol', purpose: 'custom', mountPath: '/data' },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });

    test('rejects readOnly with size set', async () => {
      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            { name: 'my-vol', purpose: 'custom', mountPath: '/data', size: '10Gi', readOnly: true },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });

    test('rejects custom purpose without mountPath', async () => {
      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            { name: 'my-vol', purpose: 'custom', claimName: 'existing-pvc' },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });

    test('rejects system path mountPath', async () => {
      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            { name: 'my-vol', purpose: 'custom', mountPath: '/proc/data', claimName: 'pvc-1' },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });

    test('rejects more than 8 volumes', async () => {
      const volumes = Array.from({ length: 9 }, (_, i) => ({
        name: `vol-${i}`,
        purpose: 'custom' as const,
        mountPath: `/data-${i}`,
        claimName: `pvc-${i}`,
      }));

      const body = {
        ...validDeploymentBody,
        storage: { volumes },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });

    test('rejects accessMode without size', async () => {
      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            { name: 'my-vol', purpose: 'custom', mountPath: '/data', claimName: 'pvc-1', accessMode: 'ReadWriteOnce' },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });

    test('rejects invalid PVC size format', async () => {
      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            { name: 'my-vol', purpose: 'custom', mountPath: '/data', size: '100GIB' },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });

    test('accepts valid Kubernetes quantity formats for size', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );

      for (const size of ['100Gi', '500Mi', '1Ti', '1024', '0.5', '1e3', '100M', '50Ki']) {
        const body = {
          ...validDeploymentBody,
          storage: {
            volumes: [
              { name: 'my-vol', purpose: 'custom', mountPath: '/data', size },
            ],
          },
        };

        const res = await app.request('/api/deployments/preview', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        expect(res.status).toBe(200);
      }
    });

    test('rejects malformed quantity strings like bare dot or double dots', async () => {
      for (const size of ['.', '1..2', '..5Gi', '.Gi']) {
        const body = {
          ...validDeploymentBody,
          storage: {
            volumes: [
              { name: 'my-vol', purpose: 'custom', mountPath: '/data', size },
            ],
          },
        };

        const res = await app.request('/api/deployments/preview', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        expect(res.status).toBe(400);
      }
    });

    test('rejects defaulted mount path collision with explicit mount path', async () => {
      // modelCache defaults to /model-cache; another volume explicitly uses /model-cache
      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            { name: 'cache', purpose: 'modelCache', size: '50Gi' },
            { name: 'other', purpose: 'custom', mountPath: '/model-cache', claimName: 'other-pvc' },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });

    test('rejects defaulted claim name collision with explicit claim name', async () => {
      // Managed vol {name:'cache', size:'10Gi'} defaults claimName to test-deploy-cache;
      // another volume explicitly uses claimName: test-deploy-cache
      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            { name: 'cache', purpose: 'modelCache', mountPath: '/model-cache', size: '10Gi' },
            { name: 'other', purpose: 'custom', mountPath: '/other', claimName: 'test-deploy-cache' },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(400);
    });
  });

  describe('POST /api/deployments/preview - admission defaults', () => {
    test('applies default claimName and accessMode for controller-created PVCs', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );

      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            {
              name: 'cache',
              purpose: 'modelCache',
              size: '100Gi',
              // Omit claimName and accessMode — webhook would default them
            },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(200);

      const data = await res.json();
      const vol = data.resources[0].manifest.spec.model.storage.volumes[0];
      expect(vol.claimName).toBe('test-deploy-cache');
      expect(vol.accessMode).toBe('ReadWriteMany');
      expect(vol.mountPath).toBe('/model-cache');
    });

    test('applies default mountPath for compilationCache purpose', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );

      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            {
              name: 'compile',
              purpose: 'compilationCache',
              size: '50Gi',
            },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(200);

      const data = await res.json();
      const vol = data.resources[0].manifest.spec.model.storage.volumes[0];
      expect(vol.mountPath).toBe('/compilation-cache');
      expect(vol.claimName).toBe('test-deploy-compile');
      expect(vol.accessMode).toBe('ReadWriteMany');
    });

    test('does not overwrite explicitly set fields', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );

      const body = {
        ...validDeploymentBody,
        storage: {
          volumes: [
            {
              name: 'cache',
              purpose: 'modelCache',
              mountPath: '/custom/path',
              size: '100Gi',
              claimName: 'test-deploy-cache',
              accessMode: 'ReadWriteOnce',
            },
          ],
        },
      };

      const res = await app.request('/api/deployments/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      expect(res.status).toBe(200);

      const data = await res.json();
      const vol = data.resources[0].manifest.spec.model.storage.volumes[0];
      // Explicit values preserved, not overwritten by defaults
      expect(vol.mountPath).toBe('/custom/path');
      expect(vol.accessMode).toBe('ReadWriteOnce');
    });
  });

  describe('GET /api/deployments/:name/manifest', () => {
    test('returns manifest with resources and primaryResource', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => mockDeploymentManifest),
      );

      const res = await app.request('/api/deployments/test-deploy/manifest');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.resources).toBeArray();
      expect(data.resources.length).toBeGreaterThan(0);
      expect(data.primaryResource).toBeDefined();
      expect(data.primaryResource.kind).toBe('ModelDeployment');
      expect(data.primaryResource.apiVersion).toBe('airunway.ai/v1alpha1');
    });

    test('returns 404 when manifest not found', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeploymentManifest', async () => null),
      );

      const res = await app.request('/api/deployments/test-deploy/manifest');
      expect(res.status).toBe(404);
    });
  });

  describe('GET /api/deployments/:name/metrics', () => {
    test('passes frontend service endpoint details to the metrics service', async () => {
      let capturedArgs: Parameters<typeof metricsService.getDeploymentMetrics> | undefined;

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          provider: 'dynamo',
          frontendService: 'test-deploy-frontend:8080',
        })),
      );
      restores.push(
        mockServiceMethod(metricsService, 'getDeploymentMetrics', async (...args) => {
          capturedArgs = args;
          return {
            available: true,
            timestamp: '2026-05-01T00:00:00.000Z',
            metrics: [],
          } satisfies MetricsResponse;
        }),
      );

      const res = await app.request('/api/deployments/test-deploy/metrics');
      expect(res.status).toBe(200);

      expect(capturedArgs).toEqual([
        'test-deploy',
        'default',
        {
          providerId: 'dynamo',
          serviceName: 'test-deploy-frontend',
          port: 8080,
        },
      ]);
    });
  });

  describe('GET /api/deployments/:name/pending-reasons', () => {
    test('returns failure reasons for pending pods', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => mockDeploymentWithPendingPod),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getPodFailureReasons', async () => mockPodFailureReasons),
      );

      const res = await app.request('/api/deployments/pending-deploy/pending-reasons');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.reasons).toBeArray();
      expect(data.reasons.length).toBeGreaterThan(0);
      expect(data.reasons[0].reason).toBe('Insufficient nvidia.com/gpu');
    });

    test('returns empty reasons when no pending pods', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => mockDeployment),
      );

      const res = await app.request('/api/deployments/test-deploy/pending-reasons');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.reasons).toEqual([]);
    });

    test('returns 404 when deployment not found', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => null),
      );

      const res = await app.request('/api/deployments/nonexistent/pending-reasons');
      expect(res.status).toBe(404);
    });
  });

  describe('GET /api/deployments/:name/logs', () => {
    test('returns logs for deployment', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => mockDeployment),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeploymentPods', async () => [{ name: 'test-deploy-abc123' }]),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getPodLogs', async () => 'log line 1\nlog line 2'),
      );

      const res = await app.request('/api/deployments/test-deploy/logs');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.logs).toBe('log line 1\nlog line 2');
      expect(data.podName).toBe('test-deploy-abc123');
    });

    test('returns empty logs when no pods found', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => mockDeployment),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeploymentPods', async () => []),
      );

      const res = await app.request('/api/deployments/test-deploy/logs');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.logs).toBe('');
      expect(data.message).toBeDefined();
    });

    test('returns 400 when specified pod not in deployment', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => mockDeployment),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeploymentPods', async () => [{ name: 'test-deploy-abc123' }]),
      );

      const res = await app.request('/api/deployments/test-deploy/logs?podName=wrong-pod');
      expect(res.status).toBe(400);
    });
  });

  describe('POST /api/deployments', () => {
    test('resolves direct KAITO GGUF deployments to the runner image', async () => {
      let capturedConfig: any;

      restores.push(
        mockServiceMethod(kubernetesService, 'createDeployment', async (config) => {
          capturedConfig = config;
          return undefined;
        }),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getClusterGpuCapacity', async () => ({
          totalGpus: 8,
          allocatedGpus: 0,
          availableGpus: 8,
          maxContiguousAvailable: 8,
          nodes: [],
        })),
      );
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'kaito-workspace'),
      );

      const res = await app.request('/api/deployments', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'nemotron-direct',
          namespace: 'kaito-workspace',
          provider: 'kaito',
          modelId: 'unsloth/NVIDIA-Nemotron-3-Nano-4B-GGUF',
          engine: 'llamacpp',
          modelSource: 'huggingface',
          ggufFile: 'NVIDIA-Nemotron-3-Nano-4B-Q4_K_M.gguf',
          ggufRunMode: 'direct',
          resources: { gpu: 1 },
        }),
      });

      expect(res.status).toBe(201);
      expect(capturedConfig.imageRef).toBe('ghcr.io/kaito-project/aikit/runners/llama-cpp-cuda:latest');
      expect(capturedConfig.engineArgs?.ggufUrl).toBe(
        'https://huggingface.co/unsloth/NVIDIA-Nemotron-3-Nano-4B-GGUF/resolve/main/NVIDIA-Nemotron-3-Nano-4B-Q4_K_M.gguf'
      );
    });

    test('resolves premade KAITO deployments to the premade image', async () => {
      let capturedConfig: any;

      restores.push(
        mockServiceMethod(kubernetesService, 'createDeployment', async (config) => {
          capturedConfig = config;
          return undefined;
        }),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getClusterGpuCapacity', async () => ({
          totalGpus: 8,
          allocatedGpus: 0,
          availableGpus: 8,
          maxContiguousAvailable: 8,
          nodes: [],
        })),
      );
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'kaito-workspace'),
      );

      const res = await app.request('/api/deployments', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'llama-premade',
          namespace: 'kaito-workspace',
          provider: 'kaito',
          modelId: 'llama3.2:3b',
          engine: 'llamacpp',
          modelSource: 'premade',
          premadeModel: 'llama3.2:3b',
        }),
      });

      expect(res.status).toBe(201);
      expect(capturedConfig.imageRef).toBe('ghcr.io/kaito-project/aikit/llama3.2:3b');
    });

    test('accepts deployment with providerOverrides', async () => {
      restores.push(
        mockServiceMethod(kubernetesService, 'createDeployment', async () => undefined),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getClusterGpuCapacity', async () => ({
          totalGpus: 16,
          allocatedGpus: 0,
          availableGpus: 16,
          maxContiguousAvailable: 8,
          nodes: [],
        })),
      );
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );

      const res = await app.request('/api/deployments', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'test-multinode',
          modelId: 'Qwen/Qwen2.5-72B',
          engine: 'vllm',
          provider: 'dynamo',
          resources: { gpu: 1 },
          providerOverrides: {
            spec: {
              services: {
                VllmWorker: {
                  multinode: { nodeCount: 2 }
                }
              }
            }
          },
          engineArgs: {
            'tensor-parallel-size': '1',
          },
        }),
      });

      expect(res.status).toBe(201);
      const data = await res.json();
      expect(data.name).toBe('test-multinode');
    });
  });
});
