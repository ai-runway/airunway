import * as k8s from '@kubernetes/client-node';
import type { AutoscalerDetectionResult, AutoscalerStatusInfo } from '@airunway/shared';
import { withRetry } from '../lib/retry';
import { loadKubeConfig, makeApiClient } from '../lib/kubeconfig';
import { getK8sErrorStatusCode } from '../lib/k8s-errors';
import logger from '../lib/logger';
import * as yaml from 'js-yaml';

/**
 * Subset of the cluster-autoscaler status ConfigMap (YAML) that we read.
 * The upstream format is loosely specified, so every field is optional.
 */
interface ClusterAutoscalerStatus {
  time?: string;
  autoscalerStatus?: string;
  clusterWide?: {
    health?: { status?: string };
  };
  nodeGroups?: Array<{
    name?: string;
    health?: {
      minSize?: number;
      maxSize?: number;
      cloudProviderTarget?: number;
      nodeCounts?: { registered?: { total?: number } };
    };
  }>;
}

class AutoscalerService {
  private kc: k8s.KubeConfig;
  private coreV1Api: k8s.CoreV1Api;
  private appsV1Api: k8s.AppsV1Api;
  private customObjectsApi: k8s.CustomObjectsApi;

  constructor() {
    this.kc = loadKubeConfig();
    this.coreV1Api = makeApiClient(this.kc, k8s.CoreV1Api);
    this.appsV1Api = makeApiClient(this.kc, k8s.AppsV1Api);
    this.customObjectsApi = makeApiClient(this.kc, k8s.CustomObjectsApi);
  }

  /**
   * Detect if the cluster is running on Azure Kubernetes Service (AKS)
   */
  async isAKSCluster(): Promise<boolean> {
    try {
      const nodesResponse = await withRetry(
        () => this.coreV1Api.listNode(),
        { operationName: 'isAKSCluster', maxRetries: 1 }
      );

      if (nodesResponse.items.length === 0) {
        return false;
      }

      const node = nodesResponse.items[0];
      const labels = node.metadata?.labels || {};
      const providerID = node.spec?.providerID || '';

      // Check for AKS-specific indicators
      const hasAzureLabel = 'kubernetes.azure.com/cluster' in labels;
      const hasAzureProviderID = providerID.startsWith('azure://');

      return hasAzureLabel || hasAzureProviderID;
    } catch (error) {
      logger.error({ error }, 'Error checking if cluster is AKS');
      return false;
    }
  }

  /**
   * Check if AKS managed autoscaler is enabled
   * AKS autoscaling is configured at the node pool level via Azure API,
   * but we can detect it by looking for specific labels on nodes
   */
  private async detectAKSManagedAutoscaler(): Promise<{
    detected: boolean;
    nodeGroupCount: number;
  }> {
    try {
      const nodesResponse = await withRetry(
        () => this.coreV1Api.listNode(),
        { operationName: 'detectAKSManagedAutoscaler', maxRetries: 1 }
      );

      // Count unique node pools (agentpools) that have autoscaling enabled
      const autoscalingNodePools = new Set<string>();

      for (const node of nodesResponse.items) {
        const labels = node.metadata?.labels || {};

        // AKS labels autoscaler-enabled nodes with this label
        const hasAutoscalerLabel = labels['cluster-autoscaler.kubernetes.io/enabled'] === 'true';

        if (hasAutoscalerLabel) {
          // Get the node pool name
          const nodePool = labels['agentpool'] || labels['kubernetes.azure.com/agentpool'];
          if (nodePool) {
            autoscalingNodePools.add(nodePool);
          }
        }
      }

      return {
        detected: autoscalingNodePools.size > 0,
        nodeGroupCount: autoscalingNodePools.size,
      };
    } catch (error) {
      logger.error({ error }, 'Error detecting AKS managed autoscaler');
      return { detected: false, nodeGroupCount: 0 };
    }
  }

