#!/usr/bin/env bash
# =============================================================================
# AI Runway Gateway Body-Based Routing (BBR) Demo
# =============================================================================
#
# This script demonstrates deploying TWO models behind a single Gateway and
# validating that Body-Based Routing (BBR) correctly routes requests to the
# right model based on the "model" field in the request body.
#
# Each ModelDeployment uses a bring-your-own (BYO) HTTPRoute to avoid conflicts
# and give you full control over routing rules.
#
# Prerequisites:
#   - Docker
#   - Go 1.25+
#   - kubectl
#   - helm
#   - make (GNU Make)
#   - kustomize
#   - curl, jq
#
# Usage:
#   ./demos/gateway-bbr/demo.sh          # Run from repo root
#   SKIP_BUILD=1 ./demos/gateway-bbr/demo.sh   # Skip image builds (re-run faster)
#   CLEANUP_ONLY=1 ./demos/gateway-bbr/demo.sh # Just tear down the cluster
#
# =============================================================================

set -euo pipefail

# ---------------------------------------------------------------------------
# Upstream component versions — single source of truth at /versions.env.
# `set -a` exports every var sourced below so they're visible to helm/kubectl.
# ---------------------------------------------------------------------------
set -a
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/versions.env"
set +a

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-airunway-bbr-demo}"
CONTROLLER_IMG="${CONTROLLER_IMG:-airunway-controller:demo}"
KAITO_PROVIDER_IMG="${KAITO_PROVIDER_IMG:-kaito-provider:demo}"
NAMESPACE="${NAMESPACE:-default}"
GATEWAY_NAME="inference-gateway"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Model A — Llama 3.2 1B (CPU, baked-in weights via KAITO aikit)
MODEL_A_NAME="model-a"
MODEL_A_IMAGE="ghcr.io/kaito-project/aikit/llama3.2:1b"
MODEL_A_SERVED_NAME="llama-3.2-1b-instruct"

# Model B — Gemma 2 2B (CPU, baked-in weights via KAITO aikit)
MODEL_B_NAME="model-b"
MODEL_B_IMAGE="ghcr.io/kaito-project/aikit/gemma2:2b"
MODEL_B_SERVED_NAME="gemma-2-2b-instruct"

# Timeouts
WAIT_TIMEOUT_SECONDS=600
POLL_INTERVAL=10

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { echo "ℹ️  $*"; }
ok()    { echo "✅ $*"; }
fail()  { echo "❌ $*"; exit 1; }
warn()  { echo "⚠️  $*"; }

wait_for() {
  local description="$1"; shift
  local max_attempts="$1"; shift
  local interval="$1"; shift
  # remaining args are the command to run
  info "Waiting for ${description}..."
  for i in $(seq 1 "$max_attempts"); do
    if "$@" 2>/dev/null; then
      ok "${description}"
      return 0
    fi
    echo "  Attempt ${i}/${max_attempts}..."
    sleep "$interval"
  done
  fail "Timed out waiting for ${description}"
}

cleanup() {
  info "Deleting Kind cluster '${CLUSTER_NAME}'..."
  kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
  ok "Cluster deleted"
}

# Handle CLEANUP_ONLY mode
if [[ "${CLEANUP_ONLY:-}" == "1" ]]; then
  cleanup
  exit 0
fi

# Ensure we run from repo root
cd "${REPO_ROOT}"

# ---------------------------------------------------------------------------
# Step 1: Create Kind cluster
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 1: Create Kind cluster"
echo "============================================================"

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  warn "Cluster '${CLUSTER_NAME}' already exists, reusing it"
else
  info "Installing Kind (if needed)..."
  go install sigs.k8s.io/kind@latest

  info "Creating Kind cluster '${CLUSTER_NAME}'..."
  kind create cluster --name "${CLUSTER_NAME}" --wait 120s

  # Allow workloads on control plane for LoadBalancer access
  kubectl label node "${CLUSTER_NAME}-control-plane" \
    node.kubernetes.io/exclude-from-external-load-balancers- 2>/dev/null || true
