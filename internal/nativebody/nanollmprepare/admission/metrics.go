package admission

import (
	"github.com/prometheus/client_golang/prometheus"
)

// engineNative is the stable, secret-free engine label for the native admission
// engine. It matches the neutral capability's engine class ("native") rather
// than a specific implementation name, so the metric never encodes a version or
// a high-cardinality identity.
const engineNative = "native"

// Mode is the bounded request-mode label for the attempts metric and the
// admission input. Only ModeCall is admissible; every other value declines at
// StageMode. The set is fixed, so the label stays bounded.
type Mode string

const (
	ModeCall          Mode = "call"
	ModeCallWithRaw   Mode = "call_with_raw"
	ModeStream        Mode = "stream"
	ModeStreamWithRaw Mode = "stream_with_raw"
	// ModeUnknown normalizes any unrecognized mode to a single bounded label so
	// a malformed input can never widen the metric's cardinality.
	ModeUnknown Mode = "unknown"
)

// normalizeMode collapses an arbitrary mode value to the bounded label set.
func normalizeMode(m Mode) Mode {
	switch m {
	case ModeCall, ModeCallWithRaw, ModeStream, ModeStreamWithRaw:
		return m
	default:
		return ModeUnknown
	}
}

// providerLabel is the bounded provider label for the attempts metric. The
// admitted surface is openai; anything else is folded to "other", and a decline
// before the provider is resolved is "unknown". This keeps the provider label a
// three-value enum, not an unbounded free string.
type providerLabel string

const (
	providerOpenAI  providerLabel = "openai"
	providerOther   providerLabel = "other"
	providerUnknown providerLabel = "unknown"
)

// Outcome is the bounded terminal-outcome label for the attempts metric. In the
// no-send admission path only OutcomeAdmitted, OutcomeDecline, and
// OutcomePlannerError are reachable (nothing is ever sent); the remaining
// send-path outcomes are declared so the enum matches the scope's bounded set
// and never needs a schema change when a send path lands.
type Outcome string

const (
	// OutcomeAdmitted: the plan was proven up to — but NOT including — the exact
	// RoundTrip, and deliberately not sent. The terminal disposition of a full
	// admit on the no-send path.
	OutcomeAdmitted Outcome = "admitted"
	// OutcomeDecline: a support/provenance parity-decline to BAML.
	OutcomeDecline Outcome = "decline"
	// OutcomePlannerError: an unexpected native planner/FFI error before any
	// socket (e.g. nanollm.New failed). Availability-first BAML fallback, but
	// distinguished so it can alert/block rollout instead of reading as normal
	// unsupported traffic.
	OutcomePlannerError Outcome = "planner_error"

	// Send-path outcomes — declared for a bounded, stable enum; never incremented
	// on the no-send path.
	OutcomeSuccess        Outcome = "success"
	OutcomeTransportError Outcome = "transport_error"
	OutcomeProviderError  Outcome = "provider_error"
	OutcomeTranslateError Outcome = "translate_error"
	OutcomeParseDecline   Outcome = "parse_decline"
	OutcomeParseError     Outcome = "parse_error"
)

// Metrics are the bounded-enum de-BAML admission collectors, registered on the
// worker's private Prometheus registry (the same *prometheus.Registry type the
// worker builds via worker.NewMetricsRegistry). Every label is a fixed enum from
// this package — no method/client/model/URL/alias/request-id/free-text — so the
// families stay bounded-cardinality. A nil *Metrics is a valid no-op receiver so
// the predicate can run without a registry in lightweight tests.
type Metrics struct {
	declines        *prometheus.CounterVec
	attempts        *prometheus.CounterVec
	planCompare     *prometheus.CounterVec
	responseCompare *prometheus.CounterVec
}

// PlanCompareResult is the bounded result label for the plan_compare metric: a
// per-field native-vs-BAML request-plan comparison either matches or mismatches.
type PlanCompareResult string

const (
	PlanCompareMatch    PlanCompareResult = "match"
	PlanCompareMismatch PlanCompareResult = "mismatch"
)

// PlanCompareField is the bounded field label for the plan_compare metric. It
// names WHICH facet of the request plan was compared; the set is fixed so the
// family stays bounded-cardinality. `meta` is the catch-all for a structural
// comparison result not attributable to a single wire field (e.g. BAML's plan
// could not be built for comparison).
type PlanCompareField string

const (
	PlanCompareFieldMethod  PlanCompareField = "method"
	PlanCompareFieldTarget  PlanCompareField = "target"
	PlanCompareFieldHost    PlanCompareField = "host"
	PlanCompareFieldHeaders PlanCompareField = "headers"
	PlanCompareFieldBody    PlanCompareField = "body"
	PlanCompareFieldMeta    PlanCompareField = "meta"
)

// ResponseCompareResult is the bounded result label for the response_compare
// metric: a per-field native-vs-BAML SAME-response comparison either matches or
// mismatches. It shares the string values with PlanCompareResult but is a
// distinct type so the two comparison families cannot be crossed by accident.
type ResponseCompareResult string

