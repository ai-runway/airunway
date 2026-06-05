import { Hono } from 'hono';
import type { Context } from 'hono';
import type { ContentfulStatusCode } from 'hono/utils/http-status';
import { zValidator } from '@hono/zod-validator';
import { z } from 'zod';
import { HTTPException } from 'hono/http-exception';
import { kubernetesService } from '../services/kubernetes';
import { configService } from '../services/config';
import { metricsService } from '../services/metrics';
import { validateGpuFit, formatGpuWarnings } from '../services/gpuValidation';
import { aikitService, GGUF_RUNNER_IMAGE } from '../services/aikit';
import { handleK8sError } from '../lib/k8s-errors';
import models from '../data/models.json';
import logger from '../lib/logger';
import type { AppEnv } from '../types/hono';
import {
  parseFrontendService,
  toModelDeploymentManifest,
  type DeploymentStatus,
  type DeploymentConfig,
} from '@airunway/shared';
import {
  namespaceSchema,
  resourceNameSchema,
} from '../lib/validation';

const listDeploymentsQuerySchema = z.object({
  namespace: namespaceSchema.optional(),
  limit: z
    .string()
    .optional()
    .transform((val) => (val ? parseInt(val, 10) : undefined))
    .pipe(z.number().int().min(1).max(100).optional()),
  offset: z
    .string()
    .optional()
    .transform((val) => (val ? parseInt(val, 10) : undefined))
    .pipe(z.number().int().min(0).optional()),
});

const deploymentQuerySchema = z.object({
  namespace: namespaceSchema.optional(),
});

const deploymentParamsSchema = z.object({
  name: resourceNameSchema,
});

const namespacedDeploymentParamsSchema = z.object({
  namespace: namespaceSchema,
  name: resourceNameSchema,
});

const chatMessageSchema = z.object({
  role: z.string().min(1),
  content: z.unknown(),
}).passthrough();

const chatCompletionSchema = z.object({
  messages: z.array(chatMessageSchema).min(1),
  model: z.string().min(1).optional(),
  temperature: z.number().optional(),
  max_tokens: z.number().int().positive().optional(),
  max_completion_tokens: z.number().int().positive().optional(),
  top_p: z.number().optional(),
  n: z.number().int().positive().optional(),
  stop: z.union([z.string(), z.array(z.string())]).optional(),
  presence_penalty: z.number().optional(),
  frequency_penalty: z.number().optional(),
  user: z.string().optional(),
  tools: z.unknown().optional(),
  tool_choice: z.unknown().optional(),
  response_format: z.unknown().optional(),
  seed: z.number().int().optional(),
}).passthrough();

const DNS_LABEL_REGEX = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

const SYSTEM_PATHS = ['/dev', '/proc', '/sys', '/etc', '/var/run'];
const DEFAULT_FRONTEND_SERVICE_PORT = 8000;
const CHAT_MODEL_DISCOVERY_TIMEOUT_MS = 1000;
const CHAT_MODEL_DISCOVERY_ACCEPT_HEADER = 'application/json';
const UPSTREAM_CHAT_ERROR_DETAILS_MAX_LENGTH = 1000;
const UPSTREAM_CHAT_ERROR_STATUS_CODES = [
  400,
  401,
  403,
  404,
  408,
  409,
  410,
  413,
  415,
  422,
  429,
  500,
  501,
  502,
  503,
  504,
] as const satisfies readonly ContentfulStatusCode[];
const CHAT_STREAM_HEADERS = {
  'Content-Type': 'text/event-stream',
  'Cache-Control': 'no-cache, no-transform',
  'Connection': 'keep-alive',
  'X-Accel-Buffering': 'no',
  'Content-Encoding': 'identity',
};

// Matches Kubernetes resource.Quantity: a valid decimal number with optional
// binary (Ki, Mi, Gi, Ti, Pi, Ei) or decimal (n, u, m, k, M, G, T, P, E) suffix.
// Requires at least one digit; rejects bare dots, multiple dots, etc.
const K8S_QUANTITY_REGEX = /^[+-]?(\d+\.?\d*|\d*\.?\d+)([eE][+-]?\d+|[KMGTPE]i?|[numkMGTPE])?$/;

const storageVolumeSchema = z.object({
  name: z.string()
    .min(1, 'Volume name is required')
    .max(63, 'Volume name must be 63 characters or less')
    .regex(DNS_LABEL_REGEX, 'Volume name must be a valid DNS label (lowercase alphanumeric with hyphens)'),
  purpose: z.enum(['modelCache', 'compilationCache', 'custom']).optional().default('custom'),
  mountPath: z.string().optional(),
  readOnly: z.boolean().optional().default(false),
  size: z.string()
    .regex(K8S_QUANTITY_REGEX, 'Size must be a valid Kubernetes quantity (e.g. 100Gi, 500Mi, 1Ti)')
    .optional(),
  claimName: z.string().optional(),
  storageClassName: z.string().optional(),
  accessMode: z.enum(['ReadWriteOnce', 'ReadWriteMany', 'ReadOnlyMany', 'ReadWriteOncePod']).optional(),
});

const storageSchema = z.object({
  volumes: z.array(storageVolumeSchema).max(8, 'Maximum 8 storage volumes allowed').optional(),
}).optional();

