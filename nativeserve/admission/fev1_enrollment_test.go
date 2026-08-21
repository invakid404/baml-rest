package admission

// De-BAML serving cutover S3b — the ENROLLMENT's own biting proof, at the layer
// the enrollment actually lives in.
//
// S3a's identity matrix (identity_test.go) proves WHICH configuration obtains an
// identity. This file asks the question that only exists once something is
// enrolled: given an identity, WHICH request may CLAIM — and it asks it against
// ProductionCohortGate(), the gate the shipped binary uses, not a hand-built one.
//
// It is untagged on purpose. Every predicate here runs before any native work —
// no nanollm New, no render, no Prepare, no socket — so the enrollment's whole
// admission behaviour is provable in the ordinary package test run rather than
// only in the gated lane.
//
// Each positive control is paired with a MUTATION BITE: the same assertions are
// re-run against a deliberately weakened artifact (the enrollment deleted, the
// fingerprint changed, the surface changed, a second cohort added, the strict
// regime downgraded to trusted-provider) and the test FAILS if they still pass.

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
)

// feV1ApprovedClient is the client name the deployment's declaration approves in
// this file. The NAME is deployment-chosen and carries no identity of its own; the
// slot does.
const feV1ApprovedClient = "FeV1ApprovedClient"

// feV1Declaration is a deployment declaration that assigns the ENROLLED slot to a
// real OpenAI configuration. It is what an operator writes to put traffic in the
// fe-v1 cohort.
func feV1Declaration(t *testing.T, fingerprint ConfigFingerprint) *trustedclients.Set {
	t.Helper()
	set, err := trustedclients.Parse(`{"trusted_clients":[{
		"name":"` + feV1ApprovedClient + `",
		"fingerprint":"` + string(fingerprint) + `",
		"provider":"openai",
		"options":{"model":"gpt-4o-mini","base_url":"https://approved.example/v1","api_key":"sk-approved-value"}
	}]}`)
	if err != nil {
		t.Fatalf("trustedclients.Parse: %v", err)
	}
	return set
}

// feV1SealedSelection is the per-request fact set for a request that merely NAMED
// the approved class and had it sealed by the deployment's config-load pass.
func feV1SealedSelection(t *testing.T, fingerprint ConfigFingerprint) ConfigSelection {
	t.Helper()
	reg := &bamlutils.ClientRegistry{
		Primary: identityStr(feV1ApprovedClient),
		Clients: []*bamlutils.ClientProperty{{Name: feV1ApprovedClient}},
	}
	feV1Declaration(t, fingerprint).Seal(reg)
	if _, _, sealed := reg.Clients[0].TrustedConfigSeal(); !sealed {
		t.Fatal("the declaration did not seal a request that merely named the approved class")
	}
	return ConfigSelection{
		Registry:          reg,
		ResolvedProvider:  "openai",
		SelectedLeaf:      feV1ApprovedClient,
		SingleLeaf:        true,
		HasBAMLPlanOracle: true,
	}
}

// feV1CallerSuppliedSelection is the same effective configuration DESCRIBED by the
// request instead of named. It must never obtain an identity, and therefore must
// never claim.
func feV1CallerSuppliedSelection() ConfigSelection {
	reg := &bamlutils.ClientRegistry{
		Primary: identityStr(feV1ApprovedClient),
		Clients: []*bamlutils.ClientProperty{{
			Name:     feV1ApprovedClient,
			Provider: "openai",
			Options: map[string]any{
				"model":    "gpt-4o-mini",
				"base_url": "https://approved.example/v1",
				"api_key":  "sk-approved-value",
			},
		}},
	}
	return ConfigSelection{
		Registry:          reg,
		ResolvedProvider:  "openai",
		SelectedLeaf:      feV1ApprovedClient,
		SingleLeaf:        true,
		HasBAMLPlanOracle: true,
	}
}

// admitThroughGate resolves a selection's identity exactly as the serve seam does
// and evaluates it against a gate. It is the composition under proof: resolver +
// gate, not either half alone.
func admitThroughGate(gate *CohortGate, surface Surface, sel ConfigSelection) (CohortID, *Decline) {
	id := ResolveConfigIdentity(sel)
	return admitCohort(surface, CohortInput{Fingerprint: id.Fingerprint, Provider: id.Provider, gate: gate})
}

