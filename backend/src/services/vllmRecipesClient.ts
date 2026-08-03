import type {
  VllmRecipeIndexEntry,
  VllmRecipeListResponse,
  VllmRecipeRawResponse,
} from '@airunway/shared';
import logger from '../lib/logger';

const DEFAULT_RECIPES_BASE_URL = 'https://recipes.vllm.ai';

// Abort recipe fetches that stall so a slow/unreachable recipes host cannot
// tie up request handlers and degrade backend availability.
const FETCH_TIMEOUT_MS = 10_000;

// Cap recipe payload size. VLLM_RECIPES_BASE_URL is operator-configurable, so an
// oversized models.json/recipe must not be able to pressure backend memory.
const MAX_RESPONSE_BYTES = 5 * 1024 * 1024; // 5 MiB

// Short in-memory cache for the recipe index and per-model payloads. The design
// requires caching so repeated UI refreshes / concurrent resolves don't each
// block on an upstream round-trip. Entries are served stale-on-error.
const CACHE_TTL_MS = 5 * 60 * 1000; // 5 minutes

// Hard cap on cached per-model entries. The GET /:org/:model route is
// unauthenticated, so iterating many unique model IDs must not grow the cache
// without bound (each entry is up to MAX_RESPONSE_BYTES). The Map's insertion
// order is the recency order, so the least-recently-used entry is evicted first.
const MAX_CACHE_ENTRIES = 256;

/**
 * Bad client input (e.g. a malformed Hugging Face model id). Callers should map
 * this to a 4xx, never a 5xx — it is not an upstream failure.
 */
export class VllmRecipeValidationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'VllmRecipeValidationError';
  }
}

/** The upstream recipes host timed out. Callers should map to 504. */
export class VllmRecipeTimeoutError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'VllmRecipeTimeoutError';
  }
}

/** The upstream recipes host failed or returned an unusable payload. Map to 502. */
export class VllmRecipeUpstreamError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'VllmRecipeUpstreamError';
  }
}

type JsonRecord = Record<string, unknown>;

interface CacheEntry<T> {
  value: T;
  expiresAt: number;
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function splitModelId(modelId: string): { org: string; model: string } {
  // A Hugging Face model ID must be exactly "<org>/<model>": a single slash,
  // both segments non-empty, and neither segment a path-traversal token. This
  // prevents crafted IDs like "foo/a/b" or "foo/../bar" from escaping the
  // recipes base path when the segments are interpolated into a fetch URL.
  const segments = modelId.split('/');
  if (segments.length !== 2) {
    throw new VllmRecipeValidationError(`Invalid Hugging Face model ID: ${modelId}`);
  }

  const [org, model] = segments;
  if (!org || !model || org === '.' || org === '..' || model === '.' || model === '..') {
    throw new VllmRecipeValidationError(`Invalid Hugging Face model ID: ${modelId}`);
  }

  return { org, model };
}

export class VllmRecipesClient {
  private listCache?: CacheEntry<VllmRecipeListResponse>;
  private readonly modelCache = new Map<string, CacheEntry<VllmRecipeRawResponse>>();

  constructor(private readonly baseUrl = process.env.VLLM_RECIPES_BASE_URL || DEFAULT_RECIPES_BASE_URL) {}

  get sourceBaseUrl(): string {
    return this.baseUrl.replace(/\/+$/, '');
  }

  async list(): Promise<VllmRecipeListResponse> {
    const now = Date.now();
    if (this.listCache && this.listCache.expiresAt > now) {
      return this.listCache.value;
    }

    const source = `${this.sourceBaseUrl}/models.json`;
    let payload: unknown;
    try {
      payload = await this.fetchJson<unknown>(source);
    } catch (error) {
      // Serve the last good index on a transient upstream failure.
      if (this.listCache) {
        logger.warn({ error }, 'Serving stale vLLM recipe index after upstream failure');
        return this.listCache.value;
      }
      throw error;
    }

    let entries: unknown;
    if (Array.isArray(payload)) {
      entries = payload;
    } else if (isRecord(payload)) {
      entries = payload.models ?? payload.recipes;
    }

    if (!Array.isArray(entries)) {
      throw new VllmRecipeUpstreamError('vLLM recipe index did not contain a models array');
    }

    const recipes = entries.filter(isRecord).map((entry) => ({ ...entry }) as VllmRecipeIndexEntry);

    const result: VllmRecipeListResponse = {
      recipes,
      total: recipes.length,
      source,
    };
    this.listCache = { value: result, expiresAt: now + CACHE_TTL_MS };
    return result;
  }

  async get(org: string, model: string): Promise<VllmRecipeRawResponse> {
    return this.getByModelId(`${org}/${model}`);
  }

  async getByModelId(modelId: string): Promise<VllmRecipeRawResponse> {
    const { org, model } = splitModelId(modelId);

    const now = Date.now();
    const cached = this.modelCache.get(modelId);
    if (cached && cached.expiresAt > now) {
      // Re-insert to move this entry to the most-recently-used position.
      this.modelCache.delete(modelId);
      this.modelCache.set(modelId, cached);
      return cached.value;
    }

    const source = `${this.sourceBaseUrl}/${encodeURIComponent(org)}/${encodeURIComponent(model)}.json`;
    let payload: unknown;
    try {
      payload = await this.fetchJson<unknown>(source);
    } catch (error) {
      if (cached) {
        logger.warn({ error, modelId }, 'Serving stale vLLM recipe after upstream failure');
        return cached.value;
      }
      throw error;
    }

    if (!isRecord(payload)) {
      throw new VllmRecipeUpstreamError(`vLLM recipe payload for ${modelId} was not a JSON object`);
    }

    const result: VllmRecipeRawResponse = {
      modelId,
      source,
      recipe: payload,
    };
    this.setModelCache(modelId, { value: result, expiresAt: now + CACHE_TTL_MS });
    return result;
  }

