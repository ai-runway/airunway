import { describe, test, expect, mock } from 'bun:test';
import type { RawMetricValue, MetricsResponse } from '@airunway/shared';
import { buildMetricsUrl, mapMetricsErrorMessage, MetricsService } from './metrics';

describe('MetricsService - buildMetricsUrl', () => {
  test('builds correct URL with service pattern', () => {
    const url = buildMetricsUrl(
      'my-model',
      'default',
      '{name}-router',
      8000,
      '/metrics'
    );
    expect(url).toBe('http://my-model-router.default.svc.cluster.local:8000/metrics');
  });

  test('handles namespace with hyphens', () => {
    const url = buildMetricsUrl(
      'llama-model',
      'ml-workloads',
      '{name}-svc',
      9090,
      '/v1/metrics'
    );
    expect(url).toBe('http://llama-model-svc.ml-workloads.svc.cluster.local:9090/v1/metrics');
  });

  test('handles pattern without placeholder', () => {
    const url = buildMetricsUrl(
      'ignored',
      'monitoring',
      'prometheus',
      9090,
      '/metrics'
    );
    expect(url).toBe('http://prometheus.monitoring.svc.cluster.local:9090/metrics');
  });
});

describe('MetricsService - Error Message Handling', () => {
  test('maps DNS resolution errors', () => {
    expect(mapMetricsErrorMessage('getaddrinfo ENOTFOUND service.namespace.svc')).toContain('Cannot resolve service DNS');
    expect(mapMetricsErrorMessage('Error: ENOTFOUND')).toContain('not exist yet');
  });

  test('maps connection refused errors', () => {
    expect(mapMetricsErrorMessage('connect ECONNREFUSED 10.0.0.1:8000')).toContain('Cannot connect to the Kubernetes cluster');
    expect(mapMetricsErrorMessage('ECONNREFUSED')).toContain('not be ready');
  });

  test('maps timeout errors', () => {
    expect(mapMetricsErrorMessage('The operation was aborted')).toContain('timed out');
    expect(mapMetricsErrorMessage('signal was aborted')).toContain('heavy load');
  });

  test('maps HTTP 404 errors', () => {
    expect(mapMetricsErrorMessage('HTTP 404: Not Found')).toContain('endpoint not found');
    expect(mapMetricsErrorMessage('HTTP 404')).toContain('not expose metrics');
  });

  test('maps HTTP 503 errors', () => {
    expect(mapMetricsErrorMessage('HTTP 503: Service Unavailable')).toContain('Service unavailable');
    expect(mapMetricsErrorMessage('HTTP 503')).toContain('starting up');
  });

  test('maps fetch errors', () => {
    expect(mapMetricsErrorMessage('fetch failed')).toContain('Verify the deployment');
    expect(mapMetricsErrorMessage('TypeError: Failed to fetch')).toContain('Verify the deployment');
  });

  test('returns original message for unknown errors', () => {
    expect(mapMetricsErrorMessage('Some unknown error')).toBe('Some unknown error');
    expect(mapMetricsErrorMessage('Unexpected condition')).toBe('Unexpected condition');
  });
});

describe('MetricsService - Caching', () => {
  test('reuses cached successful responses within the cache window', async () => {
    let now = 1700000000000;
    const fetchRawMetrics = mock(async () => 'vllm:num_requests_running 5\n');
    const service = new MetricsService({
      fetchRawMetrics,
      checkInCluster: () => true,
      now: () => now,
      successCacheTtlMs: 15000,
      errorCacheTtlMs: 5000,
    });

    const first = await service.getDeploymentMetrics('demo', 'default');
    now += 1000;
    const second = await service.getDeploymentMetrics('demo', 'default');

    expect(fetchRawMetrics).toHaveBeenCalledTimes(1);
    expect(second).toEqual(first);
  });

  test('deduplicates concurrent requests for the same deployment', async () => {
    let resolveFetch: ((value: string) => void) | undefined;
    const fetchRawMetrics = mock(() =>
      new Promise<string>((resolve) => {
        resolveFetch = resolve;
      })
    );

    const service = new MetricsService({
      fetchRawMetrics,
      checkInCluster: () => true,
      now: () => 1700000000000,
      successCacheTtlMs: 15000,
      errorCacheTtlMs: 5000,
    });

    const firstPromise = service.getDeploymentMetrics('demo', 'default');
    const secondPromise = service.getDeploymentMetrics('demo', 'default');

    expect(fetchRawMetrics).toHaveBeenCalledTimes(1);
    expect(resolveFetch).toBeDefined();

    resolveFetch?.('vllm:num_requests_running 5\n');

    const [first, second] = await Promise.all([firstPromise, secondPromise]);
    expect(second).toEqual(first);
  });

  test('briefly caches failures to avoid hammering unhealthy endpoints', async () => {
    let now = 1700000000000;
    const fetchRawMetrics = mock(async () => {
      throw new Error('HTTP 503: Service Unavailable');
    });

    const service = new MetricsService({
      fetchRawMetrics,
      checkInCluster: () => true,
      now: () => now,
      successCacheTtlMs: 15000,
      errorCacheTtlMs: 5000,
    });

    const first = await service.getDeploymentMetrics('demo', 'default');
    now += 1000;
    const second = await service.getDeploymentMetrics('demo', 'default');
    now += 5000;
    const third = await service.getDeploymentMetrics('demo', 'default');

    expect(fetchRawMetrics).toHaveBeenCalledTimes(2);
    expect(first.error).toContain('Service unavailable');
    expect(second).toEqual(first);
    expect(third.timestamp).not.toBe(first.timestamp);
  });
});

