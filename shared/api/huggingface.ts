/**
 * HuggingFace OAuth & Models API
 */

import type { RequestFn } from './client';
import type {
  HfTokenExchangeRequest,
  HfTokenExchangeResponse,
  HfSaveSecretRequest,
  HfSecretStatus,
  HfUserInfo,
  HfModelSearchResponse,
} from '../types';

export interface HfOAuthConfig {
  clientId: string;
  authorizeUrl: string;
  scopes: string[];
}

export interface HfSaveSecretResponse {
  success: boolean;
  message: string;
  user?: HfUserInfo;
  results: { namespace: string; success: boolean; error?: string }[];
}

export interface HfDeleteSecretResponse {
  success: boolean;
  message: string;
  results: { namespace: string; success: boolean; error?: string }[];
}

export interface HfSearchOptions {
  limit?: number;
  offset?: number;
  hfToken?: string;
}

export interface HfStartOAuthRequest {
  redirectUri: string;
}

export interface HfStartOAuthResponse {
  authorizationUrl: string;
  state: string;
}

export interface HfTokenWithStateRequest {
  code: string;
  state: string;
}

export interface HuggingFaceApi {
  /** Get OAuth configuration (client ID, scopes) */
  getOAuthConfig: () => Promise<HfOAuthConfig>;

  /** Start OAuth flow - backend generates PKCE and returns authorization URL */
  startOAuth: (data: HfStartOAuthRequest) => Promise<HfStartOAuthResponse>;

  /**
   * Exchange authorization code for access token using a server-side PKCE
   * verifier looked up by `state`. The verifier never leaves the server.
   * Use this from callbacks where the verifier is not held client-side
   * (e.g. the Headlamp plugin).
   */
  exchangeTokenWithState: (data: HfTokenWithStateRequest) => Promise<HfTokenExchangeResponse>;

  /** Exchange authorization code for access token */
  exchangeToken: (data: HfTokenExchangeRequest) => Promise<HfTokenExchangeResponse>;

  /** Get status of HuggingFace secret across namespaces */
  getSecretStatus: () => Promise<HfSecretStatus>;

  /** Save HuggingFace token as K8s secrets */
  saveSecret: (data: HfSaveSecretRequest) => Promise<HfSaveSecretResponse>;

  /** Delete HuggingFace secrets from all namespaces */
  deleteSecret: () => Promise<HfDeleteSecretResponse>;

  /** Search HuggingFace models with compatibility filtering */
  searchModels: (
    query: string,
    options?: HfSearchOptions
  ) => Promise<HfModelSearchResponse>;

  /** Get GGUF files available in a HuggingFace repository */
  getGgufFiles: (modelId: string, hfToken?: string) => Promise<{ files: string[] }>;
}

export function createHuggingFaceApi(request: RequestFn): HuggingFaceApi {
  return {
    getOAuthConfig: () => request<HfOAuthConfig>('/oauth/huggingface/config'),

    startOAuth: (data: HfStartOAuthRequest) =>
      request<HfStartOAuthResponse>('/oauth/huggingface/start', {
        method: 'POST',
        body: JSON.stringify(data),
      }),

    exchangeTokenWithState: (data: HfTokenWithStateRequest) =>
      request<HfTokenExchangeResponse>('/oauth/huggingface/token-with-state', {
        method: 'POST',
        body: JSON.stringify(data),
      }),

    exchangeToken: (data: HfTokenExchangeRequest) =>
      request<HfTokenExchangeResponse>('/oauth/huggingface/token', {
        method: 'POST',
        body: JSON.stringify(data),
      }),

    getSecretStatus: () => request<HfSecretStatus>('/secrets/huggingface/status'),

    saveSecret: (data: HfSaveSecretRequest) =>
      request<HfSaveSecretResponse>('/secrets/huggingface', {
        method: 'POST',
        body: JSON.stringify(data),
      }),

    deleteSecret: () =>
      request<HfDeleteSecretResponse>('/secrets/huggingface', {
        method: 'DELETE',
      }),

    searchModels: (query: string, options?: HfSearchOptions) => {
      const params = new URLSearchParams({ q: query });
      if (options?.limit) params.set('limit', options.limit.toString());
      if (options?.offset) params.set('offset', options.offset.toString());

      // Build headers - include HF token if provided for gated model access
      const headers: Record<string, string> = {};
      if (options?.hfToken) {
        headers['Authorization'] = `Bearer ${options.hfToken}`;
      }

      return request<HfModelSearchResponse>(`/models/search?${params.toString()}`, {
        headers,
      });
    },

    getGgufFiles: (modelId: string, hfToken?: string) => {
      const headers: Record<string, string> = {};
      if (hfToken) {
        headers['Authorization'] = `Bearer ${hfToken}`;
      }
      return request<{ files: string[] }>(
        `/models/${encodeURIComponent(modelId)}/gguf-files`,
        { headers }
      );
    },
  };
}
