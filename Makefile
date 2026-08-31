.PHONY: install dev dev-frontend dev-backend build compile lint test test-coverage test-coverage-backend test-coverage-frontend clean help providers-test gpu-e2e gpu-e2e-check verify-versions test-verify-versions
.PHONY: controller-build controller-docker-build controller-install controller-deploy controller-generate generate-deploy-manifests
.PHONY: model-downloader-docker-build setup-gateway cleanup-gateway

# Controller image
CONTROLLER_IMG ?= ghcr.io/ai-runway/airunway/controller:latest

# Dashboard image
DASHBOARD_IMG ?= ghcr.io/ai-runway/airunway/dashboard:latest

# Model downloader image
MODEL_DOWNLOADER_IMG ?= ghcr.io/ai-runway/airunway/model-downloader:latest

# Image build settings
PLATFORM ?= linux/amd64
PUSH ?= false
PUSH_ENABLED := $(filter true TRUE 1 yes YES on ON,$(PUSH))
IMAGE_OUTPUT_FLAG := $(if $(PUSH_ENABLED),--push,--load)

# Upstream component versions. Single source of truth at the repo root.
# Edit versions.env to bump GAIE_VERSION, DYNAMO_VERSION, etc.
include versions.env
export

# Default target
help:
	@echo "AI Runway Development Commands"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  install                Install all dependencies"
	@echo "  dev                    Start frontend and backend dev servers"
	@echo "  dev-frontend           Start frontend dev server only"
	@echo "  dev-backend            Start backend dev server only"
	@echo "  build                  Build all packages"
	@echo "  compile                Build single binary executable"
	@echo "  compile-all            Cross-compile for all platforms"
	@echo "  compile-linux          Cross-compile for Linux (x64 + arm64)"
	@echo "  compile-darwin         Cross-compile for macOS (x64 + arm64)"
	@echo "  compile-windows        Cross-compile for Windows (x64)"
	@echo "  lint                   Run linters"
	@echo "  test                   Run tests"
	@echo "  test-coverage          Run tests with coverage (frontend + backend)"
	@echo "  clean                  Remove build artifacts and node_modules"
	@echo ""
	@echo "Controller Targets:"
	@echo "  controller-build       Build the Go controller binary"
	@echo "  controller-test        Run controller tests"
	@echo "  controller-run         Run controller locally (outside cluster)"
	@echo "  controller-docker-build Build controller Docker image"
	@echo "  controller-generate    Generate CRD manifests and code"
	@echo "  model-downloader-docker-build Build model downloader Docker image"
	@echo "  controller-install     Install CRDs into cluster"
	@echo "  controller-uninstall   Uninstall CRDs from cluster"
	@echo "  controller-deploy      Deploy controller to cluster"
	@echo "  controller-undeploy    Undeploy controller from cluster"
	@echo "  generate-deploy-manifests  Generate deploy/ manifests"
	@echo ""
	@echo "Provider Targets:"
	@echo "  providers-test         Run all provider tests"
	@echo "  gpu-e2e                Run GPU e2e suite on a GPU cluster (GPU_E2E_ARGS=...)"
	@echo "  gpu-e2e-check          Cluster-free checks for the GPU e2e module (gofmt, vet, compile, unit tests)"
	@echo ""
	@echo "Cluster Setup Targets:"
	@echo "  setup-gateway          Install Gateway API CRDs, Istio, BBR, and the inference Gateway"
	@echo "  cleanup-gateway        Remove the inference Gateway and BBR (CRDs/Istio left intact)"
	@echo ""
	@echo "Image Build Variables:"
	@echo "  PLATFORM=<platform>    Target platform for image builds (default: linux/amd64)"
	@echo "  PUSH=true              Push image instead of loading it locally (default: false)"
	@echo ""
	@echo "  help                   Show this help message"

# Install dependencies
install:
	bun install

# Development servers
dev:
	bun run dev

dev-frontend:
	bun run dev:frontend

