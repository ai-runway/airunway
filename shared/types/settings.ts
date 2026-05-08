/**
 * Settings and Provider types
 */

export interface ProviderInfo {
  id: string;
  name: string;
  description: string;
  defaultNamespace: string;
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

/**
 * Runtime status for the runtimes endpoint
 * Used to show installation and health status of each runtime
 */
export interface RuntimeStatus {
  id: string;           // 'dynamo' | 'kuberay'
  name: string;         // Display name
  installed: boolean;   // Runtime is ready to use
  healthy: boolean;     // Runtime service is running
  crdFound?: boolean;   // Provider API is available
  operatorRunning?: boolean; // Runtime service pods are ready
  requiresCRD?: boolean; // Whether the provider depends on an upstream runtime operator/CRD
  version?: string;     // Detected version
  message?: string;     // Status message
}

/**
 * Response for GET /api/runtimes/status
 */
export interface RuntimesStatusResponse {
  runtimes: RuntimeStatus[];
}
