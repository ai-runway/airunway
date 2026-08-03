#!/usr/bin/env bash
#
# gpu-e2e.sh — build, deploy, and run the airunway GPU end-to-end suite against a
# pre-existing GPU cluster.
#
# This is the thin orchestration layer. It builds and pushes the controller and
# provider images, deploys them, installs any missing upstream operator, then
# hands off to the Go suite (test/e2e/gpu) which owns all ModelDeployment
# lifecycle, assertions, classification, and teardown.
#
# The cluster is never created or destroyed here. It must already have GPUs, the
# NVIDIA GPU operator, an RWX-capable StorageClass, and (for any run including
# dynamo) the inference gateway — see the preconditions in --help.

set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------
PROVIDER="all"
REGISTRY=""
IMG_TAG=""
PR=""
BUILD_NUMBER="${GITHUB_RUN_NUMBER:-}"
PLATFORM="linux/amd64"
STORAGE_CLASS="azurefile-premium"
HF_TOKEN="${HF_TOKEN:-}"
SKIP_INSTALL=false
SKIP_BUILD=false
KEEP=false

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# go test global timeout. It must exceed the worst-case single-case chain
# (upstream-CR 3m + scheduling 2m + Running 45m + gateway 5m + inference 3m +
# cleanup delete 6m ≈ 64m) with headroom, so the global timeout can never fire
# before a case's t.Cleanup runs and frees its GPU. Cases run in parallel, so
# wall-clock is the slowest single case, not the sum.
GO_TIMEOUT="75m"

# All providers the harness knows about (KubeRay is intentionally excluded).
ALL_PROVIDERS=(dynamo vllm kaito)

# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------
usage() {
    cat <<'EOF'
Usage: scripts/gpu-e2e.sh [flags]

Builds + deploys the airunway controller and providers, then runs the GPU e2e
suite (test/e2e/gpu) against the current kube context.

Flags:
  --provider <dynamo|vllm|kaito|all>  Which provider(s) to run (default: all)
  --registry <prefix>                 Image registry prefix, REQUIRED for build.
                                      Images are <prefix>/airunway/<component>:<tag>
  --img-tag <tag>                     Image tag. Default is generated; REQUIRED
                                      with --skip-build.
  --pr <n>                            PR number, woven into the generated tag.
  --build-number <n>                  Build number for the tag (default: $GITHUB_RUN_NUMBER).
  --platform <p>                      Build platform (default: linux/amd64).
  --storage-class <sc>                RWX-capable StorageClass for the Dynamo
                                      model cache (default: azurefile-premium).
  --hf-token <t>                      HuggingFace token (or HF_TOKEN env). Only
                                      needed for gated models; the default
                                      fixtures use a public model.
  --skip-install                      Do not install upstream operators even if
                                      absent (assume present).
  --skip-build                        Skip image build/push; requires --img-tag.
  --keep                              Leave ModelDeployments running after the
                                      test (for inspection).
  -h, --help                          Show this help.

Cluster preconditions (the harness installs none of these except a missing
operator via setup-<p>):
  * GPU nodes with the NVIDIA GPU operator + NFD (nvidia.com/gpu allocatable,
    nvidia.com/gpu.present=true).
  * An RWX-capable StorageClass (Azure Disk classes are RWO and will hang the
    Dynamo model-cache PVC). Pass --storage-class to override the default.
  * The inference gateway (Istio + GAIE + BBR + Gateway). On a fresh cluster
    `make -C providers/dynamo setup-dynamo` installs it; otherwise it must
    already be present and Programmed.
  * Pull access to the pushed images: the manager manifests carry no
    imagePullSecret, so the images must be public or the nodes must have pull
    access. New registry repos often default to private — make them public once.

Examples:
  scripts/gpu-e2e.sh --provider all --registry quay.io/surajd
  scripts/gpu-e2e.sh --provider vllm --registry quay.io/surajd
  scripts/gpu-e2e.sh --provider dynamo --skip-install --skip-build \
      --registry quay.io/surajd --img-tag gpu-e2e-40593eb-20260623-120000
EOF
}

die() {
    echo "❌ $*" >&2
    exit 1
}

log() {
    echo "▶ $*" >&2
}

