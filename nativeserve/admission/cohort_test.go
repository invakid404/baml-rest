package admission

// De-BAML serving cutover S1 — the DEFAULT-DENY gate's proof suite.
//
// This file is UNTAGGED on purpose. The gate runs before every native operation
// (no nanollm New, no render, no Prepare), so its whole behaviour is provable
// without the `nanollm_integration` opt-in — which means the default-deny property
// is checked by the ordinary package test run rather than only by the gated lane.
//
// Every positive control here is paired with a MUTATION BITE: the same assertions
// are re-run against a deliberately mutated artifact (a policy that admits, a
// redaction that leaks) and the test FAILS if the assertions still pass. A proof
// that cannot fail is not a proof.

import (
	"context"
	"errors"
	"go/ast"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
)

// --- shared fixtures ---------------------------------------------------------

// errProofNoDial is returned instead of dialing. A cohort-gated request must never
// reach the transport at all, so this error is itself never expected to surface;
// returning it (rather than a canned 200) means a stray RoundTrip fails loudly in
// two independent ways — the counter moves AND the caller errors.
var errProofNoDial = errors.New("proof suite: the cohort gate must decline before any dial")

// proofCountingTransport counts RoundTrips and refuses to perform one. It is
// separate from the gated suite's countingTransport (which returns a canned 200 for
// its positive control) so the two suites can be built together.
type proofCountingTransport struct{ n atomic.Int64 }

func (c *proofCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errProofNoDial
}

// testGate builds an in-package gate over the declared proof ID. Untagged tests
// build it here rather than calling ProofCohortGateForTest, which lives behind the
// `nanollm_integration` tag — the same reason the production constructors are
// unexported: nothing outside this package may select a gate.
func testGate(t *testing.T, cohort CohortID, surfaces ...Surface) *CohortGate {
	t.Helper()
	if len(surfaces) == 0 {
		surfaces = AllSurfaces()
	}
	inv, err := newConfigInventory([]ConfigRecord{{
		Fingerprint: proofConfigFingerprint,
		Cohort:      cohort,
		Surfaces:    surfaces,
		Provider:    ConfigProviderOpenAI,
		Approval:    "DEBAML-602",
	}})
	if err != nil {
		t.Fatalf("test inventory: %v", err)
	}
	entries := make([]CohortEnrollment, 0, len(surfaces))
	for _, sf := range surfaces {
		entries = append(entries, CohortEnrollment{Surface: sf, Cohort: cohort})
	}
	pol, err := newCohortPolicy("test-gate", entries...)
	if err != nil {
		t.Fatalf("test policy: %v", err)
	}
	g, err := newCohortGate(pol, inv)
	if err != nil {
		t.Fatalf("test gate: %v", err)
	}
	return g
}

// testIdentity is the enrolled identity the untagged in-package tests present.
func testIdentity(t *testing.T) CohortInput {
	t.Helper()
	// Provider is part of the identity since serving cutover S3a: the gate binds a
	// fingerprint to its inventory record, and testGate's record declares openai.
	return CohortInput{Fingerprint: proofConfigFingerprint, Provider: ConfigProviderOpenAI, gate: testGate(t, "proof")}
}

// mutantAdmittingGate is the MUTANT the bite tests use: it declares an inventory
// record for the very fingerprint one identity shape below presents, and enrolls its
// cohort on every surface. It is the smallest edit that turns "the empty policy
// admits nothing" into "the policy admits this traffic", which is exactly the
// mutation the default-deny assertions must catch. Nothing in production builds it.
func mutantAdmittingGate(t *testing.T) *CohortGate {
	t.Helper()
	return testGate(t, "mutant_a")
}

// identityShapes are the refusal shapes the scope names: no identity at all, an ID
// the inventory does not carry, an ID outside the declared vocabulary, and a hostile
// one that is not even of the opaque form. All four must resolve to a BOUNDED cohort
// and decline. The second shape is the one the mutant gate turns into an admission,
// which is what gives the bite below its teeth.
func identityShapes(gate *CohortGate) []struct {
	name   string
	in     CohortInput
	cohort CohortID
} {
	return []struct {
		name   string
		in     CohortInput
		cohort CohortID
	}{
		{"absent identity", CohortInput{gate: gate}, CohortNone},
		{"declared but not inventoried", CohortInput{Fingerprint: proofConfigFingerprint, Provider: ConfigProviderOpenAI, gate: gate}, CohortUnrecognized},
		{"well-formed but undeclared", CohortInput{Fingerprint: ConfigFingerprint("cfg123"), gate: gate}, CohortUnrecognized},
		{"malformed fingerprint", CohortInput{Fingerprint: ConfigFingerprint("https://evil.example/v1?k=sk-secret"), gate: gate}, CohortUnrecognized},
	}
}

// --- default deny ------------------------------------------------------------

// assertGateDeclinesEverySurface is the reusable assertion body the mutation bite
// re-runs. It reports failures through the supplied testing.TB, so the bite can
// hand it a recording TB and assert that it DID fail.
func assertGateDeclinesEverySurface(tb testing.TB, gate *CohortGate) {
	tb.Helper()
	for _, surface := range AllSurfaces() {
		for _, shape := range identityShapes(gate) {
			cohort, d := admitCohort(surface, shape.in)
			if d == nil {
				tb.Errorf("%s/%s: admitted, want a cohort decline", surface.Label(), shape.name)
				continue
			}
			if d.Stage != StageCohort || d.Reason != ReasonCohortNotEnrolled {
				tb.Errorf("%s/%s: decline = (%s, %s), want (cohort, cohort_not_enrolled)", surface.Label(), shape.name, d.Stage, d.Reason)
			}
			if cohort != shape.cohort {
				tb.Errorf("%s/%s: cohort = %q, want %q", surface.Label(), shape.name, cohort, shape.cohort)
			}
		}
	}
}

// TestProductionGateDeclinesEveryUnenrolledIdentity is the default-deny headline as
// serving cutover S3b leaves it: the shipped gate enrolls exactly ONE tuple, and
// every OTHER identity shape — absent, unrecognized, undeclared, hostile — still
// declines on every surface with the one bounded reason.
//
// The identity shapes below deliberately do NOT include the fe-v1 identity; that one
// has its own exactness proof (TestShippedGateAdmitsOnlyTheFeV1Tuple). What this pins
// is that adding an enrollment did not turn the gate into a pass-through for
// everything else, which is the failure mode a first enrollment actually risks.
func TestProductionGateDeclinesEveryUnenrolledIdentity(t *testing.T) {
	if got := ProductionCohortGate().Policy().Len(); got != 1 {
		t.Fatalf("production policy enrolls %d cohorts, want exactly 1 (the fe-v1 tuple)", got)
	}
	if got := ProductionCohortGate().Inventory().Len(); got != 1 {
		t.Fatalf("production inventory declares %d records, want exactly 1 (the fe-v1 record)", got)
	}
	if got := ProductionCohortGate().Policy().Version(); got != ProductionCohortPolicyVersion {
		t.Fatalf("production policy version = %q, want %q", got, ProductionCohortPolicyVersion)
	}
	assertGateDeclinesEverySurface(t, ProductionCohortGate())
	// A nil gate — the shape a forgotten wiring produces — must fail closed the same way.
	assertGateDeclinesEverySurface(t, nil)
}