fi
ok "Kind cluster ready"

# ---------------------------------------------------------------------------
# Step 2: Install cloud-provider-kind (for LoadBalancer IPs)
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 2: Install cloud-provider-kind"
echo "============================================================"

info "Starting cloud-provider-kind in background..."
go install sigs.k8s.io/cloud-provider-kind@latest
cloud-provider-kind --gateway-channel=disabled > /dev/null 2>&1 &
CPK_PID=$!
sleep 5
ok "cloud-provider-kind running (PID ${CPK_PID})"

# Kill cloud-provider-kind only on early exit (error/interrupt), not on success.
# The trap is cleared at the end of the script so it stays running after the demo.
trap 'kill ${CPK_PID} 2>/dev/null || true; echo ""; echo "To tear down the cluster: CLEANUP_ONLY=1 $0"' EXIT

# ---------------------------------------------------------------------------
# Step 3: Install Gateway API and Inference Extension CRDs
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 3: Install Gateway API & Inference Extension CRDs"
echo "============================================================"

kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml
ok "Gateway API CRDs installed"

kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/${GAIE_VERSION}/manifests.yaml"
ok "Inference Extension CRDs installed"

# ---------------------------------------------------------------------------
# Step 4: Install Istio with Inference Extension support
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 4: Install Istio"
echo "============================================================"

if ! command -v istioctl &>/dev/null; then
  info "Downloading Istio..."
  curl -sL https://istio.io/downloadIstio | sh -
  export PATH="${REPO_ROOT}/istio-*/bin:${PATH}"
fi

istioctl install --set profile=minimal \
  --set values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true -y

wait_for "Istio to be ready" 24 5 \
  kubectl wait --for=condition=Available deployment/istiod -n istio-system --timeout=5s

# ---------------------------------------------------------------------------
# Step 5: Install KAITO operator
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 5: Install KAITO operator"
echo "============================================================"

helm repo add kaito https://kaito-project.github.io/kaito/charts/kaito 2>/dev/null || true
helm repo update kaito
# --skip-crds: the Gateway API Inference Extension CRDs (including InferencePool) were
# already installed via kubectl apply above. Helm bundles the same CRDs and would
# conflict if it tried to re-install them with a different field manager.
# Note: This does not impact kaito's CRDs since they are not in the /crds folder.
helm install kaito-workspace kaito/workspace \
  --namespace kaito-workspace \
  --create-namespace \
  --set featureGates.disableNodeAutoProvisioning=true \
  --skip-crds 2>/dev/null || \
  warn "KAITO already installed (helm install returned non-zero)"

wait_for "KAITO operator to be ready" 24 5 \
  kubectl wait --for=condition=Available deployment -n kaito-workspace \
    -l app.kubernetes.io/name=workspace --timeout=5s

# ---------------------------------------------------------------------------
# Step 6: Build & deploy AI Runway controller + KAITO provider
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 6: Build & deploy controller and KAITO provider"
echo "============================================================"

if [[ "${SKIP_BUILD:-}" != "1" ]]; then
  info "Building controller image..."
  make controller-docker-build CONTROLLER_IMG="${CONTROLLER_IMG}"

  info "Building KAITO provider image..."
  make kaito-provider-docker-build KAITO_PROVIDER_IMG="${KAITO_PROVIDER_IMG}"
else
  warn "SKIP_BUILD=1 — skipping image builds"
fi

info "Loading images into Kind..."
kind load docker-image "${CONTROLLER_IMG}" --name "${CLUSTER_NAME}"
kind load docker-image "${KAITO_PROVIDER_IMG}" --name "${CLUSTER_NAME}"