// --- 1. only the approved selection claims, through the SHIPPED gate ----------

// TestOnlyTheApprovedSelectionClaimsThroughTheShippedGate runs S3a's whole identity
// biting matrix with the ENROLLED slot sealed, and requires that exactly ONE arm
// reaches an admission — the unmutated, deployment-sealed one — while every
// alternate, aliased, overridden, retried, fallback-routed, round-robined,
// ambiguous, post-seal-mutated and caller-supplied arm declines.
//
// This is the S3b question S3a could not ask: under S3a every arm declined because
// nothing was enrolled, so the matrix could not distinguish "the identity was
// refused" from "nothing can claim". Here exactly one arm claims, which makes every
// other arm's decline load-bearing.
func TestOnlyTheApprovedSelectionClaimsThroughTheShippedGate(t *testing.T) {
	gate := ProductionCohortGate()
	admitted := 0

	for _, c := range identityMatrix() {
		for _, provenance := range []struct {
			name   string
			build  func() ConfigSelection
			sealed bool
		}{
			{name: "sealed by the deployment", build: func() ConfigSelection { return feV1SealedSelection(t, FeV1ConfigFingerprint) }, sealed: true},
			{name: "supplied by the caller", build: feV1CallerSuppliedSelection},
		} {
			sel := provenance.build()
			// identityMatrix's rename mutations spell their new names from the S3a
			// fixture's constant, which is a DIFFERENT name from this fixture's. That
			// is still exactly the mutation each arm describes — renaming the selected
			// client away from the one the deployment sealed — so the arms stay
			// meaningful here; only the literal they rename TO differs.
			c.mutate(&sel)
			wantAdmit := c.identity && provenance.sealed

			cohort, dec := admitThroughGate(gate, SurfaceDynamicCall, sel)
			gotAdmit := dec == nil
			if gotAdmit != wantAdmit {
				t.Errorf("%s / %s: admitted = %v (cohort %q), want %v", c.name, provenance.name, gotAdmit, cohort, wantAdmit)
				continue
			}
			if gotAdmit {
				admitted++
				if cohort != FeV1Cohort {
					t.Errorf("%s / %s: admitted under cohort %q, want %q", c.name, provenance.name, cohort, FeV1Cohort)
				}
				continue
			}
			if dec.Stage != StageCohort || dec.Reason != ReasonCohortNotEnrolled {
				t.Errorf("%s / %s: decline = (%s, %s), want (cohort, cohort_not_enrolled)", c.name, provenance.name, dec.Stage, dec.Reason)
			}
			if cohort == FeV1Cohort {
				t.Errorf("%s / %s: a declined request was attributed to the APPROVED cohort", c.name, provenance.name)
			}
		}
	}

	if admitted != 1 {
		t.Fatalf("%d selection(s) were admitted, want exactly 1 — the enrolled tuple and nothing else", admitted)
	}
}

// TestTheApprovedSelectionClaimsOnlyTheEnrolledSurface pins the surface half: the
// same approved, sealed selection is admitted on dynamic_call and refused on every
// other surface, because the record declares one surface and the policy enrolls one
// pair.
func TestTheApprovedSelectionClaimsOnlyTheEnrolledSurface(t *testing.T) {
	gate := ProductionCohortGate()
	for _, s := range AllSurfaces() {
		sel := feV1SealedSelection(t, FeV1ConfigFingerprint)
		cohort, dec := admitThroughGate(gate, s, sel)
		if s == SurfaceDynamicCall {
			if dec != nil {
				t.Fatalf("dynamic_call: the approved selection was declined: %v", dec)
			}
			if cohort != FeV1Cohort {
				t.Fatalf("dynamic_call: cohort = %q, want %q", cohort, FeV1Cohort)
			}
			continue
		}
		if dec == nil {
			t.Errorf("%s: the approved selection was ADMITTED on a surface the record does not declare", s.Label())
		}
		if cohort == FeV1Cohort {
			t.Errorf("%s: a wrong-surface request was attributed to the approved cohort %q", s.Label(), FeV1Cohort)
		}
	}
}

