/**
 * Metrics Service
 * Fetches and processes Prometheus metrics from inference deployments
 */

import type { MetricsResponse, RawMetricValue } from '@airunway/shared';
import { parsePrometheusText } from '../lib/prometheus-parser';
import logger from '../lib/logger';
import { kubernetesService } from './kubernetes';
import * as fs from 'fs';

// Timeout for metrics fetch (5 seconds)
const METRICS_FETCH_TIMEOUT = 5000;

// Kubernetes service account token path (exists only when running in-cluster)
const K8S_SERVICE_ACCOUNT_TOKEN_PATH = '/var/run/secrets/kubernetes.io/serviceaccount/token';

// Default metrics configuration for inference deployments
const DEFAULT_METRICS_CONFIG = {
  serviceNamePattern: '{name}',
  port: 8000,
  endpointPath: '/metrics',
};

// Keep metrics reasonably fresh while collapsing duplicate scrapes from multiple UI consumers.
const METRICS_SUCCESS_CACHE_TTL_MS = 15000;
const METRICS_ERROR_CACHE_TTL_MS = 5000;

/**
 * Check if AI Runway is running inside a Kubernetes cluster
 * This is determined by the presence of the service account token
 */
function isRunningInCluster(): boolean {
  try {
    return fs.existsSync(K8S_SERVICE_ACCOUNT_TOKEN_PATH);
  } catch {
    return false;
  }
}

// Cache the in-cluster check result
let _isInCluster: boolean | null = null;
function checkInCluster(): boolean {
  if (_isInCluster === null) {
    _isInCluster = isRunningInCluster();
    logger.info({ inCluster: _isInCluster }, 'Detected cluster environment');
  }
  return _isInCluster;
}

/**
 * Build the metrics URL for a deployment
 */
export function buildMetricsUrl(
  deploymentName: string,
  namespace: string,
  servicePattern: string,
  port: number,
  endpointPath: string
): string {
  // Replace {name} placeholder with actual deployment name
  const serviceName = servicePattern.replace('{name}', deploymentName);

  // Build the in-cluster service URL
  // Format: http://<service>.<namespace>.svc.cluster.local:<port><path>
  return `http://${serviceName}.${namespace}.svc.cluster.local:${port}${endpointPath}`;
}

/**
 * Fetch raw metrics from a deployment's metrics endpoint
 */
async function fetchRawMetrics(url: string): Promise<string> {
  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), METRICS_FETCH_TIMEOUT);

  try {
    const response = await fetch(url, {
      method: 'GET',
      signal: controller.signal,
      headers: {
        'Accept': 'text/plain',
      },
    });

    if (!response.ok) {
      throw new Error(`HTTP ${response.status}: ${response.statusText}`);
    }

    return await response.text();
  } finally {
    clearTimeout(timeoutId);
  }
}

export function mapMetricsErrorMessage(errorMessage: string): string {
  if (errorMessage.includes('ENOTFOUND') || errorMessage.includes('getaddrinfo')) {
    return 'Cannot resolve service DNS. The deployment service may not exist yet.';
  }
  if (errorMessage.includes('no cluster') || errorMessage.includes('connect ECONNREFUSED')) {
    return 'Cannot connect to the Kubernetes cluster. Check your kubeconfig.';
  }
  if (errorMessage.includes('ECONNREFUSED')) {
    return 'Connection refused. The deployment may not be ready yet.';
  }
  if (errorMessage.includes('abort')) {
    return 'Request timed out. The deployment may be under heavy load or not responding.';
  }
  if (errorMessage.includes('HTTP 404') || errorMessage.includes('404')) {
    return 'Metrics endpoint not found. The deployment may not expose metrics.';
  }
  if (errorMessage.includes('HTTP 503') || errorMessage.includes('503')) {
    return 'Service unavailable. The deployment is starting up.';
  }
  if (errorMessage.includes('fetch failed') || errorMessage.includes('TypeError')) {
    return 'Cannot connect to metrics endpoint. Verify the deployment is running.';
  }

  return errorMessage;
}

interface MetricsServiceCacheEntry {
  response: MetricsResponse;
  expiresAt: number;
}

interface MetricsServiceOptions {
  fetchRawMetrics?: (url: string) => Promise<string>;
  proxyServiceGet?: (serviceName: string, namespace: string, port: number, path: string) => Promise<string>;
  checkInCluster?: () => boolean;
  now?: () => number;
  successCacheTtlMs?: number;
  errorCacheTtlMs?: number;
}

export interface DeploymentMetricsEndpointOptions {
  providerId?: string;
  serviceName?: string;
  port?: number;
  endpointPath?: string;
}

interface ResolvedMetricsTarget {
  providerId?: string;
  serviceName: string;
  port: number;
  endpointPath: string;
}

