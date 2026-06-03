import { Hono } from 'hono';
import { cors } from 'hono/cors';
import { compress } from 'hono/compress';
import { HTTPException } from 'hono/http-exception';

import { authService } from './services/auth';
import logger from './lib/logger';
import {
  isCompiled,
  loadStaticFiles,
  getStaticFileResponse,
  getIndexHtmlResponse,
  hasStaticFiles,
} from './static';
import type { UserInfo } from '@airunway/shared';
import type { AppEnv } from './types/hono';

// Import route modules
import {
  health,
  models,
  settings,
  deployments,
  installation,
  oauth,
  secrets,
  autoscaler,
  runtimes,
  aikit,
  aiconfigurator,
  costs,
  gateway,
} from './routes';

// Load static files at startup
await loadStaticFiles();

const compiled = isCompiled();
logger.info(
  { mode: compiled ? 'compiled' : 'development' },
  `🔧 Running in ${compiled ? 'compiled binary' : 'development'} mode`
);

// ============================================================================
// Main App
// ============================================================================

const LOOPBACK_HOSTS = new Set(['localhost', '127.0.0.1', '::1']);
type CorsOriginOption = NonNullable<Parameters<typeof cors>[0]>['origin'];
const AIRUNWAY_AUTH_ERROR_HEADER = 'X-Airunway-Auth-Error';

// Default cross-origin policy for browser-based UIs that talk to this backend.
// Same-origin clients (the embedded production frontend) don't go through CORS.
// When CORS_ORIGIN is unset, allow local loopback origins so Vite and Headlamp
// Desktop/browser plugin development work out of the box without reopening CORS
// to arbitrary internet origins.
const CORS_ORIGIN = process.env.CORS_ORIGIN;

// Parse CORS_ORIGIN into a value the cors middleware can use:
//   - undefined           → allow loopback browser origins by default
//   - "*"               → pass through as a string (explicit wildcard)
//   - "a,b,c"           → array of trimmed, non-empty origins
//   - malformed/empty     → fall back to the safe default rather than '*' so
//                           that a misconfigured production env can't silently
//                           fail open to wildcard CORS.
// Splitting "*" into ["*"] matches request origins literally, which never
// equals a real origin and effectively disables CORS — so handle it explicitly.
export function parseCorsOrigin(raw: string): CorsOriginOption {
  const trimmed = raw.trim();
  if (trimmed === '*') return '*';
  const list = trimmed
    .split(',')
    .map((o) => o.trim())
    .filter((o) => o.length > 0);
  if (list.length > 0) return list;
  // Fail closed: a malformed CORS_ORIGIN (e.g. ",,") should keep the secure
  // default rather than broaden access to '*'.
  logger.warn(
    { rawCorsOrigin: raw },
    'CORS_ORIGIN is set but parses to no origins; falling back to loopback origins',
  );
  return defaultCorsOrigin;
}

function isLoopbackOrigin(origin: string): boolean {
  try {
    const url = new URL(origin);
    const hostname = url.hostname.startsWith('[') && url.hostname.endsWith(']')
      ? url.hostname.slice(1, -1)
      : url.hostname;
    return (url.protocol === 'http:' || url.protocol === 'https:') && LOOPBACK_HOSTS.has(hostname);
  } catch {
    return false;
  }
}

function defaultCorsOrigin(origin: string): string | null {
  if (isLoopbackOrigin(origin)) {
    return origin;
  }
  return null;
}

const app = new Hono<AppEnv>();

// Global middleware
app.use('*', compress());
app.use(
  '*',
  cors({
    origin: CORS_ORIGIN === undefined ? defaultCorsOrigin : parseCorsOrigin(CORS_ORIGIN),
    exposeHeaders: [AIRUNWAY_AUTH_ERROR_HEADER],
  })
);

// Request logging
app.use('*', async (c, next) => {
  logger.info({ method: c.req.method, url: c.req.url }, `${c.req.method} ${c.req.path}`);
  await next();
});

// ============================================================================
// Auth Middleware
// ============================================================================

