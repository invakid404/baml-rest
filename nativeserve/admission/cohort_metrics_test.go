package admission

// De-BAML serving cutover S1 — the CARDINALITY + REDACTION proof for the bounded
// telemetry contract.
//
// The label contract in metrics.go says two things: every de-BAML label value comes
// from a fixed enum or a predeclared inventory bucket (CARDINALITY), and none of
// them can carry request-derived material (REDACTION). Both are checked here over a
// REAL gathered registry rather than by reading the call sites, and both are paired
// with a mutation bite: a deliberately leaked label must make the checker fail.

import (
	"context"
	"go/ast"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

const deBAMLFamilyPrefix = "baml_rest_debaml_"

// secretBearingRegistry builds a dynamic client registry whose every field is a
// value the label contract forbids: a real-shaped API key, a target URL, a model
// name, and a client name. Driving admission with it and then scanning the gathered
// registry is how "no forbidden label escapes" is PROVEN rather than asserted.
func secretBearingRegistry() (*bamlutils.ClientRegistry, []string) {
	const (
		clientName = "AcmeProdOpenAIClient"
		apiKey     = "sk-live-51H8xQzZzZzZzZzZzZzZ"
		baseURL    = "https://acme-internal.example.test/v1"
		model      = "gpt-4o-acme-tuned-2026"
	)
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{{
			Name:     clientName,
			Provider: "openai",
			Options: map[string]any{
				"model":    model,
				"base_url": baseURL,
				"api_key":  apiKey,
			},
		}},
	}
	return reg, []string{clientName, apiKey, baseURL, model}
}

// forbiddenLabelMaterial is what may never appear in a metric LABEL: everything the
// request carried, plus the method names the contract prohibits even though they are
// source constants rather than secrets (a method name is a cardinality and
// information-disclosure hazard on a label, wherever it came from).
//
// It is deliberately WIDER than the decline-detail scan's list: a Decline detail is
// a structural diagnostic, and saying "internal method is not Baml_Rest_Dynamic" —
// a fixed sentence naming the one admitted method — discloses nothing about the
// request. Conflating the two would force that sentence to become useless.
func forbiddenLabelMaterial() []string {
	_, secrets := secretBearingRegistry()
	return append(append([]string(nil), secrets...), "Baml_Rest_Dynamic", "Request.AcmeSecretMethod")
}

// driveEveryLaneOnce runs one request through every admission lane plus the two
// terminal recorders, so the gathered registry contains at least one series in every
// de-BAML family that S1 can produce.
func driveEveryLaneOnce(t *testing.T, m *Metrics, reg *bamlutils.ClientRegistry) {
	t.Helper()
	ctx := context.Background()
	a := NewAdmitter(m, llmhttp.NewExactExecutor(&proofCountingTransport{}))

	dyn := Input{
		WorkerCapable: true, RequestAPIPresent: true, OnBuildRequestRoute: true, FlagEnabled: true,
		Method: dynamicMethod, Mode: ModeCall, SingleLeaf: true, ResolvedProvider: "openai",
		Registry: reg, Alias: "__proof_alias__",
	}
	if _, err := a.Admit(ctx, dyn); err == nil {
		t.Fatal("the dynamic lane admitted under the production gate")
	}
	if _, err := a.AdmitClaim(ctx, dyn); err == nil {
		t.Fatal("the dynamic claim lane admitted under the production gate")
	}
	stream := dyn
	stream.Mode = ModeStream
	if _, err := a.AdmitStreamClaim(ctx, stream); err == nil {
		t.Fatal("the dynamic stream lane admitted under the production gate")
	}
	// A mode the unary lane declines BEFORE the cohort gate, so the pre-existing
	// stage/reason buckets are represented too.
	raw := dyn
	raw.Mode = ModeCallWithRaw
	if _, err := a.Admit(ctx, raw); err == nil {
		t.Fatal("call-with-raw was admitted")
	}

	// The static lanes record no admission counters of their own (they report bounded
	// observations to their caller), but they must still be driven: a leak there would
	// surface through the Decline details the caller propagates.
	static := StaticInput{
		WorkerCapable: true, RequestAPIPresent: true, OnBuildRequestRoute: true, FlagEnabled: true,
		RouteKind: RouteKindStatic, Method: "Request.AcmeSecretMethod", Mode: bamlutils.NativeStaticModeFinal, SingleLeaf: true,
	}
	_ = AdmitStatic(ctx, static)
	if _, err := AdmitStaticClaim(ctx, static); err == nil {
		t.Fatal("the static claim lane admitted under the production gate")
	}

	// The serve-boundary recorders, on every surface, for both terminal shapes.
	for _, s := range AllSurfaces() {
		m.RecordPreclaimDecline(s, CohortNone)
		m.RecordPreclaimDecline(s, CohortUnrecognized)
		m.RecordAdmissionPhase(s, CohortNone, PhaseClaimed)
		m.RecordAdmissionPhase(s, CohortNone, PhaseSameResponseOracle)
		for _, w := range []Winner{WinnerNative, WinnerBAMLParseSameResponse, WinnerFailure} {
			m.RecordPostclaimTerminal(s, CohortNone, w)
		}
	}
	// The pre-existing collectors, so the bounded-label check covers the whole set.
	m.RecordPlanCompare(PlanCompareMatch, PlanCompareFieldBody)
	m.RecordResponseCompare(ResponseCompareMismatch, ResponseCompareFieldStructured)
	m.RecordNativeSocket(NativeSocketResponded)
	m.RecordFallback(FallbackParseOnly)
	m.RecordServeOutcome(ModeCall, "openai", OutcomeSuccess)
	m.recordBedrockCredentialSource(BedrockCredentialExplicit)
}

