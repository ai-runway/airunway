import { describe, test, expect } from 'bun:test';
import {
  normalizeGpuModel,
  estimateCost,
  estimateNodePoolCosts,
  getSupportedGpuModels,
  costEstimationService,
  getGpuInfo,
} from './costEstimation';
import type { NodePoolInfo } from '@airunway/shared';

describe('normalizeGpuModel', () => {
  test('normalizes A100-80GB variants', () => {
    expect(normalizeGpuModel('NVIDIA-A100-SXM4-80GB')).toBe('A100-80GB');
    expect(normalizeGpuModel('NVIDIA-A100-80GB-PCIe')).toBe('A100-80GB');
    expect(normalizeGpuModel('A100-80GB')).toBe('A100-80GB');
  });

  test('normalizes A100-40GB variants', () => {
    expect(normalizeGpuModel('NVIDIA-A100-SXM4-40GB')).toBe('A100-40GB');
    expect(normalizeGpuModel('NVIDIA-A100-PCIE-40GB')).toBe('A100-40GB');
    expect(normalizeGpuModel('A100')).toBe('A100-40GB');
  });

  test('normalizes H100 variants', () => {
    expect(normalizeGpuModel('NVIDIA-H100-80GB-HBM3')).toBe('H100-80GB');
    expect(normalizeGpuModel('NVIDIA-H100-SXM5-80GB')).toBe('H100-80GB');
    expect(normalizeGpuModel('H100')).toBe('H100-80GB');
  });

  test('normalizes H200 variants', () => {
    expect(normalizeGpuModel('NVIDIA-H200')).toBe('H200-141GB');
    expect(normalizeGpuModel('NVIDIA-H200-141GB-HBM3e')).toBe('H200-141GB');
    expect(normalizeGpuModel('H200')).toBe('H200-141GB');
    expect(getGpuInfo('NVIDIA-H200')?.memBandwidthGBs).toBe(4800);
  });

  test('normalizes T4 variants', () => {
    expect(normalizeGpuModel('Tesla-T4')).toBe('T4');
    expect(normalizeGpuModel('NVIDIA-Tesla-T4')).toBe('T4');
    expect(normalizeGpuModel('T4')).toBe('T4');
  });

  test('normalizes L4 variants', () => {
    expect(normalizeGpuModel('NVIDIA-L4')).toBe('L4');
    expect(normalizeGpuModel('L4')).toBe('L4');
  });

  test('normalizes L40S variants', () => {
    expect(normalizeGpuModel('NVIDIA-L40S')).toBe('L40S');
    expect(normalizeGpuModel('L40S')).toBe('L40S');
  });

  test('normalizes V100 variants', () => {
    expect(normalizeGpuModel('Tesla-V100-SXM2-16GB')).toBe('V100');
    expect(normalizeGpuModel('NVIDIA-V100')).toBe('V100');
  });

  test('returns default for unknown GPU', () => {
    // Default is now A10 (since it's a common inference GPU)
    expect(normalizeGpuModel('Unknown-GPU-Model')).toBe('A10');
    expect(normalizeGpuModel('')).toBe('A10');
  });
});

describe('getGpuInfo', () => {
  test('returns GPU info for known models', () => {
    const a100Info = getGpuInfo('A100-80GB');
    expect(a100Info).toBeDefined();
    expect(a100Info!.memoryGb).toBe(80);
    expect(a100Info!.generation).toBe('Ampere');
  });

  test('returns GPU info for normalized input', () => {
    const a100Info = getGpuInfo('NVIDIA-A100-SXM4-80GB');
    expect(a100Info).toBeDefined();
    expect(a100Info!.memoryGb).toBe(80);
  });

  test('returns undefined for unknown GPU that normalizes to default', () => {
    // Unknown GPU normalizes to A10, so we get A10 info
    const info = getGpuInfo('Unknown-GPU');
    expect(info).toBeDefined();
    expect(info!.memoryGb).toBe(24);
  });
});

