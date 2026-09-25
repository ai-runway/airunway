import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
  deleteSecret: vi.fn(),
  getGatewayStatus: vi.fn(),
  getGpuStatus: vi.fn(),
  getSecretStatus: vi.fn(),
  apiClient: undefined as unknown as Record<string, object>,
}));

mocks.apiClient = {
  gateway: { getStatus: mocks.getGatewayStatus, installCrds: vi.fn() },
  gpuOperator: { getStatus: mocks.getGpuStatus, install: vi.fn() },
  huggingFace: {
    deleteSecret() {
      return mocks.deleteSecret();
    },
    getSecretStatus() {
      return mocks.getSecretStatus();
    },
  },
};

vi.mock('../lib/api-client', () => ({ useApiClient: () => mocks.apiClient }));
vi.mock('@kinvolk/headlamp-plugin/lib/CommonComponents', () => ({
  Loader: ({ title }: { title: string }) => <div>{title}</div>,
  SectionBox: ({ children, title }: { children: ReactNode; title: string }) => (
    <section aria-label={title}>{children}</section>
  ),
  StatusLabel: ({ children }: { children: ReactNode }) => <span>{children}</span>,
}));
vi.mock('@iconify/react', () => ({ Icon: () => null }));

import { Integrations } from './Integrations';

describe('Integrations', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.deleteSecret.mockResolvedValue({});
    mocks.getGpuStatus.mockResolvedValue({
      installed: true,
      operatorRunning: true,
      gpusAvailable: true,
      totalGPUs: 2,
      gpuNodes: ['worker-a'],
    });
    mocks.getGatewayStatus.mockResolvedValue({
      gatewayAvailable: true,
      gatewayApiInstalled: true,
      inferenceExtInstalled: true,
      pinnedVersion: 'v1.2.0',
      gatewayEndpoint: 'http://gateway.example',
    });
    mocks.getSecretStatus
      .mockResolvedValueOnce({
        configured: true,
        namespaces: [{ namespace: 'models', exists: true }],
        user: { name: 'airunway-user', fullname: 'Airunway User' },
      })
      .mockResolvedValue({ configured: false, namespaces: [] });
  });

  it('shows live integration state and refreshes after disconnecting HuggingFace', async () => {
    render(<Integrations />);

    expect(screen.getByText('Loading integrations...')).toBeInTheDocument();
    await screen.findByText('GPUs Enabled');
    expect(screen.getByText('Gateway API is ready for traffic routing')).toBeInTheDocument();
    expect(screen.getByText('Airunway User')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Disconnect HuggingFace' }));

    await waitFor(() => {
      expect(mocks.deleteSecret).toHaveBeenCalledOnce();
      expect(mocks.getSecretStatus).toHaveBeenCalledTimes(2);
      expect(screen.getByText(/Create a HuggingFace token at/)).toBeInTheDocument();
    });
  });
});
