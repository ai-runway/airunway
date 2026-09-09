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
    showRuntime({ installationState: 'unknown', installed: healthy, healthy, installable: true });

    expect(await screen.findByText('Status unknown')).toHaveAttribute('data-status', '');
    expect(screen.getAllByText('Not checked')).toHaveLength(2);
    expect(screen.queryByText('Running')).not.toBeInTheDocument();
    expect(screen.queryByText('Not Installed')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Deploy|Upgrade|Uninstall/ })).not.toBeInTheDocument();
  });

  it.each([
    ['installed', true],
    ['installed', false],
    [undefined, true],
    [undefined, false],
  ] as const)('uses installation rather than readiness for management actions (%s, %s)', async (installationState, healthy) => {
    showRuntime({ installationState, installed: true, healthy });

    expect(await screen.findByText(healthy ? 'Healthy' : 'Unhealthy'))
      .toHaveAttribute('data-status', healthy ? 'success' : 'warning');
    expect(screen.getByText('Installed')).toBeInTheDocument();
    expect(screen.getByText(healthy ? 'Running' : 'Not Running')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Upgrade' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Uninstall' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: 'Deploy' })).not.toBeInTheDocument();
  });

  it.each([
    ['not-installed', true],
    ['not-installed', false],
    [undefined, true],
    [undefined, false],
  ] as const)('offers deployment for missing installation regardless of readiness (%s, %s)', async (installationState, healthy) => {
    showRuntime({
      installationState,
      healthy,
      crdFound: false,
      operatorRunning: healthy,
      requiresCRD: true,
      installable: true,
    });

    expect(await screen.findByRole('button', { name: 'Deploy' })).toBeEnabled();
    expect(screen.getAllByText('Not Installed')).toHaveLength(2);
    for (const label of screen.getAllByText('Not Installed')) {
      expect(label).toHaveAttribute('data-status', 'error');
    }
    expect(screen.getByText(healthy ? 'Running' : 'Not Running')).toBeInTheDocument();
    expect(screen.queryByText('Healthy')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Upgrade|Uninstall/ })).not.toBeInTheDocument();
  });

  it('shows the CRD probe independently when installation is incomplete', async () => {
    showRuntime({
      installationState: 'not-installed',
      crdFound: true,
      operatorRunning: false,
      requiresCRD: true,
      installable: true,
    });

    expect(await screen.findByText('Installed')).toHaveAttribute('data-status', 'success');
    expect(screen.getByText('Not Installed')).toHaveAttribute('data-status', 'error');
    expect(screen.getByText('Not Running')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Deploy' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: /Upgrade|Uninstall/ })).not.toBeInTheDocument();
  });

  it.each([true, false])('honors the explicit verdict over an opposite legacy flag (installed: %s)', async (installed) => {
    showRuntime({
      installationState: installed ? 'installed' : 'not-installed',
      installed: !installed,
      healthy: true,
      crdFound: installed,
      operatorRunning: true,
      installable: true,
    });

    await screen.findByText('Running');
    expect(screen.getAllByRole('status')[0])
      .toHaveTextContent(installed ? 'Healthy' : 'Not Installed');
    expect(screen.queryAllByRole('button', { name: 'Deploy' })).toHaveLength(installed ? 0 : 1);
    expect(screen.queryAllByRole('button', { name: 'Upgrade' })).toHaveLength(installed ? 1 : 0);
    expect(screen.queryAllByRole('button', { name: 'Uninstall' })).toHaveLength(installed ? 1 : 0);
  });

  it.each([true, false])('hides installation actions when explicitly non-installable (installed: %s)', async (installed) => {
    showRuntime({
      installationState: installed ? 'installed' : 'not-installed',
      installed,
      healthy: installed,
      installable: false,
    });

    await screen.findByText(installed ? 'Healthy' : 'Not Running');
    expect(screen.queryByRole('button', { name: /Deploy|Upgrade|Uninstall/ })).not.toBeInTheDocument();
  });
});
