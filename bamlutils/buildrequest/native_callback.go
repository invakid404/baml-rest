package buildrequest

import (
	"context"
	"errors"

	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// Native child-attempt seam (de-BAML cutover Slice 1) — NEUTRAL, HARD-OFF.
//
// This seam lets a LATER slice inject a native (non-BAML) child-attempt
// implementation into the unary orchestrator WITHOUT teaching the generic
// orchestrator anything about the native engine. Everything here is neutral:
// there is no BAML type and no nanollm/go-mocklm import. The real
// implementation is injected elsewhere (the worker, in a later slice) via
// CallConfig.NativeAttempt; every production constructor leaves that callback
// nil, so with the seam off every request path is byte-identical to today.
//
// The contract mirrors the cutover scope's tri-state ownership rules:
//
//   - declined      the callback guarantees NO provider RoundTrip/socket
//                   occurred; the orchestrator runs the existing BAML
//                   build/send for the SAME selected child in the SAME retry
//                   iteration (no extra retry consumed, no fallback advance);
//   - succeeded     the callback produced the final value over a native send;
//                   the orchestrator returns it as an ordinary attempt result
//                   carrying owned raw/reasoning and a native winner-engine
//                   marker;
//   - failed        the callback claimed/attempted a native send and then
//                   failed; a socket MAY have opened, so the typed error is
//                   terminal for that child attempt and is handed to the
//                   existing outer fallback/retry loop — the orchestrator NEVER
//                   falls through to a second BAML send for the same child.

// nativeEngineMarker is the DEFAULT winner-engine token the orchestrator records
// on an attempt result produced by the native seam when the succeeded outcome
// leaves WinnerEngine empty. It is an opaque, stable, secret-free enum value;
// bamlutils assigns it no further meaning and only carries it through to outcome
// metadata so dashboards can tell a native attempt apart from a BAML one. A
// serving implementation may override it with another bounded token (e.g.
// "native_baml_parse" when native owned the socket but BAML parse-only produced
// the final) via NativeCallOutcome.WinnerEngine.
const nativeEngineMarker = "native"

// nativeBAMLParseEngineMarker is the other declared winner-engine token: native
// owned the single provider request but BAML parse-only produced the served final.
const nativeBAMLParseEngineMarker = "native_baml_parse"

// allowedWinnerEngine folds an engine token from a (public, directly-constructible)
// native outcome to the DECLARED bounded set, defaulting anything else — including
// the empty string — to the base "native" marker. This keeps winner_engine a
// bounded enum in outcome metadata even if a mis-built callback supplies arbitrary
// text.
func allowedWinnerEngine(e string) string {
	switch e {
	case nativeEngineMarker, nativeBAMLParseEngineMarker:
		return e
	default:
		return nativeEngineMarker
	}
}

// allowedPlannedEngine folds a planned-engine token to the DECLARED bounded set,
// omitting (empty) anything but the one legal value so arbitrary input can never
// reach the planned_engine metadata label.
func allowedPlannedEngine(e string) string {
	if e == nativeEngineMarker {
		return e
	}
	return ""
}

// NativeCallDisposition is the tri-state outcome of a native child attempt.
//
// The zero value is NativeCallDeclined, so a zero-valued NativeCallOutcome
// safely means "declined, fall through to BAML". A mis-constructed result can
// never be mistaken for a success that returns a nil final value, and a
// forgotten field can never silently hide a socket behind a second send.
type NativeCallDisposition uint8

const (
	// NativeCallDeclined means the callback did NOT attempt a native send: it
	// guarantees no provider RoundTrip/socket occurred. The orchestrator runs
	// the existing BAML build/send for the SAME selected child in the SAME
	// retry iteration — no extra retry is consumed and the fallback chain does
	// not advance.
	NativeCallDeclined NativeCallDisposition = iota
	// NativeCallSucceeded means the callback produced the final result over a
	// native send. The orchestrator returns it as an ordinary attempt result,
	// carrying raw/reasoning (gated on NeedsRaw) and a native winner-engine
	// marker.
	NativeCallSucceeded
	// NativeCallFailed means the callback claimed/attempted a native send and
	// then failed. A provider socket MAY have occurred, so this is TERMINAL for
	// the child attempt: the typed error is handed to the existing outer
	// fallback/retry loop and the orchestrator never falls through to a second
	// BAML send for the same child.
	NativeCallFailed
)

// NativeDeclineStage is a stable, secret-free identifier for the layer at which
// a native attempt declined (e.g. a capability / flag / provider / plan gate).
// It is a bounded enum token supplied by the injected implementation; bamlutils
// assigns no meaning to specific values and only carries them for
// observability. It must never be free-form text or carry a secret.
type NativeDeclineStage string

// NativeDeclineReason is a stable, secret-free reason code paired with a
// NativeDeclineStage. Like the stage it is a bounded enum token, never
// free-form text and never a secret.
type NativeDeclineReason string

// NativeCallAttempt is the neutral, secret-free context handed to a native
// child-attempt callback. It names the concrete leaf baml-rest has already
// selected plus the response-shaping flags the attempt needs; it deliberately
// carries no BAML or nanollm types. Threaded via CallConfig (never via HTTP
// headers and never via global per-request state).
type NativeCallAttempt struct {
	// Provider is the resolved provider for the selected child (e.g.
	// "openai"). Legacy children never reach the callback, so this is always a
	// BuildRequest-route provider.
	Provider string
	// ClientOverride is the concrete selected child/leaf client name — the same
	// value threaded into the BAML buildRequest for this attempt — or empty for
	// a single default-client request.
	ClientOverride string
	// NeedsRaw mirrors CallConfig.NeedsRaw: whether the caller wants raw
	// response text (the /call-with-raw endpoint).
	NeedsRaw bool
	// IncludeReasoning mirrors CallConfig.IncludeReasoning: whether the caller
	// opted into surfacing provider reasoning/thinking text.
	IncludeReasoning bool
	// OutputSchema is an opaque, engine-specific handle to the method's output
	// schema, forwarded verbatim from CallConfig.NativeOutputSchema. The
	// generic orchestrator never inspects it. Nil unless a caller installs a
	// native implementation.
	OutputSchema any
	// BuildBAMLRequest builds BAML's request plan for THIS selected child WITHOUT
	// sending — the same per-attempt build closure the orchestrator would use for
	// the send, pre-bound to this attempt's clientOverride. It is the neutral
	// resolution of the cutover CRUX: a shadow comparator obtains BAML's built
	// plan for the same child to compare against the native plan, without opening
	// a socket, issuing a second provider request, or importing nanollm into the
	// generic orchestrator. It opens NO socket; the returned *llmhttp.Request is
	// a built-but-unsent plan (method/URL/headers/body). Non-nil only while the
	// seam is on.
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)
	// SendHeartbeat is the neutral first-2xx liveness signal, the native twin
	// of the onSuccess callback the BAML path hands to llmhttp on the same
	// attempt. The native transport MUST call it the instant it reads 2xx
	// response headers — before it buffers the body — so the pool's hung
	// detector observes liveness while a large/slow body is still streaming and
	// does not judge the worker hung (which would trigger a retry/restart and
	// risk a duplicate provider request after the socket already opened).
	//
	// It is idempotent and best-effort: the orchestrator emits at most one
	// heartbeat frame regardless of how many times it is called, and drops the
	// signal if the output channel is momentarily full. Non-nil only while the
	// seam is on; a callback need not guard against nil in practice, but calling
	// it more than once is always safe.
	SendHeartbeat func()
}

