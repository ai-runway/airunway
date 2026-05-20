import { describe, test, expect, beforeEach, afterEach } from 'bun:test';
import app, { parseCorsOrigin } from './hono-app';
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
  });

  describe('Deployments Routes', () => {
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
      const data = await res.json();
      expect(data.error.message).toBe('Authentication required');
    });

    test('invalid bearer token returns 401', async () => {
      process.env.AUTH_ENABLED = 'true';

      const res = await app.request('/api/models', {
        headers: {
          'Authorization': 'Bearer invalid-token',
        },
      });
      expect(res.status).toBe(401);
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
