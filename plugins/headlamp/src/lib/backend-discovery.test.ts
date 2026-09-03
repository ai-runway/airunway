/**
 * Backend discovery tests
 */

import { beforeEach, describe, expect, it, vi } from 'vitest';

// Mock plugin storage
vi.mock('./plugin-storage', () => ({
  getPluginSettingsValue: vi.fn(() => null),
  setPluginSettingsValue: vi.fn(),
}));

describe('Backend Discovery', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('exports getBackendUrlSync function', async () => {
    const { getBackendUrlSync } = await import('./backend-discovery');
    expect(getBackendUrlSync).toBeDefined();
    expect(typeof getBackendUrlSync).toBe('function');
  });

  it('exports discoverBackend function', async () => {
    const { discoverBackend } = await import('./backend-discovery');
    expect(discoverBackend).toBeDefined();
    expect(typeof discoverBackend).toBe('function');
  });

  it('exports getBackendConfig function', async () => {
    const { getBackendConfig } = await import('./backend-discovery');
    expect(getBackendConfig).toBeDefined();
    expect(typeof getBackendConfig).toBe('function');
  });

  it('exports clearBackendCache function', async () => {
    const { clearBackendCache } = await import('./backend-discovery');
    expect(clearBackendCache).toBeDefined();
    expect(typeof clearBackendCache).toBe('function');
  });

  it('exports setBackendUrl function', async () => {
    const { setBackendUrl } = await import('./backend-discovery');
    expect(setBackendUrl).toBeDefined();
    expect(typeof setBackendUrl).toBe('function');
  });

  it('getBackendUrlSync returns default when no settings', async () => {
    const { getBackendUrlSync } = await import('./backend-discovery');
    const url = getBackendUrlSync();
    // Should return default localhost URL
    expect(url).toBe('http://localhost:3001');
  });
});
