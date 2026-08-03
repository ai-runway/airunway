import { Hono } from 'hono';
import { zValidator } from '@hono/zod-validator';
import { z } from 'zod';
import { HTTPException } from 'hono/http-exception';
import { configService } from '../services/config';
import { authService } from '../services/auth';
import { kubernetesService } from '../services/kubernetes';
import { extractProviderDetails, extractProviderInfo } from '../lib/providers';
import logger from '../lib/logger';

const updateSettingsSchema = z.object({
  defaultNamespace: z.string().optional(),
});

const AIRUNWAY_AUTH_ERROR_HEADER = 'X-Airunway-Auth-Error';

const settings = new Hono()
  .get('/', async (c) => {
    logger.debug('Fetching settings');
    const config = await configService.getConfig();
    const providerConfigs = await kubernetesService.listInferenceProviderConfigs();

    return c.json({
      config,
      providers: providerConfigs.map(extractProviderInfo),
      auth: {
        enabled: authService.isAuthEnabled(),
      },
    });
  })
  .get('/providers', async (c) => {
    logger.debug('Fetching provider list');
    const providerConfigs = await kubernetesService.listInferenceProviderConfigs();

    return c.json({
      providers: providerConfigs.map(extractProviderInfo),
    });
  })
  .get('/providers/:id', async (c) => {
    const id = c.req.param('id');
    logger.debug({ id }, 'Fetching provider details');

    const config = await kubernetesService.getInferenceProviderConfig(id);
    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${id}` });
    }

    return c.json(extractProviderDetails(config));
  })
  .put('/', zValidator('json', updateSettingsSchema), async (c) => {
    // Settings PUT requires authentication when auth is enabled
    if (authService.isAuthEnabled()) {
      const authHeader = c.req.header('Authorization');

      if (!authHeader || !authHeader.startsWith('Bearer ')) {
        c.header(AIRUNWAY_AUTH_ERROR_HEADER, 'true');
        return c.json(
          { error: { message: 'Authentication required', statusCode: 401 } },
          401,
        );
      }

      const token = authHeader.slice(7);
      const result = await authService.validateToken(token);
      if (!result.valid) {
        c.header(AIRUNWAY_AUTH_ERROR_HEADER, 'true');
        return c.json(
          {
            error: {
              message: result.error || 'Invalid token',
              statusCode: 401,
            },
          },
          401,
        );
      }
    }

    const data = c.req.valid('json');
    logger.info({ updates: data }, 'Updating settings');

    const updatedConfig = await configService.setConfig(data);
    logger.info({ config: updatedConfig }, 'Settings updated successfully');

    return c.json({
      message: 'Settings updated successfully',
      config: updatedConfig,
    });
  });

export default settings;
