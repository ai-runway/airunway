import logger from '../lib/logger';
import { createHash } from 'node:crypto';
import type { HfUserInfo, HfTokenExchangeResponse, HfApiModelResult, HfSearchParams, HfModelSearchResponse, ModelArchitecture } from '@airunway/shared';
import { filterCompatibleModels } from './modelCompatibility';

/**
 * HuggingFace OAuth Client ID
 * This is a public identifier and does not need to be secret
 */
const HF_CLIENT_ID = process.env.HF_CLIENT_ID || 'e05817a1-7053-4b9e-b292-29cd219fccf8';

/**
 * HuggingFace OAuth endpoints
 */
const HF_TOKEN_URL = 'https://huggingface.co/oauth/token';
const HF_WHOAMI_URL = 'https://huggingface.co/api/whoami-v2';
const HF_MODELS_URL = 'https://huggingface.co/api/models';
const HF_BASE_URL = 'https://huggingface.co';

/** Max characters per repo-id segment (HF's own repo-name limit is 96). */
const HF_REPO_SEGMENT_MAX = 96;

/**
 * Validate a HuggingFace repo id (`owner/name`, or a canonical single-segment id
 * like `gpt2`) before it is interpolated into an outbound URL.
 *
 * Security: `modelId` arrives from untrusted query/path input and is sent to
 * huggingface.co with the caller's `Authorization` token. Without this guard a
 * crafted value (`../../other`, extra `/segments`, `name?x=1`, whitespace) could
 * steer that authenticated request to an unintended path. We therefore allow
 * only 1–2 non-empty segments of safe characters and reject `.`/`..` traversal.
 */
export function isValidHfRepoId(modelId: string): boolean {
  if (typeof modelId !== 'string') return false;
  const segments = modelId.split('/');
  if (segments.length < 1 || segments.length > 2) return false;
  return segments.every(
    (seg) =>
      seg.length > 0 &&
      seg.length <= HF_REPO_SEGMENT_MAX &&
      seg !== '.' &&
      seg !== '..' &&
      /^[A-Za-z0-9._-]+$/.test(seg)
  );
}

/**
 * Percent-encode each path segment of a repo id before interpolation. Mirrors
 * the per-segment encoder in aikit.ts (`buildHuggingFaceUrl`). Callers should
 * still validate with `isValidHfRepoId` first — this is defense in depth so even
 * a validated id can never inject path/query syntax.
 */
export function encodeHfRepoPath(modelId: string): string {
  return modelId.split('/').map(encodeURIComponent).join('/');
}


/**
 * Cache for model architecture (config.json) lookups.
 *
 * Security: public (no-token) responses are keyed by modelId alone. Tokened
 * responses are keyed by `modelId + ':' + sha256(token)` so one user's gated
 * config can never be served to a caller with a different (or no) token.
 * Only successful lookups are cached, with a short TTL since `main` can move.
 *
 * Bounding: entries are TTL-expired lazily on re-access, but a single fetch of
 * many distinct modelId/token keys would otherwise keep them all resident for
 * the full TTL. An LRU size cap (`ARCH_CACHE_MAX_ENTRIES`) bounds the map under
 * wide-scan or adversarial traffic: the least-recently-used entry is evicted
 * once the cap is exceeded. The Map's insertion order is the recency order —
 * fresh hits re-insert to move themselves to the newest position.
 */
const ARCH_CACHE_TTL_MS = 60 * 60 * 1000; // 1 hour
const ARCH_CACHE_MAX_ENTRIES = 500;
const architectureCache = new Map<string, { value: ModelArchitecture; expiresAt: number }>();

function architectureCacheKey(modelId: string, hfToken?: string): string {
  if (!hfToken) {
    return modelId;
  }
  const tokenHash = createHash('sha256').update(hfToken).digest('hex');
  return `${modelId}:${tokenHash}`;
}

/** Shape of the subset of HuggingFace config.json fields we read. */
/**
 * The transformer dimensions we read from a HuggingFace `config.json`. For a
 * plain decoder LLM these live at the top level; for many multimodal /
 * image-text-to-text models they live in a nested sub-config (see below).
 */
interface HfTransformerConfig {
  num_hidden_layers?: number;
  num_attention_heads?: number;
  num_key_value_heads?: number;
  head_dim?: number;
  hidden_size?: number;
  max_position_embeddings?: number;
  torch_dtype?: string;
}

interface HfConfigJson extends HfTransformerConfig {
  // Composite/multimodal configs (LLaVA, InternVL, Llama-4, Gemma-3, Mllama, …)
  // nest the language-model dimensions under a sub-config rather than at the top
  // level. Checked in priority order by resolveTransformerConfig().
  text_config?: HfTransformerConfig;
  llm_config?: HfTransformerConfig;
  language_config?: HfTransformerConfig;
}