// TestShippedGateAdmitsOnlyTheFeV1Tuple is the positive half, and it is exact: the
// fe-v1 identity is admitted on dynamic_call and REFUSED on every other surface, and
// every near-miss on the identity itself — a different provider class, a neighbouring
// slot, no class at all — resolves to a bounded non-enrolled bucket instead.
//
// It runs against ProductionCohortGate(), i.e. the gate the shipped binary uses, not
// a hand-built one.
func TestShippedGateAdmitsOnlyTheFeV1Tuple(t *testing.T) {
	feV1 := CohortInput{Fingerprint: FeV1ConfigFingerprint, Provider: ConfigProviderOpenAI}

	cohort, d := admitCohort(SurfaceDynamicCall, feV1)
	if d != nil {
		t.Fatalf("the fe-v1 identity was declined on dynamic_call: %v", d)
	}
	if cohort != FeV1Cohort {
		t.Fatalf("the fe-v1 identity resolved to cohort %q, want %q", cohort, FeV1Cohort)
	}
	for _, s := range AllSurfaces() {
		if s == SurfaceDynamicCall {
			continue
		}
		got, d := admitCohort(s, feV1)
		if d == nil {
			t.Errorf("%s: the fe-v1 identity was ADMITTED on a surface its record does not declare", s.Label())
		}
		// The record declares dynamic_call only, so on any other surface the identity
		// is not "fe_v1 but unenrolled" — it folds onto the bounded unrecognized
		// bucket, which is what keeps a wrong-surface claim unattributable to the
		// approved cohort.
		if got != CohortUnrecognized {
			t.Errorf("%s: the fe-v1 identity resolved to %q, want %q", s.Label(), got, CohortUnrecognized)
		}
	}

	// Near-misses on the identity itself.
	for _, near := range []struct {
		name string
		in   CohortInput
	}{
		{"wrong provider class", CohortInput{Fingerprint: FeV1ConfigFingerprint, Provider: ConfigProviderAnthropic}},
		{"no provider class", CohortInput{Fingerprint: FeV1ConfigFingerprint}},
		{"neighbouring declared slot", CohortInput{Fingerprint: "cfg001", Provider: ConfigProviderOpenAI}},
		{"undeclared slot", CohortInput{Fingerprint: "cfg101", Provider: ConfigProviderOpenAI}},
	} {
		got, d := admitCohort(SurfaceDynamicCall, near.in)
		if d == nil {
			t.Errorf("%s: admitted on dynamic_call; only the exact fe-v1 identity may be", near.name)
		}
		if got == FeV1Cohort {
			t.Errorf("%s: resolved to the approved cohort %q; a near-miss must never inherit it", near.name, FeV1Cohort)
		}
	}
}

// TestEveryEnrolledProductionRecordDeclaresTheStrictOpenAIRegime is the standing
// guard behind the S3b verification check: the cutover enrolls fe-v1 to serve WITH
// both retained BAML oracles, and only [PolicyStrictOpenAI] runs them (the trusted
// regime runs neither the pre-claim plan equality nor the same-response compare).
//
// So every record the shipped policy ENROLLS must declare the strict regime, and it
// must declare one at all — an unconstrained record would permit a claim under any
// regime the mapper happened to assign, which is precisely the silent widening
// admitVerification exists to refuse.
func TestEveryEnrolledProductionRecordDeclaresTheStrictOpenAIRegime(t *testing.T) {
	inv := ProductionCohortGate().Inventory()
	for _, e := range productionEnrollments() {
		found := false
		for _, r := range inv.Records() {
			if r.Cohort != e.Cohort {
				continue
			}
			found = true
			if r.Verification == VerificationUnconstrained {
				t.Errorf("the enrolled record for cohort %q declares no verification regime; an enrolled class must name the one it was approved for", e.Cohort)
			}
			if r.Verification != VerificationStrictOpenAI {
				t.Errorf("the enrolled record for cohort %q declares the %s regime; the cutover enrolls only classes approved for strict_openai, which is what runs both retained BAML oracles",
					e.Cohort, r.Verification.Label())
			}
			// The regime is a REQUIREMENT, not a description: it must refuse the
			// trusted policy outright.
			if r.Verification.Permits(PolicyTrustedProvider) {
				t.Errorf("the enrolled record for cohort %q permits the trusted-provider policy; that regime runs neither retained BAML oracle", e.Cohort)
			}
			if !r.Verification.Permits(PolicyStrictOpenAI) {
				t.Errorf("the enrolled record for cohort %q refuses the strict-OpenAI policy it is enrolled under", e.Cohort)
			}
		}
		if !found {
			t.Errorf("the policy enrolls cohort %q with no inventory record; the gate constructor should have refused this", e.Cohort)
		}
	}
}

// recordingTB captures whether the assertion body reported a failure, so a mutation
// bite can assert the body DOES fail on a mutated artifact.
type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Errorf(string, ...any) { r.failed = true }
func (r *recordingTB) Error(...any)          { r.failed = true }
func (r *recordingTB) Fatalf(string, ...any) { r.failed = true }
func (r *recordingTB) Fatal(...any)          { r.failed = true }
func (r *recordingTB) Helper()               {}

// TestEmptyPolicyMutationBitesTheEquivalenceAssertions is the MUTATION BITE for the
// gate: mutate the empty policy into one that admits, and the default-deny
// assertions above MUST fail. Without this, a green default-deny test could mean
// "the policy is empty" OR "the assertion never checks anything".
func TestEmptyPolicyMutationBitesTheEquivalenceAssertions(t *testing.T) {
	rec := &recordingTB{TB: t}
	assertGateDeclinesEverySurface(rec, mutantAdmittingGate(t))
	if !rec.failed {
		t.Fatal("mutating the empty policy to admit did NOT fail the default-deny assertions: the assertions are vacuous")
	}
}

// TestEnrolledPairAdmitsExactlyItsOwnSurface proves the gate is a real predicate
// and not a constant `false`: an enrolled (surface, cohort) pair passes, and the
// SAME cohort on any other surface still declines.
func TestEnrolledPairAdmitsExactlyItsOwnSurface(t *testing.T) {
	gate := testGate(t, "unary_only", SurfaceDynamicCall)
	in := CohortInput{Fingerprint: proofConfigFingerprint, Provider: ConfigProviderOpenAI, gate: gate}

	cohort, d := admitCohort(SurfaceDynamicCall, in)
	if d != nil {
		t.Fatalf("enrolled pair declined: %v", d)
	}
	if cohort != "unary_only" {
		t.Fatalf("cohort = %q, want unary_only", cohort)
	}
	for _, s := range AllSurfaces() {
		if s == SurfaceDynamicCall {
			continue
		}
		if _, d := admitCohort(s, in); d == nil {
			t.Errorf("%s: the unary-only cohort was admitted on the wrong surface", s.Label())
		}
	}
}

// TestReservedCohortsAreNeverEnrollable pins the two resolution outcomes out of the
// enrollable set at BOTH layers — the constructor rejects them, and Enrolled
// re-asserts it so the property survives a constructor change.
func TestReservedCohortsAreNeverEnrollable(t *testing.T) {
	for _, reserved := range reservedCohortIDs() {
		if _, err := parseCohortID(string(reserved)); err == nil {
			t.Errorf("parseCohortID(%q) accepted a reserved cohort", reserved)
		}
		if _, err := newCohortPolicy("v1", CohortEnrollment{Surface: SurfaceDynamicCall, Cohort: reserved}); err == nil {
			t.Errorf("NewCohortPolicy enrolled the reserved cohort %q", reserved)
		}
		// Force the reserved entry into the map behind the constructor's back and
		// prove Enrolled still refuses it.
		forced := &CohortPolicy{version: "forced", entries: map[CohortEnrollment]struct{}{
			{Surface: SurfaceDynamicCall, Cohort: reserved}: {},
		}}
		if forced.Enrolled(SurfaceDynamicCall, reserved) {
			t.Errorf("Enrolled admitted the reserved cohort %q even though the policy map carried it", reserved)
		}
	}
}

// --- the lanes ---------------------------------------------------------------

// dynamicGateInput is a layer-1-valid dynamic input whose registry is deliberately
// NIL. If the gate runs where it claims to — before client mapping and before any
// native work — the decline is the cohort decline, not no_registry. That single
// assertion pins the gate's ORDER, not just its existence.
func dynamicGateInput(mode Mode) Input {
	return Input{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		Method:              dynamicMethod,
		Mode:                mode,
		SingleLeaf:          true,
		ResolvedProvider:    "openai",
		Registry:            nil,
		Alias:               "__proof_alias__",
		OutputSchema:        nil,
	}
}

