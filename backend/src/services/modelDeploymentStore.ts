import type {
  DeploymentConfig,
  DeploymentStatus,
  ModelDeployment,
  PodStatus,
} from '@airunway/shared';
import { toDeploymentStatus, toModelDeploymentManifest } from '@airunway/shared';

export interface ModelDeploymentStoreAdapter {
  listClusterModelDeployments(userToken?: string): Promise<unknown>;
  listNamespacedModelDeployments(namespace: string, userToken?: string): Promise<unknown>;
  getModelDeployment(name: string, namespace: string, userToken?: string): Promise<unknown>;
  createModelDeployment(namespace: string, manifest: Record<string, unknown>, userToken?: string): Promise<void>;
  deleteModelDeployment(name: string, namespace: string, userToken?: string): Promise<void>;
  listNamespaces(): Promise<string[]>;
  canListModelDeployments(namespace: string, userToken: string): Promise<boolean>;
  getDeploymentPods(name: string, namespace: string): Promise<PodStatus[]>;
  getDeploymentStatus(name: string, namespace: string, userToken?: string): Promise<DeploymentStatus | null>;
  logDebug(contextOrMessage: Record<string, unknown> | string, message?: string): void;
  logInfo(context: Record<string, unknown>, message: string): void;
  logError(context: Record<string, unknown>, message: string): void;
  getK8sStatusCode(error: unknown): number | undefined;
  getK8sErrorMessage(error: unknown): string;
}

export async function listDeployments(
  adapter: ModelDeploymentStoreAdapter,
  namespace?: string,
  userToken?: string
): Promise<DeploymentStatus[]> {
  adapter.logDebug({ namespace: namespace || 'all' }, 'listDeployments called');

  if (namespace) {
    return listDeploymentsInNamespace(adapter, namespace, userToken);
  }

  // No namespace specified — try cluster-wide list first
  try {
    const response = await adapter.listClusterModelDeployments(userToken);
    return convertToDeploymentStatuses(adapter, response);
  } catch (error) {
    const statusCode = adapter.getK8sStatusCode(error);

    // If user lacks cluster-wide list permission, fall back to per-namespace listing
    if (statusCode === 403 && userToken) {
      adapter.logDebug('Cluster-wide list forbidden, falling back to per-namespace listing');
      return listDeploymentsAcrossAllowedNamespaces(adapter, userToken);
    }

    if (adapter.getK8sErrorMessage(error) === 'HTTP request failed' || statusCode === 404) {
      adapter.logDebug('ModelDeployment CRD not found');
      return [];
    }

    adapter.logError({ error: adapter.getK8sErrorMessage(error) }, 'Unexpected error listing deployments');
    return [];
  }
}

/**
 * Get a ModelDeployment by name and namespace, including current pod status.
 */
export async function getDeployment(
  adapter: ModelDeploymentStoreAdapter,
  name: string,
  namespace: string,
  userToken?: string
): Promise<DeploymentStatus | null> {
  try {
    const response = await adapter.getModelDeployment(name, namespace, userToken);
    const md = response as ModelDeployment;
    const pods = await adapter.getDeploymentPods(name, namespace);
    return toDeploymentStatus(md, pods);
  } catch (error) {
    const statusCode = adapter.getK8sStatusCode(error);
    if (statusCode === 404) {
      adapter.logDebug({ name, namespace }, 'ModelDeployment not found');
      return null;
    }
    adapter.logError({ error, name, namespace }, 'Error getting deployment');
    return null;
  }
}

/**
 * Get the raw Custom Resource manifest for a deployment.
 * Returns the full CR object as stored in Kubernetes.
 */
export async function getDeploymentManifest(
  adapter: ModelDeploymentStoreAdapter,
  name: string,
  namespace: string,
  userToken?: string
): Promise<Record<string, unknown> | null> {
  try {
    const response = await adapter.getModelDeployment(name, namespace, userToken);
    return response as Record<string, unknown>;
  } catch (error) {
    const statusCode = adapter.getK8sStatusCode(error);
    if (statusCode === 404) {
      adapter.logDebug({ name, namespace }, 'ModelDeployment manifest not found');
      return null;
    }
    adapter.logError({ error, name, namespace }, 'Error getting deployment manifest');
    return null;
  }
}

