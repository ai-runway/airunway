import { useState, useEffect } from 'react'
import { useSettings } from '@/hooks/useSettings'
import { useRuntimesStatus } from '@/hooks/useRuntimes'
import { useClusterStatus } from '@/hooks/useClusterStatus'
import {
  useHelmStatus,
  useProviderInstallationStatus,
  useInstallProvider,
  useUninstallProvider,
} from '@/hooks/useInstallation'
import { useAutoscalerDetection } from '@/hooks/useAutoscaler'
import { useGpuOperatorStatus, useInstallGpuOperator } from '@/hooks/useGpuOperator'
import { useGatewayCRDStatus, useInstallGatewayCRDs } from '@/hooks/useGateway'
import { useHuggingFaceStatus, useHuggingFaceOAuth, useDeleteHuggingFaceSecret } from '@/hooks/useHuggingFace'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { AutoscalerGuidance } from '@/components/autoscaler/AutoscalerGuidance'
import { useToast } from '@/hooks/useToast'
import {
  CheckCircle,
  XCircle,
  AlertCircle,
  AlertTriangle,
  Loader2,
  Server,
  Cpu,
  Key,
  Cog,
  Layers,
  Download,
  RefreshCw,
  Copy,
  Zap,
  Trash2,
  Globe,
} from 'lucide-react'
import { cn } from '@/lib/utils'
import { useSearchParams } from 'react-router-dom'

type SettingsTab = 'general' | 'runtimes' | 'integrations'
type RuntimeId = string

type RuntimeCrdMetadata = {
  id?: string | null
  name?: string | null
  requiresCRD?: boolean | null
}

type RuntimeSelectionMetadata = RuntimeCrdMetadata & {
  installed?: boolean | null
}

const KNOWN_RUNTIME_IDS = new Set(['dynamo', 'kuberay', 'kaito', 'llmd', 'vllm'])
const CRD_LESS_RUNTIME_IDS = new Set(['llmd', 'vllm'])
const CRD_LESS_RUNTIME_DISPLAY_NAMES = new Set(['LLM-D', 'vLLM'])

const normalizeRuntimeId = (id: string | null | undefined) => String(id ?? '').toLowerCase()
const isLlmdRuntimeId = (id: string | null | undefined) => normalizeRuntimeId(id) === 'llmd'
const isVllmRuntimeId = (id: string | null | undefined) => normalizeRuntimeId(id) === 'vllm'
const isLlmdRuntimeDisplayName = (name: string | null | undefined) => String(name ?? '').trim() === 'LLM-D'
const isVllmRuntimeDisplayName = (name: string | null | undefined) => String(name ?? '').trim() === 'vLLM'
const isCrdLessRuntimeId = (id: string | null | undefined) => CRD_LESS_RUNTIME_IDS.has(normalizeRuntimeId(id))
const isCrdLessRuntimeDisplayName = (name: string | null | undefined) => CRD_LESS_RUNTIME_DISPLAY_NAMES.has(String(name ?? '').trim())
const canonicalizeRuntimeId = (id: string) => {
  const normalized = normalizeRuntimeId(id)
  return KNOWN_RUNTIME_IDS.has(normalized) ? normalized : id
}
const runtimeIdsMatch = (left: string | null | undefined, right: string | null | undefined) =>
  normalizeRuntimeId(left) === normalizeRuntimeId(right)

const runtimeRequiresCRD = (runtime: RuntimeCrdMetadata | null | undefined, fallbackId?: string | null) => {
  if (typeof runtime?.requiresCRD === 'boolean') {
    return runtime.requiresCRD
  }

  if (
    isCrdLessRuntimeId(runtime?.id) ||
    isCrdLessRuntimeDisplayName(runtime?.name) ||
    isCrdLessRuntimeId(fallbackId)
  ) {
    return false
  }

  return true
}

const runtimeDescription = (id: string, name?: string | null) => {
  if (isLlmdRuntimeId(id) || isLlmdRuntimeDisplayName(name)) {
    return 'LLM-D for distributed inference'
  }

  if (isVllmRuntimeId(id) || isVllmRuntimeDisplayName(name)) {
    return 'vLLM for high-throughput inference'
  }

  switch (normalizeRuntimeId(id)) {
    case 'kaito':
      return 'KAITO for simplified model deployment'
    case 'dynamo':
      return 'NVIDIA Dynamo for high-performance GPU inference'
    case 'kuberay':
      return 'Ray Serve via KubeRay for distributed Ray-based model serving with vLLM'
    default:
      return 'Inference runtime provider'
  }
}

const crdLessRuntimeReadinessMessage = (ready: boolean | null | undefined) => (
  ready ? 'Runtime is ready to use.' : 'Provider is registered but not ready yet.'
)

const crdLessRuntimeStateLabel = (ready: boolean | null | undefined) => (
  ready ? 'Ready' : 'Registered'
)

const selectDefaultRuntimeId = (runtimes: RuntimeSelectionMetadata[] | undefined): RuntimeId | null => {
  if (!runtimes) {
    return null
  }

  const installedRuntime = runtimes.find(r => r.installed && r.id)
  if (installedRuntime?.id) {
    return canonicalizeRuntimeId(installedRuntime.id)
  }

  const dynamoRuntime = runtimes.find(r => runtimeIdsMatch(r.id, 'dynamo') && r.id)
  if (dynamoRuntime?.id) {
    return canonicalizeRuntimeId(dynamoRuntime.id)
  }

  const firstRegisteredRuntime = runtimes.find(r => r.id)
  if (firstRegisteredRuntime?.id) {
    return canonicalizeRuntimeId(firstRegisteredRuntime.id)
  }

  return 'dynamo'
}

