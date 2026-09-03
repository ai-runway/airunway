import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
  listModels: vi.fn(),
  searchModels: vi.fn(),
  push: vi.fn(),
  createRouteURL: vi.fn(() => '/airunway/deployments/create'),
  apiClient: undefined as unknown as {
    models: { list: ReturnType<typeof vi.fn> };
    huggingFace: { searchModels: ReturnType<typeof vi.fn> };
  },
}));

mocks.apiClient = {
  models: { list: mocks.listModels },
  huggingFace: { searchModels: mocks.searchModels },
};

vi.mock('../lib/api-client', () => ({
  useApiClient: () => mocks.apiClient,
}));

vi.mock('react-router-dom', () => ({
  useHistory: () => ({ push: mocks.push }),
}));

vi.mock('@kinvolk/headlamp-plugin/lib', () => ({
  Router: { createRouteURL: mocks.createRouteURL },
}));

vi.mock('@kinvolk/headlamp-plugin/lib/CommonComponents', () => ({
  SectionBox: ({ children }: { children: ReactNode }) => <section>{children}</section>,
  Loader: ({ title }: { title: string }) => <div>{title}</div>,
  Tabs: ({ tabs }: { tabs: Array<{ component: ReactNode }> }) => <div>{tabs[0]?.component}</div>,
}));

vi.mock('@iconify/react', () => ({ Icon: () => null }));

import { ModelsCatalog } from './ModelsCatalog';

describe('ModelsCatalog', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.listModels.mockResolvedValue({
      models: [
        {
          id: 'Qwen/Qwen3-0.6B',
          name: 'Qwen 3 0.6B',
          description: 'A compact instruction model',
          size: '0.6B',
          task: 'text-generation',
          supportedEngines: ['vllm'],
          minGpus: 1,
        },
      ],
    });
  });

  it('loads curated models and routes the selected model into deployment creation', async () => {
    render(<ModelsCatalog />);

    expect(screen.getByText('Loading models...')).toBeInTheDocument();
    await screen.findByText('Qwen 3 0.6B');
    expect(screen.getByText('GPU')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Deploy' }));

    await waitFor(() => {
      expect(mocks.createRouteURL).toHaveBeenCalledWith('Create Deployment');
      expect(mocks.push).toHaveBeenCalledWith(
        '/airunway/deployments/create?modelId=Qwen%2FQwen3-0.6B'
      );
    });
  });
});
