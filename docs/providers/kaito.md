# KAITO provider

AI Runway installs the KAITO workspace chart in the `kaito-workspace`
namespace using the BYO-nodes profile from `providers/kaito/Makefile` and the
provider installation metadata:

- Node auto-provisioning is disabled.
- The NVIDIA device-plugin, local CSI driver, NFD, and GFD dependencies are
  disabled.
- Only the workspace chart's own top-level CRDs are pre-installed when they are
  missing. CRDs from disabled dependencies are not applied.

## Uninstall behavior

The regular dashboard/API uninstall removes the Helm release resources only.
It preserves the `kaito-workspace` namespace, upstream KAITO CRDs, and all
KAITO custom resources.

Complete CRD removal is a separate destructive operation. It requires the
Helm release to be gone, refuses CRDs owned by another tool, and refuses to
delete a CRD while custom resources exist. Use:

```text
POST /api/installation/providers/kaito/uninstall-crds
```

The endpoint preflights all declared provider CRDs before deleting any of them.
