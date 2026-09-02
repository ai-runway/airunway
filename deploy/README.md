# AI Runway Kubernetes Deployment

This directory contains Kubernetes manifests for deploying AI Runway to a cluster. The deployment is split into required, optional, and opt-in installer manifests:

- **`agentdeployment-webhook-guard.yaml`** — pre-rollout fail-closed guard for AgentDeployment CREATE/UPDATE (required)
- **`controller.yaml`** — CRDs, controller, staged webhooks, and RBAC (required)
- **`agentdeployment-webhook.yaml`** — post-rollout activation for the complete AgentDeployment validating and mutating protocol (required)
- **`dashboard.yaml`** — Web UI dashboard deployment and service (optional)
- **`dashboard-installer-rbac.yaml`** — optional elevated RBAC for one-click provider installation from the dashboard

## Quick Start

```bash
set -euo pipefail

# 1. Block AgentDeployment writes before changing the controller Deployment
kubectl apply -f agentdeployment-webhook-guard.yaml

# 2. Install CRDs and controller without AgentDeployment admission routes
kubectl apply -f controller.yaml

# 3. Force replacement pods even if the image string is unchanged, then wait
kubectl rollout restart deployment/airunway-controller-manager -n airunway-system
kubectl rollout status deployment/airunway-controller-manager -n airunway-system --timeout=5m

# 4. Activate admission, wait for its trusted serving CA, then remove the guard
kubectl apply -f agentdeployment-webhook.yaml
kubectl wait secret/airunway-webhook-server-cert -n airunway-system --for=jsonpath='{.data.ca\.crt}' --timeout=5m
AIRUNWAY_WEBHOOK_CA_BUNDLE="$(kubectl get secret/airunway-webhook-server-cert -n airunway-system -o jsonpath='{.data.ca\.crt}')"
test -n "${AIRUNWAY_WEBHOOK_CA_BUNDLE}"
kubectl wait mutatingwebhookconfiguration/airunway-mutating-webhook-configuration --for="jsonpath={.webhooks[?(@.name==\"magentdeployment-v1alpha1.kb.io\")].clientConfig.caBundle}=${AIRUNWAY_WEBHOOK_CA_BUNDLE}" --timeout=5m
kubectl wait validatingwebhookconfiguration/airunway-validating-webhook-configuration --for="jsonpath={.webhooks[?(@.name==\"vagentdeployment-v1alpha1.kb.io\")].clientConfig.caBundle}=${AIRUNWAY_WEBHOOK_CA_BUNDLE}" --timeout=5m
kubectl delete --ignore-not-found=true -f agentdeployment-webhook-guard.yaml

# 5. Install one or more provider shims (required — registers providers with AI Runway)
# See "Available provider shims" below for the full list
kubectl apply -f "https://raw.githubusercontent.com/ai-runway/airunway/main/providers/<provider>/deploy/<provider>.yaml"

# 6. Install dashboard UI (optional)
kubectl apply -f dashboard.yaml

# 7. Optional: enable dashboard-driven provider installs
# This grants broad cluster-scoped installer permissions. See dashboard-installer-rbac.yaml below.
kubectl apply -f dashboard-installer-rbac.yaml
```

> **Note:** The three controller phases are intentional for both installs and upgrades. The fail-closed mutating guard blocks AgentDeployment writes before any validating webhook can run; after `controller.yaml` removes both normal AgentDeployment routes, it is the only matching rule during the rollout. Existing reconciliation, status updates, and ModelDeployment admission continue. `agentdeployment-webhook.yaml` reapplies the complete validating and mutating configurations after the rollout. Its new webhook entries do not carry a static CA, so cert-controller injects the current serving CA; remove the guard only after both CA waits succeed. Provider shims must be installed before providers appear in the UI.

Use an immutable controller digest or a new tag for upgrades. The explicit
restart prevents `rollout status` from accepting an already-ready old ReplicaSet
when the rendered image string did not change.

Available provider shims:
- [kaito.yaml](../providers/kaito/deploy/kaito.yaml)
- [dynamo.yaml](../providers/dynamo/deploy/dynamo.yaml)
- [kuberay.yaml](../providers/kuberay/deploy/kuberay.yaml)
- [llmd.yaml](../providers/llmd/deploy/llmd.yaml)

## Access AIRunway

After deploying the dashboard, access AI Runway using port-forward:

```bash
kubectl port-forward -n airunway-system svc/airunway 3001:80
```

Then open http://localhost:3001 in your browser.

## What's Included

### controller.yaml

