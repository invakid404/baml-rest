//go:build debug

// Package buildrequest — call_support_debug.go provides a debug-build-only
// test hook that lets integration tests force specific providers out of the
// call-supported set without waiting for callSupportedProviders and
// supportedProviders to diverge organically. Compiled only when built with
// `-tags debug` (see cmd/build/build.sh); release builds use the no-op stub
// in call_support_stub.go.
package buildrequest

import (
	"sync"
)

// callUnsupportedProvidersOnce caches the parsed set so
// debugFilterCallSupported stays hot on the request path — os.Getenv
// takes a lock each call.
var callUnsupportedProvidersOnce sync.Once
var callUnsupportedProvidersCached map[string]bool

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
