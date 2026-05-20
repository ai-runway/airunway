import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { DetailedClusterCapacity, Model, RuntimeStatus } from '@/lib/api'
import { DeploymentForm } from './DeploymentForm'

const mutateAsync = vi.fn()
const toast = vi.fn()

vi.mock('@/hooks/useDeployments', () => ({
  useCreateDeployment: () => ({
    mutateAsync,
    isProcessing: false,
    isValidating: false,
    isSubmitting: false,
    status: 'idle',
    reset: vi.fn(),
  }),
  usePVCs: () => ({ data: undefined }),
}))

vi.mock('@/hooks/useHuggingFace', () => ({
  useHuggingFaceStatus: () => ({ data: { configured: true } }),
  useGgufFiles: () => ({ data: [], isLoading: false }),
}))

vi.mock('@/hooks/useAikit', () => ({
  usePremadeModels: () => ({ data: [] }),
}))

const gatewayMock = vi.hoisted(() => ({ data: { available: false } as { available: boolean } }))
const manifestViewerMock = vi.hoisted(() => vi.fn())

vi.mock('@/hooks/useGateway', () => ({
  useGatewayStatus: () => gatewayMock,
}))

vi.mock('@/hooks/useToast', () => ({
  useToast: () => ({ toast }),
}))

vi.mock('@/components/ui/confetti', () => ({
  useConfetti: () => ({
    trigger: vi.fn(),
    ConfettiComponent: () => null,
  }),
}))

vi.mock('./CapacityWarning', () => ({
  CapacityWarning: () => null,
}))

vi.mock('./AIConfiguratorPanel', () => ({
  AIConfiguratorPanel: () => null,
}))

vi.mock('./ManifestViewer', () => ({
  ManifestViewer: (props: unknown) => {
    manifestViewerMock(props)
    return null
  },
}))

vi.mock('./CostEstimate', () => ({
  CostEstimate: () => null,
}))

vi.mock('./StorageVolumesSection', () => ({
  StorageVolumesSection: () => null,
}))

function createModel(overrides: Partial<Model> = {}): Model {
  return {
    id: 'deepseek-ai/DeepSeek-R1',
    name: 'DeepSeek R1',
    description: 'Large language model',
    size: '671B',
    task: 'text-generation',
    supportedEngines: ['vllm'],
    parameterCount: 671_000_000_000,
    estimatedGpuMemoryGb: 900,
    contextLength: 4096,
    ...overrides,
  }
}

function createCapacity(overrides: Partial<DetailedClusterCapacity> = {}): DetailedClusterCapacity {
  return {
    totalGpus: 16,
    allocatedGpus: 0,
    availableGpus: 16,
    maxContiguousAvailable: 16,
    maxNodeGpuCapacity: 8,
    gpuNodeCount: 2,
    totalMemoryGb: 80,
    nodePools: [],
    ...overrides,
  }
}

function createRuntime(overrides: Partial<RuntimeStatus> = {}): RuntimeStatus {
  return {
    id: 'installed-runtime',
    name: 'Installed Runtime',
    installed: true,
    ...overrides,
  } as RuntimeStatus
}

