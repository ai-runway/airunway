import { describe, test, expect } from 'bun:test';
import app from '../hono-app';
import { buildKitService } from '../services/buildkit';
import { registryService } from '../services/registry';
import { mockServiceMethod } from '../test/helpers';

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
      const restores = [
        mockServiceMethod(registryService, 'checkStatus', async () => ({
          ready: true,
          message: 'Ready',
        }) as never),
        mockServiceMethod(buildKitService, 'getBuilderStatus', async () => ({
          exists: true,
          ready: true,
          running: true,
          message: 'Ready',
        }) as never),
      ];
      try {
        const res = await app.request('/api/aikit/infrastructure/status');
        expect(res.status).toBe(200);
        const data = await res.json();
        expect(data.ready).toBe(true);
        expect(data.registry.ready).toBe(true);
        expect(data.builder.ready).toBe(true);
      } finally {
        restores.forEach((restore) => restore());
      }
    });

    test('returns proper structure even when k8s unavailable', async () => {
      const restores = [
        mockServiceMethod(registryService, 'checkStatus', async () => {
          throw new Error('Kubernetes unavailable');
        }),
        mockServiceMethod(buildKitService, 'getBuilderStatus', async () => {
          throw new Error('Kubernetes unavailable');
        }),
      ];
      try {
        const res = await app.request('/api/aikit/infrastructure/status');
        const data = await res.json();
        expect(data.ready).toBe(false);
        expect(data.registry.ready).toBe(false);
        expect(data.builder.exists).toBe(false);
        expect(data.error).toBe('Kubernetes unavailable');
      } finally {
        restores.forEach((restore) => restore());
      }
    });
  });

  describe('POST /api/aikit/infrastructure/setup', () => {
    test('route exists and accepts POST', async () => {
      const restores = [
        mockServiceMethod(registryService, 'ensureRegistry', async () => ({
          ready: true,
          message: 'Ready',
        }) as never),
        mockServiceMethod(buildKitService, 'ensureBuilder', async () => ({
          exists: true,
          ready: true,
          running: true,
          message: 'Ready',
        }) as never),
      ];
      try {
        const res = await app.request('/api/aikit/infrastructure/setup', { method: 'POST' });
        expect(res.status).toBe(200);
        const data = await res.json();
        expect(data.success).toBe(true);
        expect(data.registry.ready).toBe(true);
        expect(data.builder.ready).toBe(true);
      } finally {
        restores.forEach((restore) => restore());
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
