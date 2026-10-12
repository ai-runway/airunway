import { buildPortForwardCommand } from '@airunway/shared';
import { describe, expect, it } from 'vitest';

describe('buildPortForwardCommand', () => {
  it('builds the AIKit llama.cpp port-forward command with the service port', () => {
    const command = buildPortForwardCommand({
      name: 'llama3-2-1b-3aeb',
      namespace: 'kaito-workspace',
      frontendService: 'llama3-2-1b-3aeb:80',
    });

    expect(command).toBe('kubectl port-forward svc/llama3-2-1b-3aeb 8000:80 -n kaito-workspace');
  });

  it('keeps using an explicit frontend service port when one is provided', () => {
    const command = buildPortForwardCommand({
      name: 'custom-runtime-deploy',
      namespace: 'airunway-system',
      frontendService: 'custom-runtime-deploy-frontend:7000',
    });

    expect(command).toBe('kubectl port-forward svc/custom-runtime-deploy-frontend 8000:7000 -n airunway-system');
  });
});
