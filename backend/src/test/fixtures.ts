/**
 * Shared test fixtures for backend e2e tests.
 * Provides mock data for K8s resources used across route tests.
 */

import type {
  AutoscalerDetectionResult,
  AutoscalerStatusInfo,
  AIConfiguratorStatus,
  AIConfiguratorResult,
  AIConfiguratorConfig,
  DeploymentStatus,
  PodStatus,
  HfUserInfo,
  HfSecretStatus,
  HelmStatus,
  GPUOperatorStatus,
  ClusterGpuCapacity,
  DetailedClusterCapacity,
  PodFailureReason,
} from '@airunway/shared';
import type { HelmResult } from '../services/helm';

// ============================================================================
// Autoscaler Fixtures
// ============================================================================

export const autoscalerDetectionAKS: AutoscalerDetectionResult = {
  type: 'aks-managed',
  detected: true,
  healthy: true,
  message: 'AKS managed autoscaler detected',
  nodeGroupCount: 2,
};

export const autoscalerDetectionCA: AutoscalerDetectionResult = {
  type: 'cluster-autoscaler',
  detected: true,
  healthy: true,
  message: 'Cluster Autoscaler detected',
  nodeGroupCount: 3,
  lastActivity: new Date().toISOString(),
};

export const autoscalerDetectionNone: AutoscalerDetectionResult = {
  type: 'none',
  detected: false,
  healthy: false,
  message: 'No autoscaler detected',
};

export const autoscalerStatus: AutoscalerStatusInfo = {
  health: 'Healthy',
  lastUpdateTime: new Date().toISOString(),
  nodeGroups: [
    { name: 'gpu-pool', minSize: 0, maxSize: 5, currentSize: 2 },
    { name: 'cpu-pool', minSize: 1, maxSize: 10, currentSize: 3 },
  ],
};

// ============================================================================
// AI Configurator Fixtures
// ============================================================================

export const aiConfiguratorStatusAvailable: AIConfiguratorStatus = {
  available: true,
  version: '0.4.0',
};

export const aiConfiguratorStatusUnavailable: AIConfiguratorStatus = {
  available: false,
  error: 'AI Configurator CLI not found',
};

export const aiConfiguratorDefaultConfig: AIConfiguratorConfig = {
  tensorParallelDegree: 2,
  maxBatchSize: 256,
  gpuMemoryUtilization: 0.9,
  maxModelLen: 4096,
};

export const aiConfiguratorSuccessResult: AIConfiguratorResult = {
  success: true,
  config: aiConfiguratorDefaultConfig,
  mode: 'aggregated',
  replicas: 1,
  backend: 'vllm',
  supportedBackends: ['vllm', 'sglang', 'trtllm'],
};

// ============================================================================
// Deployment Fixtures
// ============================================================================

export const mockPod: PodStatus = {
  name: 'test-deploy-abc123',
  phase: 'Running',
  ready: true,
  restarts: 0,
  age: '2h',
  node: 'gpu-node-1',
};

export const mockPendingPod: PodStatus = {
  name: 'test-deploy-pending-xyz',
  phase: 'Pending',
  ready: false,
  restarts: 0,
  age: '5m',
};

export const mockDeployment: DeploymentStatus = {
  name: 'test-deploy',
  namespace: 'default',
  modelId: 'meta-llama/Llama-3.1-8B-Instruct',
  engine: 'vllm',
  status: 'Running',
  replicas: 1,
  readyReplicas: 1,
  pods: [mockPod],
  createdAt: new Date().toISOString(),
  mode: 'aggregated',
};

export const mockDeploymentWithPendingPod: DeploymentStatus = {
  ...mockDeployment,
  name: 'pending-deploy',
  status: 'Pending',
  readyReplicas: 0,
  pods: [mockPendingPod],
};

export const mockDeploymentManifest = {
  apiVersion: 'airunway.ai/v1alpha1',
  kind: 'ModelDeployment',
  metadata: {
    name: 'test-deploy',
    namespace: 'default',
  },
  spec: {
    model: { id: 'meta-llama/Llama-3.1-8B-Instruct', source: 'huggingface' },
    engine: { type: 'vllm' },
    resources: { gpu: { count: 1 } },
  },
};

