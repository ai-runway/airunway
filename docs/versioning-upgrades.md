# Versioning & Upgrades

## API Versioning Strategy

### Version Progression

1. **v1alpha1** — Initial release
   - Experimental API
   - Breaking changes allowed
   - No stability guarantees

2. **v1beta1** — Stabilization
   - Feature complete
   - Breaking changes with deprecation warnings
   - Migration tooling provided

3. **v1** — Stable
   - No breaking changes
   - Long-term support
   - Backward compatibility required

### Conversion Webhooks

When moving between versions, conversion webhooks will handle:
- Field renames
- Structural changes
- Default value updates

## Controller Upgrades & Compatibility

### Upgrade Process

```bash
set -euo pipefail

export AIRUNWAY_TARGET_REF=vX.Y.Z
export AIRUNWAY_TARGET_BASE="https://raw.githubusercontent.com/ai-runway/airunway/${AIRUNWAY_TARGET_REF}/deploy"
export AIRUNWAY_CONTROLLER_IMAGE="ghcr.io/ai-runway/airunway/controller:${AIRUNWAY_TARGET_REF#v}"

# Phase 1 blocks AgentDeployment CREATE/UPDATE before webhook versions can mix.
kubectl apply -f "${AIRUNWAY_TARGET_BASE}/agentdeployment-webhook-guard.yaml"

# Phase 2 removes AgentDeployment validation and mutation, then starts rollout.
kubectl apply -f "${AIRUNWAY_TARGET_BASE}/controller.yaml"
kubectl set image deployment/airunway-controller-manager -n airunway-system manager="${AIRUNWAY_CONTROLLER_IMAGE}"

# Force a new ReplicaSet even if the target image string is already present,
# then wait until the webhook Service has only replacement endpoints.
kubectl rollout restart deployment/airunway-controller-manager -n airunway-system
kubectl rollout status deployment/airunway-controller-manager -n airunway-system --timeout=5m

# Phase 3 installs the complete new validator/mutator protocol. Wait until
# cert-controller injects the current serving CA before removing the guard.
kubectl apply -f "${AIRUNWAY_TARGET_BASE}/agentdeployment-webhook.yaml"
kubectl wait secret/airunway-webhook-server-cert -n airunway-system --for=jsonpath='{.data.ca\.crt}' --timeout=5m
AIRUNWAY_WEBHOOK_CA_BUNDLE="$(kubectl get secret/airunway-webhook-server-cert -n airunway-system -o jsonpath='{.data.ca\.crt}')"
test -n "${AIRUNWAY_WEBHOOK_CA_BUNDLE}"
kubectl wait mutatingwebhookconfiguration/airunway-mutating-webhook-configuration --for="jsonpath={.webhooks[?(@.name==\"magentdeployment-v1alpha1.kb.io\")].clientConfig.caBundle}=${AIRUNWAY_WEBHOOK_CA_BUNDLE}" --timeout=5m
kubectl wait validatingwebhookconfiguration/airunway-validating-webhook-configuration --for="jsonpath={.webhooks[?(@.name==\"vagentdeployment-v1alpha1.kb.io\")].clientConfig.caBundle}=${AIRUNWAY_WEBHOOK_CA_BUNDLE}" --timeout=5m
kubectl delete --ignore-not-found=true -f "${AIRUNWAY_TARGET_BASE}/agentdeployment-webhook-guard.yaml"
```

The three phases prevent Kubernetes from sending credential admission to mixed
old and new validator implementations or sending AgentDeployment mutation to an
old pod that does not serve the new path. The guard is a fail-closed mutating
webhook, so its denial completes before validating admission starts even during
the brief phase-one overlap with the old validator. A rejected CREATE therefore
cannot persist a credential record. After phase two applies, the guard is the
only AgentDeployment admission rule. Existing reconciliation, status
subresource updates, and all ModelDeployment admission continue. Use an
immutable digest or a new version tag for the controller image; the explicit
restart prevents an unchanged Pod template from making `rollout status` accept
old pods. `make controller-deploy` performs the same guard, apply, restart,
wait, activate, CA verification, and unguard sequence.

If phase two or phase three fails, keep the guard installed. Correct the
failure and resume from the failed command. A successful activation with a
failed guard deletion is safe but leaves AgentDeployment writes unavailable;
rerun only the delete command after verifying activation.

Before rolling back to a controller version that does not serve the mutating
route, first install the guard from the currently installed release. Keep it
active while the current phase-two bundle removes both AgentDeployment routes
and the Deployment rolls back. After the old pods are ready, reapply the
rollback release's controller bundle to restore its legacy validator; only then
wait for cert-controller to inject the current serving CA into that validator
and remove the guard:

