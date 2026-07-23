package bamlutils

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// Native STATIC no-send admission seam (de-BAML Slice 8B) — NEUTRAL, OBSERVE-ONLY.
//
// This is the injection point a native worker uses to run the full STATIC unary
// admission predicate for an eligible generated static method — descriptor lookup,
// argument binding, static Bundle lower/support, RenderStatic/Prepare, and the
// strict BAML `Request.<Method>` no-send plan compare — then ALWAYS force a
// pre-socket decline so the original generated `Request.<Method>` / `Parse.<Method>`
// (BAML) execute exactly as today. It NEVER opens a socket, NEVER RoundTrips, and
// NEVER serves a native result. It proves ATTACHMENT (method -> fresh descriptor ->
// exact args -> native render/Prepare -> BAML plan compare) with zero behavior
// change: BAML still serves and parses every request.
//
// It is the STATIC observe-only twin of the dynamic NativeShadowFunc (which also
// always declines): where the dynamic shadow re-derives a native plan from a
// runtime DynamicOutputSchema and generated messages, the static observer selects
// the generated function's own fresh promptdescriptor.Function (whose Return holds
// the exact schemadescriptor.Bundle) and binds the generated typed arguments.
//
// Everything here is neutral PASSIVE data/closures: no nanollm type crosses the
// boundary, no internal/* type crosses it, and the generic orchestrator + the
// generated adapter (a separate module that cannot import baml-rest's internal
// native packages, let alone the out-of-go.work nanollm bridge) only ever build
// this struct and call this public-typed callback. The concrete nanollm-backed
// observer is injected by the isolated native/shadow worker at the binary entry
// point ONLY under the umbrella flag (BAML_REST_USE_DEBAML). A nil func — every
// DEFAULT production build, every BAML-only worker, and every flag-off build —
// leaves the generated static seam's observer callback nil and every request
// byte-identical to today.
//
// SENSITIVE: Descriptor embeds the function's raw static prompt bytes and any
// inline client-option literals (including credentials declared as literals), and
// BuildBAMLRequest returns a plan carrying the real bearer Authorization + request
// body. Neither this struct nor anything derived from it (descriptor, args,
// rendered prompt, canonical body, prepared plan, BAML plan) may be logged,
// serialized, %v-formatted, error-wrapped, or emitted into a metric — only
// redacted, secret-free views that carry NO content-derived value: the method
// name, the route kind, the pipeline stage, a bounded reason token, and the
// tri-state observation. Mirror bamlutils/native_serve.go:70-83 and the Phase 8A
// descriptor contract (bamlutils/promptdescriptor/descriptor.go:22-31).

// NativeStaticMode is the bounded request-mode label handed to the static
// observer. The set is fixed so any downstream metric mode stays bounded. Only the
// unary final and parse-only observations are exercised in 8B; the stream mode is
// carried so the label is accurate but the observer never claims a stream route.
type NativeStaticMode string

const (
	// NativeStaticModeFinal is the unary `/call` final-render observation: the
	// observer renders the static prompt, builds the canonical body, and compares
	// the prepared plan against BAML's `Request.<Method>` no-send plan.
	NativeStaticModeFinal NativeStaticMode = "final"
	// NativeStaticModeParseOnly is the `/parse` observation: the observer selects
	// the descriptor's Return Bundle and MEASURES native final support, then
	// declines so BAML's `Parse.<Method>` runs on the same response as today. No
	// prompt render, body build, or provider plan is involved.
	NativeStaticModeParseOnly NativeStaticMode = "parse_only"
	// NativeStaticModeStream is carried only so a streaming static observation is
	// labeled accurately. 8B may MEASURE SupportsNativeStreamBundle but never
	// claims a stream route (no stream result/mapping contract exists yet).
	NativeStaticModeStream NativeStaticMode = "stream"
)

