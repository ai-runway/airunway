import { describe, expect, test } from 'bun:test';
import app from '../hono-app';
import { kubernetesService } from '../services/kubernetes';

if (process.env.AIRUNWAY_TEST_MODE !== 'strict') {
  throw new Error(
    'Real-cluster integration tests require AIRUNWAY_TEST_MODE=strict. Use `bun run test` for deterministic local tests.',
  );
}

const OPERATION_TIMEOUT_MS = 10_000;
const TEST_TIMEOUT_MS = 30_000;

async function withTimeout<T>(
  label: string,
  operation: Promise<T>,
  timeoutMs = OPERATION_TIMEOUT_MS,
  onTimeout?: () => void,
): Promise<T> {
  let timeout: ReturnType<typeof setTimeout> | undefined;
  const deadline = new Promise<never>((_, reject) => {
    timeout = setTimeout(() => {
      onTimeout?.();
      reject(new Error(`${label} timed out after ${timeoutMs}ms`));
    }, timeoutMs);
  });

  try {
    return await Promise.race([operation, deadline]);
  } finally {
    if (timeout !== undefined) clearTimeout(timeout);
  }
}

function requestWithTimeout(path: string, timeoutMs = OPERATION_TIMEOUT_MS): Promise<Response> {
  const controller = new AbortController();
  return withTimeout(
    `GET ${path}`,
    Promise.resolve(app.request(path, { signal: controller.signal })),
    timeoutMs,
    () => controller.abort(),
  );
}

describe('strict Kubernetes integration', () => {
  test('connects to the cluster and finds the installed Airunway resources', async () => {
    const response = await requestWithTimeout('/api/cluster/status');
    expect(response.status).toBe(200);

    const status = await response.json();
    expect(status.connected).toBe(true);
    expect(status.providerInstallation?.installed).toBe(true);

    const kaito = await withTimeout(
      'reading the kaito provider registration',
      kubernetesService.getInferenceProviderConfig('kaito'),
    );
    expect(kaito).not.toBeNull();
    expect(kaito?.status?.ready).toBe(true);
  }, TEST_TIMEOUT_MS);

  test('returns the registered and healthy KAITO runtime', async () => {
    const response = await requestWithTimeout('/api/runtimes/status');
    expect(response.status).toBe(200);

    const body = await response.json();
    expect(Array.isArray(body.runtimes)).toBe(true);
    const kaito = body.runtimes.find((runtime: { id?: string }) => runtime.id === 'kaito');
    expect(kaito).toBeDefined();
    expect(kaito.installed).toBe(true);
    expect(kaito.healthy).toBe(true);
  }, TEST_TIMEOUT_MS);

  test('executes deployment listing against the real API server', async () => {
    const deployments = await withTimeout(
      'listing deployments from the Kubernetes API',
      kubernetesService.listDeployments(),
    );
    expect(Array.isArray(deployments)).toBe(true);

    const response = await requestWithTimeout('/api/deployments');
    expect(response.status).toBe(200);
    const body = await response.json();
    expect(Array.isArray(body.deployments)).toBe(true);
    expect(body.pagination).toBeDefined();
  }, TEST_TIMEOUT_MS);

  test('reads secret status without accepting an HTTP 500 fallback', async () => {
    const response = await requestWithTimeout('/api/secrets/huggingface/status');
    expect(response.status).toBe(200);

    const body = await response.json();
    expect(typeof body.configured).toBe('boolean');
    expect(Array.isArray(body.namespaces)).toBe(true);
  }, TEST_TIMEOUT_MS);
});
