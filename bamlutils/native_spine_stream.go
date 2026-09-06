package bamlutils

import (
	"context"
	"errors"
)

// M3e-A — the NEUTRAL codegen-spine STREAM binding + executor contract, the streaming
// EXTENSION of the frozen unary contract in native_spine_unary.go.
//
// It is passive, BAML-free and CFFI-free in exactly the same sense: the emitted
// hermetic native method package imports ONLY context/bamlutils/bamlutils/
// promptdescriptor/stdlib, so it names these types and nothing from nativeserve or
// internal/*, and the production stream executor (nativeserve/spine) implements
// [NativeSpineStreamExecutor] over the existing native render/prepare/exact-stream/
// parse machinery.
//
// THREE PROPERTIES ARE LOAD-BEARING:
//
//   - It is an EXTENSION, not a replacement. [NativeSpineStreamExecutor] EMBEDS
//     [NativeSpineUnaryExecutor], so a stream-capable method still serves the frozen
//     unary Call and the socket-free final Parse through the same executor, and the
//     unary API stays source- and behavior-compatible.
//   - The ownership vocabulary is ALIASED to the unary tri-state, not re-declared.
//     There is no fourth "a partial failed but we can still fall back" state: a
//     claimed stream has no route back, so every post-claim fault is
//     [NativeSpineFailedAfterClaim].
//   - It carries NO oracle callback, NO BAML plan closure, and NO fallback function.
//     That is what keeps M3e-A the BAML-free native-only substrate rather than the
//     standard-worker composite (which is ExecBridge-U1s / M3e-B and owns its own
//     per-prefix oracle contract).
//
// SENSITIVE: a [NativeSpineStreamEvent].Partial, a [NativeSpineStreamResult].Final,
// and Raw/Reasoning are real parsed provider output — never log, %v-format,
// error-wrap, or metric-label them. Only the bounded Disposition/Stage/Reason tokens
// are safe to emit.

// NativeSpineStreamBinding is the neutral per-method STREAM registration the emitted
// native package hands the production runtime. It EMBEDS the unary binding rather
// than duplicating it: Unary owns Method, ProjectInput, and the final DecodeFinal, so
// a stream method can never acquire a second, drifting projector or final decoder.
//
// DecodePartial strictly decodes the native canonical PARTIAL bytes into the emitted
// STREAM carrier (the pointer/partial alias) via the proven
// bamlutils.DecodeStaticAliasStream core. It is resolved at REGISTRATION time
// (non-nil) and is reflection-free / BAML-free / CFFI-free.
//
// The FINAL of a stream uses Unary.DecodeFinal (the value carrier), never
// DecodePartial: a stream final is byte-identical to the unary final for the same
// completed text, so it has the ordinary final union type.
type NativeSpineStreamBinding struct {
	Unary         NativeSpineUnaryBinding
	DecodePartial func(canonicalJSON []byte) (any, error)
}

// NativeSpineStreamDisposition is the tri-state outcome of a spine Stream. It is a
// type ALIAS of [NativeSpineUnaryDisposition] because the ownership meaning is
// identical — declining certifies zero sockets and zero emitted events; succeeding
// and failing-after-claim are both terminal.
type NativeSpineStreamDisposition = NativeSpineUnaryDisposition

// The stream dispositions are the SAME constants as the unary ones, re-exported under
// stream-lane names so a stream call site reads naturally without introducing a
// second, divergible enum.
const (
	// NativeSpineStreamDeclinedPreSocket certifies NO provider socket and NO emitted
	// event. It is the only fallback-legal outcome (and the native-only lane has no
	// fallback: it surfaces as one terminal caller-visible error frame).
	NativeSpineStreamDeclinedPreSocket = NativeSpineDeclinedPreSocket
	// NativeSpineStreamSucceeded means every event already went through emit and the
	// final is decoded. Terminal — never a resend.
	NativeSpineStreamSucceeded = NativeSpineSucceeded
	// NativeSpineStreamFailedAfterClaim is terminal: an event or a socket MAY already
	// exist, so it can NEVER be reclassified as a decline.
	NativeSpineStreamFailedAfterClaim = NativeSpineFailedAfterClaim
)

// NativeSpineStreamEvent is one cadence-decided public event on a claimed stream: a
// structured partial, a raw-only delta, or both.
type NativeSpineStreamEvent struct {
	// HasPartial distinguishes "no structured partial this tick" from "a present but
	// typed-nil carrier". The exact `JSON` family is non-nullable so a typed nil never
	// arises from it, but the contract must not collapse the two states by accident —
	// a nullable family (a later slice) forwards a typed nil as a PRESENT partial
	// whose re-marshal is `null`.
	HasPartial bool
	// Partial is the emitted STREAM carrier (the pointer/partial alias) when
	// HasPartial. SENSITIVE.
	Partial any
	// Raw and Reasoning are the per-delta /stream-with-raw channels, empty on a plain
	// /stream. SENSITIVE.
	Raw       string
	Reasoning string
}