// allowedLabelValues is the complete set of label values the de-BAML families may
// carry, derived from the declared enums plus the published inventory buckets. It is
// built from the SAME sources the recorders use, so adding an enum value updates
// both sides; adding a free-form label updates neither and fails the check.
//
// The SHIPPED gate's buckets are always included, on top of whichever gate the caller
// is publishing. That is not a loophole — it is what NewMetrics actually does: it
// publishes ProductionCohortGate() and pre-initializes the rollout-stop series for
// the shipped policy's enrolled cohorts, so every registry carries those buckets
// whatever a test publishes afterwards. Before serving cutover S3b the shipped gate
// was empty and the distinction did not arise.
func allowedLabelValues(gate *CohortGate) map[string]bool {
	allow := map[string]bool{}
	add := func(vs ...string) {
		for _, v := range vs {
			allow[v] = true
		}
	}
	addGate := func(g *CohortGate) {
		for _, r := range g.Inventory().Records() {
			add(string(r.Fingerprint), string(r.Cohort), string(r.Provider), string(r.Approval))
		}
		add(g.Policy().Version())
	}
	for _, s := range AllSurfaces() {
		add(s.Label())
	}
	add(surfaceLabelInvalid)
	add(string(CohortNone), string(CohortUnrecognized))
	addGate(gate)
	addGate(ProductionCohortGate())
	add(string(PhasePreclaimDecline), string(PhaseClaimed), string(PhasePostclaimTerminal), string(PhaseSameResponseOracle))
	add(string(WinnerBAMLTransport), string(WinnerNative), string(WinnerBAMLParseSameResponse), string(WinnerFailure))
	add(string(ModeCall), string(ModeCallWithRaw), string(ModeStream), string(ModeStreamWithRaw), string(ModeUnknown))
	add(engineNative)
	add(string(providerOpenAI), string(providerAnthropic), string(providerBedrock), string(providerCerebras),
		string(providerCohere), string(providerOther), string(providerUnknown))
	add(string(OutcomeAdmitted), string(OutcomeDecline), string(OutcomePlannerError), string(OutcomeSuccess),
		string(OutcomeTransportError), string(OutcomeProviderError), string(OutcomeTranslateError),
		string(OutcomeParseDecline), string(OutcomeParseError), string(OutcomeInternalError))
	add(string(PlanCompareMatch), string(PlanCompareMismatch))
	add(string(PlanCompareFieldMethod), string(PlanCompareFieldTarget), string(PlanCompareFieldHost),
		string(PlanCompareFieldHeaders), string(PlanCompareFieldBody), string(PlanCompareFieldMeta))
	add(string(ResponseCompareMatch), string(ResponseCompareMismatch))
	add(string(ResponseCompareFieldTranslate), string(ResponseCompareFieldAssistant), string(ResponseCompareFieldStructured),
		string(ResponseCompareFieldOrder), string(ResponseCompareFieldRaw), string(ResponseCompareFieldReasoning),
		string(ResponseCompareFieldError), string(ResponseCompareFieldTyped))
	add(string(SocketFlagOn), string(SocketFlagOff))
	add(string(NativeSocketResponded), string(NativeSocketTransportError))
	add(string(FallbackParseOnly))
	add(string(BedrockCredentialExplicit), string(BedrockCredentialEnv), string(BedrockCredentialProfile),
		string(BedrockCredentialDefaultChain), string(BedrockCredentialUnknown))
	// Every decline stage/reason the fixed enums declare. They are enumerated from the
	// package source so a new bounded reason does not need a second declaration here —
	// and, more importantly, so a FREE-FORM stage/reason cannot slip past by being
	// added to this list instead of to the enum.
	for _, v := range declaredStagesAndReasons() {
		add(v)
	}
	return allow
}

// gatherDeBAML returns the de-BAML metric families from reg.
func gatherDeBAML(t *testing.T, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	all, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make([]*dto.MetricFamily, 0, len(all))
	for _, mf := range all {
		if strings.HasPrefix(mf.GetName(), deBAMLFamilyPrefix) {
			out = append(out, mf)
		}
	}
	if len(out) == 0 {
		t.Fatal("no de-BAML metric families were gathered — the checks below would be vacuous")
	}
	return out
}

// checkBoundedLabels reports every (family, label, value) whose value is outside the
// allowed set. It returns findings rather than failing, so the mutation bite can
// assert it DOES find a deliberate leak.
func checkBoundedLabels(families []*dto.MetricFamily, allow map[string]bool) []string {
	var findings []string
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if !allow[lp.GetValue()] {
					findings = append(findings, mf.GetName()+"{"+lp.GetName()+"="+lp.GetValue()+"}")
				}
			}
		}
	}
	return findings
}

// checkNoForbiddenSubstring reports every label value (and family name) containing
// one of the forbidden request-derived strings.
func checkNoForbiddenSubstring(families []*dto.MetricFamily, forbidden []string) []string {
	var findings []string
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				for _, f := range forbidden {
					if strings.Contains(lp.GetValue(), f) || strings.Contains(lp.GetName(), f) {
						findings = append(findings, mf.GetName()+"{"+lp.GetName()+"="+lp.GetValue()+"}")
					}
				}
			}
		}
	}
	return findings
}

// TestMetricLabelsAreBounded drives every lane and recorder and proves every
// gathered de-BAML label value is a declared enum value or a predeclared bucket.
func TestMetricLabelsAreBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	clients, _ := secretBearingRegistry()
	driveEveryLaneOnce(t, m, clients)

	families := gatherDeBAML(t, reg)
	if findings := checkBoundedLabels(families, allowedLabelValues(ProductionCohortGate())); len(findings) > 0 {
		t.Fatalf("unbounded label value(s): %v", findings)
	}

	// BITE: record ONE free-form label value and require the checker to catch it.
	// Without this the check could pass because it never looked at anything.
	m.declines.WithLabelValues("stage-"+"AcmeProdOpenAIClient", "reason-free-form").Inc()
	if findings := checkBoundedLabels(gatherDeBAML(t, reg), allowedLabelValues(ProductionCohortGate())); len(findings) == 0 {
		t.Fatal("a deliberately free-form label value was NOT caught: the cardinality check is vacuous")
	}
}

// TestNoForbiddenLabelValueEscapes is the REDACTION proof: admission is driven with
// a registry whose every field is forbidden material, and nothing derived from it
// reaches a label.
func TestNoForbiddenLabelValueEscapes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	clients, secrets := secretBearingRegistry()
	driveEveryLaneOnce(t, m, clients)

	families := gatherDeBAML(t, reg)
	if findings := checkNoForbiddenSubstring(families, forbiddenLabelMaterial()); len(findings) > 0 {
		t.Fatalf("forbidden material reached a metric label: %v", findings)
	}

	// BITE: leak one secret into a label and require the checker to catch it. This is
	// the "mutate the redaction to leak" mutation, applied to the observable surface
	// the redaction protects.
	m.declines.WithLabelValues("capability", secrets[1]).Inc()
	if findings := checkNoForbiddenSubstring(gatherDeBAML(t, reg), forbiddenLabelMaterial()); len(findings) == 0 {
		t.Fatal("a deliberately leaked API key was NOT caught: the redaction check is vacuous")
	}
}

// TestDeclineDetailsAreSecretFree covers the OTHER escape route: a Decline's Detail
// is not a label, but it is propagated into logs and error strings, so it must be
// structural only.
func TestDeclineDetailsAreSecretFree(t *testing.T) {
	ctx := context.Background()
	a := NewAdmitter(nil, llmhttp.NewExactExecutor(&proofCountingTransport{}))
	clients, secrets := secretBearingRegistry()

	var details []string
	collect := func(err error) {
		if d, ok := err.(*Decline); ok {
			details = append(details, d.Detail, d.Error())
		}
		if d, ok := err.(*StaticDecline); ok {
			details = append(details, d.Error())
		}
	}
	base := Input{
		WorkerCapable: true, RequestAPIPresent: true, OnBuildRequestRoute: true, FlagEnabled: true,
		Method: dynamicMethod, Mode: ModeCall, SingleLeaf: true, ResolvedProvider: "openai",
		Registry: clients, Alias: "__proof_alias__",
	}
	for _, mutate := range []func(Input) Input{
		func(in Input) Input { return in },
		func(in Input) Input { in.Cohort = CohortInput{Fingerprint: ConfigFingerprint(secrets[1])}; return in },
		func(in Input) Input { in.FlagEnabled = false; return in },
		func(in Input) Input { in.Mode = ModeStream; return in },
		func(in Input) Input { in.Method = secrets[0]; return in },
	} {
		_, err := a.Admit(ctx, mutate(base))
		collect(err)
	}
	_, err := AdmitStaticClaim(ctx, StaticInput{
		WorkerCapable: true, RequestAPIPresent: true, OnBuildRequestRoute: true, FlagEnabled: true,
		RouteKind: RouteKindStatic, Method: secrets[0], Mode: bamlutils.NativeStaticModeFinal, SingleLeaf: true,
	})
	collect(err)

	if len(details) < 6 {
		t.Fatalf("collected only %d decline details; the scan below would be weak", len(details))
	}
	for _, d := range details {
		for _, s := range secrets {
			if strings.Contains(d, s) {
				t.Errorf("decline detail leaked %q: %s", s, d)
			}
		}
	}
}

