import type { PropsWithChildren } from 'react';
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { RuntimeStatus } from '@airunway/shared';
import { RuntimesStatus } from './RuntimesStatus';

const api = { runtimes: { getStatus: vi.fn() } };

vi.mock('../lib/api-client', () => ({ useApiClient: () => api }));
vi.mock('@iconify/react', () => ({ Icon: () => null }));
vi.mock('@kinvolk/headlamp-plugin/lib/CommonComponents', () => ({
  SectionBox: ({ children, title }: PropsWithChildren<{ title: string }>) => (
    <section><h2>{title}</h2>{children}</section>
  ),
  Loader: ({ title }: { title: string }) => <div>{title}</div>,
  StatusLabel: ({ children, status }: PropsWithChildren<{ status?: string }>) => (
    <span role="status" data-status={status}>{children}</span>
  ),
}));
vi.mock('../components/ConnectionBanner', () => ({
  ConnectionError: ({ error }: { error: string }) => <div role="alert">{error}</div>,
}));

function showRuntime(overrides: Partial<RuntimeStatus>) {
  api.runtimes.getStatus.mockResolvedValue({
    runtimes: [{
      id: 'custom-runtime',
      name: 'Custom Runtime',
      installed: false,
      healthy: false,
      ...overrides,
    }],
  });
  render(<RuntimesStatus />);
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('RuntimesStatus', () => {
  it.each([true, false])('keeps unknown installation neutral when reported readiness is %s', async (healthy) => {
    showRuntime({ installationState: 'unknown', installed: healthy, healthy });

    expect(await screen.findByText('Status unknown')).toHaveAttribute('data-status', '');
    expect(screen.getAllByText('Not checked')).toHaveLength(2);
    expect(screen.queryByText('Running')).not.toBeInTheDocument();
    expect(screen.queryByText('Not Installed')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Deploy|Upgrade|Uninstall/ })).not.toBeInTheDocument();
  });

  it('retains healthy legacy installation details and actions', async () => {
    showRuntime({ installed: true, healthy: true });

    expect(await screen.findByText('Healthy')).toHaveAttribute('data-status', 'success');
    expect(screen.getByText('Installed')).toBeInTheDocument();
    expect(screen.getByText('Running')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Upgrade' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Uninstall' })).toBeEnabled();
  });

  it('retains installation guidance for confirmed absence', async () => {
    showRuntime({ installationState: 'not-installed' });

    expect(await screen.findByRole('button', { name: 'Deploy' })).toBeEnabled();
    expect(screen.getAllByText('Not Installed')).toHaveLength(2);
    expect(screen.getByText('Not Running')).toBeInTheDocument();
  });
});
