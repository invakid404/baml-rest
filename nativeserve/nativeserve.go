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
//   - Supported prebuilt OS/arch (nanollm-ffi v0.4.3, from the archives shipped
//     inside nanollm-ffi/go): darwin/arm64, linux/amd64, and linux/arm64. Other
//     targets need a nanollm-ffi build for that platform.
//   - Keep this module OUT of any workspace whose cold `go build ./...` must stay
//     CGO-free; resolve it as an ordinary `go get` dependency.
//
// See README.md for the copy-paste recipe.
package nativeserve

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/nativeserve/admission"
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

// NewStream constructs the nanollm-backed native STREAM serve implementation
// (de-BAML Phase 7D) and returns it as the neutral [bamlutils.NativeStreamServeFunc]
// a serve-profile worker installs via workerboot.Options.NativeStreamServeFactory.
// It is the streaming twin of [New]: for an admitted dynamic OpenAI `/stream{,-with-raw}/_dynamic`
// StreamRequest it serves natively (one exact provider RoundTrip driving nanollm
// DoStream, the native-only partial/final parsers owned by the orchestrator), while
// every unsupported shape declines PRE-TRANSPORT so BAML serves it. From the
// transport claim onward a failure is TERMINAL — never a BAML resend/retry/replay.
//
// With the umbrella flag (BAML_REST_USE_DEBAML) off the generated dynamic stream
// seam never installs the callback, so the stream path stays byte-identical BAML
// with zero native FFI / socket / plan build.
//
// It is a thin, stable pass-through to the relocated serve core so the subprocess
// serve worker drives byte-identical native streaming. reg is retained for
// signature symmetry with [New]; the stream lane records no de-BAML counters this
// phase (the rollout observability is owner-trimmed), so it never errors on reg.
func NewStream(reg prometheus.Registerer) (bamlutils.NativeStreamServeFunc, error) {
	return canary.NewStreamServeFunc(reg)
}

// NewStaticObserve constructs the nanollm-backed native STATIC no-send admission
// OBSERVER (de-BAML Slice 8B) and returns it as the neutral
// [bamlutils.NativeStaticObserveFunc] a native worker installs via
// workerboot.Options.NativeStaticObserveFactory (only while the umbrella flag is
// on). It is the STATIC observe-only twin of [New]: for an eligible generated static
// method it runs the FULL pre-socket admission predicate (descriptor envelope,
// arg-binder match, Return-Bundle lower/support, RenderStatic, canonical body,
// nanollm New/Prepare, and the strict BAML `Request.<Method>` no-send plan compare)
// and then ALWAYS forces a pre-socket decline so the original generated
// `Request.<Method>` / `Parse.<Method>` (BAML) execute exactly as today. It opens NO
// socket, RoundTrips NOTHING, and serves NO native result — it proves ATTACHMENT
// with zero behavior change.
//
// reg is retained for signature symmetry with [New]; 8B records no de-BAML counters
// (observe-only), so it never errors on reg. With the umbrella flag off the generated
// static seam never installs the callback, so the static path stays byte-identical
// BAML with zero native FFI / socket / plan build.
//
// The callback maps the neutral invocation onto admission.AdmitStatic — hardcoding
// the layer-1 facts (WorkerCapable / RequestAPIPresent / OnBuildRequestRoute /
// FlagEnabled all true, RouteKindStatic), exactly as the dynamic serve path's
// toAdmissionInput does — then translates the recorded observation into a forced
// NativeStaticDeclined result.
// NewStaticServe constructs the nanollm-backed native STATIC SERVE implementation
// (de-BAML Slice 8C) and returns it as the neutral [bamlutils.NativeStaticServeFunc]
// a SERVE-profile worker installs via workerboot.Options.NativeStaticServeFactory
// (only while the umbrella flag is on). It is the SERVING twin of [NewStaticObserve]:
// for an admitted static unary `/call` it runs the FULL pre-socket predicate
// (descriptor envelope, arg-binder match, Return-Bundle lower/support + the proven
// return-shape gate, RenderStatic, canonical body, nanollm New/Prepare, and the
// strict BAML `Request.<Method>` no-send plan compare) and, on a full would-admit,
// CLAIMS the attempt and performs exactly ONE provider RoundTrip — native
// translate/extract, native static SAP over the selected Return Bundle, and the S5
// same-response BAML `Parse.<Method>` safety compare — returning the winning
// flattened canonical JSON. Every unsupported shape declines PRE-SOCKET so BAML
// serves it, and from the claim onward a failure is TERMINAL (never a BAML resend).
//
// It is a thin pass-through to the SAME relocated Slice-6 serve core (canary), so
// the static and dynamic serving paths drive byte-identical transport. With the
// umbrella flag off the generated static seam never installs the callback, so the
// static path stays byte-identical BAML with zero native FFI / socket / plan build.
func NewStaticServe(reg prometheus.Registerer) (bamlutils.NativeStaticServeFunc, error) {
	return canary.NewStaticServeFunc(reg)
}

