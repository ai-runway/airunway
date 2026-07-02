# AI Runway Headlamp Plugin - Architecture & Design

This document describes the architecture, design decisions, and technical details of the AI Runway Headlamp plugin.

For installation and development instructions, see the [plugin README](https://github.com/ai-runway/airunway/blob/main/plugins/headlamp/README.md).

---

## Overview

The AI Runway Headlamp plugin integrates ML deployment management capabilities directly into the [Headlamp](https://headlamp.dev/) Kubernetes dashboard. It provides full feature parity with the main AI Runway UI, supporting all runtimes (KAITO, KubeRay, Dynamo, llm-d) through a full backend proxy architecture.

---

## Design Decisions

| # | Decision Area | Choice | Rationale |
|---|--------------|--------|-----------|
| 1 | Repository Structure | **Monorepo** (`plugins/headlamp/`) | Easier type sharing, coordinated changes, single CI |
| 2 | Data Access Strategy | **Full Backend Proxy** | Maximum feature parity, reuse existing business logic |
| 3 | Shared Types | **Reuse via workspace dependency** | Direct imports from `@airunway/shared` |
| 4 | Plugin Name | **airunway-headlamp-plugin** | Consistent with gatekeeper-headlamp-plugin naming |
| 5 | API Client Strategy | **Shared API Client Package** | Single source of truth, both UIs use same client |
| 6 | Backend Discovery | **Flexible** (In-Cluster + External) | Service discovery with manual override fallback |
| 7 | Authentication | **Pass-through Kubernetes Token** | Seamless auth, same RBAC as Headlamp |
| 8 | Component Strategy | **Rewrite for Headlamp** | Use Headlamp's CommonComponents, no regression to main UI |
| 9 | Shared Package Location | **Extend existing `shared/`** | `shared/api/` alongside `shared/types/` |

---

## Code Sharing Strategy

The monorepo + full backend proxy approach enables significant code reuse:

| Shared Code | Location | How Plugin Uses It |
|------------|----------|-------------------|
| **TypeScript Types** | `shared/types/` | Direct import via workspace dependency |
| **API Client** | `shared/api/` | Direct import, same as main frontend |
| **Utility Functions** | `shared/utils/` | Direct import |

**What is _not_ shared** (due to Headlamp's runtime constraints):
- React components — Headlamp requires its bundled React and MUI
- Styling/CSS — Headlamp uses its own theming
- UI-specific hooks

UI components are written using Headlamp's `CommonComponents` (`SectionBox`, `SimpleTable`, `Link`, `Loader`, etc.). The main AI Runway frontend is completely unaffected by the plugin.

---

## Authentication Flow

```
┌─────────────┐    K8s Token     ┌─────────────────────┐
│  Headlamp   │ ───────────────► │ AI Runway Backend │
│  (Browser)  │                  │                     │
└─────────────┘                  └─────────────────────┘
       │                                   │
       │ Plugin passes                     │ TokenReview
       │ same token                        │ validation
       ▼                                   ▼
┌─────────────┐                  ┌─────────────────────┐
│   Plugin    │                  │   Kubernetes API    │
└─────────────┘                  └─────────────────────┘
```

The plugin reuses the same Kubernetes token that Headlamp already holds. The AI Runway backend validates it via TokenReview, so no additional authentication is needed.

---

## Backend Discovery

The plugin locates the AI Runway backend in priority order:

1. **Plugin Settings** — User-configured URL in Headlamp Plugin Settings
2. **In-Cluster Service Discovery** — `airunway.<namespace>.svc:3001`
3. **Default** — `http://localhost:3001` (development fallback)

---

## Directory Layout

```
airunway/
├── frontend/                          # Main AI Runway UI (unchanged)
│
├── backend/                           # AI Runway Backend (unchanged)
│
├── shared/                            # Shared code
│   ├── types/                         # TypeScript type definitions
│   └── api/                           # Shared API client
│       ├── client.ts                  # Base request function with auth
│       ├── deployments.ts             # deploymentsApi
│       ├── models.ts                  # modelsApi
│       ├── health.ts                  # healthApi
│       ├── settings.ts               # settingsApi
│       ├── runtimes.ts               # runtimesApi
│       ├── metrics.ts                # metricsApi
│       ├── installation.ts           # installationApi
│       ├── gpu.ts                    # gpuOperatorApi
│       ├── autoscaler.ts             # autoscalerApi
│       ├── huggingface.ts            # huggingFaceApi
│       ├── aikit.ts                  # aikitApi
│       └── aiconfigurator.ts         # aiConfiguratorApi
│
├── plugins/
│   └── headlamp/                      # Headlamp plugin
│       ├── package.json
│       ├── tsconfig.json
│       ├── Makefile
│       ├── README.md
│       ├── artifacthub-pkg.yml
│       ├── artifacthub-repo.yml
│       └── src/
│           ├── index.tsx              # Entry point, route & sidebar registrations
│           ├── routes.ts              # Route path constants
│           ├── settings.tsx           # Plugin settings component
│           ├── components/            # Reusable Headlamp-compatible components
│           │   ├── ConnectionBanner.tsx
│           │   ├── DeleteDialog.tsx
│           │   ├── GPUCapacityDashboard.tsx
│           │   ├── LogsViewer.tsx
│           │   ├── MetricsPanel.tsx
│           │   └── StatusBadge.tsx
│           ├── pages/                 # Page components
│           │   ├── DeploymentsList.tsx
│           │   ├── DeploymentDetails.tsx
│           │   ├── ModelsCatalog.tsx
│           │   ├── CreateDeployment.tsx
│           │   ├── RuntimesStatus.tsx
│           │   ├── Integrations.tsx
│           │   └── HuggingFaceCallback.tsx
│           └── lib/                   # Plugin utilities
│               ├── api-client.ts      # Wraps shared API with Headlamp auth
│               ├── backend-discovery.ts
│               ├── plugin-storage.ts  # Headlamp plugin config storage
│               ├── theme.ts           # Theme utilities
│               └── utils.ts
│
└── package.json                       # Workspace root
```

---

## Plugin Routes

| Route | Page | Description |
|-------|------|-------------|
| `/airunway/deployments` | DeploymentsList | List and filter all deployments |
| `/airunway/deployments/:namespace/:name` | DeploymentDetails | Deployment info, pods, metrics, logs |
| `/airunway/deployments/create` | CreateDeployment | Multi-step deployment wizard |
| `/airunway/models` | ModelsCatalog | Browse curated models and HuggingFace |
| `/airunway/runtimes` | RuntimesStatus | Runtime installation and health |
| `/airunway/integrations` | Integrations | External integrations (e.g., HuggingFace OAuth) |
| `/airunway/settings` | Settings | Plugin configuration |

---

## Feature Summary

### Deployment Management
- List deployments with filters (namespace, runtime, status)
- Deployment details with pod status and conditions
- Multi-step create wizard with model selection, runtime choice, and resource configuration
- Delete with confirmation dialog
- AI Configurator integration for intelligent configuration suggestions

### Model Catalog
- Browse curated models
- HuggingFace search integration (with OAuth)
- Model compatibility information per runtime

### Monitoring
- Real-time metrics (requests/sec, latency, tokens/sec) with auto-refresh
- Per-pod log viewer with tail configuration
- GPU capacity dashboard with node-level breakdown
- Autoscaler status and scaling events
- Connection status indicator

### Runtimes
- KAITO, KubeRay, and Dynamo runtime status
- Installation status and operator health

---

## Known Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| Backend not reachable from Headlamp | Clear error messaging via ConnectionBanner, manual URL config in settings |
| CRD schema changes in KAITO/KubeRay/Dynamo | Backend abstracts CRD details; plugin only talks to backend API |
| Headlamp plugin API changes | Pin `@kinvolk/headlamp-plugin` version |
| Type sync issues between packages | Workspace dependencies ensure consistency; CI checks |
| Auth token issues across contexts | Comprehensive error handling and graceful degradation |
