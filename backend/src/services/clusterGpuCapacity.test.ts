import { describe, expect, test } from 'bun:test';
import type * as k8s from '@kubernetes/client-node';

import {
  detectGpuMemoryFromProduct,
  getAllNodePools,
  getClusterGpuCapacity,
  getDetailedClusterGpuCapacity,
  getNodePoolName,
  type ClusterGpuCapacity,
  type ClusterGpuCapacityAdapter,
} from './clusterGpuCapacity';

function node(input: {
  name: string;
  gpus?: number;
  labels?: Record<string, string>;
}): k8s.V1Node {
  return {
    metadata: { name: input.name, labels: input.labels },
    status: {
      allocatable: input.gpus === undefined ? {} : { 'nvidia.com/gpu': String(input.gpus) },
    },
  } as k8s.V1Node;
}

function pod(input: {
  phase: string;
  nodeName?: string;
  request?: number;
  limit?: number;
}): k8s.V1Pod {
  const resources: k8s.V1ResourceRequirements = {};
  if (input.request !== undefined) {
    resources.requests = { 'nvidia.com/gpu': String(input.request) };
  }
  if (input.limit !== undefined) {
    resources.limits = { 'nvidia.com/gpu': String(input.limit) };
  }

  return {
    status: { phase: input.phase },
    spec: {
      nodeName: input.nodeName,
      containers: [{ name: 'worker', resources }],
    },
  } as k8s.V1Pod;
}

function adapter(overrides: Partial<ClusterGpuCapacityAdapter>): ClusterGpuCapacityAdapter {
  return {
    listNodes: async () => [],
    listPodsForAllNamespaces: async () => [],
    getClusterGpuCapacity: async () => ({
      totalGpus: 0,
      allocatedGpus: 0,
      availableGpus: 0,
      maxContiguousAvailable: 0,
      maxNodeGpuCapacity: 0,
      gpuNodeCount: 0,
      nodes: [],
    }),
    logError: () => undefined,
    ...overrides,
  };
}

describe('clusterGpuCapacity Module', () => {
  test('summarizes allocatable GPUs, running/pending pod allocations, node max, and GPU memory', async () => {
    const capacity = await getClusterGpuCapacity(adapter({
      listNodes: async () => [
        node({
          name: 'gpu-a',
          gpus: 4,
          labels: { 'nvidia.com/gpu.memory': '81920' },
        }),
        node({ name: 'gpu-b', gpus: 8 }),
        node({ name: 'cpu-only' }),
      ],
      listPodsForAllNamespaces: async () => [
        pod({ phase: 'Running', nodeName: 'gpu-a', request: 2 }),
        pod({ phase: 'Pending', nodeName: 'gpu-b', limit: 3 }),
        pod({ phase: 'Succeeded', nodeName: 'gpu-b', request: 8 }),
        pod({ phase: 'Running', nodeName: 'missing-node', request: 4 }),
      ],
    }));

    expect(capacity).toEqual({
      totalGpus: 12,
      allocatedGpus: 5,
      availableGpus: 7,
      maxContiguousAvailable: 5,
      maxNodeGpuCapacity: 8,
      gpuNodeCount: 2,
      totalMemoryGb: 80,
      nodes: [
        { nodeName: 'gpu-a', totalGpus: 4, allocatedGpus: 2, availableGpus: 2 },
        { nodeName: 'gpu-b', totalGpus: 8, allocatedGpus: 3, availableGpus: 5 },
      ],
    });
  });

  test('groups detailed capacity by cloud node-pool labels and preserves the public basic-capacity seam', async () => {
    let basicCapacityCalls = 0;
    const basicCapacity: ClusterGpuCapacity = {
      totalGpus: 12,
      allocatedGpus: 5,
      availableGpus: 7,
      maxContiguousAvailable: 5,
      maxNodeGpuCapacity: 8,
      gpuNodeCount: 2,
      totalMemoryGb: 80,
      nodes: [
        { nodeName: 'aks-a', totalGpus: 4, allocatedGpus: 1, availableGpus: 3 },
        { nodeName: 'gke-b', totalGpus: 8, allocatedGpus: 4, availableGpus: 4 },
      ],
    };

    const detailed = await getDetailedClusterGpuCapacity(adapter({
      getClusterGpuCapacity: async () => {
        basicCapacityCalls += 1;
        return basicCapacity;
      },
      listNodes: async () => [
        node({
          name: 'aks-a',
          gpus: 4,
          labels: {
            agentpool: 'aks-gpu',
            'nvidia.com/gpu.product': 'NVIDIA-A100-SXM4-80GB',
            'node.kubernetes.io/instance-type': 'Standard_NC24ads_A100_v4',
            'topology.kubernetes.io/region': 'westus3',
          },
        }),
        node({
          name: 'gke-b',
          gpus: 8,
          labels: {
            'cloud.google.com/gke-nodepool': 'gke-gpu',
            'node.kubernetes.io/instance-type': 'a3-highgpu-8g',
          },
        }),
      ],
    }));

    expect(basicCapacityCalls).toBe(1);
    expect(detailed.nodePools).toEqual([
      {
        name: 'aks-gpu',
        gpuCount: 4,
        nodeCount: 1,
        availableGpus: 3,
        gpuModel: 'NVIDIA-A100-SXM4-80GB',
        instanceType: 'Standard_NC24ads_A100_v4',
        region: 'westus3',
      },
      {
        name: 'gke-gpu',
        gpuCount: 8,
        nodeCount: 1,
        availableGpus: 4,
        gpuModel: 'H100',
        instanceType: 'a3-highgpu-8g',
        region: undefined,
      },
    ]);
  });

  test('lists CPU and GPU node pools for cost estimation', async () => {
    const nodePools = await getAllNodePools(adapter({
      listNodes: async () => [
        node({
          name: 'cpu-a',
          labels: {
            'eks.amazonaws.com/nodegroup': 'cpu-pool',
            'node.kubernetes.io/instance-type': 'm6i.4xlarge',
          },
        }),
        node({
          name: 'gpu-a',
          gpus: 1,
          labels: {
            'eks.amazonaws.com/nodegroup': 'gpu-pool',
            'node.kubernetes.io/instance-type': 'g5.xlarge',
          },
        }),
      ],
    }));

    expect(nodePools).toEqual([
      {
        name: 'cpu-pool',
        gpuCount: 0,
        nodeCount: 1,
        availableGpus: 0,
        gpuModel: undefined,
        instanceType: 'm6i.4xlarge',
        region: undefined,
      },
      {
        name: 'gpu-pool',
        gpuCount: 1,
        nodeCount: 1,
        availableGpus: 0,
        gpuModel: 'A10G',
        instanceType: 'g5.xlarge',
        region: undefined,
      },
    ]);
  });

  test('keeps GPU memory and node-pool label helpers at the module interface', () => {
    expect(detectGpuMemoryFromProduct('NVIDIA-H200')).toBe(141);
    expect(detectGpuMemoryFromProduct('GeForce-RTX-3080-12GB')).toBe(12);
    expect(detectGpuMemoryFromProduct('AMD-MI250X')).toBeUndefined();

    expect(getNodePoolName({
      agentpool: 'aks-pool',
      'cloud.google.com/gke-nodepool': 'gke-pool',
    })).toBe('aks-pool');
    expect(getNodePoolName({ 'eks.amazonaws.com/nodegroup': 'eks-gpu' })).toBe('eks-gpu');
    expect(getNodePoolName(undefined)).toBe('default');
  });
});