export async function createDeployment(
  adapter: ModelDeploymentStoreAdapter,
  config: DeploymentConfig,
  userToken?: string
): Promise<void> {
  const manifest = toModelDeploymentManifest(config) as unknown as Record<string, unknown>;

  adapter.logInfo({ name: config.name, namespace: config.namespace }, 'Creating ModelDeployment');
  await adapter.createModelDeployment(config.namespace, manifest, userToken);
  adapter.logInfo({ name: config.name, namespace: config.namespace }, 'ModelDeployment created');
}

export async function deleteDeployment(
  adapter: ModelDeploymentStoreAdapter,
  name: string,
  namespace: string,
  userToken?: string
): Promise<void> {
  // First, check if deployment exists. Keep this as an adapter method so the
  // public KubernetesService.getDeployment seam remains overrideable.
  const deployment = await adapter.getDeploymentStatus(name, namespace, userToken);
  if (!deployment) {
    throw new Error(`Deployment '${name}' not found in namespace '${namespace}'`);
  }

  adapter.logInfo({ name, namespace }, 'Deleting ModelDeployment');
  await adapter.deleteModelDeployment(name, namespace, userToken);
  adapter.logInfo({ name, namespace }, 'ModelDeployment deleted');
}

async function listDeploymentsInNamespace(
  adapter: ModelDeploymentStoreAdapter,
  namespace: string,
  userToken?: string
): Promise<DeploymentStatus[]> {
  try {
    const response = await adapter.listNamespacedModelDeployments(namespace, userToken);
    return convertToDeploymentStatuses(adapter, response, namespace);
  } catch (error) {
    const statusCode = adapter.getK8sStatusCode(error);
    if (adapter.getK8sErrorMessage(error) === 'HTTP request failed' || statusCode === 404 || statusCode === 403) {
      adapter.logDebug({ namespace }, 'Cannot list deployments in namespace');
      return [];
    }

    adapter.logError({ error: adapter.getK8sErrorMessage(error) }, 'Unexpected error listing deployments');
    return [];
  }
}

async function convertToDeploymentStatuses(
  adapter: ModelDeploymentStoreAdapter,
  response: unknown,
  fallbackNamespace?: string
): Promise<DeploymentStatus[]> {
  const items = (response as { items?: ModelDeployment[] }).items || [];
  adapter.logDebug({ count: items.length }, 'Found ModelDeployments');

  const deployments: DeploymentStatus[] = [];
  for (const item of items) {
    const itemNamespace = item.metadata.namespace || fallbackNamespace || 'default';
    const pods = await adapter.getDeploymentPods(item.metadata.name, itemNamespace);
    deployments.push(toDeploymentStatus(item, pods));
  }

  return sortDeploymentsByCreatedAtDesc(deployments);
}

async function listDeploymentsAcrossAllowedNamespaces(
  adapter: ModelDeploymentStoreAdapter,
  userToken: string
): Promise<DeploymentStatus[]> {
  let namespaces: string[];
  try {
    namespaces = await adapter.listNamespaces();
  } catch (error) {
    adapter.logError({ error }, 'Failed to list namespaces for RBAC fallback');
    return [];
  }

  const allowedNamespaces: string[] = [];
  await Promise.all(
    namespaces.map(async (ns) => {
      try {
        if (await adapter.canListModelDeployments(ns, userToken)) {
          allowedNamespaces.push(ns);
        }
      } catch (error) {
        adapter.logDebug({ namespace: ns, error }, 'SelfSubjectAccessReview failed for namespace');
      }
    })
  );

  adapter.logDebug({ allowedNamespaces }, 'User has access to namespaces');

  if (allowedNamespaces.length === 0) {
    return [];
  }

  const results = await Promise.all(
    allowedNamespaces.map(ns => listDeploymentsInNamespace(adapter, ns, userToken))
  );

  return sortDeploymentsByCreatedAtDesc(results.flat());
}

function sortDeploymentsByCreatedAtDesc(deployments: DeploymentStatus[]): DeploymentStatus[] {
  return deployments.sort((a, b) => {
    const dateA = new Date(a.createdAt).getTime();
    const dateB = new Date(b.createdAt).getTime();
    return dateB - dateA;
  });
}
