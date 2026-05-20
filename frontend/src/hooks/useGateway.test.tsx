import { describe, it, expect } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { useGatewayCRDStatus, useInstallGatewayCRDs } from './useGateway'
import { createWrapper } from '@/test/test-utils'

describe('useGatewayCRDStatus', () => {
  it('fetches gateway CRD status', async () => {
    const { result } = renderHook(() => useGatewayCRDStatus(), {
      wrapper: createWrapper(),
    })

    expect(result.current.isLoading).toBe(true)

    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(result.current.data).toBeDefined()
    expect(result.current.data?.gatewayApiInstalled).toBe(true)
    expect(result.current.data?.inferenceExtInstalled).toBe(true)
    expect(result.current.data?.gatewayApiVersion).toBe('v1.2.1')
    expect(result.current.data?.inferenceExtVersion).toBe('v1.5.0')
    expect(result.current.data?.pinnedVersion).toBe('v1.3.1')
    expect(result.current.data?.installCommands).toHaveLength(2)
  })
})

describe('useInstallGatewayCRDs', () => {
  it('creates a mutation hook for CRD installation', async () => {
    const { result } = renderHook(() => useInstallGatewayCRDs(), {
      wrapper: createWrapper(),
    })

    expect(result.current.mutateAsync).toBeDefined()
    expect(result.current.isPending).toBe(false)
  })

  it('installs gateway CRDs successfully', async () => {
    const { result } = renderHook(() => useInstallGatewayCRDs(), {
      wrapper: createWrapper(),
    })

    const installResult = await result.current.mutateAsync()

    expect(installResult.success).toBe(true)
    expect(installResult.results).toHaveLength(2)
    expect(installResult.results?.[0].step).toBe('gateway-api-crds')
    expect(installResult.results?.[1].step).toBe('inference-extension-crds')
  })
})
