import type {
  AIConfiguratorInput,
  AIConfiguratorResult,
  AIConfiguratorStatus,
  AIConfiguratorConfig,
} from '@airunway/shared';
import logger from '../lib/logger';
import * as fs from 'fs';

// Kubernetes service account token path (exists only when running in-cluster)
const K8S_SERVICE_ACCOUNT_TOKEN_PATH = '/var/run/secrets/kubernetes.io/serviceaccount/token';

/**
 * Check if AI Runway is running inside a Kubernetes cluster
 * AI Configurator is only available when running locally, not in-cluster
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
  }
  return _isInCluster;
}

// Supported GPU systems in aiconfigurator
type GpuSystem = 'h100_sxm' | 'h200_sxm' | 'a100_sxm' | 'l40s' | 'b200_sxm' | 'gb200_sxm';
type Backend = 'vllm' | 'sglang' | 'trtllm';

// Map of GPU types to aiconfigurator system names
const GPU_SYSTEM_MAP: Record<string, GpuSystem> = {
  'h100': 'h100_sxm',
  'h100-80gb': 'h100_sxm',
  'h100-sxm': 'h100_sxm',
  'h200': 'h200_sxm',
  'h200-80gb': 'h200_sxm',
  'a100': 'a100_sxm',
  'a100-80gb': 'a100_sxm',
  'a100-40gb': 'a100_sxm',
  'a100-sxm': 'a100_sxm',
  'b200': 'b200_sxm',
  'gb200': 'gb200_sxm',
  'l40s': 'l40s',
  'l40': 'l40s',
};

// Supported backend combinations per GPU system
// Based on aiconfigurator's performance database availability
const SUPPORTED_BACKENDS: Record<GpuSystem, Backend[]> = {
  'h100_sxm': ['vllm', 'sglang', 'trtllm'],
  'h200_sxm': ['trtllm'],  // vllm data not available
  'a100_sxm': ['trtllm'],  // vllm data not available
  'l40s': ['trtllm'],      // vllm data not available
  'b200_sxm': ['trtllm'],  // vllm data not available
  'gb200_sxm': ['trtllm'], // vllm data not available
};

// Default fallback system when GPU type is unknown
const DEFAULT_GPU_SYSTEM: GpuSystem = 'h100_sxm';

// Cache TTL for status check (5 minutes)
const STATUS_CACHE_TTL_MS = 5 * 60 * 1000;

/**
 * AI Configurator Service
 *
 * Interfaces with NVIDIA AI Configurator CLI to get optimal inference configurations.
 * AI Configurator must be installed locally and available in PATH.
 *
 * @see https://docs.nvidia.com/dynamo/latest/performance/aiconfigurator.html
 * @see https://github.com/ai-dynamo/aiconfigurator
 */
class AIConfiguratorService {
  private readonly CLI_COMMAND = 'aiconfigurator';

  // Cached status to avoid repeated CLI calls
  private cachedStatus: AIConfiguratorStatus | null = null;
  private statusCacheTime: number = 0;

  /**
   * Check if AI Configurator is available on the system
   * Results are cached for 5 minutes to avoid repeated CLI calls
   * @param forceRefresh - If true, bypasses the cache and checks again
   */
  async checkStatus(forceRefresh = false): Promise<AIConfiguratorStatus> {
    // If running in-cluster, AI Configurator is not applicable
    if (checkInCluster()) {
      return {
        available: false,
        runningInCluster: true,
        error: 'AI Configurator is only available when running AI Runway locally',
      };
    }

    // Return cached status if still valid
    const now = Date.now();
    if (!forceRefresh && this.cachedStatus && (now - this.statusCacheTime) < STATUS_CACHE_TTL_MS) {
      return this.cachedStatus;
    }

    try {
      // aiconfigurator uses 'version' subcommand, not --version
      const proc = Bun.spawn([this.CLI_COMMAND, 'version'], {
        stdout: 'pipe',
        stderr: 'pipe',
      });

      const [exitCode, stdout, stderr] = await Promise.all([
        proc.exited,
        new Response(proc.stdout).text(),
        new Response(proc.stderr).text(),
      ]);

      if (exitCode === 0) {
        // Parse version from output like "aiconfigurator 0.4.0"
        const versionMatch = stdout.match(/aiconfigurator\s+([\d.]+)/i);
        const version = versionMatch ? versionMatch[1] : stdout.trim();
        logger.info({ version }, 'AI Configurator is available');
        const status: AIConfiguratorStatus = {
          available: true,
          version: version || 'unknown',
        };
        // Cache the result
        this.cachedStatus = status;
        this.statusCacheTime = now;
        return status;
      }

      const status: AIConfiguratorStatus = {
        available: false,
        error: `AI Configurator exited with code ${exitCode}: ${stderr}`,
      };
      // Cache negative result too (but it will be refreshed on next call after TTL)
      this.cachedStatus = status;
      this.statusCacheTime = now;
      return status;
    } catch (error) {
      const message = error instanceof Error ? error.message : 'Unknown error';
      logger.debug({ error: message }, 'AI Configurator not available');
      const status: AIConfiguratorStatus = {
        available: false,
        error: `AI Configurator CLI not found`,
      };
      // Cache negative result with shorter TTL (1 minute) so we retry sooner
      this.cachedStatus = status;
      this.statusCacheTime = now - (STATUS_CACHE_TTL_MS - 60 * 1000);
      return status;
    }
  }

