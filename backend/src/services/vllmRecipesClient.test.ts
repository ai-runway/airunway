import { describe, expect, test } from 'bun:test';
import {
  VllmRecipesClient,
  VllmRecipeValidationError,
  VllmRecipeTimeoutError,
  VllmRecipeUpstreamError,
} from './vllmRecipesClient';

describe('VllmRecipesClient', () => {
  test('resolves relative recipe references under the configured recipe base URL', () => {
    const client = new VllmRecipesClient('https://recipes.vllm.ai/catalog');

    expect(client.resolveReference('microsoft/Phi-4-mini-instruct.json')).toBe(
      'https://recipes.vllm.ai/catalog/microsoft/Phi-4-mini-instruct.json'
    );
  });

  test('allows absolute references inside the configured recipe base URL', () => {
    const client = new VllmRecipesClient('https://recipes.vllm.ai/catalog');

    expect(client.resolveReference('https://recipes.vllm.ai/catalog/microsoft/Phi-4-mini-instruct.json')).toBe(
      'https://recipes.vllm.ai/catalog/microsoft/Phi-4-mini-instruct.json'
    );
  });

  test('rejects recipe references outside the configured recipe origin', () => {
    const client = new VllmRecipesClient('https://recipes.vllm.ai');

    expect(() => client.resolveReference('https://example.com/recipe.json')).toThrow(
      'vLLM recipe references must stay under https://recipes.vllm.ai'
    );
  });

  test('rejects recipe references outside the configured recipe path prefix', () => {
    const client = new VllmRecipesClient('https://recipes.vllm.ai/catalog');

    expect(() => client.resolveReference('/other/recipe.json')).toThrow(
      'vLLM recipe references must stay under https://recipes.vllm.ai/catalog'
    );
  });

  test('rejects non-HTTPS recipe references', () => {
    const client = new VllmRecipesClient('https://recipes.vllm.ai');

    expect(() => client.resolveReference('http://recipes.vllm.ai/recipe.json')).toThrow(
      'vLLM recipe references must use HTTPS'
    );
  });

  describe('getByModelId model ID validation', () => {
    const client = new VllmRecipesClient('https://recipes.vllm.ai');

    // These must throw synchronously from validation, before any network fetch.
    test.each([
      ['extra path segments', 'acme/foo/bar'],
      ['parent-dir traversal in model', 'acme/../bar'],
      ['parent-dir traversal in org', '../acme/model'],
      ['dot segment', './model'],
      ['trailing slash', 'acme/'],
      ['leading slash', '/model'],
      ['no slash', 'justmodel'],
    ])('rejects %s (%p)', async (_label, modelId) => {
      await expect(client.getByModelId(modelId)).rejects.toThrow(
        `Invalid Hugging Face model ID: ${modelId}`
      );
    });

    test('throws a typed validation error for bad input (mapped to 4xx by the route)', async () => {
      await expect(client.getByModelId('acme/foo/bar')).rejects.toBeInstanceOf(VllmRecipeValidationError);
    });
  });

  describe('upstream error classification + caching', () => {
    const originalFetch = globalThis.fetch;

    function withFetch<T>(fetchImpl: typeof fetch, run: (client: VllmRecipesClient) => Promise<T>): Promise<T> {
      globalThis.fetch = fetchImpl;
      const client = new VllmRecipesClient('https://recipes.vllm.ai');
      return run(client).finally(() => {
        globalThis.fetch = originalFetch;
      });
    }

    test('maps an aborted fetch to a timeout error', async () => {
      await withFetch(
        (async () => {
          const err = new Error('aborted');
          err.name = 'AbortError';
          throw err;
        }) as unknown as typeof fetch,
        async (client) => {
          await expect(client.getByModelId('acme/model')).rejects.toBeInstanceOf(VllmRecipeTimeoutError);
        }
      );
    });

    test('maps a non-ok upstream response to an upstream error', async () => {
      await withFetch(
        (async () => new Response('nope', { status: 503, statusText: 'Service Unavailable' })) as unknown as typeof fetch,
        async (client) => {
          await expect(client.getByModelId('acme/model')).rejects.toBeInstanceOf(VllmRecipeUpstreamError);
        }
      );
    });

    test('rejects an oversized recipe payload', async () => {
      const huge = JSON.stringify({ blob: 'x'.repeat(6 * 1024 * 1024) });
      await withFetch(
        (async () => new Response(huge, { status: 200, headers: { 'content-type': 'application/json' } })) as unknown as typeof fetch,
        async (client) => {
          await expect(client.getByModelId('acme/model')).rejects.toBeInstanceOf(VllmRecipeUpstreamError);
        }
      );
    });

    test('caches a model recipe so a second call does not refetch', async () => {
      let calls = 0;
      await withFetch(
        (async () => {
          calls += 1;
          return new Response(JSON.stringify({ recipe: true }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          });
        }) as unknown as typeof fetch,
        async (client) => {
          await client.getByModelId('acme/model');
          await client.getByModelId('acme/model');
          expect(calls).toBe(1);
        }
      );
    });

    test('rejects an oversized chunked response without buffering the whole body', async () => {
      // A response with no content-length, streamed in chunks past the cap. The
      // streaming reader must abort before materializing the entire body.
      let producedBytes = 0;
      const chunk = new TextEncoder().encode('x'.repeat(512 * 1024)); // 512 KiB
      const stream = new ReadableStream<Uint8Array>({
        pull(controllerObj) {
          // Produce well past the 5 MiB cap if not stopped (20 MiB worth).
          if (producedBytes >= 20 * 1024 * 1024) {
            controllerObj.close();
            return;
          }
          producedBytes += chunk.byteLength;
          controllerObj.enqueue(chunk);
        },
      });

      await withFetch(
        (async () =>
          new Response(stream, {
            status: 200,
            headers: { 'content-type': 'application/json' }, // no content-length
          })) as unknown as typeof fetch,
        async (client) => {
          await expect(client.getByModelId('acme/model')).rejects.toBeInstanceOf(VllmRecipeUpstreamError);
          // The reader must have stopped near the cap, not consumed the full 20 MiB.
          expect(producedBytes).toBeLessThan(8 * 1024 * 1024);
        }
      );
    });

    test('evicts least-recently-used entries beyond the cache cap', async () => {
      // Distinct model IDs far exceeding the cap; the first ones must be evicted
      // (re-fetched), proving the cache does not grow without bound.
      const calls = new Map<string, number>();
      await withFetch(
        (async (input: string | URL | Request) => {
          const url = typeof input === 'string' ? input : input.toString();
          calls.set(url, (calls.get(url) ?? 0) + 1);
          return new Response(JSON.stringify({ recipe: true }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          });
        }) as unknown as typeof fetch,
        async (client) => {
          // Fill well beyond MAX_CACHE_ENTRIES (256).
          for (let i = 0; i < 300; i++) {
            await client.getByModelId(`org/model-${i}`);
          }
          // The earliest entry should have been evicted, so re-requesting it refetches.
          await client.getByModelId('org/model-0');
          const firstUrl = [...calls.keys()].find((u) => u.endsWith('/org/model-0.json'))!;
          expect(calls.get(firstUrl)).toBe(2);
          // A recent entry should still be cached (single fetch).
          const recentUrl = [...calls.keys()].find((u) => u.endsWith('/org/model-299.json'))!;
          expect(calls.get(recentUrl)).toBe(1);
        }
      );
    });
  });
});