describe('DeploymentForm', () => {
  beforeEach(() => {
    mutateAsync.mockReset()
    toast.mockReset()
    manifestViewerMock.mockReset()
    gatewayMock.data = { available: false }
  })

  it('renders native vLLM as a compatible registered runtime for vLLM models', () => {
    render(
      <MemoryRouter>
        <DeploymentForm
          model={createModel({ supportedEngines: ['vllm'] })}
          detailedCapacity={createCapacity()}
          runtimes={[
            createRuntime({ id: 'dynamo', name: 'Dynamo', installed: true, healthy: true }),
            createRuntime({
              id: 'vllm',
              name: 'vLLM',
              installed: true,
              healthy: true,
              requiresCRD: false,
            }),
          ]}
        />
      </MemoryRouter>
    )

    const vllmCard = screen
      .getByText('High-throughput inference with the native vLLM provider')
      .closest('[role="radio"]') as HTMLElement

    expect(vllmCard).toBeInTheDocument()
    expect(within(vllmCard).getByText('Registered')).toBeInTheDocument()
    expect(within(vllmCard).queryByText('Not Installed')).not.toBeInTheDocument()

    fireEvent.click(vllmCard)

    expect(vllmCard).toHaveAttribute('aria-checked', 'true')
    expect(screen.getByRole('button', { name: /Deploy Model/i })).toBeEnabled()
  })

  it('treats a CRD-less vLLM provider that is not ready as registered but unavailable', async () => {
    render(
      <MemoryRouter>
        <DeploymentForm
          model={createModel({ supportedEngines: ['vllm'] })}
          detailedCapacity={createCapacity()}
          runtimes={[
            createRuntime({
              id: 'vllm',
              name: 'vLLM',
              installed: false,
              healthy: false,
              requiresCRD: false,
            }),
          ]}
        />
      </MemoryRouter>
    )

    const vllmCard = screen
      .getByText('High-throughput inference with the native vLLM provider')
      .closest('[role="radio"]') as HTMLElement

    expect(vllmCard).toBeInTheDocument()
    expect(within(vllmCard).getByText('Not Ready')).toBeInTheDocument()
    expect(within(vllmCard).queryByText('Not Installed')).not.toBeInTheDocument()

    fireEvent.click(vllmCard)

    await waitFor(() => {
      expect(vllmCard).toHaveAttribute('aria-checked', 'true')
    })
    expect(screen.getByText('Provider is registered but not ready yet.')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /install vllm/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Runtime Not Ready/i })).toBeDisabled()
  })

  it('keeps manual topology edits instead of snapping back to the recommendation', async () => {
    render(
      <MemoryRouter>
        <DeploymentForm
          model={createModel()}
          detailedCapacity={createCapacity()}
          runtimes={[createRuntime()]}
        />
      </MemoryRouter>
    )

    await waitFor(() => {
      expect(
        screen.getByText(/Multi-Node \(2 nodes × 8 GPUs = 16 total\)/i)
      ).toBeInTheDocument()
    })

    const gpuInput = screen.getByRole('spinbutton', { name: /GPUs per Replica/i })
    fireEvent.change(gpuInput, { target: { value: '4' } })

    await waitFor(() => {
      expect(gpuInput).toHaveValue(4)
      expect(
        screen.getByText(/Multi-Node \(3 nodes × 4 GPUs = 12 total\)/i)
      ).toBeInTheDocument()
    })

    expect(
      screen.queryByText(/Multi-Node \(2 nodes × 8 GPUs = 16 total\)/i)
    ).not.toBeInTheDocument()
  })

  it('does not render the gateway routing toggle when no gateway is available', () => {
    gatewayMock.data = { available: false }
    render(
      <MemoryRouter>
        <DeploymentForm
          model={createModel()}
          detailedCapacity={createCapacity()}
          runtimes={[createRuntime()]}
        />
      </MemoryRouter>
    )

    expect(screen.queryByLabelText(/Gateway routing/i)).not.toBeInTheDocument()
  })

  it('clears explicit gateway routing from preview and submit when the gateway becomes unavailable', async () => {
    gatewayMock.data = { available: true }
    const { rerender } = render(
      <MemoryRouter>
        <DeploymentForm
          model={createModel()}
          detailedCapacity={createCapacity()}
          runtimes={[createRuntime({ id: 'dynamo' })]}
        />
      </MemoryRouter>
    )

    const summary = await screen.findByText(/Advanced Settings/i)
    fireEvent.click(summary)

    const toggle = await screen.findByRole('switch', { name: /Gateway routing/i })
    fireEvent.click(toggle)
    await waitFor(() => {
      const lastManifestProps = manifestViewerMock.mock.calls[
        manifestViewerMock.mock.calls.length - 1
      ]?.[0] as { config?: { gatewayEnabled?: boolean } } | undefined
      expect(lastManifestProps?.config?.gatewayEnabled).toBe(false)
    })

    gatewayMock.data = { available: false }
    rerender(
      <MemoryRouter>
        <DeploymentForm
          model={createModel()}
          detailedCapacity={createCapacity()}
          runtimes={[createRuntime({ id: 'dynamo' })]}
        />
      </MemoryRouter>
    )

    await waitFor(() => {
      const lastManifestProps = manifestViewerMock.mock.calls[
        manifestViewerMock.mock.calls.length - 1
      ]?.[0] as { config?: { gatewayEnabled?: boolean } } | undefined
      expect(lastManifestProps?.config?.gatewayEnabled).toBeUndefined()
    })
    expect(screen.queryByLabelText(/Gateway routing/i)).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /Deploy Model/i }))

    await waitFor(() => {
      expect(mutateAsync).toHaveBeenCalledTimes(1)
    })
    expect(mutateAsync.mock.calls[0][0]).not.toHaveProperty('gatewayEnabled')
  })

  it('renders the gateway routing toggle as default-on without submitting gateway routing until changed', async () => {
    gatewayMock.data = { available: true }
    render(
      <MemoryRouter>
        <DeploymentForm
          model={createModel()}
          detailedCapacity={createCapacity()}
          runtimes={[createRuntime()]}
        />
      </MemoryRouter>
    )

    // Expand the Advanced Settings <details> to make the toggle visible
    const summary = await screen.findByText(/Advanced Settings/i)
    fireEvent.click(summary)

    const toggle = await screen.findByRole('switch', { name: /Gateway routing/i })
    expect(toggle).toBeInTheDocument()
    expect(toggle).toHaveAttribute('aria-checked', 'true')

    const latestManifestConfig = () => (manifestViewerMock.mock.calls[
      manifestViewerMock.mock.calls.length - 1
    ]?.[0] as { config?: { gatewayEnabled?: boolean } } | undefined)?.config

    expect(latestManifestConfig()?.gatewayEnabled).toBeUndefined()

    fireEvent.click(toggle)
    await waitFor(() => {
      expect(toggle).toHaveAttribute('aria-checked', 'false')
      expect(latestManifestConfig()?.gatewayEnabled).toBe(false)
    })

    fireEvent.click(toggle)
    await waitFor(() => {
      expect(toggle).toHaveAttribute('aria-checked', 'true')
      expect(latestManifestConfig()?.gatewayEnabled).toBe(true)
    })
  })
})