// NativeStaticInvocation is the neutral (no nanollm type crosses it) but SENSITIVE
// request description the generated static seam hands the injected observer. It
// carries the fresh per-call descriptor the generated method selected via
// introspected.StaticPromptDescriptor(Method), the EXACT generated argument map
// (declared descriptor-argument order, already typed/media-converted by the
// generated method), the selected-child facts the admission predicate needs, and
// the closures that reproduce BAML's own no-send request/parse for the strict
// comparison.
//
// It deliberately carries NO nanollm type and NO internal/* type. The observer
// re-derives the native plan from Descriptor + Args (lower Return -> Bundle, render
// the static prompt, normalize the client, build the canonical body, nanollm
// Prepare) and compares against BuildBAMLRequest's output as a pre-socket
// precondition, then ALWAYS declines.
//
// SENSITIVE — see the package doc. Descriptor + Args + BuildBAMLRequest all carry
// secret material; never log/serialize/emit the struct or anything derived from it.
type NativeStaticInvocation struct {
	// Method is the BAML function name the generated static seam is observing.
	Method string
	// Descriptor is the FRESH promptdescriptor.Function the generated method
	// selected for Method (introspected.StaticPromptDescriptor returns a fresh
	// deep value on every call). Its Return holds the exact ordered
	// schemadescriptor.Bundle; the observer lowers it via schema.FromStaticDescriptor.
	Descriptor promptdescriptor.Function
	// Args is the EXACT generated argument map in declared descriptor-argument
	// order — the values the generated method already typed/media-converted. The
	// observer proves it matches Descriptor.Args exactly before rendering.
	Args map[string]any
	// ArgOrder is the ordered list of argument names the generated binder emitted,
	// in declared descriptor-argument order. It lets the observer prove the binder
	// matches Descriptor.Args by name AND order without relying on Go map iteration.
	ArgOrder []string
	// Mode is the bounded observation mode (final / parse-only / stream).
	Mode NativeStaticMode

	// Provider is the resolved leaf provider (e.g. "openai").
	Provider string
	// ClientOverride is the concrete selected child/leaf client name, or empty for
	// a single default-client request that uses the descriptor's ClientConfig. Any
	// non-empty override that is not provably byte-identical to the descriptor
	// ClientConfig declines at the static-client gate.
	ClientOverride string
	// Raw reports whether this is a call-with-raw request (the caller opted into the
	// owned raw/reasoning channels). The native static raw envelope is not proven in
	// 8B, so a true value declines at the mode gate BEFORE any render/Prepare — it is
	// carried so the observation is truthful, never to claim a raw route.
	Raw bool

	// SingleLeaf reports the orchestration plan resolves exactly one leaf.
	SingleLeaf bool
	// HasFallbackChain / HasRoundRobin / HasRequestRetryOverride mark the
	// whole-orchestration-plan shapes the narrow 8B matrix does not prove; any of
	// them declines at the strategy gate BEFORE a native plan is built, BAML's plan
	// is obtained, a plan_compare is recorded, or (in a later serving slice) a
	// socket is claimed. These MUST be TRUTHFUL — they are the parity-decline that
	// keeps admission honest.
	HasFallbackChain        bool
	HasRoundRobin           bool
	HasRequestRetryOverride bool

	// WouldRewriteOrProxy reports, for the request's EFFECTIVE send target, whether
	// the effective llmhttp client would rewrite the outbound URL or route it
	// through an HTTP proxy at EXECUTION time. A true result declines at the
	// strategy gate before a native plan is prepared. Nil in lightweight callers
	// that carry no send client.
	WouldRewriteOrProxy func(effectiveURL string) bool

	// BuildBAMLRequest builds BAML's request plan for THIS selected static child
	// WITHOUT sending — the same generated `Request.<Method>` no-send closure. The
	// observer calls it to obtain BAML's plan for the strict plan-compare
	// precondition; it opens NO socket. A mismatch, a build error, or a nil closure
	// records the mismatch/decline; the observer declines pre-socket regardless.
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)

	// BAMLOnlyParse is the explicit BAML-ONLY final-parse closure — the same
	// generated `Parse.<Method>` on the SAME response only. It is carried for the
	// parse-only observation's shape parity with the dynamic seam and for a later
	// serving slice's same-response safety compare; it opens no socket and issues
	// no provider request. Nil while the observer seam is off.
	//
	// SENSITIVE: the returned JSON is parsed provider output; treat like the
	// response body — never log or emit it, only redacted/secret-free views.
	BAMLOnlyParse func(ctx context.Context, raw string) ([]byte, error)

	// DecodeNativeFinal maps this method's native canonical JSON into its concrete
	// generated Go return type. It is DEFINED here so the generated per-method
	// decoder has a stable neutral home, but is UNUSED by the isolated serve core
	// (de-BAML Slice 8C): the winning flattened JSON (native or the same-response
	// BAML parse) is returned as NativeStaticServeResult.FinalJSON and decoded by
	// the generated /call seam itself — where the concrete return type lives — via
	// the per-method DecodeNativeStaticFinal helper. It is carried for shape parity
	// with the dynamic seam and for a caller that prefers an in-boundary decode.
	// Nil in the observe-only 8B callers.
	DecodeNativeFinal func(canonicalJSON []byte) (any, error)

	// IncludeReasoning mirrors CallConfig.IncludeReasoning: whether the caller opted
	// into surfacing provider reasoning/thinking text on the structured reasoning
	// channel. It gates the native reasoning extraction channel in the serve core.
	// False for the observe-only 8B callers and for a plain `call`.
	IncludeReasoning bool

	// SendHeartbeat is the neutral first-2xx liveness signal the native transport
	// MUST fire the instant it reads 2xx response headers (before buffering the
	// body) so the pool's hung detector observes liveness on a slow body. It is
	// idempotent and best-effort; nil in the observe-only 8B callers (no socket is
	// ever opened) and normalized to a no-op by the serve core.
	SendHeartbeat func()
}

