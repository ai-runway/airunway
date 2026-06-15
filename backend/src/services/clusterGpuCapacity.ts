import type * as k8s from '@kubernetes/client-node';
import type { DetailedClusterCapacity, NodePoolInfo } from '@airunway/shared';

/**
 * Per-node GPU information including allocation status.
 */
export interface NodeGpuInfo {
  nodeName: string;
  totalGpus: number;
  allocatedGpus: number;
  availableGpus: number;
}

/**
 * Cluster-wide GPU capacity for fit validation.
 */
export interface ClusterGpuCapacity {
  totalGpus: number;
  allocatedGpus: number;
  availableGpus: number;
  maxContiguousAvailable: number;
  maxNodeGpuCapacity: number;
  gpuNodeCount: number;
  totalMemoryGb?: number;
  nodes: NodeGpuInfo[];
}

export interface ClusterGpuCapacityAdapter {
  listNodes(operationName: string): Promise<k8s.V1Node[]>;
  listPodsForAllNamespaces(operationName: string): Promise<k8s.V1Pod[]>;
  getClusterGpuCapacity(): Promise<ClusterGpuCapacity>;
  logError(context: Record<string, unknown>, message: string): void;
}

export async function getClusterGpuCapacity(
  adapter: ClusterGpuCapacityAdapter
): Promise<ClusterGpuCapacity> {
  try {
    // Step 1: Get all nodes and their GPU capacity
    const nodes = await adapter.listNodes('getClusterGpuCapacity:listNodes');

    const nodeGpuMap = new Map<string, { total: number; allocated: number }>();
    let detectedGpuMemoryGb: number | undefined;

    for (const node of nodes) {
      const nodeName = node.metadata?.name || 'unknown';
      const gpuCapacity = node.status?.allocatable?.['nvidia.com/gpu'];
      if (gpuCapacity) {
        const gpuCount = parseInt(gpuCapacity, 10);
        if (gpuCount > 0) {
          nodeGpuMap.set(nodeName, { total: gpuCount, allocated: 0 });

          // Try to detect GPU memory from node labels (prefer nvidia.com/gpu.memory)
          if (!detectedGpuMemoryGb) {
            // Primary: Use nvidia.com/gpu.memory label (value in MiB from GPU Feature Discovery)
            const gpuMemoryMib = node.metadata?.labels?.['nvidia.com/gpu.memory'];
            if (gpuMemoryMib) {
              const memoryMib = parseInt(gpuMemoryMib, 10);
              if (!isNaN(memoryMib) && memoryMib > 0) {
                detectedGpuMemoryGb = Math.round(memoryMib / 1024); // Convert MiB to GB
              }
            }

            // Fallback: Detect from nvidia.com/gpu.product label
            if (!detectedGpuMemoryGb) {
              const gpuProduct = node.metadata?.labels?.['nvidia.com/gpu.product'];
              if (gpuProduct) {
                detectedGpuMemoryGb = detectGpuMemoryFromProduct(gpuProduct);
              }
            }
          }
        }
      }
    }

    // Step 2: Get all pods across all namespaces and sum their GPU requests per node
    const pods = await adapter.listPodsForAllNamespaces('getClusterGpuCapacity:listPods');

    for (const pod of pods) {
      // Only count running or pending pods (not completed/failed)
      const phase = pod.status?.phase;
      if (phase !== 'Running' && phase !== 'Pending') {
        continue;
      }

      const nodeName = pod.spec?.nodeName;
      if (!nodeName || !nodeGpuMap.has(nodeName)) {
        continue;
      }

      // Sum GPU requests from all containers in the pod
      let podGpuRequests = 0;
      for (const container of pod.spec?.containers || []) {
        const gpuRequest = container.resources?.requests?.['nvidia.com/gpu'];
        if (gpuRequest) {
          podGpuRequests += parseInt(gpuRequest, 10);
        }
        // Also check limits if requests not specified (limits can imply requests)
        if (!gpuRequest) {
          const gpuLimit = container.resources?.limits?.['nvidia.com/gpu'];
          if (gpuLimit) {
            podGpuRequests += parseInt(gpuLimit, 10);
          }
        }
      }

      if (podGpuRequests > 0) {
        const nodeInfo = nodeGpuMap.get(nodeName)!;
        nodeInfo.allocated += podGpuRequests;
      }
    }

    // Step 3: Build result
    const nodesInfo: NodeGpuInfo[] = [];
    let totalGpus = 0;
    let allocatedGpus = 0;
    let maxContiguousAvailable = 0;
    let maxNodeGpuCapacity = 0;

    for (const [nodeName, info] of nodeGpuMap) {
      const availableOnNode = Math.max(0, info.total - info.allocated);
      nodesInfo.push({
        nodeName,
        totalGpus: info.total,
        allocatedGpus: info.allocated,
        availableGpus: availableOnNode,
      });
      totalGpus += info.total;
      allocatedGpus += info.allocated;
      maxContiguousAvailable = Math.max(maxContiguousAvailable, availableOnNode);
      maxNodeGpuCapacity = Math.max(maxNodeGpuCapacity, info.total);
    }

    return {
      totalGpus,
      allocatedGpus,
      availableGpus: totalGpus - allocatedGpus,
      maxContiguousAvailable,
      maxNodeGpuCapacity,
      gpuNodeCount: nodeGpuMap.size,
      totalMemoryGb: detectedGpuMemoryGb,
      nodes: nodesInfo,
    };
  } catch (error) {
    adapter.logError({ error }, 'Error getting cluster GPU capacity');
    return emptyClusterGpuCapacity();
  }
}