// Routes that don't require authentication
// Keep this list minimal — only routes needed before login
const PUBLIC_ROUTES = [
  '/api/cluster/status',
  '/api/settings',      // Settings is public (read-only auth config needed by frontend)
  '/api/oauth',         // OAuth routes must be public for initial authentication
];

// Public routes that must match exactly (no sub-path matching)
const PUBLIC_ROUTES_EXACT = [
  '/api/health',
  '/api/health/',
  '/api/health/version',
];

// Auth middleware for protected API routes
app.use('/api/*', async (c, next) => {
  // Skip auth if not enabled
  if (!authService.isAuthEnabled()) {
    return next();
  }

  // Skip auth for exact-match public routes
  const path = c.req.path;
  if (PUBLIC_ROUTES_EXACT.includes(path)) {
    return next();
  }

  // Skip auth for prefix-match public routes (cluster/status, settings, oauth)
  if (PUBLIC_ROUTES.some(route => path === route || path.startsWith(route + '/'))) {
    return next();
  }

  // Extract bearer token
  const authHeader = c.req.header('Authorization');
  if (!authHeader || !authHeader.startsWith('Bearer ')) {
    c.header(AIRUNWAY_AUTH_ERROR_HEADER, 'true');
    return c.json(
      { error: { message: 'Authentication required', statusCode: 401 } },
      401
    );
  }

  const token = authHeader.slice(7); // Remove 'Bearer ' prefix

  // Validate token via Kubernetes TokenReview
  const result = await authService.validateToken(token);

  if (!result.valid) {
    logger.warn({ error: result.error }, 'Token validation failed');
    c.header(AIRUNWAY_AUTH_ERROR_HEADER, 'true');
    return c.json(
      { error: { message: result.error || 'Invalid token', statusCode: 401 } },
      401
    );
  }

  // Attach user info and raw token to context
  c.set('user', result.user as UserInfo);
  c.set('token', token);
  logger.debug({ username: result.user?.username }, 'Authenticated request');

  return next();
});

// API Routes
app.route('/api/health', health);
app.route('/api/cluster', health);
app.route('/api/models', models);
app.route('/api/settings', settings);
app.route('/api/deployments', deployments);
app.route('/api/installation', installation);
app.route('/api/oauth', oauth);
app.route('/api/secrets', secrets);
app.route('/api/autoscaler', autoscaler);
app.route('/api/runtimes', runtimes);
app.route('/api/aikit', aikit);
app.route('/api/aiconfigurator', aiconfigurator);
app.route('/api/costs', costs);
app.route('/api/gateway', gateway);

// Static file serving middleware - uses Bun.file() for zero-copy serving
app.use('*', async (c, next) => {
  if (c.req.path.startsWith('/api/')) {
    return next();
  }

  if (hasStaticFiles()) {
    const response = getStaticFileResponse(c.req.path);
    if (response) {
      return response;
    }
  }

  return next();
});

// SPA fallback
app.notFound((c) => {
  // If it's an API route that wasn't matched, return 404 JSON
  if (c.req.path.startsWith('/api/')) {
    logger.warn(
      { method: c.req.method, url: c.req.url, statusCode: 404 },
      `No route matched: ${c.req.method} ${c.req.url}`
    );
    return c.json(
      { error: { message: `Route not found: ${c.req.method} ${c.req.path}`, statusCode: 404 } },
      404
    );
  }

  // Serve index.html for SPA routing - uses Bun.file() for zero-copy serving
  if (hasStaticFiles()) {
    const response = getIndexHtmlResponse();
    if (response) {
      return response;
    }
  }

  return c.text('Frontend not available. Run with frontend build or in development mode.', 404);
});

// Global error handler
app.onError((err, c) => {
  logger.error({ error: err, stack: err.stack }, `Error: ${err.message}`);

  if (err instanceof HTTPException) {
    return c.json(
      {
        error: {
          message: err.status >= 500 ? 'Internal Server Error' : err.message,
          statusCode: err.status,
        },
      },
      err.status
    );
  }

  // Don't leak internal error details to clients
  return c.json(
    {
      error: {
        message: 'Internal Server Error',
        statusCode: 500,
      },
    },
    500
  );
});

// Export for RPC type inference
export type AppType = typeof app;

export default app;
