// Package staticoracle is the NEUTRAL, transport-free same-response static oracle
// core (ExecBridge-U1c). It is the single factored implementation of the
// post-attempt logic the static serve path used to inline
// (nativeserve/canary/serve_static.go's mapStaticAttempt + serveStaticStructured +
// serveStaticParseOnly): given a COMPLETED native unary attempt (the exact executor
// already owned the ONE provider request) plus the neutral BAML-only same-bytes
// parse closure, it classifies the outcome, runs BAML `Parse.<Method>` over the SAME
// assistant text native parsed, compares the two flattened structured outputs, and
// returns a closed [Result]: the winning canonical JSON (native or the same-bytes
// BAML parse) or a typed terminal error, plus bounded compare-facet observations.
//
// It has NO transport method and therefore CANNOT re-send: the only BAML input is
// [Result]-shaping over the assistant text the ONE request already returned. Both the
// rollout-named nativeserve/canary static serve path AND the deletion-oriented
// nativeserve/spine oracle Call delegate to it, so there is exactly ONE oracle
// algorithm and neither can drift. It records NOTHING itself — every caller replays
// the bounded facet observations into its own metrics — so this package depends on no
// metrics/admission type and stays a pure function.
//
// SENSITIVE: the completed attempt's Structured/AssistantText/Raw/Reasoning and the
// BAML parse of them are parsed provider output; [Result] carries owned byte/string
// payloads and bounded comparison facts, and this package never logs the plan,
// projected values, raw response, or parsed output.
package staticoracle

import (
	"context"
	"errors"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/parity"
)

// ErrMalformed2xx is today's BuildRequest extraction error class for a malformed
// provider 2xx body — a plain error (NOT an *HTTPError, NOT ErrOutputParse) so the
// worker classifier maps it to worker_error with details.raw, exactly like the BAML
// build/send path. Its exact bytes are the BAML-compatibility envelope, so both the
// canary static serve path and the spine oracle share this one value.
var ErrMalformed2xx = errors.New("buildrequest: failed to extract response content: malformed provider 2xx response")

// ErrNoBAMLOnlyParse marks a serve attempt that reached the response phase without a
// BAML-only parse closure. It is an internal invariant (production codegen always
// supplies one); the same-bytes oracle cannot run without it, so the attempt is a
// terminal parse error.
var ErrNoBAMLOnlyParse = errors.New("nativeserve/canary: same-response compare missing BAML-only parse closure")

// BAMLOnlyParse is the neutral BAML `Parse.<Method>`-over-the-same-bytes closure. It is
// local and socket-free — it CANNOT call `Request.<Method>` — and returns the flattened
// canonical JSON of the assistant text, or a terminal parse error.
type BAMLOnlyParse func(ctx context.Context, raw string) ([]byte, error)

// Outcome is the bounded serve-outcome classification (a neutral mirror of
// admission.Outcome) each caller records via its own RecordServeOutcome.
type Outcome uint8

const (
	// OutcomeSuccess: a served final (native or same-bytes BAML parse).
	OutcomeSuccess Outcome = iota
	// OutcomeParseError: a claimed native SAP parse failure, a BAML same-bytes parse
	// failure, or the missing-BAML-parse invariant — mapped to ErrOutputParse.
	OutcomeParseError
	// OutcomeParseDecline: native SAP declined the shape; BAML parsed the same bytes.
	OutcomeParseDecline
	// OutcomeTranslateError: response translate/extract failure, or a malformed 2xx body.
	OutcomeTranslateError
	// OutcomeProviderError: an ordinary provider non-2xx.
	OutcomeProviderError
	// OutcomeTransportError: transport failure, or context loss after the claim.
	OutcomeTransportError
)