// TestAnUnenrolledSlotNeverInheritsTheEnrolledCohort is the slot half: the SAME
// deployment sealing the SAME configuration under a different declared slot gets a
// bounded, non-enrolled bucket. Enrollment is per SLOT, never per provider or per
// configuration shape.
func TestAnUnenrolledSlotNeverInheritsTheEnrolledCohort(t *testing.T) {
	gate := ProductionCohortGate()
	for _, fp := range declaredConfigFingerprints() {
		sel := feV1SealedSelection(t, fp)
		cohort, dec := admitThroughGate(gate, SurfaceDynamicCall, sel)
		if fp == FeV1ConfigFingerprint {
			if dec != nil {
				t.Fatalf("%s: the ENROLLED slot was declined: %v", fp, dec)
			}
			continue
		}
		if dec == nil {
			t.Errorf("%s: an unenrolled slot was admitted", fp)
		}
		if cohort != CohortUnrecognized {
			t.Errorf("%s: resolved cohort %q, want %q", fp, cohort, CohortUnrecognized)
		}
	}
}

// --- 2. the enrollment is what admits (mutation bites) -----------------------

// feV1MutantGate builds a gate from the SHIPPED manifests with one thing changed.
// Nothing in production builds one; they exist so each half of the enrollment can
// be shown to be load-bearing.
func feV1MutantGate(t *testing.T, records []ConfigRecord, enrollments []CohortEnrollment) *CohortGate {
	t.Helper()
	g, err := buildCohortGate("mutant-policy", records, enrollments)
	if err != nil {
		t.Fatalf("mutant gate: %v", err)
	}
	return g
}

func feV1ShippedRecords() []ConfigRecord         { return productionInventoryRecords() }
func feV1ShippedEnrollments() []CohortEnrollment { return productionEnrollments() }

// TestDeletingTheEnrollmentStopsTheApprovedSelectionClaiming is the primary bite:
// remove the ONE policy line and the approved, sealed, correctly-identified
// selection must stop claiming. It proves the enrollment — not the seal, not the
// resolver, not the record — is what permits the claim.
func TestDeletingTheEnrollmentStopsTheApprovedSelectionClaiming(t *testing.T) {
	sel := feV1SealedSelection(t, FeV1ConfigFingerprint)
	if _, dec := admitThroughGate(ProductionCohortGate(), SurfaceDynamicCall, sel); dec != nil {
		t.Fatalf("the approved selection does not claim under the SHIPPED gate (%v); the bite below would be vacuous", dec)
	}

	// Same records, NO enrollment.
	gate := feV1MutantGate(t, feV1ShippedRecords(), nil)
	if _, dec := admitThroughGate(gate, SurfaceDynamicCall, feV1SealedSelection(t, FeV1ConfigFingerprint)); dec == nil {
		t.Fatal("the approved selection still claimed with the enrollment deleted: something other than the policy is permitting native traffic")
	}
}

// TestChangingTheEnrolledFingerprintStopsTheApprovedSelectionClaiming is the record
// half of the same bite: keep the enrollment, point the record at a different slot,
// and the approved selection must stop claiming.
func TestChangingTheEnrolledFingerprintStopsTheApprovedSelectionClaiming(t *testing.T) {
	records := append([]ConfigRecord(nil), feV1ShippedRecords()...)
	if len(records) != 1 {
		t.Fatalf("the shipped manifest declares %d records; this bite assumes the one fe-v1 record", len(records))
	}
	records[0].Fingerprint = "cfg002"

	gate := feV1MutantGate(t, records, feV1ShippedEnrollments())
	if _, dec := admitThroughGate(gate, SurfaceDynamicCall, feV1SealedSelection(t, FeV1ConfigFingerprint)); dec == nil {
		t.Fatal("the approved selection still claimed after the record's fingerprint moved: the opaque slot is not actually the join key")
	}
	// And the moved slot claims in its place, which proves the gate followed the
	// record rather than simply refusing everything.
	if _, dec := admitThroughGate(gate, SurfaceDynamicCall, feV1SealedSelection(t, "cfg002")); dec != nil {
		t.Fatalf("the moved slot did not claim (%v); the bite above would pass for the wrong reason", dec)
	}
}

