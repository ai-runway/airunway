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

/**
 * Aggregate a provider-level `requiresCRD` verdict from
 * `spec.capabilities.engines[].requiresCRD`.
 *
 * The CRD migration in controller/internal/controller/migration.go strips the
 * legacy top-level `capabilities.requiresCRD` field and hoists it into each
 * engine entry, so backend code must derive a provider-level value from the
 * per-engine flags.
 *
 * Semantics:
 * - Returns `true` if any engine explicitly sets `requiresCRD: true`. A
 *   provider needs a runtime CRD if any of its engines needs one.
 * - Returns `false` only when every engine in a non-empty list explicitly
 *   opts out via `requiresCRD: false`.
 * - Returns `undefined` when nothing is explicitly set, or when some engines
 *   omit the flag (per the Go API doc, an omitted value should be treated as
 *   `true` for backward compatibility — but we keep it `undefined` here so
 *   the canonical-id / display-name fallback in `providerRequiresRuntimeCRD`
 *   still runs for legacy configs).
 */
export function aggregateRequiresCRDFromCapabilities(
  capabilities: unknown,
): boolean | undefined {
  const engines = (capabilities as { engines?: unknown })?.engines;
  if (!Array.isArray(engines) || engines.length === 0) {
    return undefined;
  }

  let sawExplicit = false;
  let allFalse = true;
  for (const engine of engines) {
    const value = (engine as { requiresCRD?: unknown })?.requiresCRD;
    if (typeof value === 'boolean') {
      sawExplicit = true;
      if (value) {
        return true;
      }
    } else {
      // omitted — treat as "not explicitly false" so we do not collapse to false.
      allFalse = false;
    }
  }

  if (!sawExplicit) {
    return undefined;
  }
  return allFalse ? false : undefined;
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
