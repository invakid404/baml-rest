package bamlutils

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// Native SERVE seam (de-BAML cutover Slice 6) — NEUTRAL.
//
// This is the injection point a SERVE deploy profile uses to actually SERVE an
// admitted unary `_dynamic` call natively: it opens exactly ONE provider socket,
// translates/extracts/parses that response, and returns the ordinary generated
// final result through the already-merged Slice-1 tryOneChild seam. It is the
// serving twin of NativeShadowFunc (which always declines): the shadow seam
// proves plan/response parity with BAML still serving, this seam serves native.
//
// Everything here is neutral: no nanollm type crosses the boundary, and the
// generic orchestrator stays BAML- and nanollm-free. The seam mirrors the
// DeBAMLRenderFunc / DeBAMLParseFunc / NativeShadowFunc pattern: the generated
// dynclient adapter (a separate module that cannot import baml-rest's internal
// native packages, let alone the out-of-go.work nanollm bridge) only ever calls
// this public-typed callback; the concrete nanollm-backed serve implementation is
// injected by the worker at the serve binary's entry point. A nil func — every
// DEFAULT production build and every flag-off build — leaves the orchestrator's
// native child-attempt callback nil and every request byte-identical to today.
//
// Ownership boundary (see the cutover Slice 6 scope): before it CLAIMS the native
// attempt, the callback may return a stable DECLINE and guarantees NO provider
// RoundTrip occurred — the orchestrator then serves BAML for the same child in
// the same retry iteration. From the claim onward every terminal condition is a
// SUCCESS or a FAILURE (typed error handed to the outer policy); there is never a
// hidden same-child BAML resend after the claim.

// NativeServeMode is the bounded request-mode label handed to the serve
// implementation. Only the unary call modes reach the seam (streaming never
// routes through the non-streaming orchestrator); the set is fixed so any
// downstream metric mode stays bounded.
type NativeServeMode string

const (
	// NativeServeModeCall is the plain unary `call` — the only mode the first
	// serving surface admits.
	NativeServeModeCall NativeServeMode = "call"
	// NativeServeModeCallWithRaw is `call-with-raw`; its native serving envelope
	// is not yet proven, so the implementation declines it at the mode gate — it
	// is carried only so the mode label is accurate.
	NativeServeModeCallWithRaw NativeServeMode = "call_with_raw"
)

// Bounded, secret-free winner-engine tokens for a native-served final. They are
// carried on NativeServeResult and forwarded verbatim onto the ordinary outcome
// metadata (winner_engine) so a dashboard can tell WHICH engine produced the
// served final without overloading the X-BAML-Path contract.
const (
	// NativeServeEngineNative means native transport + native structured-output
	// parser (SAP) produced the served final.
	NativeServeEngineNative = "native"
	// NativeServeEngineBAMLParse means native owned the single provider request,
	// but BAML parse-only (on the SAME bytes) produced the served final — either
	// because native SAP declined the shape or because native/BAML structured
	// output drifted and BAML's parse is served for safety.
	NativeServeEngineBAMLParse = "native_baml_parse"
)

// NativeServeRequest is the neutral (no nanollm type crosses it) but SENSITIVE
// request description the generated adapter hands the injected serve
// implementation. It carries the same
// S3/S4/S5 facts as NativeShadowRequest plus IncludeReasoning and the first-2xx
// SendHeartbeat liveness signal the native transport must fire.
//
// It deliberately carries NO nanollm type. Registry/Messages/OutputSchema are the
// same effective one-client registry, generated dynamic messages, and dynamic
// output schema BAML built its request from; the implementation re-derives the
// native plan from them, compares against BuildBAMLRequest's output as a
// pre-socket precondition, then serves natively.
//
// SENSITIVE: Registry embeds the real api_key and BuildBAMLRequest returns a plan
// carrying the real bearer Authorization + request body. Neither this struct nor
// anything derived from it may be logged, serialized, or emitted — only redacted,
// secret-free views that carry NO content-derived value: header NAMES, a bounded
// body LENGTH (never a hash/digest — even a truncated digest can confirm/correlate
// content), and stable stage/reason tokens.
type NativeServeRequest struct {
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
	// Mode is the bounded request mode.
	Mode NativeServeMode
	// IncludeReasoning mirrors CallConfig.IncludeReasoning: whether the caller
	// opted into surfacing provider reasoning/thinking text on the structured
	// reasoning channel. It gates the native reasoning extraction channel.
	IncludeReasoning bool

	// SingleLeaf reports the orchestration plan resolves exactly one leaf.
	SingleLeaf bool
	// HasFallbackChain / HasRoundRobin / HasRequestRetryOverride mark the
	// whole-orchestration-plan shapes the initial native matrix does not prove; any
	// of them declines at the strategy gate BEFORE a native plan is built, BAML's
	// plan is obtained, a plan_compare is recorded, or a socket is claimed.
	//
	// These MUST be TRUTHFUL: they are the parity-decline that keeps serving
	// honest. HasRequestRetryOverride is set when the request carries a retry
	// policy the single-attempt exact lane would bypass.
	HasFallbackChain        bool
	HasRoundRobin           bool
	HasRequestRetryOverride bool

	// WouldRewriteOrProxy reports, for the request's EFFECTIVE send target, whether
	// the effective llmhttp client would rewrite the outbound URL or route it
	// through an HTTP proxy at EXECUTION time. It is the effective send client's
	// llmhttp.Client.WouldRewriteOrProxy, evaluated by the admission predicate
	// against the effective target it resolves (base_url + /chat/completions). A
	// true result declines at the strategy gate before a native plan is prepared,
	// BAML's plan is obtained, a plan_compare is recorded, or a socket is claimed.
	// Nil in lightweight callers that carry no send client.
	WouldRewriteOrProxy func(effectiveURL string) bool

	// BuildBAMLRequest builds BAML's request plan for THIS selected child WITHOUT
	// sending — the same closure the orchestrator uses to build the request it
	// would otherwise send. A STRICT OpenAI serve implementation calls it to obtain
	// BAML's plan for the S4 plan-compare PRECONDITION; it opens NO socket. For the
	// strict anchor a mismatch, a build error, or a nil closure declines PRE-SOCKET
	// so BAML serves.
	//
	// It is explicitly OPTIONAL. The DIRECT-LEGACY native-first probe route (a
	// single unary leaf whose existing BAML route is legacy) passes nil. Under the
	// TRUSTED verification policy the closure is never INVOKED — the trusted policy
	// claims no BAML plan and records no plan_compare at all (nanollm owns the
	// provider's transport contract) — so whether a generated caller supplies it is
	// immaterial: it may be non-nil yet stays unused. Only strict OpenAI serving
	// calls it (for the S4 plan-compare precondition). A strict OpenAI leaf that
	// reaches the probe route (e.g. a debug support override) sees a nil closure and
	// declines strict verification, preserving that debug route.
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)

	// BAMLOnlyParse is the explicit BAML-ONLY final-parse closure. Given the
	// assistant text extracted from the native provider response, it runs ONLY
	// BAML's parser — never the native-first hybrid — and returns the flattened
	// structured JSON. The serve implementation uses it for the S5 same-response
	// safety compare (native structured vs BAML parse of the SAME bytes) and as
	// the parse-only fallback when native SAP declines. It opens no socket and
	// issues no provider request — it only parses already-fetched text. Non-nil
	// only while the serve seam is on.
	//
	// SENSITIVE: the returned JSON is parsed provider output; treat like the
	// response body — never log or emit it, only redacted/secret-free views.
	BAMLOnlyParse func(ctx context.Context, raw string) ([]byte, error)

	// SendHeartbeat is the neutral first-2xx liveness signal the native transport
	// MUST fire the instant it reads 2xx response headers (before buffering the
	// body) so the pool's hung detector observes liveness on a slow body and does
	// not retry/restart a worker whose provider request is already in flight. It
	// is idempotent and best-effort — see NativeCallAttempt.SendHeartbeat. Non-nil
	// only while the serve seam is on.
	SendHeartbeat func()
}

