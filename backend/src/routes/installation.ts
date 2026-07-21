import { Hono } from 'hono';
import { HTTPException } from 'hono/http-exception';
import { zValidator } from '@hono/zod-validator';
import { z } from 'zod';
import { kubernetesService } from '../services/kubernetes';
import { helmService } from '../services/helm';
import { getProviderHealth } from '../services/providerHealth';
import { getKnownGpuInfo, normalizeKnownGpuModel, gpuSupportsFp8 } from '../services/costEstimation';
import { huggingFaceService, isValidHfRepoId } from '../services/huggingface';
import {
  bytesPerWeightFor,
  bytesPerKvFor,
  deriveTpSizeToFitWeights,
  estimatePerChatTokensPerSec,
  estimateConcurrentCapacity,
} from '../services/gpuPerformance';
import type { GpuThroughputEstimate, NodePoolInfo } from '@airunway/shared';
import logger from '../lib/logger';
import { extractProviderDetails, type ProviderHelmChartDetails } from '../lib/providers';

/** Default context length (tokens) assumed when a model doesn't specify one. */
const DEFAULT_CONTEXT_LEN = 4096;

/**
 * Cap applied to the context length used to size KV cache. Some models advertise
 * very large windows (128K–1M) and callers may forward that advertised value as
 * an explicit `contextLen`; serving rarely uses the full window, and sizing KV
 * against it would collapse concurrency estimates to ~zero. Applied to both the
 * arch-inferred max and an explicit `contextLen` query param.
 */
const MAX_CONTEXT_LEN = 32768;

/** Query schema for GET /gpu-throughput. */
const gpuThroughputQuerySchema = z.object({
  // Accept any reasonably-bounded string here rather than enforcing a valid HF
  // repo id at the schema level. The estimate can be produced from `paramCount`
  // alone (low-confidence, bandwidth-only), so a curated/custom model id that is
  // valid for Airunway but not a HuggingFace repo must NOT hard-400. The handler
  // gates the token-bearing HF architecture fetch on `isValidHfRepoId(modelId)`,
  // so an invalid/non-HF id simply skips that lookup and degrades gracefully.
  modelId: z.string().min(1).max(200).optional(),
  paramCount: z.coerce.number().positive().max(9_000_000_000_000).optional(),
  contextLen: z.coerce.number().int().positive().max(1_048_576).optional(),
  quantization: z.enum(['fp8', 'int8', 'fp16', 'bf16']).optional(),
  kvCacheDtype: z.enum(['fp8', 'int8', 'fp16', 'bf16']).optional(),
  gpuModel: z.string().min(1).optional(),
  tpSize: z.coerce.number().int().positive().max(64).optional(),
});

interface GpuEstimateSelection {
  resolvedGpuModel: string;
  perGpuMemoryGb: number;
  memBandwidthGBs: number;
  capacityLabel?: string;
  maxContiguous: number;
}

/**
 * GPUs hosted on a single node of this pool, i.e. the most GPUs that can back a
 * single replica (tensor-parallel group). Pools report `gpuCount` as the total
 * across all nodes, so divide by `nodeCount` (assuming homogeneous pools).
 */
function perNodeGpuCount(pool: NodePoolInfo): number {
  if (!pool.nodeCount || pool.nodeCount <= 0) return pool.gpuCount || 1;
  return Math.max(1, Math.floor(pool.gpuCount / pool.nodeCount));
}

/**
 * Pick the node pool / GPU model to base the throughput estimate on. Prefers a
 * pool matching an explicitly-requested gpuModel, else the highest-VRAM GPU
 * pool. Returns undefined when no pool maps to a GPU we have specs for.
 *
 * `maxContiguous` is the per-node GPU count of the selected pool (not a
 * cluster-wide or pool-total value) so it correctly bounds the per-replica
 * tensor-parallel size.
 */