export async function getDetailedClusterGpuCapacity(
  adapter: ClusterGpuCapacityAdapter
): Promise<DetailedClusterCapacity> {
  try {
    // Get basic capacity first. Keep this as an adapter method so the public
    // KubernetesService.getClusterGpuCapacity seam remains overrideable.
    const basicCapacity = await adapter.getClusterGpuCapacity();

    // Step 1: Get all nodes and group by node pool
    const nodes = await adapter.listNodes('getDetailedClusterGpuCapacity:listNodes');
    const nodePoolMap = new Map<string, {
      gpuCount: number;
      nodeCount: number;
      availableGpus: number;
      gpuModel?: string;
      instanceType?: string;
      region?: string;
    }>();

    for (const node of nodes) {
      const nodeName = node.metadata?.name || 'unknown';
      const gpuCapacity = node.status?.allocatable?.['nvidia.com/gpu'];

      if (gpuCapacity) {
        const gpuCount = parseInt(gpuCapacity, 10);
        if (gpuCount > 0) {
          const labels = node.metadata?.labels;
          const nodePoolName = getNodePoolName(labels);
          const gpuModel = getGpuModelFromLabels(labels);
          const instanceType = getInstanceType(labels);
          const region = getRegion(labels);

          // Find available GPUs for this node
          const nodeInfo = basicCapacity.nodes.find(n => n.nodeName === nodeName);
          const nodeAvailableGpus = nodeInfo?.availableGpus || 0;

          if (!nodePoolMap.has(nodePoolName)) {
            nodePoolMap.set(nodePoolName, {
              gpuCount: 0,
              nodeCount: 0,
              availableGpus: 0,
              gpuModel,
              instanceType,
              region,
            });
          }

          const poolInfo = nodePoolMap.get(nodePoolName)!;
          poolInfo.gpuCount += gpuCount;
          poolInfo.nodeCount += 1;
          poolInfo.availableGpus += nodeAvailableGpus;

          // Update GPU model if not set or if we find a more specific one
          if (!poolInfo.gpuModel && gpuModel) {
            poolInfo.gpuModel = gpuModel;
          }
          // Update instance type if not set
          if (!poolInfo.instanceType && instanceType) {
            poolInfo.instanceType = instanceType;
          }
          // Update region if not set
          if (!poolInfo.region && region) {
            poolInfo.region = region;
          }
        }
      }
    }

    return {
      totalGpus: basicCapacity.totalGpus,
      allocatedGpus: basicCapacity.allocatedGpus,
      availableGpus: basicCapacity.availableGpus,
      maxContiguousAvailable: basicCapacity.maxContiguousAvailable,
      maxNodeGpuCapacity: basicCapacity.maxNodeGpuCapacity,
      gpuNodeCount: basicCapacity.gpuNodeCount,
      totalMemoryGb: basicCapacity.totalMemoryGb,
      nodePools: toNodePoolArray(nodePoolMap),
    };
  } catch (error) {
    adapter.logError({ error }, 'Error getting detailed cluster GPU capacity');
    return {
      totalGpus: 0,
      allocatedGpus: 0,
      availableGpus: 0,
      maxContiguousAvailable: 0,
      maxNodeGpuCapacity: 0,
      gpuNodeCount: 0,
      nodePools: [],
    };
  }
}

