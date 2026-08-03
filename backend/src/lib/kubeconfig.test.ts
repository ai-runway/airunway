import { describe, it, expect, beforeEach, afterEach } from 'bun:test';
import * as k8s from '@kubernetes/client-node';
import type * as https from 'node:https';
import { kubeConfigToBunTls, BunTlsHttpLibrary, makeApiClient, type BunTlsOptions } from './kubeconfig';

/**
 * Regression guards for the Bun TLS shim (`kubeConfigToBunTls`,
 * `BunTlsHttpLibrary`, `makeApiClient`). This is security-sensitive auth/TLS
 * code: the most important assertion in this file is that the **default path
 * never disables certificate verification** — see `does NOT set
 * rejectUnauthorized on the default path`.
 *
 * Tests build real `KubeConfig` objects via `loadFromOptions` (no disk/network)
 * rather than mocking SDK internals, so they keep working across SDK patch bumps.
 */

// PEM-shaped placeholders. `applyToHTTPSOptions` only base64-decodes the *Data
// fields into Buffers and copies them; it does not parse/validate the contents.
const CA_PEM = '-----BEGIN CERTIFICATE-----\nMIIBfakeCApem\n-----END CERTIFICATE-----\n';
const CERT_PEM = '-----BEGIN CERTIFICATE-----\nMIIBfakeClientpem\n-----END CERTIFICATE-----\n';
const KEY_PEM = '-----BEGIN PRIVATE KEY-----\nMIIBfakeKeypem\n-----END PRIVATE KEY-----\n';

const b64 = (s: string) => Buffer.from(s).toString('base64');

interface KcOpts {
  skipTLSVerify?: boolean;
  tlsServerName?: string;
  token?: string;
  withClientCert?: boolean;
  caData?: string | null;
}

function makeKubeConfig(opts: KcOpts = {}): k8s.KubeConfig {
  const kc = new k8s.KubeConfig();
  const cluster: Record<string, unknown> = {
    name: 'test-cluster',
    server: 'https://api.example.test:443',
    skipTLSVerify: !!opts.skipTLSVerify,
  };
  if (opts.caData !== null) cluster.caData = opts.caData ?? b64(CA_PEM);
  if (opts.tlsServerName) cluster.tlsServerName = opts.tlsServerName;

  const user: Record<string, unknown> = { name: 'test-user' };
  if (opts.token) user.token = opts.token;
  if (opts.withClientCert) {
    user.certData = b64(CERT_PEM);
    user.keyData = b64(KEY_PEM);
  }

  kc.loadFromOptions({
    clusters: [cluster as unknown as k8s.Cluster],
    users: [user as unknown as k8s.User],
    contexts: [{ name: 'test-ctx', cluster: 'test-cluster', user: 'test-user' }],
    currentContext: 'test-ctx',
  });
  return kc;
}

describe('kubeConfigToBunTls', () => {
  it('maps ca/cert/key into the Bun tls option', async () => {
    const tls = await kubeConfigToBunTls(makeKubeConfig({ withClientCert: true }));
    expect(tls).toBeDefined();
    expect(Buffer.isBuffer(tls!.ca)).toBe(true);
    expect(Buffer.isBuffer(tls!.cert)).toBe(true);
    expect(Buffer.isBuffer(tls!.key)).toBe(true);
    expect(tls!.ca!.toString()).toBe(CA_PEM);
  });

  it('maps the kubeconfig SNI (servername) to Bun camelCase serverName', async () => {
    const tls = await kubeConfigToBunTls(makeKubeConfig({ token: 'tok', tlsServerName: 'sni.override.test' }));
    expect(tls).toBeDefined();
    expect(tls!.serverName).toBe('sni.override.test');
  });

  it('does NOT set serverName when the kubeconfig has no tls-server-name', async () => {
    const tls = await kubeConfigToBunTls(makeKubeConfig({ token: 'tok' }));
    // ca is still present, so tls is defined; serverName must be absent.
    expect(tls).toBeDefined();
    expect(tls!.serverName).toBeUndefined();
  });

  it('sets rejectUnauthorized=false when skipTLSVerify is true', async () => {
    const tls = await kubeConfigToBunTls(makeKubeConfig({ token: 'tok', skipTLSVerify: true }));
    expect(tls).toBeDefined();
    expect(tls!.rejectUnauthorized).toBe(false);
  });

  // *** KEY SECURITY REGRESSION GUARD ***
  it('does NOT set rejectUnauthorized on the default (verifying) path', async () => {
    const tls = await kubeConfigToBunTls(makeKubeConfig({ token: 'tok' }));
    expect(tls).toBeDefined();
    // Must be absent (not `true`, not `false`) so Bun keeps verification ON.
    expect(tls!.rejectUnauthorized).toBeUndefined();
    expect(Object.prototype.hasOwnProperty.call(tls!, 'rejectUnauthorized')).toBe(false);
  });

  it('returns undefined when the kubeconfig configures no TLS material', async () => {
    // No CA, no client cert, token auth, verification left on → nothing to map.
    const tls = await kubeConfigToBunTls(makeKubeConfig({ token: 'tok', caData: null }));
    expect(tls).toBeUndefined();
  });
});