info "Deploying controller..."
make controller-deploy CONTROLLER_IMG="${CONTROLLER_IMG}"
wait_for "controller to be ready" 24 5 \
  kubectl wait --for=condition=Available deployment -n airunway-system \
    -l control-plane=controller-manager --timeout=5s

info "Deploying KAITO provider..."
make kaito-provider-deploy KAITO_PROVIDER_IMG="${KAITO_PROVIDER_IMG}"
wait_for "KAITO provider to be ready" 24 5 \
  kubectl wait --for=condition=Available deployment -n airunway-system \
    -l control-plane=kaito-provider --timeout=5s

wait_for "KAITO InferenceProviderConfig to be registered" 24 5 \
  kubectl wait --for=jsonpath='{.status.ready}'=true inferenceproviderconfig/kaito --timeout=5s

# ---------------------------------------------------------------------------
# Step 7: Create the Gateway resource
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 7: Create Gateway"
echo "============================================================"

kubectl apply -f "${SCRIPT_DIR}/manifests/gateway.yaml"

info "Waiting for Gateway to be programmed..."
for i in $(seq 1 30); do
  PROGRAMMED=$(kubectl get gateway "${GATEWAY_NAME}" -n "${NAMESPACE}" \
    -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || echo "")
  if [[ "${PROGRAMMED}" == "True" ]]; then
    ok "Gateway is programmed"
    break
  fi
  echo "  Attempt ${i}/30: programmed=${PROGRAMMED}"
  if [[ "${i}" == "30" ]]; then
    warn "Gateway not programmed after 30 attempts — continuing (Kind may not support LoadBalancer)"
  fi
  sleep 5
done

# ---------------------------------------------------------------------------
# Step 8: Deploy two ModelDeployments with BYO HTTPRoutes
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 8: Deploy two models with BYO HTTPRoutes"
echo "============================================================"

info "Creating BYO HTTPRoutes..."
kubectl apply -f "${SCRIPT_DIR}/manifests/httproutes.yaml"
ok "BYO HTTPRoutes created"

info "Creating ModelDeployments..."
kubectl apply -f "${SCRIPT_DIR}/manifests/model-a.yaml"
kubectl apply -f "${SCRIPT_DIR}/manifests/model-b.yaml"
ok "ModelDeployments created"

# ---------------------------------------------------------------------------
# Step 9: Wait for both models to reach Running phase
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 9: Wait for models to be running"
echo "============================================================"

wait_for_model_running() {
  local name="$1"
  local max=$((WAIT_TIMEOUT_SECONDS / POLL_INTERVAL))
  info "Waiting for ModelDeployment '${name}' to reach Running phase..."

  # Wait for the underlying KAITO workspace first
  kubectl wait --for=condition=WorkspaceSucceeded "workspace/${name}" \
    -n "${NAMESPACE}" --timeout="${WAIT_TIMEOUT_SECONDS}s" 2>/dev/null || true

  for i in $(seq 1 "$max"); do
    PHASE=$(kubectl get modeldeployment "${name}" -n "${NAMESPACE}" \
      -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "${PHASE}" == "Running" ]]; then
      ok "ModelDeployment '${name}' is Running"
      return 0
    fi
    echo "  Attempt ${i}/${max}: phase=${PHASE}"
    sleep "${POLL_INTERVAL}"
  done
  fail "ModelDeployment '${name}' did not reach Running in time"
}

wait_for_model_running "${MODEL_A_NAME}"
wait_for_model_running "${MODEL_B_NAME}"

# ---------------------------------------------------------------------------
# Step 10: Verify InferencePools exist
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 10: Verify InferencePools"
echo "============================================================"

verify_inference_pool() {
  local name="$1"
  wait_for "InferencePool '${name}'" 30 5 \
    kubectl get inferencepool "${name}" -n "${NAMESPACE}"

  SELECTOR=$(kubectl get inferencepool "${name}" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.selector.matchLabels.airunway\.ai/model-deployment}')
  [[ "${SELECTOR}" == "${name}" ]] || fail "InferencePool '${name}' selector mismatch: got '${SELECTOR}'"
  ok "InferencePool '${name}' selector correct"
}

