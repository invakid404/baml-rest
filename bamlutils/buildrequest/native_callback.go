package buildrequest

import (
	"context"
	"errors"
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

// nativeEngineMarker is the winner-engine token the orchestrator records on an
// attempt result produced by the native seam. It is an opaque, stable,
// secret-free enum value; bamlutils assigns it no further meaning and only
// carries it through to outcome metadata so dashboards can tell a native
// attempt apart from a BAML one.
const nativeEngineMarker = "native"

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

	// Succeeded-only: the generated final value plus owned raw/reasoning text.
	//
	// Ownership contract: these strings MUST NOT alias any transport buffer once
	// the callback returns — the callback owns them outright, exactly as the
	// BAML path clones raw/reasoning out of borrowed storage before releasing
	// it. The orchestrator therefore carries them across the attempt boundary
	// without an extra copy.
	FinalResult any
	Raw         string
	Reasoning   string

	// Failed-only: the typed error handed to the outer fallback/retry loop. Use
	// existing typed error classes (e.g. llmhttp transport/HTTP errors, output
	// parse errors) so the outer policy behaves exactly as it does for a BAML
	// attempt.
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
