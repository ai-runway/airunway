/**
 * Installation and Helm types
 */

import { InstallationStep } from './settings';

/**
 * Pinned Gateway API Inference Extension version.
 * Must match the controller's DefaultGAIEVersion in controller/internal/gateway/detection.go.
 */
export const PINNED_GAIE_VERSION = 'v1.3.1';
export const GAIE_CRD_URL = `https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/${PINNED_GAIE_VERSION}/manifests.yaml`;
export const GATEWAY_API_CRD_URL = 'https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml';

/**
 * Label that marks a Gateway resource as the AIRunway inference gateway.
 * Must match the controller's LabelInferenceGateway in controller/internal/gateway/detection.go.
 */
export const INFERENCE_GATEWAY_LABEL = 'airunway.ai/inference-gateway';

export interface HelmStatus {
  available: boolean;
  version?: string;
  error?: string;
}

export interface InstallationStatus {
  providerId: string;
  providerName: string;
  installed: boolean;
  version?: string;
  message?: string;
  crdFound?: boolean;
  operatorRunning?: boolean;
  requiresCRD?: boolean;
  installable?: boolean;
  installationSteps: InstallationStep[];
  helmCommands: string[];
}

export interface InstallResult {
  success: boolean;
  message: string;
  alreadyInstalled?: boolean;
  installationStatus?: {
    installed: boolean;
    message?: string;
  };
  results?: Array<{
    step: string;
    success: boolean;
    output: string;
    error?: string;
  }>;
}

export interface GPUOperatorStatus {
  installed: boolean;
  crdFound: boolean;
  operatorRunning: boolean;
  gpusAvailable: boolean;
  totalGPUs: number;
  gpuNodes: string[];
  message: string;
  helmCommands: string[];
}

export interface GPUOperatorInstallResult {
  success: boolean;
  message: string;
  alreadyInstalled?: boolean;
  status?: GPUOperatorStatus;
  results?: Array<{
    step: string;
    success: boolean;
    output: string;
    error?: string;
  }>;
}

export interface NodeGpuInfo {
  nodeName: string;
  totalGpus: number;
  allocatedGpus: number;
  availableGpus: number;
}

export interface ClusterGpuCapacity {
  totalGpus: number;
  allocatedGpus: number;
  availableGpus: number;
  maxContiguousAvailable: number;
  totalMemoryGb?: number;         // Total GPU memory per GPU (e.g., 80 for A100 80GB)
  nodes: NodeGpuInfo[];
}

/**
 * Gateway CRD installation status
 */
export interface GatewayCRDStatus {
  gatewayApiInstalled: boolean;
  inferenceExtInstalled: boolean;
  gatewayApiVersion?: string;
  inferenceExtVersion?: string;
  pinnedVersion: string;
  gatewayAvailable: boolean;
  gatewayEndpoint?: string;
  message: string;
  installCommands: string[];
}

/**
 * Result of installing Gateway API / GAIE CRDs
 */
export interface GatewayCRDInstallResult {
  success: boolean;
  message: string;
  results?: Array<{
    step: string;
    success: boolean;
    output: string;
    error?: string;
  }>;
}
