FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy mockllm source code to match the expected import path
COPY integration/mockllm/ ./integration/mockllm/

# Create a minimal go.mod with the correct module path
RUN echo 'module github.com/invakid404/baml-rest' > go.mod && echo 'go 1.25' >> go.mod

# Build the mock server (standard library only, no external deps)
RUN CGO_ENABLED=0 GOOS=linux go build -tags=integration -o /mockllm ./integration/mockllm/cmd

FROM alpine:3.19

# Add ca-certificates for HTTPS (not strictly needed but good practice)
RUN apk --no-cache add ca-certificates

COPY --from=builder /mockllm /mockllm

ENV ADDR=:8080
EXPOSE 8080

CMD ["/mockllm"]
