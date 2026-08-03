import { Hono } from 'hono';
import { HTTPException } from 'hono/http-exception';
import { kubernetesService } from '../services/kubernetes';
import { extractProviderDetails, extractProviderInfo } from '../lib/providers';
import logger from '../lib/logger';

const providers = new Hono()
  .get('/', async (c) => {
    logger.debug('Fetching provider list');
    const providerConfigs = await kubernetesService.listInferenceProviderConfigs();

    return c.json({
      providers: providerConfigs.map(extractProviderInfo),
    });
  })
  .get('/:id', async (c) => {
    const id = c.req.param('id');
    logger.debug({ id }, 'Fetching provider details');

    const config = await kubernetesService.getInferenceProviderConfig(id);
    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${id}` });
    }

    return c.json(extractProviderDetails(config));
  });

export default providers;