const createDeploymentSchema = z.object({
  name: resourceNameSchema,
  modelId: z.string().min(1, 'Model ID is required'),
  engine: z.enum(['vllm', 'sglang', 'trtllm', 'llamacpp']),
  namespace: namespaceSchema.optional(),
  mode: z.enum(['aggregated', 'disaggregated']).optional().default('aggregated'),
  provider: resourceNameSchema.optional(),
  servedModelName: z.string().optional(),
  routerMode: z.enum(['default', 'kv', 'round-robin']).optional().default('default'),
  replicas: z.number().int().min(0).optional().default(1),
  hfTokenSecret: z.string().optional().default(''),
  contextLength: z.number().int().positive().optional(),
  enforceEager: z.boolean().optional().default(false),
  enablePrefixCaching: z.boolean().optional().default(true),
  trustRemoteCode: z.boolean().optional().default(false),
  resources: z.object({
    gpu: z.number().int().min(0),
    memory: z.string().optional(),
  }).optional(),
  engineArgs: z.record(z.string(), z.unknown()).optional(),
  providerOverrides: z.record(z.string(), z.unknown()).optional(),
  prefillReplicas: z.number().int().min(0).optional(),
  decodeReplicas: z.number().int().min(0).optional(),
  prefillGpus: z.number().int().min(0).optional(),
  decodeGpus: z.number().int().min(0).optional(),
  modelSource: z.enum(['premade', 'huggingface', 'vllm']).optional(),
  premadeModel: z.string().optional(),
  ggufFile: z.string().optional(),
  ggufRunMode: z.enum(['build', 'direct']).optional(),
  imageRef: z.string().optional(),
  computeType: z.enum(['cpu', 'gpu']).optional(),
  maxModelLen: z.number().int().positive().optional(),
  gatewayEnabled: z.boolean().optional(),
  storage: storageSchema,
}).superRefine((data, ctx) => {
  const volumes = data.storage?.volumes;
  if (!volumes || volumes.length === 0) return;

  // Default mount path map (mirrors webhook defaults)
  const DEFAULT_MOUNT_PATHS: Record<string, string> = {
    modelCache: '/model-cache',
    compilationCache: '/compilation-cache',
  };

  // Resolve effective values that the webhook would default,
  // so uniqueness checks match what the cluster will actually see.
  const resolvedMountPaths = volumes.map(
    (vol) => vol.mountPath || DEFAULT_MOUNT_PATHS[vol.purpose || ''] || ''
  );
  const resolvedClaimNames = volumes.map(
    (vol) => vol.claimName || (vol.size ? `${data.name}-${vol.name}` : '')
  );

  // Rule 1: Unique volume names
  const names = new Set<string>();
  for (let i = 0; i < volumes.length; i++) {
    if (names.has(volumes[i].name)) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: `Duplicate volume name: "${volumes[i].name}"`,
        path: ['storage', 'volumes', i, 'name'],
      });
    }
    names.add(volumes[i].name);
  }

  // Rule 2: Unique mount paths (using resolved defaults)
  const mountPaths = new Set<string>();
  for (let i = 0; i < volumes.length; i++) {
    const mp = resolvedMountPaths[i];
    if (mp) {
      if (mountPaths.has(mp)) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: `Duplicate mount path: "${mp}"`,
          path: ['storage', 'volumes', i, 'mountPath'],
        });
      }
      mountPaths.add(mp);
    }
  }

  // Rule 3: Unique claim names (using resolved defaults)
  const claimNames = new Set<string>();
  for (let i = 0; i < volumes.length; i++) {
    const cn = resolvedClaimNames[i];
    if (cn) {
      if (claimNames.has(cn)) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: `Duplicate claim name: "${cn}"`,
          path: ['storage', 'volumes', i, 'claimName'],
        });
      }
      claimNames.add(cn);
    }
  }

  // Count purpose occurrences for Rule 7
  let modelCacheCount = 0;
  let compilationCacheCount = 0;

  for (let i = 0; i < volumes.length; i++) {
    const vol = volumes[i];

    // Rule 4: mountPath must be absolute when set
    if (vol.mountPath && !vol.mountPath.startsWith('/')) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'Mount path must be an absolute path (start with /)',
        path: ['storage', 'volumes', i, 'mountPath'],
      });
    }

    // Rule 5: mountPath required when purpose is custom
    if (vol.purpose === 'custom' && !vol.mountPath) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'Mount path is required for custom purpose volumes',
        path: ['storage', 'volumes', i, 'mountPath'],
      });
    }

    // Rule 6: Reject system paths
    if (vol.mountPath) {
      for (const sysPath of SYSTEM_PATHS) {
        if (vol.mountPath === sysPath || vol.mountPath.startsWith(sysPath + '/')) {
          ctx.addIssue({
            code: z.ZodIssueCode.custom,
            message: `Mount path "${vol.mountPath}" conflicts with system path "${sysPath}"`,
            path: ['storage', 'volumes', i, 'mountPath'],
          });
          break;
        }
      }
    }

    // Rule 7: Count purposes
    if (vol.purpose === 'modelCache') modelCacheCount++;
    if (vol.purpose === 'compilationCache') compilationCacheCount++;

    // Rule 8: When size is NOT set, claimName is required
    if (!vol.size && !vol.claimName) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'Claim name is required when size is not specified (existing PVC)',
        path: ['storage', 'volumes', i, 'claimName'],
      });
    }

    // Rule 9: When size IS set, readOnly must be false
    if (vol.size && vol.readOnly) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'Read-only is not allowed for controller-created PVCs (size is set)',
        path: ['storage', 'volumes', i, 'readOnly'],
      });
    }

    // Rule 10: When size IS set, claimName must be empty or match <deploymentName>-<volumeName>
    if (vol.size && vol.claimName) {
      const expectedClaimName = `${data.name}-${vol.name}`;
      if (vol.claimName !== expectedClaimName) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: `When size is set, claim name must be empty or match "${expectedClaimName}"`,
          path: ['storage', 'volumes', i, 'claimName'],
        });
      }
    }

    // Rule 11: accessMode only valid when size is set
    if (vol.accessMode && !vol.size) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'Access mode is only valid when size is specified (new PVC)',
        path: ['storage', 'volumes', i, 'accessMode'],
      });
    }

    // Rule 12: storageClassName only valid when size is set
    if (vol.storageClassName !== undefined && !vol.size) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'Storage class name is only valid when size is specified (new PVC)',
        path: ['storage', 'volumes', i, 'storageClassName'],
      });
    }

    // Rule 13: Auto-generated claim name must be <=253 chars
    if (vol.size && !vol.claimName) {
      const autoClaimName = `${data.name}-${vol.name}`;
      if (autoClaimName.length > 253) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: `Auto-generated claim name "${autoClaimName}" exceeds 253 character limit`,
          path: ['storage', 'volumes', i, 'name'],
        });
      }
    }
  }

  // Rule 7: Max 1 of each singleton purpose
  if (modelCacheCount > 1) {
    ctx.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'Only one volume with purpose "modelCache" is allowed',
      path: ['storage', 'volumes'],
    });
  }
  if (compilationCacheCount > 1) {
    ctx.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'Only one volume with purpose "compilationCache" is allowed',
      path: ['storage', 'volumes'],
    });
  }
});