// NativeStaticObservation is the tri-state outcome the observer RECORDS for an
// eligible static invocation before forcing its decline. It is a secret-free,
// bounded enum carried only for observability; it is NOT a serving disposition
// (the observer ALWAYS declines in 8B — see NativeStaticResult.Disposition).
type NativeStaticObservation string

const (
	// NativeStaticObserveDecline: a pre-plan gate declined (bad envelope,
	// arg-binder mismatch, unsupported Bundle/prompt/client, a strategy/override
	// shape outside the narrow matrix, or a Prepare error). The Family/Stage/Reason
	// name where and why. This is a MEASURED decline, never an error.
	NativeStaticObserveDecline NativeStaticObservation = "decline"
	// NativeStaticObserveWouldAdmit: every pre-plan gate passed AND the native
	// prepared plan byte-matched BAML's `Request.<Method>` no-send plan. A later
	// serving slice could CLAIM this; 8B records it and declines.
	NativeStaticObserveWouldAdmit NativeStaticObservation = "would_admit"
	// NativeStaticObservePlanMismatch: every pre-plan gate passed and nanollm
	// Prepare succeeded, but the native plan did NOT byte-match BAML's plan. It is
	// an observable, non-user-facing routing signal — never a user-visible error —
	// and, like every other outcome, still declines to BAML.
	NativeStaticObservePlanMismatch NativeStaticObservation = "plan_mismatch"
)

// NativeStaticObserveFamily is the bounded decline-family token the static
// admission manifest counts. Every family maps a group of gates so the manifest
// pin (total / would-admit / descriptor-envelope-decline / prompt-decline /
// client-decline / plan-match) stays a small closed set with an anti-omission
// check that each family occurs at least once.
type NativeStaticObserveFamily string

