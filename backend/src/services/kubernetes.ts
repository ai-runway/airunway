import * as k8s from '@kubernetes/client-node';
import type * as https from 'node:https';
import { configService } from './config';
import type { DeploymentStatus, PodStatus, ClusterStatus, PodPhase, DeploymentConfig, RuntimeStatus, ModelDeployment, GatewayInfo, GatewayModelInfo, GatewayCRDStatus, ProviderHealthConfig } from '@airunway/shared';
import { toModelDeploymentManifest, toDeploymentStatus, INFERENCE_GATEWAY_LABEL } from '@airunway/shared';
import { withRetry } from '../lib/retry';
import { loadKubeConfig, makeApiClient, kubeConfigToBunTls, type BunTlsOptions } from '../lib/kubeconfig';
import { type K8sApiError } from '../lib/k8s-errors';
import logger from '../lib/logger';
import {
  extractProviderInfo,
  getProviderDisplayName,
} from '../lib/providers';

// ModelDeployment CRD configuration
const MODEL_DEPLOYMENT_CRD = {
  apiGroup: 'airunway.ai',
  apiVersion: 'v1alpha1',
  plural: 'modeldeployments',
  kind: 'ModelDeployment',
};

const INFERENCE_PROVIDER_CONFIG_CRD = {
  apiGroup: 'airunway.ai',
  apiVersion: 'v1alpha1',
  plural: 'inferenceproviderconfigs',
  kind: 'InferenceProviderConfig',
};

const GATEWAY_API_CRD_NAME = 'gateways.gateway.networking.k8s.io';
const INFERENCE_POOL_CRD_NAME = 'inferencepools.inference.networking.k8s.io';

const GATEWAY_API_VERSION_ANNOTATIONS = [
  'gateway.networking.k8s.io/bundle-version',
  'app.kubernetes.io/version',
];

const INFERENCE_EXTENSION_VERSION_ANNOTATIONS = [
  'inference.networking.k8s.io/bundle-version',
  'app.kubernetes.io/version',
];

const KAITO_WORKSPACE_CRD = 'workspaces.kaito.sh';
const KAITO_NAMESPACE = 'kaito-workspace';
const KAITO_OPERATOR_POD_SELECTOR = 'app.kubernetes.io/name=workspace,app.kubernetes.io/instance=kaito-workspace';
// The AKS AI-toolchain-operator add-on installs KAITO in kube-system. Verified
// against a live `--enable-ai-toolchain-operator` cluster, the add-on operator
// POD carries ONLY the bare `app=ai-toolchain-operator` label — it does NOT
// carry `app.kubernetes.io/name` (that key is present on the Deployment but not
// propagated to the pod template). So this pod probe must match on `app`; using
// `app.kubernetes.io/name=ai-toolchain-operator` here would match nothing.
// NOTE: the Go provider shim probes the Deployment instead and intentionally
// uses `app.kubernetes.io/name=ai-toolchain-operator` — see
// providers/kaito/upstream_health.go (listWorkspaceController). The two paths
// key off different labels on purpose because they inspect different objects.
const KAITO_AKS_ADDON_POD_SELECTOR = 'app=ai-toolchain-operator';
const DYNAMO_CRD = 'dynamographdeployments.nvidia.com';
const DYNAMO_NAMESPACE = 'dynamo-system';
const DYNAMO_OPERATOR_POD_SELECTOR = 'control-plane=controller-manager,app.kubernetes.io/name=dynamo-operator,app.kubernetes.io/instance=dynamo-platform';
const KUBERAY_CRD = 'rayservices.ray.io';
const KUBERAY_NAMESPACE = 'ray-system';
const KUBERAY_OPERATOR_POD_SELECTOR = 'app.kubernetes.io/name=kuberay-operator,app.kubernetes.io/instance=kuberay-operator';

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

/**
 * Per-node GPU information including allocation status
 */
export interface NodeGpuInfo {
  nodeName: string;
  totalGpus: number;      // nvidia.com/gpu allocatable on this node
  allocatedGpus: number;  // Sum of GPU requests from pods on this node
  availableGpus: number;  // totalGpus - allocatedGpus
}

/**
 * Cluster-wide GPU capacity for fit validation
 */
