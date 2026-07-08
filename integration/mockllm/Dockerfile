# renovate: datasource=docker
FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

WORKDIR /app

# Copy dependency files first for better layer caching
COPY go.mod go.sum ./

# Copy mockllm source code to match the expected import path
COPY integration/mockllm/ ./integration/mockllm/

# Build the mock server
RUN CGO_ENABLED=0 GOOS=linux go build -tags=integration -o /mockllm ./integration/mockllm/cmd

# renovate: datasource=docker
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# Add ca-certificates for HTTPS (not strictly needed but good practice)
RUN apk --no-cache add ca-certificates

COPY --from=builder /mockllm /mockllm

ENV ADDR=:8080
# 8080 = plaintext HTTP; 8443 = optional HTTPS/h2 lane (enabled when the
# integration harness supplies MOCK_LLM_TLS_CERT_PEM/KEY_PEM).
EXPOSE 8080 8443

CMD ["/mockllm"]
