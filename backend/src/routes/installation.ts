import { Hono } from 'hono';
import { HTTPException } from 'hono/http-exception';
import { kubernetesService } from '../services/kubernetes';
import { helmService } from '../services/helm';
import logger from '../lib/logger';
import { getAnnotatedProviderDisplayName, getProviderDisplayName, providerRequiresRuntimeCRD } from '../lib/providers';

interface ProviderHelmChartDetails {
  name: string;
  chart: string;
  namespace: string;
  version?: string;
  createNamespace?: boolean;
  values?: Record<string, unknown>;
  preInstallMissingCrds?: boolean;
  skipCrds?: boolean;
}

/**
 * Parse the installation annotation (JSON) from an InferenceProviderConfig CRD object.
 */
function parseInstallationAnnotation(config: any): any {
  const raw = config.metadata?.annotations?.['airunway.ai/installation'];
  if (!raw) return {};
  try {
    return JSON.parse(raw);
  } catch (error) {
    logger.warn({
      provider: config.metadata?.name,
      error: error instanceof Error ? error.message : 'Unknown error',
    }, 'Failed to parse installation annotation');
    return {};
  }
}

/**
 * Extract provider details from an InferenceProviderConfig CRD object.
 * Installation and documentation metadata are read from metadata.annotations,
 * not from spec (which only contains controller-reconciled fields).
 */
function extractProviderDetails(config: any) {
  const name = config.metadata?.name || 'unknown';
  const annotations = config.metadata?.annotations;
  const displayName = getProviderDisplayName(name, annotations);
  const annotatedDisplayName = getAnnotatedProviderDisplayName(annotations);
  const installation = parseInstallationAnnotation(config);
  const capabilities = config.spec?.capabilities || {};

  return {
    id: name,
    name: displayName,
    description: installation.description || '',
    defaultNamespace: installation.defaultNamespace || 'default',
    requiresCRD: providerRequiresRuntimeCRD(name, capabilities.requiresCRD, annotatedDisplayName),
    crdConfig: {
      apiGroup: capabilities.engines?.length ? '' : '',
    },
    helmRepos: (installation.helmRepos || []).map((r: any) => ({
      name: r.name,
      url: r.url,
    })),
    helmCharts: (installation.helmCharts || []).map((c: any): ProviderHelmChartDetails => {
      const values = c.values && typeof c.values === 'object' && !Array.isArray(c.values)
        ? c.values as Record<string, unknown>
        : undefined;
      if (c.values !== undefined && values === undefined) {
        logger.warn({ provider: name, chart: c.name }, 'Ignoring malformed Helm chart values in provider installation metadata');
      }

      return {
        name: c.name,
        chart: c.chart,
        version: c.version,
        namespace: c.namespace,
        createNamespace: c.createNamespace,
        values,
      };
    }),
    installationSteps: (installation.steps || []).map((s: any) => ({
      title: s.title,
      command: s.command,
      description: s.description,
    })),
  };
}

function shouldPreInstallMissingCrds(providerId: string, chart: ProviderHelmChartDetails) {
  return (
    (providerId === 'kaito' && chart.chart === 'kaito/workspace')
    || (providerId === 'dynamo' && chart.name === 'dynamo-platform')
  );
}

function normalizeInstallCharts(providerId: string, charts: ProviderHelmChartDetails[]): ProviderHelmChartDetails[] {
  return charts.map((chart) => (
    shouldPreInstallMissingCrds(providerId, chart)
      ? {
          ...chart,
          preInstallMissingCrds: true,
          skipCrds: true,
        }
      : chart
  ));
}

const INSTALLER_PERMISSION_GUIDANCE = 'Automatic installation requires elevated installer permissions. Ask an admin to apply the optional dashboard installer permissions manifest (deploy/dashboard-installer-rbac.yaml) or run the commands manually.';

function isInstallerPermissionError(output?: string): boolean {
  if (!output) return false;
  return /\bforbidden\b|cannot (?:create|update|patch|delete|get|list|watch)|is forbidden|attempting to grant RBAC permissions not currently held|requires.*(?:permission|privilege)/i.test(output);
}

function installationFailureStatus(output?: string): 403 | 500 {
  return isInstallerPermissionError(output) ? 403 : 500;
}

function installationFailureMessage(prefix: string, output?: string): string {
  const detail = output?.trim() || 'Unknown error';
  return isInstallerPermissionError(detail)
    ? `${prefix}: ${INSTALLER_PERMISSION_GUIDANCE} Details: ${detail}`
    : `${prefix}: ${detail}`;
}

