import { Engine } from './model';

// ==================== ModelDeployment CRD Types ====================
// These types mirror the Go CRD types in controller/api/v1alpha1/

export type ModelSource = 'huggingface' | 'custom';
export type EngineType = 'vllm' | 'sglang' | 'trtllm' | 'llamacpp';
export type ServingMode = 'aggregated' | 'disaggregated';
export type DeploymentPhase = 'Pending' | 'Deploying' | 'Running' | 'Failed' | 'Terminating';
export type PodPhase = 'Pending' | 'Running' | 'Succeeded' | 'Failed' | 'Unknown';

// Storage types (mirrors controller StorageSpec / StorageVolume)
export type VolumePurpose = 'modelCache' | 'compilationCache' | 'custom';
export type PersistentVolumeAccessMode = 'ReadWriteOnce' | 'ReadWriteMany' | 'ReadOnlyMany' | 'ReadWriteOncePod';

export interface StorageVolume {
  name: string;
  claimName?: string;
  mountPath?: string;
  purpose?: VolumePurpose;
  readOnly?: boolean;
  size?: string;
  storageClassName?: string;
  accessMode?: PersistentVolumeAccessMode;
}

export interface StorageSpec {
  volumes?: StorageVolume[];
}

// Legacy types for backward compatibility
export type DeploymentMode = ServingMode;
export type GgufRunMode = 'build' | 'direct';
export type RouterMode = 'default' | 'kv' | 'round-robin';
export type KaitoResourceType = 'workspace' | 'inferenceset';

export interface RecipeProvenance {
  source?: string;
  id?: string;
  strategy?: string;
  hardware?: string;
  variant?: string;
  precision?: string;
  features?: string[];
  revision?: string;
}

export interface DeploymentConfig {
  name: string;
  namespace: string;
  modelId: string;
  engine: Engine;
  mode: DeploymentMode;
  provider?: string;
  servedModelName?: string;
  routerMode: RouterMode;
  replicas: number;
  hfTokenSecret?: string;
  contextLength?: number;
  enforceEager: boolean;
  enablePrefixCaching: boolean;
  trustRemoteCode: boolean;
  resources?: {
    gpu: number;
    memory?: string;
  };
  engineArgs?: Record<string, unknown>;
  engineExtraArgs?: string[];
  env?: Record<string, string>;
  prefillReplicas?: number;
  decodeReplicas?: number;
  prefillGpus?: number;
  decodeGpus?: number;
  modelSource?: 'premade' | 'huggingface' | 'vllm';
  premadeModel?: string;
  ggufFile?: string;
  ggufRunMode?: GgufRunMode;
  imageRef?: string;
  computeType?: 'cpu' | 'gpu';
  maxModelLen?: number;
  kaitoResourceType?: KaitoResourceType;
  providerOverrides?: Record<string, unknown>;
  recipeProvenance?: RecipeProvenance;
  storage?: StorageSpec;
  gatewayEnabled?: boolean;
}

export interface ModelSpec {
  id: string;
  servedName?: string;
  source?: ModelSource;
  storage?: StorageSpec;
}

export interface ProviderSpec {
  name?: string;
  overrides?: Record<string, unknown>;
}

export interface EngineSpec {
  type: EngineType;
  image?: string;
  contextLength?: number;
  trustRemoteCode?: boolean;
  enablePrefixCaching?: boolean;
  enforceEager?: boolean;
  args?: Record<string, string>;
  extraArgs?: string[];
}

export interface ServingSpec {
  mode?: ServingMode;
}

export interface GPUSpec {
  count: number;
  type?: string;
}

export interface ResourceSpec {
  gpu?: GPUSpec;
  memory?: string;
  cpu?: string;
}

export interface ComponentScalingSpec {
  replicas: number;
  gpu?: GPUSpec;
}

export interface ScalingSpec {
  replicas?: number;
  minReplicas?: number;
  maxReplicas?: number;
  prefill?: ComponentScalingSpec;
  decode?: ComponentScalingSpec;
}

export interface EnvVar {
  name: string;
  value?: string;
  valueFrom?: Record<string, unknown>;
}

export interface PodTemplateSpec {
  nodeSelector?: Record<string, string>;
  tolerations?: Array<{
    key?: string;
    operator?: string;
    value?: string;
    effect?: string;
    tolerationSeconds?: number;
  }>;
  annotations?: Record<string, string>;
  labels?: Record<string, string>;
}

