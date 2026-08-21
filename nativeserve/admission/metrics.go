package admission

import (
	"errors"
	"fmt"
	"regexp"
	"sync/atomic"

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
	// Serving-cutover S1 families. They EXTEND the set above (nothing was replaced
	// or relabelled): the seven collectors above keep recording exactly what they
	// recorded before, and these four add the surface/cohort/phase/winner view plus
	// the operator-visible configuration inventory.
	admissionPhase *prometheus.CounterVec
	winner         *prometheus.CounterVec
	configInv      *prometheus.GaugeVec
	policyInfo     *prometheus.GaugeVec
	// knownCohorts is the set of cohort IDs the published inventory declares. It is
	// the STRUCTURAL bound on the cohort label: normalizeCohort folds anything that
	// is not a declared cohort or a reserved resolution outcome onto
	// CohortUnrecognized, so no caller — however mis-wired — can widen the cohort
	// label beyond |inventory| + 2. Stored as an atomic pointer so the per-request
	// read is lock-free and a config-load publish is race-free.
	knownCohorts atomic.Pointer[map[CohortID]struct{}]
}

// Phase is the bounded admission-phase label for the serving-cutover S1 phase
// metric. It makes the no-resend ownership boundary auditable: a request either
// declined BEFORE the claim (BAML owns it, zero native sockets), or it was CLAIMED
// (exactly one native provider attempt, no BAML provider attempt afterwards) and
// reached a post-claim terminal; the same-response BAML oracle is its own phase so
// a parse-only compatibility win is never read as a native transport win.
type Phase string

const (
	// PhasePreclaimDecline: the request declined to BAML before any claim. Provably
	// zero native sockets (the decline happens before the executor is ever entered).
	PhasePreclaimDecline Phase = "preclaim_decline"
	// PhaseClaimed: the serve boundary CLAIMED the attempt — recorded exactly once,
	// at the claim point, immediately before the one native provider RoundTrip.
	PhaseClaimed Phase = "claimed"
	// PhasePostclaimTerminal: a claimed attempt reached its terminal disposition.
	// From the claim onward the terminal is a success or a typed failure — never a
	// decline and never a hidden BAML provider resend.
	PhasePostclaimTerminal Phase = "postclaim_terminal"
	// PhaseSameResponseOracle: the strict same-response BAML oracle ran over the
	// bytes the ONE native provider request returned. It is a phase of its own so
	// "BAML parsed the same bytes" is never conflated with "BAML transported".
	PhaseSameResponseOracle Phase = "same_response_oracle"
)

// Winner is the bounded winner label for the serving-cutover S1 winner metric. It
// separates the safe pre-claim BAML path from the parse-only compatibility path
// from a real native win, which is what makes the fe-v1 success criterion
// ("winner is native, parse-only fallback is zero") expressible as a query.
type Winner string

const (
	// WinnerBAMLTransport: BAML transported and served the request. Every pre-claim
	// decline lands here — it is the SAFE, expected outcome while no cohort is
	// enrolled, and in S1 it is the only outcome production can produce.
	WinnerBAMLTransport Winner = "baml_transport"
	// WinnerNative: native owned the provider request AND the final structured
	// value. The only outcome that counts as a native v1 cohort success.
	WinnerNative Winner = "native"
	// WinnerBAMLParseSameResponse: native owned the ONE provider request but BAML's
	// parse of those SAME response bytes produced the final. Safe, and NOT a
	// transport fallback (no second provider request happened) — but it is not a
	// native v1 win either, so it is labelled separately.
	WinnerBAMLParseSameResponse Winner = "baml_parse_same_response"
	// WinnerFailure: a claimed attempt terminated in a typed failure handed to the
	// outer policy. Post-claim, so it is never a BAML resend.
	WinnerFailure Winner = "failure"
)

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
	// ResponseCompareFieldTyped is the de-BAML Slice 8C STATIC shadow's typed-result
	// facet: native canonical JSON and the same-response BAML parse are each decoded
	// through the per-method DecodeNativeStaticFinal into the concrete generated return
	// type and compared, proving the typed decode (not just the canonical bytes) is
	// BAML-equivalent over BAML's captured bytes.
	ResponseCompareFieldTyped ResponseCompareField = "typed"
)

