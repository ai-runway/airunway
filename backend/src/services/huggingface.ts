import logger from '../lib/logger';
import type { HfUserInfo, HfTokenExchangeResponse, HfApiModelResult, HfModelSearchResult, HfSearchParams, HfModelSearchResponse } from '@airunway/shared';
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
        const url = `${HF_MODELS_URL}?${searchParams.toString()}&expand[]=safetensors&expand[]=gated`;
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

    const url = `https://huggingface.co/api/models/${modelId}`;
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
}

// Export singleton instance
export const huggingFaceService = new HuggingFaceService();
