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
	// decoder has a stable neutral home, but is UNUSED in 8B — the observer never
	// serves a native final, so it never decodes one. A later serving slice (8C)
	// invokes it after an admitted native RoundTrip. Nil in 8B callers.
	DecodeNativeFinal func(canonicalJSON []byte) (any, error)
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
