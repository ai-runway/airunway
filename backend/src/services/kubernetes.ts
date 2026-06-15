import * as k8s from '@kubernetes/client-node';
import { configService } from './config';
import type { DeploymentStatus, PodStatus, ClusterStatus, DeploymentConfig, RuntimeStatus, ModelDeployment, GatewayInfo, GatewayModelInfo, GatewayCRDStatus } from '@airunway/shared';

import { withRetry } from '../lib/retry';
import { loadKubeConfig, makeApiClient } from '../lib/kubeconfig';
import { type K8sApiError } from '../lib/k8s-errors';
import logger from '../lib/logger';
import { aggregateRequiresCRDFromCapabilities, getAnnotatedProviderDisplayName, getProviderDisplayName, providerRequiresRuntimeCRD } from '../lib/providers';
import {
  checkOperatorBackedRuntimeInstallationStatus,
  checkRuntimeProviderInstallationStatus,
  type InstallationStatus,
  type RuntimeInstallationAdapter,
  type RuntimeProviderId,
} from './runtimeInstallation';
export type { InstallationStatus } from './runtimeInstallation';
import {
  checkGatewayCRDStatus as checkGatewayCRDStatusWithAdapter,
  getGatewayModels as getGatewayModelsWithAdapter,
  getGatewayStatus as getGatewayStatusWithAdapter,
  type GatewayItem,
  type GatewayStatusAdapter,
} from './gatewayStatus';
import {
  getAllNodePools as getAllNodePoolsWithAdapter,
  getClusterGpuCapacity as getClusterGpuCapacityWithAdapter,
  getDetailedClusterGpuCapacity as getDetailedClusterGpuCapacityWithAdapter,
  type ClusterGpuCapacity,
  type ClusterGpuCapacityAdapter,
} from './clusterGpuCapacity';
export type { ClusterGpuCapacity, NodeGpuInfo } from './clusterGpuCapacity';
import {
  proxyServiceGet as proxyServiceGetWithAdapter,
  proxyServicePostStream as proxyServicePostStreamWithAdapter,
  type ProxyServiceGetOptions,
  type ProxyServiceOptions,
  type ServiceProxyAdapter,
} from './serviceProxy';
import {
  getDeploymentPods as getDeploymentPodsWithAdapter,
  getPodFailureReasons as getPodFailureReasonsWithAdapter,
  getPodLogs as getPodLogsWithAdapter,
  type PodDiagnosticsAdapter,
} from './podDiagnostics';
export { toPodStatus } from './podDiagnostics';
import {
  createDeployment as createDeploymentWithAdapter,
  deleteDeployment as deleteDeploymentWithAdapter,
  getDeployment as getDeploymentWithAdapter,
  getDeploymentManifest as getDeploymentManifestWithAdapter,
  listDeployments as listDeploymentsWithAdapter,
  type ModelDeploymentStoreAdapter,
} from './modelDeploymentStore';

// ModelDeployment CRD configuration
const MODEL_DEPLOYMENT_CRD = {
  apiGroup: 'airunway.ai',
  apiVersion: 'v1alpha1',
  plural: 'modeldeployments',
  kind: 'ModelDeployment',
};

/**
 * GPU availability information from cluster nodes
 */
export interface GPUAvailability {
  available: boolean;
  totalGPUs: number;
  gpuNodes: string[];
}

/**
 * GPU Operator installation status
 */
export interface GPUOperatorStatus {
  installed: boolean;
  crdFound: boolean;
  operatorRunning: boolean;
  gpusAvailable: boolean;
  totalGPUs: number;
  gpuNodes: string[];
  message: string;
}

export interface PersistentVolumeClaimInfo {
  name: string;
  status: string;
  storageClass: string;
  capacity: string;
}

/**
 * Extract the first non-empty version annotation from a Kubernetes CRD object or
 * Kubernetes client response wrapper. The generated Kubernetes client has used
 * both shapes across versions (`response.body` and the resource object itself).
 */
export function extractCRDVersionFromAnnotations(crdOrResponse: unknown, annotationKeys: string[]): string | undefined {
  const maybeWrapped = crdOrResponse as { body?: unknown } | undefined;
  const crd = (maybeWrapped?.body ?? crdOrResponse) as
    | { metadata?: { annotations?: Record<string, unknown> } }
    | undefined;
  const annotations = crd?.metadata?.annotations || {};

  for (const key of annotationKeys) {
    const version = annotations[key];
    if (typeof version === 'string' && version.trim().length > 0) {
      return version.trim();
    }
  }

  return undefined;
}


function getK8sStatusCode(error: unknown): number | undefined {
  const e = error as K8sApiError | undefined;
  return e?.statusCode || e?.response?.statusCode;
}

function getK8sErrorMessage(error: unknown): string {
  const e = error as
    | {
        body?: { message?: string };
        response?: { body?: { message?: string } };
        message?: string;
      }
    | undefined;
  return e?.body?.message || e?.response?.body?.message || e?.message || String(error);
}

