import { spawn } from 'child_process';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'fs';
import { loadAll, dump } from 'js-yaml';
import { tmpdir } from 'os';
import { join } from 'path';
import logger from '../lib/logger';

/**
 * Helm repository configuration
 */
export interface HelmRepo {
  name: string;
  url: string;
}

/**
 * Helm chart configuration for installation
 */
export interface HelmChart {
  name: string;
  chart: string;
  namespace: string;
  version?: string;
  createNamespace?: boolean;
  values?: Record<string, unknown>;
  skipCrds?: boolean;
  fetchUrl?: string;
  preCrdUrls?: string[];
  preInstallMissingCrds?: boolean;
  keepCrdResources?: boolean;
}

interface ChartCrdDocument {
  name: string;
  manifest: string;
}

/**
 * Convert a values object to --set-json arguments
 * Helm's --set-json expects format: key=jsonvalue (e.g., --set-json 'featureGates={"enabled":true}')
 * NOT a single JSON object like: --set-json '{"featureGates":{"enabled":true}}'
 */
function valuesToSetJsonArgs(values: Record<string, unknown>): string[] {
  const args: string[] = [];
  for (const [key, value] of Object.entries(values)) {
    args.push('--set-json', `${key}=${JSON.stringify(value)}`);
  }
  return args;
}

// POSIX single-quote escaping: foo'bar -> 'foo'"'"'bar'.
function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\"'\"'`)}'`;
}

const KEEP_CRD_POST_RENDERER_SOURCE = String.raw`const fs = require('fs');
const input = fs.readFileSync(0, 'utf8');

function keepCrdResource(document) {
  const lines = document.split(/\r?\n/);
  const kindIndex = lines.findIndex((line) => /^kind:\s*CustomResourceDefinition\s*$/.test(line));
  if (kindIndex < 0) return document;

  const metadataIndex = lines.findIndex((line, index) => index > kindIndex && /^metadata:\s*$/.test(line));
  if (metadataIndex < 0) return document;

  const metadataEnd = lines.findIndex((line, index) => index > metadataIndex && /^[^\s]/.test(line));
  const end = metadataEnd < 0 ? lines.length : metadataEnd;
  const policyIndex = lines.findIndex((line, index) => (
    index > metadataIndex
    && index < end
    && /^\s+helm\.sh\/resource-policy\s*:/.test(line)
  ));
  if (policyIndex >= 0) return document;

  const annotationsIndex = lines.findIndex((line, index) => (
    index > metadataIndex && index < end && /^  annotations:\s*$/.test(line)
  ));
  if (annotationsIndex >= 0) {
    lines.splice(annotationsIndex + 1, 0, '    helm.sh/resource-policy: keep');
  } else {
    lines.splice(metadataIndex + 1, 0, '  annotations:', '    helm.sh/resource-policy: keep');
  }
  return lines.join('\n');
}

process.stdout.write(input.split(/(?=^---\s*$)/m).map(keepCrdResource).join(''));
`;

export function addKeepResourcePolicyToCrdManifest(manifest: string): string {
  return manifest.split(/(?=^---\s*$)/m).map((document) => {
    const lines = document.split(/\r?\n/);
    const kindIndex = lines.findIndex((line) => /^kind:\s*CustomResourceDefinition\s*$/.test(line));
    if (kindIndex < 0) return document;

    const metadataIndex = lines.findIndex((line, index) => index > kindIndex && /^metadata:\s*$/.test(line));
    if (metadataIndex < 0) return document;

    const metadataEnd = lines.findIndex((line, index) => index > metadataIndex && /^[^\s]/.test(line));
    const end = metadataEnd < 0 ? lines.length : metadataEnd;
    const policyIndex = lines.findIndex((line, index) => (
      index > metadataIndex
      && index < end
      && /^\s+helm\.sh\/resource-policy\s*:/.test(line)
    ));
    if (policyIndex >= 0) return document;

    const annotationsIndex = lines.findIndex((line, index) => (
      index > metadataIndex && index < end && /^  annotations:\s*$/.test(line)
    ));
    if (annotationsIndex >= 0) {
      lines.splice(annotationsIndex + 1, 0, '    helm.sh/resource-policy: keep');
    } else {
      lines.splice(metadataIndex + 1, 0, '  annotations:', '    helm.sh/resource-policy: keep');
    }
    return lines.join('\n');
  }).join('');
}

