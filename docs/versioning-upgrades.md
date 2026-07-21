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
# Option A: kubectl
kubectl apply -f https://raw.githubusercontent.com/kaito-project/airunway/main/deploy/controller.yaml

# Rollback to previous version
kubectl rollout undo deployment/airunway-controller-manager -n airunway-system
```

**Behavior during upgrade:**
- Controller deployment performs a rolling update (no downtime)
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
