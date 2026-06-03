import { describe, test, expect, beforeEach, afterEach } from 'bun:test';
import app, { parseCorsOrigin } from './hono-app';
import { kubernetesService } from './services/kubernetes';
import { configService } from './services/config';
import { authService } from './services/auth';
import { mockServiceMethod } from './test/helpers';
import { mockDeployment } from './test/fixtures';
import { HTTPException } from 'hono/http-exception';

// Helper to add timeout to async operations for K8s-dependent tests
async function withTimeout<T>(promise: Promise<T>, ms: number): Promise<T> {
  const timeout = new Promise<never>((_, reject) => {
    setTimeout(() => reject(new Error(`Operation timed out after ${ms}ms`)), ms);
  });
  return Promise.race([promise, timeout]);
}

// Shorter timeout for tests that depend on K8s (which may not be available)
const K8S_TEST_TIMEOUT = 2000;
const AIRUNWAY_AUTH_ERROR_HEADER = 'X-Airunway-Auth-Error';

function expectChatProxyCall(capturedArgs: unknown[] | undefined, expectedArgs: unknown[]): void {
  expect(capturedArgs?.slice(0, 5)).toEqual(expectedArgs);
  expect(capturedArgs?.[5]).toEqual({});
  const chatProxyOptions = capturedArgs?.[6] as { signal?: unknown } | undefined;
  expect(chatProxyOptions?.signal).toBeInstanceOf(AbortSignal);
}

function expectSseHeaders(res: Response): void {
  expect(res.headers.get('Content-Type')).toContain('text/event-stream');
  expect(res.headers.get('Cache-Control')).toBe('no-cache, no-transform');
  expect(res.headers.get('Content-Encoding')).toBe('identity');
}

function createChunkedErrorStream(chunks: string[]): {
  stream: ReadableStream<Uint8Array>;
  wasCancelled: () => boolean;
} {
  const encoder = new TextEncoder();
  let nextChunk = 0;
  let cancelCalled = false;

  return {
    stream: new ReadableStream<Uint8Array>({
      pull(controller) {
        if (nextChunk < chunks.length) {
          controller.enqueue(encoder.encode(chunks[nextChunk]!));
          nextChunk += 1;
          return;
        }

        controller.close();
      },
      cancel() {
        cancelCalled = true;
      },
    }),
    wasCancelled: () => cancelCalled,
  };
}

app.get('/__test/http-exception/internal', () => {
  throw new HTTPException(500, { message: 'database password is secret' });
});

app.get('/__test/http-exception/bad-request', () => {
  throw new HTTPException(400, { message: 'client supplied an invalid value' });
});