// TestCohortLabelCardinalityIsStructurallyBounded proves the cohort label cannot be
// widened from a call site: a cohort the published inventory does not declare folds
// onto `unrecognized`, so the label's cardinality is |inventory| + 2 whatever a
// caller passes.
func TestCohortLabelCardinalityIsStructurallyBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	for _, invented := range []CohortID{"invented", "AcmeProdOpenAIClient", "sk-live-51H8xQ", ""} {
		if got := m.normalizeCohort(invented); got != string(CohortUnrecognized) {
			t.Errorf("normalizeCohort(%q) = %q, want unrecognized", invented, got)
		}
	}
	if got := m.normalizeCohort(CohortNone); got != string(CohortNone) {
		t.Errorf("normalizeCohort(none) = %q, want none", got)
	}
	// After publishing a gate that DECLARES a cohort, that cohort — and only it —
	// becomes a permitted label.
	m.publishCohortGate(testGate(t, "proof"))
	if got := m.normalizeCohort("proof"); got != "proof" {
		t.Errorf("normalizeCohort(proof) = %q after publishing its inventory, want it unchanged", got)
	}
	if got := m.normalizeCohort("still_invented"); got != string(CohortUnrecognized) {
		t.Errorf("normalizeCohort(still_invented) = %q, want unrecognized", got)
	}
	// Re-publishing the production (empty) gate RETRACTS it — a config reload cannot
	// leave a stale cohort advertised as declared.
	m.publishCohortGate(ProductionCohortGate())
	if got := m.normalizeCohort("proof"); got != string(CohortUnrecognized) {
		t.Errorf("normalizeCohort(proof) = %q after retraction, want unrecognized", got)
	}
}

// TestFreshMetricsAdvertiseDefaultDeny pins what an operator sees on a native-capable
// worker running S1: a named policy enrolling ZERO cohorts, no inventory rows at all,
// and the rollout-stop alert series pre-initialized to zero so
// `increase(...{phase="claimed",cohort="none"}[w]) > 0` is well-defined from the
// first scrape.
func TestFreshMetricsAdvertiseDefaultDeny(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := NewMetrics(reg); err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	families := map[string]*dto.MetricFamily{}
	for _, mf := range gatherDeBAML(t, reg) {
		families[mf.GetName()] = mf
	}

	policy := families["baml_rest_debaml_cohort_policy_info"]
	if policy == nil || len(policy.GetMetric()) != 1 {
		t.Fatalf("cohort_policy_info: want exactly one series, got %v", policy)
	}
	// Serving cutover S3b: the shipped policy enrolls EXACTLY the one fe-v1 tuple, so
	// a fresh scrape advertises 1 — not 0, and not "however many". The count is read
	// from the manifest rather than hardcoded, so this stays a statement about "the
	// gauge publishes what the policy says" while the exactness of the policy itself
	// is pinned by TestProductionManifestsAreTheOneFeV1TupleAndTheBuilderIsTheOnlyPath.
	if got, want := policy.GetMetric()[0].GetGauge().GetValue(), float64(len(productionEnrollments())); got != want {
		t.Fatalf("cohort_policy_info = %v, want %v enrollment(s)", got, want)
	}
	if got := policy.GetMetric()[0].GetLabel()[0].GetValue(); got != ProductionCohortPolicyVersion {
		t.Fatalf("cohort_policy_info version = %q, want %q", got, ProductionCohortPolicyVersion)
	}
	// One operator-visible row per declared record × declared surface. The fe-v1
	// record declares ONE surface, so a second row here would mean the shipped record
	// quietly gained a surface — which is half of an unreviewed enrollment.
	inv := families["baml_rest_debaml_config_inventory_info"]
	wantRows := 0
	for _, r := range productionInventoryRecords() {
		wantRows += len(r.Surfaces)
	}
	if inv == nil || len(inv.GetMetric()) != wantRows {
		t.Fatalf("config_inventory_info has %d series, want %d (one per declared record × surface)", len(inv.GetMetric()), wantRows)
	}

	phase := families["baml_rest_debaml_admission_phase_total"]
	if phase == nil {
		t.Fatal("admission_phase_total was not pre-initialized")
	}
	claimedSeries := 0
	enrolledPairSeeded := false
	for _, m := range phase.GetMetric() {
		labels := map[string]string{}
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels["phase"] != string(PhaseClaimed) {
			continue
		}
		claimedSeries++
		if got := m.GetCounter().GetValue(); got != 0 {
			t.Errorf("pre-initialized claimed series %v = %v, want 0", labels, got)
		}
		for _, e := range productionEnrollments() {
			if labels["surface"] == e.Surface.Label() && labels["cohort"] == string(e.Cohort) {
				enrolledPairSeeded = true
			}
		}
	}
	// Every (surface × reserved cohort) pair, plus every surface an ENROLLED cohort
	// is NOT enrolled on — the two rollout-stop shapes an `increase(...) > 0` alert
	// has to be well-defined over from the first scrape.
	want := len(AllSurfaces()) * len(reservedCohortIDs())
	want += len(productionEnrollments()) * (len(AllSurfaces()) - 1)
	if claimedSeries != want {
		t.Fatalf("pre-initialized claimed series = %d, want %d (surface × reserved cohort, plus each enrolled cohort's non-enrolled surfaces)", claimedSeries, want)
	}
	// The ENROLLED pair must NOT be seeded: it is a legitimate series a served
	// request creates, and seeding it would make "served nothing yet" and "serves
	// this cohort" read identically on a dashboard.
	if enrolledPairSeeded {
		t.Error("the ENROLLED (surface, cohort) pair was pre-initialized; a rollout-stop seed must cover only pairs that may never claim")
	}
}

