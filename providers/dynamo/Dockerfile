# Build the provider binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
# DYNAMO_VERSION is the upstream Dynamo runtime tag pinned in /versions.env.
# It is injected into the provider binary via -ldflags so the in-image default
# tracks versions.env. The build fails fast below if it is unset, so a hand-run
# `docker build` cannot silently fall back to the Go source literal in config.go
# and produce an image untracked by versions.env.
ARG DYNAMO_VERSION

WORKDIR /workspace

# Disable workspace mode; use replace directives from go.mod
ENV GOWORK=off

# Copy controller dependency (referenced by go.mod replace directive)
COPY controller/go.mod controller/go.sum controller/

# Copy provider module manifests and download dependencies
COPY providers/dynamo/go.mod providers/dynamo/go.sum providers/dynamo/
RUN cd providers/dynamo && go mod download

# Copy the controller Go source
COPY controller/ controller/

# Copy the provider Go source
COPY providers/dynamo/ providers/dynamo/

# Ensure go.sum is complete for all transitive dependencies
RUN cd providers/dynamo && go mod tidy

# Build
# Require DYNAMO_VERSION so hand-run `docker build` cannot silently drift from
# versions.env. Builds should pass it via --build-arg (the Makefile does this).
RUN test -n "${DYNAMO_VERSION}" || (echo "ERROR: DYNAMO_VERSION build arg is required (see versions.env); pass --build-arg DYNAMO_VERSION=..." >&2; exit 1)
RUN cd providers/dynamo && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a \
      -ldflags="-X github.com/kaito-project/airunway/providers/dynamo.DynamoVersion=${DYNAMO_VERSION}" \
      -o provider cmd/main.go

# Use distroless as minimal base image to package the provider binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/providers/dynamo/provider .
USER 65532:65532

ENTRYPOINT ["/provider"]
