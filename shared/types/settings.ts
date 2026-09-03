/**
 * Settings and Provider types
 */

export interface ProviderEngineCapability {
  name: string;
  servingModes?: string[];
  gpuSupport?: boolean;
  cpuSupport?: boolean;
  requiresCRD?: boolean;
  gateway?: Record<string, unknown>;
}

export interface ProviderCapabilities {
  engines: string[];
  engineCapabilities?: ProviderEngineCapability[];
  modes: string[];
  modelSources: string[];
  routerModes: string[];
  features: Record<string, boolean>;
}

export interface ProviderDeploymentDefaults {
  defaultEngine?: string;
  defaultMode?: string;
  defaultResources?: Record<string, unknown>;
}

export interface ProviderHealthConfig {
  crds?: Array<string | { name: string; displayName?: string }>;
  operatorPods?: Array<{
    namespace?: string;
    selectors: string[];
  }>;
  /** @deprecated Use operatorPods instead. Kept for compatibility with older provider annotations. */
  operator?: {
    namespace?: string;
    podSelectors?: string[];
    fallbackPodSelectors?: string[];
    crossNamespacePodSelectors?: string[];
  };
  status?: {
    readyPath?: string;
    conditions?: string[];
  };
}

export interface ProviderInfo {
  id: string;
  name: string;
  description: string;
  defaultNamespace: string;
  documentationUrl?: string;
  icon?: string;
  warnings?: string[];
  installable?: boolean;
  requiresCRD?: boolean;
  capabilities?: ProviderCapabilities;
  deploymentDefaults?: ProviderDeploymentDefaults;
  health?: ProviderHealthConfig;
}

export interface CRDConfig {
  apiGroup: string;
  apiVersion: string;
  plural: string;
  kind: string;
}

export interface InstallationStep {
  title: string;
  command?: string;
  description: string;
}

export interface HelmRepo {
  name: string;
  url: string;
}

export interface HelmChart {
  name: string;
  chart: string;
  version?: string;
  namespace: string;
  createNamespace?: boolean;
  values?: Record<string, unknown>;
  skipCrds?: boolean;
  fetchUrl?: string;
  preCrdUrls?: string[];
  preInstallMissingCrds?: boolean;
}

export interface ProviderDetails extends ProviderInfo {
  crdConfig: CRDConfig;
  installationSteps: InstallationStep[];
  helmRepos: HelmRepo[];
  helmCharts: HelmChart[];
}

export interface AppConfig {
  /** @deprecated No longer used - each deployment specifies its own provider */
  activeProviderId?: string;
  defaultNamespace?: string;
}

/**
 * Authentication configuration exposed to frontend
 */
export interface AuthConfig {
  enabled: boolean;
}

/**
 * User information from authenticated token
 */
export interface UserInfo {
  username: string;
  groups?: string[];
}

export interface Settings {
  config: AppConfig;
  providers: ProviderInfo[];
  auth: AuthConfig;
}

export type InstallationState = 'installed' | 'not-installed' | 'unknown';

/**
 * Runtime status for the runtimes endpoint
 * Used to show installation and health status of each runtime
 */
export interface RuntimeStatus {
  id: string;
  name: string;
  description?: string;
  defaultNamespace?: string;
  documentationUrl?: string;
  icon?: string;
  warnings?: string[];
  installable?: boolean;
  requiresCRD?: boolean; // Whether the provider depends on an upstream runtime operator/CRD
  capabilities?: ProviderCapabilities;
  deploymentDefaults?: ProviderDeploymentDefaults;
  health?: ProviderHealthConfig;
  /** Explicit installation verdict. Prefer this over the legacy installed boolean when present. */
  installationState?: InstallationState;
  installed: boolean;
  healthy: boolean;
  crdFound?: boolean;
  operatorRunning?: boolean;
  version?: string;
  message?: string;
  /**
   * True when AI Runway's own integration for this runtime has registered an
   * InferenceProviderConfig in the cluster. True for anything appearing in this
   * list; stated explicitly so the UI can distinguish the integration being
   * present from the underlying runtime being installed.
   */
  shimRegistered?: boolean;
  /**
   * True when AI Runway's integration for this runtime is responding. Derived
   * from a valid heartbeat within PROVIDER_HEALTH_STALENESS_MS (3 minutes by
   * default), independently of whether the underlying runtime is ready.
   * Distinct from {@link installed}, which describes the underlying runtime.
   */
  shimConnected?: boolean;
  /** ISO timestamp of the last check-in from the integration, if reported. */
  shimLastHeartbeat?: string;
}

/**
 * Response for GET /api/runtimes/status
 */
export interface RuntimesStatusResponse {
  runtimes: RuntimeStatus[];
}
