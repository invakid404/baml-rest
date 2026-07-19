//go:build nativestreamserve

package main

// nativeStreamServeCapable reports whether THIS serve artifact ships a worker
// that can serve the de-BAML native stream lane (Phase 7D). See the default
// variant (native_stream_capable_default.go) for the full contract.
//
// This is the native-stream-capable serve build: true. build.sh sets the
// `nativestreamserve` tag alongside the subprocess NATIVE_WORKER embed, so the
// host knows — at compile time — that its embedded worker installs the native
// stream serve factory. The pool arms DisableStreamInfrastructureRetries only when
// this is true AND BAML_REST_USE_DEBAML resolves true.
const nativeStreamServeCapable = true
