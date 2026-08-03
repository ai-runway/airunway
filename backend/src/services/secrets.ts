import * as k8s from '@kubernetes/client-node';
import { loadKubeConfig, makeApiClient } from '../lib/kubeconfig';
import logger from '../lib/logger';
import { withRetry } from '../lib/retry';
import { getK8sErrorStatusCode, type K8sApiError } from '../lib/k8s-errors';
import type { HfSecretStatus, HfUserInfo } from '@airunway/shared';
import { huggingFaceService } from './huggingface';

/**
 * Standard secret name for HuggingFace token
 */
const HF_SECRET_NAME = 'hf-token-secret';

/**
 * Key used to store the token in the secret
 */
const HF_TOKEN_KEY = 'HF_TOKEN';

/**
 * Namespaces where the HF secret should be distributed
 * These are the namespaces used by different inference providers
 */
const TARGET_NAMESPACES = ['dynamo-system', 'ray-system', 'kuberay-system', 'kaito-workspace', 'default'];

/**
 * Secrets Service
 * Handles creation and management of Kubernetes secrets for HuggingFace tokens
 */
class SecretsService {
  private kc: k8s.KubeConfig;
  private coreV1Api: k8s.CoreV1Api;

  constructor() {
    this.kc = loadKubeConfig();
    this.coreV1Api = makeApiClient(this.kc, k8s.CoreV1Api);
  }

