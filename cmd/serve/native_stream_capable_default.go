//go:build !nativestreamserve

package main

// nativeStreamServeCapable reports whether THIS serve artifact ships a worker
// that can serve the de-BAML native stream lane (Phase 7D). It is an explicit
// COMPILE/DEPLOYMENT capability, NOT the umbrella flag: the pool's native-stream
// retry suppression (pool.Config.DisableStreamInfrastructureRetries) is armed
// only when this is true AND BAML_REST_USE_DEBAML resolves true, so a BAML-only
// artifact with the default-on umbrella but no native stream worker keeps the
// current pool retry behaviour (scope §7D pool rule).
//
// This is the DEFAULT (BAML-only) build: false. The native-stream-capable serve
// build (build.sh NATIVE_WORKER=true, which embeds the serve-capable subprocess
// worker) sets the `nativestreamserve` build tag, selecting the true variant in
// native_stream_capable_enabled.go.
const nativeStreamServeCapable = false
