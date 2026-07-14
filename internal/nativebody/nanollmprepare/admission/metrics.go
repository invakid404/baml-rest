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
	declines *prometheus.CounterVec
	attempts *prometheus.CounterVec
}

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
	}
	if err := reg.Register(m.declines); err != nil {
		return nil, err
	}
	if err := reg.Register(m.attempts); err != nil {
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
