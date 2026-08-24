package worker

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// De-BAML native-first direct parse — the burn-down scoreboard.
//
// The native-first dynamic final parse (direct_parse_native.go) decides, per
// request, whether the NATIVE parser's answer was byte-identical to BAML's and
// therefore served, or whether BAML's answer stood. That decision is the only
// interesting fact about the surface, and it needs to be countable: "how much of
// direct parse does native reproduce today" is the number this slice exists to
// move, and the structural reasons behind a decline are the burn-down list.
//
// The series is deliberately small and secret-free. It carries the SURFACE (a
// constant — this counter serves one endpoint class), the ENGINE that produced the
// served answer, and a STABLE STRUCTURAL REASON drawn from a closed set of
// constants below. It carries NO method name, NO cohort/fingerprint/seal/approval
// label, NO schema identity and nothing derived from the raw input or the parsed
// output — so its cardinality is bounded by construction (2 engines × the fixed
// reason list) and there is nothing in it to redact.

// directParseSurface is the constant `surface` label value. The counter is scoped
// to `/parse/{method}` and never used for another endpoint class, so the label is
// a fixed string rather than a parameter — it exists so the series joins the
// cutover's other surface-labelled series on a dashboard.
const directParseSurface = "direct_parse"

// The `engine` label: which parser's answer the request was served with.
const (
	// directParseEngineNative — the native parser's result was served. It is
	// recorded ONLY after the transition oracle proved the native bytes equal to
	// BAML's bytes for this same input, so it can never mean "native answered
	// differently and won".
	directParseEngineNative = "native"
	// directParseEngineBAML — BAML's result (or BAML's error) was served. Every
	// decline lands here, tagged with the structural reason it declined for.
	directParseEngineBAML = "baml"
)

// The `reason` label: a stable, structural account of the disposition. These are
// the burn-down categories — each names a distinct shape of native/BAML
// disagreement (or a distinct shape of native abstention), so a dashboard split by
// reason says which family of work would move the number next.
const (
	// directParseReasonAgreement — native and BAML produced byte-identical results
	// and native's bytes were served. The only reason recorded under
	// engine="native".
	directParseReasonAgreement = "agreement"

	// directParseReasonNativeUnsupported — the native parser returned
	// bamlutils.ErrDeBAMLParseUnsupported: the schema shape or the raw syntax is
	// outside its cut-line. This is the ordinary, expected decline and the biggest
	// burn-down bucket.
	directParseReasonNativeUnsupported = "native_unsupported"

	// directParseReasonNativeErrorBAMLOK — native CLAIMED a parse failure on input
	// BAML parsed successfully. BAML's success is served. This is a prevented
	// out-claim: without the oracle the request would have failed.
	directParseReasonNativeErrorBAMLOK = "native_error_baml_ok"

	// directParseReasonNativeOKBAMLError — native CLAIMED a result on input BAML
	// rejected. BAML's error is served. This is the most dangerous prevented
	// out-claim shape: without the oracle the request would have returned data
	// where stock BAML returns an error.
	directParseReasonNativeOKBAMLError = "native_ok_baml_error"

	// directParseReasonBothError — both parsers rejected the input. BAML's error is
	// served, because the error BYTES a client sees are BAML's and native's message
	// is not proven to match them. Error-class parity holds; message parity is not
	// claimed, so native does not win here.
	directParseReasonBothError = "both_error"

	// directParseReasonResultDrift — both parsers succeeded and their serialized
	// results differ. BAML's result is served. This is the shape the oracle exists
	// for. It covers two populations that the comparison cannot tell apart and
	// deliberately treats alike: real semantic drift, and a difference the HOST's
	// downstream normalization (absent-optional injection, reorder/sort) would have
	// erased before the wire. Both decline, because the worker compares what it
	// actually produces and does not model the host's pipeline.
	directParseReasonResultDrift = "result_drift"

	// directParseReasonBAMLResultUnusable — BAML parsed the input but its result
	// could not be serialized at all (a non-finite float has no JSON form, so a
	// recovered NaN/Inf fails the marshal). The request fails exactly as it always
	// did; there is simply no BAML payload to compare against, so native cannot
	// win. Counted so the surface's accounting stays complete: every request that
	// enters the bridge records exactly one disposition.
	directParseReasonBAMLResultUnusable = "baml_result_unusable"

	// directParseReasonNativeResultUnusable — native reported success but its JSON
	// could not be re-encoded into the dynamic-output envelope the surface returns
	// (not a JSON object, or undecodable). BAML's result is served. A parser bug
	// rather than a semantic disagreement, kept distinct so it cannot hide inside
	// the drift bucket.
	directParseReasonNativeResultUnusable = "native_result_unusable"
)

// directParseMetrics is the handler-scoped counter set for the native-first
// direct-parse bridge. One CounterVec; nothing else about the surface is metered
// here, because nothing else about it is a rollout fact.
type directParseMetrics struct {
	dispositions *prometheus.CounterVec
}

// newDirectParseMetrics registers (or re-uses) the direct-parse disposition
// counter on reg.
//
// Re-use rather than failure on AlreadyRegisteredError is deliberate: Config.Metrics
// is public and an in-process host may hand the SAME registry to more than one
// Handler. Two handlers sharing a registry must share the counter and keep
// counting, not fail construction or silently drop one handler's observations.
func newDirectParseMetrics(reg prometheus.Registerer) (*directParseMetrics, error) {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "debaml_direct_parse_total",
		Help: "De-BAML native-first direct parse dispositions by served engine and structural reason.",
	}, []string{"surface", "engine", "reason"})
	if reg == nil {
		return &directParseMetrics{dispositions: c}, nil
	}
	if err := reg.Register(c); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			existing, ok := already.ExistingCollector.(*prometheus.CounterVec)
			if !ok {
				return nil, err
			}
			return &directParseMetrics{dispositions: existing}, nil
		}
		return nil, err
	}
	return &directParseMetrics{dispositions: c}, nil
}

// record counts one direct-parse disposition. A nil receiver is a no-op so a
// handler constructed without metrics still serves.
func (m *directParseMetrics) record(engine, reason string) {
	if m == nil || m.dispositions == nil {
		return
	}
	m.dispositions.WithLabelValues(directParseSurface, engine, reason).Inc()
}
