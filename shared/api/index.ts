/**
 * Shared API Client for AIRunway
 *
 * This module provides a configurable API client that works in both
 * the main frontend (browser) and the Headlamp plugin environments.
 */

// Re-export client utilities
export { ApiError, createRequestFn } from './client';
export type { ApiClientConfig, RequestFn } from './client';

// Import API creators
import { createRequestFn, type ApiClientConfig } from './client';
import { createModelsApi, type ModelsApi } from './models';
import { createDeploymentsApi, type DeploymentsApi } from './deployments';
import { createHealthApi, type HealthApi } from './health';
import { createSettingsApi, type SettingsApi } from './settings';
import { createRuntimesApi, type RuntimesApi } from './runtimes';
import { createInstallationApi, type InstallationApi } from './installation';
import { createGpuOperatorApi, type GpuOperatorApi } from './gpu';
import { createGatewayApi, type GatewayApi } from './gateway';
import { createAutoscalerApi, type AutoscalerApi } from './autoscaler';
import { createHuggingFaceApi, type HuggingFaceApi } from './huggingface';
import { createAikitApi, type AikitApi } from './aikit';
import { createAIConfiguratorApi, type AIConfiguratorApi } from './aiconfigurator';
import { createMetricsApi, type MetricsApi } from './metrics';

// Re-export API types
export type { ModelsApi } from './models';
export type {
  DeploymentsApi,
  DeploymentListOptions,
  DeploymentLogsOptions,
  CreateDeploymentResponse,
  DeleteDeploymentResponse,
  DeploymentResource,
  PreviewResponse,
  ManifestResponse,
} from './deployments';
export type { HealthApi, HealthCheckResponse, ClusterNode } from './health';
export type {
  SettingsApi,
  UpdateSettingsRequest,
  UpdateSettingsResponse,
} from './settings';
export type { RuntimesApi } from './runtimes';
export type { InstallationApi, ProviderCommandsResponse } from './installation';
export type { GpuOperatorApi } from './gpu';
export type { GatewayApi } from './gateway';
export type { AutoscalerApi } from './autoscaler';
export type {
  HuggingFaceApi,
  HfOAuthConfig,
  HfSaveSecretResponse,
  HfDeleteSecretResponse,
  HfSearchOptions,
} from './huggingface';
export type {
  AikitApi,
  PremadeModel,
  AikitBuildRequest,
  AikitBuildResult,
  AikitPreviewResult,
  AikitInfrastructureStatus,
  AikitSetupResponse,
} from './aikit';
export type { AIConfiguratorApi, NormalizeGpuResponse } from './aiconfigurator';
export type { MetricsApi } from './metrics';

/**
 * Complete API client with all endpoints
 */
export interface ApiClient {
  models: ModelsApi;
  deployments: DeploymentsApi;
  health: HealthApi;
  settings: SettingsApi;
  runtimes: RuntimesApi;
  installation: InstallationApi;
  gpuOperator: GpuOperatorApi;
  gateway: GatewayApi;
  autoscaler: AutoscalerApi;
  huggingFace: HuggingFaceApi;
  aikit: AikitApi;
  aiConfigurator: AIConfiguratorApi;
  metrics: MetricsApi;
}

/**
 * Create a fully configured API client
 *
 * @param config - Configuration for the API client
 * @returns An object containing all API endpoints
 *
 * @example
 * ```typescript
 * // Browser environment
 * const client = createApiClient({
 *   baseUrl: '',  // Same origin
 *   getToken: () => localStorage.getItem('auth_token'),
 *   onUnauthorized: () => window.dispatchEvent(new CustomEvent('auth:unauthorized')),
 * });
 *
 * // Headlamp plugin environment
 * const client = createApiClient({
 *   baseUrl: 'http://airunway.airunway-system.svc:3001',
 *   getToken: () => getHeadlampToken(),
 * });
 *
 * // Use the client
 * const { deployments } = await client.deployments.list();
 * ```
 */
export function createApiClient(config: ApiClientConfig): ApiClient {
  const request = createRequestFn(config);

  return {
    models: createModelsApi(request),
    deployments: createDeploymentsApi(request),
    health: createHealthApi(request),
    settings: createSettingsApi(request),
    runtimes: createRuntimesApi(request),
    installation: createInstallationApi(request),
    gpuOperator: createGpuOperatorApi(request),
    gateway: createGatewayApi(request),
    autoscaler: createAutoscalerApi(request),
    huggingFace: createHuggingFaceApi(request),
    aikit: createAikitApi(request),
    aiConfigurator: createAIConfiguratorApi(request),
    metrics: createMetricsApi(request),
  };
}