/**
 * Nested config keys that hold the language-model transformer dimensions on
 * composite/multimodal models, in priority order.
 */
const NESTED_TEXT_CONFIG_KEYS = ['text_config', 'llm_config', 'language_config'] as const;

/**
 * Resolve the sub-config carrying the language-model transformer dimensions.
 *
 * Plain decoder LLMs keep `num_hidden_layers` & friends at the top level; many
 * multimodal / image-text-to-text configs nest them under `text_config`,
 * `llm_config`, or `language_config` while the top level only describes the
 * composite model. Prefer the top-level fields when present (normal decoder),
 * else the first nested sub-config that looks like a transformer (has
 * `num_hidden_layers`). Falls back to the top-level object when nothing
 * qualifies, preserving the prior degrade-to-low-confidence behavior.
 */
function resolveTransformerConfig(config: HfConfigJson): HfTransformerConfig {
  if (config.num_hidden_layers != null) {
    return config;
  }
  for (const key of NESTED_TEXT_CONFIG_KEYS) {
    const nested = config[key];
    if (nested && typeof nested === 'object' && nested.num_hidden_layers != null) {
      return nested;
    }
  }
  return config;
}

// HuggingFace's `expand[]` parameter behaves like an explicit field selection.
// If we request only safetensors/gated, the API omits compatibility metadata such
// as config.architectures, pipeline_tag, and library_name, which makes otherwise
// deployable models disappear from search. Keep every field used by
// modelCompatibility.ts in this list.
const HF_MODEL_SEARCH_EXPAND_FIELDS = [
  'safetensors',
  'gated',
  'config',
  'pipeline_tag',
  'library_name',
  'tags',
  'downloads',
  'likes',
];

/**
 * HuggingFace OAuth Service
 * Handles OAuth token exchange and user info retrieval using PKCE flow
 */
class HuggingFaceService {
  /**
   * Get the HuggingFace OAuth client ID
   */
  getClientId(): string {
    return HF_CLIENT_ID;
  }

