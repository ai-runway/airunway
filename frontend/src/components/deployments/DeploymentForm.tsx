import { useState, useEffect, useRef, useCallback, useMemo } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { useConfetti } from '@/components/ui/confetti'
import { useCreateDeployment, usePVCs, type DeploymentConfig } from '@/hooks/useDeployments'
import { useHuggingFaceStatus, useGgufFiles } from '@/hooks/useHuggingFace'
import { usePremadeModels } from '@/hooks/useAikit'
import { useGatewayStatus } from '@/hooks/useGateway'
import { useToast } from '@/hooks/useToast'
import { generateDeploymentName, cn } from '@/lib/utils'
import { type Model, type DetailedClusterCapacity, type AutoscalerDetectionResult, type RuntimeStatus, type PremadeModel, type AIConfiguratorResult, aikitApi, type KaitoResourceType } from '@/lib/api'
import { ChevronDown, AlertCircle, Rocket, CheckCircle2, Box, HardDrive } from 'lucide-react'
import { CapacityWarning } from './CapacityWarning'
import { AIConfiguratorPanel } from './AIConfiguratorPanel'
import { ManifestViewer } from './ManifestViewer'
import { CostEstimate } from './CostEstimate'
import { StorageVolumesSection } from './StorageVolumesSection'
import { KaitoModelConfiguration } from './KaitoModelConfiguration'
import { KaitoResourceTypeSelector } from './KaitoResourceTypeSelector'
import { EngineSelectionPanel } from './EngineSelectionPanel'
import { DeploymentOptionsPanel } from './DeploymentOptionsPanel'
import { RuntimeSelectionPanel } from './RuntimeSelectionPanel'
import { DeploymentModePanel } from './DeploymentModePanel'
import { prepareGgufImageRef } from './deploymentFormSubmit'
import { calculateGpuRecommendation } from '@/lib/gpu-recommendations'
import {
  FP8_ARG_ENGINES,
  KV_CACHE_DTYPE_ARG,
  PIPELINE_PARALLEL_SIZE_ARG,
  QUANTIZATION_ARG,
  RUNTIME_INFO,
  TENSOR_PARALLEL_SIZE_ARG,
  applyRuntimeChangeToConfig,
  applyDeploymentModeChangeToConfig,
  applyGpuPerReplicaChangeToConfig,
  buildDeploymentFormConfig,
  buildDynamoMultiNodeOverrides,
  applyAIConfiguratorResultToConfig,
  getAIConfigRecommendedValues,
  getAIConfiguratorAppliedToastDescription,
  getAvailableEnginesForRuntime,
  getDefaultRuntimeForModel,
  getDeploymentModelFacts,
  createInitialDeploymentConfig,
  getNodeCountFromOverrides,
  getNumericEngineArg,
  getDeploymentResourceSummary,
  getDeploymentSubmitButtonState,
  normalizeGatewayAvailability,
  selectPreferredGgufFile,
  setDynamoParallelismEngineArgs,
  setFp8PrecisionEngineArgs,
  type DeploymentMode,
  type GgufRunMode,
  type KaitoComputeType,
  type RuntimeId,
  type TraditionalEngine,
} from './deploymentFormModel'

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

  // Determine default runtime: prefer compatible and installed runtime
  const getDefaultRuntime = (): RuntimeId => getDefaultRuntimeForModel(model.supportedEngines, runtimes)

  const [selectedRuntime, setSelectedRuntime] = useState<RuntimeId>(getDefaultRuntime)
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

  const { isHuggingFaceGgufModel, isVllmModel } = useMemo(
    () => getDeploymentModelFacts(model),
    [model]
  )

  // Fetch GGUF files from HuggingFace repo when it's a GGUF model and KAITO is selected
  const { data: ggufFilesData, isLoading: ggufFilesLoading } = useGgufFiles(
    model.id,
    isHuggingFaceGgufModel && selectedRuntime === 'kaito'
  );
  const ggufFiles = ggufFilesData?.files || [];

  // Auto-select Q4_K_M file if available, otherwise first file
  useEffect(() => {
    const nextGgufFile = selectPreferredGgufFile(ggufFilesData?.files || [], ggufFile)
    if (nextGgufFile !== ggufFile) {
      setGgufFile(nextGgufFile)
    }
  }, [ggufFilesData, ggufFile])

  // Get supported engines for the selected runtime, filtered by model support
  const availableEngines = getAvailableEnginesForRuntime(selectedRuntime, model.supportedEngines)


  const defaultRuntime = getDefaultRuntime()

  const [showAdvanced, setShowAdvanced] = useState(false)
  const [config, setConfig] = useState<DeploymentConfig>(() =>
    createInitialDeploymentConfig({ model, runtime: defaultRuntime })
  )

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
  const currentNodeCount = getNodeCountFromOverrides(config.providerOverrides)
  const currentPipelineParallel = getNumericEngineArg(config.engineArgs, PIPELINE_PARALLEL_SIZE_ARG)

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

      if (gpuRecommendation.multiNode) {
        const recommendedPipelineParallel = gpuRecommendation.multiNode.pipelineParallelSize
        const gpuCount = prev.resources?.gpu ?? 0

        // Intentionally compare against the previous config inside setState so
        // manual topology edits do not trigger this effect and get snapped back.
        if (
          prevNodeCount === gpuRecommendation.multiNode.nodeCount &&
          prevTensorParallel === gpuRecommendation.multiNode.gpusPerNode &&
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
          providerOverrides: buildDynamoMultiNodeOverrides(gpuRecommendation.multiNode!.nodeCount),
          engineArgs: setDynamoParallelismEngineArgs(prev.engineArgs, gpuRecommendation.multiNode!),
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
    gpuRecommendation.multiNode,
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
  const handleRuntimeChange = (runtime: RuntimeId) => {
    setTopologyManagedByAIConfig(false)
    setSelectedRuntime(runtime)
    setConfig(prev => applyRuntimeChangeToConfig(prev, {
      runtime,
      modelEngines: model.supportedEngines,
      recommendedGpus: gpuRecommendation.recommendedGpus,
      estimatedMemoryGb: gpuRecommendation.estimatedMemoryGb,
      gpuMemoryGb: detailedCapacity?.totalMemoryGb,
    }))

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
      const imageRef = await prepareGgufImageRef({
        selectedRuntime,
        isHuggingFaceGgufModel,
        ggufRunMode,
        modelId: model.id,
        ggufFile,
        adapter: {
          notify: toast,
          getInfrastructureStatus: () => aikitApi.getInfrastructureStatus(),
          build: (request) => aikitApi.build(request),
        },
      })

      const deployConfig = buildDeploymentFormConfig(config, {
        selectedRuntime,
        gatewayAvailable: gatewayInfo?.available,
        kaitoResourceType,
        isHuggingFaceGgufModel,
        isVllmModel,
        modelId: model.id,
        ggufFile,
        ggufRunMode,
        kaitoComputeType,
        selectedPremadeModelId: selectedPremadeModel?.id,
        maxModelLen,
        imageRef,
      })

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
  }, [config, createDeployment, navigate, toast, triggerConfetti, selectedRuntime, kaitoComputeType, kaitoResourceType, selectedPremadeModel, isHuggingFaceGgufModel, isVllmModel, model.id, ggufFile, ggufRunMode, maxModelLen, gatewayInfo?.available])

  const updateConfig = <K extends keyof DeploymentConfig>(
    key: K,
    value: DeploymentConfig[K]
  ) => {
    setConfig((prev) => ({ ...prev, [key]: value }))
  }

  // Handler for applying AI Configurator recommendations
  const handleApplyAIConfig = useCallback((result: AIConfiguratorResult) => {
    // Store supported backends info for engine selection UI
    if (result.supportedBackends) {
      setAiConfigSupportedBackends(result.supportedBackends)
    }
    if (result.backend) {
      setAiConfigRecommendedBackend(result.backend)
    }

    // Store recommended mode and values for badges
    setAiConfigRecommendedMode(result.mode)
    setAiConfigRecommendedValues(getAIConfigRecommendedValues(result))
    setTopologyManagedByAIConfig(true)
    setConfig(prev => applyAIConfiguratorResultToConfig(prev, result, selectedRuntime))

    toast({
      title: 'Configuration Applied',
      description: getAIConfiguratorAppliedToastDescription(result),
      variant: 'success',
    })
  }, [selectedRuntime, toast])

  const { selectedGpus, currentMultiNode, maxGpusPerPod } = getDeploymentResourceSummary({
    config,
    recommendedGpus: gpuRecommendation.recommendedGpus,
    currentNodeCount,
    currentPipelineParallel,
  })
  const submitButtonState = getDeploymentSubmitButtonState({
    isProcessing: createDeployment.isProcessing,
    submitStatus: createDeployment.status,
    needsHfAuth,
    fp8Blocked,
    isRuntimeInstalled,
    isSelectedCrdLessRuntimeNotReady,
    selectedRuntime,
    isHuggingFaceGgufModel,
    isVllmModel,
    ggufFile,
    gpuCount: config.resources?.gpu || 0,
    hasSelectedPremadeModel: selectedPremadeModel !== null,
  })
  // Status-aware button content
  const getButtonContent = () => {
    if (submitButtonState.kind === 'success') {
      return (
        <>
          <CheckCircle2 className="h-4 w-4" />
          {submitButtonState.label}
        </>
      )
    }

    if (submitButtonState.kind === 'ready') {
      return (
        <>
          <Rocket className="h-4 w-4" />
          {submitButtonState.label}
          <kbd className="hidden sm:inline-flex ml-2 px-1.5 py-0.5 text-[10px] font-mono bg-primary-foreground/20 rounded">
            ⌘↵
          </kbd>
        </>
      )
    }

    return submitButtonState.label
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
        <RuntimeSelectionPanel
          runtimes={runtimes}
          selectedRuntime={selectedRuntime}
          modelEngines={model.supportedEngines}
          onRuntimeChange={handleRuntimeChange}
        />
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
                  placeholder={RUNTIME_INFO[selectedRuntime].defaultNamespace}
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

      {/* Engine Selection - show for non-KAITO runtimes OR KAITO with vLLM models */}
      {(selectedRuntime !== 'kaito' || isVllmModel) && (
        <EngineSelectionPanel
          selectedRuntime={selectedRuntime}
          isVllmModel={isVllmModel}
          runtimeName={RUNTIME_INFO[selectedRuntime].name}
          availableEngines={availableEngines}
          engine={config.engine}
          aiConfigSupportedBackends={aiConfigSupportedBackends}
          aiConfigRecommendedBackend={aiConfigRecommendedBackend}
          onEngineChange={(engine) => {
            setTopologyManagedByAIConfig(false)
            updateConfig('engine', engine)
          }}
        />
      )}

      {/* KAITO Resource Type Selection - show for KAITO runtime with vLLM models */}
      {selectedRuntime === 'kaito' && isVllmModel && (
        <div className="glass-panel">
          <h3 className="text-lg font-semibold flex items-center gap-2 mb-4">
            <Box className="h-5 w-5" />
            KAITO Resource Type
          </h3>
          <KaitoResourceTypeSelector
            value={kaitoResourceType}
            onChange={setKaitoResourceType}
            idSuffix="vllm"
          />
        </div>
      )}

      {/* KAITO Model Configuration - only show for KAITO runtime with non-vLLM models */}
      {selectedRuntime === 'kaito' && !isVllmModel && (
        <KaitoModelConfiguration
          computeType={kaitoComputeType}
          onComputeTypeChange={setKaitoComputeType}
          resourceType={kaitoResourceType}
          onResourceTypeChange={setKaitoResourceType}
          isHuggingFaceGgufModel={isHuggingFaceGgufModel}
          ggufRunMode={ggufRunMode}
          onGgufRunModeChange={setGgufRunMode}
          ggufFilesLoading={ggufFilesLoading}
          ggufFiles={ggufFiles}
          ggufFile={ggufFile}
          onGgufFileChange={setGgufFile}
        />
      )}

      {/* Deployment Mode - show for non-KAITO runtimes OR KAITO with vLLM models */}
      {(selectedRuntime !== 'kaito' || isVllmModel) && (
        <DeploymentModePanel
          mode={config.mode}
          selectedRuntime={selectedRuntime}
          aiConfigRecommendedMode={aiConfigRecommendedMode}
          onModeChange={(mode) => {
            setTopologyManagedByAIConfig(false)
            setConfig(prev => applyDeploymentModeChangeToConfig(prev, mode))
          }}
        />
      )}

      <DeploymentOptionsPanel
        config={config}
        selectedRuntime={selectedRuntime}
        isVllmModel={isVllmModel}
        kaitoComputeType={kaitoComputeType}
        detailedCapacity={detailedCapacity}
        gpuRecommendation={gpuRecommendation}
        aiConfigRecommendedValues={aiConfigRecommendedValues}
        currentMultiNode={currentMultiNode}
        onReplicasChange={(value) => updateConfig('replicas', value)}
        onGpuPerReplicaChange={(value) => {
          setTopologyManagedByAIConfig(false)
          setConfig(prev => applyGpuPerReplicaChangeToConfig(prev, {
            selectedRuntime,
            gpuCount: value,
            estimatedMemoryGb: gpuRecommendation.estimatedMemoryGb,
            gpuMemoryGb: detailedCapacity?.totalMemoryGb,
          }))
        }}
        onRouterModeChange={(value) => updateConfig('routerMode', value)}
        onPrefillReplicasChange={(value) => updateConfig('prefillReplicas', value)}
        onPrefillGpusChange={(value) => updateConfig('prefillGpus', value)}
        onDecodeReplicasChange={(value) => updateConfig('decodeReplicas', value)}
        onDecodeGpusChange={(value) => updateConfig('decodeGpus', value)}
      />

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

        {/* Manifest Preview - build config with KAITO-specific fields */}
        {(() => {
          // Build preview config with all necessary fields
          const previewConfig = buildDeploymentFormConfig(config, {
            selectedRuntime,
            gatewayAvailable: gatewayInfo?.available,
            kaitoResourceType,
            isHuggingFaceGgufModel,
            isVllmModel,
            modelId: model.id,
            ggufFile,
            ggufRunMode,
            kaitoComputeType,
            selectedPremadeModelId: selectedPremadeModel?.id,
            maxModelLen,
          })

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
          disabled={submitButtonState.disabled}
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
