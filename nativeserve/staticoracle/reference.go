package staticoracle

import (
	"bytes"
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

// ReferenceExtract is the BAML serving path's own response extractor: given the resolved
// provider, the ORIGINAL provider response body, and the request's reasoning preference, it
// returns the parseable text BAML's final parser consumes plus the /call-with-raw raw and
// reasoning channels. Production passes buildrequest.ExtractResponseContentBytes — the same
// extractor the generated orchestrator injects into RunCallOrchestration (its string and
// borrowed twins are pinned to identical output).
type ReferenceExtract func(provider string, body []byte, includeReasoning bool) (parseable, raw, reasoning string, err error)

// Reference names the flag-off byte path [ResolveReference] reproduces from the ONE provider
// response: the provider and reasoning preference the BAML leg would extract with, the
// extractor itself, and the BAML-only `Parse.<Method>` closure the BAML leg would parse the
// extracted text with.
type Reference struct {
	Provider         string
	IncludeReasoning bool
	Extract          ReferenceExtract
	Parse            BAMLOnlyParse
}

// ErrNoReferenceExtract marks a reference-oracle resolution that reached the response phase
// without the BAML-path extractor. Like [ErrNoBAMLOnlyParse] it is an internal invariant
// (the spine executor always installs one); without it no reference exists, so the claimed
// attempt is a terminal parse error rather than a native result nobody checked.
var ErrNoReferenceExtract = errors.New("nativeserve/staticoracle: reference oracle missing the BAML-path response extractor")

// errNoAttemptResult is the defensive terminal for an attempt that returned neither a
// result nor an error. RunAttempt never does; the guard keeps a broken caller terminal.
var errNoAttemptResult = errors.New("nativeserve/staticoracle: native attempt returned no result")

// ResolveReference is the RAW-AWARE same-response oracle for the live-oracle static spine
// lane. Where [Resolve] parses the assistant text NATIVE extracted from the nanollm-
// translated body and keeps native's raw/reasoning channels, this establishes the
// REFERENCE envelope independently from the ORIGINAL provider response — exactly the bytes
// the flag-off BAML path would have served for the same response:
//
//  1. the reference channels are BAML's own extraction of res.ProviderBody (never
//     res.Translated, never res.AssistantText), with ref.Provider and the request's
//     reasoning preference;
//  2. the reference value is BAML's `Parse.<Method>` of that parseable text;
//  3. native wins ONLY when it produced a structured claim AND every facet agrees with the
//     reference — assistant, raw and reasoning by exact string-byte equality, the structured
//     value and order semantically, AND the canonical final JSON literally byte-for-byte;
//  4. on ANY drift — and whenever native declined or failed after the response — the WHOLE
//     reference envelope is served (BAML's parse + the reference raw + the reference
//     reasoning). Mixing native channels with BAML's value would ship an envelope neither
//     engine produces.
//
// The reference leg is established for every complete 2xx response, so a native
// translate/extraction/parse failure is recovered from the SAME bytes when BAML succeeds,
// and a reference failure is terminal with the error BAML's serving path raises for those
// bytes (the extraction error with the provider body as details.raw, or the wrapped final
// parse error with the extracted raw text). A non-2xx original status is the provider error
// BAML's transport raises for it. A transport failure, a lost/absent response, or a caller
// cancellation is terminal. Nothing here opens a socket: there is no resend, retry, or
// fallback send — every outcome is SUCCEEDED or a typed post-claim FAILURE.
//
// onOracle, if non-nil, is a PANIC-SAFE hook invoked the instant the reference leg is
// ENTERED — before the extractor or the parser runs — so a panic in either (which unwinds
// without ResolveReference returning) never loses the same-response phase.
//
// SENSITIVE: the provider body, channels, and both parsed values are provider output; they
// only flow into the owned Result payloads, never into logs or bounded facets.
func ResolveReference(ctx context.Context, bundle *schema.Bundle, res *execute.AttemptResult, aerr error, ref Reference, onOracle func()) Result {
	// No usable response: a transport failure before any body, or caller cancellation
	// racing the post-response pipeline. Terminal, and the error is returned UNCHANGED so
	// errors.Is/As hold for the outer policy. A caller that re-expressed a body-read
	// failure on a non-2xx as the ordinary transport's *HTTPError (which reports that
	// case as the provider error with the bytes it read) is a provider error.
	if aerr != nil && (res == nil || isContextErr(aerr)) {
		var httpErr *llmhttp.HTTPError
		if res == nil && errors.As(aerr, &httpErr) {
			return failResult(OutcomeProviderError, aerr, "")
		}
		return failResult(OutcomeTransportError, aerr, "")
	}
	if res == nil {
		return failResult(OutcomeTransportError, errNoAttemptResult, "")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return failResult(OutcomeTransportError, ctxErr, "")
	}
	// The ORIGINAL status classifies the response exactly as BAML's transport does: a
	// non-2xx is an *HTTPError carrying the real status and the body capped to the public
	// diagnostic cap, whatever native's translation made of it — a native translate error
	// over such a body is superseded by the classification BAML would have made.
	if res.ProviderStatus < 200 || res.ProviderStatus >= 300 {
		return failResult(OutcomeProviderError, &llmhttp.HTTPError{
			StatusCode: res.ProviderStatus,
			Body:       capErrorBody(res.ProviderBody),
		}, "")
	}

	// PANIC-SAFE phase entry, before the extractor and the parser.
	if onOracle != nil {
		onOracle()
	}
	r := Result{SameResponseOracleRan: true}
	if ref.Parse == nil || ref.Extract == nil {
		// Internal invariant: the caller guarantees both before the claim. Without them no
		// reference exists, so nothing may be served.
		missing := ErrNoBAMLOnlyParse
		if ref.Parse != nil {
			missing = ErrNoReferenceExtract
		}
		r.Outcome = OutcomeParseError
		r.ErrorCompareRecorded, r.ErrorCompareMatch = true, false
		r.Err = &buildrequest.OutputParseError{Err: missing}
		return r
	}

	// --- the reference leg: BAML's serving path over the ORIGINAL response bytes ---
	refParseable, refRaw, refReasoning, xerr := ref.Extract(ref.Provider, res.ProviderBody, ref.IncludeReasoning)
	if xerr != nil {
		// BAML's extraction rejects these bytes: its error class, its wrap prefix, and the
		// provider body as details.raw — never a native final BAML would not have served.
		r.Outcome = OutcomeTranslateError
		r.ErrorCompareRecorded, r.ErrorCompareMatch = true, false
		r.Err = fmt.Errorf("buildrequest: failed to extract response content: %w", xerr)
		r.RawDiagnostic = string(res.ProviderBody)
		return r
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxFail(r, ctxErr)
	}
	bamlStructured, berr := ref.Parse(ctx, refParseable)
	// A parser may ignore cancellation and still return a value; a canceled request is
	// never served. Checked before berr so cancellation maps to transport_error.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxFail(r, ctxErr)
	}
	if berr != nil {
		if isContextErr(berr) {
			return ctxFail(r, berr)
		}
		// BAML's final parse rejects the reference text: terminal, with the SAME wrap and
		// the SAME details.raw (the extracted raw text) BAML's serving path attaches.
		r.Outcome = OutcomeParseError
		r.ErrorCompareRecorded, r.ErrorCompareMatch = true, false
		r.Err = &buildrequest.OutputParseError{Err: fmt.Errorf("buildrequest: failed to parse final result: %w", berr)}
		r.RawDiagnostic = refRaw
		return r
	}
	r.ErrorCompareRecorded, r.ErrorCompareMatch = true, true
	r.Served = true
	r.Outcome = OutcomeSuccess

	// --- the native leg, compared against the reference ---
	// SAPInvoked is the affirmative proof native extracted its channels (extraction runs
	// immediately before the parser, and only a clean extraction reaches it).
	if res.SAPInvoked {
		r.ChannelsCompared = true
		r.AssistantMatch = res.AssistantText == refParseable
		r.RawMatch = res.Raw == refRaw
		r.ReasoningMatch = res.Reasoning == refReasoning
	}
	switch {
	case aerr == nil && res.Outcome == execute.OutcomeStructured && res.SAPInvoked:
		r.StructuredBranchServed = true
		r.StructuredMatch, r.OrderMatch = parity.CompareStaticStructured(res.Structured, bamlStructured, bundle)
		r.CanonicalBytesMatch = bytes.Equal(res.Structured, bamlStructured)
		if r.AssistantMatch && r.RawMatch && r.ReasoningMatch && r.StructuredMatch && r.OrderMatch && r.CanonicalBytesMatch {
			// FULL MATCH: native owns the envelope. Its bytes equal the reference's
			// facet-for-facet, so the served body is the one the BAML leg would produce.
			r.FinalJSON = res.Structured
			r.Raw, r.Reasoning = res.Raw, res.Reasoning
			r.Winner = bamlutils.NativeStaticServeEngineNative
			return r
		}
	case aerr == nil && res.Outcome == execute.OutcomeParseDeclined:
		r.ParseDeclineServed = true
		r.Outcome = OutcomeParseDecline
	default:
		// Native failed after the response (translate / malformed body / extraction /
		// claimed parse error / a translated non-2xx over a 2xx original). The reference
		// leg already succeeded on the same bytes, so its envelope is served; native's own
		// error is superseded rather than returned, and recorded: the error facet is a
		// MISMATCH (the native comparison leg errored) alongside NativeFailureRecovered.
		r.NativeFailureRecovered = true
		r.ErrorCompareMatch = false
	}
	r.Fallback = true
	r.FinalJSON = bamlStructured
	r.Raw, r.Reasoning = refRaw, refReasoning
	r.Winner = bamlutils.NativeStaticServeEngineBAMLParse
	return r
}
