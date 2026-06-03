import type { Model, ModelArchitecture } from '@airunway/shared';
import { parseParameterCountFromName } from './modelCompatibility';

/**
 * Offline inference-throughput estimator.
 *
 * Produces two rough numbers (see issue #139) without running any inference:
 *  1. per-chat tokens/sec — single-stream decode speed, memory-bandwidth bound
 *     (≈ 1/TPOT). "How snappy does chat feel?"
 *  2. concurrent capacity / aggregate tokens/sec — KV-cache-budget gated, per
 *     replica. "How many requests can this serve at once?"
 *
 * These are deliberately simple heuristics; real throughput depends on the
 * serving engine, batch scheduler, prompt lengths, and quantization. All
 * numbers are presented in the UI as estimates with a methodology disclaimer.
 */

/** 1 GiB in bytes — matches gpuValidation.ts so memory math stays consistent. */
const BYTES_PER_GIB = 1024 * 1024 * 1024;

/**
 * Fraction of theoretical memory bandwidth realised in practice. Decode kernels
 * never hit peak HBM bandwidth; ~0.8 is a common rule-of-thumb fudge factor,
 * mirroring OVERHEAD_MULTIPLIER in gpuValidation.ts.
 */
export const MEM_BW_EFFICIENCY = 0.8;

/** Per-GPU activation + workspace reserve (GiB) held back from the KV budget. */
export const DECODE_HEADROOM_GIB = 5;

/** Quantization → bytes per weight. KV bytes default to the same precision. */
export type Quantization = 'fp8' | 'int8' | 'fp16' | 'bf16';

export function bytesPerWeightFor(quantization?: string): number {
  switch ((quantization || '').toLowerCase()) {
    case 'fp8':
    case 'int8':
      return 1;
    case 'fp16':
    case 'bf16':
    default:
      return 2;
  }
}

/**
 * Resolve a numeric parameter count for a model, trying the most accurate
 * source first and falling back to parsing the name / size string.
 * Returns undefined for unknown / unparseable models (e.g. MoE strings).
 */
export function resolveParamCount(
  model: Pick<Model, 'parameterCount' | 'parameters' | 'id' | 'size'>
): number | undefined {
  if (typeof model.parameterCount === 'number' && model.parameterCount > 0) {
    return model.parameterCount;
  }
  if (typeof model.parameters === 'number' && model.parameters > 0) {
    return model.parameters;
  }
  if (model.id) {
    const fromId = parseParameterCountFromName(model.id);
    if (fromId) return fromId;
  }
  if (model.size) {
    const fromSize = parseParameterCountFromName(model.size);
    if (fromSize) return fromSize;
  }
  return undefined;
}

export interface PerChatInput {
  paramCount: number;
  bytesPerWeight: number;
  memBandwidthGBs: number;
  efficiency?: number;
}

/**
 * Single-stream decode speed (tokens/sec). Each generated token requires
 * streaming the full set of model weights from HBM, so speed ≈ bandwidth /
 * model_bytes. This is the conservative single-GPU heuristic and intentionally
 * does NOT credit tensor-parallel speedup (it answers "how fast for one user").
 *
 * Note the one decimal/binary boundary: memory bandwidth is decimal GB/s
 * (vendor spec) and model bytes are decimal (paramCount × bytesPerWeight), so
 * they divide cleanly without GiB conversion.
 */
export function estimatePerChatTokensPerSec(input: PerChatInput): number {
  const { paramCount, bytesPerWeight, memBandwidthGBs, efficiency = MEM_BW_EFFICIENCY } = input;
  const modelBytesDecimal = paramCount * bytesPerWeight; // decimal bytes
  const bandwidthBytesPerSec = memBandwidthGBs * 1e9; // decimal GB/s -> bytes/s
  if (modelBytesDecimal <= 0) return 0;
  return (bandwidthBytesPerSec / modelBytesDecimal) * efficiency;
}

export interface ConcurrentCapacityInput {
  paramCount: number;
  arch: ModelArchitecture;
  perGpuMemoryGb: number;
  tpSize: number;
  contextLen: number;
  bytesPerWeight: number;
  /** Bytes per KV value; defaults to bytesPerWeight. */
  bytesPerKv?: number;
  headroomGib?: number;
  perChatTokensPerSec: number;
}

export interface ConcurrentCapacityResult {
  concurrentSequences: number;
  aggregateTokensPerSec: number;
}

/**
 * KV-cache-budget-gated concurrent capacity, per replica (per TP group).
 *
 *   KV_budget   = (perGpuHBM - weightsPerGpu - headroom) × TP   [GiB-bytes]
 *   KV_per_seq  = contextLen × 2 × layers × kvHeads × headDim × bytesPerKv
 *   concurrent  = KV_budget / KV_per_seq
 *   aggregate   = concurrent × perChatTokensPerSec
 *
 * Returns undefined when architecture details are insufficient to size the KV
 * cache, so the caller can fall back to a per-chat-only (low-confidence) result.
 */
export function estimateConcurrentCapacity(
  input: ConcurrentCapacityInput
): ConcurrentCapacityResult | undefined {
  const {
    paramCount,
    arch,
    perGpuMemoryGb,
    tpSize,
    contextLen,
    bytesPerWeight,
    bytesPerKv = bytesPerWeight,
    headroomGib = DECODE_HEADROOM_GIB,
    perChatTokensPerSec,
  } = input;

  const { numLayers, numKvHeads, headDim } = arch;
  if (!numLayers || !numKvHeads || !headDim || tpSize <= 0 || contextLen <= 0) {
    return undefined;
  }

  // All memory math in GiB-bytes for consistency with gpuValidation.ts.
  const perGpuHbmBytes = perGpuMemoryGb * BYTES_PER_GIB;
  const headroomBytes = headroomGib * BYTES_PER_GIB;
  const weightsPerGpuBytes = (paramCount * bytesPerWeight) / tpSize;

  const kvBudgetPerGpuBytes = perGpuHbmBytes - weightsPerGpuBytes - headroomBytes;
  if (kvBudgetPerGpuBytes <= 0) {
    // Weights + headroom already exceed VRAM; no room for KV cache.
    return { concurrentSequences: 0, aggregateTokensPerSec: 0 };
  }
  const kvBudgetTotalBytes = kvBudgetPerGpuBytes * tpSize;

  const kvBytesPerToken = 2 * numLayers * numKvHeads * headDim * bytesPerKv;
  const kvBytesPerSeq = contextLen * kvBytesPerToken;
  if (kvBytesPerSeq <= 0) return undefined;

  const concurrentSequences = Math.floor(kvBudgetTotalBytes / kvBytesPerSeq);
  const aggregateTokensPerSec = Math.round(concurrentSequences * perChatTokensPerSec);

  return { concurrentSequences, aggregateTokensPerSec };
}
