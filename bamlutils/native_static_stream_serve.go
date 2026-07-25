package bamlutils

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// Native STATIC STREAM serve seam (de-BAML Phase 3b) — NEUTRAL.
//
// This is the STREAMING twin of NativeStaticServeFunc (native_static_serve.go) and
// the STATIC twin of NativeStreamServeFunc (native_stream.go). It is the injection
// point a SERVE deploy profile uses to actually SERVE an admitted generated static
// `/stream{,-with-raw}` request natively: it opens exactly ONE provider socket,
// drives nanollm DoStream through the one-shot exact stream client, normalizes every
// chunk into owned answer/raw/reasoning deltas, and emits each delta SYNCHRONOUSLY
// through the orchestrator's EmitDelta callback so the SAME accumulation / cadence /
// partial-parse / emission pipeline the BAML transport path uses runs on the native
// lane too. The orchestrator keeps ownership of accumulation, the throttled partial
// parse, and the final parse — the only difference from the BAML lane is parser
// SELECTION (the native-only static-stream closures on the claimed lane).
//
// It deliberately carries the SAME static facts as NativeStaticInvocation (a fresh
// promptdescriptor.Function whose Return holds the exact schemadescriptor.Bundle,
// the ordered bound args, the selected-child facts, and the BAML no-send plan
// closure) PLUS the streaming-specific NeedsPartials / NeedsRaw / IncludeReasoning
// and the live EmitDelta / SendHeaders / SendFirstBody callbacks the transport
// drives. It is a SEPARATE type from the dynamic NativeStreamServeRequest so the
// dynamic-only fields (Messages, OutputSchema) can NEVER be fed to static admission,
// and a SEPARATE type from the unary NativeStaticInvocation so a stream lane can
// never be mistaken for a unary /call.
//
// Everything crossing the boundary stays neutral: no nanollm type, no internal/*
// type; the generated static stream seam only ever builds this struct and calls this
// public-typed callback. The concrete nanollm-backed implementation is injected by
// the isolated SERVE worker at the binary entry point ONLY under the umbrella flag
// (BAML_REST_USE_DEBAML). A nil func — every DEFAULT production build and every
// flag-off build — leaves the generated static stream seam's serve callback nil and
// every streamed request byte-identical to today (hard-off identity).
//
// Ownership boundary (mirrors NativeStreamServeFunc exactly): before it CLAIMS the
// native transport the callback may return a stable DECLINE and guarantees NO
// provider RoundTrip / no EmitDelta occurred — the orchestrator then serves BAML for
// the same child in the same retry iteration (pre-transport only). The transport
// claim (immediately before the one RoundTrip) is the POINT OF NO RETURN: from the
// claim onward every terminal condition is a Completed or a FailedAfterClaim, never a
// decline and never a hidden same-child BAML resend, retry, fallback, reset, or pool
// replay.
//
// SENSITIVE: Descriptor embeds the function's raw static prompt bytes and any inline
// client-option literals (including literal credentials), and BuildBAMLRequest
// returns a plan carrying the real bearer Authorization + request body. Neither this
// struct nor anything derived from it may be logged, serialized, %v-formatted,
// error-wrapped, or emitted into a metric — only redacted, secret-free views (the
// method name, the route kind, the pipeline stage, a bounded reason token, and the
// tri-state disposition). See native_static_serve.go's package doc.