function valuesToSetJsonCommandArgs(values: Record<string, unknown>): string[] {
  const args: string[] = [];
  for (const [key, value] of Object.entries(values)) {
    args.push(`--set-json ${shellQuote(`${key}=${JSON.stringify(value)}`)}`);
  }
  return args;
}

function appendValuesToCommand(cmd: string, values?: Record<string, unknown>): string {
  if (!values) {
    return cmd;
  }
  return `${cmd} ${valuesToSetJsonCommandArgs(values).join(' ')}`;
}

/**
 * NVIDIA GPU Operator Helm configuration
 */
export const GPU_OPERATOR_REPO: HelmRepo = {
  name: 'nvidia',
  url: 'https://helm.ngc.nvidia.com/nvidia',
};

export const GPU_OPERATOR_CHART: HelmChart = {
  name: 'gpu-operator',
  chart: 'nvidia/gpu-operator',
  namespace: 'gpu-operator',
  createNamespace: true,
};

/**
 * Result of a Helm command execution
 */
export interface HelmResult {
  success: boolean;
  stdout: string;
  stderr: string;
  exitCode: number | null;
}

/**
 * Helm release information from `helm list`
 */
export interface HelmRelease {
  name: string;
  namespace: string;
  revision: string;
  updated: string;
  status: string;
  chart: string;
  appVersion: string;
}

/**
 * Stream callback for real-time output
 */
export type StreamCallback = (data: string, stream: 'stdout' | 'stderr') => void;

/**
 * Helm Service
 * Provides Helm CLI integration for provider installation
 */
class HelmService {
  private helmPath: string;

  constructor() {
    // Use HELM_PATH env var or default to 'helm' in PATH
    this.helmPath = process.env.HELM_PATH || 'helm';
  }

  /**
   * Execute a Helm command
   */
  private async execute(
    args: string[],
    onStream?: StreamCallback,
    timeoutMs: number = 300000 // 5 minutes default timeout
  ): Promise<HelmResult> {
    return new Promise((resolve) => {
      let stdout = '';
      let stderr = '';
      let timedOut = false;
      const startTime = Date.now();
      const fullCommand = `${this.helmPath} ${args.join(' ')}`;

      logger.info({ command: fullCommand, timeoutMs }, `Executing helm command`);

      const proc = spawn(this.helmPath, args, {
        env: { ...process.env },
        shell: false,
      });

      // Set timeout
      const timeout = setTimeout(() => {
        timedOut = true;
        proc.kill('SIGTERM');
      }, timeoutMs);

      proc.stdout.on('data', (data: Buffer) => {
        const text = data.toString();
        stdout += text;
        if (onStream) {
          onStream(text, 'stdout');
        }
      });

      proc.stderr.on('data', (data: Buffer) => {
        const text = data.toString();
        stderr += text;
        if (onStream) {
          onStream(text, 'stderr');
        }
      });

      proc.on('close', (code) => {
        clearTimeout(timeout);
        const durationMs = Date.now() - startTime;
        const durationSec = (durationMs / 1000).toFixed(1);
        
        if (timedOut) {
          logger.error({ command: fullCommand, durationSec, stdout: stdout.slice(-500), stderr: stderr.slice(-500) }, `Helm command timed out after ${durationSec}s`);
          resolve({
            success: false,
            stdout,
            stderr: stderr + `\nCommand timed out after ${timeoutMs / 1000} seconds`,
            exitCode: null,
          });
        } else if (code === 0) {
          logger.info({ command: fullCommand, durationSec }, `Helm command completed successfully in ${durationSec}s`);
          resolve({
            success: true,
            stdout,
            stderr,
            exitCode: code,
          });
        } else {
          logger.error({ command: fullCommand, exitCode: code, durationSec, stdout: stdout.slice(-500), stderr: stderr.slice(-500) }, `Helm command failed with exit code ${code} after ${durationSec}s`);
          resolve({
            success: false,
            stdout,
            stderr,
            exitCode: code,
          });
        }
      });

      proc.on('error', (err) => {
        clearTimeout(timeout);
        resolve({
          success: false,
          stdout,
          stderr: `Failed to execute helm: ${err.message}`,
          exitCode: null,
        });
      });
    });
  }

