export type ProviderHealth = {
  providerId: string;
  healthy: boolean;
  reason: string;
  message: string;
  stale: boolean;
  // True when the CR carries an explicit UpstreamReady condition from the
  // provider shim. Callers use this to decide whether `reason`/`message`
  // contain a shim-authored explanation worth surfacing, versus the generic
  // Ready/NotReady fallback derived from status.ready alone.
  hasShimSignal: boolean;
  lastHeartbeat?: string;
};

/** A single status condition reported on the provider CR. */
type ProviderCondition = {
  type?: string;
  reason?: string;
  message?: string;
};

/** The minimal InferenceProviderConfig shape this module reads. */
type ProviderConfigLike = {
  status?: {
    conditions?: ProviderCondition[];
    lastHeartbeat?: string;
    ready?: boolean;
  };
} | null | undefined;

const DEFAULT_STALENESS_THRESHOLD_MS = 180_000;

function parsePositiveIntEnv(value: string | undefined, fallback: number): number {
  if (value === undefined) return fallback;
  const parsed = Number(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

const STALENESS_THRESHOLD_MS = parsePositiveIntEnv(
  process.env.PROVIDER_HEALTH_STALENESS_MS,
  DEFAULT_STALENESS_THRESHOLD_MS,
);

/**
 * Wraps the shim-reported status conditions on an InferenceProviderConfig with
 * a staleness check. The caller passes the already-fetched config object so
 * this function does no Kubernetes API calls of its own — this keeps
 * getRuntimesStatus's per-provider work bounded to a single CR read.
 */
export function getProviderHealth(providerId: string, config: ProviderConfigLike): ProviderHealth {
  const status = config?.status ?? {};
  const conditions: ProviderCondition[] = status.conditions ?? [];
  const upstreamReady = conditions.find((c) => c.type === 'UpstreamReady');
  const hasShimSignal = !!upstreamReady;
  const lastHeartbeat: string | undefined = status.lastHeartbeat;
  const ready: boolean = status.ready === true;

  // Stale = shim wrote a heartbeat in the past but hasn't refreshed it.
  // Absent heartbeat (no shim running, or provider with no health probe) is
  // NOT stale — just fall through to whatever status conditions report.
  const stale = !!lastHeartbeat
    && Date.now() - new Date(lastHeartbeat).getTime() > STALENESS_THRESHOLD_MS;

  if (stale) {
    return {
      providerId,
      healthy: false,
      reason: 'ShimStale',
      message: 'The provider is not reporting status. Check that the AI Runway provider shim is running.',
      stale: true,
      hasShimSignal,
      lastHeartbeat,
    };
  }

  return {
    providerId,
    healthy: ready,
    reason: upstreamReady?.reason ?? (ready ? 'Ready' : 'NotReady'),
    message: upstreamReady?.message ?? (ready ? 'Provider is installed and running' : 'Provider is not ready'),
    stale: false,
    hasShimSignal,
    lastHeartbeat,
  };
}
