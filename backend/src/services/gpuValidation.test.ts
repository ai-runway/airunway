import { describe, test, expect } from 'bun:test';
import {
  estimateGpuMemory,
  formatGpuMemory,
  parseGpuMemory,
  calculateRequiredGpus,
  validateGpuFit,
  formatGpuWarnings,
  type GpuFitResult,
} from './gpuValidation';
import type { DeploymentConfig } from '@airunway/shared';
import type { ClusterGpuCapacity } from './kubernetes';

describe('estimateGpuMemory', () => {
  test('estimates memory for small models', () => {
    // 1B parameters * 2 bytes * 1.2 overhead = 2.4GB -> ceil to 3GB
    const result = estimateGpuMemory(1_000_000_000);
    expect(result).toBe(3);
  });

  test('estimates memory for 7B model', () => {
    // 7B * 2 * 1.2 = 16.8GB -> ceil(15.6) = 16GB
    const result = estimateGpuMemory(7_000_000_000);
    expect(result).toBe(16);
  });

  test('estimates memory for 70B model', () => {
    // 70B * 2 * 1.2 = 168GB -> 168GB (multi-GPU)
    const result = estimateGpuMemory(70_000_000_000);
    expect(result).toBe(157); // Actual calculation
  });

  test('rounds up to nearest GB', () => {
    // Small model: should round up
    const result = estimateGpuMemory(100_000_000);
    expect(result).toBeGreaterThanOrEqual(1);
  });
});

describe('formatGpuMemory', () => {
  test('formats memory with GB suffix', () => {
    expect(formatGpuMemory(16)).toBe('16GB');
    expect(formatGpuMemory(80)).toBe('80GB');
  });
});

describe('parseGpuMemory', () => {
  test('parses GB values', () => {
    expect(parseGpuMemory('16GB')).toBe(16);
    expect(parseGpuMemory('80GB')).toBe(80);
    expect(parseGpuMemory('16 GB')).toBe(16);
  });

  test('parses MB values', () => {
    expect(parseGpuMemory('8192MB')).toBe(8);
    expect(parseGpuMemory('16384MB')).toBe(16);
  });

  test('parses TB values', () => {
    expect(parseGpuMemory('1TB')).toBe(1024);
  });

  test('parses values without units (defaults to GB)', () => {
    expect(parseGpuMemory('16')).toBe(16);
  });

  test('parses decimal values', () => {
    expect(parseGpuMemory('24.5GB')).toBe(24.5);
  });

  test('returns undefined for invalid input', () => {
    expect(parseGpuMemory('invalid')).toBeUndefined();
    expect(parseGpuMemory('')).toBeUndefined();
    expect(parseGpuMemory('GB')).toBeUndefined();
  });
});

describe('calculateRequiredGpus', () => {
  const baseConfig: DeploymentConfig = {
    name: 'test',
    namespace: 'test-ns',
    modelId: 'test/model',
    engine: 'vllm',
    mode: 'aggregated',
    routerMode: 'default',
    replicas: 1,
    hfTokenSecret: 'secret',
    enforceEager: true,
    enablePrefixCaching: false,
    trustRemoteCode: false,
  };

  test('calculates for aggregated mode with 1 replica', () => {
    const result = calculateRequiredGpus({ ...baseConfig, replicas: 1 });
    expect(result.total).toBe(1);
    expect(result.maxPerWorker).toBe(1);
  });

  test('calculates for aggregated mode with multiple replicas', () => {
    const result = calculateRequiredGpus({
      ...baseConfig,
      replicas: 3,
      resources: { gpu: 2 },
    });
    expect(result.total).toBe(6); // 3 replicas * 2 GPUs
    expect(result.maxPerWorker).toBe(2);
  });

  test('calculates for aggregated mode with providerOverrides nodeCount', () => {
    const result = calculateRequiredGpus({
      ...baseConfig,
      replicas: 1,
      resources: { gpu: 1 },
      providerOverrides: {
        spec: {
          services: {
            VllmWorker: {
              multinode: { nodeCount: 2 }
            }
          }
        }
      },
    });
    expect(result.total).toBe(2); // 1 replica * 1 GPU * 2 nodes
    expect(result.maxPerWorker).toBe(1);
  });

  test('calculates for aggregated mode with providerOverrides nodeCount and multiple replicas', () => {
    const result = calculateRequiredGpus({
      ...baseConfig,
      replicas: 3,
      resources: { gpu: 4 },
      providerOverrides: {
        spec: {
          services: {
            VllmWorker: {
              multinode: { nodeCount: 2 }
            }
          }
        }
      },
    });
    expect(result.total).toBe(24); // 3 replicas * 4 GPUs * 2 nodes
  });

  test('calculates for disaggregated mode', () => {
    const result = calculateRequiredGpus({
      ...baseConfig,
      mode: 'disaggregated',
      prefillReplicas: 2,
      decodeReplicas: 4,
      prefillGpus: 2,
      decodeGpus: 1,
    });
    expect(result.total).toBe(8); // (2*2) + (4*1)
    expect(result.maxPerWorker).toBe(2); // max of prefill/decode
    expect(result.prefillPerWorker).toBe(2);
    expect(result.decodePerWorker).toBe(1);
  });

  test('treats CPU deployments as requiring zero GPUs', () => {
    const result = calculateRequiredGpus({
      ...baseConfig,
      provider: 'kaito',
      engine: 'llamacpp',
      computeType: 'cpu',
      resources: { gpu: 4 },
    });

    expect(result.total).toBe(0);
    expect(result.maxPerWorker).toBe(0);
    expect(result.prefillPerWorker).toBe(0);
    expect(result.decodePerWorker).toBe(0);
  });
});