const installation = new Hono()
  .get('/helm/status', async (c) => {
    const helmStatus = await helmService.checkHelmAvailable();
    return c.json(helmStatus);
  })
  .get('/gpu-operator/status', async (c) => {
    const status = await kubernetesService.checkGPUOperatorStatus();
    const helmCommands = helmService.getGpuOperatorCommands();

    return c.json({
      ...status,
      helmCommands,
    });
  })
  .get('/gpu-capacity', async (c) => {
    const capacity = await kubernetesService.getClusterGpuCapacity();
    return c.json(capacity);
  })
  .get('/gpu-capacity/detailed', async (c) => {
    const capacity = await kubernetesService.getDetailedClusterGpuCapacity();
    return c.json(capacity);
  })
  .post('/gpu-operator/install', async (c) => {
    const helmStatus = await helmService.checkHelmAvailable();
    if (!helmStatus.available) {
      throw new HTTPException(400, {
        message: `Helm CLI not available: ${helmStatus.error}. Please install Helm or use the manual installation commands.`,
      });
    }

    const currentStatus = await kubernetesService.checkGPUOperatorStatus();
    if (currentStatus.installed) {
      return c.json({
        success: true,
        message: 'NVIDIA GPU Operator is already installed',
        alreadyInstalled: true,
        status: currentStatus,
      });
    }

    logger.info('Starting installation of NVIDIA GPU Operator');
    const result = await helmService.installGpuOperator((data, stream) => {
      logger.debug({ stream }, data.trim());
    });

    if (result.success) {
      const verifyStatus = await kubernetesService.checkGPUOperatorStatus();

      return c.json({
        success: true,
        message: 'NVIDIA GPU Operator installed successfully',
        status: verifyStatus,
        results: result.results.map((r) => ({
          step: r.step,
          success: r.result.success,
          output: r.result.stdout,
          error: r.result.stderr,
        })),
      });
    } else {
      const failedStep = result.results.find((r) => !r.result.success);
      const output = failedStep?.result.stderr || failedStep?.result.stdout;
      throw new HTTPException(installationFailureStatus(output), {
        message: installationFailureMessage(`Installation failed at step "${failedStep?.step}"`, output),
      });
    }
  })
  .get('/runtimes/status', async (c) => {
    const runtimesStatus = await kubernetesService.getRuntimesStatus();
    return c.json({ runtimes: runtimesStatus });
  })
  .get('/providers/:providerId/status', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    const provider = extractProviderDetails(config);
    const charts = normalizeInstallCharts(providerId, provider.helmCharts);
    const hasInstallMetadata = charts.length > 0;
    const requiresCRD = provider.requiresCRD !== false;
    const installable = requiresCRD && hasInstallMetadata;
    const status = config.status || {};
    const installationStatus = await kubernetesService.checkProviderInstallationStatus(
      providerId,
      status,
      provider.name,
      provider.requiresCRD,
    );

    return c.json({
      providerId: provider.id,
      providerName: provider.name,
      installed: installationStatus.installed,
      crdFound: installationStatus.crdFound,
      operatorRunning: installationStatus.operatorRunning,
      requiresCRD: installationStatus.requiresCRD ?? provider.requiresCRD,
      version: status.version,
      message: hasInstallMetadata || provider.requiresCRD === false
        ? installationStatus.message
        : `No installation metadata found for provider ${providerId}`,
      installable,
      installationSteps: provider.installationSteps,
      helmCommands: installable ? helmService.getInstallCommands(provider.helmRepos, charts) : [],
    });
  })
  .get('/providers/:providerId/commands', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    const provider = extractProviderDetails(config);
    const charts = normalizeInstallCharts(providerId, provider.helmCharts);
    const installable = provider.requiresCRD !== false && charts.length > 0;

    return c.json({
      providerId: provider.id,
      providerName: provider.name,
      commands: installable ? helmService.getInstallCommands(provider.helmRepos, charts) : [],
      steps: provider.installationSteps,
    });
  })
  .post('/providers/:providerId/install', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    const provider = extractProviderDetails(config);
    const charts = normalizeInstallCharts(providerId, provider.helmCharts);

    if (provider.requiresCRD === false) {
      throw new HTTPException(400, {
        message: `${provider.name} is managed by provider registration and cannot be installed from this page.`,
      });
    }

    if (charts.length === 0) {
      throw new HTTPException(400, {
        message: `No installation metadata found for provider ${providerId}. Provider config is missing the airunway.ai/installation annotation or it contains no helmCharts.`,
      });
    }

    const helmStatus = await helmService.checkHelmAvailable();
    if (!helmStatus.available) {
      throw new HTTPException(400, {
        message: `Helm CLI not available: ${helmStatus.error}. Please install Helm or use the manual installation commands.`,
      });
    }

    logger.info({ providerId }, `Starting installation of ${provider.name}`);
    const result = await helmService.installProvider(
      provider.helmRepos,
      charts,
      (data, stream) => { logger.debug({ stream, providerId }, data.trim()); }
    );

    if (result.success) {
      return c.json({
        success: true,
        message: `${provider.name} installed successfully`,
        results: result.results.map((r) => ({
          step: r.step,
          success: r.result.success,
          output: r.result.stdout,
          error: r.result.stderr,
        })),
      });
    } else {
      const failedStep = result.results.find((r) => !r.result.success);
      const output = failedStep?.result.stderr || failedStep?.result.stdout;
      throw new HTTPException(installationFailureStatus(output), {
        message: installationFailureMessage(`Installation failed at step "${failedStep?.step}"`, output),
      });
    }
  })
  .post('/providers/:providerId/uninstall', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    const provider = extractProviderDetails(config);

    if (provider.requiresCRD === false) {
      throw new HTTPException(400, {
        message: `${provider.name} is managed by provider registration and cannot be uninstalled from this page.`,
      });
    }

    const helmStatus = await helmService.checkHelmAvailable();
    if (!helmStatus.available) {
      throw new HTTPException(400, {
        message: `Helm CLI not available: ${helmStatus.error}.`,
      });
    }

    logger.info({ providerId }, `Uninstalling ${provider.name}`);
    const results: Array<{ step: string; success: boolean; output: string; error?: string }> = [];

    for (const chart of [...provider.helmCharts].reverse()) {
      const result = await helmService.uninstall(chart.name, chart.namespace);
      results.push({
        step: `uninstall-${chart.name}`,
        success: result.success,
        output: result.stdout,
        error: result.stderr,
      });
    }

    const allSuccess = results.every(r => r.success);
    const failedResult = results.find(r => !r.success);
    const failedOutput = failedResult?.error || failedResult?.output;
    return c.json({
      success: allSuccess,
      message: allSuccess
        ? `${provider.name} uninstalled successfully`
        : installationFailureMessage(`${provider.name} uninstall failed`, failedOutput),
      results,
    });
  })
  .post('/providers/:providerId/uninstall-crds', async (c) => {
    const providerId = c.req.param('providerId');
    const config = await kubernetesService.getInferenceProviderConfig(providerId);

    if (!config) {
      throw new HTTPException(404, { message: `Provider not found: ${providerId}` });
    }

    const crdConfig = config.spec?.capabilities || {};
    logger.info({ providerId }, `Removing CRDs for ${providerId}`);

    // The CRD name is typically plural.apiGroup — but since we don't store that in
    // the CRD itself, we delete the InferenceProviderConfig instance for this provider
    try {
      await kubernetesService.deleteInferenceProviderConfig(providerId);
      return c.json({
        success: true,
        message: `${providerId} provider config removed successfully`,
      });
    } catch (error) {
      throw new HTTPException(500, {
        message: `Failed to remove CRDs: ${error instanceof Error ? error.message : 'Unknown error'}`,
      });
    }
  })
  .get('/gateway/status', async (c) => {
    const status = await kubernetesService.checkGatewayCRDStatus();
    return c.json(status);
  })
  .post('/gateway/install-crds', async (c) => {
    const { GATEWAY_API_CRD_URL, GAIE_CRD_URL, PINNED_GAIE_VERSION } = await import('@airunway/shared');

    const results: Array<{ step: string; success: boolean; output: string; error?: string }> = [];

    // Install Gateway API CRDs
    logger.info('Installing Gateway API CRDs');
    const gwResult = await helmService.applyManifestUrl(GATEWAY_API_CRD_URL, (data, stream) => {
      logger.debug({ stream }, data.trim());
    });
    results.push({
      step: 'gateway-api-crds',
      success: gwResult.success,
      output: gwResult.stdout,
      error: gwResult.stderr || undefined,
    });

    if (!gwResult.success) {
      const output = gwResult.stderr || gwResult.stdout;
      throw new HTTPException(installationFailureStatus(output), {
        message: installationFailureMessage('Failed to install Gateway API CRDs', output),
      });
    }

    // Install GAIE CRDs
    logger.info(`Installing Inference Extension CRDs (${PINNED_GAIE_VERSION})`);
    const gaieResult = await helmService.applyManifestUrl(GAIE_CRD_URL, (data, stream) => {
      logger.debug({ stream }, data.trim());
    });
    results.push({
      step: 'inference-extension-crds',
      success: gaieResult.success,
      output: gaieResult.stdout,
      error: gaieResult.stderr || undefined,
    });

    if (!gaieResult.success) {
      const output = gaieResult.stderr || gaieResult.stdout;
      throw new HTTPException(installationFailureStatus(output), {
        message: installationFailureMessage('Failed to install Inference Extension CRDs', output),
      });
    }

    return c.json({
      success: true,
      message: 'Gateway API and Inference Extension CRDs installed successfully',
      results,
    });
  });

export default installation;
