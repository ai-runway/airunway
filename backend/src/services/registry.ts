import * as k8s from '@kubernetes/client-node';
import { loadKubeConfig, makeApiClient } from '../lib/kubeconfig';
import logger from '../lib/logger';
import { withRetry } from '../lib/retry';
import { getK8sErrorStatusCode } from '../lib/k8s-errors';

/**
 * In-cluster registry configuration
 * Uses NodePort to allow kubelets (running on nodes) to access the registry
 */
export const REGISTRY_CONFIG = {
  name: 'airunway-registry',
  namespace: 'airunway-system',
  port: 5000,
  nodePort: 30500, // NodePort for kubelet access
  image: 'registry:2',
  storageSize: '10Gi',
} as const;

/**
 * Registry health status
 */
export interface RegistryStatus {
  installed: boolean;
  ready: boolean;
  url: string;
  message: string;
}

/**
 * Registry Service
 * Manages in-cluster container registry for AIKit images
 */
class RegistryService {
  private kc: k8s.KubeConfig;
  private coreV1Api: k8s.CoreV1Api;
  private appsV1Api: k8s.AppsV1Api;

  constructor() {
    this.kc = loadKubeConfig();
    this.coreV1Api = makeApiClient(this.kc, k8s.CoreV1Api);
    this.appsV1Api = makeApiClient(this.kc, k8s.AppsV1Api);
  }

  /**
   * Get the in-cluster registry URL for buildx push (uses ClusterIP service name)
   */
  getRegistryUrl(): string {
    return `${REGISTRY_CONFIG.name}.${REGISTRY_CONFIG.namespace}.svc:${REGISTRY_CONFIG.port}`;
  }

  /**
   * Get the registry URL for kubelet image pulls (uses localhost + NodePort)
   * Kubelets run on nodes and can access NodePort services via localhost
   */
  getKubeletRegistryUrl(): string {
    return `localhost:${REGISTRY_CONFIG.nodePort}`;
  }

  /**
   * Get the full image reference for buildx push (uses cluster-internal URL)
   */
  getImageRef(imageName: string, imageTag: string): string {
    return `${this.getRegistryUrl()}/${imageName}:${imageTag}`;
  }

  /**
   * Get the full image reference for kubelet pulls (uses localhost + NodePort)
   */
  getKubeletImageRef(imageName: string, imageTag: string): string {
    return `${this.getKubeletRegistryUrl()}/${imageName}:${imageTag}`;
  }

  /**
   * Check if the registry is installed and ready
   */
  async checkStatus(): Promise<RegistryStatus> {
    const url = this.getRegistryUrl();

    try {
      // Check if deployment exists
      const deployment = await this.getDeployment();
      if (!deployment) {
        return {
          installed: false,
          ready: false,
          url,
          message: 'Registry deployment not found',
        };
      }

      // Check if deployment is ready
      const readyReplicas = deployment.status?.readyReplicas || 0;
      const desiredReplicas = deployment.spec?.replicas || 1;
      const isReady = readyReplicas >= desiredReplicas;

      return {
        installed: true,
        ready: isReady,
        url,
        message: isReady
          ? `Registry ready at ${url}`
          : `Registry not ready: ${readyReplicas}/${desiredReplicas} replicas`,
      };
    } catch (error) {
      logger.error({ error }, 'Error checking registry status');
      return {
        installed: false,
        ready: false,
        url,
        message: error instanceof Error ? error.message : 'Unknown error',
      };
    }
  }

