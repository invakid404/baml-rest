//go:build !debug

package main

import (
	"github.com/gofiber/fiber/v3"
	"github.com/invakid404/baml-rest/pool"
	"github.com/rs/zerolog"
)

func registerDebugEndpoints(_ fiber.Router, _ zerolog.Logger, _ *pool.Pool) {
	// No-op: debug endpoints disabled in release builds
}
