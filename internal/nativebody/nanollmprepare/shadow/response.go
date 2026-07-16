package shadow

// De-BAML cutover Slice 5 SAME-response shadow oracle. After the S4 request-plan
// comparison MATCHES and BAML serves the request, this runs the native and BAML
// response parsers INDEPENDENTLY over BAML's ALREADY-FETCHED status+body and
// records a bounded response_compare result per facet (NO values). It owns NO
// transport: it opens no socket, issues no RoundTrip, and makes NO second
// provider request — the single BAML send remains the sole provider request, and
// BAML's envelope is served byte-identically regardless of what this observes.
//
//   - Native leg: a fresh request-scoped nanollm engine (rebuilt for the SAME
//     admitted client) TranslateResponse(alias, status, body) -> assistant/raw/
//     reasoning extraction -> native SAP (internal/debaml.Parse), via the factored
//     transport-free execute.ConsumeResponse.
//   - BAML leg: the assistant/raw/reasoning extracted the BAML way from the same
//     body, plus the BAML-ONLY final-parse closure (no native-first hybrid, no
//     native->BAML->native recursion) threaded in from the generated seam.
//
// Every facet is compared and recorded independently so a per-facet drift is
// zero-tolerance-alertable: translate (native produced a comparable 2xx JSON),
// assistant (extracted parseable text), structured (final JSON, key-order
// ignored), order (schema field order), raw + reasoning (/call-with-raw
// channels), and error (the pipeline itself errored). The oracle extracts with
// reasoning ON for BOTH legs so /call-with-raw parity is proven even for a plain
// `call`, extending the Phase 6c structured-only differential to the full raw +
// reasoning envelope.

import (
	"context"
	"errors"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/parity"
)

// errNoBAMLOnlyParse marks a same-response comparison that reached the response
// phase without a BAML-only parse closure. The generated shadow seam always
// threads one, so this is a wiring bug, not an ordinary outcome; recording it as
// an `error` facet keeps a broken oracle observable instead of silently counting
// a mismatch it could not compute.
var errNoBAMLOnlyParse = errors.New("shadow: same-response comparison missing BAML-only parse closure")

// compareResponse runs the same-response native-vs-BAML comparison for one
// admitted, plan-matched request and records response_compare per facet. It is
// strictly non-authoritative: it never returns a value and never affects the
// served result. A panic in the native FFI / parse work must not escape into the
// orchestrator (which already guards it), so it is recovered here too and
// recorded as an `error` mismatch rather than lost — the comparator's own record
// is the truthful account of what happened.
func (c *Comparator) compareResponse(ctx context.Context, req bamlutils.NativeShadowRequest, status int, body []byte) {
	defer func() {
		if recover() != nil {
			c.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		}
	}()

	// Native leg — rebuild the request-scoped engine for the SAME admitted client
	// (same alias/openai/target config) purely to translate; it opens no socket.
	client, _, cerr := admission.NewResponseClient(req.Registry, shadowInternalAlias)
	if cerr != nil {
		c.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		return
	}
	defer client.Close()

	nativeRes, nerr := execute.ConsumeResponse(ctx, execute.ConsumeConfig{
		Client:       client,
		Alias:        shadowInternalAlias,
		Parse:        debaml.Parse,
		OutputSchema: req.OutputSchema,
		// Extract reasoning on BOTH legs regardless of the request mode so the
		// /call-with-raw reasoning channel is compared even for a plain `call`.
		IncludeReasoning: true,
	}, status, body)

	// BAML leg — extract the assistant/raw/reasoning the BAML serving path would,
	// from the SAME raw body, then run the BAML-ONLY parse closure on the assistant
	// text. Reasoning ON to mirror the native leg.
	bamlParseable, bamlRaw, bamlReasoning, xerr := buildrequest.ExtractResponseContentBytes("openai", body, true)

	var bamlStructured []byte
	var berr error
	switch {
	case req.BAMLOnlyParse == nil:
		berr = errNoBAMLOnlyParse
	case xerr == nil:
		bamlStructured, berr = req.BAMLOnlyParse(ctx, bamlParseable)
	}

	// A failure in EITHER leg's pipeline is an observable `error` facet; the other
	// facets cannot be compared meaningfully, so record error and stop. A clean run
	// records error=match so the ratio of clean comparisons stays visible.
	if nerr != nil || xerr != nil || berr != nil {
		c.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		return
	}
	c.metrics.RecordResponseCompare(admission.ResponseCompareMatch, admission.ResponseCompareFieldError)

	// translate: native TranslateResponse produced a comparable 2xx JSON. Both a
	// clean structured claim and a SAP decline mean translate itself succeeded; a
	// provider-error / invalid-body outcome on a body BAML served as 2xx is a
	// translate drift.
	translateOK := nativeRes.Outcome == execute.OutcomeStructured || nativeRes.Outcome == execute.OutcomeParseDeclined
	c.recordResponse(translateOK, admission.ResponseCompareFieldTranslate)

	// assistant / raw / reasoning: extractor-channel parity between the native
	// (translated body) and BAML (raw body) legs.
	c.recordResponse(nativeRes.AssistantText == bamlParseable, admission.ResponseCompareFieldAssistant)
	c.recordResponse(nativeRes.Raw == bamlRaw, admission.ResponseCompareFieldRaw)
	c.recordResponse(nativeRes.Reasoning == bamlReasoning, admission.ResponseCompareFieldReasoning)

	// structured / order: the final structured output, compared semantically
	// (key-order ignored) and then by schema field order. Only a clean native
	// structured claim is comparable against BAML's parse; a native decline where
	// BAML parsed is a real structured drift.
	if nativeRes.Outcome == execute.OutcomeStructured {
		structuredMatch, orderMatch := compareStructured(nativeRes.Structured, bamlStructured, req.OutputSchema)
		c.recordResponse(structuredMatch, admission.ResponseCompareFieldStructured)
		c.recordResponse(orderMatch, admission.ResponseCompareFieldOrder)
	} else {
		c.recordResponse(false, admission.ResponseCompareFieldStructured)
		c.recordResponse(false, admission.ResponseCompareFieldOrder)
	}
}

// recordResponse records one response_compare facet, mapping a bool to the
// bounded match/mismatch result.
func (c *Comparator) recordResponse(match bool, field admission.ResponseCompareField) {
	c.metrics.RecordResponseCompare(responseResult(match), field)
}

func responseResult(match bool) admission.ResponseCompareResult {
	if match {
		return admission.ResponseCompareMatch
	}
	return admission.ResponseCompareMismatch
}

// compareStructured delegates to the shared parity.CompareStructured so the
// shadow oracle and the native serve path apply the IDENTICAL structured/order
// comparison policy (absent-optional injection + key-order-insensitive deep-equal
// for structured, schema-reordered byte compare for order).
func compareStructured(nativeFlat, bamlFlat []byte, schema *bamlutils.DynamicOutputSchema) (structuredMatch, orderMatch bool) {
	return parity.CompareStructured(nativeFlat, bamlFlat, schema)
}
