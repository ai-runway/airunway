import { describe, test, expect } from 'bun:test';
import app from '../hono-app';

// Helper to add timeout to async operations for K8s-dependent tests
async function withTimeout<T>(promise: Promise<T>, ms: number): Promise<T> {
  const timeout = new Promise<never>((_, reject) => {
    setTimeout(() => reject(new Error(`Operation timed out after ${ms}ms`)), ms);
  });
  return Promise.race([promise, timeout]);
}

// Shorter timeout for tests that depend on K8s (which may not be available)
const K8S_TEST_TIMEOUT = 2000;

describe('AIKit Routes', () => {
  describe('GET /api/aikit/models', () => {
    test('returns list of premade models', async () => {
      const res = await app.request('/api/aikit/models');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.models).toBeDefined();
      expect(Array.isArray(data.models)).toBe(true);
      expect(data.total).toBeDefined();
      expect(typeof data.total).toBe('number');
      expect(data.total).toBeGreaterThan(0);
    });

    test('each model has required fields', async () => {
      const res = await app.request('/api/aikit/models');
      expect(res.status).toBe(200);

      const data = await res.json();
      for (const model of data.models) {
        expect(model.id).toBeDefined();
        expect(typeof model.id).toBe('string');
        expect(model.name).toBeDefined();
        expect(typeof model.name).toBe('string');
        expect(model.image).toBeDefined();
        expect(typeof model.image).toBe('string');
        expect(model.size).toBeDefined();
        expect(typeof model.size).toBe('string');
        expect(model.computeType).toBeDefined();
        expect(['cpu', 'gpu']).toContain(model.computeType);
      }
    });

    test('models include known premade models', async () => {
      const res = await app.request('/api/aikit/models');
      expect(res.status).toBe(200);

      const data = await res.json();
      const modelIds = data.models.map((m: { id: string }) => m.id);

      // Check for some known premade models (using actual IDs from PREMADE_MODELS)
      expect(modelIds).toContain('llama3.2:1b');
      expect(modelIds).toContain('phi4:14b');
      expect(modelIds).toContain('gemma2:2b');
    });

    test('cpu-capable models are marked correctly', async () => {
      const res = await app.request('/api/aikit/models');
      expect(res.status).toBe(200);

      const data = await res.json();
      // All premade AIKit models should have cpu compute type (GGUF format)
      const cpuModels = data.models.filter((m: { computeType: string }) => m.computeType === 'cpu');
      expect(cpuModels.length).toBeGreaterThan(0);
    });
  });

  describe('GET /api/aikit/models/:id', () => {
    test('returns a specific premade model', async () => {
      const res = await app.request('/api/aikit/models/llama3.2:1b');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.id).toBe('llama3.2:1b');
      expect(data.name).toBeDefined();
      expect(data.image).toBeDefined();
    });

    test('returns 404 for unknown model', async () => {
      const res = await app.request('/api/aikit/models/unknown-model-xyz');
      expect(res.status).toBe(404);

      const data = await res.json();
      expect(data.error).toBeDefined();
      expect(data.error.message).toContain('not found');
    });

    test('returns phi4:14b model', async () => {
      const res = await app.request('/api/aikit/models/phi4:14b');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.id).toBe('phi4:14b');
      expect(data.image).toContain('phi4');
    });
  });

  describe('POST /api/aikit/build', () => {
    test('validates modelSource is required', async () => {
      const res = await app.request('/api/aikit/build', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      expect(res.status).toBe(400);
    });

    test('validates modelSource is valid enum', async () => {
      const res = await app.request('/api/aikit/build', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ modelSource: 'invalid' }),
      });
      expect(res.status).toBe(400);
    });

    test('validates premade requires premadeModel', async () => {
      const res = await app.request('/api/aikit/build', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ modelSource: 'premade' }),
      });
      expect(res.status).toBe(400);

      const data = await res.json();
      expect(data.error.message).toContain('premadeModel');
    });

    test('validates huggingface requires modelId and ggufFile', async () => {
      const res = await app.request('/api/aikit/build', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ modelSource: 'huggingface' }),
      });
      expect(res.status).toBe(400);

      const data = await res.json();
      expect(data.error.message).toContain('modelId');
    });

    test('validates huggingface with only modelId still fails', async () => {
      const res = await app.request('/api/aikit/build', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          modelSource: 'huggingface',
          modelId: 'some-org/some-model',
        }),
      });
      expect(res.status).toBe(400);

      const data = await res.json();
      expect(data.error.message).toContain('ggufFile');
    });

    test('returns success for valid premade model', async () => {
      const res = await app.request('/api/aikit/build', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          modelSource: 'premade',
          premadeModel: 'llama3.2:1b',
        }),
      });
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.success).toBe(true);
      expect(data.imageRef).toBeDefined();
      expect(data.wasPremade).toBe(true);
      expect(data.imageRef).toContain('llama3.2');
    });

    test('returns error for unknown premade model', async () => {
      const res = await app.request('/api/aikit/build', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          modelSource: 'premade',
          premadeModel: 'unknown-model-xyz',
        }),
      });
      expect(res.status).toBe(400);

      const data = await res.json();
      expect(data.error.message).toContain('Unknown premade model');
    });
  });

  describe('POST /api/aikit/build/preview', () => {
    test('validates request body', async () => {
      const res = await app.request('/api/aikit/build/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      expect(res.status).toBe(400);
    });

    test('returns preview for premade model', async () => {
      const res = await app.request('/api/aikit/build/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          modelSource: 'premade',
          premadeModel: 'phi4:14b',
        }),
      });
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.imageRef).toBeDefined();
      expect(data.imageRef).toContain('phi4');
      expect(data.wasPremade).toBe(true);
      expect(data.requiresBuild).toBe(false);
    });

    test('returns preview for huggingface model', async () => {
      const res = await app.request('/api/aikit/build/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          modelSource: 'huggingface',
          modelId: 'TheBloke/Llama-2-7B-GGUF',
          ggufFile: 'llama-2-7b.Q4_K_M.gguf',
        }),
      });
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.imageRef).toBeDefined();
      expect(data.wasPremade).toBe(false);
      expect(data.requiresBuild).toBe(true);
      expect(data.registryUrl).toBeDefined();
    });

    test('returns error for unknown premade model', async () => {
      const res = await app.request('/api/aikit/build/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          modelSource: 'premade',
          premadeModel: 'unknown-model',
        }),
      });
      expect(res.status).toBe(400);
    });
  });

  describe('GET /api/aikit/infrastructure/status', () => {
    test('returns infrastructure status', async () => {
      try {
        const res = await withTimeout(
          Promise.resolve(app.request('/api/aikit/infrastructure/status')),
          K8S_TEST_TIMEOUT
        );

        // May succeed or fail depending on k8s availability
        expect([200, 500]).toContain(res.status);

        if (res.status === 200) {
          const data = await res.json();
          expect(typeof data.ready).toBe('boolean');
          expect(data.registry).toBeDefined();
          expect(data.builder).toBeDefined();
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

    test('returns proper structure even when k8s unavailable', async () => {
      try {
        const res = await withTimeout(
          Promise.resolve(app.request('/api/aikit/infrastructure/status')),
          K8S_TEST_TIMEOUT
        );

        const data = await res.json();
        // Should have these fields regardless of k8s availability
        expect(data).toHaveProperty('ready');
        expect(data).toHaveProperty('registry');
        expect(data).toHaveProperty('builder');
      } catch (error) {
        if (error instanceof Error && error.message.includes('timed out')) {
          console.log('Skipping test: K8s API not available (timeout)');
          return;
        }
        throw error;
      }
    });
  });

  describe('POST /api/aikit/infrastructure/setup', () => {
    test('route exists and accepts POST', async () => {
      try {
        const res = await withTimeout(
          Promise.resolve(app.request('/api/aikit/infrastructure/setup', { method: 'POST' })),
          K8S_TEST_TIMEOUT
        );

        // Should return 200 (success) or 500 (k8s not available)
        // Should NOT return 404 (route exists)
        expect(res.status).not.toBe(404);
        expect([200, 500]).toContain(res.status);
      } catch (error) {
        if (error instanceof Error && error.message.includes('timed out')) {
          console.log('Skipping test: K8s API not available (timeout)');
          return;
        }
        throw error;
      }
    });
  });

  describe('Model Data Integrity', () => {
    test('all models have valid image URLs', async () => {
      const res = await app.request('/api/aikit/models');
      expect(res.status).toBe(200);

      const data = await res.json();
      for (const model of data.models) {
        // Image should be a valid ghcr.io reference
        expect(model.image).toMatch(/^ghcr\.io\/kaito-project\/aikit\//);
      }
    });

    test('all models have descriptions', async () => {
      const res = await app.request('/api/aikit/models');
      expect(res.status).toBe(200);

      const data = await res.json();
      for (const model of data.models) {
        expect(model.description).toBeDefined();
        expect(model.description.length).toBeGreaterThan(0);
      }
    });

    test('all models have size information', async () => {
      const res = await app.request('/api/aikit/models');
      expect(res.status).toBe(200);

      const data = await res.json();
      for (const model of data.models) {
        expect(model.size).toBeDefined();
        expect(typeof model.size).toBe('string');
      }
    });

    test('all models have license information', async () => {
      const res = await app.request('/api/aikit/models');
      expect(res.status).toBe(200);

      const data = await res.json();
      for (const model of data.models) {
        expect(model.license).toBeDefined();
        expect(typeof model.license).toBe('string');
      }
    });
  });
});
