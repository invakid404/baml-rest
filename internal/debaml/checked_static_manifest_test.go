package debaml

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Slice 7.2c-3 — the MANIFEST proofs for the admission-widening cutover.
//
// Slice 7.2c-2 refactored the static expression profile onto the exact-i64 capability
// table (constraint_direct_i64.go) and gave it an explicit allowed-operator MANIFEST,
// which it deliberately left at `>` alone. 7.2c-3 widens that manifest to all six
// direct comparisons — the only semantic edit of the cutover — and this file is where
// the widening is pinned:
//
//  1. the manifest is EXACTLY the six direct comparisons, and every token in it is
//     EVIDENCE-GATED: it carries a full stock CFFI capture (wire bytes for check
//     true/false and assert true, plus the exact err.Error() for a false assert) on
//     both name-pinned families. A token whose capture set is missing or mispaired is
//     a RED test, not an admitted operator;
//  2. the manifest is proven from BOTH sides — widening it admits a form nothing has
//     captured (over-claim) and narrowing it leaves served rows unserved (under-claim),
//     and each direction has a mutant that must be reported;
//  3. the assert-cause length ceiling FOLLOWED the widening: it is DERIVED from the
//     manifest, so admitting the one-byte-longer `>=` / `<=` moved it from 64 to 63
//     with no edit to the bound, and the inherited 64 would have overrun stock's
//     100-byte truncation boundary by exactly one byte.

// ---------------------------------------------------------------------------
// (1) The manifest is the six direct comparisons, and every token is evidenced
// ---------------------------------------------------------------------------

// TestStaticCheckedManifestIsTheSixOperators pins the PRODUCTION allowed-operator
// manifest from both ends: the token list itself, and the behaviour of the classifier
// it drives.
//
// Pinning the token list alone would not be enough — a classifier that ignored its
// manifest would still pass — so the same test drives every one of the six canonical
// expressions through [staticCheckedThreshold] and requires all six to be admitted,
// then drives the whole-bundle fingerprint and the exported nativeserve delegate over
// both levels.
func TestStaticCheckedManifestIsTheSixOperators(t *testing.T) {
	tokens := staticCheckedManifestTokens()
	want := []string{">", ">=", "<", "<=", "==", "!="}
	if len(tokens) != len(want) {
		t.Fatalf("the production manifest is %v; Slice 7.2c-3 admits the six direct comparisons %v",
			tokens, want)
	}
	// Written out independently of the capability table's order, so a reordering there
	// cannot silently change what this test is asserting.
	gotSorted, wantSorted := append([]string(nil), tokens...), append([]string(nil), want...)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)
	for i := range wantSorted {
		if gotSorted[i] != wantSorted[i] {
			t.Fatalf("the production manifest is %v, want the six direct comparisons %v", tokens, want)
		}
	}
	// The manifest resolves against the capability, to six DISTINCT operators.
	manifest := staticCheckedManifest()
	if len(manifest) != 6 {
		t.Fatalf("the resolved manifest carries %d operators, want 6", len(manifest))
	}
	seen := map[string]bool{}
	for _, op := range manifest {
		if seen[op.ID] {
			t.Fatalf("operator %q resolves twice", op.ID)
		}
		seen[op.ID] = true
	}
	// A caller cannot widen — or narrow — a running binary by holding on to the slice.
	tokens[0] = "<>"
	if again := staticCheckedManifestTokens(); again[0] != ">" {
		t.Fatal("mutating the returned manifest slice changed the production manifest; it must be a " +
			"fresh slice every call so no code path can move the claim at runtime")
	}

	// BEHAVIOUR. All six canonical expressions are classified, and each to its own
	// literal, so a classifier that returned a constant would fail here.
	admitted := 0
	for _, op := range directCompareOperators() {
		for _, literal := range []int64{0, -1, math.MinInt64, math.MaxInt64} {
			expr := directI64Expression(op, literal)
			got, ok := staticCheckedThreshold(expr)
			if !ok {
				t.Errorf("the classifier REJECTED the admitted predicate %q", expr)
				continue
			}
			if got != literal {
				t.Errorf("%q classified to threshold %d, want %d", expr, got, literal)
			}
			admitted++
		}
	}
	if admitted != 24 {
		t.Fatalf("the classifier admitted %d of the 6 operators x 4 literals, want 24", admitted)
	}

	// And the whole-bundle fingerprint agrees, on BOTH levels, so admission is not only
	// a property of the expression helper.
	rows := 0
	for _, op := range directCompareOperators() {
		expr := directI64Expression(op, 0)
		for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
			b := staticCheckedBundle(level, "positive", expr)
			if _, ok := staticCheckedProfileOf(b); !ok {
				t.Errorf("staticCheckedProfileOf REJECTED %q at level %v", expr, level)
			}
			if !IsAdmittedStaticCheckedFamily(b) {
				t.Errorf("the nativeserve return-shape delegate REJECTED %q at level %v", expr, level)
			}
			rows++
		}
	}
	if rows != 12 {
		t.Fatalf("%d fingerprint rows were driven, want 12", rows)
	}

	// The SEVENTH forms stay out, at the same two gates. Without this the assertions
	// above would be satisfied by a grammar that accepts any comparison-shaped text.
	for _, expr := range staticCheckedDeclinedSeventhForms() {
		if _, ok := staticCheckedThreshold(expr); ok {
			t.Errorf("the classifier ADMITTED the seventh form %q", expr)
		}
		for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
			b := staticCheckedBundle(level, "positive", expr)
			if _, ok := staticCheckedProfileOf(b); ok {
				t.Errorf("staticCheckedProfileOf ADMITTED the seventh form %q at level %v", expr, level)
			}
			if IsAdmittedStaticCheckedFamily(b) {
				t.Errorf("the nativeserve delegate ADMITTED the seventh form %q at level %v", expr, level)
			}
		}
	}
	t.Logf("production manifest: %v — %d canonical expressions admitted, %d seventh forms declined",
		staticCheckedManifestTokens(), admitted, len(staticCheckedDeclinedSeventhForms()))
}

