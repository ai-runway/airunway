IMG ?= ghcr.io/ai-runway/airunway/agent-orka-provider:latest
PLATFORM ?= linux/amd64
PUSH ?= false
PUSH_ENABLED := $(filter true TRUE 1 yes YES on ON,$(PUSH))
IMAGE_OUTPUT_FLAG := $(if $(PUSH_ENABLED),--push,--load)

.PHONY: build vet test docker-build

vet:
	go vet ./...

test: vet
	go test ./...

build: vet
	go build -o bin/provider ./cmd/main.go

docker-build:
	docker buildx build --platform $(PLATFORM) $(IMAGE_OUTPUT_FLAG) -f Dockerfile -t $(IMG) ../..