// NativeSpineStreamResult is the neutral tri-state result of a spine Stream. Final /
// Raw / Reasoning are SENSITIVE; Stage/Reason are bounded, secret-free tokens
// describing WHERE/WHY the executor stepped aside or failed.
type NativeSpineStreamResult struct {
	Disposition NativeSpineStreamDisposition

	// Succeeded only. Final is the emitted FINAL (value) carrier — never the stream
	// carrier — and Raw/Reasoning are the accumulated /stream-with-raw channels.
	Final     any
	Raw       string
	Reasoning string

	// Declined (typed pre-socket decline) or Failed-after-claim (typed terminal
	// error). RawDiagnostic is the raw the claimed lane had accumulated before the
	// failure, retained for the worker's details.raw.
	Err           error
	RawDiagnostic string
	Stage         string
	Reason        string
}

// NativeSpineStreamEmit is the SYNCHRONOUS event sink the generated builder supplies.
// Returning nil consumes the event; returning an error stops the stream immediately
// (a post-claim terminal — there is never a retry).
type NativeSpineStreamEmit func(NativeSpineStreamEvent) error

// NativeSpineStreamExecutor is the neutral injected STREAM execution seam the emitted
// BuildMethod drives for a stream-capable method. It EXTENDS the frozen unary
// executor:
//
//   - Call and Parse are inherited UNCHANGED (unary final-call and socket-free final
//     parse), so a ClassStaticStream method keeps serving /call and /parse exactly as
//     a ClassStaticUnary one does;
//   - Stream performs ONE claimed provider stream, emitting every public event through
//     emit before returning the tri-state result;
//   - ParseStream is local and socket-free: the native static-stream PARTIAL parse plus
//     the emitted stream decoder, for a direct `/parse?stream=true` request.
//
// A non-admitted method returns the typed capability-decline
// ([NativeSpineUnsupportedMethodError]) — Stream as a pre-socket decline, ParseStream
// directly. An ordinary error is never a decline.
type NativeSpineStreamExecutor interface {
	NativeSpineUnaryExecutor
	Stream(ctx context.Context, method string, input any, emit NativeSpineStreamEmit) NativeSpineStreamResult
	ParseStream(ctx context.Context, method string, raw string) (any, error)
}

// ErrNativeSpineStreamFailed is the BOUNDED, secret-free stand-in a
// [FailedAfterClaimSpineStreamResult] carries when the caller supplied no error. A
// post-claim failure must never surface as a nil error (which a total switch would
// read as success), and the substituted value must never carry content.
var ErrNativeSpineStreamFailed = errors.New("nativespine: native stream failed after the claim")

// ErrNativeSpineStreamDeclined is the same guarantee for the DECLINE constructor. A
// decline is delivered to the caller as one error frame, so a nil Err would surface as a
// terminal failure carrying no usable error at all. Every non-success result therefore
// carries a non-nil, bounded, secret-free error.
var ErrNativeSpineStreamDeclined = errors.New("nativespine: native stream declined before any provider socket")

// DeclinedSpineStreamResult builds a pre-socket decline carrying the typed
// capability/admission decline and bounded stage/reason. It certifies zero provider
// sockets and zero emitted events.
//
// A nil err is replaced with [ErrNativeSpineStreamDeclined], mirroring the
// failed-after-claim constructor: the native-only lane turns a decline into one error
// frame, so a nil error would reach the caller as a terminal failure with nothing to
// report.
func DeclinedSpineStreamResult(err error, stage, reason string) NativeSpineStreamResult {
	if err == nil {
		err = ErrNativeSpineStreamDeclined
	}
	return NativeSpineStreamResult{
		Disposition: NativeSpineStreamDeclinedPreSocket,
		Err:         err,
		Stage:       stage,
		Reason:      reason,
	}
}

// SucceededSpineStreamResult builds a success carrying the emitted FINAL (value)
// carrier and the accumulated raw/reasoning channels. Every partial has already been
// delivered through the emit callback by the time this is returned.
func SucceededSpineStreamResult(final any, raw, reasoning string) NativeSpineStreamResult {
	return NativeSpineStreamResult{
		Disposition: NativeSpineStreamSucceeded,
		Final:       final,
		Raw:         raw,
		Reasoning:   reasoning,
	}
}