export async function getAllNodePools(
  adapter: ClusterGpuCapacityAdapter
): Promise<NodePoolInfo[]> {
  try {
    const nodes = await adapter.listNodes('getAllNodePools:listNodes');

    const nodePoolMap = new Map<string, {
      nodeCount: number;
      gpuCount: number;
      availableGpus: number;
      gpuModel?: string;
      instanceType?: string;
      region?: string;
    }>();

    for (const node of nodes) {
      const labels = node.metadata?.labels;
      const nodePoolName = getNodePoolName(labels);
      const instanceType = getInstanceType(labels);
      const region = getRegion(labels);

      // Check for GPU capacity
      const gpuCapacity = node.status?.allocatable?.['nvidia.com/gpu'];
      const gpuCount = gpuCapacity ? parseInt(gpuCapacity, 10) : 0;

      // Get GPU model from labels if this node has GPUs
      const gpuModel = gpuCount > 0 ? getGpuModelFromLabels(labels) : undefined;

      if (!nodePoolMap.has(nodePoolName)) {
        nodePoolMap.set(nodePoolName, {
          nodeCount: 0,
          gpuCount: 0,
          availableGpus: 0,
          gpuModel,
          instanceType,
          region,
        });
      }

      const poolInfo = nodePoolMap.get(nodePoolName)!;
      poolInfo.nodeCount += 1;
      poolInfo.gpuCount += gpuCount;

      // Update instance type if not set
      if (!poolInfo.instanceType && instanceType) {
        poolInfo.instanceType = instanceType;
      }
      // Update region if not set
      if (!poolInfo.region && region) {
        poolInfo.region = region;
      }
      // Update GPU model if not set
      if (!poolInfo.gpuModel && gpuModel) {
        poolInfo.gpuModel = gpuModel;
      }
    }

    return toNodePoolArray(nodePoolMap);
  } catch (error) {
    adapter.logError({ error }, 'Error getting all node pools');
    return [];
  }
}

export function getNodePoolName(labels: Record<string, string> | undefined): string {
  return labels?.['agentpool']
    || labels?.['kubernetes.azure.com/agentpool']
    || labels?.['cloud.google.com/gke-nodepool']
    || labels?.['eks.amazonaws.com/nodegroup']
    || 'default';
}

/**
 * Extract GPU model from cloud provider instance type labels.
 * Supports Azure, AWS, and GCP instance type naming conventions.
 */
