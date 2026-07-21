package canary

// De-BAML Slice 8C — static unary Stage-1 SHADOW comparator.
//
// ShadowStatic is the STAGE-1 twin of ServeStatic and the response-comparing twin of
// the no-send OBSERVE seam: BAML remains the SOLE provider sender, and native
// consumes BAML's ALREADY-FETCHED response bytes to compare its own translate /
// extract / static SAP / typed decode against BAML's parse of the SAME bytes — with
// ZERO native sends. It runs the full pre-socket admission predicate + the strict BAML
// `Request.<Method>` plan compare (via the shared AdmitStaticClaim) and, on a full
// plan match, returns an OnResponse continuation the generated seam threads onto its
// declined NativeCallOutcome.OnResponseShadow. The orchestrator invokes OnResponse
// with BAML's fetched (status, body) AFTER BAML serves; it opens no socket, issues no
// RoundTrip, and never changes what BAML serves. This is the MEASURED shadow-parse the
// cutover gate consumes — not inferred from a serve claim.

import (
	"context"
	"errors"
	"reflect"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/nativeserve/admission"
	"github.com/invakid404/baml-rest/nativeserve/execute"
	"github.com/invakid404/baml-rest/nativeserve/parity"
)

// staticShadowAlias is the fixed, secret-free internal nanollm alias the shadow's
// translate-only engine is configured under (an implausible token so it never
// collides with a target model or client name).
const staticShadowAlias = "__debaml_static_shadow_internal_alias__"

// stageShadowStatic is the bounded stage token the static shadow returns on its
// terminal (always-decline) disposition.
const stageShadowStatic = "shadow_static"

// NewStaticShadowFunc is the factory a SHADOW-profile worker injects via
// workerboot.Options.NativeStaticShadowFactory. It registers (reuse-tolerant) the
// bounded de-BAML collectors and returns the neutral bamlutils.NativeStaticShadowFunc
// that drives the Stage-1 comparison. It reuses the SAME serve core (admission +
// execute + parity) so the shadow and serve legs apply the identical parse/compare.
func NewStaticShadowFunc(reg prometheus.Registerer) (bamlutils.NativeStaticShadowFunc, error) {
	m, err := admission.NewMetricsReusing(reg)
	if err != nil {
		return nil, err
	}
	s := NewServer(m, llmhttp.NewExactExecutor(nil))
	return s.ShadowStatic, nil
}

// ShadowStatic is the bamlutils.NativeStaticShadowFunc. It runs the no-send static
// admission predicate + plan compare; on a full plan match it records the plan match
// and returns the same-response OnResponse continuation; on any decline it returns the
// bounded admission stage/reason. It ALWAYS declines so BAML serves — it opens no
// socket and RoundTrips nothing.
func (s *Server) ShadowStatic(ctx context.Context, inv bamlutils.NativeStaticInvocation) (result bamlutils.NativeStaticShadowResult) {
	// Non-authoritative: a panic in the native admission/FFI work must never escape
	// and turn a BAML-served request into a shadow-induced failure.
	defer func() {
		if recover() != nil {
			result = bamlutils.NativeStaticShadowResult{Stage: stageShadowStatic, Reason: reasonServedBAMLPanic}
		}
	}()

	if ctx.Err() != nil {
		return bamlutils.NativeStaticShadowResult{Stage: stageShadowStatic, Reason: reasonServedBAMLCtx}
	}

	claim, err := s.staticAdmitClaim(ctx, toStaticAdmissionInput(inv))
	if err != nil {
		var d *admission.StaticDecline
		if errors.As(err, &d) {
			return bamlutils.NativeStaticShadowResult{Stage: d.Stage, Reason: d.Reason}
		}
		return bamlutils.NativeStaticShadowResult{Stage: stagePlanner, Reason: reasonPlannerError}
	}
	bundle := claim.Bundle
	// The shadow NEVER sends: the prepared engine was only needed for the plan-match
	// decision. Close it; OnResponse rebuilds a fresh translate-only engine over BAML's
	// bytes (mirroring the dynamic shadow's rebuild).
	claim.Close()

	// The claim required the S4 plan match; record it so the shadow plan-matched tally
	// is a MEASURED admission result, not an inference.
	s.metrics.RecordPlanCompare(admission.PlanCompareMatch, admission.PlanCompareFieldBody)

	return bamlutils.NativeStaticShadowResult{
		Stage:  stageShadowStatic,
		Reason: reasonServedBAML,
		OnResponse: func(rctx context.Context, status int, body []byte) {
			s.compareStaticResponse(rctx, inv, bundle, status, body)
		},
	}
}

