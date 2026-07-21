import { useState, useEffect, useRef, useCallback, useMemo, type ReactNode } from 'react'
import { useNavigate, Link } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import { Switch } from '@/components/ui/switch'
import { Badge } from '@/components/ui/badge'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useConfetti } from '@/components/ui/confetti'
import { useCreateDeployment, usePVCs, type DeploymentConfig } from '@/hooks/useDeployments'
import { useHuggingFaceStatus, useGgufFiles } from '@/hooks/useHuggingFace'
import { usePremadeModels } from '@/hooks/useAikit'
import { useGatewayStatus } from '@/hooks/useGateway'
import { useToast } from '@/hooks/useToast'
import { generateDeploymentName, cn } from '@/lib/utils'
import { type Model, type DetailedClusterCapacity, type AutoscalerDetectionResult, type RuntimeStatus, type PremadeModel, type AIConfiguratorResult, aikitApi, vllmRecipesApi, type Engine, type KaitoResourceType, type VllmRecipeIndexEntry, type VllmRecipeResolveRequest, type VllmRecipeResolveResult } from '@/lib/api'
import { getEngineDisplayName } from '@/lib/deploymentDisplay'
import { ChevronDown, AlertCircle, Rocket, CheckCircle2, Sparkles, AlertTriangle, Server, Cpu, Box, Loader2, HardDrive } from 'lucide-react'
import { CapacityWarning } from './CapacityWarning'
import { AIConfiguratorPanel } from './AIConfiguratorPanel'
import { ManifestViewer } from './ManifestViewer'
import { CostEstimate } from './CostEstimate'
import { StorageVolumesSection } from './StorageVolumesSection'
import { calculateGpuRecommendation, calculateMultiNode, type GpuRecommendation, type MultiNodeRecommendation } from '@/lib/gpu-recommendations'

// Reusable GPU per Replica field component
interface GpuPerReplicaFieldProps {
  id: string
  value: number
  onChange: (value: number) => void
  maxGpus?: number
  recommendation: GpuRecommendation
  aiConfigRecommended?: number | null
  multiNode?: MultiNodeRecommendation | null
}

function GpuPerReplicaField({ id, value, onChange, maxGpus = 8, recommendation, aiConfigRecommended, multiNode }: GpuPerReplicaFieldProps) {
  const isAiOptimized = aiConfigRecommended != null && value === aiConfigRecommended
  const isRecommended = value === recommendation.recommendedGpus

  return (
    <div className="space-y-2">
      <Label htmlFor={id} className="flex items-center gap-2">
        GPUs per Replica
        {isAiOptimized ? (
          <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
            <Sparkles className="h-3 w-3" />
            Optimized
          </span>
        ) : isRecommended && (
          <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
            <Sparkles className="h-3 w-3" />
            Recommended
          </span>
        )}
      </Label>
      <Input
        id={id}
        type="number"
        min={1}
        max={maxGpus}
        value={value}
        onChange={(e) => onChange(parseInt(e.target.value) || 1)}
      />
      <p className="text-xs text-muted-foreground">
        {recommendation.reason}
        {recommendation.alternatives && recommendation.alternatives.length > 0 && (
          <span className="block mt-1">
            Consider: {recommendation.alternatives.join(', ')} GPUs
          </span>
        )}
      </p>
      {multiNode && (
        <div className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs font-medium bg-purple-100 dark:bg-purple-900/40 text-purple-700 dark:text-purple-300 border border-purple-200 dark:border-purple-800">
          Multi-Node ({multiNode.nodeCount} nodes × {multiNode.gpusPerNode} GPUs = {multiNode.totalGpus} total)
        </div>
      )}
    </div>
  )
}

interface RecipeMetricProps {
  label: string
  children: ReactNode
}

function RecipeMetric({ label, children }: RecipeMetricProps) {
  return (
    <div className="min-w-0 rounded-xl border border-border/70 bg-background/40 p-3 shadow-soft-xs dark:border-white/10 dark:bg-white/[0.03]">
      <span className="sr-only">{label}: {children}</span>
      <span aria-hidden="true" className="text-[11px] font-medium uppercase tracking-[0.14em] text-muted-foreground">
        {label}
      </span>
      <strong aria-hidden="true" className="mt-1 block break-words text-sm font-semibold leading-5 text-foreground">
        {children}
      </strong>
    </div>
  )
}

interface RecipeCodePanelProps {
  title: string
  value: unknown
}

function RecipeCodePanel({ title, value }: RecipeCodePanelProps) {
  return (
    <div className="min-w-0 overflow-hidden rounded-xl border border-border/70 bg-background/70 shadow-soft-xs dark:border-white/10 dark:bg-[#0b1118]/90">
      <div className="flex items-center justify-between gap-3 border-b border-border/70 bg-muted/30 px-3 py-2 dark:border-white/10 dark:bg-white/[0.03]">
        <span className="truncate font-mono text-xs font-semibold text-foreground">{title}</span>
        <Badge variant="outline" className="shrink-0 border-border/70 px-2 py-0 text-[10px] text-muted-foreground dark:border-white/10">
          JSON
        </Badge>
      </div>
      <pre className="max-h-56 overflow-auto p-3 font-mono text-xs leading-5 text-muted-foreground">
        {JSON.stringify(value, null, 2)}
      </pre>
    </div>
  )
}

interface DeploymentFormProps {
  model: Model
  detailedCapacity?: DetailedClusterCapacity
  autoscaler?: AutoscalerDetectionResult
  runtimes?: RuntimeStatus[]
  /** Weight precision chosen on the Deploy page (FP8 emits an engine arg). */
  weightQuant?: 'fp16' | 'fp8'
  /** KV-cache precision chosen on the Deploy page (FP8 emits an engine arg). */
  kvCacheDtype?: 'fp16' | 'fp8'
  /**
   * True when FP8 was selected but the target GPU has no FP8 datapath. Disables
   * the Deploy button so we never submit a flag the engine can't honor.
   */
  fp8Blocked?: boolean
  /** Human-readable reason shown when fp8Blocked is true. */
  fp8BlockReason?: string
  /**
   * True when the throughput estimate determined (with high confidence) that the
   * model does not fit on the cluster's GPU at the estimated topology. Surfaced
   * as a non-blocking warning near the Deploy button — it does NOT disable
   * deploying, since the user may select more GPUs per replica than the estimate
   * assumed.
   */
  doesNotFit?: boolean
  /** Human-readable reason shown when doesNotFit is true. */
  doesNotFitReason?: string
}

// Subset of Engine type for traditional GPU inference engines (excludes llama.cpp)
type TraditionalEngine = 'vllm' | 'sglang' | 'trtllm'
type RouterMode = 'default' | 'kv' | 'round-robin'
type DeploymentMode = 'aggregated' | 'disaggregated'
type KaitoComputeType = 'cpu' | 'gpu'
type GgufRunMode = 'build' | 'direct'

const TENSOR_PARALLEL_SIZE_ARG = 'tensor-parallel-size'
const PIPELINE_PARALLEL_SIZE_ARG = 'pipeline-parallel-size'
// FP8 precision engine flags (vLLM / SGLang). Only emitted when FP8 is selected
// on FP8-capable hardware; FP16/BF16 is the engine default and needs no flag.
const QUANTIZATION_ARG = 'quantization'
const KV_CACHE_DTYPE_ARG = 'kv-cache-dtype'
// Engines that accept the generic --quantization / --kv-cache-dtype flags.
// TRT-LLM uses a different mechanism and KAITO ignores generic engine args.
const FP8_ARG_ENGINES: TraditionalEngine[] = ['vllm', 'sglang']
const SUPPORTED_ENGINE_IDS: Engine[] = ['vllm', 'sglang', 'trtllm', 'llamacpp']
const DIRECT_VLLM_NIGHTLY_IMAGE = 'vllm/vllm-openai:cu130-nightly'
const DIRECT_VLLM_STABLE_IMAGE = 'vllm/vllm-openai:latest'

type DirectVllmImageChoice = 'nightly' | 'stable' | 'custom'

const FALLBACK_RUNTIME_INFO: Record<string, { name: string; description: string; defaultNamespace: string }> = {
  dynamo: {
    name: 'NVIDIA Dynamo',
    description: 'High-performance inference with KV-cache routing and disaggregated serving',
    defaultNamespace: 'dynamo-system',
  },
  kuberay: {
    name: 'KubeRay',
    description: 'Ray-based serving with autoscaling and distributed inference',
    defaultNamespace: 'ray-system',
  },
  kaito: {
    name: 'KAITO',
    description: 'Flexible inference with GGUF (llama.cpp) and vLLM support',
    defaultNamespace: 'kaito-workspace',
  },
  vllm: {
    name: 'Direct vLLM',
    description: 'Direct vLLM provider for newest model support and configurable launch images',
    defaultNamespace: 'default',
  },
  llmd: {
    name: 'llm-d',
    description: 'GPU-accelerated vLLM inference with disaggregated prefill/decode support',
    defaultNamespace: 'default',
  },
}

const FALLBACK_RUNTIME_ENGINES: Record<string, Engine[]> = {
  dynamo: ['vllm', 'sglang', 'trtllm'],
  kuberay: ['vllm'],
  kaito: ['vllm', 'llamacpp'],
  vllm: ['vllm'],
  llmd: ['vllm'],
}

const FALLBACK_RUNTIME_MODES: Record<string, DeploymentMode[]> = {
  dynamo: ['aggregated', 'disaggregated'],
  kuberay: ['aggregated', 'disaggregated'],
  kaito: ['aggregated'],
  vllm: ['aggregated'],
  llmd: ['aggregated', 'disaggregated'],
}

const RUNTIME_SELECTION_PRIORITY = ['dynamo', 'kuberay', 'kaito', 'llmd', 'vllm']

function isEngine(value: string): value is Engine {
  return SUPPORTED_ENGINE_IDS.includes(value as Engine)
}

function isDeploymentMode(value: string): value is DeploymentMode {
  return value === 'aggregated' || value === 'disaggregated'
}

function uniqueValues<T>(values: T[]): T[] {
  return Array.from(new Set(values))
}

function getPrioritizedRuntimes(runtimes: RuntimeStatus[]): RuntimeStatus[] {
  const priority = new Map(RUNTIME_SELECTION_PRIORITY.map((id, index) => [id, index]))
  return [...runtimes].sort((left, right) => {
    const leftPriority = priority.get(left.id) ?? Number.MAX_SAFE_INTEGER
    const rightPriority = priority.get(right.id) ?? Number.MAX_SAFE_INTEGER
    return leftPriority - rightPriority
  })
}

function getRuntimeEngines(runtime?: RuntimeStatus): Engine[] {
  const discoveredEngines = runtime?.capabilities?.engines?.filter(isEngine) ?? []
  if (discoveredEngines.length > 0) return discoveredEngines
  return runtime?.id ? (FALLBACK_RUNTIME_ENGINES[runtime.id] ?? []) : []
}

function getRuntimeModes(runtime?: RuntimeStatus, engine?: Engine): DeploymentMode[] {
  // Direct vLLM and KAITO are single-mode deployment methods in this UI even
  // when generic runtime fixtures or compatibility mirrors carry broader modes.
  if (runtime?.id === 'vllm' || runtime?.id === 'kaito') {
    return FALLBACK_RUNTIME_MODES[runtime.id]
  }

  const perEngineModes = runtime?.capabilities?.engineCapabilities
    ?.filter((capability) => !engine || capability.name === engine)
    .flatMap((capability) => capability.servingModes || [])
    .filter(isDeploymentMode) ?? []

  if (perEngineModes.length > 0) return uniqueValues(perEngineModes)

  const discoveredModes = runtime?.capabilities?.modes?.filter(isDeploymentMode) ?? []
  if (discoveredModes.length > 0) return discoveredModes

  return runtime?.id ? (FALLBACK_RUNTIME_MODES[runtime.id] ?? ['aggregated']) : ['aggregated']
}

function runtimeSupportsMode(runtime: RuntimeStatus | undefined, mode: DeploymentMode, engine?: Engine): boolean {
  return getRuntimeModes(runtime, engine).includes(mode)
}

function isRuntimeCompatible(runtime: RuntimeStatus, modelEngines: Engine[], mode: DeploymentMode = 'aggregated'): boolean {
  const runtimeEngines = getRuntimeEngines(runtime)
  return modelEngines.some((engine) => runtimeEngines.includes(engine) && runtimeSupportsMode(runtime, mode, engine))
}

function normalizeGatewayAvailability(
  config: DeploymentConfig,
  gatewayAvailable: boolean | undefined
): DeploymentConfig {
  if (gatewayAvailable !== false || !('gatewayEnabled' in config)) {
    return config
  }

  const nextConfig = { ...config }
  delete nextConfig.gatewayEnabled
  return nextConfig
}

// Extract nodeCount from providerOverrides structure
function getNodeCountFromOverrides(overrides?: Record<string, unknown>): number {
  if (!overrides) return 1;
  const spec = overrides.spec as Record<string, unknown> | undefined;
  const services = spec?.services as Record<string, unknown> | undefined;
  const vllmWorker = services?.VllmWorker as Record<string, unknown> | undefined;
  const multinode = vllmWorker?.multinode as Record<string, unknown> | undefined;
  const nodeCount = multinode?.nodeCount as number | undefined;
  return nodeCount && nodeCount > 1 ? nodeCount : 1;
}

