import { describe, test, expect } from 'bun:test';
import { getProviderHealth } from './providerHealth';
import {
  mockKaitoCRNewShimHealthy,
  mockKaitoCROldShim,
  mockKaitoCRStale,
} from '../test/fixtures';

describe('getProviderHealth', () => {
  test('returns healthy for new-shim CR with UpstreamHealthy condition', () => {
    const h = getProviderHealth('kaito', mockKaitoCRNewShimHealthy);
    expect(h.healthy).toBe(true);
    expect(h.reason).toBe('UpstreamHealthy');
    expect(h.stale).toBe(false);
    expect(h.hasShimSignal).toBe(true);
  });

  test('returns stale when heartbeat is older than threshold', () => {
    const h = getProviderHealth('kaito', mockKaitoCRStale);
    expect(h.healthy).toBe(false);
    expect(h.reason).toBe('ShimStale');
    expect(h.stale).toBe(true);
    expect(h.hasShimSignal).toBe(true);
  });

  test('returns NotReady (not stale) when no heartbeat is reported', () => {
    const cr = {
      ...mockKaitoCRNewShimHealthy,
      status: { ...mockKaitoCRNewShimHealthy.status, lastHeartbeat: undefined, ready: false, conditions: [] },
    };
    const h = getProviderHealth('kaito', cr);
    expect(h.stale).toBe(false);
    expect(h.reason).toBe('NotReady');
    expect(h.hasShimSignal).toBe(false);
  });

  test('falls back to ready/NotReady reason when no UpstreamReady condition', () => {
    const h = getProviderHealth('kaito', mockKaitoCROldShim);
    expect(h.healthy).toBe(true);
    expect(h.reason).toBe('Ready');
    expect(h.stale).toBe(false);
    expect(h.hasShimSignal).toBe(false);
  });

  test('handles missing config gracefully (not stale, not ready)', () => {
    const h = getProviderHealth('kaito', null);
    expect(h.stale).toBe(false);
    expect(h.healthy).toBe(false);
    expect(h.hasShimSignal).toBe(false);
  });

  test('flags fresh UpstreamReady=False as a shim signal (refuse-fast path)', () => {
    const cr = {
      ...mockKaitoCRNewShimHealthy,
      status: {
        ...mockKaitoCRNewShimHealthy.status,
        ready: false,
        conditions: [
          {
            type: 'UpstreamReady',
            status: 'False',
            reason: 'UpstreamControllerMissing',
            message: 'The KAITO workspace controller is not running.',
          },
        ],
      },
    };
    const h = getProviderHealth('kaito', cr);
    expect(h.healthy).toBe(false);
    expect(h.stale).toBe(false);
    expect(h.hasShimSignal).toBe(true);
    expect(h.reason).toBe('UpstreamControllerMissing');
    expect(h.message).toBe('The KAITO workspace controller is not running.');
  });
});
