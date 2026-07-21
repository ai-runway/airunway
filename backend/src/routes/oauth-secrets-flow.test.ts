import { describe, test, expect, afterEach } from 'bun:test';
import app from '../hono-app';
import { secretsService } from '../services/secrets';
import { kubernetesService } from '../services/kubernetes';
import { configService } from '../services/config';
import { mockServiceMethod, mockFetchByUrl } from '../test/helpers';
import {
  mockHfSecretStatusConfigured,
  mockHfSecretStatusEmpty,
  mockHfDistributeResult,
  mockHfDeleteResult,
  mockGpuCapacity,
} from '../test/fixtures';

describe('OAuth → Secrets → Deploy Flow', () => {
  const restores: (() => void)[] = [];

  afterEach(() => {
    // Restore in LIFO order so nested mocks of the same method unwind correctly
    restores.reverse().forEach((r) => r());
    restores.length = 0;
  });

  test('full flow: OAuth start → token exchange → save secret → deploy → cleanup', async () => {
    // ---- Step 1: Get OAuth config ----
    const configRes = await app.request('/api/oauth/huggingface/config');
    expect(configRes.status).toBe(200);
    const config = await configRes.json();
    expect(typeof config.clientId).toBe('string');
    expect(config.clientId.length).toBeGreaterThan(0);
    expect(config.authorizeUrl).toContain('/oauth/authorize');
    expect(config.scopes).toContain('openid');

    // ---- Step 2: Start OAuth flow (PKCE) ----
    const startRes = await app.request('/api/oauth/huggingface/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ redirectUri: 'http://localhost:3000/callback' }),
    });
    expect(startRes.status).toBe(200);
    const startData = await startRes.json();
    expect(startData.authorizationUrl).toContain('/oauth/authorize');
    expect(startData.authorizationUrl).toContain('code_challenge');
    expect(startData.state).toBeDefined();

    const { state } = startData;

    // ---- Step 3: Verifier endpoint is deprecated (returns 410) ----
    const verifierRes = await app.request(`/api/oauth/huggingface/verifier/${state}`);
    expect(verifierRes.status).toBe(410);

    // ---- Step 4: Exchange code for token using server-side verifier ----
    // The new /token-with-state endpoint keeps the PKCE verifier server-side
    restores.push(
      mockFetchByUrl({
        '/oauth/token': {
          body: { access_token: 'hf_test_token_abc123', expires_in: 3600, scope: 'openid profile read-repos' },
        },
        '/api/whoami-v2': {
          body: { id: 'user-123', name: 'testuser', fullname: 'Test User', email: 'test@example.com', avatarUrl: 'https://huggingface.co/avatars/testuser.png' },
        },
      }),
    );

    const tokenRes = await app.request('/api/oauth/huggingface/token-with-state', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        code: 'auth-code-123',
        state,
      }),
    });
    expect(tokenRes.status).toBe(200);
    const tokenData = await tokenRes.json();
    expect(tokenData.accessToken).toBe('hf_test_token_abc123');
    expect(tokenData.user.name).toBe('testuser');

    // ---- Step 6: Save HuggingFace secret ----
    // validateToken calls fetch(HF_WHOAMI_URL) — fetch mock for /api/whoami-v2 still active
    restores.push(
      mockServiceMethod(
        secretsService,
        'distributeHfSecret',
        (async () => mockHfDistributeResult) as typeof secretsService.distributeHfSecret,
      ),
    );

    const saveRes = await app.request('/api/secrets/huggingface', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ accessToken: tokenData.accessToken }),
    });
    expect(saveRes.status).toBe(200);
    const saveData = await saveRes.json();
    expect(saveData.success).toBe(true);
    expect(saveData.user.name).toBe('testuser');
    expect(saveData.results).toHaveLength(5);
    expect(saveData.results.map((result: { namespace: string }) => result.namespace)).toContain('ray-system');

    // Restore fetch and service mocks before next steps that use different mocks.
    // Without this, the fetch mock from Step 5 would intercept requests in later steps
    // and the distributeHfSecret mock would leak into Step 7's getHfSecretStatus call.
    // Reverse for LIFO order so nested mocks of the same method unwind correctly.
    restores.reverse().forEach((r) => r());
    restores.length = 0;

    // ---- Step 7: Verify secret status ----
    restores.push(
      mockServiceMethod(
        secretsService,
        'getHfSecretStatus',
        (async () => mockHfSecretStatusConfigured) as typeof secretsService.getHfSecretStatus,
      ),
    );

    const statusRes = await app.request('/api/secrets/huggingface/status');
    expect(statusRes.status).toBe(200);
    const statusData = await statusRes.json();
    expect(statusData.configured).toBe(true);
    expect(statusData.user.name).toBe('testuser');

    // ---- Step 8: Create deployment with gated model ----
    restores.push(
      mockServiceMethod(configService, 'getDefaultNamespace', async () => 'default'),
    );
    restores.push(
      mockServiceMethod(kubernetesService, 'createDeployment', async () => {}),
    );
    restores.push(
      mockServiceMethod(
        kubernetesService,
        'getClusterGpuCapacity',
        (async () => mockGpuCapacity) as typeof kubernetesService.getClusterGpuCapacity,
      ),
    );

    const deployRes = await app.request('/api/deployments', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        name: 'gated-model-deploy',
        modelId: 'meta-llama/Llama-3.1-8B-Instruct',
        engine: 'vllm',
      }),
    });
    expect(deployRes.status).toBe(201);
    const deployData = await deployRes.json();
    expect(deployData.name).toBe('gated-model-deploy');

    // ---- Step 9: Delete secrets ----
    restores.push(
      mockServiceMethod(
        secretsService,
        'deleteHfSecrets',
        (async () => mockHfDeleteResult) as typeof secretsService.deleteHfSecrets,
      ),
    );

    const deleteRes = await app.request('/api/secrets/huggingface', { method: 'DELETE' });
    expect(deleteRes.status).toBe(200);
    const deleteData = await deleteRes.json();
    expect(deleteData.success).toBe(true);

    // ---- Step 10: Verify secrets removed ----
    restores.push(
      mockServiceMethod(
        secretsService,
        'getHfSecretStatus',
        (async () => mockHfSecretStatusEmpty) as typeof secretsService.getHfSecretStatus,
      ),
    );

    const statusRes2 = await app.request('/api/secrets/huggingface/status');
    expect(statusRes2.status).toBe(200);
    const statusData2 = await statusRes2.json();
    expect(statusData2.configured).toBe(false);
  });

  // ==========================================================================
  // Edge cases
  // ==========================================================================

  test('POST /token-with-state preserves PKCE state after transient token exchange failure', async () => {
    const startRes = await app.request('/api/oauth/huggingface/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ redirectUri: 'http://localhost:3000/callback' }),
    });
    expect(startRes.status).toBe(200);
    const startData = await startRes.json();
    const { state } = startData;

    const restoreFailure = mockFetchByUrl({
      '/oauth/token': {
        body: { error: 'temporarily_unavailable' },
        ok: false,
        status: 500,
      },
    });
    restores.push(restoreFailure);

    const firstRes = await app.request('/api/oauth/huggingface/token-with-state', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        code: 'auth-code-transient-failure',
        state,
      }),
    });
    expect(firstRes.status).toBe(400);

    restoreFailure();
    restores.pop();

    restores.push(
      mockFetchByUrl({
        '/oauth/token': {
          body: { access_token: 'hf_retry_token_abc123', expires_in: 3600, scope: 'openid profile read-repos' },
        },
        '/api/whoami-v2': {
          body: { id: 'user-123', name: 'testuser', fullname: 'Test User', email: 'test@example.com', avatarUrl: 'https://huggingface.co/avatars/testuser.png' },
        },
      }),
    );

    const retryRes = await app.request('/api/oauth/huggingface/token-with-state', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        code: 'auth-code-retry',
        state,
      }),
    });
    expect(retryRes.status).toBe(200);
    const retryData = await retryRes.json();
    expect(retryData.accessToken).toBe('hf_retry_token_abc123');
    expect(retryData.user.name).toBe('testuser');
  });

  test('POST /token with invalid code returns 400', async () => {
    // Mock fetch to return an error from HuggingFace token endpoint
    restores.push(
      mockFetchByUrl({
        '/oauth/token': {
          body: { error: 'invalid_grant', error_description: 'Invalid code' },
          ok: false,
          status: 400,
        },
      }),
    );

    const res = await app.request('/api/oauth/huggingface/token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        code: 'invalid-code',
        codeVerifier: 'a'.repeat(43),
        redirectUri: 'http://localhost:3000/callback',
      }),
    });
    expect(res.status).toBe(400);
  });

  test('POST /secrets/huggingface with invalid token returns 400', async () => {
    // Mock fetch to return 401 from whoami endpoint (invalid token)
    restores.push(
      mockFetchByUrl({
        '/api/whoami-v2': {
          body: { error: 'Invalid username or password.' },
          ok: false,
          status: 401,
        },
      }),
    );

    const res = await app.request('/api/secrets/huggingface', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ accessToken: 'invalid-token' }),
    });
    expect(res.status).toBe(400);
  });

  test('GET /verifier/:state returns 410 (deprecated endpoint)', async () => {
    const res = await app.request('/api/oauth/huggingface/verifier/nonexistent-state');
    expect(res.status).toBe(410);
  });

  test('POST /secrets/huggingface with empty body returns 400', async () => {
    const res = await app.request('/api/secrets/huggingface', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({}),
    });
    expect(res.status).toBe(400);
  });
});
