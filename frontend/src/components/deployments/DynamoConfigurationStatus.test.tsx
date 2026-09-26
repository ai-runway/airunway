import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defaultDynamoIntent, type DeploymentStatus } from '@airunway/shared'
import { createWrapper } from '@/test/test-utils'
import { DynamoConfigurationStatus } from './DynamoConfigurationStatus'

const reconfigure = vi.hoisted(() => vi.fn())
vi.mock('@/lib/api', async importOriginal => ({ ...await importOriginal<typeof import('@/lib/api')>(), deploymentsApi: { reconfigure } }))
const deployment: DeploymentStatus = {
  name: 'auto', namespace: 'models', resourceVersion: '42', modelId: 'Qwen/Qwen3-0.6B', engine: 'vllm',
  provider: 'dynamo', configurationMode: 'automatic', intent: defaultDynamoIntent(), mode: 'aggregated', phase: 'Failed',
  createdAt: '2026-09-25', replicas: { desired: 0, ready: 0, available: 0 }, pods: [],
  providerStatus: { intent: { phase: 'Failed', profilingPhase: 'Searching' }, workloadRef: { name: 'generated-custom', namespace: 'serving' } },
}

describe('Dynamo configuration status', () => {
  beforeEach(() => { reconfigure.mockReset() })
  it('shows progress, actual serving identity and no performance guarantee', () => {
    render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    expect(screen.getByText('Searching')).toBeInTheDocument()
    expect(screen.getByText('serving/generated-custom')).toBeInTheDocument()
    expect(screen.getByText(/does not confirm that these targets were met/)).toBeInTheDocument()
    expect(screen.queryByRole('spinbutton')).not.toBeInTheDocument()
  })
  it('requires confirmation before retry and submits only the displayed revision', async () => {
    reconfigure.mockResolvedValue({ attempt: 'new' })
    render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(screen.getByText(/requests may be interrupted/)).toBeInTheDocument()
    expect(reconfigure).not.toHaveBeenCalled()
    expect(screen.queryByRole('spinbutton')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Confirm retry' }))
    await waitFor(() => expect(reconfigure).toHaveBeenCalledWith('auto', 'models', { resourceVersion: '42' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })
  it('switches exclusive traffic and latency targets without switching modes when a field is cleared', async () => {
    reconfigure.mockResolvedValue({ attempt: 'new' })
    render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    fireEvent.change(screen.getByLabelText('Expected traffic'), { target: { value: 'concurrency' } })
    fireEvent.change(screen.getByRole('spinbutton', { name: /Simultaneous requests/ }), { target: { value: '' } })
    expect(screen.getByLabelText('Expected traffic')).toHaveValue('concurrency')
    fireEvent.change(screen.getByRole('spinbutton', { name: /Simultaneous requests/ }), { target: { value: '8' } })
    fireEvent.change(screen.getByLabelText('Latency targets'), { target: { value: 'total' } })
    fireEvent.change(screen.getByRole('spinbutton', { name: /Total response target/ }), { target: { value: '' } })
    expect(screen.getByLabelText('Latency targets')).toHaveValue('total')
    fireEvent.change(screen.getByRole('spinbutton', { name: /Total response target/ }), { target: { value: '5000' } })
    fireEvent.click(screen.getByRole('button', { name: 'Confirm reconfiguration' }))
    await waitFor(() => expect(reconfigure).toHaveBeenCalledTimes(1))
    expect(reconfigure.mock.calls[0][2].intent.workload).toEqual({ isl: 1024, osl: 256, concurrency: 8 })
    expect(reconfigure.mock.calls[0][2].intent.sla).toEqual({ e2eLatency: 5000 })
  })

  it('keeps the opened revision when live status refreshes and reports a conflict without auto-retry', async () => {
    reconfigure.mockRejectedValue(new Error('Deployment changed. Refresh before reconfiguring.'))
    const view = render(<DynamoConfigurationStatus deployment={deployment} />, { wrapper: createWrapper() })
    fireEvent.click(screen.getByRole('button', { name: 'Reconfigure' }))
    fireEvent.change(screen.getByRole('spinbutton', { name: /GPU budget/ }), { target: { value: '4' } })
    fireEvent.change(screen.getByLabelText('Model ID'), { target: { value: 'Qwen/Qwen3-8B' } })
    view.rerender(<DynamoConfigurationStatus deployment={{ ...deployment, resourceVersion: '43' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Confirm reconfiguration' }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Deployment changed'))
    expect(reconfigure).toHaveBeenCalledTimes(1)
    expect(reconfigure.mock.calls[0][2]).toMatchObject({ resourceVersion: '42', modelId: 'Qwen/Qwen3-8B', intent: { hardware: { totalGpus: 4 } } })
  })
})
