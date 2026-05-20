import { Hono } from 'hono';
import { zValidator } from '@hono/zod-validator';
import { z } from 'zod';
import { costEstimationService } from '../services/costEstimation';
import { cloudPricingService } from '../services/cloudPricing';
import { kubernetesService } from '../services/kubernetes';
import type { CostEstimateResponse, NodePoolCostEstimate } from '@airunway/shared';

const costEstimateRequestSchema = z.object({
  gpuType: z.string().min(1, 'GPU type is required'),
  gpuCount: z.number().int().min(1, 'GPU count must be at least 1'),
  replicas: z.number().int().min(1, 'Replicas must be at least 1'),
  hoursPerMonth: z.number().int().min(1).max(744).optional(),
});

export const costsRoutes = new Hono()
  /**
   * Estimate deployment cost based on GPU configuration
   */
  .post('/estimate', zValidator('json', costEstimateRequestSchema), async (c) => {
    const request = c.req.valid('json');

    const breakdown = costEstimationService.estimateCost(request);

    const response: CostEstimateResponse = {
      success: true,
      breakdown,
    };

    return c.json(response);
  })

  /**
   * Get cost estimates for all node pools in the cluster using real-time pricing
   */
  .get('/node-pools', async (c) => {
    const gpuCountParam = c.req.query('gpuCount');
    const replicasParam = c.req.query('replicas');
    const useRealtime = c.req.query('realtime') !== 'false'; // Default to realtime
    const computeType = c.req.query('computeType') || 'gpu'; // 'gpu' or 'cpu'

    const gpuCount = gpuCountParam ? parseInt(gpuCountParam, 10) : 1;
    const replicas = replicasParam ? parseInt(replicasParam, 10) : 1;

    // Get node pools based on compute type
    let nodePools: Array<{
      name: string;
      gpuCount: number;
      nodeCount: number;
      availableGpus: number;
      gpuModel?: string;
      instanceType?: string;
      region?: string;
    }>;

    if (computeType === 'cpu') {
      // Get all node pools and filter to CPU-only (no GPUs)
      const allPools = await kubernetesService.getAllNodePools();
      nodePools = allPools.filter(pool => pool.gpuCount === 0);
    } else {
      // Get GPU node pools (existing behavior)
      const capacity = await kubernetesService.getDetailedClusterGpuCapacity();
      nodePools = capacity.nodePools;
    }

    // Try real-time pricing first, fall back to static
    const nodePoolCosts: NodePoolCostEstimate[] = [];

    for (const pool of nodePools) {
      // For CPU pools, create a simple cost structure
      if (computeType === 'cpu') {
        if (useRealtime && pool.instanceType) {
          const provider = cloudPricingService.detectProvider(pool.instanceType);
          if (provider) {
            const result = await cloudPricingService.getInstancePrice(
              pool.instanceType,
              provider,
              pool.region
            );

            if (result.success && result.price) {
              const hourlyPrice = result.price.hourlyPrice * replicas;
              const monthlyPrice = hourlyPrice * 730;

              nodePoolCosts.push({
                poolName: pool.name,
                gpuModel: 'CPU',
                availableGpus: 0,
                costBreakdown: {
                  estimate: {
                    hourly: hourlyPrice,
                    monthly: monthlyPrice,
                    currency: 'USD',
                    source: 'cloud-api',
                    confidence: 'high',
                  },
                  perGpu: { hourly: 0, monthly: 0 },
                  totalGpus: 0,
                  gpuModel: 'CPU',
                  normalizedGpuModel: 'CPU',
                  notes: [`Real-time pricing from ${provider.toUpperCase()}`],
                },
                realtimePricing: {
                  instanceType: pool.instanceType,
                  hourlyPrice: Math.round(hourlyPrice * 100) / 100,
                  monthlyPrice: Math.round(monthlyPrice * 100) / 100,
                  currency: result.price.currency,
                  region: result.price.region,
                  source: result.cached ? 'cached' : 'realtime',
                },
              });
              continue;
            }
          }
        }
        // No realtime pricing available for this CPU pool
        continue;
      }

      // GPU pools - existing logic
      const staticEstimate = costEstimationService.estimateNodePoolCosts([pool], gpuCount, replicas)[0];

      if (useRealtime && pool.instanceType) {
        // Try to get real-time pricing
        const provider = cloudPricingService.detectProvider(pool.instanceType);
        if (provider) {
          const result = await cloudPricingService.getInstancePrice(
            pool.instanceType,
            provider,
            pool.region
          );

          if (result.success && result.price) {
            const totalGpus = gpuCount * replicas;
            const hourlyPrice = result.price.hourlyPrice * replicas; // Full VM cost per replica
            const monthlyPrice = hourlyPrice * 730;

            nodePoolCosts.push({
              ...staticEstimate,
              realtimePricing: {
                instanceType: pool.instanceType,
                hourlyPrice: Math.round(hourlyPrice * 100) / 100,
                monthlyPrice: Math.round(monthlyPrice * 100) / 100,
                currency: result.price.currency,
                region: result.price.region,
                source: result.cached ? 'cached' : 'realtime',
              },
            });
            continue;
          }
        }
      }

      // Fall back to static estimate
      nodePoolCosts.push(staticEstimate);
    }

    return c.json({
      success: true,
      nodePoolCosts,
      pricingSource: useRealtime ? 'realtime-with-fallback' : 'static',
      cacheStats: cloudPricingService.getCacheStats(),
    });
  })

  /**
   * Get real-time pricing for a specific instance type
   */
  .get('/instance-price', async (c) => {
    const instanceType = c.req.query('instanceType');
    const region = c.req.query('region');

    if (!instanceType) {
      return c.json({ success: false, error: 'instanceType is required' }, 400);
    }

    const provider = cloudPricingService.detectProvider(instanceType);
    if (!provider) {
      return c.json({
        success: false,
        error: `Could not detect cloud provider for instance type: ${instanceType}`,
      }, 400);
    }

    const result = await cloudPricingService.getInstancePrice(instanceType, provider, region);

    if (!result.success) {
      return c.json({
        success: false,
        error: result.error,
        provider,
      }, 404);
    }

    return c.json({
      success: true,
      price: result.price,
      cached: result.cached,
    });
  })

  /**
   * Get list of supported GPU models
   */
  .get('/gpu-models', (c) => {
    const models = costEstimationService.getSupportedGpuModels();

    return c.json({
      success: true,
      models,
      note: 'For actual pricing, use /costs/node-pools or /costs/instance-price for real-time cloud provider pricing',
    });
  })

  /**
   * Normalize a GPU model name and get GPU info
   */
  .get('/normalize-gpu', (c) => {
    const gpuLabel = c.req.query('label');

    if (!gpuLabel) {
      return c.json({ success: false, error: 'GPU label is required' }, 400);
    }

    const normalizedModel = costEstimationService.normalizeGpuModel(gpuLabel);
    const gpuInfo = costEstimationService.getGpuInfo(normalizedModel);

    return c.json({
      success: true,
      originalLabel: gpuLabel,
      normalizedModel,
      gpuInfo: gpuInfo
        ? {
            memoryGb: gpuInfo.memoryGb,
            generation: gpuInfo.generation,
          }
        : null,
    });
  });