  /**
   * Execute a kubectl command
   */
  private async executeKubectl(
    args: string[],
    onStream?: StreamCallback,
    timeoutMs: number = 60000 // 1 minute default timeout for kubectl
  ): Promise<HelmResult> {
    return new Promise((resolve) => {
      let stdout = '';
      let stderr = '';
      let timedOut = false;
      const startTime = Date.now();
      const kubectlPath = process.env.KUBECTL_PATH || 'kubectl';
      const fullCommand = `${kubectlPath} ${args.join(' ')}`;

      logger.info({ command: fullCommand, timeoutMs }, `Executing kubectl command`);

      const proc = spawn(kubectlPath, args, {
        env: { ...process.env },
        shell: false,
      });

      // Set timeout
      const timeout = setTimeout(() => {
        timedOut = true;
        proc.kill('SIGTERM');
      }, timeoutMs);

      proc.stdout.on('data', (data: Buffer) => {
        const text = data.toString();
        stdout += text;
        if (onStream) {
          onStream(text, 'stdout');
        }
      });

      proc.stderr.on('data', (data: Buffer) => {
        const text = data.toString();
        stderr += text;
        if (onStream) {
          onStream(text, 'stderr');
        }
      });

      proc.on('close', (code) => {
        clearTimeout(timeout);
        const durationMs = Date.now() - startTime;
        const durationSec = (durationMs / 1000).toFixed(1);
        
        if (timedOut) {
          logger.error({ command: fullCommand, durationSec }, `kubectl command timed out after ${durationSec}s`);
          resolve({
            success: false,
            stdout,
            stderr: stderr + `\nCommand timed out after ${timeoutMs / 1000} seconds`,
            exitCode: null,
          });
        } else if (code === 0) {
          logger.info({ command: fullCommand, durationSec }, `kubectl command completed successfully in ${durationSec}s`);
          resolve({
            success: true,
            stdout,
            stderr,
            exitCode: code,
          });
        } else {
          logger.error({ command: fullCommand, exitCode: code, durationSec, stderr: stderr.slice(-500) }, `kubectl command failed with exit code ${code} after ${durationSec}s`);
          resolve({
            success: false,
            stdout,
            stderr,
            exitCode: code,
          });
        }
      });

      proc.on('error', (err) => {
        clearTimeout(timeout);
        resolve({
          success: false,
          stdout,
          stderr: `Failed to execute kubectl: ${err.message}`,
          exitCode: null,
        });
      });
    });
  }

  /**
   * Check if Helm is available
   */
  async checkHelmAvailable(): Promise<{ available: boolean; version?: string; error?: string }> {
    const result = await this.execute(['version', '--short']);
    
    if (result.success) {
      return {
        available: true,
        version: result.stdout.trim(),
      };
    }

    return {
      available: false,
      error: result.stderr || 'Helm not found. Please install Helm CLI.',
    };
  }

  /**
   * Add a Helm repository
   */
  async repoAdd(repo: HelmRepo, onStream?: StreamCallback): Promise<HelmResult> {
    return this.execute(['repo', 'add', repo.name, repo.url, '--force-update'], onStream);
  }

  /**
   * Update Helm repositories
   */
  async repoUpdate(onStream?: StreamCallback): Promise<HelmResult> {
    return this.execute(['repo', 'update'], onStream);
  }

  /**
   * List Helm releases in a namespace
   */
  async list(namespace?: string): Promise<{ success: boolean; releases: HelmRelease[]; error?: string }> {
    const args = ['list', '--output', 'json'];
    if (namespace) {
      args.push('--namespace', namespace);
    } else {
      args.push('--all-namespaces');
    }

    const result = await this.execute(args);

    if (!result.success) {
      return {
        success: false,
        releases: [],
        error: result.stderr,
      };
    }

    try {
      const releases = JSON.parse(result.stdout || '[]') as HelmRelease[];
      return {
        success: true,
        releases,
      };
    } catch {
      return {
        success: true,
        releases: [],
      };
    }
  }

  /**
   * Pull (download) a Helm chart tarball from a URL
   */
  async pull(
    url: string,
    destination: string,
    onStream?: StreamCallback
  ): Promise<HelmResult> {
    // Ensure destination directory exists
    if (!existsSync(destination)) {
      mkdirSync(destination, { recursive: true });
    }
    const args = ['pull', url, '--destination', destination];
    return this.execute(args, onStream);
  }

