import { describe, test, expect, afterEach, mock } from 'bun:test';
import app from '../hono-app';

// Routing-shape tests for the vLLM recipes router. The point is to pin down
// trailing-slash vs no-slash behavior. Hono routes strictly ("/x" != "/x/"),
// so the app registers `trimTrailingSlash()` middleware globally: a GET/HEAD
// whose trailing-slash form would 404 gets a 301 redirect to the no-slash path.
// These tests guard that a stray trailing slash on a GET resolves (via 301)
// instead of dead-ending at 404, while non-GET/HEAD methods (POST) are left to
// strict routing and still 404.
//
// fetch is mocked so the no-slash (route-matched) cases don't make a real
// network call to recipes.vllm.ai — we assert on ROUTING, not upstream content.
const originalFetch = globalThis.fetch;

function mockRecipeFetch(payload: unknown) {
  // @ts-expect-error - mocking fetch for tests
  globalThis.fetch = mock(() =>
    Promise.resolve({
      ok: true,
      status: 200,
      statusText: 'OK',
      headers: { get: () => null },
      arrayBuffer: () => Promise.resolve(new TextEncoder().encode(JSON.stringify(payload)).buffer),
      json: () => Promise.resolve(payload),
      text: () => Promise.resolve(JSON.stringify(payload)),
    } as unknown as Response)
  );
}

describe('vLLM Recipes route shapes (trailing slash vs none)', () => {
  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  describe('GET list route /api/vllm/recipes', () => {
    test('no trailing slash → route matches (not 404)', async () => {
      mockRecipeFetch({ models: [] });
      const res = await app.request('/api/vllm/recipes');
      // Route is matched and handled; the exact 2xx body depends on the mock,
      // but it must NOT be the router's 404 "Route not found".
      expect(res.status).not.toBe(404);
    });

    test('trailing slash → 301 redirect to the no-slash path', async () => {
      mockRecipeFetch({ models: [] });
      const res = await app.request('/api/vllm/recipes/');
      expect(res.status).toBe(301);
      expect(new URL(res.headers.get('location')!).pathname).toBe('/api/vllm/recipes');
    });
  });

  describe('GET per-model route /api/vllm/recipes/:org/:model', () => {
    test('no trailing slash → route matches (not 404)', async () => {
      mockRecipeFetch({ recommended_command: { argv: ['vllm', 'serve', 'x'] } });
      const res = await app.request('/api/vllm/recipes/microsoft/Phi-4-mini-instruct');
      expect(res.status).not.toBe(404);
    });

    test('trailing slash → 301 redirect to the no-slash path', async () => {
      mockRecipeFetch({ recommended_command: { argv: ['vllm', 'serve', 'x'] } });
      const res = await app.request('/api/vllm/recipes/microsoft/Phi-4-mini-instruct/');
      expect(res.status).toBe(301);
      expect(new URL(res.headers.get('location')!).pathname).toBe(
        '/api/vllm/recipes/microsoft/Phi-4-mini-instruct'
      );
    });

    test('single path segment is not the two-segment model route → 404', async () => {
      const res = await app.request('/api/vllm/recipes/onlyoneseg');
      expect(res.status).toBe(404);
    });
  });

  describe('POST resolve route /api/vllm/recipes/resolve', () => {
    test('no trailing slash → route matches (not 404)', async () => {
      mockRecipeFetch({ recommended_command: { argv: ['vllm', 'serve', 'a/b'] } });
      const res = await app.request('/api/vllm/recipes/resolve', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ modelId: 'a/b' }),
      });
      expect(res.status).not.toBe(404);
    });

    test('trailing slash → 404 (trimTrailingSlash only redirects GET/HEAD, not POST)', async () => {
      const res = await app.request('/api/vllm/recipes/resolve/', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ modelId: 'a/b' }),
      });
      expect(res.status).toBe(404);
    });
  });
});
