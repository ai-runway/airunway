# Build the provider binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
# VLLM_VERSION is the default vLLM image tag pinned in /versions.env. It is
# injected into the provider binary via -ldflags so the in-image default tracks
# versions.env and cannot silently drift from it.
ARG VLLM_VERSION

WORKDIR /workspace

# Disable workspace mode; use replace directives from go.mod
ENV GOWORK=off

# Copy controller dependency (referenced by go.mod replace directive)
COPY controller/go.mod controller/go.sum controller/

# Copy provider module manifests and download dependencies
COPY providers/vllm/go.mod providers/vllm/go.sum providers/vllm/
RUN cd providers/vllm && go mod download

# Copy the controller Go source
COPY controller/ controller/

# Copy the provider Go source
COPY providers/vllm/ providers/vllm/

# Require VLLM_VERSION so a hand-run `docker build` cannot silently drift from
# versions.env (the Makefile always passes it via --build-arg).
RUN test -n "${VLLM_VERSION}" || (echo "ERROR: VLLM_VERSION build arg is required (see versions.env); pass --build-arg VLLM_VERSION=..." >&2; exit 1)

# Build with the committed module graph. -mod=readonly fails if go.mod/go.sum are incomplete.
RUN cd providers/vllm && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
      go build -mod=readonly -a \
      -ldflags="-X github.com/kaito-project/airunway/providers/vllm.VLLMVersion=${VLLM_VERSION}" \
      -o provider cmd/main.go

# Use distroless as minimal base image to package the provider binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/providers/vllm/provider .
USER 65532:65532

ENTRYPOINT ["/provider"]
