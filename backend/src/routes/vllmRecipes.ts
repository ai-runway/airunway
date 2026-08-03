import { Hono } from 'hono';
import { zValidator } from '@hono/zod-validator';
import { z } from 'zod';
import { HTTPException } from 'hono/http-exception';
import type { ContentfulStatusCode } from 'hono/utils/http-status';
import type { VllmRecipeResolveRequest } from '@airunway/shared';
import {
  vllmRecipesClient,
  VllmRecipeValidationError,
  VllmRecipeTimeoutError,
} from '../services/vllmRecipesClient';
import { vllmRecipeResolver } from '../services/vllmRecipeResolver';
import logger from '../lib/logger';

// Map recipe errors to HTTP status so callers can distinguish bad input
// (4xx, do not retry) from an upstream recipes.vllm.ai outage (5xx).
function recipeErrorStatus(error: unknown): ContentfulStatusCode {
  if (error instanceof VllmRecipeValidationError) {
    return 400;
  }
  if (error instanceof VllmRecipeTimeoutError) {
    return 504;
  }
  return 502;
}

function recipeHttpException(error: unknown, fallbackMessage: string): HTTPException {
  return new HTTPException(recipeErrorStatus(error), {
    message: error instanceof Error ? error.message : fallbackMessage,
  });
}

const imageChoiceSchema = z.discriminatedUnion('type', [
  z.object({ type: z.literal('recipe') }),
  z.object({ type: z.literal('custom'), imageRef: z.string().min(1, 'imageRef is required') }),
  z.object({ type: z.literal('none') }),
]);

const resolveRequestSchema = z.object({
  modelId: z.string().min(1, 'modelId is required'),
  mode: z.enum(['aggregated', 'disaggregated']).optional(),
  hardware: z.string().min(1).optional(),
  strategy: z.string().min(1).optional(),
  variant: z.string().min(1).optional(),
  features: z.array(z.string().min(1)).optional(),
  imageChoice: imageChoiceSchema.optional(),
});

const vllmRecipes = new Hono()
  /**
   * GET /api/vllm/recipes
   * List recipe index entries from recipes.vllm.ai.
   */
  .get('/', async (c) => {
    try {
      const result = await vllmRecipesClient.list();
      return c.json(result);
    } catch (error) {
      logger.error({ error }, 'Failed to list vLLM recipes');
      throw recipeHttpException(error, 'Failed to list vLLM recipes');
    }
  })

  /**
   * POST /api/vllm/recipes/resolve
   * Resolve a vLLM recipe into Direct vLLM deployment fields.
   */
  .post('/resolve', zValidator('json', resolveRequestSchema), async (c) => {
    const request = c.req.valid('json') as VllmRecipeResolveRequest;

    try {
      const result = await vllmRecipeResolver.resolve(request);
      return c.json(result);
    } catch (error) {
      logger.error({ error, modelId: request.modelId }, 'Failed to resolve vLLM recipe');
      throw recipeHttpException(error, 'Failed to resolve vLLM recipe');
    }
  })

  /**
   * GET /api/vllm/recipes/:org/:model
   * Fetch the raw recipe payload for a Hugging Face model ID.
   *
   * `:model` is intentionally a single path segment (no `{.+}`): a Hugging Face
   * model ID is exactly `<org>/<model>`, and allowing `/` here would let crafted
   * paths traverse under the recipes base URL. `vllmRecipesClient` re-validates
   * the resulting ID as a second layer of defense.
   */
  .get('/:org/:model', async (c) => {
    const org = c.req.param('org');
    const model = c.req.param('model');

    try {
      const result = await vllmRecipesClient.get(org, model);
      return c.json(result);
    } catch (error) {
      logger.error({ error, org, model }, 'Failed to fetch vLLM recipe');
      throw recipeHttpException(error, 'Failed to fetch vLLM recipe');
    }
  });

export default vllmRecipes;