// TestPublishCohortGatePublishesOnlyBoundedBuckets proves the operator-visible
// inventory view is exactly the declared records, with no field invented and none
// dropped, and that its label values stay inside the allowed set.
func TestPublishCohortGatePublishesOnlyBoundedBuckets(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gate := testGate(t, "proof")
	m.publishCohortGate(gate)

	var inv *dto.MetricFamily
	for _, mf := range gatherDeBAML(t, reg) {
		if mf.GetName() == "baml_rest_debaml_config_inventory_info" {
			inv = mf
		}
	}
	if inv == nil {
		t.Fatal("config_inventory_info was not published")
	}
	wantSeries := 0
	for _, r := range gate.Inventory().Records() {
		wantSeries += len(r.Surfaces)
	}
	if len(inv.GetMetric()) != wantSeries {
		t.Fatalf("config_inventory_info has %d series, want %d (one per record × surface)", len(inv.GetMetric()), wantSeries)
	}
	for _, series := range inv.GetMetric() {
		if got := series.GetGauge().GetValue(); got != 1 {
			t.Errorf("inventory series value = %v, want 1", got)
		}
		labels := map[string]string{}
		for _, lp := range series.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels["fingerprint"] != string(proofConfigFingerprint) || labels["cohort"] != "proof" {
			t.Errorf("inventory series labels = %v, want the declared proof record", labels)
		}
		if labels["provider"] != string(ConfigProviderOpenAI) || labels["approval"] != "DEBAML-602" {
			t.Errorf("inventory series labels = %v, want the declared provider class + approval reference", labels)
		}
	}
	if findings := checkBoundedLabels(gatherDeBAML(t, reg), allowedLabelValues(gate)); len(findings) > 0 {
		t.Fatalf("the published inventory carried an unbounded label: %v", findings)
	}
}

// declaredStagesAndReasons reads the fixed Stage/Reason enum VALUES out of the
// package's DECLARATIONS. Enumerating them from the source (rather than restating
// them) is what makes the bounded-label check catch a free-form stage/reason: a value
// that is not a declared constant has nowhere to be listed.
//
// It parses rather than greps. A bot review made the point that a regex over source
// TEXT lets prose into the allow-list: a doc comment showing `StageExample Stage =
// "anything_at_all"` would have widened the set of label values this suite is willing
// to accept, and a block-comment example would slip past a line-comment stripper
// besides. Constants come from the AST, where a comment is not a declaration.
//
// A parse failure yields an EMPTY set, which the vacuity control below fails on — the
// scan cannot go quiet, and an empty allow-list rejects rather than admits.
func declaredStagesAndReasons() []string {
	sources, err := parsePackageAST()
	if err != nil {
		return nil
	}
	return constStringsOfType(sources, "Stage", "Reason")
}

// TestDeclaredStagesAndReasonsScanIsNotVacuous is the control for the scan above: if
// the regex stopped matching, the bounded-label check would silently start allowing
// nothing (and then fail on every real stage/reason), or — worse, if the check were
// ever inverted — allow everything. Pin a floor and a couple of known members.
func TestDeclaredStagesAndReasonsScanIsNotVacuous(t *testing.T) {
	got := declaredStagesAndReasons()
	if len(got) < 50 {
		t.Fatalf("the stage/reason source scan found only %d constants; the enum is far larger, so the scan is broken", len(got))
	}
	found := map[string]bool{}
	for _, v := range got {
		found[v] = true
	}
	for _, want := range []string{"cohort", "cohort_not_enrolled", "flag_disabled", "plan_match", "route_kind_not_static"} {
		if !found[want] {
			t.Errorf("the stage/reason scan missed the declared constant %q", want)
		}
	}
}

// --- the block-comment-only stage/reason, end to end -------------------------

// proseOnlyLabelToken is a stage/reason-SHAPED token that exists in this package's
// shipped source ONLY inside a block comment (metrics.go, above stageReasonForm). It is
// not a declared constant of any type.
//
// It is the subject of the proof below. A cold review made the point that the AST
// extraction was shown to skip such a token but never shown, end to end, that the token
// is actually refused when it reaches the public telemetry path — and that is the claim
// the label contract makes. So this token is carried through the whole path rather than
// checked at the helper.
const proseOnlyLabelToken = "prose_widened_reason_example"

// proseWidenedAllowList is the MUTANT derivation: the bounded-label allow-list as it
// would be if stage/reason constants were scanned out of the source TEXT — the way they
// were before the AST rewrite — so a value written in a comment widens it.
//
// It exists to make the rejection below discriminating. Without it, "the allow-list does
// not contain the token" could be true simply because nothing in the package spells the
// token at all; with it, the token demonstrably IS in the source, the old derivation
// demonstrably WOULD have accepted it, and the current one demonstrably does not.
func proseWidenedAllowList(t *testing.T) (allow map[string]bool, scanned []string) {
	t.Helper()
	textScan := regexp.MustCompile(`(?m)^\s*\w+\s+(?:Stage|Reason)\s*=\s*"([a-z0-9_]+)"`)
	allow = allowedLabelValues(ProductionCohortGate())
	for _, src := range packageRawSources(t) {
		for _, m := range textScan.FindAllStringSubmatch(src, -1) {
			allow[m[1]] = true
			scanned = append(scanned, m[1])
		}
	}
	return allow, scanned
}