// compareStaticResponse runs the same-response native-vs-BAML comparison over BAML's
// already-fetched (status, body) for one admitted, plan-matched request and records
// response_compare per facet (NO values). It is strictly non-authoritative: it never
// returns a value, never affects the served result, and opens no socket. A panic in
// the native FFI / parse work is recovered here (the orchestrator also guards it) and
// recorded as an `error` mismatch.
func (s *Server) compareStaticResponse(ctx context.Context, inv bamlutils.NativeStaticInvocation, bundle *schema.Bundle, status int, body []byte) {
	defer func() {
		if recover() != nil {
			s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		}
	}()

	// Native leg — a FRESH translate-only engine for the SAME descriptor client, purely
	// to TranslateResponse over BAML's bytes; it opens no socket. The parser is the
	// schema-neutral static Bundle closure (debaml owns the parse).
	client, _, cerr := admission.NewStaticResponseClient(inv.Descriptor, staticShadowAlias)
	if cerr != nil {
		s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		return
	}
	defer client.Close()

	nativeRes, nerr := execute.ConsumeResponse(ctx, execute.ConsumeConfig{
		Client: client,
		Alias:  staticShadowAlias,
		ParseResponse: func(pctx context.Context, raw string) ([]byte, error) {
			parsed, perr := debaml.ParseStaticBundle(pctx, bundle, raw)
			if perr != nil {
				return nil, perr
			}
			return parsed.JSON, nil
		},
		// Extract reasoning on BOTH legs regardless of request mode so the reasoning
		// channel is compared even for a plain `call`.
		IncludeReasoning: true,
	}, status, body)

	// BAML leg — extract assistant/raw/reasoning the BAML serving path would, from the
	// SAME raw body, then run the BAML-ONLY parse closure on the assistant text.
	bamlParseable, bamlRaw, bamlReasoning, xerr := buildrequest.ExtractResponseContentBytes("openai", body, true)

	var bamlStructured []byte
	var berr error
	switch {
	case inv.BAMLOnlyParse == nil:
		berr = errNoBAMLOnlyParse
	case xerr == nil:
		bamlStructured, berr = inv.BAMLOnlyParse(ctx, bamlParseable)
	}

	// A failure in EITHER leg's pipeline is an observable `error` facet; the other
	// facets cannot be compared meaningfully. A clean run records error=match.
	if nerr != nil || xerr != nil || berr != nil {
		s.metrics.RecordResponseCompare(admission.ResponseCompareMismatch, admission.ResponseCompareFieldError)
		return
	}
	s.metrics.RecordResponseCompare(admission.ResponseCompareMatch, admission.ResponseCompareFieldError)

	// translate: native produced a comparable 2xx JSON (a clean structured claim OR a
	// SAP decline both mean translate itself succeeded).
	translateOK := nativeRes.Outcome == execute.OutcomeStructured || nativeRes.Outcome == execute.OutcomeParseDeclined
	s.recordResponse(translateOK, admission.ResponseCompareFieldTranslate)

	// assistant / raw / reasoning: extractor-channel parity.
	s.recordResponse(nativeRes.AssistantText == bamlParseable, admission.ResponseCompareFieldAssistant)
	s.recordResponse(nativeRes.Raw == bamlRaw, admission.ResponseCompareFieldRaw)
	s.recordResponse(nativeRes.Reasoning == bamlReasoning, admission.ResponseCompareFieldReasoning)

	// structured / order / typed: the final structured output, compared semantically +
	// by class-field order, plus the concrete typed decode. Only a clean native
	// structured claim is comparable; a native SAP decline (e.g. a bare-string return)
	// records these as N/A rather than a fabricated mismatch — the assistant facet
	// already proves the same text was extracted, and the serve path serves the string
	// via the BAML parse-only comparator.
	if nativeRes.Outcome == execute.OutcomeStructured {
		structuredMatch, orderMatch := parity.CompareStaticStructured(nativeRes.Structured, bamlStructured, bundle)
		s.recordResponse(structuredMatch, admission.ResponseCompareFieldStructured)
		s.recordResponse(orderMatch, admission.ResponseCompareFieldOrder)
		s.recordResponse(staticTypedResultMatches(inv, nativeRes.Structured, bamlStructured), admission.ResponseCompareFieldTyped)
	}
}

// staticTypedResultMatches reports whether the per-method DecodeNativeStaticFinal maps
// the native canonical JSON and the same-response BAML parse into EQUAL concrete Go
// return values — the typed-result facet. When the shadow seam threaded no decoder
// (DecodeNativeFinal nil) it falls back to canonical byte equality (a strict superset
// signal), so the facet is never silently skipped. Both inputs are already the
// flattened canonical shape the differential proves.
func staticTypedResultMatches(inv bamlutils.NativeStaticInvocation, nativeCanonical, bamlCanonical []byte) bool {
	if inv.DecodeNativeFinal == nil {
		return string(nativeCanonical) == string(bamlCanonical)
	}
	nv, nerr := inv.DecodeNativeFinal(nativeCanonical)
	bv, berr := inv.DecodeNativeFinal(bamlCanonical)
	if nerr != nil || berr != nil {
		return false
	}
	return reflect.DeepEqual(nv, bv)
}