// NativeCallOutcome is the stable, secret-free tri-state result of a native
// child attempt. Prefer the DeclineNativeCall / SucceedNativeCall /
// FailNativeCall constructors over a hand-built literal so the disposition and
// its payload always agree.
type NativeCallOutcome struct {
	// Disposition selects which of the three field groups below is meaningful.
	Disposition NativeCallDisposition

	// Declined-only: a stable, secret-free stage/reason describing WHERE and WHY
	// the native path stepped aside. Bounded enum-like tokens (never free-form
	// text or secrets); the orchestrator only forwards them for observability.
	DeclineStage  NativeDeclineStage
	DeclineReason NativeDeclineReason

	// Declined-only: the optional SAME-response shadow continuation. When set on a
	// DECLINED outcome, the orchestrator — after it runs the ordinary BAML
	// build/send for this child and the provider returns 2xx — hands BAML's
	// ALREADY-FETCHED status + raw body to this callback for a no-transport
	// native-vs-BAML response-parity comparison. It NEVER RoundTrips, issues a
	// second provider request, or influences the served result: BAML's envelope is
	// returned byte-identical regardless. It runs at most once per attempt, only on
	// a 2xx BAML send (a non-2xx surfaces as *HTTPError before this fires), and the
	// orchestrator guards it so a panic in the shadow oracle can never fail an
	// otherwise BAML-served request. Nil (every production build, and even in the
	// shadow profile when the request-plan comparison did not match) means no
	// response parity runs and the path is byte-identical to today.
	//
	// SYNCHRONOUS BY DESIGN — the orchestrator invokes this inline on the attempt
	// path, so a shadowed 2xx attempt pays the comparator's CFFI parity cost before
	// the served response is emitted. This is deliberate and correct for the SHADOW
	// deploy profile, which is a temporary rollout stage, NOT a latency-neutral
	// production path: the cutover scope accepts this parity CPU/latency and removes
	// it "by deployment stage" (stop deploying the shadow profile), not by async
	// plumbing. Detached/off-path execution is deliberately avoided: the concrete
	// callback's BAML-only parse closure captures the per-request generated adapter
	// and drives the BAML CFFI runtime, both of which are recycled/reused once the
	// serving goroutine returns — running it after the response is served would risk
	// a use-after-free / concurrent-CFFI race and make the parity metric
	// nondeterministic. A later canary slice may revisit off-path parity once the
	// oracle no longer depends on per-request adapter state.
	//
	// SENSITIVE: body is the provider's raw response bytes. The orchestrator hands
	// them straight through; a callback must treat them as read-only and never log
	// or emit them (only redacted, secret-free views).
	OnResponseShadow func(ctx context.Context, status int, body []byte)

	// Succeeded-only: the generated final value plus owned raw/reasoning text.
	//
	// Ownership contract: these strings MUST NOT alias any transport buffer once
	// the callback returns — the callback owns them outright, exactly as the
	// BAML path clones raw/reasoning out of borrowed storage before releasing
	// it. The orchestrator therefore carries them across the attempt boundary
	// without an extra copy.
	//
	// On a FAILED outcome, Raw is reused as the owned raw DIAGNOSTIC (the raw
	// provider body or extracted assistant text) so a translate/extract/SAP
	// failure retains details.raw through retry.Execute; see FailNativeCallWithRaw.
	FinalResult any
	Raw         string
	Reasoning   string

	// Succeeded-only: an optional bounded, secret-free winner-engine token that
	// OVERRIDES the default nativeEngineMarker on the recorded outcome metadata.
	// Empty means the default ("native"); a serving implementation sets it to
	// another bounded token (e.g. "native_baml_parse" when native owned the one
	// provider request but BAML parse-only produced the served final). Never
	// free-form text or a secret.
	WinnerEngine string

	// Failed-only: the typed error handed to the outer fallback/retry loop. Use
	// existing typed error classes (e.g. llmhttp transport/HTTP errors, output
	// parse errors) so the outer policy behaves exactly as it does for a BAML
	// attempt. When Raw is non-empty on a failed outcome the orchestrator wraps
	// this error so the raw survives the retry boundary as details.raw.
	Err error
}