// ============================================================================
// InferenceProviderConfig Fixtures
// ============================================================================

export const mockInferenceProviderConfig = {
  apiVersion: 'airunway.ai/v1alpha1',
  kind: 'InferenceProviderConfig',
  metadata: {
    name: 'kaito',
    annotations: {
      'airunway.ai/display-name': 'KAITO',
      'airunway.ai/description': 'KAITO - Kubernetes AI Toolchain Operator',
      'airunway.ai/default-namespace': 'kaito-workspace',
      'airunway.ai/documentation-url': 'https://github.com/kaito-project/airunway/tree/main/docs/providers/kaito.md',
      'airunway.ai/capabilities': JSON.stringify({
        engines: ['vllm', 'llamacpp'],
        servingModes: ['aggregated'],
      }),
      'airunway.ai/health': JSON.stringify({
        crds: [
          {
            name: 'workspaces.kaito.sh',
            displayName: 'KAITO Workspace CRD',
          },
        ],
        operatorPods: [
          {
            namespace: 'kaito-workspace',
            selectors: [
              'app.kubernetes.io/name=kaito',
              'app=kaito',
              'control-plane=controller-manager',
            ],
          },
        ],
      }),
      'airunway.ai/installation': JSON.stringify({
        description: 'KAITO - Kubernetes AI Toolchain Operator',
        defaultNamespace: 'kaito-workspace',
        helmRepos: [{ name: 'kaito', url: 'https://kaito-project.github.io/kaito/charts/kaito' }],
        helmCharts: [
          {
            name: 'workspace',
            chart: 'kaito/workspace',
            version: '0.10.0',
            namespace: 'kaito-workspace',
            createNamespace: true,
            preInstallMissingCrds: true,
            skipCrds: true,
          },
        ],
        steps: [{ title: 'Install KAITO', command: 'helm install kaito-workspace kaito/workspace', description: 'Install KAITO operator' }],
      }),
      'airunway.ai/documentation': 'https://github.com/kaito-project/airunway/tree/main/docs/providers/kaito.md',
    },
  },
  spec: {
    capabilities: {
      engines: [
        { name: 'vllm', servingModes: ['aggregated'], gpuSupport: true, cpuSupport: false },
        { name: 'llamacpp', servingModes: ['aggregated'], gpuSupport: true, cpuSupport: true },
      ],
    },
  },
  status: {
    ready: true,
    version: '0.10.0',
    lastHeartbeat: new Date().toISOString(),
  },
};

// ============================================================================
// Pod Failure Reasons Fixtures
// ============================================================================

export const mockPodFailureReasons: PodFailureReason[] = [
  {
    reason: 'Insufficient nvidia.com/gpu',
    message: 'No GPU resources available',
    isResourceConstraint: true,
    resourceType: 'gpu',
    canAutoscalerHelp: true,
  },
];

// ============================================================================
// HuggingFace OAuth Fixtures
// ============================================================================

export const mockHfUser: HfUserInfo = {
  id: 'user-123',
  name: 'testuser',
  fullname: 'Test User',
  email: 'test@example.com',
  avatarUrl: 'https://huggingface.co/avatars/testuser.png',
};

// ============================================================================
// HuggingFace Secrets Fixtures
// ============================================================================

export const mockHfSecretStatusConfigured: HfSecretStatus = {
  configured: true,
  namespaces: [
    { name: 'dynamo-system', exists: true },
    { name: 'ray-system', exists: true },
    { name: 'kuberay-system', exists: true },
    { name: 'kaito-workspace', exists: true },
    { name: 'default', exists: true },
  ],
  user: mockHfUser,
};

export const mockHfSecretStatusEmpty: HfSecretStatus = {
  configured: false,
  namespaces: [
    { name: 'dynamo-system', exists: false },
    { name: 'ray-system', exists: false },
    { name: 'kuberay-system', exists: false },
    { name: 'kaito-workspace', exists: false },
    { name: 'default', exists: false },
  ],
};

export const mockHfDistributeResult = {
  success: true,
  results: [
    { namespace: 'dynamo-system', success: true },
    { namespace: 'ray-system', success: true },
    { namespace: 'kuberay-system', success: true },
    { namespace: 'kaito-workspace', success: true },
    { namespace: 'default', success: true },
  ],
};