// FailedAfterClaimSpineStreamResult builds a TERMINAL post-claim failure carrying the
// typed error, bounded stage/reason, and the owned raw diagnostic accumulated before
// the fault. A nil err is replaced with [ErrNativeSpineStreamFailed] so the result can
// never be mistaken for a success. It can NEVER become a decline: a socket and public
// events may already exist.
func FailedAfterClaimSpineStreamResult(err error, stage, reason, rawDiagnostic string) NativeSpineStreamResult {
	if err == nil {
		err = ErrNativeSpineStreamFailed
	}
	return NativeSpineStreamResult{
		Disposition:   NativeSpineStreamFailedAfterClaim,
		Err:           err,
		Stage:         stage,
		Reason:        reason,
		RawDiagnostic: rawDiagnostic,
	}
}

// ExecBridge-U1s / M3e-B — the NEUTRAL OPTIONAL oracle-capable extension to the spine
// STREAM executor. It is the streaming twin of [NativeSpineUnaryOracleExecutor] and stands
// in exactly the same relation to the M3e-A contract above: the base
// [NativeSpineStreamExecutor.Stream] is UNCHANGED and BAML-free, and a native-only emitted
// BuildMethod names neither this interface nor its result. Nothing on the emitted/runtime
// native-only path can construct the BAML closures this lane requires.

// NativeSpineStreamOracleExecutor is the optional oracle-capable spine stream executor: ONE
// claimed native stream for an admitted method under a LIVE BAML `StreamRequest`
// plan-compare admission plus a per-prefix and final BAML parse oracle over that ONE
// response.
//
// It is deliberately a SINGLE-METHOD interface rather than an extension of
// [NativeSpineStreamExecutor]: the standard composite drives StreamWithOracle and nothing
// else, so requiring it to also satisfy the native-only Call/Parse/Stream/ParseStream
// surface would couple the standard attachment (and every test double of it) to a surface
// it never uses. The exact population is decided STRUCTURALLY at construction —
// StreamWithOracle is a policy over the SAME immutable registry Stream serves, never a
// second population.
type NativeSpineStreamOracleExecutor interface {
	StreamWithOracle(ctx context.Context, inv NativeStaticStreamOracleInvocation, emit NativeSpineStreamEmit) NativeSpineStreamOracleResult
}

// NativeStreamOracleCompare is a BOUNDED, secret-free classification of ONE per-prefix (or
// final) native-vs-BAML comparison. It never carries a content-derived value.
type NativeStreamOracleCompare string

const (
	// NativeStreamCompareNone is the zero value: no comparison was performed.
	NativeStreamCompareNone NativeStreamOracleCompare = ""
	// NativeStreamCompareMatch: both engines produced a value and their PUBLIC marshaled
	// bytes are identical. Native's decoded value is served.
	NativeStreamCompareMatch NativeStreamOracleCompare = "match"
	// NativeStreamCompareNativeNoValue: native produced no value for this prefix (its
	// documented no-partial sentinel). BAML's value is served if it has one; if BAML also
	// has none the tick emits nothing and no drift is recorded.
	NativeStreamCompareNativeNoValue NativeStreamOracleCompare = "native_no_value"
	// NativeStreamCompareBAMLNoValue: native produced a value and BAML did not. The
	// structured partial is SUPPRESSED — BAML is the authority after the claim.
	NativeStreamCompareBAMLNoValue NativeStreamOracleCompare = "baml_no_value"
	// NativeStreamCompareBytesMismatch: both engines produced a value and their public
	// bytes differ. BAML's same-prefix value is served.
	NativeStreamCompareBytesMismatch NativeStreamOracleCompare = "bytes_mismatch"
	// NativeStreamCompareNativeError: native's parse, decode, or value marshal FAILED (not
	// its no-partial sentinel). BAML's value is served if it has one; either way the drift
	// latch is set.
	NativeStreamCompareNativeError NativeStreamOracleCompare = "native_error"
)

