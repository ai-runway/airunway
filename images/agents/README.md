# Agent workload images

These are the containers run **for an `AgentDeployment`**. They are different from `providers/agent-container`, which is the Kubernetes controller that creates the Deployment or Job.

| Image | Source | Runtime |
|---|---|---|
| `agent-crewai` | `crewai/Dockerfile` | CrewAI 1.15.14 |
| `agent-langgraph` | `langgraph/Dockerfile` | LangGraph 1.2.11 |
| `agent-openclaw` | `openclaw/Dockerfile` | Official OpenClaw image, pinned by digest |
| `agent-hermes` | `hermes/Dockerfile` | Official Hermes image, pinned by digest |

CrewAI and LangGraph keep a short direct-dependency file beside a complete, hash-verified `requirements.lock`. Regenerate a lock with the command recorded at its top whenever a direct dependency changes. OpenClaw and Hermes do not reinstall or independently lock the dependencies already inside their official upstream images; their reproducibility boundary is the audited upstream image digest in `/versions.env`.

Build all four locally:

```bash
make agent-images-docker-build PLATFORM=linux/arm64 \
  AGENT_CREWAI_IMG=airunway/agent-crewai:local \
  AGENT_LANGGRAPH_IMG=airunway/agent-langgraph:local \
  AGENT_OPENCLAW_IMG=airunway/agent-openclaw:local \
  AGENT_HERMES_IMG=airunway/agent-hermes:local
```

Use `linux/amd64` on an x86 cluster. `PUSH=true` is deliberately opt-in. The release workflow publishes multi-architecture images to the names used by the sample provider catalog.

Run source-level contract checks with `make agent-images-test`. After building, `images/agents/smoke.sh <runtime> <image>` verifies both lifecycles: server mode starts as UID 65532 with a read-only root filesystem, exposes unauthenticated health, rejects an unauthenticated API request, and completes an authenticated chat request; job mode performs the configured task, prints the result, and exits successfully. CI builds and smoke-tests every image independently. The complete runtime contract is documented in [../../docs/agent-container-runtime.md](../../docs/agent-container-runtime.md).
