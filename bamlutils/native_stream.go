package bamlutils

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// Native STREAM serve seam (de-BAML Phase 7D) — NEUTRAL.
//
// This is the streaming twin of NativeServeFunc (native_serve.go). It is the
// injection point a SERVE deploy profile uses to actually SERVE an admitted
// dynamic OpenAI `_dynamic` StreamRequest natively: it opens exactly ONE
// provider socket, drives nanollm DoStream through the 7A one-shot exact stream
// client, normalizes every chunk into owned answer/raw/reasoning deltas, and
// emits each delta SYNCHRONOUSLY through the orchestrator's EmitDelta callback
// so the SAME accumulation/cadence/partial-parse/emission pipeline that the BAML
// transport path uses runs on the native lane too (scope §5.4). The orchestrator
// keeps ownership of accumulation, the throttled partial parse, and the final
// parse — the only difference from the BAML lane is parser SELECTION (native-only
// closures on the claimed lane).
//
// Everything here is neutral: no nanollm type crosses the boundary, and the
// generic orchestrator stays BAML- and nanollm-free. The seam mirrors the unary
// NativeServeFunc pattern: the generated dynclient adapter (a separate module
// that cannot import baml-rest's internal native packages, let alone the
// out-of-go.work nanollm bridge) only ever calls this public-typed callback; the
// concrete nanollm-backed implementation is injected by the worker at the serve
// binary's entry point. A nil func — every DEFAULT production build and every
// flag-off build — leaves the orchestrator's native stream callback nil and every
// streamed request byte-identical to today (I1 hard-off identity).
//
// Ownership boundary (scope §4 I2/I4): before it CLAIMS the native transport the
// callback may return a stable DECLINE and guarantees NO provider RoundTrip
// occurred — the orchestrator then serves BAML for the same child in the same
// retry iteration (pre-transport only). The transport claim (immediately before
// the one RoundTrip) is the POINT OF NO RETURN: from the claim onward every
// terminal condition is a Completed or a FailedAfterClaim, never a decline and
// never a hidden same-child BAML resend, retry, fallback, reset, or pool replay.

// NativeStreamMode is the bounded public-mode label handed to the stream serve
// implementation. Only the two streaming modes reach the seam; the set is fixed
// so any downstream metric mode stays bounded.
type NativeStreamMode string

const (
	// NativeStreamModeStream is the plain /stream/_dynamic mode.
	NativeStreamModeStream NativeStreamMode = "stream"
	// NativeStreamModeStreamWithRaw is /stream-with-raw/_dynamic (the raw and
	// optional reasoning channels). Both modes are admitted at the transport
	// layer; the native-only parser owns the semantic channels.
	NativeStreamModeStreamWithRaw NativeStreamMode = "stream_with_raw"
)

// NativeStreamDelta is one normalized, OWNED increment the native transport hands
// the orchestrator's EmitDelta sink. Its three channels mirror the BAML transport
// path's sse.DeltaParts exactly: ParseableDelta is the answer channel fed to the
// native-only ParseStream, RawDelta is the text-only /stream-with-raw channel,
// and ReasoningDelta carries provider reasoning text only when the caller opted
// in. Delta strings are OWNED values and MUST NOT alias any nanollm/FFI/decoder
// buffer once EmitDelta returns.
type NativeStreamDelta struct {
	ParseableDelta string
	RawDelta       string
	ReasoningDelta string
}

// NativeStreamUsage is the bounded, secret-free token usage the native stream
// sampled after a valid terminal completion. It carries NO content. It is
// retained for internal metrics only; usage is NOT surfaced on the public stream
// wire in this phase (scope §8 metadata). A zero value means the provider sent
// no usage.
type NativeStreamUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// NativeStreamDisposition is the tri-state outcome of a native stream serve
// attempt. The zero value is NativeStreamDeclined, so a zero-valued result safely
// means "declined, fall through to BAML pre-transport".
type NativeStreamDisposition uint8

const (
	// NativeStreamDeclined: the implementation did NOT claim the native
	// transport and guarantees no provider socket / no EmitDelta occurred. The
	// orchestrator serves BAML for the same child in the same retry iteration.
	// Stage/Reason are stable tokens (I2 decline is pre-transport only).
	NativeStreamDeclined NativeStreamDisposition = iota
	// NativeStreamCompleted: the upstream stream reached a valid terminal
	// condition and the decoder closed cleanly. Every partial was emitted through
	// EmitDelta; the orchestrator still owns the FINAL parse and emission. Usage
	// is the sampled token usage; WinnerEngine is the bounded engine token.
	NativeStreamCompleted
	// NativeStreamFailedAfterClaim: the transport was CLAIMED (a socket may have
	// opened) and then failed. Err is the typed public error. This is TERMINAL:
	// the orchestrator emits the terminal error frame and NEVER falls back to a
	// BAML send/parse, retries, advances a fallback child, replays through the
	// pool, or emits a continuation reset (I4).
	NativeStreamFailedAfterClaim
)

