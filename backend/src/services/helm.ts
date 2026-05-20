import { spawn } from 'child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'fs';
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

    return [
      `(${chartDirVar}=$(mktemp -d)`,
      `trap 'rm -rf -- "${chartDirRef}"' EXIT`,
      this.buildPullChartCommand(chart, `"${chartDirRef}"`),
      `${chartPathVar}=$(find "${chartDirRef}" -mindepth 1 -maxdepth 1 -type d -print -quit)`,
      `test -n "${chartPathRef}"`,
      `find "${chartPathRef}" -type f -path "*/crds/*.yaml" -print -o -type f -path "*/crds/*.yml" -print | sort | while IFS= read -r crd; do missing=0; for crd_name in $(kubectl create --dry-run=client -f "$crd" -o name); do if [ -z "$(kubectl get "$crd_name" --ignore-not-found -o name)" ]; then missing=1; fi; done; if [ "$missing" = "1" ]; then kubectl apply --server-side --force-conflicts -f "$crd"; fi; done`,
      `${this.buildInstallCommand(chart, `"${chartPathRef}"`, false)})`,
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

  private getChartCrdDocuments(chartPath: string): ChartCrdDocument[] {
    const crdsDirs: string[] = [];

    const visit = (dir: string) => {
      if (!existsSync(dir)) {
        return;
      }

      for (const entry of readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
        if (!entry.isDirectory()) {
          continue;
        }

        const childPath = join(dir, entry.name);
        if (entry.name === 'crds') {
          crdsDirs.push(childPath);
          continue;
        }

        visit(childPath);
      }
    };

    visit(chartPath);

    const crdDocuments = new Map<string, ChartCrdDocument>();

    for (const crdsDir of crdsDirs) {
      const crdFiles = readdirSync(crdsDir)
        .filter((file) => file.endsWith('.yaml') || file.endsWith('.yml'))
        .sort();

      for (const file of crdFiles) {
        const filePath = join(crdsDir, file);
        const documents = loadAll(readFileSync(filePath, 'utf8'));

        for (const document of documents) {
          if (!document || typeof document !== 'object') {
            continue;
          }

          const kind = (document as { kind?: string }).kind;
          const metadata = (document as { metadata?: { name?: string } }).metadata;
          if (kind !== 'CustomResourceDefinition' || !metadata?.name || crdDocuments.has(metadata.name)) {
            continue;
          }

          crdDocuments.set(metadata.name, {
            name: metadata.name,
            manifest: dump(document, { noRefs: true }),
          });
        }
      }
    }

    return Array.from(crdDocuments.values());
  }

  private async ensureChartCrdsInstalled(
    chartPath: string,
    tempDir: string,
    onStream?: StreamCallback
  ): Promise<{ success: boolean; results: Array<{ step: string; result: HelmResult }> }> {
    const results: Array<{ step: string; result: HelmResult }> = [];
    const crdDocuments = this.getChartCrdDocuments(chartPath);

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

    // Don't use --wait - return immediately after submitting the install
    // The caller should poll for installation status updates
    // Timeout still applies to the install command itself
    
    logger.info({ chart: chart.name, namespace: chart.namespace, version: chart.version, values: chart.values, skipCrds: chart.skipCrds }, `Installing helm chart: ${chart.name}`);

    return this.execute(args, onStream);
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
    return this.execute(['uninstall', releaseName, '--namespace', namespace], onStream);
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

        const crdPrep = await this.ensureChartCrdsInstalled(pulledChart.chartPath, tempDirToClean, onStream);
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

      if (chart.fetchUrl) {
        // Use fetch + install for charts with fetchUrl
        const cmd = `helm fetch ${chart.fetchUrl} && ${this.buildInstallCommand(chart, chart.chart, false)}`;
        commands.push(cmd);
      } else {
        commands.push(this.buildInstallCommand(chart));
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