export interface SecretSpec {
  huggingFaceToken?: string;
  custom?: string[];
}

export interface GatewaySpec {
  enabled?: boolean;
  modelName?: string;
  httpRouteRef?: string;
}

export interface ModelDeploymentSpec {
  model: ModelSpec;
  provider?: ProviderSpec;
  engine: EngineSpec;
  serving?: ServingSpec;
  scaling?: ScalingSpec;
  resources?: ResourceSpec;
  image?: string;
  env?: EnvVar[];
  podTemplate?: PodTemplateSpec;
  secrets?: SecretSpec;
  gateway?: GatewaySpec;
}

export interface ReplicaStatus {
  desired: number;
  ready: number;
  available: number;
}

export interface ProviderStatus {
  name?: string;
  selectedReason?: string;
  resourceRef?: {
    apiVersion?: string;
    kind?: string;
    name?: string;
    namespace?: string;
  };
}

export interface Condition {
  type: string;
  status: 'True' | 'False' | 'Unknown';
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
}

export type APIFormat = 'openai-chat' | 'openai-responses' | 'anthropic-messages';

export interface GatewayStatus {
  endpoint?: string;
  modelName?: string;
  apiFormats?: APIFormat[];
}

export interface GatewayInfo {
  available: boolean;
  endpoint?: string;
  models?: GatewayModelInfo[];
}

export interface GatewayModelInfo {
  name: string;
  deploymentName: string;
  provider?: string;
  ready: boolean;
}

export interface ImageStatus {
  requested?: string;
  resolved?: string;
  repository?: string;
  tag?: string;
  digest?: string;
  source?: 'stable' | 'nightly' | 'launch' | 'custom' | string;
  createdAt?: string;
  revision?: string;
  age?: string;
  inNightly?: boolean;
  verified?: boolean;
  verificationMessage?: string;
  message?: string;
}

export interface EndpointStatus {
  service?: string;
  port?: number;
}

export interface EngineStatus {
  selectedReason?: string;
  type?: string;
}

export interface ModelDeploymentStatus {
  phase?: DeploymentPhase;
  message?: string;
  engine?: EngineStatus;
  provider?: ProviderStatus;
  replicas?: ReplicaStatus;
  prefillReplicas?: {
    desired: number;
    ready: number;
  };
  decodeReplicas?: {
    desired: number;
    ready: number;
  };
  endpoint?: EndpointStatus;
  gateway?: GatewayStatus;
  image?: ImageStatus;
  conditions?: Condition[];
  observedGeneration?: number;
}

export interface ModelDeployment {
  apiVersion: string;
  kind: string;
  metadata: {
    name: string;
    namespace: string;
    creationTimestamp?: string;
    labels?: Record<string, string>;
    annotations?: Record<string, string>;
  };
  spec: ModelDeploymentSpec;
  status?: ModelDeploymentStatus;
}

// ==================== API Types ====================

export interface PodStatus {
  name: string;
  phase: PodPhase;
  ready: boolean;
  restarts: number;
  node?: string;
  reason?: string;
  message?: string;
}

export interface DeploymentStatus {
  name: string;
  namespace: string;
  modelId: string;
  servedModelName?: string;
  engine?: Engine;
  mode: ServingMode;
  phase: DeploymentPhase;
  provider: string;
  replicas: {
    desired: number;
    ready: number;
    available: number;
  };
  conditions?: Condition[];
  pods: PodStatus[];
  createdAt: string;
  // Service reference in "name[:port]" form used by the UI/plugin for access commands.
  frontendService?: string;
  storage?: StorageSpec;
  prefillReplicas?: {
    desired: number;
    ready: number;
  };
  decodeReplicas?: {
    desired: number;
    ready: number;
  };
  gateway?: GatewayStatus;
  image?: ImageStatus;
}

// ==================== Conversion Functions ====================

const LEGACY_FRONTEND_SERVICE_PORT = 8000;

export interface FrontendServiceRef {
  serviceName: string;
  servicePort?: number;
}

export function formatFrontendService(serviceName?: string, servicePort?: number): string | undefined {
  if (!serviceName) {
    return undefined;
  }

  if (servicePort && servicePort > 0) {
    return `${serviceName}:${servicePort}`;
  }

  return serviceName;
}