function parseJsonObject(text: string): Record<string, unknown> | undefined {
  try {
    const parsed = JSON.parse(text);
    return parsed && typeof parsed === 'object' ? parsed as Record<string, unknown> : undefined;
  } catch {
    return undefined;
  }
}

function getNestedStringValue(source: unknown, path: string[]): string | undefined {
  let current = source;
  for (const segment of path) {
    if (!current || typeof current !== 'object') {
      return undefined;
    }
    current = (current as Record<string, unknown>)[segment];
  }

  return typeof current === 'string' && current.trim() ? current : undefined;
}

function truncateErrorMessage(message: string, maxLength = 500): string {
  return message.length > maxLength ? `${message.slice(0, maxLength)}…` : message;
}

function toUpstreamChatErrorStatusCode(statusCode: number): ContentfulStatusCode {
  return UPSTREAM_CHAT_ERROR_STATUS_CODES.includes(
    statusCode as (typeof UPSTREAM_CHAT_ERROR_STATUS_CODES)[number]
  )
    ? statusCode as ContentfulStatusCode
    : 502;
}

function sanitizeUpstreamErrorDetails(details: string): string | undefined {
  const normalized = details
    .replace(/[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]/g, '')
    .trim();

  if (!normalized || normalized.startsWith('<')) {
    return undefined;
  }

  return normalized.length > UPSTREAM_CHAT_ERROR_DETAILS_MAX_LENGTH
    ? `${normalized.slice(0, UPSTREAM_CHAT_ERROR_DETAILS_MAX_LENGTH - 1)}…`
    : normalized;
}

async function readUpstreamErrorDetails(
  response: Response,
  maxBytes = UPSTREAM_CHAT_ERROR_DETAILS_MAX_LENGTH + 1
): Promise<string> {
  if (!response.body) {
    return '';
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  const chunks: string[] = [];
  let bytesRead = 0;
  let reachedLimit = false;

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      if (!value) {
        continue;
      }

      const remaining = maxBytes - bytesRead;
      if (remaining <= 0) {
        reachedLimit = true;
        break;
      }

      const chunk = value.byteLength > remaining
        ? value.slice(0, remaining)
        : value;
      bytesRead += chunk.byteLength;
      chunks.push(decoder.decode(chunk, { stream: true }));

      if (value.byteLength >= remaining) {
        reachedLimit = true;
        break;
      }
    }

    chunks.push(decoder.decode());
    return chunks.join('');
  } finally {
    if (reachedLimit) {
      await reader.cancel().catch(() => undefined);
    }
    reader.releaseLock();
  }
}

