.PHONY: install dev dev-frontend dev-backend build compile lint test clean help providers-test verify-versions test-verify-versions
.PHONY: controller-build controller-docker-build controller-install controller-deploy controller-generate generate-deploy-manifests
.PHONY: model-downloader-docker-build

# Controller image
CONTROLLER_IMG ?= ghcr.io/kaito-project/airunway/controller:latest

# Dashboard image
DASHBOARD_IMG ?= ghcr.io/kaito-project/airunway/dashboard:latest

# Model downloader image
MODEL_DOWNLOADER_IMG ?= ghcr.io/kaito-project/airunway/model-downloader:latest

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
	@echo "✅ Provider tests completed"

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

# ==================== Version Drift Guard ====================

# Verify all version references are in sync with versions.env.
# Wired as a prerequisite of every build/test target so drift is caught
# the moment it is introduced.
#
# Note: Escape dots so version literals are matched as fixed strings inside
# regexes, preventing e.g. "1.5.0" from also matching "1X5Y0".
GAIE_VERSION_RE := $(subst .,\.,$(GAIE_VERSION))
DYNAMO_VERSION_RE := $(subst .,\.,$(DYNAMO_VERSION))

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
	@# 4. generated TS must be in sync with versions.env.
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
	@echo "✅ versions in sync (GAIE_VERSION=$(GAIE_VERSION), DYNAMO_VERSION=$(DYNAMO_VERSION))"

# Test the verify-versions guard itself by deliberately breaking each
# input it inspects and asserting the target exits non-zero.
test-verify-versions:
	@bash hack/test-verify-versions.sh