// NewMetrics constructs the collectors and registers them on reg. It fails if a
// family is already registered, surfacing a double-registration instead of
// silently shadowing it. Pass the worker's private registry.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	return newMetrics(reg, false)
}

// NewMetricsReusing is NewMetrics but REUSES an already-registered de-BAML collector
// (same name + labels) instead of failing on duplicate registration. It exists for
// the de-BAML Slice 8C SERVE profile, which installs BOTH the dynamic unary serve
// (nativeserve.New -> NewMetrics) AND the static serve (nativeserve.NewStaticServe ->
// this) via separate workerboot factories on the SAME worker registry: without reuse
// the second registration fails with "duplicate metrics collector registration
// attempted" and the worker exits before its go-plugin handshake. On a fresh registry
// (a static-only caller / a test) it behaves exactly like NewMetrics, so both serve
// implementations share ONE collector set and write the SAME series.
func NewMetricsReusing(reg prometheus.Registerer) (*Metrics, error) {
	return newMetrics(reg, true)
}

// newMetrics constructs the de-BAML collector set and registers it on reg. When
// reuse is true, an AlreadyRegisteredError for a *prometheus.CounterVec rebinds the
// field to the existing collector instead of failing, so a second de-BAML serve
// implementation on the same registry shares one collector set.
func newMetrics(reg prometheus.Registerer, reuse bool) (*Metrics, error) {
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
		admissionPhase: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "baml_rest_debaml_admission_phase_total",
			Help: "de-BAML serving admission phases by bounded surface/cohort/phase (preclaim_decline|claimed|postclaim_terminal|same_response_oracle). Bounded enums only: NO route/method/client/model/URL/alias/header/schema labels.",
		}, []string{"surface", "cohort", "phase"}),
		winner: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "baml_rest_debaml_winner_total",
			Help: "de-BAML serving winners by bounded surface/cohort/winner (baml_transport|native|baml_parse_same_response|failure). Bounded enums only: NO content, NO identifiers.",
		}, []string{"surface", "cohort", "winner"}),
		configInv: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "baml_rest_debaml_config_inventory_info",
			Help: "de-BAML privacy-safe configuration inventory: one series per (declared opaque configuration fingerprint, surface), value 1. Labels are PREDECLARED buckets only — an opaque fingerprint, a cohort bucket, a surface, a provider CLASS and an offline approval reference. NO client/model names, URLs, prompts, bodies, headers or secrets.",
		}, []string{"fingerprint", "cohort", "surface", "provider", "approval"}),
		policyInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "baml_rest_debaml_cohort_policy_info",
			Help: "de-BAML cohort policy: the number of (surface, cohort) enrollments the named policy version permits. 0 means DEFAULT-DENY with nothing enrolled (what serving-cutover S1 shipped); serving-cutover S3b ships 1 — the fe-v1 strict-OpenAI class on the dynamic unary call surface.",
		}, []string{"version"}),
	}
	// Register every collector in a fixed order, rolling back the ones already
	// registered by THIS call if a later Register fails, so a partial-registration
	// error never leaves stray de-BAML collectors on a reused registry. The success
	// path is unchanged (all collectors register in the same order as before, with the
	// S2 bedrock credential-source family appended last). When reuse is set, an
	// AlreadyRegisteredError rebinds the field to the existing collector (a second
	// de-BAML serve on the same registry) instead of failing.
	//
	// The spec's rebind takes a prometheus.Collector (rather than a *CounterVec)
	// because the S1 inventory/policy families are GAUGE vectors: rebind performs
	// the type assertion for its own field and reports whether it matched, so a
	// same-named collector of the WRONG type still falls through to the rollback +
	// error path exactly as before instead of being silently accepted.
	type collectorSpec struct {
		col    prometheus.Collector
		rebind func(prometheus.Collector) bool
	}
	counterSpec := func(c *prometheus.CounterVec, set func(*prometheus.CounterVec)) collectorSpec {
		return collectorSpec{col: c, rebind: func(existing prometheus.Collector) bool {
			v, ok := existing.(*prometheus.CounterVec)
			if ok {
				set(v)
			}
			return ok
		}}
	}
	gaugeSpec := func(g *prometheus.GaugeVec, set func(*prometheus.GaugeVec)) collectorSpec {
		return collectorSpec{col: g, rebind: func(existing prometheus.Collector) bool {
			v, ok := existing.(*prometheus.GaugeVec)
			if ok {
				set(v)
			}
			return ok
		}}
	}
	specs := []collectorSpec{
		counterSpec(m.declines, func(c *prometheus.CounterVec) { m.declines = c }),
		counterSpec(m.attempts, func(c *prometheus.CounterVec) { m.attempts = c }),
		counterSpec(m.planCompare, func(c *prometheus.CounterVec) { m.planCompare = c }),
		counterSpec(m.responseCompare, func(c *prometheus.CounterVec) { m.responseCompare = c }),
		counterSpec(m.nativeSockets, func(c *prometheus.CounterVec) { m.nativeSockets = c }),
		counterSpec(m.fallback, func(c *prometheus.CounterVec) { m.fallback = c }),
		counterSpec(m.bedrockCredSrc, func(c *prometheus.CounterVec) { m.bedrockCredSrc = c }),
		counterSpec(m.admissionPhase, func(c *prometheus.CounterVec) { m.admissionPhase = c }),
		counterSpec(m.winner, func(c *prometheus.CounterVec) { m.winner = c }),
		gaugeSpec(m.configInv, func(g *prometheus.GaugeVec) { m.configInv = g }),
		gaugeSpec(m.policyInfo, func(g *prometheus.GaugeVec) { m.policyInfo = g }),
	}
	registered := make([]prometheus.Collector, 0, len(specs))
	for _, s := range specs {
		if err := reg.Register(s.col); err != nil {
			var are prometheus.AlreadyRegisteredError
			if reuse && errors.As(err, &are) {
				// Rebind this field to the already-registered collector so both serve
				// implementations write the SAME series; do NOT track it for rollback
				// (this call did not register it).
				if s.rebind(are.ExistingCollector) {
					continue
				}
			}
			for _, done := range registered {
				reg.Unregister(done)
			}
			return nil, err
		}
		registered = append(registered, s.col)
	}
	// Pre-initialize the invariant flag="off" series to zero so the paging alert
	// `increase(baml_rest_debaml_native_sockets_total{flag="off"}[window]) > 0`
	// is well-defined and provably flat. No code path ever increments them — the
	// only increment site (RecordNativeSocket) hardcodes flag="on".
	m.nativeSockets.WithLabelValues(string(SocketFlagOff), string(NativeSocketResponded))
	m.nativeSockets.WithLabelValues(string(SocketFlagOff), string(NativeSocketTransportError))
	// Pre-initialize the ROLLOUT-STOP series to zero for the same reason: operational
	// invariant 4 says a NON-ENROLLED (surface, cohort) pair reporting a native claim
	// (or a native win) is a rollout-stop event, and an alert on `increase(...) > 0`
	// must be well-defined from the first scrape rather than only after the violation
	// it is meant to catch.
	//
	// Two families of pair are pre-initialized, and between them they cover every
	// rollout-stop shape the shipped policy makes possible:
	//
	//   - every (surface, RESERVED cohort) pair. The reserved outcomes `none` and
	//     `unrecognized` can never be enrolled at all, so a claim under either is a
	//     violation on any surface, on any policy, forever.
	//   - every (surface, ENROLLED cohort) pair the policy does NOT enroll. Serving
	//     cutover S3b enrolls fe_v1 on dynamic_call only, so a claim by fe_v1 on
	//     dynamic_stream / static call or stream / direct parse is exactly the
	//     "enrollment leaked onto another surface" event invariant 4 names — and
	//     before S3b there was no enrolled cohort for that series to exist under.
	//
	// The pair that IS enrolled is deliberately NOT pre-initialized here: it is a
	// legitimate series that the first served request creates, and seeding it to zero
	// would make "this worker has served nothing yet" and "this worker serves this
	// cohort" indistinguishable on a dashboard.
	rolloutStop := func(surface Surface, cohort CohortID) {
		m.admissionPhase.WithLabelValues(surface.Label(), string(cohort), string(PhaseClaimed))
		m.winner.WithLabelValues(surface.Label(), string(cohort), string(WinnerNative))
	}
	for _, surface := range AllSurfaces() {
		for _, cohort := range reservedCohortIDs() {
			rolloutStop(surface, cohort)
		}
	}
	for _, e := range ProductionCohortGate().Policy().Enrollments() {
		for _, surface := range AllSurfaces() {
			if surface == e.Surface {
				continue
			}
			rolloutStop(surface, e.Cohort)
		}
	}
	// Publish the SHIPPED production gate: the deployment's DECLARED configuration
	// inventory (one operator-visible row per declared record × surface) and the
	// versioned policy's enrollment count. An operator scraping a native-capable
	// worker sees both halves — which classes are declared, and that
	// "policy s3b-fe-v1-dynamic-call enrolls 1" — rather than inferring the rollout
	// state from the absence of data.
	//
	// A config-load failure is surfaced HERE, as the constructor's error, because
	// this is the first production call that needs the gate and its caller is a
	// factory workerboot turns into a boot failure. Publishing an empty dashboard
	// because a declaration failed to decode would be the quiet kind of wrong.
	if err := ProductionCohortGateError(); err != nil {
		for _, done := range registered {
			reg.Unregister(done)
		}
		return nil, fmt.Errorf("nativeserve/admission: declared configuration inventory: %w", err)
	}
	m.publishCohortGate(ProductionCohortGate())
	return m, nil
}