// NativeStaticStreamInvocation is the neutral (no nanollm type crosses it) but
// SENSITIVE request description the generated static stream seam hands the injected
// stream serve implementation. It fuses the static descriptor/arg facts of
// NativeStaticInvocation with the live streaming callbacks of NativeStreamServeRequest.
type NativeStaticStreamInvocation struct {
	// Method is the BAML function name the generated static stream seam is serving.
	Method string
	// Descriptor is the FRESH promptdescriptor.Function the generated method selected
	// for Method (introspected.StaticPromptDescriptor returns a fresh deep value on
	// every call). Its Return holds the exact ordered schemadescriptor.Bundle; the
	// implementation lowers it via schema.FromStaticDescriptor. Mode (NativeStreamMode:
	// NativeStreamModeStream / NativeStreamModeStreamWithRaw) is a transport-mode label
	// only — Function.Return keeps the FINAL non-streaming Bundle (BAML renders the same
	// non-streaming output_format for both modes); the streaming parse profile is derived
	// INTERNALLY inside the neutral internal/debaml closure, never by flipping Bundle.Stream.
	Descriptor promptdescriptor.Function
	// Args is the EXACT generated argument map in declared descriptor-argument order —
	// the values the generated method already typed/media-converted. The implementation
	// proves it matches Descriptor.Args exactly before rendering.
	Args map[string]any
	// ArgOrder is the ordered list of argument names the generated binder emitted, in
	// declared descriptor-argument order.
	ArgOrder []string
	// Mode is the bounded streaming request mode (stream / stream_with_raw). Only the
	// two REAL public streaming modes reach the seam; a unary /call{,-with-raw} bridged
	// through the StreamRequest builder is never installed here.
	Mode NativeStreamMode

	// Provider is the resolved leaf provider (e.g. "openai").
	Provider string
	// ClientOverride is the concrete selected child/leaf client name, or empty for a
	// single default-client request that uses the descriptor's ClientConfig.
	ClientOverride string

	// SingleLeaf reports the orchestration plan resolves exactly one leaf.
	SingleLeaf bool
	// HasFallbackChain / HasRoundRobin / HasRequestRetryOverride mark the
	// whole-orchestration-plan shapes the narrow matrix does not prove; any of them
	// declines at the strategy gate BEFORE the transport is claimed. These MUST be
	// TRUTHFUL — they are the parity-decline that keeps serving honest.
	HasFallbackChain        bool
	HasRoundRobin           bool
	HasRequestRetryOverride bool

	// WouldRewriteOrProxy reports, for the request's EFFECTIVE send target, whether the
	// effective llmhttp client would rewrite the outbound URL or route it through an
	// HTTP proxy at EXECUTION time. A true result declines at the strategy gate before
	// the transport is claimed. Nil in lightweight callers that carry no send client.
	WouldRewriteOrProxy func(effectiveURL string) bool

	// NeedsPartials mirrors StreamConfig.NeedsPartials: whether the caller wants
	// intermediate parsed partials on this stream.
	NeedsPartials bool
	// NeedsRaw mirrors StreamConfig.NeedsRaw: whether the caller wants raw response
	// text (the /stream-with-raw endpoint).
	NeedsRaw bool
	// IncludeReasoning mirrors StreamConfig.IncludeReasoning: whether the caller opted
	// into surfacing provider reasoning text on the reasoning channel.
	IncludeReasoning bool

	// BuildBAMLRequest builds BAML's StreamRequest plan for THIS selected static child
	// WITHOUT sending — the same generated `StreamRequest.<Method>` no-send closure. The
	// STRICT OpenAI stream serve implementation calls it to obtain BAML's plan for the
	// pre-transport StreamRequest-plan-compare precondition; it opens NO socket. A
	// mismatch, a build error, or a nil closure declines PRE-TRANSPORT so BAML serves.
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)

	// EmitDelta is the SYNCHRONOUS owned-delta sink the orchestrator supplies. The
	// transport calls it once per nonempty normalized chunk; the orchestrator
	// accumulates, runs the throttled native-only static ParseStream, and emits the
	// ordinary partial StreamResult. Returning an error asks the transport to STOP
	// reading immediately (a terminal FailedAfterClaim, never a retry). Delta strings
	// are owned and must not alias nanollm/FFI buffers. Non-nil only while the seam is on.
	EmitDelta func(NativeStreamDelta) error

	// SendHeaders is an idempotent liveness signal the transport MUST fire the instant
	// it reads 2xx response headers (before the body) so the pool's hung detector
	// observes liveness on a slow body. Non-nil only while the seam is on.
	SendHeaders func()

	// SendFirstBody is an idempotent metric/liveness signal from the exact body wrapper
	// on the first raw upstream body byte (including an SSE comment). Non-nil only while
	// the seam is on.
	SendFirstBody func()
}

// NativeStaticStreamServeFunc actually serves one admitted generated static
// `/stream{,-with-raw}` request natively (or declines pre-transport to BAML). It
// reuses the byte-for-byte NativeStreamServeResult tri-state (native_stream.go): a
// stream result carries NO parsed output — the native-only static parsers run in the
// orchestrator over the EmitDelta-accumulated text, exactly as on the dynamic lane.
// Installed AND enabled only in a serve deploy profile with the umbrella flag on; nil
// in every default production build and every flag-off build.
type NativeStaticStreamServeFunc func(ctx context.Context, inv NativeStaticStreamInvocation) NativeStreamServeResult