// TestStaticCheckedManifestIsEvidenceGated is the per-operator evidence gate the 7.2c
// scope requires: an operator may be in the manifest ONLY if its exact stock CFFI rows
// are present and green.
//
// The join is mechanical rather than asserted. [directCompareOp.ID] is the key
// internal/debaml/predicatewire files its per-operator captures under, so this walks the
// resolved production manifest, looks each token's capture up by that key, and requires
// four non-empty byte strings that really quote THAT operator's canonical text. A token
// with no capture — or with one that quotes a different operator — fails here.
//
// It runs against the untagged copies in [stockOperatorCaptures], which
// [TestStaticCheckedOperatorCapturesAgreeWithPredicatewire] proves byte-identical to the
// tagged originals; the integration lane repeats the join against those originals. That
// two-step is deliberate: this proof has to run in the ordinary CGO-free lane (it gates
// production admission), and a copy that could drift from its authority would gate
// nothing.
func TestStaticCheckedManifestIsEvidenceGated(t *testing.T) {
	manifest := staticCheckedManifest()
	if len(manifest) == 0 {
		t.Fatal("the production manifest is empty; the evidence join would be vacuous")
	}
	for _, op := range manifest {
		t.Run(op.ID, func(t *testing.T) {
			c, ok := stockOperatorCaptures[op.ID]
			if !ok {
				t.Fatalf("operator %q (%s) is ADMITTED by the production manifest with NO stock capture; "+
					"the 7.2c scope requires a missing row to leave the operator DECLINED, not to be "+
					"masked by a grammar that happens to parse it", op.ID, op.Token)
			}
			// FOUR rows, all non-empty: wire bytes for check true/false, wire bytes for
			// a passing assert, and the whole err.Error() for a failing one. A partial
			// capture is not evidence for the operator, only for some of its outcomes.
			for _, f := range []struct{ name, value string }{
				{"checkTrue", c.checkTrue}, {"checkFalse", c.checkFalse},
				{"assertTrue", c.assertTrue}, {"assertFail", c.assertFail},
			} {
				if f.value == "" {
					t.Errorf("operator %q has an EMPTY %s capture; that outcome is unevidenced", op.ID, f.name)
				}
			}
			// The capture is THIS operator's: stock retained the operator's own canonical
			// text in the check expression and in the assertion cause.
			want := directI64Expression(op, stockOperatorLiteral)
			for _, f := range []struct{ name, value string }{
				{"checkTrue", c.checkTrue}, {"checkFalse", c.checkFalse}, {"assertFail", c.assertFail},
			} {
				if !strings.Contains(f.value, want) {
					t.Errorf("operator %q's %s capture does not quote %q; the manifest token and the "+
						"capture that authorises it are mispaired", op.ID, f.name, want)
				}
			}
			// The two outcomes are driven at DIFFERENT values, so "true" and "false"
			// really are two measurements rather than one recorded twice.
			if c.trueVal == c.falseVal {
				t.Errorf("operator %q drives confidence=%d for both outcomes", op.ID, c.trueVal)
			}
			if got := op.Holds(c.trueVal, stockOperatorLiteral); !got {
				t.Errorf("operator %q's capture claims %q holds at confidence=%d, but the exact "+
					"comparison says it does not", op.ID, want, c.trueVal)
			}
			if got := op.Holds(c.falseVal, stockOperatorLiteral); got {
				t.Errorf("operator %q's capture claims %q fails at confidence=%d, but the exact "+
					"comparison says it holds", op.ID, want, c.falseVal)
			}
		})
	}
	t.Logf("evidence gate: %d manifest operators, each joined to 4 stock CFFI capture rows by "+
		"directCompareOp.ID", len(manifest))
}

