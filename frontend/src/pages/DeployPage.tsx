import { useParams, useNavigate, useSearchParams } from 'react-router-dom'
import { useModel, useHfModel } from '@/hooks/useModels'
import { useAutoscalerDetection, useDetailedCapacity } from '@/hooks/useAutoscaler'
import { useRuntimesStatus } from '@/hooks/useRuntimes'
import { DeploymentForm } from '@/components/deployments/DeploymentForm'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Loader2, ArrowLeft, Cpu, HardDrive, Layers, ExternalLink } from 'lucide-react'
import { GpuFitIndicator } from '@/components/models/GpuFitIndicator'
import { ThroughputEstimate } from '@/components/models/ThroughputEstimate'
import { getGpuFitCapacityDisplay } from '@/lib/gpu-fit-capacity'
import { buildThroughputParams } from '@/lib/gpu-throughput-params'
import { useGpuThroughput } from '@/hooks/useGpuOperator'

export function DeployPage() {
  const { modelId } = useParams<{ modelId: string }>()
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const decodedModelId = modelId ? decodeURIComponent(modelId) : undefined
  const isHfSource = searchParams.get('source') === 'hf'

  // Use appropriate hook based on source
  const localModelQuery = useModel(isHfSource ? undefined : decodedModelId)
  const hfModelQuery = useHfModel(isHfSource ? decodedModelId : undefined)

  const { data: model, isLoading: modelLoading, error } = isHfSource ? hfModelQuery : localModelQuery
  const { data: detailedCapacity } = useDetailedCapacity()
  const { data: autoscaler } = useAutoscalerDetection()
  const { data: runtimesData, isLoading: runtimesLoading } = useRuntimesStatus()
  const gpuFitCapacity = getGpuFitCapacityDisplay(detailedCapacity)

  // Estimated inference throughput for this model on the cluster's GPUs.
  const throughputParams = model ? buildThroughputParams(model, detailedCapacity) : undefined
  const { data: throughput, isLoading: throughputLoading } = useGpuThroughput(
    throughputParams ?? {},
    { enabled: !!throughputParams }
  )

  // Wait for both model and runtimes to load before showing the form
  // This ensures the runtime selector is visible when the form renders
  if (modelLoading || runtimesLoading) {
    return (
      <div className="flex items-center justify-center py-12">
        <Loader2 className="h-8 w-8 animate-spin text-muted-foreground" />
      </div>
    )
  }

  if (error || !model) {
    return (
      <div className="flex flex-col items-center justify-center py-12 text-center">
        <p className="text-lg font-medium text-destructive">
          Model not found
        </p>
        <p className="text-sm text-muted-foreground mt-1 mb-4">
          The requested model could not be found
        </p>
        <Button onClick={() => navigate('/')}>
          Back to Catalog
        </Button>
      </div>
    )
  }

  return (
    <div className="space-y-6 max-w-4xl mx-auto animate-slide-up">
      <div className="flex items-center gap-4">
        <Button variant="ghost" size="icon" onClick={() => navigate('/')}>
          <ArrowLeft className="h-5 w-5" />
        </Button>
        <div>
          <h1 className="text-3xl font-heading">Deploy Model</h1>
          <p className="text-muted-foreground mt-1">
            Configure and deploy {model.name}
          </p>
        </div>
      </div>

      {/* Model Summary Card */}
      <div className="glass-panel animate-slide-up" style={{ animationDelay: '50ms', animationFillMode: 'both' }}>
        <div className="flex items-start justify-between gap-4 mb-4">
          <div>
            <div className="flex items-center gap-2">
              <h2 className="text-xl font-semibold">{model.name}</h2>
              {model.fromHfSearch && (
                <a
                  href={`https://huggingface.co/${model.id}`}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-muted-foreground hover:text-foreground transition-colors"
                >
                  <ExternalLink className="h-4 w-4" />
                </a>
              )}
            </div>
            <p className="text-sm text-muted-foreground">{model.id}</p>
            {model.gated && (
              <Badge variant="outline" className="mt-2 text-yellow-500 border-yellow-500/50 bg-yellow-500/10">
                Gated Model
              </Badge>
            )}
          </div>
          <Badge variant="outline" className="text-lg px-3 py-1 border-white/10 bg-white/[0.03]">
            {model.size}
          </Badge>
        </div>

        <p className="text-muted-foreground mb-4">{model.description}</p>

        <div className="flex flex-wrap gap-4 text-sm">
          <div className="flex items-center gap-2 text-muted-foreground">
            <Cpu className="h-4 w-4" />
            {model.estimatedGpuMemoryGb && detailedCapacity?.totalMemoryGb ? (
                <GpuFitIndicator
                  estimatedGpuMemoryGb={model.estimatedGpuMemoryGb}
                  clusterCapacityGb={detailedCapacity.totalMemoryGb}
                  gpuCount={gpuFitCapacity.gpuCount}
                  capacityLabel={gpuFitCapacity.capacityLabel}
                />
            ) : (
              <span>GPU: {model.estimatedGpuMemory || model.minGpuMemory || 'N/A'}</span>
            )}
          </div>

          {model.contextLength && (
            <div className="flex items-center gap-2 text-muted-foreground">
              <Layers className="h-4 w-4" />
              <span>Context: {model.contextLength.toLocaleString()}</span>
            </div>
          )}

          <div className="flex items-center gap-2 text-muted-foreground">
            <HardDrive className="h-4 w-4" />
            <span>{model.conversational ? 'Chat' : model.task === 'image-text-to-text' ? 'Multimodal' : 'Text Generation'}</span>
          </div>

          {throughputParams && (throughputLoading || throughput) && (
            <ThroughputEstimate estimate={throughput} isLoading={throughputLoading} />
          )}
        </div>

        <div className="flex flex-wrap gap-2 mt-4">
          {model.supportedEngines.map((engine) => (
            <Badge key={engine} variant="secondary" className="bg-white/[0.06] border-white/10">
              {engine.toUpperCase()}
            </Badge>
          ))}
        </div>
      </div>

      {/* Deployment Form */}
      <div className="animate-slide-up" style={{ animationDelay: '100ms', animationFillMode: 'both' }}>
        <DeploymentForm
          model={model}
          detailedCapacity={detailedCapacity}
          autoscaler={autoscaler}
          runtimes={runtimesData?.runtimes}
        />
      </div>
    </div>
  )
}
