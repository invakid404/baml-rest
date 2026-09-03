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

// ExecBridge-U1c — the NEUTRAL OPTIONAL oracle-capable extension to the spine unary
// executor. It is the concrete form of the "outer injected composite" the base
// contract documents: a standard SERVE worker can construct BAML's no-send plan and
// same-bytes parse (the neutral closures carried on a [NativeStaticInvocation]), so it
// drives the exact-U1 population through a LIVE BAML plan-compare oracle + same-bytes
// safety parse and receives canonical JSON for the standard generated decoder. The
// base [NativeSpineUnaryExecutor.Call]/[NativeSpineUnaryExecutor.Parse] are UNCHANGED:
// a native-only emitted BuildMethod names neither this interface nor its result, and
// nothing on the emitted/runtime native-only path can construct the BAML closures it
// requires.

// NativeSpineUnaryOracleExecutor is the optional oracle-capable spine executor. It
// extends the base neutral executor with CallWithOracle: one unary final call for an
// admitted method through a LIVE BAML plan-compare admission + a same-bytes BAML parse
// oracle over the ONE provider response, returning canonical JSON. The exact-U1
// population is decided structurally at construction — CallWithOracle is a policy over
// the SAME immutable registry Call serves, never a second population.
type NativeSpineUnaryOracleExecutor interface {
	NativeSpineUnaryExecutor
	CallWithOracle(ctx context.Context, inv NativeStaticInvocation) NativeSpineUnaryOracleResult
}

// NativeSpineUnaryOracleResult is the neutral tri-state result of a spine unary
// CallWithOracle. It mirrors [NativeStaticServeResult]'s discipline so the standard
// composite adapter maps it with a total switch. The zero value is the safe
// NativeSpineDeclinedPreSocket (zero RoundTrips, fallback-legal).
//
// Invariants (the spelling is implementation detail; these are not):
//
//   - A DECLINED result (NativeSpineDeclinedPreSocket) certifies zero provider
//     RoundTrips and carries only the typed decline plus bounded Stage/Reason. It is
//     the ONLY fallback-legal outcome — the outer composite returns it to the BAML
//     orchestrator, which serves the same call.
//   - A SUCCEEDED result (NativeSpineSucceeded) carries owned canonical FinalJSON (for
//     the standard generated decoder), owned Raw/Reasoning, and a bounded WinnerEngine
//     token. It does NOT expose the native-emitted typed carrier — those packages
//     define distinct Go types, so the boundary is crossed as canonical JSON.
//   - A FAILED-AFTER-CLAIM result (NativeSpineFailedAfterClaim) carries a typed
//     terminal error and an optional owned RawDiagnostic; it can NEVER become a
//     decline (a socket may have opened).
//   - An unknown integer disposition is treated as post-claim-possible and therefore
//     TERMINAL, matching the fail-closed branch in the generated static seam.
//
// SENSITIVE: FinalJSON/Raw/Reasoning are parsed provider output; treat like the
// response body. Only Disposition/Stage/Reason/WinnerEngine are safe to emit.
type NativeSpineUnaryOracleResult struct {
	Disposition NativeSpineUnaryDisposition

	// Succeeded-only: owned canonical JSON for the standard generated decoder, the
	// owned /call-with-raw channels, and the bounded winner-engine token
	// (NativeStaticServeEngineNative / NativeStaticServeEngineBAMLParse).
	FinalJSON    []byte
	Raw          string
	Reasoning    string
	WinnerEngine string

	// Declined (typed capability/plan decline) or Failed-after-claim (typed terminal
	// error). Stage/Reason are bounded, secret-free tokens; RawDiagnostic is an owned
	// failure diagnostic retained for details.raw.
	Err           error
	RawDiagnostic string
	Stage         string
	Reason        string

	// Observations are the bounded, secret-free metric facts the outer standard composite
	// replays into the worker's de-BAML metric series. They are carried out on EVERY path
	// (including a post-claim panic), so evidence for plan-match, exact-one-socket,
	// same-response facets, and drift is never lost.
	Observations NativeSpineUnaryOracleObservations
}