| Resource | Description |
|----------|-------------|
| `Namespace` | `airunway-system` — dedicated namespace |
| `CustomResourceDefinition` | `ModelDeployment` CRD |
| `CustomResourceDefinition` | `InferenceProviderConfig` CRD |
| `Deployment` | Controller manager deployment |
| `ServiceAccount` | Service account for the controller |
| `ClusterRole` | RBAC permissions for CRD and Kubernetes resource access |
| `ClusterRoleBinding` | Binds cluster role to controller service account |
| `MutatingWebhookConfiguration` | Phase-two mutating admission webhook for `ModelDeployment` |
| `ValidatingWebhookConfiguration` | Phase-two validating admission webhook for `ModelDeployment` |
| `Service` | Webhook service endpoint |
| `Secret` | Webhook TLS certificate secret |
| `Service` | Controller metrics service |
| `Role` / `RoleBinding` | Leader election RBAC |

### agentdeployment-webhook-guard.yaml

This phase-one manifest registers a fail-closed mutating webhook for
AgentDeployment CREATE and UPDATE. It deliberately references a Service that
does not exist, so the API server rejects those writes before validating
admission while controller webhook versions are mixed. That ordering prevents
an old side-effecting validator from persisting a credential record for a
rejected CREATE. The guard is the only matching AgentDeployment webhook after
phase two applies and does not match the status subresource or any other API.

If an install or upgrade stops after this manifest is applied, leave it in
place, fix the failed rollout or activation, and resume. Removing it early
reopens the mixed-admission window.

### agentdeployment-webhook.yaml

This phase-three manifest reapplies the shared webhook Service and the complete
`ValidatingWebhookConfiguration` and `MutatingWebhookConfiguration`, adding the
AgentDeployment credential-attestation route only after the controller rollout
is complete. The routes remain fail-closed; staging changes when they are
registered, not their failure policy. Delete the guard only after this manifest
applies successfully and cert-controller has populated both AgentDeployment
routes with the CA from `airunway-webhook-server-cert`.

### dashboard-installer-rbac.yaml

This manifest is optional and should be applied only when you want the dashboard
to run Helm/kubectl for provider installation. Provider charts for KAITO, Dynamo,
and KubeRay create cluster-scoped resources such as CRDs, admission webhooks,
ClusterRoles, and ClusterRoleBindings. Kubernetes therefore requires elevated
installer permissions, including RBAC `bind` and `escalate`. Treat this as
cluster-admin-like access for the dashboard ServiceAccount.

If you do not apply this manifest, the dashboard can still show the manual
installation commands. Automatic install buttons will return a clear permission
message when the dashboard lacks installer permissions.

### dashboard.yaml

| Resource | Description |
|----------|-------------|
| `ServiceAccount` | Service account for the dashboard pod |
| `ClusterRole` | RBAC permissions for dashboard read access |
| `ClusterRoleBinding` | Binds cluster role to dashboard service account |
| `Deployment` | Dashboard web UI deployment |
| `Service` | ClusterIP service on port 80 |

## Configuration

### Dashboard Environment Variables

The following environment variables can be configured on the **dashboard** deployment in `dashboard.yaml`:

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `3001` | Server port |
| `LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |
| `AUTH_ENABLED` | `true` | Enable authentication |
| `CORS_ORIGIN` | loopback origins | Comma-separated browser origins to allow, or explicit `*` to allow all origins |

### Authentication

Authentication is enabled by default in `dashboard.yaml`:

```yaml
env:
  - name: AUTH_ENABLED
    value: "true"
```

To intentionally disable authentication (not recommended), remove this variable or set it to `"false"`.

### CORS

By default, the dashboard API accepts browser CORS requests only from loopback
origins used for local access, such as `localhost`, `127.0.0.1`, and `[::1]`.
For a dashboard exposed behind a real host name, set `CORS_ORIGIN` to the exact
allowed origin or a comma-separated allowlist:

```yaml
env:
  - name: CORS_ORIGIN
    value: "https://airunway.example.com,https://admin.example.com"
```

Use `CORS_ORIGIN="*"` only when you intentionally want to allow every browser
origin. Empty or malformed values fall back to the safe loopback-only default.

## Verify Deployment

```bash
# Check all pods
kubectl get pods -n airunway-system

# Check services
kubectl get svc -n airunway-system

# View controller logs
kubectl logs -n airunway-system -l control-plane=controller-manager -f

# View dashboard logs
kubectl logs -n airunway-system -l app.kubernetes.io/name=airunway -f

# Test dashboard health endpoint
kubectl exec -it -n airunway-system deploy/airunway -- curl localhost:3001/api/health
```

## Uninstall

```bash
# Remove a guard left by an interrupted install or upgrade
kubectl delete --ignore-not-found=true -f agentdeployment-webhook-guard.yaml

# Remove optional dashboard installer RBAC (if installed)
kubectl delete -f dashboard-installer-rbac.yaml

# Remove dashboard (if installed)
kubectl delete -f dashboard.yaml

# Remove controller, CRDs, and namespace
kubectl delete -f controller.yaml
```

## Metrics Feature

Once deployed in-cluster, AI Runway can fetch real-time metrics from inference deployments (vLLM, Ray Serve). This feature requires in-cluster deployment as it uses Kubernetes service DNS to reach metrics endpoints.
