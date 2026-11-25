import { describe, it, expect } from 'vitest'
import type { MetricsResponse, RawMetricValue } from '@airunway/shared'
import { computeMetrics } from './useMetrics'

const labels = { engine: '0', model_name: 'Qwen/Qwen3-0.6B' }

function response(metrics: RawMetricValue[]): MetricsResponse {
  return { available: true, timestamp: '2026-11-25T08:00:00.000Z', metrics }
}

function byName(metrics: RawMetricValue[]) {
  const computed = computeMetrics(response(metrics), 'dynamo').metrics
  return Object.fromEntries(computed.map((m) => [m.displayName, m]))
}

describe('computeMetrics with vLLM metrics', () => {
  it('reads the names current vLLM exports (V1 engine, 0.12 and later)', () => {
    const shown = byName([
      { name: 'vllm:num_requests_running', value: 3, labels },
      { name: 'vllm:num_requests_waiting', value: 12, labels },
      { name: 'vllm:kv_cache_usage_perc', value: 0.75, labels },
      { name: 'vllm:prefix_cache_queries_total', value: 400, labels },
      { name: 'vllm:prefix_cache_hits_total', value: 100, labels },
      { name: 'vllm:inter_token_latency_seconds_sum', value: 2, labels },
      { name: 'vllm:inter_token_latency_seconds_count', value: 100, labels },
    ])

    expect(shown['GPU Cache Usage'].formattedValue).toBe('75.0%')
    expect(shown['Prefix Cache Hit Rate'].formattedValue).toBe('25.0%')
    expect(shown['Time per Token'].formattedValue).toBe('20.0ms')
    expect(shown['Waiting Requests'].value).toBe(12)
  })

  it('still reads the old names from older vLLM versions', () => {
    const shown = byName([
      { name: 'vllm:gpu_cache_usage_perc', value: 0.5, labels },
      { name: 'vllm:gpu_prefix_cache_hit_rate', value: 0.4, labels },
      { name: 'vllm:time_per_output_token_seconds_sum', value: 3, labels },
      { name: 'vllm:time_per_output_token_seconds_count', value: 100, labels },
    ])

    expect(shown['GPU Cache Usage'].formattedValue).toBe('50.0%')
    expect(shown['Prefix Cache Hit Rate'].formattedValue).toBe('40.0%')
    expect(shown['Time per Token'].formattedValue).toBe('30.0ms')
  })

  it('prefers the current name when both are exported, without adding them up', () => {
    // vLLM 0.11 with --show-hidden-metrics-for-version=0.10 exports both cache gauges
    const shown = byName([
      { name: 'vllm:kv_cache_usage_perc', value: 0.6, labels },
      { name: 'vllm:gpu_cache_usage_perc', value: 0.6, labels },
    ])

    expect(shown['GPU Cache Usage'].formattedValue).toBe('60.0%')
  })

  it('leaves out the prefix cache hit rate before any prefix lookups', () => {
    const shown = byName([
      { name: 'vllm:prefix_cache_queries_total', value: 0, labels },
      { name: 'vllm:prefix_cache_hits_total', value: 0, labels },
    ])

    expect(shown['Prefix Cache Hit Rate']).toBeUndefined()
  })
})