# ---------------------------------------------------------------------------
# Flag parsing
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
    --provider)
        PROVIDER="$2"
        shift 2
        ;;
    --registry)
        REGISTRY="$2"
        shift 2
        ;;
    --img-tag)
        IMG_TAG="$2"
        shift 2
        ;;
    --pr)
        PR="$2"
        shift 2
        ;;
    --build-number)
        BUILD_NUMBER="$2"
        shift 2
        ;;
    --platform)
        PLATFORM="$2"
        shift 2
        ;;
    --storage-class)
        STORAGE_CLASS="$2"
        shift 2
        ;;
    --hf-token)
        HF_TOKEN="$2"
        shift 2
        ;;
    --skip-install)
        SKIP_INSTALL=true
        shift
        ;;
    --skip-build)
        SKIP_BUILD=true
        shift
        ;;
    --keep)
        KEEP=true
        shift
        ;;
    -h | --help)
        usage
        exit 0
        ;;
    *) die "unknown flag: $1 (see --help)" ;;
    esac
done

# Resolve the provider list.
case "$PROVIDER" in
all) PROVIDERS=("${ALL_PROVIDERS[@]}") ;;
dynamo | vllm | kaito) PROVIDERS=("$PROVIDER") ;;
kuberay) die "kuberay is not supported by this harness yet" ;;
*) die "invalid --provider: $PROVIDER" ;;
esac

run_includes() {
    local want="$1"
    for p in "${PROVIDERS[@]}"; do
        [[ "$p" == "$want" ]] && return 0
    done
    return 1
}