  /**
   * Ensure a namespace exists, creating it if necessary
   */
  private async ensureNamespace(namespace: string): Promise<boolean> {
    try {
      await this.coreV1Api.readNamespace({ name: namespace });
      return true;
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);
      if (statusCode === 404) {
        // Namespace doesn't exist, create it
        try {
          await this.coreV1Api.createNamespace({
            body: {
              apiVersion: 'v1',
              kind: 'Namespace',
              metadata: {
                name: namespace,
              },
            },
          });
          logger.info({ namespace }, 'Created namespace for HF secret');
          return true;
        } catch (createError) {
          logger.error({ error: createError, namespace }, 'Failed to create namespace');
          return false;
        }
      }
      logger.error({ error, namespace }, 'Error checking namespace');
      return false;
    }
  }

  /**
   * Create or update the HuggingFace token secret in a single namespace
   */
  private async upsertSecretInNamespace(
    namespace: string,
    token: string
  ): Promise<{ success: boolean; error?: string }> {
    const secretManifest: k8s.V1Secret = {
      apiVersion: 'v1',
      kind: 'Secret',
      metadata: {
        name: HF_SECRET_NAME,
        namespace,
        labels: {
          'app.kubernetes.io/managed-by': 'airunway',
          'airunway.ai/secret-type': 'huggingface-token',
        },
      },
      type: 'Opaque',
      stringData: {
        [HF_TOKEN_KEY]: token,
      },
    };

    try {
      // Try to read existing secret
      await this.coreV1Api.readNamespacedSecret({ name: HF_SECRET_NAME, namespace });

      // Secret exists, update it
      await withRetry(
        () => this.coreV1Api.replaceNamespacedSecret({ name: HF_SECRET_NAME, namespace, body: secretManifest }),
        { operationName: `updateSecret:${namespace}`, maxRetries: 2 }
      );
      logger.info({ namespace, secretName: HF_SECRET_NAME }, 'Updated HuggingFace secret');
      return { success: true };
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);

      if (statusCode === 404) {
        // Secret doesn't exist, create it
        try {
          await withRetry(
            () => this.coreV1Api.createNamespacedSecret({ namespace, body: secretManifest }),
            { operationName: `createSecret:${namespace}`, maxRetries: 2 }
          );
          logger.info({ namespace, secretName: HF_SECRET_NAME }, 'Created HuggingFace secret');
          return { success: true };
        } catch (createError) {
          const errorMsg = (createError as K8sApiError)?.message || 'Unknown error';
          logger.error({ error: createError, namespace }, 'Failed to create HuggingFace secret');
          return { success: false, error: errorMsg };
        }
      }

      const errorMsg = (error as K8sApiError)?.message || 'Unknown error';
      logger.error({ error, namespace }, 'Failed to update HuggingFace secret');
      return { success: false, error: errorMsg };
    }
  }

  /**
   * Delete the HuggingFace token secret from a single namespace
   */
  private async deleteSecretFromNamespace(
    namespace: string
  ): Promise<{ success: boolean; error?: string }> {
    try {
      await withRetry(
        () => this.coreV1Api.deleteNamespacedSecret({ name: HF_SECRET_NAME, namespace }),
        { operationName: `deleteSecret:${namespace}`, maxRetries: 2 }
      );
      logger.info({ namespace, secretName: HF_SECRET_NAME }, 'Deleted HuggingFace secret');
      return { success: true };
    } catch (error) {
      const statusCode = getK8sErrorStatusCode(error);

      if (statusCode === 404) {
        // Secret doesn't exist, consider it success
        return { success: true };
      }

      const errorMsg = (error as K8sApiError)?.message || 'Unknown error';
      logger.error({ error, namespace }, 'Failed to delete HuggingFace secret');
      return { success: false, error: errorMsg };
    }
  }

  /**
   * Check if a secret exists in a namespace
   */
  private async checkSecretInNamespace(namespace: string): Promise<boolean> {
    try {
      await this.coreV1Api.readNamespacedSecret({ name: HF_SECRET_NAME, namespace });
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Distribute the HuggingFace token to all target namespaces
   * Creates namespaces if they don't exist
   */
  async distributeHfSecret(token: string): Promise<{
    success: boolean;
    results: { namespace: string; success: boolean; error?: string }[];
  }> {
    logger.info({ namespaces: TARGET_NAMESPACES }, 'Distributing HuggingFace secret');

    const results: { namespace: string; success: boolean; error?: string }[] = [];

    for (const namespace of TARGET_NAMESPACES) {
      // Ensure namespace exists
      const nsExists = await this.ensureNamespace(namespace);
      if (!nsExists) {
        logger.warn({ namespace }, 'Namespace does not exist and could not be created');
        results.push({ namespace, success: false, error: 'Failed to create namespace' });
        continue;
      }

      // Create/update secret
      const result = await this.upsertSecretInNamespace(namespace, token);
      logger.info({ namespace, success: result.success, error: result.error }, 'Secret upsert result');
      results.push({ namespace, ...result });
    }

    const allSuccess = results.every((r) => r.success);
    logger.info({ allSuccess, results }, 'HuggingFace secret distribution complete');
    return { success: allSuccess, results };
  }

  /**
   * Get the status of HuggingFace secrets across all target namespaces
   * Also validates the token if a secret exists
   */
  async getHfSecretStatus(): Promise<HfSecretStatus> {
    const namespaces: { name: string; exists: boolean }[] = [];
    let user: HfUserInfo | undefined;
    let token: string | undefined;

    for (const namespace of TARGET_NAMESPACES) {
      const exists = await this.checkSecretInNamespace(namespace);
      namespaces.push({ name: namespace, exists });

      // Try to get the token from the first existing secret to validate it
      if (exists && !token) {
        try {
          const secret = await this.coreV1Api.readNamespacedSecret({ name: HF_SECRET_NAME, namespace });
          const tokenData = secret.data?.[HF_TOKEN_KEY];
          if (tokenData) {
            token = Buffer.from(tokenData, 'base64').toString('utf-8');
          }
        } catch {
          // Ignore errors reading secret data
        }
      }
    }

    const configured = namespaces.some((n) => n.exists);

    // Validate token and get user info if we have a token
    if (token) {
      try {
        const validation = await huggingFaceService.validateToken(token);
        if (validation.valid && validation.user) {
          user = validation.user;
        }
      } catch {
        // Token validation failed, but secret exists
        logger.warn('HuggingFace token validation failed');
      }
    }

    return { configured, namespaces, user };
  }

  /**
   * Delete HuggingFace secrets from all target namespaces
   */
  async deleteHfSecrets(): Promise<{
    success: boolean;
    results: { namespace: string; success: boolean; error?: string }[];
  }> {
    logger.info({ namespaces: TARGET_NAMESPACES }, 'Deleting HuggingFace secrets');

    const results: { namespace: string; success: boolean; error?: string }[] = [];

    for (const namespace of TARGET_NAMESPACES) {
      const result = await this.deleteSecretFromNamespace(namespace);
      results.push({ namespace, ...result });
    }

    const allSuccess = results.every((r) => r.success);
    return { success: allSuccess, results };
  }
}

// Export singleton instance
export const secretsService = new SecretsService();