  /**
   * Exchange an authorization code for an access token using PKCE
   * @param code - The authorization code from HuggingFace OAuth callback
   * @param codeVerifier - The PKCE code verifier (original random string)
   * @param redirectUri - The redirect URI used in the authorization request
   */
  async exchangeCodeForToken(
    code: string,
    codeVerifier: string,
    redirectUri: string
  ): Promise<{ accessToken: string; expiresIn?: number; scope?: string }> {
    logger.debug({ redirectUri }, 'Exchanging HuggingFace authorization code for token');

    const params = new URLSearchParams({
      grant_type: 'authorization_code',
      client_id: HF_CLIENT_ID,
      code,
      redirect_uri: redirectUri,
      code_verifier: codeVerifier,
    });

    const response = await fetch(HF_TOKEN_URL, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
      },
      body: params.toString(),
    });

    if (!response.ok) {
      const errorText = await response.text();
      logger.error({ status: response.status, error: errorText }, 'Failed to exchange HF auth code');
      throw new Error(`Failed to exchange authorization code: ${response.status} ${errorText}`);
    }

    const data = await response.json();
    
    return {
      accessToken: data.access_token,
      expiresIn: data.expires_in,
      scope: data.scope,
    };
  }

  /**
   * Get user information from HuggingFace using an access token
   * @param accessToken - The HuggingFace access token
   */
  async getUserInfo(accessToken: string): Promise<HfUserInfo> {
    logger.debug('Fetching HuggingFace user info');

    const response = await fetch(HF_WHOAMI_URL, {
      headers: {
        Authorization: `Bearer ${accessToken}`,
      },
    });

    if (!response.ok) {
      const errorText = await response.text();
      logger.error({ status: response.status, error: errorText }, 'Failed to get HF user info');
      throw new Error(`Failed to get user info: ${response.status} ${errorText}`);
    }

    const data = await response.json();

    return {
      id: data.id,
      name: data.name,
      fullname: data.fullname || data.name,
      email: data.email,
      avatarUrl: data.avatarUrl && data.avatarUrl.startsWith('/') ? `${HF_BASE_URL}${data.avatarUrl}` : data.avatarUrl, // Ensure avatar URL is absolute
    };
  }

  /**
   * Validate an access token by attempting to fetch user info
   * @param accessToken - The HuggingFace access token to validate
   */
  async validateToken(accessToken: string): Promise<{ valid: boolean; user?: HfUserInfo; error?: string }> {
    try {
      const user = await this.getUserInfo(accessToken);
      return { valid: true, user };
    } catch (error) {
      return {
        valid: false,
        error: error instanceof Error ? error.message : 'Token validation failed',
      };
    }
  }

  /**
   * Exchange code and get user info in one call
   * This is the main method used by the OAuth callback endpoint
   */
  async handleOAuthCallback(
    code: string,
    codeVerifier: string,
    redirectUri: string
  ): Promise<HfTokenExchangeResponse> {
    // Exchange code for token
    const tokenResult = await this.exchangeCodeForToken(code, codeVerifier, redirectUri);

    // Get user info
    const user = await this.getUserInfo(tokenResult.accessToken);

    logger.info({ username: user.name }, 'HuggingFace OAuth successful');

    return {
      accessToken: tokenResult.accessToken,
      tokenType: 'Bearer',
      expiresIn: tokenResult.expiresIn,
      scope: tokenResult.scope,
      user,
    };
  }

  /**
   * Search HuggingFace models with compatibility filtering
   * 
   * @param params - Search parameters (query, limit, offset)
   * @param token - Optional HuggingFace access token for gated models
   * @returns Filtered search results with only compatible models
   */
  async searchModels(
    params: HfSearchParams,
    token?: string
  ): Promise<HfModelSearchResponse> {
    const { query, limit = 20, offset = 0 } = params;
    
    logger.debug({ query, limit, offset }, 'Searching HuggingFace models');

    // Build base search params (shared across pipeline tag queries)
    const baseParams = {
      search: query,
      full: 'true',
      config: 'true',
      limit: String(limit + offset + 10), // Fetch extra since we filter client-side
    };

    const headers: Record<string, string> = {};
    if (token) {
      headers['Authorization'] = `Bearer ${token}`;
    }

    // Search both text-generation and image-text-to-text pipeline tags in parallel.
    // Many modern multimodal models (Llama 4, Gemma 3, Kimi K2.5, etc.) are tagged
    // image-text-to-text on HuggingFace but work perfectly for text generation.
    const pipelineTags = ['text-generation', 'image-text-to-text'];
    const fetchResults = await Promise.all(
      pipelineTags.map(async (tag) => {
        const searchParams = new URLSearchParams({ ...baseParams, filter: tag });
        for (const field of HF_MODEL_SEARCH_EXPAND_FIELDS) {
          searchParams.append('expand[]', field);
        }
        const url = `${HF_MODELS_URL}?${searchParams.toString()}`;
        const response = await fetch(url, { headers });
        if (!response.ok) {
          logger.warn({ status: response.status, tag }, 'HuggingFace search failed for pipeline tag');
          return [] as HfApiModelResult[];
        }
        return (await response.json()) as HfApiModelResult[];
      })
    );

    // Merge and deduplicate results by model ID
    const seen = new Set<string>();
    const rawModels: HfApiModelResult[] = [];
    for (const results of fetchResults) {
      for (const model of results) {
        if (!seen.has(model.id)) {
          seen.add(model.id);
          rawModels.push(model);
        }
      }
    }
    
    // Filter for compatible models only
    let compatibleModels = filterCompatibleModels(rawModels);

    // When not logged in, exclude gated models since the user can't deploy them
    if (!token) {
      compatibleModels = compatibleModels.filter(model => !model.gated);
    }

    // Apply pagination after filtering
    const paginatedModels = compatibleModels.slice(offset, offset + limit);
    
    logger.debug(
      { 
        rawCount: rawModels.length, 
        compatibleCount: compatibleModels.length,
        returnedCount: paginatedModels.length 
      }, 
      'Model search completed'
    );

    return {
      models: paginatedModels,
      total: compatibleModels.length,
      hasMore: offset + paginatedModels.length < compatibleModels.length,
      query,
    };
  }

  /**
   * Get GGUF files from a HuggingFace repository
   * @param modelId - The model ID (e.g., 'unsloth/Qwen3-4B-GGUF')
   * @param accessToken - Optional access token for gated models
   */
  async getGgufFiles(modelId: string, accessToken?: string): Promise<string[]> {
    logger.debug({ modelId }, 'Fetching GGUF files from HuggingFace repo');

    // Reject malformed ids before issuing a token-bearing request (see
    // isValidHfRepoId). Throwing keeps this method's existing throw-on-failure
    // contract; callers already surface errors.
    if (!isValidHfRepoId(modelId)) {
      throw new Error('Invalid Hugging Face model id');
    }

    const url = `https://huggingface.co/api/models/${encodeHfRepoPath(modelId)}`;
    const headers: Record<string, string> = {};
    if (accessToken) {
      headers['Authorization'] = `Bearer ${accessToken}`;
    }

    const response = await fetch(url, { headers });

    if (!response.ok) {
      logger.error({ status: response.status, modelId }, 'Failed to fetch model info');
      throw new Error(`Failed to fetch model info: ${response.status}`);
    }

    const data = await response.json();
    
    // Extract GGUF files from siblings array
    const siblings = data.siblings || [];
    const ggufFiles = siblings
      .filter((file: { rfilename: string }) => file.rfilename.endsWith('.gguf'))
      .map((file: { rfilename: string }) => file.rfilename)
      .sort();

    logger.debug({ modelId, count: ggufFiles.length }, 'Found GGUF files');
    return ggufFiles;
  }

  /**
   * Fetch transformer architecture details from a model's config.json.
   *
   * Used to size the KV cache when estimating concurrent serving capacity.
   * Returns `undefined` on any failure (network, 404, gated without token,
   * unparseable) so callers degrade gracefully to a bandwidth-only estimate.
   *
   * @param modelId - HuggingFace model ID (e.g. 'meta-llama/Meta-Llama-3-70B')
   * @param hfToken - Optional HF access token for gated/private models
   */
  async getModelArchitecture(modelId: string, hfToken?: string): Promise<ModelArchitecture | undefined> {
    // Reject malformed ids before any cache work or token-bearing fetch. The
    // method's contract is graceful degradation (undefined → bandwidth-only
    // estimate), so an invalid id is treated the same as a lookup miss.
    if (!isValidHfRepoId(modelId)) {
      logger.debug({ modelId }, 'Rejecting invalid HuggingFace model id');
      return undefined;
    }

    const cacheKey = architectureCacheKey(modelId, hfToken);
    const cached = architectureCache.get(cacheKey);
    if (cached) {
      if (cached.expiresAt > Date.now()) {
        // Bump recency: delete + re-insert moves this key to the newest
        // position so the LRU eviction below sheds genuinely cold entries.
        architectureCache.delete(cacheKey);
        architectureCache.set(cacheKey, cached);
        return cached.value;
      }
      // Drop the stale entry so the cache stays bounded by "used within TTL"
      // rather than growing unbounded across many distinct modelIds/tokens.
      architectureCache.delete(cacheKey);
    }

    const url = `${HF_BASE_URL}/${encodeHfRepoPath(modelId)}/resolve/main/config.json`;
    const headers: Record<string, string> = {};
    if (hfToken) {
      headers['Authorization'] = `Bearer ${hfToken}`;
    }

    let config: HfConfigJson;
    try {
      const response = await fetch(url, { headers });
      if (!response.ok) {
        logger.debug({ status: response.status, modelId }, 'Failed to fetch model config.json');
        return undefined;
      }
      config = (await response.json()) as HfConfigJson;
    } catch (error) {
      logger.debug({ error, modelId }, 'Error fetching model config.json');
      return undefined;
    }

    // For multimodal/composite models the transformer dimensions live in a
    // nested sub-config (text_config / llm_config / language_config) rather than
    // at the top level; resolve to whichever object actually holds them.
    const tf = resolveTransformerConfig(config);

    // num_key_value_heads is absent on MHA models; fall back to attention heads.
    const numKvHeads = tf.num_key_value_heads ?? tf.num_attention_heads;
    // head_dim is often omitted; derive from hidden_size / num_attention_heads.
    const headDim =
      tf.head_dim ??
      (tf.hidden_size && tf.num_attention_heads
        ? tf.hidden_size / tf.num_attention_heads
        : undefined);

    const arch: ModelArchitecture = {
      numLayers: tf.num_hidden_layers,
      numKvHeads,
      headDim,
      maxPositionEmbeddings: tf.max_position_embeddings,
      torchDtype: tf.torch_dtype ?? config.torch_dtype,
    };

    // Cache successful lookups only.
    architectureCache.set(cacheKey, { value: arch, expiresAt: Date.now() + ARCH_CACHE_TTL_MS });
    // Evict the least-recently-used entry (the Map's oldest insertion) when the
    // size cap is exceeded, bounding memory under wide-scan/adversarial traffic.
    if (architectureCache.size > ARCH_CACHE_MAX_ENTRIES) {
      const oldestKey = architectureCache.keys().next().value;
      if (oldestKey !== undefined) {
        architectureCache.delete(oldestKey);
      }
    }
    return arch;
  }

  /**
   * Clear the in-memory model-architecture cache. Intended for tests, which
   * need a clean cache between cases without reloading the module (reloading
   * would replace this shared singleton and break other suites that captured
   * the original instance at import time).
   */
  clearArchitectureCacheForTests(): void {
    architectureCache.clear();
  }
}

// Export singleton instance
export const huggingFaceService = new HuggingFaceService();
