import { useParams, useNavigate, useSearchParams } from 'react-router-dom'
import { useState } from 'react'
import { useModel, useHfModel } from '@/hooks/useModels'
import { useAutoscalerDetection, useDetailedCapacity } from '@/hooks/useAutoscaler'
import { useRuntimesStatus } from '@/hooks/useRuntimes'
import { DeploymentForm } from '@/components/deployments/DeploymentForm'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Loader2, ArrowLeft, Cpu, HardDrive, Layers, ExternalLink, Info } from 'lucide-react'
import { GpuFitIndicator } from '@/components/models/GpuFitIndicator'
import { ThroughputEstimate } from '@/components/models/ThroughputEstimate'
import { getEngineDisplayName } from '@/lib/deploymentDisplay'
import { getGpuFitCapacityDisplay } from '@/lib/gpu-fit-capacity'
import { buildThroughputParams } from '@/lib/gpu-throughput-params'
import { useGpuThroughput } from '@/hooks/useGpuOperator'

type WeightQuant = 'fp16' | 'fp8'
type KvQuant = 'fp16' | 'fp8'

const WEIGHT_QUANT_LABELS: Record<WeightQuant, string> = {
  fp16: 'FP16 / BF16 (Default)',
  fp8: 'FP8',
}

const KV_QUANT_LABELS: Record<KvQuant, string> = {
  fp16: 'FP16 / BF16 (Default)',
  fp8: 'FP8',
}

// Lightweight info tooltip (native title + hover popover), matching the pattern
// used elsewhere in the deployment UI.
function InfoHint({ text }: { text: string }) {
  return (
    <span className="relative group/hint inline-flex">
      <button
        type="button"
        tabIndex={0}
        aria-label={text}
        title={text}
        className="inline-flex items-center justify-center text-muted-foreground hover:text-foreground transition-colors"
      >
        <Info className="h-3.5 w-3.5" />
      </button>
      <span
        role="tooltip"
        className="pointer-events-none absolute bottom-full left-1/2 -translate-x-1/2 mb-2 w-max max-w-xs rounded-lg border border-white/10 bg-[#0F1419]/95 backdrop-blur-md px-3 py-1.5 text-sm text-popover-foreground shadow-md opacity-0 transition-opacity group-hover/hint:opacity-100 group-focus-within/hint:opacity-100 z-50"
      >
        {text}
      </span>
    </span>
  )
}

