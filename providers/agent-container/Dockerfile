# Build the provider binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH

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

# Ensure go.sum is complete for all transitive dependencies
RUN cd providers/agent-container && go mod tidy

# Build
RUN cd providers/agent-container && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o provider cmd/main.go

# Use distroless as minimal base image to package the provider binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/providers/agent-container/provider .
USER 65532:65532

ENTRYPOINT ["/provider"]