// Result is the closed neutral outcome of the same-response static oracle over one
// completed attempt. Exactly one of the success (Served) or failure payloads is set;
// the facet fields are bounded observations a caller may replay into its own metrics.
type Result struct {
	// Served reports a served final. FinalJSON is the winning flattened canonical JSON
	// (native SAP output or the same-bytes BAML parse), Raw/Reasoning are the
	// native-owned /call-with-raw channels, and Winner is the bounded engine token
	// (bamlutils.NativeStaticServeEngineNative / NativeStaticServeEngineBAMLParse).
	Served    bool
	FinalJSON []byte
	Raw       string
	Reasoning string
	Winner    string

	// Failure payload (Served == false): the typed terminal error handed to the outer
	// policy and an optional owned raw diagnostic retained as details.raw.
	Err           error
	RawDiagnostic string

	// Outcome is the bounded serve-outcome classification.
	Outcome Outcome

	// --- bounded compare-facet observations (a caller decides whether to record) ---

	// SameResponseOracleRan reports the structured branch was entered (the strict
	// same-response BAML oracle ran or was about to). It is the phase the canary path
	// records as PhaseSameResponseOracle. It is NOT set on the parse-decline branch.
	SameResponseOracleRan bool
	// ErrorCompareRecorded / ErrorCompareMatch: whether a FieldError response-compare
	// was made and its result (match on a clean BAML parse, mismatch on a nil/errored
	// BAML parse).
	ErrorCompareRecorded bool
	ErrorCompareMatch    bool
	// StructuredBranchServed: a served structured result (translate/assistant/raw/
	// reasoning facets = true, structured/order facets = the compare booleans below).
	StructuredBranchServed bool
	StructuredMatch        bool
	OrderMatch             bool
	// ParseDeclineServed: a served parse-decline result (translate = true, structured =
	// false, order = false).
	ParseDeclineServed bool
	// Fallback: the served final came from the same-bytes BAML parse (drift or
	// parse-decline), recorded as FallbackParseOnly.
	Fallback bool
}

// Resolve maps one COMPLETED native static attempt's (result, error) onto the closed
// same-response oracle Result. It NEVER opens a socket and NEVER re-sends: bamlOnlyParse
// is the only BAML input and it runs over the assistant text the ONE provider request
// already returned. A nil res with a non-nil aerr is a transport failure; every path is
// terminal (success or typed failure) — there is no decline here, because the caller
// already CLAIMED the attempt.
func Resolve(ctx context.Context, bundle *schema.Bundle, res *execute.AttemptResult, aerr error, bamlOnlyParse BAMLOnlyParse) Result {
	if aerr != nil {
		if isContextErr(aerr) {
			return failResult(OutcomeTransportError, aerr, "")
		}
		if res == nil {
			return failResult(OutcomeTransportError, aerr, "")
		}
		if res.SAPInvoked {
			// CLAIMED native SAP parse failure -> ErrOutputParse + the extracted assistant
			// text as details.raw (mirrors BAML's parse_error envelope). The wrap prefix is
			// part of that envelope, not decoration — BAML's own final-parse site wraps
			// identically.
			return failResult(OutcomeParseError, &buildrequest.OutputParseError{
				Err: fmt.Errorf("buildrequest: failed to parse final result: %w", aerr),
			}, res.Raw)
		}
		// Translate / non-JSON-2xx / assistant-extraction failure -> today's extraction
		// error class with the raw upstream body retained as details.raw.
		return failResult(OutcomeTranslateError, fmt.Errorf("buildrequest: failed to extract response content: %w", aerr), string(res.ProviderBody))
	}

	switch res.Outcome {
	case execute.OutcomeProviderError:
		return failResult(OutcomeProviderError, &llmhttp.HTTPError{
			StatusCode: res.ProviderStatus,
			Body:       capErrorBody(res.ProviderBody),
		}, "")
	case execute.OutcomeInvalidBody:
		return failResult(OutcomeTranslateError, ErrMalformed2xx, string(res.ProviderBody))
	case execute.OutcomeParseDeclined:
		return resolveParseOnly(ctx, res, bamlOnlyParse)
	case execute.OutcomeStructured:
		return resolveStructured(ctx, bundle, res, bamlOnlyParse)
	default:
		return failResult(OutcomeParseError, fmt.Errorf("nativeserve/staticoracle: unexpected static attempt outcome %v", res.Outcome), "")
	}
}

