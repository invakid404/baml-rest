package buildrequest

import (
	"context"
	"errors"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// Native STREAM child-attempt seam (de-BAML Phase 7D) — NEUTRAL, HARD-OFF.
//
// This seam lets the serve profile inject a native (non-BAML) streaming
// child-attempt implementation into RunStreamOrchestration WITHOUT teaching the
// generic orchestrator anything about the native engine. Everything here is
// neutral: no nanollm import, and only bamlutils.NativeStreamDelta (a plain text
// triple) crosses to the transport. The real implementation is injected elsewhere
// (the worker) via StreamConfig.NativeAttempt; every production constructor leaves
// that callback nil, so with the seam off every streamed request is byte-identical
// to today (I1 hard-off identity).
//
// The contract mirrors the streaming ownership rules (scope §4/§5.3):
//
//   - declined          the callback guarantees NO provider RoundTrip/socket and
//                       NO EmitDelta occurred; the orchestrator runs the existing
//                       BAML build/send for the SAME selected child in the SAME
//                       retry iteration (pre-transport only, I2);
//   - completed         the transport reached a valid terminal condition and every
//                       partial was emitted through EmitDelta; the orchestrator
//                       still owns the FINAL parse + emission and marks
//                       winner_engine=native;
//   - failedAfterClaim  the transport was CLAIMED (a socket may have opened) and
//                       then failed; the typed error is TERMINAL for the whole
//                       stream — the orchestrator emits the terminal error frame
//                       and NEVER falls back to a BAML send/parse, retries, advances
//                       a fallback child, replays through the pool, or emits a
//                       continuation reset (I4).

// NativeStreamDisposition is the tri-state outcome of a native stream child
// attempt. The zero value is NativeStreamDeclined, so a zero-valued
// NativeStreamOutcome safely means "declined, fall through to BAML pre-transport".
type NativeStreamDisposition uint8

const (
	// NativeStreamDeclined means the callback did NOT claim the transport: no
	// provider socket, no EmitDelta. The orchestrator runs the existing BAML
	// build/send for the SAME selected child in the SAME retry iteration — no extra
	// retry consumed and the fallback chain does not advance.
	NativeStreamDeclined NativeStreamDisposition = iota
	// NativeStreamCompleted means the native transport streamed to a valid terminal
	// condition. Every partial was already emitted through the orchestrator's
	// EmitDelta sink; the orchestrator runs the native-only FINAL parse over the
	// accumulated text and emits the single final result with winner_engine=native.
	NativeStreamCompleted
	// NativeStreamFailedAfterClaim means the native transport was claimed and then
	// failed. A provider socket MAY have opened, so this is TERMINAL for the stream:
	// the typed error is wrapped in a terminal carrier that bypasses retry.Execute,
	// the fallback loop, and the pool replay owner — the orchestrator emits the
	// terminal error frame with no reset and never resends through BAML.
	NativeStreamFailedAfterClaim
)

// NativeStreamAttempt is the neutral, secret-free context handed to a native
// stream child-attempt callback. It names the concrete leaf baml-rest already
// selected plus the streaming-shaping flags, the per-child BAML plan oracle, and
// the live EmitDelta / liveness callbacks the transport drives. It carries no BAML
// or nanollm types.
type NativeStreamAttempt struct {
	// Provider is the resolved provider for the selected child (e.g. "openai").
	Provider string
	// ClientOverride is the concrete selected child/leaf client name threaded into
	// the BAML buildRequest for this attempt, or empty for a single default-client
	// request.
	ClientOverride string
	// NeedsPartials / NeedsRaw / IncludeReasoning mirror StreamConfig.
	NeedsPartials    bool
	NeedsRaw         bool
	IncludeReasoning bool
	// OutputSchema is an opaque, engine-specific handle to the method's output
	// schema, forwarded verbatim from StreamConfig.NativeOutputSchema. The generic
	// orchestrator never inspects it. Nil unless a caller installs a native
	// implementation.
	OutputSchema any
	// BuildBAMLRequest builds BAML's request plan for THIS selected child WITHOUT
	// sending — the same per-attempt build closure the orchestrator would use for
	// the send, pre-bound to this attempt's clientOverride. The native
	// implementation compares it against the native plan as a pre-transport
	// precondition. It opens NO socket. Non-nil only while the seam is on.
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)
	// EmitDelta is the SYNCHRONOUS owned-delta sink the orchestrator supplies. The
	// native transport calls it once per nonempty normalized chunk; the orchestrator
	// accumulates, runs the throttled native-only ParseStream, and emits the
	// ordinary partial. Returning an error asks the transport to STOP (a terminal
	// FailedAfterClaim). Non-nil only while the seam is on.
	EmitDelta func(bamlutils.NativeStreamDelta) error
	// SendHeaders is the idempotent first-2xx liveness signal (the native twin of
	// the BAML path's onSuccess heartbeat): the transport fires it the instant it
	// reads 2xx response headers so the pool's hung detector sees liveness on a slow
	// body. Idempotent + best-effort. Non-nil only while the seam is on.
	SendHeaders func()
	// SendFirstBody is the idempotent first-raw-body-byte metric/liveness signal.
	// Non-nil only while the seam is on.
	SendFirstBody func()
}

