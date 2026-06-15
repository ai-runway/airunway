import { Hono } from 'hono';
import { autoscalerService } from '../services/autoscaler';
import logger from '../lib/logger';

const autoscaler = new Hono()
  /**
   * GET /api/autoscaler/detection
   * Detect autoscaler type and health status
   */
  .get('/detection', async (c) => {
    try {
      const result = await autoscalerService.detectAutoscaler();
      return c.json(result);
    } catch (error) {
      logger.error({ error }, 'Error detecting autoscaler');
      return c.json(
        {
          error: {
            message: error instanceof Error ? error.message : 'Failed to detect autoscaler',
            statusCode: 500,
          },
        },
        500
      );
    }
  })

  /**
   * GET /api/autoscaler/status
   * Get detailed autoscaler status from ConfigMap
   */
  .get('/status', async (c) => {
    try {
      const status = await autoscalerService.getAutoscalerStatus();

      if (!status) {
        return c.json(
          {
            error: {
              message: 'Autoscaler status not available',
              statusCode: 404,
            },
          },
          404
        );
      }

      return c.json(status);
    } catch (error) {
      logger.error({ error }, 'Error getting autoscaler status');
      return c.json(
        {
          error: {
            message: error instanceof Error ? error.message : 'Failed to get autoscaler status',
            statusCode: 500,
          },
        },
        500
      );
    }
  });

export default autoscaler;
