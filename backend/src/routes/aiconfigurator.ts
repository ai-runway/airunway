import { Hono } from 'hono';
import { z } from 'zod';
import { aiConfiguratorService } from '../services/aiconfigurator';
import type { AIConfiguratorInput } from '@airunway/shared';
import logger from '../lib/logger';

const aiconfigurator = new Hono();

/**
 * GET /api/aiconfigurator/status
 * Check if AI Configurator is available on the system
 */
aiconfigurator.get('/status', async (c) => {
  logger.debug('Checking AI Configurator status');
  const status = await aiConfiguratorService.checkStatus();
  return c.json(status);
});

/**
 * POST /api/aiconfigurator/analyze
 * Analyze a model + GPU combination and return optimal configuration
 */
const analyzeSchema = z.object({
  modelId: z.string().min(1, 'Model ID is required'),
  gpuType: z.string().min(1, 'GPU type is required'),
  gpuCount: z.number().int().min(1, 'GPU count must be at least 1'),
  optimizeFor: z.enum(['throughput', 'latency']).optional(),
  maxLatencyMs: z.number().positive().optional(),
});

aiconfigurator.post('/analyze', async (c) => {
  const body = await c.req.json();

  const validation = analyzeSchema.safeParse(body);
  if (!validation.success) {
    const errors = validation.error.issues.map(e => `${e.path.join('.')}: ${e.message}`);
    logger.warn({ errors }, 'AI Configurator analyze validation failed');
    return c.json(
      { error: { message: 'Invalid request', errors, statusCode: 400 } },
      400
    );
  }

  const input: AIConfiguratorInput = validation.data;
  logger.info({ input }, 'AI Configurator analyze request');

  const result = await aiConfiguratorService.analyze(input);

  if (!result.success) {
    logger.warn({ result }, 'AI Configurator analysis failed');
  }

  return c.json(result);
});

/**
 * POST /api/aiconfigurator/normalize-gpu
 * Normalize a GPU product string to AI Configurator format
 */
aiconfigurator.post('/normalize-gpu', async (c) => {
  const body = await c.req.json();

  if (!body.gpuProduct || typeof body.gpuProduct !== 'string') {
    return c.json(
      { error: { message: 'gpuProduct string is required', statusCode: 400 } },
      400
    );
  }

  const normalized = aiConfiguratorService.normalizeGpuType(body.gpuProduct);
  return c.json({ gpuProduct: body.gpuProduct, normalized });
});

export default aiconfigurator;
