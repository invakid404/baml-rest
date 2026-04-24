//go:build debug

// Package buildrequest — call_support_debug.go provides a debug-build-only
// test hook that lets integration tests force specific providers out of the
// call-supported set without waiting for callSupportedProviders and
// supportedProviders to diverge organically. Compiled only when built with
// `-tags debug` (see cmd/build/build.sh); release builds use the no-op stub
// in call_support_stub.go.
package buildrequest

import (
	"os"
	"strings"
	"sync"
)

// callUnsupportedProvidersOnce caches the parsed set so
// debugFilterCallSupported stays hot on the request path. Same rationale
// as useBuildRequestOnce — os.Getenv takes a lock each call.
var callUnsupportedProvidersOnce sync.Once
var callUnsupportedProvidersCached map[string]bool

// parseCallUnsupportedProvidersEnv reads BAML_REST_CALL_UNSUPPORTED_PROVIDERS
// as a comma-separated list and returns the providers marked as
// call-unsupported. Whitespace around each entry is trimmed; empty entries
// are ignored. Extracted so the test can verify parsing without the cache.
func parseCallUnsupportedProvidersEnv() map[string]bool {
	raw := os.Getenv("BAML_REST_CALL_UNSUPPORTED_PROVIDERS")
	if raw == "" {
		return nil
	}
	set := make(map[string]bool)
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			set[p] = true
		}
	}
	return set
}

func callUnsupportedProviders() map[string]bool {
	callUnsupportedProvidersOnce.Do(func() {
		callUnsupportedProvidersCached = parseCallUnsupportedProvidersEnv()
	})
	return callUnsupportedProvidersCached
}

// debugFilterCallSupported refines the static call-supported answer with the
// debug override. When a provider is listed in
// BAML_REST_CALL_UNSUPPORTED_PROVIDERS, it is reported as call-unsupported
// regardless of the static map — letting tests put that provider into a
// fallback chain next to a truly call-supported one to exercise the mixed-
// chain fall-through gate.
func debugFilterCallSupported(provider string, staticSupported bool) bool {
	if callUnsupportedProviders()[provider] {
		return false
	}
	return staticSupported
}
