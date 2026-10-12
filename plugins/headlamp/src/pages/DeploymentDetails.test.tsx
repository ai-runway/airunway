import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
  deleteDeployment: vi.fn(),
  getDeployment: vi.fn(),
  getLogs: vi.fn(),
  getMetrics: vi.fn(),
  getPods: vi.fn(),
  push: vi.fn(),
  createRouteURL: vi.fn(() => '/airunway/deployments'),
  apiClient: undefined as unknown as {
    deployments: {
      delete: ReturnType<typeof vi.fn>;
      get: ReturnType<typeof vi.fn>;
      getLogs: ReturnType<typeof vi.fn>;
      getMetrics: ReturnType<typeof vi.fn>;
      getPods: ReturnType<typeof vi.fn>;
    };
  },
}));

mocks.apiClient = {
  deployments: {
    delete: mocks.deleteDeployment,
    get: mocks.getDeployment,
    getLogs: mocks.getLogs,
    getMetrics: mocks.getMetrics,
    getPods: mocks.getPods,
  },
};

vi.mock('../lib/api-client', () => ({ useApiClient: () => mocks.apiClient }));
vi.mock('react-router-dom', () => ({
  useHistory: () => ({ push: mocks.push }),
  useParams: () => ({ name: 'qwen', namespace: 'models' }),
}));
vi.mock('@kinvolk/headlamp-plugin/lib', () => ({
  Router: { createRouteURL: mocks.createRouteURL },
}));
vi.mock('@kinvolk/headlamp-plugin/lib/CommonComponents', () => ({
  Loader: ({ title }: { title: string }) => <div>{title}</div>,
  SectionBox: ({ children }: { children: ReactNode }) => <section>{children}</section>,
  SimpleTable: () => null,
  StatusLabel: ({ children }: { children: ReactNode }) => <span>{children}</span>,
  Tabs: ({ tabs }: { tabs: Array<{ component: ReactNode }> }) => <div>{tabs[0]?.component}</div>,
}));
vi.mock('../components/DeleteDialog', () => ({
  DeleteDialog: ({ open, onConfirm }: { open: boolean; onConfirm: () => void }) =>
    open ? <button onClick={onConfirm}>Confirm deletion</button> : null,
}));
vi.mock('@iconify/react', () => ({ Icon: () => null }));

import { DeploymentDetails } from './DeploymentDetails';

describe('DeploymentDetails', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.deleteDeployment.mockResolvedValue({});
    mocks.getDeployment.mockResolvedValue({
      name: 'qwen',
      namespace: 'models',
      phase: 'Ready',
      provider: 'dynamo',
      engine: 'vllm',
      mode: 'aggregated',
      modelId: 'Qwen/Qwen3-0.6B',
      replicas: { ready: 1, desired: 1 },
      createdAt: '2026-01-01T00:00:00.000Z',
      frontendService: 'qwen-frontend',
      conditions: [],
    });
    mocks.getPods.mockResolvedValue({ pods: [] });
    mocks.getMetrics.mockResolvedValue({});
    mocks.getLogs.mockResolvedValue({ logs: '' });
  });

  it('loads the named deployment and deletes it only after confirmation', async () => {
    render(<DeploymentDetails />);

    expect(screen.getByText('Loading deployment details...')).toBeInTheDocument();
    await screen.findByRole('heading', { name: 'qwen' });
    expect(mocks.getDeployment).toHaveBeenCalledWith('qwen', 'models');
    expect(mocks.getPods).toHaveBeenCalledWith('qwen', 'models');

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    expect(mocks.deleteDeployment).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Confirm deletion' }));

    await waitFor(() => {
      expect(mocks.deleteDeployment).toHaveBeenCalledWith('qwen', 'models');
      expect(mocks.push).toHaveBeenCalledWith('/airunway/deployments');
    });
  });
});