  private sanitizeNameForStep(name: string): string {
    return name.replace(/[^a-zA-Z0-9-]+/g, '-');
  }

  private getManagedChartVarPrefix(chart: HelmChart): string {
    return this.sanitizeNameForStep(chart.name).replace(/-/g, '_').toUpperCase();
  }

  private buildInstallCommand(
    chart: HelmChart,
    chartRef: string = chart.chart,
    includeVersion: boolean = true
  ): string {
    let cmd = `helm install ${chart.name} ${chartRef}`;
    cmd += ` --namespace ${chart.namespace}`;
    if (chart.createNamespace) {
      cmd += ' --create-namespace';
    }
    if (includeVersion && chart.version) {
      cmd += ` --version ${chart.version}`;
    }
    cmd = appendValuesToCommand(cmd, chart.values);
    if (chart.skipCrds) {
      cmd += ' --skip-crds';
    }
    return cmd;
  }

  private buildKeepCrdPostRendererCommand(installCommand: string, rendererVar: string): string {
    const rendererRef = `$${rendererVar}`;
    return [
      `(${rendererVar}=$(mktemp)`,
      'set -e',
      `trap 'rm -f -- "${rendererRef}"' EXIT`,
      `cat > "${rendererRef}" <<'AIRUNWAY_KEEP_CRD_POST_RENDERER'`,
      `#!/usr/bin/env node\n${KEEP_CRD_POST_RENDERER_SOURCE}`,
      'AIRUNWAY_KEEP_CRD_POST_RENDERER',
      `chmod +x "${rendererRef}"`,
      `${installCommand} --post-renderer "${rendererRef}")`,
    ].join('\n');
  }

  private buildPullChartCommand(chart: HelmChart, untarDir: string): string {
    let cmd = `helm pull ${chart.fetchUrl || chart.chart} --untar --untardir ${untarDir}`;
    if (!chart.fetchUrl && chart.version) {
      cmd += ` --version ${chart.version}`;
    }
    return cmd;
  }

  private buildPreInstallMissingCrdsCommand(chart: HelmChart): string {
    const varPrefix = this.getManagedChartVarPrefix(chart);
    const chartDirVar = `${varPrefix}_CHART_DIR`;
    const chartPathVar = `${varPrefix}_CHART_PATH`;
    const chartDirRef = `$${chartDirVar}`;
    const chartPathRef = `$${chartPathVar}`;
    const installCommand = this.buildInstallCommand(chart, `"${chartPathRef}"`, false);
    const installWithPostRenderer = chart.keepCrdResources
      ? this.buildKeepCrdPostRendererCommand(
          installCommand,
          `${varPrefix}_KEEP_CRD_POST_RENDERER`,
        )
      : installCommand;

    return [
      `(${chartDirVar}=$(mktemp -d)`,
      'set -e',
      `trap 'rm -rf -- "${chartDirRef}"' EXIT`,
      this.buildPullChartCommand(chart, `"${chartDirRef}"`),
      `${chartPathVar}=$(find "${chartDirRef}" -mindepth 1 -maxdepth 1 -type d -print -quit)`,
      `test -n "${chartPathRef}"`,
      `for crd in "${chartPathRef}/crds/"*.yaml "${chartPathRef}/crds/"*.yml; do if [ -f "$crd" ]; then missing=0; crd_names=$(kubectl create --dry-run=client -f "$crd" -o name) || exit $?; for crd_name in $crd_names; do existing=$(kubectl get "$crd_name" --ignore-not-found -o name) || exit $?; if [ -z "$existing" ]; then missing=1; fi; done; if [ "$missing" = "1" ]; then kubectl apply --server-side --force-conflicts -f "$crd" || exit $?; fi; fi; done`,
      `${installWithPostRenderer})`,
    ].join(' && ');
  }

  private createSyntheticResult(stdout: string): HelmResult {
    return {
      success: true,
      stdout,
      stderr: '',
      exitCode: 0,
    };
  }