export const mockHfDeleteResult = {
  success: true,
  results: [
    { namespace: 'dynamo-system', success: true, deleted: true },
    { namespace: 'ray-system', success: true, deleted: true },
    { namespace: 'kuberay-system', success: true, deleted: true },
    { namespace: 'kaito-workspace', success: true, deleted: true },
    { namespace: 'default', success: true, deleted: true },
  ],
};

// ============================================================================
// GPU & Installation Fixtures
// ============================================================================

// Note: getClusterGpuCapacity() returns maxNodeGpuCapacity and gpuNodeCount at runtime
// even though the shared ClusterGpuCapacity type omits them. These fields are used in the
// frontend, so we include them here to match the actual API response shape.
export const mockGpuCapacity: ClusterGpuCapacity & { maxNodeGpuCapacity: number; gpuNodeCount: number } = {
  totalGpus: 4,
  allocatedGpus: 0,
  availableGpus: 4,
  maxContiguousAvailable: 4,
  maxNodeGpuCapacity: 4,
  gpuNodeCount: 1,
  nodes: [],
};

export const mockDetailedGpuCapacity: DetailedClusterCapacity = {
  totalGpus: 4,
  allocatedGpus: 1,
  availableGpus: 3,
  maxContiguousAvailable: 3,
  maxNodeGpuCapacity: 4,
  gpuNodeCount: 1,
  nodePools: [
    {
      name: 'gpu-node-1',
      gpuCount: 4,
      nodeCount: 1,
      availableGpus: 3,
      gpuModel: 'nvidia.com/gpu',
    },
  ],
};

export const mockGpuOperatorStatus: Omit<GPUOperatorStatus, 'helmCommands'> = {
  installed: true,
  crdFound: true,
  operatorRunning: true,
  gpusAvailable: true,
  totalGPUs: 4,
  gpuNodes: ['gpu-node-1'],
  message: 'GPUs enabled: 4 GPU(s) on 1 node(s)',
};

export const mockHelmAvailable: HelmStatus = {
  available: true,
  version: '3.14.0',
};

export const mockHelmUnavailable: HelmStatus = {
  available: false,
  error: 'Helm CLI not found in PATH',
};

export const mockProviderInstallResult = {
  success: true,
  results: [
    { step: 'repo-add-kaito', result: { success: true, stdout: 'repo added', stderr: '' } },
    { step: 'repo-update', result: { success: true, stdout: 'updated', stderr: '' } },
    { step: 'install-workspace', result: { success: true, stdout: 'installed', stderr: '' } },
  ],
};

export const mockProviderUninstallResult: HelmResult = {
  success: true,
  stdout: 'release "workspace" uninstalled',
  stderr: '',
  exitCode: 0,
};

export const mockInferenceProviderConfigNotReady = {
  ...mockInferenceProviderConfig,
  status: {
    ready: false,
    // No version — provider is not yet installed/ready
  },
};

// ============================================================================
// Provider health fixtures (issue #179)
// ============================================================================

const freshHeartbeat = new Date(Date.now() - 30_000).toISOString();
const staleHeartbeat = new Date(Date.now() - 5 * 60_000).toISOString();

export const mockKaitoCRNewShimHealthy = {
  ...mockInferenceProviderConfig,
  status: {
    ready: true,
    version: 'kaito-provider:v0.1.0',
    lastHeartbeat: freshHeartbeat,
    conditions: [
      { type: 'UpstreamReady', status: 'True', reason: 'UpstreamHealthy', message: 'KAITO workspace controller kaito-workspace/kaito-workspace is ready' },
    ],
  },
};

export const mockKaitoCROldShim = {
  ...mockInferenceProviderConfig,
  status: {
    ready: true,
    version: 'kaito-provider:v0.0.9',
    lastHeartbeat: freshHeartbeat,
    conditions: [],
  },
};

export const mockKaitoCRStale = {
  ...mockInferenceProviderConfig,
  status: {
    ready: true,
    version: 'kaito-provider:v0.1.0',
    lastHeartbeat: staleHeartbeat,
    conditions: [
      { type: 'UpstreamReady', status: 'True', reason: 'UpstreamHealthy', message: 'ok' },
    ],
  },
};