// NativeSpineStreamOracleObservations are the bounded, secret-free facts a StreamWithOracle
// carries out so the standard composite can replay the worker's de-BAML metric series
// without re-deriving them.
//
// Every field is false/zero on a decline taken BEFORE admission. The one exception is a
// plan-MISMATCH decline, which sets PlanCompareRan (with PlanMatched false) — the compare
// genuinely ran and genuinely refused, and a metric consumer must be able to tell that
// apart from "no comparison happened". NONE of them carries a content-derived value: no
// method output, prefix, raw text, request plan, panic value, or error string.
//
// The counters are carried out on EVERY path (including a post-claim panic), so evidence
// for plan-match, exactly-one-socket, per-prefix comparison, and drift is never lost.
type NativeSpineStreamOracleObservations struct {
	// PlanCompareRan: the live BAML StreamRequest plan compare executed; PlanMatched: it
	// byte-matched (which is why the attempt CLAIMED). A plan-mismatch decline sets
	// ran=true, matched=false.
	PlanCompareRan bool
	PlanMatched    bool
	// SocketOpened: the single provider stream was attempted (== claimed); SocketResponded:
	// the transport completed cleanly.
	SocketOpened    bool
	SocketResponded bool

	// PrefixComparisons is the number of structured cadence ticks the oracle was ENTERED
	// on. The five counters below partition the ticks it RESOLVED; a tick whose oracle
	// could not be established (which terminates the stream) is counted here and in none
	// of them, so `comparisons - sum(results)` is the count of oracle terminals rather
	// than an unexplained gap.
	PrefixComparisons   int
	PrefixMatch         int
	PrefixNativeNoValue int
	PrefixBAMLNoValue   int
	PrefixBytesMismatch int
	PrefixNativeError   int

	// FinalOracleRan: the final comparison was ENTERED (set before either leg runs, so a
	// panic in one does not lose the phase). FinalCompare is its bounded result.
	FinalOracleRan bool
	FinalCompare   NativeStreamOracleCompare

	// Substituted: at least one prefix or the final was served from BAML's same-response
	// value. Suppressed: at least one native structured partial was withheld because BAML
	// established no value for that prefix.
	Substituted bool
	Suppressed  bool

	// OracleTerminal: the oracle itself terminated the claimed stream (a BAML leg that
	// could not be established, a comparison invariant failure, or a final with no value).
	OracleTerminal bool

	// ServeOutcome is the bounded serve-outcome classification for RecordServeOutcome.
	ServeOutcome NativeStaticServeOutcome
}

// NativeSpineStreamOracleResult is the neutral tri-state result of a spine
// StreamWithOracle. It is a DISTINCT type from [NativeSpineStreamResult] on purpose: the
// M3e-A native-only contract carries no winner and no observations, and folding the oracle's
// evidence into it would weaken that contract rather than extend it. There is no "partial
// fallback" disposition — a claimed stream has no route back.
//
// SENSITIVE: Final/Raw/Reasoning are parsed provider output. Only
// Disposition/Stage/Reason/WinnerEngine and the bounded Observations are safe to emit.
type NativeSpineStreamOracleResult struct {
	Disposition NativeSpineStreamDisposition

	// Succeeded-only: the ALREADY-ORACLED typed final in the STANDARD method's concrete
	// carrier (never the hermetic emitted spine carrier), the accumulated
	// /stream-with-raw channels, and the bounded winner-engine token
	// (NativeStaticServeEngineNative / NativeStaticServeEngineBAMLParse).
	Final        any
	Raw          string
	Reasoning    string
	WinnerEngine string

	// Declined (typed pre-socket decline) or Failed-after-claim (typed terminal error).
	Err           error
	RawDiagnostic string
	Stage         string
	Reason        string

	Observations NativeSpineStreamOracleObservations
}

// DeclinedSpineStreamOracleResult builds a pre-socket decline certifying zero provider
// sockets and zero emitted events. A nil err is replaced with the bounded
// [ErrNativeSpineStreamDeclined] so a decline can never surface as a terminal failure
// carrying no error at all.
func DeclinedSpineStreamOracleResult(err error, stage, reason string) NativeSpineStreamOracleResult {
	if err == nil {
		err = ErrNativeSpineStreamDeclined
	}
	return NativeSpineStreamOracleResult{
		Disposition: NativeSpineStreamDeclinedPreSocket,
		Err:         err,
		Stage:       stage,
		Reason:      reason,
	}
}

// SucceededSpineStreamOracleResult builds a success carrying the oracled typed final, the
// accumulated raw/reasoning channels, and the bounded winner-engine token. Every public
// event has already been delivered through the emit callback by the time it is returned.
func SucceededSpineStreamOracleResult(final any, raw, reasoning, winnerEngine string) NativeSpineStreamOracleResult {
	return NativeSpineStreamOracleResult{
		Disposition:  NativeSpineStreamSucceeded,
		Final:        final,
		Raw:          raw,
		Reasoning:    reasoning,
		WinnerEngine: winnerEngine,
	}
}

// FailedAfterClaimSpineStreamOracleResult builds a TERMINAL post-claim failure carrying the
// typed error, bounded stage/reason, and the owned raw diagnostic accumulated before the
// fault. A nil err is replaced with [ErrNativeSpineStreamFailed] so the result can never be
// mistaken for a success. It can NEVER become a decline.
func FailedAfterClaimSpineStreamOracleResult(err error, stage, reason, rawDiagnostic string) NativeSpineStreamOracleResult {
	if err == nil {
		err = ErrNativeSpineStreamFailed
	}
	return NativeSpineStreamOracleResult{
		Disposition:   NativeSpineStreamFailedAfterClaim,
		Err:           err,
		Stage:         stage,
		Reason:        reason,
		RawDiagnostic: rawDiagnostic,
	}
}
