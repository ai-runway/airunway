import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { providersApi, settingsApi, type Settings, type ProviderInfo } from '@/lib/api'

export function useSettings() {
  return useQuery<Settings>({
    queryKey: ['settings'],
    queryFn: () => settingsApi.get(),
  })
}

/**
 * Update settings (currently only defaultNamespace).
 * Active provider is no longer a global setting - each deployment specifies its runtime.
 */
export function useUpdateSettings() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (settings: { defaultNamespace?: string }) =>
      settingsApi.update(settings),
    onSuccess: () => {
      // Invalidate settings and cluster status queries
      queryClient.invalidateQueries({ queryKey: ['settings'] })
      queryClient.invalidateQueries({ queryKey: ['cluster-status'] })
    },
  })
}

export function useProviders() {
  return useQuery<{ providers: ProviderInfo[] }>({
    queryKey: ['providers'],
    queryFn: () => providersApi.list(),
  })
}

export function useProviderDetails(providerId: string) {
  return useQuery({
    queryKey: ['provider', providerId],
    queryFn: () => providersApi.get(providerId),
    enabled: !!providerId,
  })
}