export function extractGpuModelFromInstanceType(
  labels: Record<string, string> | undefined
): string | undefined {
  if (!labels) return undefined;

  // Get instance type from standard Kubernetes labels
  const instanceType = getInstanceType(labels);
  if (!instanceType) return undefined;

  const instanceLower = instanceType.toLowerCase();

  // Azure NV-series GPU mapping
  // Standard_NV36ads_A10_v5 -> A10
  // Standard_NC24ads_A100_v4 -> A100
  // Standard_ND96asr_A100_v4 -> A100
  // Standard_NC6s_v3 (V100), Standard_NC24s_v3, etc.
  // Standard_NV6 (M60 - older)
  if (instanceLower.includes('_a10')) return 'A10';
  if (instanceLower.includes('_a100')) return 'A100-80GB';
  if (instanceLower.includes('_h100')) return 'H100';
  if (instanceLower.includes('nc') && instanceLower.includes('_v3'))
    return 'V100';
  if (instanceLower.includes('nc') && instanceLower.includes('t4'))
    return 'T4';

  // AWS instance type mapping
  // p4d.24xlarge -> A100
  // p5.48xlarge -> H100
  // g4dn.xlarge -> T4
  // g5.xlarge -> A10G
  // g6.xlarge -> L4
  // g6e.xlarge -> L40S
  if (instanceLower.startsWith('p5')) return 'H100';
  if (instanceLower.startsWith('p4d') || instanceLower.startsWith('p4de'))
    return 'A100-40GB';
  if (instanceLower.startsWith('p3')) return 'V100';
  if (instanceLower.startsWith('g4dn') || instanceLower.startsWith('g4ad'))
    return 'T4';
  if (instanceLower.startsWith('g5g') || instanceLower.startsWith('g5.'))
    return 'A10G';
  if (instanceLower.startsWith('g6e')) return 'L40S';
  if (instanceLower.startsWith('g6.')) return 'L4';

  // GCP machine type mapping
  // a2-highgpu-1g (A100 40GB)
  // a2-ultragpu-1g (A100 80GB)
  // a3-highgpu-8g (H100)
  // n1-standard-4 with nvidia-tesla-t4
  // g2-standard-4 (L4)
  if (instanceLower.startsWith('a3')) return 'H100';
  if (instanceLower.startsWith('a2-ultra')) return 'A100-80GB';
  if (instanceLower.startsWith('a2')) return 'A100-40GB';
  if (instanceLower.startsWith('g2')) return 'L4';

  return undefined;
}

/**
 * Detect GPU memory from NVIDIA GPU product name.
 * This is a best-effort mapping based on common GPU models.
 */
export function detectGpuMemoryFromProduct(gpuProduct: string): number | undefined {
  const product = gpuProduct.toLowerCase();

  // NVIDIA Data Center GPUs
  if (product.includes('a100') && product.includes('80')) return 80;
  if (product.includes('a100') && product.includes('40')) return 40;
  if (product.includes('a100')) return 40; // Default A100 is 40GB
  if (product.includes('h100') && product.includes('80')) return 80;
  if (product.includes('h100')) return 80;
  if (product.includes('h200')) return 141;
  if (product.includes('a10g')) return 24;
  if (product.includes('a10')) return 24;
  if (product.includes('l40s')) return 48;
  if (product.includes('l40')) return 48;
  if (product.includes('l4')) return 24;
  if (product.includes('t4')) return 16;
  if (product.includes('v100') && product.includes('32')) return 32;
  if (product.includes('v100')) return 16;

  // NVIDIA Consumer GPUs
  if (product.includes('4090')) return 24;
  if (product.includes('4080')) return 16;
  if (product.includes('3090')) return 24;
  if (product.includes('3080') && product.includes('12')) return 12;
  if (product.includes('3080')) return 10;

  return undefined;
}

function getGpuModelFromLabels(labels: Record<string, string> | undefined): string | undefined {
  return labels?.['nvidia.com/gpu.product']
    || extractGpuModelFromInstanceType(labels)
    || labels?.['accelerator'];
}

function getInstanceType(labels: Record<string, string> | undefined): string | undefined {
  return labels?.['node.kubernetes.io/instance-type']
    || labels?.['beta.kubernetes.io/instance-type'];
}

function getRegion(labels: Record<string, string> | undefined): string | undefined {
  return labels?.['topology.kubernetes.io/region']
    || labels?.['failure-domain.beta.kubernetes.io/region'];
}

function toNodePoolArray(nodePoolMap: Map<string, {
  gpuCount: number;
  nodeCount: number;
  availableGpus: number;
  gpuModel?: string;
  instanceType?: string;
  region?: string;
}>): NodePoolInfo[] {
  const nodePools: NodePoolInfo[] = [];
  for (const [name, info] of nodePoolMap) {
    nodePools.push({
      name,
      gpuCount: info.gpuCount,
      nodeCount: info.nodeCount,
      availableGpus: info.availableGpus,
      gpuModel: info.gpuModel,
      instanceType: info.instanceType,
      region: info.region,
    });
  }

  return nodePools;
}

function emptyClusterGpuCapacity(): ClusterGpuCapacity {
  return {
    totalGpus: 0,
    allocatedGpus: 0,
    availableGpus: 0,
    maxContiguousAvailable: 0,
    maxNodeGpuCapacity: 0,
    gpuNodeCount: 0,
    nodes: [],
  };
}