export function SettingsPage() {
  const [searchParams, setSearchParams] = useSearchParams()
  const { isLoading: settingsLoading } = useSettings()
  const { data: runtimesStatus, isLoading: runtimesLoading, refetch: refetchRuntimesStatus } = useRuntimesStatus()
  const { data: clusterStatus, isLoading: clusterLoading } = useClusterStatus()
  const { data: helmStatus, isLoading: helmLoading } = useHelmStatus()
  const { data: autoscaler, isLoading: autoscalerLoading } = useAutoscalerDetection()
  const { data: gpuOperatorStatus, isLoading: gpuStatusLoading, refetch: refetchGpuStatus } = useGpuOperatorStatus()
  const { data: gatewayCRDStatus, isLoading: gatewayStatusLoading, refetch: refetchGatewayStatus } = useGatewayCRDStatus()
  const installGatewayCRDs = useInstallGatewayCRDs()
  const { data: hfStatus, isLoading: hfStatusLoading, refetch: refetchHfStatus } = useHuggingFaceStatus()
  const { startOAuth } = useHuggingFaceOAuth()
  const deleteHfSecret = useDeleteHuggingFaceSecret()
  const installGpuOperator = useInstallGpuOperator()
  const { toast } = useToast()

  const [isInstallingGpu, setIsInstallingGpu] = useState(false)
  const [isInstallingGateway, setIsInstallingGateway] = useState(false)
  const [isConnectingHf, setIsConnectingHf] = useState(false)
  
  // Tab state from URL params or default
  const tabFromUrl = searchParams.get('tab') as SettingsTab | null
  const [activeTab, setActiveTab] = useState<SettingsTab>(tabFromUrl || 'general')
  
  // Runtime installation state
  const [selectedRuntime, setSelectedRuntime] = useState<RuntimeId | null>(null)
  const [isInstalling, setIsInstalling] = useState(false)
  const [pendingInstallRuntime, setPendingInstallRuntime] = useState<RuntimeId | null>(null)
  const [isUninstalling, setIsUninstalling] = useState(false)
  const [showUninstallDialog, setShowUninstallDialog] = useState(false)

  const runtimes = runtimesStatus?.runtimes || []
  const readyRuntimeCount = runtimes.filter(r => runtimeRequiresCRD(r) ? r.installed : (r.installed || r.healthy)).length
  const helmAvailable = helmStatus?.available ?? false
  const defaultRuntime = selectDefaultRuntimeId(runtimesStatus?.runtimes)

  // Set default runtime once data is loaded
  useEffect(() => {
    if (runtimesStatus?.runtimes && selectedRuntime === null && defaultRuntime) {
      setSelectedRuntime(defaultRuntime)
    }
  }, [runtimesStatus, selectedRuntime, defaultRuntime])

  // Update URL when tab changes
  useEffect(() => {
    if (activeTab !== 'general') {
      setSearchParams({ tab: activeTab })
    } else {
      setSearchParams({})
    }
  }, [activeTab, setSearchParams])

  const effectiveRuntime = selectedRuntime || defaultRuntime || ''

  const {
    data: installationStatus,
    isLoading: installationLoading,
    refetch: refetchInstallation,
  } = useProviderInstallationStatus(effectiveRuntime)

  const currentRuntime = runtimes.find(r => runtimeIdsMatch(r.id, effectiveRuntime))
  const selectedRuntimeRequiresCRD = runtimeRequiresCRD({
    id: currentRuntime?.id ?? effectiveRuntime,
    name: currentRuntime?.name ?? installationStatus?.providerName,
    requiresCRD: installationStatus?.requiresCRD ?? currentRuntime?.requiresCRD,
  }, effectiveRuntime)

  const installProvider = useInstallProvider()
  const uninstallProvider = useUninstallProvider()

  const handleInstall = async (providerId: RuntimeId) => {
    setIsInstalling(true)
    try {
      const result = await installProvider.mutateAsync(providerId)
      if (result.success) {
        setPendingInstallRuntime(providerId)
        toast({ title: 'Installation Started', description: `${result.message}. Waiting for the runtime service to become ready.` })
        refetchInstallation()
        refetchRuntimesStatus()
      } else {
        toast({ title: 'Installation Failed', description: result.message, variant: 'destructive' })
      }
    } catch (error) {
      toast({ title: 'Installation Error', description: error instanceof Error ? error.message : 'Unknown error occurred', variant: 'destructive' })
    } finally {
      setIsInstalling(false)
    }
  }

  const handleUninstall = async (providerId: RuntimeId) => {
    setIsUninstalling(true)
    setShowUninstallDialog(false)
    try {
      const result = await uninstallProvider.mutateAsync(providerId)
      if (result.success) {
        setPendingInstallRuntime((current) => current === providerId ? null : current)
        toast({ title: 'Uninstall Complete', description: result.message })
        refetchInstallation()
        refetchRuntimesStatus()
      } else {
        toast({ title: 'Uninstall Failed', description: result.message, variant: 'destructive' })
      }
    } catch (error) {
      toast({ title: 'Uninstall Error', description: error instanceof Error ? error.message : 'Unknown error occurred', variant: 'destructive' })
    } finally {
      setIsUninstalling(false)
    }
  }

  const copyToClipboard = (text: string) => {
    navigator.clipboard.writeText(text)
    toast({ title: 'Copied', description: 'Command copied to clipboard' })
  }

  const isInstalled = installationStatus?.installed ?? false
  const isWaitingForInstall = selectedRuntimeRequiresCRD && pendingInstallRuntime !== null && runtimeIdsMatch(pendingInstallRuntime, effectiveRuntime) && !isInstalled
  const selectedRuntimeMessage = isWaitingForInstall
    ? 'Install command completed. Waiting for the runtime service to become ready...'
    : selectedRuntimeRequiresCRD
      ? installationStatus?.message || 'Checking installation status...'
      : installationLoading && !installationStatus
        ? 'Checking readiness...'
        : crdLessRuntimeReadinessMessage(isInstalled)

  useEffect(() => {
    if (runtimeIdsMatch(pendingInstallRuntime, effectiveRuntime) && isInstalled) {
      setPendingInstallRuntime(null)
    }
  }, [effectiveRuntime, isInstalled, pendingInstallRuntime])

  useEffect(() => {
    if (!isWaitingForInstall) return

    const intervalId = window.setInterval(() => {
      refetchInstallation()
      refetchRuntimesStatus()
    }, 5000)

    return () => window.clearInterval(intervalId)
  }, [isWaitingForInstall, refetchInstallation, refetchRuntimesStatus])

  if (settingsLoading || clusterLoading || runtimesLoading) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-3xl font-heading font-bold tracking-tight flex items-center gap-2">
            <Cog className="h-7 w-7 text-muted-foreground" />
            Settings
          </h1>
          <p className="text-muted-foreground mt-1">
            Configure your inference runtimes and application settings.
          </p>
        </div>
        <div className="space-y-4">
          <Skeleton className="h-10 w-64" />
          <Skeleton className="h-48 w-full rounded-lg" />
          <Skeleton className="h-48 w-full rounded-lg" />
        </div>
      </div>
    )
  }

  const tabs = [
    { id: 'general' as const, label: 'General', icon: Server },
    { id: 'runtimes' as const, label: 'Runtimes', icon: Layers },
    { id: 'integrations' as const, label: 'Integrations', icon: Key },
  ]

  return (
    <div className="space-y-6 animate-slide-up">
      <div>
        <h1 className="text-3xl font-heading font-bold tracking-tight flex items-center gap-2">
          <Cog className="h-7 w-7 text-muted-foreground" />
          Settings
        </h1>
        <p className="text-muted-foreground mt-1">
          Configure your inference runtimes and application settings.
        </p>
      </div>

      {/* Tab Navigation */}
      <div className="flex gap-1 border-b border-white/5">
        {tabs.map((tab) => (
          <button
            key={tab.id}
            onClick={() => setActiveTab(tab.id)}
            className={cn(
              'flex items-center gap-2 px-4 py-2.5 text-sm font-medium transition-all duration-200 border-b-2 -mb-px',
              activeTab === tab.id
                ? 'border-cyan-400 text-cyan-400'
                : 'border-transparent text-slate-500 hover:text-foreground'
            )}
          >
            <tab.icon className={cn(
              "h-4 w-4 transition-transform duration-200",
              activeTab === tab.id && "scale-110"
            )} />
            {tab.label}
          </button>
        ))}
      </div>

      {/* General Tab */}
      {activeTab === 'general' && (
        <div className="space-y-6 animate-slide-up">
          {/* Cluster Status */}
          <div className="bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm">
            <div className="mb-4">
              <h3 className="font-heading text-lg font-semibold flex items-center gap-2">
                <Server className="h-5 w-5" />
                Cluster Status
              </h3>
              <p className="text-sm text-muted-foreground mt-1">
                Current connection status
              </p>
            </div>
            <div className="space-y-4">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">Connection</span>
                <div className="flex items-center gap-2">
                  {clusterStatus?.connected ? (
                    <>
                      <CheckCircle className="h-4 w-4 text-green-400" />
                      <span className="text-sm text-green-400">Connected</span>
                    </>
                  ) : (
                    <>
                      <XCircle className="h-4 w-4 text-red-500" />
                      <span className="text-sm text-red-500">Disconnected</span>
                    </>
                  )}
                </div>
              </div>

              {clusterStatus?.clusterName && (
                <div className="flex items-center justify-between">
                  <span className="text-sm font-medium">Cluster</span>
                  <span className="text-sm text-muted-foreground font-mono">{clusterStatus.clusterName}</span>
                </div>
              )}

              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">Runtimes Ready</span>
                <Badge variant={readyRuntimeCount > 0 ? 'default' : 'secondary'}>
                  {readyRuntimeCount} of {runtimes.length}
                </Badge>
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Runtimes Tab */}
      {activeTab === 'runtimes' && (
        <div className="space-y-6 animate-slide-up">
          {/* Prerequisites */}
          <div className="bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm">
            <div className="mb-4">
              <h3 className="font-heading text-lg font-semibold flex items-center gap-2">
                <Server className="h-5 w-5" />
                Prerequisites
              </h3>
              <p className="text-sm text-muted-foreground mt-1">
                Required components for runtime installation
              </p>
            </div>
            <div className="space-y-4">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">Environment</span>
                <div className="flex items-center gap-2">
                  {clusterStatus?.connected ? (
                    <>
                      <CheckCircle className="h-4 w-4 text-green-500" />
                      <span className="text-sm text-green-600">Connected</span>
                    </>
                  ) : (
                    <>
                      <XCircle className="h-4 w-4 text-red-500" />
                      <span className="text-sm text-red-600">Not Connected</span>
                    </>
                  )}
                </div>
              </div>

              <div className="flex items-center justify-between">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium">Helm CLI</span>
                  {helmStatus?.version && (
                    <span className="text-xs text-muted-foreground">({helmStatus.version})</span>
                  )}
                </div>
                <div className="flex items-center gap-2">
                  {helmLoading ? (
                    <Loader2 className="h-4 w-4 animate-spin" />
                  ) : helmAvailable ? (
                    <>
                      <CheckCircle className="h-4 w-4 text-green-500" />
                      <span className="text-sm text-green-600">Available</span>
                    </>
                  ) : (
                    <>
                      <XCircle className="h-4 w-4 text-red-500" />
                      <span className="text-sm text-red-600">Not Found</span>
                    </>
                  )}
                </div>
              </div>

              {!helmAvailable && helmStatus?.error && (
                <div className="rounded-lg bg-red-50 p-3 text-sm text-red-800 dark:bg-red-950 dark:text-red-200">
                  {helmStatus.error}
                </div>
              )}
            </div>
          </div>

          {/* Cluster Autoscaling Status */}
          <div className="bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm">
            <div className="mb-4">
              <h3 className="font-heading text-lg font-semibold flex items-center justify-between">
                <div className="flex items-center gap-2">
                  <Zap className="h-5 w-5" />
                  Cluster Autoscaling
                </div>
                {autoscalerLoading ? (
                  <Loader2 className="h-4 w-4 animate-spin" />
                ) : autoscaler?.detected ? (
                  <Badge variant={autoscaler.healthy ? 'default' : 'destructive'}>
                    {autoscaler.healthy ? 'Healthy' : 'Unhealthy'}
                  </Badge>
                ) : (
                  <Badge variant="secondary">Not Detected</Badge>
                )}
              </h3>
              <p className="text-sm text-muted-foreground mt-1">
                Automatically provision GPU compute resources when deployments require more resources
              </p>
            </div>
            <div className="space-y-4">
              {autoscalerLoading ? (
                <div className="flex items-center justify-center py-4">
                  <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
                </div>
              ) : (
                <>
                  <div className="space-y-3 text-sm">
                    <div className="flex items-center justify-between rounded-lg bg-muted p-3">
                      <span>Status</span>
                      <div className="flex items-center gap-2">
                        {autoscaler?.detected ? (
                          <>
                            {autoscaler.healthy ? (
                              <CheckCircle className="h-4 w-4 text-green-500" />
                            ) : (
                              <AlertTriangle className="h-4 w-4 text-yellow-500" />
                            )}
                            <span className="font-medium">
                              {autoscaler.type === 'aks-managed' ? 'AKS Managed' : 'Cluster Autoscaler'}
                            </span>
                          </>
                        ) : (
                          <>
                            <XCircle className="h-4 w-4 text-gray-400" />
                            <span className="text-muted-foreground">Not Detected</span>
                          </>
                        )}
                      </div>
                    </div>

                    {autoscaler?.detected && autoscaler.nodeGroupCount !== undefined && (
                      <div className="flex items-center justify-between rounded-lg bg-muted p-3">
                        <span>Node Pools</span>
                        <span className="font-medium">{autoscaler.nodeGroupCount}</span>
                      </div>
                    )}

                    {autoscaler?.message && (
                      <div className={cn(
                        'rounded-lg p-3 text-sm',
                        autoscaler.healthy
                          ? 'bg-green-50 text-green-800 dark:bg-green-950 dark:text-green-200'
                          : autoscaler.detected
                            ? 'bg-yellow-50 text-yellow-800 dark:bg-yellow-950 dark:text-yellow-200'
                            : 'bg-muted text-muted-foreground'
                      )}>
                        {autoscaler.message}
                      </div>
                    )}
                  </div>

                  {autoscaler && !autoscaler.detected && (
                    <AutoscalerGuidance autoscaler={autoscaler} />
                  )}
                </>
              )}
            </div>
          </div>

          {/* Runtimes Overview */}
          <div>
            <h2 className="text-xl font-heading font-semibold mb-4">Available Runtimes</h2>
            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
              {runtimes.map((runtime) => (
                <div
                  key={runtime.id}
                  className={cn(
                    'bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm transition-all cursor-pointer',
                    runtimeIdsMatch(effectiveRuntime, runtime.id)
                      ? 'ring-2 ring-cyan-400'
                      : 'hover:border-white/10'
                  )}
                  onClick={() => setSelectedRuntime(canonicalizeRuntimeId(runtime.id))}
                >
                  <div className="mb-3">
                    <div className="flex items-center justify-between">
                      <span className="font-heading font-bold">{runtime.name}</span>
                      {!runtimeRequiresCRD(runtime) ? (
                        runtime.installed || runtime.healthy ? (
                          <Badge variant="success" className="shrink-0">
                            <CheckCircle className="h-4 w-4" />
                            {crdLessRuntimeStateLabel(true)}
                          </Badge>
                        ) : (
                          <span className="text-muted-foreground text-sm flex items-center gap-1">
                            <AlertCircle className="h-4 w-4 text-yellow-500" />
                            {crdLessRuntimeStateLabel(false)}
                          </span>
                        )
                      ) : runtime.installed ? (
                        <Badge variant="success" className="shrink-0">
                          <CheckCircle className="h-4 w-4" />
                          Installed
                        </Badge>
                      ) : runtimeIdsMatch(pendingInstallRuntime, runtime.id) ? (
                        <span className="text-cyan-400 text-sm flex items-center gap-1">
                          <Loader2 className="h-4 w-4 animate-spin" />
                          Starting
                        </span>
                      ) : (
                        <span className="text-muted-foreground text-sm flex items-center gap-1">
                          <XCircle className="h-4 w-4 text-red-500" />
                          Not Installed
                        </span>
                      )}
                    </div>
                    <p className="text-sm text-muted-foreground mt-1">
                      {runtime.description || runtimeDescription(runtime.id, runtime.name)}
                    </p>
                  </div>
                  <div>
                    <div className="space-y-2 text-sm">
                      {!runtimeRequiresCRD(runtime) ? (
                        <div className="flex items-center gap-2 rounded-lg bg-muted/60 p-3 text-muted-foreground">
                          {runtime.installed || runtime.healthy ? (
                            <CheckCircle className="h-4 w-4 text-green-400" />
                          ) : (
                            <AlertCircle className="h-4 w-4 text-yellow-500" />
                          )}
                          <span>{crdLessRuntimeReadinessMessage(runtime.installed || runtime.healthy)}</span>
                        </div>
                      ) : (
                        <>
                          <div className="flex items-center justify-between">
                            <span className="text-muted-foreground">CRD</span>
                            {runtime.crdFound ?? runtime.installed ? (
                              <CheckCircle className="h-4 w-4 text-green-400" />
                            ) : (
                              <XCircle className="h-4 w-4 text-red-500" />
                            )}
                          </div>
                          <div className="flex items-center justify-between">
                            <span className="text-muted-foreground">Operator</span>
                            {runtime.operatorRunning ?? runtime.healthy ? (
                              <CheckCircle className="h-4 w-4 text-green-400" />
                            ) : (
                              <XCircle className="h-4 w-4 text-red-500" />
                            )}
                          </div>
                        </>
                      )}
                      {runtime.version && (
                        <div className="flex items-center justify-between">
                          <span className="text-muted-foreground">Version</span>
                          <span className="font-mono text-xs">{runtime.version}</span>
                        </div>
                      )}
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </div>

          {/* Selected Runtime Installation Details */}
          {runtimes.length > 0 && (
          <div className="bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm">
            <div className="mb-4">
              <h3 className="font-heading text-lg font-semibold flex items-center justify-between">
                <div className="flex items-center gap-2">
                  {selectedRuntimeRequiresCRD ? (
                    <Download className="h-5 w-5" />
                  ) : (
                    <Server className="h-5 w-5" />
                  )}
                  {installationStatus?.providerName || currentRuntime?.name || 'Runtime'} {selectedRuntimeRequiresCRD ? 'Installation' : 'Status'}
                </div>
                {!selectedRuntimeRequiresCRD ? (
                  isInstalled ? (
                    <Badge variant="success" className="shrink-0">
                      <CheckCircle className="h-4 w-4" />
                      {crdLessRuntimeStateLabel(true)}
                    </Badge>
                  ) : (
                    <span className="text-muted-foreground text-sm flex items-center gap-1">
                      <AlertCircle className="h-4 w-4 text-yellow-500" />
                      {crdLessRuntimeStateLabel(false)}
                    </span>
                  )
                ) : isInstalled ? (
                  <Badge variant="success" className="shrink-0">
                    <CheckCircle className="h-4 w-4" />
                    Installed
                  </Badge>
                ) : isWaitingForInstall ? (
                  <span className="text-cyan-400 text-sm flex items-center gap-1">
                    <Loader2 className="h-4 w-4 animate-spin" />
                    Starting
                  </span>
                ) : (
                  <span className="text-muted-foreground text-sm flex items-center gap-1">
                    <XCircle className="h-4 w-4 text-red-500" />
                    Not Installed
                  </span>
                )}
              </h3>
              <p className="text-sm text-muted-foreground mt-1">
                {selectedRuntimeMessage}
              </p>
            </div>
            <div className="space-y-4">
              {installationLoading ? (
                <div className="flex items-center justify-center py-8">
                  <Loader2 className="h-8 w-8 animate-spin text-muted-foreground" />
                </div>
              ) : (
                <>
                  {selectedRuntimeRequiresCRD ? (
                    <div className="grid grid-cols-2 gap-4 text-sm">
                      <div className="flex items-center justify-between rounded-lg bg-muted p-3">
                        <span>CRD Installed</span>
                        {installationStatus?.crdFound ? (
                          <CheckCircle className="h-4 w-4 text-green-500" />
                        ) : (
                          <XCircle className="h-4 w-4 text-red-500" />
                        )}
                      </div>
                      <div className="flex items-center justify-between rounded-lg bg-muted p-3">
                        <span>Operator Running</span>
                        {installationStatus?.operatorRunning ? (
                          <CheckCircle className="h-4 w-4 text-green-500" />
                        ) : (
                          <XCircle className="h-4 w-4 text-red-500" />
                        )}
                      </div>
                    </div>
                  ) : (
                    <div className="flex items-center gap-2 rounded-lg bg-muted p-3 text-sm text-muted-foreground">
                      {isInstalled ? (
                        <CheckCircle className="h-4 w-4 text-green-500" />
                      ) : (
                        <AlertCircle className="h-4 w-4 text-yellow-500" />
                      )}
                      <span>{crdLessRuntimeReadinessMessage(isInstalled)}</span>
                    </div>
                  )}

                  {selectedRuntimeRequiresCRD && (
                    <div className="flex gap-3">
                      {!isInstalled && (
                        <Button
                          onClick={() => handleInstall(effectiveRuntime)}
                          disabled={isInstalling || isWaitingForInstall || !helmAvailable || !clusterStatus?.connected}
                          className="flex items-center gap-2"
                        >
                          {isInstalling ? (
                            <>
                              <Loader2 className="h-4 w-4 animate-spin" />
                              Installing...
                            </>
                          ) : isWaitingForInstall ? (
                            <>
                              <Loader2 className="h-4 w-4 animate-spin" />
                              Checking runtime...
                            </>
                          ) : (
                            <>
                              <Download className="h-4 w-4" />
                              Install {currentRuntime?.name || 'Runtime'}
                            </>
                          )}
                        </Button>
                      )}

                      {isInstalled && (
                        <Button
                          variant="destructive"
                          onClick={() => setShowUninstallDialog(true)}
                          disabled={isUninstalling || !helmAvailable || !clusterStatus?.connected}
                          className="flex items-center gap-2"
                        >
                          {isUninstalling ? (
                            <>
                              <Loader2 className="h-4 w-4 animate-spin" />
                              Uninstalling...
                            </>
                          ) : (
                            <>
                              <Trash2 className="h-4 w-4" />
                              Uninstall
                            </>
                          )}
                        </Button>
                      )}

                      <Button
                        variant="outline"
                        onClick={() => {
                          refetchInstallation()
                          refetchRuntimesStatus()
                        }}
                        disabled={installationLoading}
                      >
                        <RefreshCw className={cn('h-4 w-4', installationLoading && 'animate-spin')} />
                      </Button>
                    </div>
                  )}

                  {selectedRuntimeRequiresCRD && !helmAvailable && (
                    <div className="flex items-start gap-2 rounded-lg bg-yellow-50 p-4 text-sm text-yellow-800 dark:bg-yellow-950 dark:text-yellow-200">
                      <AlertTriangle className="h-5 w-5 flex-shrink-0 mt-0.5" />
                      <div>
                        <p className="font-medium">Helm CLI not available</p>
                        <p className="mt-1">
                          Automatic installation requires Helm. You can install the runtime manually using the commands below.
                        </p>
                      </div>
                    </div>
                  )}
                </>
              )}
            </div>
          </div>
          )}

          {runtimes.length === 0 && !runtimesLoading && (
            <div className="bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm">
              <div className="flex flex-col items-center justify-center py-8 text-center">
                <Download className="h-8 w-8 text-muted-foreground mb-3" />
                <p className="text-sm text-muted-foreground">
                  No inference providers are registered. Deploy an InferenceProviderConfig to get started.
                </p>
              </div>
            </div>
          )}

          {/* Installation Steps */}
          {installationStatus?.installationSteps && installationStatus.installationSteps.length > 0 && (
            <div className="bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm">
              <div className="mb-4">
                <h3 className="font-heading text-lg font-semibold">Manual Installation Steps</h3>
                <p className="text-sm text-muted-foreground mt-1">
                  Detailed steps for installing {installationStatus.providerName}
                </p>
              </div>
              <div className="space-y-4">
                {installationStatus.installationSteps.map((step, index) => (
                  <div key={index} className="space-y-2">
                    <div className="flex items-center gap-2">
                      <span className="flex h-6 w-6 items-center justify-center rounded-full bg-primary text-xs font-semibold text-primary-foreground">
                        {index + 1}
                      </span>
                      <span className="font-medium">{step.title}</span>
                    </div>
                    <p className="ml-8 text-sm text-muted-foreground">{step.description}</p>
                    {step.command && (
                      <div className="ml-8 flex items-center gap-2">
                        <code className="flex-1 rounded bg-muted px-3 py-2 text-sm font-mono">{step.command}</code>
                        <Button variant="ghost" size="sm" onClick={() => copyToClipboard(step.command!)}>
                          <Copy className="h-4 w-4" />
                        </Button>
                      </div>
                    )}
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
      )}

      {/* Integrations Tab */}
      {activeTab === 'integrations' && (
        <div className="space-y-6 animate-slide-up">
          {/* GPU Operator */}
          <div className="bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm">
            <div className="mb-4">
              <h3 className="font-heading text-lg font-semibold flex items-center gap-2">
                <Cpu className="h-5 w-5" />
                NVIDIA GPU Operator
              </h3>
              <p className="text-sm text-muted-foreground mt-1">
                Install the NVIDIA GPU Operator to enable GPU support
              </p>
            </div>
            <div className="space-y-4">
              {/* Prerequisites check */}
              {(!clusterStatus?.connected || !helmStatus?.available) && (
                <div className="rounded-lg bg-yellow-50 dark:bg-yellow-950 p-3 text-sm text-yellow-800 dark:text-yellow-200">
                  <div className="flex items-center gap-2 mb-2">
                    <AlertCircle className="h-4 w-4" />
                    <span className="font-medium">Prerequisites not met</span>
                  </div>
                  <ul className="list-disc list-inside space-y-1 ml-2">
                    {!clusterStatus?.connected && (
                      <li>Not connected</li>
                    )}
                    {!helmStatus?.available && (
                      <li>Helm CLI not available</li>
                    )}
                  </ul>
                </div>
              )}

              {/* GPU Status Display */}
              {gpuStatusLoading ? (
                <div className="flex items-center gap-2 text-muted-foreground">
                  <Loader2 className="h-4 w-4 animate-spin" />
                  <span className="text-sm">Checking GPU status...</span>
                </div>
              ) : gpuOperatorStatus?.gpusAvailable ? (
                // GPUs are already available
                <div className="space-y-3">
                  <div className="flex items-center justify-between">
                    <span className="text-sm font-medium">GPU Status</span>
                    <Badge variant="success">
                      <CheckCircle className="h-3 w-3 mr-1" />
                      GPUs Enabled
                    </Badge>
                  </div>
                  <div className="rounded-lg bg-green-50 dark:bg-green-950 p-3 text-sm text-green-800 dark:text-green-200">
                    <div className="flex items-center gap-2">
                      <CheckCircle className="h-4 w-4" />
                      <span>{gpuOperatorStatus.message}</span>
                    </div>
                    {gpuOperatorStatus.gpuNodes.length > 0 && (
                      <div className="mt-2 text-xs">
                        Nodes: {gpuOperatorStatus.gpuNodes.join(', ')}
                      </div>
                    )}
                  </div>
                </div>
              ) : gpuOperatorStatus?.installed ? (
                // Operator installed but no GPUs detected
                <div className="space-y-3">
                  <div className="flex items-center justify-between">
                    <span className="text-sm font-medium">GPU Status</span>
                    <Badge variant="secondary">
                      <AlertCircle className="h-3 w-3 mr-1" />
                      Operator Installed
                    </Badge>
                  </div>
                  <div className="rounded-lg bg-yellow-50 dark:bg-yellow-950 p-3 text-sm text-yellow-800 dark:text-yellow-200">
                    <div className="flex items-center gap-2">
                      <AlertCircle className="h-4 w-4" />
                      <span>{gpuOperatorStatus.message}</span>
                    </div>
                  </div>
                </div>
              ) : (
                // Not installed - show install option
                <div className="space-y-4">
                  <div className="flex items-center justify-between">
                    <div className="space-y-0.5">
                      <Label htmlFor="gpu-operator-switch">Enable GPU Operator</Label>
                      <p className="text-xs text-muted-foreground">
                        Automatically installs the NVIDIA GPU Operator via Helm
                      </p>
                    </div>
                    <Switch
                      id="gpu-operator-switch"
                      checked={false}
                      disabled={!clusterStatus?.connected || !helmStatus?.available || isInstallingGpu}
                      onCheckedChange={async (checked) => {
                        if (checked) {
                          setIsInstallingGpu(true)
                          try {
                            const result = await installGpuOperator.mutateAsync()
                            if (result.success) {
                              toast({
                                title: 'GPU Operator Installed',
                                description: result.message,
                              })
                              refetchGpuStatus()
                            }
                          } catch (error) {
                            toast({
                              title: 'Installation Failed',
                              description: error instanceof Error ? error.message : 'Unknown error',
                              variant: 'destructive',
                            })
                          } finally {
                            setIsInstallingGpu(false)
                          }
                        }
                      }}
                    />
                  </div>

                  {isInstallingGpu && (
                    <div className="flex items-center gap-2 text-muted-foreground">
                      <Loader2 className="h-4 w-4 animate-spin" />
                      <span className="text-sm">Installing GPU Operator... This may take several minutes.</span>
                    </div>
                  )}

                  {/* Manual installation commands */}
                  {gpuOperatorStatus?.helmCommands && gpuOperatorStatus.helmCommands.length > 0 && (
                    <div className="space-y-2">
                      <span className="text-sm font-medium">Manual Installation</span>
                      <div className="space-y-1">
                        {gpuOperatorStatus.helmCommands.map((cmd, index) => (
                          <div key={index} className="flex items-center gap-2">
                            <code className="flex-1 rounded bg-muted px-3 py-2 text-xs font-mono">
                              {cmd}
                            </code>
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => {
                                navigator.clipboard.writeText(cmd)
                                toast({
                                  title: 'Copied',
                                  description: 'Command copied to clipboard',
                                })
                              }}
                            >
                              Copy
                            </Button>
                          </div>
                        ))}
                      </div>
                    </div>
                  )}
                </div>
              )}
            </div>
          </div>

          {/* Gateway API */}
          <div className="bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm">
            <div className="mb-4">
              <h3 className="font-heading text-lg font-semibold flex items-center justify-between">
                <div className="flex items-center gap-2">
                  <Globe className="h-5 w-5" />
                  Gateway API
                </div>
                <Button
                  variant="ghost"
                  size="icon"
                  onClick={() => refetchGatewayStatus()}
                  disabled={gatewayStatusLoading}
                >
                  <RefreshCw className={cn('h-4 w-4', gatewayStatusLoading && 'animate-spin')} />
                </Button>
              </h3>
              <p className="text-sm text-muted-foreground mt-1">
                Install Gateway API and Inference Extension CRDs for unified model access
              </p>
            </div>
            <div className="space-y-4">
              {gatewayStatusLoading ? (
                <div className="flex items-center gap-2 text-muted-foreground">
                  <Loader2 className="h-4 w-4 animate-spin" />
                  <span className="text-sm">Checking gateway CRD status...</span>
                </div>
              ) : (
                <>
                  {/* CRD Status */}
                  <div className="grid grid-cols-2 gap-4 text-sm">
                    <div className="flex items-center justify-between rounded-lg bg-muted p-3">
                      <span>Gateway API CRDs</span>
                      {gatewayCRDStatus?.gatewayApiInstalled ? (
                        <CheckCircle className="h-4 w-4 text-green-500" />
                      ) : (
                        <XCircle className="h-4 w-4 text-red-500" />
                      )}
                    </div>
                    <div className="flex items-center justify-between rounded-lg bg-muted p-3">
                      <div className="flex items-center gap-1">
                        <span>Inference Extension</span>
                        {gatewayCRDStatus?.inferenceExtInstalled && gatewayCRDStatus?.inferenceExtVersion && (
                          <span className="text-xs text-muted-foreground">({gatewayCRDStatus.inferenceExtVersion})</span>
                        )}
                      </div>
                      {gatewayCRDStatus?.inferenceExtInstalled ? (
                        <CheckCircle className="h-4 w-4 text-green-500" />
                      ) : (
                        <XCircle className="h-4 w-4 text-red-500" />
                      )}
                    </div>
                  </div>

                  {/* Gateway Status */}
                  {gatewayCRDStatus?.gatewayApiInstalled && gatewayCRDStatus?.inferenceExtInstalled && (
                    <div className="flex items-center justify-between rounded-lg bg-muted p-3 text-sm">
                      <span>Gateway</span>
                      <div className="flex items-center gap-2">
                        {gatewayCRDStatus.gatewayAvailable ? (
                          <>
                            <CheckCircle className="h-4 w-4 text-green-500" />
                            <span className="text-green-600 dark:text-green-400">
                              {gatewayCRDStatus.gatewayEndpoint || 'Available'}
                            </span>
                          </>
                        ) : (
                          <>
                            <AlertCircle className="h-4 w-4 text-yellow-500" />
                            <span className="text-muted-foreground">Not detected</span>
                          </>
                        )}
                      </div>
                    </div>
                  )}

                  {/* Status Message */}
                  {gatewayCRDStatus?.message && (
                    <div className={cn(
                      'rounded-lg p-3 text-sm',
                      gatewayCRDStatus.gatewayApiInstalled && gatewayCRDStatus.inferenceExtInstalled
                        ? gatewayCRDStatus.gatewayAvailable
                          ? 'bg-green-50 text-green-800 dark:bg-green-950 dark:text-green-200'
                          : 'bg-yellow-50 text-yellow-800 dark:bg-yellow-950 dark:text-yellow-200'
                        : 'bg-muted text-muted-foreground'
                    )}>
                      {gatewayCRDStatus.message}
                    </div>
                  )}

                  {/* Install Button */}
                  {(!gatewayCRDStatus?.gatewayApiInstalled || !gatewayCRDStatus?.inferenceExtInstalled) && (
                    <Button
                      onClick={async () => {
                        setIsInstallingGateway(true)
                        try {
                          const result = await installGatewayCRDs.mutateAsync()
                          if (result.success) {
                            toast({
                              title: 'CRDs Installed',
                              description: result.message,
                            })
                            refetchGatewayStatus()
                          }
                        } catch (error) {
                          toast({
                            title: 'Installation Failed',
                            description: error instanceof Error ? error.message : 'Unknown error',
                            variant: 'destructive',
                          })
                        } finally {
                          setIsInstallingGateway(false)
                        }
                      }}
                      disabled={isInstallingGateway || !clusterStatus?.connected}
                      className="flex items-center gap-2"
                    >
                      {isInstallingGateway ? (
                        <>
                          <Loader2 className="h-4 w-4 animate-spin" />
                          Installing CRDs...
                        </>
                      ) : (
                        <>
                          <Download className="h-4 w-4" />
                          Install CRDs
                        </>
                      )}
                    </Button>
                  )}

                  {/* Manual Installation Commands */}
                  {gatewayCRDStatus?.installCommands && gatewayCRDStatus.installCommands.length > 0 && (
                    <div className="space-y-2">
                      <span className="text-sm font-medium">Manual Installation</span>
                      <div className="space-y-1">
                        {gatewayCRDStatus.installCommands.map((cmd, index) => (
                          <div key={index} className="flex items-center gap-2">
                            <code className="flex-1 rounded bg-muted px-3 py-2 text-xs font-mono overflow-x-auto">
                              {cmd}
                            </code>
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => {
                                navigator.clipboard.writeText(cmd)
                                toast({
                                  title: 'Copied',
                                  description: 'Command copied to clipboard',
                                })
                              }}
                            >
                              Copy
                            </Button>
                          </div>
                        ))}
                      </div>
                    </div>
                  )}
                </>
              )}
            </div>
          </div>

          {/* HuggingFace Token */}
          <div className="bg-white/[0.03] border border-white/5 rounded-2xl p-6 backdrop-blur-sm">
            <div className="mb-4">
              <h3 className="font-heading text-lg font-semibold flex items-center gap-2">
                <Key className="h-5 w-5" />
                HuggingFace Token
              </h3>
              <p className="text-sm text-muted-foreground mt-1">
                Connect your HuggingFace account to access gated models like Llama
              </p>
            </div>
            <div className="space-y-4">
              {hfStatusLoading ? (
                <div className="flex items-center gap-2 text-muted-foreground">
                  <Loader2 className="h-4 w-4 animate-spin" />
                  <span className="text-sm">Checking HuggingFace connection...</span>
                </div>
              ) : hfStatus?.configured ? (
                // Connected state - token exists in K8s secrets
                <div className="space-y-4">
                  <div className="flex items-center justify-between">
                    <div className="flex items-center gap-3">
                      {hfStatus.user?.avatarUrl ? (
                        <img
                          src={hfStatus.user.avatarUrl}
                          alt={hfStatus.user.name}
                          className="h-10 w-10 rounded-full"
                        />
                      ) : (
                        <div className="h-10 w-10 rounded-full bg-muted flex items-center justify-center">
                          <Key className="h-5 w-5 text-muted-foreground" />
                        </div>
                      )}
                      <div>
                        {hfStatus.user ? (
                          <>
                            <div className="font-medium">{hfStatus.user.fullname || hfStatus.user.name}</div>
                            <div className="text-sm text-muted-foreground">@{hfStatus.user.name}</div>
                          </>
                        ) : (
                          <>
                            <div className="font-medium">HuggingFace Token</div>
                            <div className="text-sm text-muted-foreground">Token configured</div>
                          </>
                        )}
                      </div>
                    </div>
                    <Badge variant="success">
                      <CheckCircle className="h-3 w-3 mr-1" />
                      Connected
                    </Badge>
                  </div>

                  <div className="rounded-lg bg-green-50 dark:bg-green-950 p-3 text-sm text-green-800 dark:text-green-200">
                    <div className="flex items-center gap-2">
                      <CheckCircle className="h-4 w-4" />
                      <span>Token saved successfully</span>
                    </div>
                  </div>

                  <Button
                    variant="outline"
                    size="sm"
                    onClick={async () => {
                      try {
                        await deleteHfSecret.mutateAsync()
                        toast({
                          title: 'Disconnected',
                          description: 'HuggingFace token has been removed',
                        })
                        refetchHfStatus()
                      } catch (error) {
                        toast({
                          title: 'Error',
                          description: error instanceof Error ? error.message : 'Failed to disconnect',
                          variant: 'destructive',
                        })
                      }
                    }}
                    disabled={deleteHfSecret.isPending}
                  >
                    {deleteHfSecret.isPending ? (
                      <>
                        <Loader2 className="h-4 w-4 mr-2 animate-spin" />
                        Disconnecting...
                      </>
                    ) : (
                      'Disconnect HuggingFace'
                    )}
                  </Button>
                </div>
              ) : (
                // Not connected state
                <div className="space-y-4">
                  <div className="text-sm text-muted-foreground">
                    Sign in with HuggingFace to automatically configure your token for accessing gated models.
                    The token will be securely stored.
                  </div>

                  <Button
                    onClick={async () => {
                      setIsConnectingHf(true)
                      try {
                        await startOAuth()
                      } catch (error) {
                        toast({
                          title: 'Error',
                          description: error instanceof Error ? error.message : 'Failed to start OAuth',
                          variant: 'destructive',
                        })
                        setIsConnectingHf(false)
                      }
                    }}
                    disabled={isConnectingHf}
                    className="bg-[#FFD21E] hover:bg-[#FFD21E]/90 text-black"
                  >
                    {isConnectingHf ? (
                      <>
                        <Loader2 className="h-4 w-4 mr-2 animate-spin" />
                        Redirecting...
                      </>
                    ) : (
                      <>
                        <span aria-hidden="true" className="mr-2 text-base leading-none">🤗</span>
                        Sign in with Hugging Face
                      </>
                    )}
                  </Button>

                  {hfStatus?.configured && !hfStatus.user && (
                    <div className="rounded-lg bg-yellow-50 dark:bg-yellow-950 p-3 text-sm text-yellow-800 dark:text-yellow-200">
                      <div className="flex items-center gap-2">
                        <AlertCircle className="h-4 w-4" />
                        <span>Token exists but could not be validated. Try reconnecting.</span>
                      </div>
                    </div>
                  )}
                </div>
              )}
            </div>
          </div>
        </div>
      )}

      {/* Uninstall Confirmation Dialog */}
      <Dialog open={showUninstallDialog} onOpenChange={setShowUninstallDialog}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Uninstall Runtime</DialogTitle>
            <DialogDescription>
              Are you sure you want to uninstall {runtimes.find(r => r.id === effectiveRuntime)?.name || 'this runtime'}?
              This will remove the operator and all associated resources.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setShowUninstallDialog(false)}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={() => handleUninstall(effectiveRuntime)}>
              Uninstall
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