describe('validateGpuFit', () => {
  const baseConfig: DeploymentConfig = {
    name: 'test',
    namespace: 'test-ns',
    modelId: 'test/model',
    engine: 'vllm',
    mode: 'aggregated',
    routerMode: 'default',
    replicas: 1,
    hfTokenSecret: 'secret',
    enforceEager: true,
    enablePrefixCaching: false,
    trustRemoteCode: false,
  };

  const clusterWithCapacity = (available: number, maxContiguous: number): ClusterGpuCapacity => ({
    totalGpus: available + 2,
    allocatedGpus: 2,
    availableGpus: available,
    maxContiguousAvailable: maxContiguous,
    nodes: [],
  });

  test('fits when sufficient GPUs available', () => {
    const result = validateGpuFit(
      { ...baseConfig, replicas: 2 },
      clusterWithCapacity(4, 4)
    );
    expect(result.fits).toBe(true);
    expect(result.warnings).toHaveLength(0);
  });

  test('warns when total GPUs insufficient', () => {
    const result = validateGpuFit(
      { ...baseConfig, replicas: 4, resources: { gpu: 2 } },
      clusterWithCapacity(4, 4)
    );
    expect(result.fits).toBe(false);
    expect(result.warnings.some(w => w.type === 'total_insufficient')).toBe(true);
  });

  test('warns when contiguous GPUs insufficient', () => {
    const result = validateGpuFit(
      { ...baseConfig, replicas: 1, resources: { gpu: 4 } },
      clusterWithCapacity(8, 2) // 8 total but max 2 per node
    );
    expect(result.fits).toBe(false);
    expect(result.warnings.some(w => w.type === 'contiguous_insufficient')).toBe(true);
  });

  test('warns when configured GPUs below model minimum', () => {
    const result = validateGpuFit(
      { ...baseConfig, replicas: 1, resources: { gpu: 1 } },
      clusterWithCapacity(8, 8),
      4 // model requires 4 GPUs minimum
    );
    expect(result.fits).toBe(false);
    expect(result.warnings.some(w => w.type === 'model_minimum')).toBe(true);
  });

  test('multiple warnings can occur', () => {
    const result = validateGpuFit(
      { ...baseConfig, replicas: 4, resources: { gpu: 4 } },
      clusterWithCapacity(2, 1),
      2
    );
    expect(result.fits).toBe(false);
    expect(result.warnings.length).toBeGreaterThanOrEqual(2);
  });

  test('skips GPU warnings for CPU deployments', () => {
    const result = validateGpuFit(
      {
        ...baseConfig,
        provider: 'kaito',
        engine: 'llamacpp',
        computeType: 'cpu',
        resources: { gpu: 4 },
      },
      clusterWithCapacity(0, 0),
      4
    );

    expect(result.fits).toBe(true);
    expect(result.warnings).toHaveLength(0);
  });
});

describe('formatGpuWarnings', () => {
  test('formats total insufficient warning', () => {
    const result: GpuFitResult = {
      fits: false,
      warnings: [{
        type: 'total_insufficient',
        message: 'Not enough GPUs',
        required: 8,
        available: 4,
      }],
    };
    const formatted = formatGpuWarnings(result);
    expect(formatted[0]).toContain('⚠️');
    expect(formatted[0]).toContain('Insufficient cluster GPUs');
  });

  test('formats contiguous insufficient warning', () => {
    const result: GpuFitResult = {
      fits: false,
      warnings: [{
        type: 'contiguous_insufficient',
        message: 'No node has enough GPUs',
        required: 4,
        available: 2,
      }],
    };
    const formatted = formatGpuWarnings(result);
    expect(formatted[0]).toContain('Scheduling constraint');
  });

  test('formats model minimum warning', () => {
    const result: GpuFitResult = {
      fits: false,
      warnings: [{
        type: 'model_minimum',
        message: 'Model needs more GPUs',
        required: 4,
        available: 1,
      }],
    };
    const formatted = formatGpuWarnings(result);
    expect(formatted[0]).toContain('Model requirement');
  });
});
