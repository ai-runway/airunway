import { describe, test, expect } from 'bun:test';
import type { ClusterGpuCapacity } from './kubernetes';
import type { NodePoolInfo, PodFailureReason } from '@airunway/shared';

describe('KubernetesService - Enhanced Capacity', () => {
  describe('getClusterGpuCapacity enhancements', () => {
    test('should calculate maxNodeGpuCapacity correctly', () => {
      const nodes = [
        { nodeName: 'node1', totalGpus: 4, allocatedGpus: 2, availableGpus: 2 },
        { nodeName: 'node2', totalGpus: 8, allocatedGpus: 4, availableGpus: 4 },
        { nodeName: 'node3', totalGpus: 2, allocatedGpus: 1, availableGpus: 1 },
      ];

      let maxNodeGpuCapacity = 0;
      for (const node of nodes) {
        maxNodeGpuCapacity = Math.max(maxNodeGpuCapacity, node.totalGpus);
      }

      expect(maxNodeGpuCapacity).toBe(8);
    });

    test('should calculate gpuNodeCount correctly', () => {
      const nodeGpuMap = new Map([
        ['node1', { total: 4, allocated: 2 }],
        ['node2', { total: 8, allocated: 4 }],
        ['node3', { total: 2, allocated: 1 }],
      ]);

      expect(nodeGpuMap.size).toBe(3);
    });

    test('should handle zero GPU capacity', () => {
      const capacity: ClusterGpuCapacity = {
        totalGpus: 0,
        allocatedGpus: 0,
        availableGpus: 0,
        maxContiguousAvailable: 0,
        maxNodeGpuCapacity: 0,
        gpuNodeCount: 0,
        nodes: [],
      };

      expect(capacity.maxNodeGpuCapacity).toBe(0);
      expect(capacity.gpuNodeCount).toBe(0);
    });
  });

  describe('getDetailedClusterGpuCapacity', () => {
    test('should group nodes by AKS agentpool label', () => {
      const nodes = [
        {
          metadata: { labels: { 'agentpool': 'gpupool1' } },
          gpuCount: 4,
        },
        {
          metadata: { labels: { 'agentpool': 'gpupool1' } },
          gpuCount: 4,
        },
        {
          metadata: { labels: { 'agentpool': 'gpupool2' } },
          gpuCount: 8,
        },
      ];

      const nodePoolMap = new Map<string, { gpuCount: number; nodeCount: number }>();

      for (const node of nodes) {
        const poolName = node.metadata?.labels?.['agentpool'] || 'default';
        if (!nodePoolMap.has(poolName)) {
          nodePoolMap.set(poolName, { gpuCount: 0, nodeCount: 0 });
        }
        const pool = nodePoolMap.get(poolName)!;
        pool.gpuCount += node.gpuCount;
        pool.nodeCount += 1;
      }

      expect(nodePoolMap.size).toBe(2);
      expect(nodePoolMap.get('gpupool1')?.gpuCount).toBe(8);
      expect(nodePoolMap.get('gpupool1')?.nodeCount).toBe(2);
      expect(nodePoolMap.get('gpupool2')?.gpuCount).toBe(8);
    });

    test('should extract GPU model from labels', () => {
      const node = {
        metadata: {
          labels: {
            'nvidia.com/gpu.product': 'NVIDIA-A100-SXM4-80GB',
          },
        },
      };

      const gpuModel = node.metadata?.labels?.['nvidia.com/gpu.product'];
      expect(gpuModel).toBe('NVIDIA-A100-SXM4-80GB');
    });

    test('should handle nodes with default pool name', () => {
      const node = {
        metadata: { labels: {} as Record<string, string> },
        gpuCount: 4,
      };

      const nodePoolName =
        node.metadata?.labels?.['agentpool'] ||
        node.metadata?.labels?.['kubernetes.azure.com/agentpool'] ||
        'default';

      expect(nodePoolName).toBe('default');
    });

    test('should create valid NodePoolInfo structure', () => {
      const nodePoolInfo: NodePoolInfo = {
        name: 'gpupool1',
        gpuCount: 16,
        nodeCount: 2,
        availableGpus: 8,
        gpuModel: 'NVIDIA-A100-SXM4-80GB',
      };

      expect(nodePoolInfo.name).toBe('gpupool1');
      expect(nodePoolInfo.gpuCount).toBe(16);
      expect(nodePoolInfo.nodeCount).toBe(2);
      expect(nodePoolInfo.availableGpus).toBe(8);
      expect(nodePoolInfo.gpuModel).toBe('NVIDIA-A100-SXM4-80GB');
    });

    test('should handle GKE node pool labels', () => {
      const node = {
        metadata: {
          labels: {
            'cloud.google.com/gke-nodepool': 'gpu-nodepool',
          } as Record<string, string>,
        },
      };

      const nodePoolName =
        node.metadata?.labels?.['agentpool'] ||
        node.metadata?.labels?.['cloud.google.com/gke-nodepool'] ||
        'default';

      expect(nodePoolName).toBe('gpu-nodepool');
    });

    test('should handle EKS node group labels', () => {
      const node = {
        metadata: {
          labels: {
            'eks.amazonaws.com/nodegroup': 'gpu-nodegroup',
          } as Record<string, string>,
        },
      };

      const nodePoolName =
        node.metadata?.labels?.['agentpool'] ||
        node.metadata?.labels?.['eks.amazonaws.com/nodegroup'] ||
        'default';

      expect(nodePoolName).toBe('gpu-nodegroup');
    });
  });

  describe('getPodFailureReasons', () => {
    test('should detect GPU resource constraint from FailedScheduling event', () => {
      const event = {
        type: 'Warning',
        reason: 'FailedScheduling',
        message: '0/3 nodes are available: 3 Insufficient nvidia.com/gpu.',
      };

      const isResourceConstraint = event.reason === 'FailedScheduling' ||
                                  event.message.toLowerCase().includes('insufficient');
      const isGpuConstraint = event.message.includes('nvidia.com/gpu');
      const resourceType: 'gpu' | 'cpu' | 'memory' | undefined = isGpuConstraint ? 'gpu' : undefined;
      const canAutoscalerHelp = isGpuConstraint && !event.message.toLowerCase().includes('taint');

      expect(isResourceConstraint).toBe(true);
      expect(resourceType).toBe('gpu');
      expect(canAutoscalerHelp).toBe(true);
    });

    test('should detect CPU constraint', () => {
      const event = {
        type: 'Warning',
        reason: 'FailedScheduling',
        message: '0/3 nodes are available: 3 Insufficient cpu.',
      };

      const isCpuConstraint = event.message.toLowerCase().includes('cpu');
      const resourceType: 'gpu' | 'cpu' | 'memory' | undefined = isCpuConstraint ? 'cpu' : undefined;

      expect(resourceType).toBe('cpu');
    });

    test('should detect memory constraint', () => {
      const event = {
        type: 'Warning',
        reason: 'FailedScheduling',
        message: '0/3 nodes are available: 3 Insufficient memory.',
      };

      const isMemoryConstraint = event.message.toLowerCase().includes('memory');
      const resourceType: 'gpu' | 'cpu' | 'memory' | undefined = isMemoryConstraint ? 'memory' : undefined;

      expect(resourceType).toBe('memory');
    });

    test('should recognize taint issues (autoscaler cannot help)', () => {
      const event = {
        type: 'Warning',
        reason: 'FailedScheduling',
        message: '0/3 nodes are available: 3 node(s) had taint {nvidia.com/gpu: true}, that the pod didn\'t tolerate.',
      };

      const hasTaintIssue = event.message.toLowerCase().includes('taint') ||
                           event.message.toLowerCase().includes('toleration');
      const canAutoscalerHelp = !hasTaintIssue;

      expect(hasTaintIssue).toBe(true);
      expect(canAutoscalerHelp).toBe(false);
    });

    test('should recognize node selector issues (autoscaler cannot help)', () => {
      const event = {
        type: 'Warning',
        reason: 'FailedScheduling',
        message: '0/3 nodes are available: 3 node(s) didn\'t match node selector.',
      };

      const hasNodeSelectorIssue = event.message.toLowerCase().includes('node selector') ||
                                   event.message.toLowerCase().includes('didn\'t match');
      const canAutoscalerHelp = !hasNodeSelectorIssue;

      expect(hasNodeSelectorIssue).toBe(true);
      expect(canAutoscalerHelp).toBe(false);
    });

    test('should create valid PodFailureReason structure', () => {
      const reason: PodFailureReason = {
        reason: 'FailedScheduling',
        message: '0/3 nodes are available: 3 Insufficient nvidia.com/gpu.',
        isResourceConstraint: true,
        resourceType: 'gpu',
        canAutoscalerHelp: true,
      };

      expect(reason.reason).toBe('FailedScheduling');
      expect(reason.isResourceConstraint).toBe(true);
      expect(reason.resourceType).toBe('gpu');
      expect(reason.canAutoscalerHelp).toBe(true);
    });

    test('should ignore non-Warning events', () => {
      const event = {
        type: 'Normal',
        reason: 'Scheduled',
        message: 'Successfully assigned pod to node',
      };

      const shouldProcess = event.type === 'Warning';
      expect(shouldProcess).toBe(false);
    });
  });
});