function normalizeEndpointPath(endpointPath?: string): string {
  const path = endpointPath?.trim() || DEFAULT_METRICS_CONFIG.endpointPath;
  return path.startsWith('/') ? path : `/${path}`;
}

/**
 * MetricsService class for fetching deployment metrics
 */
export class MetricsService {
  private readonly responseCache = new Map<string, MetricsServiceCacheEntry>();
  private readonly inFlightRequests = new Map<string, Promise<MetricsResponse>>();
  private readonly fetchRawMetricsFn: (url: string) => Promise<string>;
  private readonly proxyServiceGetFn: (serviceName: string, namespace: string, port: number, path: string) => Promise<string>;
  private readonly checkInClusterFn: () => boolean;
  private readonly nowFn: () => number;
  private readonly successCacheTtlMs: number;
  private readonly errorCacheTtlMs: number;

  constructor(options: MetricsServiceOptions = {}) {
    this.fetchRawMetricsFn = options.fetchRawMetrics ?? fetchRawMetrics;
    this.proxyServiceGetFn = options.proxyServiceGet ?? ((serviceName, namespace, port, path) =>
      kubernetesService.proxyServiceGet(serviceName, namespace, port, path));
    this.checkInClusterFn = options.checkInCluster ?? checkInCluster;
    this.nowFn = options.now ?? Date.now;
    this.successCacheTtlMs = options.successCacheTtlMs ?? METRICS_SUCCESS_CACHE_TTL_MS;
    this.errorCacheTtlMs = options.errorCacheTtlMs ?? METRICS_ERROR_CACHE_TTL_MS;
  }

  /**
   * Check if metrics fetching is available (requires cluster connection)
   */
  isMetricsAvailable(): boolean {
    return true;
  }

  clearCache(): void {
    this.responseCache.clear();
    this.inFlightRequests.clear();
  }

  private resolveMetricsTarget(
    deploymentName: string,
    options: DeploymentMetricsEndpointOptions = {}
  ): ResolvedMetricsTarget {
    const providerId = options.providerId?.trim();
    const normalizedProviderId = providerId?.toLowerCase();
    const explicitServiceName = options.serviceName?.trim();
    const explicitPort =
      options.port && Number.isFinite(options.port) && options.port > 0
        ? options.port
        : undefined;

    let serviceName = explicitServiceName;
    const port = explicitPort ?? DEFAULT_METRICS_CONFIG.port;

    if (!serviceName) {
      if (normalizedProviderId === 'dynamo') {
        serviceName = `${deploymentName}-frontend`;
      } else {
        serviceName = DEFAULT_METRICS_CONFIG.serviceNamePattern.replace('{name}', deploymentName);
      }
    }

    return {
      providerId,
      serviceName,
      port,
      endpointPath: normalizeEndpointPath(options.endpointPath),
    };
  }

  private buildCacheKey(deploymentName: string, namespace: string, target: ResolvedMetricsTarget): string {
    return [
      namespace,
      deploymentName,
      target.providerId ?? 'default',
      target.serviceName,
      target.port,
      target.endpointPath,
    ].join('/');
  }

  private getCachedResponse(cacheKey: string): MetricsResponse | null {
    const cached = this.responseCache.get(cacheKey);
    if (!cached) {
      return null;
    }

    if (cached.expiresAt <= this.nowFn()) {
      this.responseCache.delete(cacheKey);
      return null;
    }

    return cached.response;
  }

  private cacheResponse(cacheKey: string, response: MetricsResponse): void {
    const ttlMs = response.available ? this.successCacheTtlMs : this.errorCacheTtlMs;
    if (ttlMs <= 0) {
      return;
    }

    this.responseCache.set(cacheKey, {
      response,
      expiresAt: this.nowFn() + ttlMs,
    });
  }

  /**
   * Get metrics for a deployment
   *
   * @param deploymentName - Name of the deployment
   * @param namespace - Kubernetes namespace
   * @param providerIdOrOptions - Optional provider ID or endpoint options
   * @returns MetricsResponse with available metrics or error
   */
  async getDeploymentMetrics(
    deploymentName: string,
    namespace: string,
    providerIdOrOptions?: string | DeploymentMetricsEndpointOptions
  ): Promise<MetricsResponse> {
    const options: DeploymentMetricsEndpointOptions =
      typeof providerIdOrOptions === 'string'
        ? { providerId: providerIdOrOptions }
        : (providerIdOrOptions ?? {});
    const target = this.resolveMetricsTarget(deploymentName, options);
    const cacheKey = this.buildCacheKey(deploymentName, namespace, target);
    const cachedResponse = this.getCachedResponse(cacheKey);
    if (cachedResponse) {
      logger.debug({ deploymentName, namespace }, 'Serving cached deployment metrics');
      return cachedResponse;
    }

    const inFlightRequest = this.inFlightRequests.get(cacheKey);
    if (inFlightRequest) {
      logger.debug({ deploymentName, namespace }, 'Joining in-flight deployment metrics request');
      return inFlightRequest;
    }

    const request = this.fetchDeploymentMetrics(deploymentName, namespace, target, cacheKey).finally(() => {
      this.inFlightRequests.delete(cacheKey);
    });

    this.inFlightRequests.set(cacheKey, request);
    return request;
  }