// recordDecline increments the declines family for a parity-decline and the
// attempts family with OutcomeDecline. mode/provider are the bounded context of
// the request that declined.
func (m *Metrics) recordDecline(mode Mode, provider providerLabel, d *Decline) {
	if m == nil || d == nil {
		return
	}
	m.declines.WithLabelValues(normalizeStage(d.Stage), normalizeReason(d.Reason)).Inc()
	m.attempts.WithLabelValues(string(normalizeMode(mode)), engineNative, string(provider), normalizeOutcome(OutcomeDecline)).Inc()
}

// recordAttempt increments only the attempts family — for a full admit
// (OutcomeAdmitted) or an unexpected planner error (OutcomePlannerError), where
// there is no decline stage/reason to record.
func (m *Metrics) recordAttempt(mode Mode, provider providerLabel, outcome Outcome) {
	if m == nil {
		return
	}
	m.attempts.WithLabelValues(string(normalizeMode(mode)), engineNative, string(provider), normalizeOutcome(outcome)).Inc()
}

// RecordPlanCompare increments the plan_compare family for one field's
// native-vs-BAML comparison result. It records only the bounded (result, field)
// enum pair — NEVER a value (no header value, body byte, URL, alias, or token).
// A nil *Metrics is a valid no-op receiver.
func (m *Metrics) RecordPlanCompare(result PlanCompareResult, field PlanCompareField) {
	if m == nil {
		return
	}
	m.planCompare.WithLabelValues(normalizePlanCompareResult(result), normalizePlanCompareField(field)).Inc()
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
	m.responseCompare.WithLabelValues(normalizeResponseCompareResult(result), normalizeResponseCompareField(field)).Inc()
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
	m.attempts.WithLabelValues(string(normalizeMode(mode)), engineNative, string(providerFromResolved(resolvedProvider)), normalizeOutcome(outcome)).Inc()
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
	m.nativeSockets.WithLabelValues(string(SocketFlagOn), normalizeSocketOutcome(outcome)).Inc()
}