// TestDynamicLanesDeclinePreNativeWork drives all three dynamic entry points with
// the production identity and proves: the cohort decline wins over the later
// no_registry / output_schema_absent gates (so it runs BEFORE them), and ZERO
// RoundTrips occur.
func TestDynamicLanesDeclinePreNativeWork(t *testing.T) {
	ct := &proofCountingTransport{}
	a := NewAdmitter(nil, llmhttp.NewExactExecutor(ct))
	ctx := context.Background()

	assertCohortDecline := func(name string, err error) {
		t.Helper()
		d, ok := err.(*Decline)
		if !ok {
			t.Fatalf("%s: err = %v (%T), want *Decline", name, err, err)
		}
		if d.Stage != StageCohort || d.Reason != ReasonCohortNotEnrolled {
			t.Fatalf("%s: decline = (%s, %s), want (cohort, cohort_not_enrolled)", name, d.Stage, d.Reason)
		}
	}

	_, err := a.Admit(ctx, dynamicGateInput(ModeCall))
	assertCohortDecline("Admit", err)
	claim, err := a.AdmitClaim(ctx, dynamicGateInput(ModeCall))
	assertCohortDecline("AdmitClaim", err)
	if claim != nil {
		t.Fatal("AdmitClaim returned a claim alongside a decline")
	}
	sclaim, err := a.AdmitStreamClaim(ctx, dynamicGateInput(ModeStream))
	assertCohortDecline("AdmitStreamClaim", err)
	if sclaim != nil {
		t.Fatal("AdmitStreamClaim returned a claim alongside a decline")
	}
	if n := ct.n.Load(); n != 0 {
		t.Fatalf("admission performed %d RoundTrips, want 0", n)
	}
}

// TestStaticLanesDeclinePreNativeWork is the static twin: every static entry point
// declines at the cohort gate under the production identity, and does so BEFORE the
// descriptor envelope check (the descriptor here is empty, which would otherwise
// decline as descriptor_absent).
func TestStaticLanesDeclinePreNativeWork(t *testing.T) {
	ctx := context.Background()
	base := StaticInput{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		RouteKind:           RouteKindStatic,
		Method:              "M",
		Mode:                bamlutils.NativeStaticModeFinal,
		SingleLeaf:          true,
	}

	obs := AdmitStatic(ctx, base)
	assertStaticCohortObservation(t, "AdmitStatic", obs)

	parseIn := base
	parseIn.Mode = bamlutils.NativeStaticModeParseOnly
	assertStaticCohortObservation(t, "AdmitStaticParse", AdmitStaticParse(ctx, parseIn))

	claim, err := AdmitStaticClaim(ctx, base)
	if claim != nil {
		t.Fatal("AdmitStaticClaim returned a claim alongside a decline")
	}
	assertStaticCohortDecline(t, "AdmitStaticClaim", err)

	streamClaim, err := AdmitStaticStreamClaim(ctx, StaticStreamInput{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		RouteKind:           RouteKindStatic,
		Method:              "M",
		Mode:                bamlutils.NativeStreamModeStream,
		SingleLeaf:          true,
	})
	if streamClaim != nil {
		t.Fatal("AdmitStaticStreamClaim returned a claim alongside a decline")
	}
	assertStaticCohortDecline(t, "AdmitStaticStreamClaim", err)
}

func assertStaticCohortObservation(t *testing.T, name string, obs StaticObservation) {
	t.Helper()
	if obs.Observation != bamlutils.NativeStaticObserveDecline {
		t.Fatalf("%s: observation = %q, want decline", name, obs.Observation)
	}
	if obs.Stage != string(StageCohort) || obs.Reason != string(ReasonCohortNotEnrolled) {
		t.Fatalf("%s: (stage, reason) = (%s, %s), want (cohort, cohort_not_enrolled)", name, obs.Stage, obs.Reason)
	}
}

func assertStaticCohortDecline(t *testing.T, name string, err error) {
	t.Helper()
	d, ok := err.(*StaticDecline)
	if !ok {
		t.Fatalf("%s: err = %v (%T), want *StaticDecline", name, err, err)
	}
	if d.Stage != string(StageCohort) || d.Reason != string(ReasonCohortNotEnrolled) {
		t.Fatalf("%s: (stage, reason) = (%s, %s), want (cohort, cohort_not_enrolled)", name, d.Stage, d.Reason)
	}
}

// --- structural guards over the source --------------------------------------

// The structural guards below run on the PARSED package (see guardast_test.go), not
// on its source text. Discovery therefore keys on declarations — a receiver's type, a
// parameter's type, a struct's fields — and is indifferent to the variable names an
// author picked, to how a signature wraps, and to anything written in a comment.

// isAdmissionEntryPoint is THE predicate for "this declaration is an exported admission
// entry point". There is exactly one copy of it, and both the production package scan and
// the receiver-varied bite run it — so narrowing it (back to a particular receiver, say)
// fails the bite as well as widening the guard. A bot review found the first version of
// the bite re-implementing this test inline, which made the two agree by construction.
func isAdmissionEntryPoint(fn *ast.FuncDecl) bool {
	return fn.Name.IsExported() && strings.HasPrefix(fn.Name.Name, "Admit")
}

// isSyntheticClaimBuilder separates the …ForTest claim builders out of that set. They
// construct a claim WITHOUT running the predicate, so they are test doubles rather than
// admission entry points — but a test double linkable into a production build WOULD be a
// bypass, which is why TestSyntheticClaimBuildersStayBehindTheGatedTag pins them behind
// the opt-in tag instead of merely skipping them.
func isSyntheticClaimBuilder(name string) bool { return strings.HasSuffix(name, "ForTest") }

// admissionEntryPoints finds every exported `Admit*` declaration in the given sources,
// WHATEVER it hangs off: a package function, a method on *Admitter, a method on some type
// a later refactor introduces, a value receiver, a renamed receiver.
//
// The original version matched the literal receiver `a *Admitter`. A bot review pointed
// out the consequence: a new exported Admit* method on any other receiver — or on the same
// one with a renamed variable — would simply not be discovered, and the all-entry-points
// proof would pass while not covering it.
func admissionEntryPoints(sources []astSource) []discoveredFunc {
	return discoverFuncs(sources, isAdmissionEntryPoint)
}

// TestEveryAdmissionEntryPointIsCohortGated is the guard that keeps the "one rule,
// no exceptions" claim true as the package grows: it discovers the exported entry
// points from the SOURCE and fails if one appears that this suite does not cover.
// A list derived from the covered set would shrink silently with it.
func TestEveryAdmissionEntryPointIsCohortGated(t *testing.T) {
	// The entry points TestDynamicLanesDeclinePreNativeWork,
	// TestStaticLanesDeclinePreNativeWork and TestAdmitDirectParseAlwaysDeclines drive.
	covered := map[string]bool{
		"Admit":                  true,
		"AdmitClaim":             true,
		"AdmitStreamClaim":       true,
		"AdmitStatic":            true,
		"AdmitStaticClaim":       true,
		"AdmitStaticParse":       true,
		"AdmitStaticStreamClaim": true,
		"AdmitDirectParse":       true,
	}
	// spineLaneExempt is the deliberate, DOCUMENTED set of exceptions to the
	// dynamic-rollout cohort gate: the ExecBridge-U1 codegen-spine unary lanes
	// (AdmitStaticSpineClaim, the frozen-evidence native-only entry, and
	// AdmitStaticSpineOracleClaim, the ExecBridge-U1c live-oracle standard-worker
	// entry). Neither runs admitCohort because the spine is a SEPARATE default-deny
	// lane whose admission is its own root-owned totality predicate
	// (debaml.SupportsNativeStaticStreamBundle — the exact five-arm JSON alias)
	// resolved at REGISTRATION, and membership is structural, NOT an enrollment. U1c
	// default-selects the exact population through a live BAML plan-compare oracle, but
	// still must NOT widen the dynamic cohort manifest, and enrolling a static cohort to
	// satisfy this gate would do exactly that. Both STAY discoverable here — renaming to
	// dodge the scan is the evasion this guard forbids — so the exemption is explicit and
	// reviewed; the default-deny is proven separately by TestSpineLaneSkipsDynamicCohortGate
	// (this package, showing they decline at a NON-cohort stage where the cohort-gated
	// lanes decline at cohort) and the nativeserve/spine registration-gate tests.
	spineLaneExempt := map[string]bool{
		"AdmitStaticSpineClaim":       true,
		"AdmitStaticSpineOracleClaim": true,
	}
	found := map[string]bool{}
	for _, ep := range admissionEntryPoints(packageAST(t)) {
		if isSyntheticClaimBuilder(ep.name) {
			continue
		}
		found[ep.name] = true
		if !covered[ep.name] && !spineLaneExempt[ep.name] {
			t.Errorf("%s declares exported admission entry point %s (receiver %q), which no cohort-gate proof drives. "+
				"Add it to TestDynamicLanesDeclinePreNativeWork / TestStaticLanesDeclinePreNativeWork and to this table.",
				ep.file, ep.name, ep.receiver)
		}
	}
	// NON-VACUITY: discovery finding nothing would make the loop above trivially clean,
	// which is precisely the false-green this guard exists to prevent.
	if len(found) == 0 {
		t.Fatal("no exported admission entry points discovered; the cohort-gate proof is vacuous")
	}
	for name := range covered {
		if !found[name] {
			t.Errorf("the coverage table names %s but the package no longer declares it — remove the stale entry", name)
		}
	}
	// A stale exemption is a silent hole: if the spine lane is renamed/removed, the
	// exemption must go with it rather than silently excuse a future same-named entry.
	for name := range spineLaneExempt {
		if !found[name] {
			t.Errorf("the spine-lane exemption names %s but the package no longer declares it — remove the stale exemption", name)
		}
	}
}

