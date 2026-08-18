import assert from "node:assert/strict";
import { EventEmitter, once } from "node:events";
import http from "node:http";
import net from "node:net";
import { performance } from "node:perf_hooks";
import test from "node:test";

import { createBoundedShutdown, createRuntimeProxy } from "./runtime_proxy.mjs";

async function listen(server) {
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  return server.address().port;
}

async function close(server) {
  if (!server.listening) return;
  await new Promise((resolve) => server.close(resolve));
}

function call(port, { method = "GET", path = "/healthz", headers = {}, body } = {}) {
  return new Promise((resolve, reject) => {
    const request = http.request(
      { host: "127.0.0.1", port, method, path, headers },
      (response) => {
        const chunks = [];
        response.on("data", (chunk) => chunks.push(chunk));
        response.on("end", () =>
          resolve({ status: response.statusCode, body: Buffer.concat(chunks).toString() }),
        );
      },
    );
    request.on("error", reject);
    if (body) request.write(body);
    request.end();
  });
}

function createProxy(internalPort, overrides = {}) {
  return createRuntimeProxy({
    accessToken: "access-token",
    gatewayToken: "gateway-token",
    internalPort,
    ...overrides,
  });
}

test("accepts case-insensitive Bearer schemes but rejects other credentials", async (t) => {
  const upstream = http.createServer((_request, response) => response.end("ok"));
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort);
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  for (const authorization of ["bearer access-token", "bEaReR access-token"]) {
    const response = await call(proxyPort, {
      path: "/v1/models",
      headers: { authorization },
    });
    assert.equal(response.status, 200);
  }

  for (const authorization of ["Basic access-token", "Bearer wrong-token"]) {
    const response = await call(proxyPort, {
      path: "/v1/models",
      headers: { authorization },
    });
    assert.equal(response.status, 401);
  }
});

test("uses independent work and health request budgets", async (t) => {
  let releaseFirst;
  let firstReceived;
  const received = new Promise((resolve) => {
    firstReceived = resolve;
  });
  const upstream = http.createServer((request, response) => {
    if (request.url === "/v1/chat/completions") {
      firstReceived();
      new Promise((resolve) => {
        releaseFirst = resolve;
      }).then(() => response.end("held"));
      return;
    }
    response.end("healthy");
  });
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort, {
    maxConcurrentRequests: 1,
    maxConcurrentHealthRequests: 1,
  });
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  const first = call(proxyPort, {
    method: "POST",
    path: "/v1/chat/completions",
    headers: {
      authorization: "Bearer access-token",
      "content-length": "2",
    },
    body: "{}",
  });
  await received;

  const saturated = await call(proxyPort, {
    path: "/v1/models",
    headers: { authorization: "Bearer access-token" },
  });
  assert.equal(saturated.status, 503);
  assert.match(saturated.body, /server is at capacity/);

  const health = await call(proxyPort);
  assert.equal(health.status, 200);
  releaseFirst();
  assert.equal((await first).status, 200);
  assert.equal(proxy.maxConnections, 2);
});

test("bounds unauthenticated health requests", async (t) => {
  let releaseFirst;
  let firstReceived;
  let upstreamCalls = 0;
  const received = new Promise((resolve) => {
    firstReceived = resolve;
  });
  const upstream = http.createServer((_request, response) => {
    upstreamCalls += 1;
    firstReceived();
    new Promise((resolve) => {
      releaseFirst = resolve;
    }).then(() => response.end("healthy"));
  });
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort, { maxConcurrentHealthRequests: 1 });
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  const first = call(proxyPort);
  await received;
  const saturated = await call(proxyPort);
  assert.equal(saturated.status, 503);
  assert.match(saturated.body, /server is at capacity/);
  assert.equal(upstreamCalls, 1);

  releaseFirst();
  assert.equal((await first).status, 200);
});

test("accepts GET requests with Content-Length zero", async (t) => {
  const upstream = http.createServer((_request, response) => response.end("healthy"));
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort);
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  const response = await call(proxyPort, {
    headers: { "content-length": "0" },
  });
  assert.equal(response.status, 200);
  assert.equal(response.body, "healthy");
});

