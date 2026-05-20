import { describe, test, expect, afterEach } from 'bun:test';
import app from '../hono-app';
import { kubernetesService } from '../services/kubernetes';
import { helmService } from '../services/helm';
import { mockServiceMethod } from '../test/helpers';
import {
  mockGpuCapacity,
  mockDetailedGpuCapacity,
  mockGpuOperatorStatus,
  mockHelmAvailable,
  mockHelmUnavailable,
  mockProviderInstallResult,
  mockProviderUninstallResult,
  mockInferenceProviderConfig,
  mockInferenceProviderConfigNotReady,
} from '../test/fixtures';

describe('Provider Installation Flow', () => {
  const restores: (() => void)[] = [];

  afterEach(() => {
    // Restore in LIFO order so nested mocks of the same method unwind correctly
    restores.reverse().forEach((r) => r());
    restores.length = 0;
  });

  test('full flow: check GPU → check provider → install → verify → uninstall → verify removed', async () => {
    // ---- Step 1: Check GPU capacity ----
    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getClusterGpuCapacity',
        (async () => mockGpuCapacity) as typeof kubernetesService.getClusterGpuCapacity,
      ),
    );

    const gpuRes = await app.request('/api/installation/gpu-capacity');
    expect(gpuRes.status).toBe(200);
    const gpuData = await gpuRes.json();
    expect(gpuData.totalGpus).toBe(4);
    expect(gpuData.availableGpus).toBe(4);
    expect(gpuData.gpuNodeCount).toBe(1);

    // ---- Step 2: Check GPU operator status ----
    restores.push(
      mockServiceMethod(
        kubernetesService,
        'checkGPUOperatorStatus',
        (async () => mockGpuOperatorStatus) as typeof kubernetesService.checkGPUOperatorStatus,
      ),
    );
    restores.push(
      mockServiceMethod(
        helmService,
        'getGpuOperatorCommands',
        (() => ['helm install gpu-operator nvidia/gpu-operator']) as typeof helmService.getGpuOperatorCommands,
      ),
    );

    const gpuOpRes = await app.request('/api/installation/gpu-operator/status');
    expect(gpuOpRes.status).toBe(200);
    const gpuOpData = await gpuOpRes.json();
    expect(gpuOpData.installed).toBe(true);
    expect(gpuOpData.operatorRunning).toBe(true);
    expect(gpuOpData.gpusAvailable).toBe(true);

    // ---- Step 3: Check provider status (not yet ready) ----
    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getInferenceProviderConfig',
        (async () => mockInferenceProviderConfigNotReady) as typeof kubernetesService.getInferenceProviderConfig,
      ),
    );
    restores.push(
      mockServiceMethod(
        helmService,
        'getInstallCommands',
        (() => ['helm repo add kaito ...', 'helm install kaito-workspace ...']) as typeof helmService.getInstallCommands,
      ),
      mockServiceMethod(
        kubernetesService,
        'checkKaitoInstallationStatus',
        (async () => ({
          installed: false,
          crdFound: false,
          operatorRunning: false,
          message: 'KAITO workspace CRD not found',
        })) as typeof kubernetesService.checkKaitoInstallationStatus,
      ),
    );

    const statusRes1 = await app.request('/api/installation/providers/kaito/status');
    expect(statusRes1.status).toBe(200);
    const statusData1 = await statusRes1.json();
    expect(statusData1.providerId).toBe('kaito');
    expect(statusData1.installed).toBe(false);

    // ---- Step 4: Get install commands ----
    const cmdRes = await app.request('/api/installation/providers/kaito/commands');
    expect(cmdRes.status).toBe(200);
    const cmdData = await cmdRes.json();
    expect(cmdData.providerId).toBe('kaito');
    expect(cmdData.commands).toBeDefined();
    expect(cmdData.steps).toBeDefined();

    // ---- Step 5: Check Helm availability ----
    restores.push(
      mockServiceMethod(
        helmService,
        'checkHelmAvailable',
        (async () => mockHelmAvailable) as typeof helmService.checkHelmAvailable,
      ),
    );

    const helmRes = await app.request('/api/installation/helm/status');
    expect(helmRes.status).toBe(200);
    const helmData = await helmRes.json();
    expect(helmData.available).toBe(true);

    // ---- Step 6: Install provider ----
    restores.push(
      mockServiceMethod(
        helmService,
        'installProvider',
        (async () => mockProviderInstallResult) as typeof helmService.installProvider,
      ),
    );

    const installRes = await app.request('/api/installation/providers/kaito/install', {
      method: 'POST',
    });
    expect(installRes.status).toBe(200);
    const installData = await installRes.json();
    expect(installData.success).toBe(true);
    expect(installData.results).toHaveLength(3);

    // ---- Step 7: Verify provider is now ready ----
    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getInferenceProviderConfig',
        (async () => mockInferenceProviderConfig) as typeof kubernetesService.getInferenceProviderConfig,
      ),
      mockServiceMethod(
        kubernetesService,
        'checkKaitoInstallationStatus',
        (async () => ({
          installed: true,
          crdFound: true,
          operatorRunning: true,
          message: 'KAITO workspace CRD found and KAITO operator pods are ready',
        })) as typeof kubernetesService.checkKaitoInstallationStatus,
      ),
    );

    const statusRes2 = await app.request('/api/installation/providers/kaito/status');
    expect(statusRes2.status).toBe(200);
    const statusData2 = await statusRes2.json();
    expect(statusData2.providerId).toBe('kaito');
    expect(statusData2.installed).toBe(true);
    expect(statusData2.operatorRunning).toBe(true);

    // ---- Step 8: Uninstall provider ----
    restores.push(
      mockServiceMethod(
        helmService,
        'uninstall',
        (async () => mockProviderUninstallResult) as typeof helmService.uninstall,
      ),
    );

    const uninstallRes = await app.request('/api/installation/providers/kaito/uninstall', {
      method: 'POST',
    });
    expect(uninstallRes.status).toBe(200);
    const uninstallData = await uninstallRes.json();
    expect(uninstallData.success).toBe(true);
    expect(typeof uninstallData.message).toBe('string');
    expect(uninstallData.results).toBeDefined();
    expect(uninstallData.results.length).toBeGreaterThan(0);

    // ---- Step 9: Verify provider removed ----
    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getInferenceProviderConfig',
        (async () => null) as typeof kubernetesService.getInferenceProviderConfig,
      ),
    );

    const statusRes3 = await app.request('/api/installation/providers/kaito/status');
    expect(statusRes3.status).toBe(404);
  });

  test('GET /providers/llmd/status reports no runtime CRD requirement as installed and not installable', async () => {
    const llmdProviderConfig = {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'llmd',
        annotations: {
          'airunway.io/provider-name': 'LLM-D',
        },
      },
      spec: {
        ...mockInferenceProviderConfig.spec,
        capabilities: {
          ...mockInferenceProviderConfig.spec.capabilities,
          requiresCRD: false,
        },
      },
      status: {
        ready: true,
        version: '0.1.0',
      },
    };

    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getInferenceProviderConfig',
        (async (name: string) => name === 'llmd' ? llmdProviderConfig : null) as typeof kubernetesService.getInferenceProviderConfig,
      ),
    );

    const response = await app.request('/api/installation/providers/llmd/status');
    expect(response.status).toBe(200);

    const body = await response.json();
    expect(body.providerId).toBe('llmd');
    expect(body.providerName).toBe('LLM-D');
    expect(body.installed).toBe(true);
    expect(body.crdFound).toBe(true);
    expect(body.operatorRunning).toBe(true);
    expect(body.requiresCRD).toBe(false);
    expect(body.installable).toBe(false);
    expect(body.installationSteps).toEqual([]);
    expect(body.helmCommands).toEqual([]);
    expect(body.message).toBe('Runtime is ready to use.');
  });

  test('GET /providers/llmd/status honors provider readiness even without runtime CRD requirement', async () => {
    const llmdProviderConfig = {
      ...mockInferenceProviderConfig,
      metadata: {
        ...mockInferenceProviderConfig.metadata,
        name: 'llmd',
        annotations: {
          'airunway.io/provider-name': 'LLM-D',
        },
      },
      spec: {
        ...mockInferenceProviderConfig.spec,
        capabilities: {
          ...mockInferenceProviderConfig.spec.capabilities,
          requiresCRD: false,
        },
      },
      status: {
        ready: false,
        version: '0.1.0',
      },
    };

    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getInferenceProviderConfig',
        (async (name: string) => name === 'llmd' ? llmdProviderConfig : null) as typeof kubernetesService.getInferenceProviderConfig,
      ),
    );

    const response = await app.request('/api/installation/providers/llmd/status');
    expect(response.status).toBe(200);

    const body = await response.json();
    expect(body.providerId).toBe('llmd');
    expect(body.providerName).toBe('LLM-D');
    expect(body.installed).toBe(false);
    expect(body.crdFound).toBe(true);
    expect(body.operatorRunning).toBe(false);
    expect(body.requiresCRD).toBe(false);
    expect(body.installable).toBe(false);
    expect(body.helmCommands).toEqual([]);
    expect(body.message).toBe('Provider is registered but not ready yet.');
  });

  // ==========================================================================
  // Edge cases
  // ==========================================================================

  test('POST /providers/unknown-provider/install returns 404', async () => {
    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getInferenceProviderConfig',
        (async () => null) as typeof kubernetesService.getInferenceProviderConfig,
      ),
    );

    const res = await app.request('/api/installation/providers/unknown-provider/install', {
      method: 'POST',
    });
    expect(res.status).toBe(404);
  });

  test('POST /providers/kaito/install with Helm unavailable returns 400', async () => {
    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getInferenceProviderConfig',
        (async () => mockInferenceProviderConfig) as typeof kubernetesService.getInferenceProviderConfig,
      ),
    );
    restores.push(
      mockServiceMethod(
        helmService,
        'checkHelmAvailable',
        (async () => mockHelmUnavailable) as typeof helmService.checkHelmAvailable,
      ),
    );

    const res = await app.request('/api/installation/providers/kaito/install', {
      method: 'POST',
    });
    expect(res.status).toBe(400);
  });

  test('GET /gpu-capacity/detailed returns per-node breakdown', async () => {
    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getDetailedClusterGpuCapacity',
        (async () => mockDetailedGpuCapacity) as typeof kubernetesService.getDetailedClusterGpuCapacity,
      ),
    );

    const res = await app.request('/api/installation/gpu-capacity/detailed');
    expect(res.status).toBe(200);
    const data = await res.json();
    expect(data.totalGpus).toBe(4);
    expect(data.nodePools).toHaveLength(1);
    expect(data.nodePools[0].name).toBe('gpu-node-1');
    expect(data.nodePools[0].availableGpus).toBe(3);
  });
});