function selectGpuForEstimate(
  capacity: Awaited<ReturnType<typeof kubernetesService.getDetailedClusterGpuCapacity>>,
  requestedGpuModel?: string
): GpuEstimateSelection | undefined {
  const pools = (capacity.nodePools || []).filter((p) => p.gpuModel);

  // 1. Explicit request: only honor it if a cluster pool actually runs that GPU
  //    model. Otherwise fall through so we never estimate for absent hardware.
  //    Strict normalization: an unrecognized requested label yields `undefined`
  //    here, so we never match it against a pool and instead fall through to the
  //    highest-VRAM known pool (rather than coercing it to an A10).
  const requestedNormalized = requestedGpuModel
    ? normalizeKnownGpuModel(requestedGpuModel)
    : undefined;
  if (requestedNormalized) {
    const matchedPool = pools.find(
      (p) => normalizeKnownGpuModel(p.gpuModel as string) === requestedNormalized
    );
    const info = matchedPool ? getKnownGpuInfo(matchedPool.gpuModel as string) : undefined;
    if (matchedPool && info) {
      const perNode = perNodeGpuCount(matchedPool);
      return {
        resolvedGpuModel: requestedNormalized,
        perGpuMemoryGb: info.memoryGb,
        memBandwidthGBs: info.memBandwidthGBs,
        capacityLabel: `${perNode}x${info.memoryGb} GB`,
        maxContiguous: perNode,
      };
    }
  }

  // 2. Otherwise choose the pool with the most per-GPU VRAM. Pools whose GPU
  //    label isn't in our static spec table are skipped (getKnownGpuInfo →
  //    undefined) so an unknown GPU is never silently estimated as an A10.
  let best: GpuEstimateSelection | undefined;
  for (const pool of pools) {
    const info = getKnownGpuInfo(pool.gpuModel as string);
    if (!info) continue;
    if (!best || info.memoryGb > best.perGpuMemoryGb) {
      const perNode = perNodeGpuCount(pool);
      best = {
        resolvedGpuModel: normalizeKnownGpuModel(pool.gpuModel as string) as string,
        perGpuMemoryGb: info.memoryGb,
        memBandwidthGBs: info.memBandwidthGBs,
        capacityLabel: `${perNode}x${info.memoryGb} GB`,
        maxContiguous: perNode,
      };
    }
  }
  return best;
}

function shouldPreInstallMissingCrds(providerId: string, chart: ProviderHelmChartDetails) {
  return (
    (providerId === 'kaito' && chart.chart === 'kaito/workspace')
    || (providerId === 'dynamo' && chart.name === 'dynamo-platform')
  );
}

function normalizeInstallCharts(providerId: string, charts: ProviderHelmChartDetails[]): ProviderHelmChartDetails[] {
  return charts.map((chart) => (
    shouldPreInstallMissingCrds(providerId, chart)
      ? {
          ...chart,
          preInstallMissingCrds: true,
          skipCrds: true,
        }
      : chart
  ));
}

const INSTALLER_PERMISSION_GUIDANCE = 'Automatic installation requires elevated installer permissions. Ask an admin to apply the optional dashboard installer permissions manifest (deploy/dashboard-installer-rbac.yaml) or run the commands manually.';

function isInstallerPermissionError(output?: string): boolean {
  if (!output) return false;
  return /\bforbidden\b|cannot (?:create|update|patch|delete|get|list|watch)|is forbidden|attempting to grant RBAC permissions not currently held|requires.*(?:permission|privilege)/i.test(output);
}

function isHelmOwnershipError(output?: string): boolean {
  if (!output) return false;
  return /invalid ownership metadata|cannot be imported into the current release|missing key "app\.kubernetes\.io\/managed-by"|missing key "meta\.helm\.sh\/release-name"/i.test(output);
}

function extractOwnershipConflictResource(output: string): string | null {
  // Helm formats: `CustomResourceDefinition "name" in namespace "ns" exists ...`
  const match = output.match(/(\w[\w-]*)\s+"([^"]+)"\s+in namespace\s+"([^"]*)"\s+exists/i);
  if (!match) return null;
  const [, kind, name, ns] = match;
  return ns ? `${kind} "${name}" in namespace "${ns}"` : `${kind} "${name}"`;
}

function installationFailureStatus(output?: string): 403 | 409 | 500 {
  if (isInstallerPermissionError(output)) return 403;
  if (isHelmOwnershipError(output)) return 409;
  return 500;
}

