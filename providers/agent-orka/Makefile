MODULE       := $(shell go list -f '{{.Module.Path}}' ./cmd)
GIT_SHA      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GIT_DIRTY    := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo '-dirty')
SHIM_VERSION ?= dev-$(GIT_SHA)$(GIT_DIRTY)

# Inject the reported shim version into the provider binary so it tracks the
# release tag. The Go fallback in version.go only applies when bypassing this
# Makefile (e.g. `go run`, `go test`).
LDFLAGS := -X $(MODULE).shimVersion=$(SHIM_VERSION)

GOLANGCI_LINT ?= ../../bin/golangci-lint
GOVULNCHECK ?= ../../bin/govulncheck

IMG ?= ghcr.io/ai-runway/airunway/agent-orka-provider:latest
PLATFORM ?= linux/amd64
PUSH ?= false
PUSH_ENABLED := $(filter true TRUE 1 yes YES on ON,$(PUSH))
IMAGE_OUTPUT_FLAG := $(if $(PUSH_ENABLED),--push,--load)

.PHONY: build vet test docker-build lint lint-fix lint-config golangci-lint govulncheck govulncheck-tool

vet:
	go vet ./...

test: vet
	go test ./...

build: vet
	go build -ldflags="$(LDFLAGS)" -o bin/provider ./cmd/main.go

docker-build:
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) --build-arg SHIM_VERSION=$(SHIM_VERSION) -f Dockerfile -t $(IMG) ../..

lint: golangci-lint
	"$(GOLANGCI_LINT)" run --config=../../.golangci.yml

lint-fix: golangci-lint
	"$(GOLANGCI_LINT)" run --config=../../.golangci.yml --fix

lint-config: golangci-lint
	"$(GOLANGCI_LINT)" config verify --config=../../.golangci.yml

golangci-lint:
	@$(MAKE) -C ../../controller golangci-lint

govulncheck: govulncheck-tool
	"$(GOVULNCHECK)" ./...

govulncheck-tool:
ifneq ($(origin GOVULNCHECK), file)
	@echo "Using supplied GOVULNCHECK binary: $(GOVULNCHECK)"
else
	@$(MAKE) -C ../../controller govulncheck-tool
endif
