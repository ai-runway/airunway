import type { ModelArchitecture } from '@airunway/shared';

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

/**
 * Tensor-parallel decode scaling efficiency. Under TP the weights are sharded
 * across `tpSize` GPUs whose HBM bandwidth aggregates, so single-stream decode
 * speeds up ~`tpSize×` — minus a per-GPU haircut for all-reduce / interconnect
 * overhead. Aggregate effective bandwidth ≈ `tpSize × perGpuBW × efficiency`.
 *
 * Real single-stream TP decode is latency/communication bound, so the per-GPU
 * haircut grows with larger TP groups (and is worse across PCIe / multi-node
 * than NVLink). We therefore step the factor down with group size rather than
 * applying a single flat value (see `tpDecodeEfficiency`): small groups stay on
 * one NVLink domain, while big groups cross domains / nodes and lose more to
 * communication. These remain coarse heuristics, consistent with the
 * estimator's overall disclaimer-backed nature.
 *
 * `TP_DECODE_EFFICIENCY` is the mid-tier value (TP 2–4, a typical single NVLink
 * domain). Larger groups use `TP_DECODE_EFFICIENCY_LARGE`.
 */
export const TP_DECODE_EFFICIENCY = 0.85;

/**
 * Decode efficiency for large TP groups (more than 4 GPUs per replica). Beyond a
 * typical 4-GPU NVLink domain the all-reduce traffic increasingly crosses slower
 * links (multi-domain NVSwitch, PCIe, or multi-node fabric), so the realised
 * per-GPU bandwidth fraction drops further. 0.75 is a deliberately rough
 * step-down for the TP≥8 regime.
 */
export const TP_DECODE_EFFICIENCY_LARGE = 0.75;

/**
 * Per-GPU decode efficiency fraction for a given tensor-parallel size. Stepped
 * by group size to approximate the growing communication haircut:
 *   - TP 1     → 1.0  (no cross-GPU all-reduce on the decode path)
 *   - TP 2–4   → TP_DECODE_EFFICIENCY (0.85; typically one NVLink domain)
 *   - TP > 4   → TP_DECODE_EFFICIENCY_LARGE (0.75; crosses domains / nodes)
 *
 * The cutover at 4 mirrors common 4-GPU NVLink partitioning; the values are
 * heuristic upper bounds, not measured constants.
 */
export function tpDecodeEfficiency(tpSize: number): number {
  if (tpSize <= 1) return 1;
  if (tpSize <= 4) return TP_DECODE_EFFICIENCY;
  return TP_DECODE_EFFICIENCY_LARGE;
}

/** Per-GPU activation + workspace reserve (GiB) held back from the KV budget. */
export const DECODE_HEADROOM_GIB = 5;

/** Quantization → bytes per weight. */
export type Quantization = 'fp8' | 'int8' | 'fp16' | 'bf16';

