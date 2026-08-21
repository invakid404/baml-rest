package admission

// De-BAML serving cutover S1 — the CONFIG-LOAD inventory path's proofs.
//
// The load-bearing property is not "the spec decodes". It is that a deployment can
// declare anything it likes and STILL cannot enroll it: the spec feeds the inventory,
// the enrollment manifest is compile-time, and the two are bound by a gate that
// refuses whatever the policy does not name. That is what makes a configuration-driven
// inventory safe to ship in a slice whose contract is that it flips nothing.

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestLoadDeclaredInventoryDecodesAValidSpec pins the grammar on a realistic
// deployment declaration: two slots, several surfaces, whitespace and both record
// separators.
func TestLoadDeclaredInventoryDecodesAValidSpec(t *testing.T) {
	inv, err := LoadDeclaredInventory(
		"cfg001:openai_unary_a:dynamic_call:openai:DEBAML-1234,\n" +
			"  cfg002:openai_stream_b:dynamic_call|dynamic_stream:openai:ACME-7  \n")
	if err != nil {
		t.Fatalf("LoadDeclaredInventory: %v", err)
	}
	if inv.Len() != 2 {
		t.Fatalf("declared %d records, want 2", inv.Len())
	}
	first, ok := inv.Lookup("cfg001")
	if !ok {
		t.Fatal("cfg001 was not declared")
	}
	if first.Cohort != "openai_unary_a" || first.Provider != ConfigProviderOpenAI || first.Approval != "DEBAML-1234" {
		t.Errorf("cfg001 = %+v, want the declared fields", first)
	}
	if len(first.Surfaces) != 1 || first.Surfaces[0] != SurfaceDynamicCall {
		t.Errorf("cfg001 surfaces = %v, want [dynamic_call]", first.Surfaces)
	}
	second, _ := inv.Lookup("cfg002")
	if len(second.Surfaces) != 2 || second.Surfaces[1] != SurfaceDynamicStream {
		t.Errorf("cfg002 surfaces = %v, want [dynamic_call dynamic_stream]", second.Surfaces)
	}
}

// TestLoadDeclaredInventoryRejectsMalformedSpecs proves the decoder is strict in the
// proof-integrity sense: it refuses rather than repairing or partially applying, so a
// fat-fingered declaration cannot silently publish a different inventory than the one
// the operator wrote.
func TestLoadDeclaredInventoryRejectsMalformedSpecs(t *testing.T) {
	for _, bad := range []string{
		"cfg001:openai_unary_a:dynamic_call:openai",                                   // too few fields
		"cfg001:openai_unary_a:dynamic_call:openai:DEBAML-1:extra",                    // too many
		"cfg017:openai_unary_a:dynamic_call:openai:DEBAML-1",                          // ID outside the vocabulary
		"gpt-4o-acme:openai_unary_a:dynamic_call:openai:DEBAML-1",                     // ID that is a model name
		"cfg001:none:dynamic_call:openai:DEBAML-1",                                    // reserved cohort
		"cfg001:sk_live_abc:dynamic_call:openai:DEBAML-1",                             // credential-shaped cohort
		"cfg001:openai_unary_a:parse_everything:openai:DEBAML-1",                      // unknown surface
		"cfg001:openai_unary_a::openai:DEBAML-1",                                      // empty surface list
		"cfg001:openai_unary_a:dynamic_call|:openai:DEBAML-1",                         // empty surface element
		"cfg001:openai_unary_a:dynamic_call:acme-internal:DEBAML-1",                   // unknown provider class
		"cfg001:openai_unary_a:dynamic_call:openai:see the wiki",                      // free-text approval
		"cfg001:openai_unary_a:dynamic_call:openai:https://t.example/x",               // URL approval
		"cfg001:a:dynamic_call:openai:DEBAML-1,cfg001:b:static_call:openai:DEBAML-2",  // duplicate ID
		"cfg001:a:dynamic_call:openai:DEBAML-1,cfg002:a:dynamic_call:openai:DEBAML-2", // (cohort, surface) collision
	} {
		if _, err := LoadDeclaredInventory(bad); err == nil {
			t.Errorf("LoadDeclaredInventory(%q) accepted a malformed spec", bad)
		}
	}
	// An empty or whitespace-only spec is not an error — it declares nothing.
	for _, empty := range []string{"", "   ", "\n\n", ",,"} {
		inv, err := LoadDeclaredInventory(empty)
		if err != nil {
			t.Errorf("LoadDeclaredInventory(%q) errored: %v", empty, err)
		}
		if inv.Len() != 0 {
			t.Errorf("LoadDeclaredInventory(%q) declared %d records, want 0", empty, inv.Len())
		}
	}
}