// RecordFallback increments the fallback family for one native-served request
// that fell back to a BAML parse of the same response bytes. A nil *Metrics is a
// valid no-op receiver.
func (m *Metrics) RecordFallback(kind FallbackKind) {
	if m == nil {
		return
	}
	m.fallback.WithLabelValues(normalizeFallbackKind(kind)).Inc()
}

// recordBedrockCredentialSource increments the bedrock credential-source family
// once per successfully-mapped aws-bedrock admission (creds resolved, engine
// constructed). It records ONLY the bounded source enum — never a client/profile/
// region name or a credential value. A nil *Metrics is a valid no-op receiver.
func (m *Metrics) recordBedrockCredentialSource(source BedrockCredentialSource) {
	if m == nil {
		return
	}
	m.bedrockCredSrc.WithLabelValues(normalizeBedrockCredentialSource(source)).Inc()
}

// --- Serving-cutover S1 recorders -------------------------------------------
//
// THE LABEL CONTRACT (binding for every recorder in this file, old and new).
// A de-BAML metric label may ONLY be a value from a fixed enum declared in this
// package, or a PREDECLARED bucket from the configuration inventory. The following
// are PROHIBITED as labels, without exception:
//
//   - raw request or response content (bodies, assistant text, partials, deltas);
//   - client aliases and client/registry names;
//   - model names and target models;
//   - target URLs, hosts, paths or any endpoint fragment;
//   - API keys, Authorization values, or any other credential material;
//   - header names or header values;
//   - BAML method names (dynamic or static) and route templates;
//   - arbitrary or per-request schema fingerprints and content hashes.
//
// Where a stable identifier is genuinely needed it is a small predeclared cohort /
// configuration bucket — never a per-request hash. TestMetricLabelsAreBounded and
// TestNoForbiddenLabelValueEscapes enforce this over the real gathered registry.