  /**
   * Check if cluster-autoscaler is installed in the cluster
   * Primary detection: Check for cluster-autoscaler-status ConfigMap in kube-system namespace
   * Fallback: Look for cluster-autoscaler deployment
   */
  private async detectClusterAutoscalerDeployment(): Promise<{
    detected: boolean;
    healthy: boolean;
    nodeGroupCount: number;
  }> {
    try {
      // Primary detection: Check for cluster-autoscaler-status ConfigMap
      // This is the most reliable indicator that cluster-autoscaler is running
      const statusInfo = await this.getAutoscalerStatus();

      if (statusInfo) {
        // ConfigMap exists, so cluster-autoscaler is present
        const nodeGroupCount = statusInfo.nodeGroups?.length || 0;

        // Check if status is fresh (updated within last 5 minutes)
        let healthy = true;
        if (statusInfo.lastUpdateTime) {
          const lastUpdate = new Date(statusInfo.lastUpdateTime);
          const now = new Date();
          const ageMinutes = (now.getTime() - lastUpdate.getTime()) / (1000 * 60);
          healthy = ageMinutes <= 5;
        }

        return {
          detected: true,
          healthy,
          nodeGroupCount,
        };
      }

      // Fallback: Look for cluster-autoscaler deployment
      const deploymentsResponse = await withRetry(
        () => this.appsV1Api.listNamespacedDeployment({
          namespace: 'kube-system',
          labelSelector: 'app=cluster-autoscaler',
        }),
        { operationName: 'detectClusterAutoscalerDeployment', maxRetries: 1 }
      );

      const deployments = deploymentsResponse.items;

      if (deployments.length === 0) {
        // Try without label selector
        const allDeployments = await this.appsV1Api.listNamespacedDeployment({ namespace: 'kube-system' });
        const caDeployment = allDeployments.items.find(
          d => d.metadata?.name?.includes('cluster-autoscaler')
        );

        if (!caDeployment) {
          return { detected: false, healthy: false, nodeGroupCount: 0 };
        }

        deployments.push(caDeployment);
      }

      const deployment = deployments[0];

      // Check if deployment is healthy
      const replicas = deployment.status?.replicas || 0;
      const readyReplicas = deployment.status?.readyReplicas || 0;
      const healthy = replicas > 0 && readyReplicas === replicas;

      return {
        detected: true,
        healthy,
        nodeGroupCount: 0, // ConfigMap not available, can't determine node group count
      };
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);
      if (statusCode !== 404 && statusCode !== 403) {
        logger.error({ error }, 'Error detecting cluster-autoscaler');
      }
      return { detected: false, healthy: false, nodeGroupCount: 0 };
    }
  }

  /**
   * Get cluster-autoscaler status from ConfigMap
   */
  async getAutoscalerStatus(): Promise<AutoscalerStatusInfo | null> {
    try {
      const configMapResponse = await withRetry(
        () => this.coreV1Api.readNamespacedConfigMap({
          name: 'cluster-autoscaler-status',
          namespace: 'kube-system',
        }),
        { operationName: 'getAutoscalerStatus', maxRetries: 1 }
      );

      const configMap = configMapResponse;
      const statusData = configMap.data?.['status'] || '{}';

      let parsedStatus: ClusterAutoscalerStatus;
      try {
        // Parse YAML format (cluster-autoscaler uses YAML in the ConfigMap)
        parsedStatus = (yaml.load(statusData) as ClusterAutoscalerStatus) ?? {};
      } catch (error) {
        logger.warn({ error }, 'Failed to parse cluster-autoscaler-status ConfigMap');
        return null;
      }

      // Parse node groups from the YAML structure
      const nodeGroups: AutoscalerStatusInfo['nodeGroups'] = [];
      if (Array.isArray(parsedStatus.nodeGroups)) {
        for (const group of parsedStatus.nodeGroups) {
          nodeGroups.push({
            name: group.name || 'unknown',
            minSize: group.health?.minSize || 0,
            maxSize: group.health?.maxSize || 0,
            currentSize: group.health?.cloudProviderTarget || group.health?.nodeCounts?.registered?.total || 0,
          });
        }
      }

      // Extract timestamp from the 'time' field
      const lastUpdateTime = parsedStatus.time ? new Date(parsedStatus.time).toISOString() : undefined;

      return {
        health: parsedStatus.clusterWide?.health?.status || parsedStatus.autoscalerStatus || 'unknown',
        lastUpdateTime,
        nodeGroups,
      };
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);
      if (statusCode !== 404 && statusCode !== 403) {
        logger.error({ error }, 'Error getting autoscaler status');
      }
      return null;
    }
  }

  /**
   * Detect autoscaler type and health status
   */
  async detectAutoscaler(): Promise<AutoscalerDetectionResult> {
    try {
      // Step 1: Check if this is an AKS cluster
      const isAKS = await this.isAKSCluster();

      if (isAKS) {
        // Step 2a: Check for AKS managed autoscaler
        const aksManagedResult = await this.detectAKSManagedAutoscaler();

        if (aksManagedResult.detected) {
          return {
            type: 'aks-managed',
            detected: true,
            healthy: true, // AKS managed autoscaler is managed by Azure, assume healthy
            message: `AKS managed autoscaler detected on ${aksManagedResult.nodeGroupCount} node pool(s)`,
            nodeGroupCount: aksManagedResult.nodeGroupCount,
          };
        }
      }

      // Step 2b: Check for in-cluster cluster-autoscaler (via ConfigMap or deployment)
      const clusterAutoscalerResult = await this.detectClusterAutoscalerDeployment();

      if (clusterAutoscalerResult.detected) {
        const message = clusterAutoscalerResult.healthy
          ? clusterAutoscalerResult.nodeGroupCount > 0
            ? `Cluster Autoscaler running on ${clusterAutoscalerResult.nodeGroupCount} node group(s)`
            : 'Cluster Autoscaler running'
          : 'Cluster Autoscaler detected but status is stale or unhealthy';

        return {
          type: 'cluster-autoscaler',
          detected: true,
          healthy: clusterAutoscalerResult.healthy,
          message,
          nodeGroupCount: clusterAutoscalerResult.nodeGroupCount,
        };
      }

      // Step 3: No autoscaler detected
      return {
        type: 'none',
        detected: false,
        healthy: false,
        message: isAKS
          ? 'No autoscaler detected. Enable AKS node pool autoscaling to automatically scale your cluster.'
          : 'No autoscaler detected. Install Kubernetes Cluster Autoscaler to automatically scale your cluster.',
      };
    } catch (error) {
      logger.error({ error }, 'Error detecting autoscaler');
      return {
        type: 'unknown',
        detected: false,
        healthy: false,
        message: 'Unable to determine autoscaler status due to an error',
      };
    }
  }
}

export const autoscalerService = new AutoscalerService();
