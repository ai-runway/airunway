// API Base URL - when not specified, use relative URL (same origin)
// This allows the frontend to work both in development (with VITE_API_URL=http://localhost:3001)
// and in production (served from the same container as the backend)
const API_BASE = import.meta.env.VITE_API_URL || '';

console.log('[API] API_BASE:', API_BASE || '(same origin)');

// Auth token storage key
const AUTH_TOKEN_KEY = 'airunway_auth_token';
const AIRUNWAY_AUTH_ERROR_HEADER = 'X-Airunway-Auth-Error';

/**
 * Get the stored auth token
 */
function getAuthToken(): string | null {
  try {
    return localStorage.getItem(AUTH_TOKEN_KEY);
  } catch {
    return null;
  }
}

/**
 * Dispatch unauthorized event to trigger logout
 */
function dispatchUnauthorized(): void {
  window.dispatchEvent(new CustomEvent('auth:unauthorized'));
}

function isAuthenticationError(response: Response): boolean {
  return response.status === 401 &&
    response.headers.get(AIRUNWAY_AUTH_ERROR_HEADER)?.toLowerCase() === 'true';
}

// ============================================================================
// Re-export types from @airunway/shared
// ============================================================================

// Core types
export type {
  Engine,
  ModelTask,
  Model,
  DeploymentMode,
  RouterMode,
  DeploymentPhase,
  PodPhase,
  GgufRunMode,
  KaitoResourceType,
  DeploymentConfig,
  PodStatus,
  DeploymentStatus,
  ClusterStatus,
  GatewayStatus,
  GatewayInfo,
  GatewayModelInfo,
  StorageVolume,
  StorageSpec,
  VolumePurpose,
  PersistentVolumeAccessMode,
  VllmRecipeIndexEntry,
  VllmRecipeListResponse,
  VllmRecipeRawResponse,
  VllmRecipeResolveRequest,
  VllmRecipeResolveResult,
} from '@airunway/shared';

// Settings types
export type {
  ProviderInfo,
  ProviderDetails,
  Settings,
  RuntimeStatus,
  RuntimesStatusResponse,
} from '@airunway/shared';

// Installation types
export type {
  HelmStatus,
  InstallationStatus,
  InstallResult,
  GPUOperatorStatus,
  GPUOperatorInstallResult,
  NodeGpuInfo,
  ClusterGpuCapacity,
  GatewayCRDStatus,
  GatewayCRDInstallResult,
} from '@airunway/shared';

// HuggingFace types
export type {
  HfUserInfo,
  HfTokenExchangeRequest,
  HfTokenExchangeResponse,
  HfSaveSecretRequest,
  HfSecretStatus,
  HfModelSearchResult,
  HfModelSearchResponse,
  HfSearchParams,
} from '@airunway/shared';

// API response types
export type {
  Pagination,
  DeploymentsListResponse,
  ClusterStatusResponse,
} from '@airunway/shared';

// Metrics types
export type {
  MetricsResponse,
  RawMetricValue,
  ComputedMetric,
  ComputedMetrics,
  MetricDefinition,
} from '@airunway/shared';

// Autoscaler types
export type {
  AutoscalerDetectionResult,
  AutoscalerStatusInfo,
  DetailedClusterCapacity,
  GpuThroughputEstimate,
  NodePoolInfo,
  PodFailureReason,
  PodLogsOptions,
  PodLogsResponse,
} from '@airunway/shared';

// AIKit types
export type {
  PremadeModel,
  AikitBuildRequest,
  AikitBuildResult,
  AikitPreviewResult,
  AikitInfrastructureStatus,
} from '@airunway/shared';

// Import types for internal use
import type {
  Model,
  DeploymentConfig,
  DeploymentStatus,
  PodStatus,
  Settings,
  ProviderInfo,
  ProviderDetails,
  HelmStatus,
  InstallationStatus,
  InstallResult,
  GPUOperatorStatus,
  GPUOperatorInstallResult,
  ClusterGpuCapacity,
  GatewayCRDStatus,
  GatewayCRDInstallResult,
  DeploymentsListResponse,
  ClusterStatusResponse,
  MetricsResponse,
  HfTokenExchangeRequest,
  HfTokenExchangeResponse,
  HfSaveSecretRequest,
  HfSecretStatus,
  HfUserInfo,
  HfModelSearchResponse,
  AutoscalerDetectionResult,
  AutoscalerStatusInfo,
  DetailedClusterCapacity,
  GpuThroughputEstimate,
  PodFailureReason,
  RuntimesStatusResponse,
  PodLogsResponse,
  PremadeModel,
  AikitBuildRequest,
  AikitBuildResult,
  AikitPreviewResult,
  AikitInfrastructureStatus,
  VllmRecipeListResponse,
  VllmRecipeRawResponse,
  VllmRecipeResolveRequest,
  VllmRecipeResolveResult,
} from '@airunway/shared';

