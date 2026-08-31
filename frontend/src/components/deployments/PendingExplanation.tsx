import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Loader2, AlertTriangle, Info, Clock } from 'lucide-react';
import type { PodFailureReason, AutoscalerDetectionResult } from '@/lib/api';

interface PendingExplanationProps {
  reasons: PodFailureReason[];
  autoscaler?: AutoscalerDetectionResult;
  isLoading?: boolean;
}

export function PendingExplanation({ reasons, autoscaler, isLoading }: PendingExplanationProps) {
  if (isLoading) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Checking deployment status...</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex items-center gap-2 text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            <span>Loading failure reasons...</span>
          </div>
        </CardContent>
      </Card>
    );
  }

  if (!reasons || reasons.length === 0) {
    return null;
  }

  // Group reasons by type
  const gpuReasons = reasons.filter(r => r.resourceType === 'gpu');
  const cpuReasons = reasons.filter(r => r.resourceType === 'cpu');
  const memoryReasons = reasons.filter(r => r.resourceType === 'memory');
  const otherReasons = reasons.filter(r => !r.resourceType);

  const hasAutoscalerHelp = reasons.some(r => r.canAutoscalerHelp);
  const hasTaintIssue = reasons.some(r =>
    r.message.toLowerCase().includes('taint') || r.message.toLowerCase().includes('toleration')
  );
  const hasNodeSelectorIssue = reasons.some(r =>
    r.message.toLowerCase().includes('node selector')
  );

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Clock className="h-5 w-5" />
          Why is this deployment pending?
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* GPU Resource Constraints */}
        {gpuReasons.length > 0 && (
          <Alert className="border-blue-500 bg-blue-50 dark:bg-blue-950/20">
            <Info className="h-4 w-4 text-blue-600" />
            <AlertDescription className="text-blue-700 dark:text-blue-300">
              <p className="font-medium">Insufficient GPU resources</p>
              <p className="text-sm mt-1">{gpuReasons[0].message}</p>

              {hasAutoscalerHelp && autoscaler?.detected && (
                <div className="mt-3 flex items-start gap-2 text-sm">
                  <Loader2 className="h-4 w-4 animate-spin flex-shrink-0 mt-0.5" />
                  <div>
                    <p className="font-medium">
                      {autoscaler.type === 'aks-managed' ? 'AKS managed autoscaler' : 'Autoscaler'} is scaling up
                    </p>
                    <p className="text-xs mt-1">
                      The system will automatically add GPU compute resources. This typically takes 5-10 minutes.
                      {autoscaler.lastActivity && ` Last activity: ${new Date(autoscaler.lastActivity).toLocaleTimeString()}`}
                    </p>
                  </div>
                </div>
              )}

              {hasAutoscalerHelp && !autoscaler?.detected && (
                <div className="mt-3 text-sm">
                  <p className="font-medium">Action required</p>
                  <p className="text-xs mt-1">
                    Enable autoscaling to automatically add GPU compute resources when needed.
                  </p>
                  <a
                    href="https://learn.microsoft.com/en-us/azure/aks/cluster-autoscaler"
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-xs underline hover:no-underline mt-1 inline-block"
                  >
                    Learn how to enable autoscaling →
                  </a>
                </div>
              )}
            </AlertDescription>
          </Alert>
        )}

        {/* CPU/Memory Resource Constraints */}
        {(cpuReasons.length > 0 || memoryReasons.length > 0) && (
          <Alert className="border-blue-500 bg-blue-50 dark:bg-blue-950/20">
            <Info className="h-4 w-4 text-blue-600" />
            <AlertDescription className="text-blue-700 dark:text-blue-300">
              <p className="font-medium">Insufficient {cpuReasons.length > 0 ? 'CPU' : 'memory'} resources</p>
              <p className="text-sm mt-1">
                {(cpuReasons[0] || memoryReasons[0])?.message}
              </p>
              {autoscaler?.detected && (
                <p className="text-xs mt-2">
                  Autoscaler will attempt to add more compute resources.
                </p>
              )}
            </AlertDescription>
          </Alert>
        )}

        {/* Taint/Toleration Issues */}
        {hasTaintIssue && (
          <Alert variant="destructive">
            <AlertTriangle className="h-4 w-4" />
            <AlertDescription>
              <p className="font-medium">Deployment has scheduling constraints</p>
              <p className="text-sm mt-1">
                GPU compute resources have constraints that prevent scheduling. You may need to add tolerations to your deployment.
              </p>
              <details className="mt-2 text-xs">
                <summary className="cursor-pointer hover:underline">View event details</summary>
                <pre className="mt-1 p-2 bg-black/5 dark:bg-white/5 rounded overflow-x-auto">
                  {reasons.find(r => r.message.toLowerCase().includes('taint'))?.message}
                </pre>
              </details>
            </AlertDescription>
          </Alert>
        )}

        {/* Node Selector Issues */}
        {hasNodeSelectorIssue && (
          <Alert variant="destructive">
            <AlertTriangle className="h-4 w-4" />
            <AlertDescription>
              <p className="font-medium">No available compute resources match the deployment requirements</p>
              <p className="text-sm mt-1">
                Check that deployment requirements match available compute resources.
              </p>
            </AlertDescription>
          </Alert>
        )}

        {/* Other Reasons */}
        {otherReasons.length > 0 && gpuReasons.length === 0 && cpuReasons.length === 0 && memoryReasons.length === 0 && (
          <Alert>
            <Info className="h-4 w-4" />
            <AlertDescription>
              <p className="font-medium">Scheduling constraint</p>
              <p className="text-sm mt-1">{otherReasons[0].message}</p>
            </AlertDescription>
          </Alert>
        )}

        {/* Autoscaler Status Summary */}
        {autoscaler && (
          <div className="text-xs text-muted-foreground pt-2 border-t space-y-1">
            <p>
              Autoscaler: {autoscaler.detected
                ? `${autoscaler.type === 'aks-managed' ? 'AKS Managed' : 'Autoscaler'} (${autoscaler.healthy ? 'Healthy' : 'Unhealthy'})`
                : 'Not detected'}
            </p>
            {!autoscaler.detected && (
              <a
                href="https://github.com/ai-runway/airunway/blob/main/docs/azure-autoscaling.md"
                target="_blank"
                rel="noopener noreferrer"
                className="text-primary hover:underline inline-flex items-center gap-1"
              >
                Learn how to enable autoscaling →
              </a>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
