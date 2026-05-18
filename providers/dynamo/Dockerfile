# Build the provider binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
# DYNAMO_VERSION is the upstream Dynamo runtime tag pinned in /versions.env.
# When provided (e.g. via `make docker-build`), it is injected into the provider
# binary via -ldflags so the in-image default tracks versions.env. When unset
# (hand-run `docker build`), the Go source fallback in config.go wins.
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
RUN cd providers/dynamo && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a \
      -ldflags="${DYNAMO_VERSION:+-X github.com/kaito-project/airunway/providers/dynamo.DynamoVersion=${DYNAMO_VERSION}}" \
      -o provider cmd/main.go

# Use distroless as minimal base image to package the provider binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/providers/dynamo/provider .
USER 65532:65532

ENTRYPOINT ["/provider"]
