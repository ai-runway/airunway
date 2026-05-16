/**
 * Route path constants for the AI Runway Headlamp plugin
 */

export const ROUTES = {
  /** Base path for all AI Runway routes */
  BASE: '/airunway',

  /** Deployments list page */
  DEPLOYMENTS: '/airunway/deployments',

  /** Deployment details page (with name and namespace params) */
  DEPLOYMENT_DETAILS: '/airunway/deployments/:namespace/:name',

  /** Models catalog page */
  MODELS: '/airunway/models',

  /** Runtimes status page */
  RUNTIMES: '/airunway/runtimes',

  /** Gateway status page */
  GATEWAY: '/airunway/gateway',

  /** Settings page */
  SETTINGS: '/airunway/settings',

  /** Integrations page */
  INTEGRATIONS: '/airunway/integrations',

  /** Create deployment wizard */
  CREATE_DEPLOYMENT: '/airunway/deployments/create',

  /** HuggingFace OAuth callback */
  HUGGINGFACE_CALLBACK: '/airunway/oauth/callback/huggingface',
} as const;

/**
 * Generate a deployment details URL
 */
export function getDeploymentDetailsUrl(name: string, namespace: string): string {
  return `/airunway/deployments/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
}

/**
 * Generate a create deployment URL with model pre-selected
 */
export function getCreateDeploymentUrl(modelId: string, source?: 'huggingface' | 'curated'): string {
  const params = new URLSearchParams({ modelId });
  if (source) {
    params.append('source', source);
  }
  return `${ROUTES.CREATE_DEPLOYMENT}?${params.toString()}`;
}

/**
 * Get the full OAuth callback URL for HuggingFace
 * Uses the current window origin to support different environments
 */
export function getHuggingFaceCallbackUrl(): string {
  return `${window.location.origin}${ROUTES.HUGGINGFACE_CALLBACK}`;
}
