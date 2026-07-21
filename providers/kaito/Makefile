KUSTOMIZE ?= ../../controller/bin/kustomize
IMG ?= ghcr.io/ai-runway/airunway/kaito-provider:latest
PLATFORM ?= linux/amd64
PUSH ?= false
PUSH_ENABLED := $(filter true TRUE 1 yes YES on ON,$(PUSH))
IMAGE_OUTPUT_FLAG := $(if $(PUSH_ENABLED),--push,--load)

# Upstream component versions (single source of truth at /versions.env).
include ../../versions.env
export

# Module path, resolved from go.mod so the -X target can never drift from a
# hand-typed string. Drives the -X below.
MODULE := $(shell go list -m)

# Reported shim version: a git stamp for local/CI builds, overridden by the
# release workflow with the tag (SHIM_VERSION=v0.3.0). bin/provider is
# gitignored, so building never dirties the tree. The dirty check uses a
# repo-wide `git status --porcelain` (no pathspec) on purpose: it is the
# conservative superset — it never reports a clean sha when the tree has *any*
# uncommitted divergence. That includes the controller/ dependency this binary
# compiles in via the go.mod replace directive, not just this package dir, so
# the stamp cannot claim "clean" while the binary actually differs. The
# tradeoff is that unrelated edits (e.g. docs/) also flip it to -dirty.
GIT_SHA      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GIT_DIRTY    := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo '-dirty')
SHIM_VERSION ?= dev-$(GIT_SHA)$(GIT_DIRTY)

# Inject the reported shim version into the provider binary so it tracks the
# release tag. The Go fallback in config.go only applies when bypassing this
# Makefile (e.g. `go run`, `go test`).
LDFLAGS := -X $(MODULE).shimVersion=$(SHIM_VERSION)

.PHONY: build vet test docker-build deploy generate-deploy-manifests setup-kaito cleanup-kaito verify-versions

## Verify upstream versions are in sync (delegates to root Makefile).
verify-versions:
ifneq ($(VERIFIED_VERSIONS),1)
	@$(MAKE) -C ../.. verify-versions
endif

## Run go vet against code
vet:
	go vet ./...
	@echo "✅ KAITO provider vet passed"

## Run unit tests
test: vet
	go test ./...
	@echo "✅ KAITO provider unit tests passed"

## Build the provider binary
build: vet
	go build -ldflags="$(LDFLAGS)" -o bin/provider ./cmd/main.go
	@echo "✅ KAITO provider built"

## Build provider Docker image
docker-build:
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) --build-arg SHIM_VERSION=$(SHIM_VERSION) -f Dockerfile -t $(IMG) ../..
	@echo "✅ KAITO provider image built: $(IMG) ($(PLATFORM), $(if $(PUSH_ENABLED),pushed,loaded locally))"

## Deploy provider to the K8s cluster
deploy:
	cd config/manager && ../../$(KUSTOMIZE) edit set image IMAGE_PLACEHOLDER=$(IMG)
	$(KUSTOMIZE) build config/default | kubectl apply -f -
	@git checkout config/manager/kustomization.yaml 2>/dev/null || true
	@echo "✅ KAITO provider deployed"

## Generate deploy manifest
generate-deploy-manifests:
	@mkdir -p deploy
	cd config/manager && ../../$(KUSTOMIZE) edit set image IMAGE_PLACEHOLDER=$(IMG)
	$(KUSTOMIZE) build config/default > deploy/kaito.yaml
	@git checkout config/manager/kustomization.yaml 2>/dev/null || true
	@echo "✅ Generated deploy/kaito.yaml"

## Install KAITO workspace operator on the cluster (run before deploying the provider)
KAITO_NAMESPACE ?= kaito-workspace
KAITO_RELEASE   ?= kaito-workspace
KAITO_REPO_URL  ?= https://kaito-project.github.io/kaito/charts/kaito

setup-kaito: verify-versions
	@echo "Installing KAITO workspace operator v$(KAITO_VERSION)..."
	@helm repo add kaito $(KAITO_REPO_URL) --force-update
	@helm repo update kaito
	@helm upgrade --install $(KAITO_RELEASE) kaito/workspace \
		--version $(KAITO_VERSION) \
		--namespace $(KAITO_NAMESPACE) \
		--create-namespace \
		--set featureGates.disableNodeAutoProvisioning=true \
		--set nvidiaDevicePlugin.enabled=false \
		--set gpu-feature-discovery.nfd.enabled=false \
		--set gpu-feature-discovery.gfd.enabled=false \
		--wait --timeout=5m
	@echo "✅ KAITO workspace operator v$(KAITO_VERSION) installed in namespace $(KAITO_NAMESPACE)"

## Uninstall KAITO workspace operator from the cluster
cleanup-kaito:
	@helm uninstall $(KAITO_RELEASE) --namespace $(KAITO_NAMESPACE) --ignore-not-found
	@kubectl delete namespace $(KAITO_NAMESPACE) --ignore-not-found
	@echo "✅ KAITO workspace operator uninstalled from namespace $(KAITO_NAMESPACE)"