const (
	// NativeStaticFamilyNone is the zero value: no decline family (would-admit or
	// plan-mismatch rows).
	NativeStaticFamilyNone NativeStaticObserveFamily = ""
	// NativeStaticFamilyCapability groups the layer-1 build/flag/route declines
	// (worker not capable, Request API absent, not on the BuildRequest route, flag
	// disabled). These are the same pre-descriptor gates the dynamic path applies.
	NativeStaticFamilyCapability NativeStaticObserveFamily = "capability"
	// NativeStaticFamilyDescriptorEnvelope groups every descriptor/Return-side
	// decline: a missing/declined descriptor, a version/method/Return-envelope
	// mismatch, an arg-binder that does not exactly match Descriptor.Args, a Return
	// Bundle that fails FromStaticDescriptor/ValidateOutput, and a Bundle outside
	// the native final SAP support bounds (the correct 8B recursion decline).
	NativeStaticFamilyDescriptorEnvelope NativeStaticObserveFamily = "descriptor_envelope"
	// NativeStaticFamilyPrompt groups the static PROMPT declines: SupportsStatic /
	// RenderStatic reports the prompt shape (or its argument interpolation) is
	// outside the proven byte-exact surface.
	NativeStaticFamilyPrompt NativeStaticObserveFamily = "prompt"
	// NativeStaticFamilyClient groups the static CLIENT + body declines: a
	// non-literal model, a body-affecting/request_body option, a client-registry /
	// WithClient override not proven byte-identical to the descriptor ClientConfig,
	// a strategy parent / fallback / round-robin / retry override, a proxy/rewrite,
	// or a canonical-body shape BuildOpenAIChat cannot serialize byte-exact.
	NativeStaticFamilyClient NativeStaticObserveFamily = "client"
	// NativeStaticFamilyPrepare groups a nanollm New/Prepare error after every
	// pre-Prepare gate passed. It is distinct from a plan mismatch (which is a
	// successful Prepare whose plan simply differs).
	NativeStaticFamilyPrepare NativeStaticObserveFamily = "prepare"
)

// NativeStaticDisposition is the disposition of a native static observation. In 8B
// the ONLY value ever produced is NativeStaticDeclined — the observer never claims
// a native attempt and guarantees no provider socket occurred. The zero value is
// NativeStaticDeclined, so a zero-valued result safely means "declined, BAML
// serves". The enum is carried so a later serving slice (8C) can add a Served
// disposition without reshaping the neutral result.
type NativeStaticDisposition uint8

const (
	// NativeStaticDeclined: the observer did NOT claim a native attempt and
	// guarantees no provider socket occurred. The generated `Request.<Method>` /
	// `Parse.<Method>` (BAML) execute exactly as today. This is the ONLY 8B value.
	NativeStaticDeclined NativeStaticDisposition = iota
)

// NativeStaticResult is the neutral, secret-free result of a static observation.
// The observer ALWAYS declines in 8B; Observation/Family/Stage/Reason are stable,
// bounded enum-like tokens describing WHAT it observed and WHERE/WHY it stepped
// aside. They are carried only for observability and must never be free-form text
// or a secret.
type NativeStaticResult struct {
	// Disposition is always NativeStaticDeclined in 8B (the zero value), so a
	// zero-valued result safely means "declined, BAML serves".
	Disposition NativeStaticDisposition

	// Observation is the recorded tri-state: would-admit, plan-mismatch, or decline.
	Observation NativeStaticObservation
	// Family is the bounded decline family (NativeStaticFamilyNone for a
	// would-admit / plan-mismatch observation).
	Family NativeStaticObserveFamily
	// Stage and Reason are stable, secret-free tokens (bounded enum-like; never
	// free-form text or a secret) describing the exact pipeline stage and the
	// reason the observer recorded.
	Stage  string
	Reason string
}

// NativeStaticObserveFunc runs one no-socket STATIC admission observation for an
// eligible generated static method and returns a stable decline. It NEVER opens a
// socket, RoundTrips, or serves a native result. Installed AND enabled only in a
// native worker with the umbrella flag on; nil in every default production build,
// every BAML-only worker, and every flag-off build.
type NativeStaticObserveFunc func(ctx context.Context, inv NativeStaticInvocation) NativeStaticResult