dev-backend:
	bun run dev:backend

# Build
build: verify-versions
	bun run build

# Compile single binary (includes frontend)
compile: verify-versions
	bun run compile
	@echo ""
	@echo "✅ Binary created: dist/airunway (includes frontend)"
	@ls -lh dist/airunway

# Cross-compile for all platforms
compile-all: compile-linux compile-darwin compile-windows
	@echo ""
	@echo "✅ All binaries created in dist/"
	@ls -lh dist/

compile-linux: verify-versions
	bun run build:frontend
	cd backend && bun run compile:linux-x64
	cd backend && bun run compile:linux-arm64
	@echo "✅ Linux binaries created"

compile-darwin: verify-versions
	bun run build:frontend
	cd backend && bun run compile:darwin-x64
	cd backend && bun run compile:darwin-arm64
	@echo "✅ macOS binaries created"

compile-windows: verify-versions
	bun run build:frontend
	cd backend && bun run compile:windows-x64
	@echo "✅ Windows binary created"

# Linting
lint:
	bun run lint

# Testing
test: verify-versions
	bun run test

# Testing with coverage (CI entrypoint). Coverage prints to stdout;
# GitHub step-summary formatting lives in the workflow, not here.
# Uses `cd <ws> && bun run test:coverage` (not `bun run --filter`) so output
# stays unprefixed and the coverage tables render cleanly in the CI summary.
test-coverage: verify-versions test-coverage-backend test-coverage-frontend

test-coverage-backend:
	cd backend && bun run test:coverage

test-coverage-frontend:
	cd frontend && bun run test:coverage

# Clean build artifacts
clean:
	rm -rf node_modules frontend/node_modules backend/node_modules shared/node_modules
	rm -rf dist frontend/dist backend/dist shared/dist
	rm -f bun.lockb
	@echo "✅ Cleaned all build artifacts"

# ==================== Controller Targets ====================

# Build the controller binary
controller-build: verify-versions
	cd controller && $(MAKE) VERIFIED_VERSIONS=1 build
	@echo "✅ Controller binary built: controller/bin/manager"

# Build controller Docker image
controller-docker-build: verify-versions
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) --build-arg GAIE_VERSION=$(GAIE_VERSION) -f controller/Dockerfile -t $(CONTROLLER_IMG) .
	@echo "✅ Controller image built: $(CONTROLLER_IMG) ($(PLATFORM), $(if $(PUSH_ENABLED),pushed,loaded locally))"

# Generate CRD manifests and deep copy code
controller-generate:
	cd controller && make generate manifests
	@echo "✅ Generated CRDs and code"

# Install CRDs into the K8s cluster
controller-install:
	cd controller && make install
	@echo "✅ CRDs installed into cluster"

# Deploy controller to the K8s cluster
controller-deploy:
	cd controller && make deploy IMG=$(CONTROLLER_IMG)
	@echo "✅ Controller deployed to cluster"

# Uninstall CRDs from the K8s cluster
controller-uninstall:
	cd controller && make uninstall
	@echo "✅ CRDs uninstalled from cluster"

# Undeploy controller from the K8s cluster
controller-undeploy:
	cd controller && make undeploy
	@echo "✅ Controller undeployed from cluster"

# Run controller locally (outside cluster)
controller-run:
	cd controller && go run ./cmd/main.go --enable-provider-selector=true

# Run controller tests
controller-test: verify-versions
	cd controller && go test ./... -coverprofile cover.out
	@echo "✅ Controller tests completed"

# Run provider tests
providers-test: verify-versions
	cd providers/dynamo && go test ./...
	cd providers/kaito && go test ./...
	cd providers/kuberay && go test ./...
	cd providers/llmd && go test ./...
	cd providers/vllm && go test ./...
	@echo "✅ Provider tests completed"