// TestStaticCheckedEvidenceGateIsProvenToBite drives the join above against captures
// that are missing, empty or mispaired, and requires each to be REPORTED.
//
// Without it, "every admitted operator is evidence-gated" would be a claim about a loop
// that might never fail. The mutants are stand-ins fed to the same checker the test
// above uses; the production manifest and the real captures are untouched.
func TestStaticCheckedEvidenceGateIsProvenToBite(t *testing.T) {
	manifest := staticCheckedManifest()
	// CONTROL: the real captures pass, or every rejection below could be for the wrong
	// reason.
	if got := staticCheckedEvidenceViolations(manifest, stockOperatorCaptures); len(got) != 0 {
		t.Fatalf("the PRODUCTION manifest already fails the evidence join, so the mutants prove "+
			"nothing:\n  %s", strings.Join(got, "\n  "))
	}
	for _, m := range []struct {
		name    string
		mutate  func(map[string]stockOperatorCapture)
		wantHit string
	}{{
		name:    "a capture removed entirely",
		mutate:  func(c map[string]stockOperatorCapture) { delete(c, "le") },
		wantHit: "le",
	}, {
		name: "a capture present but EMPTY",
		mutate: func(c map[string]stockOperatorCapture) {
			row := c["eq"]
			row.assertFail = ""
			c["eq"] = row
		},
		wantHit: "eq",
	}, {
		name: "a capture MISPAIRED with another operator's bytes",
		mutate: func(c map[string]stockOperatorCapture) {
			row := c["ge"]
			row.checkTrue = c["gt"].checkTrue
			c["ge"] = row
		},
		wantHit: "ge",
	}, {
		name: "a capture whose true/false values agree with each other",
		mutate: func(c map[string]stockOperatorCapture) {
			row := c["ne"]
			row.falseVal = row.trueVal
			c["ne"] = row
		},
		wantHit: "ne",
	}, {
		name: "a capture whose outcome contradicts the exact comparison",
		mutate: func(c map[string]stockOperatorCapture) {
			row := c["lt"]
			row.trueVal, row.falseVal = row.falseVal, row.trueVal
			c["lt"] = row
		},
		wantHit: "lt",
	}} {
		t.Run(m.name, func(t *testing.T) {
			mutated := map[string]stockOperatorCapture{}
			for k, v := range stockOperatorCaptures {
				mutated[k] = v
			}
			m.mutate(mutated)
			got := staticCheckedEvidenceViolations(manifest, mutated)
			if len(got) == 0 {
				t.Fatalf("the evidence join ACCEPTED %s; a manifest token could then be admitted with "+
					"no usable stock capture behind it", m.name)
			}
			named := false
			for _, line := range got {
				if strings.Contains(line, strconv.Quote(m.wantHit)) {
					named = true
				}
			}
			if !named {
				t.Errorf("the violation did not name operator %q: %v", m.wantHit, got)
			}
		})
	}
}