  /**
   * Invalidate the cached status, forcing next check to query CLI
   */
  invalidateStatusCache(): void {
    this.cachedStatus = null;
    this.statusCacheTime = 0;
  }

  /**
   * Run AI Configurator to analyze a model + GPU combination
   * Returns optimal configuration recommendations
   */
  async analyze(input: AIConfiguratorInput): Promise<AIConfiguratorResult> {
    logger.info({ input }, 'Running AI Configurator analysis');

    // Validate modelId to prevent command injection
    // HuggingFace model IDs follow pattern: org/model-name or just model-name
    const modelIdPattern = /^[a-zA-Z0-9_-]+(\/[a-zA-Z0-9._-]+)?$/;
    if (!modelIdPattern.test(input.modelId)) {
      return {
        success: false,
        config: this.getDefaultConfig(input),
        mode: 'aggregated',
        replicas: 1,
        error: 'Invalid model ID format. Expected format: org/model-name or model-name',
        warnings: ['Using default configuration due to invalid model ID'],
      };
    }

    // First check if available
    const status = await this.checkStatus();
    if (!status.available) {
      return {
        success: false,
        config: this.getDefaultConfig(input),
        mode: 'aggregated',
        replicas: 1,
        error: status.error,
        warnings: ['AI Configurator not available, using default configuration'],
      };
    }

    // Create temp directory for output - aiconfigurator requires a directory in home or cwd
    const homeDir = process.env.HOME || process.env.USERPROFILE || '.';
    const tempDir = `${homeDir}/.airunway/aiconfigurator-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;

    try {
      await Bun.$`mkdir -p ${tempDir}`;

      // Build command arguments for 'cli default' subcommand
      const { args, system, backend } = this.buildCommandArgs(input, tempDir);

      logger.debug({ command: this.CLI_COMMAND, args, system, backend }, 'Executing AI Configurator');

      const proc = Bun.spawn([this.CLI_COMMAND, ...args], {
        stdout: 'pipe',
        stderr: 'pipe',
        env: {
          ...process.env,
        },
      });

      const [exitCode, stdout, stderr] = await Promise.all([
        proc.exited,
        new Response(proc.stdout).text(),
        new Response(proc.stderr).text(),
      ]);

      if (exitCode !== 0) {
        logger.warn({ exitCode, stderr, stdout }, 'AI Configurator failed');
        return {
          success: false,
          config: this.getDefaultConfig(input),
          mode: 'aggregated',
          replicas: 1,
          error: this.extractErrorMessage(stderr, stdout),
          warnings: [`Using default configuration. Note: ${system} only supports ${SUPPORTED_BACKENDS[system].join(', ')} backend(s).`],
        };
      }

      // Parse output from saved files
      const result = await this.parseOutputFiles(tempDir, input);

      // Add backend info to result
      result.backend = backend;
      result.supportedBackends = SUPPORTED_BACKENDS[system];

      // Add backend info to warnings if not using vllm
      if (backend !== 'vllm' && result.warnings) {
        result.warnings.unshift(`Using ${backend.toUpperCase()} backend (vLLM not available for ${input.gpuType})`);
      }

      logger.info({ result }, 'AI Configurator analysis complete');
      return result;

    } catch (error) {
      const message = error instanceof Error ? error.message : 'Unknown error';
      logger.error({ error: message }, 'AI Configurator execution failed');
      return {
        success: false,
        config: this.getDefaultConfig(input),
        mode: 'aggregated',
        replicas: 1,
        error: message,
        warnings: ['Using default configuration due to execution error'],
      };
    } finally {
      // Always clean up temp directory
      try {
        await Bun.$`rm -rf ${tempDir}`.quiet();
      } catch {
        // Ignore cleanup errors
      }
    }
  }

  /**
   * Build command line arguments for AI Configurator
   * Uses 'cli default' subcommand with --model and --system flags
   */
  private buildCommandArgs(input: AIConfiguratorInput, saveDir: string): { args: string[]; system: GpuSystem; backend: Backend } {
    // Map GPU type to aiconfigurator system name
    const systemName = this.mapGpuToSystem(input.gpuType);

    // Get the best supported backend for this GPU
    const backend = this.getBestBackend(systemName);

    // Ensure at least 2 GPUs for disaggregated mode comparison
    const gpuCount = Math.max(input.gpuCount, 2);

    const args: string[] = [
      'cli', 'default',
      '--model', input.modelId,
      '--total-gpus', String(gpuCount),
      '--system', systemName,
      '--backend', backend,
      '--save-dir', saveDir,
    ];

    // Add latency constraints if specified
    if (input.maxLatencyMs) {
      // ttft = time to first token in ms
      args.push('--ttft', String(input.maxLatencyMs));
    }

    return { args, system: systemName, backend };
  }

  /**
   * Map GPU type string to aiconfigurator system name
   */
  private mapGpuToSystem(gpuType: string): GpuSystem {
    const normalized = gpuType.toLowerCase().replace(/[-_\s]+/g, '-');

    // Check direct matches first
    for (const [key, value] of Object.entries(GPU_SYSTEM_MAP)) {
      if (normalized.includes(key)) {
        return value;
      }
    }

    // Default to h100_sxm for unknown GPUs
    logger.warn({ gpuType }, 'Unknown GPU type, defaulting to h100_sxm');
    return DEFAULT_GPU_SYSTEM;
  }

  /**
   * Get the best supported backend for a GPU system
   * Prefers vllm > sglang > trtllm
   */
  private getBestBackend(system: GpuSystem): Backend {
    const supported = SUPPORTED_BACKENDS[system] || ['trtllm'];
    if (supported.includes('vllm')) return 'vllm';
    if (supported.includes('sglang')) return 'sglang';
    return 'trtllm';
  }

  /**
   * Extract a user-friendly error message from stderr/stdout
   */
  private extractErrorMessage(stderr: string, stdout: string): string {
    // Look for HuggingFace auth errors first (gated models)
    if (stderr.includes('401: Unauthorized') || stderr.includes('gated model')) {
      return 'This is a gated model. Please authenticate with HuggingFace first (huggingface-cli login) or use a non-gated model.';
    }

    // Look for "no successful experiments" - usually means model too large for GPU count
    if (stderr.includes('No successful experiment runs to compare')) {
      return 'Model may be too large for available GPUs, or latency constraints cannot be met. Try with more GPUs or a smaller model.';
    }

    // Look for TTFT/TPOT constraint warnings
    if (stderr.includes('TTFT and TPOT constraints may need to be relaxed')) {
      return 'Latency constraints cannot be met with current configuration. Model may need more GPUs.';
    }

    // Look for common error patterns
    const valueErrorMatch = stderr.match(/ValueError:\s*(.+)/);
    if (valueErrorMatch) {
      return valueErrorMatch[1];
    }

    // Look for aiconfigurator-specific error patterns
    const argErrorMatch = stderr.match(/aiconfigurator.*error:\s*(.+)/i);
    if (argErrorMatch) {
      return argErrorMatch[1];
    }

    const errorMatch = stderr.match(/Error:\s*(.+)/i);
    if (errorMatch) {
      return errorMatch[1];
    }

    // Look for TypeError (common Python errors)
    const typeErrorMatch = stderr.match(/TypeError:\s*(.+)/);
    if (typeErrorMatch) {
      return typeErrorMatch[1];
    }

    // Return last non-empty line from stderr
    const lines = stderr.split('\n').filter(l => l.trim() && !l.includes('WARNING'));
    if (lines.length > 0) {
      return lines[lines.length - 1];
    }

    return 'AI Configurator failed with unknown error';
  }

  /**
   * Parse AI Configurator output files (CSV format)
   */
  private async parseOutputFiles(saveDir: string, input: AIConfiguratorInput): Promise<AIConfiguratorResult> {
    try {
      // Find the output directory (it contains the model name in the path)
      const findResult = await Bun.$`find ${saveDir} -name "best_config_topn.csv" -type f 2>/dev/null`.text();
      const csvFiles = findResult.trim().split('\n').filter(f => f);

      if (csvFiles.length === 0) {
        logger.warn({ saveDir }, 'No CSV output files found');
        return {
          success: false,
          config: this.getDefaultConfig(input),
          mode: 'aggregated',
          replicas: 1,
          error: 'AI Configurator did not produce output files',
          warnings: ['Using default configuration'],
        };
      }

      // Find agg and disagg CSV files
      const aggCsvPath = csvFiles.find(f => f.includes('/agg/'));
      const disaggCsvPath = csvFiles.find(f => f.includes('/disagg/'));

      // Parse aggregated config (preferred)
      let aggConfig: Record<string, string> | null = null;
      let disaggConfig: Record<string, string> | null = null;

      if (aggCsvPath) {
        aggConfig = await this.parseCsvTopConfig(aggCsvPath);
      }
      if (disaggCsvPath) {
        disaggConfig = await this.parseCsvTopConfig(disaggCsvPath);
      }

      // Determine best mode based on optimization target
      const optimizeFor = input.optimizeFor || 'throughput';

      let useDisagg: boolean;
      if (optimizeFor === 'latency') {
        // For latency optimization, compare TTFT (time to first token)
        const aggLatency = aggConfig ? parseFloat(aggConfig['ttft'] || '999999') : 999999;
        const disaggLatency = disaggConfig ? parseFloat(disaggConfig['ttft'] || '999999') : 999999;
        useDisagg = disaggLatency < aggLatency;
      } else {
        // For throughput optimization, compare tokens/s/gpu_cluster
        const aggThroughput = aggConfig ? parseFloat(aggConfig['tokens/s/gpu_cluster'] || aggConfig['tokens/s/gpu'] || '0') : 0;
        const disaggThroughput = disaggConfig ? parseFloat(disaggConfig['tokens/s/gpu_cluster'] || disaggConfig['tokens/s/gpu'] || '0') : 0;
        useDisagg = disaggThroughput > aggThroughput;
      }

      const bestConfig = useDisagg && disaggConfig ? disaggConfig : aggConfig;

      if (!bestConfig) {
        return {
          success: false,
          config: this.getDefaultConfig(input),
          mode: 'aggregated',
          replicas: 1,
          error: 'Failed to parse AI Configurator output',
          warnings: ['Using default configuration'],
        };
      }

      // Convert CSV row to our config format
      const config: AIConfiguratorConfig = {
        tensorParallelDegree: parseInt(bestConfig['tp'] || '1', 10),
        pipelineParallelDegree: parseInt(bestConfig['pp'] || '1', 10),
        maxBatchSize: parseInt(bestConfig['bs'] || '256', 10),
        maxNumSeqs: parseInt(bestConfig['concurrency']?.split('(')[0]?.trim() || '256', 10),
        gpuMemoryUtilization: 0.8, // Default, from free_gpu_memory_fraction
        maxModelLen: parseInt(bestConfig['isl'] || '4096', 10) + parseInt(bestConfig['osl'] || '1000', 10),
      };

      const result: AIConfiguratorResult = {
        success: true,
        config,
        mode: useDisagg ? 'disaggregated' : 'aggregated',
        replicas: parseInt(bestConfig['replicas'] || '1', 10),
        warnings: [],
        estimatedPerformance: {
          throughputTokensPerSec: parseFloat(bestConfig['tokens/s'] || bestConfig['tokens/s/gpu'] || '0'),
          latencyP50Ms: parseFloat(bestConfig['ttft'] || '0'),
          latencyP99Ms: parseFloat(bestConfig['ttft'] || '0') * 1.5, // Estimate P99
          gpuUtilization: 0.8,
        },
      };

      // Add disaggregated mode details if applicable
      if (useDisagg && disaggConfig) {
        // Parse disagg-specific fields from parallel column like "tp1pp1"
        const prefillTp = this.extractTpFromParallel(disaggConfig['(p)parallel'] || '');
        const decodeTp = this.extractTpFromParallel(disaggConfig['(d)parallel'] || '');

        config.prefillTensorParallel = prefillTp || config.tensorParallelDegree;
        config.decodeTensorParallel = decodeTp || config.tensorParallelDegree;
        config.prefillReplicas = parseInt(disaggConfig['(p)workers'] || '1', 10);
        config.decodeReplicas = parseInt(disaggConfig['(d)workers'] || '1', 10);
      }

      return result;

    } catch (error) {
      const message = error instanceof Error ? error.message : 'Unknown error';
      logger.error({ error: message }, 'Failed to parse AI Configurator output files');
      return {
        success: false,
        config: this.getDefaultConfig(input),
        mode: 'aggregated',
        replicas: 1,
        error: message,
        warnings: ['Using default configuration'],
      };
    }
  }

  /**
   * Parse the first data row from a CSV file
   * Handles quoted values that may contain commas
   */
  private async parseCsvTopConfig(csvPath: string): Promise<Record<string, string> | null> {
    try {
      const content = await Bun.file(csvPath).text();
      const lines = content.trim().split('\n');

      if (lines.length < 2) {
        return null;
      }

      const headers = this.parseCsvLine(lines[0]);
      const values = this.parseCsvLine(lines[1]);

      const result: Record<string, string> = {};
      for (let i = 0; i < headers.length && i < values.length; i++) {
        result[headers[i].trim()] = values[i].trim();
      }

      return result;
    } catch {
      return null;
    }
  }

  /**
   * Parse a single CSV line, handling quoted values with commas
   */
  private parseCsvLine(line: string): string[] {
    const result: string[] = [];
    let current = '';
    let inQuotes = false;

    for (let i = 0; i < line.length; i++) {
      const char = line[i];

      if (char === '"') {
        inQuotes = !inQuotes;
      } else if (char === ',' && !inQuotes) {
        result.push(current);
        current = '';
      } else {
        current += char;
      }
    }

    // Don't forget the last field
    result.push(current);

    return result;
  }

  /**
   * Extract tensor parallel degree from parallel string like "tp1pp1" or "tp2pp1dp1etp1ep1"
   */
  private extractTpFromParallel(parallel: string): number {
    const match = parallel.match(/tp(\d+)/i);
    return match ? parseInt(match[1], 10) : 1;
  }

  /**
   * Get default configuration when AI Configurator is unavailable
   * Uses simple heuristics based on GPU count
   */
  private getDefaultConfig(input: AIConfiguratorInput): AIConfiguratorConfig {
    // Simple heuristics for defaults
    const tensorParallel = Math.min(input.gpuCount, 8);

    return {
      tensorParallelDegree: tensorParallel,
      maxBatchSize: 256,
      gpuMemoryUtilization: 0.9,
      maxModelLen: 4096,
    };
  }

  /**
   * Get GPU type string from node labels
   * Normalizes different label formats to AI Configurator expected format
   */
  normalizeGpuType(gpuProduct: string): string {
    // Common GPU product label values and their normalized names
    const normalizations: Record<string, string> = {
      'nvidia-a100-80gb': 'A100-80GB',
      'nvidia-a100-40gb': 'A100-40GB',
      'nvidia-a100-sxm4-80gb': 'A100-80GB',
      'nvidia-a100-sxm4-40gb': 'A100-40GB',
      'nvidia-a100-pcie-80gb': 'A100-80GB',
      'nvidia-a100-pcie-40gb': 'A100-40GB',
      'nvidia-h100-80gb': 'H100-80GB',
      'nvidia-h100-sxm5-80gb': 'H100-80GB',
      'nvidia-h100-pcie-80gb': 'H100-80GB',
      'nvidia-l40s': 'L40S',
      'nvidia-l40': 'L40',
      'nvidia-l4': 'L4',
      'nvidia-t4': 'T4',
      'nvidia-v100': 'V100',
      'nvidia-v100-32gb': 'V100-32GB',
      'nvidia-v100-16gb': 'V100-16GB',
      'tesla-t4': 'T4',
      'tesla-v100': 'V100',
    };

    const lowerProduct = gpuProduct.toLowerCase().replace(/\s+/g, '-');

    // Check exact match first
    if (normalizations[lowerProduct]) {
      return normalizations[lowerProduct];
    }

    // Try partial matches
    for (const [key, value] of Object.entries(normalizations)) {
      if (lowerProduct.includes(key.replace('nvidia-', ''))) {
        return value;
      }
    }

    // If no match, try to clean up the string
    // Remove "nvidia-" prefix and convert to uppercase
    let cleaned = gpuProduct
      .replace(/^nvidia[-\s]*/i, '')
      .replace(/[-\s]+/g, '-')
      .toUpperCase();

    // Common patterns: add memory suffix if detected
    const memoryMatch = cleaned.match(/(\d+)\s*GB/i);
    if (memoryMatch && !cleaned.includes('GB')) {
      cleaned = cleaned.replace(memoryMatch[0], '') + `-${memoryMatch[1]}GB`;
    }

    return cleaned || gpuProduct;
  }
}

// Export singleton instance
export const aiConfiguratorService = new AIConfiguratorService();
