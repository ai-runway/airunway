import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { SkeletonTable } from '@/components/ui/skeleton'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { useDeleteDeployment, type DeploymentStatus } from '@/hooks/useDeployments'
import { useToast } from '@/hooks/useToast'
import { formatRelativeTime } from '@/lib/utils'
import { getEngineDisplayName, getProviderDisplayName } from '@/lib/deploymentDisplay'
import { Eye, Trash2, Rocket } from 'lucide-react'

interface DeploymentListProps {
  deployments: DeploymentStatus[]
  isLoading?: boolean
}

function getStatusDotColor(phase: DeploymentStatus['phase']): string {
  switch (phase) {
    case 'Running':     return 'bg-green-500'
    case 'Pending':     return 'bg-amber-400 animate-pulse'
    case 'Deploying':   return 'bg-blue-500 animate-pulse'
    case 'Failed':      return 'bg-red-400'
    case 'Terminating': return 'bg-slate-400 animate-pulse'
    default:            return 'bg-slate-500'
  }
}

function getReplicaColorClass(deployment: DeploymentStatus): string {
  if (deployment.mode === 'disaggregated' && deployment.prefillReplicas && deployment.decodeReplicas) {
    const allReady = deployment.prefillReplicas.ready === deployment.prefillReplicas.desired &&
                     deployment.decodeReplicas.ready === deployment.decodeReplicas.desired
    return allReady ? 'text-green-400' : 'text-amber-400'
  }
  return deployment.replicas.ready === deployment.replicas.desired ? 'text-green-400' : 'text-amber-400'
}

/**
 * Format replica status for display
 * For disaggregated mode, shows "P: x/y, D: x/y" format
 * For aggregated mode, shows "x/y" format
 */
function formatReplicaStatus(deployment: DeploymentStatus): string {
  if (deployment.mode === 'disaggregated' && deployment.prefillReplicas && deployment.decodeReplicas) {
    const pReady = deployment.prefillReplicas.ready
    const pDesired = deployment.prefillReplicas.desired
    const dReady = deployment.decodeReplicas.ready
    const dDesired = deployment.decodeReplicas.desired
    return `P: ${pReady}/${pDesired}, D: ${dReady}/${dDesired}`
  }
  return `${deployment.replicas.ready}/${deployment.replicas.desired}`
}

export function DeploymentList({ deployments, isLoading }: DeploymentListProps) {
  const navigate = useNavigate()
  const { toast } = useToast()
  const deleteDeployment = useDeleteDeployment()
  const [deleteTarget, setDeleteTarget] = useState<DeploymentStatus | null>(null)

  const handleDelete = async () => {
    if (!deleteTarget) return

    try {
      await deleteDeployment.mutateAsync({
        name: deleteTarget.name,
        namespace: deleteTarget.namespace,
      })
      toast({
        title: 'Deployment Deleted',
        description: `${deleteTarget.name} has been deleted`,
        variant: 'success',
      })
      setDeleteTarget(null)
    } catch (error) {
      toast({
        title: 'Delete Failed',
        description: error instanceof Error ? error.message : 'Failed to delete deployment',
        variant: 'destructive',
      })
    }
  }

  // Loading state with skeleton
  if (isLoading) {
    return <SkeletonTable rows={5} columns={7} className="rounded-lg border" />
  }

  // Empty state
  if (deployments.length === 0) {
    return (
      <div className="glass-panel flex flex-col items-center justify-center py-16 text-center">
        <Rocket className="h-12 w-12 text-muted-foreground/50 mb-4" />
        <h3 className="text-lg font-medium text-foreground mb-1">No deployments yet</h3>
        <p className="text-sm text-muted-foreground mb-6 max-w-md">
          Deploy your first model to start serving inference requests.
        </p>
        <Button onClick={() => navigate('/')} className="bg-cyan-600 hover:bg-cyan-700 text-white">
          Deploy your first model
        </Button>
      </div>
    )
  }

  return (
    <>
      {/* Card-based rows */}
      <div className="space-y-3">
        {deployments.map((deployment, index) => (
          <div
            key={deployment.name}
            className="glass-panel !p-4 flex items-center gap-4 group hover:bg-white/5 hover:border-white/10 transition-all duration-200 animate-slide-up"
            style={{ animationDelay: `${Math.min(index, 12) * 50}ms`, animationFillMode: 'both' }}
          >
            {/* Status dot */}
            <div className="shrink-0">
              <span className={`h-3 w-3 rounded-full inline-block ${getStatusDotColor(deployment.phase)}`} />
            </div>

            {/* Name & Model */}
            <div className="flex-1 min-w-0">
              <Link
                to={`/deployments/${deployment.name}?namespace=${deployment.namespace}`}
                className="font-medium text-foreground hover:text-primary transition-colors"
              >
                {deployment.name}
              </Link>
              <p className="text-sm text-muted-foreground truncate">
                {deployment.modelId}
              </p>
            </div>

            {/* Badges (hidden on small screens) */}
            <div className="hidden md:flex items-center gap-2">
              <Badge
                variant="secondary"
              >
                {getProviderDisplayName(deployment.provider)}
              </Badge>
              <Badge variant="outline">
                {getEngineDisplayName(deployment.engine)}
              </Badge>
              {deployment.mode === 'disaggregated' && (
                <Badge variant="secondary" className="text-xs">P/D</Badge>
              )}
              <span
                className={`text-sm tabular-nums ${getReplicaColorClass(deployment)}`}
                title={deployment.mode === 'disaggregated' ? 'Prefill / Decode replicas' : 'Worker replicas'}
              >
                {formatReplicaStatus(deployment)} ready
              </span>
            </div>

            {/* Age & Actions */}
            <div className="flex items-center gap-1">
              <span className="text-sm text-muted-foreground hidden lg:inline mr-2">
                {formatRelativeTime(deployment.createdAt)}
              </span>
              <Link to={`/deployments/${deployment.name}?namespace=${deployment.namespace}`}>
                <Button size="sm" variant="ghost" title="View details">
                  <Eye className="h-4 w-4" />
                </Button>
              </Link>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => setDeleteTarget(deployment)}
                title="Delete deployment"
                className="text-red-400 hover:bg-red-500/10 hover:text-red-400"
              >
                <Trash2 className="h-4 w-4" />
              </Button>
            </div>
          </div>
        ))}
      </div>

      {/* Delete Confirmation Dialog */}
      <Dialog open={!!deleteTarget} onOpenChange={() => setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete Deployment</DialogTitle>
            <DialogDescription>
              Are you sure you want to delete <strong>{deleteTarget?.name}</strong>?
              This action cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={handleDelete}
              loading={deleteDeployment.isProcessing}
              loadingText="Deleting..."
            >
              Delete
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