  // setModelCache inserts an entry and evicts the least-recently-used one(s) when
  // the cache exceeds MAX_CACHE_ENTRIES, keeping memory bounded under wide scans.
  private setModelCache(modelId: string, entry: CacheEntry<VllmRecipeRawResponse>): void {
    // Delete first so a re-write moves the key to the newest insertion position.
    this.modelCache.delete(modelId);
    this.modelCache.set(modelId, entry);
    while (this.modelCache.size > MAX_CACHE_ENTRIES) {
      const oldest = this.modelCache.keys().next().value;
      if (oldest === undefined) {
        break;
      }
      this.modelCache.delete(oldest);
    }
  }

  async fetchReference(reference: string): Promise<JsonRecord> {
    const source = this.resolveReference(reference);
    const payload = await this.fetchJson<unknown>(source);

    if (!isRecord(payload)) {
      throw new VllmRecipeUpstreamError(`vLLM recipe reference ${source} was not a JSON object`);
    }

    return payload;
  }

  resolveReference(reference: string): string {
    const baseUrl = new URL(`${this.sourceBaseUrl}/`);
    const resolved = new URL(reference, baseUrl);

    if (resolved.protocol !== 'https:') {
      throw new VllmRecipeValidationError(`vLLM recipe references must use HTTPS: ${reference}`);
    }

    if (resolved.origin !== baseUrl.origin) {
      throw new VllmRecipeValidationError(`vLLM recipe references must stay under ${baseUrl.origin}: ${reference}`);
    }

    if (!resolved.pathname.startsWith(baseUrl.pathname)) {
      throw new VllmRecipeValidationError(`vLLM recipe references must stay under ${this.sourceBaseUrl}: ${reference}`);
    }

    return resolved.toString();
  }

  private async fetchJson<T>(url: string): Promise<T> {
    logger.debug({ url }, 'Fetching vLLM recipe JSON');

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);

    let response: Response;
    try {
      response = await fetch(url, {
        headers: {
          Accept: 'application/json',
        },
        signal: controller.signal,
      });
    } catch (error) {
      if (error instanceof Error && error.name === 'AbortError') {
        throw new VllmRecipeTimeoutError(`Timed out fetching ${url} after ${FETCH_TIMEOUT_MS}ms`);
      }
      throw new VllmRecipeUpstreamError(
        `Failed to reach ${url}: ${error instanceof Error ? error.message : String(error)}`
      );
    } finally {
      clearTimeout(timeoutId);
    }

    if (!response.ok) {
      throw new VllmRecipeUpstreamError(`Failed to fetch ${url}: ${response.status} ${response.statusText}`);
    }

    // Reject up front when the server advertises an oversized length...
    const declaredLength = Number(response.headers.get('content-length'));
    if (Number.isFinite(declaredLength) && declaredLength > MAX_RESPONSE_BYTES) {
      throw new VllmRecipeUpstreamError(
        `vLLM recipe response from ${url} exceeds ${MAX_RESPONSE_BYTES} bytes (content-length ${declaredLength})`
      );
    }

    // ...and bound the bytes actually read so a chunked / no-content-length reply
    // cannot stream past the cap before we reject it. Aborting the controller
    // cancels the underlying connection.
    const text = await this.readBoundedBody(response, url, controller);

    try {
      return JSON.parse(text) as T;
    } catch (error) {
      throw new VllmRecipeUpstreamError(
        `vLLM recipe response from ${url} was not valid JSON: ${error instanceof Error ? error.message : String(error)}`
      );
    }
  }

  // readBoundedBody streams the response body, aborting as soon as the running
  // byte total exceeds MAX_RESPONSE_BYTES. Falls back to a buffered read only when
  // the body is not a readable stream (then re-checks the size).
  private async readBoundedBody(response: Response, url: string, controller: AbortController): Promise<string> {
    const body = response.body;
    if (!body || typeof body.getReader !== 'function') {
      const buffer = await response.arrayBuffer();
      if (buffer.byteLength > MAX_RESPONSE_BYTES) {
        throw new VllmRecipeUpstreamError(`vLLM recipe response from ${url} exceeds ${MAX_RESPONSE_BYTES} bytes`);
      }
      return new TextDecoder().decode(buffer);
    }

    const reader = body.getReader();
    const decoder = new TextDecoder();
    let received = 0;
    let text = '';
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) {
          break;
        }
        if (value) {
          received += value.byteLength;
          if (received > MAX_RESPONSE_BYTES) {
            controller.abort();
            throw new VllmRecipeUpstreamError(
              `vLLM recipe response from ${url} exceeds ${MAX_RESPONSE_BYTES} bytes`
            );
          }
          text += decoder.decode(value, { stream: true });
        }
      }
      text += decoder.decode();
      return text;
    } finally {
      reader.releaseLock();
    }
  }
}

export const vllmRecipesClient = new VllmRecipesClient();