// TestSpineLaneSkipsDynamicCohortGate is the compensating proof for the TWO cohort-gate
// exemptions (AdmitStaticSpineClaim, the frozen-evidence native-only entry, and
// AdmitStaticSpineOracleClaim, the ExecBridge-U1c live-oracle standard-worker entry): where
// the cohort-gated static claim lane declines at
// the (cohort, cohort_not_enrolled) gate, the spine lane deliberately SKIPS it (SpineLane)
// and declines at a LATER, non-cohort stage — its default-deny being its own
// registration-time totality predicate, not the dynamic-rollout manifest. If a future
// change routed the spine lane back through admitCohort, this test would go red (it would
// decline at the cohort gate), and if it removed the skip's effect the two lanes would no
// longer differ.
func TestSpineLaneSkipsDynamicCohortGate(t *testing.T) {
	ctx := context.Background()
	// Reaches layer 1b (the cohort gate) with an unenrolled CohortNone identity.
	base := StaticInput{
		WorkerCapable:       true,
		RequestAPIPresent:   true,
		OnBuildRequestRoute: true,
		FlagEnabled:         true,
		RouteKind:           RouteKindStatic,
		Method:              "M",
		Mode:                bamlutils.NativeStaticModeFinal,
		SingleLeaf:          true,
	}

	// The cohort-gated lane declines AT the cohort gate.
	if _, err := AdmitStaticClaim(ctx, base); true {
		assertStaticCohortDecline(t, "AdmitStaticClaim", err)
	}

	// Both spine lanes decline, but NEVER at the cohort gate — they skip it by design.
	for _, lane := range []struct {
		name  string
		admit func(context.Context, StaticInput) (*StaticClaim, error)
	}{
		{"AdmitStaticSpineClaim", AdmitStaticSpineClaim},
		{"AdmitStaticSpineOracleClaim", AdmitStaticSpineOracleClaim},
	} {
		claim, err := lane.admit(ctx, base)
		if claim != nil {
			t.Fatalf("%s returned a claim alongside a decline", lane.name)
		}
		d, ok := err.(*StaticDecline)
		if !ok {
			t.Fatalf("%s: err = %v (%T), want *StaticDecline", lane.name, err, err)
		}
		if d.Stage == string(StageCohort) {
			t.Fatalf("%s declined at the dynamic cohort gate (%s, %s); it must skip it and be default-deny by its own registration-time totality gate", lane.name, d.Stage, d.Reason)
		}
	}
}

// TestEntryPointDiscoveryIsReceiverAgnostic is the BITE for the discovery above: an
// exported Admit* declaration must be found whatever receiver it carries.
//
// It drives the REAL admissionEntryPoints over a synthetic file, rather than restating the
// predicate, so narrowing that predicate turns this red too — the false-green a bot review
// identified in the first version, which had its own inline copy of the rule.
func TestEntryPointDiscoveryIsReceiverAgnostic(t *testing.T) {
	const synthetic = `package admission

func AdmitPackageLevel()                     {}
func (a *Admitter) AdmitOnAdmitter()         {}
func (renamed *Admitter) AdmitRenamedRecv()  {}
func (g *SomeFutureGateway) AdmitElsewhere() {}
func (v Admitter) AdmitValueReceiver()       {}
func AdmitSomethingForTest()                 {}
func NotAnEntryPoint()                       {}
func admitUnexported()                       {}
`
	got := map[string]string{} // name -> receiver type, as the real discovery reports it
	for _, ep := range admissionEntryPoints([]astSource{syntheticSource(t, "synthetic.go", synthetic)}) {
		got[ep.name] = ep.receiver
	}
	for name, wantRecv := range map[string]string{
		"AdmitPackageLevel":     "",
		"AdmitOnAdmitter":       "Admitter",
		"AdmitRenamedRecv":      "Admitter",
		"AdmitElsewhere":        "SomeFutureGateway",
		"AdmitValueReceiver":    "Admitter",
		"AdmitSomethingForTest": "", // discovered, then classified as a synthetic builder
	} {
		recv, ok := got[name]
		if !ok {
			t.Errorf("%s was not discovered; an admission entry point on that receiver shape could evade the cohort-gate proof", name)
			continue
		}
		if recv != wantRecv {
			t.Errorf("%s discovered with receiver %q, want %q", name, recv, wantRecv)
		}
	}
	for _, unwanted := range []string{"NotAnEntryPoint", "admitUnexported"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("%s was discovered as an admission entry point, which it is not", unwanted)
		}
	}
	// The …ForTest split is part of the guard's logic, so pin it here too rather than
	// leaving it to the production scan alone.
	if !isSyntheticClaimBuilder("AdmitSomethingForTest") || isSyntheticClaimBuilder("AdmitPackageLevel") {
		t.Error("the synthetic-claim-builder classification no longer separates …ForTest from real entry points")
	}
}

// TestOnlyDirectParseAdmissionDerivesDirectParse pins where the fifth surface may be
// derived, and what deriving it is allowed to mean.
//
// `/parse/{method}` has no native implementation — worker/parse.go invokes BAML's
// method.Impl / method.StreamImpl directly — so the surface exists for ACCOUNTING:
// AdmitDirectParse runs the same default-deny cohort gate the other four run and then
// refuses unconditionally, and the worker's parse route reports each request to an
// observer that records the resulting decline. Nothing else in the package may derive
// the surface, because anything else deriving it would be a native parse path, and
// that is the scope's S9 to design.
func TestOnlyDirectParseAdmissionDerivesDirectParse(t *testing.T) {
	const owner = "direct_parse.go"
	// The files allowed to name the surface for reasons other than deriving it: the
	// declarations, the label enums and the recorders.
	permitted := map[string]bool{
		"cohort.go":              true,
		"cohort_test_support.go": true,
		"metrics.go":             true,
	}
	sources := packageAST(t)
	// The exclusion list must name REAL files. A stale entry is a silent hole: create a
	// file with that name later and it is excused from the scan without anyone deciding
	// so. (The audit that added this check found exactly one stale name here.)
	present := map[string]bool{}
	for _, src := range sources {
		present[src.name] = true
	}
	for name := range permitted {
		if !present[name] {
			t.Errorf("the exclusion list names %s, which this package no longer declares; a future "+
				"file with that name would be excused from this scan silently", name)
		}
	}
	found := false
	for _, src := range sources {
		switch {
		case permitted[src.name]:
			continue
		case src.name == owner:
			if !mentionsIdent(src, "SurfaceDirectParse") {
				t.Errorf("%s no longer derives SurfaceDirectParse; the fifth surface has lost its accounting", owner)
			}
			found = true
			continue
		}
		// mentionsIdent reads the AST, so a comment that merely NAMES the surface —
		// explaining why a lane does not use it, say — is not a use of it, in a line
		// comment or a block comment alike.
		if mentionsIdent(src, "SurfaceDirectParse") {
			t.Errorf("%s references SurfaceDirectParse: only the direct-parse admission may derive it, and only to refuse", src.name)
		}
	}
	if !found {
		t.Fatalf("%s is missing; the direct-parse surface has no admission entry point", owner)
	}
}