/**
 * Minimal shape of an InferenceProviderConfig custom resource as returned by
 * the Kubernetes custom-objects API. Only the fields this service reads are
 * modeled; everything is optional because the API returns untyped objects.
 */
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

class KubernetesService {
  private kc: k8s.KubeConfig;
  private customObjectsApi: k8s.CustomObjectsApi;
  private coreV1Api: k8s.CoreV1Api;
  private apiExtensionsApi: k8s.ApiextensionsV1Api;
  private defaultNamespace: string;

  constructor() {
    this.kc = loadKubeConfig();
    this.customObjectsApi = makeApiClient(this.kc, k8s.CustomObjectsApi);
    this.coreV1Api = makeApiClient(this.kc, k8s.CoreV1Api);
    this.apiExtensionsApi = makeApiClient(this.kc, k8s.ApiextensionsV1Api);
    this.defaultNamespace = process.env.DEFAULT_NAMESPACE || 'airunway-system';
  }

  private createUserKubeConfig(userToken: string): k8s.KubeConfig {
    const userKc = new k8s.KubeConfig();
    const cluster = this.kc.getCurrentCluster();
    const user: k8s.User = { name: 'user', token: userToken };
    userKc.loadFromClusterAndUser(cluster!, user);
    return userKc;
  }

  /**
   * Create a CustomObjectsApi client authenticated with the given user token.
   */
  private getCustomObjectsApi(userToken?: string): k8s.CustomObjectsApi {
    if (!userToken) {
      return this.customObjectsApi;
    }
    return makeApiClient(this.createUserKubeConfig(userToken), k8s.CustomObjectsApi);
  }

  /**
   * Create a CoreV1Api client authenticated with the given user token.
   */
  private getCoreV1Api(userToken?: string): k8s.CoreV1Api {
    if (!userToken) {
      return this.coreV1Api;
    }
    return makeApiClient(this.createUserKubeConfig(userToken), k8s.CoreV1Api);
  }

  /**
   * Create user-scoped API clients for authorization checks (e.g. SSAR).
   */
  private createUserClients(userToken: string) {
    const userKc = this.createUserKubeConfig(userToken);
    return {
      authorizationV1Api: makeApiClient(userKc, k8s.AuthorizationV1Api),
    };
  }

  async checkClusterConnection(): Promise<ClusterStatus> {
    try {
      await withRetry(
        () => this.coreV1Api.listNamespace(),
        { operationName: 'checkClusterConnection', maxRetries: 2 }
      );
      const currentContext = this.kc.getCurrentContext();
      return {
        connected: true,
        namespace: this.defaultNamespace,
        clusterName: currentContext,
      };
    } catch (error) {
      return {
        connected: false,
        namespace: this.defaultNamespace,
        error: error instanceof Error ? error.message : 'Unknown error',
      };
    }
  }

  async listDeployments(namespace?: string, userToken?: string): Promise<DeploymentStatus[]> {
    return listDeploymentsWithAdapter(this.createModelDeploymentStoreAdapter(), namespace, userToken);
  }

  async getDeployment(name: string, namespace: string, userToken?: string): Promise<DeploymentStatus | null> {
    return getDeploymentWithAdapter(this.createModelDeploymentStoreAdapter(), name, namespace, userToken);
  }

  /**
   * Get the raw Custom Resource manifest for a deployment
   * Returns the full CR object as stored in Kubernetes
   */
  async getDeploymentManifest(name: string, namespace: string, userToken?: string): Promise<Record<string, unknown> | null> {
    return getDeploymentManifestWithAdapter(this.createModelDeploymentStoreAdapter(), name, namespace, userToken);
  }

  async createDeployment(config: DeploymentConfig, userToken?: string): Promise<void> {
    return createDeploymentWithAdapter(this.createModelDeploymentStoreAdapter(), config, userToken);
  }

  async deleteDeployment(name: string, namespace: string, userToken?: string): Promise<void> {
    return deleteDeploymentWithAdapter(this.createModelDeploymentStoreAdapter(), name, namespace, userToken);
  }

