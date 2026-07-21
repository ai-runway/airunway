KUSTOMIZE ?= ../../controller/bin/kustomize
IMG ?= ghcr.io/kaito-project/airunway/llmd-provider:latest
PLATFORM ?= linux/amd64
PUSH ?= false
PUSH_ENABLED := $(filter true TRUE 1 yes YES on ON,$(PUSH))
IMAGE_OUTPUT_FLAG := $(if $(PUSH_ENABLED),--push,--load)

# Upstream component versions (single source of truth at /versions.env).
include ../../versions.env
export

# Inject the build-time tag into the provider binary so it
# tracks /versions.env automatically. The Go fallback in config.go only
# applies when bypassing this Makefile (e.g. `go run`, `go test`).
LDFLAGS := -X github.com/kaito-project/airunway/providers/llmd.LLMDSchedulerImage=ghcr.io/llm-d/llm-d-inference-scheduler:v$(LLMD_VERSION)

.PHONY: build vet test docker-build deploy generate-deploy-manifests verify-versions

## Verify upstream versions are in sync (delegates to root Makefile).
verify-versions:
ifneq ($(VERIFIED_VERSIONS),1)
	@$(MAKE) -C ../.. verify-versions
endif

## Run go vet against code
vet:
	go vet ./...
	@echo "✅ llm-d provider vet passed"

## Run unit tests
test: vet
	go test ./...
	@echo "✅ llm-d provider unit tests passed"

## Build the provider binary
build: verify-versions vet
	go build -ldflags="$(LDFLAGS)" -o bin/provider ./cmd/main.go
	@echo "✅ llm-d provider built"

## Build provider Docker image
docker-build: verify-versions
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) --build-arg LLMD_VERSION=$(LLMD_VERSION) -f Dockerfile -t $(IMG) ../..
	@echo "✅ llm-d provider image built: $(IMG) ($(PLATFORM), $(if $(PUSH_ENABLED),pushed,loaded locally))"

## Deploy provider to the K8s cluster
deploy:
	cd config/manager && ../../$(KUSTOMIZE) edit set image IMAGE_PLACEHOLDER=$(IMG)
	$(KUSTOMIZE) build config/default | kubectl apply -f -
	@git checkout config/manager/kustomization.yaml 2>/dev/null || true
	@echo "✅ llm-d provider deployed"

## Generate deploy manifest
generate-deploy-manifests:
	@mkdir -p deploy
	cd config/manager && ../../$(KUSTOMIZE) edit set image IMAGE_PLACEHOLDER=$(IMG)
	$(KUSTOMIZE) build config/default > deploy/llmd.yaml
	@git checkout config/manager/kustomization.yaml 2>/dev/null || true
	@echo "✅ Generated deploy/llmd.yaml"
