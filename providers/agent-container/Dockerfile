# Build the provider binary
FROM --platform=$BUILDPLATFORM golang:1.25@sha256:fc32980f4e03a79db932b28485cf190a28067be1f2431844405451105fd0525f AS builder
ARG TARGETOS
ARG TARGETARCH
# SHIM_VERSION is the reported shim version tag. Required so a hand-run
# `docker build` cannot silently ship status.version=agent-container-provider:dev
# under a real release tag. The Makefile always passes it via --build-arg.
ARG SHIM_VERSION

WORKDIR /workspace

# Disable workspace mode; use replace directives from go.mod
ENV GOWORK=off

# Copy controller dependency (referenced by go.mod replace directive)
COPY controller/go.mod controller/go.sum controller/

# Copy provider module manifests and download dependencies
COPY providers/agent-container/go.mod providers/agent-container/go.sum providers/agent-container/
RUN cd providers/agent-container && go mod download

# Copy the controller Go source
COPY controller/ controller/

# Copy the provider Go source
COPY providers/agent-container/ providers/agent-container/

# Build
RUN test -n "${SHIM_VERSION}" || (echo "ERROR: SHIM_VERSION build arg is required; pass --build-arg SHIM_VERSION=..." >&2; exit 1)
# MODULE is resolved from go.mod (not hand-typed) so the -X target can never
# drift from the package path.
RUN cd providers/agent-container && MODULE=$(go list -m) && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a \
      -ldflags="-X ${MODULE}.shimVersion=${SHIM_VERSION}" \
      -o provider cmd/main.go

# Use distroless as minimal base image to package the provider binary
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
WORKDIR /
COPY --from=builder /workspace/providers/agent-container/provider .
USER 65532:65532

ENTRYPOINT ["/provider"]