// TestBlockCommentOnlyReasonIsUnallowedThroughThePublicPath is the end-to-end proof that
// prose cannot widen the bounded-label allow-list — stated where it matters, on the
// gathered Prometheus series rather than on a helper's return value.
//
// Four arms, each load-bearing:
//
//  1. SUBJECT. The token really is in the shipped source, and really is only prose.
//  2. ALLOW-LIST. It is absent from the declared enum and from the allow-list built
//     from it.
//  3. PUBLIC PATH. Driven through every exported recorder the way a consumer would, it
//     never becomes a live label — it folds, exactly like any other out-of-allow-list
//     value.
//  4. BITE, both directions. Emitted as a decline reason — which the SHAPE fold passes
//     by design, so the allow-list is the only thing left — it DOES reach a label, and
//     the bounded-label check REJECTS it. Under the prose-widened allow-list the very
//     same series is ACCEPTED. So a regression in either the prose filter or the fold
//     turns this red: widen the allow-list with prose and arm 2 fails; let the token
//     reach a label and arm 3 fails; stop rejecting a leaked undeclared reason and arm 4
//     fails.
func TestBlockCommentOnlyReasonIsUnallowedThroughThePublicPath(t *testing.T) {
	sources := packageAST(t)

	// (1) SUBJECT CONTROL. If the block comment is ever deleted, this proof loses its
	// subject — and says so, rather than passing vacuously.
	file := blockCommentContaining(sources, proseOnlyLabelToken)
	if file == "" {
		t.Fatalf("no block comment in this package contains %q; the proof has lost its subject. "+
			"Restore the worked example above stageReasonForm in metrics.go, or retire this test deliberately.",
			proseOnlyLabelToken)
	}
	for _, declared := range constStringsOfType(sources, "Stage", "Reason") {
		if declared == proseOnlyLabelToken {
			t.Fatalf("%s declares %q as a real Stage/Reason constant; the token must exist ONLY as prose",
				file, proseOnlyLabelToken)
		}
	}

	// (2) ALLOW-LIST ARM.
	for _, v := range declaredStagesAndReasons() {
		if v == proseOnlyLabelToken {
			t.Fatalf("the declared stage/reason set contains %q, which is written only in a comment: "+
				"prose has widened the bounded-label allow-list", proseOnlyLabelToken)
		}
	}
	if allowedLabelValues(ProductionCohortGate())[proseOnlyLabelToken] {
		t.Fatalf("the bounded-label allow-list accepts %q, which no declaration spells", proseOnlyLabelToken)
	}

	// (3) PUBLIC PATH ARM. Every exported recorder, driven with the token by a consumer
	// holding a *Metrics built by the exported constructor.
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	driveEveryPublicRecorder(m, proseOnlyLabelToken)
	families := gatherDeBAML(t, reg)
	if hasLabelValue(families, proseOnlyLabelToken) {
		t.Errorf("%q became a live Prometheus label through a PUBLIC recorder", proseOnlyLabelToken)
	}
	if findings := checkBoundedLabels(families, allowedLabelValues(ProductionCohortGate())); len(findings) > 0 {
		t.Errorf("driving %q through the public recorders produced an unbounded label: %v", proseOnlyLabelToken, findings)
	}
	// Not vacuous: the token reached the recorders and landed in the out-of-band bucket
	// rather than being dropped before it got there.
	if !hasLabelValue(families, labelInvalid) {
		t.Fatal("no out-of-band label was produced; the token never reached the public recorders")
	}

	// (4) BITE. The stage/reason labels are folded by SHAPE, and this token is
	// shape-valid — so if a decline ever carried it, it would reach the label and only
	// the allow-list would stand in the way. Emit exactly that, through the production
	// decline emitter, and require the check to catch it.
	leakReg := prometheus.NewRegistry()
	leakM, err := NewMetrics(leakReg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	leakM.recordDecline(ModeCall, providerOpenAI, &Decline{Stage: StageCohort, Reason: Reason(proseOnlyLabelToken)})
	leaked := gatherDeBAML(t, leakReg)
	if !hasLabelValue(leaked, proseOnlyLabelToken) {
		t.Fatalf("the shape fold rejected %q, so the allow-list arm below proves nothing about it; "+
			"pick a token the shape fence admits", proseOnlyLabelToken)
	}
	if findings := checkBoundedLabels(leaked, allowedLabelValues(ProductionCohortGate())); len(findings) == 0 {
		t.Fatal("a block-comment-only reason reached a live label and the bounded-label check ACCEPTED it")
	}
	// The other direction: under the prose-permissive derivation the same series passes.
	// That is the false-green the AST rewrite removed, and it is what makes the rejection
	// above attributable to the rewrite rather than to some unrelated bound.
	mutantAllow, scanned := proseWidenedAllowList(t)
	if !slices.Contains(scanned, proseOnlyLabelToken) {
		t.Fatalf("the text scan did not pick %q out of the source; the mutant is not a mutant "+
			"and the comparison below is meaningless", proseOnlyLabelToken)
	}
	if findings := checkBoundedLabels(leaked, mutantAllow); len(findings) > 0 {
		t.Fatalf("the prose-widened allow-list still rejected the leak (%v); the two derivations do not "+
			"actually differ on this token, so arm 4 does not attribute the rejection to the AST rewrite", findings)
	}
}

// --- the PUBLIC recorders, driven with hostile input -------------------------

// hostileLabelInputs is the material a cold review actually pushed through the new
// recorders and the inventory publisher: raw content, an alias, a model name, a URL,
// an API key, an Authorization value, a method name and a schema fingerprint. Every
// one of them must be folded or refused, never labelled.
func hostileLabelInputs() []string {
	return []string{
		"gpt-4o-acme-tuned-2026",
		"Authorization_Bearer_sk-live-example",
		"sk-live-51H8xQzZzZzZzZzZzZzZ",
		"Bearer sk-live-51H8xQ",
		"https://acme-internal.example.test/v1/chat/completions",
		"AcmeProdOpenAIClient",
		"Baml_Rest_Dynamic",
		"Request.AcmeSecretMethod",
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"{\"messages\":[{\"role\":\"user\",\"content\":\"secret prompt\"}]}",
		strings.Repeat("x", 4096),
		"",
	}
}

// TestPublicRecordersFoldHostileInput is the review's direct ask: drive the EXPORTED
// recorders — not the old private collectors — with forbidden material in every
// position a caller controls, then gather and prove nothing escaped.
//
// Phase and Winner are exported string types, so `admission.Phase(anything)`
// compiles; the first draft wrote them straight into labels and a review published
// `phase=gpt-4o-acme-tuned-2026` from a live registry. The normalizers are what make
// that impossible, and this test is what keeps them.
func TestPublicRecordersFoldHostileInput(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	for _, hostile := range hostileLabelInputs() {
		for _, s := range append(AllSurfaces(), Surface(200), surfaceInvalid) {
			m.RecordAdmissionPhase(s, CohortID(hostile), Phase(hostile))
			m.RecordWinner(s, CohortID(hostile), Winner(hostile))
			m.RecordPreclaimDecline(s, CohortID(hostile))
			m.RecordPostclaimTerminal(s, CohortID(hostile), Winner(hostile))
		}
	}

	families := gatherDeBAML(t, reg)
	if findings := checkNoForbiddenSubstring(families, hostileLabelInputs()[:len(hostileLabelInputs())-1]); len(findings) > 0 {
		t.Fatalf("hostile material reached a label through the public recorders: %v", findings)
	}
	if findings := checkBoundedLabels(families, allowedLabelValues(ProductionCohortGate())); len(findings) > 0 {
		t.Fatalf("the public recorders emitted an unbounded label: %v", findings)
	}

	// Every phase/winner value that survived is one of the closed enum members or the
	// single out-of-band bucket — nothing else.
	okPhase := map[string]bool{
		string(PhasePreclaimDecline): true, string(PhaseClaimed): true,
		string(PhasePostclaimTerminal): true, string(PhaseSameResponseOracle): true,
		phaseLabelInvalid: true,
	}
	okWinner := map[string]bool{
		string(WinnerBAMLTransport): true, string(WinnerNative): true,
		string(WinnerBAMLParseSameResponse): true, string(WinnerFailure): true,
		winnerLabelInvalid: true,
	}
	seenInvalidPhase, seenInvalidWinner := false, false
	for _, mf := range families {
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				switch lp.GetName() {
				case "phase":
					if !okPhase[lp.GetValue()] {
						t.Errorf("phase label %q is outside the closed set", lp.GetValue())
					}
					seenInvalidPhase = seenInvalidPhase || lp.GetValue() == phaseLabelInvalid
				case "winner":
					if !okWinner[lp.GetValue()] {
						t.Errorf("winner label %q is outside the closed set", lp.GetValue())
					}
					seenInvalidWinner = seenInvalidWinner || lp.GetValue() == winnerLabelInvalid
				case "surface":
					if lp.GetValue() == "" {
						t.Error("surface label is empty")
					}
				}
			}
		}
	}
	// The fold is not vacuous: the hostile values DID reach the recorders and DID
	// land in the out-of-band bucket rather than being silently dropped.
	if !seenInvalidPhase || !seenInvalidWinner {
		t.Fatalf("no out-of-band phase/winner series was produced (phase=%v winner=%v); the hostile inputs never reached the recorders", seenInvalidPhase, seenInvalidWinner)
	}
}