# Run the GPU end-to-end suite against a pre-existing GPU cluster.
# All logic lives in scripts/gpu-e2e.sh; pass flags via GPU_E2E_ARGS.
# Example: make gpu-e2e GPU_E2E_ARGS="--provider all --registry quay.io/surajd"
gpu-e2e:
	@bash scripts/gpu-e2e.sh $(GPU_E2E_ARGS)

# Cluster-free validation of the GPU e2e module (test/e2e/gpu). Runs in CI on a
# plain runner: it never touches a cluster. Three guarantees:
#   1. gofmt   — the module stays formatted.
#   2. vet + compile under -tags=e2e — the cluster-coupled suite keeps building
#      even though CI never runs it (catches selector/API drift at PR time).
#   3. unit tests for the cluster-free packages (sched, e2eutil) — the
#      classifier, response parser, and storage-class injector are exercised
#      for real. These carry no build tag, so `go test` picks them up directly.
gpu-e2e-check:
	@echo "▶ gofmt"
	@test -z "$$(gofmt -l test/e2e/gpu)" || { echo "❌ gofmt: run 'gofmt -w test/e2e/gpu'"; gofmt -l test/e2e/gpu; exit 1; }
	@echo "▶ go vet (-tags=e2e)"
	go vet -C test/e2e/gpu -tags=e2e ./...
	@echo "▶ compile e2e suite (-tags=e2e)"
	go test -C test/e2e/gpu -tags=e2e -c -o /dev/null ./
	@echo "▶ unit tests (cluster-free packages)"
	go test -C test/e2e/gpu ./sched/ ./e2eutil/
	@echo "✅ GPU e2e module checks passed"

# Generate deploy manifests for controller and dashboard
generate-deploy-manifests:
	cd controller && $(MAKE) kustomize
	cd controller/config/manager && ../../bin/kustomize edit set image controller=$(CONTROLLER_IMG)
	cd controller && bin/kustomize build config/default > ../deploy/controller.yaml
	@echo "✅ Generated deploy/controller.yaml"
	cd backend/config/manager && ../../../controller/bin/kustomize edit set image IMAGE_PLACEHOLDER=$(DASHBOARD_IMG)
	controller/bin/kustomize build backend/config/default > deploy/dashboard.yaml
	@git checkout backend/config/manager/kustomization.yaml 2>/dev/null || true
	@echo "✅ Generated deploy/dashboard.yaml"

# ==================== Model Downloader Targets ====================

# Build model downloader Docker image
model-downloader-docker-build:
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) -f images/model-downloader/Dockerfile -t $(MODEL_DOWNLOADER_IMG) images/model-downloader
	@echo "✅ Model downloader image built: $(MODEL_DOWNLOADER_IMG) ($(PLATFORM), $(if $(PUSH_ENABLED),pushed,loaded locally))"

# ==================== Cluster Setup Targets ====================

# Provider-agnostic inference-gateway bootstrap: installs Gateway API CRDs,
# Istio (with the Gateway API Inference Extension enabled), the Body-Based
# Router, and an `inference-gateway` Gateway resource. Required before
# deploying any provider that routes through the inference gateway. Versions
# are pinned in versions.env (GATEWAY_API_VERSION, ISTIO_VERSION, GAIE_VERSION).
GATEWAY_NAMESPACE ?= default
GATEWAY_NAME ?= inference-gateway
GATEWAY_API_URL := https://github.com/kubernetes-sigs/gateway-api/releases/download/$(GATEWAY_API_VERSION)/standard-install.yaml
GATEWAY_MANIFEST := hack/inference-gateway.yaml
BBR_CHART := oci://registry.k8s.io/gateway-api-inference-extension/charts/body-based-routing
GAIE_MANIFEST_URL := https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/$(GAIE_VERSION)/manifests.yaml

