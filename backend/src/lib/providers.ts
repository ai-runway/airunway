const PROVIDER_DISPLAY_NAMES: Record<string, string> = {
  kaito: 'KAITO',
  dynamo: 'Dynamo',
  kuberay: 'KubeRay',
  llmd: 'LLM-D',
};

const DISPLAY_NAME_ANNOTATION_KEYS = [
  'airunway.ai/provider-name',
  'airunway.io/provider-name',
  'airunway.ai/display-name',
  'airunway.io/display-name',
];

export function getProviderDisplayName(
  providerId: string,
  annotations?: Record<string, unknown>,
): string {
  for (const key of DISPLAY_NAME_ANNOTATION_KEYS) {
    const value = annotations?.[key];
    if (typeof value === 'string' && value.trim().length > 0) {
      return value.trim();
    }
  }

  const normalizedProviderId = providerId.toLowerCase();
  const knownDisplayName = PROVIDER_DISPLAY_NAMES[normalizedProviderId];
  if (knownDisplayName) {
    return knownDisplayName;
  }

  return providerId.charAt(0).toUpperCase() + providerId.slice(1);
}
