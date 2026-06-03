import type { DetailedClusterCapacity, Model } from '@airunway/shared';
import type { ThroughputParams } from '@/hooks/useGpuOperator';

/** Parse a parameter count (absolute) from a model's fields or size string. */
export function resolveModelParamCount(
  model: Partial<Pick<Model, 'parameterCount' | 'parameters' | 'size'>> & Pick<Model, 'id'>
): number | undefined {
  if (typeof model.parameterCount === 'number' && model.parameterCount > 0) {
    return model.parameterCount;
  }
  if (typeof model.parameters === 'number' && model.parameters > 0) {
    return model.parameters;
  }
  const text = `${model.size ?? ''} ${model.id ?? ''}`;
  const bMatch = text.match(/(?:^|[-_./\s])(\d+(?:\.\d+)?)\s*B(?:$|[-_./\s])/i);
  if (bMatch) {
    return parseFloat(bMatch[1]) * 1_000_000_000;
  }
  const mMatch = text.match(/(?:^|[-_./\s])(\d+(?:\.\d+)?)\s*M(?:$|[-_./\s])/i);
  if (mMatch) {
    return parseFloat(mMatch[1]) * 1_000_000;
  }
  return undefined;
}

/** Pick the GPU model to estimate on: the pool with the most GPUs (most likely target). */
export function pickGpuModel(capacity?: DetailedClusterCapacity): string | undefined {
  const pools = (capacity?.nodePools ?? []).filter((p) => p.gpuModel);
  if (pools.length === 0) return undefined;
  return pools.reduce((best, p) => (p.gpuCount > best.gpuCount ? p : best)).gpuModel;
}

/**
 * Build throughput-estimate query params for a model on a specific GPU model.
 * Returns undefined when there's nothing useful to ask for (no GPU, no params).
 *
 * tpSize defaults to the model's minimum GPUs (bounded server-side); the Deploy
 * summary card and catalog cards use this default since they're outside the
 * deployment form where an explicit GPU-per-replica count would be chosen.
 */
export function buildThroughputParamsForGpu(
  model: Partial<Pick<Model, 'size' | 'parameterCount' | 'parameters' | 'contextLength' | 'minGpus'>> &
    Pick<Model, 'id'>,
  gpuModel?: string
): ThroughputParams | undefined {
  const paramCount = resolveModelParamCount(model);
  if (!paramCount || !gpuModel) {
    return undefined;
  }
  return {
    modelId: model.id,
    paramCount,
    contextLen: model.contextLength,
    gpuModel,
    tpSize: model.minGpus && model.minGpus > 0 ? model.minGpus : undefined,
  };
}

/**
 * Convenience for the Deploy page, which has detailed capacity (with node pools).
 */
export function buildThroughputParams(
  model: Pick<Model, 'id' | 'size' | 'parameterCount' | 'parameters' | 'contextLength' | 'minGpus'>,
  capacity?: DetailedClusterCapacity
): ThroughputParams | undefined {
  return buildThroughputParamsForGpu(model, pickGpuModel(capacity));
}