// TestConfigLoadCanDeclareButNeverEnroll is the safety property, and the reason a
// configuration-driven inventory is shippable in S1 at all.
//
// It declares EVERY slot in the vocabulary, on EVERY surface, through the config-load
// path, binds them to the production enrollment manifest exactly as the shipped gate
// does — and then requires every one of those identities to decline on every surface.
// A deployment cannot enroll itself by editing configuration; enrollment is a
// compile-time manifest and a reviewed code change.
func TestConfigLoadCanDeclareButNeverEnroll(t *testing.T) {
	var spec []string
	surfaces := make([]string, 0, len(AllSurfaces()))
	for _, s := range AllSurfaces() {
		surfaces = append(surfaces, s.Label())
	}
	all := strings.Join(surfaces, "|")
	declaredByConfig := 0
	for i, fp := range declaredConfigFingerprints() {
		// The one ASSIGNED slot is skipped: the compile-time manifest already declares
		// it, and a spec that re-declared it would be refused as a duplicate
		// fingerprint rather than exercising this path. That refusal is a property in
		// its own right, proved by
		// TestConfigurationCannotRedeclareOrShadowTheEnrolledSlot below.
		if fp == FeV1ConfigFingerprint {
			continue
		}
		spec = append(spec, string(fp)+":declared_"+string(rune('a'+i))+":"+all+":openai:DEBAML-1")
		declaredByConfig++
	}

	loaded, err := LoadDeclaredInventory(strings.Join(spec, ","))
	if err != nil {
		t.Fatalf("LoadDeclaredInventory: %v", err)
	}
	if loaded.Len() != declaredByConfig {
		t.Fatalf("declared %d records, want %d", loaded.Len(), declaredByConfig)
	}

	// Bound to the SHIPPED manifests, exactly as the production gate is: the
	// compile-time inventory (which substantiates the one enrollment) PLUS everything
	// configuration declared. This mirrors loadProductionInventory, so what is proved
	// below is proved about the real composition rather than about a config-only gate.
	gate, err := buildCohortGate(ProductionCohortPolicyVersion,
		append(append([]ConfigRecord(nil), productionInventoryRecords()...), loaded.Records()...),
		productionEnrollments())
	if err != nil {
		t.Fatalf("gate: %v", err)
	}

	for _, r := range loaded.Records() {
		in := CohortInput{Fingerprint: r.Fingerprint, Provider: r.Provider, gate: gate}
		// The declaration WORKS — the identity resolves to its declared cohort on a
		// surface the record declares, so the decline stays attributable and the
		// offline join is real.
		if got := ResolveCohort(r.Surfaces[0], in); got != r.Cohort {
			t.Errorf("%s resolved to %q, want its declared cohort %q", r.Fingerprint, got, r.Cohort)
		}
		// And it is refused everywhere, because declaring is not enrolling.
		for _, s := range AllSurfaces() {
			if _, d := admitCohort(s, in); d == nil {
				t.Fatalf("%s on %s was ADMITTED by a config-declared record: configuration must never enroll", r.Fingerprint, s.Label())
			}
		}
	}

	// The control that makes the sweep above mean something now that the policy is
	// NOT empty: the compile-time fe-v1 tuple IS admitted through this very gate. A
	// gate that refused everything would satisfy the loop without proving that
	// CONFIGURATION is the thing that cannot enroll.
	feV1 := CohortInput{Fingerprint: FeV1ConfigFingerprint, Provider: ConfigProviderOpenAI, gate: gate}
	if _, d := admitCohort(SurfaceDynamicCall, feV1); d != nil {
		t.Fatalf("the compile-time fe-v1 tuple was declined through the config-load gate (%v); the sweep above proves nothing if nothing can be admitted", d)
	}
}

// TestConfigurationCannotRedeclareOrShadowTheEnrolledSlot is the other half of "a
// deployment cannot enroll itself": now that ONE slot is enrolled by the compile-time
// manifest, configuration must not be able to REDESCRIBE it either — pointing the
// enrolled slot at a different cohort, a different provider class or extra surfaces
// through the environment would be an enrollment change made from configuration.
//
// The composition fails LOUDLY rather than resolving a winner, and fail-loud is the
// correct behaviour rather than merely the convenient one: a silent "compiled record
// wins" would leave an operator reading a declaration that does nothing, and a silent
// "declared record wins" would be the second rollout switch the cutover forbids.
func TestConfigurationCannotRedeclareOrShadowTheEnrolledSlot(t *testing.T) {
	for _, spec := range []struct {
		name string
		row  string
	}{
		{"same slot, different cohort", string(FeV1ConfigFingerprint) + ":other_cohort:dynamic_call:openai:DEBAML-1"},
		{"same slot, same cohort", string(FeV1ConfigFingerprint) + ":" + string(FeV1Cohort) + ":dynamic_call:openai:DEBAML-1"},
		{"same slot, more surfaces", string(FeV1ConfigFingerprint) + ":" + string(FeV1Cohort) + ":dynamic_call|dynamic_stream:openai:DEBAML-1"},
		{"different slot, the ENROLLED cohort", "cfg001:" + string(FeV1Cohort) + ":dynamic_call:openai:DEBAML-1"},
	} {
		t.Run(spec.name, func(t *testing.T) {
			loaded, err := parseInventorySpec(spec.row)
			if err != nil {
				t.Fatalf("parseInventorySpec: %v", err)
			}
			if _, err := buildCohortGate(ProductionCohortPolicyVersion,
				append(append([]ConfigRecord(nil), productionInventoryRecords()...), loaded...),
				productionEnrollments()); err == nil {
				t.Fatal("a declaration that redescribes the ENROLLED slot was accepted; configuration must not be able to change an enrollment")
			}
		})
	}
}

