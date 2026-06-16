import type { RuntimeStatus } from '@airunway/shared';

import {
  aggregateRequiresCRDFromCapabilities,
  getAnnotatedProviderDisplayName,
  getProviderDisplayName,
  providerRequiresRuntimeCRD,
} from '../lib/providers';
import { getProviderHealth } from './providerHealth';
import type { InstallationStatus } from './runtimeInstallation';

export interface InferenceProviderConfigResource {
  metadata?: {
    name?: string;
    annotations?: Record<string, string>;
  };
  spec?: {
    capabilities?: { engines?: unknown[] } & Record<string, unknown>;
  };
  status?: {
    version?: string;
    ready?: boolean;
    lastHeartbeat?: string;
    conditions?: Array<{ type?: string; reason?: string; message?: string }>;
  } & Record<string, unknown>;
}

export interface RuntimeStatusAdapter {
  checkCRDInstallation(): Promise<Pick<InstallationStatus, 'installed'>>;
  listInferenceProviderConfigs(): Promise<InferenceProviderConfigResource[]>;
  getInferenceProviderConfig(name: string): Promise<InferenceProviderConfigResource | null>;
  checkProviderInstallationStatus(
    providerId: string,
    status?: { ready?: boolean },
    providerName?: string,
    requiresCRD?: boolean,
  ): Promise<InstallationStatus>;
  getK8sStatusCode(error: unknown): number | undefined;
  getK8sErrorMessage(error: unknown): string;
  logWarn(context: Record<string, unknown>, message: string): void;
}

export async function getRuntimesStatus(adapter: RuntimeStatusAdapter): Promise<RuntimeStatus[]> {
  const runtimes: RuntimeStatus[] = [];

  const crdStatus = await adapter.checkCRDInstallation();
  if (!crdStatus.installed) {
    return runtimes;
  }

  try {
    const items = await adapter.listInferenceProviderConfigs();
    const runtimeEntries = await Promise.all(
      items.map(async (item): Promise<RuntimeStatus> => runtimeStatusFromProviderConfig(adapter, item))
    );
    runtimes.push(...runtimeEntries);
  } catch (error) {
    const statusCode = adapter.getK8sStatusCode(error);
    if (statusCode !== 404) {
      adapter.logWarn({ error: adapter.getK8sErrorMessage(error) }, 'Failed to list InferenceProviderConfigs');
    }
  }

  return runtimes;
}

export async function runtimeStatusFromProviderConfig(
  adapter: Pick<RuntimeStatusAdapter, 'checkProviderInstallationStatus'>,
  item: InferenceProviderConfigResource,
): Promise<RuntimeStatus> {
  const name = item.metadata?.name || 'unknown';
  const status = item.status || {};
  const annotations = item.metadata?.annotations;
  const displayName = getProviderDisplayName(name, annotations);
  const annotatedDisplayName = getAnnotatedProviderDisplayName(annotations);
  const requiresCRD = providerRequiresRuntimeCRD(
    name,
    aggregateRequiresCRDFromCapabilities(item.spec?.capabilities),
    annotatedDisplayName,
  );
  const runtimeStatus = await adapter.checkProviderInstallationStatus(name, status, displayName, requiresCRD);

  const health = getProviderHealth(name, item);
  const useShimMessage = health.stale || (!health.healthy && health.hasShimSignal);
  const message = useShimMessage ? health.message : runtimeStatus.message;

  return {
    id: name,
    name: displayName,
    installed: runtimeStatus.installed,
    healthy: runtimeStatus.operatorRunning ?? false,
    crdFound: runtimeStatus.crdFound ?? runtimeStatus.installed,
    operatorRunning: runtimeStatus.operatorRunning ?? false,
    requiresCRD: runtimeStatus.requiresCRD ?? requiresCRD,
    version: status.version,
    message,
  };
}

export async function getInferenceProviderConfig(
  adapter: RuntimeStatusAdapter,
  name: string,
): Promise<InferenceProviderConfigResource | null> {
  return adapter.getInferenceProviderConfig(name);
}
