KUSTOMIZE ?= ../../controller/bin/kustomize
IMG ?= ghcr.io/kaito-project/airunway/dynamo-provider:latest
PLATFORM ?= linux/amd64
PUSH ?= false
PUSH_ENABLED := $(filter true TRUE 1 yes YES on ON,$(PUSH))
IMAGE_OUTPUT_FLAG := $(if $(PUSH_ENABLED),--push,--load)

.PHONY: build docker-build deploy generate-deploy-manifests setup-dynamo cleanup-dynamo test-e2e

## Build the provider binary
build:
	go build -o bin/provider ./cmd/main.go
	@echo "✅ Dynamo provider built"

## Build provider Docker image
docker-build:
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) -f Dockerfile -t $(IMG) ../..
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
DYNAMO_VERSION ?= 1.1.1
DYNAMO_CHART_URL := https://helm.ngc.nvidia.com/nvidia/ai-dynamo/charts/dynamo-platform-$(DYNAMO_VERSION).tgz
DYNAMO_PRE_DEPLOY_URL := https://raw.githubusercontent.com/ai-dynamo/dynamo/v$(DYNAMO_VERSION)/deploy/pre-deployment/pre-deployment-check.sh
GAIE_VERSION := v1.5.0
GAIE_MANIFEST_URL := https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/$(GAIE_VERSION)/manifests.yaml

setup-dynamo:
	@echo "Running Dynamo pre-deployment checks..."
	@TMPSCRIPT=$$(mktemp) && \
		trap 'rm -f "$$TMPSCRIPT"' EXIT && \
		curl -fsSL "$(DYNAMO_PRE_DEPLOY_URL)" -o "$$TMPSCRIPT" && \
		chmod +x "$$TMPSCRIPT" && \
		bash "$$TMPSCRIPT"
	@echo "Installing Dynamo platform v$(DYNAMO_VERSION)..."
	@kubectl apply -f $(GAIE_MANIFEST_URL)
	@helm upgrade -i dynamo-platform "$(DYNAMO_CHART_URL)" \
			--namespace $(DYNAMO_NAMESPACE) \
			--create-namespace \
			--set "dynamo-operator.controllerManager.manager.image.tag=$(DYNAMO_VERSION)"
	@echo "✅ Dynamo platform v$(DYNAMO_VERSION) installed in namespace $(DYNAMO_NAMESPACE)"

## Uninstall Dynamo platform from the cluster
cleanup-dynamo:
	helm uninstall dynamo-platform --namespace $(DYNAMO_NAMESPACE) --ignore-not-found
	@kubectl delete -f $(GAIE_MANIFEST_URL) --ignore-not-found
	@echo "✅ Dynamo platform uninstalled from namespace $(DYNAMO_NAMESPACE)"

## Run e2e tests (requires DYNAMO_INSTALLED=true and a cluster with dynamo provider deployed)
test-e2e:
	DYNAMO_INSTALLED=true go test -tags=e2e -v -timeout 45m ./test/e2e/
