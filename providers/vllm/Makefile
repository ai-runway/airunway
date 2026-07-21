KUSTOMIZE ?= ../../controller/bin/kustomize
IMG ?= ghcr.io/ai-runway/airunway/vllm-provider:latest
PLATFORM ?= linux/amd64
PUSH ?= false
PUSH_ENABLED := $(filter true TRUE 1 yes YES on ON,$(PUSH))
IMAGE_OUTPUT_FLAG := $(if $(PUSH_ENABLED),--push,--load)

# Upstream component versions (single source of truth at /versions.env).
include ../../versions.env
export

# Module path, resolved from go.mod so the -X target can never drift from a
# hand-typed string. Drives every -X below.
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

# Inject the build-time vLLM image tag and the reported shim version into the
# provider binary so the default launch image tracks /versions.env and the
# reported version tracks the release tag. The Go fallbacks in transformer.go
# and config.go only apply when bypassing this Makefile (e.g. `go run`,
# `go test`).
LDFLAGS := -X $(MODULE).VLLMVersion=$(VLLM_VERSION)
LDFLAGS += -X $(MODULE).shimVersion=$(SHIM_VERSION)

.PHONY: build vet test docker-build deploy generate-deploy-manifests verify-versions

## Verify upstream versions are in sync (delegates to root Makefile).
verify-versions:
ifneq ($(VERIFIED_VERSIONS),1)
	@$(MAKE) -C ../.. verify-versions
endif

## Run go vet against code
vet:
	go vet ./...
	@echo "✅ vLLM provider vet passed"

## Run unit tests
test: vet
	go test ./...
	@echo "✅ vLLM provider unit tests passed"

## Build the provider binary
build: verify-versions vet
	go build -ldflags="$(LDFLAGS)" -o bin/provider ./cmd/main.go
	@echo "✅ vLLM provider built"

## Build provider Docker image
docker-build: verify-versions
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) --build-arg VLLM_VERSION=$(VLLM_VERSION) --build-arg SHIM_VERSION=$(SHIM_VERSION) -f Dockerfile -t $(IMG) ../..
	@echo "✅ vLLM provider image built: $(IMG) ($(PLATFORM), $(if $(PUSH_ENABLED),pushed,loaded locally))"

## Deploy provider to the K8s cluster
deploy:
	cd config/manager && ../../$(KUSTOMIZE) edit set image IMAGE_PLACEHOLDER=$(IMG)
	$(KUSTOMIZE) build config/default | kubectl apply -f -
	@git checkout config/manager/kustomization.yaml 2>/dev/null || true
	@echo "✅ vLLM provider deployed"

## Generate deploy manifest
generate-deploy-manifests:
	@mkdir -p deploy
	cd config/manager && ../../$(KUSTOMIZE) edit set image IMAGE_PLACEHOLDER=$(IMG)
	$(KUSTOMIZE) build config/default > deploy/vllm.yaml
	@git checkout config/manager/kustomization.yaml 2>/dev/null || true
	@echo "✅ Generated deploy/vllm.yaml"