function getUpstreamChatErrorMessage(
  statusCode: number,
  details: string,
  deploymentName: string
): string {
  const status = parseJsonObject(details);
  const statusMessage = getNestedStringValue(status, ['message']);
  const openAiErrorMessage = getNestedStringValue(status, ['error', 'message']);
  const detailMessage = getNestedStringValue(status, ['detail']);
  const reason = getNestedStringValue(status, ['reason']);
  const detailObject = status?.details && typeof status.details === 'object'
    ? status.details as Record<string, unknown>
    : undefined;
  const kind = getNestedStringValue(detailObject, ['kind']);

  if (statusCode === 404 && reason === 'NotFound' && kind === 'services') {
    return `The model endpoint for '${deploymentName}' is not available yet. The deployment may still be starting, or its endpoint may have changed. Try again in a moment or check the logs.`;
  }

  const parsedMessage = openAiErrorMessage || statusMessage || detailMessage;
  if (parsedMessage) {
    return truncateErrorMessage(parsedMessage);
  }

  const plainDetails = details.trim();
  if (plainDetails && !plainDetails.startsWith('<')) {
    return truncateErrorMessage(plainDetails);
  }

  return `The model did not accept the chat request (HTTP ${statusCode}). Try again in a moment.`;
}

function isMissingServiceProxyResponse(statusCode: number, details: string): boolean {
  const status = parseJsonObject(details);
  const reason = typeof status?.reason === 'string' ? status.reason : undefined;
  const detailObject = status?.details && typeof status.details === 'object'
    ? status.details as Record<string, unknown>
    : undefined;
  const kind = typeof detailObject?.kind === 'string' ? detailObject.kind : undefined;

  return statusCode === 404 && reason === 'NotFound' && kind === 'services';
}

function buildGatewayChatUrl(endpoint: string): string {
  const withScheme = endpoint.includes('://') ? endpoint : `http://${endpoint}`;
  const baseUrl = new URL(withScheme);
  const normalizedPath = baseUrl.pathname.replace(/\/+$/, '');
  baseUrl.pathname = normalizedPath.endsWith('/v1')
    ? `${normalizedPath}/chat/completions`
    : `${normalizedPath}/v1/chat/completions`;
  baseUrl.search = '';
  baseUrl.hash = '';
  return baseUrl.toString();
}

async function proxyGatewayChatPostStream(
  endpoint: string,
  body: unknown,
  modelName: string,
  signal?: AbortSignal
): Promise<Response> {
  return fetch(buildGatewayChatUrl(endpoint), {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Accept': 'text/event-stream',
      'X-Gateway-Model-Name': modelName,
    },
    body: JSON.stringify(body),
    signal,
  });
}

function extractFirstModelId(modelsResponse: unknown): string | undefined {
  if (!modelsResponse || typeof modelsResponse !== 'object') {
    return undefined;
  }

  const data = (modelsResponse as { data?: unknown }).data;
  if (!Array.isArray(data) || data.length === 0) {
    return undefined;
  }

  const firstModel = data[0];
  if (!firstModel || typeof firstModel !== 'object') {
    return undefined;
  }

  const id = (firstModel as { id?: unknown }).id;
  return typeof id === 'string' && id.length > 0 ? id : undefined;
}

function isKaitoLlamaCppDeployment(deployment: DeploymentStatus): boolean {
  return deployment.provider === 'kaito' && deployment.engine === 'llamacpp';
}

// Returns the name the underlying model server is serving on its /v1/* API,
// or undefined when we should ask the server (or fall back to modelId).
// Excludes deployment.gateway.modelName on purpose — that's the HTTPRoute alias
// used by the gateway and is unrelated to what the frontend service responds to.
function getServedChatModelName(deployment: DeploymentStatus): string | undefined {
  if (deployment.servedModelName && !isKaitoLlamaCppDeployment(deployment)) {
    return deployment.servedModelName;
  }
  return undefined;
}

function createRequestScopedTimeoutSignal(
  requestSignal: AbortSignal,
  timeoutMs: number
): { signal: AbortSignal; cleanup: () => void } {
  const controller = new AbortController();
  let timeout: ReturnType<typeof setTimeout> | undefined;

  const abort = () => {
    if (!controller.signal.aborted) {
      controller.abort();
    }
  };

  if (requestSignal.aborted) {
    abort();
  } else {
    requestSignal.addEventListener('abort', abort, { once: true });
    timeout = setTimeout(abort, timeoutMs);

    if (requestSignal.aborted) {
      abort();
    }
  }

  return {
    signal: controller.signal,
    cleanup: () => {
      if (timeout) {
        clearTimeout(timeout);
      }
      requestSignal.removeEventListener('abort', abort);
    },
  };
}

async function discoverUpstreamChatModel(
  deployment: DeploymentStatus,
  serviceName: string,
  namespace: string,
  servicePort: number,
  requestSignal: AbortSignal,
  userToken?: string
): Promise<string | undefined> {
  const scopedSignal = createRequestScopedTimeoutSignal(
    requestSignal,
    CHAT_MODEL_DISCOVERY_TIMEOUT_MS
  );

  try {
    const modelsText = await kubernetesService.proxyServiceGet(
      serviceName,
      namespace,
      servicePort,
      'v1/models',
      { accept: CHAT_MODEL_DISCOVERY_ACCEPT_HEADER, signal: scopedSignal.signal, userToken }
    );
    return extractFirstModelId(JSON.parse(modelsText));
  } catch (error) {
    logger.debug(
      { error, deploymentName: deployment.name, namespace, serviceName, servicePort },
      'Could not resolve model from upstream /v1/models; falling back to deployment model ID'
    );
    return undefined;
  } finally {
    scopedSignal.cleanup();
  }
}