// TestAdmitDirectParseAlwaysDeclines is the behavioural half: whatever identity is
// presented and whichever parse shape it is, the surface refuses — and the REASON
// distinguishes the two refusals honestly.
func TestAdmitDirectParseAlwaysDeclines(t *testing.T) {
	ctx := context.Background()

	// Production identity: nothing enrolled, so the cohort gate refuses first and the
	// dashboard shows the same reason as every other surface.
	for _, stream := range []bool{false, true} {
		cohort, d := AdmitDirectParse(ctx, DirectParseInput{Stream: stream})
		if d == nil {
			t.Fatalf("stream=%v: direct parse was ADMITTED", stream)
		}
		if d.Stage != StageCohort || d.Reason != ReasonCohortNotEnrolled {
			t.Errorf("stream=%v: decline = (%s, %s), want (cohort, cohort_not_enrolled)", stream, d.Stage, d.Reason)
		}
		if cohort != CohortNone {
			t.Errorf("stream=%v: cohort = %q, want none", stream, cohort)
		}
	}

	// ENROLLED identity: the policy would permit the class, and the surface STILL
	// refuses — with the distinct reason that says why. This is the property that
	// makes enrollment safe: it can never conjure a native parse path.
	in := DirectParseInput{Cohort: testIdentity(t)}
	cohort, d := AdmitDirectParse(ctx, in)
	if d == nil {
		t.Fatal("an enrolled cohort ADMITTED direct parse; enrollment must not create a native parse path")
	}
	if d.Reason != ReasonDirectParseUnproven {
		t.Errorf("enrolled decline reason = %q, want direct_parse_unproven", d.Reason)
	}
	if cohort != "proof" {
		t.Errorf("enrolled cohort = %q, want the enrolled bucket (the decline must stay attributable)", cohort)
	}

	// A cancelled request declines before anything else, like every other lane.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, d := AdmitDirectParse(cancelled, DirectParseInput{}); d == nil || d.Stage != StageContext {
		t.Errorf("cancelled direct parse: decline = %v, want the context stage", d)
	}
}

// TestLaneSurfacesAreDerivedNotSupplied proves the surface cannot be forged: no lane
// input struct carries a Surface field, so every Surface reaching the gate comes from
// the lane's own constant.
//
// The guard has to assert the declarations EXIST before it can assert anything about
// their fields. A bot review found the first version skipped an input struct it could
// not find, so a rename or a split of `StaticStreamInput` would have left this passing
// while inspecting nothing — the classic vacuous structural proof. It now requires
// exactly one declaration of each: absent means the guard lost its subject, and two
// mean it would have inspected an arbitrary one of them.
func TestLaneSurfacesAreDerivedNotSupplied(t *testing.T) {
	sources := packageAST(t)
	// Every input type that reaches an admission entry point. AdmitDirectParse's input
	// is here too: it is a real gate evaluation, so a forgeable Surface on it would be
	// as much a hole as on the other four.
	for _, name := range laneInputStructs() {
		decl, problem := soleStructDecl(sources, name)
		if problem != "" {
			t.Errorf("%s — the surface-is-derived proof cannot inspect it. If the type was "+
				"renamed or split, update laneInputStructs; do not delete the entry.", problem)
			continue
		}
		if field := fieldsOfType(decl.spec, "Surface"); field != "" {
			t.Errorf("%s: %s.%s is a caller-supplied Surface field; the surface must be derived by the lane",
				decl.file, name, field)
		}
	}
}

// laneInputStructs names every input type that reaches an admission entry point.
func laneInputStructs() []string {
	return []string{"Input", "StaticInput", "StaticStreamInput", "DirectParseInput"}
}

// fieldsOfType returns the name of the first field whose type is the named type, or ""
// if the struct has none. Embedded fields count: an embedded Surface would be as
// settable by a caller as a named one.
func fieldsOfType(st *ast.StructType, typeName string) string {
	if st.Fields == nil {
		return ""
	}
	for _, f := range st.Fields.List {
		id, ok := f.Type.(*ast.Ident)
		if !ok || id.Name != typeName {
			continue
		}
		if len(f.Names) == 0 {
			return typeName // embedded
		}
		return f.Names[0].Name
	}
	return ""
}

// TestLaneInputDiscoveryBites is the BITE for the guard above. It drives the REAL
// soleStructDecl / fieldsOfType over a synthetic file — rather than restating their rules
// — so narrowing either turns this red as well.
func TestLaneInputDiscoveryBites(t *testing.T) {
	const synthetic = `package admission

type Input struct {
	Mode    Mode
	Surface Surface
}

type StaticInput struct {
	Mode Mode
}

type StaticInput struct {
	Other int
}

type EmbeddedInput struct {
	Surface
	Mode Mode
}
`
	sources := []astSource{syntheticSource(t, "synthetic.go", synthetic)}

	// ABSENT: the type this guard exists to inspect is not declared at all.
	if _, problem := soleStructDecl(sources, "StaticStreamInput"); problem == "" {
		t.Error("an ABSENT lane input was accepted for inspection; the guard would run on nothing")
	}
	// DUPLICATED: two declarations, so inspecting "the" struct is meaningless.
	if _, problem := soleStructDecl(sources, "StaticInput"); problem == "" {
		t.Error("a DUPLICATED lane input was accepted for inspection; the guard would inspect an arbitrary one")
	}
	// FORGED: a caller-supplied Surface field is seen.
	one, problem := soleStructDecl(sources, "Input")
	if problem != "" {
		t.Fatalf("Input: %s", problem)
	}
	if got := fieldsOfType(one.spec, "Surface"); got != "Surface" {
		t.Errorf("fieldsOfType(Input) = %q, want the forged Surface field", got)
	}
	// EMBEDDED: an embedded Surface is just as settable by a caller.
	emb, problem := soleStructDecl(sources, "EmbeddedInput")
	if problem != "" {
		t.Fatalf("EmbeddedInput: %s", problem)
	}
	if got := fieldsOfType(emb.spec, "Surface"); got != "Surface" {
		t.Errorf("fieldsOfType(EmbeddedInput) = %q, want the embedded Surface field", got)
	}
	// CLEAN: a struct with no Surface field reports none. Read off the duplicate set so
	// the helper is exercised on a real declaration rather than skipped.
	if got := fieldsOfType(structTypeDecls(sources, "StaticInput")[0].spec, "Surface"); got != "" {
		t.Errorf("fieldsOfType(StaticInput) = %q, want none", got)
	}
	// The production guard must actually ASK about every lane input; a shrinking list is
	// the other way this proof goes quiet.
	if len(laneInputStructs()) != 4 {
		t.Errorf("laneInputStructs names %d types, want the four admission inputs", len(laneInputStructs()))
	}
}

// --- strict decoders ---------------------------------------------------------

func TestParseConfigFingerprintRejectsHostileInput(t *testing.T) {
	// Two independent rules must both hold: the OPAQUE FORM (cfg + 3-6 digits, which
	// cannot spell a name, a URL, a header value or a credential) and membership in
	// the FINITE DECLARED VOCABULARY. The first block is rejected by the form, the
	// second by the vocabulary — and the review that prompted both is the reason
	// `gpt-4o-acme-tuned-2026` and `sk-live-…` head the list: the previous grammar
	// accepted them, and published them as a metric label.
	for _, bad := range []string{
		"",
		"gpt-4o-acme-tuned-2026",
		"sk-live-abcdefghijklmnop",
		"ghp_abcdefghijklmnop",
		"akiaabcdefghijklmnop",
		"api-key-1",
		"cfg-proof-suite",
		"Cfg900",
		"cfg 900",
		"cfg900x",
		"xcfg900",
		"cfg90",      // too few digits
		"cfg9000000", // too many digits
		"cfg",        // no digits
		"https://api.openai.com/v1",
		"Bearer abc",
		"user@example.com",
		"acme-prod-openai-client",
		strings.Repeat("9", 64),
	} {
		if got, err := parseConfigFingerprint(bad); err == nil {
			t.Errorf("parseConfigFingerprint(%q) = %q, want an error", bad, got)
		}
	}
	// Well-formed but UNDECLARED: the form is right, the vocabulary is the fence.
	// (cfg001..cfg016 ARE declared slots; these are outside that range.)
	for _, undeclared := range []string{"cfg017", "cfg123", "cfg999999"} {
		if configFingerprintForm.MatchString(undeclared) == false {
			t.Fatalf("%q should match the opaque form; the test below would be vacuous", undeclared)
		}
		if got, err := parseConfigFingerprint(undeclared); err == nil {
			t.Errorf("parseConfigFingerprint(%q) = %q, want a vocabulary rejection", undeclared, got)
		}
	}
	// Every declared ID round-trips.
	for _, declared := range declaredConfigFingerprints() {
		if got, err := parseConfigFingerprint(string(declared)); err != nil || got != declared {
			t.Errorf("parseConfigFingerprint(%q) = (%q, %v), want the declared ID", declared, got, err)
		}
	}
}