// NativeStaticServeOutcome is a neutral, bounded mirror of the serve-outcome the standard
// composite records via its admission metrics (RecordServeOutcome). NativeStaticOutcomeNone
// means no claimed terminal happened (a pre-socket decline).
type NativeStaticServeOutcome uint8

const (
	NativeStaticOutcomeNone NativeStaticServeOutcome = iota
	NativeStaticOutcomeSuccess
	NativeStaticOutcomeParseError
	NativeStaticOutcomeParseDecline
	NativeStaticOutcomeTranslateError
	NativeStaticOutcomeProviderError
	NativeStaticOutcomeTransportError
)

// NativeSpineUnaryOracleObservations are the bounded, secret-free facts a CallWithOracle
// carries out so the standard composite can replay the worker's de-BAML metric series
// without re-deriving them. Every field is false/zero on a pre-admission decline (no plan
// compare, no socket). None of them carries a content-derived value.
type NativeSpineUnaryOracleObservations struct {
	// PlanCompareRan: the live BAML plan compare executed; PlanMatched: it byte-matched
	// (which is why the attempt CLAIMED). A plan-mismatch decline sets ran=true, matched=false.
	PlanCompareRan bool
	PlanMatched    bool
	// SocketOpened: the single provider RoundTrip was attempted (== claimed);
	// SocketResponded: the socket returned a response (vs a transport failure).
	SocketOpened    bool
	SocketResponded bool
	// SameResponseOracleRan: the structured same-response BAML oracle was ENTERED. It is set
	// before the parser runs, so a parser panic does not lose the phase.
	SameResponseOracleRan bool
	// ErrorCompareRecorded / ErrorCompareMatch: the FieldError response-compare and its result.
	ErrorCompareRecorded bool
	ErrorCompareMatch    bool
	// StructuredBranchServed: a served structured result (translate/assistant/raw/reasoning =
	// true, structured/order = the compare booleans below).
	StructuredBranchServed bool
	StructuredMatch        bool
	OrderMatch             bool
	// ParseDeclineServed: a served parse-decline result (translate = true, structured/order =
	// false).
	ParseDeclineServed bool
	// Fallback: the served final came from the same-bytes BAML parse (drift / parse-decline).
	Fallback bool
	// ServeOutcome: the bounded serve-outcome classification for RecordServeOutcome.
	ServeOutcome NativeStaticServeOutcome
}

// DeclinedOracleResult builds a NativeSpineDeclinedPreSocket oracle result carrying the
// typed decline and bounded stage/reason. It certifies zero RoundTrips.
func DeclinedOracleResult(err error, stage, reason string) NativeSpineUnaryOracleResult {
	return NativeSpineUnaryOracleResult{
		Disposition: NativeSpineDeclinedPreSocket,
		Err:         err,
		Stage:       stage,
		Reason:      reason,
	}
}

// SucceededOracleResult builds a NativeSpineSucceeded oracle result carrying the owned
// canonical FinalJSON, /call-with-raw channels, and bounded winner-engine token.
func SucceededOracleResult(finalJSON []byte, raw, reasoning, winnerEngine string) NativeSpineUnaryOracleResult {
	return NativeSpineUnaryOracleResult{
		Disposition:  NativeSpineSucceeded,
		FinalJSON:    finalJSON,
		Raw:          raw,
		Reasoning:    reasoning,
		WinnerEngine: winnerEngine,
	}
}

// FailedAfterClaimOracleResult builds a NativeSpineFailedAfterClaim oracle result
// carrying the typed terminal error, bounded stage/reason, and an optional owned raw
// diagnostic. It can never become a decline.
func FailedAfterClaimOracleResult(err error, stage, reason, rawDiagnostic string) NativeSpineUnaryOracleResult {
	return NativeSpineUnaryOracleResult{
		Disposition:   NativeSpineFailedAfterClaim,
		Err:           err,
		Stage:         stage,
		Reason:        reason,
		RawDiagnostic: rawDiagnostic,
	}
}
