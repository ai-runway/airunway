import { useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { GpuFitIndicator } from './GpuFitIndicator'
import { ThroughputEstimate } from './ThroughputEstimate'
import { type Model } from '@/lib/api'
import { Cpu, HardDrive, Layers } from 'lucide-react'
import { cn } from '@/lib/utils'
import { useGpuThroughput } from '@/hooks/useGpuOperator'
import { useInView } from '@/hooks/useInView'
import { buildThroughputParamsForGpu } from '@/lib/gpu-throughput-params'
import { getEngineDisplayName } from '@/lib/deploymentDisplay'

interface ModelCardProps {
  model: Model
  gpuCapacityGb?: number
  gpuCount?: number
  gpuCapacityLabel?: string
  /** Whether the cluster has any GPU pool to estimate on (backend picks which). */
  gpuPresent?: boolean
}

/**
 * Format task label for display
 */
function formatTaskLabel(task: string): string {
  switch (task) {
    case 'text-generation': return 'Text Generation'
    case 'image-text-to-text': return 'Multimodal'
    default: return task.replace(/-/g, ' ')
  }
}

/**
 * Parse a GPU memory string like "8GB" or "16 GB" into a numeric GB value.
 */
function parseGpuMemoryGb(memoryStr?: string): number | undefined {
  if (!memoryStr) return undefined
  const match = memoryStr.match(/(\d+(?:\.\d+)?)\s*GB/i)
  return match ? parseFloat(match[1]) : undefined
}

/**
 * Estimate RAM needed for a GGUF model based on parameter count.
 * GGUF Q4_K_M quantization ≈ 0.6 GB per billion parameters + overhead.
 */
function estimateGgufRamGb(sizeStr?: string): number | undefined {
  if (!sizeStr) return undefined
  // Handle MoE format like "8x7B"
  const moeMatch = sizeStr.match(/(\d+)x(\d+(?:\.\d+)?)\s*B/i)
  if (moeMatch) {
    const experts = parseFloat(moeMatch[1])
    const perExpert = parseFloat(moeMatch[2])
    // MoE: all expert params exist in memory
    return Math.ceil(experts * perExpert * 0.6 + 2)
  }
  const match = sizeStr.match(/(\d+(?:\.\d+)?)\s*B/i)
  if (!match) return undefined
  const billions = parseFloat(match[1])
  // Q4_K_M ≈ 0.6 GB/B + ~2 GB overhead for KV cache and runtime
  return Math.ceil(billions * 0.6 + 2)
}

export function ModelCard({ model, gpuCapacityGb, gpuCount, gpuCapacityLabel, gpuPresent }: ModelCardProps) {
  const navigate = useNavigate()

  const handleDeploy = () => {
    navigate(`/deploy/${encodeURIComponent(model.id)}`)
  }

  const isCpuModel = model.minGpus === 0 || model.supportedEngines.every(e => e === 'llamacpp')
  const estimatedGpuMemoryGb = model.estimatedGpuMemoryGb ?? parseGpuMemoryGb(model.minGpuMemory)
  const estimatedCpuRamGb = isCpuModel ? estimateGgufRamGb(model.size) : undefined

  // Lazy throughput estimate: only fetch once the card scrolls into view, and
  // skip CPU-only models (no GPU throughput to estimate).
  const { ref: inViewRef, inView } = useInView<HTMLDivElement>()
  const throughputParams = !isCpuModel ? buildThroughputParamsForGpu(model, gpuPresent) : undefined
  const { data: throughput, isLoading: throughputLoading } = useGpuThroughput(
    throughputParams ?? {},
    { enabled: inView && !!throughputParams }
  )

  return (
    <div
      ref={inViewRef}
      className={cn(
        "flex flex-col h-full group rounded-2xl p-5",
        "bg-white/[0.03] border border-white/5",
        "transition-all duration-200 ease-out",
        "hover:border-cyan-500/30 hover:shadow-glow-card hover:-translate-y-0.5"
      )}
    >
      <div className="pb-3">
        <div className="flex items-start justify-between gap-2">
          <h3 className="text-lg font-semibold text-white leading-tight truncate group-hover:text-cyan-400 transition-colors duration-200">
            {model.name}
          </h3>
          <Badge variant="outline" className="shrink-0 font-mono text-xs">
            {model.size}
          </Badge>
        </div>
        <p className="text-xs text-slate-500 font-mono truncate mt-1">
          {model.id}
        </p>
      </div>

      <div className="flex-1">
        <p className="text-sm text-slate-500 mb-4 line-clamp-2">
          {model.description}
        </p>

        <div className="space-y-2 text-sm">
          <div className="flex items-center gap-2 text-slate-400">
            <Cpu className="h-4 w-4 shrink-0" />
            {isCpuModel && estimatedCpuRamGb ? (
              <span className="truncate">CPU/GPU · ~{estimatedCpuRamGb} GB</span>
            ) : estimatedGpuMemoryGb && gpuCapacityGb ? (
              <GpuFitIndicator
                estimatedGpuMemoryGb={estimatedGpuMemoryGb}
                clusterCapacityGb={gpuCapacityGb}
                gpuCount={gpuCount}
                capacityLabel={gpuCapacityLabel}
              />
            ) : (
              <span className="truncate">GPU: {model.minGpuMemory || 'N/A'}</span>
            )}
          </div>

          {model.contextLength && (
            <div className="flex items-center gap-2 text-slate-400">
              <Layers className="h-4 w-4 shrink-0" />
              <span>Context: {model.contextLength.toLocaleString()}</span>
            </div>
          )}

          <div className="flex items-center gap-2 text-slate-400">
            <HardDrive className="h-4 w-4 shrink-0" />
            <span>{model.conversational ? 'Chat' : formatTaskLabel(model.task)}</span>
          </div>

          {throughputParams && (throughputLoading || throughput) && (
            <ThroughputEstimate estimate={throughput} isLoading={throughputLoading} />
          )}
        </div>

        <div className="flex flex-wrap gap-1.5 mt-4">
          {model.supportedEngines.map((engine) => (
            <Badge
              key={engine}
              variant="secondary"
              className="text-xs font-medium rounded-full"
            >
              {getEngineDisplayName(engine)}
            </Badge>
          ))}
        </div>
      </div>

      <div className="pt-4">
        <Button
          variant="ghost"
          onClick={handleDeploy}
          className="w-full text-cyan-400 hover:text-cyan-300 hover:bg-cyan-400/10 rounded-xl"
        >
          Deploy →
        </Button>
      </div>
    </div>
  )
}
