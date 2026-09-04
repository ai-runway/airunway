import assert from "node:assert/strict";
import test from "node:test";

import { chatOnlyToolPolicy, modelProvider } from "./model_provider.mjs";

test("keeps the chat-only runtime from switching its bound model", () => {
  assert.deepEqual(chatOnlyToolPolicy(), {
    profile: "minimal",
    deny: ["session_status"],
  });
});

test("builds a classic Azure OpenAI deployment provider without persisting the key", () => {
  const provider = modelProvider({
    AZURE_OPENAI_MODEL: "gpt-4o deployment",
    AZURE_OPENAI_ENDPOINT: "https://example.openai.azure.com/",
    AZURE_OPENAI_API_KEY: "must-not-be-persisted",
  });

  assert.equal(
    provider.baseUrl,
    "https://example.openai.azure.com/openai/deployments/gpt-4o%20deployment",
  );
  assert.deepEqual(provider.headers, {
    "api-key": "${AZURE_OPENAI_API_KEY}",
    "api-version": "2024-06-01",
  });
  assert.equal(provider.apiKey, "${AZURE_OPENAI_API_KEY}");
  assert.equal(provider.authHeader, false);
  assert.doesNotMatch(JSON.stringify(provider), /must-not-be-persisted/);
});

test("keeps Azure's OpenAI-compatible v1 endpoint shape", () => {
  const provider = modelProvider({
    AZURE_OPENAI_MODEL: "deployment",
    AZURE_OPENAI_ENDPOINT: "https://example.openai.azure.com/openai/v1/",
    AZURE_OPENAI_API_KEY: "key",
  });

  assert.equal(provider.baseUrl, "https://example.openai.azure.com/openai/v1");
  assert.deepEqual(provider.headers, { "api-key": "${AZURE_OPENAI_API_KEY}" });
});

test("uses an explicitly configured Azure API version", () => {
  const provider = modelProvider({
    AZURE_OPENAI_MODEL: "deployment",
    AZURE_OPENAI_ENDPOINT: "https://example.services.ai.azure.com",
    AZURE_OPENAI_API_KEY: "key",
    AZURE_OPENAI_API_VERSION: "2025-04-01-preview",
  });

  assert.equal(provider.headers["api-version"], "2025-04-01-preview");
});

test("rejects missing Azure credentials and non-Azure endpoints", () => {
  assert.throws(
    () =>
      modelProvider({
        AZURE_OPENAI_MODEL: "deployment",
        AZURE_OPENAI_ENDPOINT: "https://example.openai.azure.com",
      }),
    /AZURE_OPENAI_API_KEY is required/,
  );
  assert.throws(
    () =>
      modelProvider({
        AZURE_OPENAI_MODEL: "deployment",
        AZURE_OPENAI_ENDPOINT: "https://models.example.com",
        AZURE_OPENAI_API_KEY: "key",
      }),
    /must be an Azure OpenAI endpoint/,
  );
});