// TestPublicRecorderFoldMutationBites is the bite for the fold: with normalizePhase /
// normalizeWinner reduced to a pass-through, the assertions above would pass a raw
// model name into a label. This drives the raw path directly and requires the
// checkers to catch it, so a future edit that drops the normalizers cannot leave a
// green suite behind.
func TestPublicRecorderFoldMutationBites(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	leak := "gpt-4o-acme-tuned-2026"
	// The MUTANT recorder: what RecordAdmissionPhase/RecordWinner would do without
	// their normalizers.
	m.admissionPhase.WithLabelValues(SurfaceDynamicCall.Label(), string(CohortNone), leak).Inc()
	m.winner.WithLabelValues(SurfaceDynamicCall.Label(), string(CohortNone), leak).Inc()

	families := gatherDeBAML(t, reg)
	if findings := checkNoForbiddenSubstring(families, []string{leak}); len(findings) == 0 {
		t.Fatal("an unnormalized phase/winner label was NOT caught: the redaction check is vacuous")
	}
	if findings := checkBoundedLabels(families, allowedLabelValues(ProductionCohortGate())); len(findings) == 0 {
		t.Fatal("an unnormalized phase/winner label was NOT caught by the cardinality check")
	}
}

// TestInventoryPublisherCannotCarryHostileInput closes the other half the review
// found: the publisher emits the fingerprint as a label, so an inventory that could
// hold a model name would publish one. It cannot — the manifest fails to build.
func TestInventoryPublisherCannotCarryHostileInput(t *testing.T) {
	for _, hostile := range hostileLabelInputs() {
		if _, err := newConfigInventory([]ConfigRecord{{
			Fingerprint: ConfigFingerprint(hostile),
			Cohort:      "a",
			Surfaces:    []Surface{SurfaceDynamicCall},
			Provider:    ConfigProviderOpenAI,
			Approval:    "DEBAML-1",
		}}); err == nil {
			t.Errorf("an inventory accepted the hostile fingerprint %q", hostile)
		}
		if _, err := newConfigInventory([]ConfigRecord{{
			Fingerprint: proofConfigFingerprint,
			Cohort:      CohortID(hostile),
			Surfaces:    []Surface{SurfaceDynamicCall},
			Provider:    ConfigProviderOpenAI,
			Approval:    "DEBAML-1",
		}}); err == nil {
			t.Errorf("an inventory accepted the hostile cohort %q", hostile)
		}
		if _, err := newConfigInventory([]ConfigRecord{{
			Fingerprint: proofConfigFingerprint,
			Cohort:      "a",
			Surfaces:    []Surface{SurfaceDynamicCall},
			Provider:    ConfigProviderClass(hostile),
			Approval:    "DEBAML-1",
		}}); err == nil {
			t.Errorf("an inventory accepted the hostile provider class %q", hostile)
		}
		if _, err := newConfigInventory([]ConfigRecord{{
			Fingerprint: proofConfigFingerprint,
			Cohort:      "a",
			Surfaces:    []Surface{SurfaceDynamicCall},
			Provider:    ConfigProviderOpenAI,
			Approval:    ApprovalRef(hostile),
		}}); err == nil {
			t.Errorf("an inventory accepted the hostile approval reference %q", hostile)
		}
	}

	// And the publisher itself, driven with the only inventory that CAN be built,
	// emits nothing outside the bounded set.
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	gate := testGate(t, "proof")
	m.publishCohortGate(gate)
	families := gatherDeBAML(t, reg)
	if findings := checkNoForbiddenSubstring(families, hostileLabelInputs()[:len(hostileLabelInputs())-1]); len(findings) > 0 {
		t.Fatalf("the inventory publisher leaked: %v", findings)
	}
	if findings := checkBoundedLabels(families, allowedLabelValues(gate)); len(findings) > 0 {
		t.Fatalf("the inventory publisher emitted an unbounded label: %v", findings)
	}
}

// TestDeclaredButUnenrolledRecordIsVisibleAndStillDeclines exercises the operator
// workflow the inventory exists for, THROUGH THE PRODUCTION CONFIG-LOAD PATH: a
// configuration class is DECLARED (so an operator sees it on the control-plane
// dashboard and joins it to their own approved-configuration record, offline, by its
// opaque ID) while the policy enrolls nothing — and traffic carrying that exact
// identity still declines.
//
// It calls buildCohortGate, which is the same function mustProductionGate calls with
// the two shipped manifests. A cold review noted that the previous version assembled
// a gate by hand with private setup, so it proved the mechanism rather than the path;
// this proves the path.
//
// It still does NOT fabricate an "approved" production record for a class nobody has
// approved — inventing an approval reference would be false evidence on an
// operator-facing dashboard, and the scope is explicit that the production
// configuration identity is operator input, not something to guess from repository
// code. What is proven is that the moment a real record IS declared, it becomes
// visible and remains refused.
func TestDeclaredButUnenrolledRecordIsVisibleAndStillDeclines(t *testing.T) {
	// The production builder, with a declared record and NO enrollment.
	gate, err := buildCohortGate("declared-but-unenrolled", []ConfigRecord{{
		Fingerprint: proofConfigFingerprint,
		Cohort:      "candidate",
		Surfaces:    []Surface{SurfaceDynamicCall},
		Provider:    ConfigProviderOpenAI,
		Approval:    "DEBAML-602",
	}}, nil)
	if err != nil {
		t.Fatalf("buildCohortGate: %v", err)
	}

	// The operator-visible half: the record is published, with its opaque ID and its
	// offline approval reference, and nothing else.
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	m.publishCohortGate(gate)
	rows := 0
	for _, mf := range gatherDeBAML(t, reg) {
		if mf.GetName() != "baml_rest_debaml_config_inventory_info" {
			continue
		}
		rows = len(mf.GetMetric())
	}
	if rows != 1 {
		t.Fatalf("config_inventory_info published %d rows, want 1 — the declared class is not operator-visible", rows)
	}
	if got := gate.Policy().Len(); got != 0 {
		t.Fatalf("the policy enrolls %d, want 0", got)
	}

	// The admission half: traffic carrying that exact declared identity resolves to
	// the record's cohort — the join works — and is STILL refused, on every surface,
	// because declaring a class is not enrolling it.
	in := CohortInput{Fingerprint: proofConfigFingerprint, Provider: ConfigProviderOpenAI, gate: gate}
	if got := ResolveCohort(SurfaceDynamicCall, in); got != "candidate" {
		t.Fatalf("declared identity resolved to %q, want the record's cohort", got)
	}
	for _, s := range AllSurfaces() {
		cohort, d := admitCohort(s, in)
		if d == nil {
			t.Errorf("%s: a DECLARED but UNENROLLED class was admitted", s.Label())
		}
		// Since serving cutover S3a the identity is bound to its RECORD, not to the
		// opaque bucket alone: it is the declared cohort exactly on the surface the
		// record declares, and the bounded unrecognized bucket everywhere else. Both
		// are attributable; neither is enrolled.
		want := CohortUnrecognized
		if s == SurfaceDynamicCall {
			want = "candidate"
		}
		if cohort != want {
			t.Errorf("%s: cohort = %q, want %q (the decline must still be attributable)", s.Label(), cohort, want)
		}
	}
}

