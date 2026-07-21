/**
 * vLLM Recipes API
 */

import type { RequestFn } from './client';
import type {
  VllmRecipeListResponse,
  VllmRecipeRawResponse,
  VllmRecipeResolveRequest,
  VllmRecipeResolveResult,
} from '../types';

export interface VllmRecipesApi {
  /** List recipes available from recipes.vllm.ai */
  list: () => Promise<VllmRecipeListResponse>;

  /** Get a raw recipe payload by Hugging Face org and model name */
  get: (org: string, model: string) => Promise<VllmRecipeRawResponse>;

  /** Resolve a recipe into Direct vLLM deployment fields */
  resolve: (request: VllmRecipeResolveRequest) => Promise<VllmRecipeResolveResult>;
}

export function createVllmRecipesApi(request: RequestFn): VllmRecipesApi {
  return {
    list: () => request<VllmRecipeListResponse>('/vllm/recipes'),

    get: (org: string, model: string) =>
      request<VllmRecipeRawResponse>(
        `/vllm/recipes/${encodeURIComponent(org)}/${encodeURIComponent(model)}`
      ),

    resolve: (resolveRequest: VllmRecipeResolveRequest) =>
      request<VllmRecipeResolveResult>('/vllm/recipes/resolve', {
        method: 'POST',
        body: JSON.stringify(resolveRequest),
      }),
  };
}
