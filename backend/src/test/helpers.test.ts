import { describe, test, expect, afterEach } from 'bun:test';
import { mockFetchByUrl } from './helpers';

describe('mockFetchByUrl', () => {
  let restore: (() => void) | undefined;

  afterEach(() => {
    restore?.();
    restore = undefined;
  });

  test('routes responses by URL substring match', async () => {
    restore = mockFetchByUrl({
      '/oauth/token': { body: { access_token: 'test-token' } },
      '/api/whoami-v2': { body: { name: 'testuser' } },
    });

    const tokenRes = await fetch('https://huggingface.co/oauth/token', { method: 'POST' });
    expect(tokenRes.ok).toBe(true);
    const tokenData = await tokenRes.json();
    expect(tokenData.access_token).toBe('test-token');

    const whoamiRes = await fetch('https://huggingface.co/api/whoami-v2');
    expect(whoamiRes.ok).toBe(true);
    const whoamiData = await whoamiRes.json();
    expect(whoamiData.name).toBe('testuser');
  });

  test('returns 404 for unmatched URLs', async () => {
    restore = mockFetchByUrl({
      '/oauth/token': { body: { access_token: 'test-token' } },
    });

    const res = await fetch('https://example.com/unknown');
    expect(res.ok).toBe(false);
    expect(res.status).toBe(404);
  });

  test('supports custom status and ok fields', async () => {
    restore = mockFetchByUrl({
      '/api/fail': { body: { error: 'bad request' }, ok: false, status: 400 },
    });

    const res = await fetch('https://example.com/api/fail');
    expect(res.ok).toBe(false);
    expect(res.status).toBe(400);
    const data = await res.json();
    expect(data.error).toBe('bad request');
  });

  test('restores original fetch after calling restore', async () => {
    const originalFetch = globalThis.fetch;
    restore = mockFetchByUrl({
      '/test': { body: { mocked: true } },
    });
    expect(globalThis.fetch).not.toBe(originalFetch);

    restore();
    restore = undefined;
    expect(globalThis.fetch).toBe(originalFetch);
  });

  test('preserves fetch.preconnect on the mock and restores the original fetch', () => {
    const actualFetch = globalThis.fetch;
    const preconnectCalls: Array<unknown[]> = [];
    const tempFetch = (async () => new Response('{}')) as typeof fetch & {
      preconnect: (...args: unknown[]) => void;
    };
    tempFetch.preconnect = (...args: unknown[]) => {
      preconnectCalls.push(args);
    };

    let localRestore: (() => void) | undefined;
    globalThis.fetch = tempFetch;
    try {
      localRestore = mockFetchByUrl({
        '/test': { body: { mocked: true } },
      });

      const mockedFetch = globalThis.fetch as typeof fetch & {
        preconnect?: (...args: unknown[]) => unknown;
      };
      expect(mockedFetch).not.toBe(tempFetch);
      expect(typeof mockedFetch.preconnect).toBe('function');

      mockedFetch.preconnect?.('https://example.com');
      expect(preconnectCalls).toEqual([['https://example.com']]);

      localRestore();
      localRestore = undefined;
      expect(globalThis.fetch).toBe(tempFetch);
    } finally {
      localRestore?.();
      globalThis.fetch = actualFetch;
    }
  });

  test('first matching pattern wins for overlapping URL substrings', async () => {
    // '/api/whoami-v2' should match before '/api/whoami' when listed first
    restore = mockFetchByUrl({
      '/api/whoami-v2': { body: { version: 2 } },
      '/api/whoami': { body: { version: 1 } },
    });

    const res = await fetch('https://huggingface.co/api/whoami-v2');
    const data = await res.json();
    expect(data.version).toBe(2);
  });

  test('less-specific pattern matches when more-specific is listed second', async () => {
    // When '/api/whoami' is listed first, it matches '/api/whoami-v2' too
    restore = mockFetchByUrl({
      '/api/whoami': { body: { version: 1 } },
      '/api/whoami-v2': { body: { version: 2 } },
    });

    const res = await fetch('https://huggingface.co/api/whoami-v2');
    const data = await res.json();
    // The less-specific pattern '/api/whoami' matches first — this is the
    // "first match wins" footgun documented in TESTING.md and helpers.ts.
    expect(data.version).toBe(1);
  });
});