// staticCheckedEvidenceViolations is the evidence join itself, factored out so it can
// be driven with DELIBERATELY BROKEN captures and shown to report them.
//
// It returns one line per violation, naming the operator, so a red run says which token
// lost its authority rather than only that something is wrong.
func staticCheckedEvidenceViolations(
	manifest []directCompareOp, captures map[string]stockOperatorCapture,
) []string {
	var out []string
	for _, op := range manifest {
		c, ok := captures[op.ID]
		if !ok {
			out = append(out, fmt.Sprintf("operator %q (%s) is in the manifest with no stock capture",
				op.ID, op.Token))
			continue
		}
		want := directI64Expression(op, stockOperatorLiteral)
		for _, f := range []struct{ name, value string }{
			{"checkTrue", c.checkTrue}, {"checkFalse", c.checkFalse},
			{"assertTrue", c.assertTrue}, {"assertFail", c.assertFail},
		} {
			if f.value == "" {
				out = append(out, fmt.Sprintf("operator %q has an empty %s capture", op.ID, f.name))
			}
		}
		for _, f := range []struct{ name, value string }{
			{"checkTrue", c.checkTrue}, {"checkFalse", c.checkFalse}, {"assertFail", c.assertFail},
		} {
			if f.value != "" && !strings.Contains(f.value, want) {
				out = append(out, fmt.Sprintf("operator %q's %s capture does not quote %q",
					op.ID, f.name, want))
			}
		}
		if c.trueVal == c.falseVal {
			out = append(out, fmt.Sprintf("operator %q drives one value for both outcomes", op.ID))
			continue
		}
		if !op.Holds(c.trueVal, stockOperatorLiteral) || op.Holds(c.falseVal, stockOperatorLiteral) {
			out = append(out, fmt.Sprintf("operator %q's captured outcomes contradict the exact comparison",
				op.ID))
		}
	}
	return out
}

// staticCheckedManifestTwinAdmits is the MUTANT: the production classifier with a
// DIFFERENT manifest, and with nothing else changed.
//
// It is built through the SAME seam production uses — [directCompareManifest] over the
// capability table, then [parseDirectI64Comparison] — rather than as a hand-written
// operator switch. That is what makes it a proof about the manifest: the only difference
// from production is the token list, so a disagreement it produces is attributable to
// the manifest and to nothing else.
//
// The rest of the fingerprint is delegated to the real classifier by rewriting the
// expression to one production admits, exactly as [staticCheckedSeventhFormTwinAdmits]
// does for the shape mutant.
func staticCheckedManifestTwinAdmits(tokens []string) func(*schema.Bundle) bool {
	manifest, ok := directCompareManifest(tokens)
	if !ok {
		return func(*schema.Bundle) bool { return false }
	}
	return func(b *schema.Bundle) bool {
		if b == nil || len(b.Classes) != 1 || len(b.Classes[0].Fields) != 2 {
			return false
		}
		cs := b.Classes[0].Fields[1].Type.Meta.Constraints
		if len(cs) != 1 {
			return false
		}
		// The mutant reads the SOURCE text the same way production does: it calls
		// PRODUCTION's own padding scan ([staticCheckedStripExprPadding], shared with
		// [staticCheckedCanonicalExpression]) rather than a copy of it, so the only
		// semantic difference between this twin and the real classifier is the operator
		// manifest — which is what makes a disagreement below attributable to the
		// manifest and to nothing else.
		canonical, ok := staticCheckedStripExprPadding(cs[0].Expression)
		if !ok {
			return false
		}
		cmp, ok := parseDirectI64Comparison(canonical, manifest)
		if !ok {
			return false
		}
		rewritten := *b
		rewritten.Classes = []schema.ClassDef{b.Classes[0]}
		rewritten.Classes[0].Fields = append([]schema.ClassField(nil), b.Classes[0].Fields...)
		rewritten.Classes[0].Fields[1].Type.Meta.Constraints = []schema.Constraint{{
			Level:      cs[0].Level,
			Expression: "this > " + strconv.FormatInt(cmp.literal, 10),
			Label:      cs[0].Label,
		}}
		if err := rewritten.RebuildIndexes(); err != nil {
			return false
		}
		return staticCheckedFingerprintAdmits(&rewritten)
	}
}

