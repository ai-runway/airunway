import type { DeploymentMode, RecipeProvenance } from './deployment';
import type { Engine } from './model';

export interface VllmRecipeIndexEntry {
  hf_id: string;
  title?: string;
  provider?: string;
  url?: string;
  json?: string;
  [key: string]: unknown;
}

export interface VllmRecipeListResponse {
  recipes: VllmRecipeIndexEntry[];
  total: number;
  source: string;
}

export type VllmRecipeRawPayload = Record<string, unknown>;

export interface VllmRecipeRawResponse {
  modelId: string;
  source: string;
  recipe: VllmRecipeRawPayload;
}

export type VllmRecipeImageChoice =
  | { type: 'recipe' }
  | { type: 'custom'; imageRef: string }
  | { type: 'none' };

export interface VllmRecipeResolveRequest {
  modelId: string;
  mode?: DeploymentMode;
  hardware?: string;
  strategy?: string;
  variant?: string;
  features?: string[];
  imageChoice?: VllmRecipeImageChoice;
}

export interface VllmRecipeResolvedResources {
  gpu: number;
  memory?: string;
}

export interface VllmRecipeResolveResult {
  provider: 'vllm';
  engine: Engine;
  mode: DeploymentMode;
  imageRef?: string;
  resources: VllmRecipeResolvedResources;
  engineArgs: Record<string, string>;
  engineExtraArgs: string[];
  env: Record<string, string>;
  annotations: Record<string, string>;
  recipeProvenance: RecipeProvenance;
  warnings: string[];
}
