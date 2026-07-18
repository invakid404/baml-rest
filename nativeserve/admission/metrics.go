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

// providerLabel is the bounded provider label for the attempts metric (§9). The
// five known nanollm provider classes each get their own label so a non-openai
// decline is observable per provider; anything else is folded to "other", and a
// decline before the provider is resolved is "unknown". The BAML `aws-bedrock`
// spelling folds onto `bedrock` (observability normalization, not admission).
// This keeps the provider label a bounded enum, never an unbounded free string,
// and it OBSERVES but never DECIDES admission.
type providerLabel string

const (
	providerOpenAI    providerLabel = "openai"
	providerAnthropic providerLabel = "anthropic"
	providerBedrock   providerLabel = "bedrock"
	providerCerebras  providerLabel = "cerebras"
	providerCohere    providerLabel = "cohere"
	providerOther     providerLabel = "other"
	providerUnknown   providerLabel = "unknown"
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
	// OutcomeInternalError: an UNEXPECTED post-claim panic (in the executor,
	// translation, or parser) that the serve guard turned into a terminal failure.
	// It is a distinct bounded label so a pre-parse panic (e.g. an executor/
	// transport panic) is never misclassified as parse_error — a panic anywhere in
	// the claimed pipeline reads honestly as an internal error to alert on.
	OutcomeInternalError Outcome = "internal_error"
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
	nativeSockets   *prometheus.CounterVec
	fallback        *prometheus.CounterVec
	bedrockCredSrc  *prometheus.CounterVec
}

// NativeSocketFlag is the bounded flag label for the native_sockets metric. It
// records whether the umbrella flag was resolved on or off when a socket was
// claimed. It is ALWAYS "on" at the only increment site (RecordNativeSocket) —
// the serve path is installed only when the flag is enabled — so any "off"
// increment is an invariant violation that is UNREACHABLE by construction, not
// merely alertable. The "off" series is pre-initialized to zero so the paging
// alert expression `increase(...{flag="off"}[window]) > 0` is well-defined.
type NativeSocketFlag string

const (
	SocketFlagOn  NativeSocketFlag = "on"
	SocketFlagOff NativeSocketFlag = "off"
)

// NativeSocketOutcome is the bounded outcome label for the native_sockets metric:
// whether the single claimed exact attempt produced an HTTP response (any status)
// or failed at the transport layer (dial/reset/timeout/read — a socket may still
// have opened, so it is counted).
type NativeSocketOutcome string

const (
	// NativeSocketResponded: the exact attempt produced an HTTP response (2xx or
	// non-2xx) — the socket completed a round trip.
	NativeSocketResponded NativeSocketOutcome = "responded"
	// NativeSocketTransportError: the exact attempt failed at the transport layer
	// (dial refusal/reset/timeout/body-read). Counted as a socket-possible attempt.
	NativeSocketTransportError NativeSocketOutcome = "transport_error"
)

// FallbackKind is the bounded kind label for the fallback metric. The first
// serving surface records only parse_only — native owned the one provider request
// but BAML parse-only produced the final (native SAP declined, or structured
// output drifted and BAML's parse is served for safety).
type FallbackKind string

const (
	FallbackParseOnly FallbackKind = "parse_only"
)

// BedrockCredentialSource is the bounded source label for the S2 aws-bedrock
// credential-source metric (§9). It records WHICH documented credential source a
// successfully-mapped Bedrock admission resolved through — never a client name,
// profile name, region, or any credential value. The full AWS chain (owner
// decision A) is folded into three observable classes: `explicit` (a declared
// static access/secret pair), `profile` (a declared shared-config profile), and
// `default_chain` (nothing declared — the AWS default chain: env / shared config
// / ECS-IMDS / SSO). `env` and `unknown` are declared for a stable enum but the
// mapper folds ambient-env resolution into default_chain (env is part of the
// default chain under decision A). The label OBSERVES; it never decides admission.
type BedrockCredentialSource string