setup-gateway: verify-versions
	@command -v istioctl >/dev/null 2>&1 || { echo "❌ istioctl not found on PATH. Install Istio $(ISTIO_VERSION): https://istio.io/latest/docs/setup/getting-started/"; exit 1; }
	@echo "Installing Gateway API CRDs $(GATEWAY_API_VERSION)..."
	kubectl apply -f $(GATEWAY_API_URL)
	@echo "Installing Gateway API Inference Extension (GAIE) CRDs $(GAIE_VERSION)..."
	kubectl apply -f $(GAIE_MANIFEST_URL)
	@echo "Installing Istio $(ISTIO_VERSION) (inference extension enabled)..."
	istioctl install --skip-confirmation \
		--set profile=minimal \
		--set tag=$(ISTIO_VERSION) \
		--set values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true
	@echo "Installing Body-Based Router (BBR) $(GAIE_VERSION) into namespace $(GATEWAY_NAMESPACE)..."
	helm upgrade -i body-based-router \
		--namespace $(GATEWAY_NAMESPACE) --create-namespace \
		--set provider.name=istio \
		--version "$(GAIE_VERSION)" \
		--wait \
		$(BBR_CHART)
	@echo "Creating Gateway resource $(GATEWAY_NAME) in namespace $(GATEWAY_NAMESPACE)..."
	@command -v envsubst >/dev/null 2>&1 || { echo "❌ envsubst not found on PATH (provided by gettext)."; exit 1; }
	GATEWAY_NAME=$(GATEWAY_NAME) GATEWAY_NAMESPACE=$(GATEWAY_NAMESPACE) \
		envsubst < $(GATEWAY_MANIFEST) | kubectl apply -f -
	@echo "✅ Inference gateway ready (Istio $(ISTIO_VERSION), gateway/$(GATEWAY_NAME))"

# Tear down the inference Gateway and BBR. Gateway API CRDs and Istio are left
# intact because they may be shared with other workloads.
cleanup-gateway:
	@command -v envsubst >/dev/null 2>&1 || { echo "❌ envsubst not found on PATH (provided by gettext)."; exit 1; }
	@GATEWAY_NAME=$(GATEWAY_NAME) GATEWAY_NAMESPACE=$(GATEWAY_NAMESPACE) \
		envsubst < $(GATEWAY_MANIFEST) | kubectl delete -f - --ignore-not-found
	@helm uninstall body-based-router --namespace $(GATEWAY_NAMESPACE) --ignore-not-found || helm uninstall body-based-router --namespace $(GATEWAY_NAMESPACE) || true
	@echo "⚠️ Gateway API CRDs, GAIE CRDs, and Istio left intact (may be shared). Remove manually if needed:"
	@echo "    kubectl delete -f \"$(GATEWAY_API_URL)\" --ignore-not-found"
	@echo "    kubectl delete -f \"$(GAIE_MANIFEST_URL)\" --ignore-not-found"
	@echo "    istioctl uninstall --purge -y"
	@echo "✅ Inference gateway and BBR removed"

# ==================== Version Drift Guard ====================

# Verify all version references are in sync with versions.env.
# Wired as a prerequisite of every build/test target so drift is caught
# the moment it is introduced.
#
# Note: Escape dots so version literals are matched as fixed strings inside
# regexes, preventing e.g. "1.5.0" from also matching "1X5Y0".
GAIE_VERSION_RE := $(subst .,\.,$(GAIE_VERSION))
DYNAMO_VERSION_RE := $(subst .,\.,$(DYNAMO_VERSION))
KAITO_VERSION_RE := $(subst .,\.,$(KAITO_VERSION))
VLLM_VERSION_RE := $(subst .,\.,$(VLLM_VERSION))
LLMD_VERSION_RE := $(subst .,\.,$(LLMD_VERSION))

