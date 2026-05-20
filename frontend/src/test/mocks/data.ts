/**
 * Shared mock data used by both MSW handlers (Vitest) and Playwright fixtures (e2e).
 *
 * This is the SINGLE SOURCE OF TRUTH for all test mock data.
 * Do not duplicate these constants in handlers.ts or fixtures.ts.
 */

export const mockModels = [
  {
    id: 'Qwen/Qwen3-0.6B',
    name: 'Qwen3-0.6B',
    description: 'Small but capable Qwen model',
    size: '0.6B',
    task: 'chat' as const,
    parameters: 600_000_000,
    contextLength: 8192,
    license: 'Apache 2.0',
    supportedEngines: ['vllm', 'sglang', 'trtllm'] as const,
    minGpuMemory: '4GB',
    gated: false,
  },
  {
    id: 'meta-llama/Llama-3.2-1B-Instruct',
    name: 'Llama-3.2-1B-Instruct',
    description: 'Instruction-tuned Llama model',
    size: '1B',
    task: 'chat' as const,
    parameters: 1_000_000_000,
    contextLength: 4096,
    license: 'Meta Llama License',
    supportedEngines: ['vllm', 'sglang', 'trtllm'] as const,
    minGpuMemory: '8GB',
    gated: true,
  },
]

export const mockDeployments = [
  {
    name: 'qwen3-0-6b-vllm-abc123',
    namespace: 'airunway-system',
    modelId: 'Qwen/Qwen3-0.6B',
    engine: 'vllm' as const,
    mode: 'aggregated' as const,
    phase: 'Running' as const,
    replicas: { desired: 1, ready: 1, available: 1 },
    pods: [
      {
        name: 'qwen3-0-6b-vllm-abc123-worker-0',
        phase: 'Running' as const,
        ready: true,
        restarts: 0,
        node: 'gpu-node-1',
      },
    ],
    createdAt: '2025-01-15T10:30:00.000Z',
    frontendService: 'qwen3-0-6b-vllm-abc123-frontend:8000',
  },
  {
    name: 'llama-1b-pending-def456',
    namespace: 'airunway-system',
    modelId: 'meta-llama/Llama-3.2-1B-Instruct',
    engine: 'sglang' as const,
    mode: 'aggregated' as const,
    phase: 'Pending' as const,
    replicas: { desired: 1, ready: 0, available: 0 },
    pods: [],
    createdAt: '2025-01-15T11:00:00.000Z',
  },
]

export const mockSettings = {
  config: { defaultNamespace: 'airunway-system' },
  auth: { enabled: false },
  providers: [
    {
      id: 'runtime-a',
      name: 'Primary Runtime',
      description: 'General-purpose runtime for standard workloads',
      defaultNamespace: 'runtime-a-system',
    },
    {
      id: 'runtime-b',
      name: 'Distributed Runtime',
      description: 'Runtime for larger distributed workloads',
      defaultNamespace: 'runtime-b-system',
    },
    {
      id: 'runtime-c',
      name: 'Flexible Runtime',
      description: 'Runtime with multiple deployment styles',
      defaultNamespace: 'runtime-c-system',
    },
  ],
}

export const mockClusterStatus = {
  connected: true,
  namespace: 'airunway-system',
  clusterName: 'test-cluster',
  provider: { id: 'runtime-a', name: 'Primary Runtime' },
  providerInstallation: {
    installed: true,
    version: '1.0.0',
    crdFound: true,
    operatorRunning: true,
  },
}

export const mockRuntimesStatus = {
  runtimes: [
    {
      id: 'runtime-a',
      name: 'Primary Runtime',
      installed: true,
      version: '1.0.0',
      crdFound: true,
      operatorRunning: true,
    },
  ],
}

export const mockGpuCapacity = {
  totalGpus: 4,
  allocatedGpus: 2,
  availableGpus: 2,
  maxContiguousAvailable: 2,
  maxNodeGpuCapacity: 2,
  gpuNodeCount: 2,
  totalMemoryGb: 80,
  nodePools: [
    { name: 'gpu-pool', gpuType: 'NVIDIA A100', gpuCount: 2, nodeCount: 2, availableGpus: 2, totalMemoryGb: 80 },
  ],
}
