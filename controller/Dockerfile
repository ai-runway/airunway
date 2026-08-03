# Build the manager binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
# GAIE_VERSION is the upstream Gateway API Inference Extension version pinned in
# /versions.env. It is injected into the controller binary via -ldflags so the
# in-image default tracks versions.env. The build fails fast below if it is unset,
# so a hand-run `docker build` cannot silently fall back to the Go source literal
# and produce an image untracked by versions.env.
ARG GAIE_VERSION

# AGENT_PROVIDER_VERSION is the version the in-process agent providers report
# into AgentProviderConfig.status.version. Deliberately no default, matching
# GAIE_VERSION above and the three shim Dockerfiles: defaulting to "dev" made a
# hand-run build publish a plausible-looking version under a release tag, which
# is precisely the skew the reported-version contract exists to make visible.
# `make controller-docker-build` and the release workflow both pass it.
ARG AGENT_PROVIDER_VERSION

WORKDIR /workspace

# Copy the Go Modules manifests
COPY controller/go.mod controller/go.sum controller/
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN cd controller && go mod download

# Copy the controller Go source
COPY controller/ controller/

# Build
# Require GAIE_VERSION so hand-run `docker build` cannot silently drift from
# versions.env. Builds should pass it via --build-arg (the Makefile does this).
RUN test -n "${GAIE_VERSION}" || (echo "ERROR: GAIE_VERSION build arg is required (see versions.env); pass --build-arg GAIE_VERSION=..." >&2; exit 1)
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN cd controller && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a \
      -ldflags="-X github.com/ai-runway/airunway/controller/internal/gateway.DefaultGAIEVersion=${GAIE_VERSION} -X main.agentProviderVersion=${AGENT_PROVIDER_VERSION}" \
      -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/controller/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