// TestDeclaredInventoryIsPublishedAndStaysBounded proves the operator-visible half of
// the path: declared records become config_inventory_info rows, one per record ×
// surface, carrying only bounded buckets.
func TestDeclaredInventoryIsPublishedAndStaysBounded(t *testing.T) {
	inv, err := LoadDeclaredInventory("cfg001:openai_unary_a:dynamic_call|static_call:openai:DEBAML-1234")
	if err != nil {
		t.Fatalf("LoadDeclaredInventory: %v", err)
	}
	pol, err := newCohortPolicy(ProductionCohortPolicyVersion)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	gate, err := newCohortGate(pol, inv)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	m.publishCohortGate(gate)

	var rows []*dto.Metric
	for _, mf := range gatherDeBAML(t, reg) {
		if mf.GetName() == "baml_rest_debaml_config_inventory_info" {
			rows = mf.GetMetric()
		}
	}
	if len(rows) != 2 {
		t.Fatalf("published %d inventory rows, want 2 (one record × two surfaces)", len(rows))
	}
	for _, row := range rows {
		labels := map[string]string{}
		for _, lp := range row.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels["fingerprint"] != "cfg001" || labels["cohort"] != "openai_unary_a" ||
			labels["provider"] != "openai" || labels["approval"] != "DEBAML-1234" {
			t.Errorf("inventory row = %v, want the declared buckets", labels)
		}
		if row.GetGauge().GetValue() != 1 {
			t.Errorf("inventory row value = %v, want 1", row.GetGauge().GetValue())
		}
	}
	if findings := checkBoundedLabels(gatherDeBAML(t, reg), allowedLabelValues(gate)); len(findings) > 0 {
		t.Fatalf("a config-loaded record published an unbounded label: %v", findings)
	}
	// The policy still enrolls nothing, so the published class is declared-only.
	if got := gate.Policy().Len(); got != 0 {
		t.Fatalf("the policy enrolls %d, want 0", got)
	}
}

// TestProductionGateReflectsTheConfigLoadPath pins that the shipped gate really is
// the product of the config-load path rather than a hardcoded value, and that the
// ambient (undeclared) deployment therefore publishes exactly the compile-time fe-v1
// record and nothing else.
func TestProductionGateReflectsTheConfigLoadPath(t *testing.T) {
	if err := ProductionCohortGateError(); err != nil {
		t.Fatalf("the ambient config load failed: %v", err)
	}
	loaded, err := loadProductionInventory()
	if err != nil {
		t.Fatalf("loadProductionInventory: %v", err)
	}
	if got, want := ProductionCohortGate().Inventory().Len(), loaded.Len(); got != want {
		t.Fatalf("the shipped gate declares %d records, the config-load path yields %d; the gate is not built from it", got, want)
	}
	// This build sets no ConfigInventoryEnv, so the ambient inventory is exactly the
	// compile-time manifest: the one fe-v1 record, and one enrollment.
	if got := ProductionCohortGate().Inventory().Len(); got != len(productionInventoryRecords()) {
		t.Errorf("the ambient deployment declares %d records, want %d (the compile-time manifest alone)", got, len(productionInventoryRecords()))
	}
	if got := ProductionCohortGate().Policy().Len(); got != len(productionEnrollments()) {
		t.Errorf("the shipped policy enrolls %d, want %d", got, len(productionEnrollments()))
	}
	// A request presenting NO identity — which is what every request on a deployment
	// that sealed nothing presents — is still refused on every surface. Enrollment
	// changed WHICH identity may claim, never whether an identity is needed.
	for _, s := range AllSurfaces() {
		if _, d := admitCohort(s, CohortInput{}); d == nil {
			t.Errorf("%s: the shipped gate admitted a request carrying no configuration identity", s.Label())
		}
	}
	_ = context.Background()
}