  private createKeepCrdPostRenderer(tempDir: string): string {
    const scriptPath = join(tempDir, 'keep-crd-resources.js');
    writeFileSync(scriptPath, `#!/usr/bin/env node\n${KEEP_CRD_POST_RENDERER_SOURCE}`, 'utf8');

    if (process.platform === 'win32') {
      const wrapperPath = join(tempDir, 'keep-crd-resources.cmd');
      writeFileSync(wrapperPath, `@echo off\r\n"${process.execPath}" "${scriptPath}"\r\n`, 'utf8');
      return wrapperPath;
    }

    const wrapperPath = join(tempDir, 'keep-crd-resources.sh');
    writeFileSync(
      wrapperPath,
      `#!/bin/sh\nexec ${shellQuote(process.execPath)} ${shellQuote(scriptPath)}\n`,
      'utf8',
    );
    chmodSync(wrapperPath, 0o755);
    return wrapperPath;
  }

  private async pullChartToTempDir(
    chart: HelmChart,
    onStream?: StreamCallback
  ): Promise<{ success: boolean; chartPath?: string; tempDir?: string; result?: HelmResult }> {
    if (!chart.fetchUrl && existsSync(chart.chart)) {
      return {
        success: true,
        chartPath: chart.chart,
      };
    }

    const tempDir = mkdtempSync(join(tmpdir(), 'helm-chart-'));
    const args = ['pull', chart.fetchUrl || chart.chart, '--untar', '--untardir', tempDir];

    if (!chart.fetchUrl && chart.version) {
      args.push('--version', chart.version);
    }

    const result = await this.execute(args, onStream);
    if (!result.success) {
      rmSync(tempDir, { recursive: true, force: true });
      return { success: false, result };
    }

    const chartDir = readdirSync(tempDir, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .find((entry) => existsSync(join(tempDir, entry.name, 'Chart.yaml')));
    if (!chartDir) {
      const failure = {
        success: false,
        stdout: result.stdout,
        stderr: 'Failed to locate extracted chart contents after helm pull',
        exitCode: 1,
      };
      rmSync(tempDir, { recursive: true, force: true });
      return { success: false, result: failure };
    }

    return {
      success: true,
      chartPath: join(tempDir, chartDir.name),
      tempDir,
    };
  }

  private async getChartCrdDocuments(
    chartPath: string,
  ): Promise<{ success: boolean; documents: ChartCrdDocument[]; result?: HelmResult }> {
    const crdsPath = join(chartPath, 'crds');
    if (!existsSync(crdsPath)) {
      return { success: true, documents: [] };
    }

    try {
      const crdFiles = readdirSync(crdsPath, { withFileTypes: true })
        .filter((entry) => entry.isFile() && /\.(yaml|yml)$/i.test(entry.name))
        .sort((a, b) => a.name.localeCompare(b.name));
      const crdDocuments = new Map<string, ChartCrdDocument>();

      for (const crdFile of crdFiles) {
        for (const document of loadAll(readFileSync(join(crdsPath, crdFile.name), 'utf8'))) {
          if (!document || typeof document !== 'object') continue;

          const kind = (document as { kind?: string }).kind;
          const metadata = (document as { metadata?: { name?: string } }).metadata;
          if (kind !== 'CustomResourceDefinition' || !metadata?.name || crdDocuments.has(metadata.name)) continue;

          crdDocuments.set(metadata.name, {
            name: metadata.name,
            manifest: dump(document, { noRefs: true }),
          });
        }
      }

      return { success: true, documents: Array.from(crdDocuments.values()) };
    } catch (error) {
      return {
        success: false,
        documents: [],
        result: {
          success: false,
          stdout: '',
          stderr: `Failed to read chart CRDs: ${error instanceof Error ? error.message : String(error)}`,
          exitCode: 1,
        },
      };
    }
  }

  private async ensureChartCrdsInstalled(
    chart: HelmChart,
    chartPath: string,
    tempDir: string,
    onStream?: StreamCallback
  ): Promise<{ success: boolean; results: Array<{ step: string; result: HelmResult }> }> {
    const results: Array<{ step: string; result: HelmResult }> = [];
    const crdPreparation = await this.getChartCrdDocuments(chartPath);
    if (!crdPreparation.success) {
      results.push({
        step: `render-chart-crds-${chart.name}`,
        result: crdPreparation.result ?? {
          success: false,
          stdout: '',
          stderr: 'Failed to render chart CRDs',
          exitCode: 1,
        },
      });
      return { success: false, results };
    }

    const crdDocuments = crdPreparation.documents;

    for (let i = 0; i < crdDocuments.length; i++) {
      const crd = crdDocuments[i];
      const stepName = this.sanitizeNameForStep(crd.name);

      const checkResult = await this.executeKubectl(
        ['get', 'crd', crd.name, '--ignore-not-found', '-o', 'name'],
        onStream,
      );
      if (!checkResult.success) {
        results.push({ step: `check-crd-${stepName}`, result: checkResult });
        return { success: false, results };
      }

      if (checkResult.stdout.trim().length > 0) {
        results.push({
          step: `skip-crd-${stepName}`,
          result: this.createSyntheticResult(`CRD ${crd.name} already exists, skipping chart CRD install.`),
        });
        continue;
      }

      const manifestPath = join(tempDir, `crd-${i}-${stepName}.yaml`);
      writeFileSync(manifestPath, crd.manifest, 'utf8');

      const applyResult = await this.executeKubectl(['apply', '--server-side', '--force-conflicts', '-f', manifestPath], onStream);
      results.push({ step: `apply-crd-${stepName}`, result: applyResult });
      if (!applyResult.success) {
        return { success: false, results };
      }
    }

    return { success: true, results };
  }

  /**
   * Install a Helm chart (uses upgrade --install to handle existing releases)
   * If chart has a fetchUrl, pulls the tarball first and installs from it
   */
  async install(
    chart: HelmChart,
    onStream?: StreamCallback
  ): Promise<HelmResult> {
    let chartPath = chart.chart;
    let postRendererDir: string | undefined;

    // If fetchUrl is provided, pull the chart first
    if (chart.fetchUrl) {
      const tempDir = '/tmp/helm-charts';
      // Ensure temp directory exists
      if (!existsSync(tempDir)) {
        mkdirSync(tempDir, { recursive: true });
      }
      
      const pullResult = await this.execute(['pull', chart.fetchUrl, '--destination', tempDir], onStream);
      if (!pullResult.success) {
        return pullResult;
      }
      // Extract filename from URL
      const urlParts = chart.fetchUrl.split('/');
      const filename = urlParts[urlParts.length - 1];
      chartPath = `${tempDir}/${filename}`;
    }

    // Use upgrade --install to handle both fresh installs and existing releases
    const args = ['upgrade', chart.name, chartPath, '--install'];
    
    args.push('--namespace', chart.namespace);
    
    if (chart.createNamespace) {
      args.push('--create-namespace');
    }

    if (chart.version) {
      args.push('--version', chart.version);
    }

    if (chart.values) {
      args.push(...valuesToSetJsonArgs(chart.values));
    }

    // Skip CRDs if specified (useful when CRDs already exist from another operator)
    if (chart.skipCrds) {
      args.push('--skip-crds');
    }

    if (chart.keepCrdResources) {
      postRendererDir = mkdtempSync(join(tmpdir(), 'helm-crd-renderer-'));
      args.push('--post-renderer', this.createKeepCrdPostRenderer(postRendererDir));
    }

    // Don't use --wait - return immediately after submitting the install
    // The caller should poll for installation status updates
    // Timeout still applies to the install command itself
    
    logger.info({ chart: chart.name, namespace: chart.namespace, version: chart.version, values: chart.values, skipCrds: chart.skipCrds }, `Installing helm chart: ${chart.name}`);

    try {
      return await this.execute(args, onStream);
    } finally {
      if (postRendererDir) {
        rmSync(postRendererDir, { recursive: true, force: true });
      }
    }
  }

  /**
   * Upgrade a Helm release (or install if not exists)
   */
  async upgrade(
    chart: HelmChart,
    onStream?: StreamCallback
  ): Promise<HelmResult> {
    const args = ['upgrade', chart.name, chart.chart, '--install'];
    
    args.push('--namespace', chart.namespace);
    
    if (chart.createNamespace) {
      args.push('--create-namespace');
    }

    if (chart.version) {
      args.push('--version', chart.version);
    }

    if (chart.values) {
      args.push(...valuesToSetJsonArgs(chart.values));
    }

    args.push('--wait', '--timeout', '10m');

    return this.execute(args, onStream);
  }

  /**
   * Uninstall a Helm release
   */
  async uninstall(
    releaseName: string,
    namespace: string,
    onStream?: StreamCallback
  ): Promise<HelmResult> {
    return this.execute(['uninstall', releaseName, '--namespace', namespace, '--wait'], onStream);
  }

  /**
   * Get release status
   */
  async status(releaseName: string, namespace: string): Promise<HelmResult> {
    return this.execute(['status', releaseName, '--namespace', namespace]);
  }

  /**
   * Get detailed release info including status
   */
  async getReleaseInfo(releaseName: string, namespace: string): Promise<{ exists: boolean; status?: string; release?: HelmRelease; error?: string }> {
    const listResult = await this.list(namespace);
    if (!listResult.success) {
      return { exists: false, error: listResult.error };
    }

    const release = listResult.releases.find(r => r.name === releaseName);
    if (!release) {
      return { exists: false };
    }

    return {
      exists: true,
      status: release.status,
      release,
    };
  }

  /**
   * Check if a release is in a truly problematic state (failed only)
   * Note: pending-install and pending-upgrade are expected during installation and are NOT problems
   */
  async checkReleaseProblems(charts: HelmChart[]): Promise<{ hasProblems: boolean; problems: Array<{ chart: string; namespace: string; status: string; message: string }> }> {
    const problems: Array<{ chart: string; namespace: string; status: string; message: string }> = [];

    for (const chart of charts) {
      const info = await this.getReleaseInfo(chart.name, chart.namespace);
      if (info.exists && info.status) {
        const status = info.status.toLowerCase();
        // Only treat 'failed' as problematic
        // pending-install and pending-upgrade are normal during installation
        if (status === 'failed') {
          problems.push({
            chart: chart.name,
            namespace: chart.namespace,
            status: info.status,
            message: `Release "${chart.name}" is in failed state. Run "helm uninstall ${chart.name} -n ${chart.namespace}" and retry installation.`,
          });
        }
      }
    }

    return {
      hasProblems: problems.length > 0,
      problems,
    };
  }

  /**
   * Check if any chart is currently being installed/upgraded (pending state)
   * This is used to detect if a previous install is still in progress
   */
  async checkInstallInProgress(charts: HelmChart[]): Promise<{ inProgress: boolean; pendingCharts: Array<{ chart: string; namespace: string; status: string }> }> {
    const pendingCharts: Array<{ chart: string; namespace: string; status: string }> = [];

    for (const chart of charts) {
      const info = await this.getReleaseInfo(chart.name, chart.namespace);
      if (info.exists && info.status) {
        const status = info.status.toLowerCase();
        if (status === 'pending-install' || status === 'pending-upgrade' || status === 'pending-rollback') {
          pendingCharts.push({
            chart: chart.name,
            namespace: chart.namespace,
            status: info.status,
          });
        }
      }
    }

    return {
      inProgress: pendingCharts.length > 0,
      pendingCharts,
    };
  }

  /**
   * Install all required repos and charts for a provider
   */
  async installProvider(
    repos: HelmRepo[],
    charts: HelmChart[],
    onStream?: StreamCallback
  ): Promise<{ success: boolean; results: Array<{ step: string; result: HelmResult }> }> {
    const results: Array<{ step: string; result: HelmResult }> = [];

    // Add repos
    for (const repo of repos) {
      if (onStream) {
        onStream(`Adding Helm repository: ${repo.name}\n`, 'stdout');
      }
      const result = await this.repoAdd(repo, onStream);
      results.push({ step: `repo-add-${repo.name}`, result });
      if (!result.success) {
        return { success: false, results };
      }
    }

    // Update repos
    if (repos.length > 0) {
      if (onStream) {
        onStream('Updating Helm repositories...\n', 'stdout');
      }
      const updateResult = await this.repoUpdate(onStream);
      results.push({ step: 'repo-update', result: updateResult });
      if (!updateResult.success) {
        return { success: false, results };
      }
    }

    // Install charts
    for (const chart of charts) {
      let chartToInstall = chart;
      let tempDirToClean: string | undefined;

      // Apply pre-CRD URLs if specified (for installing specific CRDs before the chart when skipCrds is used)
      if (chart.preCrdUrls && chart.preCrdUrls.length > 0) {
        for (const crdUrl of chart.preCrdUrls) {
          if (onStream) {
            onStream(`Applying CRD from: ${crdUrl}\n`, 'stdout');
          }
          const kubectlResult = await this.executeKubectl(['apply', '-f', crdUrl], onStream);
          results.push({ step: `apply-crd-${crdUrl.split('/').pop()}`, result: kubectlResult });
          if (!kubectlResult.success) {
            return { success: false, results };
          }
        }
      }

      if (chart.preInstallMissingCrds) {
        if (onStream) {
          onStream(`Preparing chart CRDs for: ${chart.chart}\n`, 'stdout');
        }

        const pulledChart = await this.pullChartToTempDir(chart, onStream);
        if (!pulledChart.success || !pulledChart.chartPath) {
          results.push({
            step: `pull-chart-${chart.name}`,
            result: pulledChart.result ?? {
              success: false,
              stdout: '',
              stderr: 'Failed to prepare chart for CRD installation',
              exitCode: 1,
            },
          });
          return { success: false, results };
        }

        tempDirToClean = pulledChart.tempDir ?? mkdtempSync(join(tmpdir(), 'helm-chart-crds-'));

        const crdPrep = await this.ensureChartCrdsInstalled(chart, pulledChart.chartPath, tempDirToClean, onStream);
        results.push(...crdPrep.results);
        if (!crdPrep.success) {
          if (tempDirToClean) {
            rmSync(tempDirToClean, { recursive: true, force: true });
          }
          return { success: false, results };
        }

        chartToInstall = {
          ...chart,
          chart: pulledChart.chartPath,
          fetchUrl: undefined,
          version: undefined,
          skipCrds: true,
        };
      }

      if (onStream) {
        onStream(`Installing chart: ${chartToInstall.chart}\n`, 'stdout');
      }

      try {
        const result = await this.install(chartToInstall, onStream);
        results.push({ step: `install-${chart.name}`, result });
        if (!result.success) {
          return { success: false, results };
        }
      } finally {
        if (tempDirToClean) {
          rmSync(tempDirToClean, { recursive: true, force: true });
        }
      }
    }

    return { success: true, results };
  }

  /**
   * Get the Helm commands that would be run for provider installation
   * Useful for displaying to users before actually running
   */
  getInstallCommands(repos: HelmRepo[], charts: HelmChart[]): string[] {
    const commands: string[] = [];

    for (const repo of repos) {
      commands.push(`helm repo add ${repo.name} ${repo.url}`);
    }

    if (repos.length > 0) {
      commands.push('helm repo update');
    }

    for (const chart of charts) {
      if (chart.preCrdUrls && chart.preCrdUrls.length > 0) {
        for (const crdUrl of chart.preCrdUrls) {
          commands.push(`kubectl apply -f ${crdUrl}`);
        }
      }

      if (chart.preInstallMissingCrds) {
        commands.push(this.buildPreInstallMissingCrdsCommand(chart));
        continue;
      }

      const installCommand = this.buildInstallCommand(chart, chart.chart, !chart.fetchUrl);
      const installWithPostRenderer = chart.keepCrdResources
        ? this.buildKeepCrdPostRendererCommand(
            installCommand,
            `${this.getManagedChartVarPrefix(chart)}_KEEP_CRD_POST_RENDERER`,
          )
        : installCommand;

      if (chart.fetchUrl) {
        // Use fetch + install for charts with fetchUrl
        const cmd = `helm fetch ${chart.fetchUrl} && ${installWithPostRenderer}`;
        commands.push(cmd);
      } else {
        commands.push(installWithPostRenderer);
      }
    }

    return commands;
  }

  /**
   * Install the NVIDIA GPU Operator
   */
  async installGpuOperator(
    onStream?: StreamCallback
  ): Promise<{ success: boolean; results: Array<{ step: string; result: HelmResult }> }> {
    return this.installProvider([GPU_OPERATOR_REPO], [GPU_OPERATOR_CHART], onStream);
  }

  /**
   * Get the Helm commands for GPU Operator installation
   */
  getGpuOperatorCommands(): string[] {
    return this.getInstallCommands([GPU_OPERATOR_REPO], [GPU_OPERATOR_CHART]);
  }

  /**
   * Apply a manifest from a URL using kubectl apply -f
   */
  async applyManifestUrl(
    url: string,
    onStream?: StreamCallback
  ): Promise<HelmResult> {
    return this.executeKubectl(['apply', '-f', url], onStream);
  }
}

// Export singleton instance
export const helmService = new HelmService();
