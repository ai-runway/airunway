import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { AlertTriangle, Info, XCircle, Layers } from 'lucide-react';
import type { DetailedClusterCapacity, AutoscalerDetectionResult, DeploymentMode } from '@/lib/api';
import type { MultiNodeRecommendation } from '@/lib/gpu-recommendations';

interface CapacityWarningProps {
  selectedGpus: number;
  capacity: DetailedClusterCapacity;
  autoscaler?: AutoscalerDetectionResult;
  /** Maximum GPUs needed by a single pod (for node placement) */
  maxGpusPerPod?: number;
  /** Deployment mode for better messaging */
  deploymentMode?: DeploymentMode;
  /** Number of replicas (for aggregated mode) */
  replicas?: number;
  /** GPUs per replica (for aggregated mode) */
  gpusPerReplica?: number;
  /** Multi-node deployment info */
  multiNode?: MultiNodeRecommendation | null;
}

export function CapacityWarning({
  selectedGpus,
  capacity,
  autoscaler,
  maxGpusPerPod,
  deploymentMode,
  replicas,
  gpusPerReplica,
  multiNode
}: CapacityWarningProps) {
  // Use maxGpusPerPod if provided (for disaggregated), otherwise assume all GPUs for one pod
  const largestPodGpus = maxGpusPerPod || selectedGpus;

  // Build deployment breakdown message for aggregated mode
  const getDeploymentBreakdown = () => {
    if (deploymentMode === 'aggregated' && replicas && gpusPerReplica) {
      return `${gpusPerReplica} GPU${gpusPerReplica > 1 ? 's' : ''} × ${replicas} replica${replicas > 1 ? 's' : ''} = ${selectedGpus} total GPU${selectedGpus > 1 ? 's' : ''}`;
    }
    return `${selectedGpus} GPU${selectedGpus > 1 ? 's' : ''}`;
  };

  const deploymentBreakdown = getDeploymentBreakdown();

  // Build multi-node info banner (rendered alongside any availability warning)
  let multiNodeBanner: React.ReactNode = null;
  if (multiNode) {
    const totalMultiNodeGpus = multiNode.totalGpus * (replicas || 1);

    // Purple info banner — will be combined with any availability warning below
    multiNodeBanner = (
      <Alert className="border-purple-500 bg-purple-50 dark:bg-purple-950/20">
        <Layers className="h-4 w-4 text-purple-600" />
        <AlertTitle className="text-purple-800 dark:text-purple-200">
          Multi-node deployment
        </AlertTitle>
        <AlertDescription className="text-purple-700 dark:text-purple-300">
          <p>
            Model distributed across {multiNode.nodeCount} node{multiNode.nodeCount > 1 ? 's' : ''} × {multiNode.gpusPerNode} GPU{multiNode.gpusPerNode > 1 ? 's' : ''} ({multiNode.totalGpus} GPUs per replica)
          </p>
          {replicas && replicas > 1 && (
            <p className="mt-1 text-xs">
              Total: {totalMultiNodeGpus} GPUs ({replicas} replicas × {multiNode.totalGpus} GPUs)
            </p>
          )}
        </AlertDescription>
      </Alert>
    );
  }

  // No warning needed - capacity is sufficient (but still show multi-node info if present)
  if (selectedGpus <= capacity.availableGpus && largestPodGpus <= capacity.maxNodeGpuCapacity) {
    return multiNodeBanner ? <>{multiNodeBanner}</> : null;
  }

  // Red Error: Impossible to fit on any single node (skip for multi-node since it handles cross-node)
  if (!multiNode && largestPodGpus > capacity.maxNodeGpuCapacity) {
    return (
      <Alert variant="destructive">
        <XCircle className="h-4 w-4" />
        <AlertTitle>Deployment exceeds available capacity</AlertTitle>
        <AlertDescription>
          <p>
            This deployment requires {largestPodGpus} GPU{largestPodGpus > 1 ? 's' : ''} per instance,
            but the largest available GPU compute resource has only {capacity.maxNodeGpuCapacity} GPU{capacity.maxNodeGpuCapacity > 1 ? 's' : ''}.
          </p>
          {deploymentMode === 'aggregated' && replicas && gpusPerReplica && (
            <p className="mt-1 text-xs">
              ({deploymentBreakdown})
            </p>
          )}
          <p className="mt-2">
            You must either:
          </p>
          <ul className="list-disc list-inside mt-1 space-y-1">
            <li>Reduce GPU count to {capacity.maxNodeGpuCapacity} or fewer per instance</li>
            <li>Add larger GPU compute resources</li>
          </ul>
          {capacity.nodePools.length > 0 && (
            <div className="mt-3 text-xs">
              <p className="font-medium">Current resource pools:</p>
              <ul className="list-disc list-inside mt-1">
                {capacity.nodePools.map((pool) => (
                  <li key={pool.name}>
                    {pool.name}: {pool.gpuCount} GPUs across {pool.nodeCount} compute resource{pool.nodeCount > 1 ? 's' : ''}
                    {pool.gpuModel && ` (${pool.gpuModel})`}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </AlertDescription>
      </Alert>
    );
  }

  // Yellow Warning: May trigger scale-up
  if (selectedGpus > capacity.availableGpus) {
    const availabilityWarning = autoscaler?.detected ? (
      <Alert className="border-yellow-500 bg-yellow-50 dark:bg-yellow-950/20">
        <AlertTriangle className="h-4 w-4 text-yellow-600" />
        <AlertTitle className="text-yellow-800 dark:text-yellow-200">
          System will attempt to scale up
        </AlertTitle>
        <AlertDescription className="text-yellow-700 dark:text-yellow-300">
          <p>
            This deployment requires {deploymentBreakdown},
            but only {capacity.availableGpus} {capacity.availableGpus === 1 ? 'is' : 'are'} currently available.
          </p>
          <p className="mt-2">
            <span className="font-medium">{autoscaler.type === 'aks-managed' ? 'AKS managed autoscaler' : 'Autoscaler'}</span> is
            enabled and will attempt to scale up automatically.
          </p>
          <div className="flex items-start gap-2 mt-2 text-sm">
            <Info className="h-4 w-4 flex-shrink-0 mt-0.5" />
            <div>
              <p className="text-xs">
                {capacity.availableGpus}/{capacity.totalGpus} GPUs available •
                {autoscaler.nodeGroupCount && ` ${autoscaler.nodeGroupCount} autoscaling resource pool${autoscaler.nodeGroupCount > 1 ? 's' : ''}`}
              </p>
            </div>
          </div>
        </AlertDescription>
      </Alert>
    ) : (
      <Alert className="border-yellow-500 bg-yellow-50 dark:bg-yellow-950/20">
        <AlertTriangle className="h-4 w-4 text-yellow-600" />
        <AlertTitle className="text-yellow-800 dark:text-yellow-200">
          Insufficient GPU capacity
        </AlertTitle>
        <AlertDescription className="text-yellow-700 dark:text-yellow-300">
          <p>
            This deployment requires {deploymentBreakdown},
            but only {capacity.availableGpus} {capacity.availableGpus === 1 ? 'is' : 'are'} currently available.
          </p>
          <p className="mt-2">
            Autoscaling is not detected. The deployment will remain pending until resources become available.
          </p>
          <div className="mt-3 text-sm">
            <a
              href="https://github.com/ai-runway/airunway/blob/main/docs/azure-autoscaling.md"
              target="_blank"
              rel="noopener noreferrer"
              className="underline hover:no-underline"
            >
              Learn how to enable autoscaling →
            </a>
          </div>
          <p className="text-xs mt-2">
            {capacity.availableGpus}/{capacity.totalGpus} GPUs available
          </p>
        </AlertDescription>
      </Alert>
    );

    return (
      <>
        {multiNodeBanner}
        {availabilityWarning}
      </>
    );
  }

  return multiNodeBanner ? <>{multiNodeBanner}</> : null;
}
