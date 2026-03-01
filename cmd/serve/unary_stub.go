//go:build !unaryserver

package main

import (
	"net/http"

	"github.com/invakid404/baml-rest/pool"
	"github.com/rs/zerolog"
)

// newUnaryServer is a no-op when the unaryserver build tag is not set.
func newUnaryServer(_ zerolog.Logger, _ *pool.Pool, _ []string, _ bool) *http.Server {
	return nil
}
