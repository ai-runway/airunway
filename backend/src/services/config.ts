import * as k8s from '@kubernetes/client-node';
import { loadKubeConfig } from '../lib/kubeconfig';
import logger from '../lib/logger';

// Default namespace for AI Runway deployments
const DEFAULT_AIRUNWAY_NAMESPACE = 'airunway-system';

/**
 * Application configuration stored in Kubernetes ConfigMap
 */
export interface AppConfig {
  defaultNamespace?: string;
}

const CONFIG_NAMESPACE = 'airunway-system';
const CONFIG_NAME = 'airunway-config';
const CONFIG_KEY = 'config.json';

/**
 * Config Service
 * Manages application configuration with Kubernetes ConfigMap persistence
 */
class ConfigService {
  private kc: k8s.KubeConfig;
  private coreV1Api: k8s.CoreV1Api;
  private cachedConfig: AppConfig | null = null;
  private initialized = false;

  constructor() {
    this.kc = loadKubeConfig();
    this.coreV1Api = this.kc.makeApiClient(k8s.CoreV1Api);
  }

  /**
   * Get the default configuration
   */
  private getDefaultConfig(): AppConfig {
    return {
      defaultNamespace: process.env.DEFAULT_NAMESPACE || DEFAULT_AIRUNWAY_NAMESPACE,
    };
  }

  /**
   * Ensure the airunway namespace exists
   */
  private async ensureNamespace(): Promise<void> {
    try {
      await this.coreV1Api.readNamespace(CONFIG_NAMESPACE);
    } catch (error: unknown) {
      const k8sError = error as { response?: { statusCode?: number } };
      if (k8sError?.response?.statusCode === 404) {
        // Create namespace
        await this.coreV1Api.createNamespace({
          metadata: {
            name: CONFIG_NAMESPACE,
            labels: {
              'app.kubernetes.io/name': 'airunway',
              'app.kubernetes.io/managed-by': 'airunway',
            },
          },
        });
        logger.info({ namespace: CONFIG_NAMESPACE }, `Created namespace '${CONFIG_NAMESPACE}'`);
      } else {
        throw error;
      }
    }
  }

  /**
   * Get the current configuration
   * Falls back to defaults if ConfigMap doesn't exist
   */
  async getConfig(): Promise<AppConfig> {
    // Return cached config if available
    if (this.cachedConfig && this.initialized) {
      return this.cachedConfig;
    }

    try {
      const response = await this.coreV1Api.readNamespacedConfigMap(
        CONFIG_NAME,
        CONFIG_NAMESPACE
      );

      const configData = response.body.data?.[CONFIG_KEY];
      if (configData) {
        this.cachedConfig = JSON.parse(configData) as AppConfig;
        this.initialized = true;
        return this.cachedConfig;
      }
    } catch (error: unknown) {
      const k8sError = error as { response?: { statusCode?: number } };
      if (k8sError?.response?.statusCode === 404) {
        // ConfigMap doesn't exist, use defaults
        logger.debug('ConfigMap not found, using default configuration');
      } else {
        logger.error({ error }, 'Error reading ConfigMap');
      }
    }

    // Return default config
    this.cachedConfig = this.getDefaultConfig();
    this.initialized = true;
    return this.cachedConfig;
  }

  /**
   * Save configuration to ConfigMap
   */
  async setConfig(config: Partial<AppConfig>): Promise<AppConfig> {
    // Merge with existing config
    const currentConfig = await this.getConfig();
    const newConfig: AppConfig = {
      ...currentConfig,
      ...config,
    };

    try {
      // Ensure namespace exists
      await this.ensureNamespace();

      const configMapBody: k8s.V1ConfigMap = {
        metadata: {
          name: CONFIG_NAME,
          namespace: CONFIG_NAMESPACE,
          labels: {
            'app.kubernetes.io/name': 'airunway',
            'app.kubernetes.io/managed-by': 'airunway',
          },
        },
        data: {
          [CONFIG_KEY]: JSON.stringify(newConfig, null, 2),
        },
      };

      try {
        // Try to update existing ConfigMap
        await this.coreV1Api.replaceNamespacedConfigMap(
          CONFIG_NAME,
          CONFIG_NAMESPACE,
          configMapBody
        );
      } catch (error: unknown) {
        const k8sError = error as { response?: { statusCode?: number } };
        if (k8sError?.response?.statusCode === 404) {
          // Create new ConfigMap
          await this.coreV1Api.createNamespacedConfigMap(
            CONFIG_NAMESPACE,
            configMapBody
          );
        } else {
          throw error;
        }
      }

      // Update cache
      this.cachedConfig = newConfig;
      return newConfig;
    } catch (error) {
      logger.error({ error }, 'Error saving ConfigMap');
      throw new Error(`Failed to save configuration: ${error instanceof Error ? error.message : 'Unknown error'}`);
    }
  }

  /**
   * Get the default namespace for deployments.
   */
  async getDefaultNamespace(): Promise<string> {
    const config = await this.getConfig();
    return config.defaultNamespace || DEFAULT_AIRUNWAY_NAMESPACE;
  }

  /**
   * Clear the cached configuration
   * Useful for testing or forcing a refresh
   */
  clearCache(): void {
    this.cachedConfig = null;
    this.initialized = false;
  }
}

// Export singleton instance
export const configService = new ConfigService();
