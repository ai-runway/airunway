import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsPage } from './SettingsPage'

const mutateAsync = vi.fn()
const refetch = vi.fn()
const startOAuth = vi.fn()
const toast = vi.fn()
let mockGpuStatus = {
  installed: false,
  gpusAvailable: false,
  operatorRunning: false,
  totalGPUs: 0,
  message: '',
  gpuNodes: [] as string[],
  helmCommands: [] as string[],
}
let mockHfStatus: {
  configured: boolean
  user?: {
    name: string
    fullname?: string
    avatarUrl?: string
  }
} = {
  configured: false,
}

let mockGatewayStatus = {
  gatewayApiInstalled: false,
  inferenceExtInstalled: false,
  gatewayAvailable: false,
  installCommands: [] as string[],
  message: '',
  pinnedVersion: 'v1.3.1',
  gatewayApiVersion: undefined as string | undefined,
  inferenceExtVersion: undefined as string | undefined,
}

type MockRuntimeStatus = {
  id: string
  name: string
  installed: boolean
  healthy: boolean
  crdFound?: boolean
  operatorRunning?: boolean
  requiresCRD?: boolean
  version?: string
}

const defaultMockRuntimes = (): MockRuntimeStatus[] => [
  {
    id: 'installed-runtime',
    name: 'Installed Runtime',
    installed: true,
    healthy: true,
    version: '1.0.0',
  },
  {
    id: 'available-runtime',
    name: 'Available Runtime',
    installed: false,
    healthy: false,
  },
  {
    id: 'kuberay',
    name: 'Kuberay',
    installed: false,
    healthy: false,
    crdFound: true,
    operatorRunning: false,
  },
  {
    id: 'llmd',
    name: 'LLM-D',
    installed: true,
    healthy: true,
    crdFound: true,
    operatorRunning: true,
    requiresCRD: true,
  },
  {
    id: 'vLLM',
    name: 'vLLM',
    installed: true,
    healthy: true,
    crdFound: true,
    operatorRunning: true,
    requiresCRD: true,
  },
]

let mockRuntimes = defaultMockRuntimes()

const getMockInstallationStatus = (providerId: string) => {
  switch (providerId) {
    case 'available-runtime':
      return {
        installed: false,
        providerName: 'Available Runtime',
        message: 'Available Runtime is not installed yet.',
        crdFound: false,
        operatorRunning: false,
        installationSteps: [],
      }
    case 'kuberay':
      return {
        installed: false,
        providerName: 'Kuberay',
        message: 'KubeRay CRD found but no ready KubeRay operator pods were detected in ray-system',
        crdFound: true,
        operatorRunning: false,
        installationSteps: [],
      }
    case 'llmd':
    case 'custom-llmd-registration':
      return {
        installed: true,
        providerName: 'LLM-D',
        message: 'Runtime is ready to use.',
        crdFound: true,
        operatorRunning: true,
        requiresCRD: true,
        installationSteps: [
          { title: 'Install LLM-D CRD', commands: ['kubectl apply -f llmd-crd.yaml'] },
          { title: 'Start LLM-D operator', commands: ['helm install llmd-operator llmd/operator'] },
        ],
      }
    case 'vllm':
    case 'custom-vllm-registration':
      return {
        installed: true,
        providerName: 'vLLM',
        message: 'Runtime is ready to use.',
        crdFound: true,
        operatorRunning: true,
        requiresCRD: true,
        installationSteps: [
          { title: 'Install vLLM CRD', commands: ['kubectl apply -f vllm-crd.yaml'] },
          { title: 'Start vLLM operator', commands: ['helm install vllm-operator vllm/operator'] },
        ],
      }
    default:
      return {
        installed: true,
        providerName: 'Installed Runtime',
        message: 'Installed Runtime is ready.',
        crdFound: true,
        operatorRunning: true,
        installationSteps: [],
      }
  }
}