export interface ClusterGpuCapacity {
  totalGpus: number;              // Sum of allocatable GPUs across all nodes
  allocatedGpus: number;          // Sum of GPU requests from all pods
  availableGpus: number;          // totalGpus - allocatedGpus
  maxContiguousAvailable: number; // Highest available GPUs on any single node
  maxNodeGpuCapacity: number;     // Largest GPU count on any single node
  gpuNodeCount: number;           // Total number of nodes with GPUs
  totalMemoryGb?: number;         // Total GPU memory per GPU (e.g., 80 for A100 80GB)
  nodes: NodeGpuInfo[];           // Per-node breakdown
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

/**
 * Installation status for CRDs
 */
export interface InstallationStatus {
  installed: boolean;
  crdFound?: boolean;
  operatorRunning?: boolean;
  requiresCRD?: boolean;
  version?: string;
  message?: string;
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

interface RuntimeInstallationProbe {
  providerName: string;
  crdDisplayName?: string;
  crdName: string;
  operatorNamespace: string;
  operatorPodSelectors: string[];
  fallbackPodSelectors: string[];
  crossNamespaceFallbackPodSelectors?: string[];
}

interface OperatorPodProbeResult {
  ready: boolean;
  namespace?: string;
  selector?: string;
  podName?: string;
  error?: string;
}

interface HealthCrdProbe {
  name: string;
  displayName?: string;
}


function normalizeHealthCrds(health?: ProviderHealthConfig): HealthCrdProbe[] {
  return (health?.crds || []).flatMap((crd): HealthCrdProbe[] => {
    if (typeof crd === 'string') {
      const name = crd.trim();
      return name ? [{ name }] : [];
    }

    const name = crd?.name?.trim();
    if (!name) return [];

    const displayName = typeof crd.displayName === 'string'
      ? crd.displayName.trim() || undefined
      : undefined;

    return [{
      name,
      displayName,
    }];
  });
}

function normalizeHealthOperatorPods(health?: ProviderHealthConfig): NonNullable<ProviderHealthConfig['operatorPods']> {
  if (health?.operatorPods?.length) {
    return health.operatorPods.flatMap((probe) => {
      const selectors = Array.from(new Set((probe.selectors || []).map((selector) => selector.trim()).filter(Boolean)));
      if (selectors.length === 0) return [];
      return [{
        namespace: probe.namespace?.trim() || undefined,
        selectors,
      }];
    });
  }

  const operator = health?.operator;
  if (!operator) return [];

  const namespacedSelectors = Array.from(new Set([
    ...(operator.podSelectors || []),
    ...(operator.fallbackPodSelectors || []),
  ].map((selector) => selector.trim()).filter(Boolean)));
  const crossNamespaceSelectors = Array.from(new Set((operator.crossNamespacePodSelectors || [])
    .map((selector) => selector.trim())
    .filter(Boolean)));

  return [
    ...(namespacedSelectors.length > 0
      ? [{ namespace: operator.namespace?.trim() || undefined, selectors: namespacedSelectors }]
      : []),
    ...(crossNamespaceSelectors.length > 0
      ? [{ selectors: crossNamespaceSelectors }]
      : []),
  ];
}

function getValueAtPath(value: unknown, path?: string): unknown {
  const normalizedPath = (path || 'ready').replace(/^status\./, '');
  return normalizedPath.split('.').reduce<unknown>((current, part) => {
    if (!part) return current;
    if (!current || typeof current !== 'object') return undefined;
    return (current as Record<string, unknown>)[part];
  }, value);
}

function conditionStatusIsTrue(status: unknown): boolean {
  return status === true || status === 'True' || status === 'true';
}

function getProviderStatusReady(status?: Record<string, unknown>, health?: ProviderHealthConfig): boolean {
  const statusHealth = health?.status;
  const readyValue = getValueAtPath(status || {}, statusHealth?.readyPath);
  if (readyValue === true) return true;

  const expectedConditions = statusHealth?.conditions || [];
  if (expectedConditions.length === 0) return false;

  const conditions = Array.isArray(status?.conditions) ? status.conditions : [];
  return expectedConditions.every((expectedType) => conditions.some((condition) => {
    const typedCondition = condition as Record<string, unknown>;
    return typedCondition.type === expectedType && conditionStatusIsTrue(typedCondition.status);
  }));
}

function describeCrd(crd: HealthCrdProbe): string {
  return crd.displayName || crd.name;
}

function describeCrdSet(crds: HealthCrdProbe[]): string {
  if (crds.length === 0) return 'Provider health checks';
  if (crds.length === 1) return describeCrd(crds[0]);
  return 'Required CRDs';
}

function describeOperatorProbeNamespaces(operatorPods: NonNullable<ProviderHealthConfig['operatorPods']>): string {
  const namespaces = Array.from(new Set(operatorPods.map((probe) => probe.namespace).filter(Boolean))) as string[];
  if (namespaces.length === 0) return 'matching configured labels';
  if (namespaces.length === 1) return namespaces[0];
  return namespaces.join(', ');
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

const RUNTIME_INSTALLATION_PROBES: Record<string, RuntimeInstallationProbe> = {
  kaito: {
    providerName: 'KAITO',
    crdDisplayName: 'KAITO workspace CRD',
    crdName: KAITO_WORKSPACE_CRD,
    operatorNamespace: KAITO_NAMESPACE,
    operatorPodSelectors: [KAITO_OPERATOR_POD_SELECTOR],
    fallbackPodSelectors: ['app.kubernetes.io/name=workspace'],
    // The AKS add-on pod only ever lives in kube-system, never in
    // kaito-workspace, so it is matched exclusively in the cross-namespace
    // pass. Listing it explicitly here (rather than relying on the implicit
    // `crossNamespaceFallbackPodSelectors = fallbackPodSelectors` default)
    // keeps add-on detection working even if KAITO later gains other
    // same-namespace fallbacks, and avoids a guaranteed-empty query for
    // `app=ai-toolchain-operator` against kaito-workspace on every probe.
    crossNamespaceFallbackPodSelectors: ['app.kubernetes.io/name=workspace', KAITO_AKS_ADDON_POD_SELECTOR],
  },
  dynamo: {
    providerName: 'Dynamo',
    crdName: DYNAMO_CRD,
    operatorNamespace: DYNAMO_NAMESPACE,
    operatorPodSelectors: [DYNAMO_OPERATOR_POD_SELECTOR],
    fallbackPodSelectors: ['app.kubernetes.io/name=dynamo-operator', 'control-plane=controller-manager'],
    crossNamespaceFallbackPodSelectors: ['app.kubernetes.io/name=dynamo-operator'],
  },
  kuberay: {
    providerName: 'KubeRay',
    crdName: KUBERAY_CRD,
    operatorNamespace: KUBERAY_NAMESPACE,
    operatorPodSelectors: [KUBERAY_OPERATOR_POD_SELECTOR],
    fallbackPodSelectors: ['app.kubernetes.io/name=kuberay-operator'],
  },
};

export function toPodStatus(pod: k8s.V1Pod): PodStatus {
  const initStatuses = pod.status?.initContainerStatuses || [];
  const containerStatuses = pod.status?.containerStatuses || [];
  const allStatuses = [...initStatuses, ...containerStatuses];
  const waitingState = allStatuses.find((status) => status.state?.waiting)?.state?.waiting;
  const terminatedState = allStatuses.find((status) => status.state?.terminated)?.state?.terminated;

  return {
    name: pod.metadata?.name || 'unknown',
    phase: (pod.status?.phase as PodPhase) || 'Unknown',
    ready: containerStatuses.length > 0 && containerStatuses.every((status) => status.ready),
    restarts: allStatuses.reduce((sum, status) => sum + status.restartCount, 0),
    node: pod.spec?.nodeName,
    reason: waitingState?.reason || terminatedState?.reason || pod.status?.reason,
    message: waitingState?.message || terminatedState?.message || pod.status?.message,
  };
}

function isRunningAndReadyPod(pod: k8s.V1Pod): boolean {
  const containerStatuses = pod.status?.containerStatuses || [];
  return pod.status?.phase === 'Running'
    && containerStatuses.length > 0
    && containerStatuses.every((status) => status.ready);
}

type ProxyServiceOptions = {
  signal?: AbortSignal;
  userToken?: string;
};

type ProxyServiceGetOptions = ProxyServiceOptions & {
  accept?: string;
};

type ProxyServiceRequestInit = RequestInit & {
  userToken?: string;
};

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
    logger.debug({ namespace: namespace || 'all' }, 'listDeployments called');

    if (namespace) {
      return this.listDeploymentsInNamespace(namespace, userToken);
    }

    // No namespace specified — try cluster-wide list first
    try {
      const api = this.getCustomObjectsApi(userToken);
      const response = await withRetry(
        () => api.listClusterCustomObject({
          group: MODEL_DEPLOYMENT_CRD.apiGroup,
          version: MODEL_DEPLOYMENT_CRD.apiVersion,
          plural: MODEL_DEPLOYMENT_CRD.plural,
        }),
        { operationName: 'listDeployments:allNamespaces' }
      );

      return this.convertToDeploymentStatuses(response);
    } catch (error) {
      const statusCode = getK8sStatusCode(error);

      // If user lacks cluster-wide list permission, fall back to per-namespace listing
      if (statusCode === 403 && userToken) {
        logger.debug('Cluster-wide list forbidden, falling back to per-namespace listing');
        return this.listDeploymentsAcrossAllowedNamespaces(userToken);
      }

      if (getK8sErrorMessage(error) === 'HTTP request failed' || statusCode === 404) {
        logger.debug('ModelDeployment CRD not found');
        return [];
      }

      logger.error({ error: getK8sErrorMessage(error) }, 'Unexpected error listing deployments');
      return [];
    }
  }

  /**
   * List deployments in a single namespace using the provided credentials.
   */
  private async listDeploymentsInNamespace(namespace: string, userToken?: string): Promise<DeploymentStatus[]> {
    try {
      const api = this.getCustomObjectsApi(userToken);
      const response = await withRetry(
        () => api.listNamespacedCustomObject({
          group: MODEL_DEPLOYMENT_CRD.apiGroup,
          version: MODEL_DEPLOYMENT_CRD.apiVersion,
          namespace,
          plural: MODEL_DEPLOYMENT_CRD.plural,
        }),
        { operationName: 'listDeployments' }
      );

      return this.convertToDeploymentStatuses(response, namespace);
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (getK8sErrorMessage(error) === 'HTTP request failed' || statusCode === 404 || statusCode === 403) {
        logger.debug({ namespace }, 'Cannot list deployments in namespace');
        return [];
      }

      logger.error({ error: getK8sErrorMessage(error) }, 'Unexpected error listing deployments');
      return [];
    }
  }

  /**
   * Convert a K8s API list response to DeploymentStatus array.
   */
  private async convertToDeploymentStatuses(
    response: unknown,
    fallbackNamespace?: string
  ): Promise<DeploymentStatus[]> {
    const items = (response as { items?: ModelDeployment[] }).items || [];
    logger.debug({ count: items.length }, 'Found ModelDeployments');

    const deployments: DeploymentStatus[] = [];
    for (const item of items) {
      const itemNamespace = item.metadata.namespace || fallbackNamespace || 'default';
      const pods = await this.getDeploymentPods(item.metadata.name, itemNamespace);
      deployments.push(toDeploymentStatus(item, pods));
    }

    deployments.sort((a, b) => {
      const dateA = new Date(a.createdAt).getTime();
      const dateB = new Date(b.createdAt).getTime();
      return dateB - dateA;
    });

    return deployments;
  }

  /**
   * Fallback for users without cluster-wide list permission.
   * Discovers which namespaces the user can list ModelDeployments in,
   * then queries each one individually.
   */
  private async listDeploymentsAcrossAllowedNamespaces(userToken: string): Promise<DeploymentStatus[]> {
    // List all namespaces using the service account (users may not have namespace list permission)
    let namespaces: string[];
    try {
      const nsResponse = await withRetry(
        () => this.coreV1Api.listNamespace(),
        { operationName: 'listNamespaces:forRBACFallback', maxRetries: 1 }
      );
      namespaces = nsResponse.items
        .map(ns => ns.metadata?.name)
        .filter((name): name is string => !!name);
    } catch (error) {
      logger.error({ error }, 'Failed to list namespaces for RBAC fallback');
      return [];
    }

    // Check which namespaces the user can list ModelDeployments in
    const userClients = this.createUserClients(userToken);
    const authApi = userClients.authorizationV1Api;

    const allowedNamespaces: string[] = [];
    await Promise.all(
      namespaces.map(async (ns) => {
        try {
          const review: k8s.V1SelfSubjectAccessReview = {
            apiVersion: 'authorization.k8s.io/v1',
            kind: 'SelfSubjectAccessReview',
            spec: {
              resourceAttributes: {
                namespace: ns,
                verb: 'list',
                group: MODEL_DEPLOYMENT_CRD.apiGroup,
                resource: MODEL_DEPLOYMENT_CRD.plural,
              },
            },
          };

          const result = await authApi.createSelfSubjectAccessReview({ body: review });
          if (result.status?.allowed) {
            allowedNamespaces.push(ns);
          }
        } catch (error) {
          logger.debug({ namespace: ns, error }, 'SelfSubjectAccessReview failed for namespace');
        }
      })
    );

    logger.debug({ allowedNamespaces }, 'User has access to namespaces');

    if (allowedNamespaces.length === 0) {
      return [];
    }

    // List deployments in each allowed namespace
    const results = await Promise.all(
      allowedNamespaces.map(ns => this.listDeploymentsInNamespace(ns, userToken))
    );

    const allDeployments = results.flat();
    allDeployments.sort((a, b) => {
      const dateA = new Date(a.createdAt).getTime();
      const dateB = new Date(b.createdAt).getTime();
      return dateB - dateA;
    });

    return allDeployments;
  }

