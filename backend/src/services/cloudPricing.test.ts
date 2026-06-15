import { describe, test, expect, beforeEach, afterEach, mock } from 'bun:test';
import { cloudPricingService } from './cloudPricing';

// Mock fetch globally
const originalFetch = globalThis.fetch;

function mockFetch(response: unknown, options?: { ok?: boolean; status?: number }) {
  const mockFn = mock(() =>
    Promise.resolve({
      ok: options?.ok ?? true,
      status: options?.status ?? 200,
      statusText: 'OK',
      json: () => Promise.resolve(response),
    } as Response)
  );
  // @ts-expect-error - mocking fetch for tests
  globalThis.fetch = mockFn;
  return mockFn;
}

function mockFetchError(error: Error) {
  // @ts-expect-error - mocking fetch for tests
  globalThis.fetch = mock(() => Promise.reject(error));
}

describe('CloudPricingService', () => {
  beforeEach(() => {
    // Clear cache before each test
    cloudPricingService.clearCache();
  });

  afterEach(() => {
    // Restore original fetch
    globalThis.fetch = originalFetch;
  });

  describe('detectProvider', () => {
    test('detects Azure instance types', () => {
      expect(cloudPricingService.detectProvider('Standard_NC24ads_A100_v4')).toBe('azure');
      expect(cloudPricingService.detectProvider('Standard_D4s_v3')).toBe('azure');
      expect(cloudPricingService.detectProvider('Basic_A1')).toBe('azure');
    });

    test('detects AWS instance types', () => {
      expect(cloudPricingService.detectProvider('p4d.24xlarge')).toBe('aws');
      expect(cloudPricingService.detectProvider('g5.xlarge')).toBe('aws');
      expect(cloudPricingService.detectProvider('m5.large')).toBe('aws');
    });

    test('detects GCP instance types', () => {
      // GCP instances with explicit prefixes are correctly detected
      expect(cloudPricingService.detectProvider('n1-standard-4')).toBe('gcp');
      expect(cloudPricingService.detectProvider('a2-highgpu-1g')).toBe('gcp');
      expect(cloudPricingService.detectProvider('e2-medium')).toBe('gcp');
      expect(cloudPricingService.detectProvider('n2-standard-8')).toBe('gcp');
      expect(cloudPricingService.detectProvider('c2-standard-4')).toBe('gcp');
      expect(cloudPricingService.detectProvider('custom-2-4096')).toBe('gcp');
    });

    test('detects AWS instance types', () => {
      // AWS instances have letter+number followed by dot and size
      expect(cloudPricingService.detectProvider('p4d.24xlarge')).toBe('aws');
      expect(cloudPricingService.detectProvider('g5.xlarge')).toBe('aws');
      expect(cloudPricingService.detectProvider('m5.large')).toBe('aws');
    });

    test('returns undefined for unknown instance types', () => {
      // Instance types without any cloud provider pattern return undefined
      expect(cloudPricingService.detectProvider('unknownformat')).toBeUndefined();
      expect(cloudPricingService.detectProvider('')).toBeUndefined();
    });
  });

  describe('input validation', () => {
    test('rejects instance types with OData injection attempts', async () => {
      const result = await cloudPricingService.getInstancePrice(
        "Standard_NC24' or '1'='1",
        'azure',
        'eastus'
      );
      expect(result.success).toBe(false);
      expect(result.error).toContain('Invalid instance type format');
    });

    test('rejects instance types with special characters', async () => {
      const result = await cloudPricingService.getInstancePrice(
        'Standard_NC24; DROP TABLE',
        'azure',
        'eastus'
      );
      expect(result.success).toBe(false);
      expect(result.error).toContain('Invalid instance type format');
    });

    test('rejects regions with injection attempts', async () => {
      const result = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads_A100_v4',
        'azure',
        "eastus' or '1'='1"
      );
      expect(result.success).toBe(false);
      expect(result.error).toContain('Invalid region format');
    });

    test('rejects overly long instance types', async () => {
      const result = await cloudPricingService.getInstancePrice(
        'Standard_' + 'A'.repeat(100),
        'azure',
        'eastus'
      );
      expect(result.success).toBe(false);
      expect(result.error).toContain('Instance type too long');
    });

    test('accepts valid Azure instance types', async () => {
      mockFetch({ Items: [] }); // Empty response is fine, we're testing validation passes
      
      const result = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads_A100_v4',
        'azure',
        'eastus'
      );
      // Should reach the API (validation passed) even if no price found
      expect(result.error).not.toContain('Invalid');
    });

    test('accepts instance types with hyphens', async () => {
      mockFetch({ Items: [] });
      
      const result = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads-A100-v4',
        'azure',
        'eastus'
      );
      expect(result.error).not.toContain('Invalid');
    });
  });

  describe('getInstancePrice', () => {
    test('returns cached price on cache hit', async () => {
      // First call - cache miss
      const mockResponse = {
        Items: [
          {
            retailPrice: 3.5,
            currencyCode: 'USD',
            armRegionName: 'eastus',
            effectiveStartDate: '2024-01-01',
            productName: 'Virtual Machines NC Series',
            meterName: 'NC24ads A100 v4',
            unitOfMeasure: '1 Hour',
          },
        ],
      };
      const fetchMock = mockFetch(mockResponse);

      const result1 = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads_A100_v4',
        'azure',
        'eastus'
      );

      expect(result1.success).toBe(true);
      expect(result1.cached).toBe(false);
      expect(fetchMock).toHaveBeenCalledTimes(1);

      // Second call - cache hit
      const result2 = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads_A100_v4',
        'azure',
        'eastus'
      );

      expect(result2.success).toBe(true);
      expect(result2.cached).toBe(true);
      expect(fetchMock).toHaveBeenCalledTimes(1); // No additional fetch
    });

    test('fetches Azure pricing successfully', async () => {
      const mockResponse = {
        Items: [
          {
            retailPrice: 3.5,
            currencyCode: 'USD',
            armRegionName: 'eastus',
            effectiveStartDate: '2024-01-01',
            productName: 'Virtual Machines NC Series',
            meterName: 'NC24ads A100 v4',
            unitOfMeasure: '1 Hour',
          },
        ],
      };
      mockFetch(mockResponse);

      const result = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads_A100_v4',
        'azure',
        'eastus'
      );

      expect(result.success).toBe(true);
      expect(result.price).toBeDefined();
      expect(result.price!.hourlyPrice).toBe(3.5);
      expect(result.price!.currency).toBe('USD');
      expect(result.price!.provider).toBe('azure');
      expect(result.price!.region).toBe('eastus');
    });

    test('extracts GPU info for known Azure GPU instances', async () => {
      const mockResponse = {
        Items: [
          {
            retailPrice: 3.5,
            currencyCode: 'USD',
            armRegionName: 'eastus',
            effectiveStartDate: '2024-01-01',
            productName: 'Virtual Machines NC Series',
            meterName: 'NC24ads A100 v4',
            unitOfMeasure: '1 Hour',
          },
        ],
      };
      mockFetch(mockResponse);

      const result = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads_A100_v4',
        'azure',
        'eastus'
      );

      expect(result.success).toBe(true);
      expect(result.price!.gpuCount).toBe(1);
      expect(result.price!.gpuModel).toBe('A100-80GB');
    });

    test('prefers Linux pricing over Windows', async () => {
      const mockResponse = {
        Items: [
          {
            retailPrice: 5.0,
            currencyCode: 'USD',
            armRegionName: 'eastus',
            effectiveStartDate: '2024-01-01',
            productName: 'Virtual Machines NC Windows',
            meterName: 'NC24ads A100 v4',
            unitOfMeasure: '1 Hour',
          },
          {
            retailPrice: 3.5,
            currencyCode: 'USD',
            armRegionName: 'eastus',
            effectiveStartDate: '2024-01-01',
            productName: 'Virtual Machines NC Series',
            meterName: 'NC24ads A100 v4',
            unitOfMeasure: '1 Hour',
          },
        ],
      };
      mockFetch(mockResponse);

      const result = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads_A100_v4',
        'azure',
        'eastus'
      );

      expect(result.success).toBe(true);
      expect(result.price!.hourlyPrice).toBe(3.5); // Linux price, not Windows
    });

    test('handles empty response from Azure API', async () => {
      mockFetch({ Items: [] });

      const result = await cloudPricingService.getInstancePrice(
        'Standard_Unknown_Instance',
        'azure',
        'eastus'
      );

      expect(result.success).toBe(false);
      expect(result.error).toBe('Price not found');
    });

    test('handles Azure API error response', async () => {
      mockFetch({ error: 'Bad request' }, { ok: false, status: 400 });

      const result = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads_A100_v4',
        'azure',
        'eastus'
      );

      expect(result.success).toBe(false);
      expect(result.error).toContain('Azure pricing API returned 400');
    });

    test('handles network errors with retry', async () => {
      mockFetchError(new Error('Network error'));

      const result = await cloudPricingService.getInstancePrice(
        'Standard_NC24ads_A100_v4',
        'azure',
        'eastus'
      );

      expect(result.success).toBe(false);
      expect(result.error).toContain('Network error');
    });

    test('returns error for AWS (not implemented)', async () => {
      const result = await cloudPricingService.getInstancePrice('p4d.24xlarge', 'aws', 'us-east-1');

      expect(result.success).toBe(false);
      expect(result.error).toContain('AWS pricing API not yet implemented');
    });

    test('returns error for GCP (not implemented)', async () => {
      const result = await cloudPricingService.getInstancePrice(
        'a2-highgpu-1g',
        'gcp',
        'us-central1'
      );

      expect(result.success).toBe(false);
      expect(result.error).toContain('GCP pricing API not yet implemented');
    });
  });

  describe('getNodePoolPricing', () => {
    test('returns pricing for multiple node pools', async () => {
      const mockResponse = {
        Items: [
          {
            retailPrice: 3.5,
            currencyCode: 'USD',
            armRegionName: 'eastus',
            effectiveStartDate: '2024-01-01',
            productName: 'Virtual Machines NC Series',
            meterName: 'NC24ads A100 v4',
            unitOfMeasure: '1 Hour',
          },
        ],
      };
      mockFetch(mockResponse);

      const pools = [
        { name: 'gpu-pool-1', instanceType: 'Standard_NC24ads_A100_v4', region: 'eastus' },
        { name: 'gpu-pool-2', instanceType: 'Standard_NC24ads_A100_v4', region: 'eastus' },
      ];

      const results = await cloudPricingService.getNodePoolPricing(pools);

      expect(results.size).toBe(2);
      expect(results.get('gpu-pool-1')?.success).toBe(true);
      expect(results.get('gpu-pool-2')?.success).toBe(true);
    });

    test('handles pools without instance type', async () => {
      const pools = [{ name: 'no-instance-pool' }];

      const results = await cloudPricingService.getNodePoolPricing(pools);

      expect(results.get('no-instance-pool')?.success).toBe(false);
      expect(results.get('no-instance-pool')?.error).toContain('Instance type not available');
    });

    test('handles unknown provider', async () => {
      // Note: 'unknownformat' (no dashes) doesn't match any pattern
      const pools = [{ name: 'unknown-pool', instanceType: 'unknownformat' }];

      const results = await cloudPricingService.getNodePoolPricing(pools);

      expect(results.get('unknown-pool')?.success).toBe(false);
      expect(results.get('unknown-pool')?.error).toContain('Unknown provider');
    });
  });

  describe('cache management', () => {
    test('clearCache removes all cached entries', async () => {
      const mockResponse = {
        Items: [
          {
            retailPrice: 3.5,
            currencyCode: 'USD',
            armRegionName: 'eastus',
            effectiveStartDate: '2024-01-01',
            productName: 'Virtual Machines NC Series',
            meterName: 'NC24ads A100 v4',
            unitOfMeasure: '1 Hour',
          },
        ],
      };
      mockFetch(mockResponse);

      // Populate cache
      await cloudPricingService.getInstancePrice('Standard_NC24ads_A100_v4', 'azure', 'eastus');
      expect(cloudPricingService.getCacheStats().size).toBe(1);

      // Clear cache
      cloudPricingService.clearCache();
      expect(cloudPricingService.getCacheStats().size).toBe(0);
    });

    test('getCacheStats returns correct stats', () => {
      const stats = cloudPricingService.getCacheStats();
      expect(stats).toHaveProperty('size');
      expect(stats).toHaveProperty('ttlMs');
      expect(stats.ttlMs).toBe(60 * 60 * 1000); // 1 hour
    });
  });

  describe('Azure GPU info mapping', () => {
    const testCases = [
      { instance: 'Standard_NC24ads_A100_v4', gpuCount: 1, gpuModel: 'A100-80GB' },
      { instance: 'Standard_NC48ads_A100_v4', gpuCount: 2, gpuModel: 'A100-80GB' },
      { instance: 'Standard_NC96ads_A100_v4', gpuCount: 4, gpuModel: 'A100-80GB' },
      { instance: 'Standard_NC40ads_H100_v5', gpuCount: 1, gpuModel: 'H100' },
      { instance: 'Standard_NC4as_T4_v3', gpuCount: 1, gpuModel: 'T4' },
      { instance: 'Standard_NV36ads_A10_v5', gpuCount: 1, gpuModel: 'A10' },
    ];

    for (const { instance, gpuCount, gpuModel } of testCases) {
      test(`maps ${instance} to ${gpuCount}x ${gpuModel}`, async () => {
        const mockResponse = {
          Items: [
            {
              retailPrice: 3.5,
              currencyCode: 'USD',
              armRegionName: 'eastus',
              effectiveStartDate: '2024-01-01',
              productName: 'Virtual Machines NC Series',
              meterName: 'Test',
              unitOfMeasure: '1 Hour',
            },
          ],
        };
        mockFetch(mockResponse);

        const result = await cloudPricingService.getInstancePrice(instance, 'azure', 'eastus');

        expect(result.success).toBe(true);
        expect(result.price!.gpuCount).toBe(gpuCount);
        expect(result.price!.gpuModel).toBe(gpuModel);
      });
    }
  });
});