# ---------------------------------------------------------------------------
# Tooling + reachability
# ---------------------------------------------------------------------------
require_tools() {
    # docker is required even with --skip-build: preflight_pull runs
    # `docker manifest inspect` in both the build and skip-build paths.
    local tools=(kubectl helm jq go docker)
    # The Dynamo setup bundle (setup-dynamo -> setup-gateway) needs these.
    if run_includes dynamo && ! $SKIP_INSTALL; then
        tools+=(istioctl envsubst curl)
    fi
    local missing=()
    for t in "${tools[@]}"; do
        command -v "$t" >/dev/null 2>&1 || missing+=("$t")
    done
    [[ ${#missing[@]} -eq 0 ]] || die "missing required tools: ${missing[*]}"

    kubectl cluster-info >/dev/null 2>&1 || die "kubectl cannot reach a cluster"
    log "kube context: $(kubectl config current-context)"
}

# ---------------------------------------------------------------------------
# Image refs + tag
# ---------------------------------------------------------------------------
resolve_tag() {
    if [[ -n "$IMG_TAG" ]]; then
        return
    fi
    if $SKIP_BUILD; then
        die "--skip-build requires --img-tag (the generated tag is not reproducible)"
    fi
    local sha ts parts
    sha="$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
    ts="$(date +%Y%m%d-%H%M%S)"
    parts="gpu-e2e"
    [[ -n "$PR" ]] && parts="${parts}-${PR}"
    [[ -n "$BUILD_NUMBER" ]] && parts="${parts}-${BUILD_NUMBER}"
    IMG_TAG="${parts}-${sha}-${ts}"
}

# component_image <component> -> <registry>/airunway/<component>:<tag>
component_image() {
    echo "${REGISTRY}/airunway/$1:${IMG_TAG}"
}

# ---------------------------------------------------------------------------
# Build + push
# ---------------------------------------------------------------------------

# build_one <name> <make-dir> <target> <img-var> <img> <logfile> builds+pushes a
# single image, streaming output to a per-image log. It retries up to 3 times
# with a short backoff to absorb transient registry/daemon contention under the
# four concurrent buildx --push invocations.
build_one() {
    local name="$1" dir="$2" target="$3" imgvar="$4" img="$5" logfile="$6"
    local attempts=3 attempt
    for ((attempt = 1; attempt <= attempts; attempt++)); do
        if make -C "$dir" "$target" "${imgvar}=${img}" PUSH=true PLATFORM="$PLATFORM" \
            >"$logfile" 2>&1; then
            if [[ $attempt -eq 1 ]]; then
                log "✓ built ${name}: ${img}"
            else
                log "✓ built ${name} (attempt ${attempt}): ${img}"
            fi
            return 0
        fi
        if [[ $attempt -lt $attempts ]]; then
            log "… ${name} build failed (attempt ${attempt}/${attempts}), retrying in $((attempt * 5))s"
            sleep $((attempt * 5))
        fi
    done
    log "✗ build FAILED after ${attempts} attempts: ${name} (see output below)"
    sed "s/^/[${name}] /" "$logfile" >&2
    return 1
}

build_and_deliver() {
    # Push delivery only. (A `kind load` branch would go here for local clusters.)
    # The controller and provider images are independent, so build them in
    # parallel — each writes to its own log and we collect exit codes afterward.
    local tmpdir
    tmpdir="$(mktemp -d)"
    local pids=() names=()

    local controller_img
    controller_img="$(component_image controller)"
    log "building controller image: ${controller_img}"
    build_one "controller" "$REPO_ROOT" "controller-docker-build" \
        "CONTROLLER_IMG" "$controller_img" "$tmpdir/controller.log" &
    pids+=("$!")
    names+=("controller")

    local img
    for p in "${PROVIDERS[@]}"; do
        img="$(component_image "${p}-provider")"
        log "building ${p} provider image: ${img}"
        build_one "${p}-provider" "$REPO_ROOT/providers/$p" "docker-build" \
            "IMG" "$img" "$tmpdir/${p}.log" &
        pids+=("$!")
        names+=("${p}-provider")
    done

    # Wait for all builds; collect failures by name.
    local failed=()
    local i
    for i in "${!pids[@]}"; do
        if ! wait "${pids[$i]}"; then
            failed+=("${names[$i]}")
        fi
    done

    rm -rf "$tmpdir"
    if [[ ${#failed[@]} -gt 0 ]]; then
        die "image build failed for: ${failed[*]}"
    fi
    log "all images built and pushed"
}

# Verify pushed images are anonymously pullable (the cluster has no pull secret).
preflight_pull() {
    local refs=("$(component_image controller)")
    for p in "${PROVIDERS[@]}"; do
        refs+=("$(component_image "${p}-provider")")
    done
    for ref in "${refs[@]}"; do
        docker manifest inspect "$ref" >/dev/null 2>&1 ||
            die "image not pullable: ${ref}
  If this is a new registry repo, make it public, or grant the nodes pull access."
    done
    log "all images pullable"
}

# ---------------------------------------------------------------------------
# Operator install (gated on operator-deployment health)
# ---------------------------------------------------------------------------
# operator_deployment <provider> -> "<namespace> <label>" or empty (no operator)
# kaito_operator_available checks cluster-wide for a KAITO workspace controller
# Deployment with ready replicas, matching either the Helm chart
# (app.kubernetes.io/name=workspace, ns kaito-workspace) or the AKS AI-toolchain
# add-on (app.kubernetes.io/name=ai-toolchain-operator, ns kube-system). This
# mirrors providers/kaito/upstream_health.go, so an AKS add-on cluster is
# recognized instead of getting a second KAITO controller installed.
kaito_operator_available() {
    local ready
    ready="$(kubectl get deploy --all-namespaces \
        -l 'app.kubernetes.io/name in (workspace,ai-toolchain-operator)' \
        -o jsonpath='{range .items[*]}{.status.readyReplicas}{"\n"}{end}' 2>/dev/null || true)"
    grep -qE '^[1-9]' <<<"$ready"
}

operator_deployment() {
    case "$1" in
    dynamo) echo "dynamo-system app.kubernetes.io/name=dynamo-operator" ;;
    kaito) echo "kaito-workspace app.kubernetes.io/name=workspace" ;;
    vllm) echo "" ;; # no upstream operator
    esac
}

operator_available() {
    local ns="$1" label="$2"
    local avail
    avail="$(kubectl get deploy -n "$ns" -l "$label" \
        -o jsonpath='{.items[0].status.conditions[?(@.type=="Available")].status}' 2>/dev/null || true)"
    [[ "$avail" == "True" ]]
}

ensure_operator() {
    local p="$1"
    local spec ns label
    spec="$(operator_deployment "$p")"
    [[ -z "$spec" ]] && return 0 # vllm: nothing to install
    read -r ns label <<<"$spec"

    # KAITO can run from the Helm chart (kaito-workspace) or the AKS add-on
    # (kube-system); check both cluster-wide before deciding to install.
    if [[ "$p" == "kaito" ]] && kaito_operator_available; then
        log "kaito operator already Available (chart or AKS add-on); skipping setup-kaito"
        return 0
    fi

    if operator_available "$ns" "$label"; then
        log "${p} operator already Available; skipping setup-${p}"
        return 0
    fi
    if $SKIP_INSTALL; then
        die "${p} operator not Available and --skip-install set"
    fi
    # If the deployment exists but is not Available, it's broken — don't reinstall.
    if kubectl get deploy -n "$ns" -l "$label" -o name 2>/dev/null | grep -q .; then
        die "${p} operator present but not Available in ns/${ns}; remediate manually"
    fi
    log "installing ${p} operator via setup-${p}"
    make -C "$REPO_ROOT/providers/$p" "setup-${p}"
}

# ---------------------------------------------------------------------------
# Deploy controller + providers
# ---------------------------------------------------------------------------
deploy_airunway() {
    local controller_img
    controller_img="$(component_image controller)"
    log "deploying controller: ${controller_img}"
    make -C "$REPO_ROOT" controller-deploy CONTROLLER_IMG="${controller_img}"
    kubectl wait --for=condition=Available --timeout=300s \
        -n airunway-system deployment -l control-plane=controller-manager

    local img
    for p in "${PROVIDERS[@]}"; do
        img="$(component_image "${p}-provider")"
        log "deploying ${p} provider: ${img}"
        make -C "$REPO_ROOT/providers/$p" deploy IMG="${img}"
        kubectl wait --for=condition=Available --timeout=300s \
            -n airunway-system deployment -l "control-plane=${p}-provider"
        kubectl wait --for=jsonpath='{.status.ready}'=true --timeout=120s \
            "inferenceproviderconfig/${p}"
    done
}

# ---------------------------------------------------------------------------
# HF secret (only if a token was provided; the default model is public)
# ---------------------------------------------------------------------------
maybe_create_hf_secret() {
    [[ -z "$HF_TOKEN" ]] && return 0
    log "creating hf-token-secret in default namespace"
    # Feed the token via stdin (--from-file=...=/dev/stdin) rather than
    # --from-literal so it never appears in the process argv (ps / proc /
    # CI arg-logging).
    printf '%s' "$HF_TOKEN" | kubectl create secret generic hf-token-secret \
        --from-file=HF_TOKEN=/dev/stdin -n default \
        --dry-run=client -o yaml | kubectl apply -f -
}

# ---------------------------------------------------------------------------
# Run the Go suite
# ---------------------------------------------------------------------------
run_suite() {
    local run_filter=""
    if [[ "$PROVIDER" != "all" ]]; then
        run_filter="TestGPUProviders/${PROVIDER}"
    else
        run_filter="TestGPUProviders"
    fi

    local nproviders="${#PROVIDERS[@]}"
    log "running suite: -run ${run_filter} (parallel ${nproviders})"

    GPU_E2E_STORAGE_CLASS="$STORAGE_CLASS" \
        GPU_E2E_KEEP="$($KEEP && echo true || echo false)" \
        go test -C "$REPO_ROOT/test/e2e/gpu" -tags=e2e -v \
        -timeout "$GO_TIMEOUT" -parallel "$nproviders" -run "$run_filter" ./
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    require_tools

    if ! $SKIP_BUILD; then
        [[ -n "$REGISTRY" ]] || die "--registry is required when building.
  Direct: scripts/gpu-e2e.sh --registry quay.io/surajd
  Make:   make gpu-e2e GPU_E2E_ARGS=\"--registry quay.io/surajd\"
  (flags must go inside GPU_E2E_ARGS; 'make gpu-e2e --registry ...' passes the flag to make, not the script.)"
    fi
    resolve_tag
    log "image tag: ${IMG_TAG}"

    for p in "${PROVIDERS[@]}"; do
        ensure_operator "$p"
    done

    if $SKIP_BUILD; then
        [[ -n "$REGISTRY" ]] || die "--registry is required to resolve image refs"
        preflight_pull
    else
        build_and_deliver
        preflight_pull
    fi

    deploy_airunway
    maybe_create_hf_secret
    run_suite
}

main "$@"
