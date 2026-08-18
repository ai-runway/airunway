# Agent container runtime

> The Agent Marketplace controllers ship enabled. Set the controller argument `--enable-agent-marketplace=false` to turn the alpha feature off.

The feature starts additional cluster-wide watches for agent workloads and their supporting objects. Operators on large clusters should monitor controller memory and raise the shipped limit if needed.

The generic agent container provider creates a Kubernetes Deployment for a long-running agent or a Job for a one-shot task. This document is the contract between that controller and a workload image.

## Inputs

The complete `AgentDeployment.spec.config` JSON object is mounted read-only at `/etc/airunway/agent.json`. `AIRUNWAY_AGENT_CONFIG` contains that path.

The controller also sets:

| Variable | Meaning |
|---|---|
| `AIRUNWAY_AGENT_MODE` | `server` for a Deployment, `job` for a Job |
| `AIRUNWAY_AGENT_PORT` | HTTP port selected from `spec.config.port`, default `8080` |
| `AIRUNWAY_AGENT_API_KEY` | Per-agent bearer token, sourced from a provider-owned Secret in server mode only |
| `OPENAI_BASE_URL`, `OPENAI_MODEL`, `OPENAI_API_KEY` | OpenAI-compatible binding |
| `ANTHROPIC_BASE_URL`, `ANTHROPIC_MODEL`, `ANTHROPIC_API_KEY` | Anthropic binding |
| `AZURE_OPENAI_ENDPOINT`, `AZURE_OPENAI_MODEL`, `AZURE_OPENAI_API_KEY` | Azure OpenAI binding |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL` | Optional telemetry export |

Exactly one model-variable family is present. The API key can be a harmless placeholder for a keyless in-cluster model endpoint.

## Server mode

The repo-owned images expose this HTTP surface:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/healthz` | Process liveness |
| `GET` | `/readyz` | Ready to accept agent requests |
| `GET` | `/v1/models` | OpenAI-compatible model discovery |
| `POST` | `/v1/chat/completions` | OpenAI-compatible agent turn |

`/healthz` and `/readyz` are deliberately unauthenticated so Kubernetes can probe the process without reading a Secret. Every other route requires `Authorization: Bearer <AIRUNWAY_AGENT_API_KEY>`. The provider publishes the Secret name and key in `status.runtime.authSecretRef`; callers still need normal Kubernetes permission to read that Secret. The access token is separate from the model credential so granting permission to call one agent does not reveal the key that agent uses to reach its model.

Request bodies are limited to 4 MiB and chunked request bodies are rejected by the repo-owned wrappers. Server-mode bring-your-own images must implement the same authentication and request-bounding contract; the generic Service cannot retrofit authentication around a non-compliant image.

The generic controller uses TCP startup, readiness and liveness probes on the selected port. This preserves compatibility with bring-your-own images that predate these health paths. The repo-owned CrewAI and LangGraph wrappers guarantee non-streaming, text-message chat completions. OpenClaw and Hermes use their native OpenAI-compatible servers behind a narrow in-container proxy; the proxy keeps each native gateway's bearer credential on loopback and does not expose the administrative UI or control API. OpenClaw runs with its `minimal` tool profile, while Hermes gives the API-server platform an explicit empty toolset. This keeps an untrusted chat request from turning the model credential into filesystem, process, browser or control-plane access inside the pod.

This ingress-token contract applies only to the generic container backend. CRD-backed frameworks such as kagent and Orka do not receive an `AIRUNWAY_AGENT_API_KEY` or publish `status.runtime.authSecretRef`; their upstream operators own the serving endpoint and its authentication policy.

## Job mode

In job mode the image reads `task` (or its `prompt` alias) from the mounted config, performs one agent turn, writes the final answer to standard output and exits. A missing task is a configuration error. HTTP probes are intentionally omitted from Jobs, and no ingress token is created or injected because Jobs do not expose a Service.

## Security posture

Workloads run as numeric UID/GID 65532, cannot gain privileges, drop every Linux capability, do not receive a Kubernetes service-account token and use a RuntimeDefault seccomp profile. `/tmp` is always writable. Images must work with a read-only root filesystem unless their `AgentProviderConfig` explicitly owns the writable-root exception.

The model credential remains available to the workload process because the framework must call the model endpoint. Images must not print, persist or return it. The OpenClaw and Hermes wrappers reference model keys through environment substitution and generate their internal gateway keys at process startup. Their outward proxies replace, rather than forward, the caller's Authorization header before sending a request to the loopback gateway.
