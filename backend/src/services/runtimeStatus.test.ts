import { describe, expect, test, vi } from 'bun:test';

import {
  getRuntimesStatus,
  runtimeStatusFromProviderConfig,
  type InferenceProviderConfigResource,
  type RuntimeStatusAdapter,
} from './runtimeStatus';
import type { InstallationStatus } from './runtimeInstallation';

function adapter(overrides: Partial<RuntimeStatusAdapter> = {}): RuntimeStatusAdapter {
  return {
    checkCRDInstallation: vi.fn(async () => ({ installed: true })),
    listInferenceProviderConfigs: vi.fn(async () => []),
    getInferenceProviderConfig: vi.fn(async () => null),
    checkProviderInstallationStatus: vi.fn(async (_id, status, name, requiresCRD): Promise<InstallationStatus> => ({
      providerId: 'runtime',
      providerName: name || 'Runtime',
      installed: status?.ready ?? false,
      crdFound: true,
      operatorRunning: status?.ready ?? false,
      requiresCRD,
      installationSteps: [],
      helmCommands: [],
      message: status?.ready ? 'Runtime is ready to use.' : 'Provider is registered but not ready yet.',
    })),
    getK8sStatusCode: (error) => (error as { statusCode?: number })?.statusCode,
    getK8sErrorMessage: (error) => (error as Error)?.message || String(error),
    logWarn: vi.fn(),
    ...overrides,
  };
}

function providerConfig(overrides: Partial<InferenceProviderConfigResource> = {}): InferenceProviderConfigResource {
  return {
    metadata: { name: 'vllm' },
    spec: {
      capabilities: {
        engines: [{ name: 'vllm', servingModes: ['aggregated'], gpuSupport: true, requiresCRD: false }],
      },
    },
    status: { ready: true, version: '0.8.0' },
    ...overrides,
  };
}

describe('runtimeStatus', () => {
  test('returns no runtimes when the AI Runway CRD is not installed', async () => {
    const a = adapter({ checkCRDInstallation: vi.fn(async () => ({ installed: false })) });

    await expect(getRuntimesStatus(a)).resolves.toEqual([]);
    expect(a.listInferenceProviderConfigs).not.toHaveBeenCalled();
  });

  test('maps provider configs through live installation status and display metadata', async () => {
    const a = adapter({
      listInferenceProviderConfigs: vi.fn(async () => [providerConfig({
        metadata: { name: 'custom-vllm', annotations: { 'airunway.ai/provider-name': 'vLLM' } },
        status: { ready: true, version: '1.2.3' },
      })]),
    });

    await expect(getRuntimesStatus(a)).resolves.toEqual([{
      id: 'custom-vllm',
      name: 'vLLM',
      installed: true,
      healthy: true,
      crdFound: true,
      operatorRunning: true,
      requiresCRD: false,
      version: '1.2.3',
      message: 'Runtime is ready to use.',
    }]);
    expect(a.checkProviderInstallationStatus).toHaveBeenCalledWith('custom-vllm', { ready: true, version: '1.2.3' }, 'vLLM', false);
  });

  test('uses shim heartbeat messages when the provider health signal is stale', async () => {
    const stale = new Date(Date.now() - 10 * 60 * 1000).toISOString();
    const result = await runtimeStatusFromProviderConfig(adapter(), providerConfig({
      metadata: { name: 'llmd' },
      status: { ready: true, version: '0.1.0', lastHeartbeat: stale },
    }));

    expect(result.message).toBe('The provider is not reporting status. Check that the AI Runway provider shim is running.');
    expect(result.installed).toBe(true);
  });

  test('logs non-not-found list errors and returns an empty list', async () => {
    const a = adapter({
      listInferenceProviderConfigs: vi.fn(async () => { throw { statusCode: 500, message: 'boom' }; }),
      getK8sErrorMessage: () => 'boom',
    });

    await expect(getRuntimesStatus(a)).resolves.toEqual([]);
    expect(a.logWarn).toHaveBeenCalledWith({ error: 'boom' }, 'Failed to list InferenceProviderConfigs');
  });

  test('silently treats not-found provider config lists as empty during bootstrap', async () => {
    const a = adapter({
      listInferenceProviderConfigs: vi.fn(async () => { throw { statusCode: 404, message: 'not found' }; }),
    });

    await expect(getRuntimesStatus(a)).resolves.toEqual([]);
    expect(a.logWarn).not.toHaveBeenCalled();
  });
});