// TestProductionManifestsAreTheOneFeV1TupleAndTheBuilderIsTheOnlyPath pins the
// SHIPPED enrollment at its source — the two manifests — rather than at the gate they
// produce, and pins that the gate really is built from them.
//
// It is written field-by-field and count-exact on purpose. This is the diff that
// permits native traffic to exist at all, so "one record, one enrollment, these exact
// values" has to be a test rather than a review promise: adding a second record,
// widening the record's surfaces, switching the provider class, dropping the strict
// verification regime, or enrolling a second pair each fail here.
func TestProductionManifestsAreTheOneFeV1TupleAndTheBuilderIsTheOnlyPath(t *testing.T) {
	records := productionInventoryRecords()
	if len(records) != 1 {
		t.Fatalf("the production inventory manifest declares %d record(s); the cutover declares exactly 1 (fe-v1)", len(records))
	}
	got := records[0]
	want := ConfigRecord{
		Fingerprint:  FeV1ConfigFingerprint,
		Cohort:       FeV1Cohort,
		Surfaces:     []Surface{SurfaceDynamicCall},
		Provider:     ConfigProviderOpenAI,
		Verification: VerificationStrictOpenAI,
		Approval:     FeV1Approval,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the fe-v1 record is %+v, want %+v", got, want)
	}
	// Spelled out again as the values a reviewer reads, so a future edit to the
	// constants themselves cannot silently satisfy the DeepEqual above.
	if got.Fingerprint != "cfg100" || got.Cohort != "fe_v1" || got.Provider != "openai" {
		t.Errorf("the fe-v1 record's opaque identity drifted: fingerprint=%q cohort=%q provider=%q", got.Fingerprint, got.Cohort, got.Provider)
	}
	if got.Verification != VerificationStrictOpenAI {
		t.Errorf("the fe-v1 record declares the %s verification regime; fe-v1 is enrolled under strict_openai, which is what runs BOTH retained BAML oracles", got.Verification.Label())
	}

	enrollments := productionEnrollments()
	if len(enrollments) != 1 {
		t.Fatalf("the production enrollment manifest has %d entr(ies); the cutover enrolls exactly 1", len(enrollments))
	}
	if enrollments[0] != (CohortEnrollment{Surface: SurfaceDynamicCall, Cohort: FeV1Cohort}) {
		t.Fatalf("the enrollment is %+v, want (dynamic_call, fe_v1)", enrollments[0])
	}
	// Nothing else is enrolled, surface by surface — the assertion an operator
	// actually cares about, stated over the closed set rather than over a count.
	pol, err := newCohortPolicy(ProductionCohortPolicyVersion, enrollments...)
	if err != nil {
		t.Fatalf("production policy: %v", err)
	}
	for _, s := range AllSurfaces() {
		if enrolled := pol.Enrolled(s, FeV1Cohort); enrolled != (s == SurfaceDynamicCall) {
			t.Errorf("%s: fe_v1 enrolled = %v, want %v", s.Label(), enrolled, s == SurfaceDynamicCall)
		}
	}
	// The shipped gate is exactly what the builder makes of those manifests.
	rebuilt, err := buildCohortGate(ProductionCohortPolicyVersion, productionInventoryRecords(), productionEnrollments())
	if err != nil {
		t.Fatalf("buildCohortGate on the production manifests: %v", err)
	}
	if rebuilt.Policy().Version() != ProductionCohortGate().Policy().Version() ||
		rebuilt.Policy().Len() != ProductionCohortGate().Policy().Len() ||
		rebuilt.Inventory().Len() != ProductionCohortGate().Inventory().Len() {
		t.Fatal("the shipped gate is not what the production manifests build; the config-load path is bypassed somewhere")
	}
}

// --- EVERY public recorder, driven the way an external consumer reaches them ----

// TestEveryPublicRecorderFoldsHostileInput closes the gap a second cold review found
// and demonstrated: the first round folded only the NEW serving-cutover recorders,
// while RecordPlanCompare / RecordResponseCompare / RecordServeOutcome /
// RecordNativeSocket / RecordFallback still passed their exported string-alias
// arguments straight into labels. A fresh external module resolved the published
// tip and emitted `field=gpt-4o-acme-tuned-2026` and
// `result=Authorization_Bearer_sk-live-example` through that public API.
//
// This drives EVERY exported recorder — reached exactly as an external consumer
// reaches them, through NewMetrics on a plain registry — with the full hostile
// corpus, and requires that nothing escapes and every label stays inside its closed
// set.
//
// It is deliberately enumerated rather than table-driven over "the recorders I
// remembered": TestPublicRecorderSurfaceIsFullyCovered below checks this list
// against the package source, so a new exported recorder cannot be added without
// being driven here.
// driveEveryPublicRecorder pushes one value through EVERY exported recorder, in every
// label position that recorder owns — the way an external consumer holding a *Metrics
// can. Both the hostile-corpus proof and the block-comment proof drive through this one
// helper so they cannot drift apart: a new public recorder added here is exercised by
// both, and TestPublicRecorderSurfaceIsFullyCovered fails if one is added and not
// driven at all.
func driveEveryPublicRecorder(m *Metrics, v string) {
	// The serving-cutover recorders.
	for _, s := range append(AllSurfaces(), Surface(200), surfaceInvalid) {
		m.RecordAdmissionPhase(s, CohortID(v), Phase(v))
		m.RecordWinner(s, CohortID(v), Winner(v))
		m.RecordPreclaimDecline(s, CohortID(v))
		m.RecordPostclaimTerminal(s, CohortID(v), Winner(v))
	}
	// The pre-existing recorders — the ones the review reached.
	m.RecordPlanCompare(PlanCompareResult(v), PlanCompareField(v))
	m.RecordResponseCompare(ResponseCompareResult(v), ResponseCompareField(v))
	m.RecordServeOutcome(Mode(v), v, Outcome(v))
	m.RecordNativeSocket(NativeSocketOutcome(v))
	m.RecordFallback(FallbackKind(v))
}

func TestEveryPublicRecorderFoldsHostileInput(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	for _, hostile := range hostileLabelInputs() {
		driveEveryPublicRecorder(m, hostile)
	}

	families := gatherDeBAML(t, reg)
	if findings := checkNoForbiddenSubstring(families, forbiddenLabelMaterial()); len(findings) > 0 {
		t.Fatalf("hostile material reached a label through a PUBLIC recorder: %v", findings)
	}
	if findings := checkBoundedLabels(families, allowedLabelValues(ProductionCohortGate())); len(findings) > 0 {
		t.Fatalf("a public recorder emitted an unbounded label: %v", findings)
	}
	// The folds are not vacuous: the hostile values reached the recorders and landed
	// in the out-of-band bucket instead of being dropped.
	if !hasLabelValue(families, labelInvalid) {
		t.Fatal("no out-of-band label was produced; the hostile inputs never reached the public recorders")
	}
}

