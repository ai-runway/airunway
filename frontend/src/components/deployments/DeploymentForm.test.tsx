import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { DetailedClusterCapacity, Model, RuntimeStatus } from '@/lib/api'
import { DeploymentForm, setFp8PrecisionEngineArgs } from './DeploymentForm'

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

  it('warns but does not block deploying when the throughput estimate says the model does not fit', () => {
    render(
      <MemoryRouter>
        <DeploymentForm
          model={createModel({ supportedEngines: ['vllm'] })}
          detailedCapacity={createCapacity()}
          runtimes={[
            createRuntime({
              id: 'vllm',
              name: 'vLLM',
              installed: true,
              healthy: true,
              requiresCRD: false,
            }),
          ]}
          doesNotFit
          doesNotFitReason="This model is estimated not to fit on this cluster's GPU (A10) at 1 GPU per replica."
        />
      </MemoryRouter>
    )

    const vllmCard = screen
      .getByText('High-throughput inference with the native vLLM provider')
      .closest('[role="radio"]') as HTMLElement
    fireEvent.click(vllmCard)

    // The warning is surfaced...
    expect(
      screen.getByText(/estimated not to fit on this cluster's GPU \(A10\)/i)
    ).toBeInTheDocument()
    // ...but Deploy stays enabled (the user may pick more GPUs per replica).
    expect(screen.getByRole('button', { name: /Deploy Model/i })).toBeEnabled()
  })

  it('hides the does-not-fit warning when FP8 is already blocking deployment', () => {
    render(
      <MemoryRouter>
        <DeploymentForm
          model={createModel({ supportedEngines: ['vllm'] })}
          detailedCapacity={createCapacity()}
          runtimes={[
            createRuntime({
              id: 'vllm',
              name: 'vLLM',
              installed: true,
              healthy: true,
              requiresCRD: false,
            }),
          ]}
          doesNotFit
          doesNotFitReason="This model is estimated not to fit."
          fp8Blocked
          fp8BlockReason="FP8 is only supported on H100/H200 GPUs."
        />
      </MemoryRouter>
    )

    // The blocking FP8 message wins; the does-not-fit warning is suppressed to
    // avoid stacking two conflicting messages.
    expect(screen.getByText(/FP8 is only supported on H100\/H200 GPUs/i)).toBeInTheDocument()
    expect(screen.queryByText(/estimated not to fit/i)).not.toBeInTheDocument()
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

describe('setFp8PrecisionEngineArgs', () => {
  it('preserves a user-set non-fp8 quantization when weight precision is not FP8', () => {
    // Regression: the precision dropdowns must not clobber an awq/gptq value the
    // user typed into the advanced engine-args editor.
    const result = setFp8PrecisionEngineArgs(
      { quantization: 'awq' },
      { weightFp8: false, kvFp8: false }
    )
    expect(result).toEqual({ quantization: 'awq' })
  })

  it('strips a quantization value it owns (fp8) when weight precision is not FP8', () => {
    const result = setFp8PrecisionEngineArgs(
      { quantization: 'fp8' },
      { weightFp8: false, kvFp8: false }
    )
    expect(result).toBeUndefined()
  })

  it('sets quantization to fp8 when weight precision is FP8, overriding a prior awq', () => {
    const result = setFp8PrecisionEngineArgs(
      { quantization: 'awq' },
      { weightFp8: true, kvFp8: false }
    )
    expect(result).toEqual({ quantization: 'fp8' })
  })

  it('preserves a user-set non-fp8 kv-cache-dtype when KV precision is not FP8', () => {
    const result = setFp8PrecisionEngineArgs(
      { 'kv-cache-dtype': 'int8' },
      { weightFp8: false, kvFp8: false }
    )
    expect(result).toEqual({ 'kv-cache-dtype': 'int8' })
  })

  it('strips a kv-cache-dtype value it owns (fp8) when KV precision is not FP8', () => {
    const result = setFp8PrecisionEngineArgs(
      { 'kv-cache-dtype': 'fp8' },
      { weightFp8: false, kvFp8: false }
    )
    expect(result).toBeUndefined()
  })

  it('sets kv-cache-dtype to fp8 when KV precision is FP8, overriding a prior int8', () => {
    const result = setFp8PrecisionEngineArgs(
      { 'kv-cache-dtype': 'int8' },
      { weightFp8: false, kvFp8: true }
    )
    expect(result).toEqual({ 'kv-cache-dtype': 'fp8' })
  })

  it('leaves unrelated engine args untouched', () => {
    const result = setFp8PrecisionEngineArgs(
      { 'max-model-len': '8192', quantization: 'gptq' },
      { weightFp8: false, kvFp8: false }
    )
    expect(result).toEqual({ 'max-model-len': '8192', quantization: 'gptq' })
  })
})
