# KAITO provider

AI Runway installs KAITO workspace chart `0.10.0` with the BYO-nodes profile
from `providers/kaito/installation-values.json`. The automatic installer, the
generated Settings commands, and the Makefile use this same values file.

Running a generated Helm command or the make setup-kaito target requires
Node.js on the machine running Helm because the command uses a local CRD-retention
post-renderer. The packaged Airunway automatic installer uses its embedded
executable instead.

The profile disables node auto-provisioning, the NVIDIA device plugin, local CSI,
NFD, and GFD. Disabled dependencies contribute no workloads or CRDs. Automatic
installation applies only the chart's top-level CRDs, and only when missing;
template-managed KAITO CRDs remain part of the Helm release.

A render of this profile creates the `kaito-workspace` namespace and exactly
these cluster-scoped resources:

- CRD `inferencepools.inference.networking.k8s.io`
- CRD `inferenceobjectives.inference.networking.x-k8s.io`
- CRD `nodeclaims.karpenter.sh`
- CRD `inferencesets.kaito.sh`
- CRD `workspaces.kaito.sh`
- ClusterRole `kaito-workspace-clusterrole`
- ClusterRoleBinding `kaito-workspace-rolebinding`
- StorageClass `kaito-local-nvme-disk`
- ValidatingWebhookConfiguration `validation.workspace.kaito.sh`

## Uninstall behavior

Regular dashboard/API uninstall removes the remaining resources owned by the
Helm release, including the operator workloads, ClusterRole, ClusterRoleBinding,
StorageClass, and ValidatingWebhookConfiguration. It retains the
`kaito-workspace` namespace and all five CRDs. The three top-level chart CRDs
are outside the Helm release; `inferencesets.kaito.sh` and `workspaces.kaito.sh`
are kept in the release manifest with `helm.sh/resource-policy: keep`.
Existing KAITO custom resources remain intact.

Releases installed before this retention policy may not carry the keep metadata.
Regular uninstall fails closed for those releases and performs no uninstall.
After upgrading Airunway, retrieve the current command with
GET /api/installation/providers/kaito/commands and run it once to upgrade the
existing release with the BYO-node profile and retained-CRD post-renderer. Then
retry regular uninstall.

The explicit CRD-removal endpoint is a separate destructive operation:

```text
POST /api/installation/providers/kaito/uninstall-crds
```

It requires the Helm release to be gone and deletes only the two KAITO-specific
CRDs declared by the provider: `inferencesets.kaito.sh` and
`workspaces.kaito.sh`. It does not remove the three top-level chart CRDs
(`inferencepools.inference.networking.k8s.io`,
`inferenceobjectives.inference.networking.x-k8s.io`, and
`nodeclaims.karpenter.sh`) or the namespace. Ownership and custom-resource
checks run before deletion; a different tool's CRD ownership or any existing
custom resource blocks removal. A Kubernetes delete failure after preflight can
still leave a partial result, which the response reports per CRD.