// NewStaticShadow constructs the nanollm-backed native STATIC Stage-1 SHADOW
// comparator (de-BAML Slice 8C) and returns it as the neutral
// [bamlutils.NativeStaticShadowFunc] a SHADOW-profile worker installs via
// workerboot.Options.NativeStaticShadowFactory. For an eligible generated static
// `/call` it runs the FULL pre-socket admission predicate + the strict BAML
// `Request.<Method>` plan compare and, on a full plan match, threads an OnResponse
// continuation onto its declined outcome; the orchestrator invokes it with BAML's
// already-fetched bytes AFTER BAML serves, and native compares its translate/extract/
// static-SAP/typed-decode against BAML's parse of the SAME bytes — with ZERO native
// sends. It ALWAYS declines so BAML serves. With the umbrella flag off the generated
// static seam never installs it. It reuses the SAME serve core as [NewStaticServe].
func NewStaticShadow(reg prometheus.Registerer) (bamlutils.NativeStaticShadowFunc, error) {
	return canary.NewStaticShadowFunc(reg)
}

func NewStaticObserve(reg prometheus.Registerer) (bamlutils.NativeStaticObserveFunc, error) {
	return func(ctx context.Context, inv bamlutils.NativeStaticInvocation) bamlutils.NativeStaticResult {
		si := admission.StaticInput{
			WorkerCapable:           true,
			RequestAPIPresent:       true,
			OnBuildRequestRoute:     true,
			FlagEnabled:             true,
			RouteKind:               admission.RouteKindStatic,
			Method:                  inv.Method,
			Descriptor:              inv.Descriptor,
			Args:                    inv.Args,
			ArgOrder:                inv.ArgOrder,
			Mode:                    inv.Mode,
			SingleLeaf:              inv.SingleLeaf,
			HasFallbackChain:        inv.HasFallbackChain,
			HasRoundRobin:           inv.HasRoundRobin,
			HasRequestRetryOverride: inv.HasRequestRetryOverride,
			Raw:                     inv.Raw,
			ClientOverride:          inv.ClientOverride,
			Provider:                inv.Provider,
			WouldRewriteOrProxy:     inv.WouldRewriteOrProxy,
			BuildBAMLRequest:        inv.BuildBAMLRequest,
		}
		// Route STRICTLY on the observation mode and FAIL CLOSED on anything else: only
		// final runs the full predicate; parse-only observes the Return-Bundle final
		// support; a stream (or any unrecognized) mode declines here WITHOUT reaching
		// render/Prepare, so a NativeStaticModeStream invocation can never be admitted.
		var obs admission.StaticObservation
		switch inv.Mode {
		case bamlutils.NativeStaticModeFinal:
			obs = admission.AdmitStatic(ctx, si)
		case bamlutils.NativeStaticModeParseOnly:
			obs = admission.AdmitStaticParse(ctx, si)
		default:
			obs = admission.DeclineStaticMode(inv.Mode)
		}
		// OBSERVE-ONLY: force a pre-socket decline regardless of the observation.
		return bamlutils.NativeStaticResult{
			Disposition: bamlutils.NativeStaticDeclined,
			Observation: obs.Observation,
			Family:      obs.Family,
			Stage:       obs.Stage,
			Reason:      obs.Reason,
		}
	}, nil
}