verify-versions:
	@# 1. controller/go.mod must pin GAIE_VERSION
	@grep -qE "gateway-api-inference-extension v?$(GAIE_VERSION_RE)([[:space:]]|$$)" controller/go.mod || \
	  { echo "❌ controller/go.mod GAIE version != $(GAIE_VERSION) (from versions.env)"; exit 1; }
	@# 2. providers/dynamo/config.go fallback literal must match DYNAMO_VERSION
	@grep -qE '^var DynamoVersion = "$(DYNAMO_VERSION_RE)"$$' providers/dynamo/config.go || \
	  { echo "❌ providers/dynamo/config.go DynamoVersion fallback != $(DYNAMO_VERSION) (from versions.env)"; exit 1; }
	@# 3. controller/internal/gateway/detection.go fallback literal must match GAIE_VERSION
	@grep -qE '^var DefaultGAIEVersion = "$(GAIE_VERSION_RE)"$$' controller/internal/gateway/detection.go || \
	  { echo "❌ controller/internal/gateway/detection.go DefaultGAIEVersion fallback != $(GAIE_VERSION) (from versions.env)"; exit 1; }
	@# 4. providers/kaito/config.go chart version literal must match KAITO_VERSION
	@grep -qE 'Version:[[:space:]]+"$(KAITO_VERSION_RE)"' providers/kaito/config.go || \
	  { echo "❌ providers/kaito/config.go chart Version != $(KAITO_VERSION) (from versions.env)"; exit 1; }
	@# 5. providers/kaito/config.go install Command --version arg must match KAITO_VERSION
	@grep -qE -- '--version $(KAITO_VERSION_RE) ' providers/kaito/config.go || \
	  { echo "❌ providers/kaito/config.go install Command --version != $(KAITO_VERSION) (from versions.env)"; exit 1; }
	@# 6. providers/vllm/transformer.go fallback literal must match VLLM_VERSION
	@grep -qE '^var VLLMVersion = "$(VLLM_VERSION_RE)"$$' providers/vllm/transformer.go || \
	  { echo "❌ providers/vllm/transformer.go VLLMVersion fallback != $(VLLM_VERSION) (from versions.env)"; exit 1; }
	@# 7. providers/llmd/config.go fallback literal must match LLMD_VERSION
	@grep -qE '^var LLMDSchedulerImage = "ghcr\.io/llm-d/llm-d-inference-scheduler:v$(LLMD_VERSION_RE)"$$' providers/llmd/config.go || \
	  { echo "❌ providers/llmd/config.go LLMDSchedulerImage tag != $(LLMD_VERSION) (from versions.env)"; exit 1; }
	@# 8. generated TS must be in sync with versions.env.
	@#    Generate to a temp file and diff against the working-tree copy so
	@#    that synced uncommitted edits pass (the local-dev case) while
	@#    stale committed files still fail (the CI case — CI's working
	@#    tree equals HEAD). Crucially this does NOT mutate the working
	@#    tree, unlike a regenerate-in-place + `git diff HEAD` approach.
	@set -e; \
	  if ! command -v bun >/dev/null 2>&1; then \
	    echo "❌ bun not found. Install bun (https://bun.sh) — it is the project standard."; \
	    exit 1; \
	  fi; \
	  tmp=$$(mktemp 2>/dev/null) || { echo "❌ failed to create temp file"; exit 1; }; \
	  trap 'rm -f "$$tmp"' EXIT; \
	  (cd shared && bun run scripts/generate-versions.ts --out "$$tmp" >/dev/null); \
	  diff -u shared/types/versions.generated.ts "$$tmp" >/dev/null || { \
	    echo "❌ shared/types/versions.generated.ts is stale — run 'cd shared && bun run generate-versions' and commit the result"; \
	    exit 1; \
	  }
	@# Print the versions straight from versions.env so this summary stays in
	@# sync automatically as keys are added (no hardcoded list to maintain).
	@printf '✅ versions in sync (%s)\n' "$$(awk -F= '/^[A-Z][A-Z0-9_]*=/ { printf "%s%s=%s", sep, $$1, $$2; sep=", " }' versions.env)"

# Test the verify-versions guard itself by deliberately breaking each
# input it inspects and asserting the target exits non-zero.
test-verify-versions:
	@bash hack/test-verify-versions.sh
