import type {
  CostBreakdown,
  CostEstimateRequest,
  NodePoolCostEstimate,
} from '@airunway/shared';
import type { NodePoolInfo } from '@airunway/shared';
import { logger } from '../lib/logger';

/**
 * GPU model info for normalization (moved from static JSON)
 * This is now only used for GPU name normalization, not pricing.
 * Actual pricing comes from cloudPricing.ts via cloud provider APIs.
 */
export interface GpuModelInfo {
  aliases: string[];
  memoryGb: number;
  generation: string;
  /**
   * Peak HBM memory bandwidth in GB/s (decimal, vendor spec).
   * Used by the inference-throughput estimator (gpuPerformance.ts) — decode
   * speed is memory-bandwidth-bound, so this is the dominant per-token factor.
   */
  memBandwidthGBs: number;
}

const GPU_MODELS: Record<string, GpuModelInfo> = {
  'H200-141GB': {
    aliases: ['NVIDIA-H200', 'NVIDIA-H200-141GB-HBM3e', 'H200', 'NVIDIA H200'],
    memoryGb: 141,
    generation: 'Hopper',
    memBandwidthGBs: 4800,
  },
  'H100-80GB': {
    aliases: ['NVIDIA-H100-80GB-HBM3', 'H100', 'NVIDIA H100'],
    memoryGb: 80,
    generation: 'Hopper',
    memBandwidthGBs: 3350,
  },
  'A100-80GB': {
    aliases: ['NVIDIA-A100-SXM4-80GB', 'NVIDIA-A100-80GB-PCIe', 'A100-80GB', 'A100 80GB'],
    memoryGb: 80,
    generation: 'Ampere',
    memBandwidthGBs: 2039,
  },
  'A100-40GB': {
    aliases: ['NVIDIA-A100-SXM4-40GB', 'NVIDIA-A100-40GB-PCIe', 'A100-40GB', 'A100 40GB', 'A100'],
    memoryGb: 40,
    generation: 'Ampere',
    memBandwidthGBs: 1555,
  },
  L40S: {
    aliases: ['NVIDIA-L40S', 'L40S'],
    memoryGb: 48,
    generation: 'Ada Lovelace',
    memBandwidthGBs: 864,
  },
  L4: {
    aliases: ['NVIDIA-L4', 'L4'],
    memoryGb: 24,
    generation: 'Ada Lovelace',
    memBandwidthGBs: 300,
  },
  A10G: {
    aliases: ['NVIDIA-A10G', 'A10G', 'NVIDIA A10G'],
    memoryGb: 24,
    generation: 'Ampere',
    memBandwidthGBs: 600,
  },
  A10: {
    aliases: ['NVIDIA-A10', 'A10', 'NVIDIA A10'],
    memoryGb: 24,
    generation: 'Ampere',
    memBandwidthGBs: 600,
  },
  T4: {
    aliases: ['NVIDIA-Tesla-T4', 'Tesla-T4', 'T4', 'NVIDIA T4'],
    memoryGb: 16,
    generation: 'Turing',
    memBandwidthGBs: 320,
  },
  V100: {
    aliases: ['NVIDIA-Tesla-V100', 'Tesla-V100', 'V100', 'V100-SXM2-16GB', 'V100-PCIE-16GB'],
    memoryGb: 16,
    generation: 'Volta',
    memBandwidthGBs: 900,
  },
};

const DEFAULT_GPU = 'A10';

/**
 * Match a raw GPU label to a known GPU_MODELS key, or `undefined` when no entry
 * matches. This is the strict core shared by both the lenient and strict public
 * helpers — it never substitutes a default, so callers that must distinguish
 * "unknown GPU" from "real match" (e.g. the throughput estimator) can do so.
 *
 * @param gpuLabel - The raw GPU label from nvidia.com/gpu.product
 * @returns The matched GPU_MODELS key, or undefined if unrecognized
 */