export function parseFrontendService(frontendService?: string): FrontendServiceRef | undefined {
  if (!frontendService) {
    return undefined;
  }

  const [serviceName, rawServicePort] = frontendService.split(':', 2);

  if (!serviceName) {
    return undefined;
  }

  if (!rawServicePort) {
    return { serviceName };
  }

  const servicePort = Number.parseInt(rawServicePort, 10);
  if (Number.isNaN(servicePort) || servicePort <= 0) {
    return { serviceName };
  }

  return {
    serviceName,
    servicePort,
  };
}

export function buildPortForwardCommand(
  deployment: Pick<DeploymentStatus, 'name' | 'namespace' | 'frontendService'>,
  localPort = LEGACY_FRONTEND_SERVICE_PORT
): string {
  const frontendService = parseFrontendService(deployment.frontendService);
  const serviceName = frontendService?.serviceName || `${deployment.name}-frontend`;
  const servicePort = frontendService?.servicePort || LEGACY_FRONTEND_SERVICE_PORT;

  return `kubectl port-forward svc/${serviceName} ${localPort}:${servicePort} -n ${deployment.namespace}`;
}

const FATAL_POD_REASONS = new Set([
  'CrashLoopBackOff',
  'CreateContainerConfigError',
  'CreateContainerError',
  'ErrImagePull',
  'ImagePullBackOff',
  'InvalidImageName',
  'RunContainerError',
  'StartError',
]);

function hasReadyCondition(status: ModelDeploymentStatus): boolean {
  return status.conditions?.some((condition) => condition.type === 'Ready' && condition.status === 'True') ?? false;
}

function resolveReplicaStatus(
  spec: ModelDeploymentSpec,
  status: ModelDeploymentStatus,
  pods: PodStatus[],
): ReplicaStatus {
  const desired = status.replicas?.desired;
  const ready = status.replicas?.ready;
  const available = status.replicas?.available;

  if (desired !== undefined || ready !== undefined || available !== undefined) {
    return {
      desired: desired ?? 0,
      ready: ready ?? 0,
      available: available ?? 0,
    };
  }

  if (spec.serving?.mode === 'disaggregated') {
    const prefillDesired = status.prefillReplicas?.desired ?? spec.scaling?.prefill?.replicas ?? 0;
    const decodeDesired = status.decodeReplicas?.desired ?? spec.scaling?.decode?.replicas ?? 0;
    const prefillReady = status.prefillReplicas?.ready ?? 0;
    const decodeReady = status.decodeReplicas?.ready ?? 0;
    const totalReady = prefillReady + decodeReady;

    return {
      desired: prefillDesired + decodeDesired,
      ready: totalReady,
      available: totalReady,
    };
  }

  const derivedDesired = spec.scaling?.replicas ?? (pods.length > 0 ? 1 : 0);
  const allPodsReady = pods.length > 0 && pods.every((pod) => pod.ready);
  const derivedReady = hasReadyCondition(status) || allPodsReady ? derivedDesired : 0;

  return {
    desired: derivedDesired,
    ready: derivedReady,
    available: derivedReady,
  };
}

function resolveDeploymentPhase(spec: ModelDeploymentSpec, status: ModelDeploymentStatus, pods: PodStatus[]): DeploymentPhase {
  const reportedPhase = status.phase;

  if (reportedPhase === 'Terminating') {
    return 'Terminating';
  }

  const { desired: desiredReplicas, ready: readyReplicas } = resolveReplicaStatus(spec, status, pods);
  const hasReadyPods = pods.some((pod) => pod.ready);
  const hasRunningPods = pods.some((pod) => pod.phase === 'Running');
  const hasScheduledPendingPods = pods.some((pod) => pod.phase === 'Pending' && Boolean(pod.node));
  const hasUnscheduledPendingPods = pods.some((pod) => pod.phase === 'Pending' && !pod.node);
  const hasFailedPods = pods.some(
    (pod) => pod.phase === 'Failed' || (pod.reason ? FATAL_POD_REASONS.has(pod.reason) : false),
  );
  const isReady = desiredReplicas > 0 ? readyReplicas >= desiredReplicas : hasReadyPods;

  if (isReady && (reportedPhase === 'Running' || hasReadyPods)) {
    return 'Running';
  }

  if (hasFailedPods) {
    return 'Failed';
  }

  if (hasRunningPods || hasScheduledPendingPods) {
    return 'Deploying';
  }

  if (hasUnscheduledPendingPods) {
    return 'Pending';
  }

  return reportedPhase || 'Pending';
}

