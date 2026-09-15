import crypto from "node:crypto";
import http from "node:http";

export const MAX_CONCURRENT_REQUESTS = 32;
export const MAX_CONCURRENT_HEALTH_REQUESTS = 2;
export const HEADER_TIMEOUT_MS = 10_000;
export const REQUEST_TIMEOUT_MS = 30_000;
export const UPSTREAM_TIMEOUT_MS = 120_000;
export const SHUTDOWN_TIMEOUT_MS = 10_000;

const allowedRoutes = new Set([
  "GET /healthz",
  "GET /readyz",
  "GET /v1/models",
  "POST /v1/chat/completions",
]);
const socketRequestHandled = Symbol("airunwayRequestHandled");

function authorized(request, accessToken) {
  const value = request.headers.authorization || "";
  const prefix = "Bearer ";
  if (value.slice(0, prefix.length).toLowerCase() !== prefix.toLowerCase()) {
    return false;
  }
  const expected = crypto.createHash("sha256").update(accessToken).digest();
  const supplied = crypto.createHash("sha256").update(value.slice(prefix.length)).digest();
  return crypto.timingSafeEqual(expected, supplied);
}

function jsonError(response, status, message, headers = {}) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, {
    "content-type": "application/json",
    connection: "close",
    ...headers,
  });
  response.end(JSON.stringify({ error: { message } }));
}

export function createRuntimeProxy({
  accessToken,
  gatewayToken,
  internalPort,
  maxRequestBytes = 4 * 1024 * 1024,
  maxConcurrentRequests = MAX_CONCURRENT_REQUESTS,
  maxConcurrentHealthRequests = MAX_CONCURRENT_HEALTH_REQUESTS,
  headerTimeoutMs = HEADER_TIMEOUT_MS,
  requestTimeoutMs = REQUEST_TIMEOUT_MS,
  upstreamTimeoutMs = UPSTREAM_TIMEOUT_MS,
  connectionsCheckingInterval = 1_000,
}) {
  if (!Number.isInteger(maxConcurrentRequests) || maxConcurrentRequests < 1) {
    throw new Error("maxConcurrentRequests must be a positive integer");
  }
  if (!Number.isInteger(maxConcurrentHealthRequests) || maxConcurrentHealthRequests < 1) {
    throw new Error("maxConcurrentHealthRequests must be a positive integer");
  }
  if (headerTimeoutMs <= 0 || requestTimeoutMs < headerTimeoutMs || upstreamTimeoutMs <= 0) {
    throw new Error("proxy timeouts must be positive and requestTimeoutMs must cover headerTimeoutMs");
  }

  let activeWorkRequests = 0;
  let activeHealthRequests = 0;
  const server = http.createServer(
    {
      headersTimeout: headerTimeoutMs,
      requestTimeout: requestTimeoutMs,
      keepAliveTimeout: 5_000,
      connectionsCheckingInterval,
    },
    (request, response) => {
      // A connection may carry exactly one request. This prevents both
      // concurrent pipelining and serial keep-alive traffic from retaining a
      // slot in the global connection ceiling.
      if (request.socket[socketRequestHandled]) {
        request.socket.destroy();
        return;
      }
      request.socket[socketRequestHandled] = true;
      response.shouldKeepAlive = false;

      if (!allowedRoutes.has(`${request.method} ${request.url}`)) {
        jsonError(response, 404, "not found");
        return;
      }

      const healthRequest = request.url === "/healthz" || request.url === "/readyz";
      const chatRequest = request.method === "POST" && request.url === "/v1/chat/completions";
      if (!healthRequest && !authorized(request, accessToken)) {
        jsonError(response, 401, "valid bearer authentication is required", {
          "www-authenticate": "Bearer",
        });
        return;
      }
      if (chatRequest && request.headers["x-openclaw-session-key"] !== undefined) {
        jsonError(
          response,
          400,
          "x-openclaw-session-key is not supported; send conversation history in messages",
        );
        return;
      }
      if (request.headers["transfer-encoding"]) {
        jsonError(response, 400, "chunked request bodies are not supported");
        return;
      }
      let contentLength = 0;
      if (request.method === "POST") {
        const rawLength = request.headers["content-length"];
        contentLength = Number(rawLength);
        if (!rawLength || !Number.isInteger(contentLength) || contentLength < 1) {
          jsonError(response, 400, "a non-empty Content-Length is required");
          return;
        }
        if (contentLength > maxRequestBytes) {
          jsonError(response, 413, "request body exceeds 4 MiB");
          return;
        }
      }
      if (request.method === "GET") {
        const rawLength = request.headers["content-length"];
        if (rawLength !== undefined) {
          const getLength = Number(rawLength);
          if (!Number.isInteger(getLength) || getLength !== 0) {
            jsonError(response, 400, "GET request bodies are not supported");
            return;
          }
        }
      }

      const atCapacity = healthRequest
        ? activeHealthRequests >= maxConcurrentHealthRequests
        : activeWorkRequests >= maxConcurrentRequests;
      if (atCapacity) {
        jsonError(response, 503, "server is at capacity", { connection: "close" });
        return;
      }
      if (healthRequest) activeHealthRequests += 1;
      else activeWorkRequests += 1;
      let budgetReleased = false;
      const releaseBudget = () => {
        if (budgetReleased) return;
        budgetReleased = true;
        if (healthRequest) activeHealthRequests -= 1;
        else activeWorkRequests -= 1;
      };
      response.once("finish", releaseBudget);
      response.once("close", releaseBudget);

      const headers = { ...request.headers, authorization: `Bearer ${gatewayToken}` };
      delete headers.host;
      delete headers["x-openclaw-session-key"];
      if (chatRequest) {
        // OpenClaw otherwise treats the standard OpenAI `user` field as a
        // durable session identifier. Chat Completions is stateless: callers
        // provide the complete conversation in `messages`, so isolate every
        // request from prior OpenClaw transcript state.
        headers["x-openclaw-session-key"] = `airunway:${crypto.randomUUID()}`;
      }

      let downstreamClosed = false;
      let upstream;
      let upstreamResponse;
      const abortUpstream = () => {
        if (response.writableEnded || downstreamClosed) return;
        downstreamClosed = true;
        upstream?.destroy(new Error("downstream connection closed"));
        upstreamResponse?.destroy();
      };
      request.once("aborted", abortUpstream);
      response.once("close", abortUpstream);

      const forward = (body) => {
        if (downstreamClosed || response.destroyed || response.writableEnded) return;
        let timedOut = false;
        let upstreamDeadline;
        const clearUpstreamDeadline = () => clearTimeout(upstreamDeadline);
        upstream = http.request(
          {
            host: "127.0.0.1",
            port: internalPort,
            path: request.url,
            method: request.method,
            headers,
          },
          (receivedResponse) => {
            upstreamResponse = receivedResponse;
            receivedResponse.once("close", clearUpstreamDeadline);
            receivedResponse.on("error", () => {
              clearUpstreamDeadline();
              if (!downstreamClosed && !response.destroyed && !response.writableEnded) {
                response.destroy();
              }
            });
            if (downstreamClosed) {
              receivedResponse.destroy();
              return;
            }
            const responseHeaders = { ...receivedResponse.headers, connection: "close" };
            delete responseHeaders["keep-alive"];
            response.writeHead(receivedResponse.statusCode || 502, responseHeaders);
            receivedResponse.pipe(response);
          },
        );

        upstreamDeadline = setTimeout(() => {
          timedOut = true;
          if (response.headersSent) response.destroy();
          else jsonError(response, 504, "gateway request timed out");
          upstreamResponse?.destroy();
          upstream.destroy(new Error("upstream deadline exceeded"));
        }, upstreamTimeoutMs);

        upstream.on("error", () => {
          clearUpstreamDeadline();
          if (downstreamClosed || response.destroyed || response.writableEnded) return;
          if (response.headersSent) {
            response.destroy();
            return;
          }
          jsonError(
            response,
            timedOut ? 504 : 503,
            timedOut ? "gateway request timed out" : "gateway is not ready",
          );
        });
        upstream.end(body);
      };

      if (request.method !== "POST") {
        forward();
        return;
      }

      const chunks = [];
      let receivedBytes = 0;
      request.on("data", (chunk) => {
        receivedBytes += chunk.length;
        if (receivedBytes <= contentLength) chunks.push(chunk);
      });
      request.once("end", () => {
        if (receivedBytes !== contentLength) {
          jsonError(response, 400, "request body did not match Content-Length");
          return;
        }
        forward(Buffer.concat(chunks, receivedBytes));
      });
      request.on("error", abortUpstream);
    },
  );

  // With exactly one request per socket, this is also the overall
  // concurrent request ceiling. Route-specific capacity is enforced above.
  server.maxConnections = maxConcurrentRequests + maxConcurrentHealthRequests;
  return server;
}