function installationFailureMessage(prefix: string, output?: string): string {
  const detail = output?.trim() || 'Unknown error';
  if (isInstallerPermissionError(detail)) {
    return `${prefix}: ${INSTALLER_PERMISSION_GUIDANCE} Details: ${detail}`;
  }
  if (isHelmOwnershipError(detail)) {
    const resource = extractOwnershipConflictResource(detail);
    const subject = resource ?? 'a required cluster resource';
    return `${prefix}: Cannot install because ${subject} already exists on the cluster and is owned by another tool. Uninstall the conflicting tool, or use the manual installation commands shown below.`;
  }
  return `${prefix}: ${detail}`;
}

const installation = new Hono()
  .get('/helm/status', async (c) => {
    const helmStatus = await helmService.checkHelmAvailable();
    return c.json(helmStatus);
  })
  .get('/gpu-operator/status', async (c) => {
    const status = await kubernetesService.checkGPUOperatorStatus();
    const helmCommands = helmService.getGpuOperatorCommands();

    return c.json({
      ...status,
      helmCommands,
    });
  })
  .get('/gpu-capacity', async (c) => {
    const capacity = await kubernetesService.getClusterGpuCapacity();
    return c.json(capacity);
  })
  .get('/gpu-capacity/detailed', async (c) => {
    const capacity = await kubernetesService.getDetailedClusterGpuCapacity();
    return c.json(capacity);
  })
  .get('/gpu-throughput', zValidator('query', gpuThroughputQuerySchema), async (c) => {
    const { modelId, paramCount, contextLen, quantization, kvCacheDtype, gpuModel, tpSize } = c.req.valid('query');
    const hfToken = c.req.header('X-HF-Token') || undefined;

    // Resolve the node pool / GPU model to estimate for.
    const capacity = await kubernetesService.getDetailedClusterGpuCapacity();
    const selection = selectGpuForEstimate(capacity, gpuModel);
    if (!selection) {
      throw new HTTPException(404, {
        message: 'No GPU node pool with known specs found in the cluster.',
      });
    }
    const { resolvedGpuModel, perGpuMemoryGb, memBandwidthGBs, capacityLabel, maxContiguous } = selection;

    const bytesPerWeight = bytesPerWeightFor(quantization);

    // TP size = GPUs per replica, bounded by what a single node can host. An
    // explicit request (deployment form, curated cards carrying minGpus) is
    // honored as-is. When omitted — notably HuggingFace search cards, whose
    // model objects carry no minGpus hint — derive the smallest TP size whose
    // weight shard still leaves room for a KV cache, so a large model is
    // estimated at a topology that fits (e.g. tp=2 on 80 GB) instead of
    // defaulting to tp=1 and spuriously reporting "does not fit" while the
    // curated/Deploy tabs show full capacity for the same model and cluster.
    const requestedTpSize =
      tpSize ??
      deriveTpSizeToFitWeights({
        paramCount: paramCount ?? 0,
        bytesPerWeight,
        perGpuMemoryGb,
        maxContiguous,
      });
    const effectiveTpSize = Math.max(1, Math.min(requestedTpSize, maxContiguous || requestedTpSize || 1));

    // KV-cache precision is independent of weight quantization. Default to
    // fp16/bf16 (2 bytes); an explicit fp8 KV cache is only realistic on GPUs
    // with a native FP8 datapath (Ada Lovelace and Hopper), so downgrade
    // fp8 → fp16 on older generations for the estimate. (int8 KV is not an
    // FP8-datapath concern, so it is honored on any GPU.)
    let effectiveKvDtype: 'fp8' | 'int8' | 'fp16' | 'bf16' = kvCacheDtype ?? 'fp16';
    if (effectiveKvDtype === 'fp8' && !gpuSupportsFp8(resolvedGpuModel)) {
      effectiveKvDtype = 'fp16';
    }
    const bytesPerKv = bytesPerKvFor(effectiveKvDtype);

    // Whether the resolved GPU has a native FP8 datapath (Ada Lovelace or
    // Hopper). Surfaced so the UI can block FP8 deployments on non-FP8 hardware
    // without re-implementing the GPU→generation mapping client-side.
    const fp8Supported = gpuSupportsFp8(resolvedGpuModel);

    // paramCount is required to compute anything; without it return a shaped
    // low-confidence response rather than guessing.
    if (!paramCount || paramCount <= 0) {
      const empty: GpuThroughputEstimate = {
        perChatTokensPerSec: 0,
        gpuModel: resolvedGpuModel,
        perGpuMemoryGb,
        memBandwidthGBs,
        tpSize: effectiveTpSize,
        contextLen: Math.min(contextLen ?? DEFAULT_CONTEXT_LEN, MAX_CONTEXT_LEN),
        kvCacheDtype: effectiveKvDtype,
        fp8Supported,
        capacityLabel,
        lowConfidence: true,
      };
      return c.json(empty);
    }

    const perChatTokensPerSec = estimatePerChatTokensPerSec({
      paramCount,
      bytesPerWeight,
      memBandwidthGBs,
      tpSize: effectiveTpSize,
    });

    // Architecture details (config.json) are needed for the concurrency number;
    // degrade gracefully to per-chat-only when unavailable. Only a valid HF repo
    // id triggers the token-bearing fetch — a curated/custom id (valid for
    // Airunway but not a HuggingFace repo) skips the lookup and still produces
    // the bandwidth-only estimate from `paramCount` instead of erroring.
    const arch =
      modelId && isValidHfRepoId(modelId)
        ? await huggingFaceService.getModelArchitecture(modelId, hfToken)
        : undefined;

    // Resolve the context length used for KV sizing *after* fetching arch, so
    // HuggingFace models (which carry no explicit contextLen) use their real
    // advertised window rather than the 4K default. An explicit query param wins
    // over the model's max; either way the value is capped at MAX_CONTEXT_LEN so
    // a huge advertised window (which callers forward verbatim) can't collapse
    // the concurrency estimate to ~zero.
    const requestedContextLen = contextLen ?? arch?.maxPositionEmbeddings ?? DEFAULT_CONTEXT_LEN;
    const resolvedContextLen = Math.min(requestedContextLen, MAX_CONTEXT_LEN);

    const capacityResult = arch
      ? estimateConcurrentCapacity({
          paramCount,
          arch,
          perGpuMemoryGb,
          tpSize: effectiveTpSize,
          contextLen: resolvedContextLen,
          bytesPerWeight,
          bytesPerKv,
          perChatTokensPerSec,
        })
      : undefined;

    const estimate: GpuThroughputEstimate = {
      perChatTokensPerSec: Math.round(perChatTokensPerSec),
      concurrentSequences: capacityResult?.concurrentSequences,
      aggregateTokensPerSec: capacityResult?.aggregateTokensPerSec,
      gpuModel: resolvedGpuModel,
      perGpuMemoryGb,
      memBandwidthGBs,
      tpSize: effectiveTpSize,
      contextLen: resolvedContextLen,
      kvCacheDtype: effectiveKvDtype,
      fp8Supported,
      capacityLabel,
      lowConfidence: !capacityResult,
      // High-confidence "model does not fit": arch was available and KV budget
      // left no room for even a single sequence.
      doesNotFit: capacityResult ? capacityResult.concurrentSequences === 0 : undefined,
    };
    return c.json(estimate);
  })
  .post('/gpu-operator/install', async (c) => {
    const helmStatus = await helmService.checkHelmAvailable();
    if (!helmStatus.available) {
      throw new HTTPException(400, {
        message: `Helm CLI not available: ${helmStatus.error}. Please install Helm or use the manual installation commands.`,
      });
    }

    const currentStatus = await kubernetesService.checkGPUOperatorStatus();
    if (currentStatus.installed) {
      return c.json({
        success: true,
        message: 'NVIDIA GPU Operator is already installed',
        alreadyInstalled: true,
        status: currentStatus,
      });
    }

    logger.info('Starting installation of NVIDIA GPU Operator');
    const result = await helmService.installGpuOperator((data, stream) => {
      logger.debug({ stream }, data.trim());
    });

    if (result.success) {
      const verifyStatus = await kubernetesService.checkGPUOperatorStatus();

      return c.json({
        success: true,
        message: 'NVIDIA GPU Operator installed successfully',
        status: verifyStatus,
        results: result.results.map((r) => ({
          step: r.step,
          success: r.result.success,
          output: r.result.stdout,
          error: r.result.stderr,
        })),
      });
    } else {
      const failedStep = result.results.find((r) => !r.result.success);
      const output = failedStep?.result.stderr || failedStep?.result.stdout;
      throw new HTTPException(installationFailureStatus(output), {
        message: installationFailureMessage(`Installation failed at step "${failedStep?.step}"`, output),
      });
    }
  })
  .get('/runtimes/status', async (c) => {
    const runtimesStatus = await kubernetesService.getRuntimesStatus();
    return c.json({ runtimes: runtimesStatus });
  })
  .get('/providers/:providerId/status', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    const provider = extractProviderDetails(config);
    const charts = normalizeInstallCharts(providerId, provider.helmCharts);
    const hasInstallMetadata = charts.length > 0;
    const requiresCRD = provider.requiresCRD !== false;
    const installable = requiresCRD && hasInstallMetadata;
    const status = config.status || {};
    const installationStatus = await kubernetesService.checkProviderInstallationStatus(
      providerId,
      status,
      provider.name,
      provider.health,
      provider.requiresCRD,
    );
    // Layer the shim's heartbeat-aware health view on top of the live
    // installation check. Prefer the shim's message whenever it has an
    // actionable signal — either a stale heartbeat OR a fresh UpstreamReady
    // condition reporting unhealthy (the refuse-fast path). Structural fields
    // (installed/operatorRunning) stay sourced from installationStatus since
    // that reflects what's actually in the cluster regardless of shim state.
    const health = getProviderHealth(providerId, config);
    const baseMessage = hasInstallMetadata || provider.requiresCRD === false
      ? installationStatus.message
      : `No installation metadata found for provider ${providerId}`;
    const useShimMessage = health.stale || (!health.healthy && health.hasShimSignal);
    const message = useShimMessage ? health.message : baseMessage;

    return c.json({
      providerId: provider.id,
      providerName: provider.name,
      installed: installationStatus.installed,
      crdFound: installationStatus.crdFound,
      operatorRunning: installationStatus.operatorRunning ?? false,
      requiresCRD: installationStatus.requiresCRD ?? provider.requiresCRD,
      version: status.version,
      message,
      installable,
      installationSteps: provider.installationSteps,
      helmCommands: installable ? helmService.getInstallCommands(provider.helmRepos, charts) : [],
    });
  })
  .get('/providers/:providerId/commands', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    const provider = extractProviderDetails(config);
    const charts = normalizeInstallCharts(providerId, provider.helmCharts);
    const installable = provider.requiresCRD !== false && charts.length > 0;

    return c.json({
      providerId: provider.id,
      providerName: provider.name,
      commands: installable ? helmService.getInstallCommands(provider.helmRepos, charts) : [],
      steps: provider.installationSteps,
    });
  })
  .post('/providers/:providerId/install', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    const provider = extractProviderDetails(config);
    const charts = normalizeInstallCharts(providerId, provider.helmCharts);

    if (provider.requiresCRD === false) {
      throw new HTTPException(400, {
        message: `${provider.name} is managed by provider registration and cannot be installed from this page.`,
      });
    }

    if (charts.length === 0) {
      throw new HTTPException(400, {
        message: `No installation metadata found for provider ${providerId}. Provider config is missing the airunway.ai/installation annotation or it contains no helmCharts.`,
      });
    }

    const helmStatus = await helmService.checkHelmAvailable();
    if (!helmStatus.available) {
      throw new HTTPException(400, {
        message: `Helm CLI not available: ${helmStatus.error}. Please install Helm or use the manual installation commands.`,
      });
    }

    logger.info({ providerId }, `Starting installation of ${provider.name}`);
    const result = await helmService.installProvider(
      provider.helmRepos,
      charts,
      (data, stream) => { logger.debug({ stream, providerId }, data.trim()); }
    );

    if (result.success) {
      return c.json({
        success: true,
        message: `${provider.name} installed successfully`,
        results: result.results.map((r) => ({
          step: r.step,
          success: r.result.success,
          output: r.result.stdout,
          error: r.result.stderr,
        })),
      });
    } else {
      const failedStep = result.results.find((r) => !r.result.success);
      const output = failedStep?.result.stderr || failedStep?.result.stdout;
      throw new HTTPException(installationFailureStatus(output), {
        message: installationFailureMessage(`Installation failed at step "${failedStep?.step}"`, output),
      });
    }
  })
  .post('/providers/:providerId/uninstall', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    const provider = extractProviderDetails(config);

    if (provider.requiresCRD === false) {
      throw new HTTPException(400, {
        message: `${provider.name} is managed by provider registration and cannot be uninstalled from this page.`,
      });
    }

    const helmStatus = await helmService.checkHelmAvailable();
    if (!helmStatus.available) {
      throw new HTTPException(400, {
        message: `Helm CLI not available: ${helmStatus.error}.`,
      });
    }

    logger.info({ providerId }, `Uninstalling ${provider.name}`);
    const results: Array<{ step: string; success: boolean; output: string; error?: string }> = [];

    for (const chart of [...provider.helmCharts].reverse()) {
      const result = await helmService.uninstall(chart.name, chart.namespace);
      results.push({
        step: `uninstall-${chart.name}`,
        success: result.success,
        output: result.stdout,
        error: result.stderr,
      });
    }

    const allSuccess = results.every(r => r.success);
    const failedResult = results.find(r => !r.success);
    const failedOutput = failedResult?.error || failedResult?.output;
    return c.json({
      success: allSuccess,
      message: allSuccess
        ? `${provider.name} uninstalled successfully`
        : installationFailureMessage(`${provider.name} uninstall failed`, failedOutput),
      results,
    });
  })
  .post('/providers/:providerId/uninstall-crds', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    logger.info({ providerId }, `Removing CRDs for ${providerId}`);

    // The CRD name is typically plural.apiGroup — but since we don't store that in
    // the CRD itself, we delete the InferenceProviderConfig instance for this provider
    try {
      await kubernetesService.deleteInferenceProviderConfig(providerId);
      return c.json({
        success: true,
        message: `${providerId} provider config removed successfully`,
      });
    } catch (error) {
      throw new HTTPException(500, {
        message: `Failed to remove CRDs: ${error instanceof Error ? error.message : 'Unknown error'}`,
      });
    }
  })
  .get('/gateway/status', async (c) => {
    const status = await kubernetesService.checkGatewayCRDStatus();
    return c.json(status);
  })
  .post('/gateway/install-crds', async (c) => {
    const { GATEWAY_API_CRD_URL, GAIE_CRD_URL, PINNED_GAIE_VERSION } = await import('@airunway/shared');

    const results: Array<{ step: string; success: boolean; output: string; error?: string }> = [];

    // Install Gateway API CRDs
    logger.info('Installing Gateway API CRDs');
    const gwResult = await helmService.applyManifestUrl(GATEWAY_API_CRD_URL, (data, stream) => {
      logger.debug({ stream }, data.trim());
    });
    results.push({
      step: 'gateway-api-crds',
      success: gwResult.success,
      output: gwResult.stdout,
      error: gwResult.stderr || undefined,
    });

    if (!gwResult.success) {
      const output = gwResult.stderr || gwResult.stdout;
      throw new HTTPException(installationFailureStatus(output), {
        message: installationFailureMessage('Failed to install Gateway API CRDs', output),
      });
    }

    // Install GAIE CRDs
    logger.info(`Installing Inference Extension CRDs (${PINNED_GAIE_VERSION})`);
    const gaieResult = await helmService.applyManifestUrl(GAIE_CRD_URL, (data, stream) => {
      logger.debug({ stream }, data.trim());
    });
    results.push({
      step: 'inference-extension-crds',
      success: gaieResult.success,
      output: gaieResult.stdout,
      error: gaieResult.stderr || undefined,
    });

    if (!gaieResult.success) {
      const output = gaieResult.stderr || gaieResult.stdout;
      throw new HTTPException(installationFailureStatus(output), {
        message: installationFailureMessage('Failed to install Inference Extension CRDs', output),
      });
    }

    return c.json({
      success: true,
      message: 'Gateway API and Inference Extension CRDs installed successfully',
      results,
    });
  });

export default installation;