vi.mock('@/hooks/useSettings', () => ({
  useSettings: () => ({ isLoading: false }),
}))

vi.mock('@/hooks/useRuntimes', () => ({
  useRuntimesStatus: () => ({
    data: {
      runtimes: mockRuntimes,
    },
    isLoading: false,
    refetch,
  }),
}))

vi.mock('@/hooks/useClusterStatus', () => ({
  useClusterStatus: () => ({
    data: {
      connected: true,
      clusterName: 'test-cluster',
    },
    isLoading: false,
  }),
}))

vi.mock('@/hooks/useInstallation', () => ({
  useHelmStatus: () => ({
    data: {
      available: true,
      version: '3.15.0',
    },
    isLoading: false,
  }),
  useProviderInstallationStatus: (providerId: string) => ({
    data: getMockInstallationStatus(providerId),
    isLoading: false,
    refetch,
  }),
  useInstallProvider: () => ({
    mutateAsync,
  }),
  useUninstallProvider: () => ({
    mutateAsync,
  }),
}))

vi.mock('@/hooks/useAutoscaler', () => ({
  useAutoscalerDetection: () => ({
    data: null,
    isLoading: false,
  }),
}))

vi.mock('@/hooks/useGpuOperator', () => ({
  useGpuOperatorStatus: () => ({
    data: mockGpuStatus,
    isLoading: false,
    refetch,
  }),
  useInstallGpuOperator: () => ({
    mutateAsync,
  }),
}))

vi.mock('@/hooks/useGateway', () => ({
  useGatewayCRDStatus: () => ({
    data: mockGatewayStatus,
    isLoading: false,
    refetch,
  }),
  useInstallGatewayCRDs: () => ({
    mutateAsync,
  }),
}))

vi.mock('@/hooks/useHuggingFace', () => ({
  useHuggingFaceStatus: () => ({
    data: mockHfStatus,
    isLoading: false,
    refetch,
  }),
  useHuggingFaceOAuth: () => ({
    startOAuth,
  }),
  useDeleteHuggingFaceSecret: () => ({
    mutateAsync,
    isPending: false,
  }),
}))

vi.mock('@/hooks/useToast', () => ({
  useToast: () => ({
    toast,
  }),
}))

vi.mock('@/components/autoscaler/AutoscalerGuidance', () => ({
  AutoscalerGuidance: () => null,
}))