// errNativeCallFailedNil backstops a NativeCallFailed outcome built with a nil
// error. Returning (nil, nil) from the attempt would be read as success with a
// nil final value, so FailNativeCall substitutes this sentinel to keep the
// failure observable.
var errNativeCallFailedNil = errors.New("buildrequest: native call attempt failed (no error provided)")

// DeclineNativeCall builds a declined outcome. A decline asserts no provider
// socket occurred, so the orchestrator continues with the existing BAML
// build/send for the same child in the same retry iteration. Stage and reason
// are stable, secret-free tokens carried only for observability.
func DeclineNativeCall(stage NativeDeclineStage, reason NativeDeclineReason) NativeCallOutcome {
	return NativeCallOutcome{
		Disposition:   NativeCallDeclined,
		DeclineStage:  stage,
		DeclineReason: reason,
	}
}

// SucceedNativeCall builds a succeeded outcome carrying the native final value
// plus owned raw/reasoning text (see NativeCallOutcome's ownership contract).
func SucceedNativeCall(finalResult any, raw, reasoning string) NativeCallOutcome {
	return NativeCallOutcome{
		Disposition: NativeCallSucceeded,
		FinalResult: finalResult,
		Raw:         raw,
		Reasoning:   reasoning,
	}
}

// FailNativeCall builds a failed outcome. Use it once a native send may have
// opened a socket: the error is terminal for the child attempt and the
// orchestrator never falls through to a second BAML send. A nil err is replaced
// with a generic sentinel so the failure can never be silently swallowed.
func FailNativeCall(err error) NativeCallOutcome {
	if err == nil {
		err = errNativeCallFailedNil
	}
	return NativeCallOutcome{
		Disposition: NativeCallFailed,
		Err:         err,
	}
}

// FailNativeCallWithRaw is FailNativeCall carrying an owned raw diagnostic (the
// raw provider body, or the extracted assistant text on a parse failure) so a
// post-claim translate/extract/SAP failure retains details.raw through
// retry.Execute — the orchestrator wraps the error with the raw at the failed
// arm exactly as the BAML build/send path wraps its own extraction/parse errors.
// An empty raw is equivalent to FailNativeCall.
func FailNativeCallWithRaw(err error, raw string) NativeCallOutcome {
	o := FailNativeCall(err)
	o.Raw = raw
	return o
}

// NativeCallAttemptFunc is the optional native child-attempt callback. When
// installed AND enabled (see CallConfig.NativeAttempt / NativeAttemptEnabled),
// the unary orchestrator invokes it as the FIRST operation for each selected
// non-legacy child, before any BAML build/send, and dispatches on the returned
// NativeCallOutcome per the disposition contract.
//
// The callback MUST honour that contract precisely: it may only return
// NativeCallDeclined when it can guarantee no provider socket occurred. Any
// other terminal condition after a possible socket must be NativeCallFailed so
// the orchestrator never issues a hidden second same-child request.
//
// This is the seam a later slice fills with the nanollm-backed implementation;
// the generic orchestrator stays BAML- and nanollm-neutral.
type NativeCallAttemptFunc func(ctx context.Context, attempt NativeCallAttempt) NativeCallOutcome
