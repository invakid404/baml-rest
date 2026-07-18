package admission

import (
	"errors"

	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// engineDisposition is the typed classification of a nanollm New/Prepare/Build
// failure. It decides — WITHOUT ever string-matching an error message — whether
// the failure is an ordinary pre-socket "provider does not support this" decline
// to BAML, or an unexpected planner/FFI error that must alert instead of reading
// as expected unsupported traffic.
type engineDisposition uint8

const (
	// engineUnsupported: nanollm returned a typed *Error whose Code names an
	// ordinary pre-socket unsupported condition — unsupported_request (this
	// provider/spec does not support this operation) or invalid_provider (an
	// unknown provider prefix). Both are ordinary declines to the same BAML child;
	// no socket opened.
	engineUnsupported engineDisposition = iota
	// enginePlannerError: any OTHER failure — a generic nanollm config error, an
	// ABI/marshal/transform-config error, or a non-nanollm error entirely. It is
	// availability-first (the canary still safely falls back to BAML pre-socket)
	// but is recorded as OutcomePlannerError so it alerts / blocks rollout rather
	// than being counted as ordinary unsupported traffic.
	enginePlannerError
)

// The two typed nanollm error CODES that classify as an ordinary pre-socket
// unsupported decline. They are matched on the typed *nanollm.Error.Code field
// (via errors.As), NEVER by parsing an error message.
//
//   - codeUnsupportedRequest is surfaced by the FFI for Rust
//     EngineError::Unsupported (present in v0.4.3).
//   - codeInvalidProvider is the stable unknown-provider code Viktor's upstream
//     P0 adds. nanollm v0.4.3 does NOT emit it yet (an unknown prefix fails
//     nanollm.New as the generic "config" code there, which classifies as
//     enginePlannerError); keying on the code now makes the classifier
//     forward-ready without a string match or a provider-membership table.
//
// KNOWN LIMITATION — deferred to the multi-provider epic (#546) / ledger (#583):
// because v0.4.3 has no typed invalid_provider code, a truly-UNKNOWN provider
// prefix classifies as enginePlannerError (OutcomePlannerError) rather than an
// ordinary unsupported decline. This is metric NOISE, NOT a correctness gap — the
// request STILL declines to BAML pre-socket with zero sockets; only the metric
// reads planner_error (which alerts) instead of a clean invalid_provider decline.
// When the upstream P0 lands the typed code, the classifier flips it to
// engineUnsupported with no baml-rest logic change, and the gated
// TestEngineBoundary_UnknownProviderKnownLimitation is updated. (Cohere is a
// separate deferral: v0.4.3 plans /v2/embed for a chat request, caught fail-closed
// PRE-socket by the provider-neutral ReasonEmbeddingPlan gate in validateGenericPlan.)
const (
	codeUnsupportedRequest = "unsupported_request"
	codeInvalidProvider    = "invalid_provider"
)

// classifyEngineError classifies a nanollm New/Prepare/Build error by TYPED code
// (§5.1). A non-nanollm error, or a *nanollm.Error whose Code is anything other
// than the two ordinary-unsupported codes, is an enginePlannerError; only the two
// typed codes are engineUnsupported. It never inspects the error message text.
func classifyEngineError(err error) engineDisposition {
	var ne *nanollm.Error
	if !errors.As(err, &ne) {
		return enginePlannerError
	}
	switch ne.Code {
	case codeUnsupportedRequest, codeInvalidProvider:
		return engineUnsupported
	default:
		return enginePlannerError
	}
}

// prepareUnsupportedReason maps an engineUnsupported nanollm *Error onto the
// stable prepare decline reason for its code, so the declines metric distinguishes
// unsupported_request from invalid_provider. It is only called on an error already
// classified engineUnsupported; a code that is not one of the two (which cannot
// happen for an engineUnsupported error) falls back to unsupported_request.
func prepareUnsupportedReason(err error) Reason {
	var ne *nanollm.Error
	if errors.As(err, &ne) && ne.Code == codeInvalidProvider {
		return ReasonPrepareInvalidProvider
	}
	return ReasonPrepareUnsupportedRequest
}
