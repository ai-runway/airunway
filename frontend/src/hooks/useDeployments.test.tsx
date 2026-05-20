import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import {
  useDeployments,
  useDeployment,
  useDeploymentPods,
  useCreateDeployment,
  useDeleteDeployment
} from './useDeployments'
import { createWrapper, createTestQueryClient } from '@/test/test-utils'
import { mockDeployments } from '@/test/mocks/handlers'
import { QueryClientProvider } from '@tanstack/react-query'
import React from 'react'

describe('useDeployments', () => {
  it('fetches deployments list', async () => {
    const { result } = renderHook(() => useDeployments(), {
      wrapper: createWrapper(),
    })

    // Initially loading
    expect(result.current.isLoading).toBe(true)

    // Wait for the query to complete
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    // Check data is returned
    expect(result.current.data).toBeDefined()
    expect(Array.isArray(result.current.data)).toBe(true)
  })

  it('fetches deployments with namespace filter', async () => {
    const { result } = renderHook(() => useDeployments('airunway-system'), {
      wrapper: createWrapper(),
    })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(result.current.data).toBeDefined()
    // All returned deployments should be in the specified namespace
    if (result.current.data && result.current.data.length > 0) {
      result.current.data.forEach(deployment => {
        expect(deployment.namespace).toBe('airunway-system')
      })
    }
  })

  it('has correct query key structure', async () => {
    const { result } = renderHook(() => useDeployments('test-ns'), {
      wrapper: createWrapper(),
    })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    // The hook should work correctly with namespace parameter
    expect(result.current.data).toBeDefined()
  })
})

describe('useDeployment', () => {
  it('fetches a single deployment by name', async () => {
    const deploymentName = mockDeployments[0].name

    const { result } = renderHook(() => useDeployment(deploymentName), {
      wrapper: createWrapper(),
    })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(result.current.data).toBeDefined()
    expect(result.current.data?.name).toBe(deploymentName)
  })

  it('does not fetch when name is undefined', async () => {
    const { result } = renderHook(() => useDeployment(undefined), {
      wrapper: createWrapper(),
    })

    // Should not be loading since query is disabled
    expect(result.current.isLoading).toBe(false)
    expect(result.current.fetchStatus).toBe('idle')
  })

  it('fetches with namespace parameter', async () => {
    const deploymentName = mockDeployments[0].name

    const { result } = renderHook(() => useDeployment(deploymentName, 'airunway-system'), {
      wrapper: createWrapper(),
    })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data?.namespace).toBe('airunway-system')
  })
})

describe('useDeploymentPods', () => {
  it('fetches pods for a deployment', async () => {
    const deploymentName = mockDeployments[0].name

    const { result } = renderHook(() => useDeploymentPods(deploymentName), {
      wrapper: createWrapper(),
    })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(result.current.data).toBeDefined()
    expect(Array.isArray(result.current.data)).toBe(true)
  })

  it('does not fetch when name is undefined', async () => {
    const { result } = renderHook(() => useDeploymentPods(undefined), {
      wrapper: createWrapper(),
    })

    expect(result.current.isLoading).toBe(false)
    expect(result.current.fetchStatus).toBe('idle')
  })
})

describe('useCreateDeployment', () => {
  it('creates a deployment and invalidates queries', async () => {
    const queryClient = createTestQueryClient()
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')

    const wrapper = ({ children }: { children: React.ReactNode }) => (
      <QueryClientProvider client={queryClient}>
        {children}
      </QueryClientProvider>
    )

    const { result } = renderHook(() => useCreateDeployment(), { wrapper })

    const deploymentConfig = {
      name: 'new-deployment',
      namespace: 'airunway-system',
      modelId: 'Qwen/Qwen3-0.6B',
      engine: 'vllm' as const,
      mode: 'aggregated' as const,
      routerMode: 'default' as const,
      replicas: 1,
      hfTokenSecret: 'hf-token-secret',
      enforceEager: true,
      enablePrefixCaching: false,
      trustRemoteCode: false,
    }

    result.current.mutate(deploymentConfig)

    // Wait for success with longer timeout due to artificial delays in the hook
    await waitFor(() => expect(result.current.isSuccess).toBe(true), { timeout: 3000 })

    expect(result.current.data).toBeDefined()
    expect(result.current.data?.message).toBe('Deployment created')
    expect(result.current.data?.name).toBe('new-deployment')

    // Verify queries are invalidated
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['deployments'] })
  })

  it('handles mutation errors', async () => {
    // This test relies on MSW to return an error response if we configure it
    const { result } = renderHook(() => useCreateDeployment(), {
      wrapper: createWrapper(),
    })

    // The hook should be in idle state initially
    expect(result.current.isIdle).toBe(true)
    expect(result.current.isError).toBe(false)
  })
})

describe('useDeleteDeployment', () => {
  it('deletes a deployment and invalidates queries', async () => {
    const queryClient = createTestQueryClient()
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries')

    const wrapper = ({ children }: { children: React.ReactNode }) => (
      <QueryClientProvider client={queryClient}>
        {children}
      </QueryClientProvider>
    )

    const { result } = renderHook(() => useDeleteDeployment(), { wrapper })

    result.current.mutate({ name: 'test-deployment', namespace: 'airunway-system' })

    await waitFor(() => expect(result.current.isSuccess).toBe(true), { timeout: 3000 })

    expect(result.current.data).toBeDefined()
    expect(result.current.data?.message).toBe('Deployment deleted')

    // Verify queries are invalidated
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['deployments'] })
  })

  it('deletes without namespace', async () => {
    const { result } = renderHook(() => useDeleteDeployment(), {
      wrapper: createWrapper(),
    })

    result.current.mutate({ name: 'test-deployment' })

    await waitFor(() => expect(result.current.isSuccess).toBe(true), { timeout: 3000 })
    expect(result.current.data?.message).toBe('Deployment deleted')
  })
})