// hasLabelValue reports whether any gathered series carries the given label value.
func hasLabelValue(families []*dto.MetricFamily, want string) bool {
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetValue() == want {
					return true
				}
			}
		}
	}
	return false
}

// TestPublicRecorderSurfaceIsFullyCovered keeps the enumeration above honest: every
// exported Record*/Publish* method on *Metrics that the package declares must be
// driven by the hostile-input test. A new public recorder that quietly skipped it
// would be exactly the hole the review found.
func TestPublicRecorderSurfaceIsFullyCovered(t *testing.T) {
	driven := map[string]bool{
		"RecordAdmissionPhase":    true,
		"RecordWinner":            true,
		"RecordPreclaimDecline":   true,
		"RecordPostclaimTerminal": true,
		"RecordPlanCompare":       true,
		"RecordResponseCompare":   true,
		"RecordServeOutcome":      true,
		"RecordNativeSocket":      true,
		"RecordFallback":          true,
	}
	found := map[string]string{} // method -> declaring file
	for _, r := range exportedMetricsRecorders(packageAST(t)) {
		found[r.name] = r.file
	}
	if len(found) == 0 {
		t.Fatal("no exported (*Metrics) recorders discovered; the coverage guard is vacuous")
	}
	// The BIJECTION is the point: everything declared is driven, and everything the
	// table names is still declared. Either half alone rots.
	for name, file := range found {
		if !driven[name] {
			t.Errorf("%s declares exported recorder (*Metrics).%s, which TestEveryPublicRecorderFoldsHostileInput does not drive", file, name)
		}
	}
	for name := range driven {
		if _, ok := found[name]; !ok {
			t.Errorf("the coverage table names (*Metrics).%s but the package no longer declares it", name)
		}
	}
}

// isExportedMetricsRecorder is THE predicate for "this declaration is a public telemetry
// recorder". It keys on the receiver's TYPE, never on the variable name: the original
// regex required the literal `(m *Metrics)`, and a bot review pointed out that renaming
// that receiver on a newly exported recorder would drop it out of hostile-input coverage
// without failing anything.
func isExportedMetricsRecorder(fn *ast.FuncDecl) bool {
	return fn.Name.IsExported() && receiverTypeName(fn) == "Metrics"
}

// exportedMetricsRecorders runs that predicate over the given sources. The production
// coverage guard and its receiver-varied bite both call this, so narrowing the predicate
// turns both red — the second bot finding of this pass was that the bite used to carry its
// own copy and so could not notice.
func exportedMetricsRecorders(sources []astSource) []discoveredFunc {
	return discoverFuncs(sources, isExportedMetricsRecorder)
}

// TestRecorderDiscoveryIgnoresTheReceiverName is the BITE: a recorder must be discovered
// whatever its receiver variable is called, and a method on some other type must not be.
// Driven through the REAL exportedMetricsRecorders over a synthetic file, so the proof does
// not depend on this package currently renaming a receiver — and cannot drift from the
// guard.
func TestRecorderDiscoveryIgnoresTheReceiverName(t *testing.T) {
	const synthetic = `package admission

func (m *Metrics) RecordConventional()      {}
func (metrics *Metrics) RecordRenamedRecv() {}
func (Metrics) RecordAnonymousValueRecv()   {}
func (m *Metrics) unexportedHelper()        {}
func (o *Other) RecordOnAnotherType()       {}
func RecordPackageLevel()                   {}
`
	got := map[string]bool{}
	for _, r := range exportedMetricsRecorders([]astSource{syntheticSource(t, "synthetic.go", synthetic)}) {
		got[r.name] = true
	}
	for _, want := range []string{"RecordConventional", "RecordRenamedRecv", "RecordAnonymousValueRecv"} {
		if !got[want] {
			t.Errorf("%s was not discovered; a renamed receiver would drop a public recorder out of hostile-input coverage", want)
		}
	}
	for _, unwanted := range []string{"unexportedHelper", "RecordOnAnotherType", "RecordPackageLevel"} {
		if got[unwanted] {
			t.Errorf("%s was discovered as an exported (*Metrics) recorder, which it is not", unwanted)
		}
	}
}

// TestPublicRecorderFoldMutationBitesThroughTheExportedAPI is the bite for the fix,
// driven through the PUBLIC API rather than through private CounterVec wiring — the
// specific weakness the review named in the previous round's bite.
//
// It re-runs the hostile drive against MUTANT recorders (the pre-fold call the
// production methods used to make) and requires the checkers to catch every one, so
// a future edit that drops any single fold cannot leave this suite green.
func TestPublicRecorderFoldMutationBitesThroughTheExportedAPI(t *testing.T) {
	leak := "Authorization_Bearer_sk-live-example"
	mutants := []struct {
		name string
		emit func(*Metrics)
	}{
		{"plan_compare result", func(m *Metrics) { m.planCompare.WithLabelValues(leak, string(PlanCompareFieldBody)).Inc() }},
		{"plan_compare field", func(m *Metrics) { m.planCompare.WithLabelValues(string(PlanCompareMatch), leak).Inc() }},
		{"response_compare result", func(m *Metrics) { m.responseCompare.WithLabelValues(leak, string(ResponseCompareFieldRaw)).Inc() }},
		{"response_compare field", func(m *Metrics) { m.responseCompare.WithLabelValues(string(ResponseCompareMatch), leak).Inc() }},
		{"attempts outcome", func(m *Metrics) {
			m.attempts.WithLabelValues(string(ModeCall), engineNative, string(providerOpenAI), leak).Inc()
		}},
		{"native_sockets outcome", func(m *Metrics) { m.nativeSockets.WithLabelValues(string(SocketFlagOn), leak).Inc() }},
		{"fallback kind", func(m *Metrics) { m.fallback.WithLabelValues(leak).Inc() }},
		{"admission phase", func(m *Metrics) {
			m.admissionPhase.WithLabelValues(SurfaceDynamicCall.Label(), string(CohortNone), leak).Inc()
		}},
		{"winner", func(m *Metrics) {
			m.winner.WithLabelValues(SurfaceDynamicCall.Label(), string(CohortNone), leak).Inc()
		}},
	}
	for _, mut := range mutants {
		t.Run(mut.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m, err := NewMetrics(reg)
			if err != nil {
				t.Fatalf("NewMetrics: %v", err)
			}
			mut.emit(m)
			families := gatherDeBAML(t, reg)
			if findings := checkNoForbiddenSubstring(families, []string{leak}); len(findings) == 0 {
				t.Fatalf("an unfolded %s label was NOT caught: the redaction check is vacuous for it", mut.name)
			}
			if findings := checkBoundedLabels(families, allowedLabelValues(ProductionCohortGate())); len(findings) == 0 {
				t.Fatalf("an unfolded %s label was NOT caught by the cardinality check", mut.name)
			}
		})
	}
}
