import { Hono } from 'hono';
import { zValidator } from '@hono/zod-validator';
import { z } from 'zod';
import { HTTPException } from 'hono/http-exception';
import { aikitService, type AikitBuildRequest } from '../services/aikit';
import { registryService } from '../services/registry';
import { buildKitService } from '../services/buildkit';
import logger from '../lib/logger';

/**
 * AIKit build request schema (basic shape validation)
 * Additional validation is done in the route handlers
 */
const aikitBuildRequestSchema = z.object({
  modelSource: z.enum(['premade', 'huggingface']),
  premadeModel: z.string().optional(),
  modelId: z.string().optional(),
  ggufFile: z.string().optional(),
  imageName: z.string().optional(),
  imageTag: z.string().optional(),
});

/**
 * Validates the build request based on modelSource
 */
function validateBuildRequest(data: z.infer<typeof aikitBuildRequestSchema>): string | null {
  if (data.modelSource === 'premade') {
    if (!data.premadeModel) {
      return 'premade requires premadeModel field';
    }
  } else if (data.modelSource === 'huggingface') {
    if (!data.modelId || !data.ggufFile) {
      return 'huggingface requires modelId and ggufFile fields';
    }
  }
  return null;
}

/**
 * AIKit Routes
 * Provides endpoints for AIKit model management and image building
 */
const aikit = new Hono()
  /**
   * GET /models - List available premade models
   */
  .get('/models', (c) => {
    const models = aikitService.getPremadeModels();
    return c.json({
      models,
      total: models.length,
    });
  })

  /**
   * GET /models/:id - Get a specific premade model
   */
  .get('/models/:id', (c) => {
    const modelId = c.req.param('id');
    const model = aikitService.getPremadeModel(modelId);

    if (!model) {
      throw new HTTPException(404, {
        message: `Premade model not found: ${modelId}`,
      });
    }

    return c.json(model);
  })

  /**
   * POST /build - Build an AIKit image
   * For premade models, returns the existing image reference immediately.
   * For HuggingFace GGUF models, triggers a build using BuildKit.
   */
  .post('/build', zValidator('json', aikitBuildRequestSchema), async (c) => {
    const data = c.req.valid('json');
    
    // Validate based on modelSource
    const validationError = validateBuildRequest(data);
    if (validationError) {
      throw new HTTPException(400, { message: validationError });
    }
    
    const request = data as AikitBuildRequest;

    logger.info(
      { modelSource: request.modelSource, modelId: request.modelId || request.premadeModel },
      'AIKit build request received'
    );

    // Execute the build
    const result = await aikitService.buildImage(request);

    if (!result.success) {
      logger.error({ error: result.error }, 'AIKit build failed');
      const message = result.error || 'Build failed';
      const status = message.startsWith('Invalid build request:') ? 400 : 500;
      throw new HTTPException(status, { message });
    }

    return c.json({
      success: true,
      imageRef: result.imageRef,
      buildTime: result.buildTime,
      wasPremade: result.wasPremade,
      message: result.wasPremade
        ? 'Using premade AIKit image'
        : 'AIKit image built successfully',
    });
  })

  /**
   * POST /build/preview - Preview what image would be built (dry-run)
   * Returns the expected image reference without actually building.
   */
  .post('/build/preview', zValidator('json', aikitBuildRequestSchema), (c) => {
    const data = c.req.valid('json');
    
    // Validate based on modelSource
    const validationError = validateBuildRequest(data);
    if (validationError) {
      throw new HTTPException(400, { message: validationError });
    }
    
    const request = data as AikitBuildRequest;

    // Validate the request
    const validation = aikitService.validateBuildRequest(request);
    if (!validation.valid) {
      throw new HTTPException(400, {
        message: `Invalid request: ${validation.errors.join(', ')}`,
      });
    }

    // Get the preview image reference
    const imageRef = aikitService.getImageRef(request);

    if (!imageRef) {
      throw new HTTPException(400, {
        message: 'Unable to determine image reference for this configuration',
      });
    }

    return c.json({
      imageRef,
      wasPremade: request.modelSource === 'premade',
      requiresBuild: request.modelSource === 'huggingface',
      registryUrl: registryService.getRegistryUrl(),
    });
  })

  /**
   * GET /infrastructure/status - Get build infrastructure status
   * Returns the status of the registry and BuildKit builder.
   */
  .get('/infrastructure/status', async (c) => {
    try {
      const [registryStatus, builderStatus] = await Promise.all([
        registryService.checkStatus(),
        buildKitService.getBuilderStatus(),
      ]);

      const ready = registryStatus.ready && builderStatus.exists && builderStatus.ready;

      return c.json({
        ready,
        registry: registryStatus,
        builder: builderStatus,
      });
    } catch (error) {
      logger.error({ error }, 'Failed to check infrastructure status');
      return c.json({
        ready: false,
        registry: { ready: false, message: 'Failed to check registry status' },
        builder: { exists: false, running: false, message: 'Failed to check builder status' },
        error: error instanceof Error ? error.message : 'Unknown error',
      });
    }
  })

  /**
   * POST /infrastructure/setup - Set up build infrastructure
   * Ensures the registry and BuildKit builder are deployed and ready.
   */
  .post('/infrastructure/setup', async (c) => {
    logger.info('Setting up AIKit build infrastructure');

    try {
      // Set up registry
      const registryResult = await registryService.ensureRegistry();
      if (!registryResult.ready) {
        throw new HTTPException(500, {
          message: `Failed to set up registry: ${registryResult.message}`,
        });
      }

      // Set up builder
      const builderResult = await buildKitService.ensureBuilder();
      if (!builderResult.ready) {
        throw new HTTPException(500, {
          message: `Failed to set up builder: ${builderResult.message}`,
        });
      }

      return c.json({
        success: true,
        message: 'Build infrastructure is ready',
        registry: {
          url: registryService.getRegistryUrl(),
          ready: registryResult.ready,
        },
        builder: {
          name: buildKitService.getBuilderName(),
          ready: builderResult.ready,
        },
      });
    } catch (error) {
      if (error instanceof HTTPException) {
        throw error;
      }
      logger.error({ error }, 'Failed to set up infrastructure');
      throw new HTTPException(500, {
        message: error instanceof Error ? error.message : 'Failed to set up infrastructure',
      });
    }
  });

export default aikit;
