import { describe, expect, it } from 'vitest'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { screen, waitFor, renderWithProviders } from '@/test/test-utils'
import { server } from '@/test/mocks/server'
import { DeploymentLogs } from './DeploymentLogs'
import type { PodStatus } from '@airunway/shared'

const API_BASE = '*/api'

function createPod(name: string): PodStatus {
  return {
    name,
    phase: 'Running',
    ready: true,
    restarts: 0,
  }
}

describe('DeploymentLogs', () => {
  it('loads logs for the default instance and switches when another instance is selected', async () => {
    const pods = [createPod('demo-abc123'), createPod('demo-def456')]
    const requestedPodNames: Array<string | null> = []

    server.use(
      http.get(`${API_BASE}/deployments/:name/pods`, () => {
        return HttpResponse.json({ pods })
      }),
      http.get(`${API_BASE}/deployments/:name/logs`, ({ request }) => {
        const url = new URL(request.url)
        const podName = url.searchParams.get('podName')
        requestedPodNames.push(podName)

        return HttpResponse.json({
          logs: `logs for ${podName}`,
          podName,
        })
      })
    )

    renderWithProviders(<DeploymentLogs deploymentName="demo" namespace="default" />)

    expect(await screen.findByRole('combobox', { name: /instance/i })).toBeInTheDocument()
    expect(await screen.findByText('logs for demo-abc123')).toBeInTheDocument()
    expect(requestedPodNames[0]).toBe('demo-abc123')

    const user = userEvent.setup()
    await user.click(screen.getByRole('combobox', { name: /instance/i }))
    await user.click(await screen.findByRole('option', { name: /demo-def456/i }))

    expect(await screen.findByText('logs for demo-def456')).toBeInTheDocument()
    await waitFor(() => {
      expect(requestedPodNames).toContain('demo-def456')
    })
  })

  it('moves back to an available instance when the selected instance disappears', async () => {
    let pods = [createPod('demo-abc123'), createPod('demo-def456')]
    const requestedPodNames: string[] = []

    server.use(
      http.get(`${API_BASE}/deployments/:name/pods`, () => {
        return HttpResponse.json({ pods })
      }),
      http.get(`${API_BASE}/deployments/:name/logs`, ({ request }) => {
        const url = new URL(request.url)
        const podName = url.searchParams.get('podName') || ''
        requestedPodNames.push(podName)

        return HttpResponse.json({
          logs: `logs for ${podName}`,
          podName,
        })
      })
    )

    const { queryClient } = renderWithProviders(
      <DeploymentLogs deploymentName="demo" namespace="default" />
    )

    expect(await screen.findByText('logs for demo-abc123')).toBeInTheDocument()

    const user = userEvent.setup()
    await user.click(screen.getByRole('combobox', { name: /instance/i }))
    await user.click(await screen.findByRole('option', { name: /demo-def456/i }))
    expect(await screen.findByText('logs for demo-def456')).toBeInTheDocument()

    pods = [createPod('demo-abc123')]
    await queryClient.invalidateQueries({ queryKey: ['deployment-pods', 'demo', 'default'] })

    await waitFor(() => {
      expect(requestedPodNames[requestedPodNames.length - 1]).toBe('demo-abc123')
    })
    expect(await screen.findByText('logs for demo-abc123')).toBeInTheDocument()
    expect(screen.queryByRole('combobox', { name: /instance/i })).not.toBeInTheDocument()
  })
})
