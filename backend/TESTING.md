# Backend Testing Guide

## Quick Start

```bash
# Run all deterministic unit and route tests
cd backend
bun run test

# Watch mode (re-runs on file changes)
bun run test:watch

# With coverage report
bun run test:coverage

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

#### 3. Strict Kubernetes integration tests

Real-cluster tests live only in `src/integration/kubernetes.integration.ts` and are excluded
from the deterministic unit and coverage commands. They require explicit strict mode and fail
on timeouts, HTTP 500 responses, missing Airunway resources, or unhealthy providers.

Run them only inside the dedicated disposable-cluster workflow:

```bash
cd backend
bun run test:integration
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

Runs `bun run test:coverage` against the backend with no cluster. The package script excludes
`src/integration`, so this lane is deterministic and never converts cluster failures into skips.
Coverage summary is posted to the GitHub Actions step summary.

### `e2e-backend.yml` — Full integration (every PR)

1. Creates a Kind cluster
2. Installs KAITO operator via Helm
3. Builds and deploys the AI Runway controller + KAITO provider into the cluster
4. Runs `cd backend && bun run test:integration` with explicit strict mode

The local suite stays fast and mocked, while the integration suite fails closed when the
disposable cluster does not execute the expected real Kubernetes behavior.
