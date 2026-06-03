import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { gpuOperatorApi, type GPUOperatorStatus, type GPUOperatorInstallResult, type ClusterGpuCapacity } from '@/lib/api'
import type { GpuThroughputEstimate } from '@airunway/shared'
import { getHfAccessToken } from './useHuggingFace'

export function useGpuOperatorStatus() {
  return useQuery<GPUOperatorStatus>({
    queryKey: ['gpu-operator-status'],
    queryFn: () => gpuOperatorApi.getStatus(),
    refetchInterval: 30000, // Refetch every 30 seconds
  })
}

export function useGpuCapacity() {
  return useQuery<ClusterGpuCapacity>({
    queryKey: ['gpu-capacity'],
    queryFn: () => gpuOperatorApi.getCapacity(),
    refetchInterval: 30000, // Refetch every 30 seconds
    staleTime: 10000, // Consider data stale after 10 seconds
  })
}

export interface ThroughputParams {
  modelId?: string
  paramCount?: number
  contextLen?: number
  quantization?: 'fp8' | 'int8' | 'fp16' | 'bf16'
  gpuModel?: string
  tpSize?: number
}

/**
 * Estimate inference throughput for a model on the cluster's GPUs.
 *
 * Pass `enabled: false` to defer the fetch (e.g. until a catalog card scrolls
 * into view) — this avoids firing an HF config.json lookup for every rendered
 * card. The query is also disabled until a GPU model and param count are known.
 */
export function useGpuThroughput(params: ThroughputParams, options?: { enabled?: boolean }) {
  const hfToken = getHfAccessToken()
  const enabled =
    (options?.enabled ?? true) && !!params.gpuModel && !!params.paramCount

  return useQuery<GpuThroughputEstimate>({
    queryKey: [
      'gpu-throughput',
      params.modelId,
      params.gpuModel,
      params.paramCount,
      params.contextLen,
      params.tpSize,
      params.quantization,
    ],
    queryFn: () => gpuOperatorApi.getThroughput(params, hfToken ?? undefined),
    enabled,
    staleTime: 5 * 60 * 1000, // estimates are stable; cache for 5 minutes
  })
}

export function useInstallGpuOperator() {
  const queryClient = useQueryClient()

  return useMutation<GPUOperatorInstallResult, Error>({
    mutationFn: () => gpuOperatorApi.install(),
    onSuccess: () => {
      // Invalidate relevant queries
      queryClient.invalidateQueries({ queryKey: ['gpu-operator-status'] })
      queryClient.invalidateQueries({ queryKey: ['gpu-capacity'] })
      queryClient.invalidateQueries({ queryKey: ['cluster-status'] })
    },
  })
}