async function resolveServedChatModel(
  deployment: DeploymentStatus,
  serviceName: string,
  namespace: string,
  servicePort: number,
  requestSignal: AbortSignal,
  userToken?: string
): Promise<string> {
  const served = getServedChatModelName(deployment);
  if (served) {
    return served;
  }

  return (await discoverUpstreamChatModel(
    deployment,
    serviceName,
    namespace,
    servicePort,
    requestSignal,
    userToken
  )) || deployment.modelId;
}

async function resolveDirectChatModel(
  deployment: DeploymentStatus,
  serviceName: string,
  namespace: string,
  servicePort: number,
  requestSignal: AbortSignal,
  userToken?: string,
  requestedModel?: string
): Promise<string> {
  if (requestedModel) {
    return requestedModel;
  }

  // Direct service-proxy path: ignore deployment.gateway.modelName (that's the
  // HTTPRoute alias the gateway routes by; the frontend service doesn't know it
  // and would return a model-not-found error).
  return resolveServedChatModel(
    deployment,
    serviceName,
    namespace,
    servicePort,
    requestSignal,
    userToken
  );
}

async function resolveGatewayChatModel(
  deployment: DeploymentStatus,
  serviceName: string,
  namespace: string,
  servicePort: number,
  requestSignal: AbortSignal,
  userToken?: string
): Promise<string> {
  // Gateway path: the HTTPRoute alias is exactly what the gateway routes by.
  if (deployment.gateway?.modelName) {
    return deployment.gateway.modelName;
  }

  return resolveServedChatModel(
    deployment,
    serviceName,
    namespace,
    servicePort,
    requestSignal,
    userToken
  );
}

async function handleDeploymentChat(
  c: Context<AppEnv>,
  name: string,
  body: z.infer<typeof chatCompletionSchema>,
  namespace?: string
) {
  const resolvedNamespace = namespace || (await configService.getDefaultNamespace());
  const userToken = c.get('token') as string | undefined;
  const signal = c.req.raw.signal;

  const deployment = await kubernetesService.getDeployment(name, resolvedNamespace, userToken);
  if (!deployment) {
    throw new HTTPException(404, { message: 'Deployment not found' });
  }

  if (deployment.phase !== 'Running') {
    throw new HTTPException(409, {
      message: `Deployment '${name}' is not running (current phase: ${deployment.phase})`,
    });
  }

  const frontendService = parseFrontendService(deployment.frontendService);
  if (!frontendService?.serviceName) {
    throw new HTTPException(409, {
      message: `Deployment '${name}' does not expose a frontend service for chat`,
    });
  }

  const frontendServicePort = frontendService.servicePort || DEFAULT_FRONTEND_SERVICE_PORT;

  const directModel = await resolveDirectChatModel(
    deployment,
    frontendService.serviceName,
    resolvedNamespace,
    frontendServicePort,
    signal,
    userToken,
    body.model
  );

  const upstreamResponse = await kubernetesService.proxyServicePostStream(
    frontendService.serviceName,
    resolvedNamespace,
    frontendServicePort,
    'v1/chat/completions',
    {
      ...body,
      model: directModel,
      stream: true,
    },
    {},
    { signal, userToken }
  );

  if (!upstreamResponse.ok) {
    const details = await readUpstreamErrorDetails(upstreamResponse);

    if (deployment.gateway?.endpoint && isMissingServiceProxyResponse(upstreamResponse.status, details)) {
      const gatewayModel = await resolveGatewayChatModel(
        deployment,
        frontendService.serviceName,
        resolvedNamespace,
        frontendServicePort,
        signal,
        userToken
      );
      const gatewayResponse = await proxyGatewayChatPostStream(
        deployment.gateway.endpoint,
        {
          ...body,
          model: gatewayModel,
          stream: true,
        },
        gatewayModel,
        signal
      );

      if (gatewayResponse.ok) {
        if (!gatewayResponse.body) {
          return c.json(
            {
              error: {
                message: 'Gateway chat response did not include a stream body',
                statusCode: 502,
              },
            },
            502
          );
        }

        return new Response(gatewayResponse.body, {
          status: 200,
          headers: CHAT_STREAM_HEADERS,
        });
      }

      const gatewayDetails = await readUpstreamErrorDetails(gatewayResponse);
      const statusCode = toUpstreamChatErrorStatusCode(gatewayResponse.status);
      const sanitizedDetails = sanitizeUpstreamErrorDetails(gatewayDetails);

      return c.json(
        {
          error: {
            message: getUpstreamChatErrorMessage(gatewayResponse.status, gatewayDetails, name),
            statusCode,
            ...(sanitizedDetails ? { details: sanitizedDetails } : {}),
          },
        },
        statusCode
      );
    }

    const statusCode = toUpstreamChatErrorStatusCode(upstreamResponse.status);
    const sanitizedDetails = sanitizeUpstreamErrorDetails(details);

    return c.json(
      {
        error: {
          message: getUpstreamChatErrorMessage(upstreamResponse.status, details, name),
          statusCode,
          ...(sanitizedDetails ? { details: sanitizedDetails } : {}),
        },
      },
      statusCode
    );
  }

  if (!upstreamResponse.body) {
    return c.json(
      {
        error: {
          message: 'Upstream chat completion response did not include a stream body',
          statusCode: 502,
        },
      },
      502
    );
  }

  return new Response(upstreamResponse.body, {
    status: 200,
    headers: CHAT_STREAM_HEADERS,
  });
}