verify_inference_pool "${MODEL_A_NAME}"
verify_inference_pool "${MODEL_B_NAME}"

# ---------------------------------------------------------------------------
# Step 11: Verify BYO HTTPRoutes are accepted
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 11: Verify BYO HTTPRoutes"
echo "============================================================"

verify_httproute() {
  local name="$1"
  local expected_model="$2"

  PARENT=$(kubectl get httproute "${name}-route" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.parentRefs[0].name}')
  [[ "${PARENT}" == "${GATEWAY_NAME}" ]] || \
    fail "HTTPRoute '${name}-route' parent mismatch: expected '${GATEWAY_NAME}', got '${PARENT}'"
  ok "HTTPRoute '${name}-route' parent ref correct"

  BACKEND_GROUP=$(kubectl get httproute "${name}-route" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.rules[0].backendRefs[0].group}')
  BACKEND_KIND=$(kubectl get httproute "${name}-route" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.rules[0].backendRefs[0].kind}')
  [[ "${BACKEND_GROUP}" == "inference.networking.k8s.io" && "${BACKEND_KIND}" == "InferencePool" ]] || \
    fail "HTTPRoute '${name}-route' backend ref mismatch: group=${BACKEND_GROUP} kind=${BACKEND_KIND}"
  ok "HTTPRoute '${name}-route' backend ref → InferencePool"
}

verify_httproute "${MODEL_A_NAME}" "${MODEL_A_SERVED_NAME}"
verify_httproute "${MODEL_B_NAME}" "${MODEL_B_SERVED_NAME}"

# ---------------------------------------------------------------------------
# Step 12: Wait for EPPs to be ready
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 12: Wait for Endpoint Picker Proxies (EPP)"
echo "============================================================"

wait_for_epp() {
  local name="$1"
  wait_for "EPP '${name}-epp' to be ready" 30 10 \
    kubectl rollout status deployment "${name}-epp" -n "${NAMESPACE}" --timeout=5s
}

wait_for_epp "${MODEL_A_NAME}"
wait_for_epp "${MODEL_B_NAME}"

# ---------------------------------------------------------------------------
# Step 13: Confirm Istio DestinationRules were created for EPPs
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 13: Confirm Istio DestinationRules were created for EPPs"
echo "============================================================"

wait_for_dr() {
  local name="$1"
  wait_for "DestinationRule '${name}-epp' to be ready" 30 10 \
    kubectl get destinationrules "${name}-epp" -n "${NAMESPACE}" --timeout=5s
}

wait_for_dr "${MODEL_A_NAME}"
wait_for_dr "${MODEL_B_NAME}"

# ---------------------------------------------------------------------------
# Step 14: Install Body-Based Router (BBR)
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 14: Install Body-Based Router (BBR)"
echo "============================================================"

helm install body-based-router \
  --set provider.name=istio \
  --version "${GAIE_VERSION}" \
  oci://registry.k8s.io/gateway-api-inference-extension/charts/body-based-routing \
  --wait --timeout 120s 2>/dev/null || \
  warn "BBR already installed"
ok "BBR installed"

# ---------------------------------------------------------------------------
# Step 15: Test Body-Based Routing — send traffic to BOTH models
# ---------------------------------------------------------------------------
echo ""
echo "============================================================"
echo " Step 15: Test Body-Based Routing"
echo "============================================================"

# Resolve Gateway IP
GW_IP=""
for i in $(seq 1 30); do
  GW_IP=$(kubectl get gateway "${GATEWAY_NAME}" -n "${NAMESPACE}" \
    -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || echo "")
  if [[ -n "${GW_IP}" ]]; then
    ok "Gateway IP: ${GW_IP}"
    break
  fi
  echo "  Waiting for Gateway IP... attempt ${i}/30"
  sleep 5