// TestStaticCheckedManifestTwinSharesTheProductionPaddingScan pins the sharing the
// mutation proofs depend on.
//
// The twin's whole value is that its ONLY semantic difference from the production
// classifier is the operator manifest. If it carried its own padding scan, a drift
// between the two would surface as a manifest disagreement that was really a padding
// disagreement — so the twin calls [staticCheckedStripExprPadding], and this test drives
// the shared helper against the production entry point over the padding boundary to
// prove the two really do agree.
//
// SLICE 7.2c-3: the rows are driven on a TWO-BYTE operator as well as the one-byte one.
// The padding rule counts ASCII spaces on the OUTSIDE, so it is independent of the
// operator's width — but that is a claim about the implementation, and after the cutover
// both widths are admitted, so it is measured on both rather than argued.
func TestStaticCheckedManifestTwinSharesTheProductionPaddingScan(t *testing.T) {
	for _, inner := range []string{"this > 0", "this <= 0"} {
		for _, tc := range []struct {
			src  string
			want string
			ok   bool
		}{
			{src: inner, want: inner, ok: true},
			{src: " " + inner + " ", want: inner, ok: true},
			{src: " " + inner, want: inner, ok: true},
			{src: inner + " ", want: inner, ok: true},
			// Two spaces on either side is outside the admitted padding.
			{src: "  " + inner + " ", ok: false},
			{src: " " + inner + "  ", ok: false},
			{src: "  " + inner + "  ", ok: false},
			// Whitespace that is not an ASCII space is not padding at all.
			{src: "\t" + inner, want: "\t" + inner, ok: true},
			{src: "\n" + inner + "\n", want: "\n" + inner + "\n", ok: true},
		} {
			got, ok := staticCheckedStripExprPadding(tc.src)
			if ok != tc.ok {
				t.Errorf("staticCheckedStripExprPadding(%q) ok = %v, want %v", tc.src, ok, tc.ok)
				continue
			}
			if ok && got != tc.want {
				t.Errorf("staticCheckedStripExprPadding(%q) = %q, want %q", tc.src, got, tc.want)
			}
			// AGREEMENT with the production entry point: whenever the scan accepts a
			// source whose inner text is an admitted predicate, so must
			// staticCheckedCanonicalExpression — and it must return the SAME string.
			canonical, cok := staticCheckedCanonicalExpression(tc.src)
			wantCanonical := ok && got == inner
			if cok != wantCanonical {
				t.Errorf("staticCheckedCanonicalExpression(%q) ok = %v, want %v; the two scans have drifted",
					tc.src, cok, wantCanonical)
				continue
			}
			if cok && canonical != got {
				t.Errorf("staticCheckedCanonicalExpression(%q) = %q but the shared scan returned %q",
					tc.src, canonical, got)
			}
		}
	}
	// All padding, and empty — neither carries an inner expression at all, so they are
	// driven once rather than per operator.
	for _, src := range []string{" ", "  ", ""} {
		if _, ok := staticCheckedStripExprPadding(src); ok {
			t.Errorf("staticCheckedStripExprPadding(%q) accepted an all-padding source", src)
		}
	}

	// NON-VACUITY: the twin really does route through the shared helper, so a source the
	// helper refuses is refused by the twin too.
	twin := staticCheckedManifestTwinAdmits([]string{">", ">="})
	if twin(staticCheckedBundle(schema.ConstraintCheck, "positive", "  this >= 0  ")) {
		t.Error("the manifest twin admitted a two-space-padded source; it is not using the " +
			"production padding scan")
	}
	if !twin(staticCheckedBundle(schema.ConstraintCheck, "positive", " this >= 0 ")) {
		t.Error("the manifest twin rejected a one-space-padded source, which production admits; " +
			"the twin differs from production by more than the operator manifest")
	}
}

