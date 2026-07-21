# Backend Testing Guide

## Quick Start

```bash
# Run all tests
cd backend && bun test

# Watch mode (re-runs on file changes)
bun test --watch

# With coverage report
bun test --coverage

# Run a single test file
bun test src/routes/deployments.test.ts

# Run tests matching a pattern
bun test --grep "deployment lifecycle"
```

## Test Architecture

The backend uses [Bun's built-in test runner](https://bun.sh/docs/cli/test) with
[Hono's `app.request()`](https://hono.dev/docs/api/hono#request) for in-process HTTP
testing. Tests are co-located with source files using the `*.test.ts` naming convention.

### Three test patterns

#### 1. Route tests with mocked services (most common)

Import the Hono `app` and use `app.request()` to make HTTP requests without starting a
real server. Service singletons are monkey-patched via `mockServiceMethod()`.

```typescript
import app from '../hono-app';
import { autoscalerService } from '../services/autoscaler';
import { mockServiceMethod } from '../test/helpers';
import { autoscalerDetectionAKS } from '../test/fixtures';

test('returns autoscaler detection', async () => {
  const restore = mockServiceMethod(
    autoscalerService,
    'detectAutoscaler',
    async () => autoscalerDetectionAKS,
  );

  const res = await app.request('/api/autoscaler/detection');
  expect(res.status).toBe(200);

  restore();
});
```

This tests the full Hono middleware chain (auth, CORS, compression, error handling,
serialization) without network overhead.

#### 2. Pure unit tests

Direct function/class testing for services and library utilities:

```typescript
import { namespaceSchema } from './validation';

test('accepts valid namespaces', () => {
  expect(namespaceSchema.safeParse('default').success).toBe(true);
});
```

#### 3. K8s-tolerant tests

For tests that can optionally run against a real cluster. These use `withTimeout` to
gracefully skip when no cluster is available:

```typescript
import { withTimeout, K8S_TEST_TIMEOUT } from '../test/helpers';

test('gets real cluster status', async () => {
  try {
    const res = await withTimeout(app.request('/api/health'), K8S_TEST_TIMEOUT);
    expect(res.status).toBe(200);
  } catch (error) {
    if (error instanceof Error && error.message.includes('timed out')) {
      console.log('Skipping: no cluster available');
      return;
    }
    throw error;
  }
});
```

### Multi-step flow tests

Flow tests chain sequential `app.request()` calls within a single `test()` block to
exercise cross-route interactions. Mocks are re-pushed between steps to simulate state
changes.

See these files for examples:
- `src/routes/lifecycle.test.ts` — deployment create → get → delete → verify (predates this test infrastructure; uses inline mocks)
- `src/routes/oauth-secrets-flow.test.ts` — OAuth → secrets → deploy → cleanup
- `src/routes/provider-installation-flow.test.ts` — GPU check → install → verify → uninstall

Pattern:
```typescript
const restores: (() => void)[] = [];

afterEach(() => {
  restores.forEach((r) => r());
  restores.length = 0;
});

test('multi-step flow', async () => {
  // Step 1: mock + request + assert
  restores.push(mockServiceMethod(service, 'method', async () => stateA));
  const res1 = await app.request('/api/step1');
  expect(res1.status).toBe(200);

  // Step 2: re-mock to simulate state change
  restores.push(mockServiceMethod(service, 'method', async () => stateB));
  const res2 = await app.request('/api/step2');
  expect(res2.status).toBe(200);
});
```

## Gotchas

### Never `delete require.cache` on a shared-singleton service module

Do **not** reset test state by reloading a service module, e.g.:

```typescript
// ❌ Don't do this — forks the shared singleton.
delete require.cache[require.resolve('./huggingface')];
const { huggingFaceService } = await import('./huggingface');
```

Services are exported as singletons (`export const fooService = new FooService()`).
Other modules — including route handlers — capture that instance at import time.
Reloading the module installs a *different* instance in the registry, so a test
mocking the reloaded copy patches an object the route never calls. Because test
files run in a shared worker and their order isn't deterministic, this surfaces
as an order-dependent, CI-only flake (see PR #358).

Instead, reset internal state through an explicit test-only method on the
singleton and keep a single static import:

```typescript
// ✅ Static import + explicit state reset.
import { huggingFaceService } from './huggingface';

beforeEach(() => {
  huggingFaceService.clearArchitectureCacheForTests();
});
```

Note that services reading `global.fetch` at call time only need the global
mocked (via `mockFetch` / `mockFetchByUrl`) — no module reload is required to
intercept their network calls.

## Test Helpers

Located in `src/test/helpers.ts`:

| Helper | Purpose |
|--------|---------|
| `mockServiceMethod(service, method, impl)` | Replace a method on a service singleton. Returns a restore function. |
| `mockFetch(response, options?)` | Replace `globalThis.fetch` with a static response. Returns a restore function. |
| `mockFetchByUrl(routes)` | Replace `globalThis.fetch` with URL-based routing. **First substring match wins** — list more-specific patterns before less-specific ones (e.g. `/api/whoami-v2` before `/api/whoami`). Returns a restore function. |
| `withTimeout(promise, ms)` | Race a promise against a timeout. Used for K8s-tolerant tests. |
| `K8S_TEST_TIMEOUT` | Default timeout (2000ms) for K8s-dependent tests. |

## Fixtures

Located in `src/test/fixtures.ts`. Shared mock data organized by domain:

- **Autoscaler**: `autoscalerDetectionAKS`, `autoscalerDetectionCA`, `autoscalerDetectionNone`, `autoscalerStatus`
- **AI Configurator**: `aiConfiguratorStatusAvailable`, `aiConfiguratorStatusUnavailable`, `aiConfiguratorSuccessResult`
- **Deployments**: `mockDeployment`, `mockDeploymentWithPendingPod`, `mockDeploymentManifest`, `mockPod`, `mockPendingPod`
- **HuggingFace OAuth**: `mockHfUser`, `mockHfTokenExchange`, `mockHfTokenValidation`, `mockHfTokenValidationInvalid`
- **HuggingFace Secrets**: `mockHfSecretStatusConfigured`, `mockHfSecretStatusEmpty`, `mockHfDistributeResult`, `mockHfDeleteResult`
- **GPU & Installation**: `mockGpuCapacity`, `mockGpuCapacityEmpty`, `mockDetailedGpuCapacity`, `mockGpuOperatorStatus`
- **Helm**: `mockHelmAvailable`, `mockHelmUnavailable`, `mockProviderInstallResult`, `mockProviderUninstallResult`
- **Provider Config**: `mockInferenceProviderConfig`, `mockInferenceProviderConfigNotReady`
- **Pod Failures**: `mockPodFailureReasons`

When adding new fixtures, follow the existing pattern: typed exports grouped under a
comment header for the domain.

## CI Integration

Tests run in two CI workflows:

### `test.yml` — Unit + route tests (every PR)

Runs `bun test --coverage` against the backend with no cluster. K8s-tolerant tests
gracefully skip via the timeout pattern. Coverage summary is posted to the GitHub Actions
step summary.

### `e2e-backend.yml` — Full integration (every PR)

1. Creates a Kind cluster
2. Installs KAITO operator via Helm
3. Builds and deploys the AI Runway controller + KAITO provider into the cluster
4. Runs the same `bun test` suite — K8s-tolerant tests now succeed against the real cluster

This means the same test files serve both local development (fast, mocked) and CI
integration (full stack, real K8s).
