FROM golang:1.26-alpine AS builder

WORKDIR /app

# Copy dependency files first for better layer caching
COPY go.mod go.sum ./

# Copy mockllm source code to match the expected import path
COPY integration/mockllm/ ./integration/mockllm/

# Build the mock server
RUN CGO_ENABLED=0 GOOS=linux go build -tags=integration -o /mockllm ./integration/mockllm/cmd

FROM alpine:3.19

# Add ca-certificates for HTTPS (not strictly needed but good practice)
RUN apk --no-cache add ca-certificates

COPY --from=builder /mockllm /mockllm

ENV ADDR=:8080
EXPOSE 8080

CMD ["/mockllm"]
