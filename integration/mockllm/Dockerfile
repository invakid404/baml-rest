# renovate: datasource=docker
FROM golang:1.26.3-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d AS builder

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