describe('MetricsService - Endpoint Resolution', () => {
  test('uses frontend service override for in-cluster scraping', async () => {
    const urls: string[] = [];
    const fetchRawMetrics = mock(async (url: string) => {
      urls.push(url);
      return 'vllm:num_requests_running 5\n';
    });

    const service = new MetricsService({
      fetchRawMetrics,
      checkInCluster: () => true,
      now: () => 1700000000000,
      successCacheTtlMs: 0,
      errorCacheTtlMs: 0,
    });

    const response = await service.getDeploymentMetrics('demo', 'default', {
      providerId: 'dynamo',
      serviceName: 'demo-frontend',
      port: 8000,
    });

    expect(response.available).toBe(true);
    expect(urls).toEqual(['http://demo-frontend.default.svc.cluster.local:8000/metrics']);
  });

  test('uses resolved service and port for off-cluster proxy scraping', async () => {
    const calls: Array<[string, string, number, string]> = [];
    const proxyServiceGet = mock(async (serviceName: string, namespace: string, port: number, path: string) => {
      calls.push([serviceName, namespace, port, path]);
      return 'vllm:num_requests_running 5\n';
    });

    const service = new MetricsService({
      proxyServiceGet,
      checkInCluster: () => false,
      now: () => 1700000000000,
      successCacheTtlMs: 0,
      errorCacheTtlMs: 0,
    });

    const response = await service.getDeploymentMetrics('demo', 'models', {
      serviceName: 'custom-svc',
      port: 9000,
    });

    expect(response.available).toBe(true);
    expect(calls).toEqual([['custom-svc', 'models', 9000, 'metrics']]);
  });

  test('keeps default deployment-name service behavior when no endpoint override is supplied', async () => {
    const urls: string[] = [];
    const fetchRawMetrics = mock(async (url: string) => {
      urls.push(url);
      return 'vllm:num_requests_running 5\n';
    });

    const service = new MetricsService({
      fetchRawMetrics,
      checkInCluster: () => true,
      now: () => 1700000000000,
      successCacheTtlMs: 0,
      errorCacheTtlMs: 0,
    });

    const response = await service.getDeploymentMetrics('demo', 'default');

    expect(response.available).toBe(true);
    expect(urls).toEqual(['http://demo.default.svc.cluster.local:8000/metrics']);
  });

  test('cache keys differ when service or port differs for the same deployment', async () => {
    let now = 1700000000000;
    const fetchRawMetrics = mock(async () => 'vllm:num_requests_running 5\n');
    const service = new MetricsService({
      fetchRawMetrics,
      checkInCluster: () => true,
      now: () => now,
      successCacheTtlMs: 15000,
      errorCacheTtlMs: 5000,
    });

    await service.getDeploymentMetrics('demo', 'default', { serviceName: 'demo-a', port: 8000 });
    now += 1000;
    await service.getDeploymentMetrics('demo', 'default', { serviceName: 'demo-b', port: 8000 });
    now += 1000;
    await service.getDeploymentMetrics('demo', 'default', { serviceName: 'demo-a', port: 8000 });

    expect(fetchRawMetrics).toHaveBeenCalledTimes(2);
  });

  test('uses Dynamo frontend service fallback when provider is dynamo and no endpoint override is supplied', async () => {
    const urls: string[] = [];
    const fetchRawMetrics = mock(async (url: string) => {
      urls.push(url);
      return 'vllm:num_requests_running 5\n';
    });

    const service = new MetricsService({
      fetchRawMetrics,
      checkInCluster: () => true,
      now: () => 1700000000000,
      successCacheTtlMs: 0,
      errorCacheTtlMs: 0,
    });

    const response = await service.getDeploymentMetrics('demo', 'default', 'dynamo');

    expect(response.available).toBe(true);
    expect(urls).toEqual(['http://demo-frontend.default.svc.cluster.local:8000/metrics']);
  });
});

