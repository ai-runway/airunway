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

  test('honors explicit requiresCRD flags for canonical or display-name CRD-less providers', () => {
    expect(providerRequiresRuntimeCRD('llmd', true)).toBe(true);
    expect(providerRequiresRuntimeCRD('vllm', true)).toBe(true);
    expect(providerRequiresRuntimeCRD('custom-llmd-registration', true, 'LLM-D')).toBe(true);
    expect(providerRequiresRuntimeCRD('custom-vllm-registration', true, 'vLLM')).toBe(true);
    expect(providerRequiresRuntimeCRD('llmd', false)).toBe(false);
    expect(providerRequiresRuntimeCRD('custom-llmd-registration', false, 'LLM-D')).toBe(false);
  });

  test('uses CRD-less id and display-name fallbacks only when requiresCRD is omitted', () => {
    expect(providerRequiresRuntimeCRD('llmd', undefined)).toBe(false);
    expect(providerRequiresRuntimeCRD('vllm', undefined)).toBe(false);
    expect(providerRequiresRuntimeCRD('custom-llmd-registration', undefined, 'LLM-D')).toBe(false);
    expect(providerRequiresRuntimeCRD('custom-vllm-registration', undefined, 'vLLM')).toBe(false);
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
