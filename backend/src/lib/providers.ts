const PROVIDER_DISPLAY_NAMES: Record<string, string> = {
  kaito: 'KAITO',
  dynamo: 'Dynamo',
  kuberay: 'KubeRay',
  llmd: 'LLM-D',
  vllm: 'vLLM',
};

const CRD_LESS_PROVIDER_IDS = new Set([
  'llmd',
  'vllm',
]);

const CRD_LESS_PROVIDER_DISPLAY_NAMES = new Set([
  'LLM-D',
  'vLLM',
]);

function normalizeCanonicalProviderId(providerId: string | null | undefined): string {
  return String(providerId ?? '').toLowerCase();
}

function isCanonicalCrdLessProviderId(providerId: string | null | undefined): boolean {
  const normalizedProviderId = normalizeCanonicalProviderId(providerId);
  return CRD_LESS_PROVIDER_IDS.has(normalizedProviderId);
}

function isCrdLessProviderDisplayName(providerName: string | null | undefined): boolean {
  return CRD_LESS_PROVIDER_DISPLAY_NAMES.has(String(providerName ?? '').trim());
}

export function providerRequiresRuntimeCRD(
  providerId: string,
  explicitRequiresCRD?: unknown,
  providerName?: string | null,
): boolean {
  if (typeof explicitRequiresCRD === 'boolean') {
    return explicitRequiresCRD;
  }

  if (isCanonicalCrdLessProviderId(providerId) || isCrdLessProviderDisplayName(providerName)) {
    return false;
  }

  return true;
}

const DISPLAY_NAME_ANNOTATION_KEYS = [
  'airunway.ai/provider-name',
  'airunway.io/provider-name',
  'airunway.ai/display-name',
  'airunway.io/display-name',
];

export function getAnnotatedProviderDisplayName(
  annotations?: Record<string, unknown>,
): string | undefined {
  for (const key of DISPLAY_NAME_ANNOTATION_KEYS) {
    const value = annotations?.[key];
    if (typeof value === 'string' && value.trim().length > 0) {
      return value.trim();
    }
  }

  return undefined;
}

export function getProviderDisplayName(
  providerId: string,
  annotations?: Record<string, unknown>,
): string {
  const annotatedDisplayName = getAnnotatedProviderDisplayName(annotations);
  if (annotatedDisplayName) {
    return annotatedDisplayName;
  }

  const normalizedProviderId = providerId.toLowerCase();
  const knownDisplayName = PROVIDER_DISPLAY_NAMES[normalizedProviderId];
  if (knownDisplayName) {
    return knownDisplayName;
  }

  return providerId.charAt(0).toUpperCase() + providerId.slice(1);
}
