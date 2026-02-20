# Base Builder Image for BAML REST API
# This image includes Node.js, Go, and all required build tools
# Can be used as a base image for faster builds

# Go version (defined once, re-declared in each stage that needs it)
ARG GO_VERSION=1.26.0

# ============================================================================
# Stage 1: Builder (native platform for fast builds with cross-compilation)
# ============================================================================
FROM --platform=$BUILDPLATFORM node:22-bookworm AS builder

# Build arguments for cross-compilation
ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG GO_VERSION

# Extract build platform architecture (always amd64 on GitHub Actions)
RUN case "${BUILDPLATFORM}" in \
        "linux/amd64") BUILDARCH="amd64" ;; \
        "linux/arm64") BUILDARCH="arm64" ;; \
        *) BUILDARCH="amd64" ;; \
    esac && \
    apt-get update && \
    apt-get install -y wget git ca-certificates && \
    wget -q https://go.dev/dl/go${GO_VERSION}.linux-${BUILDARCH}.tar.gz && \
    tar -C /usr/local -xzf go${GO_VERSION}.linux-${BUILDARCH}.tar.gz && \
    rm go${GO_VERSION}.linux-${BUILDARCH}.tar.gz && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Set up Go environment
ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOPATH="/go"
ENV PATH="${GOPATH}/bin:${PATH}"

# Cross-compile goimports for target platform
RUN git clone --depth 1 https://go.googlesource.com/tools /tmp/go-tools && \
    cd /tmp/go-tools/cmd/goimports && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=0 go build -o /tmp/goimports . && \
    rm -rf /tmp/go-tools

# Cross-compile baml-rest binary for target platform
COPY . /tmp/baml-rest-src
WORKDIR /tmp/baml-rest-src
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=0 go build -o /tmp/baml-rest ./cmd/build/main.go

# ============================================================================
# Stage 2: Runtime (target platform with correct architecture binaries)
# ============================================================================
FROM node:22-bookworm

# Build arguments for target platform
ARG TARGETARCH
ARG GO_VERSION

# Install Go for the target platform
RUN apt-get update && \
    apt-get install -y wget git ca-certificates && \
    wget -q https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz && \
    tar -C /usr/local -xzf go${GO_VERSION}.linux-${TARGETARCH}.tar.gz && \
    rm go${GO_VERSION}.linux-${TARGETARCH}.tar.gz && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Set up Go environment
ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOPATH="/go"
ENV PATH="${GOPATH}/bin:${PATH}"

# Install additional runtime dependencies
RUN apt-get update && \
    apt-get install -y gettext bash gawk && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Copy cross-compiled binaries from builder stage
COPY --from=builder /tmp/goimports /usr/local/bin/goimports
COPY --from=builder /tmp/baml-rest /usr/local/bin/baml-rest

# Set up unified cache environment variables
ENV NPM_CONFIG_CACHE="/cache/npm"
ENV GOMODCACHE="/cache/go/mod"
ENV GOCACHE="/cache/go/build"
ENV BAML_CACHE_DIR="/cache/baml"

# Create cache directory structure
RUN mkdir -p /cache/npm /cache/go/mod /cache/go/build /cache/baml

# Set working directory
WORKDIR /workspace

# Metadata
LABEL maintainer="BAML REST"
LABEL description="Base builder image with Node.js, Go, and BAML REST build tools"
LABEL version="1.0"