function getNumericEngineArg(engineArgs: Record<string, unknown> | undefined, key: string): number | undefined {
  const value = engineArgs?.[key];
  if (typeof value === 'number' && Number.isFinite(value)) return value;
  if (typeof value === 'string') {
    const parsed = Number(value);
    if (Number.isFinite(parsed)) return parsed;
  }
  return undefined;
}

function buildDynamoMultiNodeOverrides(nodeCount: number): Record<string, unknown> {
  return {
    spec: {
      services: {
        VllmWorker: {
          multinode: {
            nodeCount,
          },
        },
      },
    },
  };
}

function setDynamoParallelismEngineArgs(
  engineArgs: Record<string, unknown> | undefined,
  multiNode: MultiNodeRecommendation | null
): Record<string, unknown> | undefined {
  const nextEngineArgs = { ...(engineArgs || {}) };

  if (multiNode) {
    nextEngineArgs[TENSOR_PARALLEL_SIZE_ARG] = String(multiNode.gpusPerNode);
    nextEngineArgs[PIPELINE_PARALLEL_SIZE_ARG] = String(multiNode.pipelineParallelSize);
  } else {
    delete nextEngineArgs[TENSOR_PARALLEL_SIZE_ARG];
    delete nextEngineArgs[PIPELINE_PARALLEL_SIZE_ARG];
  }

  return Object.keys(nextEngineArgs).length > 0 ? nextEngineArgs : undefined;
}

/**
 * Merge or strip the FP8 precision engine args (`quantization`,
 * `kv-cache-dtype`) without clobbering other args. Sets a key to `fp8` when the
 * corresponding precision is FP8 and the engine supports the flag; otherwise
 * removes ONLY a value this control owns (`fp8`). A user-provided non-fp8 value
 * (e.g. `awq`/`gptq` typed into the advanced engine-args editor) is preserved,
 * since FP16/BF16 is merely the engine default and shouldn't override an
 * explicit user choice.
 */
export function setFp8PrecisionEngineArgs(
  engineArgs: Record<string, unknown> | undefined,
  opts: { weightFp8: boolean; kvFp8: boolean }
): Record<string, unknown> | undefined {
  const nextEngineArgs = { ...(engineArgs || {}) };

  if (opts.weightFp8) {
    nextEngineArgs[QUANTIZATION_ARG] = 'fp8';
  } else if (nextEngineArgs[QUANTIZATION_ARG] === 'fp8') {
    // Only strip the value WE set. A user-provided non-fp8 quantization
    // (e.g. awq, gptq) from the advanced engine-args editor is preserved.
    delete nextEngineArgs[QUANTIZATION_ARG];
  }

  if (opts.kvFp8) {
    nextEngineArgs[KV_CACHE_DTYPE_ARG] = 'fp8';
  } else if (nextEngineArgs[KV_CACHE_DTYPE_ARG] === 'fp8') {
    delete nextEngineArgs[KV_CACHE_DTYPE_ARG];
  }

  return Object.keys(nextEngineArgs).length > 0 ? nextEngineArgs : undefined;
}

function getDirectVllmImageRef(choice: DirectVllmImageChoice, customImage: string): string {
  switch (choice) {
    case 'stable':
      return DIRECT_VLLM_STABLE_IMAGE
    case 'custom':
      return customImage.trim()
    case 'nightly':
    default:
      return DIRECT_VLLM_NIGHTLY_IMAGE
  }
}

function getDirectVllmImageSelectionForRef(imageRef: string): { choice: DirectVllmImageChoice; customImage: string } {
  if (imageRef === DIRECT_VLLM_NIGHTLY_IMAGE) {
    return { choice: 'nightly', customImage: '' }
  }

  if (imageRef === DIRECT_VLLM_STABLE_IMAGE) {
    return { choice: 'stable', customImage: '' }
  }

  return { choice: 'custom', customImage: imageRef }
}

function normalizeDirectVllmConfig(
  baseConfig: DeploymentConfig,
  imageRef: string,
  fallbackModelId: string
): DeploymentConfig {
  return {
    ...baseConfig,
    provider: 'vllm',
    engine: 'vllm',
    modelSource: 'vllm',
    modelId: baseConfig.modelId || fallbackModelId,
    imageRef,
    resources: {
      ...baseConfig.resources,
      gpu: Math.max(1, baseConfig.resources?.gpu ?? 1),
    },
  }
}