export function normalizeKnownGpuModel(gpuLabel: string): string | undefined {
  if (!gpuLabel) {
    return undefined;
  }

  const normalizedLabel = gpuLabel.trim();

  // Check each GPU model for matching aliases
  for (const [modelKey, modelData] of Object.entries(GPU_MODELS)) {
    // Check exact match with model key
    if (normalizedLabel.toLowerCase() === modelKey.toLowerCase()) {
      return modelKey;
    }

    // Check aliases
    for (const alias of modelData.aliases) {
      if (normalizedLabel.toLowerCase() === alias.toLowerCase()) {
        return modelKey;
      }
      // Also check if the label contains the alias (for partial matches)
      if (normalizedLabel.toLowerCase().includes(alias.toLowerCase())) {
        return modelKey;
      }
    }
  }

  // Try to extract GPU model from common patterns
  // Pattern: NVIDIA-A100-SXM4-80GB -> A100-80GB
  const memoryMatch = normalizedLabel.match(/(\d+)\s*GB/i);
  const memoryGb = memoryMatch ? parseInt(memoryMatch[1], 10) : null;

  // Check for known GPU families
  const gpuFamilies = ['H200', 'H100', 'A100', 'L40S', 'L40', 'L4', 'A10G', 'A10', 'T4', 'V100', 'MI300'];
  for (const family of gpuFamilies) {
    if (normalizedLabel.toUpperCase().includes(family)) {
      // If we have memory info, try to find exact match
      if (memoryGb) {
        const modelWithMemory = `${family}-${memoryGb}GB`;
        if (GPU_MODELS[modelWithMemory]) {
          return modelWithMemory;
        }
      }
      // Return first matching model for this family
      for (const modelKey of Object.keys(GPU_MODELS)) {
        if (modelKey.startsWith(family)) {
          return modelKey;
        }
      }
    }
  }

  return undefined;
}

/**
 * Normalize GPU model name from Kubernetes node label to our GPU key.
 *
 * Lenient: unrecognized labels fall back to {@link DEFAULT_GPU} (A10). This is
 * acceptable for the legacy cost-estimation paths (which only label the result
 * and never silently report wrong specs), but NOT for the throughput estimator —
 * an A10 substitution there yields confidently-wrong speed and FP8 numbers. Use
 * {@link normalizeKnownGpuModel} when an unknown GPU must be treated as unknown.
 *
 * @param gpuLabel - The raw GPU label from nvidia.com/gpu.product
 * @returns Normalized GPU model name (defaults to A10 when unrecognized)
 */
export function normalizeGpuModel(gpuLabel: string): string {
  const matched = normalizeKnownGpuModel(gpuLabel);
  if (matched) {
    return matched;
  }
  logger.warn({ gpuLabel }, 'Could not normalize GPU model, using default');
  return DEFAULT_GPU;
}

/**
 * Get GPU model info (memory, generation) for a GPU model.
 *
 * Lenient: an unrecognized label resolves through the A10 default, so this
 * returns A10's info rather than `undefined` for unknown GPUs. For estimation
 * paths that must skip unknown hardware, use {@link getKnownGpuInfo}.
 *
 * Note: For actual pricing, use cloudPricing.ts
 */
export function getGpuInfo(gpuModel: string): GpuModelInfo | undefined {
  const normalizedModel = normalizeGpuModel(gpuModel);
  return GPU_MODELS[normalizedModel];
}

/**
 * Strict variant of {@link getGpuInfo}: returns the GPU's specs only when the
 * label maps to a known GPU_MODELS entry, and `undefined` otherwise (no A10
 * fallback). The throughput estimator uses this so a new/unsupported GPU label
 * is skipped or surfaced as "unknown" instead of silently estimated as an A10.
 */
export function getKnownGpuInfo(gpuModel: string): GpuModelInfo | undefined {
  const normalizedModel = normalizeKnownGpuModel(gpuModel);
  return normalizedModel ? GPU_MODELS[normalizedModel] : undefined;
}