const (
	BedrockCredentialExplicit     BedrockCredentialSource = "explicit"
	BedrockCredentialEnv          BedrockCredentialSource = "env"
	BedrockCredentialProfile      BedrockCredentialSource = "profile"
	BedrockCredentialDefaultChain BedrockCredentialSource = "default_chain"
	BedrockCredentialUnknown      BedrockCredentialSource = "unknown"
)

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
		nativeSockets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "baml_rest_debaml_native_sockets_total",
			Help: "de-BAML native provider sockets claimed, by bounded flag/outcome. flag is always \"on\" (the serve path is unreachable while the umbrella flag is off); any flag=\"off\" increment is a paging invariant violation.",
		}, []string{"flag", "outcome"}),
		fallback: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "baml_rest_debaml_fallback_total",
			Help: "de-BAML native-served requests that fell back to a BAML parse of the same response bytes, by bounded kind.",
		}, []string{"kind"}),
		bedrockCredSrc: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "baml_rest_debaml_bedrock_credential_source_total",
			Help: "de-BAML aws-bedrock admissions by the documented credential source they resolved through (explicit/env/profile/default_chain/unknown). NO client/profile/region names, NO credential values.",
		}, []string{"source"}),
	}
	// Register every collector in a fixed order, rolling back the ones already
	// registered if a later Register fails, so a partial-registration error never
	// leaves stray de-BAML collectors on a reused registry. The success path is
	// unchanged (all collectors register in the same order as before, with the S2
	// bedrock credential-source family appended last).
	registered := make([]prometheus.Collector, 0, 7)
	for _, c := range []prometheus.Collector{
		m.declines, m.attempts, m.planCompare, m.responseCompare, m.nativeSockets, m.fallback, m.bedrockCredSrc,
	} {
		if err := reg.Register(c); err != nil {
			for _, done := range registered {
				reg.Unregister(done)
			}
			return nil, err
		}
		registered = append(registered, c)
	}
	// Pre-initialize the invariant flag="off" series to zero so the paging alert
	// `increase(baml_rest_debaml_native_sockets_total{flag="off"}[window]) > 0`
	// is well-defined and provably flat. No code path ever increments them — the
	// only increment site (RecordNativeSocket) hardcodes flag="on".
	m.nativeSockets.WithLabelValues(string(SocketFlagOff), string(NativeSocketResponded))
	m.nativeSockets.WithLabelValues(string(SocketFlagOff), string(NativeSocketTransportError))
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

// RecordServeOutcome records ONE terminal serving outcome on the attempts family
// for a native serve attempt that got past admission (a claimed attempt or a
// post-claim failure). It folds the arbitrary resolved provider string into the
// bounded provider label internally, so callers outside the package need not
// spell the unexported providerLabel. The S3 admission `admitted` outcome is NOT
// recorded on the serve path, so success is never double-counted. A nil *Metrics
// is a valid no-op receiver.
func (m *Metrics) RecordServeOutcome(mode Mode, resolvedProvider string, outcome Outcome) {
	if m == nil {
		return
	}
	m.attempts.WithLabelValues(string(normalizeMode(mode)), engineNative, string(providerFromResolved(resolvedProvider)), string(outcome)).Inc()
}

// RecordNativeSocket increments the native_sockets family EXACTLY ONCE per
// claimed exact attempt (including transport/dial failures). The flag label is
// hardcoded "on": this recorder is reachable ONLY from the serve path, which is
// installed only when the umbrella flag is enabled, so a flag="off" increment is
// UNREACHABLE by construction (the "off" series stays at the zero pre-initialized
// in NewMetrics). A nil *Metrics is a valid no-op receiver.
func (m *Metrics) RecordNativeSocket(outcome NativeSocketOutcome) {
	if m == nil {
		return
	}
	m.nativeSockets.WithLabelValues(string(SocketFlagOn), string(outcome)).Inc()
}

// RecordFallback increments the fallback family for one native-served request
// that fell back to a BAML parse of the same response bytes. A nil *Metrics is a
// valid no-op receiver.
func (m *Metrics) RecordFallback(kind FallbackKind) {
	if m == nil {
		return
	}
	m.fallback.WithLabelValues(string(kind)).Inc()
}

// recordBedrockCredentialSource increments the bedrock credential-source family
// once per successfully-mapped aws-bedrock admission (creds resolved, engine
// constructed). It records ONLY the bounded source enum — never a client/profile/
// region name or a credential value. A nil *Metrics is a valid no-op receiver.
func (m *Metrics) recordBedrockCredentialSource(source BedrockCredentialSource) {
	if m == nil {
		return
	}
	m.bedrockCredSrc.WithLabelValues(string(source)).Inc()
}