describe('estimateCost (deprecated - returns low confidence)', () => {
  test('returns low confidence result for any GPU', () => {
    const result = estimateCost({
      gpuType: 'A100-80GB',
      gpuCount: 1,
      replicas: 1,
    });

    expect(result.totalGpus).toBe(1);
    expect(result.normalizedGpuModel).toBe('A100-80GB');
    expect(result.estimate.hourly).toBe(0); // No static pricing anymore
    expect(result.estimate.monthly).toBe(0);
    expect(result.estimate.currency).toBe('USD');
    expect(result.estimate.source).toBe('static');
    expect(result.estimate.confidence).toBe('low');
    expect(result.notes).toBeDefined();
    expect(result.notes!.length).toBeGreaterThan(0);
  });

  test('returns correct total GPUs for multi-GPU deployment', () => {
    const result = estimateCost({
      gpuType: 'A100-80GB',
      gpuCount: 4,
      replicas: 2,
    });

    expect(result.totalGpus).toBe(8);
    expect(result.estimate.confidence).toBe('low');
  });

  test('normalizes unknown GPU and returns low confidence', () => {
    const result = estimateCost({
      gpuType: 'Unknown-GPU',
      gpuCount: 1,
      replicas: 1,
    });

    // Should fall back to default A10
    expect(result.normalizedGpuModel).toBe('A10');
    expect(result.estimate.confidence).toBe('low');
    expect(result.notes!.some(n => n.includes('real-time pricing'))).toBe(true);
  });
});

describe('estimateNodePoolCosts (deprecated - uses cloudPricing for actual costs)', () => {
  test('returns pool info with low confidence costs', () => {
    const nodePools: NodePoolInfo[] = [
      { name: 'a100-pool', gpuCount: 8, nodeCount: 2, availableGpus: 6, gpuModel: 'NVIDIA-A100-SXM4-80GB' },
      { name: 't4-pool', gpuCount: 4, nodeCount: 2, availableGpus: 4, gpuModel: 'Tesla-T4' },
    ];

    const results = estimateNodePoolCosts(nodePools, 2, 1);

    expect(results.length).toBe(2);

    const a100Pool = results.find((r) => r.poolName === 'a100-pool');
    expect(a100Pool).toBeDefined();
    expect(a100Pool!.costBreakdown.normalizedGpuModel).toBe('A100-80GB');
    expect(a100Pool!.costBreakdown.estimate.confidence).toBe('low');

    const t4Pool = results.find((r) => r.poolName === 't4-pool');
    expect(t4Pool).toBeDefined();
    expect(t4Pool!.costBreakdown.normalizedGpuModel).toBe('T4');
  });

  test('skips pools without GPU model', () => {
    const nodePools: NodePoolInfo[] = [
      { name: 'gpu-pool', gpuCount: 4, nodeCount: 1, availableGpus: 4, gpuModel: 'NVIDIA-A100-SXM4-80GB' },
      { name: 'cpu-pool', gpuCount: 0, nodeCount: 3, availableGpus: 0 }, // No gpuModel
    ];

    const results = estimateNodePoolCosts(nodePools, 1, 1);

    expect(results.length).toBe(1);
    expect(results[0].poolName).toBe('gpu-pool');
  });
});

describe('getSupportedGpuModels', () => {
  test('returns list of supported GPU models with info', () => {
    const models = getSupportedGpuModels();

    expect(models.length).toBeGreaterThan(0);

    const a100 = models.find((m) => m.model === 'A100-80GB');
    expect(a100).toBeDefined();
    expect(a100!.memoryGb).toBe(80);
    expect(a100!.generation).toBe('Ampere');
    // No longer has avgHourlyRate (use cloudPricing for actual pricing)
  });
});

describe('costEstimationService', () => {
  test('exposes all functions', () => {
    expect(typeof costEstimationService.normalizeGpuModel).toBe('function');
    expect(typeof costEstimationService.getGpuInfo).toBe('function');
    expect(typeof costEstimationService.estimateCost).toBe('function');
    expect(typeof costEstimationService.estimateNodePoolCosts).toBe('function');
    expect(typeof costEstimationService.getSupportedGpuModels).toBe('function');
    // getPricingLastUpdated no longer exists (use cloudPricing for actual pricing)
  });
});
