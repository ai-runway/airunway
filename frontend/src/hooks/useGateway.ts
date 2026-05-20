import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { installationApi, gatewayApi, type GatewayCRDStatus, type GatewayCRDInstallResult, type GatewayInfo } from '@/lib/api'

/**
 * Hook to get Gateway API / GAIE CRD installation status
 */
export function useGatewayCRDStatus() {
  return useQuery<GatewayCRDStatus>({
    queryKey: ['gateway-crd-status'],
    queryFn: () => installationApi.getGatewayCRDStatus(),
    refetchInterval: 30000,
  })
}

/**
 * Hook to install Gateway API and GAIE CRDs
 */
export function useInstallGatewayCRDs() {
  const queryClient = useQueryClient()

  return useMutation<GatewayCRDInstallResult, Error>({
    mutationFn: () => installationApi.installGatewayCRDs(),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['gateway-crd-status'] })
      queryClient.invalidateQueries({ queryKey: ['gateway-status'] })
    },
  })
}

/**
 * Hook to get current Gateway resource availability and endpoint
 */
export function useGatewayStatus() {
  return useQuery<GatewayInfo>({
    queryKey: ['gateway-status'],
    queryFn: () => gatewayApi.getStatus(),
    refetchInterval: 30000,
  })
}
