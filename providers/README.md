# Providers

> **Note:** These provider implementations are included in-tree temporarily for testing and development purposes. The intention is for all providers to live out-of-tree as independent operators.

## Inference providers

- `providers/dynamo`
- `providers/kaito`
- `providers/kuberay`
- `providers/llmd`

## Agent providers

Agent provider shims are split into separate modules so they can run out-of-tree as independent controllers:

- `providers/agent-container`
- `providers/agent-kagent`
- `providers/agent-orka`

These binaries currently reuse reconciler implementations exported through `controller/pkg/agentproviders`, which is the extraction seam used to decouple provider deployment from the main controller binary.