// TestStaticCheckedManifestMutationBites is the manifest's fail-closed proof.
//
// Slice 7.2c-2's version of this test widened the manifest one token at a time and
// required a disagreement. That direction has moved: after the cutover every token the
// CAPABILITY carries is already in the manifest, so there is no wider resolvable
// manifest left to build — which is itself the property worth pinning, because it means
// the classifier cannot be widened by editing a token list alone.
//
// What is left to prove here is that a manifest naming something the capability cannot
// decide FAILS CLOSED: it must admit NOTHING rather than fall back to the subset it
// could resolve. The over-claim direction is carried by
// [TestStaticCheckedSeventhFormWideningIsProvenToBite] (a hand-written seventh operator
// form, which is what a real over-claim would look like) and the under-claim direction
// by [TestStaticCheckedManifestNarrowingIsProvenToBite].
func TestStaticCheckedManifestMutationBites(t *testing.T) {
	corpus := staticCheckedAgreementCorpus()
	gates := staticCheckedSchemaGates()

	// CONTROL FIRST. The twin built from the PRODUCTION manifest must agree with the
	// production gates exactly — otherwise every result below could come from the twin's
	// own construction rather than from the manifest it was handed.
	control := staticCheckedManifestTwinAdmits(staticCheckedManifestTokens())
	if got := staticCheckedGateDisagreements(control, gates, corpus); len(got) != 0 {
		t.Fatalf("the twin built from the PRODUCTION manifest already disagrees with the gates:\n  %s",
			strings.Join(got, "\n  "))
	}

	// The manifest is the WHOLE capability, so no wider one resolves. Adding a token the
	// capability cannot decide must yield NO manifest — not the six it could resolve.
	for _, extra := range []string{"<>", "===", "=", "!==", "=>"} {
		t.Run("unresolvable/"+extra, func(t *testing.T) {
			tokens := append(staticCheckedManifestTokens(), extra)
			if _, ok := directCompareManifest(tokens); ok {
				t.Fatalf("the capability resolved %q; this test assumes it is outside the capability, "+
					"and if that changed the manifest must be re-evidenced before it may include it", extra)
			}
			widened := staticCheckedManifestTwinAdmits(tokens)
			for _, op := range directCompareOperators() {
				expr := directI64Expression(op, 0)
				if widened(staticCheckedBundle(schema.ConstraintCheck, "positive", expr)) {
					t.Errorf("an unresolvable manifest still admitted %q; it must fail closed and admit "+
						"NOTHING rather than silently claim the subset it could resolve", expr)
				}
			}
		})
	}

	// A DUPLICATED token is also a manifest that was not written carefully, and it must
	// fail closed for the same reason.
	dup := staticCheckedManifestTwinAdmits([]string{">", ">", ">="})
	if dup(staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")) {
		t.Error("a manifest with a duplicated token still admitted; it must fail closed")
	}
}

// TestStaticCheckedSixOperatorsServeAtTheRoute re-states the cutover where it is
// actually served: the one admitted route.
//
// All six operators must be SERVED on ParseStaticBundleUnaryCall — the ONLY route that
// carries the claim capability — on both levels, and every seventh form must still be
// refused there. A classifier that widened past the manifest would show up here as a
// carrier where BAML should have taken over; one that narrowed would show up as a BAML
// fallback on a row the boundary manifest counts as served.
func TestStaticCheckedSixOperatorsServeAtTheRoute(t *testing.T) {
	served := 0
	for _, op := range directCompareOperators() {
		expr := directI64Expression(op, stockOperatorLiteral)
		// The value the predicate HOLDS at, taken from that operator's own stock
		// capture. A fixed `9` would make `this < 0` / `this <= 0` / `this == 0` false,
		// and a false @assert is a CLAIMED FAILURE — a served outcome that looks like a
		// refusal to a test that only checks `err != nil`.
		holding := stockOperatorCaptureOf(t, op).trueVal
		for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
			b := staticCheckedBundle(level, "positive", expr)
			res, err := ParseStaticBundleUnaryCall(t.Context(), b, stockOperatorRaw(holding))
			if err != nil {
				t.Errorf("the ADMITTED predicate %q at level %v was refused on the claiming route "+
					"over confidence=%d: %v", expr, level, holding, err)
				continue
			}
			if len(res.JSON) == 0 {
				t.Errorf("%q at level %v served no bytes", expr, level)
				continue
			}
			served++
		}
	}
	if served != 12 {
		t.Fatalf("%d of the 12 admitted rows were served on the claiming route", served)
	}
	declined := 0
	for _, expr := range staticCheckedDeclinedSeventhForms() {
		for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
			b := staticCheckedBundle(level, "positive", expr)
			res, err := ParseStaticBundleUnaryCall(t.Context(), b, `{"answer": "sunny", "confidence": 9}`)
			// The DECLINE SENTINEL specifically. A non-sentinel error would mean native
			// CLAIMED the row and then failed, which is a different — and worse —
			// outcome than leaving it to BAML.
			if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("the SEVENTH form %q at level %v was CLAIMED on the claiming route (%s, %v)",
					expr, level, res.JSON, err)
				continue
			}
			declined++
		}
	}
	if declined != len(staticCheckedDeclinedSeventhForms())*2 {
		t.Fatalf("%d seventh-form rows declined at the route, want %d",
			declined, len(staticCheckedDeclinedSeventhForms())*2)
	}
	t.Logf("route manifest: %d rows SERVED on the static unary /call route, %d seventh-form rows "+
		"declined there", served, declined)
}

