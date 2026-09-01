package bamlutils

import (
	"context"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// ExecBridge-U1 — the NEUTRAL codegen-spine unary binding + executor contract.
//
// These are the passive, BAML-free, CFFI-free types the emitted hermetic native
// method package and the production spine executor exchange. They are the whole
// public surface the boundary crosses: the emitted module imports ONLY
// context/bamlutils/bamlutils/promptdescriptor/stdlib, so it names these types and
// nothing from nativeserve or internal/*, and the production executor (in
// nativeserve) implements [NativeSpineUnaryExecutor] over the existing native
// render/prepare/exact-send/parse machinery WITHOUT any generated BAML or CFFI on
// the emitted/runtime path.
//
// The tri-state disposition is the Phase-7 claim discipline made neutral: a
// pre-socket DECLINE guarantees zero RoundTrips/events and is fallback-legal; a
// SUCCESS or a FAILED-AFTER-CLAIM is terminal and never resends (never a hidden
// BAML/oracle re-run for the same call). Any oracle/fallback is an OUTER injected
// composite executor that intercepts ONLY a matched pre-socket decline; the emitted
// module never knows about it.
//
// SENSITIVE: a [NativeSpineUnaryBinding]'s projected argument vector and the
// [NativeSpineUnaryResult].Final carrier are real request input / parsed provider
// output — never log, %v-format, error-wrap, or metric-label them. Only the bounded
// Stage/Reason tokens and the disposition are safe to emit.

// NativeSpineUnaryBinding is the neutral per-method registration the emitted native
// package hands the production runtime. Both closures are resolved at REGISTRATION
// time (non-nil) and are reflection-free / BAML-free / CFFI-free:
//
//   - ProjectInput lowers the emitted typed input carrier into the ordered projected
//     argument vector (exact type assertions, direct scalar fields, canonical BAML
//     names as literals — no reflection, JSON round-trip, or map iteration).
//   - DecodeFinal strictly decodes the native canonical JSON into the emitted output
//     carrier via the proven bamlutils.DecodeStatic*Final core.
type NativeSpineUnaryBinding struct {
	Method       string
	ProjectInput func(input any) ([]promptdescriptor.ArgumentValue, error)
	DecodeFinal  func(canonicalJSON []byte) (any, error)
}

// NativeSpineUnaryDisposition is the tri-state outcome of a spine unary Call,
// mirroring the Phase-7 static-stream tri-state. The zero value is
// NativeSpineDeclinedPreSocket, so a zero-valued result safely means "declined
// before any socket, BAML/fallback is legal".
type NativeSpineUnaryDisposition uint8

const (
	// NativeSpineDeclinedPreSocket: the executor did NOT claim the attempt and
	// guarantees zero provider RoundTrips and zero native events. This is the ONLY
	// fallback-legal outcome — an outer composite may invoke an oracle/fallback here.
	// Err carries the typed capability-decline; Stage/Reason are bounded tokens.
	NativeSpineDeclinedPreSocket NativeSpineUnaryDisposition = iota
	// NativeSpineSucceeded: the executor owned exactly one provider request and
	// natively produced Final (the emitted carrier). Terminal — never a resend.
	NativeSpineSucceeded
	// NativeSpineFailedAfterClaim: the executor claimed the attempt (a socket MAY
	// have opened) and then failed terminally. Err is the typed failure. NEVER a
	// decline, NEVER a fallback/BAML resend for the same call.
	NativeSpineFailedAfterClaim
)

// String returns a bounded, secret-free token for the disposition.
func (d NativeSpineUnaryDisposition) String() string {
	switch d {
	case NativeSpineDeclinedPreSocket:
		return "declined_pre_socket"
	case NativeSpineSucceeded:
		return "succeeded"
	case NativeSpineFailedAfterClaim:
		return "failed_after_claim"
	default:
		return "unknown"
	}
}

// NativeSpineUnaryResult is the neutral tri-state result of a spine unary Call. Its
// Stage/Reason are secret-free bounded tokens describing WHERE/WHY the executor
// stepped aside or failed; Final is SENSITIVE (the parsed provider carrier).
type NativeSpineUnaryResult struct {
	Disposition NativeSpineUnaryDisposition
	// Final is the emitted output carrier on NativeSpineSucceeded (nil otherwise).
	Final any
	// Err is the typed decline (NativeSpineDeclinedPreSocket) or the terminal failure
	// (NativeSpineFailedAfterClaim); nil on success.
	Err error
	// Stage and Reason are bounded, secret-free enum-like tokens (never free-form
	// text or a secret).
	Stage  string
	Reason string
}

// NativeSpineUnaryExecutor is the neutral injected execution seam the emitted
// BuildMethod drives. Call performs one unary final call and returns the tri-state
// result; Parse is local and socket-free (native final parse + emitted decode). A
// non-admitted method returns the typed capability-decline (Call as a pre-socket
// decline, Parse as [NativeSpineUnsupportedMethodError]); an ordinary error is never
// a decline. The context passed is the request adapter (a bamlutils.Adapter embeds
// context.Context), so a cancelled request is observed inside the executor.
type NativeSpineUnaryExecutor interface {
	Call(ctx context.Context, method string, input any) NativeSpineUnaryResult
	Parse(ctx context.Context, method string, raw string) (any, error)
}

// NativeSpineUnsupportedMethodError is the typed capability-decline the hermetic
// native runtime surfaces for an UNSUPPORTED (non-admitted) method. It is a
// bounded, secret-free value (method name + bounded reason token). Parse returns
// it directly; Call surfaces it as the Err of a NativeSpineDeclinedPreSocket result.
// A malformed raw for an ADMITTED method is an ordinary terminal parse error, never
// this typed decline.
type NativeSpineUnsupportedMethodError struct {
	Method string
	Reason string
}

func (e *NativeSpineUnsupportedMethodError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("nativespine: method %q is not admitted for native unary serving", e.Method)
	}
	return fmt.Sprintf("nativespine: method %q is not admitted for native unary serving (%s)", e.Method, e.Reason)
}

// DeclinedSpineResult builds a NativeSpineDeclinedPreSocket result carrying the
// typed capability-decline and bounded stage/reason.
func DeclinedSpineResult(err error, stage, reason string) NativeSpineUnaryResult {
	return NativeSpineUnaryResult{
		Disposition: NativeSpineDeclinedPreSocket,
		Err:         err,
		Stage:       stage,
		Reason:      reason,
	}
}

// SucceededSpineResult builds a NativeSpineSucceeded result carrying the emitted
// final carrier.
func SucceededSpineResult(final any) NativeSpineUnaryResult {
	return NativeSpineUnaryResult{
		Disposition: NativeSpineSucceeded,
		Final:       final,
	}
}

// FailedAfterClaimSpineResult builds a NativeSpineFailedAfterClaim result carrying
// the terminal failure and bounded stage/reason.
func FailedAfterClaimSpineResult(err error, stage, reason string) NativeSpineUnaryResult {
	return NativeSpineUnaryResult{
		Disposition: NativeSpineFailedAfterClaim,
		Err:         err,
		Stage:       stage,
		Reason:      reason,
	}
}