// ============================================================================
// Error Handling
// ============================================================================

class ApiError extends Error {
  constructor(public statusCode: number, message: string) {
    super(message);
    this.name = 'ApiError';
  }
}

// Extended options that include timeout
interface RequestOptions extends RequestInit {
  timeoutMs?: number;
}

// Default timeout for most requests (30 seconds)
const DEFAULT_TIMEOUT_MS = 30000;

// Longer timeout for installation operations (10 minutes - Helm can be slow)
const INSTALLATION_TIMEOUT_MS = 600000;

// Detect test environment - disable timeout for tests as it can interfere with MSW
// Check for common test environment indicators
const isTestEnv = typeof import.meta !== 'undefined' &&
  ((import.meta as { env?: { MODE?: string } }).env?.MODE === 'test' ||
   (import.meta as { env?: { VITEST?: string } }).env?.VITEST === 'true');

async function request<T>(endpoint: string, options?: RequestOptions): Promise<T> {
  const url = `${API_BASE}/api${endpoint}`;
  console.log('[API] Fetching:', url);

  // Build headers with auth token if available
  const headers: HeadersInit = {
    'Content-Type': 'application/json',
    ...(options?.headers || {}),
  };

  const token = getAuthToken();
  if (token) {
    (headers as Record<string, string>)['Authorization'] = `Bearer ${token}`;
  }

  // Use AbortController for timeout (disabled in test environment)
  const timeoutMs = options?.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const controller = new AbortController();
  const timeoutId = isTestEnv ? null : setTimeout(() => controller.abort(), timeoutMs);

  let response: Response;
  try {
    response = await fetch(url, {
      ...options,
      headers,
      signal: isTestEnv ? undefined : controller.signal,
    });
  } catch (error) {
    if (timeoutId) clearTimeout(timeoutId);
    if (error instanceof Error && error.name === 'AbortError') {
      throw new ApiError(408, `Request timeout after ${timeoutMs / 1000} seconds`);
    }
    throw error;
  } finally {
    if (timeoutId) clearTimeout(timeoutId);
  }

  console.log('[API] Response status:', response.status, 'for', url);

  if (!response.ok) {
    // Handle Airunway auth failures - dispatch event to trigger logout.
    // Upstream/model 401s are application errors and should not log out the user.
    if (isAuthenticationError(response)) {
      console.warn('[API] Unauthorized - dispatching auth:unauthorized event');
      dispatchUnauthorized();
    }

    // Try to parse error response body
    let errorMessage: string;
    try {
      const error = await response.json();
      errorMessage = error.error?.message || error.message || `Request failed with status ${response.status}`;
    } catch {
      // Response body is empty or not valid JSON
      errorMessage = `Request failed with status ${response.status}: ${response.statusText || 'No response body'}`;
    }

    console.error('[API] Error response:', errorMessage);
    throw new ApiError(response.status, errorMessage);
  }

  return response.json();
}

// ============================================================================
// Models API
// ============================================================================

export const modelsApi = {
  list: () => request<{ models: Model[] }>('/models'),
  get: (id: string) => request<Model>(`/models/${encodeURIComponent(id)}`),
};


// ============================================================================
// vLLM Recipes API
// ============================================================================

