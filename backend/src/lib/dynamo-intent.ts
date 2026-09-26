import { z } from 'zod';
import { DYNAMO_ATTEMPT_ANNOTATION, type ModelDeployment, type DynamoReconfigureRequest } from '@airunway/shared';
import { HTTPException } from 'hono/http-exception';

const positive = () => z.number().finite().positive();
const tokens = () => positive().int().max(2_147_483_647);

export const dynamoIntentSchema = z.object({
  hardware: z.object({
    totalGpus: z.number().int().min(1).max(64),
    gpuSku: z.string().trim().min(1).max(128).optional(),
    vramMb: positive().max(10_000_000).optional(),
    numGpusPerNode: z.number().int().min(1).max(64).optional(),
  }).strict(),
  searchStrategy: z.literal('rapid').optional().default('rapid'),
  workload: z.object({
    isl: tokens().optional(),
    osl: tokens().optional(),
    requestRate: positive().max(1_000_000).optional(),
    concurrency: positive().max(1_000_000).optional(),
  }).strict().refine(w => w.requestRate === undefined || w.concurrency === undefined, {
    message: 'Specify requestRate or concurrency, not both',
  }).optional(),
  sla: z.object({
    ttft: positive().max(86_400_000).optional(),
    itl: positive().max(86_400_000).optional(),
    e2eLatency: positive().max(86_400_000).optional(),
  }).strict().refine(s => s.e2eLatency === undefined || (s.ttft === undefined && s.itl === undefined), {
    message: 'Specify e2eLatency or ttft/itl, not both',
  }).optional(),
}).strict();

export const dynamoOverridesSchema = z.object({
  deploymentMode: z.literal('intent'),
  intent: dynamoIntentSchema,
}).strict();

export const dynamoReconfigureSchema = z.object({
  resourceVersion: z.string().min(1).max(256),
  intent: dynamoIntentSchema.optional(),
  modelId: z.string().trim().min(1).max(512).optional(),
  engine: z.enum(['vllm', 'sglang', 'trtllm']).optional(),
}).strict();

/** Prepare one optimistic update. Never retry a stale read or drop unrelated fields. */
export function reconfigureDynamoDeployment(
  current: ModelDeployment, request: DynamoReconfigureRequest, attempt = crypto.randomUUID(),
): ModelDeployment {
  if (current.spec.provider?.name !== 'dynamo' || current.spec.provider.overrides?.deploymentMode !== 'intent') {
    throw new HTTPException(422, { message: 'Only automatic Dynamo deployments can be reconfigured' });
  }
  if (current.metadata.resourceVersion !== request.resourceVersion) {
    throw new HTTPException(409, { message: 'Deployment changed. Refresh it before retrying or reconfiguring.' });
  }
  const spec = current.spec;
  const extra = spec as unknown as Record<string, unknown>;
  const engine = spec.engine;
  if (spec.image || engine.image || engine.contextLength || engine.trustRemoteCode || engine.enforceEager
    || Object.keys(engine.args || {}).length || engine.extraArgs?.length || spec.model.servedName
    || spec.env?.length || spec.model.storage?.volumes?.length
    || Object.keys((extra.nodeSelector as object) || {}).length
    || (Array.isArray(extra.tolerations) && extra.tolerations.length)
    || (spec.podTemplate && Object.keys(spec.podTemplate).length)
    || (spec.secrets?.huggingFaceToken && spec.secrets.huggingFaceToken !== 'hf-token-secret')
    || current.metadata.annotations?.['airunway.ai/dynamo-test-backend'] === 'mocker') {
    throw new HTTPException(422, { message: 'This deployment has custom runtime, storage or access settings that require manual configuration' });
  }
  const next = structuredClone(current);
  const intent = request.intent ?? current.spec.provider.overrides.intent;
  const result = dynamoIntentSchema.safeParse(intent);
  if (!result.success) {
    throw new HTTPException(422, { message: 'A valid typed intent is required to reconfigure this deployment' });
  }
  next.metadata.annotations = { ...next.metadata.annotations, [DYNAMO_ATTEMPT_ANNOTATION]: attempt };
  // Keep unrelated provider options. Legacy spec and typed intent cannot coexist.
  const overrides = { ...next.spec.provider!.overrides, deploymentMode: 'intent', intent: result.data };
  delete (overrides as Record<string, unknown>).spec;
  next.spec.provider!.overrides = overrides;
  if (request.modelId !== undefined) next.spec.model.id = request.modelId;
  if (request.engine !== undefined) next.spec.engine.type = request.engine;
  delete next.spec.resources;
  delete next.spec.scaling;
  delete next.spec.serving;
  return next;
}