// phaseLabelInvalid / winnerLabelInvalid are the out-of-band buckets an
// unrecognized Phase/Winner folds onto. Phase and Winner are exported string types,
// so `admission.Phase(anythingAtAll)` compiles — a cold review demonstrated exactly
// that by driving `phase="gpt-4o-acme-tuned-2026"` and
// `winner="Authorization_Bearer_sk-live-example"` into a live registry through these
// recorders. Folding at the recorder (the same thing normalizeMode has always done
// for the mode label) makes the families bounded no matter what a caller passes, and
// makes a leak impossible rather than merely discouraged.
const (
	phaseLabelInvalid  = "invalid"
	winnerLabelInvalid = "invalid"
)

// normalizePhase folds an arbitrary Phase onto the closed set.
func normalizePhase(p Phase) string {
	switch p {
	case PhasePreclaimDecline, PhaseClaimed, PhasePostclaimTerminal, PhaseSameResponseOracle:
		return string(p)
	default:
		return phaseLabelInvalid
	}
}

// normalizeWinner folds an arbitrary Winner onto the closed set.
func normalizeWinner(w Winner) string {
	switch w {
	case WinnerBAMLTransport, WinnerNative, WinnerBAMLParseSameResponse, WinnerFailure:
		return string(w)
	default:
		return winnerLabelInvalid
	}
}

// normalizeSurface folds an arbitrary Surface onto the closed set. Surface is
// lane-derived so an out-of-set value is unreachable today, but the recorders are
// exported and a bounded family must not depend on that staying true.
func normalizeSurface(s Surface) string { return s.Label() }

// labelInvalid is the single out-of-band bucket every fold below uses.
const labelInvalid = "invalid"

// EVERY exported recorder folds EVERY label argument. This is not defence in depth,
// it is the contract: `Metrics`, `NewMetrics` and the recorder methods are part of a
// PUBLIC Go module, and every label type below is an exported string alias, so
// `admission.PlanCompareField("gpt-4o-acme-tuned-2026")` compiles for any consumer.
//
// A second cold review demonstrated precisely that, resolving the published module
// from a fresh external consumer and emitting
//
//	field=gpt-4o-acme-tuned-2026
//	result=Authorization_Bearer_sk-live-example
//
// through RecordPlanCompare / RecordResponseCompare. The first round folded only the
// NEW serving-cutover recorders and left these — which is exactly the gap that
// review found. They all fold now, and TestEveryPublicRecorderFoldsHostileInput
// drives every one of them with that material.

// stageReasonForm is the shape every declared Stage/Reason constant takes:
// lowercase ASCII words joined by underscores, ≤64 bytes.
//
// Stage and Reason are folded by SHAPE rather than by membership, and that is a
// deliberate, narrower guarantee than the other folds make. There are ~100 declared
// constants across decline.go and static.go, and a hand-maintained membership switch
// would rot the moment one is added — the failure mode being a silently-dropped
// legitimate reason, which is worse than the thing it guards. What the shape fence
// buys is the property the label contract actually needs: no URL, no header value,
// no key, no model name, no prompt and nothing unbounded can be spelled this way.
//
// Exact membership IS enforced, but by the bounded-label check, which PARSES the
// declared constants out of this package (go/ast, not a text scan) and rejects any
// gathered stage/reason outside them — a check that cannot rot because it derives its
// expectation from the same declarations the constants live in. And the only writer of
// these two labels is the UNEXPORTED recordDecline, reached solely from admission's own
// predicates, which build Declines from those constants.
//
// Because the allow-list is derived from the source, one thing must never be able to
// widen it: PROSE. A token that appears only in a comment is documentation, not a
// declaration, and it must not become an accepted label value.
//
/*
	Worked example, written in a BLOCK comment on purpose:

		ReasonProseWidened Reason = "prose_widened_reason_example"

	That line is not a const spec, so the AST-based extraction never sees it and the
	token never enters the bounded-label allow-list. A text scan over this file WOULD
	see it, which is what the extraction used to be and why it changed.

	TestBlockCommentOnlyReasonIsUnallowedThroughThePublicPath is the end-to-end proof:
	it drives this exact token through every exported recorder and requires it to fold
	away, then emits it as a decline reason and requires the bounded-label check to
	REJECT the resulting series — and to ACCEPT it under a prose-widened allow-list,
	which is what makes the rejection load-bearing rather than incidental.
*/
var stageReasonForm = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// normalizeStage folds the decline stage label.
func normalizeStage(st Stage) string {
	if stageReasonForm.MatchString(string(st)) {
		return string(st)
	}
	return labelInvalid
}