const (
	ResponseCompareMatch    ResponseCompareResult = "match"
	ResponseCompareMismatch ResponseCompareResult = "mismatch"
)

// ResponseCompareField is the bounded field label for the response_compare
// metric. It names WHICH facet of the SAME (BAML-fetched) response was compared
// native-vs-BAML; the set is fixed so the family stays bounded-cardinality:
//
//   - translate:  native TranslateResponse produced a comparable 2xx JSON body;
//   - assistant:  the extracted assistant (parseable) text matched;
//   - structured: the final structured output matched semantically (key order ignored);
//   - order:      the structured output's schema field order matched;
//   - raw:        the /call-with-raw raw channel matched;
//   - reasoning:  the /call-with-raw reasoning channel matched;
//   - error:      the comparison pipeline itself errored (native or BAML leg) — a
//     catch-all recorded as a mismatch so a broken oracle leg is observable, never
//     silently counted as a match.
type ResponseCompareField string

const (
	ResponseCompareFieldTranslate  ResponseCompareField = "translate"
	ResponseCompareFieldAssistant  ResponseCompareField = "assistant"
	ResponseCompareFieldStructured ResponseCompareField = "structured"
	ResponseCompareFieldOrder      ResponseCompareField = "order"
	ResponseCompareFieldRaw        ResponseCompareField = "raw"
	ResponseCompareFieldReasoning  ResponseCompareField = "reasoning"
	ResponseCompareFieldError      ResponseCompareField = "error"
)

// NewMetrics constructs the collectors and registers them on reg. It fails if a
// family is already registered, surfacing a double-registration instead of
// silently shadowing it. Pass the worker's private registry.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		declines: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "baml_rest_debaml_declines_total",
			Help: "de-BAML native admission declines to BAML, by fixed-enum stage and reason.",
		}, []string{"stage", "reason"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "baml_rest_debaml_attempts_total",
			Help: "de-BAML native admission attempts, by bounded mode/engine/provider/outcome.",
		}, []string{"mode", "engine", "provider", "outcome"}),
		planCompare: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "baml_rest_debaml_plan_compare_total",
			Help: "de-BAML one-send shadow native-vs-BAML request-plan comparisons, by bounded result/field. NO values.",
		}, []string{"result", "field"}),
		responseCompare: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "baml_rest_debaml_response_compare_total",
			Help: "de-BAML same-response shadow native-vs-BAML response comparisons (translate/assistant/structured/order/raw/reasoning/error), by bounded result/field. NO values.",
		}, []string{"result", "field"}),
	}
	if err := reg.Register(m.declines); err != nil {
		return nil, err
	}
	if err := reg.Register(m.attempts); err != nil {
		return nil, err
	}
	if err := reg.Register(m.planCompare); err != nil {
		return nil, err
	}
	if err := reg.Register(m.responseCompare); err != nil {
		return nil, err
	}
	return m, nil
}

// recordDecline increments the declines family for a parity-decline and the
// attempts family with OutcomeDecline. mode/provider are the bounded context of
// the request that declined.
func (m *Metrics) recordDecline(mode Mode, provider providerLabel, d *Decline) {
	if m == nil || d == nil {
		return
	}
	m.declines.WithLabelValues(string(d.Stage), string(d.Reason)).Inc()
	m.attempts.WithLabelValues(string(normalizeMode(mode)), engineNative, string(provider), string(OutcomeDecline)).Inc()
}

// recordAttempt increments only the attempts family — for a full admit
// (OutcomeAdmitted) or an unexpected planner error (OutcomePlannerError), where
// there is no decline stage/reason to record.
func (m *Metrics) recordAttempt(mode Mode, provider providerLabel, outcome Outcome) {
	if m == nil {
		return
	}
	m.attempts.WithLabelValues(string(normalizeMode(mode)), engineNative, string(provider), string(outcome)).Inc()
}

// RecordPlanCompare increments the plan_compare family for one field's
// native-vs-BAML comparison result. It records only the bounded (result, field)
// enum pair — NEVER a value (no header value, body byte, URL, alias, or token).
// A nil *Metrics is a valid no-op receiver.
func (m *Metrics) RecordPlanCompare(result PlanCompareResult, field PlanCompareField) {
	if m == nil {
		return
	}
	m.planCompare.WithLabelValues(string(result), string(field)).Inc()
}

// RecordResponseCompare increments the response_compare family for one field's
// native-vs-BAML SAME-response comparison result. Like RecordPlanCompare it
// records only the bounded (result, field) enum pair — NEVER a value (no
// assistant text, structured output, raw/reasoning bytes, or token). A nil
// *Metrics is a valid no-op receiver.
func (m *Metrics) RecordResponseCompare(result ResponseCompareResult, field ResponseCompareField) {
	if m == nil {
		return
	}
	m.responseCompare.WithLabelValues(string(result), string(field)).Inc()
}