export function createBoundedShutdown({ server, child, timeoutMs = SHUTDOWN_TIMEOUT_MS }) {
  if (timeoutMs <= 0) throw new Error("timeoutMs must be positive");
  let shutdownPromise;

  return function shutdown(signal = "SIGTERM") {
    if (shutdownPromise) return shutdownPromise;
    shutdownPromise = new Promise((resolve) => {
      let serverClosed = false;
      let childClosed = child.exitCode !== null || child.signalCode !== null;
      let settled = false;

      const settle = () => {
        if (settled) return;
        settled = true;
        clearTimeout(deadline);
        child.removeListener("exit", onChildExit);
        resolve();
      };
      const finishIfClosed = () => {
        if (serverClosed && childClosed) settle();
      };
      const onChildExit = () => {
        childClosed = true;
        finishIfClosed();
      };

      if (!childClosed) child.once("exit", onChildExit);
      const deadline = setTimeout(() => {
        try {
          server.closeAllConnections?.();
        } catch {
          // The process exits after the bounded shutdown attempt.
        }
        if (!childClosed) {
          try {
            child.kill("SIGKILL");
          } catch {
            // The child may have exited between the state check and kill.
          }
        }
        settle();
      }, timeoutMs);

      try {
        server.close(() => {
          serverClosed = true;
          finishIfClosed();
        });
        server.closeIdleConnections?.();
      } catch {
        serverClosed = true;
      }

      if (!childClosed) {
        try {
          child.kill(signal);
        } catch {
          childClosed = true;
        }
      }
      finishIfClosed();
    });
    return shutdownPromise;
  };
}