  async getDeployment(name: string, namespace: string, userToken?: string): Promise<DeploymentStatus | null> {
    try {
      const api = this.getCustomObjectsApi(userToken);
      const response = await withRetry(
        () => api.getNamespacedCustomObject({
          group: MODEL_DEPLOYMENT_CRD.apiGroup,
          version: MODEL_DEPLOYMENT_CRD.apiVersion,
          namespace,
          plural: MODEL_DEPLOYMENT_CRD.plural,
          name,
        }),
        { operationName: 'getDeployment' }
      );

      const md = response as ModelDeployment;
      const pods = await this.getDeploymentPods(name, namespace);
      return toDeploymentStatus(md, pods);
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 404) {
        logger.debug({ name, namespace }, 'ModelDeployment not found');
        return null;
      }
      logger.error({ error, name, namespace }, 'Error getting deployment');
      return null;
    }
  }

  /**
   * Get the raw Custom Resource manifest for a deployment
   * Returns the full CR object as stored in Kubernetes
   */
  async getDeploymentManifest(name: string, namespace: string, userToken?: string): Promise<Record<string, unknown> | null> {
    try {
      const api = this.getCustomObjectsApi(userToken);
      const response = await withRetry(
        () => api.getNamespacedCustomObject({
          group: MODEL_DEPLOYMENT_CRD.apiGroup,
          version: MODEL_DEPLOYMENT_CRD.apiVersion,
          namespace,
          plural: MODEL_DEPLOYMENT_CRD.plural,
          name,
        }),
        { operationName: 'getDeploymentManifest' }
      );

      return response as Record<string, unknown>;
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 404) {
        logger.debug({ name, namespace }, 'ModelDeployment manifest not found');
        return null;
      }
      logger.error({ error, name, namespace }, 'Error getting deployment manifest');
      return null;
    }
  }

  async createDeployment(config: DeploymentConfig, userToken?: string): Promise<void> {
    // Generate ModelDeployment manifest from config
    const manifest = toModelDeploymentManifest(config) as unknown as Record<string, unknown>;

    logger.info({ name: config.name, namespace: config.namespace }, 'Creating ModelDeployment');

    const api = this.getCustomObjectsApi(userToken);
    await withRetry(
      () => api.createNamespacedCustomObject({
        group: MODEL_DEPLOYMENT_CRD.apiGroup,
        version: MODEL_DEPLOYMENT_CRD.apiVersion,
        namespace: config.namespace,
        plural: MODEL_DEPLOYMENT_CRD.plural,
        body: manifest,
      }),
      { operationName: 'createDeployment' }
    );

    logger.info({ name: config.name, namespace: config.namespace }, 'ModelDeployment created');
  }

  async deleteDeployment(name: string, namespace: string, userToken?: string): Promise<void> {
    // First, check if deployment exists
    const deployment = await this.getDeployment(name, namespace, userToken);
    if (!deployment) {
      throw new Error(`Deployment '${name}' not found in namespace '${namespace}'`);
    }

    logger.info({ name, namespace }, 'Deleting ModelDeployment');

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

    logger.info({ name, namespace }, 'ModelDeployment deleted');
  }

  async getDeploymentPods(name: string, namespace: string): Promise<PodStatus[]> {
    const coreApi = this.coreV1Api;
    const podsByName = new Map<string, k8s.V1Pod>();
    const addPods = (pods: k8s.V1Pod[]) => {
      for (const pod of pods) {
        const podName = pod.metadata?.name;
        if (podName && !podsByName.has(podName)) {
          podsByName.set(podName, pod);
        }
      }
    };

    // Try multiple exact label selectors since different providers use different labels.
    // Some deployment stacks create related components with different labels, so
    // aggregate across all exact matches instead of stopping at the first selector.
    const exactLabelSelectors = [
      `app.kubernetes.io/instance=${name}`,      // Standard K8s label (Dynamo)
      `airunway.ai/deployment=${name}`,          // AIRunway label
      `airunway.ai/model-deployment=${name}`,    // Pod-template label used by KubeRay
      `nvidia.com/dynamo-graph-deployment-name=${name}`, // Runtime label used by Dynamo/Grove pods
      `kaito.sh/workspace=${name}`,              // KAITO workspace label
    ];

    const listPodsByLabelSelector = async (labelSelector: string, operationName = 'getDeploymentPods'): Promise<k8s.V1Pod[]> => {
      try {
        const response = await withRetry(
          () => coreApi.listNamespacedPod({
            namespace,
            labelSelector,
          }),
          { operationName, maxRetries: 1 }
        );

        if (response.items.length > 0) {
          logger.debug({ name, namespace, labelSelector, podCount: response.items.length }, 'Found pods with selector');
        }
        return response.items;
      } catch (error) {
        logger.debug({ error, name, namespace, labelSelector }, 'Error trying label selector');
        return [];
      }
    };

    const exactSelectorResults = await Promise.all(
      exactLabelSelectors.map(labelSelector => listPodsByLabelSelector(labelSelector))
    );
    exactSelectorResults.forEach(addPods);

    // KubeRay creates pods with ray.io/cluster label set to a generated RayCluster name.
    // Modern Airunway KubeRay pods carry airunway.ai/model-deployment (handled above),
    // but keep this as a backwards-compatible fallback. Only accept an exact name or
    // the RayService-generated "<deployment>-raycluster..." form so deployments like
    // "demo" do not match unrelated clusters like "demo2" or "demo-extra".
    try {
      const response = await withRetry(
        () => coreApi.listNamespacedPod({
          namespace,
          labelSelector: 'ray.io/cluster', // Just filter to Ray pods, then filter by name prefix
        }),
        { operationName: 'getDeploymentPods:kuberay', maxRetries: 1 }
      );

      const matchingPods = response.items.filter(pod => {
        const clusterLabel = pod.metadata?.labels?.['ray.io/cluster'] || '';
        return clusterLabel === name || clusterLabel.startsWith(`${name}-raycluster`);
      });

      if (matchingPods.length > 0) {
        logger.debug({ name, namespace, podCount: matchingPods.length }, 'Found KubeRay pods by cluster label prefix');
        addPods(matchingPods);
      }
    } catch (error) {
      logger.debug({ error, name, namespace }, 'Error trying KubeRay cluster label selector');
    }

    if (podsByName.size === 0) {
      // Last-resort fallback for older or third-party manifests that only set app=<name>.
      // Avoid aggregating this broad label with canonical matches because unrelated pods
      // can legitimately share the same app label in a namespace.
      try {
        const labelSelector = `app=${name}`;
        const pods = await listPodsByLabelSelector(labelSelector, 'getDeploymentPods:fallbackApp');
        addPods(pods);
      } catch (error) {
        logger.debug({ error, name, namespace }, 'Error trying fallback app label selector');
      }
    }

    const pods = Array.from(podsByName.values())
      .sort((a, b) => (a.metadata?.name || '').localeCompare(b.metadata?.name || ''));
    if (pods.length === 0) {
      logger.debug({ name, namespace }, 'No pods found with any label selector');
      return [];
    }

    logger.debug({ name, namespace, podCount: pods.length }, 'Found deployment pods');
    return pods.map((pod) => toPodStatus(pod));
  }

  /**
   * Check if the ModelDeployment CRD is installed in the cluster
   */
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

  async listInferenceProviderConfigs(): Promise<InferenceProviderConfigResource[]> {
    try {
      const response = await withRetry(
        () => this.customObjectsApi.listClusterCustomObject({
          group: INFERENCE_PROVIDER_CONFIG_CRD.apiGroup,
          version: INFERENCE_PROVIDER_CONFIG_CRD.apiVersion,
          plural: INFERENCE_PROVIDER_CONFIG_CRD.plural,
        }),
        { operationName: 'listInferenceProviderConfigs', maxRetries: 1 },
      );

      return (((response as { body?: { items?: InferenceProviderConfigResource[] }; items?: InferenceProviderConfigResource[] }).body || response)?.items) || [];
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 404) {
        return [];
      }

      logger.warn(
        { error: getK8sErrorMessage(error) },
        'Failed to list InferenceProviderConfigs',
      );
      return [];
    }
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
      const items = await this.listInferenceProviderConfigs();

      const runtimeEntries = await Promise.all(
        items.map(async (item): Promise<RuntimeStatus> => {
          const providerInfo = extractProviderInfo(item);
          const status = item.status || {};
          const requiresCRD = providerInfo.requiresCRD ?? true;
          const runtimeStatus = await this.checkProviderInstallationStatus(
            providerInfo.id,
            status,
            providerInfo.name,
            providerInfo.health,
            requiresCRD,
          );

          // Layer the shim's heartbeat-aware view over the live installation
          // check: prefer the shim's message when it carries an actionable
          // signal (stale heartbeat, or a fresh UpstreamReady=False from the
          // refuse-fast path) so users see the specific reason. Structural
          // fields (installed/operatorRunning) stay sourced from the live
          // check — they reflect what's actually in the cluster.
          const { getProviderHealth } = await import('./providerHealth');
          const health = getProviderHealth(providerInfo.id, item);
          const useShimMessage = health.stale || (!health.healthy && health.hasShimSignal);
          const message = useShimMessage ? health.message : runtimeStatus.message;

          return {
            id: providerInfo.id,
            name: providerInfo.name,
            description: providerInfo.description,
            defaultNamespace: providerInfo.defaultNamespace,
            documentationUrl: providerInfo.documentationUrl,
            icon: providerInfo.icon,
            warnings: providerInfo.warnings,
            installable: providerInfo.installable,
            capabilities: providerInfo.capabilities,
            deploymentDefaults: providerInfo.deploymentDefaults,
            health: providerInfo.health,
            installed: runtimeStatus.installed,
            healthy: runtimeStatus.operatorRunning ?? false,
            crdFound: runtimeStatus.crdFound ?? runtimeStatus.installed,
            operatorRunning: runtimeStatus.operatorRunning ?? false,
            requiresCRD: runtimeStatus.requiresCRD ?? requiresCRD,
            version: status.version,
            message,
          };
        }),
      );

      runtimes.push(...runtimeEntries);
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
          group: INFERENCE_PROVIDER_CONFIG_CRD.apiGroup,
          version: INFERENCE_PROVIDER_CONFIG_CRD.apiVersion,
          plural: INFERENCE_PROVIDER_CONFIG_CRD.plural,
          name,
        }),
        { operationName: `getInferenceProviderConfig:${name}`, maxRetries: 1 }
      );
      return ((response as { body?: InferenceProviderConfigResource })?.body
        || response) as InferenceProviderConfigResource;
    } catch (error) {
      if (getK8sStatusCode(error) === 404) {
        return null;
      }
      throw error;
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
    status?: Record<string, unknown>,
    providerName?: string,
    health?: ProviderHealthConfig,
    requiresCRD = true,
  ): Promise<InstallationStatus> {
    const displayName = providerName || getProviderDisplayName(providerId);
    const statusReady = getProviderStatusReady(status, health);

    if (!requiresCRD) {
      return {
        installed: statusReady,
        crdFound: true,
        operatorRunning: statusReady,
        requiresCRD: false,
        message: statusReady
          ? 'Runtime is ready to use.'
          : 'Provider is registered but not ready yet.',
      };
    }

    const crds = normalizeHealthCrds(health);
    const operatorPods = normalizeHealthOperatorPods(health);

    if (crds.length > 0 || operatorPods.length > 0) {
      return this.checkAnnotationDrivenProviderInstallationStatus(
        displayName,
        statusReady,
        crds,
        operatorPods,
      );
    }

    switch (providerId) {
      case 'kaito':
        return this.checkKaitoInstallationStatus();
      case 'dynamo':
        return this.checkDynamoInstallationStatus();
      case 'kuberay':
        return this.checkKubeRayInstallationStatus();
      default:
        break;
    }

    if (statusReady) {
      return {
        installed: true,
        crdFound: true,
        operatorRunning: true,
        requiresCRD: true,
        message: `${displayName} is installed and running`,
      };
    }

    return {
      installed: false,
      crdFound: false,
      operatorRunning: false,
      requiresCRD: true,
      message: `${displayName} is registered but not ready`,
    };
  }

  private async checkAnnotationDrivenProviderInstallationStatus(
    providerName: string,
    statusReady: boolean,
    crds: HealthCrdProbe[],
    operatorPods: NonNullable<ProviderHealthConfig['operatorPods']>,
  ): Promise<InstallationStatus> {
    const [crdResults, operatorProbe] = await Promise.all([
      Promise.all(crds.map(async (crd) => ({
        ...crd,
        found: await this.checkCRDExists(crd.name),
      }))),
      operatorPods.length > 0
        ? this.findReadyConfiguredOperatorPod(operatorPods)
        : Promise.resolve<OperatorPodProbeResult>({ ready: false }),
    ]);

    const crdFound = crdResults.length > 0
      ? crdResults.every((crd) => crd.found)
      : statusReady;
    const operatorRunning = operatorPods.length > 0
      ? operatorProbe.ready
      : (crdResults.length > 0 ? crdFound && statusReady : statusReady);
    const installed = crdResults.length > 0 && operatorPods.length > 0
      ? crdFound && operatorRunning
      : crdResults.length > 0
        ? crdFound && statusReady
        : operatorRunning;

    const crdLabel = describeCrdSet(crds);
    let message: string;

    if (crdResults.length > 0 && !crdFound) {
      const missingCrds = crdResults.filter((crd) => !crd.found).map(describeCrd);
      message = missingCrds.length === 1
        ? `${missingCrds[0]} not found`
        : `Required CRDs not found: ${missingCrds.join(', ')}`;
    } else if (operatorPods.length > 0 && operatorRunning) {
      const location = operatorProbe.namespace
        ? ` in ${operatorProbe.namespace}`
        : '';
      message = crdResults.length > 0
        ? `${crdLabel} found and ${providerName} operator pods are ready${location}`
        : `${providerName} operator pods are ready${location}`;
    } else if (operatorPods.length > 0 && operatorProbe.error) {
      message = crdResults.length > 0
        ? `${crdLabel} found but ${providerName} operator pods could not be checked: ${operatorProbe.error}`
        : `${providerName} operator pods could not be checked: ${operatorProbe.error}`;
    } else if (operatorPods.length > 0) {
      const namespaces = describeOperatorProbeNamespaces(operatorPods);
      message = crdResults.length > 0
        ? `${crdLabel} found but no ready ${providerName} operator pods were detected in ${namespaces}`
        : `No ready ${providerName} operator pods were detected in ${namespaces}`;
    } else if (statusReady) {
      message = `${crdLabel} found and ${providerName} is ready`;
    } else if (crdResults.length > 0) {
      message = `${crdLabel} found but ${providerName} is not ready`;
    } else {
      message = `${crdLabel} found`;
    }

    return {
      installed,
      crdFound,
      operatorRunning,
      requiresCRD: true,
      message,
    };
  }

  private async checkOperatorBackedInstallationStatus(providerId: keyof typeof RUNTIME_INSTALLATION_PROBES): Promise<InstallationStatus> {
    const probe = RUNTIME_INSTALLATION_PROBES[providerId];
    const crdDisplayName = probe.crdDisplayName || `${probe.providerName} CRD`;
    const [crdFound, operatorProbe] = await Promise.all([
      this.checkCRDExists(probe.crdName),
      this.findReadyOperatorPod(
        probe.operatorNamespace,
        probe.operatorPodSelectors,
        probe.fallbackPodSelectors,
        `check${probe.providerName.replace(/[^a-zA-Z0-9]/g, '')}OperatorPods`,
        probe.crossNamespaceFallbackPodSelectors
      ),
    ]);
    const operatorRunning = operatorProbe.ready;
    const installed = crdFound && operatorRunning;

    let message: string;
    if (crdFound && operatorRunning) {
      const location = operatorProbe.namespace && operatorProbe.namespace !== probe.operatorNamespace
        ? ` in ${operatorProbe.namespace}`
        : '';
      message = `${crdDisplayName} found and ${probe.providerName} operator pods are ready${location}`;
    } else if (crdFound && operatorProbe.error) {
      message = `${crdDisplayName} found but ${probe.providerName} operator pods could not be checked: ${operatorProbe.error}`;
    } else if (crdFound) {
      message = `${crdDisplayName} found but no ready ${probe.providerName} operator pods were detected in ${probe.operatorNamespace} or matching known provider labels`;
    } else {
      message = `${crdDisplayName} not found`;
    }

    return {
      installed,
      crdFound,
      operatorRunning,
      requiresCRD: true,
      message,
    };
  }

  private async findReadyConfiguredOperatorPod(
    operatorPods: NonNullable<ProviderHealthConfig['operatorPods']>,
  ): Promise<OperatorPodProbeResult> {
    let firstError: string | undefined;

    for (const probe of operatorPods) {
      const selectors = Array.from(new Set(probe.selectors));

      for (const selector of selectors) {
        try {
          const pods = probe.namespace
            ? await withRetry(
                () => this.coreV1Api.listNamespacedPod({
                  namespace: probe.namespace!,
                  labelSelector: selector,
                }),
                { operationName: `checkProviderOperatorPods:${probe.namespace}`, maxRetries: 1 },
              )
            : await withRetry(
                () => this.coreV1Api.listPodForAllNamespaces({
                  labelSelector: selector,
                }),
                { operationName: 'checkProviderOperatorPods:all-namespaces', maxRetries: 1 },
              );

          const readyPod = pods.items.find((pod) => isRunningAndReadyPod(pod));
          if (readyPod) {
            return {
              ready: true,
              namespace: readyPod.metadata?.namespace || probe.namespace,
              selector,
              podName: readyPod.metadata?.name,
            };
          }
        } catch (error) {
          const statusCode = getK8sStatusCode(error);
          if (statusCode !== 404 && !firstError) {
            firstError = getK8sErrorMessage(error);
            logger.warn(
              { error: firstError, namespace: probe.namespace, selector },
              'Unable to check provider operator pods from provider health metadata',
            );
          }
        }
      }
    }

    return { ready: false, error: firstError };
  }

  private async findReadyOperatorPod(
    namespace: string,
    operatorPodSelectors: string[],
    fallbackPodSelectors: string[],
    operationName: string,
    crossNamespaceFallbackPodSelectors: string[] = fallbackPodSelectors,
  ): Promise<OperatorPodProbeResult> {
    const selectors = Array.from(new Set([...operatorPodSelectors, ...fallbackPodSelectors]));
    const crossNamespaceSelectors = Array.from(new Set(crossNamespaceFallbackPodSelectors));
    let firstError: string | undefined;

    for (const selector of selectors) {
      try {
        const pods = await withRetry(
          () => this.coreV1Api.listNamespacedPod({
            namespace,
            labelSelector: selector,
          }),
          { operationName: `${operationName}:${namespace}`, maxRetries: 1 }
        );
        const readyPod = pods.items.find((pod) => isRunningAndReadyPod(pod));
        if (readyPod) {
          return {
            ready: true,
            namespace,
            selector,
            podName: readyPod.metadata?.name,
          };
        }
      } catch (error) {
        const statusCode = getK8sStatusCode(error);
        if (statusCode !== 404 && !firstError) {
          firstError = getK8sErrorMessage(error);
          logger.warn({ error: firstError, namespace, selector }, 'Unable to check provider operator pods in expected namespace');
        }
      }
    }

    for (const selector of crossNamespaceSelectors) {
      try {
        const pods = await withRetry(
          () => this.coreV1Api.listPodForAllNamespaces({
            labelSelector: selector,
          }),
          { operationName: `${operationName}:all-namespaces`, maxRetries: 1 }
        );
        const readyPod = pods.items.find((pod) => isRunningAndReadyPod(pod));
        if (readyPod) {
          return {
            ready: true,
            namespace: readyPod.metadata?.namespace,
            selector,
            podName: readyPod.metadata?.name,
          };
        }
      } catch (error) {
        const statusCode = getK8sStatusCode(error);
        if (statusCode !== 404 && !firstError) {
          firstError = getK8sErrorMessage(error);
          logger.warn({ error: firstError, selector }, 'Unable to check provider operator pods across namespaces');
        }
      }
    }

    return { ready: false, error: firstError };
  }

  /**
   * Get detailed GPU capacity including per-node availability.
   * This accounts for GPUs currently allocated to running pods.
   */
  async getClusterGpuCapacity(): Promise<ClusterGpuCapacity> {
    try {
      // Step 1: Get all nodes and their GPU capacity
      const nodesResponse = await withRetry(
        () => this.coreV1Api.listNode(),
        { operationName: 'getClusterGpuCapacity:listNodes' }
      );

      const nodeGpuMap = new Map<string, { total: number; allocated: number }>();
      let detectedGpuMemoryGb: number | undefined;

      for (const node of nodesResponse.items) {
        const nodeName = node.metadata?.name || 'unknown';
        const gpuCapacity = node.status?.allocatable?.['nvidia.com/gpu'];
        if (gpuCapacity) {
          const gpuCount = parseInt(gpuCapacity, 10);
          if (gpuCount > 0) {
            nodeGpuMap.set(nodeName, { total: gpuCount, allocated: 0 });

            // Try to detect GPU memory from node labels (prefer nvidia.com/gpu.memory)
            if (!detectedGpuMemoryGb) {
              // Primary: Use nvidia.com/gpu.memory label (value in MiB from GPU Feature Discovery)
              const gpuMemoryMib = node.metadata?.labels?.['nvidia.com/gpu.memory'];
              if (gpuMemoryMib) {
                const memoryMib = parseInt(gpuMemoryMib, 10);
                if (!isNaN(memoryMib) && memoryMib > 0) {
                  detectedGpuMemoryGb = Math.round(memoryMib / 1024); // Convert MiB to GB
                }
              }

              // Fallback: Detect from nvidia.com/gpu.product label
              if (!detectedGpuMemoryGb) {
                const gpuProduct = node.metadata?.labels?.['nvidia.com/gpu.product'];
                if (gpuProduct) {
                  detectedGpuMemoryGb = this.detectGpuMemoryFromProduct(gpuProduct);
                }
              }
            }
          }
        }
      }

      // Step 2: Get all pods across all namespaces and sum their GPU requests per node
      const podsResponse = await withRetry(
        () => this.coreV1Api.listPodForAllNamespaces(),
        { operationName: 'getClusterGpuCapacity:listPods' }
      );

      for (const pod of podsResponse.items) {
        // Only count running or pending pods (not completed/failed)
        const phase = pod.status?.phase;
        if (phase !== 'Running' && phase !== 'Pending') {
          continue;
        }

        const nodeName = pod.spec?.nodeName;
        if (!nodeName || !nodeGpuMap.has(nodeName)) {
          continue;
        }

        // Sum GPU requests from all containers in the pod
        let podGpuRequests = 0;
        for (const container of pod.spec?.containers || []) {
          const gpuRequest = container.resources?.requests?.['nvidia.com/gpu'];
          if (gpuRequest) {
            podGpuRequests += parseInt(gpuRequest, 10);
          }
          // Also check limits if requests not specified (limits can imply requests)
          if (!gpuRequest) {
            const gpuLimit = container.resources?.limits?.['nvidia.com/gpu'];
            if (gpuLimit) {
              podGpuRequests += parseInt(gpuLimit, 10);
            }
          }
        }

        if (podGpuRequests > 0) {
          const nodeInfo = nodeGpuMap.get(nodeName)!;
          nodeInfo.allocated += podGpuRequests;
        }
      }

      // Step 3: Build result
      const nodes: NodeGpuInfo[] = [];
      let totalGpus = 0;
      let allocatedGpus = 0;
      let maxContiguousAvailable = 0;
      let maxNodeGpuCapacity = 0;

      for (const [nodeName, info] of nodeGpuMap) {
        const availableOnNode = Math.max(0, info.total - info.allocated);
        nodes.push({
          nodeName,
          totalGpus: info.total,
          allocatedGpus: info.allocated,
          availableGpus: availableOnNode,
        });
        totalGpus += info.total;
        allocatedGpus += info.allocated;
        maxContiguousAvailable = Math.max(maxContiguousAvailable, availableOnNode);
        maxNodeGpuCapacity = Math.max(maxNodeGpuCapacity, info.total);
      }

      return {
        totalGpus,
        allocatedGpus,
        availableGpus: totalGpus - allocatedGpus,
        maxContiguousAvailable,
        maxNodeGpuCapacity,
        gpuNodeCount: nodeGpuMap.size,
        totalMemoryGb: detectedGpuMemoryGb,
        nodes,
      };
    } catch (error) {
      logger.error({ error }, 'Error getting cluster GPU capacity');
      return {
        totalGpus: 0,
        allocatedGpus: 0,
        availableGpus: 0,
        maxContiguousAvailable: 0,
        maxNodeGpuCapacity: 0,
        gpuNodeCount: 0,
        nodes: [],
      };
    }
  }

  /**
   * Get detailed GPU capacity including per-node pool breakdown.
   * This groups nodes by node pool labels and includes GPU model information.
   */
  async getDetailedClusterGpuCapacity(): Promise<import('@airunway/shared').DetailedClusterCapacity> {
    try {
      // Get basic capacity first
      const basicCapacity = await this.getClusterGpuCapacity();

      // Step 1: Get all nodes and group by node pool
      const nodesResponse = await withRetry(
        () => this.coreV1Api.listNode(),
        { operationName: 'getDetailedClusterGpuCapacity:listNodes' }
      );

      const nodePoolMap = new Map<string, {
        gpuCount: number;
        nodeCount: number;
        availableGpus: number;
        gpuModel?: string;
        instanceType?: string;
        region?: string;
      }>();

      for (const node of nodesResponse.items) {
        const nodeName = node.metadata?.name || 'unknown';
        const gpuCapacity = node.status?.allocatable?.['nvidia.com/gpu'];

        if (gpuCapacity) {
          const gpuCount = parseInt(gpuCapacity, 10);
          if (gpuCount > 0) {
            // Determine node pool name from labels
            // AKS uses: agentpool, kubernetes.azure.com/agentpool
            // GKE uses: cloud.google.com/gke-nodepool
            // EKS uses: eks.amazonaws.com/nodegroup
            const nodePoolName =
              node.metadata?.labels?.['agentpool'] ||
              node.metadata?.labels?.['kubernetes.azure.com/agentpool'] ||
              node.metadata?.labels?.['cloud.google.com/gke-nodepool'] ||
              node.metadata?.labels?.['eks.amazonaws.com/nodegroup'] ||
              'default';

            // Get GPU model from labels - try multiple sources
            const gpuModel =
              node.metadata?.labels?.['nvidia.com/gpu.product'] ||
              this.extractGpuModelFromInstanceType(node.metadata?.labels) ||
              node.metadata?.labels?.['accelerator'];

            // Get instance type from standard Kubernetes labels
            const instanceType =
              node.metadata?.labels?.['node.kubernetes.io/instance-type'] ||
              node.metadata?.labels?.['beta.kubernetes.io/instance-type'];

            // Get region from labels
            const region =
              node.metadata?.labels?.['topology.kubernetes.io/region'] ||
              node.metadata?.labels?.['failure-domain.beta.kubernetes.io/region'];

            // Find available GPUs for this node
            const nodeInfo = basicCapacity.nodes.find(n => n.nodeName === nodeName);
            const nodeAvailableGpus = nodeInfo?.availableGpus || 0;

            if (!nodePoolMap.has(nodePoolName)) {
              nodePoolMap.set(nodePoolName, {
                gpuCount: 0,
                nodeCount: 0,
                availableGpus: 0,
                gpuModel,
                instanceType,
                region,
              });
            }

            const poolInfo = nodePoolMap.get(nodePoolName)!;
            poolInfo.gpuCount += gpuCount;
            poolInfo.nodeCount += 1;
            poolInfo.availableGpus += nodeAvailableGpus;

            // Update GPU model if not set or if we find a more specific one
            if (!poolInfo.gpuModel && gpuModel) {
              poolInfo.gpuModel = gpuModel;
            }
            // Update instance type if not set
            if (!poolInfo.instanceType && instanceType) {
              poolInfo.instanceType = instanceType;
            }
            // Update region if not set
            if (!poolInfo.region && region) {
              poolInfo.region = region;
            }
          }
        }
      }

      // Convert to array
      const nodePools: import('@airunway/shared').NodePoolInfo[] = [];
      for (const [name, info] of nodePoolMap) {
        nodePools.push({
          name,
          gpuCount: info.gpuCount,
          nodeCount: info.nodeCount,
          availableGpus: info.availableGpus,
          gpuModel: info.gpuModel,
          instanceType: info.instanceType,
          region: info.region,
        });
      }

      return {
        totalGpus: basicCapacity.totalGpus,
        allocatedGpus: basicCapacity.allocatedGpus,
        availableGpus: basicCapacity.availableGpus,
        maxContiguousAvailable: basicCapacity.maxContiguousAvailable,
        maxNodeGpuCapacity: basicCapacity.maxNodeGpuCapacity,
        gpuNodeCount: basicCapacity.gpuNodeCount,
        totalMemoryGb: basicCapacity.totalMemoryGb,
        nodePools,
      };
    } catch (error) {
      logger.error({ error }, 'Error getting detailed cluster GPU capacity');
      return {
        totalGpus: 0,
        allocatedGpus: 0,
        availableGpus: 0,
        maxContiguousAvailable: 0,
        maxNodeGpuCapacity: 0,
        gpuNodeCount: 0,
        nodePools: [],
      };
    }
  }

  /**
   * Get all node pools in the cluster (both CPU and GPU).
   * Used for cost estimation of CPU-based deployments.
   */
  async getAllNodePools(): Promise<import('@airunway/shared').NodePoolInfo[]> {
    try {
      const nodesResponse = await withRetry(
        () => this.coreV1Api.listNode(),
        { operationName: 'getAllNodePools:listNodes' }
      );

      const nodePoolMap = new Map<string, {
        nodeCount: number;
        gpuCount: number;
        availableGpus: number;
        gpuModel?: string;
        instanceType?: string;
        region?: string;
      }>();

      for (const node of nodesResponse.items) {
        // Determine node pool name from labels
        const nodePoolName =
          node.metadata?.labels?.['agentpool'] ||
          node.metadata?.labels?.['kubernetes.azure.com/agentpool'] ||
          node.metadata?.labels?.['cloud.google.com/gke-nodepool'] ||
          node.metadata?.labels?.['eks.amazonaws.com/nodegroup'] ||
          'default';

        // Get instance type from standard Kubernetes labels
        const instanceType =
          node.metadata?.labels?.['node.kubernetes.io/instance-type'] ||
          node.metadata?.labels?.['beta.kubernetes.io/instance-type'];

        // Get region from labels
        const region =
          node.metadata?.labels?.['topology.kubernetes.io/region'] ||
          node.metadata?.labels?.['failure-domain.beta.kubernetes.io/region'];

        // Check for GPU capacity
        const gpuCapacity = node.status?.allocatable?.['nvidia.com/gpu'];
        const gpuCount = gpuCapacity ? parseInt(gpuCapacity, 10) : 0;

        // Get GPU model from labels if this node has GPUs
        const gpuModel = gpuCount > 0 ? (
          node.metadata?.labels?.['nvidia.com/gpu.product'] ||
          this.extractGpuModelFromInstanceType(node.metadata?.labels)
        ) : undefined;

        if (!nodePoolMap.has(nodePoolName)) {
          nodePoolMap.set(nodePoolName, {
            nodeCount: 0,
            gpuCount: 0,
            availableGpus: 0,
            gpuModel,
            instanceType,
            region,
          });
        }

        const poolInfo = nodePoolMap.get(nodePoolName)!;
        poolInfo.nodeCount += 1;
        poolInfo.gpuCount += gpuCount;

        // Update instance type if not set
        if (!poolInfo.instanceType && instanceType) {
          poolInfo.instanceType = instanceType;
        }
        // Update region if not set
        if (!poolInfo.region && region) {
          poolInfo.region = region;
        }
        // Update GPU model if not set
        if (!poolInfo.gpuModel && gpuModel) {
          poolInfo.gpuModel = gpuModel;
        }
      }

      // Convert to array
      const nodePools: import('@airunway/shared').NodePoolInfo[] = [];
      for (const [name, info] of nodePoolMap) {
        nodePools.push({
          name,
          gpuCount: info.gpuCount,
          nodeCount: info.nodeCount,
          availableGpus: info.availableGpus,
          gpuModel: info.gpuModel,
          instanceType: info.instanceType,
          region: info.region,
        });
      }

      return nodePools;
    } catch (error) {
      logger.error({ error }, 'Error getting all node pools');
      return [];
    }
  }

  /**
   * Get failure reasons for a pending pod by parsing Kubernetes Events
   */
  async getPodFailureReasons(
    podName: string,
    namespace: string,
  ): Promise<import('@airunway/shared').PodFailureReason[]> {
    try {
      const coreApi = this.coreV1Api;
      const eventsResponse = await withRetry(
        () => coreApi.listNamespacedEvent({
          namespace,
          fieldSelector: `involvedObject.name=${podName}`,
        }),
        { operationName: 'getPodFailureReasons' }
      );

      const reasons: import('@airunway/shared').PodFailureReason[] = [];

      for (const event of eventsResponse.items) {
        // Focus on Warning events related to scheduling failures
        if (event.type !== 'Warning') {
          continue;
        }

        const reason = event.reason || 'Unknown';
        const message = event.message || '';

        // Analyze the event to determine if it's a resource constraint
        const isResourceConstraint = reason === 'FailedScheduling' ||
          message.toLowerCase().includes('insufficient');

        let resourceType: 'gpu' | 'cpu' | 'memory' | undefined;
        let canAutoscalerHelp = false;

        if (isResourceConstraint) {
          // Detect resource type from message
          if (message.includes('nvidia.com/gpu')) {
            resourceType = 'gpu';
            canAutoscalerHelp = true; // Autoscaler can add GPU nodes
          } else if (message.toLowerCase().includes('cpu')) {
            resourceType = 'cpu';
            canAutoscalerHelp = true;
          } else if (message.toLowerCase().includes('memory')) {
            resourceType = 'memory';
            canAutoscalerHelp = true;
          }

          // Check for taint-related failures (autoscaler can't help with these)
          if (message.toLowerCase().includes('taint') ||
            message.toLowerCase().includes('toleration')) {
            canAutoscalerHelp = false;
          }

          // Check for node selector failures (autoscaler can't help with these)
          if (message.toLowerCase().includes('node selector') ||
            message.toLowerCase().includes('didn\'t match')) {
            canAutoscalerHelp = false;
          }
        }

        reasons.push({
          reason,
          message,
          isResourceConstraint,
          resourceType,
          canAutoscalerHelp,
        });
      }

      return reasons;
    } catch (error) {
      logger.error({ error, podName, namespace }, 'Error getting pod failure reasons');
      return [];
    }
  }

  /**
   * Extract GPU model from cloud provider instance type labels
   * Supports Azure, AWS, and GCP instance type naming conventions
   */
  private extractGpuModelFromInstanceType(
    labels: Record<string, string> | undefined
  ): string | undefined {
    if (!labels) return undefined;

    // Get instance type from standard Kubernetes labels
    const instanceType =
      labels['node.kubernetes.io/instance-type'] ||
      labels['beta.kubernetes.io/instance-type'];

    if (!instanceType) return undefined;

    const instanceLower = instanceType.toLowerCase();

    // Azure NV-series GPU mapping
    // Standard_NV36ads_A10_v5 -> A10
    // Standard_NC24ads_A100_v4 -> A100
    // Standard_ND96asr_A100_v4 -> A100
    // Standard_NC6s_v3 (V100), Standard_NC24s_v3, etc.
    // Standard_NV6 (M60 - older)
    if (instanceLower.includes('_a10')) return 'A10';
    if (instanceLower.includes('_a100')) return 'A100-80GB';
    if (instanceLower.includes('_h100')) return 'H100';
    if (instanceLower.includes('nc') && instanceLower.includes('_v3'))
      return 'V100';
    if (instanceLower.includes('nc') && instanceLower.includes('t4'))
      return 'T4';

    // AWS instance type mapping
    // p4d.24xlarge -> A100
    // p5.48xlarge -> H100
    // g4dn.xlarge -> T4
    // g5.xlarge -> A10G
    // g6.xlarge -> L4
    // g6e.xlarge -> L40S
    if (instanceLower.startsWith('p5')) return 'H100';
    if (instanceLower.startsWith('p4d') || instanceLower.startsWith('p4de'))
      return 'A100-40GB';
    if (instanceLower.startsWith('p3')) return 'V100';
    if (instanceLower.startsWith('g4dn') || instanceLower.startsWith('g4ad'))
      return 'T4';
    if (instanceLower.startsWith('g5g') || instanceLower.startsWith('g5.'))
      return 'A10G';
    if (instanceLower.startsWith('g6e')) return 'L40S';
    if (instanceLower.startsWith('g6.')) return 'L4';

    // GCP machine type mapping
    // a2-highgpu-1g (A100 40GB)
    // a2-ultragpu-1g (A100 80GB)
    // a3-highgpu-8g (H100)
    // n1-standard-4 with nvidia-tesla-t4
    // g2-standard-4 (L4)
    if (instanceLower.startsWith('a3')) return 'H100';
    if (instanceLower.startsWith('a2-ultra')) return 'A100-80GB';
    if (instanceLower.startsWith('a2')) return 'A100-40GB';
    if (instanceLower.startsWith('g2')) return 'L4';

    return undefined;
  }

  /**
   * Detect GPU memory from NVIDIA GPU product name
   * This is a best-effort mapping based on common GPU models
   */
  private detectGpuMemoryFromProduct(gpuProduct: string): number | undefined {
    const product = gpuProduct.toLowerCase();

    // NVIDIA Data Center GPUs
    if (product.includes('a100') && product.includes('80')) return 80;
    if (product.includes('a100') && product.includes('40')) return 40;
    if (product.includes('a100')) return 40; // Default A100 is 40GB
    if (product.includes('h100') && product.includes('80')) return 80;
    if (product.includes('h100')) return 80;
    if (product.includes('h200')) return 141;
    if (product.includes('a10g')) return 24;
    if (product.includes('a10')) return 24;
    if (product.includes('l40s')) return 48;
    if (product.includes('l40')) return 48;
    if (product.includes('l4')) return 24;
    if (product.includes('t4')) return 16;
    if (product.includes('v100') && product.includes('32')) return 32;
    if (product.includes('v100')) return 16;

    // NVIDIA Consumer GPUs
    if (product.includes('4090')) return 24;
    if (product.includes('4080')) return 16;
    if (product.includes('3090')) return 24;
    if (product.includes('3080') && product.includes('12')) return 12;
    if (product.includes('3080')) return 10;

    return undefined;
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

  private selectLogContainer(pod: k8s.V1Pod): string | undefined {
    const containers = pod.spec?.containers || [];
    if (containers.length === 0) {
      return undefined;
    }

    const statuses = new Map((pod.status?.containerStatuses || []).map(status => [status.name, status]));
    const preferredNames = ['main', 'vllm', 'model', 'ray-head', 'ray-worker', 'inference', 'worker', 'server', 'frontend'];

    for (const name of preferredNames) {
      if (containers.some(container => container.name === name)) {
        return name;
      }
    }

    const readyContainer = containers.find(container => statuses.get(container.name)?.ready);
    return readyContainer?.name || containers[0].name;
  }

  private async resolveLogContainer(podName: string, namespace: string, requestedContainer?: string): Promise<string | undefined> {
    if (requestedContainer) {
      return requestedContainer;
    }

    const response = await withRetry(
      () => this.coreV1Api.listNamespacedPod({
        namespace,
        fieldSelector: `metadata.name=${podName}`,
        limit: 1,
      }),
      { operationName: 'getPodLogs:listPodByName', maxRetries: 1 }
    );

    const pod = response.items[0];
    return pod ? this.selectLogContainer(pod) : undefined;
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
    try {
      const coreApi = this.coreV1Api;
      const container = await this.resolveLogContainer(podName, namespace, options?.container);
      const response = await withRetry(
        () => coreApi.readNamespacedPodLog({
          name: podName,
          namespace,
          container,
          tailLines: options?.tailLines ?? 100,
          timestamps: options?.timestamps ?? false,
        }),
        { operationName: 'getPodLogs', maxRetries: 2 }
      );

      // Strip ANSI color codes from logs
      const logs = response || '';
      const ansiRegex = /\x1b\[[0-9;]*m/g;
      return logs.replace(ansiRegex, '');
    } catch (error) {
      const statusCode = getK8sStatusCode(error);
      if (statusCode === 404) {
        throw new Error(`Pod '${podName}' not found in namespace '${namespace}'`);
      }
      logger.error({ error, podName, namespace }, 'Error getting pod logs');
      throw new Error(`Failed to get logs for pod '${podName}': ${getK8sErrorMessage(error)}`);
    }
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
          group: INFERENCE_PROVIDER_CONFIG_CRD.apiGroup,
          version: INFERENCE_PROVIDER_CONFIG_CRD.apiVersion,
          plural: INFERENCE_PROVIDER_CONFIG_CRD.plural,
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
   *
   * `available` is true only when the CRDs exist and a Gateway can be selected
   * (a single Gateway, or a Gateway labeled `INFERENCE_GATEWAY_LABEL=true` when
   * multiple Gateways exist). `endpoint` is the selected Gateway's first status
   * address value, when the Gateway has published one.
   */
  async getGatewayStatus(): Promise<GatewayInfo> {
    // Check if InferencePool CRD exists - without it, gateway integration is not supported.
    const inferencePoolCrdExists = await this.checkCRDExists('inferencepools.inference.networking.k8s.io');
    if (!inferencePoolCrdExists) {
      return { available: false };
    }

    // The controller creates HTTPRoutes, so the HTTPRoute CRD must be present.
    const httpRouteCrdExists = await this.checkCRDExists('httproutes.gateway.networking.k8s.io');
    if (!httpRouteCrdExists) {
      return { available: false };
    }

    // The Gateway CRD must exist before the backend can list Gateway resources.
    const gatewayCrdExists = await this.checkCRDExists('gateways.gateway.networking.k8s.io');
    if (!gatewayCrdExists) {
      return { available: false };
    }

    // "Available" means the controller auto-detection can select a Gateway -
    // mirror that path so the UI matches what it will actually pick when
    // reconciling a ModelDeployment with gateway.enabled=true and no explicit
    // gateway override.
    type GatewayItem = {
      metadata?: { name?: string; namespace?: string; labels?: Record<string, string> };
      status?: { addresses?: Array<{ value?: string }> };
    };
    let items: GatewayItem[] = [];
    try {
      const response = await withRetry(
        () => this.customObjectsApi.listClusterCustomObject({
          group: 'gateway.networking.k8s.io',
          version: 'v1',
          plural: 'gateways',
        }),
        { operationName: 'listGateways', maxRetries: 1 }
      );
      items = (response as { items?: GatewayItem[] }).items || [];
    } catch (error) {
      logger.debug({ error: getK8sErrorMessage(error) }, 'Could not list Gateway resources');
      return { available: false };
    }

    if (items.length === 0) {
      return { available: false };
    }

    let selected: GatewayItem | undefined;
    if (items.length === 1) {
      selected = items[0];
    } else {
      // Multiple Gateways: require the controller's inference-gateway label to disambiguate.
      const labeled = items.filter((gw) => gw.metadata?.labels?.[INFERENCE_GATEWAY_LABEL] === 'true');
      if (labeled.length === 0) {
        return { available: false };
      }
      selected = labeled[0];
    }

    const endpoint = selected?.status?.addresses?.[0]?.value;
    return { available: true, endpoint };
  }

  /**
   * List all models accessible through the gateway by checking ModelDeployment status.gateway
   */
  async getGatewayModels(): Promise<GatewayModelInfo[]> {
    const namespace = await this.getDefaultNamespace();
    const models: GatewayModelInfo[] = [];

    try {
      const response = await withRetry(
        () => this.customObjectsApi.listNamespacedCustomObject({
          group: MODEL_DEPLOYMENT_CRD.apiGroup,
          version: MODEL_DEPLOYMENT_CRD.apiVersion,
          namespace,
          plural: MODEL_DEPLOYMENT_CRD.plural,
        }),
        { operationName: 'listDeploymentsForGateway' }
      );

      const items = (response as { items?: ModelDeployment[] }).items || [];
      for (const md of items) {
        const gw = md.status?.gateway;
        if (gw?.modelName) {
          models.push({
            name: gw.modelName,
            deploymentName: md.metadata.name,
            provider: md.status?.provider?.name || md.spec.provider?.name,
            ready: md.status?.conditions?.some(
              (c: { type: string; status: string }) => c.type === 'GatewayReady' && c.status === 'True'
            ) ?? false,
          });
        }
      }
    } catch (error) {
      logger.debug({ error: getK8sErrorMessage(error) }, 'Could not list ModelDeployments for gateway models');
    }

    return models;
  }

  /**
   * Check Gateway API and GAIE CRD installation status.
   * Also includes live gateway availability info.
   */
  async checkGatewayCRDStatus(): Promise<GatewayCRDStatus> {
    const { PINNED_GAIE_VERSION, GAIE_CRD_URL, GATEWAY_API_CRD_URL } = await import('@airunway/shared');

    const [gatewayApiStatus, inferenceExtStatus] = await Promise.all([
      this.getCRDStatusFromAnnotations(GATEWAY_API_CRD_NAME, GATEWAY_API_VERSION_ANNOTATIONS),
      this.getCRDStatusFromAnnotations(INFERENCE_POOL_CRD_NAME, INFERENCE_EXTENSION_VERSION_ANNOTATIONS),
    ]);

    const gatewayApiInstalled = gatewayApiStatus.installed;
    const inferenceExtInstalled = inferenceExtStatus.installed;
    const gatewayApiVersion = gatewayApiStatus.version;
    const inferenceExtVersion = inferenceExtStatus.version;

    // Get live gateway status
    let gatewayAvailable = false;
    let gatewayEndpoint: string | undefined;
    if (gatewayApiInstalled && inferenceExtInstalled) {
      try {
        const gwStatus = await this.getGatewayStatus();
        gatewayAvailable = gwStatus.available;
        gatewayEndpoint = gwStatus.endpoint;
      } catch {
        // Gateway status check failed, not critical
      }
    }

    const allInstalled = gatewayApiInstalled && inferenceExtInstalled;
    let message: string;
    if (allInstalled && gatewayAvailable) {
      message = 'Gateway API and Inference Extension CRDs are installed. Gateway is available.';
    } else if (allInstalled) {
      message = 'Gateway API and Inference Extension CRDs are installed. No active gateway detected.';
    } else if (!gatewayApiInstalled && !inferenceExtInstalled) {
      message = 'Gateway API and Inference Extension CRDs are not installed.';
    } else if (!gatewayApiInstalled) {
      message = 'Gateway API CRDs are not installed.';
    } else {
      message = 'Inference Extension CRDs are not installed.';
    }

    return {
      gatewayApiInstalled,
      inferenceExtInstalled,
      gatewayApiVersion,
      inferenceExtVersion,
      pinnedVersion: PINNED_GAIE_VERSION,
      gatewayAvailable,
      gatewayEndpoint,
      message,
      installCommands: [
        `kubectl apply -f ${GATEWAY_API_CRD_URL}`,
        `kubectl apply -f ${GAIE_CRD_URL}`,
      ],
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
    const response = await this.proxyServiceRequest(serviceName, namespace, port, path, {
      method: 'GET',
      headers: {
        'Accept': options.accept ?? 'text/plain',
      },
      signal: options.signal,
      userToken: options.userToken,
    });

    if (!response.ok) {
      throw new Error(`HTTP ${response.status}: ${response.statusText}`);
    }
    return await response.text();
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
    return await this.proxyServiceRequest(serviceName, namespace, port, path, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Accept': 'text/event-stream',
        ...headers,
      },
      body: JSON.stringify(body),
      signal: options.signal,
      userToken: options.userToken,
    });
  }

  private async proxyServiceRequest(
    serviceName: string,
    namespace: string,
    port: number,
    path: string,
    init: ProxyServiceRequestInit
  ): Promise<Response> {
    const { userToken, ...requestInit } = init;
    const kubeConfig = userToken ? this.createUserKubeConfig(userToken) : this.kc;
    const cluster = kubeConfig.getCurrentCluster();
    if (!cluster) {
      throw new Error('No active Kubernetes cluster configured');
    }

    // Build proxy URL: /api/v1/namespaces/{ns}/services/{name}:{port}/proxy/{path}
    const proxyUrl = `${cluster.server}/api/v1/namespaces/${encodeURIComponent(namespace)}/services/${encodeURIComponent(serviceName)}:${port}/proxy/${path}`;

    // Extract auth headers from KubeConfig
    const authOpts = await kubeConfig.applyToFetchOptions({ headers: {} } as https.RequestOptions);

    // Extract TLS material (CA, client cert/key, SNI, verification mode) via the
    // shared kubeconfig→Bun mapping, so this raw-`fetch` path and the typed-API
    // path (`BunTlsHttpLibrary`) stay in lockstep and cannot drift.
    const tlsOpts = await kubeConfigToBunTls(kubeConfig);

    const headers = new Headers((authOpts.headers as HeadersInit) || {});
    if (requestInit.headers) {
      new Headers(requestInit.headers).forEach((value, key) => headers.set(key, value));
    }

    const fetchOpts: RequestInit & { tls?: BunTlsOptions } = {
      ...requestInit,
      headers,
    };

    if (tlsOpts) {
      fetchOpts.tls = tlsOpts;
    }

    return await fetch(proxyUrl, fetchOpts);
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
