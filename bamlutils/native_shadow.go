package bamlutils

import (
	"context"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// Native one-send SHADOW comparison seam (de-BAML cutover Slice 4) — NEUTRAL.
//
// This is the injection point a SHADOW deploy profile uses to run a no-socket
// native-vs-BAML request-plan comparison for an admitted unary `_dynamic` call,
// then serve BAML unchanged. Everything here is neutral: no nanollm type crosses
// the boundary, and the generic orchestrator stays BAML- and nanollm-free.
//
// The seam mirrors the DeBAMLRenderFunc / DeBAMLParseFunc pattern: the generated
// dynclient adapter (a separate module that cannot import baml-rest's internal
// native packages, let alone the out-of-go.work nanollm bridge) only ever calls
// this public-typed callback; the concrete nanollm-backed comparator is injected
// by the worker at the shadow binary's entry point. A nil func — every DEFAULT
// production build — leaves the orchestrator's native child-attempt callback nil
// and every request byte-identical to today.
//
// The comparator ALWAYS declines to BAML (it never serves a native result in this
// slice): it builds the native plan, obtains BAML's built plan for the SAME
// selected child WITHOUT sending, records a field-level plan_compare result (NO
// values), and returns a stable, secret-free decline. BAML stays authoritative
// and the user-facing envelope is BAML's, byte-identical. A plan mismatch is an
// OBSERVABLE, non-user-facing routing signal, never a user-visible error.

// NativeShadowMode is the bounded request-mode label handed to the shadow
// comparator. Only the unary call modes reach the seam (streaming never routes
// through the non-streaming orchestrator); the set is fixed so any downstream
// metric mode stays bounded.
type NativeShadowMode string

const (
	// NativeShadowModeCall is the plain unary `call`.
	NativeShadowModeCall NativeShadowMode = "call"
	// NativeShadowModeCallWithRaw is `call-with-raw`; its native response fields
	// are not yet proven, so the comparator declines it at the mode gate — it is
	// carried only so the mode label is accurate.
	NativeShadowModeCallWithRaw NativeShadowMode = "call_with_raw"
)

// NativeShadowRequest is the neutral, secret-free request description the
// generated adapter hands the injected shadow comparator. It names the concrete
// leaf baml-rest already selected plus the request-wide facts the native
// admission predicate needs, and carries BuildBAMLRequest — the closure that
// builds BAML's request plan for the SAME child WITHOUT opening a socket.
//
// It deliberately carries NO nanollm type. Registry/Messages/OutputSchema are the
// same effective one-client registry, generated dynamic messages, and dynamic
// output schema BAML built its request from; the comparator re-derives the native
// plan from them and compares against BuildBAMLRequest's output.
//
// SENSITIVE: Registry embeds the real api_key and BuildBAMLRequest returns a plan
// carrying the real bearer Authorization + request body. Neither this struct nor
// anything derived from it may be logged, serialized, or emitted — only redacted,
// secret-free views (header NAMES, body length/hash, stable stage/reason).
type NativeShadowRequest struct {
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
	Mode NativeShadowMode

	// SingleLeaf reports the orchestration plan resolves exactly one leaf.
	SingleLeaf bool
	// HasFallbackChain / HasRoundRobin / HasRequestRetryOverride mark the
	// whole-orchestration-plan shapes the initial native matrix does not prove; any
	// of them declines at the strategy gate BEFORE a native plan is built, BAML's
	// plan is obtained, or a plan_compare is recorded.
	//
	// These MUST be TRUTHFUL: they are the parity-decline that keeps the shadow
	// honest. HasRequestRetryOverride is set when the request carries a retry
	// policy the single-attempt exact lane would bypass.
	HasFallbackChain        bool
	HasRoundRobin           bool
	HasRequestRetryOverride bool

	// WouldRewriteOrProxy reports, for the request's EFFECTIVE send target, whether
	// the effective llmhttp client would rewrite the outbound URL or route it
	// through an HTTP proxy at EXECUTION time — AFTER the plan the comparator
	// captured is built — either of which could otherwise record a false native/BAML
	// "match" while BAML actually sends elsewhere. It is the effective send client's
	// llmhttp.Client.WouldRewriteOrProxy, evaluated by the admission predicate
	// against the effective target it resolves (base_url + /chat/completions). The
	// proxy decision is EXACT for the tuned default transport's URL-only
	// http.ProxyFromEnvironment (its own cached resolver against the REAL target,
	// provably equivalent to the actual send); a caller-supplied Proxy resolver that
	// could inspect other request fields FAILS CLOSED. A true result declines at the
	// strategy gate before a native plan is prepared, BAML's plan is obtained, or a
	// plan_compare is recorded. Nil in lightweight callers that carry no send client.
	WouldRewriteOrProxy func(effectiveURL string) bool

	// BuildBAMLRequest builds BAML's request plan for THIS selected child WITHOUT
	// sending — the same closure the orchestrator uses to build the request it
	// then sends. The comparator calls it to obtain BAML's plan for comparison; it
	// opens NO socket. It is the orchestrator's per-attempt build closure, threaded
	// through from NativeCallAttempt.
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)
}

// NativeShadowResult is the neutral, secret-free outcome of a shadow comparison.
// The comparator always declines to BAML; Stage/Reason are stable, bounded
// enum-like tokens describing WHERE/WHY (an admission decline layer, or the
// terminal shadow-served-BAML disposition). They are carried only for
// observability and must never be free-form text or a secret.
type NativeShadowResult struct {
	Stage  string
	Reason string
}

// NativeShadowFunc runs one no-socket native-vs-BAML request-plan shadow
// comparison and returns a stable decline. Installed AND enabled only in a shadow
// deploy profile with the umbrella flag on; nil in every default production build.
type NativeShadowFunc func(ctx context.Context, req NativeShadowRequest) NativeShadowResult