```bash
set -euo pipefail

export AIRUNWAY_CURRENT_REF=vX.Y.Z
export AIRUNWAY_CURRENT_BASE="https://raw.githubusercontent.com/ai-runway/airunway/${AIRUNWAY_CURRENT_REF}/deploy"
export AIRUNWAY_ROLLBACK_REF=vW.X.Y
export AIRUNWAY_ROLLBACK_BASE="https://raw.githubusercontent.com/ai-runway/airunway/${AIRUNWAY_ROLLBACK_REF}/deploy"
export AIRUNWAY_ROLLBACK_IMAGE="ghcr.io/ai-runway/airunway/controller:${AIRUNWAY_ROLLBACK_REF#v}"

kubectl apply -f "${AIRUNWAY_CURRENT_BASE}/agentdeployment-webhook-guard.yaml"
kubectl apply -f "${AIRUNWAY_CURRENT_BASE}/controller.yaml"
kubectl rollout undo deployment/airunway-controller-manager -n airunway-system
kubectl rollout status deployment/airunway-controller-manager -n airunway-system --timeout=5m
kubectl apply -f "${AIRUNWAY_ROLLBACK_BASE}/controller.yaml"
kubectl set image deployment/airunway-controller-manager -n airunway-system manager="${AIRUNWAY_ROLLBACK_IMAGE}"
kubectl rollout restart deployment/airunway-controller-manager -n airunway-system
kubectl rollout status deployment/airunway-controller-manager -n airunway-system --timeout=5m
kubectl wait secret/airunway-webhook-server-cert -n airunway-system --for=jsonpath='{.data.ca\.crt}' --timeout=5m
AIRUNWAY_WEBHOOK_CA_BUNDLE="$(kubectl get secret/airunway-webhook-server-cert -n airunway-system -o jsonpath='{.data.ca\.crt}')"
test -n "${AIRUNWAY_WEBHOOK_CA_BUNDLE}"
kubectl wait validatingwebhookconfiguration/airunway-validating-webhook-configuration --for="jsonpath={.webhooks[?(@.name==\"vagentdeployment-v1alpha1.kb.io\")].clientConfig.caBundle}=${AIRUNWAY_WEBHOOK_CA_BUNDLE}" --timeout=5m
kubectl delete --ignore-not-found=true -f "${AIRUNWAY_CURRENT_BASE}/agentdeployment-webhook-guard.yaml"
```

If the rollback fails, leave the guard installed until the controller and its
admission configuration are on one compatible version.

**Behavior during upgrade:**
- Controller deployment performs a rolling update (no downtime)
- AgentDeployment CREATE/UPDATE is intentionally unavailable from phase one until phase three completes
- AgentDeployment validating and mutating routes are absent during the rollout and restored only after all new endpoints are ready
- Existing `ModelDeployment` resources continue to function
- In-flight reconciliations complete with the old controller, then new controller takes over
- Provider resources are not disrupted during controller upgrade

**CRD updates:**
- New controller versions may include updated CRD schemas
- Existing resources remain valid (new fields have defaults)
- Breaking CRD changes only occur between API versions (e.g., v1alpha1 → v1beta1)

### Version Compatibility Matrix

| AI Runway Controller | Kubernetes | KAITO Operator | Dynamo Operator | KubeRay Operator |
|------------------------|------------|----------------|-----------------|------------------|
| v0.1.x                 | 1.26-1.30  | v0.3.x         | v1.0.x          | v1.1.x           |

| Provider | Minimum Version | CRD API Version     | Notes                                        |
|----------|-----------------|---------------------|----------------------------------------------|
| KAITO    | v0.3.0          | kaito.sh/v1beta1    | Requires GPU operator for GPU workloads      |
| Dynamo   | v1.0.0          | nvidia.com/v1alpha1 | Requires NVIDIA GPU operator; CRDs are bundled in the platform chart |
| KubeRay  | v1.1.0          | ray.io/v1           | Optional: KubeRay autoscaler for scaling     |
| llm-d    | Provider-specific | Provider-specific | Register an `InferenceProviderConfig`; compatibility follows the installed llm-d provider stack |
| Direct vLLM | v0.1.0 | apps/v1 `Deployment` | Repo-local provider shim is in `providers/vllm/deploy/vllm.yaml`; use `spec.engine.image` for the vLLM server image |

Controller version is independent of provider operator versions. The controller detects provider CRD versions dynamically.

---

**See also:** [Architecture Overview](architecture.md) | [CRD Reference](crd-reference.md)
