#!/usr/bin/env bash
set -euo pipefail

readonly HELM_VERSION=v3.18.4
readonly HELM_SHA256=f8180838c23d7c7d797b208861fecb591d9ce1690d8704ed1e4cb8e2add966c1
readonly KAITO_VERSION=0.10.0
readonly CHART_SHA256=199bf46a9379dd61ec3f7a5a7ec89a5c0a5d0be442047f1b937d905f2e48812d
readonly REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
readonly WORK_DIR="$(mktemp -d /tmp/kaito-install-profile.XXXXXX)"
trap 'rm -rf -- "$WORK_DIR"' EXIT

helm_bin="$(command -v helm || true)"
helm_candidate_version=""
if [[ -n "$helm_bin" ]]; then
  helm_candidate_version="$("$helm_bin" version --template '{{ .Version }}' 2>/dev/null || true)"
fi

if [[ "$helm_candidate_version" != "$HELM_VERSION" ]]; then
  case "$(uname -s):$(uname -m)" in
    Linux:x86_64|Linux:amd64)
      ;;
    *)
      echo "This profile check requires Helm $HELM_VERSION on Linux amd64." >&2
      exit 1
      ;;
  esac

  archive="$WORK_DIR/helm.tar.gz"
  curl --retry 2 --retry-all-errors -fsSL \
    "https://get.helm.sh/helm-$HELM_VERSION-linux-amd64.tar.gz" \
    -o "$archive"
  printf '%s  %s\n' "$HELM_SHA256" "$archive" | sha256sum --check -
  tar -xzf "$archive" -C "$WORK_DIR"
  helm_bin="$WORK_DIR/linux-amd64/helm"
fi

export HELM_CONFIG_HOME="$WORK_DIR/helm/config"
export HELM_CACHE_HOME="$WORK_DIR/helm/cache"
export HELM_DATA_HOME="$WORK_DIR/helm/data"
mkdir -p "$HELM_CONFIG_HOME" "$HELM_CACHE_HOME" "$HELM_DATA_HOME" "$WORK_DIR/chart"

"$helm_bin" repo add kaito https://kaito-project.github.io/kaito/charts/kaito --force-update
"$helm_bin" repo update kaito
"$helm_bin" pull kaito/workspace --version "$KAITO_VERSION" --destination "$WORK_DIR/chart"

chart="$WORK_DIR/chart/workspace-$KAITO_VERSION.tgz"
printf '%s  %s\n' "$CHART_SHA256" "$chart" | sha256sum --check -

rendered="$WORK_DIR/profile.yaml"
"$helm_bin" template kaito-workspace "$chart" \
  --namespace kaito-workspace \
  --include-crds \
  --values "$REPO_ROOT/providers/kaito/installation-values.json" \
  > "$rendered"

actual_resources="$(awk '
function emit() {
  gsub(/"/, "", name)
  if (kind ~ /^(CustomResourceDefinition|ClusterRole|ClusterRoleBinding|ValidatingWebhookConfiguration|StorageClass)$/) {
    print kind "/" name
  }
}
/^---$/ { emit(); kind=""; name=""; in_metadata=0; next }
/^kind: / && kind == "" { kind=$2; next }
/^metadata:$/ { in_metadata=1; next }
in_metadata && /^  name: / && name == "" { name=$2; next }
in_metadata && /^[^ ]/ { in_metadata=0 }
END { emit() }
' "$rendered" | sort)"
expected_resources="$(printf '%s\n' \
  'ClusterRole/kaito-workspace-clusterrole' \
  'ClusterRoleBinding/kaito-workspace-rolebinding' \
  'CustomResourceDefinition/inferenceobjectives.inference.networking.x-k8s.io' \
  'CustomResourceDefinition/inferencepools.inference.networking.k8s.io' \
  'CustomResourceDefinition/inferencesets.kaito.sh' \
  'CustomResourceDefinition/nodeclaims.karpenter.sh' \
  'CustomResourceDefinition/workspaces.kaito.sh' \
  'StorageClass/kaito-local-nvme-disk' \
  'ValidatingWebhookConfiguration/validation.workspace.kaito.sh' | sort)"

if [[ "$actual_resources" != "$expected_resources" ]]; then
  diff -u <(printf '%s\n' "$expected_resources") <(printf '%s\n' "$actual_resources") || true
  echo "KAITO cluster-scoped install footprint changed." >&2
  exit 1
fi

disabled_workloads="$(awk '
function emit() {
  if (kind ~ /^(Deployment|DaemonSet)$/ && name ~ /(node-feature-discovery|nvidia-device-plugin|csi-local)/) {
    print kind "/" name
  }
}
/^---$/ { emit(); kind=""; name=""; in_metadata=0; next }
/^kind: / && kind == "" { kind=$2; next }
/^metadata:$/ { in_metadata=1; next }
in_metadata && /^  name: / && name == "" { name=$2; next }
in_metadata && /^[^ ]/ { in_metadata=0 }
END { emit() }
' "$rendered")"
if [[ -n "$disabled_workloads" ]]; then
  printf 'Disabled dependencies rendered workloads:\n%s\n' "$disabled_workloads" >&2
  exit 1
fi

release_manifest="$WORK_DIR/release.yaml"
"$helm_bin" template kaito-workspace "$chart" \
  --namespace kaito-workspace \
  --skip-crds \
  --values "$REPO_ROOT/providers/kaito/installation-values.json" \
  --post-renderer "$REPO_ROOT/providers/kaito/keep-crd-resources.js" \
  > "$release_manifest"
node "$REPO_ROOT/providers/kaito/keep-crd-resources.js" \
  --check-kept inferencesets.kaito.sh workspaces.kaito.sh \
  < "$release_manifest"

printf 'Verified KAITO %s install footprint and retained release CRDs.\n' "$KAITO_VERSION"