export function DeploymentForm({ model, detailedCapacity, autoscaler, runtimes, weightQuant = 'fp16', kvCacheDtype = 'fp16', fp8Blocked = false, fp8BlockReason, doesNotFit = false, doesNotFitReason }: DeploymentFormProps) {
  const navigate = useNavigate()
  const { toast } = useToast()
  const createDeployment = useCreateDeployment()
  const { data: hfStatus } = useHuggingFaceStatus()
  const { data: premadeModels } = usePremadeModels()
  const { data: gatewayInfo } = useGatewayStatus()
  const formRef = useRef<HTMLFormElement>(null)
  const { trigger: triggerConfetti, ConfettiComponent } = useConfetti(2500)

  // Check if this is a gated model and HF is not configured
  const isGatedModel = model.gated === true
  const needsHfAuth = isGatedModel && !hfStatus?.configured

  const getRuntimeDefaultNamespace = (runtime: string): string => {
    const runtimeStatus = runtimes?.find(r => r.id === runtime)
    return runtimeStatus?.defaultNamespace || FALLBACK_RUNTIME_INFO[runtime]?.defaultNamespace || 'default'
  }

  const getRuntimeDisplayName = (runtime: string): string => {
    const runtimeStatus = runtimes?.find(r => r.id === runtime)
    return runtimeStatus?.name || FALLBACK_RUNTIME_INFO[runtime]?.name || runtime || 'selected runtime'
  }

  const getRuntimeDescription = (runtime: RuntimeStatus): string => (
    runtime.description || FALLBACK_RUNTIME_INFO[runtime.id]?.description || 'No description available'
  )

  // Determine default runtime from provider discovery: prefer compatible and installed runtime.
  const getDefaultRuntime = (): string => {
    if (!runtimes || runtimes.length === 0) {
      return model.supportedEngines.includes('llamacpp') ? 'kaito' : 'dynamo'
    }

    const prioritizedRuntimes = getPrioritizedRuntimes(runtimes)
    const compatibleInstalled = prioritizedRuntimes.find(
      (runtime) => runtime.installed && isRuntimeCompatible(runtime, model.supportedEngines)
    )
    if (compatibleInstalled) return compatibleInstalled.id

    const compatible = prioritizedRuntimes.find((runtime) =>
      isRuntimeCompatible(runtime, model.supportedEngines)
    )
    if (compatible) return compatible.id

    return prioritizedRuntimes.find((runtime) => runtime.installed)?.id || prioritizedRuntimes[0]?.id || 'dynamo'
  }
  const [selectedRuntime, setSelectedRuntime] = useState<string>(getDefaultRuntime)
  const runtimeManuallySelectedRef = useRef(false)
  const selectedRuntimeStatus = runtimes?.find(r => r.id === selectedRuntime)
  const isSelectedCrdLessRuntime = selectedRuntimeStatus?.requiresCRD === false
  const isSelectedCrdLessRuntimeNotReady = isSelectedCrdLessRuntime && !selectedRuntimeStatus?.installed
  const isRuntimeInstalled = selectedRuntimeStatus?.installed ?? false

  // AI Configurator state - tracks supported backends and recommended mode
  const [aiConfigSupportedBackends, setAiConfigSupportedBackends] = useState<string[] | null>(null)
  const [aiConfigRecommendedBackend, setAiConfigRecommendedBackend] = useState<string | null>(null)
  const [aiConfigRecommendedMode, setAiConfigRecommendedMode] = useState<DeploymentMode | null>(null)
  const [topologyManagedByAIConfig, setTopologyManagedByAIConfig] = useState(false)
  // Track AI Configurator recommended values for disaggregated mode
  const [aiConfigRecommendedValues, setAiConfigRecommendedValues] = useState<{
    prefillReplicas?: number
    decodeReplicas?: number
    prefillGpus?: number
    decodeGpus?: number
    gpuPerReplica?: number
  } | null>(null)

  // KAITO-specific state
  const [kaitoComputeType, setKaitoComputeType] = useState<KaitoComputeType>('cpu')
  const [kaitoResourceType, setKaitoResourceType] = useState<KaitoResourceType>('workspace')
  const [selectedPremadeModel, setSelectedPremadeModel] = useState<PremadeModel | null>(null)
  const [ggufFile, setGgufFile] = useState<string>('')
  const [ggufRunMode, setGgufRunMode] = useState<GgufRunMode>('direct')
  const [maxModelLen, setMaxModelLen] = useState<number | undefined>(undefined)

  // Direct vLLM image and recipe state
  const [directVllmImageChoice, setDirectVllmImageChoice] = useState<DirectVllmImageChoice>('nightly')
  const [directVllmCustomImage, setDirectVllmCustomImage] = useState('')
  const [vllmRecipes, setVllmRecipes] = useState<VllmRecipeIndexEntry[]>([])
  const [vllmRecipesSource, setVllmRecipesSource] = useState('')
  const [vllmRecipesLoading, setVllmRecipesLoading] = useState(false)
  const [vllmRecipesLoaded, setVllmRecipesLoaded] = useState(false)
  const [vllmRecipesError, setVllmRecipesError] = useState<string | null>(null)
  const [vllmRecipeApplying, setVllmRecipeApplying] = useState(false)
  const [vllmRecipeWarnings, setVllmRecipeWarnings] = useState<string[]>([])
  const [resolvedVllmRecipe, setResolvedVllmRecipe] = useState<VllmRecipeResolveResult | null>(null)

  // Check if this is a HuggingFace GGUF model (not a premade model)
  // GGUF models have only llamacpp as supported engine and come from HuggingFace
  const isHuggingFaceGgufModel = model.supportedEngines.length === 1 &&
                                  model.supportedEngines[0] === 'llamacpp' &&
                                  !model.id.startsWith('kaito/');

  // Check if this is a vLLM-compatible model for KAITO
  // vLLM models have 'vllm' in supported engines but NOT 'llamacpp'
  const isVllmModel = model.supportedEngines.includes('vllm') &&
                      !model.supportedEngines.includes('llamacpp');

  // Fetch GGUF files from HuggingFace repo when it's a GGUF model and KAITO is selected
  const { data: ggufFilesData, isLoading: ggufFilesLoading } = useGgufFiles(
    model.id,
    isHuggingFaceGgufModel && selectedRuntime === 'kaito'
  );
  const ggufFiles = ggufFilesData?.files || [];

  // Auto-select Q4_K_M file if available, otherwise first file
  useEffect(() => {
    const files = ggufFilesData?.files || [];
    if (files.length > 0 && !ggufFile) {
      // Look for Q4_K_M variant (case-insensitive)
      const q4kmFile = files.find(f => /q4_k_m/i.test(f));
      if (q4kmFile) {
        setGgufFile(q4kmFile);
      } else {
        // Fallback to first file
        setGgufFile(files[0]);
      }
    }
  }, [ggufFilesData, ggufFile]);

  // Get supported engines for the selected runtime, filtered by model support.
  const getAvailableEngines = (): Engine[] => {
    const runtimeEngines = getRuntimeEngines(selectedRuntimeStatus)
    return model.supportedEngines.filter((engine) => runtimeEngines.includes(engine))
  }
  const availableEngines = getAvailableEngines()

  const getDefaultEngineForRuntime = (runtime: string): Engine => {
    if (model.supportedEngines.length === 1) {
      return model.supportedEngines[0]
    }

    const runtimeEngines = getRuntimeEngines(runtimes?.find(r => r.id === runtime))
    return model.supportedEngines.find((engine) => runtimeEngines.includes(engine)) || model.supportedEngines[0] || 'vllm'
  }

  const defaultRuntime = getDefaultRuntime()

  const [showAdvanced, setShowAdvanced] = useState(false)
  const [config, setConfig] = useState<DeploymentConfig>({
    name: generateDeploymentName(model.id),
    namespace: getRuntimeDefaultNamespace(defaultRuntime),
    modelId: model.id,
    engine: getDefaultEngineForRuntime(defaultRuntime),
    mode: 'aggregated',
    provider: defaultRuntime,
    routerMode: 'default',
    replicas: 1,
    hfTokenSecret: import.meta.env.VITE_DEFAULT_HF_SECRET || '',
    enforceEager: true,
    enablePrefixCaching: true,
    trustRemoteCode: false,
    ...(defaultRuntime === 'vllm' ? {
      modelSource: 'vllm',
      imageRef: DIRECT_VLLM_NIGHTLY_IMAGE,
    } : {}),
    // Disaggregated mode defaults
    prefillReplicas: 1,
    decodeReplicas: 1,
    prefillGpus: 1,
    decodeGpus: 1,
    // GPU resources for aggregated mode
    resources: {
      gpu: 0, // Will be set from recommendation
    },
  })

  // If runtimes arrive after the form initializes, select the best discovered runtime.
  useEffect(() => {
    if (!runtimes || runtimes.length === 0 || runtimeManuallySelectedRef.current) return

    const runtime = getDefaultRuntime()
    if (!runtime || runtime === selectedRuntime) return

    setSelectedRuntime(runtime)
    setConfig(prev => {
      const leavingDirectVllm = prev.provider === 'vllm' && runtime !== 'vllm'
      return {
        ...prev,
        provider: runtime,
        namespace: getRuntimeDefaultNamespace(runtime),
        engine: runtime === 'vllm' ? 'vllm' : getDefaultEngineForRuntime(runtime),
        mode: runtime === 'kaito' || runtime === 'vllm' ? 'aggregated' : prev.mode,
        providerOverrides: runtime === 'vllm' ? undefined : prev.providerOverrides,
        modelSource: runtime === 'vllm' ? 'vllm' : leavingDirectVllm ? undefined : prev.modelSource,
        imageRef: runtime === 'vllm' ? (prev.imageRef || DIRECT_VLLM_NIGHTLY_IMAGE) : leavingDirectVllm ? undefined : prev.imageRef,
        recipeProvenance: runtime === 'vllm' ? prev.recipeProvenance : undefined,
      }
    })
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runtimes, selectedRuntime])

  // Fetch PVCs for the selected namespace (for existing disk selection)
  const { data: availablePVCs } = usePVCs(
    selectedRuntime === 'dynamo' ? config.namespace : undefined
  )

  // Calculate GPU recommendation based on model characteristics.
  // Memoized so the object identity is stable across renders, letting effects
  // depend on it without re-running on every render.
  const gpuRecommendation = useMemo(
    () => calculateGpuRecommendation(model, detailedCapacity),
    [model, detailedCapacity]
  )
  const recommendedMultiNodeNodeCount = gpuRecommendation.multiNode?.nodeCount
  const recommendedMultiNodeGpusPerNode = gpuRecommendation.multiNode?.gpusPerNode
  const recommendedMultiNodePipelineParallelSize = gpuRecommendation.multiNode?.pipelineParallelSize

  const currentNodeCount = getNodeCountFromOverrides(config.providerOverrides)
  const currentPipelineParallel = getNumericEngineArg(config.engineArgs, PIPELINE_PARALLEL_SIZE_ARG)
  const directVllmImageRef = getDirectVllmImageRef(directVllmImageChoice, directVllmCustomImage)
  const selectedDirectVllmImageRef = config.imageRef || directVllmImageRef
  const directVllmCustomImageRequired = selectedRuntime === 'vllm' && directVllmImageChoice === 'custom' && !directVllmImageRef
  const exactVllmRecipe = vllmRecipes.find(recipe => recipe.hf_id === model.id)

  const loadVllmRecipes = useCallback(async () => {
    setVllmRecipesLoading(true)
    setVllmRecipesError(null)

    try {
      const response = await vllmRecipesApi.list()
      setVllmRecipes(response.recipes || [])
      setVllmRecipesSource(response.source || '')
      setVllmRecipesLoaded(true)
    } catch (err) {
      setVllmRecipesError(err instanceof Error ? err.message : 'Failed to load vLLM recipes')
    } finally {
      setVllmRecipesLoading(false)
    }
  }, [])

  const handleApplyVllmRecipe = async () => {
    const recipeModelId = model.id
    const imageRef = getDirectVllmImageRef(directVllmImageChoice, directVllmCustomImage)

    if (!exactVllmRecipe) {
      toast({
        title: 'No recipe match',
        description: 'No official vLLM recipe exactly matches this model id.',
        variant: 'destructive',
      })
      return
    }

    if (directVllmImageChoice === 'custom' && !imageRef) {
      toast({
        title: 'Launch image required',
        description: 'Enter a vLLM launch image before applying a recipe.',
        variant: 'destructive',
      })
      return
    }

    const imageChoice: VllmRecipeResolveRequest['imageChoice'] = directVllmImageChoice === 'custom'
      ? { type: 'custom', imageRef }
      : { type: 'recipe' }
    const resolveRequest: VllmRecipeResolveRequest = {
      modelId: recipeModelId,
      mode: config.mode,
      imageChoice,
    }

    setVllmRecipeApplying(true)
    setVllmRecipesError(null)
    setVllmRecipeWarnings([])

    try {
      const resolved = await vllmRecipesApi.resolve(resolveRequest)
      setResolvedVllmRecipe(resolved)
      setVllmRecipeWarnings(resolved.warnings || [])
      const resolvedImageRef = resolved.imageRef || imageRef
      const resolvedImageSelection = getDirectVllmImageSelectionForRef(resolvedImageRef)
      setDirectVllmImageChoice(resolvedImageSelection.choice)
      setDirectVllmCustomImage(resolvedImageSelection.customImage)
      setConfig(prev => normalizeDirectVllmConfig({
        ...prev,
        modelId: recipeModelId,
        engine: resolved.engine || 'vllm',
        mode: resolved.mode || prev.mode,
        resources: {
          ...prev.resources,
          ...resolved.resources,
        },
        engineArgs: resolved.engineArgs,
        engineExtraArgs: resolved.engineExtraArgs,
        env: {
          ...(prev.env || {}),
          ...(resolved.env || {}),
        },
        recipeProvenance: resolved.recipeProvenance,
        providerOverrides: undefined,
        routerMode: 'default',
      }, resolvedImageRef, recipeModelId))

      toast({
        title: 'Recipe applied',
        description: `${recipeModelId} settings were applied to this Direct vLLM deployment.`,
      })
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Failed to apply vLLM recipe'
      setVllmRecipesError(message)
      toast({
        title: 'Recipe failed',
        description: message,
        variant: 'destructive',
      })
    } finally {
      setVllmRecipeApplying(false)
    }
  }

  // Auto-populate HF token secret when user is logged in
  useEffect(() => {
    if (hfStatus?.configured && !config.hfTokenSecret) {
      setConfig(prev => ({ ...prev, hfTokenSecret: 'hf-token-secret' }))
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hfStatus?.configured])

  // Clear stale gatewayEnabled when gateway support disappears; keep the default-on UI
  // state implicit so untouched deployments omit spec.gateway.
  useEffect(() => {
    setConfig(prev => normalizeGatewayAvailability(prev, gatewayInfo?.available))
  }, [gatewayInfo?.available])

  useEffect(() => {
    if (selectedRuntime === 'vllm' && !vllmRecipesLoaded && !vllmRecipesLoading) {
      void loadVllmRecipes()
    }
  }, [loadVllmRecipes, selectedRuntime, vllmRecipesLoaded, vllmRecipesLoading])

  useEffect(() => {
    if (selectedRuntime !== 'vllm') return

    setConfig(prev => {
      const next = normalizeDirectVllmConfig(prev, directVllmImageRef, model.id)
      if (
        prev.provider === next.provider &&
        prev.engine === next.engine &&
        prev.modelSource === next.modelSource &&
        prev.imageRef === next.imageRef &&
        prev.modelId === next.modelId &&
        prev.resources?.gpu === next.resources?.gpu
      ) {
        return prev
      }
      return next
    })
  }, [selectedRuntime, directVllmImageRef, model.id])

  // Set initial GPU value from recommendation when component mounts
  useEffect(() => {
    if (config.resources?.gpu === 0 && gpuRecommendation.recommendedGpus > 0) {
      setConfig(prev => ({
        ...prev,
        resources: {
          ...prev.resources,
          gpu: gpuRecommendation.recommendedGpus
        }
      }))
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gpuRecommendation.recommendedGpus])

  // Separate effect: apply/clear multi-node config when recommendation changes (e.g. after capacity loads)
  // Only applies to aggregated-mode Dynamo + vLLM deployments.
  useEffect(() => {
    const shouldManageDynamoParallelism =
      selectedRuntime === 'dynamo' &&
      config.mode === 'aggregated' &&
      config.engine === 'vllm';

    if (topologyManagedByAIConfig) {
      return;
    }

    setConfig(prev => {
      const prevNodeCount = getNodeCountFromOverrides(prev.providerOverrides)
      const prevTensorParallel = getNumericEngineArg(prev.engineArgs, TENSOR_PARALLEL_SIZE_ARG)
      const prevPipelineParallel = getNumericEngineArg(prev.engineArgs, PIPELINE_PARALLEL_SIZE_ARG)

      if (!shouldManageDynamoParallelism) {
        if (prevNodeCount <= 1 && prevTensorParallel === undefined && prevPipelineParallel === undefined) {
          return prev
        }

        return {
          ...prev,
          providerOverrides: undefined,
          engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, null),
        }
      }

      if (
        recommendedMultiNodeNodeCount !== undefined &&
        recommendedMultiNodeGpusPerNode !== undefined &&
        recommendedMultiNodePipelineParallelSize !== undefined
      ) {
        const recommendedPipelineParallel = recommendedMultiNodePipelineParallelSize
        const gpuCount = prev.resources?.gpu ?? 0
        const recommendedMultiNode: MultiNodeRecommendation = {
          nodeCount: recommendedMultiNodeNodeCount,
          gpusPerNode: recommendedMultiNodeGpusPerNode,
          totalGpus: recommendedMultiNodeNodeCount * recommendedMultiNodeGpusPerNode,
          pipelineParallelSize: recommendedMultiNodePipelineParallelSize,
        }

        // Intentionally compare against the previous config inside setState so
        // manual topology edits do not trigger this effect and get snapped back.
        if (
          prevNodeCount === recommendedMultiNode.nodeCount &&
          prevTensorParallel === recommendedMultiNode.gpusPerNode &&
          prevPipelineParallel === recommendedPipelineParallel &&
          gpuCount === gpuRecommendation.recommendedGpus
        ) {
          return prev
        }

        return {
          ...prev,
          resources: {
            ...prev.resources,
            gpu: gpuRecommendation.recommendedGpus,
          },
          providerOverrides: buildDynamoMultiNodeOverrides(recommendedMultiNode.nodeCount),
          engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, recommendedMultiNode),
        }
      }

      if (prevNodeCount <= 1 && prevTensorParallel === undefined && prevPipelineParallel === undefined) {
        return prev
      }

      return {
        ...prev,
        providerOverrides: undefined,
        engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, null),
      }
    })
  }, [
    recommendedMultiNodeGpusPerNode,
    recommendedMultiNodeNodeCount,
    recommendedMultiNodePipelineParallelSize,
    gpuRecommendation.recommendedGpus,
    selectedRuntime,
    config.engine,
    config.mode,
    topologyManagedByAIConfig,
  ])

  // Apply (or strip) FP8 precision engine args based on the Deploy page's
  // precision dropdowns. Only emitted for engines that accept the generic flags
  // (vLLM / SGLang) and never when FP8 is blocked on hardware without an FP8
  // datapath.
  useEffect(() => {
    const engineSupportsFp8Args = FP8_ARG_ENGINES.includes(config.engine as TraditionalEngine)
    const weightFp8 = weightQuant === 'fp8' && engineSupportsFp8Args && !fp8Blocked
    const kvFp8 = kvCacheDtype === 'fp8' && engineSupportsFp8Args && !fp8Blocked

    setConfig(prev => {
      const nextEngineArgs = setFp8PrecisionEngineArgs(prev.engineArgs, { weightFp8, kvFp8 })
      const prevQuant = prev.engineArgs?.[QUANTIZATION_ARG]
      const prevKv = prev.engineArgs?.[KV_CACHE_DTYPE_ARG]
      const nextQuant = nextEngineArgs?.[QUANTIZATION_ARG]
      const nextKv = nextEngineArgs?.[KV_CACHE_DTYPE_ARG]
      if (prevQuant === nextQuant && prevKv === nextKv) {
        return prev
      }
      return { ...prev, engineArgs: nextEngineArgs }
    })
  }, [weightQuant, kvCacheDtype, fp8Blocked, config.engine])

  // Auto-select matching premade model when navigating with a KAITO model from Models page
  useEffect(() => {
    if (premadeModels && premadeModels.length > 0 && !selectedPremadeModel) {
      // Try to match model.id (e.g., 'kaito/llama3.2-1b') to premade model id (e.g., 'llama3.2:1b')
      const modelIdWithoutPrefix = model.id.replace('kaito/', '').replace('-', ':');
      const matchingPremade = premadeModels.find(pm => pm.id === modelIdWithoutPrefix);
      if (matchingPremade) {
        setSelectedPremadeModel(matchingPremade);
        setConfig(prev => ({
          ...prev,
          name: generateDeploymentName(matchingPremade.id),
          modelId: matchingPremade.id,
        }));
      }
    }
  }, [premadeModels, model.id, selectedPremadeModel])

  // Handle runtime change - update namespace and engine
  const handleRuntimeChange = (runtime: string) => {
    runtimeManuallySelectedRef.current = true
    setTopologyManagedByAIConfig(false)
    setSelectedRuntime(runtime)
    const runtimeEngines = getRuntimeEngines(runtimes?.find(r => r.id === runtime))
    const newAvailableEngines = model.supportedEngines.filter((engine) => runtimeEngines.includes(engine))
    const currentEngineSupported = newAvailableEngines.includes(config.engine)

    setConfig(prev => {
      const nextEngine = currentEngineSupported ? prev.engine : getDefaultEngineForRuntime(runtime)
      const shouldManageDynamoParallelism =
        runtime === 'dynamo' &&
        prev.mode === 'aggregated' &&
        nextEngine === 'vllm'

      let newEngineArgs = setDynamoParallelismEngineArgs(prev.engineArgs, null)
      let newProviderOverrides = shouldManageDynamoParallelism ? prev.providerOverrides : undefined

      // When switching TO Dynamo + vLLM, recalculate multi-node from current GPU config.
      if (shouldManageDynamoParallelism) {
        const estimatedMem = gpuRecommendation.estimatedMemoryGb;
        const gpuMem = detailedCapacity?.totalMemoryGb;
        const currentGpu = prev.resources?.gpu || gpuRecommendation.recommendedGpus || 1;

        if (estimatedMem && gpuMem) {
          const multiNodeResult = calculateMultiNode(estimatedMem, gpuMem, currentGpu);
          if (multiNodeResult) {
            newProviderOverrides = buildDynamoMultiNodeOverrides(multiNodeResult.nodeCount)
            newEngineArgs = setDynamoParallelismEngineArgs(newEngineArgs, multiNodeResult)
          } else {
            newProviderOverrides = undefined;
          }
        }
      }

      const leavingDirectVllm = prev.provider === 'vllm' && runtime !== 'vllm'

      return {
        ...prev,
        provider: runtime,
        namespace: getRuntimeDefaultNamespace(runtime),
        // Reset engine if current one isn't supported by new runtime
        engine: runtime === 'vllm' ? 'vllm' : nextEngine,
        // Reset router mode if switching away from Dynamo
        routerMode: runtime === 'dynamo' ? prev.routerMode : 'default',
        // Start single-runtime providers in standard aggregated mode
        mode: runtime === 'kaito' || runtime === 'vllm' ? 'aggregated' : prev.mode,
        providerOverrides: runtime === 'vllm' ? undefined : newProviderOverrides,
        engineArgs: newEngineArgs,
        modelSource: runtime === 'vllm' ? 'vllm' : leavingDirectVllm ? undefined : prev.modelSource,
        imageRef: runtime === 'vllm' ? directVllmImageRef : leavingDirectVllm ? undefined : prev.imageRef,
        recipeProvenance: runtime === 'vllm' ? prev.recipeProvenance : undefined,
      }
    })

    // Reset KAITO-specific state when switching away from KAITO
    if (runtime !== 'kaito') {
      setSelectedPremadeModel(null)
      setKaitoComputeType('cpu')
    }

    // Reset AI Configurator state when switching away from Dynamo
    // This ensures optimization badges are cleared when changing providers
    if (runtime !== 'dynamo') {
      setAiConfigSupportedBackends(null)
      setAiConfigRecommendedBackend(null)
      setAiConfigRecommendedMode(null)
      setAiConfigRecommendedValues(null)
      // Clear storage config (storage volumes are only for Dynamo)
      setConfig(prev => ({ ...prev, storage: undefined }))
    }
  }

  // Handle premade model selection for KAITO (also used in auto-selection useEffect above)
  const handlePremadeModelSelect = useCallback((premadeModel: PremadeModel) => {
    setSelectedPremadeModel(premadeModel)
    setConfig(prev => ({
      ...prev,
      name: generateDeploymentName(premadeModel.id),
      modelId: premadeModel.id,
    }))
  }, [])

  // Use the handler to ensure it's not considered unused
  void handlePremadeModelSelect;

  // Keyboard shortcut: Cmd/Ctrl+Enter to submit
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
        e.preventDefault()
        if (!createDeployment.isProcessing && !needsHfAuth) {
          formRef.current?.requestSubmit()
        }
      }
    }

    document.addEventListener('keydown', handleKeyDown)
    return () => document.removeEventListener('keydown', handleKeyDown)
  }, [createDeployment.isProcessing, needsHfAuth])

  const handleSubmit = useCallback(async (e: React.FormEvent) => {
    e.preventDefault()

    try {
      // Build the deployment config, adding KAITO-specific fields if needed
      let deployConfig = normalizeGatewayAvailability(config, gatewayInfo?.available)

      if (selectedRuntime === 'vllm') {
        if (directVllmCustomImageRequired) {
          throw new Error('Enter a vLLM launch image before deploying.')
        }

        deployConfig = normalizeDirectVllmConfig(deployConfig, deployConfig.imageRef || directVllmImageRef, model.id)
      }

      if (selectedRuntime === 'kaito') {
        // Add kaitoResourceType to all KAITO deployments
        deployConfig = { ...deployConfig, kaitoResourceType }

        if (isHuggingFaceGgufModel) {
          if (ggufRunMode === 'direct') {
            // Direct run mode - no Docker/build required
            // The runner image will download the model at runtime using huggingface:// URI
            deployConfig = {
              ...deployConfig,
              modelSource: 'huggingface',
              modelId: model.id,
              ggufFile: ggufFile,
              ggufRunMode: 'direct',
              computeType: kaitoComputeType,
            }
          } else {
            // Build mode - requires Docker and building an image

            // Check if build infrastructure (Docker) is available
            toast({
              title: 'Checking Build Infrastructure',
              description: 'Verifying Docker and build tools are available...',
            })

            const infraStatus = await aikitApi.getInfrastructureStatus()
            if (!infraStatus.ready) {
              const errorMsg = infraStatus.error ||
                (!infraStatus.builder.running ? 'Docker is not running. Please start Docker and try again.' :
                  !infraStatus.registry.ready ? 'Container registry is not available.' :
                 'Build infrastructure is not ready.')
              throw new Error(errorMsg)
            }

            // Build the image first
            toast({
              title: 'Building Image',
              description: `Building GGUF model image for ${model.id}. This may take a few minutes...`,
            })

            const buildResult = await aikitApi.build({
              modelSource: 'huggingface',
              modelId: model.id,
              ggufFile: ggufFile,
            })

            if (!buildResult.success || !buildResult.imageRef) {
              throw new Error(buildResult.error || 'Failed to build model image')
            }

            toast({
              title: 'Image Built Successfully',
              description: `Image: ${buildResult.imageRef}`,
              variant: 'success',
            })

            // Use the built image in the deployment config
            deployConfig = {
              ...deployConfig,
              modelSource: 'huggingface',
              modelId: model.id,
              ggufFile: ggufFile,
              ggufRunMode: 'build',
              imageRef: buildResult.imageRef,
              computeType: kaitoComputeType,
            }
          }
        } else if (isVllmModel) {
          // vLLM model via KAITO - GPU always required
          const gpuCount = config.resources?.gpu || 1;
          deployConfig = {
            ...deployConfig,
            modelSource: 'vllm',
            modelId: model.id,
            computeType: 'gpu',  // vLLM always requires GPU
            resources: { gpu: gpuCount },
            ...(maxModelLen && { maxModelLen }),
            ...(config.hfTokenSecret && { hfTokenSecret: config.hfTokenSecret }),
          }
        } else {
          // Premade model
          deployConfig = {
            ...deployConfig,
            modelSource: 'premade',
            computeType: kaitoComputeType,
            premadeModel: selectedPremadeModel?.id,
          }
        }
      }

      await createDeployment.mutateAsync(deployConfig)

      // Trigger confetti celebration!
      triggerConfetti()

      toast({
        title: 'Deployment Created',
        description: `${config.name} is being deployed`,
        variant: 'success',
      })

      // Delay navigation slightly to let user see confetti
      setTimeout(() => {
        navigate('/deployments')
      }, 1500)
    } catch (error) {
      toast({
        title: 'Deployment Failed',
        description: error instanceof Error ? error.message : 'Failed to create deployment',
        variant: 'destructive',
      })
    }
  }, [config, createDeployment, navigate, toast, triggerConfetti, selectedRuntime, directVllmCustomImageRequired, directVllmImageRef, kaitoComputeType, kaitoResourceType, selectedPremadeModel, isHuggingFaceGgufModel, isVllmModel, model.id, model.gated, ggufFile, ggufRunMode, maxModelLen, gatewayInfo?.available])

  const updateConfig = <K extends keyof DeploymentConfig>(
    key: K,
    value: DeploymentConfig[K]
  ) => {
    setConfig((prev) => ({ ...prev, [key]: value }))
  }

  // Handler for applying AI Configurator recommendations
  const handleApplyAIConfig = useCallback((result: AIConfiguratorResult) => {
    const cfg = result.config

    // Map AI Configurator backend to our engine type
    const backendToEngine: Record<string, Engine> = {
      'vllm': 'vllm',
      'sglang': 'sglang',
      'trtllm': 'trtllm',
    }
    const recommendedEngine = result.backend ? backendToEngine[result.backend] : undefined

    // Store supported backends info for engine selection UI
    if (result.supportedBackends) {
      setAiConfigSupportedBackends(result.supportedBackends)
    }
    if (result.backend) {
      setAiConfigRecommendedBackend(result.backend)
    }

    // Store recommended mode
    setAiConfigRecommendedMode(result.mode)

    // Store recommended values for badges
    setAiConfigRecommendedValues({
      prefillReplicas: cfg.prefillReplicas,
      decodeReplicas: cfg.decodeReplicas,
      prefillGpus: cfg.prefillTensorParallel || cfg.tensorParallelDegree,
      decodeGpus: cfg.decodeTensorParallel || cfg.tensorParallelDegree,
      gpuPerReplica: cfg.tensorParallelDegree,
    })
    setTopologyManagedByAIConfig(true)

    setConfig(prev => {
      const nextEngine = recommendedEngine || prev.engine
      const pipelineParallelDegree = Math.max(1, cfg.pipelineParallelDegree || 1)
      const shouldApplyDynamoParallelism =
        selectedRuntime === 'dynamo' &&
        result.mode === 'aggregated' &&
        nextEngine === 'vllm' &&
        pipelineParallelDegree > 1

      const multiNodeConfig: MultiNodeRecommendation | null = shouldApplyDynamoParallelism
        ? {
            nodeCount: pipelineParallelDegree,
            gpusPerNode: cfg.tensorParallelDegree,
            totalGpus: pipelineParallelDegree * cfg.tensorParallelDegree,
            pipelineParallelSize: pipelineParallelDegree,
          }
        : null

      const engineArgs = setDynamoParallelismEngineArgs(
        {
          ...prev.engineArgs,
          'max-num-batched-tokens': cfg.maxBatchSize,
          'gpu-memory-utilization': cfg.gpuMemoryUtilization,
          ...(cfg.maxNumSeqs && { 'max-num-seqs': cfg.maxNumSeqs }),
        },
        multiNodeConfig
      )

      return {
        ...prev,
        mode: result.mode,
        replicas: result.replicas,
        contextLength: cfg.maxModelLen,
        // Set engine if AI Configurator recommended one
        ...(recommendedEngine && { engine: recommendedEngine }),
        resources: {
          ...prev.resources,
          gpu: cfg.tensorParallelDegree,
        },
        providerOverrides: multiNodeConfig ? buildDynamoMultiNodeOverrides(multiNodeConfig.nodeCount) : undefined,
        // Disaggregated mode settings
        ...(result.mode === 'disaggregated' && {
          prefillReplicas: cfg.prefillReplicas || 1,
          decodeReplicas: cfg.decodeReplicas || 1,
          prefillGpus: cfg.prefillTensorParallel || cfg.tensorParallelDegree,
          decodeGpus: cfg.decodeTensorParallel || cfg.tensorParallelDegree,
        }),
        // Engine args for advanced settings
        engineArgs,
      }
    })

    const engineInfo = recommendedEngine ? `, Engine=${recommendedEngine.toUpperCase()}` : ''
    const pipelineInfo = cfg.pipelineParallelDegree && cfg.pipelineParallelDegree > 1
      ? `, PP=${cfg.pipelineParallelDegree}`
      : ''
    toast({
      title: 'Configuration Applied',
      description: `AI Configurator recommendations applied. TP=${cfg.tensorParallelDegree}${pipelineInfo}, Context=${cfg.maxModelLen}${engineInfo}`,
      variant: 'success',
    })
  }, [selectedRuntime, toast])

  // Calculate total GPUs needed for the deployment
  const calculateSelectedGpus = (): number => {
    if (config.mode === 'disaggregated') {
      // For disaggregated, calculate total GPUs across all workers
      const prefillTotal = (config.prefillReplicas || 1) * (config.prefillGpus || 1);
      const decodeTotal = (config.decodeReplicas || 1) * (config.decodeGpus || 1);
      return prefillTotal + decodeTotal;
    }
    // For aggregated, multiply GPUs per replica by number of replicas
    const gpusPerReplica = config.resources?.gpu || gpuRecommendation.recommendedGpus || 1;
    const replicas = config.replicas || 1;

    // Account for multi-node: nodeCount multiplies the per-replica GPU count
    return gpusPerReplica * replicas * currentNodeCount;
  }

  const selectedGpus = calculateSelectedGpus()
  const supportsDisaggregatedMode = runtimeSupportsMode(selectedRuntimeStatus, 'disaggregated', config.engine)

  // Compute current multi-node state from providerOverrides
  const currentMultiNode: MultiNodeRecommendation | null = (() => {
    if (currentNodeCount <= 1) return null;
    const gpusPerNode = config.resources?.gpu || gpuRecommendation.recommendedGpus || 1;
    return {
      nodeCount: currentNodeCount,
      gpusPerNode,
      totalGpus: currentNodeCount * gpusPerNode,
      pipelineParallelSize: currentPipelineParallel || currentNodeCount,
    };
  })()

  // Calculate the maximum GPUs per single pod (for node placement constraints)
  const maxGpusPerPod = config.mode === 'disaggregated'
    ? Math.max(config.prefillGpus || 1, config.decodeGpus || 1)
    : (config.resources?.gpu || gpuRecommendation.recommendedGpus || 1);

  // Check if KAITO configuration is valid
  // For HuggingFace GGUF models, we need a ggufFile for both direct and build modes
  // For vLLM models, we need at least 1 GPU
  // For premade, we need a selected model
  const isKaitoConfigValid = selectedRuntime !== 'kaito' ||
    (isHuggingFaceGgufModel
      ? ggufFile.endsWith('.gguf')
      : isVllmModel
        ? (config.resources?.gpu || 0) >= 1
        : selectedPremadeModel !== null)

  // Status-aware button content
  const getButtonContent = () => {
    if (needsHfAuth) {
      return 'HuggingFace Auth Required'
    }

    if (fp8Blocked) {
      return 'FP8 Not Supported on This GPU'
    }

    if (!isRuntimeInstalled) {
      return isSelectedCrdLessRuntimeNotReady ? 'Runtime Not Ready' : 'Runtime Not Installed'
    }

    if (selectedRuntime === 'kaito' && !isHuggingFaceGgufModel && !isVllmModel && !selectedPremadeModel) {
      return 'Select a Model'
    }

    if (selectedRuntime === 'kaito' && isHuggingFaceGgufModel && !ggufFile.endsWith('.gguf')) {
      return 'Select GGUF File'
    }

    if (selectedRuntime === 'kaito' && isVllmModel && (config.resources?.gpu || 0) < 1) {
      return 'Configure GPUs'
    }

    switch (createDeployment.status) {
      case 'validating':
        return 'Validating...'
      case 'submitting':
        return 'Deploying...'
      case 'success':
        return (
          <>
            <CheckCircle2 className="h-4 w-4" />
            Deployed!
          </>
        )
      default:
        return (
          <>
            <Rocket className="h-4 w-4" />
            Deploy Model
            <kbd className="hidden sm:inline-flex ml-2 px-1.5 py-0.5 text-[10px] font-mono bg-primary-foreground/20 rounded">
              ⌘↵
            </kbd>
          </>
        )
    }
  }

  return (
    <>
      <ConfettiComponent count={60} />
      <form ref={formRef} onSubmit={handleSubmit} className="space-y-6">
      {/* Gated Model Warning */}
      {needsHfAuth && (
        <div className="rounded-lg bg-yellow-50 dark:bg-yellow-950 border border-yellow-200 dark:border-yellow-800 p-4">
          <div className="flex items-start gap-3">
            <AlertCircle className="h-5 w-5 text-yellow-600 dark:text-yellow-400 mt-0.5 flex-shrink-0" />
            <div>
              <h3 className="font-medium text-yellow-800 dark:text-yellow-200">
                HuggingFace Authentication Required
              </h3>
              <p className="text-sm text-yellow-700 dark:text-yellow-300 mt-1">
                <strong>{model.name}</strong> is a gated model that requires HuggingFace authentication.
                Please{' '}
                  <a
                    href="/settings"
                  className="underline font-medium hover:text-yellow-900 dark:hover:text-yellow-100"
                >
                  sign in with HuggingFace
                </a>{' '}
                in Settings before deploying.
              </p>
            </div>
          </div>
        </div>
      )}

      {/* Runtime Selection */}
      {runtimes && runtimes.length > 0 && (
        <div className="glass-panel">
          <h3 className="text-lg font-semibold flex items-center gap-2 mb-4">
            <Server className="h-5 w-5" />
            Deployment method
          </h3>
          <div className="grid gap-4 sm:grid-cols-2">
            {runtimes.map((runtime) => {
              const runtimeId = runtime.id
              const isCompatible = isRuntimeCompatible(runtime, model.supportedEngines, config.mode)
              const isSelected = selectedRuntime === runtimeId
              const displayName = getRuntimeDisplayName(runtimeId)
              const description = getRuntimeDescription(runtime)
              const isCrdLessRuntime = runtime.requiresCRD === false
              const isCrdLessRuntimeNotReady = isCrdLessRuntime && !runtime.installed

              return (
                <div
                  key={runtimeId}
                  role="radio"
                  aria-checked={isSelected}
                  tabIndex={isCompatible ? 0 : -1}
                  onClick={() => {
                    if (isCompatible) {
                      handleRuntimeChange(runtimeId)
                    }
                  }}
                  onKeyDown={(e) => {
                    if (isCompatible && (e.key === 'Enter' || e.key === ' ')) {
                      e.preventDefault()
                      handleRuntimeChange(runtimeId)
                    }
                  }}
                  className={cn(
                    "relative flex items-start space-x-3 rounded-xl border p-4 transition-all duration-200 bg-white/[0.02]",
                    !isCompatible && "opacity-50 cursor-not-allowed",
                    isCompatible && "cursor-pointer",
                    isCompatible && isSelected
                      ? "border-cyan-400/50 bg-cyan-500/5 shadow-[0_0_15px_rgba(0,217,255,0.15)]"
                      : "border-white/5",
                    isCompatible && !isSelected && "hover:border-white/10 hover:bg-white/[0.03]",
                    isCompatible && !runtime.installed && "opacity-75"
                  )}
                >
                  {/* Custom radio indicator */}
                  <div
                    className={cn(
                      "mt-1 h-4 w-4 rounded-full border flex items-center justify-center shrink-0",
                      isSelected ? "border-cyan-400" : "border-muted-foreground/50",
                      !isCompatible && "opacity-50"
                    )}
                  >
                    {isSelected && (
                      <div className="h-2.5 w-2.5 rounded-full bg-cyan-400" />
                    )}
                  </div>
                  <div className="flex-1 space-y-1">
                    <div className="flex items-center gap-2">
                      <span
                        className={cn(
                          "font-medium text-sm",
                          isCompatible ? "cursor-pointer" : "cursor-not-allowed"
                        )}
                      >
                        {displayName}
                      </span>
                      {!isCompatible ? (
                        <Badge variant="outline" className="text-muted-foreground border-muted text-xs">
                          Not Compatible
                        </Badge>
                      ) : runtime.installed ? (
                        <Badge variant="outline" className="text-green-400 border-green-500/50 bg-green-500/10 text-xs">
                          <CheckCircle2 className="h-3 w-3 mr-1" />
                          {isCrdLessRuntime ? 'Registered' : 'Installed'}
                        </Badge>
                      ) : isCrdLessRuntimeNotReady ? (
                        <Badge variant="outline" className="text-yellow-400 border-yellow-500/50 bg-yellow-500/10 text-xs">
                          <AlertTriangle className="h-3 w-3 mr-1" />
                          Not Ready
                        </Badge>
                      ) : (
                        <Badge variant="outline" className="text-yellow-400 border-yellow-500/50 bg-yellow-500/10 text-xs">
                          <AlertTriangle className="h-3 w-3 mr-1" />
                          Not Installed
                        </Badge>
                      )}
                    </div>
                    <p className="text-xs text-muted-foreground">
                      {description}
                    </p>
                    {!isCompatible && (
                      <p className="text-xs text-muted-foreground mt-1">
                        This model requires {model.supportedEngines.includes('llamacpp') ? 'llama.cpp' : model.supportedEngines.join('/')} which is not supported by this deployment method.
                      </p>
                    )}
                    {isCompatible && !runtime.installed && isSelected && (
                      <p className="text-xs text-yellow-600 dark:text-yellow-400 mt-2">
                        {isCrdLessRuntime ? (
                          'Provider is registered but not ready yet.'
                        ) : (
                          <>
                            <Link to="/installation" className="underline hover:no-underline">
                              Install {displayName}
                            </Link>{' '}
                            before deploying.
                          </>
                        )}
                      </p>
                    )}
                  </div>
                </div>
              )
            })}
          </div>
        </div>
      )}

      {selectedRuntime === 'vllm' && (
        <div className="rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-900 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-100">
          <div className="flex items-start gap-3">
            <AlertTriangle className="h-5 w-5 shrink-0 mt-0.5" />
            <div className="space-y-1">
              <p className="font-medium">Direct vLLM deployment method</p>
              <p>
                The Direct vLLM deployment method uses the direct vLLM provider to run the OpenAI-compatible vLLM model server. Use it when a model is too new for managed deployment methods. This gives you newer model support sooner, but images and settings may change quickly.
              </p>
              <p>
                Nightly images change often. The controller may surface image verification or unsupported image warnings during rollout.
              </p>
            </div>
          </div>
        </div>
      )}

      {/* AI Configurator Panel - only show for Dynamo runtime */}
      {selectedRuntime === 'dynamo' && (
        <AIConfiguratorPanel
          modelId={model.id}
          detailedCapacity={detailedCapacity}
          onApplyConfig={handleApplyAIConfig}
          onDiscard={() => {
            // Clear AI Configurator state when discarding
            setTopologyManagedByAIConfig(false)
            setAiConfigSupportedBackends(null)
            setAiConfigRecommendedBackend(null)
            setAiConfigRecommendedMode(null)
            setAiConfigRecommendedValues(null)
          }}
        />
      )}

      {/* Basic Configuration */}
      <div className="glass-panel">
        <h3 className="text-lg font-semibold mb-4">Basic Configuration</h3>
        <div className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="name">Deployment Name</Label>
            <Input
              id="name"
              value={config.name}
              onChange={(e) => updateConfig('name', e.target.value)}
              placeholder="my-deployment"
              required
              pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
            />
            <p className="text-xs text-muted-foreground">
              Lowercase letters, numbers, and hyphens only
            </p>
          </div>

          <details className="mt-4">
            <summary className="text-sm font-medium cursor-pointer text-muted-foreground hover:text-foreground">
              Advanced Settings
            </summary>
            <div className="mt-3 space-y-4">
              <div className="space-y-2">
                <Label htmlFor="namespace">Namespace</Label>
                <Input
                  id="namespace"
                  value={config.namespace}
                  onChange={(e) => updateConfig('namespace', e.target.value)}
                  placeholder={getRuntimeDefaultNamespace(selectedRuntime)}
                  required
                />
              </div>

              {gatewayInfo?.available && (
                <div className="flex items-center justify-between">
                  <div className="space-y-0.5">
                    <Label htmlFor="gateway-enabled">Gateway routing</Label>
                    <p className="text-xs text-muted-foreground">
                      Route requests to this model through the cluster gateway. Defaults to enabled when a gateway is detected.
                    </p>
                  </div>
                  <Switch
                    id="gateway-enabled"
                    checked={config.gatewayEnabled ?? true}
                    onCheckedChange={(checked) => updateConfig('gatewayEnabled', checked)}
                  />
                </div>
              )}
            </div>
          </details>
        </div>
      </div>

      {/* Engine Selection - hide Direct vLLM because vLLM is its only model server */}
      {selectedRuntime !== 'vllm' && (selectedRuntime !== 'kaito' || isVllmModel) && (
      <div className="glass-panel">
        <h3 className="text-lg font-semibold mb-4">Model server</h3>
        <div>
          {availableEngines.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No compatible model servers available for this model with {getRuntimeDisplayName(selectedRuntime)}.
            </p>
          ) : availableEngines.length === 1 ? (
            <div className="space-y-2">
              <div className="inline-flex items-center gap-2 rounded-md border border-white/10 bg-white/[0.03] px-3 py-2">
                <span className="font-medium text-sm">{getEngineDisplayName(availableEngines[0])}</span>
                <Badge variant="secondary" className="text-xs">
                  Selected automatically
                </Badge>
              </div>
              <p className="text-xs text-muted-foreground">
                {getEngineDisplayName(availableEngines[0])} is the only compatible model server for {getRuntimeDisplayName(selectedRuntime)}.
              </p>
            </div>
          ) : (
            <div className="space-y-3">
              <RadioGroup
                value={config.engine}
                onValueChange={(value) => {
                  // Only allow changing to supported backends if AI Configurator has set restrictions
                  if (!aiConfigSupportedBackends || aiConfigSupportedBackends.includes(value)) {
                    setTopologyManagedByAIConfig(false)
                    updateConfig('engine', value as Engine)
                  }
                }}
                className="grid gap-4 sm:grid-cols-3"
              >
                {availableEngines.map((engine) => {
                  const isUnavailable = aiConfigSupportedBackends !== null && !aiConfigSupportedBackends.includes(engine)
                  const isRecommended = aiConfigRecommendedBackend === engine

                  return (
                    <div
                      key={engine}
                      className={cn(
                        "flex items-center space-x-2",
                        isUnavailable && "opacity-50"
                      )}
                    >
                      <RadioGroupItem
                        value={engine}
                        id={engine}
                        disabled={isUnavailable}
                      />
                      <Label
                        htmlFor={engine}
                        className={cn(
                          isUnavailable ? "cursor-not-allowed" : "cursor-pointer",
                          "flex items-center gap-2"
                        )}
                      >
                        {getEngineDisplayName(engine)}
                        {isRecommended && (
                          <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
                            <Sparkles className="h-3 w-3" />
                            Optimized
                          </span>
                        )}
                      </Label>
                    </div>
                  )
                })}
              </RadioGroup>
              {aiConfigSupportedBackends && aiConfigSupportedBackends.length < availableEngines.length && (
                <p className="text-xs text-muted-foreground">
                  Some model servers are unavailable based on your GPU type. AI Configurator recommends {aiConfigRecommendedBackend ? getEngineDisplayName(aiConfigRecommendedBackend) : 'a compatible model server'}.
                </p>
              )}
            </div>
          )}
        </div>
      </div>
      )}

      {/* KAITO Resource Type Selection - show for KAITO runtime with vLLM models */}
      {selectedRuntime === 'kaito' && isVllmModel && (
      <div className="glass-panel">
        <h3 className="text-lg font-semibold flex items-center gap-2 mb-4">
          <Box className="h-5 w-5" />
          KAITO Resource Type
        </h3>
        <div>
          <RadioGroup
            value={kaitoResourceType}
            onValueChange={(value) => setKaitoResourceType(value as KaitoResourceType)}
            className="grid gap-3"
          >
            <label
              htmlFor="resource-workspace-vllm"
              className={cn(
                "flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors",
                kaitoResourceType === 'workspace'
                  ? "border-primary bg-primary/5"
                  : "border-border hover:border-muted-foreground/50"
              )}
            >
              <RadioGroupItem value="workspace" id="resource-workspace-vllm" className="mt-1" />
              <div className="flex-1">
                <div className="flex items-center gap-2">
                  <span className="font-medium">Workspace</span>
                  <Badge variant="secondary" className="text-xs">Stable</Badge>
                </div>
                <p className="text-xs text-muted-foreground mt-1">
                  Original KAITO resource type (v1beta1). Recommended for most deployments.
                </p>
              </div>
            </label>
            <label
              htmlFor="resource-inferenceset-vllm"
              className={cn(
                "flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors",
                kaitoResourceType === 'inferenceset'
                  ? "border-primary bg-primary/5"
                  : "border-border hover:border-muted-foreground/50"
              )}
            >
              <RadioGroupItem value="inferenceset" id="resource-inferenceset-vllm" className="mt-1" />
              <div className="flex-1">
                <span className="font-medium">InferenceSet</span>
                <p className="text-xs text-muted-foreground mt-1">
                  Newer KAITO resource type (v1alpha1). Supports flexible replica scaling.
                </p>
              </div>
            </label>
          </RadioGroup>
        </div>
      </div>
      )}

      {/* KAITO Model Configuration - only show for KAITO runtime with non-vLLM models */}
      {selectedRuntime === 'kaito' && !isVllmModel && (
        <div className="glass-panel">
          <h3 className="text-lg font-semibold flex items-center gap-2 mb-4">
            <Box className="h-5 w-5" />
            KAITO Model Configuration
          </h3>
          <div className="space-y-6">
            {/* Compute Type Selection - only for non-vLLM models (vLLM always requires GPU) */}
            <div className="space-y-3">
              <Label>Compute Type</Label>
              <RadioGroup
                value={kaitoComputeType}
                onValueChange={(value) => setKaitoComputeType(value as KaitoComputeType)}
                className="flex gap-4"
              >
                <div className="flex items-center space-x-2">
                  <RadioGroupItem value="cpu" id="compute-cpu" />
                  <Label htmlFor="compute-cpu" className="cursor-pointer flex items-center gap-1">
                    <Cpu className="h-4 w-4" />
                    CPU
                  </Label>
                </div>
                <div className="flex items-center space-x-2">
                  <RadioGroupItem value="gpu" id="compute-gpu" />
                  <Label htmlFor="compute-gpu" className="cursor-pointer flex items-center gap-1">
                    <Server className="h-4 w-4" />
                    GPU
                  </Label>
                </div>
              </RadioGroup>
              <p className="text-xs text-muted-foreground">
                  {kaitoComputeType === 'cpu'
                  ? 'Run inference on CPU compute - slower but no GPU required'
                  : 'Run inference on GPU compute - faster performance'}
              </p>
            </div>

            {/* KAITO Resource Type Selection */}
            <div className="space-y-3">
              <Label>Resource Type</Label>
              <RadioGroup
                value={kaitoResourceType}
                onValueChange={(value) => setKaitoResourceType(value as KaitoResourceType)}
                className="grid gap-3"
              >
                <label
                  htmlFor="resource-workspace"
                  className={cn(
                    "flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors",
                    kaitoResourceType === 'workspace'
                      ? "border-primary bg-primary/5"
                      : "border-border hover:border-muted-foreground/50"
                  )}
                >
                  <RadioGroupItem value="workspace" id="resource-workspace" className="mt-1" />
                  <div className="flex-1">
                    <div className="flex items-center gap-2">
                      <span className="font-medium">Workspace</span>
                      <Badge variant="secondary" className="text-xs">Stable</Badge>
                    </div>
                    <p className="text-xs text-muted-foreground mt-1">
                      Original KAITO resource type (v1beta1). Recommended for most deployments.
                    </p>
                  </div>
                </label>
                <label
                  htmlFor="resource-inferenceset"
                  className={cn(
                    "flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors",
                    kaitoResourceType === 'inferenceset'
                      ? "border-primary bg-primary/5"
                      : "border-border hover:border-muted-foreground/50"
                  )}
                >
                  <RadioGroupItem value="inferenceset" id="resource-inferenceset" className="mt-1" />
                  <div className="flex-1">
                    <span className="font-medium">InferenceSet</span>
                    <p className="text-xs text-muted-foreground mt-1">
                      Newer KAITO resource type (v1alpha1). Supports flexible replica scaling.
                    </p>
                  </div>
                </label>
              </RadioGroup>
            </div>

            {/* Run Mode Selection - only for HuggingFace GGUF models */}
            {isHuggingFaceGgufModel && (
              <div className="space-y-3">
                <Label>Run Mode</Label>
                <RadioGroup
                  value={ggufRunMode}
                  onValueChange={(value) => setGgufRunMode(value as GgufRunMode)}
                  className="grid gap-3"
                >
                  <label
                    htmlFor="run-direct"
                    className={cn(
                      "flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors",
                      ggufRunMode === 'direct'
                        ? "border-primary bg-primary/5"
                        : "border-border hover:border-muted-foreground/50"
                    )}
                  >
                    <RadioGroupItem value="direct" id="run-direct" className="mt-1" />
                    <div className="flex-1">
                      <div className="flex items-center gap-2">
                        <span className="font-medium">Direct Run</span>
                        <Badge variant="secondary" className="text-xs">Recommended</Badge>
                      </div>
                      <p className="text-xs text-muted-foreground mt-1">
                        Downloads model at runtime. No Docker required.
                      </p>
                    </div>
                  </label>
                  <label
                    htmlFor="run-build"
                    className={cn(
                      "flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors",
                      ggufRunMode === 'build'
                        ? "border-primary bg-primary/5"
                        : "border-border hover:border-muted-foreground/50"
                    )}
                  >
                    <RadioGroupItem value="build" id="run-build" className="mt-1" />
                    <div className="flex-1">
                      <span className="font-medium">Build Image</span>
                      <p className="text-xs text-muted-foreground mt-1">
                        Pre-builds container image. Requires Docker running locally.
                      </p>
                    </div>
                  </label>
                </RadioGroup>
              </div>
            )}

            {/* GGUF File Selection - for HuggingFace GGUF models */}
            {isHuggingFaceGgufModel && (
              <div className="space-y-3">
                <Label htmlFor="ggufFile">GGUF File</Label>
                {ggufFilesLoading ? (
                  <div className="flex items-center gap-2 text-sm text-muted-foreground py-2">
                    <Loader2 className="h-4 w-4 animate-spin" />
                    Loading GGUF files from repository...
                  </div>
                ) : ggufFiles.length > 0 ? (
                  <Select value={ggufFile} onValueChange={setGgufFile}>
                    <SelectTrigger>
                      <SelectValue placeholder="Select a GGUF file" />
                    </SelectTrigger>
                    <SelectContent>
                      {ggufFiles.map((file) => (
                        <SelectItem key={file} value={file}>
                          {file}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                ) : (
                  <div className="text-sm text-muted-foreground py-2">
                    No GGUF files found in this repository.
                  </div>
                )}
                <p className="text-xs text-muted-foreground">
                  Select the quantization variant to use. Q4_K_M offers a good balance of quality and size.
                </p>
              </div>
            )}
          </div>
        </div>
      )}

      {/* Deployment Mode - show for non-KAITO runtimes OR KAITO with vLLM models */}
      {(selectedRuntime !== 'kaito' || isVllmModel) && (
      <div className="glass-panel">
        <h3 className="text-lg font-semibold mb-4">Deployment Mode</h3>
        <div>
          <RadioGroup
            value={config.mode}
            onValueChange={(value) => {
              // Only allow changing to disaggregated for runtimes that advertise it
              if (supportsDisaggregatedMode || value === 'aggregated') {
                const newMode = value as DeploymentMode;
                setTopologyManagedByAIConfig(false)
                // Clear aggregated-only multi-node overrides when switching to disaggregated
                if (newMode === 'disaggregated') {
                  setConfig(prev => {
                    return {
                      ...prev,
                      mode: newMode,
                      providerOverrides: undefined,
                      engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, null),
                    };
                  })
                } else {
                  updateConfig('mode', newMode)
                }
              }
            }}
            className="grid gap-4 sm:grid-cols-2"
          >
            <div className="flex items-start space-x-2">
              <RadioGroupItem value="aggregated" id="mode-aggregated" className="mt-1" />
              <div>
                <Label htmlFor="mode-aggregated" className="cursor-pointer font-medium flex items-center gap-2">
                  Aggregated (Standard)
                  {aiConfigRecommendedMode === 'aggregated' && (
                    <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
                      <Sparkles className="h-3 w-3" />
                      Optimized
                    </span>
                  )}
                </Label>
                <p className="text-xs text-muted-foreground">
                  Combined prefill and decode on same workers
                </p>
              </div>
            </div>
            <div className={cn("flex items-start space-x-2", !supportsDisaggregatedMode && "opacity-50")}>
                  <RadioGroupItem
                    value="disaggregated"
                    id="mode-disaggregated"
                    className="mt-1"
                disabled={!supportsDisaggregatedMode}
              />
              <div>
                    <Label
                      htmlFor="mode-disaggregated"
                  className={cn("font-medium flex items-center gap-2", !supportsDisaggregatedMode ? "cursor-not-allowed" : "cursor-pointer")}
                >
                  Disaggregated (P/D)
                  {aiConfigRecommendedMode === 'disaggregated' && (
                    <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
                      <Sparkles className="h-3 w-3" />
                      Optimized
                    </span>
                  )}
                </Label>
                <p className="text-xs text-muted-foreground">
                      {selectedRuntime === 'kaito'
                    ? 'Separate prefill and decode workers - not supported by KAITO'
                    : selectedRuntime === 'vllm'
                      ? 'Use Dynamo, KubeRay, or llm-d for prefill/decode serving'
                      : 'Separate prefill and decode workers for better resource utilization'}
                </p>
              </div>
            </div>
          </RadioGroup>
        </div>
      </div>
      )}

      {/* Deployment Options - show for all runtimes with vLLM/GPU */}
      {(selectedRuntime !== 'kaito' || isVllmModel || kaitoComputeType === 'gpu') && (
      <div className="glass-panel">
        <h3 className="text-lg font-semibold mb-4">Deployment Options</h3>
        <div className="space-y-4">
          {config.mode === 'aggregated' || selectedRuntime === 'kaito' ? (
            /* Aggregated mode: single replica count (KAITO always uses aggregated) */
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2">
                <Label htmlFor="replicas">Worker Replicas</Label>
                <Input
                  id="replicas"
                  type="number"
                  min={1}
                  max={10}
                  value={config.replicas}
                  onChange={(e) => updateConfig('replicas', parseInt(e.target.value) || 1)}
                />
              </div>

              {/* GPU per Replica with recommendation */}
              <GpuPerReplicaField
                id="gpusPerReplica"
                value={config.resources?.gpu || gpuRecommendation.recommendedGpus}
                onChange={(value) => {
                  setTopologyManagedByAIConfig(false)
                  // Recalculate multi-node when GPU count changes (Dynamo + vLLM only)
                  const estimatedMem = gpuRecommendation.estimatedMemoryGb;
                  const gpuMem = detailedCapacity?.totalMemoryGb;

                  if (selectedRuntime === 'dynamo' && config.engine === 'vllm' && estimatedMem && gpuMem) {
                    const multiNodeResult = calculateMultiNode(estimatedMem, gpuMem, value);
                    if (multiNodeResult) {
                      // Model needs multi-node
                      setConfig(prev => ({
                        ...prev,
                        resources: { ...prev.resources, gpu: value },
                        providerOverrides: buildDynamoMultiNodeOverrides(multiNodeResult.nodeCount),
                        engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, multiNodeResult),
                      }))
                    } else {
                      // Model fits on a single node - clear multi-node overrides and Dynamo parallel args.
                      setConfig(prev => {
                        return {
                          ...prev,
                          resources: { ...prev.resources, gpu: value },
                          providerOverrides: undefined,
                          engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, null),
                        };
                      })
                    }
                  } else {
                    setConfig(prev => ({
                      ...prev,
                      resources: { ...prev.resources, gpu: value }
                    }))
                  }
                }}
                maxGpus={detailedCapacity?.maxNodeGpuCapacity || 8}
                recommendation={gpuRecommendation}
                aiConfigRecommended={aiConfigRecommendedValues?.gpuPerReplica}
                multiNode={currentMultiNode}
              />

              {/* Router Mode is only applicable to Dynamo provider */}
              {selectedRuntime === 'dynamo' && (
                <div className="space-y-2">
                  <Label>Router Mode</Label>
                  <RadioGroup
                    value={config.routerMode}
                    onValueChange={(value) => updateConfig('routerMode', value as RouterMode)}
                    className="flex gap-4"
                  >
                    <div className="flex items-center space-x-2">
                      <RadioGroupItem value="default" id="router-default" />
                      <Label htmlFor="router-default" className="cursor-pointer">Default</Label>
                    </div>
                    <div className="flex items-center space-x-2">
                      <RadioGroupItem value="kv" id="router-kv" />
                      <Label htmlFor="router-kv" className="cursor-pointer">KV-Aware</Label>
                    </div>
                    <div className="flex items-center space-x-2">
                      <RadioGroupItem value="round-robin" id="router-rr" />
                      <Label htmlFor="router-rr" className="cursor-pointer">Round Robin</Label>
                    </div>
                  </RadioGroup>
                </div>
              )}
            </div>
          ) : (
            /* Disaggregated mode: separate prefill/decode configuration */
            <div className="space-y-6">
              {/* Prefill Workers */}
              <div className="space-y-3">
                <h4 className="font-medium text-sm">Prefill Workers</h4>
                <div className="grid gap-4 sm:grid-cols-2">
                  <div className="space-y-2">
                    <Label htmlFor="prefillReplicas" className="flex items-center gap-2">
                      Replicas
                      {aiConfigRecommendedValues?.prefillReplicas === config.prefillReplicas && (
                        <span className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded-full text-xs font-medium bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
                          <Sparkles className="h-2.5 w-2.5" />
                        </span>
                      )}
                    </Label>
                    <Input
                      id="prefillReplicas"
                      type="number"
                      min={1}
                      max={10}
                      value={config.prefillReplicas || 1}
                      onChange={(e) => updateConfig('prefillReplicas', parseInt(e.target.value) || 1)}
                    />
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="prefillGpus" className="flex items-center gap-2">
                      GPUs per Worker
                      {aiConfigRecommendedValues?.prefillGpus === config.prefillGpus && (
                        <span className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded-full text-xs font-medium bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
                          <Sparkles className="h-2.5 w-2.5" />
                        </span>
                      )}
                    </Label>
                    <Input
                      id="prefillGpus"
                      type="number"
                      min={1}
                      max={8}
                      value={config.prefillGpus || 1}
                      onChange={(e) => updateConfig('prefillGpus', parseInt(e.target.value) || 1)}
                    />
                  </div>
                </div>
              </div>

              {/* Decode Workers */}
              <div className="space-y-3">
                <h4 className="font-medium text-sm">Decode Workers</h4>
                <div className="grid gap-4 sm:grid-cols-2">
                  <div className="space-y-2">
                    <Label htmlFor="decodeReplicas" className="flex items-center gap-2">
                      Replicas
                      {aiConfigRecommendedValues?.decodeReplicas === config.decodeReplicas && (
                        <span className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded-full text-xs font-medium bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
                          <Sparkles className="h-2.5 w-2.5" />
                        </span>
                      )}
                    </Label>
                    <Input
                      id="decodeReplicas"
                      type="number"
                      min={1}
                      max={10}
                      value={config.decodeReplicas || 1}
                      onChange={(e) => updateConfig('decodeReplicas', parseInt(e.target.value) || 1)}
                    />
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="decodeGpus" className="flex items-center gap-2">
                      GPUs per Worker
                      {aiConfigRecommendedValues?.decodeGpus === config.decodeGpus && (
                        <span className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded-full text-xs font-medium bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
                          <Sparkles className="h-2.5 w-2.5" />
                        </span>
                      )}
                    </Label>
                    <Input
                      id="decodeGpus"
                      type="number"
                      min={1}
                      max={8}
                      value={config.decodeGpus || 1}
                      onChange={(e) => updateConfig('decodeGpus', parseInt(e.target.value) || 1)}
                    />
                  </div>
                </div>
              </div>
            </div>
          )}
        </div>
      </div>
      )}

      {/* Storage Volumes - only shown for Dynamo runtime */}
      {selectedRuntime === 'dynamo' && (
        <div className="glass-panel">
          <h3 className="text-lg font-semibold flex items-center gap-2 mb-1">
            <HardDrive className="h-5 w-5" />
            Storage Volumes
            <span className="text-sm font-normal text-muted-foreground">(optional)</span>
          </h3>
          <p className="text-xs text-muted-foreground mb-4">
            Add persistent disks to speed up deployments. A <strong>Model Cache</strong> disk automatically downloads and stores model weights so restarts and scale-ups skip the download. A <strong>Compilation Cache</strong> disk stores engine compilation artifacts to avoid recompilation.
          </p>
          <StorageVolumesSection
            volumes={config.storage?.volumes || []}
            onChange={(volumes) => {
              setConfig(prev => ({
                ...prev,
                storage: volumes.length > 0 ? { volumes } : undefined,
              }))
            }}
            deploymentName={config.name}
            availablePVCs={availablePVCs}
          />
        </div>
      )}

      {/* Direct vLLM image and recipe options */}
      {selectedRuntime === 'vllm' && (
        <div className="glass-panel space-y-6">
          <div>
            <h3 className="text-lg font-semibold flex items-center gap-2 mb-1">
              <Rocket className="h-5 w-5" />
              Launch image
            </h3>
            <p className="text-xs text-muted-foreground">
              Choose the vLLM OpenAI server image that will be written to the deployment engine image.
            </p>
          </div>

          <RadioGroup
            value={directVllmImageChoice}
            onValueChange={(value) => setDirectVllmImageChoice(value as DirectVllmImageChoice)}
            className="grid gap-3 md:grid-cols-3"
          >
            <Label
              htmlFor="direct-vllm-image-nightly"
              className={cn(
                "flex cursor-pointer items-start gap-3 rounded-lg border p-4 transition-colors",
                directVllmImageChoice === 'nightly' && "border-primary bg-primary/5"
              )}
            >
              <RadioGroupItem id="direct-vllm-image-nightly" value="nightly" className="mt-0.5" />
              <div className="min-w-0 space-y-1">
                <div className="flex items-center gap-2">
                  <span className="font-medium">Nightly</span>
                  <Badge variant="secondary">Default</Badge>
                </div>
                <p className="break-all font-mono text-xs text-muted-foreground">
                  {DIRECT_VLLM_NIGHTLY_IMAGE}
                </p>
                <p className="text-xs text-muted-foreground">Newest model support.</p>
              </div>
            </Label>

            <Label
              htmlFor="direct-vllm-image-stable"
              className={cn(
                "flex cursor-pointer items-start gap-3 rounded-lg border p-4 transition-colors",
                directVllmImageChoice === 'stable' && "border-primary bg-primary/5"
              )}
            >
              <RadioGroupItem id="direct-vllm-image-stable" value="stable" className="mt-0.5" />
              <div className="min-w-0 space-y-1">
                <span className="font-medium">Stable</span>
                <p className="break-all font-mono text-xs text-muted-foreground">
                  {DIRECT_VLLM_STABLE_IMAGE}
                </p>
                <p className="text-xs text-muted-foreground">Latest stable vLLM image.</p>
              </div>
            </Label>

            <Label
              htmlFor="direct-vllm-image-custom"
              className={cn(
                "flex cursor-pointer items-start gap-3 rounded-lg border p-4 transition-colors",
                directVllmImageChoice === 'custom' && "border-primary bg-primary/5"
              )}
            >
              <RadioGroupItem id="direct-vllm-image-custom" value="custom" className="mt-0.5" />
              <div className="min-w-0 space-y-1">
                <span className="font-medium">Launch image</span>
                <p className="text-xs text-muted-foreground">Enter a vLLM-compatible launch image.</p>
              </div>
            </Label>
          </RadioGroup>

          {directVllmImageChoice === 'custom' && (
            <div className="space-y-2">
              <Label htmlFor="directVllmCustomImage" className="flex items-center gap-2">
                Launch image
                <Badge variant="outline">Required</Badge>
              </Label>
              <Input
                id="directVllmCustomImage"
                placeholder="registry.example.com/vllm-openai:tag"
                value={directVllmCustomImage}
                onChange={(e) => setDirectVllmCustomImage(e.target.value)}
                className={cn(directVllmCustomImageRequired && "border-destructive focus-visible:ring-destructive")}
              />
              {directVllmCustomImageRequired ? (
                <p className="flex items-center gap-1.5 text-xs text-destructive">
                  <AlertCircle className="h-3.5 w-3.5" />
                  Enter a vLLM launch image before applying a recipe or deploying.
                </p>
              ) : (
                <p className="text-xs text-muted-foreground">
                  This launch image will override any image from an applied recipe.
                </p>
              )}
            </div>
          )}

          <p className="rounded-lg border border-border/70 bg-background/40 px-3 py-2 font-mono text-xs text-muted-foreground dark:border-white/10 dark:bg-white/[0.03]">
            {`Selected image: ${directVllmImageRef || 'No launch image entered'}`}
          </p>

          <div className="overflow-hidden rounded-2xl border border-border/80 bg-gradient-to-br from-emerald-500/10 via-card/80 to-card p-4 shadow-soft dark:border-white/10 dark:from-emerald-500/[0.08] dark:via-white/[0.035] dark:to-white/[0.02]">
            <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
              <div className="flex min-w-0 gap-3">
                <div className="hidden h-10 w-10 shrink-0 place-items-center rounded-2xl border border-emerald-500/25 bg-emerald-500/10 text-emerald-600 dark:text-emerald-300 sm:grid">
                  <Sparkles className="h-4 w-4" />
                </div>
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2">
                    <h4 className="font-semibold text-foreground">
                      Official vLLM recipe
                    </h4>
                    {resolvedVllmRecipe && (
                      <Badge variant="success" className="gap-1 border-emerald-500/20">
                        <CheckCircle2 className="h-3 w-3" />
                        Applied
                      </Badge>
                    )}
                  </div>
                  <p className="mt-1 max-w-3xl text-xs leading-5 text-muted-foreground">
                    Airunway checks the official vLLM recipe catalog for an exact match to this model id and applies recommended launch settings when available.
                  </p>
                </div>
              </div>
              {vllmRecipesLoaded && (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="shrink-0"
                  onClick={loadVllmRecipes}
                  disabled={vllmRecipesLoading}
                >
                  {vllmRecipesLoading ? (
                    <>
                      <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                      Refreshing
                    </>
                  ) : (
                    'Refresh'
                  )}
                </Button>
              )}
            </div>

            <div className="mt-4 space-y-4">
              {vllmRecipesError && !vllmRecipesLoaded ? (
                <div className="rounded-xl border border-destructive/30 bg-destructive/10 p-4 text-sm text-destructive">
                  <div className="font-medium">Could not check official vLLM recipes</div>
                  <div className="mt-1">{vllmRecipesError}</div>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="mt-3"
                    onClick={loadVllmRecipes}
                    disabled={vllmRecipesLoading}
                  >
                    {vllmRecipesLoading ? (
                      <>
                        <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                        Retrying
                      </>
                    ) : (
                      'Retry'
                    )}
                  </Button>
                </div>
              ) : vllmRecipesLoading && !vllmRecipesLoaded ? (
                <div className="flex items-center gap-3 rounded-xl border border-border/70 bg-background/40 p-4 text-sm text-muted-foreground dark:border-white/10 dark:bg-white/[0.03]">
                  <Loader2 className="h-4 w-4 shrink-0 animate-spin text-primary" />
                  <span className="min-w-0 break-words">Checking official vLLM recipes for {model.id}…</span>
                </div>
              ) : exactVllmRecipe ? (
                <div className="rounded-xl border border-emerald-500/25 bg-emerald-500/[0.06] p-4 shadow-soft-xs dark:bg-emerald-500/[0.07]">
                  <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
                    <div className="flex min-w-0 gap-3">
                      <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full border border-emerald-500/30 bg-emerald-500/10 text-emerald-600 dark:text-emerald-300">
                        <CheckCircle2 className="h-4 w-4" />
                      </div>
                      <div className="min-w-0">
                        <div className="font-semibold text-emerald-700 dark:text-emerald-300">
                          Official vLLM recipe found
                        </div>
                        <p className="mt-1 break-all text-xs text-muted-foreground">
                          {exactVllmRecipe.hf_id}
                        </p>
                      </div>
                    </div>
                    <Button
                      type="button"
                      className="shrink-0"
                      onClick={() => void handleApplyVllmRecipe()}
                      disabled={vllmRecipeApplying || directVllmCustomImageRequired}
                    >
                      {vllmRecipeApplying ? (
                        <>
                          <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                          Applying recipe
                        </>
                      ) : (
                        'Apply recipe'
                      )}
                    </Button>
                  </div>

                  <div className="mt-4 grid gap-3 md:grid-cols-2 xl:grid-cols-3">
                    <div className="min-w-0 rounded-lg border border-border/60 bg-background/40 p-3 dark:border-white/10 dark:bg-white/[0.03]">
                      <span className="text-[11px] font-medium uppercase tracking-[0.14em] text-muted-foreground">Model</span>
                      <p className="mt-1 break-all text-xs font-medium text-foreground">{exactVllmRecipe.hf_id}</p>
                    </div>
                    <div className="min-w-0 rounded-lg border border-border/60 bg-background/40 p-3 dark:border-white/10 dark:bg-white/[0.03]">
                      <span className="text-[11px] font-medium uppercase tracking-[0.14em] text-muted-foreground">Source</span>
                      <p className="mt-1 break-all text-xs font-medium text-foreground">{vllmRecipesSource || 'Official vLLM catalog'}</p>
                    </div>
                    <div className="min-w-0 rounded-lg border border-border/60 bg-background/40 p-3 dark:border-white/10 dark:bg-white/[0.03] md:col-span-2 xl:col-span-1">
                      <span className="text-[11px] font-medium uppercase tracking-[0.14em] text-muted-foreground">Default image</span>
                      <p className="mt-1 break-all text-xs font-medium text-foreground">
                        {directVllmImageChoice === 'custom'
                          ? selectedDirectVllmImageRef || 'Custom image required'
                          : 'Official recipe image'}
                      </p>
                    </div>
                  </div>
                </div>
              ) : vllmRecipesLoaded ? (
                <div className="rounded-xl border border-border/70 bg-background/40 p-4 text-sm text-muted-foreground dark:border-white/10 dark:bg-white/[0.03]">
                  <div className="font-medium text-foreground">No exact official vLLM recipe found for {model.id}</div>
                  <p className="mt-1">
                    Airunway will continue with the default Direct vLLM settings.
                  </p>
                </div>
              ) : null}

              {vllmRecipesError && vllmRecipesLoaded && (
                <div className="rounded-xl border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
                  {vllmRecipesError}
                </div>
              )}

              {vllmRecipeWarnings.length > 0 && (
                <div className="rounded-xl border border-yellow-500/30 bg-yellow-500/10 p-4 text-sm text-yellow-800 dark:text-yellow-200">
                  <div className="mb-2 flex items-center gap-2 font-medium">
                    <AlertTriangle className="h-4 w-4" />
                    Recipe warnings
                  </div>
                  <ul className="list-disc space-y-1 pl-5">
                    {vllmRecipeWarnings.map((warning, index) => (
                      <li key={`${warning}-${index}`}>{warning}</li>
                    ))}
                  </ul>
                </div>
              )}

              {resolvedVllmRecipe && (
                <div className="overflow-hidden rounded-xl border border-emerald-500/25 bg-card/80 shadow-soft-xs dark:bg-white/[0.025]">
                  <div className="flex flex-col gap-3 border-b border-border/70 bg-emerald-500/[0.05] p-4 dark:border-white/10 dark:bg-emerald-500/[0.06] sm:flex-row sm:items-start sm:justify-between">
                    <div className="flex min-w-0 gap-3">
                      <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full border border-emerald-500/30 bg-emerald-500/10 text-emerald-600 dark:text-emerald-300">
                        <CheckCircle2 className="h-4 w-4" />
                      </div>
                      <div className="min-w-0">
                        <div className="font-semibold text-emerald-700 dark:text-emerald-300">
                          Recipe applied to the deployment form
                        </div>
                        <p className="mt-1 text-xs leading-5 text-muted-foreground">
                          Recommended vLLM launch settings are now loaded and ready to deploy.
                        </p>
                      </div>
                    </div>
                    <Badge variant="success" className="w-fit shrink-0 border-emerald-500/20">
                      Ready
                    </Badge>
                  </div>

                  <div className="space-y-4 p-4">
                    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
                      <RecipeMetric label="Mode">
                        {resolvedVllmRecipe.mode}
                      </RecipeMetric>
                      <RecipeMetric label="GPUs">
                        {resolvedVllmRecipe.resources.gpu}
                      </RecipeMetric>
                      <RecipeMetric label="Image">
                        {resolvedVllmRecipe.imageRef || directVllmImageRef}
                      </RecipeMetric>
                      <RecipeMetric label="Model-server args">
                        {Object.keys(resolvedVllmRecipe.engineArgs || {}).length}
                      </RecipeMetric>
                      <RecipeMetric label="Extra args">
                        {resolvedVllmRecipe.engineExtraArgs?.length || 0}
                      </RecipeMetric>
                      <RecipeMetric label="Environment values">
                        {Object.keys(resolvedVllmRecipe.env || {}).length}
                      </RecipeMetric>
                    </div>

                    <div className="grid gap-3 lg:grid-cols-3">
                      <RecipeCodePanel title="engine.args" value={resolvedVllmRecipe.engineArgs || {}} />
                      <RecipeCodePanel title="engine.extraArgs" value={resolvedVllmRecipe.engineExtraArgs || []} />
                      <RecipeCodePanel title="env" value={resolvedVllmRecipe.env || {}} />
                    </div>
                  </div>
                </div>
              )}
            </div>
          </div>
        </div>
      )}

      {/* Advanced Options - show for non-KAITO runtimes OR KAITO with vLLM models */}
      {(selectedRuntime !== 'kaito' || isVllmModel) && (
      <div className="glass-panel !p-0 overflow-hidden">
        <div
          className="cursor-pointer select-none px-6 py-4"
          onClick={() => setShowAdvanced(!showAdvanced)}
        >
          <div className="flex items-center justify-between">
            <h3 className="text-lg font-semibold">Advanced Options</h3>
              <ChevronDown
              className={cn(
                "h-5 w-5 text-muted-foreground transition-transform duration-200 ease-out",
                showAdvanced && "rotate-180"
                )}
            />
          </div>
        </div>

        {/* Smooth accordion animation */}
          <div
          className={cn(
            "grid transition-all duration-300 ease-out-expo",
            showAdvanced ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0"
          )}
        >
          <div className="overflow-hidden">
            <div className="space-y-4 px-6 pb-6 pt-0">
            {/* These options only apply to non-KAITO runtimes */}
            {selectedRuntime !== 'kaito' && (
              <>
                <div className="flex items-center justify-between">
                  <div className="space-y-0.5">
                    <Label>Enforce Eager Mode</Label>
                    <p className="text-xs text-muted-foreground">
                      Use eager mode for faster startup
                    </p>
                  </div>
                  <Switch
                    checked={config.enforceEager}
                    onCheckedChange={(checked) => updateConfig('enforceEager', checked)}
                  />
                </div>

                <div className="flex items-center justify-between">
                  <div className="space-y-0.5">
                    <Label>Enable Prefix Caching</Label>
                    <p className="text-xs text-muted-foreground">
                      Cache common prefixes for faster inference
                    </p>
                  </div>
                  <Switch
                    checked={config.enablePrefixCaching}
                    onCheckedChange={(checked) => updateConfig('enablePrefixCaching', checked)}
                  />
                </div>

                <div className="flex items-center justify-between">
                  <div className="space-y-0.5">
                    <Label>Trust Remote Code</Label>
                    <p className="text-xs text-muted-foreground">
                      Required for some models with custom code
                    </p>
                  </div>
                  <Switch
                    checked={config.trustRemoteCode}
                    onCheckedChange={(checked) => updateConfig('trustRemoteCode', checked)}
                  />
                </div>
              </>
            )}

            {/* Context Length - shown for all runtimes, but uses different state for KAITO */}
            <div className="space-y-2">
              <Label htmlFor="contextLength">Context Length (optional)</Label>
              <Input
                id="contextLength"
                type="number"
                placeholder={model.contextLength?.toString() || 'Default'}
                value={selectedRuntime === 'kaito' ? (maxModelLen || '') : (config.contextLength || '')}
                onChange={(e) => {
                  const value = e.target.value ? parseInt(e.target.value) : undefined
                  if (selectedRuntime === 'kaito') {
                    setMaxModelLen(value)
                  } else {
                    updateConfig('contextLength', value)
                  }
                }}
              />
            </div>
            </div>
          </div>
        </div>
      </div>
      )}

        {/* Capacity Warning - only show for non-KAITO or KAITO with GPU/vLLM */}
        {detailedCapacity && (selectedRuntime !== 'kaito' || kaitoComputeType === 'gpu' || isVllmModel) && (
          <CapacityWarning
            selectedGpus={selectedGpus}
            capacity={detailedCapacity}
            autoscaler={autoscaler}
            maxGpusPerPod={maxGpusPerPod}
            deploymentMode={config.mode}
            replicas={config.replicas}
            gpusPerReplica={config.resources?.gpu || gpuRecommendation.recommendedGpus || 1}
            multiNode={currentMultiNode}
          />
        )}

        {/* Manifest Preview - build config with runtime-specific fields */}
        {(() => {
          // Build preview config with all necessary fields
          let previewConfig = normalizeGatewayAvailability(config, gatewayInfo?.available);

          if (selectedRuntime === 'vllm') {
            previewConfig = normalizeDirectVllmConfig(previewConfig, previewConfig.imageRef || directVllmImageRef, model.id);
          }

          if (selectedRuntime === 'kaito') {
            // Always include kaitoResourceType for KAITO deployments
            previewConfig = { ...previewConfig, kaitoResourceType };

            if (isHuggingFaceGgufModel) {
              previewConfig = {
                ...previewConfig,
                modelSource: 'huggingface' as const,
                modelId: model.id,
                ggufFile: ggufFile,
                ggufRunMode: ggufRunMode,
                computeType: kaitoComputeType,
              };
            } else if (isVllmModel) {
              previewConfig = {
                ...previewConfig,
                modelSource: 'vllm' as const,
                modelId: model.id,
                computeType: 'gpu' as const,
                ...(maxModelLen && { maxModelLen }),
              };
            } else if (selectedPremadeModel) {
              previewConfig = {
                ...previewConfig,
                modelSource: 'premade' as const,
                computeType: kaitoComputeType,
                premadeModel: selectedPremadeModel.id,
              };
            }
          }

          return (
            <ManifestViewer
              mode="preview"
              config={previewConfig}
              provider={selectedRuntime}
            />
          );
        })()}
        {/* Cost Estimate - show for GPU and CPU deployments */}
        {(selectedRuntime === 'kaito') && (
          <CostEstimate
            nodePools={detailedCapacity?.nodePools}
            gpuCount={config.mode === 'disaggregated' 
              ? Math.max(config.prefillGpus || 1, config.decodeGpus || 1)
              : (config.resources?.gpu || gpuRecommendation.recommendedGpus || 1)}
            replicas={config.mode === 'disaggregated'
              ? (config.prefillReplicas || 1) + (config.decodeReplicas || 1)
              : config.replicas}
            computeType={kaitoComputeType === 'cpu' && !isVllmModel ? 'cpu' : 'gpu'}
          />
        )}
        {/* Cost Estimate for non-KAITO runtimes (always GPU) */}
        {selectedRuntime !== 'kaito' && detailedCapacity && detailedCapacity.nodePools.length > 0 && (
          <CostEstimate
            nodePools={detailedCapacity.nodePools}
            gpuCount={config.mode === 'disaggregated'
              ? Math.max(config.prefillGpus || 1, config.decodeGpus || 1)
              : (config.resources?.gpu || gpuRecommendation.recommendedGpus || 1)}
            replicas={config.mode === 'disaggregated'
              ? (config.prefillReplicas || 1) + (config.decodeReplicas || 1)
              : config.replicas * getNodeCountFromOverrides(config.providerOverrides)}
            computeType="gpu"
          />
        )}

      {/* Submit Button */}
      <div className="flex gap-4">
        <Button
          type="button"
          variant="outline"
          className="rounded-2xl"
          onClick={() => navigate('/')}
        >
          Cancel
        </Button>
        <Button
          type="submit"
          disabled={createDeployment.isProcessing || needsHfAuth || !isRuntimeInstalled || !isKaitoConfigValid || fp8Blocked}
          loading={createDeployment.isProcessing}
          className={cn(
            "flex-1 h-14 rounded-2xl bg-primary text-primary-foreground font-bold shadow-glow-button gap-2",
            createDeployment.status === 'success' && "bg-green-600 hover:bg-green-600"
          )}
        >
          {getButtonContent()}
        </Button>
      </div>
      {fp8Blocked && (
        <p className="text-sm text-destructive text-center">
          {fp8BlockReason || 'FP8 is only supported on L40S/L4 and H100/H200 GPUs. Choose FP16/BF16 to deploy.'}
        </p>
      )}
      {/* Non-blocking "does not fit" warning. Deploy stays enabled: the estimate
          assumes a fixed GPUs-per-replica, but the user may select more here, so
          we caution rather than block. Hidden when fp8Blocked already explains a
          blocking reason. */}
      {doesNotFit && !fp8Blocked && (
        <p className="text-sm text-yellow-500/90 text-center">
          {doesNotFitReason || "This model is estimated not to fit on this cluster's GPUs at the selected precision. Try more GPUs per replica, a smaller model, or FP8 precision."}
        </p>
      )}
    </form>
    </>
  )
}
