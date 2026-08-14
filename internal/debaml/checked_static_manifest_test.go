package debaml

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Slice 7.2c-2 — the NO-ADMISSION-FLIP and RECOMPUTED-BOUND proofs for the
// data-driven direct-comparison classifier.
//
// Slice 7.2c-2 refactored the static expression profile onto the exact-i64
// capability table (constraint_direct_i64.go) and gave it an explicit
// allowed-operator MANIFEST. The refactor is only safe if two things hold, and this
// file proves both:
//
//  1. the manifest is still `>` and only `>`, so nothing was widened by the
//     refactor — and the DECLINE assertions that say so are proven to bite by
//     driving a twin classifier whose manifest carries a second operator;
//  2. the assert-cause length ceiling is DERIVED from that manifest rather than
//     inherited from the `>`-shaped literal it used to be written as, so the
//     one-byte-longer `>=` / `<=` cannot silently push a rendered cause into
//     stock's 100-byte truncation regime when 7.2c-3 admits them.

// ---------------------------------------------------------------------------
// (1) The manifest is still `>` only
// ---------------------------------------------------------------------------

// TestStaticCheckedManifestIsGreaterThanOnly pins the PRODUCTION allowed-operator
// manifest, from both ends: the token list itself, and the behaviour of the
// classifier it drives.
//
// Pinning the token list alone would not be enough — a classifier that ignored its
// manifest would still pass — so the same test drives every one of the six
// canonical expressions through [staticCheckedThreshold] and requires exactly one
// to be admitted.
func TestStaticCheckedManifestIsGreaterThanOnly(t *testing.T) {
	tokens := staticCheckedManifestTokens()
	if len(tokens) != 1 || tokens[0] != ">" {
		t.Fatalf("the production manifest is %v; Slice 7.2c-2 ships NO admission flip and it must "+
			"stay exactly [>] — 7.2c-3 is the slice that may add a token, per operator, against that "+
			"operator's CFFI evidence", tokens)
	}
	// The manifest resolves against the capability, and to exactly one operator.
	manifest := staticCheckedManifest()
	if len(manifest) != 1 || manifest[0].Token != ">" || manifest[0].ID != "gt" {
		t.Fatalf("the resolved manifest is %v, want the single `>` operator", manifest)
	}
	// A caller cannot widen a running binary by holding on to the slice.
	tokens[0] = ">="
	if again := staticCheckedManifestTokens(); again[0] != ">" {
		t.Fatal("mutating the returned manifest slice changed the production manifest; it must be a " +
			"fresh slice every call so no code path can widen the claim at runtime")
	}

	// BEHAVIOUR. Exactly one of the six canonical expressions is classified.
	admitted, declined := 0, 0
	for _, op := range directCompareOperators() {
		expr := directI64Expression(op, 0)
		got, ok := staticCheckedThreshold(expr)
		switch op.Token {
		case ">":
			if !ok {
				t.Errorf("the classifier REJECTED the admitted predicate %q", expr)
				continue
			}
			if got != 0 {
				t.Errorf("%q classified to threshold %d, want 0", expr, got)
			}
			admitted++
		default:
			if ok {
				t.Errorf("the classifier ADMITTED %q (threshold %d); that is an admission flip and "+
					"this slice ships none", expr, got)
				continue
			}
			declined++
		}
	}
	if admitted != 1 || declined != 5 {
		t.Fatalf("the classifier admitted %d and declined %d of the six direct operators, want 1 and 5",
			admitted, declined)
	}

	// And the whole-bundle fingerprint agrees, on BOTH levels, so the decline is not
	// only a property of the expression helper.
	for _, expr := range staticCheckedDeclinedDirectOperators() {
		for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
			b := staticCheckedBundle(level, "positive", expr)
			if _, ok := staticCheckedProfileOf(b); ok {
				t.Errorf("staticCheckedProfileOf ADMITTED %q at level %v", expr, level)
			}
			if IsAdmittedStaticCheckedFamily(b) {
				t.Errorf("the nativeserve return-shape delegate ADMITTED %q at level %v", expr, level)
			}
		}
	}
}