function resolveDeploymentImages(config: DeploymentConfig): DeploymentConfig {
  if (config.provider !== 'kaito') {
    return config;
  }

  if (config.modelSource === 'premade' && config.premadeModel) {
    if (config.imageRef) {
      return config;
    }

    const imageRef = aikitService.getImageRef({
      modelSource: 'premade',
      premadeModel: config.premadeModel,
    });
    return imageRef ? { ...config, imageRef } : config;
  }

  if (config.modelSource === 'huggingface' && config.ggufRunMode === 'direct') {
    const resolvedConfig: DeploymentConfig = {
      ...config,
      imageRef: config.imageRef || GGUF_RUNNER_IMAGE,
    };

    if (config.ggufFile) {
      resolvedConfig.engineArgs = {
        ...(config.engineArgs || {}),
        ggufUrl: aikitService.buildHuggingFaceUrl(config.modelId, config.ggufFile),
      };
    }

    return resolvedConfig;
  }

  return config;
}

const deployments = new Hono<AppEnv>()
  .get('/', zValidator('query', listDeploymentsQuerySchema), async (c) => {
    try {
      const { namespace, limit, offset } = c.req.valid('query');
      const userToken = c.get('token') as string | undefined;

      let deploymentsList: DeploymentStatus[] = await kubernetesService.listDeployments(namespace, userToken);

      const total = deploymentsList.length;

      // Apply pagination
      if (offset !== undefined || limit !== undefined) {
        const start = offset || 0;
        const end = limit ? start + limit : undefined;
        deploymentsList = deploymentsList.slice(start, end);
      }

      return c.json({
        deployments: deploymentsList || [],
        pagination: {
          total,
          limit: limit || total,
          offset: offset || 0,
          hasMore: (offset || 0) + deploymentsList.length < total,
        },
      });
    } catch (error) {
      logger.error({ error }, 'Error in GET /deployments');
      return c.json({
        deployments: [],
        pagination: { total: 0, limit: 0, offset: 0, hasMore: false },
      });
    }
  })
  .post('/', zValidator('json', createDeploymentSchema), async (c) => {
    const body = c.req.valid('json');

    const config = resolveDeploymentImages({
      ...body,
      namespace: body.namespace || (await configService.getDefaultNamespace()),
    });

    // GPU fit validation
    let gpuWarnings: string[] = [];
    try {
      const capacity = await kubernetesService.getClusterGpuCapacity();

      const model = models.models.find((m) => m.id === config.modelId);
      const modelMinGpus = (model as { minGpus?: number })?.minGpus ?? 1;

      const gpuFitResult = validateGpuFit(config, capacity, modelMinGpus);
      if (!gpuFitResult.fits) {
        gpuWarnings = formatGpuWarnings(gpuFitResult);
        logger.warn(
          {
            modelId: config.modelId,
            warnings: gpuWarnings,
            capacity: {
              available: capacity.availableGpus,
              maxContiguous: capacity.maxContiguousAvailable,
            },
          },
          'GPU fit warnings for deployment'
        );
      }
    } catch (gpuError) {
      logger.warn({ error: gpuError }, 'Could not perform GPU fit validation');
    }

    // Create deployment with detailed error handling
    const userToken = c.get('token') as string | undefined;
    try {
      await kubernetesService.createDeployment(config, userToken);
    } catch (error) {
      const { message, statusCode } = handleK8sError(error, {
        operation: 'createDeployment',
        deploymentName: config.name,
        namespace: config.namespace,
        modelId: config.modelId,
      });

      throw new HTTPException(statusCode as 400 | 403 | 404 | 409 | 422 | 500, {
        message: `Failed to create deployment: ${message}`,
      });
    }

    return c.json(
      {
        message: 'Deployment created successfully',
        name: config.name,
        namespace: config.namespace,
        ...(gpuWarnings.length > 0 && { warnings: gpuWarnings }),
      },
      201
    );
  })
  .post('/preview', zValidator('json', createDeploymentSchema), async (c) => {
    const body = c.req.valid('json');
    const config = resolveDeploymentImages({
      ...body,
      namespace: body.namespace || (await configService.getDefaultNamespace()),
    });

    // Apply storage defaults that the mutating webhook would add,
    // so the preview manifest matches what Kubernetes will persist.
    if (config.storage?.volumes) {
      config.storage = {
        volumes: config.storage.volumes.map((vol) => {
          const defaulted = { ...vol };
          // Default purpose
          if (!defaulted.purpose) {
            defaulted.purpose = 'custom';
          }
          // Default mountPath based on purpose
          if (!defaulted.mountPath) {
            if (defaulted.purpose === 'modelCache') defaulted.mountPath = '/model-cache';
            if (defaulted.purpose === 'compilationCache') defaulted.mountPath = '/compilation-cache';
          }
          // When size is set (controller-created PVC mode):
          if (defaulted.size) {
            // Default claimName to <deploymentName>-<volumeName>
            if (!defaulted.claimName) {
              defaulted.claimName = `${config.name}-${defaulted.name}`;
            }
            // Default accessMode to ReadWriteMany
            if (!defaulted.accessMode) {
              defaulted.accessMode = 'ReadWriteMany';
            }
          }
          return defaulted;
        }),
      };
    }

    const manifest = toModelDeploymentManifest(config);
    return c.json({
      resources: [{
        kind: 'ModelDeployment',
        apiVersion: 'airunway.ai/v1alpha1',
        name: config.name,
        manifest: manifest as unknown as Record<string, unknown>,
      }],
      primaryResource: { kind: 'ModelDeployment', apiVersion: 'airunway.ai/v1alpha1' },
    });
  })
  .post(
    '/:namespace/:name/chat',
    zValidator('param', namespacedDeploymentParamsSchema),
    zValidator('json', chatCompletionSchema),
    async (c) => {
      const { namespace, name } = c.req.valid('param');
      return handleDeploymentChat(c, name, c.req.valid('json'), namespace);
    }
  )
  .post(
    '/:name/chat',
    zValidator('param', deploymentParamsSchema),
    zValidator('query', deploymentQuerySchema),
    zValidator('json', chatCompletionSchema),
    async (c) => {
      const { name } = c.req.valid('param');
      const { namespace } = c.req.valid('query');
      return handleDeploymentChat(c, name, c.req.valid('json'), namespace);
    }
  )
  // List PVCs in a namespace (for storage volume selection)
  // Use a reserved segment to avoid conflicting with deployment names like "pvcs".
  .get('/-/pvcs', zValidator('query', z.object({ namespace: namespaceSchema })), async (c) => {
    const { namespace } = c.req.valid('query');
    const userToken = c.get('token') as string | undefined;
    try {
      const pvcs = await kubernetesService.listPVCs(namespace, userToken);
      return c.json({ pvcs });
    } catch (error) {
      const { message, statusCode } = handleK8sError(error, {
        operation: 'listPVCs',
        namespace,
      });

      throw new HTTPException(statusCode as 400 | 401 | 403 | 404 | 409 | 422 | 500 | 502 | 503 | 504, {
        message: `Failed to list storage disks: ${message}`,
      });
    }
  })
  .get(
    '/:name',
    zValidator('param', deploymentParamsSchema),
    zValidator('query', deploymentQuerySchema),
    async (c) => {
      const { name } = c.req.valid('param');
      const { namespace } = c.req.valid('query');
      const resolvedNamespace = namespace || (await configService.getDefaultNamespace());
      const userToken = c.get('token') as string | undefined;

      const deployment = await kubernetesService.getDeployment(name, resolvedNamespace, userToken);

      if (!deployment) {
        throw new HTTPException(404, { message: 'Deployment not found' });
      }

      return c.json(deployment);
    }
  )
  .get(
    '/:name/manifest',
    zValidator('param', deploymentParamsSchema),
    zValidator('query', deploymentQuerySchema),
    async (c) => {
      const { name } = c.req.valid('param');
      const { namespace } = c.req.valid('query');
      const resolvedNamespace = namespace || (await configService.getDefaultNamespace());
      const userToken = c.get('token') as string | undefined;

      // Get the main CR manifest
      const manifest = await kubernetesService.getDeploymentManifest(name, resolvedNamespace, userToken);

      if (!manifest) {
        throw new HTTPException(404, { message: 'Deployment manifest not found' });
      }

      const kind = (manifest.kind as string) || 'ModelDeployment';
      const apiVersion = (manifest.apiVersion as string) || 'airunway.ai/v1alpha1';

      // Build array of resources
      const resources: Array<{
        kind: string;
        apiVersion: string;
        name: string;
        manifest: Record<string, unknown>;
      }> = [];

      // Add main CR
      resources.push({
        kind,
        apiVersion,
        name,
        manifest,
      });

      return c.json({
        resources,
        primaryResource: {
          kind,
          apiVersion,
        },
      });
    }
  )
  .delete(
    '/:name',
    zValidator('param', deploymentParamsSchema),
    zValidator('query', deploymentQuerySchema),
    async (c) => {
      const { name } = c.req.valid('param');
      const { namespace } = c.req.valid('query');
      const resolvedNamespace = namespace || (await configService.getDefaultNamespace());
      const userToken = c.get('token') as string | undefined;

      try {
        await kubernetesService.deleteDeployment(name, resolvedNamespace, userToken);
      } catch (error) {
        // Check if it's a "not found" error from our own code
        if (error instanceof Error && error.message.includes('not found')) {
          throw new HTTPException(404, { message: error.message });
        }

        const { message, statusCode } = handleK8sError(error, {
          operation: 'deleteDeployment',
          deploymentName: name,
          namespace: resolvedNamespace,
        });

        throw new HTTPException(statusCode as 400 | 403 | 404 | 500, {
          message: `Failed to delete deployment: ${message}`,
        });
      }

      return c.json({ message: 'Deployment deleted successfully' });
    }
  )
  .get(
    '/:name/pods',
    zValidator('param', deploymentParamsSchema),
    zValidator('query', deploymentQuerySchema),
    async (c) => {
      const { name } = c.req.valid('param');
      const { namespace } = c.req.valid('query');
      const resolvedNamespace = namespace || (await configService.getDefaultNamespace());
      const userToken = c.get('token') as string | undefined;

      // Verify user has access to the parent ModelDeployment
      const deployment = await kubernetesService.getDeployment(name, resolvedNamespace, userToken);
      if (!deployment) {
        throw new HTTPException(404, { message: 'Deployment not found' });
      }

      const pods = await kubernetesService.getDeploymentPods(name, resolvedNamespace);
      return c.json({ pods });
    }
  )
  .get(
    '/:name/metrics',
    zValidator('param', deploymentParamsSchema),
    zValidator('query', deploymentQuerySchema),
    async (c) => {
      const { name } = c.req.valid('param');
      const { namespace } = c.req.valid('query');
      const resolvedNamespace = namespace || (await configService.getDefaultNamespace());
      const userToken = c.get('token') as string | undefined;

      // Verify user has access to the parent ModelDeployment
      const deployment = await kubernetesService.getDeployment(name, resolvedNamespace, userToken);
      if (!deployment) {
        throw new HTTPException(404, { message: 'Deployment not found' });
      }

      const frontendService = parseFrontendService(deployment.frontendService);
      const metricsResponse = await metricsService.getDeploymentMetrics(name, resolvedNamespace, {
        providerId: deployment.provider,
        serviceName: frontendService?.serviceName,
        port: frontendService?.servicePort,
      });
      return c.json(metricsResponse);
    }
)
  .get(
    '/:name/pending-reasons',
    zValidator('param', deploymentParamsSchema),
    zValidator('query', deploymentQuerySchema),
    async (c) => {
      const { name } = c.req.valid('param');
      const { namespace } = c.req.valid('query');
      const resolvedNamespace = namespace || (await configService.getDefaultNamespace());
      const userToken = c.get('token') as string | undefined;

      try {
        // Get deployment to find pending pods
        const deployment = await kubernetesService.getDeployment(name, resolvedNamespace, userToken);

        if (!deployment) {
          throw new HTTPException(404, { message: 'Deployment not found' });
        }

        // Get all pending pods
        const pendingPods = deployment.pods.filter(pod => pod.phase === 'Pending');

        if (pendingPods.length === 0) {
          return c.json({ reasons: [] });
        }

        // Get failure reasons for the first pending pod (they're typically the same)
        const podName = pendingPods[0].name;
        const reasons = await kubernetesService.getPodFailureReasons(podName, resolvedNamespace);

        return c.json({ reasons });
      } catch (error) {
        if (error instanceof HTTPException) {
          throw error;
        }
        logger.error({ error, name, namespace: resolvedNamespace }, 'Error getting pending reasons');
        return c.json(
          {
            error: {
              message: error instanceof Error ? error.message : 'Failed to get pending reasons',
              statusCode: 500,
            },
          },
          500
        );
      }
    }
  )
  .get(
    '/:name/logs',
    zValidator('param', deploymentParamsSchema),
    zValidator('query', z.object({
      namespace: namespaceSchema.optional(),
      podName: z.string().optional(),
      container: z.string().optional(),
      tailLines: z.string().optional()
        .transform((val) => (val ? parseInt(val, 10) : undefined))
        .pipe(z.number().int().min(1).max(10000).optional()),
      timestamps: z.string().optional()
        .transform((val) => val === 'true'),
    })),
    async (c) => {
      const { name } = c.req.valid('param');
      const { namespace, podName, container, tailLines, timestamps } = c.req.valid('query');
      const resolvedNamespace = namespace || (await configService.getDefaultNamespace());
      const userToken = c.get('token') as string | undefined;

      try {
        // Verify user has access to the parent ModelDeployment
        const deployment = await kubernetesService.getDeployment(name, resolvedNamespace, userToken);
        if (!deployment) {
          throw new HTTPException(404, { message: 'Deployment not found' });
        }

        // Use service account for pod listing and log fetching
        const pods = await kubernetesService.getDeploymentPods(name, resolvedNamespace);

        if (pods.length === 0) {
          logger.debug({ name, namespace: resolvedNamespace }, 'No pods found for deployment');
          return c.json({ logs: '', podName: '', message: 'No pods found for this deployment' });
        }

        // Use specified pod or default to first pod
        const targetPodName = podName || pods[0].name;

        // Verify the pod belongs to this deployment
        const podExists = pods.some(pod => pod.name === targetPodName);
        if (!podExists) {
          throw new HTTPException(400, {
            message: `Pod '${targetPodName}' is not part of deployment '${name}'`
          });
        }

        logger.debug({ name, namespace: resolvedNamespace, targetPodName }, 'Fetching logs for pod');

        const logs = await kubernetesService.getPodLogs(targetPodName, resolvedNamespace, {
          container,
          tailLines: tailLines || 100,
          timestamps: timestamps || false,
        });

        return c.json({
          logs,
          podName: targetPodName,
          container: container || undefined,
        });
      } catch (error) {
        if (error instanceof HTTPException) {
          throw error;
        }
        logger.error({ error, name, namespace: resolvedNamespace }, 'Error getting deployment logs');
        return c.json(
          {
            error: {
              message: error instanceof Error ? error.message : 'Failed to get logs',
              statusCode: 500,
            },
          },
          500
        );
      }
    }
  );

export default deployments;
