/**
 * API Client Wrapper for Headlamp Plugin
 *
 * Wraps the shared API client with Headlamp-specific configuration,
 * including authentication token injection from Headlamp's context.
 */

import { createApiClient, type ApiClient } from '@airunway/shared/api';
import { getBackendUrlSync, clearBackendCache } from './backend-discovery';

// Re-export types for convenience
export type { ApiClient } from '@airunway/shared/api';
export * from '@airunway/shared/api';

// Singleton API client instance
let apiClientInstance: ApiClient | null = null;
let currentBaseUrl: string | null = null;

/**
 * Get the Kubernetes auth token from Headlamp's context
 */
function getHeadlampToken(): string | null {
  try {
    // Headlamp stores the token in different ways depending on context
    // Try the plugin API first
    const win = window as unknown as Record<string, unknown>;
    if (
      typeof window !== 'undefined' &&
      win.pluginLib &&
      typeof (win.pluginLib as Record<string, unknown>).getToken === 'function'
    ) {
      return ((win.pluginLib as { getToken: () => string })).getToken();
    }

    // Try localStorage token storage (used by Headlamp desktop)
    const token = localStorage.getItem('headlamp_token');
    if (token) {
      return token;
    }

    // For in-cluster Headlamp, the token might be in a cookie or injected
    // Check for any stored cluster tokens
    const clusterTokens = localStorage.getItem('headlampAuthToken');
    if (clusterTokens) {
      try {
        const parsed = JSON.parse(clusterTokens);
        // Get the token for the current cluster
        const currentCluster = localStorage.getItem('headlampCurrentCluster');
        if (currentCluster && parsed[currentCluster]) {
          return parsed[currentCluster];
        }
        // Return the first available token
        const tokens = Object.values(parsed) as string[];
        if (tokens.length > 0) {
          return tokens[0];
        }
      } catch {
        // JSON parse failed, token might be a direct string
        return clusterTokens;
      }
    }

    return null;
  } catch {
    console.warn('[AIRunway] Failed to get Headlamp token');
    return null;
  }
}

/**
 * Get or create the API client instance
 */
export function getApiClient(): ApiClient {
  const baseUrl = getBackendUrlSync();

  // Create new client if base URL changed or no client exists
  if (!apiClientInstance || currentBaseUrl !== baseUrl) {
    currentBaseUrl = baseUrl;
    apiClientInstance = createApiClient({
      baseUrl,
      getToken: getHeadlampToken,
      onUnauthorized: () => {
        console.warn('[AIRunway] Unauthorized - token may be invalid');
        // Could dispatch an event here to show a notification
      },
    });
  }

  return apiClientInstance;
}

/**
 * Reset the API client (call when backend URL changes)
 */
export function resetApiClient(): void {
  apiClientInstance = null;
  currentBaseUrl = null;
  clearBackendCache();
}

/**
 * React hook to get the API client
 *
 * Note: This is a simple getter, not a true hook with state.
 * The API client is a singleton that doesn't change during the session.
 */
export function useApiClient(): ApiClient {
  return getApiClient();
}

// ============================================================================
// Convenience API exports (direct access without needing the client object)
// ============================================================================

export const api = {
  get models() {
    return getApiClient().models;
  },
  get deployments() {
    return getApiClient().deployments;
  },
  get health() {
    return getApiClient().health;
  },
  get settings() {
    return getApiClient().settings;
  },
  get runtimes() {
    return getApiClient().runtimes;
  },
  get installation() {
    return getApiClient().installation;
  },
  get gpuOperator() {
    return getApiClient().gpuOperator;
  },
  get gateway() {
    return getApiClient().gateway;
  },
  get autoscaler() {
    return getApiClient().autoscaler;
  },
  get huggingFace() {
    return getApiClient().huggingFace;
  },
  get aikit() {
    return getApiClient().aikit;
  },
  get aiConfigurator() {
    return getApiClient().aiConfigurator;
  },
  get metrics() {
    return getApiClient().metrics;
  },
};
