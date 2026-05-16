import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { deploymentsApi, type DeploymentConfig, type DeploymentStatus } from '@/lib/api'
import { useState, useCallback } from 'react'

/**
 * Granular status for deployment operations
 * Provides more detailed feedback than simple isPending boolean
 */
export type DeploymentMutationStatus = 
  | 'idle'
  | 'validating'
  | 'submitting'
  | 'success'
  | 'error'

export function useDeployments(namespace?: string) {
  return useQuery({
    queryKey: ['deployments', namespace],
    queryFn: () => deploymentsApi.list(namespace),
    select: (data) => data.deployments,
    refetchInterval: 10000, // Refresh every 10 seconds
  })
}

export function useDeployment(name: string | undefined, namespace?: string) {
  return useQuery({
    queryKey: ['deployment', name, namespace],
    queryFn: () => deploymentsApi.get(name!, namespace),
    enabled: !!name,
    refetchInterval: 5000, // Refresh every 5 seconds
  })
}

export function useDeploymentPods(name: string | undefined, namespace?: string) {
  return useQuery({
    queryKey: ['deployment-pods', name, namespace],
    queryFn: () => deploymentsApi.getPods(name!, namespace),
    select: (data) => data.pods,
    enabled: !!name,
    refetchInterval: 5000,
  })
}

export function useDeploymentLogs(
  name: string | undefined,
  namespace?: string,
  options?: { podName?: string; container?: string; tailLines?: number; timestamps?: boolean }
) {
  return useQuery({
    queryKey: ['deployment-logs', name, namespace, options?.podName, options?.container, options?.tailLines, options?.timestamps],
    queryFn: () => deploymentsApi.getLogs(name!, namespace, options),
    enabled: !!name && !!options?.podName, // Require both name and podName
    refetchInterval: 10000, // Refresh logs every 10 seconds
    staleTime: 5000,
    retry: 1, // Only retry once on failure
  })
}

/**
 * Hook to fetch manifests for a deployment
 * Returns all resources including the main CR and related resources (Services, ConfigMaps, etc.)
 */
export function useDeploymentManifest(name: string | undefined, namespace?: string) {
  return useQuery({
    queryKey: ['deployment-manifest', name, namespace],
    queryFn: () => deploymentsApi.getManifest(name!, namespace),
    enabled: !!name,
    staleTime: 30000, // Cache for 30 seconds (manifests don't change often)
    retry: 1,
  })
}

/**
 * Enhanced create deployment hook with granular status tracking
 * Provides status: 'idle' | 'validating' | 'submitting' | 'success' | 'error'
 */
export function useCreateDeployment() {
  const queryClient = useQueryClient()
  const [status, setStatus] = useState<DeploymentMutationStatus>('idle')

  const mutation = useMutation({
    mutationFn: async (config: DeploymentConfig) => {
      // Validation phase
      setStatus('validating')
      await new Promise(resolve => setTimeout(resolve, 300)) // Brief validation delay for UX
      
      // Submission phase
      setStatus('submitting')
      return deploymentsApi.create(config)
    },
    onSuccess: () => {
      setStatus('success')
      queryClient.invalidateQueries({ queryKey: ['deployments'] })
      // Reset to idle after a brief success state
      setTimeout(() => setStatus('idle'), 2000)
    },
    onError: () => {
      setStatus('error')
      // Reset to idle after error acknowledged
      setTimeout(() => setStatus('idle'), 3000)
    },
  })

  const reset = useCallback(() => {
    setStatus('idle')
    mutation.reset()
  }, [mutation])

  return {
    ...mutation,
    status,
    reset,
    // Convenience booleans for common checks
    isValidating: status === 'validating',
    isSubmitting: status === 'submitting',
    isProcessing: status === 'validating' || status === 'submitting',
  }
}

/**
 * Enhanced delete deployment hook with granular status tracking
 */
export function useDeleteDeployment() {
  const queryClient = useQueryClient()
  const [status, setStatus] = useState<DeploymentMutationStatus>('idle')

  const mutation = useMutation({
    mutationFn: async ({ name, namespace }: { name: string; namespace?: string }) => {
      setStatus('submitting')
      return deploymentsApi.delete(name, namespace)
    },
    onSuccess: () => {
      setStatus('success')
      queryClient.invalidateQueries({ queryKey: ['deployments'] })
      setTimeout(() => setStatus('idle'), 1000)
    },
    onError: () => {
      setStatus('error')
      setTimeout(() => setStatus('idle'), 3000)
    },
  })

  const reset = useCallback(() => {
    setStatus('idle')
    mutation.reset()
  }, [mutation])

  return {
    ...mutation,
    status,
    reset,
    isProcessing: status === 'submitting',
  }
}

export type { DeploymentConfig, DeploymentStatus }

/**
 * Hook to list PVCs in a specific namespace
 */
export function usePVCs(namespace: string | undefined) {
  return useQuery({
    queryKey: ['pvcs', namespace],
    queryFn: () => deploymentsApi.listPVCs(namespace!),
    enabled: !!namespace,
    staleTime: 30 * 1000,
    select: (data) => data.pvcs,
  })
}