// ---------------------------------------------------------------------------
// De-BAML Slice 8C — static unary SHADOW→SERVE seam.
//
// This is the SERVING twin of NativeStaticObserveFunc: for an admitted static
// unary `/call` it actually SERVES natively — it opens exactly ONE provider
// socket, translates/extracts the response, runs the native static SAP over the
// selected Return Bundle, then runs BAML `Parse.<Method>` on the SAME bytes ONLY
// as a differential/safety compare, and returns the winning flattened canonical
// JSON. Everything crossing the boundary stays neutral: no nanollm type, no
// internal/* type; the generated /call seam decodes the returned FinalJSON into
// the method's concrete return type via the per-method DecodeNativeStaticFinal.
//
// It reuses the SAME NativeStaticInvocation the observer receives (descriptor,
// exact args, selected-child facts, BuildBAMLRequest / BAMLOnlyParse closures)
// plus IncludeReasoning + SendHeartbeat. The concrete nanollm-backed serve
// implementation is injected by the isolated SERVE worker at the binary entry
// point ONLY under the umbrella flag; a nil func — every default production build
// and every flag-off build — leaves the generated static seam's serve callback
// nil and every request byte-identical BAML.
//
// Ownership boundary (mirrors NativeServeFunc exactly): BEFORE it CLAIMS the
// native attempt, the callback may return a stable DECLINE and guarantees NO
// provider RoundTrip occurred — the generated seam then runs BAML's
// `Request.<Method>` / `Parse.<Method>` for the same call exactly as today. From
// the CLAIM onward every terminal condition is a SUCCESS or a typed FAILURE; there
// is never a hidden BAML resend after the claim, and BAML `Parse.<Method>` over
// the identical completed response is permitted as a safety comparator only — it
// may never build or send a second provider request.
//
// SENSITIVE: the SUCCESS payload (FinalJSON / Raw / Reasoning) is parsed provider
// output; treat it like the response body and never log/serialize/emit the struct
// wholesale — see the package doc.

// Bounded, secret-free winner-engine tokens for a native-served static final. They
// mirror NativeServeEngineNative / NativeServeEngineBAMLParse so a dashboard can
// tell WHICH engine produced the served final without carrying any content.
const (
	// NativeStaticServeEngineNative: native transport + native static SAP produced
	// the served final.
	NativeStaticServeEngineNative = "native"
	// NativeStaticServeEngineBAMLParse: native owned the single provider request,
	// but BAML `Parse.<Method>` (on the SAME bytes) produced the served final —
	// either because native SAP declined the shape or because native/BAML
	// structured output drifted and BAML's parse is served for safety.
	NativeStaticServeEngineBAMLParse = "native_baml_parse"
)

// NativeStaticServeDisposition is the tri-state outcome of a native static serve
// attempt, mirroring NativeServeDisposition. The zero value is
// NativeStaticServeDeclined, so a zero-valued result safely means "declined, BAML
// serves".
type NativeStaticServeDisposition uint8

const (
	// NativeStaticServeDeclined: the implementation did NOT claim a native attempt
	// and guarantees no provider socket occurred. The generated seam runs BAML's
	// `Request.<Method>` / `Parse.<Method>` for the same call exactly as today.
	// Stage/Reason are stable tokens.
	NativeStaticServeDeclined NativeStaticServeDisposition = iota
	// NativeStaticServeSucceeded: native owned the single provider request and
	// produced the served final. FinalJSON is the winning flattened canonical JSON
	// (native SAP or the same-response BAML parse); Raw/Reasoning are the owned
	// /call-with-raw channels; WinnerEngine is the bounded engine token.
	NativeStaticServeSucceeded
	// NativeStaticServeFailed: native claimed the attempt (a socket MAY have opened)
	// and then failed. Err is the typed error handed to the outer policy;
	// RawDiagnostic is the owned raw body/text retained for details.raw. The
	// generated seam NEVER falls through to a second BAML send for the same call.
	NativeStaticServeFailed
)