// TestDeclaredFingerprintVocabularyIsOpaqueAndTiny pins the vocabulary itself: every
// declared ID is of the opaque form (so no entry can ever be a name), and the list
// stays small enough to be a reviewed allowlist rather than a namespace.
func TestDeclaredFingerprintVocabularyIsOpaqueAndTiny(t *testing.T) {
	declared := declaredConfigFingerprints()
	if len(declared) > maxInventoryRecords {
		t.Fatalf("the declared vocabulary has %d entries, cap is %d", len(declared), maxInventoryRecords)
	}
	seen := map[ConfigFingerprint]bool{}
	for _, fp := range declared {
		if !configFingerprintForm.MatchString(string(fp)) {
			t.Errorf("declared fingerprint %q is not of the opaque form", fp)
		}
		if seen[fp] {
			t.Errorf("declared fingerprint %q appears twice", fp)
		}
		seen[fp] = true
	}
	// The vocabulary is a namespace of SLOTS; the shipped inventory assigns exactly
	// one of them (fe-v1). A second assigned slot is a second declared class, which
	// is a reviewed enrollment decision and not something a vocabulary edit may make.
	shipped := ProductionCohortGate().Inventory()
	if shipped.Len() != 1 {
		t.Fatalf("the shipped inventory declares %d records, want exactly 1 (fe-v1)", shipped.Len())
	}
	if _, ok := shipped.Lookup(FeV1ConfigFingerprint); !ok {
		t.Fatalf("the shipped inventory does not declare %q", FeV1ConfigFingerprint)
	}
	if !seen[FeV1ConfigFingerprint] {
		t.Fatalf("the assigned fe-v1 slot %q is not in the declared vocabulary", FeV1ConfigFingerprint)
	}
	if !seen[proofConfigFingerprint] {
		t.Fatalf("the proof slot %q is not in the declared vocabulary", proofConfigFingerprint)
	}
	if FeV1ConfigFingerprint == proofConfigFingerprint {
		t.Fatal("the fe-v1 slot is the test-only proof slot; the enrolled identity must not be the proof value")
	}
}

func TestParseCohortIDRejectsHostileInput(t *testing.T) {
	for _, bad := range []string{
		"", "None", "none", "unrecognized", "0leading", "_leading", "has-dash", "has space",
		"UPPER", "sk-secret", strings.Repeat("a", maxCohortIDLen+1),
	} {
		if got, err := parseCohortID(bad); err == nil {
			t.Errorf("parseCohortID(%q) = %q, want an error", bad, got)
		}
	}
	for _, good := range []string{"proof", "fe_v1", "a", "openai_unary_1"} {
		if _, err := parseCohortID(good); err != nil {
			t.Errorf("parseCohortID(%q) errored: %v", good, err)
		}
	}
}

func TestParseApprovalRefRejectsHostileInput(t *testing.T) {
	for _, bad := range []string{
		"", "DEBAML", "-1", "DEBAML-", "debaml-1", "DEBAML-1a", "DEBAML 1",
		"https://tracker.example/DEBAML-1", "sk-1", "DEBAML-1234567890",
		strings.Repeat("A", maxApprovalTagLen+1) + "-1",
	} {
		if got, err := parseApprovalRef(bad); err == nil {
			t.Errorf("parseApprovalRef(%q) = %q, want an error", bad, got)
		}
	}
	for _, good := range []string{"DEBAML-673", "A-0", "ABCDEFGHIJKLMNOP-999999999"} {
		if _, err := parseApprovalRef(good); err != nil {
			t.Errorf("parseApprovalRef(%q) errored: %v", good, err)
		}
	}
}

// --- inventory / policy / gate constructors ----------------------------------

func TestNewConfigInventoryRejectsMalformedManifests(t *testing.T) {
	ok := ConfigRecord{Fingerprint: proofConfigFingerprint, Cohort: "a", Surfaces: []Surface{SurfaceDynamicCall}, Provider: ConfigProviderOpenAI, Approval: "DEBAML-1"}
	mutate := func(f func(*ConfigRecord)) []ConfigRecord {
		r := cloneRecord(ok)
		f(&r)
		return []ConfigRecord{r}
	}
	cases := []struct {
		name    string
		records []ConfigRecord
	}{
		{"fingerprint outside the opaque form", mutate(func(r *ConfigRecord) { r.Fingerprint = "acme-prod-openai" })},
		{"fingerprint outside the declared vocabulary", mutate(func(r *ConfigRecord) { r.Fingerprint = "cfg017" })},
		{"reserved cohort", mutate(func(r *ConfigRecord) { r.Cohort = CohortNone })},
		{"unknown provider class", mutate(func(r *ConfigRecord) { r.Provider = "acme-internal" })},
		{"bad approval", mutate(func(r *ConfigRecord) { r.Approval = "see the wiki" })},
		{"no surfaces", mutate(func(r *ConfigRecord) { r.Surfaces = nil })},
		{"invalid surface", mutate(func(r *ConfigRecord) { r.Surfaces = []Surface{surfaceInvalid} })},
		{"duplicate surface", mutate(func(r *ConfigRecord) {
			r.Surfaces = []Surface{SurfaceDynamicCall, SurfaceDynamicCall}
		})},
		// Two records claiming one (cohort, surface) is unreachable while the declared
		// vocabulary has a single entry (a duplicate fingerprint is caught first), so
		// the guard is driven directly instead of through a manifest that cannot exist.
		{"duplicate fingerprint (also the collision guard)", []ConfigRecord{ok, ok}},
		{"over the record cap", overCapRecords()},
	}
	for _, c := range cases {
		if _, err := newConfigInventory(c.records); err == nil {
			t.Errorf("%s: NewConfigInventory accepted a malformed manifest", c.name)
		}
	}
	if _, err := newConfigInventory([]ConfigRecord{ok}); err != nil {
		t.Fatalf("the well-formed control manifest was rejected: %v", err)
	}
}

// overCapRecords is maxInventoryRecords+1 rows. They all carry the one declared
// fingerprint (the vocabulary has a single entry), which is fine here: the cap is
// checked BEFORE any per-record validation, which is exactly the ordering this
// exercises — a manifest cannot get past the cardinality fence by being malformed.
func overCapRecords() []ConfigRecord {
	out := make([]ConfigRecord, 0, maxInventoryRecords+1)
	for i := 0; i <= maxInventoryRecords; i++ {
		out = append(out, ConfigRecord{
			Fingerprint: proofConfigFingerprint,
			Cohort:      "a",
			Surfaces:    []Surface{SurfaceDynamicCall},
			Provider:    ConfigProviderOpenAI,
			Approval:    "DEBAML-1",
		})
	}
	return out
}

// TestInventoryRecordsAreDefensiveCopies proves an operator-facing caller cannot
// mutate the frozen inventory through the rows it is handed.
func TestInventoryRecordsAreDefensiveCopies(t *testing.T) {
	src := []ConfigRecord{{Fingerprint: proofConfigFingerprint, Cohort: "a", Surfaces: []Surface{SurfaceDynamicCall}, Provider: ConfigProviderOpenAI, Approval: "DEBAML-1"}}
	inv, err := newConfigInventory(src)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	src[0].Surfaces[0] = SurfaceDynamicStream // mutate the caller's slice
	got := inv.Records()
	if got[0].Surfaces[0] != SurfaceDynamicCall {
		t.Fatal("mutating the constructor's input slice changed the frozen inventory")
	}
	got[0].Surfaces[0] = SurfaceStaticStream // mutate the returned rows
	if again := inv.Records(); again[0].Surfaces[0] != SurfaceDynamicCall {
		t.Fatal("mutating a returned record changed the frozen inventory")
	}
	if r, ok := inv.Lookup(proofConfigFingerprint); !ok || r.Surfaces[0] != SurfaceDynamicCall {
		t.Fatal("Lookup returned a mutated record")
	}
}

