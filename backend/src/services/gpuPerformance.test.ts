import { describe, test, expect } from 'bun:test';
import {
  resolveParamCount,
  bytesPerWeightFor,
  estimatePerChatTokensPerSec,
  estimateConcurrentCapacity,
} from './gpuPerformance';
import type { ModelArchitecture } from '@airunway/shared';

describe('resolveParamCount', () => {
  test('prefers explicit parameterCount', () => {
    expect(resolveParamCount({ parameterCount: 8_000_000_000, id: 'x/y-3B' })).toBe(8_000_000_000);
  });

  test('falls back to parameters', () => {
    expect(resolveParamCount({ parameters: 7_000_000_000, id: 'x/y' })).toBe(7_000_000_000);
  });

  test('parses from model id', () => {
    expect(resolveParamCount({ id: 'meta-llama/Meta-Llama-3-70B' })).toBe(70_000_000_000);
  });

  test('parses from size string', () => {
    expect(resolveParamCount({ id: 'x/curated', size: '3.8B' })).toBe(3_800_000_000);
    expect(resolveParamCount({ id: 'x/curated', size: '0.6B' })).toBe(600_000_000);
  });

  test('returns undefined for unknown / unparseable', () => {
    expect(resolveParamCount({ id: 'x/mystery', size: 'Unknown' })).toBeUndefined();
    expect(resolveParamCount({ id: 'org/model' })).toBeUndefined();
  });
});

describe('bytesPerWeightFor', () => {
  test('maps fp8/int8 to 1 byte', () => {
    expect(bytesPerWeightFor('fp8')).toBe(1);
    expect(bytesPerWeightFor('int8')).toBe(1);
  });

  test('defaults to 2 bytes', () => {
    expect(bytesPerWeightFor('bf16')).toBe(2);
    expect(bytesPerWeightFor('fp16')).toBe(2);
    expect(bytesPerWeightFor(undefined)).toBe(2);
  });
});

describe('estimatePerChatTokensPerSec', () => {
  test('Llama-3-70B FP8 on H100 (~38 tok/s)', () => {
    const tps = estimatePerChatTokensPerSec({
      paramCount: 70_000_000_000,
      bytesPerWeight: 1,
      memBandwidthGBs: 3350,
    });
    expect(tps).toBeGreaterThan(20);
    expect(tps).toBeLessThan(60);
  });

  test('smaller model is faster', () => {
    const big = estimatePerChatTokensPerSec({ paramCount: 70e9, bytesPerWeight: 2, memBandwidthGBs: 3350 });
    const small = estimatePerChatTokensPerSec({ paramCount: 8e9, bytesPerWeight: 2, memBandwidthGBs: 3350 });
    expect(small).toBeGreaterThan(big);
  });
});

describe('estimateConcurrentCapacity', () => {
  // Llama-3-70B architecture (GQA): 80 layers, 8 KV heads, head dim 128.
  const llama70bArch: ModelArchitecture = { numLayers: 80, numKvHeads: 8, headDim: 128 };

  test('Llama-3-70B FP8 on 4xH100-80GB / 4K context lands in expected bands', () => {
    const perChat = estimatePerChatTokensPerSec({
      paramCount: 70e9,
      bytesPerWeight: 1,
      memBandwidthGBs: 3350,
    });
    const result = estimateConcurrentCapacity({
      paramCount: 70e9,
      arch: llama70bArch,
      perGpuMemoryGb: 80,
      tpSize: 4,
      contextLen: 4096,
      bytesPerWeight: 1,
      perChatTokensPerSec: perChat,
    });
    expect(result).toBeDefined();
    expect(result!.concurrentSequences).toBeGreaterThan(250);
    expect(result!.concurrentSequences).toBeLessThan(450);
    expect(result!.aggregateTokensPerSec).toBeGreaterThan(10_000);
    expect(result!.aggregateTokensPerSec).toBeLessThan(25_000);
  });

  test('returns undefined when architecture is incomplete', () => {
    const result = estimateConcurrentCapacity({
      paramCount: 70e9,
      arch: { numLayers: 80 }, // missing kv heads / head dim
      perGpuMemoryGb: 80,
      tpSize: 4,
      contextLen: 4096,
      bytesPerWeight: 1,
      perChatTokensPerSec: 40,
    });
    expect(result).toBeUndefined();
  });

  test('longer context reduces concurrency', () => {
    const base = {
      paramCount: 70e9,
      arch: llama70bArch,
      perGpuMemoryGb: 80,
      tpSize: 4,
      bytesPerWeight: 1,
      perChatTokensPerSec: 40,
    };
    const short = estimateConcurrentCapacity({ ...base, contextLen: 4096 })!;
    const long = estimateConcurrentCapacity({ ...base, contextLen: 32768 })!;
    expect(long.concurrentSequences).toBeLessThan(short.concurrentSequences);
  });

  test('zero capacity when weights exceed VRAM', () => {
    const result = estimateConcurrentCapacity({
      paramCount: 70e9,
      arch: llama70bArch,
      perGpuMemoryGb: 24, // single small GPU, tp=1: 70GB weights don't fit
      tpSize: 1,
      contextLen: 4096,
      bytesPerWeight: 2,
      perChatTokensPerSec: 10,
    });
    expect(result!.concurrentSequences).toBe(0);
  });
});
