import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
  createDeployment: vi.fn(),
  getRuntimesStatus: vi.fn(),
  getSecretStatus: vi.fn(),
  listModels: vi.fn(),
  push: vi.fn(),
  createRouteURL: vi.fn((name: string) => `/${name.toLowerCase().replace(/ /g, '-')}`),
  apiClient: undefined as unknown as Record<string, object>,
}));

mocks.apiClient = {
  deployments: { create: mocks.createDeployment },
  huggingFace: {
    getSecretStatus() {
      return mocks.getSecretStatus();
    },
    searchModels: vi.fn(),
  },
  models: { list: mocks.listModels },
  runtimes: { getStatus: mocks.getRuntimesStatus },
};

vi.mock('../lib/api-client', () => ({ useApiClient: () => mocks.apiClient }));
vi.mock('react-router-dom', () => ({
  useHistory: () => ({ push: mocks.push }),
  useLocation: () => ({ search: '?modelId=Qwen%2FQwen3-0.6B' }),
}));
vi.mock('@kinvolk/headlamp-plugin/lib', () => ({
  Router: { createRouteURL: mocks.createRouteURL },
}));
vi.mock('@kinvolk/headlamp-plugin/lib/CommonComponents', () => ({
  Loader: ({ title }: { title: string }) => <div>{title}</div>,
  SectionBox: ({ children }: { children: ReactNode }) => <section>{children}</section>,
}));
vi.mock('@iconify/react', () => ({ Icon: () => null }));

import { CreateDeployment } from './CreateDeployment';

describe('CreateDeployment', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.createDeployment.mockResolvedValue({});
    mocks.getSecretStatus.mockResolvedValue({ configured: false, namespaces: [] });
    mocks.getRuntimesStatus.mockResolvedValue({
      runtimes: [
        { id: 'dynamo', name: 'NVIDIA Dynamo', installed: true, healthy: true },
        { id: 'kaito', name: 'KAITO', installed: true, healthy: true },
      ],
    });
    mocks.listModels.mockResolvedValue({
      models: [
        {
          id: 'Qwen/Qwen3-0.6B',
          name: 'Qwen 3 0.6B',
          description: 'Compact instruction model',
          size: '0.6B',
          task: 'text-generation',
          supportedEngines: ['vllm'],
          minGpus: 1,
        },
      ],
    });
  });

  it('creates the selected model with the healthy compatible runtime and routes to the list', async () => {
    render(<CreateDeployment />);

    expect(screen.getByText('Loading model...')).toBeInTheDocument();
    await screen.findByText('Qwen 3 0.6B');
    await waitFor(() => expect(screen.getByLabelText('Namespace')).toHaveValue('dynamo-system'));

    fireEvent.click(screen.getByRole('button', { name: 'Deploy Model' }));

    await waitFor(() => {
      expect(mocks.createDeployment).toHaveBeenCalledWith(
        expect.objectContaining({
          name: 'qwen-qwen3-0-6b',
          namespace: 'dynamo-system',
          modelId: 'Qwen/Qwen3-0.6B',
          provider: 'dynamo',
          engine: 'vllm',
          mode: 'aggregated',
          resources: { gpu: 1 },
        })
      );
      expect(mocks.push).toHaveBeenCalledWith('/ai-runway-deployments');
    });
  });

  it('keeps runtime selection and installation navigation as separate controls', async () => {
    mocks.getRuntimesStatus.mockResolvedValue({
      runtimes: [
        { id: 'dynamo', name: 'NVIDIA Dynamo', installed: false, healthy: false },
        { id: 'kaito', name: 'KAITO', installed: true, healthy: true },
      ],
    });

    render(<CreateDeployment />);

    await screen.findByText('Qwen 3 0.6B');
    await waitFor(() => expect(screen.getByLabelText('Namespace')).toHaveValue('kaito-workspace'));

    const runtimeButton = screen.getByRole('button', { name: /NVIDIA Dynamo/ });
    fireEvent.click(runtimeButton);

    await waitFor(() => expect(screen.getByLabelText('Namespace')).toHaveValue('dynamo-system'));
    const installLink = screen.getByRole('link', { name: 'Install NVIDIA Dynamo' });

    expect(runtimeButton).not.toContainElement(installLink);
    fireEvent.click(installLink);
    expect(mocks.push).toHaveBeenCalledWith('/ai-runway-runtimes');
  });
});
