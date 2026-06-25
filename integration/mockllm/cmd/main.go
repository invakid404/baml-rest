//go:build integration

package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/invakid404/baml-rest/integration/mockllm"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	server := mockllm.NewServer(addr)

	// Start server
	log.Printf("Starting mock LLM server on %s", addr)
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

	// Optional HTTPS/h2 listener. When the integration harness supplies a
	// PEM cert + key, also serve the app over TLS with ALPN advertising h2,
	// so the llmhttp `auto` selector can negotiate h2 and pick net/http.
	if certPEM, keyPEM := os.Getenv("MOCK_LLM_TLS_CERT_PEM"), os.Getenv("MOCK_LLM_TLS_KEY_PEM"); certPEM != "" && keyPEM != "" {
		tlsAddr := os.Getenv("MOCK_LLM_TLS_ADDR")
		if tlsAddr == "" {
			tlsAddr = ":8443"
		}
		log.Printf("Starting mock LLM TLS (h2) listener on %s", tlsAddr)
		if err := server.StartTLS(tlsAddr, certPEM, keyPEM); err != nil {
			log.Fatalf("Failed to start TLS listener: %v", err)
		}
	}

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}
}