/**
 * GPU generations with a native FP8 datapath (W8A8 / FP8 KV cache). vLLM gates
 * full FP8 on compute capability >= 8.9, i.e. Ada Lovelace (L40S, L4) and Hopper
 * (H100, H200). Ampere and older (A100, A10, T4, V100) lack the FP8 Tensor-Core
 * path — A100 only does weight-only W8A16 via Marlin, which is not what the KV
 * cache models — so they are excluded.
 */
const FP8_CAPABLE_GENERATIONS = new Set(['Ada Lovelace', 'Hopper']);

/**
 * Whether a GPU model has a native FP8 datapath. Used to gate FP8 KV-cache
 * sizing in the throughput estimator: only Ada Lovelace (L40S, L4) and Hopper
 * (H100, H200) have hardware FP8 support, so requesting an FP8 KV cache on older
 * generations should fall back to 2-byte (fp16/bf16) for a realistic estimate.
 *
 * Uses the strict lookup so an unknown GPU label reports `false` because we
 * genuinely don't know it supports FP8 — not because it was coerced to a
 * (non-FP8) A10 default.
 */
export function gpuSupportsFp8(gpuModel: string): boolean {
  const generation = getKnownGpuInfo(gpuModel)?.generation;
  return generation !== undefined && FP8_CAPABLE_GENERATIONS.has(generation);
}

/**
 * @deprecated Use cloudPricing.ts for real-time pricing
 * This function is kept for backward compatibility but returns low-confidence estimates
 */
export function estimateCost(request: CostEstimateRequest): CostBreakdown {
  const normalizedGpuModel = normalizeGpuModel(request.gpuType);
  const gpuInfo = GPU_MODELS[normalizedGpuModel];

  const totalGpus = request.gpuCount * request.replicas;

  // Return low-confidence result indicating that real-time pricing should be used
  return {
    estimate: {
      hourly: 0,
      monthly: 0,
      currency: 'USD',
      source: 'static',
      confidence: 'low',
    },
    perGpu: { hourly: 0, monthly: 0 },
    totalGpus,
    gpuModel: request.gpuType,
    normalizedGpuModel,
    notes: [
      'Static pricing has been removed. Use real-time pricing from cloud provider APIs.',
      gpuInfo ? `GPU: ${normalizedGpuModel} (${gpuInfo.memoryGb}GB, ${gpuInfo.generation})` : `Unknown GPU: ${request.gpuType}`,
    ],
  };
}

/**
 * @deprecated Use cloudPricing.ts for real-time pricing
 * Estimate costs for each node pool in the cluster
 */
export function estimateNodePoolCosts(
  nodePools: NodePoolInfo[],
  gpuCount: number,
  replicas: number
): NodePoolCostEstimate[] {
  return nodePools
    .filter((pool) => pool.gpuModel) // Only pools with known GPU models
    .map((pool) => {
      const costBreakdown = estimateCost({
        gpuType: pool.gpuModel!,
        gpuCount,
        replicas,
      });

      return {
        poolName: pool.name,
        gpuModel: pool.gpuModel!,
        availableGpus: pool.availableGpus,
        costBreakdown,
      };
    });
}

/**
 * Get all supported GPU models with their info
 * Note: For actual pricing, use cloudPricing.ts
 */
export function getSupportedGpuModels(): Array<{
  model: string;
  memoryGb: number;
  generation: string;
}> {
  return Object.entries(GPU_MODELS).map(([model, data]) => ({
    model,
    memoryGb: data.memoryGb,
    generation: data.generation,
  }));
}

/**
 * Cost estimation service singleton
 * Note: For actual pricing, use cloudPricingService from cloudPricing.ts
 */
export const costEstimationService = {
  normalizeGpuModel,
  normalizeKnownGpuModel,
  getGpuInfo,
  getKnownGpuInfo,
  estimateCost,
  estimateNodePoolCosts,
  getSupportedGpuModels,
};
