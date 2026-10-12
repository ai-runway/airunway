import type { ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';

interface RouteRegistration {
  path: string;
  name: string;
  component: () => ReactNode;
}

interface SidebarRegistration {
  name: string;
  parent: string | null;
  url: string;
}

const registrations = vi.hoisted(() => ({
  routes: [] as RouteRegistration[],
  sidebars: [] as SidebarRegistration[],
  settings: [] as unknown[][],
}));

vi.mock('@kinvolk/headlamp-plugin/lib', () => ({
  registerRoute: (route: RouteRegistration) => registrations.routes.push(route),
  registerSidebarEntry: (entry: SidebarRegistration) => registrations.sidebars.push(entry),
  registerPluginSettings: (...args: unknown[]) => registrations.settings.push(args),
}));

vi.mock('./pages/CreateDeployment', () => ({ CreateDeployment: () => 'create' }));
vi.mock('./pages/DeploymentDetails', () => ({ DeploymentDetails: () => 'details' }));
vi.mock('./pages/DeploymentsList', () => ({ DeploymentsList: () => 'deployments' }));
vi.mock('./pages/GatewayStatus', () => ({ GatewayStatus: () => 'gateway' }));
vi.mock('./pages/HuggingFaceCallback', () => ({ HuggingFaceCallback: () => 'callback' }));
vi.mock('./pages/Integrations', () => ({ Integrations: () => 'integrations' }));
vi.mock('./pages/ModelsCatalog', () => ({ ModelsCatalog: () => 'models' }));
vi.mock('./pages/RuntimesStatus', () => ({ RuntimesStatus: () => 'runtimes' }));
vi.mock('./settings', () => ({ PluginSettings: () => 'settings' }));

await import('./index');

describe('plugin registration', () => {
  it('registers every sidebar and route, with create before deployment details', () => {
    expect(registrations.sidebars.map(({ name }) => name)).toEqual([
      'airunway',
      'kf-deployments',
      'kf-models',
      'kf-runtimes',
      'kf-gateway',
      'kf-integrations',
      'kf-settings',
    ]);

    expect(registrations.routes.map(({ name }) => name)).toEqual([
      'Create Deployment',
      'AI Runway Deployments',
      'Deployment Details',
      'AI Runway Models',
      'AI Runway Runtimes',
      'AI Runway Gateway',
      'AI Runway Integrations',
      'HuggingFace Callback',
      'AI Runway Settings',
    ]);

    const createIndex = registrations.routes.findIndex(({ name }) => name === 'Create Deployment');
    const detailsIndex = registrations.routes.findIndex(
      ({ name }) => name === 'Deployment Details'
    );
    expect(createIndex).toBeLessThan(detailsIndex);
    const createElement = registrations.routes[createIndex]?.component() as {
      type?: () => ReactNode;
    };
    expect(createElement.type?.()).toBe('create');
    expect(registrations.settings[0]?.[0]).toBe('ai-runway-headlamp-plugin');
    expect(registrations.settings[0]?.[2]).toBe(true);
  });
});