describe('MetricsService - MetricsResponse structure', () => {
  test('creates unavailable response for off-cluster', () => {
    const response: MetricsResponse = {
      available: false,
      error: 'Metrics are only available when AI Runway is deployed inside the Kubernetes cluster.',
      timestamp: new Date().toISOString(),
      metrics: [],
      runningOffCluster: true,
    };

    expect(response.available).toBe(false);
    expect(response.runningOffCluster).toBe(true);
    expect(response.metrics).toHaveLength(0);
    expect(response.error).toContain('inside the Kubernetes cluster');
  });

  test('creates available response with metrics', () => {
    const metrics: RawMetricValue[] = [
      { name: 'vllm:num_requests_running', value: 5, labels: {} },
      { name: 'vllm:gpu_cache_usage_perc', value: 0.73, labels: { model: 'llama' } },
    ];

    const response: MetricsResponse = {
      available: true,
      timestamp: new Date().toISOString(),
      metrics,
    };

    expect(response.available).toBe(true);
    expect(response.error).toBeUndefined();
    expect(response.metrics).toHaveLength(2);
    expect(response.metrics[0].name).toBe('vllm:num_requests_running');
  });

  test('creates error response', () => {
    const response: MetricsResponse = {
      available: false,
      error: 'Connection refused. The deployment may not be ready yet.',
      timestamp: new Date().toISOString(),
      metrics: [],
    };

    expect(response.available).toBe(false);
    expect(response.error).toContain('Connection refused');
    expect(response.runningOffCluster).toBeUndefined();
  });
});

describe('MetricsService - Key Metrics Filtering', () => {
  // Test the logic for filtering key metrics from raw metrics
  function extractKeyMetrics(
    rawMetrics: RawMetricValue[],
    keyMetricNames: Set<string>
  ): RawMetricValue[] {
    // Expand key metric names to include histogram variants
    const expandedNames = new Set(keyMetricNames);
    for (const name of keyMetricNames) {
      expandedNames.add(`${name}_sum`);
      expandedNames.add(`${name}_count`);
      expandedNames.add(`${name}_bucket`);
      expandedNames.add(`${name}_total`);
    }

    return rawMetrics.filter(m => expandedNames.has(m.name));
  }

  test('filters to only key metrics', () => {
    const rawMetrics: RawMetricValue[] = [
      { name: 'vllm:num_requests_running', value: 5, labels: {} },
      { name: 'vllm:gpu_cache_usage_perc', value: 0.73, labels: {} },
      { name: 'unrelated_metric', value: 100, labels: {} },
      { name: 'another_random_metric', value: 42, labels: {} },
    ];

    const keyNames = new Set(['vllm:num_requests_running', 'vllm:gpu_cache_usage_perc']);
    const filtered = extractKeyMetrics(rawMetrics, keyNames);

    expect(filtered).toHaveLength(2);
    expect(filtered.map(m => m.name)).toContain('vllm:num_requests_running');
    expect(filtered.map(m => m.name)).toContain('vllm:gpu_cache_usage_perc');
  });

  test('includes histogram variants (_sum, _count, _bucket)', () => {
    const rawMetrics: RawMetricValue[] = [
      { name: 'latency_seconds', value: 5, labels: {} },
      { name: 'latency_seconds_sum', value: 500, labels: {} },
      { name: 'latency_seconds_count', value: 100, labels: {} },
      { name: 'latency_seconds_bucket', value: 50, labels: { le: '0.5' } },
      { name: 'other_metric', value: 42, labels: {} },
    ];

    const keyNames = new Set(['latency_seconds']);
    const filtered = extractKeyMetrics(rawMetrics, keyNames);

    expect(filtered).toHaveLength(4);
    expect(filtered.map(m => m.name)).not.toContain('other_metric');
  });

  test('includes counter variants (_total)', () => {
    const rawMetrics: RawMetricValue[] = [
      { name: 'requests', value: 100, labels: {} },
      { name: 'requests_total', value: 100, labels: {} },
      { name: 'other_total', value: 50, labels: {} },
    ];

    const keyNames = new Set(['requests']);
    const filtered = extractKeyMetrics(rawMetrics, keyNames);

    expect(filtered).toHaveLength(2);
    expect(filtered.map(m => m.name)).toContain('requests');
    expect(filtered.map(m => m.name)).toContain('requests_total');
  });

  test('returns empty array when no matches', () => {
    const rawMetrics: RawMetricValue[] = [
      { name: 'metric_a', value: 1, labels: {} },
      { name: 'metric_b', value: 2, labels: {} },
    ];

    const keyNames = new Set(['metric_c', 'metric_d']);
    const filtered = extractKeyMetrics(rawMetrics, keyNames);

    expect(filtered).toHaveLength(0);
  });
});
