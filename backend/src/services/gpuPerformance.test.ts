import { describe, test, expect } from 'bun:test';
import {
  bytesPerWeightFor,
  bytesPerKvFor,
  deriveTpSizeToFitWeights,
  estimatePerChatTokensPerSec,
  estimateConcurrentCapacity,
  tpDecodeEfficiency,
  TP_DECODE_EFFICIENCY,
  TP_DECODE_EFFICIENCY_LARGE,
} from './gpuPerformance';
import { resolveModelParamCount } from '@airunway/shared';
import { gpuSupportsFp8 } from './costEstimation';
import type { ModelArchitecture } from '@airunway/shared';

describe('resolveModelParamCount', () => {
  test('prefers explicit parameterCount', () => {
    expect(resolveModelParamCount({ parameterCount: 8_000_000_000, id: 'x/y-3B' })).toBe(8_000_000_000);
  });

  test('falls back to parameters', () => {
    expect(resolveModelParamCount({ parameters: 7_000_000_000, id: 'x/y' })).toBe(7_000_000_000);
  });

  test('parses from model id', () => {
    expect(resolveModelParamCount({ id: 'meta-llama/Meta-Llama-3-70B' })).toBe(70_000_000_000);
  });

  test('parses from size string', () => {
    expect(resolveModelParamCount({ id: 'x/curated', size: '3.8B' })).toBe(3_800_000_000);
    expect(resolveModelParamCount({ id: 'x/curated', size: '0.6B' })).toBe(600_000_000);
  });

  test('returns undefined for unknown / unparseable', () => {
    expect(resolveModelParamCount({ id: 'x/mystery', size: 'Unknown' })).toBeUndefined();
    expect(resolveModelParamCount({ id: 'org/model' })).toBeUndefined();
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

describe('bytesPerKvFor', () => {
  test('maps fp8/int8 to 1 byte', () => {
    expect(bytesPerKvFor('fp8')).toBe(1);
    expect(bytesPerKvFor('int8')).toBe(1);
  });

  test('defaults to 2 bytes (decoupled from weight quantization)', () => {
    expect(bytesPerKvFor('fp16')).toBe(2);
    expect(bytesPerKvFor('bf16')).toBe(2);
    expect(bytesPerKvFor(undefined)).toBe(2);
  });
});

describe('deriveTpSizeToFitWeights', () => {
  test('bumps TP until a large model fits (70B fp16 on 80GB -> tp=2)', () => {
    // 140GB of weights don't fit one 80GB GPU (75GB usable after headroom) but
    // fit across two, so the derived TP should be 2 rather than the bare default 1.
    expect(
      deriveTpSizeToFitWeights({
        paramCount: 70e9,
        bytesPerWeight: 2,
        perGpuMemoryGb: 80,
        maxContiguous: 8,
      })
    ).toBe(2);
  });

  test('stops at maxContiguous even when weights still do not fit', () => {
    // 140GB of weights never fit across 8x24GB; cap at maxContiguous and let the
    // downstream capacity estimate report "does not fit".
    expect(
      deriveTpSizeToFitWeights({
        paramCount: 70e9,
        bytesPerWeight: 2,
        perGpuMemoryGb: 24,
        maxContiguous: 8,
      })
    ).toBe(8);
  });

  test('never exceeds a single-GPU cap', () => {
    expect(
      deriveTpSizeToFitWeights({
        paramCount: 70e9,
        bytesPerWeight: 2,
        perGpuMemoryGb: 80,
        maxContiguous: 1,
      })
    ).toBe(1);
  });

  test('small model that fits one GPU stays at tp=1', () => {
    expect(
      deriveTpSizeToFitWeights({
        paramCount: 7e9,
        bytesPerWeight: 2,
        perGpuMemoryGb: 80,
        maxContiguous: 8,
      })
    ).toBe(1);
  });

  test('fp8 weights let a 70B model fit one 80GB GPU (tp=1)', () => {
    // 70GB of fp8 weights fit within 75GB usable, so no TP bump is needed.
    expect(
      deriveTpSizeToFitWeights({
        paramCount: 70e9,
        bytesPerWeight: 1,
        perGpuMemoryGb: 80,
        maxContiguous: 8,
      })
    ).toBe(1);
  });

  test('returns 1 when paramCount is unknown (<= 0)', () => {
    expect(
      deriveTpSizeToFitWeights({
        paramCount: 0,
        bytesPerWeight: 2,
        perGpuMemoryGb: 80,
        maxContiguous: 8,
      })
    ).toBe(1);
  });

  test('estimates at the cap when headroom alone exceeds VRAM', () => {
    // A tiny GPU whose headroom reserve already exceeds its VRAM: no TP size
    // helps, so fall back to the cap.
    expect(
      deriveTpSizeToFitWeights({
        paramCount: 70e9,
        bytesPerWeight: 2,
        perGpuMemoryGb: 4,
        maxContiguous: 8,
      })
    ).toBe(8);
  });

  test('derived TP fits where tp=1 reports "does not fit" (cross-tab consistency)', () => {
    // Mirrors the HF-search bug: with tp=1 a 70B model on 80GB returns
    // concurrentSequences=0 ("does not fit"); the derived TP must leave room.
    const arch: ModelArchitecture = { numLayers: 80, numKvHeads: 8, headDim: 128 };
    const atTp1 = estimateConcurrentCapacity({
      paramCount: 70e9,
      arch,
      perGpuMemoryGb: 80,
      tpSize: 1,
      contextLen: 4096,
      bytesPerWeight: 2,
      perChatTokensPerSec: 10,
    })!;
    expect(atTp1.concurrentSequences).toBe(0);

    const tp = deriveTpSizeToFitWeights({
      paramCount: 70e9,
      bytesPerWeight: 2,
      perGpuMemoryGb: 80,
      maxContiguous: 8,
    });
    const atDerived = estimateConcurrentCapacity({
      paramCount: 70e9,
      arch,
      perGpuMemoryGb: 80,
      tpSize: tp,
      contextLen: 4096,
      bytesPerWeight: 2,
      perChatTokensPerSec: 10,
    })!;
    expect(atDerived.concurrentSequences).toBeGreaterThan(0);
  });
});

describe('gpuSupportsFp8 (KV-cache FP8 gating)', () => {
  test('true only for Hopper GPUs', () => {
    expect(gpuSupportsFp8('H100')).toBe(true);
    expect(gpuSupportsFp8('NVIDIA-H200')).toBe(true);
  });

  test('false for non-Hopper generations', () => {
    expect(gpuSupportsFp8('L4')).toBe(false);
    expect(gpuSupportsFp8('L40S')).toBe(false);
    expect(gpuSupportsFp8('A100-80GB')).toBe(false);
    expect(gpuSupportsFp8('T4')).toBe(false);
    expect(gpuSupportsFp8('V100')).toBe(false);
  });

  test('false for an unknown GPU (strict lookup — not coerced to a default)', () => {
    // An unrecognized label resolves to undefined under the strict lookup, so
    // FP8 is reported unsupported because we genuinely don't know it — never
    // because it was coerced to an A10 (or, worse, mistaken for Hopper).
    expect(gpuSupportsFp8('NVIDIA-B200-192GB')).toBe(false);
    expect(gpuSupportsFp8('Some-Future-GPU')).toBe(false);
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

  test('tensor parallelism speeds up single-stream decode by ~tpSize × efficiency', () => {
    const single = estimatePerChatTokensPerSec({
      paramCount: 70e9,
      bytesPerWeight: 2,
      memBandwidthGBs: 3350,
      tpSize: 1,
    });
    const quad = estimatePerChatTokensPerSec({
      paramCount: 70e9,
      bytesPerWeight: 2,
      memBandwidthGBs: 3350,
      tpSize: 4,
    });
    expect(quad).toBeGreaterThan(single);
    // Aggregated bandwidth scales by tpSize × TP_DECODE_EFFICIENCY.
    expect(quad / single).toBeCloseTo(4 * TP_DECODE_EFFICIENCY, 5);
  });

  test('tpSize=1 matches the omitted-tpSize single-GPU number exactly', () => {
    const omitted = estimatePerChatTokensPerSec({
      paramCount: 70e9,
      bytesPerWeight: 2,
      memBandwidthGBs: 3350,
    });
    const explicit = estimatePerChatTokensPerSec({
      paramCount: 70e9,
      bytesPerWeight: 2,
      memBandwidthGBs: 3350,
      tpSize: 1,
    });
    expect(explicit).toBe(omitted);
  });

  test('tpDecodeEfficiency steps down with TP group size', () => {
    // TP1: no cross-GPU all-reduce on the decode path.
    expect(tpDecodeEfficiency(1)).toBe(1);
    // TP2–4: single NVLink-domain mid tier.
    expect(tpDecodeEfficiency(2)).toBe(TP_DECODE_EFFICIENCY);
    expect(tpDecodeEfficiency(4)).toBe(TP_DECODE_EFFICIENCY);
    // TP>4: crosses domains / nodes → larger haircut.
    expect(tpDecodeEfficiency(8)).toBe(TP_DECODE_EFFICIENCY_LARGE);
    expect(tpDecodeEfficiency(16)).toBe(TP_DECODE_EFFICIENCY_LARGE);
    // Lower tier is the optimistic bound.
    expect(TP_DECODE_EFFICIENCY).toBeGreaterThan(TP_DECODE_EFFICIENCY_LARGE);
  });

  test('large TP groups (>4) use the reduced decode efficiency tier', () => {
    const single = estimatePerChatTokensPerSec({
      paramCount: 70e9,
      bytesPerWeight: 2,
      memBandwidthGBs: 3350,
      tpSize: 1,
    });
    const octa = estimatePerChatTokensPerSec({
      paramCount: 70e9,
      bytesPerWeight: 2,
      memBandwidthGBs: 3350,
      tpSize: 8,
    });
    // TP8 scales by 8 × TP_DECODE_EFFICIENCY_LARGE, not the mid-tier 0.85.
    expect(octa / single).toBeCloseTo(8 * TP_DECODE_EFFICIENCY_LARGE, 5);
  });
});

describe('estimateConcurrentCapacity', () => {
  // Llama-3-70B architecture (GQA): 80 layers, 8 KV heads, head dim 128.
  const llama70bArch: ModelArchitecture = { numLayers: 80, numKvHeads: 8, headDim: 128 };

  test('Llama-3-70B FP8 weights + FP8 KV on 4xH100-80GB / 4K context lands in expected bands', () => {
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
      bytesPerKv: 1, // explicit FP8 KV cache (Hopper)
      perChatTokensPerSec: perChat,
    });
    expect(result).toBeDefined();
    expect(result!.concurrentSequences).toBeGreaterThan(250);
    expect(result!.concurrentSequences).toBeLessThan(450);
    // aggregate = concurrentSequences × perChatTokensPerSec (single-stream rate).
    expect(result!.aggregateTokensPerSec).toBe(
      Math.round(result!.concurrentSequences * perChat)
    );
  });

  test('aggregate scales with perChatTokensPerSec; concurrency does not', () => {
    const base = {
      paramCount: 70e9,
      arch: llama70bArch,
      perGpuMemoryGb: 80,
      tpSize: 4,
      contextLen: 4096,
      bytesPerWeight: 1,
      bytesPerKv: 1,
    };
    const slow = estimateConcurrentCapacity({ ...base, perChatTokensPerSec: 5 })!;
    const fast = estimateConcurrentCapacity({ ...base, perChatTokensPerSec: 5000 })!;
    // Concurrency is KV-budget gated, so the single-stream speed must not change it.
    expect(fast.concurrentSequences).toBe(slow.concurrentSequences);
    // Aggregate is concurrency × single-stream rate, so it tracks perChat.
    expect(slow.aggregateTokensPerSec).toBe(Math.round(slow.concurrentSequences * 5));
    expect(fast.aggregateTokensPerSec).toBe(Math.round(fast.concurrentSequences * 5000));
  });

  test('FP8 weights still use 2-byte KV by default (KV decoupled from weight quant)', () => {
    const base = {
      paramCount: 70e9,
      arch: llama70bArch,
      perGpuMemoryGb: 80,
      tpSize: 4,
      contextLen: 4096,
      bytesPerWeight: 1, // fp8 weights
      perChatTokensPerSec: 40,
    };
    // No bytesPerKv => defaults to 2 (fp16/bf16), NOT 1 (which would follow weights).
    const defaultKv = estimateConcurrentCapacity(base)!;
    const explicitFp8Kv = estimateConcurrentCapacity({ ...base, bytesPerKv: 1 })!;
    // 2-byte KV halves per-seq KV cost => ~2x concurrency vs 1-byte KV.
    expect(explicitFp8Kv.concurrentSequences).toBeGreaterThan(defaultKv.concurrentSequences);
    expect(defaultKv.concurrentSequences).toBeGreaterThan(0);
    // Roughly half (allow slack for weight/headroom terms in the budget).
    const ratio = explicitFp8Kv.concurrentSequences / defaultKv.concurrentSequences;
    expect(ratio).toBeGreaterThan(1.8);
    expect(ratio).toBeLessThan(2.2);
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
