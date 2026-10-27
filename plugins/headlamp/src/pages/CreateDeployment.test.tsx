import type { PropsWithChildren } from 'react';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { CreateDeployment } from './CreateDeployment';

const api = {
  runtimes: { getStatus: vi.fn() },
  models: { list: vi.fn() },
  huggingFace: { getSecretStatus: vi.fn() },
  deployments: { create: vi.fn() },
};
const push = vi.fn();

vi.mock('../lib/api-client', () => ({ useApiClient: () => api }));
vi.mock('react-router-dom', () => ({
  useHistory: () => ({ push }),
  useLocation: () => ({ search: '?modelId=test/model' }),
}));
vi.mock('@kinvolk/headlamp-plugin/lib', () => ({
  Router: { createRouteURL: () => '/deployments' },
}));
vi.mock('@kinvolk/headlamp-plugin/lib/CommonComponents', () => ({
  SectionBox: ({ children }: PropsWithChildren) => <section>{children}</section>,
  Loader: ({ title }: { title: string }) => <div>{title}</div>,
}));
vi.mock('@iconify/react', () => ({ Icon: () => null }));
vi.mock('../components/StorageVolumesEditor', () => ({ StorageVolumesEditor: () => null }));
vi.mock('../components/ManifestPreview', () => ({
  ManifestPreview: ({ manifest }: { manifest: string | null }) => (
    <pre data-testid="manifest">{manifest}</pre>
  ),
}));

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

it('enables prefix caching in both the preview and submitted deployment', async () => {
  api.runtimes.getStatus.mockResolvedValue({
    runtimes: [{ id: 'dynamo', name: 'Dynamo', installed: true, healthy: true }],
  });
  api.models.list.mockResolvedValue({
    models: [{
      id: 'test/model',
      name: 'Test Model',
      description: '',
      size: '1B',
      minGpuMemory: '8GB',
      task: 'text-generation',
      supportedEngines: ['vllm'],
    }],
  });
  api.huggingFace.getSecretStatus.mockResolvedValue({ configured: false });
  api.deployments.create.mockResolvedValue({});

  render(<CreateDeployment />);
  const deploy = await screen.findByRole('button', { name: 'Deploy Model' });
  await waitFor(() => expect(deploy).toBeEnabled());

  fireEvent.click(screen.getByRole('button', { name: /Preview Manifest/ }));
  await waitFor(() => {
    const manifest = JSON.parse(screen.getByTestId('manifest').textContent || '{}');
    expect(manifest.spec.engine.enablePrefixCaching).toBe(true);
  });

  fireEvent.click(deploy);
  await waitFor(() => expect(api.deployments.create).toHaveBeenCalledWith(
    expect.objectContaining({ enablePrefixCaching: true })
  ));
  await waitFor(() => expect(push).toHaveBeenCalledWith('/deployments'));
});
