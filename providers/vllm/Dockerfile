# Build the provider binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
# VLLM_VERSION is the default vLLM image tag pinned in /versions.env. It is
# injected into the provider binary via -ldflags so the in-image default tracks
# versions.env and cannot silently drift from it.
ARG VLLM_VERSION
# SHIM_VERSION is the reported shim version tag. Required so a hand-run
# `docker build` cannot silently ship status.version=vllm-provider:dev under
# a real release tag. The Makefile and release workflow always pass it via
# --build-arg.
ARG SHIM_VERSION

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

# Require VLLM_VERSION and SHIM_VERSION so a hand-run `docker build` cannot
# silently drift from versions.env or ship status.version=vllm-provider:dev
# (the Makefile and release workflow always pass both via --build-arg).
RUN test -n "${VLLM_VERSION}" || (echo "ERROR: VLLM_VERSION build arg is required (see versions.env); pass --build-arg VLLM_VERSION=..." >&2; exit 1)
RUN test -n "${SHIM_VERSION}" || (echo "ERROR: SHIM_VERSION build arg is required; pass --build-arg SHIM_VERSION=..." >&2; exit 1)

# Build with the committed module graph. -mod=readonly fails if go.mod/go.sum are incomplete.
# MODULE is resolved from go.mod (not hand-typed) so every -X target can never
# drift from the package path.
RUN cd providers/vllm && MODULE=$(go list -m) && \
      CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
      go build -mod=readonly -a \
      -ldflags="-X ${MODULE}.VLLMVersion=${VLLM_VERSION} -X ${MODULE}.shimVersion=${SHIM_VERSION}" \
      -o provider cmd/main.go

# Use distroless as minimal base image to package the provider binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/providers/vllm/provider .
USER 65532:65532

ENTRYPOINT ["/provider"]
