# Base Builder Image for BAML REST API
# This image includes Node.js, Go, and all required build tools
# Can be used as a base image for faster builds

FROM node:22-bookworm

# Install Go (multi-architecture support)
ARG TARGETARCH
RUN apt-get update && \
    apt-get install -y wget git ca-certificates && \
    wget -q https://go.dev/dl/go1.23.4.linux-${TARGETARCH}.tar.gz && \
    tar -C /usr/local -xzf go1.23.4.linux-${TARGETARCH}.tar.gz && \
    rm go1.23.4.linux-${TARGETARCH}.tar.gz && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Set up Go environment
ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOPATH="/go"
ENV PATH="${GOPATH}/bin:${PATH}"

# Install required tools
RUN apt-get update && \
    apt-get install -y gettext bash gawk && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Install goimports
RUN go install golang.org/x/tools/cmd/goimports@latest

# Set up unified cache environment variables
ENV NPM_CONFIG_CACHE="/cache/npm"
ENV GOMODCACHE="/cache/go/mod"
ENV GOCACHE="/cache/go/build"
ENV BAML_CACHE_DIR="/cache/baml"

# Create cache directory structure
RUN mkdir -p /cache/npm /cache/go/mod /cache/go/build /cache/baml

# Build and install baml-rest build command
COPY . /tmp/baml-rest-src
WORKDIR /tmp/baml-rest-src
RUN go build -o /usr/local/bin/baml-rest ./cmd/build/main.go && \
    rm -rf /tmp/baml-rest-src

# Set working directory
WORKDIR /workspace

# Metadata
LABEL maintainer="BAML REST"
LABEL description="Base builder image with Node.js, Go, and BAML REST build tools"
LABEL version="1.0"
