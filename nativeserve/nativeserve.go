// Package nativeserve is the PUBLIC, nanollm-linked native SERVE implementation
// for de-BAML (#624). It exposes a single stable constructor, [New], that returns
// the neutral [bamlutils.NativeServeFunc] a dynclient consumer passes to
// dynclient.WithNativeServeComparator to gain native transport at parity with the
// subprocess serve worker: an admitted unary OpenAI `/call/_dynamic` call is served
// natively (one exact provider RoundTrip, native translate/extract/parse, the S4
// plan-compare pre-socket precondition + the S5 same-response BAML-parse safety
// compare), while every unsupported shape (streaming, non-openai, fallback/round-
// robin, static, call-with-raw) declines PRE-SOCKET so BAML serves it — inherited
// from the shared admission, not re-specified here. With the umbrella flag
// (BAML_REST_USE_DEBAML) off, the generated dynamic call seam never installs the
// callback, so the dynamic path stays byte-identical BAML with zero native FFI /
// socket / plan build.
//
// The implementation is the EXACT same Slice-6 serve core the subprocess worker
// uses — [github.com/invakid404/baml-rest/nativeserve/canary] plus admission /
// execute / parity — so an in-process dynclient caller and the serve worker are at
// transport parity by construction (New delegates to canary.NewServeFunc; the
// worker injects New via workerboot.Options.NativeServeFactory).
//
// # CGO build recipe
//
// This module links github.com/viktordanov/nanollm-ffi/go, a CGO package that
// links a prebuilt Rust static archive committed per-platform inside that module.
// A consumer importing nativeserve therefore opts THEIR link-graph into CGO +
// nanollm (baml-rest's own host/root and the DEFAULT dynclient stay zero-nanollm /
// CGO-free — only this import pulls nanollm in). To build:
//
//   - Set CGO_ENABLED=1 and provide a C toolchain (clang/gcc).
//   - Supported prebuilt OS/arch (nanollm-ffi v0.3.2, from the archives shipped
//     inside nanollm-ffi/go): darwin/arm64, linux/amd64, and linux/arm64. Other
//     targets need a nanollm-ffi build for that platform.
//   - Keep this module OUT of any workspace whose cold `go build ./...` must stay
//     CGO-free; resolve it as an ordinary `go get` dependency.
//
// See README.md for the copy-paste recipe.
package nativeserve

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/nativeserve/canary"
)

// New constructs the nanollm-backed native serve implementation and returns it as
// the neutral [bamlutils.NativeServeFunc] that dynclient.WithNativeServeComparator
// (or workerboot.Options.NativeServeFactory) installs. It registers the bounded
// de-BAML collectors (declines / attempts / plan_compare / response_compare /
// native_sockets / fallback) on reg, so pass the non-nil registry whose metrics you gather
// (e.g. a prometheus.NewRegistry(), or prometheus.DefaultRegisterer). reg MUST be
// non-nil — the collectors are registered eagerly, so a nil registerer panics.
//
// New is a thin, stable pass-through to the relocated Slice-6 serve core so the
// in-process dynclient path and the subprocess serve worker drive byte-identical
// serving behaviour. It returns an error only if a collector fails to register on
// reg (e.g. a duplicate registration on a reused registry).
func New(reg prometheus.Registerer) (bamlutils.NativeServeFunc, error) {
	return canary.NewServeFunc(reg)
}
