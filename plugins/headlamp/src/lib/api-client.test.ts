/**
 * API Client tests
 */

import { beforeEach, describe, expect, it, vi } from 'vitest';

// Mock shared API
vi.mock('@airunway/shared/api', () => ({
  createApiClient: vi.fn(() => ({
    health: { ping: vi.fn() },
    deployments: { list: vi.fn(), get: vi.fn(), create: vi.fn(), delete: vi.fn() },
    models: { list: vi.fn() },
    runtimes: { getStatus: vi.fn() },
    settings: { get: vi.fn(), update: vi.fn() },
    installation: { install: vi.fn(), uninstall: vi.fn() },
    gpuOperator: { validate: vi.fn() },
    autoscaler: { getStatus: vi.fn() },
    huggingFace: { listModels: vi.fn(), getModelInfo: vi.fn() },
    aikit: { listModels: vi.fn(), buildModel: vi.fn() },
    aiConfigurator: { analyze: vi.fn() },
    metrics: { getDeploymentMetrics: vi.fn() },
  })),
}));

// Mock backend discovery
vi.mock('./backend-discovery', () => ({
  getBackendUrlSync: vi.fn(() => 'http://localhost:3001'),
  clearBackendCache: vi.fn(),
}));

// Mock plugin storage
vi.mock('./plugin-storage', () => ({
  getPluginSettingsValue: vi.fn(() => null),
  setPluginSettingsValue: vi.fn(),
}));

describe('API Client', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('exports getApiClient function', async () => {
    const { getApiClient } = await import('./api-client');
    expect(getApiClient).toBeDefined();
    expect(typeof getApiClient).toBe('function');
  });

  it('exports useApiClient hook', async () => {
    const { useApiClient } = await import('./api-client');
    expect(useApiClient).toBeDefined();
    expect(typeof useApiClient).toBe('function');
  });

  it('exports resetApiClient function', async () => {
    const { resetApiClient } = await import('./api-client');
    expect(resetApiClient).toBeDefined();
    expect(typeof resetApiClient).toBe('function');
  });

  it('exports api convenience object', async () => {
    const { api } = await import('./api-client');
    expect(api).toBeDefined();
    expect(api.deployments).toBeDefined();
    expect(api.models).toBeDefined();
    expect(api.health).toBeDefined();
  });
});