// staticCheckedManifestTwinAdmits is the MUTANT: the production classifier with a
// widened manifest, and with nothing else changed.
//
// It is built through the SAME seam production uses — [directCompareManifest] over
// the capability table, then [parseDirectI64Comparison] — rather than as a
// hand-written operator switch. That is what makes it a proof about THIS slice's
// refactor: the only difference from production is the token list, so a
// disagreement it produces is attributable to the manifest and to nothing else.
//
// The rest of the fingerprint is delegated to the real classifier by rewriting the
// expression to the one predicate production admits, exactly as
// [staticCheckedWidenedOperatorTwinAdmits] does for the 7.2c-1 shape mutant.
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
		// semantic difference between this twin and the real classifier is the
		// operator manifest — which is what makes a disagreement below attributable
		// to the manifest and to nothing else.
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
// mutation proof depends on.
//
// The twin's whole value is that its ONLY semantic difference from the production
// classifier is the operator manifest. If it carried its own padding scan, a drift
// between the two would surface as a manifest disagreement that was really a padding
// disagreement — so the twin calls [staticCheckedStripExprPadding], and this test
// drives the shared helper against the production entry point over the padding
// boundary to prove the two really do agree.
func TestStaticCheckedManifestTwinSharesTheProductionPaddingScan(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want string
		ok   bool
	}{
		{src: "this > 0", want: "this > 0", ok: true},
		{src: " this > 0 ", want: "this > 0", ok: true},
		{src: " this > 0", want: "this > 0", ok: true},
		{src: "this > 0 ", want: "this > 0", ok: true},
		// Two spaces on either side is outside the admitted padding.
		{src: "  this > 0 ", ok: false},
		{src: " this > 0  ", ok: false},
		{src: "  this > 0  ", ok: false},
		// Whitespace that is not an ASCII space is not padding at all.
		{src: "\tthis > 0", want: "\tthis > 0", ok: true},
		{src: "\nthis > 0\n", want: "\nthis > 0\n", ok: true},
		// All padding, and empty.
		{src: " ", ok: false},
		{src: "  ", ok: false},
		{src: "", ok: false},
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
		// source whose inner text is the admitted predicate, so must
		// staticCheckedCanonicalExpression — and it must return the SAME string.
		canonical, cok := staticCheckedCanonicalExpression(tc.src)
		wantCanonical := ok && got == "this > 0"
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

	// NON-VACUITY: the twin really does route through the shared helper, so a source
	// the helper refuses is refused by the twin too.
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

// TestStaticCheckedManifestMutationBites is the assertion the 7.2c-2 brief requires:
// flipping the classifier's PRODUCTION MANIFEST to include a second operator must
// FAIL a decline test.
//
// It drives the shared five-gate agreement corpus — the same corpus and the same
// comparison [TestStaticCheckedGatesShareOneFingerprint] uses — against a twin whose
// manifest is `>` plus one more token, one token at a time, and requires that
// operator's rows to be reported every time. A silent widening of the manifest is
// therefore a red test rather than a green one.
//
// It differs from 7.2c-1's TestStaticCheckedOperatorWideningIsProvenToBite in what
// it mutates: that one replaces the whole expression clause with a hand-written
// six-operator matcher, this one changes the DATA the production classifier is
// driven from. Both must bite, because after this slice either could be the thing
// that slips.
func TestStaticCheckedManifestMutationBites(t *testing.T) {
	corpus := staticCheckedAgreementCorpus()
	gates := staticCheckedSchemaGates()

	// CONTROL FIRST. The twin built from the PRODUCTION manifest must agree with the
	// production gates exactly — otherwise every disagreement below could come from
	// the twin's own construction rather than from the widened token.
	control := staticCheckedManifestTwinAdmits(staticCheckedManifestTokens())
	if got := staticCheckedGateDisagreements(control, gates, corpus); len(got) != 0 {
		t.Fatalf("the twin built from the PRODUCTION manifest already disagrees with the gates, so a "+
			"widened one would prove nothing:\n  %s", strings.Join(got, "\n  "))
	}

	for _, extra := range []string{">=", "<", "<=", "==", "!="} {
		t.Run(extra, func(t *testing.T) {
			widened := staticCheckedManifestTwinAdmits([]string{">", extra})
			// The widened twin must still admit the CURRENT predicate, or it is a
			// different classifier rather than a broader one.
			if !widened(staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")) {
				t.Fatal("the widened twin does not admit `this > 0`, so it is not a broadening")
			}
			got := staticCheckedGateDisagreements(
				func(b *schema.Bundle) bool { return staticCheckedFingerprintAdmits(b) || widened(b) },
				gates, corpus)
			if len(got) == 0 {
				t.Fatalf("adding %q to the classifier's manifest produced NO disagreement; the decline "+
					"assertions cannot detect an admission flip through the manifest", extra)
			}
			// The rows named must be THIS operator's, on both levels.
			expr := directI64Expression(mustOpByToken(t, extra), 0)
			for _, suffix := range []string{"", " (assert)"} {
				row := fmt.Sprintf("%q", "direct operator "+strconv.Quote(expr)+suffix)
				found := false
				for _, line := range got {
					if strings.Contains(line, row) {
						found = true
					}
				}
				if !found {
					t.Errorf("widening the manifest with %q produced no disagreement naming %s; that "+
						"row is not actually driven through the gates", extra, row)
				}
			}
		})
	}

	// AND THE OTHER DIRECTION: a manifest the capability cannot resolve must admit
	// NOTHING rather than fall back to a partial one.
	unresolvable := staticCheckedManifestTwinAdmits([]string{">", "<=>"})
	if unresolvable(staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")) {
		t.Error("an unresolvable manifest still admitted the current predicate; it must fail closed")
	}
}

// TestStaticCheckedNoAdmissionFlipAtTheRoute re-states the no-flip claim where it
// is actually served: the one admitted route.
//
// The five declined operators must decline on ParseStaticBundleUnaryCall — the ONLY
// route that carries the claim capability — and the admitted one must still be
// served there. A refactor that widened the classifier would show up here as a
// carrier where BAML should have taken over.
func TestStaticCheckedNoAdmissionFlipAtTheRoute(t *testing.T) {
	served := 0
	for _, op := range directCompareOperators() {
		expr := directI64Expression(op, 0)
		for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
			b := staticCheckedBundle(level, "positive", expr)
			res, err := ParseStaticBundleUnaryCall(t.Context(), b, `{"answer": "sunny", "confidence": 9}`)
			if op.Token == ">" {
				if err != nil {
					t.Errorf("the ADMITTED predicate %q at level %v was refused on the claiming route: %v",
						expr, level, err)
					continue
				}
				if len(res.JSON) == 0 {
					t.Errorf("%q at level %v served no bytes", expr, level)
				}
				served++
				continue
			}
			if err == nil {
				t.Errorf("the DECLINED predicate %q at level %v was SERVED on the claiming route (%s); "+
					"that is an admission flip", expr, level, res.JSON)
			}
		}
	}
	if served != 2 {
		t.Fatalf("%d of the 2 `this > I` rows were served on the claiming route; the control is stale", served)
	}
}

