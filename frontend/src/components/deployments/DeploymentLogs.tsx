import { useState, useRef, useEffect } from 'react';
import { Button } from '@/components/ui/button';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { useDeploymentLogs, useDeploymentPods } from '@/hooks/useDeployments';
import { Loader2, RefreshCw, Copy, ScrollText, ArrowDown } from 'lucide-react';
import { useToast } from '@/hooks/useToast';

interface DeploymentLogsProps {
  deploymentName: string;
  namespace: string;
}

const TAIL_LINE_OPTIONS = [
  { value: '50', label: '50 lines' },
  { value: '100', label: '100 lines' },
  { value: '500', label: '500 lines' },
  { value: '1000', label: '1000 lines' },
];

export function DeploymentLogs({ deploymentName, namespace }: DeploymentLogsProps) {
  const { toast } = useToast();
  const logsContainerRef = useRef<HTMLPreElement>(null);
  
  const [selectedPod, setSelectedPod] = useState<string | undefined>();
  const [tailLines, setTailLines] = useState<number>(100);
  const [autoScroll, setAutoScroll] = useState(true);
  const [timestamps, setTimestamps] = useState(false);

  // Fetch pods for this deployment
  const { data: pods } = useDeploymentPods(deploymentName, namespace);
  const selectedPodIsAvailable = pods?.some((pod) => pod.name === selectedPod) ?? false;
  const effectiveSelectedPod = selectedPodIsAvailable ? selectedPod : pods?.[0]?.name;

  // Keep a valid selected instance as pods change (for example during rollouts).
  useEffect(() => {
    if (!pods || pods.length === 0) {
      setSelectedPod(undefined);
      return;
    }

    if (!selectedPod || !pods.some((pod) => pod.name === selectedPod)) {
      setSelectedPod(pods[0].name);
    }
  }, [pods, selectedPod]);

  // Fetch logs
  const { data: logsData, isLoading, refetch, isFetching, error } = useDeploymentLogs(
    deploymentName,
    namespace,
    { podName: effectiveSelectedPod, tailLines, timestamps }
  );

  // Auto-scroll to bottom when new logs arrive
  useEffect(() => {
    if (autoScroll && logsContainerRef.current) {
      logsContainerRef.current.scrollTop = logsContainerRef.current.scrollHeight;
    }
  }, [logsData?.logs, autoScroll]);

  const handleCopyLogs = () => {
    if (logsData?.logs) {
      navigator.clipboard.writeText(logsData.logs);
      toast({
        title: 'Copied to clipboard',
        description: 'Logs copied successfully',
      });
    }
  };

  const handleRefresh = () => {
    refetch();
  };

  const handleScrollToBottom = () => {
    if (logsContainerRef.current) {
      logsContainerRef.current.scrollTop = logsContainerRef.current.scrollHeight;
    }
  };

  // Handle scroll event to detect if user scrolled up
  const handleScroll = () => {
    if (logsContainerRef.current) {
      const { scrollTop, scrollHeight, clientHeight } = logsContainerRef.current;
      const isAtBottom = scrollHeight - scrollTop - clientHeight < 50;
      setAutoScroll(isAtBottom);
    }
  };

  if (!pods || pods.length === 0) {
    return (
      <div className="glass-panel">
        <div className="flex items-center gap-2 mb-4">
          <ScrollText className="h-5 w-5" />
          <h2 className="text-lg font-heading">Logs</h2>
        </div>
        <p className="text-sm text-muted-foreground">
          No instances available. Logs will appear once instances are running.
        </p>
      </div>
    );
  }

  return (
    <div className="glass-panel">
      <div className="mb-4">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <ScrollText className="h-5 w-5" />
            <h2 className="text-lg font-heading">Logs</h2>
          </div>
          <div className="flex items-center gap-2">
            {/* Instance Selector */}
            {pods.length > 1 && (
              <Select value={effectiveSelectedPod} onValueChange={setSelectedPod}>
                <SelectTrigger className="w-[200px]" aria-label="Instance">
                  <SelectValue placeholder="Select instance" />
                </SelectTrigger>
                <SelectContent>
                  {pods.map((pod) => (
                    <SelectItem key={pod.name} value={pod.name}>
                      <span className="font-mono-code text-xs">{pod.name}</span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}

            {/* Tail Lines Selector */}
            <Select value={tailLines.toString()} onValueChange={(v) => setTailLines(parseInt(v))}>
              <SelectTrigger className="w-[120px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {TAIL_LINE_OPTIONS.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>

            {/* Timestamps Toggle */}
            <Button
              variant={timestamps ? 'secondary' : 'outline'}
              size="sm"
              onClick={() => setTimestamps(!timestamps)}
              title="Toggle timestamps"
            >
              T
            </Button>

            {/* Copy Button */}
            <Button
              variant="outline"
              size="icon"
              onClick={handleCopyLogs}
              disabled={!logsData?.logs}
              title="Copy logs"
            >
              <Copy className="h-4 w-4" />
            </Button>

            {/* Refresh Button */}
            <Button
              variant="outline"
              size="icon"
              onClick={handleRefresh}
              disabled={isFetching}
              title="Refresh logs"
            >
              <RefreshCw className={`h-4 w-4 ${isFetching ? 'animate-spin' : ''}`} />
            </Button>
          </div>
        </div>
      </div>
      <div>
        <div className="relative">
          {isLoading || !effectiveSelectedPod ? (
            <div className="flex items-center justify-center py-12">
              <Loader2 className="h-8 w-8 animate-spin text-muted-foreground" />
            </div>
          ) : error ? (
            <div className="rounded-lg bg-[#0A0A0A] text-red-400 p-4 text-xs font-mono-code min-h-[200px]">
              <p className="font-semibold mb-2">Error fetching logs:</p>
              <p>{error instanceof Error ? error.message : 'Unknown error'}</p>
              <Button
                variant="outline"
                size="sm"
                onClick={() => refetch()}
                className="mt-4"
              >
                <RefreshCw className="h-4 w-4 mr-2" />
                Retry
              </Button>
            </div>
          ) : (
            <>
              <pre
                ref={logsContainerRef}
                onScroll={handleScroll}
                className="rounded-lg bg-[#0A0A0A] text-zinc-100 p-4 text-xs font-mono-code overflow-auto max-h-[500px] min-h-[200px] whitespace-pre-wrap break-all"
              >
                {logsData?.logs || 'No logs available'}
              </pre>

              {!autoScroll && (
                <Button
                  variant="secondary"
                  size="sm"
                  className="absolute bottom-4 right-4 shadow-lg"
                  onClick={handleScrollToBottom}
                >
                  <ArrowDown className="h-4 w-4 mr-1" />
                  Latest
                </Button>
              )}
            </>
          )}
        </div>

        {/* Instance info */}
        {logsData?.podName && (
          <p className="text-xs text-muted-foreground mt-2">
            Showing logs from instance: <span className="font-mono-code">{logsData.podName}</span>
            {logsData.container && <> (container: <span className="font-mono-code">{logsData.container}</span>)</>}
          </p>
        )}
      </div>
    </div>
  );
}