  private async fetchDeploymentMetrics(
    deploymentName: string,
    namespace: string,
    target: ResolvedMetricsTarget,
    cacheKey: string
  ): Promise<MetricsResponse> {
    const timestamp = new Date(this.nowFn()).toISOString();
    const inCluster = this.checkInClusterFn();

    try {
      let rawText: string;

      if (inCluster) {
        // In-cluster: fetch directly via cluster DNS (fast path)
        const url = buildMetricsUrl(
          deploymentName,
          namespace,
          target.serviceName,
          target.port,
          target.endpointPath
        );

        logger.debug(
          {
            url,
            deploymentName,
            namespace,
            providerId: target.providerId,
            serviceName: target.serviceName,
            port: target.port,
            path: target.endpointPath,
          },
          'Fetching metrics from deployment (in-cluster)'
        );
        rawText = await this.fetchRawMetricsFn(url);
      } else {
        // Off-cluster: proxy through the K8s API server via kubeconfig
        const path = target.endpointPath.replace(/^\//, ''); // strip leading slash
        logger.debug(
          {
            deploymentName,
            namespace,
            providerId: target.providerId,
            serviceName: target.serviceName,
            port: target.port,
            path,
          },
          'Fetching metrics via K8s API proxy (off-cluster)'
        );
        rawText = await this.proxyServiceGetFn(target.serviceName, namespace, target.port, path);
      }

      // Parse Prometheus format
      const metrics = parsePrometheusText(rawText);

      logger.debug(
        { deploymentName, namespace, metricCount: metrics.length },
        'Successfully fetched and parsed metrics'
      );

      const response: MetricsResponse = {
        available: true,
        timestamp,
        metrics,
      };

      this.cacheResponse(cacheKey, response);
      return response;
    } catch (error) {
      const errorMessage = error instanceof Error ? error.message : 'Unknown error';
      const userMessage = mapMetricsErrorMessage(errorMessage);

      logger.warn(
        {
          deploymentName,
          namespace,
          providerId: target.providerId,
          serviceName: target.serviceName,
          port: target.port,
          path: target.endpointPath,
          error: errorMessage,
        },
        'Failed to fetch deployment metrics'
      );

      const response: MetricsResponse = {
        available: false,
        error: userMessage,
        timestamp,
        metrics: [],
      };

      this.cacheResponse(cacheKey, response);
      return response;
    }
  }

  /**
   * Get the key metrics definitions (common vLLM/inference metrics)
   */
  getKeyMetricsDefinitions() {
    return [
      { name: 'vllm:num_requests_running', type: 'gauge', description: 'Number of requests currently running' },
      { name: 'vllm:num_requests_waiting', type: 'gauge', description: 'Number of requests waiting in queue' },
      { name: 'vllm:gpu_cache_usage_perc', type: 'gauge', description: 'GPU KV cache usage percentage' },
      { name: 'vllm:cpu_cache_usage_perc', type: 'gauge', description: 'CPU KV cache usage percentage' },
      { name: 'vllm:e2e_request_latency_seconds', type: 'histogram', description: 'End-to-end request latency' },
      { name: 'vllm:time_to_first_token_seconds', type: 'histogram', description: 'Time to first token' },
      { name: 'vllm:time_per_output_token_seconds', type: 'histogram', description: 'Time per output token' },
    ];
  }

  /**
   * Extract key metrics from raw metrics based on definitions
   * This filters raw metrics to only include the ones defined as "key metrics"
   */
  extractKeyMetrics(rawMetrics: RawMetricValue[]): RawMetricValue[] {
    const definitions = this.getKeyMetricsDefinitions();
    const keyMetricNames = new Set(definitions.map(d => d.name));

    // For histograms, also include _sum and _count variants
    for (const def of definitions) {
      if (def.type === 'histogram') {
        keyMetricNames.add(`${def.name}_sum`);
        keyMetricNames.add(`${def.name}_count`);
        keyMetricNames.add(`${def.name}_bucket`);
      }
      // For counters, include _total variant if not already present
      if (def.type === 'counter' && !def.name.endsWith('_total')) {
        keyMetricNames.add(`${def.name}_total`);
      }
    }

    return rawMetrics.filter(m => keyMetricNames.has(m.name));
  }
}

// Export singleton instance
export const metricsService = new MetricsService();