test("closes keep-alive connections after one invalid request", async (t) => {
  let upstreamCalls = 0;
  const upstream = http.createServer((_request, response) => {
    upstreamCalls += 1;
    response.end("unexpected");
  });
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort);
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  const client = net.connect(proxyPort, "127.0.0.1");
  client.on("error", () => {});
  const chunks = [];
  client.on("data", (chunk) => chunks.push(chunk));
  const closed = new Promise((resolve) => client.once("close", resolve));
  client.write(
    "GET /not-found HTTP/1.1\r\n" +
      "Host: localhost\r\n" +
      "Connection: keep-alive\r\n\r\n",
  );
  await closed;

  const response = Buffer.concat(chunks).toString();
  assert.match(response, /HTTP\/1\.1 404 Not Found/);
  assert.match(response, /connection: close/i);
  assert.equal(upstreamCalls, 0);
});

test("closes a connection that pipelines concurrent requests", async (t) => {
  let upstreamCalls = 0;
  const upstream = http.createServer(() => {
    upstreamCalls += 1;
  });
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort);
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  const client = net.connect(proxyPort, "127.0.0.1");
  client.on("error", () => {});
  const closed = new Promise((resolve) => client.once("close", resolve));
  const requests = Array.from(
    { length: 100 },
    () => "GET /healthz HTTP/1.1\r\nHost: localhost\r\n\r\n",
  ).join("");
  client.write(requests);
  await closed;
  await new Promise((resolve) => setImmediate(resolve));
  assert.ok(upstreamCalls <= 1, `forwarded ${upstreamCalls} pipelined requests`);
});

test("enforces header and complete-request deadlines", async (t) => {
  const upstream = http.createServer((_request, response) => response.end("ok"));
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort, {
    headerTimeoutMs: 50,
    requestTimeoutMs: 100,
    connectionsCheckingInterval: 10,
  });
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  assert.equal(proxy.headersTimeout, 50);
  assert.equal(proxy.requestTimeout, 100);

  const partialHeader = net.connect(proxyPort, "127.0.0.1");
  partialHeader.write("GET /healthz HTTP/1.1\r\nX-Slow: ");
  const [headerResponse] = await once(partialHeader, "data");
  assert.match(headerResponse.toString(), /HTTP\/1\.1 408 Request Timeout/);
  partialHeader.destroy();

  const partialBody = net.connect(proxyPort, "127.0.0.1");
  partialBody.write(
    "POST /v1/chat/completions HTTP/1.1\r\n" +
      "Host: localhost\r\n" +
      "Authorization: Bearer access-token\r\n" +
      "Content-Length: 10\r\n\r\nx",
  );
  const [bodyResponse] = await once(partialBody, "data");
  assert.match(bodyResponse.toString(), /HTTP\/1\.1 408 Request Timeout/);
  partialBody.destroy();
});

test("uses an absolute upstream deadline", async (t) => {
  let upstreamActivity = 0;
  let upstreamFinished = false;
  const upstream = http.createServer((_request, response) => {
    const writeActivity = () => {
      if (response.destroyed || response.writableEnded) return;
      upstreamActivity += 1;
      response.writeProcessing();
    };
    writeActivity();
    const activity = setInterval(writeActivity, 10);
    const finish = setTimeout(() => {
      upstreamFinished = true;
      clearInterval(activity);
      response.end("late response");
    }, 200);
    response.once("close", () => {
      clearInterval(activity);
      clearTimeout(finish);
    });
  });
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort, { upstreamTimeoutMs: 60 });
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  const response = await call(proxyPort, {
    path: "/v1/models",
    headers: { authorization: "Bearer access-token" },
  });
  assert.equal(response.status, 504);
  assert.deepEqual(JSON.parse(response.body), {
    error: { message: "gateway request timed out" },
  });
  assert.ok(upstreamActivity >= 2, `received only ${upstreamActivity} upstream activity events`);
  assert.equal(upstreamFinished, false, "periodic upstream activity must not extend the deadline");
});

test("rejects chunked GET bodies before contacting the gateway", async (t) => {
  let upstreamCalls = 0;
  const upstream = http.createServer((_request, response) => {
    upstreamCalls += 1;
    response.end("unexpected");
  });
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort);
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  const response = await call(proxyPort, {
    headers: { "transfer-encoding": "chunked" },
  });
  assert.equal(response.status, 400);
  assert.match(response.body, /chunked request bodies are not supported/);
  assert.equal(upstreamCalls, 0);
});

