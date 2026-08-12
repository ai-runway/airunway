import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { spawn } from "node:child_process";

const mountedConfig = process.env.AIRUNWAY_AGENT_CONFIG || "/etc/airunway/agent.json";
const runtimeConfig = JSON.parse(fs.readFileSync(mountedConfig, "utf8"));
const stateDir = "/tmp/airunway-openclaw";
const workspaceDir = path.join(stateDir, "workspace");
const externalPort = Number(process.env.AIRUNWAY_AGENT_PORT || "18789");
if (!Number.isInteger(externalPort) || externalPort < 1 || externalPort > 65535) {
  throw new Error("AIRUNWAY_AGENT_PORT must be an integer between 1 and 65535");
}
const internalPort = externalPort === 65535 ? 65534 : externalPort + 1;
const gatewayToken = crypto.randomBytes(32).toString("hex");
const maxRequestBytes = 4 * 1024 * 1024;

function required(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function modelProvider() {
  if (process.env.ANTHROPIC_MODEL) {
    return {
      id: "airunway",
      model: required("ANTHROPIC_MODEL"),
      baseUrl: required("ANTHROPIC_BASE_URL"),
      apiKey: "${ANTHROPIC_API_KEY}",
      api: "anthropic-messages",
    };
  }
  if (process.env.AZURE_OPENAI_MODEL) {
    throw new Error("the OpenClaw image does not yet support azureOpenAI bindings");
  }
  return {
    id: "airunway",
    model: required("OPENAI_MODEL"),
    baseUrl: required("OPENAI_BASE_URL"),
    apiKey: "${OPENAI_API_KEY}",
    api: "openai-completions",
  };
}

const provider = modelProvider();
fs.mkdirSync(workspaceDir, { recursive: true });
const systemPrompt = runtimeConfig.systemPrompt;
if (typeof systemPrompt === "string" && systemPrompt.trim()) {
  fs.writeFileSync(path.join(workspaceDir, "SOUL.md"), `${systemPrompt.trim()}\n`, { mode: 0o600 });
}

const openclawConfig = {
  // The image is release-pinned. Runtime update traffic and in-place package
  // mutation would bypass that supply-chain boundary.
  update: { checkOnStart: false, auto: { enabled: false } },
  // The public runtime contract is chat-only. Keep host filesystem, process,
  // browser, messaging, and control tools out of the model's tool surface.
  tools: { profile: "minimal" },
  gateway: {
    mode: "local",
    bind: "loopback",
    port: internalPort,
    auth: { mode: "token", token: "${OPENCLAW_GATEWAY_TOKEN}" },
    http: { endpoints: { chatCompletions: { enabled: true } } },
    controlUi: { enabled: false },
  },
  agents: {
    defaults: {
      workspace: workspaceDir,
      model: { primary: `${provider.id}/${provider.model}` },
    },
  },
  models: {
    mode: "merge",
    providers: {
      [provider.id]: {
        baseUrl: provider.baseUrl,
        apiKey: provider.apiKey,
        api: provider.api,
        models: [{ id: provider.model, name: provider.model }],
      },
    },
  },
};

const configPath = path.join(stateDir, "openclaw.json");
fs.writeFileSync(configPath, JSON.stringify(openclawConfig), { mode: 0o600 });
Object.assign(process.env, {
  HOME: stateDir,
  OPENCLAW_HOME: stateDir,
  OPENCLAW_STATE_DIR: stateDir,
  OPENCLAW_CONFIG_PATH: configPath,
  OPENCLAW_WORKSPACE_DIR: workspaceDir,
  OPENCLAW_GATEWAY_TOKEN: gatewayToken,
  OPENCLAW_DISABLE_BONJOUR: "1",
  OPENCLAW_NO_AUTO_UPDATE: "1",
});

const mode = process.env.AIRUNWAY_AGENT_MODE || "server";
if (mode === "job") {
  const task = runtimeConfig.task || runtimeConfig.prompt;
  if (typeof task !== "string" || !task.trim()) {
    throw new Error("job lifecycle requires spec.config.task or spec.config.prompt");
  }
  const child = spawn(
    "node",
    [
      "/app/openclaw.mjs",
      "agent",
      "--local",
      "--agent",
      "main",
      "--message",
      task,
      "--model",
      `${provider.id}/${provider.model}`,
    ],
    { stdio: "inherit", env: process.env },
  );
  child.on("exit", (code, signal) => {
    if (signal) process.kill(process.pid, signal);
    else process.exit(code ?? 1);
  });
} else {
  const accessToken = required("AIRUNWAY_AGENT_API_KEY");
  const gateway = spawn("node", ["/app/openclaw.mjs", "gateway"], {
    stdio: "inherit",
    env: process.env,
  });

  gateway.on("exit", (code, signal) => {
    if (signal) process.kill(process.pid, signal);
    else process.exit(code ?? 1);
  });

  const allowed = new Set([
    "GET /healthz",
    "GET /readyz",
    "GET /v1/models",
    "POST /v1/chat/completions",
  ]);

  function authorized(request) {
    const value = request.headers.authorization || "";
    if (!value.startsWith("Bearer ")) return false;
    const expected = crypto.createHash("sha256").update(accessToken).digest();
    const supplied = crypto.createHash("sha256").update(value.slice(7)).digest();
    return crypto.timingSafeEqual(expected, supplied);
  }

  function jsonError(response, status, message, headers = {}) {
    response.writeHead(status, { "content-type": "application/json", ...headers });
    response.end(JSON.stringify({ error: { message } }));
  }

  const proxy = http.createServer((request, response) => {
    if (!allowed.has(`${request.method} ${request.url}`)) {
      jsonError(response, 404, "not found");
      return;
    }
    const healthRequest = request.url === "/healthz" || request.url === "/readyz";
    if (!healthRequest && !authorized(request)) {
      jsonError(response, 401, "valid bearer authentication is required", {
        "www-authenticate": "Bearer",
      });
      return;
    }
    if (request.method === "POST") {
      if (request.headers["transfer-encoding"]) {
        jsonError(response, 400, "chunked request bodies are not supported");
        return;
      }
      const rawLength = request.headers["content-length"];
      const length = Number(rawLength);
      if (!rawLength || !Number.isInteger(length) || length < 1) {
        jsonError(response, 400, "a non-empty Content-Length is required");
        return;
      }
      if (length > maxRequestBytes) {
        jsonError(response, 413, "request body exceeds 4 MiB");
        return;
      }
    }
    if (request.method === "GET" && request.headers["content-length"] !== undefined) {
      jsonError(response, 400, "GET request bodies are not supported");
      return;
    }
    const headers = { ...request.headers, authorization: `Bearer ${gatewayToken}` };
    delete headers.host;
    const upstream = http.request(
      {
        host: "127.0.0.1",
        port: internalPort,
        path: request.url,
        method: request.method,
        headers,
      },
      (upstreamResponse) => {
        response.writeHead(upstreamResponse.statusCode || 502, upstreamResponse.headers);
        upstreamResponse.pipe(response);
      },
    );
    upstream.on("error", () => {
      if (!response.headersSent) {
        response.writeHead(503, { "content-type": "application/json" });
      }
      response.end('{"error":{"message":"gateway is not ready"}}');
    });
    request.pipe(upstream);
  });
  proxy.listen(externalPort, "0.0.0.0", () => {
    console.log(`AI Runway OpenClaw proxy listening on :${externalPort}`);
  });

  for (const signal of ["SIGINT", "SIGTERM"]) {
    process.on(signal, () => {
      proxy.close();
      gateway.kill(signal);
    });
  }
}
