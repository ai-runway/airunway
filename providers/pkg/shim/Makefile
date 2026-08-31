GOLANGCI_LINT ?= ../../../bin/golangci-lint

.PHONY: test cover lint lint-fix lint-config golangci-lint

## Run unit tests
test:
	go test ./...

## Run unit tests with coverage profile output
cover:
	go test ./... -coverprofile cover.out
	@echo "✅ provider shim coverage profile generated: cover.out"

## Run golangci-lint against code
lint: golangci-lint
	"$(GOLANGCI_LINT)" run --config=../../../.golangci.yml

## Run golangci-lint and apply auto-fixes
lint-fix: golangci-lint
	"$(GOLANGCI_LINT)" run --config=../../../.golangci.yml --fix

## Verify golangci-lint configuration
lint-config: golangci-lint
	"$(GOLANGCI_LINT)" config verify --config=../../../.golangci.yml

## Ensure golangci-lint is installed via the controller toolchain pin
golangci-lint:
	@$(MAKE) -C ../../../controller golangci-lint