// resolveStructured runs the strict same-response BAML parse over the SAME bytes for a
// clean native structured claim: on a structured/order MATCH it serves the native
// flattened JSON, on drift it serves the BAML parse of the same bytes (still one
// provider request). A nil/errored BAML parse of those bytes is compatibility-terminal.
func resolveStructured(ctx context.Context, bundle *schema.Bundle, res *execute.AttemptResult, bamlOnlyParse BAMLOnlyParse) Result {
	r := Result{SameResponseOracleRan: true}
	if bamlOnlyParse == nil {
		r.Outcome = OutcomeParseError
		r.ErrorCompareRecorded, r.ErrorCompareMatch = true, false
		r.Err = &buildrequest.OutputParseError{Err: ErrNoBAMLOnlyParse}
		r.RawDiagnostic = res.Raw
		return r
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxFail(r, ctxErr)
	}
	// S5 same-response BAML Parse over the SAME assistant text native parsed:
	// res.AssistantText — the text-only channel extracted from the OpenAI-TRANSLATED
	// body — NEVER a re-extraction from the pre-translation provider body.
	bamlStructured, berr := bamlOnlyParse(ctx, res.AssistantText)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxFail(r, ctxErr)
	}
	if berr != nil {
		if isContextErr(berr) {
			return ctxFail(r, berr)
		}
		r.Outcome = OutcomeParseError
		r.ErrorCompareRecorded, r.ErrorCompareMatch = true, false
		r.Err = &buildrequest.OutputParseError{Err: berr}
		r.RawDiagnostic = res.Raw
		return r
	}
	r.ErrorCompareRecorded, r.ErrorCompareMatch = true, true
	r.StructuredBranchServed = true
	structuredMatch, orderMatch := parity.CompareStaticStructured(res.Structured, bamlStructured, bundle)
	r.StructuredMatch, r.OrderMatch = structuredMatch, orderMatch
	r.Served = true
	r.Raw, r.Reasoning = res.Raw, res.Reasoning
	r.Outcome = OutcomeSuccess
	if structuredMatch && orderMatch {
		// MATCH -> serve the native flattened JSON with native-owned raw/reasoning.
		r.FinalJSON = res.Structured
		r.Winner = bamlutils.NativeStaticServeEngineNative
		return r
	}
	// Structured/order drift -> serve the BAML parse of the SAME bytes for safety.
	r.Fallback = true
	r.FinalJSON = bamlStructured
	r.Winner = bamlutils.NativeStaticServeEngineBAMLParse
	return r
}

// resolveParseOnly handles a native SAP decline (OutcomeParseDeclined): native
// transported and translated cleanly but declined the parse shape, so BAML
// `Parse.<Method>` runs on the SAME extracted assistant text and serves that final. One
// provider request, zero re-sends.
func resolveParseOnly(ctx context.Context, res *execute.AttemptResult, bamlOnlyParse BAMLOnlyParse) Result {
	var r Result
	if bamlOnlyParse == nil {
		r.Outcome = OutcomeParseError
		r.ErrorCompareRecorded, r.ErrorCompareMatch = true, false
		r.Err = &buildrequest.OutputParseError{Err: ErrNoBAMLOnlyParse}
		r.RawDiagnostic = res.Raw
		return r
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxFail(r, ctxErr)
	}
	bamlStructured, berr := bamlOnlyParse(ctx, res.AssistantText)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxFail(r, ctxErr)
	}
	if berr != nil {
		if isContextErr(berr) {
			return ctxFail(r, berr)
		}
		r.Outcome = OutcomeParseError
		r.Err = &buildrequest.OutputParseError{Err: berr}
		r.RawDiagnostic = res.Raw
		return r
	}
	// native declined where BAML parsed -> a real structured/order divergence.
	r.ParseDeclineServed = true
	r.Fallback = true
	r.Served = true
	r.FinalJSON = bamlStructured
	r.Raw, r.Reasoning = res.Raw, res.Reasoning
	r.Winner = bamlutils.NativeStaticServeEngineBAMLParse
	r.Outcome = OutcomeParseDecline
	return r
}

// ctxFail returns the terminal for the post-claim BAML-only parse ctx gates: a
// canceled/deadline-exceeded request must NEVER be served a native/parse result, even
// if the BAMLOnlyParse callback ignored cancellation and returned a valid value. It
// records transport_error and returns the context error UNCHANGED (no OutputParseError
// wrap, no details.raw) so errors.Is holds for the outer policy. It preserves the
// SameResponseOracleRan facet the caller already entered.
func ctxFail(r Result, ctxErr error) Result {
	same := r.SameResponseOracleRan
	return Result{SameResponseOracleRan: same, Outcome: OutcomeTransportError, Err: ctxErr}
}

func failResult(outcome Outcome, err error, rawDiagnostic string) Result {
	return Result{Outcome: outcome, Err: err, RawDiagnostic: rawDiagnostic}
}

// isContextErr reports whether err is a caller cancellation or deadline.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// capErrorBody caps the raw provider body to the 4 KiB PUBLIC diagnostic cap
// (llmhttp.MaxErrorBodyBytes) used by the existing provider_error envelope.
func capErrorBody(b []byte) string {
	if len(b) > llmhttp.MaxErrorBodyBytes {
		b = b[:llmhttp.MaxErrorBodyBytes]
	}
	return string(b)
}