function resolveEngineType(config: DeploymentConfig): EngineType {
  if (config.provider === 'kaito') {
    if (config.modelSource === 'vllm') {
      return 'vllm';
    }
    if (config.modelSource === 'huggingface' || config.modelSource === 'premade') {
      return 'llamacpp';
    }
  }

  return config.engine as EngineType;
}

const RECIPE_PROVENANCE_GENERATED_BY = 'vllm-recipe-resolver';
const RECIPE_PROVENANCE_ANNOTATION_PREFIX = 'airunway.ai/recipe.';

export function recipeProvenanceToAnnotations(recipeProvenance?: RecipeProvenance): Record<string, string> | undefined {
  if (!recipeProvenance) {
    return undefined;
  }

  const annotations: Record<string, string> = {};
  let hasProvenance = false;

  const addStringAnnotation = (field: keyof Omit<RecipeProvenance, 'features'>) => {
    const value = recipeProvenance[field];
    // Only emit meaningful provenance: skip undefined/null and empty/whitespace
    // strings so we never produce annotations like airunway.ai/recipe.source=""
    // or mark the object generated-by without real provenance.
    if (typeof value === 'string' && value.trim() !== '') {
      hasProvenance = true;
      annotations[`${RECIPE_PROVENANCE_ANNOTATION_PREFIX}${field}`] = value.trim();
    }
  };

  addStringAnnotation('source');
  addStringAnnotation('id');
  addStringAnnotation('strategy');
  addStringAnnotation('hardware');
  addStringAnnotation('variant');
  addStringAnnotation('precision');
  addStringAnnotation('revision');

  if (Array.isArray(recipeProvenance.features) && recipeProvenance.features.length > 0) {
    hasProvenance = true;
    annotations[`${RECIPE_PROVENANCE_ANNOTATION_PREFIX}features`] = JSON.stringify(recipeProvenance.features);
  }

  if (!hasProvenance) {
    return undefined;
  }

  return {
    'airunway.ai/generated-by': RECIPE_PROVENANCE_GENERATED_BY,
    ...annotations,
  };
}

function normalizeEngineArgs(engineArgs?: Record<string, unknown>): Record<string, string> | undefined {
  if (!engineArgs) {
    return undefined;
  }

  const normalized = Object.entries(engineArgs).flatMap(([key, value]) => {
    if (value === undefined || value === null) {
      return [];
    }

    if (typeof value === 'string') {
      return [[key, value] as const];
    }

    if (typeof value === 'number' || typeof value === 'boolean' || typeof value === 'bigint') {
      return [[key, String(value)] as const];
    }

    return [[key, JSON.stringify(value)] as const];
  });

  return normalized.length > 0 ? Object.fromEntries(normalized) : undefined;
}

export function isCpuOnlyDeployment(config: Pick<DeploymentConfig, 'computeType'>): boolean {
  return config.computeType === 'cpu';
}