// NativeStreamOutcome is the stable, secret-free tri-state result of a native
// stream child attempt. Prefer the DeclineNativeStream / CompleteNativeStream /
// FailNativeStreamAfterClaim constructors over a hand-built literal so the
// disposition and its payload always agree.
type NativeStreamOutcome struct {
	// Disposition selects which of the field groups below is meaningful.
	Disposition NativeStreamDisposition

	// Declined-only: a stable, secret-free stage/reason describing WHERE and WHY the
	// native path stepped aside. Bounded enum-like tokens (never free-form text or
	// secrets); the orchestrator only forwards them for observability.
	DeclineStage  NativeDeclineStage
	DeclineReason NativeDeclineReason

	// Completed-only: the bounded, secret-free winner-engine token recorded on the
	// outcome metadata (folded to the DECLARED set; empty means the base "native"
	// marker).
	WinnerEngine string

	// FailedAfterClaim-only: the typed public error handed to the terminal path, and
	// an optional owned raw diagnostic retained as details.raw.
	Err           error
	RawDiagnostic string
}

// DeclineNativeStream builds a declined outcome. A decline asserts no provider
// socket and no EmitDelta occurred, so the orchestrator continues with the
// existing BAML build/send for the same child in the same retry iteration.
func DeclineNativeStream(stage NativeDeclineStage, reason NativeDeclineReason) NativeStreamOutcome {
	return NativeStreamOutcome{
		Disposition:   NativeStreamDeclined,
		DeclineStage:  stage,
		DeclineReason: reason,
	}
}

// CompleteNativeStream builds a completed outcome carrying the bounded
// winner-engine token. The orchestrator owns the final parse + emission.
func CompleteNativeStream(winnerEngine string) NativeStreamOutcome {
	return NativeStreamOutcome{
		Disposition:  NativeStreamCompleted,
		WinnerEngine: winnerEngine,
	}
}

// errNativeStreamFailedNil backstops a FailedAfterClaim outcome built with a nil
// error, keeping the terminal failure observable rather than reading as success.
var errNativeStreamFailedNil = errors.New("buildrequest: native stream attempt failed after claim (no error provided)")

// FailNativeStreamAfterClaim builds a terminal failed outcome. Use it once the
// native transport may have opened a socket: the error is terminal for the whole
// stream and the orchestrator never falls through to a BAML send/parse, retries,
// advances a fallback child, or replays through the pool. A nil err is replaced
// with a generic sentinel so the failure can never be silently swallowed. raw is
// an optional owned diagnostic retained as details.raw.
func FailNativeStreamAfterClaim(err error, raw string) NativeStreamOutcome {
	if err == nil {
		err = errNativeStreamFailedNil
	}
	return NativeStreamOutcome{
		Disposition:   NativeStreamFailedAfterClaim,
		Err:           err,
		RawDiagnostic: raw,
	}
}

// NativeStreamAttemptFunc is the optional native stream child-attempt callback.
// When installed AND enabled (see StreamConfig.NativeAttempt /
// NativeAttemptEnabled) the orchestrator invokes it as the FIRST operation for a
// selected non-legacy, non-bedrock stream child, before any BAML build/send, and
// dispatches on the returned NativeStreamOutcome per the disposition contract.
//
// The callback MUST honour that contract precisely: it may only return
// NativeStreamDeclined when it can guarantee no provider socket / no EmitDelta
// occurred. Any other terminal condition after a possible socket must be
// NativeStreamFailedAfterClaim so the orchestrator never issues a hidden resend or
// replays a claimed native stream.
type NativeStreamAttemptFunc func(ctx context.Context, attempt NativeStreamAttempt) NativeStreamOutcome

// nativeStreamTerminalError marks a post-transport-claim native stream failure as
// TERMINAL: it must NOT be retried by retry.Execute, must NOT advance a fallback
// child, and must NOT be replayed by the pool (scope §4 I4). It implements the
// retry package's terminal-abort interface (RetryTerminal) so retry.Execute
// returns it immediately without consuming another attempt or firing the reset
// callback, and it unwraps to the underlying typed cause so the outer error
// emission classifies it exactly like a BAML terminal (errors.Is/As reach the
// preserved *llmhttp.HTTPError / typed stream error / context error).
type nativeStreamTerminalError struct {
	err error
}

func (e *nativeStreamTerminalError) Error() string {
	return "buildrequest: native stream failed after transport claim: " + e.err.Error()
}

func (e *nativeStreamTerminalError) Unwrap() error { return e.err }

// RetryTerminal reports that this error must never be retried. retry.Execute
// checks for this via errors.As so the terminal survives any rawCarryingError
// wrap.
func (e *nativeStreamTerminalError) RetryTerminal() bool { return true }

// newNativeStreamTerminalError wraps err as a terminal native-stream failure,
// additionally attaching raw so details.raw survives the outer emission. A nil err
// is replaced with the generic sentinel so the terminal is never silently lost.
func newNativeStreamTerminalError(err error, raw string) error {
	if err == nil {
		err = errNativeStreamFailedNil
	}
	return newRawError(&nativeStreamTerminalError{err: err}, raw)
}

// isNativeStreamTerminal reports whether err is (or wraps) a native-stream
// post-claim terminal failure. The orchestrator's fallback loop uses it as
// defense-in-depth to break out immediately rather than advancing to the next
// child — even though native admission declines fallback chains pre-transport, so
// a claimed native stream can never actually arise inside a fallback iteration.
func isNativeStreamTerminal(err error) bool {
	var t *nativeStreamTerminalError
	return errors.As(err, &t)
}