  private createModelDeploymentStoreAdapter(): ModelDeploymentStoreAdapter {
    return {
      listClusterModelDeployments: async (userToken) => {
        const api = this.getCustomObjectsApi(userToken);
        return withRetry(
          () => api.listClusterCustomObject({
            group: MODEL_DEPLOYMENT_CRD.apiGroup,
            version: MODEL_DEPLOYMENT_CRD.apiVersion,
            plural: MODEL_DEPLOYMENT_CRD.plural,
          }),
          { operationName: 'listDeployments:allNamespaces' }
        );
      },
      listNamespacedModelDeployments: async (namespace, userToken) => {
        const api = this.getCustomObjectsApi(userToken);
        return withRetry(
          () => api.listNamespacedCustomObject({
            group: MODEL_DEPLOYMENT_CRD.apiGroup,
            version: MODEL_DEPLOYMENT_CRD.apiVersion,
            namespace,
            plural: MODEL_DEPLOYMENT_CRD.plural,
          }),
          { operationName: 'listDeployments' }
        );
      },
      getModelDeployment: async (name, namespace, userToken) => {
        const api = this.getCustomObjectsApi(userToken);
        return withRetry(
          () => api.getNamespacedCustomObject({
            group: MODEL_DEPLOYMENT_CRD.apiGroup,
            version: MODEL_DEPLOYMENT_CRD.apiVersion,
            namespace,
            plural: MODEL_DEPLOYMENT_CRD.plural,
            name,
          }),
          { operationName: 'getDeployment' }
        );
      },
      createModelDeployment: async (namespace, manifest, userToken) => {
        const api = this.getCustomObjectsApi(userToken);
        await withRetry(
          () => api.createNamespacedCustomObject({
            group: MODEL_DEPLOYMENT_CRD.apiGroup,
            version: MODEL_DEPLOYMENT_CRD.apiVersion,
            namespace,
            plural: MODEL_DEPLOYMENT_CRD.plural,
            body: manifest,
          }),
          { operationName: 'createDeployment' }
        );
      },
      deleteModelDeployment: async (name, namespace, userToken) => {
        const api = this.getCustomObjectsApi(userToken);
        await withRetry(
          () => api.deleteNamespacedCustomObject({
            group: MODEL_DEPLOYMENT_CRD.apiGroup,
            version: MODEL_DEPLOYMENT_CRD.apiVersion,
            namespace,
            plural: MODEL_DEPLOYMENT_CRD.plural,
            name,
          }),
          { operationName: 'deleteDeployment' }
        );
      },
      listNamespaces: async () => {
        const nsResponse = await withRetry(
          () => this.coreV1Api.listNamespace(),
          { operationName: 'listNamespaces:forRBACFallback', maxRetries: 1 }
        );
        return nsResponse.items
          .map(ns => ns.metadata?.name)
          .filter((name): name is string => !!name);
      },
      canListModelDeployments: async (namespace, userToken) => {
        const { authorizationV1Api } = this.createUserClients(userToken);
        const review: k8s.V1SelfSubjectAccessReview = {
          apiVersion: 'authorization.k8s.io/v1',
          kind: 'SelfSubjectAccessReview',
          spec: {
            resourceAttributes: {
              namespace,
              verb: 'list',
              group: MODEL_DEPLOYMENT_CRD.apiGroup,
              resource: MODEL_DEPLOYMENT_CRD.plural,
            },
          },
        };
        const result = await authorizationV1Api.createSelfSubjectAccessReview({ body: review });
        return result.status?.allowed === true;
      },
      getDeploymentPods: (name, namespace) => this.getDeploymentPods(name, namespace),
      getDeploymentStatus: (name, namespace, userToken) => this.getDeployment(name, namespace, userToken),
      logDebug: (contextOrMessage, message) => {
        if (typeof contextOrMessage === 'string') {
          logger.debug(contextOrMessage);
        } else {
          logger.debug(contextOrMessage, message);
        }
      },
      logInfo: (context, message) => logger.info(context, message),
      logError: (context, message) => logger.error(context, message),
      getK8sStatusCode,
      getK8sErrorMessage,
    };
  }

  async getDeploymentPods(name: string, namespace: string): Promise<PodStatus[]> {
    return getDeploymentPodsWithAdapter(this.createPodDiagnosticsAdapter(), name, namespace);
  }

