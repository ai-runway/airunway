import { describe, expect, it } from 'vitest';
import { generateDeploymentName } from './deployment-name';

describe('generateDeploymentName', () => {
  it('keeps the generated download Job name within the 63-character label limit', () => {
    const name = generateDeploymentName(`organization/${'model'.repeat(20)}`);

    expect(name.length).toBe(48);
    expect(`${name}-model-download`.length).toBe(63);
  });

  it('normalizes model identifiers into Kubernetes-compatible names', () => {
    expect(generateDeploymentName('Org/Model_Name:Variant.Name')).toBe('org-model-name-variant-name');
    expect(generateDeploymentName(`${'a'.repeat(47)}--model`)).toBe('a'.repeat(47));
  });
});