// normalizeReason folds the decline reason label.
func normalizeReason(r Reason) string {
	if stageReasonForm.MatchString(string(r)) {
		return string(r)
	}
	return labelInvalid
}

// normalizePlanCompareResult folds the plan-compare result label.
func normalizePlanCompareResult(r PlanCompareResult) string {
	switch r {
	case PlanCompareMatch, PlanCompareMismatch:
		return string(r)
	default:
		return labelInvalid
	}
}

// normalizePlanCompareField folds the plan-compare field label.
func normalizePlanCompareField(f PlanCompareField) string {
	switch f {
	case PlanCompareFieldMethod, PlanCompareFieldTarget, PlanCompareFieldHost,
		PlanCompareFieldHeaders, PlanCompareFieldBody, PlanCompareFieldMeta:
		return string(f)
	default:
		return labelInvalid
	}
}

// normalizeResponseCompareResult folds the response-compare result label.
func normalizeResponseCompareResult(r ResponseCompareResult) string {
	switch r {
	case ResponseCompareMatch, ResponseCompareMismatch:
		return string(r)
	default:
		return labelInvalid
	}
}

// normalizeResponseCompareField folds the response-compare field label.
func normalizeResponseCompareField(f ResponseCompareField) string {
	switch f {
	case ResponseCompareFieldTranslate, ResponseCompareFieldAssistant, ResponseCompareFieldStructured,
		ResponseCompareFieldOrder, ResponseCompareFieldRaw, ResponseCompareFieldReasoning,
		ResponseCompareFieldError, ResponseCompareFieldTyped:
		return string(f)
	default:
		return labelInvalid
	}
}

// normalizeOutcome folds the attempts family's outcome label.
func normalizeOutcome(o Outcome) string {
	switch o {
	case OutcomeAdmitted, OutcomeDecline, OutcomePlannerError, OutcomeSuccess,
		OutcomeTransportError, OutcomeProviderError, OutcomeTranslateError,
		OutcomeParseDecline, OutcomeParseError, OutcomeInternalError:
		return string(o)
	default:
		return labelInvalid
	}
}

// normalizeSocketOutcome folds the native-socket outcome label.
func normalizeSocketOutcome(o NativeSocketOutcome) string {
	switch o {
	case NativeSocketResponded, NativeSocketTransportError:
		return string(o)
	default:
		return labelInvalid
	}
}

// normalizeFallbackKind folds the fallback kind label.
func normalizeFallbackKind(k FallbackKind) string {
	switch k {
	case FallbackParseOnly:
		return string(k)
	default:
		return labelInvalid
	}
}

// normalizeBedrockCredentialSource folds the bedrock credential-source label. The
// recorder is unexported, but the enum is not, and one unfolded call site is all it
// takes — so it folds like the rest.
func normalizeBedrockCredentialSource(src BedrockCredentialSource) string {
	switch src {
	case BedrockCredentialExplicit, BedrockCredentialEnv, BedrockCredentialProfile,
		BedrockCredentialDefaultChain, BedrockCredentialUnknown:
		return string(src)
	default:
		return labelInvalid
	}
}