// ---------------------------------------------------------------------------
// (2) The recomputed assert-cause bound
// ---------------------------------------------------------------------------

// TestStaticCheckedCauseBoundIsDerivedFromTheManifest is the proof for the 7.2c scope's
// risk 4 — "Error bounds change with `>=`/`<=`. A one-byte-longer expression can cross
// stock's 100-byte assert-cause boundary; labels must be re-bounded, not inherited."
//
// It proves four things, in order:
//
//  1. the bound MOVED with the cutover, to 63, and is derived from the widened
//     manifest's own longest canonical expression rather than restated;
//  2. the longest cause the widened manifest can render is EXACTLY 100 bytes — the last
//     length stock does not truncate — and it really renders;
//  3. the pre-cutover `>`-only manifest would have derived 64, so the derivation really
//     follows the manifest rather than returning a constant;
//  4. THE HAZARD, MEASURED: the INHERITED 64-byte label under a `>=` expression renders
//     a 101-byte cause — one byte INSIDE stock's truncation regime — and the renderer's
//     backstop refuses it rather than emitting an untruncated string.
//
// Point 4 is the one that matters, and it is no longer hypothetical: this is the slice
// that admitted the two-character operators, so a bound left at 64 would have been an
// active byte divergence rather than a latent one.
func TestStaticCheckedCauseBoundIsDerivedFromTheManifest(t *testing.T) {
	// (1) The present value, and that it is derived from the present manifest.
	longest := directI64LongestExpression(staticCheckedManifest())
	if longest != "this >= "+strconv.FormatInt(math.MinInt64, 10) &&
		longest != "this <= "+strconv.FormatInt(math.MinInt64, 10) {
		t.Fatalf("the longest expression the production manifest can produce is %q; under the six-operator "+
			"manifest it must be a TWO-character operator against math.MinInt64", longest)
	}
	wantBound := staticCheckedMaxCauseLen - len(staticCheckedCausePrefix) - 1 - len(longest)
	if staticCheckedMaxLabelLen != wantBound {
		t.Fatalf("staticCheckedMaxLabelLen = %d, want %d derived from %q",
			staticCheckedMaxLabelLen, wantBound, longest)
	}
	if staticCheckedMaxLabelLen != 63 {
		t.Fatalf("the derived bound is %d; under the six-operator manifest it must be 63 — one byte "+
			"TIGHTER than the 64 the `>`-only manifest derived", staticCheckedMaxLabelLen)
	}

	// (2) The longest admissible cause sits exactly ON stock's boundary.
	maxLabel := strings.Repeat("z", staticCheckedMaxLabelLen)
	worst := staticCheckedCausePrefix + maxLabel + " " + longest
	if len(worst) != staticCheckedMaxCauseLen {
		t.Fatalf("the longest admissible cause is %d bytes, want exactly %d", len(worst), staticCheckedMaxCauseLen)
	}
	// And it really renders, so the bound is not merely arithmetic.
	if _, err := staticCheckedAssertFailure(staticCheckedConfidenceField, maxLabel, longest); err != nil {
		t.Fatalf("the boundary-length cause did not render: %v", err)
	}
	// It is also ADMITTED end to end: the label passes the fingerprint's own ASCII bound,
	// so the boundary length is a served length rather than one only the renderer sees.
	if !staticCheckedASCIILabel(maxLabel) {
		t.Fatalf("a %d-byte label is refused by the fingerprint although it is the derived bound",
			len(maxLabel))
	}

	// (3) The PRE-CUTOVER manifest derives a LOOSER bound. This is what makes the
	// derivation a behaviour rather than a constant that happens to be right.
	oneOp, ok := directCompareManifest([]string{">"})
	if !ok {
		t.Fatal("the `>`-only manifest does not resolve against the capability")
	}
	oneLongest := directI64LongestExpression(oneOp)
	oneBound := staticCheckedMaxCauseLen - len(staticCheckedCausePrefix) - 1 - len(oneLongest)
	if oneBound != staticCheckedMaxLabelLen+1 {
		t.Fatalf("the `>`-only manifest derives a %d-byte label bound and the six-operator one %d; the "+
			"two-character operators must cost exactly one byte", oneBound, staticCheckedMaxLabelLen)
	}
	if oneBound != 64 {
		t.Fatalf("the `>`-only label bound is %d, want the 64 Slice 7.2b-3 shipped", oneBound)
	}

	// (4) THE HAZARD, MEASURED. The INHERITED bound under a two-character operator
	// overruns stock's boundary by exactly one byte, and the renderer's backstop refuses
	// it rather than emitting an untruncated string.
	inheritedLabel := strings.Repeat("z", oneBound)
	inherited := staticCheckedCausePrefix + inheritedLabel + " " + longest
	if len(inherited) != staticCheckedMaxCauseLen+1 {
		t.Fatalf("the inherited-bound cause is %d bytes, want %d — one PAST the boundary, which is the "+
			"hazard this derivation exists to close", len(inherited), staticCheckedMaxCauseLen+1)
	}
	if _, err := staticCheckedAssertFailure(staticCheckedConfidenceField, inheritedLabel, longest); err == nil {
		t.Fatal("the renderer EMITTED a 101-byte cause; stock truncates above 100 and that boundary is " +
			"not byte-proven here, so it must decline")
	}
	// And the FINGERPRINT refuses that label too, so the backstop is never the only
	// thing standing between the widened manifest and an untruncated cause.
	if staticCheckedASCIILabel(inheritedLabel) {
		t.Fatalf("the fingerprint still admits a %d-byte label after the cutover; the inherited bound "+
			"would reach the renderer", len(inheritedLabel))
	}

	// NON-VACUITY: the two-character operators really are one byte longer, measured
	// rather than assumed.
	for _, tok := range []string{">=", "<="} {
		op := mustOpByToken(t, tok)
		if got, want := len(directI64Expression(op, math.MinInt64)), len(oneLongest)+1; got != want {
			t.Errorf("%q renders a %d-byte longest expression, want %d", tok, got, want)
		}
	}
	t.Logf("cause bound: %d bytes, derived from %q (the six-operator manifest); the pre-cutover "+
		"`>`-only manifest derived %d, which would have rendered a %d-byte cause",
		staticCheckedMaxLabelLen, longest, oneBound, len(inherited))
}