export function bytesPerWeightFor(quantization?: Quantization): number {
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
 * KV-cache precision. Deliberately independent of weight quantization — common
 * serving defaults keep an fp16/bf16 KV cache even when weights are fp8/int8,
 * unless KV-cache quantization is explicitly enabled.
 */
export type KvCacheDtype = 'fp8' | 'int8' | 'fp16' | 'bf16';

/**
 * Bytes per KV value for a given KV-cache dtype. Defaults to 2 (fp16/bf16) and
 * is NOT tied to weight quantization — callers must pass the KV dtype explicitly
 * to opt into a 1-byte KV cache.
 */
export function bytesPerKvFor(dtype?: KvCacheDtype): number {
  switch ((dtype || '').toLowerCase()) {
    case 'fp8':
    case 'int8':
      return 1;
    case 'fp16':
    case 'bf16':
    default:
      return 2;
  }
}

export interface PerChatInput {
  paramCount: number;
  bytesPerWeight: number;
  memBandwidthGBs: number;
  /**
   * Tensor-parallel size (GPUs per replica). Defaults to 1. With TP > 1 the
   * weights shard across `tpSize` GPUs whose HBM bandwidth aggregates, so the
   * effective decode bandwidth scales by `tpSize × tpDecodeEfficiency(tpSize)`.
   */
  tpSize?: number;
  efficiency?: number;
}

/**
 * Single-stream decode speed (tokens/sec). Each generated token requires
 * streaming the full set of model weights from HBM, so speed ≈ bandwidth /
 * model_bytes. Under tensor parallelism the weights shard across `tpSize` GPUs
 * whose HBM bandwidth aggregates, so single-stream decode scales ~`tpSize×`
 * (minus the per-group `tpDecodeEfficiency` haircut for interconnect overhead);
 * tpSize=1 reduces to the exact single-GPU figure.
 *
 * Note the one decimal/binary boundary: memory bandwidth is decimal GB/s
 * (vendor spec) and model bytes are decimal (paramCount × bytesPerWeight), so
 * they divide cleanly without GiB conversion.
 */
export function estimatePerChatTokensPerSec(input: PerChatInput): number {
  const {
    paramCount,
    bytesPerWeight,
    memBandwidthGBs,
    tpSize = 1,
    efficiency = MEM_BW_EFFICIENCY,
  } = input;
  const modelBytesDecimal = paramCount * bytesPerWeight; // decimal bytes
  // TP aggregates per-GPU bandwidth; tpSize=1 keeps the exact single-GPU number.
  // The per-GPU efficiency steps down with group size (see tpDecodeEfficiency).
  const tpScale = tpSize > 1 ? tpSize * tpDecodeEfficiency(tpSize) : 1;
  const bandwidthBytesPerSec = memBandwidthGBs * 1e9 * tpScale; // decimal GB/s -> bytes/s
  if (modelBytesDecimal <= 0) return 0;
  return (bandwidthBytesPerSec / modelBytesDecimal) * efficiency;
}

export interface DeriveTpSizeInput {
  /** Full model parameter count (not per-GPU). */
  paramCount: number;
  /** Bytes per weight for the chosen quantization (see bytesPerWeightFor). */
  bytesPerWeight: number;
  /** Per-GPU VRAM in GiB for the target GPU. */
  perGpuMemoryGb: number;
  /** Upper bound on GPUs per replica (per-node GPU count of the pool). */
  maxContiguous: number;
  /** Per-GPU activation/workspace reserve held back from VRAM (GiB). */
  headroomGib?: number;
}

/**
 * Smallest tensor-parallel size (GPUs per replica) whose per-GPU weight shard
 * leaves positive room for a KV cache, bounded by `maxContiguous`.
 *
 * Callers that know the intended TP size (the deployment form, curated cards
 * with `minGpus`) should pass it through explicitly. This is for the paths that
 * have no TP hint — notably HuggingFace search cards, whose model objects carry
 * no `minGpus` — so that a 70B model is estimated at a TP size that actually
 * fits (e.g. tp=2 on 80 GB) instead of defaulting to tp=1 and spuriously
 * reporting "does not fit". The fit test mirrors `estimateConcurrentCapacity`
 * exactly (`perGpuHBM - weights/tp - headroom > 0`) so the derived size agrees
 * with the concurrency math that follows.
 *
 * TP is doubled (1 → 2 → 4 → 8 …) rather than incremented because real
 * tensor-parallel groups are powers of two. Returns at least 1, and never more
 * than `maxContiguous`, even when the weights still don't fit at that cap (the
 * downstream estimate then legitimately reports "does not fit").
 */
export function deriveTpSizeToFitWeights(input: DeriveTpSizeInput): number {
  const {
    paramCount,
    bytesPerWeight,
    perGpuMemoryGb,
    maxContiguous,
    headroomGib = DECODE_HEADROOM_GIB,
  } = input;

  const cap = Math.max(1, Math.floor(maxContiguous || 1));
  if (paramCount <= 0 || bytesPerWeight <= 0 || perGpuMemoryGb <= 0) {
    return 1;
  }

  const usablePerGpuBytes = perGpuMemoryGb * BYTES_PER_GIB - headroomGib * BYTES_PER_GIB;
  // Even an empty (weightless) shard wouldn't fit the headroom reserve — no TP
  // size helps, so estimate at the cap and let the caller surface "does not fit".
  if (usablePerGpuBytes <= 0) return cap;

  const totalWeightBytes = paramCount * bytesPerWeight;
  let tp = 1;
  while (tp < cap && totalWeightBytes / tp >= usablePerGpuBytes) {
    tp *= 2;
  }
  return Math.min(tp, cap);
}

export interface ConcurrentCapacityInput {
  paramCount: number;
  arch: ModelArchitecture;
  perGpuMemoryGb: number;
  tpSize: number;
  contextLen: number;
  bytesPerWeight: number;
  /**
   * Bytes per KV value. Independent of weight quantization; defaults to 2
   * (fp16/bf16). Pass 1 only when an FP8/INT8 KV cache is explicitly requested
   * (and supported by the target hardware).
   */
  bytesPerKv?: number;
  headroomGib?: number;
  /**
   * Single-stream decode speed (tokens/sec). Used to derive the aggregate
   * tokens/sec figure (aggregate = concurrentSequences × perChatTokensPerSec).
   */
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
 * Aggregate throughput is the number of concurrent sequences scaled by the
 * single-stream decode rate. The single-stream `perChatTokensPerSec` is also
 * reported separately as the "per-user speed".
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
    bytesPerKv = 2,
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
  // Aggregate throughput: concurrency scaled by the single-stream decode rate.
  const aggregateTokensPerSec = Math.round(concurrentSequences * perChatTokensPerSec);

  return { concurrentSequences, aggregateTokensPerSec };
}