test("aborts the upstream request when the downstream disconnects", async (t) => {
  let upstreamReceived;
  const received = new Promise((resolve) => {
    upstreamReceived = resolve;
  });
  let upstreamClosed;
  const closed = new Promise((resolve) => {
    upstreamClosed = resolve;
  });
  const upstream = http.createServer((_request, response) => {
    upstreamReceived();
    response.once("close", () => {
      if (!response.writableEnded) upstreamClosed();
    });
  });
  const upstreamPort = await listen(upstream);
  const proxy = createProxy(upstreamPort);
  const proxyPort = await listen(proxy);
  t.after(async () => {
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    await Promise.all([close(proxy), close(upstream)]);
  });

  const downstream = http.request({
    host: "127.0.0.1",
    port: proxyPort,
    path: "/v1/models",
    headers: { authorization: "Bearer access-token" },
  });
  downstream.on("error", () => {});
  downstream.end();
  await received;
  downstream.destroy();
  await closed;
});

test("shutdown is idempotent and waits for both server and child", async () => {
  class FakeServer {
    closeCalls = 0;
    idleCalls = 0;
    closeCallback;
    close(callback) {
      this.closeCalls += 1;
      this.closeCallback = callback;
    }
    closeIdleConnections() {
      this.idleCalls += 1;
    }
    finishClose() {
      const callback = this.closeCallback;
      this.closeCallback = undefined;
      callback?.();
    }
  }
  class FakeChild extends EventEmitter {
    exitCode = null;
    signalCode = null;
    signals = [];
    kill(signal) {
      this.signals.push(signal);
      return true;
    }
    finishExit() {
      this.exitCode = 0;
      this.emit("exit", 0, null);
    }
  }

  for (const firstCompletion of ["server", "child"]) {
    const server = new FakeServer();
    const child = new FakeChild();
    const shutdown = createBoundedShutdown({ server, child, timeoutMs: 100 });
    const first = shutdown("SIGTERM");
    const second = shutdown("SIGINT");
    assert.strictEqual(first, second);

    let settled = false;
    first.then(() => {
      settled = true;
    });
    await Promise.resolve();
    const settledBeforeEither = settled;

    if (firstCompletion === "server") server.finishClose();
    else child.finishExit();
    await Promise.resolve();
    const settledAfterFirst = settled;

    if (firstCompletion === "server") child.finishExit();
    else server.finishClose();
    await first;

    assert.equal(settledBeforeEither, false, `${firstCompletion}-first shutdown settled immediately`);
    assert.equal(settledAfterFirst, false, `${firstCompletion}-first shutdown ignored one resource`);
    assert.equal(settled, true);
    assert.equal(server.closeCalls, 1);
    assert.equal(server.idleCalls, 1);
    assert.deepEqual(child.signals, ["SIGTERM"]);
  }
});

test("shutdown hard-stops lingering resources only at its deadline", async () => {
  class StuckServer {
    closeCalls = 0;
    hardCloseCalls = 0;
    hardCloseAt;
    close() {
      this.closeCalls += 1;
    }
    closeAllConnections() {
      this.hardCloseCalls += 1;
      this.hardCloseAt = performance.now();
    }
  }
  class StuckChild extends EventEmitter {
    exitCode = null;
    signalCode = null;
    signals = [];
    signalTimes = [];
    kill(signal) {
      this.signals.push(signal);
      this.signalTimes.push(performance.now());
      return true;
    }
  }

  const server = new StuckServer();
  const child = new StuckChild();
  const timeoutMs = 40;
  const shutdown = createBoundedShutdown({ server, child, timeoutMs });
  const startedAt = performance.now();
  const pending = shutdown("SIGTERM");
  assert.equal(server.hardCloseCalls, 0);
  assert.deepEqual(child.signals, ["SIGTERM"]);
  await pending;

  assert.equal(server.closeCalls, 1);
  assert.equal(server.hardCloseCalls, 1);
  assert.deepEqual(child.signals, ["SIGTERM", "SIGKILL"]);
  const earliestDeadline = startedAt + timeoutMs - 5;
  assert.ok(
    server.hardCloseAt >= earliestDeadline,
    `server hard-stopped ${Math.round(server.hardCloseAt - startedAt)}ms into a ${timeoutMs}ms grace period`,
  );
  assert.ok(
    child.signalTimes[1] >= earliestDeadline,
    `child was SIGKILLed ${Math.round(child.signalTimes[1] - startedAt)}ms into a ${timeoutMs}ms grace period`,
  );
});