// TestStaticCheckedCauseBoundDerivationIsProvenToBite drives the derivation itself
// against a mutant that INHERITS the old literal, and requires the difference to be
// visible.
//
// Without it, "the bound is derived" would be a claim about code shape rather than about
// behaviour: a derivation that happened to return the same number for every manifest
// would satisfy every assertion above except this one.
func TestStaticCheckedCauseBoundDerivationIsProvenToBite(t *testing.T) {
	// The MUTANT: the pre-7.2c-2 spelling, a literal that ignores the manifest.
	inherited := func([]directCompareOp) string { return "this > -9223372036854775808" }

	manifests := [][]string{
		{">"},
		{">", ">="},
		{"<="},
		{">", ">=", "<", "<=", "==", "!="},
	}
	differed := 0
	for _, tokens := range manifests {
		ops, ok := directCompareManifest(tokens)
		if !ok {
			t.Fatalf("manifest %v does not resolve", tokens)
		}
		derived, stale := directI64LongestExpression(ops), inherited(ops)
		if len(derived) != len(stale) {
			differed++
		}
	}
	if differed == 0 {
		t.Fatal("the derived bound matched the inherited literal for EVERY manifest driven, including " +
			"ones carrying a two-character operator; the derivation is not actually following the manifest")
	}
	if differed != 3 {
		t.Errorf("%d of the 4 manifests derived a different length than the inherited literal, want 3 "+
			"(every manifest carrying a two-character operator)", differed)
	}
	// And the PRODUCTION manifest is one of the three, so the mutant is not merely
	// distinguishable on hypothetical inputs.
	if len(directI64LongestExpression(staticCheckedManifest())) == len(inherited(nil)) {
		t.Fatal("the PRODUCTION manifest derives the same length as the inherited literal; after the " +
			"cutover it must be one byte longer")
	}
}