// TestEnrollingTheWrongSurfaceDoesNotServeTheDynamicCall is the surface bite: move
// the one enrollment to another surface and dynamic_call must stop claiming.
func TestEnrollingTheWrongSurfaceDoesNotServeTheDynamicCall(t *testing.T) {
	records := append([]ConfigRecord(nil), feV1ShippedRecords()...)
	records[0].Surfaces = []Surface{SurfaceDynamicStream}
	gate := feV1MutantGate(t, records, []CohortEnrollment{{Surface: SurfaceDynamicStream, Cohort: FeV1Cohort}})

	if _, dec := admitThroughGate(gate, SurfaceDynamicCall, feV1SealedSelection(t, FeV1ConfigFingerprint)); dec == nil {
		t.Fatal("dynamic_call still claimed with the enrollment moved to dynamic_stream: the surface is not part of the permission")
	}
}

// TestASecondCohortOnTheSameSurfaceIsRefusedAtConstruction pins the "exactly one
// tuple" property structurally: two records cannot both claim the enrolled cohort
// on the enrolled surface, and a policy enrolling a cohort no record substantiates
// does not build at all. Either mistake fails at CONSTRUCTION — at worker boot —
// rather than at admission time on live traffic.
func TestASecondCohortOnTheSameSurfaceIsRefusedAtConstruction(t *testing.T) {
	second := feV1ShippedRecords()[0]
	second.Fingerprint = "cfg002"
	if _, err := buildCohortGate("mutant-policy",
		append(append([]ConfigRecord(nil), feV1ShippedRecords()...), second),
		feV1ShippedEnrollments()); err == nil {
		t.Error("two records claiming (fe_v1, dynamic_call) built a gate; the collision must be refused")
	}

	if _, err := buildCohortGate("mutant-policy", feV1ShippedRecords(),
		append(append([]CohortEnrollment(nil), feV1ShippedEnrollments()...),
			CohortEnrollment{Surface: SurfaceStaticCall, Cohort: "unsubstantiated"})); err == nil {
		t.Error("a policy enrolling a cohort no record substantiates built a gate; the cross-check must refuse it")
	}
}

// --- 3. the strict-OpenAI verification regime --------------------------------

// TestRequiredVerificationPermitsExactlyItsOwnRegime is the unit half of the S3b
// regime check.
func TestRequiredVerificationPermitsExactlyItsOwnRegime(t *testing.T) {
	for _, tc := range []struct {
		required RequiredVerification
		policy   VerificationPolicy
		permits  bool
	}{
		{VerificationStrictOpenAI, PolicyStrictOpenAI, true},
		{VerificationStrictOpenAI, PolicyTrustedProvider, false},
		{VerificationTrustedProvider, PolicyTrustedProvider, true},
		{VerificationTrustedProvider, PolicyStrictOpenAI, false},
		// The zero value is what a CONFIG-DECLARED row carries. It permits
		// everything, which is safe only because such a row can never be enrolled —
		// a property TestEveryEnrolledProductionRecordDeclaresTheStrictOpenAIRegime
		// and TestConfigLoadCanDeclareButNeverEnroll hold from the other side.
		{VerificationUnconstrained, PolicyStrictOpenAI, true},
		{VerificationUnconstrained, PolicyTrustedProvider, true},
	} {
		if got := tc.required.Permits(tc.policy); got != tc.permits {
			t.Errorf("%s.Permits(%s) = %v, want %v", tc.required.Label(), tc.policy, got, tc.permits)
		}
	}
	if RequiredVerification(200).Valid() {
		t.Error("an out-of-range verification regime reported Valid")
	}
	if _, err := newConfigInventory([]ConfigRecord{{
		Fingerprint: FeV1ConfigFingerprint, Cohort: FeV1Cohort, Surfaces: []Surface{SurfaceDynamicCall},
		Provider: ConfigProviderOpenAI, Verification: RequiredVerification(200), Approval: FeV1Approval,
	}}); err == nil {
		t.Error("an inventory record carrying an out-of-range verification regime was accepted")
	}
}

