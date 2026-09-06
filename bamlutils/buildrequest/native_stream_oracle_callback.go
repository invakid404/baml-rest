package buildrequest

import (
	"context"
	"errors"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// ExecBridge-U1s / M3e-B — the ORACLE-OWNED native STREAM child-attempt seam. NEUTRAL,
// HARD-OFF, and DELIBERATELY DISTINCT from the legacy [NativeStreamAttemptFunc] seam next
// door.
//
// WHY A SECOND SEAM RATHER THAN A FLAG ON THE FIRST. The two seams differ in WHO OWNS THE
// PARSE, which is not something a boolean can express safely:
//
//   - The legacy seam owns only TRANSPORT. It drives EmitDelta, and the ORCHESTRATOR then
//     accumulates, runs StreamConfig.NativeParseStream on each throttled tick, and runs
//     NativeParseFinal at the end.
//   - This seam owns the WHOLE claimed stream: cadence, per-prefix oracle, emission, and
//     the final. Every event it delivers through EmitResolved has ALREADY been resolved
//     against BAML's parse of the same accumulated prefix, and the final it returns has
//     already been resolved too.
//
// Overloading one completion mode for both would invite exactly the two bugs this split
// prevents: the orchestrator parsing an already-parsed partial a second time, and the
// legacy error-SWALLOWING cadence (CadenceParseErrorsAreNoEvent) silently absorbing an
// oracle failure that must be terminal. The orchestrator therefore FAILS CLOSED when both
// seams are installed at once, rather than picking one.
//
// Ownership contract, identical to the legacy seam's and not relaxed by the oracle:
//
//   - declined          no provider socket and NO public event occurred; the orchestrator
//     runs the existing BAML build/send for the SAME child in the SAME
//     retry iteration, exactly once;
//   - completed         the claimed stream reached a valid terminal condition, every
//     resolved partial was already delivered through EmitResolved, and the
//     ALREADY-ORACLED final rides on the outcome;
//   - failedAfterClaim  the transport was CLAIMED and then failed; TERMINAL for the whole
//     stream — no BAML send/parse, no retry, no fallback child, no pool
//     replay, no continuation reset.

// NativeStreamOracleAttempt is the neutral, secret-free context handed to an oracle-owned
// native stream child attempt. It names the concrete leaf baml-rest already selected plus
// the streaming-shaping flags, the per-child BAML no-send plan closure, the RESOLVED-event
// sink, and the liveness callbacks. It carries no BAML or nanollm type.
type NativeStreamOracleAttempt struct {
	// Provider is the resolved provider for the selected child (e.g. "openai").
	Provider string
	// ClientOverride is the concrete selected child/leaf client name threaded into the BAML
	// buildRequest for this attempt, or empty for a single default-client request.
	ClientOverride string
	// NeedsPartials / NeedsRaw / IncludeReasoning mirror StreamConfig.
	NeedsPartials    bool
	NeedsRaw         bool
	IncludeReasoning bool
	// BuildBAMLRequest builds BAML's StreamRequest plan for THIS selected child WITHOUT
	// sending — the same per-attempt build closure the orchestrator would use for the send,
	// pre-bound to this attempt's clientOverride. The oracle implementation compares it
	// against the native plan as a PRE-CLAIM precondition; it opens NO socket. On a DECLINE
	// the orchestrator still runs buildRequest(ctx, clientOverride) itself.
	BuildBAMLRequest func(ctx context.Context) (*llmhttp.Request, error)
	// EmitResolved is the SYNCHRONOUS sink for one ALREADY-ORACLED public event: the
	// implementation has already run both engines over the exact accumulated prefix and
	// decided what (if anything) to release. The orchestrator only DELIVERS it, preserving
	// the shared drop-on-full, cancellation and sawStreamFrame semantics — it never parses
	// it again. Returning an error asks the implementation to STOP immediately (a terminal
	// FailedAfterClaim, never a retry). Non-nil only while the seam is on.
	EmitResolved func(bamlutils.NativeSpineStreamEvent) error
	// SendHeaders is the idempotent first-2xx liveness signal so the pool's hung detector
	// sees liveness on a slow body; SendFirstBody is the idempotent first-raw-body-byte
	// metric/liveness signal. Both best-effort, non-nil only while the seam is on.
	SendHeaders   func()
	SendFirstBody func()
}

// NativeStreamOracleOutcome is the stable, secret-free tri-state result of an oracle-owned
// native stream child attempt. It reuses [NativeStreamDisposition] because the ownership
// meaning is identical; the payload differs because this seam returns the FINAL it already
// resolved. Prefer the constructors below over a hand-built literal so the disposition and
// its payload always agree.
type NativeStreamOracleOutcome struct {
	Disposition NativeStreamDisposition

	// Declined-only: bounded, secret-free tokens describing WHERE and WHY the native path
	// stepped aside. The orchestrator only forwards them for observability.
	DeclineStage  NativeDeclineStage
	DeclineReason NativeDeclineReason

	// Completed-only: the ALREADY-ORACLED typed final, the accumulated raw/reasoning
	// channels, and the bounded winner-engine token. The orchestrator emits the final
	// verbatim — it MUST NOT re-parse the accumulated text, which would discard the
	// oracle's decision.
	Final        any
	Raw          string
	Reasoning    string
	WinnerEngine string

	// FailedAfterClaim-only: the typed public error handed to the terminal path, and an
	// optional owned raw diagnostic retained as details.raw.
	Err           error
	RawDiagnostic string
}

// DeclineNativeStreamOracle builds a declined outcome. A decline asserts NO provider socket
// and NO EmitResolved occurred, so the orchestrator continues with the existing BAML
// build/send for the same child in the same retry iteration.
func DeclineNativeStreamOracle(stage NativeDeclineStage, reason NativeDeclineReason) NativeStreamOracleOutcome {
	return NativeStreamOracleOutcome{
		Disposition:   NativeStreamDeclined,
		DeclineStage:  stage,
		DeclineReason: reason,
	}
}

// CompleteNativeStreamOracle builds a completed outcome carrying the already-oracled final,
// the accumulated channels, and the bounded winner-engine token.
func CompleteNativeStreamOracle(final any, raw, reasoning, winnerEngine string) NativeStreamOracleOutcome {
	return NativeStreamOracleOutcome{
		Disposition:  NativeStreamCompleted,
		Final:        final,
		Raw:          raw,
		Reasoning:    reasoning,
		WinnerEngine: winnerEngine,
	}
}

// errNativeStreamOracleFailedNil backstops a FailedAfterClaim outcome built with a nil
// error, keeping the terminal failure observable rather than reading as success.
var errNativeStreamOracleFailedNil = errors.New("buildrequest: oracle-owned native stream attempt failed after claim (no error provided)")

// FailNativeStreamOracleAfterClaim builds a terminal failed outcome. A nil err is replaced
// with a generic sentinel so the failure can never be silently swallowed; raw is an optional
// owned diagnostic retained as details.raw.
func FailNativeStreamOracleAfterClaim(err error, raw string) NativeStreamOracleOutcome {
	if err == nil {
		err = errNativeStreamOracleFailedNil
	}
	return NativeStreamOracleOutcome{
		Disposition:   NativeStreamFailedAfterClaim,
		Err:           err,
		RawDiagnostic: raw,
	}
}

// NativeStreamOracleAttemptFunc is the optional oracle-owned native stream child-attempt
// callback. When installed AND enabled (see StreamConfig.NativeOracleAttempt /
// NativeOracleAttemptEnabled) the orchestrator invokes it as the FIRST operation for a
// selected non-legacy, non-bedrock stream child, before any BAML build/send, and dispatches
// on the returned outcome per the disposition contract above.
//
// The callback MUST honour that contract precisely: it may return NativeStreamDeclined ONLY
// when it can guarantee no provider socket and no EmitResolved occurred. Any other terminal
// condition after a possible socket must be NativeStreamFailedAfterClaim, so the
// orchestrator never issues a hidden resend or replays a claimed native stream.
type NativeStreamOracleAttemptFunc func(ctx context.Context, attempt NativeStreamOracleAttempt) NativeStreamOracleOutcome
