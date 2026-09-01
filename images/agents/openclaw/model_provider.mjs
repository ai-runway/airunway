const DEFAULT_AZURE_API_VERSION = "2024-06-01";
const AZURE_HOST_SUFFIXES = [
  ".openai.azure.com",
  ".services.ai.azure.com",
  ".cognitiveservices.azure.com",
];

export function chatOnlyToolPolicy() {
  return {
    profile: "minimal",
    // OpenClaw's minimal profile still includes session_status. Supplying its
    // optional model argument persists a session-level model override, which
    // would escape Airunway's fixed ModelBinding contract.
    deny: ["session_status"],
  };
}

export function required(name, env = process.env) {
  const value = env[name];
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(`${name} is required`);
  }
  return value.trim();
}

function azureProvider(env) {
  const model = required("AZURE_OPENAI_MODEL", env);
  required("AZURE_OPENAI_API_KEY", env);
  const endpoint = new URL(required("AZURE_OPENAI_ENDPOINT", env));
  if (endpoint.protocol !== "https:") {
    throw new Error("AZURE_OPENAI_ENDPOINT must use https");
  }
  const hostname = endpoint.hostname.toLowerCase();
  if (!AZURE_HOST_SUFFIXES.some((suffix) => hostname.endsWith(suffix))) {
    throw new Error("AZURE_OPENAI_ENDPOINT must be an Azure OpenAI endpoint");
  }
  if (endpoint.username || endpoint.password) {
    throw new Error("AZURE_OPENAI_ENDPOINT must not contain credentials");
  }
  endpoint.hash = "";
  endpoint.search = "";

  const path = endpoint.pathname.replace(/\/+$/, "");
  const openAIV1 = path.toLowerCase().endsWith("/openai/v1");
  if (!openAIV1) {
    const deploymentPrefix = "/openai/deployments/";
    const existingDeployment = path.toLowerCase().lastIndexOf(deploymentPrefix);
    const basePath = existingDeployment >= 0 ? path.slice(0, existingDeployment) : path;
    endpoint.pathname = `${basePath}${deploymentPrefix}${encodeURIComponent(model)}`;
  }

  const headers = { "api-key": "${AZURE_OPENAI_API_KEY}" };
  if (!openAIV1) {
    // OpenClaw's pinned OpenAI transport recognizes this header on Azure hosts
    // and moves it to the required `api-version` query parameter.
    headers["api-version"] =
      env.AZURE_OPENAI_API_VERSION?.trim() ||
      env.AZURE_API_VERSION?.trim() ||
      DEFAULT_AZURE_API_VERSION;
  }

  return {
    id: "airunway",
    model,
    baseUrl: endpoint.toString().replace(/\/+$/, ""),
    apiKey: "${AZURE_OPENAI_API_KEY}",
    api: "openai-completions",
    authHeader: false,
    headers,
  };
}

export function modelProvider(env = process.env) {
  if (env.ANTHROPIC_MODEL) {
    required("ANTHROPIC_API_KEY", env);
    return {
      id: "airunway",
      model: required("ANTHROPIC_MODEL", env),
      baseUrl: required("ANTHROPIC_BASE_URL", env),
      apiKey: "${ANTHROPIC_API_KEY}",
      api: "anthropic-messages",
    };
  }
  if (env.AZURE_OPENAI_MODEL) {
    return azureProvider(env);
  }
  required("OPENAI_API_KEY", env);
  return {
    id: "airunway",
    model: required("OPENAI_MODEL", env),
    baseUrl: required("OPENAI_BASE_URL", env),
    apiKey: "${OPENAI_API_KEY}",
    api: "openai-completions",
  };
}