// ---------------------------------------------------------------------------
// (2) The recomputed assert-cause bound
// ---------------------------------------------------------------------------

// TestStaticCheckedCauseBoundIsDerivedFromTheManifest is the proof for the 7.2c
// scope's risk 4 — "Error bounds change with `>=`/`<=`. A one-byte-longer expression
// can cross stock's 100-byte assert-cause boundary; labels must be re-bounded, not
// inherited."
//
// It proves four things, in order:
//
//  1. the present bound, under the `>`-only manifest, is what it always was (64) —
//     so this slice changed no admitted label;
//  2. the longest cause the present manifest can render is EXACTLY 100 bytes, the
//     last length stock does not truncate;
//  3. under the six-operator manifest the derived bound is 63, one byte TIGHTER —
//     i.e. the derivation really follows the manifest;
//  4. the INHERITED bound would have overrun: a 64-byte label with a `>=`
//     expression renders a 101-byte cause, which is inside stock's truncation
//     regime and has no byte proof here.
//
// Point 4 is the one that matters. Without the derivation, 7.2c-3 would inherit a
// limit that is correct for `>` and one byte too generous for `>=`/`<=`, and the
// first over-long cause would be emitted UNTRUNCATED where stock truncates and
// appends `...`.
func TestStaticCheckedCauseBoundIsDerivedFromTheManifest(t *testing.T) {
	// (1) The present value, and that it is derived from the present manifest.
	longest := directI64LongestExpression(staticCheckedManifest())
	if longest != "this > "+strconv.FormatInt(math.MinInt64, 10) {
		t.Fatalf("the longest expression the production manifest can produce is %q; the bound below "+
			"would then be derived from the wrong string", longest)
	}
	wantBound := staticCheckedMaxCauseLen - len(staticCheckedCausePrefix) - 1 - len(longest)
	if staticCheckedMaxLabelLen != wantBound {
		t.Fatalf("staticCheckedMaxLabelLen = %d, want %d derived from %q",
			staticCheckedMaxLabelLen, wantBound, longest)
	}
	if staticCheckedMaxLabelLen != 64 {
		t.Fatalf("the derived bound is %d; under the `>`-only manifest it must still be 64, because "+
			"this slice flips no admission and must change no admitted label", staticCheckedMaxLabelLen)
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

	// (3) The SIX-OPERATOR manifest derives a TIGHTER bound. This is the whole point
	// of deriving rather than restating.
	sixOps, ok := directCompareManifest([]string{">", ">=", "<", "<=", "==", "!="})
	if !ok {
		t.Fatal("the six-operator manifest does not resolve against the capability")
	}
	sixLongest := directI64LongestExpression(sixOps)
	sixBound := staticCheckedMaxCauseLen - len(staticCheckedCausePrefix) - 1 - len(sixLongest)
	if sixBound != staticCheckedMaxLabelLen-1 {
		t.Fatalf("the six-operator manifest derives a %d-byte label bound and the `>`-only one %d; "+
			"the two-character operators must cost exactly one byte", sixBound, staticCheckedMaxLabelLen)
	}
	if sixBound != 63 {
		t.Fatalf("the six-operator label bound is %d, want 63", sixBound)
	}
	if len(staticCheckedCausePrefix+strings.Repeat("z", sixBound)+" "+sixLongest) != staticCheckedMaxCauseLen {
		t.Fatal("the six-operator bound does not land on the truncation boundary either")
	}

	// (4) THE HAZARD, MEASURED. Inheriting the `>`-derived bound under a `>=`
	// expression overruns stock's boundary by exactly one byte, and the renderer's
	// backstop refuses it rather than emitting an untruncated string.
	inherited := staticCheckedCausePrefix + maxLabel + " " + sixLongest
	if len(inherited) != staticCheckedMaxCauseLen+1 {
		t.Fatalf("the inherited-bound cause is %d bytes, want %d — one PAST the boundary, which is "+
			"the hazard this derivation exists to close", len(inherited), staticCheckedMaxCauseLen+1)
	}
	if _, err := staticCheckedAssertFailure(staticCheckedConfidenceField, maxLabel, sixLongest); err == nil {
		t.Fatal("the renderer EMITTED a 101-byte cause; stock truncates above 100 and that boundary is " +
			"not byte-proven here, so it must decline")
	}

	// NON-VACUITY: the two-character operators really are one byte longer, measured
	// rather than assumed.
	for _, tok := range []string{">=", "<="} {
		op := mustOpByToken(t, tok)
		if got, want := len(directI64Expression(op, math.MinInt64)), len(longest)+1; got != want {
			t.Errorf("%q renders a %d-byte longest expression, want %d", tok, got, want)
		}
	}
}

// TestStaticCheckedCauseBoundDerivationIsProvenToBite drives the derivation itself
// against a mutant that INHERITS the old literal, and requires the difference to be
// visible.
//
// Without it, "the bound is derived" would be a claim about code shape rather than
// about behaviour: a derivation that happened to return the same number for every
// manifest would satisfy every assertion above except this one.
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
}
