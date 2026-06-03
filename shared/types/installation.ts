/**
 * Installation and Helm types
 */

import { InstallationStep } from './settings';
import { GAIE_VERSION } from './versions.generated';

/**
 * Pinned Gateway API Inference Extension version.
 *
 * Single source of truth: /versions.env at the repo root. The value is
 * codegen'd into ./versions.generated.ts; the controller's
 * gateway.DefaultGAIEVersion is injected from the same file via ldflags.
 * Run `make verify-versions` to confirm everything is in sync.
 */
export const PINNED_GAIE_VERSION = GAIE_VERSION;
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
 * Estimated inference throughput for a model on the cluster's GPUs.
 *
 * Two distinct numbers (see issue #139):
 *  - perChatTokensPerSec: single-stream decode speed (memory-bandwidth bound,
 *    ~1/TPOT) — "how snappy does chat feel?"
 *  - concurrentSequences / aggregateTokensPerSec: KV-cache-budget gated capacity
 *    per replica — "how many requests can this serve at once?"
 *
 * Both are rough estimates (no inference is run). When architecture details are
 * unavailable, only perChatTokensPerSec is populated and lowConfidence is true.
 */
export interface GpuThroughputEstimate {
  /** Single-stream decode speed (tokens/sec per chat). */
  perChatTokensPerSec: number;
  /** Approx concurrent sequences per replica at the assumed context length. */
  concurrentSequences?: number;
  /** Aggregate tokens/sec per replica across concurrent sequences. */
  aggregateTokensPerSec?: number;
  /** Resolved GPU model the estimate was computed for. */
  gpuModel: string;
  /** Per-GPU VRAM (GB) used in the estimate. */
  perGpuMemoryGb: number;
  /** Per-GPU memory bandwidth (GB/s) used in the estimate. */
  memBandwidthGBs: number;
  /** Tensor-parallel size (GPUs per replica) assumed. */
  tpSize: number;
  /** Context length (tokens) assumed for KV sizing. */
  contextLen: number;
  /** Topology / capacity label, e.g. "4x80 GB". */
  capacityLabel?: string;
  /** True when architecture data was unavailable, so only perChat is meaningful. */
  lowConfidence: boolean;
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