export const vllmRecipesApi = {
  /** List known upstream vLLM deployment recipes */
  list: () => request<VllmRecipeListResponse>('/vllm/recipes'),

  /** Resolve a recipe into deployment-ready engine args, image, resources, and provenance */
  resolve: (data: VllmRecipeResolveRequest) =>
    request<VllmRecipeResolveResult>('/vllm/recipes/resolve', {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  /** Fetch a raw recipe payload for a Hugging Face organization/model pair */
  get: (org: string, model: string) =>
    request<VllmRecipeRawResponse>(
      `/vllm/recipes/${encodeURIComponent(org)}/${encodeURIComponent(model)}`
    ),
};

// ============================================================================
// Deployments API
// ============================================================================

export interface ChatMessage {
  role: string;
  content: unknown;
}

export interface ChatCompletionRequest {
  messages: ChatMessage[];
  model?: string;
  temperature?: number;
  max_tokens?: number;
  max_completion_tokens?: number;
  top_p?: number;
  [key: string]: unknown;
}

export const deploymentsApi = {
  list: (namespace?: string, options?: { limit?: number; offset?: number }) => {
    const params = new URLSearchParams();
    if (namespace) params.set('namespace', namespace);
    if (options?.limit) params.set('limit', options.limit.toString());
    if (options?.offset) params.set('offset', options.offset.toString());
    const query = params.toString();
    return request<DeploymentsListResponse>(`/deployments${query ? `?${query}` : ''}`);
  },

  get: (name: string, namespace?: string) =>
    request<DeploymentStatus>(
      `/deployments/${encodeURIComponent(name)}${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`
    ),

  create: (config: DeploymentConfig) =>
    request<{ message: string; name: string; namespace: string; warnings?: string[] }>('/deployments', {
      method: 'POST',
      body: JSON.stringify(config),
    }),

  preview: (config: DeploymentConfig) =>
    request<{
      resources: Array<{
        kind: string;
        apiVersion: string;
        name: string;
        manifest: Record<string, unknown>;
      }>;
      primaryResource: { kind: string; apiVersion: string };
    }>('/deployments/preview', {
      method: 'POST',
      body: JSON.stringify(config),
    }),

  delete: (name: string, namespace?: string) =>
    request<{ message: string }>(
      `/deployments/${encodeURIComponent(name)}${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`,
      { method: 'DELETE' }
    ),

  getPods: (name: string, namespace?: string) =>
    request<{ pods: PodStatus[] }>(
      `/deployments/${encodeURIComponent(name)}/pods${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`
    ),

  getMetrics: (name: string, namespace?: string) =>
    request<MetricsResponse>(
      `/deployments/${encodeURIComponent(name)}/metrics${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`
    ),

  getLogs: (name: string, namespace?: string, options?: { podName?: string; container?: string; tailLines?: number; timestamps?: boolean }) => {
    const params = new URLSearchParams();
    if (namespace) params.set('namespace', namespace);
    if (options?.podName) params.set('podName', options.podName);
    if (options?.container) params.set('container', options.container);
    if (options?.tailLines) params.set('tailLines', options.tailLines.toString());
    if (options?.timestamps) params.set('timestamps', 'true');
    const query = params.toString();
    return request<PodLogsResponse>(
      `/deployments/${encodeURIComponent(name)}/logs${query ? `?${query}` : ''}`
    );
  },

  chat: async (name: string, payload: ChatCompletionRequest, namespace?: string, options?: { signal?: AbortSignal }): Promise<Response> => {
    const url = namespace
      ? `${API_BASE}/api/deployments/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/chat`
      : `${API_BASE}/api/deployments/${encodeURIComponent(name)}/chat`;

    const headers = new Headers({ 'Content-Type': 'application/json' });
    const token = getAuthToken();
    if (token) {
      headers.set('Authorization', `Bearer ${token}`);
    }

    const response = await fetch(url, {
      method: 'POST',
      headers,
      body: JSON.stringify(payload),
      signal: options?.signal,
    });

    if (isAuthenticationError(response)) {
      console.warn('[API] Unauthorized chat response - dispatching auth:unauthorized event');
      dispatchUnauthorized();
    }

    return response;
  },

  getManifest: (name: string, namespace?: string) =>
    request<{
      resources: Array<{
        kind: string;
        apiVersion: string;
        name: string;
        manifest: Record<string, unknown>;
      }>;
      primaryResource: {
        kind: string;
        apiVersion: string;
      };
    }>(
      `/deployments/${encodeURIComponent(name)}/manifest${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`
    ),

  listPVCs: (namespace: string) =>
    request<{ pvcs: Array<{ name: string; status: string; storageClass: string; capacity: string }> }>(
      `/deployments/-/pvcs?namespace=${encodeURIComponent(namespace)}`
    ),
};

// ============================================================================
// Metrics API
// ============================================================================

export const metricsApi = {
  get: (deploymentName: string, namespace?: string) =>
    request<MetricsResponse>(
      `/deployments/${encodeURIComponent(deploymentName)}/metrics${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`
    ),
};

// ============================================================================
// Health API
// ============================================================================

export interface ClusterNode {
  name: string;
  ready: boolean;
  gpuCount: number;
}

export const healthApi = {
  check: () => request<{ status: string; timestamp: string }>('/health'),
  clusterStatus: () => request<ClusterStatusResponse>('/cluster/status'),
  getClusterNodes: () => request<{ nodes: ClusterNode[] }>('/cluster/nodes'),
};

// ============================================================================
// Settings API
// ============================================================================

export const settingsApi = {
  get: () => request<Settings>('/settings'),
  update: (settings: { defaultNamespace?: string }) =>
    request<{ message: string; config: Settings['config'] }>('/settings', {
      method: 'PUT',
      body: JSON.stringify(settings),
    }),
};

// ============================================================================
// Providers API
// ============================================================================

export const providersApi = {
  list: () => request<{ providers: ProviderInfo[] }>('/providers'),
  get: (id: string) => request<ProviderDetails>(`/providers/${encodeURIComponent(id)}`),
};

// ============================================================================
// Runtimes API
// ============================================================================

export const runtimesApi = {
  /** Get status of all runtimes (installation and health) */
  getStatus: () => request<RuntimesStatusResponse>('/runtimes/status'),
};

// ============================================================================
// Installation API
// ============================================================================

export const installationApi = {
  getHelmStatus: () => request<HelmStatus>('/installation/helm/status'),

  getProviderStatus: (providerId: string) =>
    request<InstallationStatus>(`/installation/providers/${encodeURIComponent(providerId)}/status`),

  getProviderCommands: (providerId: string) =>
    request<{
      providerId: string;
      providerName: string;
      commands: string[];
      steps: Array<{ title: string; command?: string; description: string }>;
    }>(`/installation/providers/${encodeURIComponent(providerId)}/commands`),

  installProvider: (providerId: string) =>
    request<InstallResult>(`/installation/providers/${encodeURIComponent(providerId)}/install`, {
      method: 'POST',
      timeoutMs: INSTALLATION_TIMEOUT_MS,
    }),

  upgradeProvider: (providerId: string) =>
    request<InstallResult>(`/installation/providers/${encodeURIComponent(providerId)}/upgrade`, {
      method: 'POST',
      timeoutMs: INSTALLATION_TIMEOUT_MS,
    }),

  uninstallProvider: (providerId: string) =>
    request<InstallResult>(`/installation/providers/${encodeURIComponent(providerId)}/uninstall`, {
      method: 'POST',
      timeoutMs: INSTALLATION_TIMEOUT_MS,
    }),

  uninstallProviderCRDs: (providerId: string) =>
    request<InstallResult>(`/installation/providers/${encodeURIComponent(providerId)}/uninstall-crds`, {
      method: 'POST',
      timeoutMs: INSTALLATION_TIMEOUT_MS,
    }),

  getGatewayCRDStatus: () => request<GatewayCRDStatus>('/installation/gateway/status'),

  installGatewayCRDs: () =>
    request<GatewayCRDInstallResult>('/installation/gateway/install-crds', {
      method: 'POST',
      timeoutMs: INSTALLATION_TIMEOUT_MS,
    }),
};

// ============================================================================
// GPU Operator API
// ============================================================================

export const gpuOperatorApi = {
  getStatus: () => request<GPUOperatorStatus>('/installation/gpu-operator/status'),

  install: () =>
    request<GPUOperatorInstallResult>('/installation/gpu-operator/install', {
      method: 'POST',
      timeoutMs: INSTALLATION_TIMEOUT_MS,
    }),

  getCapacity: () => request<ClusterGpuCapacity>('/installation/gpu-capacity'),

  getDetailedCapacity: () => request<DetailedClusterCapacity>('/installation/gpu-capacity/detailed'),

  /** Estimate inference throughput (per-chat speed + concurrent capacity) for a model on the cluster's GPUs */
  getThroughput: (
    params: {
      modelId?: string;
      paramCount?: number;
      contextLen?: number;
      quantization?: 'fp8' | 'int8' | 'fp16' | 'bf16';
      kvCacheDtype?: 'fp8' | 'int8' | 'fp16' | 'bf16';
      gpuModel?: string;
      tpSize?: number;
    },
    hfToken?: string
  ) => {
    const search = new URLSearchParams();
    if (params.modelId) search.set('modelId', params.modelId);
    if (params.paramCount) search.set('paramCount', String(params.paramCount));
    if (params.contextLen) search.set('contextLen', String(params.contextLen));
    if (params.quantization) search.set('quantization', params.quantization);
    if (params.kvCacheDtype) search.set('kvCacheDtype', params.kvCacheDtype);
    if (params.gpuModel) search.set('gpuModel', params.gpuModel);
    if (params.tpSize) search.set('tpSize', String(params.tpSize));

    const headers: Record<string, string> = {};
    if (hfToken) {
      headers['X-HF-Token'] = hfToken;
    }
    return request<GpuThroughputEstimate>(`/installation/gpu-throughput?${search.toString()}`, {
      headers,
    });
  },
};

// ============================================================================
// Autoscaler API
// ============================================================================

export const autoscalerApi = {
  /** Detect autoscaler type and health status */
  detect: () => request<AutoscalerDetectionResult>('/autoscaler/detection'),

  /** Get detailed autoscaler status from ConfigMap */
  getStatus: () => request<AutoscalerStatusInfo>('/autoscaler/status'),

  /** Get reasons why a deployment's pods are pending */
  getPendingReasons: (deploymentName: string, namespace?: string) =>
    request<{ reasons: PodFailureReason[] }>(
      `/deployments/${encodeURIComponent(deploymentName)}/pending-reasons${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`
    ),
};

// ============================================================================
// HuggingFace OAuth API
// ============================================================================

export const huggingFaceApi = {
  /** Get OAuth configuration (client ID, scopes) */
  getOAuthConfig: () =>
    request<{
      clientId: string;
      authorizeUrl: string;
      scopes: string[];
    }>('/oauth/huggingface/config'),

  /** Exchange authorization code for access token */
  exchangeToken: (data: HfTokenExchangeRequest) =>
    request<HfTokenExchangeResponse>('/oauth/huggingface/token', {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  /** Get status of HuggingFace secret across namespaces */
  getSecretStatus: () => request<HfSecretStatus>('/secrets/huggingface/status'),

  /** Save HuggingFace token as K8s secrets */
  saveSecret: (data: HfSaveSecretRequest) =>
    request<{
      success: boolean;
      message: string;
      user?: HfUserInfo;
      results: { namespace: string; success: boolean; error?: string }[];
    }>('/secrets/huggingface', {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  /** Delete HuggingFace secrets from all namespaces */
  deleteSecret: () =>
    request<{
      success: boolean;
      message: string;
      results: { namespace: string; success: boolean; error?: string }[];
    }>('/secrets/huggingface', {
      method: 'DELETE',
    }),

  /** Search HuggingFace models with compatibility filtering */
  searchModels: (query: string, options?: { limit?: number; offset?: number; hfToken?: string }) => {
    const params = new URLSearchParams({
      q: query,
    });
    if (options?.limit) params.set('limit', options.limit.toString());
    if (options?.offset) params.set('offset', options.offset.toString());

    // Build headers - include HF token via dedicated header (not Authorization, which is for cluster auth)
    const headers: Record<string, string> = {};
    if (options?.hfToken) {
      headers['X-HF-Token'] = options.hfToken;
    }

    return request<HfModelSearchResponse>(`/models/search?${params.toString()}`, {
      headers,
    });
  },

  /** Get GGUF files available in a HuggingFace repository */
  getGgufFiles: (modelId: string, hfToken?: string) => {
    const headers: Record<string, string> = {};
    if (hfToken) {
      headers['X-HF-Token'] = hfToken;
    }
    return request<{ files: string[] }>(`/models/${encodeURIComponent(modelId)}/gguf-files`, {
      headers,
    });
  },
};

// ============================================================================
// AIKit API (KAITO/GGUF Models)
// ============================================================================

export const aikitApi = {
  /** List available premade KAITO models */
  listModels: () =>
    request<{ models: PremadeModel[]; total: number }>('/aikit/models'),

  /** Get a specific premade model by ID */
  getModel: (id: string) =>
    request<PremadeModel>(`/aikit/models/${encodeURIComponent(id)}`),

  /** Build an AIKit image (premade returns immediately, HuggingFace triggers build) */
  build: (req: AikitBuildRequest) =>
    request<AikitBuildResult>('/aikit/build', {
      method: 'POST',
      body: JSON.stringify(req),
    }),

  /** Preview what image would be built without actually building */
  preview: (req: AikitBuildRequest) =>
    request<AikitPreviewResult>('/aikit/build/preview', {
      method: 'POST',
      body: JSON.stringify(req),
    }),

  /** Get build infrastructure status (registry + BuildKit) */
  getInfrastructureStatus: () =>
    request<AikitInfrastructureStatus>('/aikit/infrastructure/status'),

  /** Set up build infrastructure (registry + BuildKit) */
  setupInfrastructure: () =>
    request<{
      success: boolean;
      message: string;
      registry: { url: string; ready: boolean };
      builder: { name: string; ready: boolean };
    }>('/aikit/infrastructure/setup', {
      method: 'POST',
      timeoutMs: INSTALLATION_TIMEOUT_MS,
    }),
};

// ============================================================================
// AI Configurator API
// ============================================================================

// Re-export AI Configurator types from shared
export type {
  AIConfiguratorInput,
  AIConfiguratorResult,
  AIConfiguratorStatus,
  AIConfiguratorConfig,
  AIConfiguratorPerformance,
} from '@airunway/shared';

// Import types for internal use
import type {
  AIConfiguratorInput,
  AIConfiguratorResult,
  AIConfiguratorStatus,
} from '@airunway/shared';

export const aiConfiguratorApi = {
  /** Check if AI Configurator is available */
  getStatus: () => request<AIConfiguratorStatus>('/aiconfigurator/status'),

  /** Analyze model + GPU and get optimal configuration */
  analyze: (input: AIConfiguratorInput) =>
    request<AIConfiguratorResult>('/aiconfigurator/analyze', {
      method: 'POST',
      body: JSON.stringify(input),
    }),

  /** Normalize GPU product string to AI Configurator format */
  normalizeGpu: (gpuProduct: string) =>
    request<{ gpuProduct: string; normalized: string }>('/aiconfigurator/normalize-gpu', {
      method: 'POST',
      body: JSON.stringify({ gpuProduct }),
    }),
};

// ============================================================================
// Cost Estimation API
// ============================================================================

// Re-export Cost Estimation types from shared
export type {
  CostBreakdown,
  CostEstimate,
  CostEstimateRequest,
  CostEstimateResponse,
  NodePoolCostEstimate,
  RealtimePricing,
  GpuPricing,
  CloudProvider,
} from '@airunway/shared';

// Import types for internal use
import type {
  CostEstimateRequest,
  CostEstimateResponse,
  NodePoolCostEstimate,
} from '@airunway/shared';

// Import gateway types for internal use
import type {
  GatewayInfo,
  GatewayModelInfo,
} from '@airunway/shared';

export const costsApi = {
  /** Estimate deployment cost based on GPU configuration */
  estimate: (input: CostEstimateRequest) =>
    request<CostEstimateResponse>('/costs/estimate', {
      method: 'POST',
      body: JSON.stringify(input),
    }),

  /** Get cost estimates for all node pools in the cluster */
  getNodePoolCosts: (gpuCount: number = 1, replicas: number = 1, computeType: 'gpu' | 'cpu' = 'gpu') =>
    request<{
      success: boolean;
      nodePoolCosts: NodePoolCostEstimate[];
      pricingSource: 'realtime-with-fallback' | 'static';
      cacheStats: {
        size: number;
        ttlMs: number;
        maxEntries: number;
      };
    }>(`/costs/node-pools?gpuCount=${gpuCount}&replicas=${replicas}&computeType=${computeType}`),

  /** Get list of supported GPU models with specifications */
  getGpuModels: () =>
    request<{
      success: boolean;
      models: Array<{
        model: string;
        memoryGb: number;
        generation: string;
      }>;
      note: string;
    }>('/costs/gpu-models'),

  /** Normalize a GPU model name to our pricing key */
  normalizeGpu: (label: string) =>
    request<{
      success: boolean;
      originalLabel: string;
      normalizedModel: string;
      pricing: {
        memoryGb: number;
        generation: string;
        hourlyRate: { aws?: number; azure?: number; gcp?: number };
      } | null;
    }>(`/costs/normalize-gpu?label=${encodeURIComponent(label)}`),
};

// ============================================================================
// Gateway API
// ============================================================================

export const gatewayApi = {
  /** Get gateway readiness and endpoint URL */
  getStatus: () => request<GatewayInfo>('/gateway/status'),

  /** List all models accessible through the gateway */
  getModels: () => request<{ models: GatewayModelInfo[] }>('/gateway/models'),
};