describe('BunTlsHttpLibrary.send', () => {
  const realFetch = globalThis.fetch;
  let calls: Array<{ url: string; options: RequestInit & { tls?: BunTlsOptions } }>;

  beforeEach(() => {
    calls = [];
  });
  afterEach(() => {
    globalThis.fetch = realFetch;
  });

  function stubFetch(status = 200, body = '{"ok":true}') {
    globalThis.fetch = (async (url: RequestInfo | URL, options: RequestInit & { tls?: BunTlsOptions }) => {
      calls.push({ url: String(url), options });
      return new Response(body, { status, headers: { 'content-type': 'application/json' } });
    }) as typeof fetch;
  }

  function makeRequest(): k8s.RequestContext {
    const req = new k8s.RequestContext('https://api.example.test:443/healthz', k8s.HttpMethod.GET);
    // Mirror how the generated client applies auth before send() runs.
    req.setHeaderParam('Authorization', 'Bearer test-token-123');
    return req;
  }

  it('passes the mapped tls material to fetch', async () => {
    stubFetch();
    const lib = new BunTlsHttpLibrary(makeKubeConfig({ withClientCert: true, tlsServerName: 'sni.test' }));
    await lib.send(makeRequest()).toPromise();

    expect(calls).toHaveLength(1);
    const tls = calls[0].options.tls;
    expect(tls).toBeDefined();
    expect(Buffer.isBuffer(tls.ca)).toBe(true);
    expect(tls.serverName).toBe('sni.test');
  });

  it('preserves the Authorization header applied upstream (auth survives the override)', async () => {
    stubFetch();
    const lib = new BunTlsHttpLibrary(makeKubeConfig({ token: 'ignored' }));
    await lib.send(makeRequest()).toPromise();

    const headers = new Headers(calls[0].options.headers);
    expect(headers.get('Authorization')).toBe('Bearer test-token-123');
  });

  it('omits tls entirely when the kubeconfig configures none', async () => {
    stubFetch();
    const lib = new BunTlsHttpLibrary(makeKubeConfig({ token: 'tok', caData: null }));
    await lib.send(makeRequest()).toPromise();
    expect(calls[0].options.tls).toBeUndefined();
  });

  it('returns a ResponseContext (not a thrown error) for a non-2xx response', async () => {
    stubFetch(404, '{"kind":"Status","code":404}');
    const lib = new BunTlsHttpLibrary(makeKubeConfig({ token: 'tok' }));
    const res = await lib.send(makeRequest()).toPromise();

    expect(res).toBeInstanceOf(k8s.ResponseContext);
    expect(res.httpStatusCode).toBe(404);
    expect(await res.body.text()).toContain('Status');
  });

  it('resolves the kubeconfig TLS material only once across multiple requests', async () => {
    stubFetch();
    const kc = makeKubeConfig({ withClientCert: true });
    let applyCount = 0;
    const orig = kc.applyToHTTPSOptions.bind(kc);
    kc.applyToHTTPSOptions = (async (opts: https.RequestOptions) => {
      applyCount += 1;
      return orig(opts);
    }) as typeof kc.applyToHTTPSOptions;

    const lib = new BunTlsHttpLibrary(kc);
    await lib.send(makeRequest()).toPromise();
    await lib.send(makeRequest()).toPromise();
    await lib.send(makeRequest()).toPromise();

    // Cached after the first call — guards against re-running the auth/cert
    // pipeline (e.g. exec credential plugins) on every request.
    expect(applyCount).toBe(1);
  });
});

describe('makeApiClient', () => {
  it('builds a typed API client wired to the Bun TLS http library', () => {
    const api = makeApiClient(makeKubeConfig({ token: 'tok' }), k8s.CoreV1Api);
    expect(api).toBeInstanceOf(k8s.CoreV1Api);
  });

  it('throws when the kubeconfig has no active cluster', () => {
    const kc = new k8s.KubeConfig();
    expect(() => makeApiClient(kc, k8s.CoreV1Api)).toThrow('No active cluster!');
  });
});