export function DeployPage() {
  const { modelId } = useParams<{ modelId: string }>()
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const decodedModelId = modelId ? decodeURIComponent(modelId) : undefined
  const isHfSource = searchParams.get('source') === 'hf'

  // Precision controls for the throughput estimate. Weight precision and
  // KV-cache precision are independent knobs (KV cache often stays fp16/bf16
  // even when weights are quantized).
  const [weightQuant, setWeightQuant] = useState<WeightQuant>('fp16')
  const [kvCacheDtype, setKvCacheDtype] = useState<KvQuant>('fp16')

  // Use appropriate hook based on source
  const localModelQuery = useModel(isHfSource ? undefined : decodedModelId)
  const hfModelQuery = useHfModel(isHfSource ? decodedModelId : undefined)

  const { data: model, isLoading: modelLoading, error } = isHfSource ? hfModelQuery : localModelQuery
  const { data: detailedCapacity } = useDetailedCapacity()
  const { data: autoscaler } = useAutoscalerDetection()
  const { data: runtimesData, isLoading: runtimesLoading } = useRuntimesStatus()
  const gpuFitCapacity = getGpuFitCapacityDisplay(detailedCapacity)

  // Estimated inference throughput for this model on the cluster's GPUs.
  const throughputParams = model
    ? buildThroughputParams(model, detailedCapacity, { quantization: weightQuant, kvCacheDtype })
    : undefined
  const { data: throughput, isLoading: throughputLoading } = useGpuThroughput(
    throughputParams ?? {},
    { enabled: !!throughputParams }
  )

  // The backend downgrades an FP8 KV cache to FP16 on GPUs without a native FP8
  // datapath (only Ada Lovelace and Hopper — L40S/L4/H100/H200 — support it).
  // Surface that to the user.
  const kvDowngraded =
    kvCacheDtype === 'fp8' &&
    !!throughput?.kvCacheDtype &&
    throughput.kvCacheDtype !== 'fp8'

  // Block deploying with FP8 on hardware that has no FP8 datapath. The estimate
  // response carries the resolved GPU's FP8 capability so we don't re-implement
  // the GPU→generation mapping client-side.
  const fp8Selected = weightQuant === 'fp8' || kvCacheDtype === 'fp8'
  const fp8Blocked = fp8Selected && throughput?.fp8Supported === false
  const fp8BlockReason = fp8Blocked
    ? `FP8 is only supported on L40S/L4 and H100/H200 GPUs. This cluster's GPU${throughput?.gpuModel ? ` (${throughput.gpuModel})` : ''} can't run FP8 — choose FP16 / BF16 to deploy.`
    : undefined

  // FP8 selected but we couldn't confirm GPU support: the estimate has settled
  // (not in-flight) yet carries no fp8Supported signal — e.g. it errored or
  // 404'd (no GPU pool with known specs). We don't block (the hardware may
  // support FP8 even if we lack specs), but we warn so an unsupported flag isn't
  // sent silently. Mutually exclusive with fp8Blocked (false → blocked;
  // undefined → unknown; true → fine).
  const fp8CapabilityUnknown =
    fp8Selected && !throughputLoading && throughput?.fp8Supported === undefined

  // High-confidence "model does not fit": the backend had real architecture
  // details and the KV budget left no room for even one sequence on the
  // estimate's assumed topology. We surface a non-blocking warning (the user may
  // still pick more GPUs-per-replica in the form than the estimate assumed, which
  // can make it fit) rather than disabling Deploy outright.
  const doesNotFit = !!throughput?.doesNotFit && !throughput?.lowConfidence
  const doesNotFitReason = doesNotFit
    ? `This model is estimated not to fit on this cluster's GPU${throughput?.gpuModel ? ` (${throughput.gpuModel})` : ''} at ${throughput?.tpSize ?? 1} GPU${(throughput?.tpSize ?? 1) > 1 ? 's' : ''} per replica — the model weights plus reserved memory leave no room for the conversation cache. Increasing GPUs per replica below, choosing a smaller model, or using FP8 precision may help.`
    : undefined

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
        </div>

        <div className="flex flex-wrap gap-2 mt-4">
          {model.supportedEngines.map((engine) => (
            <Badge key={engine} variant="secondary" className="bg-white/[0.06] border-white/10">
              {getEngineDisplayName(engine)}
            </Badge>
          ))}
        </div>
      </div>

      {/* Performance & Precision: precision controls + speed estimate live here.
          Changing any control recomputes the estimate. FP8 selections also feed
          the real deployment (engine args) — see DeploymentForm. */}
      {throughputParams && (
        <div className="glass-panel animate-slide-up" style={{ animationDelay: '75ms', animationFillMode: 'both' }}>
          <h2 className="text-lg font-semibold mb-1">Performance &amp; Precision</h2>
          <p className="text-sm text-muted-foreground mb-4">
            Estimated speed for this model on your cluster's GPUs. Adjust precision to
            explore the tradeoffs — FP8 choices are applied to the deployment.
          </p>

          <div className="flex flex-wrap items-start gap-4">
            <div className="space-y-1.5">
              <div className="flex items-center gap-2">
                <Label htmlFor="weight-quant">
                  Model Weights Precision
                </Label>
                <InfoHint text="How compactly the model's weights are stored. Lower precision (FP8) uses less GPU memory and runs faster, but can slightly reduce answer quality. FP8 needs a recent GPU (L40S/L4 or H100/H200)." />
              </div>
              <Select value={weightQuant} onValueChange={(v) => setWeightQuant(v as WeightQuant)}>
                <SelectTrigger id="weight-quant" className="h-8 w-44 text-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {(Object.keys(WEIGHT_QUANT_LABELS) as WeightQuant[]).map((q) => (
                    <SelectItem key={q} value={q}>
                      {WEIGHT_QUANT_LABELS[q]}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="space-y-1.5">
              <div className="flex items-center gap-2">
                <Label htmlFor="kv-quant">
                  KV Cache Precision
                </Label>
                <InfoHint text="Precision of the running conversation memory the model keeps while generating. Lower precision lets the GPU handle more simultaneous chats. FP8 needs a recent GPU (L40S/L4 or H100/H200)." />
              </div>
              <Select value={kvCacheDtype} onValueChange={(v) => setKvCacheDtype(v as KvQuant)}>
                <SelectTrigger id="kv-quant" className="h-8 w-44 text-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {(Object.keys(KV_QUANT_LABELS) as KvQuant[]).map((q) => (
                    <SelectItem key={q} value={q}>
                      {KV_QUANT_LABELS[q]}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="mt-4 flex items-center gap-2 text-sm">
            <ThroughputEstimate estimate={throughput} isLoading={throughputLoading} />
          </div>

          {kvDowngraded && (
            <p className="mt-2 text-xs text-yellow-500/90">
              FP8 KV cache isn’t supported on{throughput?.gpuModel ? ` ${throughput.gpuModel}` : ' this GPU'} — showing the estimate with standard precision instead.
            </p>
          )}
          {fp8Blocked && (
            <p className="mt-2 text-xs text-destructive">
              {fp8BlockReason}
            </p>
          )}
          {fp8CapabilityUnknown && (
            <p className="mt-2 text-xs text-yellow-500/90">
              We couldn’t verify FP8 support for this cluster’s GPUs. FP8 will still be applied —
              if the hardware doesn’t support it, the model may fail to start. Choose FP16 / BF16 if unsure.
            </p>
          )}
        </div>
      )}

      {/* Deployment Form */}
      <div className="animate-slide-up" style={{ animationDelay: '100ms', animationFillMode: 'both' }}>
        <DeploymentForm
          model={model}
          detailedCapacity={detailedCapacity}
          autoscaler={autoscaler}
          runtimes={runtimesData?.runtimes}
          weightQuant={weightQuant}
          kvCacheDtype={kvCacheDtype}
          fp8Blocked={fp8Blocked}
          fp8BlockReason={fp8BlockReason}
          doesNotFit={doesNotFit}
          doesNotFitReason={doesNotFitReason}
        />
      </div>
    </div>
  )
}