// NativeStreamServeRequest is the neutral (no nanollm type crosses it) but
// SENSITIVE request description the generated adapter hands the injected stream
// serve implementation. It carries the same admission facts as NativeServeRequest
// plus the streaming-specific NeedsPartials and the live EmitDelta / SendHeaders /
// SendFirstBody callbacks the transport drives.
//
// SENSITIVE: Registry embeds the real api_key and BuildBAMLRequest returns a plan
// carrying the real bearer Authorization + request body. Neither this struct nor
// anything derived from it may be logged, serialized, or emitted — only redacted,
// secret-free views (header NAMES, a bounded body LENGTH, stable stage/reason
// tokens).
type NativeStreamServeRequest struct {
	// Registry is the effective client registry BAML resolved the leaf from.
	Registry *ClientRegistry
	// Messages are the generated dynamic messages (output-format markers intact;
	// the native renderer substitutes them exactly as BAML's de-BAML render did).
	Messages []DynamicMessage
	// OutputSchema is the dynamic output schema, or nil.
	OutputSchema *DynamicOutputSchema

	// Provider is the resolved leaf provider (e.g. "openai").
	Provider string
	// ClientOverride is the concrete selected child/leaf client name, or empty for
	// a single default-client request.
	ClientOverride string
	// Mode is the bounded streaming request mode.
	Mode NativeStreamMode
	// NeedsPartials mirrors StreamConfig.NeedsPartials: whether the caller wants
	// intermediate parsed partials on this stream.
	NeedsPartials bool
	// NeedsRaw mirrors StreamConfig.NeedsRaw: whether the caller wants raw response
	// text (the /stream-with-raw endpoint).
	NeedsRaw bool
	// IncludeReasoning mirrors StreamConfig.IncludeReasoning: whether the caller
	// opted into surfacing provider reasoning text on the reasoning channel.
	IncludeReasoning bool

	// SingleLeaf reports the orchestration plan resolves exactly one leaf.
	SingleLeaf bool
	// HasFallbackChain / HasRoundRobin / HasRequestRetryOverride mark the
	// whole-orchestration-plan shapes the initial native matrix does not prove; any
	// of them declines at the strategy gate BEFORE the transport is claimed. These
	// MUST be TRUTHFUL: they are the parity-decline that keeps serving honest.
	HasFallbackChain        bool
	HasRoundRobin           bool
	HasRequestRetryOverride bool

	// WouldRewriteOrProxy reports, for the request's EFFECTIVE send target, whether
	// the effective llmhttp client would rewrite the outbound URL or route it
	// through an HTTP proxy at EXECUTION time. A true result declines at the
	// strategy gate before the transport is claimed. Nil in lightweight callers
	// that carry no send client.
	WouldRewriteOrProxy func(effectiveURL string) bool

	// BuildBAMLRequest builds BAML's request plan for THIS selected child WITHOUT
	// sending — the same closure the orchestrator uses to build the request it
	// would otherwise send. The STRICT OpenAI stream serve implementation calls it
	// to obtain BAML's plan for the pre-transport plan-compare precondition; it
	// opens NO socket. A mismatch, a build error, or a nil closure declines
	// PRE-TRANSPORT so BAML serves.
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)

	// EmitDelta is the SYNCHRONOUS owned-delta sink the orchestrator supplies. The
	// transport calls it once per nonempty normalized chunk; the orchestrator
	// accumulates, runs the throttled native-only ParseStream, and emits the
	// ordinary partial StreamResult. Returning nil consumes the delta; returning an
	// error asks the transport to STOP reading immediately (a terminal
	// FailedAfterClaim, never a retry). Delta strings are owned and must not alias
	// nanollm/FFI buffers. Non-nil only while the seam is on.
	EmitDelta func(NativeStreamDelta) error

	// SendHeaders is an idempotent liveness signal the transport MUST fire the
	// instant it reads 2xx response headers (before the body) so the pool's hung
	// detector observes liveness on a slow body. It does NOT cancel the first-body
	// timer and does NOT authorize pool replay. Non-nil only while the seam is on.
	SendHeaders func()

	// SendFirstBody is an idempotent metric/liveness signal from the exact body
	// wrapper on the first raw upstream body byte (including an SSE comment). Non-
	// nil only while the seam is on.
	SendFirstBody func()
}

// NativeStreamServeResult is the neutral tri-state result of a native stream serve
// attempt. Its Stage/Reason (decline) and WinnerEngine tokens are secret-free
// bounded enums. The struct carries NO parsed output — the native-only parsers run
// in the orchestrator over the EmitDelta-accumulated text.
type NativeStreamServeResult struct {
	// Disposition selects which field group below is meaningful.
	Disposition NativeStreamDisposition

	// Declined-only: stable, secret-free stage/reason (bounded enum-like tokens;
	// never free-form text or a secret) describing WHERE/WHY native stepped aside.
	Stage  string
	Reason string

	// Completed-only: the bounded winner-engine token (NativeServeEngineNative) and
	// the sampled token usage (internal metrics only, never surfaced publicly).
	WinnerEngine string
	Usage        NativeStreamUsage

	// FailedAfterClaim-only: the typed public error handed to the orchestrator's
	// terminal path (use existing typed classes — *llmhttp.HTTPError, the typed
	// stream terminal classes — so classification behaves as it does for a BAML
	// attempt), plus an optional owned raw diagnostic retained as details.raw.
	Err           error
	RawDiagnostic string
}

// NativeStreamServeFunc actually serves one admitted dynamic OpenAI `_dynamic`
// StreamRequest natively (or declines pre-transport to BAML). Installed AND
// enabled only in a serve deploy profile with the umbrella flag on; nil in every
// default production build and every flag-off build.
type NativeStreamServeFunc func(ctx context.Context, req NativeStreamServeRequest) NativeStreamServeResult