// NativeServeDisposition is the tri-state outcome of a native serve attempt,
// mirroring buildrequest.NativeCallDisposition across the neutral worker/generated
// boundary. The zero value is NativeServeDeclined, so a zero-valued result safely
// means "declined, fall through to BAML".
type NativeServeDisposition uint8

const (
	// NativeServeDeclined: the implementation did NOT claim a native attempt and
	// guarantees no provider socket occurred. The orchestrator serves BAML for
	// the same child in the same retry iteration. Stage/Reason are stable tokens.
	NativeServeDeclined NativeServeDisposition = iota
	// NativeServeSucceeded: native owned the single provider request and produced
	// the served final. FinalJSON is the flattened dynamic-output JSON (the
	// generated helper wraps it via wrapDeBAMLDynamicOutput); Raw/Reasoning are
	// the owned /call-with-raw channels; WinnerEngine is the bounded engine token.
	NativeServeSucceeded
	// NativeServeFailed: native claimed the attempt (a socket MAY have opened) and
	// then failed. Err is the typed error handed to the outer policy; RawDiagnostic
	// is the owned raw body/text retained for details.raw. The orchestrator NEVER
	// falls through to a second BAML send for the same child.
	NativeServeFailed
)

// NativeServeResult is the neutral tri-state result of a native serve attempt.
// Its Stage/Reason (decline) and WinnerEngine tokens are secret-free bounded
// enums, but the SUCCESS payload is SENSITIVE — FinalJSON is parsed provider
// output and Raw/Reasoning are the provider's own text; treat them like the
// response body and never log/serialize/emit the struct wholesale, only redacted
// views.
type NativeServeResult struct {
	// Disposition selects which field group below is meaningful.
	Disposition NativeServeDisposition

	// Declined-only: stable, secret-free stage/reason (bounded enum-like tokens;
	// never free-form text or a secret) describing WHERE/WHY native stepped aside.
	Stage  string
	Reason string

	// Succeeded-only: the flattened dynamic-output JSON the generated helper wraps
	// via wrapDeBAMLDynamicOutput, the owned /call-with-raw raw + reasoning
	// channels, and the bounded winner-engine token (NativeServeEngineNative or
	// NativeServeEngineBAMLParse).
	//
	// Ownership contract: FinalJSON/Raw/Reasoning MUST NOT alias any transport
	// buffer once the callback returns — the implementation owns them outright.
	FinalJSON    []byte
	Raw          string
	Reasoning    string
	WinnerEngine string

	// Failed-only: the typed error handed to the outer fallback/retry loop (use
	// existing typed classes — *llmhttp.HTTPError, *buildrequest.OutputParseError,
	// llmhttp transport errors — so the outer policy behaves exactly as for a BAML
	// attempt), plus an optional owned raw diagnostic retained as details.raw.
	Err           error
	RawDiagnostic string
}

// NativeServeFunc actually serves one admitted unary `_dynamic` call natively (or
// declines pre-socket to BAML). Installed AND enabled only in a serve deploy
// profile with the umbrella flag on; nil in every default production build and
// every flag-off build.
type NativeServeFunc func(ctx context.Context, req NativeServeRequest) NativeServeResult
