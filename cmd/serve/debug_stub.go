//go:build !debug

package main

import (
	"github.com/go-chi/chi/v5"
	"github.com/invakid404/baml-rest/pool"
	"github.com/rs/zerolog"
)

func registerDebugEndpoints(_ chi.Router, _ zerolog.Logger, _ *pool.Pool) {
	// No-op: debug endpoints disabled in release builds
}
