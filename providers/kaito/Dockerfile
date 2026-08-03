# Build the provider binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
# SHIM_VERSION is the reported shim version tag. Required so a hand-run
# `docker build` cannot silently ship status.version=kaito-provider:dev under
# a real release tag. The Makefile and release workflow always pass it via
# --build-arg.
ARG SHIM_VERSION

WORKDIR /workspace

# Disable workspace mode; use replace directives from go.mod
ENV GOWORK=off

# Copy controller dependency (referenced by go.mod replace directive)
COPY controller/go.mod controller/go.sum controller/

# Copy provider module manifests and download dependencies
COPY providers/kaito/go.mod providers/kaito/go.sum providers/kaito/
RUN cd providers/kaito && go mod download

# Copy the controller Go source
COPY controller/ controller/

# Copy the provider Go source
COPY providers/kaito/ providers/kaito/

# Ensure go.sum is complete for all transitive dependencies
RUN cd providers/kaito && go mod tidy

# Build
# Require SHIM_VERSION so a hand-run `docker build` cannot silently ship
# status.version=kaito-provider:dev under a real release tag. Builds should pass
# it via --build-arg (the Makefile and release workflow do this).
RUN test -n "${SHIM_VERSION}" || (echo "ERROR: SHIM_VERSION build arg is required; pass --build-arg SHIM_VERSION=..." >&2; exit 1)
# MODULE is resolved from go.mod (not hand-typed) so the -X target can never
# drift from the package path.
RUN cd providers/kaito && MODULE=$(go list -m) && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a \
      -ldflags="-X ${MODULE}.shimVersion=${SHIM_VERSION}" \
      -o provider cmd/main.go

# Use distroless as minimal base image to package the provider binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/providers/kaito/provider .
USER 65532:65532

ENTRYPOINT ["/provider"]