// normalizeCohort folds a cohort ID onto the bounded label set: the two reserved
// resolution outcomes, plus exactly the cohorts the PUBLISHED inventory declares.
// Anything else — a caller that invented a cohort, a stale value after a config
// reload — becomes CohortUnrecognized, so the cohort label's cardinality is
// structurally |inventory| + 2 and cannot be widened from a call site.
func (m *Metrics) normalizeCohort(c CohortID) string {
	for _, r := range reservedCohortIDs() {
		if c == r {
			return string(c)
		}
	}
	if known := m.knownCohorts.Load(); known != nil {
		if _, ok := (*known)[c]; ok {
			return string(c)
		}
	}
	return string(CohortUnrecognized)
}

// publishCohortGate publishes the operator-visible control-plane view of a cohort
// gate: one config_inventory_info series per (declared configuration fingerprint,
// declared surface) and the policy's enrollment count under its version. It also
// (re)binds the cohort-label allow-list used by normalizeCohort.
//
// It REPLACES any previously published view (both gauges are reset first), so a
// config reload cannot leave a stale record advertised as current. It is called at
// metrics construction with the shipped production gate; it is UNEXPORTED because a
// gate is the one thing no caller outside this package may supply. A nil *Metrics is
// a valid no-op receiver.
//
// Everything it publishes is a predeclared bucket — the constructors in cohort.go
// already rejected anything else — so there is no redaction step here to forget.
func (m *Metrics) publishCohortGate(g *CohortGate) {
	if m == nil {
		return
	}
	known := make(map[CohortID]struct{}, g.Inventory().Len())
	m.configInv.Reset()
	for _, r := range g.Inventory().Records() {
		known[r.Cohort] = struct{}{}
		for _, s := range r.Surfaces {
			m.configInv.WithLabelValues(
				string(r.Fingerprint),
				string(r.Cohort),
				s.Label(),
				string(r.Provider),
				string(r.Approval),
			).Set(1)
		}
	}
	m.knownCohorts.Store(&known)
	m.policyInfo.Reset()
	m.policyInfo.WithLabelValues(g.Policy().Version()).Set(float64(g.Policy().Len()))
}

// RecordAdmissionPhase increments the admission-phase family for ONE phase
// observation, labelled with the bounded surface + cohort. Callers record exactly
// one phase per disposition: preclaim_decline OR (claimed then postclaim_terminal),
// plus same_response_oracle when the strict BAML oracle ran over the native
// response bytes. A nil *Metrics is a valid no-op receiver.
func (m *Metrics) RecordAdmissionPhase(surface Surface, cohort CohortID, phase Phase) {
	if m == nil {
		return
	}
	m.admissionPhase.WithLabelValues(normalizeSurface(surface), m.normalizeCohort(cohort), normalizePhase(phase)).Inc()
}

// RecordWinner increments the winner family for ONE request's terminal ownership,
// labelled with the bounded surface + cohort. Exactly one winner is recorded per
// request that reaches a disposition: baml_transport for every pre-claim decline,
// and native / baml_parse_same_response / failure for a claimed attempt. A nil
// *Metrics is a valid no-op receiver.
func (m *Metrics) RecordWinner(surface Surface, cohort CohortID, winner Winner) {
	if m == nil {
		return
	}
	m.winner.WithLabelValues(normalizeSurface(surface), m.normalizeCohort(cohort), normalizeWinner(winner)).Inc()
}

// RecordPreclaimDecline records the pair every pre-claim decline produces: the
// preclaim_decline phase and the baml_transport winner. It exists so the two can
// never drift apart at a call site — a decline that recorded a phase but no winner
// (or vice versa) would make the "every request has exactly one winner" invariant
// unprovable. A nil *Metrics is a valid no-op receiver.
func (m *Metrics) RecordPreclaimDecline(surface Surface, cohort CohortID) {
	if m == nil {
		return
	}
	m.RecordAdmissionPhase(surface, cohort, PhasePreclaimDecline)
	m.RecordWinner(surface, cohort, WinnerBAMLTransport)
}

// RecordPostclaimTerminal records the pair every CLAIMED attempt's terminal
// produces: the postclaim_terminal phase and the given winner (native /
// baml_parse_same_response / failure). Like RecordPreclaimDecline it keeps the
// phase and the winner in lockstep at one call site. A nil *Metrics is a valid
// no-op receiver.
func (m *Metrics) RecordPostclaimTerminal(surface Surface, cohort CohortID, winner Winner) {
	if m == nil {
		return
	}
	m.RecordAdmissionPhase(surface, cohort, PhasePostclaimTerminal)
	m.RecordWinner(surface, cohort, winner)
}
