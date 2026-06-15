import * as k8s from '@kubernetes/client-node';
import type * as https from 'node:https';
import logger from './logger';

/**
 * Load a KubeConfig from the default location.
 *
 * When AUTH_ENABLED=true, client certificates are stripped from the current
 * user BEFORE any API client is created.  This is critical because Bun shares
 * TLS sessions process-wide: if *any* HTTP client establishes a connection
 * with admin client certificates, all subsequent requests to the same K8s API
 * server (including native `fetch`) inherit that TLS identity, causing the API
 * server to authenticate them as admin and ignore Bearer tokens.
 */
export function loadKubeConfig(): k8s.KubeConfig {
  const kc = new k8s.KubeConfig();

  try {
    kc.loadFromDefault();
  } catch {
    logger.warn('No kubeconfig found, using mock mode');
  }

  if (process.env.AUTH_ENABLED?.toLowerCase() === 'true' || process.env.AUTH_ENABLED === '1') {
    const currentUser = kc.getCurrentUser();
    if (currentUser) {
      // The k8s User fields are declared `readonly`, but we must clear the
      // client certificates before any API client is created. Cast to a
      // writable view of just those fields instead of using `any`.
      const writableUser = currentUser as {
        certData?: string;
        certFile?: string;
        keyData?: string;
        keyFile?: string;
      };
      writableUser.certData = undefined;
      writableUser.certFile = undefined;
      writableUser.keyData = undefined;
      writableUser.keyFile = undefined;
      logger.debug('Stripped client certificates from kubeconfig (AUTH_ENABLED)');
    }
  }

  return kc;
}

/**
 * TLS material in the shape Bun's native `fetch` understands via its
 * non-standard per-request `tls` option (see Bun's `TLSOptions`).
 *
 * Field names follow Bun, not Node: in particular SNI is `serverName`
 * (camelCase), whereas the kubeconfig/Node side spells it `servername`.
 */
export interface BunTlsOptions {
  ca?: Buffer;
  cert?: Buffer;
  key?: Buffer;
  passphrase?: string;
  serverName?: string;
  rejectUnauthorized?: boolean;
}

/**
 * Translate a `KubeConfig`'s TLS material into Bun's `fetch` `tls` option.
 *
 * This is the **single source of truth** for kubeconfig → Bun TLS mapping. Both
 * {@link BunTlsHttpLibrary} (typed-API clients) and `proxyServiceRequest`
 * (raw service-proxy `fetch`) must call it, so the two paths cannot drift —
 * e.g. one gaining SNI support while the other silently misses it.
 *
 * It asks the SDK for the same `https.Agent` options its Node path would use
 * (`applyToHTTPSOptions`, verified against `@kubernetes/client-node@1.4.0`
 * `config.js`), then re-maps the subset Bun honours:
 *   - `ca`/`cert`/`key`            — CA bundle + client cert/key
 *   - `passphrase`                 — passphrase for an encrypted client key
 *   - `servername` → `serverName`  — SNI / hostname-verification override
 *                                    (`cluster.tls-server-name`)
 *   - `rejectUnauthorized:false`   — only when `skipTLSVerify` (or the SDK)
 *                                    explicitly disabled verification; the
 *                                    default path leaves it unset so Bun keeps
 *                                    verification ON.
 *
 * KNOWN LIMITATIONS (kubeconfig features the SDK's Node agent supports but
 * Bun's `fetch` cannot express, so they are intentionally dropped here):
 *   - `pfx` (PKCS#12 client certs) — Bun's `TLSOptions` has no `pfx` field.
 *     PEM `cert`/`key` work; `.pfx`-based auth does not under Bun.
 *   - `cluster.proxy-url`          — Bun has no per-request proxy-agent option;
 *     kubeconfig-configured HTTP/SOCKS proxies are not honoured.
 *
 * @returns the mapped options, or `undefined` if the kubeconfig configured no
 *          TLS material at all (so callers can omit `tls` entirely).
 */
export async function kubeConfigToBunTls(kc: k8s.KubeConfig): Promise<BunTlsOptions | undefined> {
  // Ask the SDK for the Node `https.Agent` options it would build, then re-map
  // the subset Bun honours. `servername`/`pfx`/`passphrase` are populated by
  // the SDK even though our narrowed type only names what we forward.
  const httpsOptions: {
    ca?: Buffer;
    cert?: Buffer;
    key?: Buffer;
    passphrase?: string;
    servername?: string;
    rejectUnauthorized?: boolean;
  } = {};
  await kc.applyToHTTPSOptions(httpsOptions as https.RequestOptions);

  const tls: BunTlsOptions = {};
  if (httpsOptions.ca) tls.ca = httpsOptions.ca;
  if (httpsOptions.cert) tls.cert = httpsOptions.cert;
  if (httpsOptions.key) tls.key = httpsOptions.key;
  if (httpsOptions.passphrase) tls.passphrase = httpsOptions.passphrase;
  // Node spells SNI `servername`; Bun's `tls` option spells it `serverName`.
  if (httpsOptions.servername) tls.serverName = httpsOptions.servername;
  if (kc.getCurrentCluster()?.skipTLSVerify || httpsOptions.rejectUnauthorized === false) {
    tls.rejectUnauthorized = false;
  }

  return Object.keys(tls).length > 0 ? tls : undefined;
}

