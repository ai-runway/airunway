# syntax=docker/dockerfile:1

# Stage 1: Build frontend and compile binary
FROM --platform=$BUILDPLATFORM oven/bun:1 AS builder

# Build arguments
ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG VERSION=dev
ARG GIT_COMMIT=unknown

WORKDIR /app

# Copy package files for dependency installation
COPY package.json bun.lock* ./
COPY backend/package.json backend/
COPY frontend/package.json frontend/
COPY shared/package.json shared/
COPY plugins/headlamp/package.json plugins/headlamp/

# Install dependencies
RUN bun install --frozen-lockfile

# Copy source code
COPY . .

# Build frontend
RUN bun run build:frontend

# Embed frontend assets into backend
RUN cd backend && bun run embed

# Compile static binary for target platform
RUN cd backend && \
    if [ "$TARGETPLATFORM" = "linux/arm64" ]; then \
      TARGET="bun-linux-arm64"; \
    else \
      TARGET="bun-linux-x64"; \
    fi && \
    VERSION=${VERSION} GIT_COMMIT=${GIT_COMMIT} bun run scripts/compile.ts \
      --target=$TARGET \
      --outfile=airunway

# Stage 2: Download CLI tools used by backend installation routes
FROM --platform=$BUILDPLATFORM alpine:3.22 AS cli-tools

ARG TARGETARCH
ARG HELM_VERSION=v4.1.4
ARG KUBECTL_VERSION=v1.34.1

RUN apk add --no-cache ca-certificates curl gzip tar && \
    HELM_PACKAGE="helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz" && \
    curl -fsSL "https://get.helm.sh/${HELM_PACKAGE}" -o "/tmp/${HELM_PACKAGE}" && \
    curl -fsSL "https://get.helm.sh/${HELM_PACKAGE}.sha256sum" -o "/tmp/${HELM_PACKAGE}.sha256sum" && \
    (cd /tmp && sha256sum -c "${HELM_PACKAGE}.sha256sum") && \
    tar -xzf "/tmp/${HELM_PACKAGE}" -C /tmp && \
    mv "/tmp/linux-${TARGETARCH}/helm" /usr/local/bin/helm && \
    chmod 0755 /usr/local/bin/helm && \
    curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" -o /usr/local/bin/kubectl && \
    curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256" -o /tmp/kubectl.sha256 && \
    echo "$(cat /tmp/kubectl.sha256)  /usr/local/bin/kubectl" | sha256sum -c - && \
    chmod 0755 /usr/local/bin/kubectl && \
    helm version --short && \
    kubectl version --client=true

# Stage 3: Runtime with distroless
# Using cc-debian12 which includes glibc (required by Bun-compiled binaries)
FROM gcr.io/distroless/cc-debian12:nonroot

# Labels for container registry
LABEL org.opencontainers.image.title="AIRunway"
LABEL org.opencontainers.image.description="Web-based platform for deploying and managing LLM frameworks on Kubernetes"
LABEL org.opencontainers.image.source="https://github.com/kaito-project/airunway"
LABEL org.opencontainers.image.licenses="MIT"

# Copy the compiled binary and CLI tools used by installation routes
COPY --from=builder /app/dist/airunway /airunway
COPY --from=cli-tools /usr/local/bin/helm /usr/local/bin/helm
COPY --from=cli-tools /usr/local/bin/kubectl /usr/local/bin/kubectl

ENV PATH="/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# Expose the default port
EXPOSE 3001

# Run as non-root user (provided by distroless:nonroot)
USER nonroot:nonroot

# Start the application
ENTRYPOINT ["/airunway"]
