KUSTOMIZE ?= ../../controller/bin/kustomize
IMG ?= ghcr.io/kaito-project/airunway/kaito-provider:latest
PLATFORM ?= linux/amd64
PUSH ?= false
PUSH_ENABLED := $(filter true TRUE 1 yes YES on ON,$(PUSH))
IMAGE_OUTPUT_FLAG := $(if $(PUSH_ENABLED),--push,--load)

# Upstream component versions (single source of truth at /versions.env).
include ../../versions.env
export

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
	go build -o bin/provider ./cmd/main.go
	@echo "✅ KAITO provider built"

## Build provider Docker image
docker-build:
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) -f Dockerfile -t $(IMG) ../..
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
