import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip';
import { Zap } from 'lucide-react';
import { cn } from '@/lib/utils';
import type { GpuThroughputEstimate } from '@airunway/shared';

interface ThroughputEstimateProps {
  estimate?: GpuThroughputEstimate;
  isLoading?: boolean;
  className?: string;
}

/** Compact number formatting: 18234 -> "18.2k". */
function formatCount(n: number): string {
  if (n >= 1000) {
    return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
  }
  return `${Math.round(n)}`;
}

/**
 * Shows a rough inference-speed estimate for a model on the cluster's GPUs.
 *
 * Two numbers (see issue #139):
 *  - per-chat token speed (single-stream decode, "how snappy chat feels")
 *  - concurrent requests + aggregate tokens/sec (KV-cache-budget gated)
 *
 * Both are estimates — no inference is run. When architecture data is missing
 * (lowConfidence) only the per-chat number is shown.
 */
export function ThroughputEstimate({ estimate, isLoading, className }: ThroughputEstimateProps) {
  if (isLoading) {
    return (
      <span className={cn('flex items-center gap-2 text-sm text-muted-foreground', className)}>
        <Zap className="h-4 w-4" />
        Estimating speed…
      </span>
    );
  }

  if (!estimate || estimate.perChatTokensPerSec <= 0) {
    return null;
  }

  const {
    perChatTokensPerSec,
    concurrentSequences,
    aggregateTokensPerSec,
    gpuModel,
    tpSize,
    contextLen,
    lowConfidence,
  } = estimate;

  const hasConcurrency =
    !lowConfidence && typeof concurrentSequences === 'number' && concurrentSequences > 0;

  const label = hasConcurrency
    ? `~${Math.round(perChatTokensPerSec)} tok/s per chat · ~${formatCount(concurrentSequences!)} concurrent`
    : `~${Math.round(perChatTokensPerSec)} tok/s per chat`;

  const tooltip = hasConcurrency
    ? `Rough estimate on ${gpuModel} (assuming ${tpSize} GPU${tpSize > 1 ? 's' : ''} per replica at ${contextLen.toLocaleString()} token context): about ${Math.round(perChatTokensPerSec)} tokens/sec for a single chat, and roughly ${concurrentSequences!.toLocaleString()} requests at once (~${aggregateTokensPerSec?.toLocaleString()} tokens/sec total). Based on memory-bandwidth and KV-cache heuristics — actual speed varies with batch size, prompt length, and quantization.`
    : `Rough single-chat speed estimate on ${gpuModel}: about ${Math.round(perChatTokensPerSec)} tokens/sec. Concurrent-capacity estimate unavailable (model architecture details could not be read). Actual speed varies with batch size, prompt length, and quantization.`;

  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            className={cn(
              'flex items-center gap-2 text-sm text-muted-foreground',
              className
            )}
          >
            <Zap className="h-4 w-4 shrink-0" />
            <span className="truncate">{label}</span>
          </span>
        </TooltipTrigger>
        <TooltipContent>
          <p className="max-w-xs">{tooltip}</p>
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