export function toModelDeploymentSpec(config: DeploymentConfig): ModelDeploymentSpec {
  const cpuOnlyDeployment = isCpuOnlyDeployment(config);
  const spec: ModelDeploymentSpec = {
    model: {
      id: config.modelId,
      servedName: config.servedModelName,
      source: 'huggingface',
    },
    engine: {
      type: resolveEngineType(config),
      contextLength: config.contextLength || config.maxModelLen,
      trustRemoteCode: config.trustRemoteCode,
      enablePrefixCaching: config.enablePrefixCaching,
      enforceEager: config.enforceEager,
      args: normalizeEngineArgs(config.engineArgs),
      extraArgs: config.engineExtraArgs,
    },
    serving: {
      mode: config.mode,
    },
  };

  if (config.env && Object.keys(config.env).length > 0) {
    spec.env = Object.entries(config.env).map(([name, value]) => ({ name, value }));
  }

  if (config.imageRef) {
    if (config.provider === 'vllm') {
      spec.engine.image = config.imageRef;
    } else {
      spec.image = config.imageRef;
    }
  }

  // Merge routerMode into providerOverrides when set to a non-default value.
  const effectiveOverrides: Record<string, unknown> = {
    ...config.providerOverrides,
    ...(config.routerMode && config.routerMode !== 'default' && { routerMode: config.routerMode }),
  };
  const hasOverrides = Object.keys(effectiveOverrides).length > 0;

  if (config.provider || hasOverrides) {
    spec.provider = {
      ...(config.provider && { name: config.provider }),
      ...(hasOverrides && { overrides: effectiveOverrides }),
    };
  }

  if (config.mode === 'aggregated') {
    spec.scaling = {
      replicas: config.replicas,
    };
    if (cpuOnlyDeployment) {
      spec.resources = {
        gpu: {
          count: 0,
          type: 'nvidia.com/gpu',
        },
        memory: config.resources?.memory,
      };
    } else if (config.resources?.gpu) {
      spec.resources = {
        gpu: {
          count: config.resources.gpu,
          type: 'nvidia.com/gpu',
        },
        memory: config.resources.memory,
      };
    }
  } else if (config.mode === 'disaggregated') {
    spec.scaling = {
      prefill: {
        replicas: config.prefillReplicas || 1,
        gpu: !cpuOnlyDeployment && config.prefillGpus
          ? { count: config.prefillGpus, type: 'nvidia.com/gpu' }
          : undefined,
      },
      decode: {
        replicas: config.decodeReplicas || 1,
        gpu: !cpuOnlyDeployment && config.decodeGpus
          ? { count: config.decodeGpus, type: 'nvidia.com/gpu' }
          : undefined,
      },
    };
  }

  if (config.hfTokenSecret) {
    spec.secrets = {
      huggingFaceToken: config.hfTokenSecret,
    };
  }

  // Add storage volumes if configured
  if (config.storage?.volumes && config.storage.volumes.length > 0) {
    spec.model.storage = {
      volumes: config.storage.volumes,
    };
  }

  if (config.gatewayEnabled !== undefined) {
    spec.gateway = {
      enabled: config.gatewayEnabled,
    };
  }

  return spec;
}

export function toDeploymentStatus(md: ModelDeployment, pods: PodStatus[] = []): DeploymentStatus {
  const status = md.status || {};
  const spec = md.spec;
  const frontendServiceName = status.endpoint?.service || md.metadata.name;
  const replicas = resolveReplicaStatus(spec, status, pods);

  return {
    name: md.metadata.name,
    namespace: md.metadata.namespace,
    modelId: spec.model.id,
    servedModelName: spec.model.servedName,
    engine: (spec.engine?.type as Engine) || (status.engine?.type as Engine) || undefined,
    mode: spec.serving?.mode || 'aggregated',
    phase: resolveDeploymentPhase(spec, status, pods),
    provider: status.provider?.name || spec.provider?.name || 'unknown',
    replicas,
    conditions: status.conditions,
    pods,
    createdAt: md.metadata.creationTimestamp || new Date().toISOString(),
    frontendService: formatFrontendService(frontendServiceName, status.endpoint?.port),
    prefillReplicas: status.prefillReplicas,
    decodeReplicas: status.decodeReplicas,
    gateway: status.gateway,
    image: status.image,
    storage: spec.model.storage,
  };
}

export function toModelDeploymentManifest(config: DeploymentConfig): ModelDeployment {
  const annotations = recipeProvenanceToAnnotations(config.recipeProvenance);

  return {
    apiVersion: 'airunway.ai/v1alpha1',
    kind: 'ModelDeployment',
    metadata: {
      name: config.name,
      namespace: config.namespace,
      labels: {
        'app.kubernetes.io/name': 'airunway',
        'app.kubernetes.io/instance': config.name,
        'app.kubernetes.io/managed-by': 'airunway',
      },
      ...(annotations && { annotations }),
    },
    spec: toModelDeploymentSpec(config),
  };
}

// ==================== Request/Response Types ====================

export interface CreateDeploymentRequest {
  config: DeploymentConfig;
}

export interface DeploymentListResponse {
  deployments: DeploymentStatus[];
}

export interface ClusterStatus {
  connected: boolean;
  namespace: string;
  clusterName?: string;
  error?: string;
}

export interface PodLogsOptions {
  podName?: string;
  container?: string;
  tailLines?: number;
  timestamps?: boolean;
}

export interface PodLogsResponse {
  logs: string;
  podName: string;
  container?: string;
}