// TestTheEnrolledClassRefusesTheTrustedRegimeAgainstTheRealMapper is the composition
// proof, and it is deliberately built from the REAL mapper rather than from an
// assumed class->regime table: mapClientConfig is pure (it constructs no engine and
// opens no socket), so the regime the production mapper would assign can be resolved
// here and fed straight to the production check.
//
// openai maps to the strict anchor and the enrolled record admits it; a
// trusted-provider class maps to the trusted regime and the SAME record refuses it
// pre-claim with the bounded verification reason.
func TestTheEnrolledClassRefusesTheTrustedRegimeAgainstTheRealMapper(t *testing.T) {
	gate := ProductionCohortGate()
	in := CohortInput{Fingerprint: FeV1ConfigFingerprint, Provider: ConfigProviderOpenAI, gate: gate}

	strict, dec, err := mapClientConfig(context.Background(), mappingInput{
		registry: s1Registry("openai"), alias: "__fe_v1_alias__", resolvedProvider: "openai",
	})
	if err != nil || dec != nil {
		t.Fatalf("the real mapper refused the strict OpenAI shape: dec=%v err=%v", dec, err)
	}
	if strict.verification != PolicyStrictOpenAI {
		t.Fatalf("the real mapper assigned %s to openai; the enrollment assumes strict_openai", strict.verification)
	}
	if d := admitVerification(in, strict.verification); d != nil {
		t.Fatalf("the enrolled record refused the regime it was approved for: %v", d)
	}

	trusted, dec, err := mapClientConfig(context.Background(), mappingInput{
		registry: s1Registry("anthropic"), alias: "__fe_v1_alias__", resolvedProvider: "anthropic",
	})
	if err != nil || dec != nil {
		t.Fatalf("the real mapper refused a trusted-provider shape: dec=%v err=%v", dec, err)
	}
	if trusted.verification != PolicyTrustedProvider {
		t.Fatalf("the real mapper assigned %s to anthropic; this control assumes trusted_provider", trusted.verification)
	}
	d := admitVerification(in, trusted.verification)
	if d == nil {
		t.Fatal("the enrolled record permitted the TRUSTED regime: fe-v1 would then serve natively with NEITHER retained BAML oracle running")
	}
	if d.Stage != StageVerification || d.Reason != ReasonVerificationUnapproved {
		t.Errorf("decline = (%s, %s), want (verification, verification_regime_unapproved)", d.Stage, d.Reason)
	}
	if containsAny(d.Detail, "sk-", "https://", "gpt-", "SECRET") {
		t.Errorf("the verification decline detail leaked configuration material: %q", d.Detail)
	}
}

// TestWeakeningTheEnrolledRegimeToTrustedProviderChangesAdmission is the bite the
// scope names explicitly: downgrade the enrolled record from strict OpenAI to
// trusted-provider and admission must change. Without this, "the record declares
// strict" would be a comment rather than a checked property.
func TestWeakeningTheEnrolledRegimeToTrustedProviderChangesAdmission(t *testing.T) {
	weakened := append([]ConfigRecord(nil), feV1ShippedRecords()...)
	weakened[0].Verification = VerificationTrustedProvider
	gate := feV1MutantGate(t, weakened, feV1ShippedEnrollments())
	in := CohortInput{Fingerprint: FeV1ConfigFingerprint, Provider: ConfigProviderOpenAI, gate: gate}

	// The weakened record now REFUSES the strict regime the real openai mapper
	// assigns — i.e. it no longer describes the class it is enrolled for.
	if d := admitVerification(in, PolicyStrictOpenAI); d == nil {
		t.Fatal("weakening the enrolled record to trusted-provider did not change admission: the declared regime is not enforced")
	}
	// And it would have PERMITTED the regime that runs neither retained oracle,
	// which is exactly the widening the check exists to refuse.
	if d := admitVerification(in, PolicyTrustedProvider); d != nil {
		t.Fatalf("the weakened record refused the trusted regime too (%v); the bite above passed for the wrong reason", d)
	}

	// The SHIPPED record is the other way round on both counts.
	shipped := CohortInput{Fingerprint: FeV1ConfigFingerprint, Provider: ConfigProviderOpenAI, gate: ProductionCohortGate()}
	if d := admitVerification(shipped, PolicyStrictOpenAI); d != nil {
		t.Errorf("the shipped record refuses strict_openai: %v", d)
	}
	if d := admitVerification(shipped, PolicyTrustedProvider); d == nil {
		t.Error("the shipped record permits trusted_provider")
	}
}

// containsAny reports whether s contains any of the needles.
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && len(s) >= len(n) && stringsContains(s, n) {
			return true
		}
	}
	return false
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