done
[[ -n "${GW_IP}" ]] || fail "Gateway IP not assigned"

# --- Helper: send an inference request and validate the response ----------
send_inference() {
  local model_served_name="$1"
  local label="$2"

  info "Sending inference request for '${model_served_name}' (${label})..."
  for attempt in $(seq 1 18); do
    HTTP_CODE=$(curl -s -o /tmp/bbr_response.json -w '%{http_code}' --max-time 30 \
      "http://${GW_IP}/v1/chat/completions" \
      -H "Content-Type: application/json" \
      -d "{
        \"model\": \"${model_served_name}\",
        \"messages\": [{\"role\": \"user\", \"content\": \"Say hello in one word.\"}],
        \"max_tokens\": 10
      }" 2>/dev/null || true)
    RESPONSE=$(cat /tmp/bbr_response.json 2>/dev/null || echo "")

    if [[ "${HTTP_CODE}" == "200" ]] && echo "${RESPONSE}" | jq -e '.choices[0].message.content' >/dev/null 2>&1; then
      CONTENT=$(echo "${RESPONSE}" | jq -r '.choices[0].message.content')
      RESP_MODEL=$(echo "${RESPONSE}" | jq -r '.model // "unknown"')
      ok "${label} responded (model=${RESP_MODEL}): ${CONTENT}"
      return 0
    fi
    echo "  Attempt ${attempt}/18: HTTP=${HTTP_CODE} body=$(echo "${RESPONSE}" | head -c 200)"
    sleep 10
  done
  fail "Inference request to '${model_served_name}' (${label}) failed after all retries"
}

# Get the model names the gateway knows about
MODEL_A_GW_NAME=$(kubectl get modeldeployment "${MODEL_A_NAME}" -n "${NAMESPACE}" \
  -o jsonpath='{.status.gateway.modelName}' 2>/dev/null || echo "${MODEL_A_SERVED_NAME}")
MODEL_B_GW_NAME=$(kubectl get modeldeployment "${MODEL_B_NAME}" -n "${NAMESPACE}" \
  -o jsonpath='{.status.gateway.modelName}' 2>/dev/null || echo "${MODEL_B_SERVED_NAME}")

info "Model A gateway name: ${MODEL_A_GW_NAME}"
info "Model B gateway name: ${MODEL_B_GW_NAME}"

echo ""
info "--- Sending request to Model A ---"
send_inference "${MODEL_A_GW_NAME}" "Model A"

echo ""
info "--- Sending request to Model B ---"
send_inference "${MODEL_B_GW_NAME}" "Model B"

# ---------------------------------------------------------------------------
# Step 16: Summary
# ---------------------------------------------------------------------------

# Clear the EXIT trap so cloud-provider-kind stays running after the demo
trap - EXIT

echo ""
echo "============================================================"
echo " 🎉 Demo Complete!"
echo "============================================================"
echo ""
echo "Two ModelDeployments are running behind a single Gateway with"
echo "Body-Based Routing (BBR). Requests are routed to the correct"
echo "model based on the 'model' field in the JSON body."
echo ""
echo "  Gateway endpoint : http://${GW_IP}"
echo "  Model A          : ${MODEL_A_SERVED_NAME} (${MODEL_A_NAME})"
echo "  Model B          : ${MODEL_B_SERVED_NAME} (${MODEL_B_NAME})"
echo ""
echo "Try it yourself:"
echo ""
echo "  curl http://${GW_IP}/v1/chat/completions \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"model\": \"${MODEL_A_SERVED_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hi\"}]}'"
echo ""
echo "  curl http://${GW_IP}/v1/chat/completions \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"model\": \"${MODEL_B_SERVED_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hi\"}]}'"
echo ""
echo "To tear down:"
echo "  CLEANUP_ONLY=1 ./demos/gateway-bbr/demo.sh"
echo ""
