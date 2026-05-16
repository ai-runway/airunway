/**
 * Plugin routes tests
 *
 * Basic tests to ensure the plugin routes are configured correctly.
 */

import { describe, it, expect } from 'vitest';
import { ROUTES, getDeploymentDetailsUrl } from './routes';

describe('AI Runway Plugin Routes', () => {
  it('exports ROUTES configuration', () => {
    expect(ROUTES).toBeDefined();
    expect(ROUTES.BASE).toBe('/airunway');
    expect(ROUTES.DEPLOYMENTS).toBe('/airunway/deployments');
    expect(ROUTES.MODELS).toBe('/airunway/models');
    expect(ROUTES.RUNTIMES).toBe('/airunway/runtimes');
    expect(ROUTES.GATEWAY).toBe('/airunway/gateway');
    expect(ROUTES.SETTINGS).toBe('/airunway/settings');
    expect(ROUTES.CREATE_DEPLOYMENT).toBe('/airunway/deployments/create');
  });

  it('generates correct deployment details URL', () => {
    const url = getDeploymentDetailsUrl('my-deployment', 'default');
    expect(url).toBe('/airunway/deployments/default/my-deployment');
  });

  it('encodes special characters in deployment details URL', () => {
    const url = getDeploymentDetailsUrl('my deployment', 'my namespace');
    expect(url).toBe('/airunway/deployments/my%20namespace/my%20deployment');
  });
});