/**
 * Bun-compatible HTTP library for `@kubernetes/client-node`.
 *
 * WHY THIS EXISTS:
 * The client's default `IsomorphicFetchHttpLibrary` imports `node-fetch` and
 * passes the kubeconfig CA (and client cert/key) as a Node.js `https.Agent`
 * (`request.getAgent()`). Bun's runtime resolves `node-fetch` to its native
 * `fetch`, which **ignores** the Node `https.Agent` entirely — it only honours
 * TLS material supplied via the per-request `tls` option. The CA therefore never
 * reaches the TLS stack, so every request to a cluster whose API server uses a
 * private CA (e.g. AKS) fails with `UNABLE_TO_VERIFY_LEAF_SIGNATURE`.
 *
 * This subclass overrides `send()` to call Bun's native `fetch` directly,
 * translating the kubeconfig's TLS material (via {@link kubeConfigToBunTls})
 * into the `tls` option Bun understands. Auth headers (Bearer tokens, etc.) are
 * still applied by the generated client via `authMethods` before `send()` runs,
 * so we only need to re-inject the TLS material here. It shares the exact same
 * mapping helper as `proxyServiceRequest` in `kubernetes.ts`.
 *
 * The TLS material is resolved **once** and cached: `applyToHTTPSOptions` re-runs
 * the kubeconfig's full option pipeline (re-reading cert files from disk and, for
 * exec/OIDC users, re-invoking the auth plugin). Doing that per request would run
 * exec credentials twice on every call — once here and once in the auth pipeline
 * the generated client already applied. A KubeConfig's TLS material is fixed for
 * the lifetime of a client, so caching is safe; auth headers are unaffected
 * because they are applied upstream per request, not here.
 *
 * The response is wrapped exactly as the upstream library does: an `Observable`
 * constructed from a `Promise<ResponseContext>` (see `rxjsStub`), with a
 * `ResponseBody` exposing `text()` and `binary()`.
 */
export class BunTlsHttpLibrary extends k8s.IsomorphicFetchHttpLibrary {
  private tlsPromise?: Promise<BunTlsOptions | undefined>;

  constructor(private readonly kc: k8s.KubeConfig) {
    super();
  }

  /** Resolve the kubeconfig's Bun `tls` material once and memoise it. */
  private getTls(): Promise<BunTlsOptions | undefined> {
    if (!this.tlsPromise) {
      this.tlsPromise = kubeConfigToBunTls(this.kc);
    }
    return this.tlsPromise;
  }

  send(request: k8s.RequestContext): k8s.Observable<k8s.ResponseContext> {
    const responsePromise = (async (): Promise<k8s.ResponseContext> => {
      const tls = await this.getTls();

      // NOTE: the generated API classes used by this backend send only JSON or
      // empty bodies. The SDK's Node path can produce `form-data` multipart
      // bodies (e.g. some file-upload endpoints) which would NOT serialise under
      // Bun's `fetch`; if a future caller hits such an endpoint, body handling
      // here must be revisited.
      const fetchOptions: RequestInit & { tls?: BunTlsOptions } = {
        method: request.getHttpMethod().toString(),
        body: request.getBody() as BodyInit | undefined,
        headers: request.getHeaders(),
        signal: request.getSignal(),
      };
      if (tls) {
        fetchOptions.tls = tls;
      }

      const response = await fetch(request.getUrl(), fetchOptions);

      const headers: Record<string, string> = {};
      response.headers.forEach((value, name) => {
        headers[name] = value;
      });

      return new k8s.ResponseContext(response.status, headers, {
        text: () => response.text(),
        binary: async () => Buffer.from(await response.arrayBuffer()),
      });
    })();

    return new k8s.Observable<k8s.ResponseContext>(responsePromise);
  }
}

/**
 * Build a Kubernetes API client that works under Bun.
 *
 * Drop-in replacement for `kc.makeApiClient(ApiClass)`. It reproduces the SDK's
 * own `makeApiClient` wiring (`createConfiguration` with the kubeconfig as the
 * `default` auth method and a `ServerConfiguration` for the current cluster) but
 * swaps in {@link BunTlsHttpLibrary} so the kubeconfig CA is honoured on Bun's
 * native `fetch`.
 *
 * All backend services must construct their clients through this helper rather
 * than calling `kc.makeApiClient(...)` directly; otherwise requests to clusters
 * with a private CA fail with `UNABLE_TO_VERIFY_LEAF_SIGNATURE` under Bun.
 *
 * NOTE (SDK coupling): this hand-reproduces the SDK's own `makeApiClient` wiring
 * (`createConfiguration` + `ServerConfiguration`) and subclasses
 * `IsomorphicFetchHttpLibrary`. Verified against `@kubernetes/client-node@1.4.0`.
 * These are generated/internal surfaces — re-check this wiring (and the
 * `kubeConfigToBunTls` field mapping) when bumping the SDK across a major version.
 */
export function makeApiClient<T extends k8s.ApiType>(
  kc: k8s.KubeConfig,
  apiClientType: k8s.ApiConstructor<T>
): T {
  const cluster = kc.getCurrentCluster();
  if (!cluster) {
    throw new Error('No active cluster!');
  }

  const configuration = k8s.createConfiguration({
    baseServer: new k8s.ServerConfiguration(cluster.server, {}),
    authMethods: { default: kc },
    httpApi: new BunTlsHttpLibrary(kc),
  });

  return new apiClientType(configuration);
}
