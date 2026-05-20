/**
 * Shared test helpers for backend e2e tests.
 * Provides mock factories and utilities used across test files.
 */

/**
 * Replace a method on a service singleton and return a restore function.
 *
 * Usage:
 *   const restore = mockServiceMethod(autoscalerService, 'detectAutoscaler', async () => fixture);
 *   // ... run test ...
 *   restore();
 */
export function mockServiceMethod<S extends Record<string, any>, K extends keyof S>(
  service: S,
  method: K,
  implementation: S[K],
): () => void {
  const original = service[method];
  service[method] = implementation;
  return () => {
    service[method] = original;
  };
}

/**
 * Creates a mock fetch function that routes responses by URL substring match.
 * First matching pattern wins — order your routes from most-specific to
 * least-specific to avoid accidental prefix collisions (e.g. `/api/whoami-v2`
 * before `/api/whoami`). Unmatched URLs return 404.
 * Returns a restore function to reset globalThis.fetch.
 *
 * The `init` parameter (headers, method, body, etc.) is accepted to match the
 * native `fetch` signature but is not inspected — route matching is URL-only.
 *
 * Usage:
 *   const restore = mockFetchByUrl({
 *     '/oauth/token': { body: { access_token: 'tok' } },
 *     '/api/whoami-v2': { body: { name: 'user' }, status: 200 },
 *     '/api/fail': { body: { error: 'bad' }, ok: false, status: 400 },
 *   });
 */
export function mockFetchByUrl(
  routes: Record<string, { body: unknown; ok?: boolean; status?: number }>
): () => void {
  const originalFetch = globalThis.fetch;
  const mockFetch = (async (input: RequestInfo | URL, _init?: RequestInit): Promise<Response> => {
    let url: string;
    if (input instanceof Request) {
      url = input.url;
    } else if (input instanceof URL) {
      url = input.toString();
    } else {
      url = input;
    }
    for (const [pattern, cfg] of Object.entries(routes)) {
      if (url.includes(pattern)) {
        const status = cfg.status ?? 200;
        return new Response(JSON.stringify(cfg.body), {
          status,
          statusText: cfg.ok === false ? 'Error' : 'OK',
          headers: { 'Content-Type': 'application/json' },
        });
      }
    }
    return new Response('{}', {
      status: 404,
      statusText: 'Not Found',
      headers: { 'Content-Type': 'application/json' },
    });
  }) as typeof fetch;

  const fetchWithPreconnect = originalFetch as typeof fetch & {
    preconnect?: (...args: unknown[]) => unknown;
  };
  if (typeof fetchWithPreconnect.preconnect === 'function') {
    Object.assign(mockFetch, { preconnect: fetchWithPreconnect.preconnect.bind(originalFetch) });
  }

  globalThis.fetch = mockFetch;
  return () => {
    globalThis.fetch = originalFetch;
  };
}
