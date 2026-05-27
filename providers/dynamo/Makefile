KUSTOMIZE ?= ../../controller/bin/kustomize
IMG ?= ghcr.io/kaito-project/airunway/dynamo-provider:latest
PLATFORM ?= linux/amd64
PUSH ?= false
PUSH_ENABLED := $(filter true TRUE 1 yes YES on ON,$(PUSH))
IMAGE_OUTPUT_FLAG := $(if $(PUSH_ENABLED),--push,--load)

# Upstream component versions (single source of truth at /versions.env).
include ../../versions.env
export

# Inject the build-time Dynamo runtime tag into the provider binary so it
# tracks /versions.env automatically. The Go fallback in config.go only
# applies when bypassing this Makefile (e.g. `go run`, `go test`).
LDFLAGS := -X github.com/kaito-project/airunway/providers/dynamo.DynamoVersion=$(DYNAMO_VERSION)

.PHONY: build vet test docker-build deploy generate-deploy-manifests setup-dynamo cleanup-dynamo test-e2e verify-versions

## Verify upstream versions are in sync (delegates to root Makefile).
verify-versions:
ifneq ($(VERIFIED_VERSIONS),1)
	@$(MAKE) -C ../.. verify-versions
endif

## Run go vet against code
vet:
	go vet ./...
	@echo "✅ Dynamo provider vet passed"

## Run unit tests
test: vet
	go test ./...
	@echo "✅ Dynamo provider unit tests passed"

## Build the provider binary
build: verify-versions vet
	go build -ldflags="$(LDFLAGS)" -o bin/provider ./cmd/main.go
	@echo "✅ Dynamo provider built"

## Build provider Docker image
docker-build: verify-versions
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) --build-arg DYNAMO_VERSION=$(DYNAMO_VERSION) -f Dockerfile -t $(IMG) ../..
	@echo "✅ Dynamo provider image built: $(IMG) ($(PLATFORM), $(if $(PUSH_ENABLED),pushed,loaded locally))"

## Deploy provider to the K8s cluster
deploy:
	cd config/manager && ../../$(KUSTOMIZE) edit set image IMAGE_PLACEHOLDER=$(IMG)
	$(KUSTOMIZE) build config/default | kubectl apply -f -
	@git checkout config/manager/kustomization.yaml 2>/dev/null || true
	@echo "✅ Dynamo provider deployed"

## Generate deploy manifest
generate-deploy-manifests:
	@mkdir -p deploy
	cd config/manager && ../../$(KUSTOMIZE) edit set image IMAGE_PLACEHOLDER=$(IMG)
	$(KUSTOMIZE) build config/default > deploy/dynamo.yaml
	@git checkout config/manager/kustomization.yaml 2>/dev/null || true
	@echo "✅ Generated deploy/dynamo.yaml"

## Install Dynamo platform on the cluster (run before test-e2e)
DYNAMO_NAMESPACE ?= dynamo-system
DYNAMO_CHART_URL := https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-$(DYNAMO_VERSION).tgz
DYNAMO_PRE_DEPLOY_URL := https://raw.githubusercontent.com/ai-dynamo/dynamo/v$(DYNAMO_VERSION)/deploy/pre-deployment/pre-deployment-check.sh
GAIE_MANIFEST_URL := https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/$(GAIE_VERSION)/manifests.yaml

setup-dynamo: verify-versions
	@echo "Running Dynamo pre-deployment checks..."
	@TMPSCRIPT=$$(mktemp) && \
		trap 'rm -f "$$TMPSCRIPT"' EXIT && \
		curl -fsSL "$(DYNAMO_PRE_DEPLOY_URL)" -o "$$TMPSCRIPT" && \
		chmod +x "$$TMPSCRIPT" && \
		bash "$$TMPSCRIPT"
	@echo "Pre-deployment checks passed. Installing GAIE components..."
	@kubectl apply -f $(GAIE_MANIFEST_URL)
	@echo "Installing Dynamo platform v$(DYNAMO_VERSION)..."
	@helm upgrade -i dynamo-platform "$(DYNAMO_CHART_URL)" \
			--namespace $(DYNAMO_NAMESPACE) \
			--create-namespace \
			--set "dynamo-operator.controllerManager.manager.image.tag=$(DYNAMO_VERSION)"
	@echo "✅ Dynamo platform v$(DYNAMO_VERSION) installed in namespace $(DYNAMO_NAMESPACE)"

## Uninstall Dynamo platform from the cluster
cleanup-dynamo:
	@echo "⚠️ Skipping GAIE components uninstall as they may be shared. Please uninstall them manually if needed by running:"
	@echo
	@echo "kubectl delete -f \"$(GAIE_MANIFEST_URL)\" --ignore-not-found"
	@echo
	@helm uninstall dynamo-platform --namespace $(DYNAMO_NAMESPACE) --ignore-not-found
	@echo "✅ Dynamo platform uninstalled from namespace $(DYNAMO_NAMESPACE)"

## Run e2e tests (requires DYNAMO_INSTALLED=true and a cluster with dynamo provider deployed)
test-e2e:
	DYNAMO_INSTALLED=true go test -tags=e2e -v -timeout 45m ./test/e2e/
