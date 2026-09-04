import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { CreateDeployment } from './CreateDeployment';

const mockApi = vi.hoisted(() => ({
  deployments: {
    create: vi.fn(),
  },
  huggingFace: {
    getSecretStatus: vi.fn().mockResolvedValue({ configured: false }),
    searchModels: vi.fn().mockResolvedValue({
      models: [{
        id: 'org/model',
        name: 'Model',
        estimatedGpuMemory: '8Gi',
        estimatedGpuMemoryGb: 8,
        pipelineTag: 'text-generation',
        supportedEngines: ['vllm'],
        gated: false,
      }],
    }),
  },
  models: {
    list: vi.fn(),
  },
  runtimes: {
    getStatus: vi.fn().mockResolvedValue({
      runtimes: [{
        id: 'dynamo',
        name: 'NVIDIA Dynamo',
        installed: true,
        healthy: true,
      }],
    }),
  },
}));

vi.mock('react-router-dom', () => ({
  useHistory: () => ({ push: vi.fn() }),
  useLocation: () => ({ search: '?modelId=org%2Fmodel&source=huggingface' }),
}));

vi.mock('@kinvolk/headlamp-plugin/lib/CommonComponents', () => ({
  Loader: () => <div>Loading</div>,
  SectionBox: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
}));

vi.mock('@kinvolk/headlamp-plugin/lib', () => ({
  Router: {
    createRouteURL: vi.fn(() => '/airunway/deployments'),
  },
}));

vi.mock('../lib/api-client', () => ({
  useApiClient: () => mockApi,
}));

vi.mock('../components/ManifestPreview', () => ({
  ManifestPreview: () => null,
}));

vi.mock('../components/StorageVolumesEditor', () => ({
  StorageVolumesEditor: ({
    volumes,
    onChange,
  }: {
    volumes: unknown[];
    onChange: (volumes: unknown[]) => void;
  }) => (
    <div data-testid="storage-volumes-editor" data-volume-count={volumes.length}>
      <button
        type="button"
        onClick={() => onChange([{
          name: 'existing-cache',
          purpose: 'modelCache',
          claimName: 'same-name-in-both-namespaces',
        }])}
      >
        Add existing claim volume
      </button>
    </div>
  ),
}));

describe('CreateDeployment', () => {
  it('clears existing claim storage when the namespace is edited directly', async () => {
    render(<CreateDeployment />);

    await screen.findByText(/Storage Volumes/);
    fireEvent.click(screen.getByRole('button', { name: 'Add existing claim volume' }));
    expect(screen.getByTestId('storage-volumes-editor')).toHaveAttribute('data-volume-count', '1');

    const namespaceLabel = screen.getByText('Namespace');
    const namespaceInput = namespaceLabel.parentElement?.querySelector('input');
    expect(namespaceInput).not.toBeNull();
    fireEvent.change(namespaceInput!, { target: { value: 'other-namespace' } });

    await waitFor(() => {
      expect(screen.getByTestId('storage-volumes-editor')).toHaveAttribute('data-volume-count', '0');
    });
  });
});