  async checkCRDInstallation(): Promise<InstallationStatus> {
    try {
      await withRetry(
        () => this.apiExtensionsApi.readCustomResourceDefinition({
          name: `${MODEL_DEPLOYMENT_CRD.plural}.${MODEL_DEPLOYMENT_CRD.apiGroup}`,
        }),
        { operationName: 'checkCRDInstallation', maxRetries: 1 }
      );

      return {
        installed: true,
        crdFound: true,
        message: 'ModelDeployment CRD is installed',
      };
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 404) {
        return {
          installed: false,
          crdFound: false,
          message: 'ModelDeployment CRD not found. Please install AI Runway controller.',
        };
      }
      logger.error({ error }, 'Error checking CRD installation');
      return {
        installed: false,
        crdFound: false,
        message: `Error checking CRD: ${getK8sErrorMessage(error)}`,
      };
    }
  }

  /**
   * Check if a specific CRD exists in the cluster
   */
  async checkCRDExists(crdName: string): Promise<boolean> {
    try {
      await withRetry(
        () => this.apiExtensionsApi.readCustomResourceDefinition({ name: crdName }),
        { operationName: `checkCRDExists:${crdName}`, maxRetries: 1 }
      );
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Read a CRD once and derive both existence and version from the same response.
   */
  private async getCRDStatusFromAnnotations(
    crdName: string,
    annotationKeys: string[]
  ): Promise<{ installed: boolean; version?: string }> {
    try {
      const response = await withRetry(
        () => this.apiExtensionsApi.readCustomResourceDefinition({ name: crdName }),
        { operationName: `getCRDStatusFromAnnotations:${crdName}`, maxRetries: 1 }
      );

      return {
        installed: true,
        version: extractCRDVersionFromAnnotations(response, annotationKeys),
      };
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode !== 404) {
        logger.debug({ error: getK8sErrorMessage(error), crdName }, 'Could not read CRD status');
      }
    }

    return { installed: false };
  }

  /**
   * Get status of all runtimes (providers) in the cluster.
   * Returns installation and health status for each runtime.
   */
  async getRuntimesStatus(): Promise<RuntimeStatus[]> {
    const runtimes: RuntimeStatus[] = [];

    // Check if AI Runway controller is installed by checking for the CRD
    const crdStatus = await this.checkCRDInstallation();

    // List InferenceProviderConfig resources to discover registered providers
    if (crdStatus.installed) {
      try {
        const response = await withRetry(
          () => this.customObjectsApi.listClusterCustomObject({
            group: MODEL_DEPLOYMENT_CRD.apiGroup,
            version: MODEL_DEPLOYMENT_CRD.apiVersion,
            plural: 'inferenceproviderconfigs',
          }),
          { operationName: 'listInferenceProviderConfigs', maxRetries: 1 }
        );

        const items = ((response as { items?: InferenceProviderConfigResource[] })?.items) || [];
        const runtimeEntries = await Promise.all(
          items.map(async (item): Promise<RuntimeStatus> => {
            const name = item.metadata?.name || 'unknown';
            const status = item.status || {};
            const annotations = item.metadata?.annotations;
            const displayName = getProviderDisplayName(name, annotations);
            const annotatedDisplayName = getAnnotatedProviderDisplayName(annotations);
            const requiresCRD = providerRequiresRuntimeCRD(name, aggregateRequiresCRDFromCapabilities(item.spec?.capabilities), annotatedDisplayName);
            const runtimeStatus = await this.checkProviderInstallationStatus(name, status, displayName, requiresCRD);

            // Layer the shim's heartbeat-aware view over the live installation
            // check: prefer the shim's message when it carries an actionable
            // signal (stale heartbeat, or a fresh UpstreamReady=False from the
            // refuse-fast path) so users see the specific reason. Structural
            // fields (installed/operatorRunning) stay sourced from the live
            // check — they reflect what's actually in the cluster.
            const { getProviderHealth } = await import('./providerHealth');
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
          })
        );
        runtimes.push(...runtimeEntries);
      } catch (error) {
        const statusCode = getK8sStatusCode(error);
        if (statusCode !== 404) {
          logger.warn({ error: getK8sErrorMessage(error) }, 'Failed to list InferenceProviderConfigs');
        }
      }
    }

    return runtimes;
  }

  /**
   * Get a specific InferenceProviderConfig by name from the cluster.
   * Returns the full CRD object or null if not found.
   */
  async getInferenceProviderConfig(name: string): Promise<InferenceProviderConfigResource | null> {
    try {
      const response = await withRetry(
        () => this.customObjectsApi.getClusterCustomObject({
          group: MODEL_DEPLOYMENT_CRD.apiGroup,
          version: MODEL_DEPLOYMENT_CRD.apiVersion,
          plural: 'inferenceproviderconfigs',
          name,
        }),
        { operationName: `getInferenceProviderConfig:${name}`, maxRetries: 1 }
      );
      return ((response as { body?: InferenceProviderConfigResource })?.body
        || response) as InferenceProviderConfigResource;
    } catch {
      return null;
    }
  }

  /**
   * Get the default namespace for the active provider
   */
  async getDefaultNamespace(): Promise<string> {
    return configService.getDefaultNamespace();
  }

  /**
   * Check if NVIDIA GPUs are available on cluster nodes
   */
  async checkGPUAvailability(): Promise<GPUAvailability> {
    try {
      const response = await withRetry(
        () => this.coreV1Api.listNode(),
        { operationName: 'checkGPUAvailability' }
      );
      const nodes = response.items;

      let totalGPUs = 0;
      const gpuNodes: string[] = [];

      for (const node of nodes) {
        // Check allocatable resources for nvidia.com/gpu
        const gpuCapacity = node.status?.allocatable?.['nvidia.com/gpu'];
        if (gpuCapacity) {
          const gpuCount = parseInt(gpuCapacity, 10);
          if (gpuCount > 0) {
            totalGPUs += gpuCount;
            gpuNodes.push(node.metadata?.name || 'unknown');
          }
        }
      }

      return {
        available: totalGPUs > 0,
        totalGPUs,
        gpuNodes,
      };
    } catch (error) {
      logger.error({ error }, 'Error checking GPU availability');
      return { available: false, totalGPUs: 0, gpuNodes: [] };
    }
  }

  /**
   * Check if the NVIDIA GPU Operator is installed
   */
  async checkGPUOperatorStatus(): Promise<GPUOperatorStatus> {
    // Check for GPU availability on nodes
    const gpuAvailability = await this.checkGPUAvailability();

    // Check for GPU Operator CRD (ClusterPolicy)
    let crdFound = false;
    try {
      await withRetry(
        () => this.customObjectsApi.listClusterCustomObject({
          group: 'nvidia.com',
          version: 'v1',
          plural: 'clusterpolicies',
        }),
        { operationName: 'checkGPUOperatorCRD', maxRetries: 1 }
      );
      crdFound = true;
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode !== 404) {
        logger.error({ error: getK8sErrorMessage(error) }, 'Error checking GPU Operator CRD');
      }
      crdFound = false;
    }

    // Check for GPU Operator pods in gpu-operator namespace
    let operatorRunning = false;
    try {
      const pods = await withRetry(
        () => this.coreV1Api.listNamespacedPod({
          namespace: 'gpu-operator',
          labelSelector: 'app=gpu-operator',
        }),
        { operationName: 'checkGPUOperatorPods', maxRetries: 1 }
      );
      operatorRunning = pods.items.some(
        (pod) => pod.status?.phase === 'Running'
      );

      // Alternative: check for any running pods in gpu-operator namespace if label didn't match
      if (!operatorRunning) {
        const allPods = await this.coreV1Api.listNamespacedPod({ namespace: 'gpu-operator' });
        operatorRunning = allPods.items.some(
          (pod) => pod.status?.phase === 'Running'
        );
      }
    } catch {
      // Namespace might not exist
      operatorRunning = false;
    }

    const installed = crdFound && operatorRunning;

    let message: string;
    if (gpuAvailability.available) {
      message = `GPUs enabled: ${gpuAvailability.totalGPUs} GPU(s) on ${gpuAvailability.gpuNodes.length} node(s)`;
    } else if (installed) {
      message = 'GPU Operator installed but no GPUs detected on nodes';
    } else if (crdFound) {
      message = 'GPU Operator CRD found but operator is not running';
    } else {
      message = 'GPU Operator not installed';
    }

    return {
      installed,
      crdFound,
      operatorRunning,
      gpusAvailable: gpuAvailability.available,
      totalGPUs: gpuAvailability.totalGPUs,
      gpuNodes: gpuAvailability.gpuNodes,
      message,
    };
  }

  /**
   * Check whether the KAITO workspace operator is installed and running.
   */
  async checkKaitoInstallationStatus(): Promise<InstallationStatus> {
    return this.checkOperatorBackedInstallationStatus('kaito');
  }

  async checkDynamoInstallationStatus(): Promise<InstallationStatus> {
    return this.checkOperatorBackedInstallationStatus('dynamo');
  }

  async checkKubeRayInstallationStatus(): Promise<InstallationStatus> {
    return this.checkOperatorBackedInstallationStatus('kuberay');
  }

  async checkProviderInstallationStatus(
    providerId: string,
    status?: { ready?: boolean },
    providerName?: string,
    requiresCRD = true,
  ): Promise<InstallationStatus> {
    if (requiresCRD) {
      switch (providerId) {
        case 'kaito':
          return this.checkKaitoInstallationStatus();
        case 'dynamo':
          return this.checkDynamoInstallationStatus();
        case 'kuberay':
          return this.checkKubeRayInstallationStatus();
      }
    }

    return checkRuntimeProviderInstallationStatus(
      this.createRuntimeInstallationAdapter(),
      providerId,
      status,
      providerName,
      requiresCRD
    );
  }

  private async checkOperatorBackedInstallationStatus(providerId: RuntimeProviderId): Promise<InstallationStatus> {
    return checkOperatorBackedRuntimeInstallationStatus(
      this.createRuntimeInstallationAdapter(),
      providerId
    );
  }

  private createRuntimeInstallationAdapter(): RuntimeInstallationAdapter {
    return {
      checkCRDExists: (crdName) => this.checkCRDExists(crdName),
      listNamespacedPods: async ({ namespace, labelSelector, operationName }) => {
        const pods = await withRetry(
          () => this.coreV1Api.listNamespacedPod({
            namespace,
            labelSelector,
          }),
          { operationName, maxRetries: 1 }
        );
        return pods.items;
      },
      listPodsForAllNamespaces: async ({ labelSelector, operationName }) => {
        const pods = await withRetry(
          () => this.coreV1Api.listPodForAllNamespaces({
            labelSelector,
          }),
          { operationName, maxRetries: 1 }
        );
        return pods.items;
      },
    };
  }

  /**
   * Get detailed GPU capacity including per-node availability.
   * This accounts for GPUs currently allocated to running pods.
   */
  async getClusterGpuCapacity(): Promise<ClusterGpuCapacity> {
    return getClusterGpuCapacityWithAdapter(this.createClusterGpuCapacityAdapter());
  }

  /**
   * Get detailed GPU capacity including per-node pool breakdown.
   * This groups nodes by node pool labels and includes GPU model information.
   */
  async getDetailedClusterGpuCapacity(): Promise<import('@airunway/shared').DetailedClusterCapacity> {
    return getDetailedClusterGpuCapacityWithAdapter(this.createClusterGpuCapacityAdapter());
  }

  /**
   * Get all node pools in the cluster (both CPU and GPU).
   * Used for cost estimation of CPU-based deployments.
   */
  async getAllNodePools(): Promise<import('@airunway/shared').NodePoolInfo[]> {
    return getAllNodePoolsWithAdapter(this.createClusterGpuCapacityAdapter());
  }

  private createClusterGpuCapacityAdapter(): ClusterGpuCapacityAdapter {
    return {
      listNodes: async (operationName) => {
        const response = await withRetry(
          () => this.coreV1Api.listNode(),
          { operationName }
        );
        return response.items;
      },
      listPodsForAllNamespaces: async (operationName) => {
        const response = await withRetry(
          () => this.coreV1Api.listPodForAllNamespaces(),
          { operationName }
        );
        return response.items;
      },
      getClusterGpuCapacity: () => this.getClusterGpuCapacity(),
      logError: (context, message) => logger.error(context, message),
    };
  }

  /**
   * Get failure reasons for a pending pod by parsing Kubernetes Events
   */
  async getPodFailureReasons(
    podName: string,
    namespace: string,
  ): Promise<import('@airunway/shared').PodFailureReason[]> {
    return getPodFailureReasonsWithAdapter(this.createPodDiagnosticsAdapter(), podName, namespace);
  }

  /**
   * Get list of cluster node names for deployment targeting
   * Returns all nodes that are Ready and schedulable
   */
  async getClusterNodes(): Promise<{ name: string; ready: boolean; gpuCount: number }[]> {
    try {
      const nodesResponse = await withRetry(
        () => this.coreV1Api.listNode(),
        { operationName: 'getClusterNodes' }
      );

      return nodesResponse.items
        .filter(node => {
          // Filter out nodes that are unschedulable (cordoned)
          return !node.spec?.unschedulable;
        })
        .map(node => {
          const nodeName = node.metadata?.name || 'unknown';

          // Check if node is Ready
          const readyCondition = node.status?.conditions?.find(c => c.type === 'Ready');
          const isReady = readyCondition?.status === 'True';

          // Get GPU count if available
          const gpuCapacity = node.status?.allocatable?.['nvidia.com/gpu'];
          const gpuCount = gpuCapacity ? parseInt(gpuCapacity, 10) : 0;

          return {
            name: nodeName,
            ready: isReady,
            gpuCount,
          };
        })
        .sort((a, b) => a.name.localeCompare(b.name));
    } catch (error) {
      logger.error({ error }, 'Failed to get cluster nodes');
      return [];
    }
  }

  /**
   * Get logs from a pod
   */
  async getPodLogs(
    podName: string,
    namespace: string,
    options?: {
      container?: string;
      tailLines?: number;
      timestamps?: boolean;
    },
  ): Promise<string> {
    return getPodLogsWithAdapter(this.createPodDiagnosticsAdapter(), podName, namespace, options);
  }

  private createPodDiagnosticsAdapter(): PodDiagnosticsAdapter {
    return {
      listPodsByLabelSelector: async ({ namespace, labelSelector, operationName }) => {
        const response = await withRetry(
          () => this.coreV1Api.listNamespacedPod({
            namespace,
            labelSelector,
          }),
          { operationName, maxRetries: 1 }
        );
        return response.items;
      },
      listPodsByFieldSelector: async ({ namespace, fieldSelector, limit, operationName }) => {
        const response = await withRetry(
          () => this.coreV1Api.listNamespacedPod({
            namespace,
            fieldSelector,
            limit,
          }),
          { operationName, maxRetries: 1 }
        );
        return response.items;
      },
      listEvents: async ({ namespace, fieldSelector, operationName }) => {
        const response = await withRetry(
          () => this.coreV1Api.listNamespacedEvent({
            namespace,
            fieldSelector,
          }),
          { operationName }
        );
        return response.items;
      },
      readPodLog: ({ name, namespace, container, tailLines, timestamps, operationName }) => (
        withRetry(
          () => this.coreV1Api.readNamespacedPodLog({
            name,
            namespace,
            container,
            tailLines,
            timestamps,
          }),
          { operationName, maxRetries: 2 }
        )
      ),
      logDebug: (context, message) => logger.debug(context, message),
      logError: (context, message) => logger.error(context, message),
      getK8sStatusCode,
      getK8sErrorMessage,
    };
  }

  /**
   * Create a Kubernetes Service for a deployment
   * Used when the provider's controller doesn't create the correct service (e.g., KAITO vLLM)
   */
  async createService(
    name: string,
    namespace: string,
    port: number,
    targetPort: number,
    selector: Record<string, string>
  ): Promise<void> {
    const service: k8s.V1Service = {
      apiVersion: 'v1',
      kind: 'Service',
      metadata: {
        name: `${name}-vllm`,
        namespace,
        labels: {
          'app.kubernetes.io/name': 'airunway',
          'app.kubernetes.io/instance': name,
          'app.kubernetes.io/managed-by': 'airunway',
          'airunway.ai/service-type': 'vllm',
        },
      },
      spec: {
        type: 'ClusterIP',
        ports: [
          {
            port,
            targetPort: targetPort as unknown as k8s.IntOrString,
            protocol: 'TCP',
            name: 'http',
          },
        ],
        selector,
      },
    };

    try {
      await withRetry(
        () => this.coreV1Api.createNamespacedService({ namespace, body: service }),
        { operationName: 'createService' }
      );
      logger.info({ name: `${name}-vllm`, namespace, port, targetPort }, 'Created vLLM service');
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 409) {
        // Service already exists, that's fine
        logger.debug({ name: `${name}-vllm`, namespace }, 'Service already exists');
        return;
      }
      throw error;
    }
  }

  /**
   * Delete a Kubernetes Service
   */
  async deleteService(name: string, namespace: string): Promise<void> {
    try {
      await withRetry(
        () => this.coreV1Api.deleteNamespacedService({ name, namespace }),
        { operationName: 'deleteService' }
      );
      logger.info({ name, namespace }, 'Deleted service');
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 404) {
        // Service doesn't exist, that's fine
        logger.debug({ name, namespace }, 'Service not found (already deleted)');
        return;
      }
      throw error;
    }
  }

  /**
   * Delete a Custom Resource Definition (CRD) from the cluster
   * @param crdName - Full CRD name (e.g., 'workspaces.kaito.sh')
   * @returns true if deleted or not found, false on error
   */
  async deleteCRD(crdName: string): Promise<{ success: boolean; message: string }> {
    try {
      logger.info({ crdName }, 'Deleting CRD');
      await withRetry(
        () => this.apiExtensionsApi.deleteCustomResourceDefinition({ name: crdName }),
        { operationName: 'deleteCRD', maxRetries: 2 }
      );
      logger.info({ crdName }, 'CRD deleted successfully');
      return { success: true, message: `CRD ${crdName} deleted` };
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 404) {
        logger.debug({ crdName }, 'CRD not found (already deleted)');
        return { success: true, message: `CRD ${crdName} not found (already deleted)` };
      }
      logger.error({ error, crdName }, 'Error deleting CRD');
      return { success: false, message: `Failed to delete CRD ${crdName}: ${getK8sErrorMessage(error)}` };
    }
  }

  /**
   * Delete an InferenceProviderConfig instance (cluster-scoped custom resource)
   * @param name - The name of the InferenceProviderConfig to delete
   */
  async deleteInferenceProviderConfig(name: string): Promise<{ success: boolean; message: string }> {
    try {
      logger.info({ name }, 'Deleting InferenceProviderConfig');
      await withRetry(
        () => this.customObjectsApi.deleteClusterCustomObject({
          group: MODEL_DEPLOYMENT_CRD.apiGroup,
          version: MODEL_DEPLOYMENT_CRD.apiVersion,
          plural: 'inferenceproviderconfigs',
          name,
        }),
        { operationName: `deleteInferenceProviderConfig:${name}`, maxRetries: 2 }
      );
      logger.info({ name }, 'InferenceProviderConfig deleted successfully');
      return { success: true, message: `InferenceProviderConfig ${name} deleted` };
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 404) {
        logger.debug({ name }, 'InferenceProviderConfig not found (already deleted)');
        return { success: true, message: `InferenceProviderConfig ${name} not found (already deleted)` };
      }
      logger.error({ error, name }, 'Error deleting InferenceProviderConfig');
      return { success: false, message: `Failed to delete InferenceProviderConfig ${name}: ${getK8sErrorMessage(error)}` };
    }
  }

  /**
   * Delete a namespace from the cluster
   * @param namespace - Namespace name to delete
   * @returns true if deleted or not found, false on error
   */
  async deleteNamespace(namespace: string): Promise<{ success: boolean; message: string }> {
    // Protect critical namespaces
    const protectedNamespaces = ['default', 'kube-system', 'kube-public', 'kube-node-lease'];
    if (protectedNamespaces.includes(namespace)) {
      logger.warn({ namespace }, 'Attempted to delete protected namespace');
      return { success: false, message: `Cannot delete protected namespace: ${namespace}` };
    }

    try {
      logger.info({ namespace }, 'Deleting namespace');
      await withRetry(
        () => this.coreV1Api.deleteNamespace({ name: namespace }),
        { operationName: 'deleteNamespace', maxRetries: 2 }
      );
      logger.info({ namespace }, 'Namespace deletion initiated');
      return { success: true, message: `Namespace ${namespace} deletion initiated` };
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 404) {
        logger.debug({ namespace }, 'Namespace not found (already deleted)');
        return { success: true, message: `Namespace ${namespace} not found (already deleted)` };
      }
      logger.error({ error, namespace }, 'Error deleting namespace');
      return { success: false, message: `Failed to delete namespace ${namespace}: ${getK8sErrorMessage(error)}` };
    }
  }

  /**
   * Get gateway status by checking the required InferencePool, HTTPRoute, and
   * Gateway CRDs, listing Gateway resources, and selecting the Gateway the
   * controller auto-detection would select.
   */
  async getGatewayStatus(): Promise<GatewayInfo> {
    return getGatewayStatusWithAdapter(this.createGatewayStatusAdapter());
  }

  /**
   * List all models accessible through the gateway by checking ModelDeployment status.gateway
   */
  async getGatewayModels(): Promise<GatewayModelInfo[]> {
    return getGatewayModelsWithAdapter(this.createGatewayStatusAdapter());
  }

  /**
   * Check Gateway API and GAIE CRD installation status.
   * Also includes live gateway availability info.
   */
  async checkGatewayCRDStatus(): Promise<GatewayCRDStatus> {
    return checkGatewayCRDStatusWithAdapter(this.createGatewayStatusAdapter());
  }

  private createGatewayStatusAdapter(): GatewayStatusAdapter {
    return {
      checkCRDExists: (crdName) => this.checkCRDExists(crdName),
      listGateways: async () => {
        const response = await withRetry(
          () => this.customObjectsApi.listClusterCustomObject({
            group: 'gateway.networking.k8s.io',
            version: 'v1',
            plural: 'gateways',
          }),
          { operationName: 'listGateways', maxRetries: 1 }
        );
        return (response as { items?: GatewayItem[] }).items || [];
      },
      getDefaultNamespace: () => this.getDefaultNamespace(),
      listModelDeployments: async (namespace) => {
        const response = await withRetry(
          () => this.customObjectsApi.listNamespacedCustomObject({
            group: MODEL_DEPLOYMENT_CRD.apiGroup,
            version: MODEL_DEPLOYMENT_CRD.apiVersion,
            namespace,
            plural: MODEL_DEPLOYMENT_CRD.plural,
          }),
          { operationName: 'listDeploymentsForGateway' }
        );
        return (response as { items?: ModelDeployment[] }).items || [];
      },
      getGatewayStatus: () => this.getGatewayStatus(),
      getCRDStatusFromAnnotations: (crdName, annotationKeys) => (
        this.getCRDStatusFromAnnotations(crdName, annotationKeys)
      ),
      logDebug: (context, message) => logger.debug(context, message),
    };
  }

  /**
   * Proxy a GET request to a Kubernetes service through the API server.
   * This allows fetching service endpoints (e.g. /metrics) even when running off-cluster.
   * Uses raw fetch instead of the generated client to support text/plain responses.
   */
  async proxyServiceGet(
    serviceName: string,
    namespace: string,
    port: number,
    path: string,
    options: ProxyServiceGetOptions = {},
  ): Promise<string> {
    return proxyServiceGetWithAdapter(
      this.createServiceProxyAdapter(),
      serviceName,
      namespace,
      port,
      path,
      options
    );
  }

  /**
   * Proxy a POST request to a Kubernetes service and return the raw response.
   * Used for streaming OpenAI-compatible responses where the route must pipe bytes.
   */
  async proxyServicePostStream(
    serviceName: string,
    namespace: string,
    port: number,
    path: string,
    body: unknown,
    headers: Record<string, string> = {},
    options: ProxyServiceOptions = {}
  ): Promise<Response> {
    return proxyServicePostStreamWithAdapter(
      this.createServiceProxyAdapter(),
      serviceName,
      namespace,
      port,
      path,
      body,
      headers,
      options
    );
  }

  private createServiceProxyAdapter(): ServiceProxyAdapter {
    return {
      getKubeConfig: (userToken) => userToken ? this.createUserKubeConfig(userToken) : this.kc,
      fetch: (input, init) => fetch(input, init),
    };
  }

  /**
   * List PersistentVolumeClaims in a namespace
   */
  async listPVCs(namespace: string, userToken?: string): Promise<PersistentVolumeClaimInfo[]> {
    const api = this.getCoreV1Api(userToken);
    const response = await withRetry(
      () => api.listNamespacedPersistentVolumeClaim({ namespace }),
      { operationName: 'listPVCs', maxRetries: 1 }
    );

    return (response.items || []).flatMap((pvc) => {
      const name = pvc.metadata?.name;
      if (!name) {
        return [];
      }

      return [{
        name,
        status: pvc.status?.phase || 'Unknown',
        storageClass: pvc.spec?.storageClassName || '',
        capacity: pvc.status?.capacity?.['storage'] || pvc.spec?.resources?.requests?.['storage'] || '',
      }];
    });
  }
}

export const kubernetesService = new KubernetesService();