  /**
   * Get the registry deployment if it exists
   */
  private async getDeployment(): Promise<k8s.V1Deployment | null> {
    try {
      const response = await withRetry(
        () => this.appsV1Api.readNamespacedDeployment({
          name: REGISTRY_CONFIG.name,
          namespace: REGISTRY_CONFIG.namespace,
        }),
        { operationName: 'getRegistryDeployment', maxRetries: 1 }
      );
      return response;
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);
      if (statusCode === 404) {
        return null;
      }
      throw error;
    }
  }

  /**
   * Get the registry service if it exists
   */
  private async getService(): Promise<k8s.V1Service | null> {
    try {
      const response = await withRetry(
        () => this.coreV1Api.readNamespacedService({
          name: REGISTRY_CONFIG.name,
          namespace: REGISTRY_CONFIG.namespace,
        }),
        { operationName: 'getRegistryService', maxRetries: 1 }
      );
      return response;
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);
      if (statusCode === 404) {
        return null;
      }
      throw error;
    }
  }

  /**
   * Ensure the namespace exists
   */
  private async ensureNamespace(): Promise<void> {
    try {
      await this.coreV1Api.readNamespace({ name: REGISTRY_CONFIG.namespace });
      logger.debug({ namespace: REGISTRY_CONFIG.namespace }, 'Namespace already exists');
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);
      if (statusCode === 404) {
        logger.info({ namespace: REGISTRY_CONFIG.namespace }, 'Creating namespace');
        await this.coreV1Api.createNamespace({
          body: {
            metadata: {
              name: REGISTRY_CONFIG.namespace,
              labels: {
                'app.kubernetes.io/managed-by': 'airunway',
              },
            },
          },
        });
      } else {
        throw error;
      }
    }
  }

  /**
   * Create the registry deployment
   */
  private async createDeployment(): Promise<void> {
    const deployment: k8s.V1Deployment = {
      apiVersion: 'apps/v1',
      kind: 'Deployment',
      metadata: {
        name: REGISTRY_CONFIG.name,
        namespace: REGISTRY_CONFIG.namespace,
        labels: {
          app: REGISTRY_CONFIG.name,
          'app.kubernetes.io/name': REGISTRY_CONFIG.name,
          'app.kubernetes.io/managed-by': 'airunway',
        },
      },
      spec: {
        replicas: 1,
        selector: {
          matchLabels: {
            app: REGISTRY_CONFIG.name,
          },
        },
        template: {
          metadata: {
            labels: {
              app: REGISTRY_CONFIG.name,
            },
          },
          spec: {
            containers: [
              {
                name: 'registry',
                image: REGISTRY_CONFIG.image,
                ports: [
                  {
                    containerPort: REGISTRY_CONFIG.port,
                    protocol: 'TCP',
                  },
                ],
                env: [
                  {
                    name: 'REGISTRY_STORAGE_DELETE_ENABLED',
                    value: 'true',
                  },
                ],
                resources: {
                  requests: {
                    cpu: '100m',
                    memory: '256Mi',
                  },
                  limits: {
                    cpu: '500m',
                    memory: '512Mi',
                  },
                },
                livenessProbe: {
                  httpGet: {
                    path: '/v2/',
                    port: REGISTRY_CONFIG.port,
                  },
                  initialDelaySeconds: 5,
                  periodSeconds: 10,
                },
                readinessProbe: {
                  httpGet: {
                    path: '/v2/',
                    port: REGISTRY_CONFIG.port,
                  },
                  initialDelaySeconds: 2,
                  periodSeconds: 5,
                },
                volumeMounts: [
                  {
                    name: 'registry-data',
                    mountPath: '/var/lib/registry',
                  },
                ],
              },
            ],
            volumes: [
              {
                name: 'registry-data',
                emptyDir: {}, // TODO: Add PVC for persistence in production
              },
            ],
          },
        },
      },
    };

    await withRetry(
      () => this.appsV1Api.createNamespacedDeployment({
        namespace: REGISTRY_CONFIG.namespace,
        body: deployment,
      }),
      { operationName: 'createRegistryDeployment' }
    );

    logger.info({ name: REGISTRY_CONFIG.name, namespace: REGISTRY_CONFIG.namespace }, 'Created registry deployment');
  }

  /**
   * Create the registry service
   */
  private async createService(): Promise<void> {
    const service: k8s.V1Service = {
      apiVersion: 'v1',
      kind: 'Service',
      metadata: {
        name: REGISTRY_CONFIG.name,
        namespace: REGISTRY_CONFIG.namespace,
        labels: {
          app: REGISTRY_CONFIG.name,
          'app.kubernetes.io/name': REGISTRY_CONFIG.name,
          'app.kubernetes.io/managed-by': 'airunway',
        },
      },
      spec: {
        type: 'NodePort',
        selector: {
          app: REGISTRY_CONFIG.name,
        },
        ports: [
          {
            name: 'registry',
            port: REGISTRY_CONFIG.port,
            targetPort: REGISTRY_CONFIG.port,
            nodePort: REGISTRY_CONFIG.nodePort,
            protocol: 'TCP',
          },
        ],
      },
    };

    await withRetry(
      () => this.coreV1Api.createNamespacedService({
        namespace: REGISTRY_CONFIG.namespace,
        body: service,
      }),
      { operationName: 'createRegistryService' }
    );

    logger.info({ name: REGISTRY_CONFIG.name, namespace: REGISTRY_CONFIG.namespace }, 'Created registry service');
  }

  /**
   * Ensure the in-cluster registry is deployed and ready
   * Creates the registry if it doesn't exist
   */
  async ensureRegistry(): Promise<RegistryStatus> {
    logger.info('Ensuring in-cluster registry is deployed');

    try {
      // Ensure namespace exists
      await this.ensureNamespace();

      // Check if deployment exists, create if not
      const existingDeployment = await this.getDeployment();
      if (!existingDeployment) {
        await this.createDeployment();
      } else {
        logger.debug({ name: REGISTRY_CONFIG.name }, 'Registry deployment already exists');
      }

      // Check if service exists, create if not
      const existingService = await this.getService();
      if (!existingService) {
        await this.createService();
      } else {
        logger.debug({ name: REGISTRY_CONFIG.name }, 'Registry service already exists');
      }

      // Wait for deployment to be ready (with timeout)
      const status = await this.waitForReady(60000); // 60 second timeout

      return status;
    } catch (error) {
      logger.error({ error }, 'Error ensuring registry');
      return {
        installed: false,
        ready: false,
        url: this.getRegistryUrl(),
        message: error instanceof Error ? error.message : 'Unknown error',
      };
    }
  }

  /**
   * Wait for the registry to be ready
   */
  private async waitForReady(timeoutMs: number = 60000): Promise<RegistryStatus> {
    const startTime = Date.now();
    const pollInterval = 2000; // 2 seconds

    while (Date.now() - startTime < timeoutMs) {
      const status = await this.checkStatus();
      if (status.ready) {
        return status;
      }

      logger.debug({ status }, 'Waiting for registry to be ready');
      await new Promise((resolve) => setTimeout(resolve, pollInterval));
    }

    // Final status check after timeout
    const finalStatus = await this.checkStatus();
    if (!finalStatus.ready) {
      finalStatus.message = `Registry not ready after ${timeoutMs / 1000} seconds: ${finalStatus.message}`;
    }

    return finalStatus;
  }

  /**
   * Delete the in-cluster registry (for cleanup)
   */
  async deleteRegistry(): Promise<void> {
    logger.info({ name: REGISTRY_CONFIG.name, namespace: REGISTRY_CONFIG.namespace }, 'Deleting registry');

    try {
      // Delete deployment
      await this.appsV1Api.deleteNamespacedDeployment({
        name: REGISTRY_CONFIG.name,
        namespace: REGISTRY_CONFIG.namespace,
      });
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);
      if (statusCode !== 404) {
        throw error;
      }
    }

    try {
      // Delete service
      await this.coreV1Api.deleteNamespacedService({
        name: REGISTRY_CONFIG.name,
        namespace: REGISTRY_CONFIG.namespace,
      });
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);
      if (statusCode !== 404) {
        throw error;
      }
    }

    logger.info('Registry deleted');
  }
}

// Export singleton instance
export const registryService = new RegistryService();