// TestNewCohortGateRequiresInventorySubstantiation is what makes the policy
// EVIDENCE: an enrollment nobody inventoried, or one on a surface the record does
// not declare, fails to build.
func TestNewCohortGateRequiresInventorySubstantiation(t *testing.T) {
	inv, err := newConfigInventory([]ConfigRecord{{
		Fingerprint: proofConfigFingerprint, Cohort: "a", Surfaces: []Surface{SurfaceDynamicCall},
		Provider: ConfigProviderOpenAI, Approval: "DEBAML-1",
	}})
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	unknownCohort, err := newCohortPolicy("v", CohortEnrollment{Surface: SurfaceDynamicCall, Cohort: "ghost"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := newCohortGate(unknownCohort, inv); err == nil {
		t.Error("NewCohortGate accepted an enrollment with no inventory record")
	}
	wrongSurface, err := newCohortPolicy("v", CohortEnrollment{Surface: SurfaceStaticCall, Cohort: "a"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := newCohortGate(wrongSurface, inv); err == nil {
		t.Error("NewCohortGate accepted an enrollment on a surface the record does not declare")
	}
	good, err := newCohortPolicy("v", CohortEnrollment{Surface: SurfaceDynamicCall, Cohort: "a"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, err := newCohortGate(good, inv); err != nil {
		t.Fatalf("the substantiated control was rejected: %v", err)
	}
}

// --- the no-send observe identity is not a serving permission -----------------

func TestNoUntaggedExportedGateInjection(t *testing.T) {
	// (1) The production policy enrolls nothing at all — no cohort, reserved or not.
	for _, s := range AllSurfaces() {
		for _, c := range []CohortID{"proof", "observe_only", "mutant_a", CohortNone, CohortUnrecognized} {
			if ProductionCohortGate().Policy().Enrolled(s, c) {
				t.Errorf("%s: the shipped policy enrolls %q", s.Label(), c)
			}
		}
	}

	// (2) CohortInput exposes NO exported field that can carry a gate. This is the
	// structural half of the fix: whatever an external caller composes, it resolves
	// against ProductionCohortGate.
	typ := reflect.TypeOf(CohortInput{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Type == reflect.TypeOf((*CohortGate)(nil)) {
			t.Errorf("CohortInput.%s is an exported *CohortGate: a released consumer could select its own admission policy", f.Name)
		}
	}
	if _, ok := typ.FieldByName("Gate"); ok {
		t.Error("CohortInput.Gate is exported again; the gate override must stay unexported")
	}

	// (3) No UNTAGGED source in this package exports a function that hands out or
	// accepts a *CohortGate, other than the read-only production accessor. A cold
	// review found the first draft shipped exactly such a door (an exported proof
	// gate plus an exported override field), so this is a standing guard rather than
	// a one-off check.
	inspected, doors := untaggedExportedGateDoors(packageAST(t))
	for _, d := range doors {
		t.Errorf("%s: exported %s mentions %s in its signature in untagged source; "+
			"a released consumer could select its own admission policy", d.file, d.name, d.mention)
	}
	// NON-VACUITY: if nothing was inspected the scan above proves nothing. The package
	// exports a great deal of untagged API, so zero means discovery broke.
	if inspected == 0 {
		t.Fatal("no untagged exported functions were inspected; the no-injection guard is vacuous")
	}
}

// gateDoor is an untagged exported function that hands a *CohortGate across the package
// boundary — i.e. a finding.
type gateDoor struct {
	file    string
	name    string
	mention string
}

// untaggedExportedGateDoors is THE gate-injection predicate, run by both the production
// guard above and the nested-signature bite below. It returns how many untagged exported
// functions it inspected (the non-vacuity number) and the doors it found.
//
// The signature is read from the AST. The original regex bounded the parameter list with
// `[^)]*`, which stops at the FIRST `)` — so an exported function taking
// `func(*CohortGate) error`, or returning one from a nested type, was invisible to it. No
// such function exists today; the guard simply could not have said so.
func untaggedExportedGateDoors(sources []astSource) (inspected int, doors []gateDoor) {
	// The read-only production accessor is the one permitted mention.
	allowed := map[string]bool{"ProductionCohortGate": true}
	untagged := make([]astSource, 0, len(sources))
	for _, src := range sources {
		if src.tagged {
			continue // opt-in tag: cannot be linked by a released consumer
		}
		untagged = append(untagged, src)
	}
	for _, fn := range discoverFuncs(untagged, func(fn *ast.FuncDecl) bool { return fn.Name.IsExported() }) {
		if allowed[fn.name] {
			continue
		}
		inspected++
		// A *CohortGate ANYWHERE in the signature is a door: handing one in selects a
		// policy, handing one out lets a caller select one later.
		if hit := signatureMentions(fn.decl, "CohortGate"); hit != "" {
			doors = append(doors, gateDoor{file: fn.file, name: fn.name, mention: hit})
			continue
		}
		// A RETURNED CohortInput is a door too, and the audit that added this arm found
		// the guard had been blind to it: CohortInput's gate field is unexported, so a
		// caller cannot BUILD an enrolled one — but a function that HANDS one BACK can
		// give away exactly the enrolled identity the unexported field was protecting.
		// (`ProofCohortInputForTest` is precisely that function, and it is tagged. If it
		// were ever untagged, only this arm would notice.) Taking one as a PARAMETER is
		// harmless by the same reasoning: whatever a caller can construct carries no gate.
		if hit := resultsMention(fn.decl, "CohortInput"); hit != "" {
			doors = append(doors, gateDoor{file: fn.file, name: fn.name, mention: "returned " + hit})
		}
	}
	return inspected, doors
}

// TestGateInjectionGuardSeesNestedSignatures is the BITE for the predicate above: a
// *CohortGate buried inside a nested parameter type, a slice, a map or a return position
// must be caught. It drives the REAL untaggedExportedGateDoors over a synthetic source, so
// narrowing that function fails here too.
func TestGateInjectionGuardSeesNestedSignatures(t *testing.T) {
	const synthetic = `package admission

func NestedParam(cb func(*CohortGate) error) {}
func SliceParam(g []*CohortGate)             {}
func MapParam(g map[string]*CohortGate)      {}
func VariadicParam(g ...*CohortGate)         {}
func NestedResult() func() *CohortGate       { return nil }
func WrappedResult() (*CohortGate, error)    { return nil, nil }
func MultilineParam(
	name string,
	cb func(gate *CohortGate),
) {
}
func Clean(name string) error            { return nil }
func unexportedTakesGate(g *CohortGate)  {}
func ProductionCohortGate() *CohortGate  { return nil }
func HandsBackIdentity() CohortInput     { return CohortInput{} }
func HandsBackIdentityPtr() *CohortInput { return nil }
func TakesIdentity(in CohortInput)       {}
func unexportedHandsBack() CohortInput   { return CohortInput{} }
`
	inspected, doors := untaggedExportedGateDoors([]astSource{syntheticSource(t, "synthetic.go", synthetic)})
	if inspected == 0 {
		t.Fatal("the real predicate inspected nothing in the synthetic source; the bite is vacuous")
	}
	flagged := map[string]bool{}
	for _, d := range doors {
		flagged[d.name] = true
	}
	for _, want := range []string{
		"NestedParam", "SliceParam", "MapParam", "VariadicParam",
		"NestedResult", "WrappedResult", "MultilineParam",
		// The returned-identity arm: handing back a CohortInput hands back whatever
		// unexported gate it carries.
		"HandsBackIdentity", "HandsBackIdentityPtr",
	} {
		if !flagged[want] {
			t.Errorf("%s hands a cohort gate or identity across the package boundary and was NOT flagged", want)
		}
	}
	for _, unwanted := range []string{
		"Clean", "unexportedTakesGate", "ProductionCohortGate",
		// Taking an identity is not a door: a caller can only construct a gateless one.
		"TakesIdentity", "unexportedHandsBack",
	} {
		if flagged[unwanted] {
			t.Errorf("%s was flagged; the guard must not fire on a clean signature, an unexported "+
				"function, the permitted read-only accessor, or an identity PARAMETER", unwanted)
		}
	}
}

// TestUntaggedBuildHasNoEnrollableIdentity is the behavioural companion to the guard
// above: every CohortInput an external caller can actually construct — i.e. one with
// only the exported Fingerprint field set — declines on every surface, whatever it
// puts in that field.
func TestUntaggedBuildHasNoEnrollableIdentity(t *testing.T) {
	for _, fp := range []ConfigFingerprint{
		"", "cfg900", "cfg001", "cfg123456", "proof", "observe_only",
		"gpt-4o-acme-tuned-2026", "sk-live-51H8xQ", "https://acme.example/v1",
	} {
		for _, s := range AllSurfaces() {
			cohort, d := admitCohort(s, CohortInput{Fingerprint: fp})
			if d == nil {
				t.Fatalf("%s: fingerprint %q was ADMITTED against the shipped gate", s.Label(), fp)
			}
			if cohort != CohortNone && cohort != CohortUnrecognized {
				t.Errorf("%s: fingerprint %q resolved to unbounded cohort %q", s.Label(), fp, cohort)
			}
		}
	}
}

// --- provider class / surface enums ------------------------------------------

// TestConfigProviderClassMatchesMetricProviderEnum keeps the inventory's provider
// CLASS set and the attempts metric's provider label set in lockstep: the inventory
// declares exactly the real classes, and the metric adds only its two folding
// buckets. A new provider in one place without the other fails here.
func TestConfigProviderClassMatchesMetricProviderEnum(t *testing.T) {
	metric := map[string]bool{
		string(providerOpenAI): true, string(providerAnthropic): true, string(providerBedrock): true,
		string(providerCerebras): true, string(providerCohere): true,
	}
	inventory := map[string]bool{}
	for _, p := range AllConfigProviderClasses() {
		if !p.Valid() {
			t.Errorf("%q is in AllConfigProviderClasses but not Valid", p)
		}
		inventory[string(p)] = true
	}
	for p := range metric {
		if !inventory[p] {
			t.Errorf("metric provider class %q has no ConfigProviderClass", p)
		}
	}
	for p := range inventory {
		if !metric[p] {
			t.Errorf("ConfigProviderClass %q is not a metric provider class", p)
		}
	}
	if ConfigProviderClass(providerOther).Valid() || ConfigProviderClass(providerUnknown).Valid() {
		t.Error("the metric's folding buckets must not be declarable inventory provider classes")
	}
}

// TestSurfaceLabelsAreTheClosedFive pins the label set the scope names, and pins
// the zero value out of it.
func TestSurfaceLabelsAreTheClosedFive(t *testing.T) {
	want := []string{"dynamic_call", "dynamic_stream", "static_call", "static_stream", "direct_parse"}
	got := make([]string, 0, len(AllSurfaces()))
	seen := map[string]bool{}
	for _, s := range AllSurfaces() {
		if !s.Valid() {
			t.Errorf("AllSurfaces contains an invalid surface %d", s)
		}
		if seen[s.Label()] {
			t.Errorf("duplicate surface label %q", s.Label())
		}
		seen[s.Label()] = true
		got = append(got, s.Label())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("surface labels = %v, want %v", got, want)
	}
	if surfaceInvalid.Valid() || surfaceInvalid.Label() != surfaceLabelInvalid {
		t.Fatal("the zero Surface must be invalid and label as the out-of-band bucket")
	}
	if Surface(200).Valid() {
		t.Fatal("an out-of-range Surface must not be Valid")
	}
}

// TestCredentialShapedIdentifiersAreRejected pins the redaction rule the COHORT-ID
// charset cannot express, and BITES it: with the credential-prefix list emptied (the
// mutation "the redaction leaks"), the same inputs would be accepted.
//
// It applies to cohort IDs only. Configuration IDs no longer need it — their opaque
// form admits no letters at all, so a credential cannot be spelled as one — which is
// itself the stronger fix the review asked for.
func TestCredentialShapedIdentifiersAreRejected(t *testing.T) {
	credentialish := []string{"sk_live_abc", "secret_abc", "token_abc", "apikey_abc", "password_abc", "bearer_abc"}
	for _, v := range credentialish {
		if _, err := parseCohortID(v); err == nil {
			t.Errorf("parseCohortID(%q) accepted a credential-shaped value", v)
		}
	}
	// The BITE: every value above passes the charset rules, so the credential-prefix
	// list is the only thing rejecting them. If it were emptied, the assertions above
	// would pass vacuously — this loop proves they would not.
	leaked := 0
	for _, v := range credentialish {
		if rejectCredentialShaped("cohort ID", v) != nil && charsetOnlyCohortIDOK(v) {
			leaked++
		}
	}
	if leaked != len(credentialish) {
		t.Fatalf("only %d/%d credential-shaped values were rejected SOLELY by the prefix list; the bite is weaker than it claims", leaked, len(credentialish))
	}
}

// charsetOnlyCohortIDOK re-implements parseCohortID's charset half — and ONLY that
// half — so the bite above can show which rule did the rejecting.
func charsetOnlyCohortIDOK(s string) bool {
	if s == "" || len(s) > maxCohortIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '_'):
		default:
			return false
		}
	}
	for _, r := range reservedCohortIDs() {
		if CohortID(s) == r {
			return false
		}
	}
	return true
}

// TestSyntheticClaimBuildersStayBehindTheGatedTag: the …ForTest synthetic-claim
// builders construct a claim WITHOUT running the predicate, so they would be a
// cohort-gate bypass if they could be linked into a production build. They may only
// exist in files carrying the `nanollm_integration` opt-in tag.
func TestSyntheticClaimBuildersStayBehindTheGatedTag(t *testing.T) {
	found := 0
	for _, ep := range admissionEntryPoints(packageAST(t)) {
		if !isSyntheticClaimBuilder(ep.name) {
			continue
		}
		found++
		if !ep.tagged {
			t.Errorf("%s declares the synthetic claim builder %s but is not behind the nanollm_integration tag", ep.file, ep.name)
		}
	}
	// NON-VACUITY. This used to `t.Skip` on zero, which is a pass — so a discovery
	// regression would have retired the guard silently. The package DOES declare these
	// builders (they are read from source, not from the compiled build, so the opt-in tag
	// does not hide them), and if it ever stops, that is a change worth failing on rather
	// than skipping past.
	if found == 0 {
		t.Fatal("no synthetic claim builders were discovered; this guard is the only thing keeping " +
			"a predicate-free claim constructor out of a released build, and it just went vacuous")
	}
}

// TestTheCohortGateIsWhatDeclines is the causal control for the lane tests: the
// SAME lane input that declines at the cohort gate under the production identity
// declines SOMEWHERE ELSE once an enrolled identity is presented. Without it,
// "declines at cohort" could be true for a reason that has nothing to do with the
// gate, and enrolling a cohort in S3 could silently change nothing.
func TestTheCohortGateIsWhatDeclines(t *testing.T) {
	ctx := context.Background()
	a := NewAdmitter(nil, llmhttp.NewExactExecutor(&proofCountingTransport{}))

	in := dynamicGateInput(ModeCall)
	in.Cohort = testIdentity(t)
	_, err := a.Admit(ctx, in)
	d, ok := err.(*Decline)
	if !ok {
		t.Fatalf("err = %v (%T), want *Decline", err, err)
	}
	if d.Stage == StageCohort {
		t.Fatal("an ENROLLED identity still declined at the cohort gate: enrollment has no effect")
	}
	// It declines at the next unsatisfied layer instead — here the absent output
	// schema, which is the layer immediately after the gate.
	if d.Stage != StagePrompt || d.Reason != ReasonOutputSchemaAbsent {
		t.Fatalf("enrolled decline = (%s, %s), want (prompt, output_schema_absent) — the layer right after the gate", d.Stage, d.Reason)
	}
}
