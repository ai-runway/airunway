import type { DeploymentConfig } from './deployment';

export const DYNAMO_ATTEMPT_ANNOTATION = 'airunway.ai/dynamo-attempt';

/** Bounded automatic-configuration contract shared with the Dynamo provider. */
export interface DynamoIntent {
  hardware: {
    totalGpus: number;
    gpuSku?: string;
    vramMb?: number;
    numGpusPerNode?: number;
  };
  searchStrategy?: 'rapid';
  workload?: { isl?: number; osl?: number; requestRate?: number; concurrency?: number };
  sla?: { ttft?: number; itl?: number; e2eLatency?: number };
}

export interface DynamoReconfigureRequest {
  resourceVersion: string;
  intent?: DynamoIntent;
  modelId?: string;
  engine?: 'vllm' | 'sglang' | 'trtllm';
}

export function isDynamoIntent(provider?: string, overrides?: Record<string, unknown>): boolean {
  return provider === 'dynamo' && overrides?.deploymentMode === 'intent';
}

export function getDynamoIntent(provider?: string, overrides?: Record<string, unknown>): DynamoIntent | undefined {
  if (!isDynamoIntent(provider, overrides)) return undefined;
  const intent = overrides?.intent as DynamoIntent | undefined;
  return intent && !Array.isArray(intent) && typeof intent.hardware?.totalGpus === 'number'
    ? intent : undefined;
}

export function defaultDynamoIntent(): DynamoIntent {
  return {
    hardware: { totalGpus: 1 },
    searchStrategy: 'rapid',
    workload: { isl: 1024, osl: 256, requestRate: 1 },
    sla: { ttft: 1000, itl: 50 },
  };
}

/** Remove form-only manual defaults from the automatic configuration wire payload. */
export function deploymentRequest(config: DeploymentConfig): Partial<DeploymentConfig> {
  if (!getDynamoIntent(config.provider, config.providerOverrides)) return config;
  const payload: Partial<DeploymentConfig> = { ...config };
  for (const key of [
    'resources', 'replicas', 'mode', 'prefillReplicas', 'decodeReplicas', 'prefillGpus', 'decodeGpus',
  ] as const) delete payload[key];
  return payload;
}
