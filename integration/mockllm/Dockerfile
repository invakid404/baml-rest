# renovate: datasource=docker
FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS builder

WORKDIR /app

# Copy dependency files first for better layer caching
COPY go.mod go.sum ./

# Copy mockllm source code to match the expected import path
COPY integration/mockllm/ ./integration/mockllm/

# Build the mock server
RUN CGO_ENABLED=0 GOOS=linux go build -tags=integration -o /mockllm ./integration/mockllm/cmd

# renovate: datasource=docker
FROM alpine:3.19@sha256:6baf43584bcb78f2e5847d1de515f23499913ac9f12bdf834811a3145eb11ca1

# Add ca-certificates for HTTPS (not strictly needed but good practice)
RUN apk --no-cache add ca-certificates

COPY --from=builder /mockllm /mockllm

ENV ADDR=:8080
EXPOSE 8080

CMD ["/mockllm"]