describe('Hono Routes', () => {
  describe('Health Routes', () => {
    test('GET /api/health returns healthy status', async () => {
      const res = await app.request('/api/health');
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.status).toBe('healthy');
      expect(data.timestamp).toBeDefined();
    });
  });

  describe('CORS', () => {
    const defaultCorsTest = process.env.CORS_ORIGIN === undefined ? test : test.skip;

    defaultCorsTest('default CORS allows loopback origins and rejects non-loopback origins', async () => {
      const loopbackOrigins = [
        'http://localhost:4466',
        'http://127.0.0.1:4466',
        'http://[::1]:4466',
      ];

      for (const loopbackOrigin of loopbackOrigins) {
        const loopbackRes = await app.request('/api/health', {
          headers: { Origin: loopbackOrigin },
        });
        expect(loopbackRes.headers.get('Access-Control-Allow-Origin')).toBe(loopbackOrigin);
      }

      const externalRes = await app.request('/api/health', {
        headers: { Origin: 'https://example.com' },
      });
      expect(externalRes.headers.get('Access-Control-Allow-Origin')).toBeNull();
    });

    test('empty CORS_ORIGIN falls back to loopback origins', () => {
      const origin = parseCorsOrigin(' , ');
      expect(typeof origin).toBe('function');

      if (typeof origin !== 'function') {
        throw new Error('expected empty CORS_ORIGIN fallback to be a function');
      }

      expect(origin('http://localhost:4466', {} as never)).toBe('http://localhost:4466');
      expect(origin('http://127.0.0.1:4466', {} as never)).toBe('http://127.0.0.1:4466');
      expect(origin('http://[::1]:4466', {} as never)).toBe('http://[::1]:4466');
      expect(origin('https://example.com', {} as never)).toBeNull();
    });
  });

  describe('Models Routes', () => {
    test('GET /api/models returns model list', async () => {
      const res = await app.request('/api/models');
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.models).toBeDefined();
      expect(Array.isArray(data.models)).toBe(true);
    });

    test('GET /api/models/:id with slashes captures full model ID', async () => {
      // Test that the wildcard pattern captures model IDs with slashes
      const res = await app.request('/api/models/Qwen/Qwen3-0.6B');
      // Should return 404 if model doesn't exist, but importantly it should NOT
      // be a route-level 404 (which would indicate the pattern didn't match)
      const data = await res.json();

      // If model exists, should return it
      // If model doesn't exist, should return { error: { message: 'Model not found' } }
      // NOT { error: { message: 'Route not found...' } }
      if (res.status === 404) {
        expect(data.error?.message).toBe('Model not found');
      } else {
        expect(res.status).toBe(200);
        expect(data.id).toBe('Qwen/Qwen3-0.6B');
      }
    });

    test('GET /api/models/:id with deeply nested slashes', async () => {
      const res = await app.request('/api/models/org/repo/variant');
      expect(res.status).toBe(404);
      const data = await res.json();
      // Should be model not found, not route not found
      expect(data.error?.message).toBe('Model not found');
    });
  });

  describe('Settings Routes', () => {
    const restores: Array<() => void> = [];

    afterEach(() => {
      restores.forEach((restore) => restore());
      restores.length = 0;
    });

    test('GET /api/settings returns settings', async () => {
      const res = await app.request('/api/settings');
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.config).toBeDefined();
    });

    test('GET /api/settings returns auth config', async () => {
      const res = await app.request('/api/settings');
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.auth).toBeDefined();
      expect(typeof data.auth.enabled).toBe('boolean');
    });

    test('PUT /api/settings marks missing token auth failures as Airunway auth errors', async () => {
      restores.push(
        mockServiceMethod(authService, 'isAuthEnabled', () => true),
      );

      const res = await app.request('/api/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ defaultNamespace: 'default' }),
      });

      expect(res.status).toBe(401);
      expect(res.headers.get(AIRUNWAY_AUTH_ERROR_HEADER)).toBe('true');
      const data = await res.json();
      expect(data.error.message).toBe('Authentication required');
      expect(data.error.statusCode).toBe(401);
    });

    test('PUT /api/settings marks invalid token auth failures as Airunway auth errors', async () => {
      restores.push(
        mockServiceMethod(authService, 'isAuthEnabled', () => true),
      );
      restores.push(
        mockServiceMethod(authService, 'validateToken', async () => ({
          valid: false,
          error: 'Token expired',
        })),
      );

      const res = await app.request('/api/settings', {
        method: 'PUT',
        headers: {
          'Authorization': 'Bearer invalid-token',
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ defaultNamespace: 'default' }),
      });

      expect(res.status).toBe(401);
      expect(res.headers.get(AIRUNWAY_AUTH_ERROR_HEADER)).toBe('true');
      const data = await res.json();
      expect(data.error.message).toBe('Token expired');
      expect(data.error.statusCode).toBe(401);
    });
  });

  describe('Deployments Routes', () => {
    const restores: Array<() => void> = [];

    afterEach(() => {
      restores.forEach((restore) => restore());
      restores.length = 0;
    });

    test('GET /api/deployments returns deployment list with pagination', async () => {
      try {
        const res = await withTimeout(app.request('/api/deployments'), K8S_TEST_TIMEOUT);
        // May fail if no k8s cluster, but should return valid response structure
        const status = res.status;
        expect([200, 500]).toContain(status);

        if (status === 200) {
          const data = await res.json();
          expect(data.deployments).toBeDefined();
          expect(data.pagination).toBeDefined();
          expect(Array.isArray(data.deployments)).toBe(true);
        }
      } catch (error) {
        // If K8s is not available, the request may timeout - that's acceptable
        if (error instanceof Error && error.message.includes('timed out')) {
          console.log('Skipping test: K8s API not available (timeout)');
          return;
        }
        throw error;
      }
    });

    test('POST /api/deployments/:name/chat streams proxied chat completions', async () => {
      let capturedModelLookupArgs: unknown[] | undefined;
      let capturedChatProxyArgs: unknown[] | undefined;
      const upstreamSse = 'data: {"choices":[{"delta":{"content":"Hello"}}]}\n\ndata: [DONE]\n\n';

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServiceGet', async (...args: unknown[]) => {
          capturedModelLookupArgs = args;
          return JSON.stringify({ data: [{ id: 'served-from-models-endpoint' }] });
        }),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
          capturedChatProxyArgs = args;
          return new Response(upstreamSse, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(200);
      expectSseHeaders(res);
      expect(await res.text()).toBe(upstreamSse);

      expect(capturedModelLookupArgs?.slice(0, 4)).toEqual([
        'test-deploy-frontend',
        'default',
        8080,
        'v1/models',
      ]);
      const modelLookupOptions = capturedModelLookupArgs?.[4] as { accept?: string; signal?: unknown } | undefined;
      expect(modelLookupOptions?.accept).toBe('application/json');
      expect(modelLookupOptions?.signal).toBeInstanceOf(AbortSignal);
      expectChatProxyCall(capturedChatProxyArgs, [
        'test-deploy-frontend',
        'default',
        8080,
        'v1/chat/completions',
        {
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'served-from-models-endpoint',
          stream: true,
        },
      ]);
    });

    test('POST /api/deployments/:name/chat propagates authenticated user token to Kubernetes lookups and proxies', async () => {
      let capturedValidateTokenArgs: unknown[] | undefined;
      let capturedDeploymentArgs: unknown[] | undefined;
      let capturedModelLookupArgs: unknown[] | undefined;
      let capturedChatProxyArgs: unknown[] | undefined;
      const upstreamSse = 'data: [DONE]\n\n';

      restores.push(
        mockServiceMethod(authService, 'isAuthEnabled', () => true),
      );
      restores.push(
        mockServiceMethod(authService, 'validateToken', async (...args: unknown[]) => {
          capturedValidateTokenArgs = args;
          return {
            valid: true,
            user: {
              username: 'test-user',
              uid: 'test-uid',
              groups: ['developers'],
            },
          };
        }),
      );
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async (...args: unknown[]) => {
          capturedDeploymentArgs = args;
          return {
            ...mockDeployment,
            phase: 'Running',
            provider: 'dynamo',
            replicas: { desired: 1, ready: 1, available: 1 },
            frontendService: 'test-deploy-frontend:8080',
          } as never;
        }),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServiceGet', async (...args: unknown[]) => {
          capturedModelLookupArgs = args;
          return JSON.stringify({ data: [{ id: 'served-from-authenticated-models-endpoint' }] });
        }),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
          capturedChatProxyArgs = args;
          return new Response(upstreamSse, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: {
          'Authorization': 'Bearer user-token',
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(200);
      expect(await res.text()).toBe(upstreamSse);
      expect(capturedValidateTokenArgs).toEqual(['user-token']);
      expect(capturedDeploymentArgs).toEqual(['test-deploy', 'default', 'user-token']);
      expect(capturedModelLookupArgs?.slice(0, 4)).toEqual([
        'test-deploy-frontend',
        'default',
        8080,
        'v1/models',
      ]);
      const modelLookupOptions = capturedModelLookupArgs?.[4] as {
        accept?: string;
        signal?: unknown;
        userToken?: string;
      } | undefined;
      expect(modelLookupOptions?.accept).toBe('application/json');
      expect(modelLookupOptions?.signal).toBeInstanceOf(AbortSignal);
      expect(modelLookupOptions?.userToken).toBe('user-token');
      expect(capturedChatProxyArgs?.slice(0, 5)).toEqual([
        'test-deploy-frontend',
        'default',
        8080,
        'v1/chat/completions',
        {
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'served-from-authenticated-models-endpoint',
          stream: true,
        },
      ]);
      expect(capturedChatProxyArgs?.[5]).toEqual({});
      const chatProxyOptions = capturedChatProxyArgs?.[6] as {
        signal?: unknown;
        userToken?: string;
      } | undefined;
      expect(chatProxyOptions?.signal).toBeInstanceOf(AbortSignal);
      expect(chatProxyOptions?.userToken).toBe('user-token');
    });

    test('POST /api/deployments/:namespace/:name/chat streams proxied chat completions', async () => {
      let capturedDeploymentArgs: unknown[] | undefined;
      let capturedChatProxyArgs: unknown[] | undefined;
      const upstreamSse = 'data: [DONE]\n\n';

      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async (...args: unknown[]) => {
          capturedDeploymentArgs = args;
          return {
            ...mockDeployment,
            phase: 'Running',
            provider: 'dynamo',
            replicas: { desired: 1, ready: 1, available: 1 },
            frontendService: 'test-deploy-frontend:8080',
            servedModelName: 'deployment-served-model',
          } as never;
        }),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
          capturedChatProxyArgs = args;
          return new Response(upstreamSse, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }),
      );

      const res = await app.request('/api/deployments/custom-ns/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(200);
      expect(await res.text()).toBe(upstreamSse);
      expect(capturedDeploymentArgs).toEqual(['test-deploy', 'custom-ns', undefined]);
      expectChatProxyCall(capturedChatProxyArgs, [
        'test-deploy-frontend',
        'custom-ns',
        8080,
        'v1/chat/completions',
        {
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'deployment-served-model',
          stream: true,
        },
      ]);
    });

    test('POST /api/deployments/:name/chat direct path ignores gateway.modelName (HTTPRoute alias)', async () => {
      let capturedChatProxyArgs: unknown[] | undefined;
      const upstreamSse = 'data: [DONE]\n\n';

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      // Deployment exposes a gateway alias ('served-from-gateway') AND a real
      // served model name. The direct service-proxy path must use the served
      // model name; the gateway alias would 404 against the frontend service.
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
          gateway: { endpoint: 'https://gateway.example.com', modelName: 'served-from-gateway' },
          servedModelName: 'deployment-served-model',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
          capturedChatProxyArgs = args;
          return new Response(upstreamSse, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(200);
      expect(await res.text()).toBe(upstreamSse);
      expectChatProxyCall(capturedChatProxyArgs, [
        'test-deploy-frontend',
        'default',
        8080,
        'v1/chat/completions',
        {
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'deployment-served-model',
          stream: true,
        },
      ]);
    });


    test('POST /api/deployments/:name/chat gateway fallback ignores request model', async () => {
      let capturedDirectProxyArgs: unknown[] | undefined;
      let capturedGatewayBody: Record<string, unknown> | undefined;
      let capturedGatewayHeader: string | null = null;
      let capturedGatewaySignal: AbortSignal | null | undefined;
      const gatewaySse = 'data: {"choices":[{"delta":{"content":"Hello from gateway"}}]}\n\ndata: [DONE]\n\n';
      const originalFetch = globalThis.fetch;

      restores.push(() => {
        globalThis.fetch = originalFetch;
      });
      globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
        capturedGatewayBody = JSON.parse(String(init?.body)) as Record<string, unknown>;
        capturedGatewayHeader = new Headers(init?.headers).get('X-Gateway-Model-Name');
        capturedGatewaySignal = init?.signal;
        return new Response(gatewaySse, {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        });
      }) as typeof fetch;

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
          gateway: { endpoint: 'https://gateway.example.com', modelName: 'gateway-configured-model' },
          servedModelName: 'deployment-served-model',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
          capturedDirectProxyArgs = args;
          return new Response(JSON.stringify({
            reason: 'NotFound',
            details: { kind: 'services' },
          }), { status: 404 });
        }),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'user-requested-model',
        }),
      });

      expect(res.status).toBe(200);
      expectSseHeaders(res);
      expect(await res.text()).toBe(gatewaySse);
      expectChatProxyCall(capturedDirectProxyArgs, [
        'test-deploy-frontend',
        'default',
        8080,
        'v1/chat/completions',
        {
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'user-requested-model',
          stream: true,
        },
      ]);
      expect(capturedGatewayBody).toEqual({
        messages: [{ role: 'user', content: 'Hello' }],
        model: 'gateway-configured-model',
        stream: true,
      });
      expect(capturedGatewayHeader).toBe('gateway-configured-model');
      expect(capturedGatewaySignal).toBeInstanceOf(AbortSignal);
    });

    test('POST /api/deployments/:name/chat skips KAITO llama.cpp servedModelName and discovers model', async () => {
      let capturedModelLookupArgs: unknown[] | undefined;
      let capturedChatProxyArgs: unknown[] | undefined;
      const upstreamSse = 'data: [DONE]\n\n';

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'kaito',
          engine: 'llamacpp',
          modelId: 'deployment-model-id',
          servedModelName: 'incorrect-served-model-name',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServiceGet', async (...args: unknown[]) => {
          capturedModelLookupArgs = args;
          return JSON.stringify({ data: [{ id: 'discovered-llamacpp-model' }] });
        }),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
          capturedChatProxyArgs = args;
          return new Response(upstreamSse, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(200);
      expect(await res.text()).toBe(upstreamSse);
      expect(capturedModelLookupArgs?.slice(0, 4)).toEqual([
        'test-deploy-frontend',
        'default',
        8080,
        'v1/models',
      ]);
      const modelLookupOptions = capturedModelLookupArgs?.[4] as { accept?: string; signal?: unknown } | undefined;
      expect(modelLookupOptions?.accept).toBe('application/json');
      expect(modelLookupOptions?.signal).toBeInstanceOf(AbortSignal);
      expectChatProxyCall(capturedChatProxyArgs, [
        'test-deploy-frontend',
        'default',
        8080,
        'v1/chat/completions',
        {
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'discovered-llamacpp-model',
          stream: true,
        },
      ]);
    });

    test('POST /api/deployments/:name/chat aborts KAITO llama.cpp model discovery when the client aborts', async () => {
      let capturedModelLookupSignal: AbortSignal | undefined;
      let capturedChatProxyArgs: unknown[] | undefined;
      let resolveModelLookupStarted!: () => void;
      let resolveModelLookupAborted!: () => void;
      const modelLookupStarted = new Promise<void>((resolve) => {
        resolveModelLookupStarted = resolve;
      });
      const modelLookupAborted = new Promise<void>((resolve) => {
        resolveModelLookupAborted = resolve;
      });
      const upstreamSse = 'data: [DONE]\n\n';

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'kaito',
          engine: 'llamacpp',
          modelId: 'deployment-model-id',
          servedModelName: 'incorrect-served-model-name',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServiceGet', async (...args: unknown[]) => {
          const modelLookupOptions = args[4] as { signal?: AbortSignal } | undefined;
          capturedModelLookupSignal = modelLookupOptions?.signal;
          resolveModelLookupStarted();

          return new Promise<string>((_resolve, reject) => {
            const signal = modelLookupOptions?.signal;
            if (!signal) {
              reject(new Error('missing model discovery signal'));
              return;
            }

            const rejectAsAborted = () => {
              resolveModelLookupAborted();
              reject(new Error('aborted'));
            };

            if (signal.aborted) {
              rejectAsAborted();
              return;
            }

            signal.addEventListener('abort', rejectAsAborted, { once: true });
          });
        }),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
          capturedChatProxyArgs = args;
          return new Response(upstreamSse, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }),
      );

      const clientController = new AbortController();
      const requestPromise = app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        signal: clientController.signal,
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      await modelLookupStarted;
      expect(capturedModelLookupSignal).toBeInstanceOf(AbortSignal);
      expect(capturedModelLookupSignal?.aborted).toBe(false);

      clientController.abort();
      await modelLookupAborted;

      const res = await requestPromise;

      expect(res.status).toBe(200);
      expect(await res.text()).toBe(upstreamSse);
      expect(capturedModelLookupSignal?.aborted).toBe(true);
      expectChatProxyCall(capturedChatProxyArgs, [
        'test-deploy-frontend',
        'default',
        8080,
        'v1/chat/completions',
        {
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'deployment-model-id',
          stream: true,
        },
      ]);
      const chatProxyOptions = capturedChatProxyArgs?.[6] as { signal?: AbortSignal } | undefined;
      expect(chatProxyOptions?.signal?.aborted).toBe(true);
    });

    test('POST /api/deployments/:name/chat skips KAITO llama.cpp servedModelName and falls back to modelId', async () => {
      let capturedChatProxyArgs: unknown[] | undefined;
      const upstreamSse = 'data: [DONE]\n\n';

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'kaito',
          engine: 'llamacpp',
          modelId: 'deployment-model-id',
          servedModelName: 'incorrect-served-model-name',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServiceGet', async () => {
          throw new Error('model discovery failed');
        }),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
          capturedChatProxyArgs = args;
          return new Response(upstreamSse, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(200);
      expect(await res.text()).toBe(upstreamSse);
      expectChatProxyCall(capturedChatProxyArgs, [
        'test-deploy-frontend',
        'default',
        8080,
        'v1/chat/completions',
        {
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'deployment-model-id',
          stream: true,
        },
      ]);
    });

    test('POST /api/deployments/:name/chat defaults legacy frontend services to port 8000', async () => {
      let capturedChatProxyArgs: unknown[] | undefined;
      const upstreamSse = 'data: [DONE]\n\n';

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'legacy-frontend',
          servedModelName: 'served-from-status',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async (...args: unknown[]) => {
          capturedChatProxyArgs = args;
          return new Response(upstreamSse, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(200);
      expectChatProxyCall(capturedChatProxyArgs, [
        'legacy-frontend',
        'default',
        8000,
        'v1/chat/completions',
        {
          messages: [{ role: 'user', content: 'Hello' }],
          model: 'served-from-status',
          stream: true,
        },
      ]);
    });

    test('POST /api/deployments/:name/chat falls back to the gateway when the internal model endpoint is missing', async () => {
      let capturedGatewayUrl: string | undefined;
      let capturedGatewayBody: unknown;
      let capturedGatewayHeaders: Headers;
      let capturedGatewaySignal: AbortSignal | null | undefined;
      const originalFetch = globalThis.fetch;
      const missingServiceDetails = JSON.stringify({
        kind: 'Status',
        apiVersion: 'v1',
        status: 'Failure',
        message: 'services \"test-deploy-frontend\" not found',
        reason: 'NotFound',
        details: { name: 'test-deploy-frontend', kind: 'services' },
        code: 404,
      });
      const gatewaySse = 'data: {"choices":[{"delta":{"content":"Hello from gateway"}}]}\n\ndata: [DONE]\n\n';

      restores.push(() => {
        globalThis.fetch = originalFetch;
      });
      globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
        capturedGatewayUrl = input.toString();
        capturedGatewayBody = init?.body ? JSON.parse(init.body.toString()) : undefined;
        capturedGatewayHeaders = new Headers(init?.headers);
        capturedGatewaySignal = init?.signal;
        return new Response(gatewaySse, {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        });
      }) as typeof fetch;

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
          gateway: { endpoint: '20.92.155.15', modelName: 'served-from-gateway' },
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async () => (
          new Response(missingServiceDetails, {
            status: 404,
            headers: { 'Content-Type': 'application/json' },
          })
        )),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(200);
      expectSseHeaders(res);
      expect(await res.text()).toBe(gatewaySse);
      expect(capturedGatewayUrl).toBe('http://20.92.155.15/v1/chat/completions');
      expect(capturedGatewayHeaders!.get('X-Gateway-Model-Name')).toBe('served-from-gateway');
      expect(capturedGatewaySignal).toBeInstanceOf(AbortSignal);
      expect(capturedGatewayBody).toEqual({
        messages: [{ role: 'user', content: 'Hello' }],
        model: 'served-from-gateway',
        stream: true,
      });
    });

    test('POST /api/deployments/:name/chat preserves OpenAI-compatible gateway error messages', async () => {
      const originalFetch = globalThis.fetch;
      const missingServiceDetails = JSON.stringify({
        kind: 'Status',
        apiVersion: 'v1',
        status: 'Failure',
        message: 'services \"test-deploy-frontend\" not found',
        reason: 'NotFound',
        details: { name: 'test-deploy-frontend', kind: 'services' },
        code: 404,
      });
      const gatewayError = JSON.stringify({
        error: {
          message: 'Model served-from-gateway is not loaded by this endpoint.',
          type: 'invalid_request_error',
        },
      });

      restores.push(() => {
        globalThis.fetch = originalFetch;
      });
      globalThis.fetch = (async () => (
        new Response(gatewayError, {
          status: 400,
          headers: { 'Content-Type': 'application/json' },
        })
      )) as typeof fetch;

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
          gateway: { endpoint: '20.92.155.15', modelName: 'served-from-gateway' },
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async () => (
          new Response(missingServiceDetails, {
            status: 404,
            headers: { 'Content-Type': 'application/json' },
          })
        )),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(400);
      const data = await res.json();
      expect(data.error.message).toBe('Model served-from-gateway is not loaded by this endpoint.');
      expect(data.error.details).toBe(gatewayError);
    });

    test('POST /api/deployments/:name/chat truncates and cancels large gateway error bodies', async () => {
      const originalFetch = globalThis.fetch;
      const missingServiceDetails = JSON.stringify({
        kind: 'Status',
        apiVersion: 'v1',
        status: 'Failure',
        message: 'services "test-deploy-frontend" not found',
        reason: 'NotFound',
        details: { name: 'test-deploy-frontend', kind: 'services' },
        code: 404,
      });
      const gatewayError = createChunkedErrorStream([
        'A'.repeat(400),
        'B'.repeat(400),
        'C'.repeat(400),
        'D'.repeat(400),
      ]);

      restores.push(() => {
        globalThis.fetch = originalFetch;
      });
      globalThis.fetch = (async () => (
        new Response(gatewayError.stream, {
          status: 502,
          headers: { 'Content-Type': 'text/plain' },
        })
      )) as typeof fetch;

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
          gateway: { endpoint: '20.92.155.15', modelName: 'served-from-gateway' },
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async () => (
          new Response(missingServiceDetails, {
            status: 404,
            headers: { 'Content-Type': 'application/json' },
          })
        )),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(502);
      const data = await res.json();
      expect(data.error.details.length).toBe(1000);
      expect(data.error.details.endsWith('…')).toBe(true);
      expect(data.error.details.includes('D')).toBe(false);
      expect(gatewayError.wasCancelled()).toBe(true);
    });

    test('POST /api/deployments/:name/chat preserves upstream 401s without marking them as Airunway auth errors', async () => {
      const upstreamError = JSON.stringify({
        error: {
          message: 'Invalid upstream API key',
          type: 'authentication_error',
        },
      });

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
          servedModelName: 'served-from-status',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async () => (
          new Response(upstreamError, {
            status: 401,
            headers: { 'Content-Type': 'application/json' },
          })
        )),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(401);
      expect(res.headers.get(AIRUNWAY_AUTH_ERROR_HEADER)).toBeNull();
      const data = await res.json();
      expect(data.error.message).toBe('Invalid upstream API key');
      expect(data.error.statusCode).toBe(401);
      expect(data.error.details).toBe(upstreamError);
    });

    test('POST /api/deployments/:name/chat truncates and cancels large upstream error bodies', async () => {
      const upstreamError = createChunkedErrorStream([
        'A'.repeat(400),
        'B'.repeat(400),
        'C'.repeat(400),
        'D'.repeat(400),
      ]);

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
          servedModelName: 'served-from-status',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async () => (
          new Response(upstreamError.stream, {
            status: 500,
            headers: { 'Content-Type': 'text/plain' },
          })
        )),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(500);
      const data = await res.json();
      expect(data.error.details.length).toBe(1000);
      expect(data.error.details.endsWith('…')).toBe(true);
      expect(data.error.details.includes('D')).toBe(false);
      expect(upstreamError.wasCancelled()).toBe(true);
    });

    test('POST /api/deployments/:name/chat returns a readable message when the model endpoint is missing', async () => {
      const upstreamDetails = JSON.stringify({
        kind: 'Status',
        apiVersion: 'v1',
        status: 'Failure',
        message: 'services \"test-deploy-frontend\" not found',
        reason: 'NotFound',
        details: { name: 'test-deploy-frontend', kind: 'services' },
        code: 404,
      });

      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Running',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 1, available: 1 },
          frontendService: 'test-deploy-frontend:8080',
          servedModelName: 'served-from-status',
        } as never)),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'proxyServicePostStream', async () => (
          new Response(upstreamDetails, {
            status: 404,
            headers: { 'Content-Type': 'application/json' },
          })
        )),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(404);
      const data = await res.json();
      expect(data.error.message).toBe(
        "The model endpoint for 'test-deploy' is not available yet. The deployment may still be starting, or its endpoint may have changed. Try again in a moment or check the logs."
      );
      expect(data.error.details).toBe(upstreamDetails);
    });

    test('POST /api/deployments/:name/chat rejects deployments that are not running', async () => {
      restores.push(
        mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
      );
      restores.push(
        mockServiceMethod(kubernetesService, 'getDeployment', async () => ({
          ...mockDeployment,
          phase: 'Pending',
          provider: 'dynamo',
          replicas: { desired: 1, ready: 0, available: 0 },
          frontendService: 'test-deploy-frontend:8080',
        } as never)),
      );

      const res = await app.request('/api/deployments/test-deploy/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: [{ role: 'user', content: 'Hello' }],
        }),
      });

      expect(res.status).toBe(409);
      const data = await res.json();
      expect(data.error.message).toContain("Deployment 'test-deploy' is not running");
    });
  });

  describe('Runtimes Routes', () => {
    test('GET /api/runtimes/status returns runtimes status', async () => {
      try {
        const res = await withTimeout(app.request('/api/runtimes/status'), K8S_TEST_TIMEOUT);
        // May succeed or fail depending on k8s availability
        const status = res.status;
        expect([200, 500]).toContain(status);

        if (status === 200) {
          const data = await res.json();
          expect(data.runtimes).toBeDefined();
          expect(Array.isArray(data.runtimes)).toBe(true);
          // Validate shape of each runtime if any are returned
          for (const runtime of data.runtimes) {
            expect(runtime.id).toBeDefined();
            expect(runtime.name).toBeDefined();
            expect(typeof runtime.installed).toBe('boolean');
            expect(typeof runtime.healthy).toBe('boolean');
          }
        }
      } catch (error) {
        // If K8s is not available, the request may timeout - that's acceptable
        if (error instanceof Error && error.message.includes('timed out')) {
          console.log('Skipping test: K8s API not available (timeout)');
          return;
        }
        throw error;
      }
    });
  });

  describe('Installation Routes', () => {
    test('GET /api/installation/helm/status returns helm status', async () => {
      const res = await app.request('/api/installation/helm/status');
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.available).toBeDefined();
    });
  });

  describe('Auth Middleware', () => {
    const originalEnv = process.env.AUTH_ENABLED;

    afterEach(() => {
      if (originalEnv !== undefined) {
        process.env.AUTH_ENABLED = originalEnv;
      } else {
        delete process.env.AUTH_ENABLED;
      }
    });

    test('public routes work without auth when AUTH_ENABLED=true', async () => {
      process.env.AUTH_ENABLED = 'true';

      // Health endpoint should be public
      const healthRes = await app.request('/api/health');
      expect(healthRes.status).toBe(200);

      // Cluster status should be public (may timeout without k8s)
      try {
        const clusterRes = await withTimeout(
          app.request('/api/cluster/status'),
          K8S_TEST_TIMEOUT
        );
        expect([200, 500]).toContain(clusterRes.status); // May fail without k8s
      } catch (error) {
        if (error instanceof Error && error.message.includes('timed out')) {
          console.log('Skipping cluster status check: K8s API not available (timeout)');
        } else {
          throw error;
        }
      }

      // Settings should be public (frontend needs to check auth config)
      const settingsRes = await app.request('/api/settings');
      expect([200, 500]).toContain(settingsRes.status);
    });

    test('protected routes work without auth when AUTH_ENABLED=false', async () => {
      process.env.AUTH_ENABLED = 'false';

      // Models endpoint should work without auth
      const res = await app.request('/api/models');
      expect(res.status).toBe(200);
    });

    test('protected routes require auth when AUTH_ENABLED=true', async () => {
      process.env.AUTH_ENABLED = 'true';

      // Models endpoint should require auth
      const res = await app.request('/api/models');
      expect(res.status).toBe(401);
      expect(res.headers.get(AIRUNWAY_AUTH_ERROR_HEADER)).toBe('true');
      const data = await res.json();
      expect(data.error.message).toBe('Authentication required');
    });

    test('invalid bearer token returns 401', async () => {
      process.env.AUTH_ENABLED = 'true';

      const restoreValidate = mockServiceMethod(authService, 'validateToken', async () => ({
        valid: false,
        error: 'Invalid token',
      }));

      try {
        const res = await app.request('/api/models', {
          headers: {
            'Authorization': 'Bearer invalid-token',
          },
        });
        expect(res.status).toBe(401);
        expect(res.headers.get(AIRUNWAY_AUTH_ERROR_HEADER)).toBe('true');
      } finally {
        restoreValidate();
      }
    });

    test('health-like paths do not bypass auth when AUTH_ENABLED=true', async () => {
      process.env.AUTH_ENABLED = 'true';

      for (const path of ['/api/healthz', '/api/health/foo']) {
        const res = await app.request(path);
        expect(res.status).toBe(401);
        const data = await res.json();
        expect(data.error.message).toBe('Authentication required');
      }
    });
  });

  describe('Error Handling', () => {
    test('sanitizes 5xx HTTPException messages', async () => {
      const res = await app.request('/__test/http-exception/internal');
      expect(res.status).toBe(500);
      const data = await res.json();
      expect(data.error.message).toBe('Internal Server Error');
      expect(data.error.message).not.toContain('database password');
    });

    test('preserves 4xx HTTPException messages', async () => {
      const res = await app.request('/__test/http-exception/bad-request');
      expect(res.status).toBe(400);
      const data = await res.json();
      expect(data.error.message).toBe('client supplied an invalid value');
    });
  });

  describe('404 Handling', () => {
    test('Unknown API route returns JSON 404', async () => {
      const res = await app.request('/api/unknown');
      expect(res.status).toBe(404);
      const data = await res.json();
      expect(data.error.message).toContain('Route not found');
    });

    test('Non-API route returns SPA fallback or not found', async () => {
      const res = await app.request('/some-page');
      // Should either serve index.html (200) or return not found (404)
      expect([200, 404]).toContain(res.status);
    });
  });

  describe('HuggingFace OAuth Routes', () => {
    test('GET /api/oauth/huggingface/config returns OAuth config', async () => {
      const res = await app.request('/api/oauth/huggingface/config');
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.clientId).toBeDefined();
      expect(data.authorizeUrl).toBe('https://huggingface.co/oauth/authorize');
      expect(data.scopes).toBeDefined();
      expect(Array.isArray(data.scopes)).toBe(true);
      expect(data.scopes).toContain('openid');
      expect(data.scopes).toContain('profile');
      expect(data.scopes).toContain('read-repos');
    });

    test('POST /api/oauth/huggingface/token validates required fields', async () => {
      const res = await app.request('/api/oauth/huggingface/token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      expect(res.status).toBe(400);
    });

    test('POST /api/oauth/huggingface/token validates code verifier length', async () => {
      const res = await app.request('/api/oauth/huggingface/token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          code: 'test_code',
          codeVerifier: 'short', // Must be at least 43 characters
          redirectUri: 'http://localhost:3000/callback',
        }),
      });
      expect(res.status).toBe(400);
    });

    test('POST /api/oauth/huggingface/token validates redirect URI format', async () => {
      const res = await app.request('/api/oauth/huggingface/token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          code: 'test_code',
          codeVerifier: 'a'.repeat(50), // Valid length
          redirectUri: 'not-a-valid-url',
        }),
      });
      expect(res.status).toBe(400);
    });
  });

  describe('HuggingFace Secrets Routes', () => {
    test('GET /api/secrets/huggingface/status returns status', async () => {
      try {
        const res = await withTimeout(
          app.request('/api/secrets/huggingface/status'),
          K8S_TEST_TIMEOUT
        );
        // May fail without k8s, but should return valid response structure or 500
        const status = res.status;
        expect([200, 500]).toContain(status);

        if (status === 200) {
          const data = await res.json();
          expect(data.configured).toBeDefined();
          expect(data.namespaces).toBeDefined();
          expect(Array.isArray(data.namespaces)).toBe(true);
        }
      } catch (error) {
        // If K8s is not available, the request may timeout - that's acceptable
        if (error instanceof Error && error.message.includes('timed out')) {
          console.log('Skipping test: K8s API not available (timeout)');
          return;
        }
        throw error;
      }
    });

    test('POST /api/secrets/huggingface validates required fields', async () => {
      const res = await app.request('/api/secrets/huggingface', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      expect(res.status).toBe(400);
    });

    test('POST /api/secrets/huggingface validates access token is not empty', async () => {
      const res = await app.request('/api/secrets/huggingface', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ accessToken: '' }),
      });
      expect(res.status).toBe(400);
    });

    test('DELETE /api/secrets/huggingface route exists', async () => {
      // Mock the secretsService to avoid actually deleting secrets from a real cluster
      const { secretsService } = await import('./services/secrets');
      const originalDeleteHfSecrets = secretsService.deleteHfSecrets;

      // Replace with mock that returns success without touching K8s
      secretsService.deleteHfSecrets = async () => ({
        success: true,
        results: [{ namespace: 'test-ns', success: true }],
      });

      try {
        const res = await app.request('/api/secrets/huggingface', { method: 'DELETE' });
        expect(res.status).toBe(200);
        const data = await res.json();
        expect(data.success).toBe(true);
        expect(data.message).toBe('HuggingFace secrets deleted successfully');
      } finally {
        // Restore original function
        secretsService.deleteHfSecrets = originalDeleteHfSecrets;
      }
    });
  });
});
