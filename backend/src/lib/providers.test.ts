import { describe, expect, test } from 'bun:test';
import { getProviderDisplayName, providerRequiresRuntimeCRD } from './providers';

describe('provider metadata helpers', () => {
  test('uses known display names for CRD-less providers', () => {
    expect(getProviderDisplayName('llmd')).toBe('LLM-D');
    expect(getProviderDisplayName('vllm')).toBe('vLLM');
  });

  test('defaults canonical CRD-less providers to not requiring runtime CRDs', () => {
    expect(providerRequiresRuntimeCRD('llmd')).toBe(false);
    expect(providerRequiresRuntimeCRD('vllm')).toBe(false);
  });

  test('does not treat non-canonical llm-d or vLLM-like IDs as CRD-less aliases', () => {
    expect(providerRequiresRuntimeCRD('llmdruntime')).toBe(true);
    expect(providerRequiresRuntimeCRD('llmd-provider')).toBe(true);
    expect(providerRequiresRuntimeCRD('vllmruntime')).toBe(true);
    expect(providerRequiresRuntimeCRD('vLLM-provider')).toBe(true);
  });

  test('does not let stale requiresCRD flags override canonical or display-name CRD-less providers', () => {
    expect(providerRequiresRuntimeCRD('llmd', true)).toBe(false);
    expect(providerRequiresRuntimeCRD('vllm', true)).toBe(false);
    expect(providerRequiresRuntimeCRD('custom-llmd-registration', true, 'LLM-D')).toBe(false);
    expect(providerRequiresRuntimeCRD('custom-vllm-registration', true, 'vLLM')).toBe(false);
  });

  test('preserves explicit requiresCRD flags for operator-backed providers', () => {
    expect(providerRequiresRuntimeCRD('dynamo', false)).toBe(false);
    expect(providerRequiresRuntimeCRD('custom-provider', true, 'Custom Provider')).toBe(true);
  });

  test('defaults operator-backed providers to requiring runtime CRDs', () => {
    expect(providerRequiresRuntimeCRD('dynamo')).toBe(true);
    expect(providerRequiresRuntimeCRD('kaito')).toBe(true);
    expect(providerRequiresRuntimeCRD('kuberay')).toBe(true);
  });
});