describe('SettingsPage', () => {
  beforeEach(() => {
    mutateAsync.mockReset()
    refetch.mockReset()
    startOAuth.mockReset()
    toast.mockReset()
    mockRuntimes = defaultMockRuntimes()
    mockGpuStatus = {
      installed: false,
      gpusAvailable: false,
      operatorRunning: false,
      totalGPUs: 0,
      message: '',
      gpuNodes: [],
      helmCommands: [],
    }
    mockHfStatus = {
      configured: false,
    }
    mockGatewayStatus = {
      gatewayApiInstalled: false,
      inferenceExtInstalled: false,
      gatewayAvailable: false,
      installCommands: [],
      message: '',
      pinnedVersion: 'v1.3.1',
      gatewayApiVersion: undefined,
      inferenceExtVersion: undefined,
    }
  })

  it('keeps uninstalled runtime surfaces neutral while showing red X icons', () => {
    const { container } = render(
      <MemoryRouter initialEntries={['/settings?tab=runtimes']}>
        <SettingsPage />
      </MemoryRouter>
    )

    expect(screen.getByText('Available Runtimes')).toBeInTheDocument()
    expect(screen.getByText('Installed Runtime')).toBeInTheDocument()
    expect(screen.getByText('Available Runtime')).toBeInTheDocument()
    expect(container.querySelector('[style*="border-top-color"]')).toBeNull()
    expect(container.querySelector('[style*="border-top-width"]')).toBeNull()

    const availableCard = screen.getByText('Available Runtime').closest('.rounded-2xl')
    expect(availableCard).not.toHaveClass('bg-destructive/10', 'border-destructive/20')
    const availableStatus = within(availableCard as HTMLElement).getByText('Not Installed').closest('span')
    expect(availableStatus).toHaveClass('text-muted-foreground')
    expect(availableStatus?.querySelector('svg')).toHaveClass('text-red-500')

    fireEvent.click(screen.getByText('Available Runtime'))

    const installationPanel = screen.getByText('Available Runtime Installation').closest('.rounded-2xl')
    expect(installationPanel).not.toHaveClass('bg-destructive/10', 'border-destructive/20')
    const installationStatus = within(installationPanel as HTMLElement).getByText('Not Installed').closest('span')
    expect(installationStatus).toHaveClass('text-muted-foreground')
    expect(installationStatus?.querySelector('svg')).toHaveClass('text-red-500')
  })

  it('does not show uninstall for a runtime that has only its CRD installed', () => {
    render(
      <MemoryRouter initialEntries={['/settings?tab=runtimes']}>
        <SettingsPage />
      </MemoryRouter>
    )

    fireEvent.click(screen.getByText('Kuberay'))

    const installationPanel = screen.getByText('Kuberay Installation').closest('.rounded-2xl')
    expect(within(installationPanel as HTMLElement).getByText('Not Installed')).toBeInTheDocument()
    expect(within(installationPanel as HTMLElement).getByText('CRD Installed')).toBeInTheDocument()
    expect(within(installationPanel as HTMLElement).getByText('Operator Running')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^uninstall$/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /install kuberay/i })).toBeInTheDocument()
  })

  it('shows providers that do not require runtime operators without CRD controls', () => {
    render(
      <MemoryRouter initialEntries={['/settings?tab=runtimes']}>
        <SettingsPage />
      </MemoryRouter>
    )

    fireEvent.click(screen.getByText('LLM-D'))

    const llmdCard = screen.getByText('LLM-D').closest('.rounded-2xl')
    expect(within(llmdCard as HTMLElement).getByText('LLM-D for distributed inference')).toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).getByText('Runtime is ready to use.')).toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).getByText('Ready')).toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).queryByText('Installed')).not.toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).queryByText('Not Installed')).not.toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).queryByText('CRD')).not.toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).queryByText('Operator')).not.toBeInTheDocument()
    expect(llmdCard).not.toHaveTextContent(/CRD|operator/i)

    const llmdInstallationPanel = screen.getByText('LLM-D Status').closest('.rounded-2xl')
    expect(within(llmdInstallationPanel as HTMLElement).getByText('Ready')).toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByText('Installed')).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByText('Not Installed')).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).getAllByText('Runtime is ready to use.').length).toBeGreaterThan(0)
    expect(llmdInstallationPanel).not.toHaveTextContent(/CRD|operator/i)
    expect(llmdInstallationPanel).not.toHaveTextContent('Manual Installation Steps')
    expect(within(llmdInstallationPanel as HTMLElement).queryByText('CRD Installed')).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByText('Operator Running')).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByRole('button', { name: /install llm-d/i })).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByRole('button', { name: /^uninstall$/i })).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByRole('button')).not.toBeInTheDocument()

    fireEvent.click(screen.getByText('vLLM'))

    const vllmCard = screen.getByText('vLLM').closest('.rounded-2xl')
    expect(within(vllmCard as HTMLElement).getByText('vLLM for high-throughput inference')).toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).getByText('Runtime is ready to use.')).toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).getByText('Ready')).toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).queryByText('Installed')).not.toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).queryByText('Not Installed')).not.toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).queryByText('CRD')).not.toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).queryByText('Operator')).not.toBeInTheDocument()
    expect(vllmCard).not.toHaveTextContent(/CRD|operator/i)

    const vllmInstallationPanel = screen.getByText('vLLM Status').closest('.rounded-2xl')
    expect(within(vllmInstallationPanel as HTMLElement).getByText('Ready')).toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByText('Installed')).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByText('Not Installed')).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).getAllByText('Runtime is ready to use.').length).toBeGreaterThan(0)
    expect(vllmInstallationPanel).not.toHaveTextContent(/CRD|operator/i)
    expect(vllmInstallationPanel).not.toHaveTextContent('Manual Installation Steps')
    expect(within(vllmInstallationPanel as HTMLElement).queryByText('CRD Installed')).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByText('Operator Running')).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByRole('button', { name: /install vllm/i })).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByRole('button', { name: /^uninstall$/i })).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByRole('button')).not.toBeInTheDocument()
  })

  it('uses display names to hide CRD controls for CRD-less providers with custom ids', async () => {
    mockRuntimes = [
      {
        id: 'custom-llmd-registration',
        name: 'LLM-D',
        installed: true,
        healthy: true,
        crdFound: true,
        operatorRunning: true,
        requiresCRD: true,
      },
      {
        id: 'custom-vllm-registration',
        name: 'vLLM',
        installed: true,
        healthy: true,
        crdFound: true,
        operatorRunning: true,
        requiresCRD: true,
      },
    ]

    render(
      <MemoryRouter initialEntries={['/settings?tab=runtimes']}>
        <SettingsPage />
      </MemoryRouter>
    )

    await screen.findByText('LLM-D Status')

    const llmdCard = screen.getByText('LLM-D').closest('.rounded-2xl')
    expect(within(llmdCard as HTMLElement).getByText('LLM-D for distributed inference')).toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).queryByText('Ray Serve via KubeRay for distributed Ray-based model serving with vLLM')).not.toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).getByText('Runtime is ready to use.')).toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).getByText('Ready')).toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).queryByText('Installed')).not.toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).queryByText('Not Installed')).not.toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).queryByText('CRD')).not.toBeInTheDocument()
    expect(within(llmdCard as HTMLElement).queryByText('Operator')).not.toBeInTheDocument()
    expect(llmdCard).not.toHaveTextContent(/CRD|operator/i)

    const llmdInstallationPanel = screen.getByText('LLM-D Status').closest('.rounded-2xl')
    expect(within(llmdInstallationPanel as HTMLElement).getByText('Ready')).toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByText('Installed')).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByText('Not Installed')).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).getAllByText('Runtime is ready to use.').length).toBeGreaterThan(0)
    expect(llmdInstallationPanel).not.toHaveTextContent(/CRD|operator/i)
    expect(llmdInstallationPanel).not.toHaveTextContent('Manual Installation Steps')
    expect(within(llmdInstallationPanel as HTMLElement).queryByText('CRD Installed')).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByText('Operator Running')).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByRole('button', { name: /install llm-d/i })).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByRole('button', { name: /^uninstall$/i })).not.toBeInTheDocument()
    expect(within(llmdInstallationPanel as HTMLElement).queryByRole('button')).not.toBeInTheDocument()

    fireEvent.click(screen.getByText('vLLM'))

    await screen.findByText('vLLM Status')

    const vllmCard = screen.getByText('vLLM').closest('.rounded-2xl')
    expect(within(vllmCard as HTMLElement).getByText('vLLM for high-throughput inference')).toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).queryByText('Ray Serve via KubeRay for distributed Ray-based model serving with vLLM')).not.toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).getByText('Runtime is ready to use.')).toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).getByText('Ready')).toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).queryByText('Installed')).not.toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).queryByText('Not Installed')).not.toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).queryByText('CRD')).not.toBeInTheDocument()
    expect(within(vllmCard as HTMLElement).queryByText('Operator')).not.toBeInTheDocument()
    expect(vllmCard).not.toHaveTextContent(/CRD|operator/i)

    const vllmInstallationPanel = screen.getByText('vLLM Status').closest('.rounded-2xl')
    expect(within(vllmInstallationPanel as HTMLElement).getByText('Ready')).toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByText('Installed')).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByText('Not Installed')).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).getAllByText('Runtime is ready to use.').length).toBeGreaterThan(0)
    expect(vllmInstallationPanel).not.toHaveTextContent(/CRD|operator/i)
    expect(vllmInstallationPanel).not.toHaveTextContent('Manual Installation Steps')
    expect(within(vllmInstallationPanel as HTMLElement).queryByText('CRD Installed')).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByText('Operator Running')).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByRole('button', { name: /install vllm/i })).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByRole('button', { name: /^uninstall$/i })).not.toBeInTheDocument()
    expect(within(vllmInstallationPanel as HTMLElement).queryByRole('button')).not.toBeInTheDocument()
  })

  it('keeps a runtime in a starting state after install command succeeds but operator is not ready yet', async () => {
    mutateAsync.mockResolvedValueOnce({
      success: true,
      message: 'Kuberay installed successfully',
    })

    render(
      <MemoryRouter initialEntries={['/settings?tab=runtimes']}>
        <SettingsPage />
      </MemoryRouter>
    )

    fireEvent.click(screen.getByText('Kuberay'))
    fireEvent.click(screen.getByRole('button', { name: /install kuberay/i }))

    await waitFor(() => {
      expect(toast).toHaveBeenCalledWith({
        title: 'Installation Started',
        description: 'Kuberay installed successfully. Waiting for the runtime service to become ready.',
      })
    })

    const installationPanel = screen.getByText('Kuberay Installation').closest('.rounded-2xl')
    expect(within(installationPanel as HTMLElement).getByText('Starting')).toBeInTheDocument()
    expect(within(installationPanel as HTMLElement).getByText('Install command completed. Waiting for the runtime service to become ready...')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /install kuberay/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /checking runtime/i })).toBeDisabled()
  })

  it('uses success badge styling for readable integration connection states', () => {
    mockGpuStatus = {
      installed: true,
      gpusAvailable: true,
      operatorRunning: true,
      totalGPUs: 4,
      message: 'GPU support is ready',
      gpuNodes: ['worker-a'],
      helmCommands: [],
    }
    mockHfStatus = {
      configured: true,
      user: {
        name: 'test-user',
        fullname: 'Test User',
      },
    }

    render(
      <MemoryRouter initialEntries={['/settings?tab=integrations']}>
        <SettingsPage />
      </MemoryRouter>
    )

    expect(screen.getByText('GPUs Enabled')).toHaveClass('bg-green-500/15', 'text-green-600', 'dark:text-green-400')
    expect(screen.getByText('Connected')).toHaveClass('bg-green-500/15', 'text-green-600', 'dark:text-green-400')
  })

  it('shows the installed Inference Extension version instead of the pinned install version', () => {
    mockGatewayStatus = {
      gatewayApiInstalled: true,
      inferenceExtInstalled: true,
      gatewayAvailable: false,
      installCommands: [],
      message: 'Gateway API and Inference Extension CRDs are installed. No active gateway detected.',
      pinnedVersion: 'v1.3.1',
      gatewayApiVersion: undefined,
      inferenceExtVersion: 'v1.5.0',
    }

    render(
      <MemoryRouter initialEntries={['/settings?tab=integrations']}>
        <SettingsPage />
      </MemoryRouter>
    )

    const inferenceExtensionLabel = screen.getByText('Inference Extension').closest('div')
    expect(inferenceExtensionLabel).toHaveTextContent('(v1.5.0)')
    expect(inferenceExtensionLabel).not.toHaveTextContent('v1.3.1')
  })

  it('uses the Hugging Face emoji on the connect button', () => {
    render(
      <MemoryRouter initialEntries={['/settings?tab=integrations']}>
        <SettingsPage />
      </MemoryRouter>
    )

    expect(screen.getByRole('button', { name: /sign in with hugging face/i })).toHaveTextContent('🤗')
  })
})