// NativeStaticServeResult is the neutral tri-state result of a native static serve
// attempt. Its Stage/Reason (decline) and WinnerEngine tokens are secret-free
// bounded enums, but the SUCCESS payload is SENSITIVE — FinalJSON is parsed
// provider output and Raw/Reasoning are the provider's own text; treat them like
// the response body and never log/serialize/emit the struct wholesale.
type NativeStaticServeResult struct {
	// Disposition selects which field group below is meaningful.
	Disposition NativeStaticServeDisposition

	// Declined-only: stable, secret-free stage/reason (bounded enum-like tokens;
	// never free-form text or a secret) describing WHERE/WHY native stepped aside.
	Stage  string
	Reason string

	// Succeeded-only: the winning flattened canonical JSON (native static SAP or the
	// same-response BAML parse), the owned /call-with-raw raw + reasoning channels,
	// and the bounded winner-engine token (NativeStaticServeEngineNative or
	// NativeStaticServeEngineBAMLParse). The generated /call seam decodes FinalJSON
	// into the method's concrete return type via DecodeNativeStaticFinal.
	//
	// Ownership contract: FinalJSON/Raw/Reasoning MUST NOT alias any transport
	// buffer once the callback returns — the implementation owns them outright.
	FinalJSON    []byte
	Raw          string
	Reasoning    string
	WinnerEngine string

	// Failed-only: the typed error handed to the outer policy (use existing typed
	// classes — *llmhttp.HTTPError, *buildrequest.OutputParseError, llmhttp
	// transport errors — so the outer policy behaves exactly as for a BAML attempt),
	// plus an optional owned raw diagnostic retained as details.raw.
	Err           error
	RawDiagnostic string
}

// NativeStaticServeFunc actually serves one admitted static unary `/call` natively
// (or declines pre-socket to BAML). Installed AND enabled only in a SERVE deploy
// profile with the umbrella flag on; nil in every default production build and
// every flag-off build.
type NativeStaticServeFunc func(ctx context.Context, inv NativeStaticInvocation) NativeStaticServeResult

// ---------------------------------------------------------------------------
// De-BAML Slice 8C — static unary Stage-1 SHADOW seam.
//
// This is the STAGE-1 twin of the SERVE seam and the response-comparing twin of the
// no-send OBSERVE seam: BAML remains the SOLE provider sender, and native consumes
// BAML's ALREADY-FETCHED response bytes to compare its own translate/extract/SAP/
// typed-decode against BAML's parse of the SAME bytes — with ZERO native sends.
//
// The comparator runs the full no-send admission predicate + the strict BAML
// `Request.<Method>` plan compare (like OBSERVE), then — on a full plan match —
// returns an OnResponse continuation the generated seam threads onto its declined
// NativeCallOutcome (OnResponseShadow). The orchestrator invokes OnResponse with
// BAML's fetched (status, body) AFTER BAML serves; it opens no socket, issues no
// RoundTrip, and never changes what BAML serves. It is the MEASURED shadow-parse the
// cutover gate requires — NOT inferred from a serve claim.
//
// SENSITIVE: the OnResponse bytes are provider output; the comparator records only
// bounded per-facet match/mismatch tokens, never the content — see the package doc.

// NativeStaticShadowResult is the neutral result of one static shadow comparison. The
// comparator ALWAYS declines (BAML serves): Stage/Reason are the bounded admission /
// shadow tokens, and OnResponse — non-nil ONLY when the request plan matched every
// facet — is the same-response continuation the generated seam forwards onto its
// declined NativeCallOutcome.OnResponseShadow.
type NativeStaticShadowResult struct {
	// Stage/Reason are stable, secret-free tokens (the admission stage/reason on a
	// decline, or the bounded shadow tokens) describing the routing decision.
	Stage  string
	Reason string

	// OnResponse is the same-response native-vs-BAML comparison continuation. It is
	// non-nil ONLY on a full plan match; the orchestrator invokes it EXACTLY once with
	// BAML's already-fetched status+body AFTER BAML serves. It opens no socket, issues
	// no RoundTrip, and its return value is discarded — it only records bounded
	// per-facet comparison tokens. Nil means "no same-response compare for this
	// request" (an admission/plan decline, or the comparator seam off).
	OnResponse func(ctx context.Context, status int, body []byte)
}

// NativeStaticShadowFunc runs one no-send STATIC shadow comparison for an eligible
// generated static method and ALWAYS declines so BAML serves. Installed AND enabled
// only in a SHADOW deploy profile with the umbrella flag on; nil in every default
// production build, every serve/BAML-only worker, and every flag-off build.
type NativeStaticShadowFunc func(ctx context.Context, inv NativeStaticInvocation) NativeStaticShadowResult
