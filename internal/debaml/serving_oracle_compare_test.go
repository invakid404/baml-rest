//go:build integration

package debaml

// The CONTRACT: how a stock envelope and a native envelope are allowed to relate.
//
// There is one rule and it is fail-closed:
//
//	native may reproduce stock exactly, or refuse to decide. It may never produce
//	a boolean stock did not produce, and it may never produce a DIFFERENT one.
//
// Everything below is that rule made discriminating. Each row lands in exactly one
// agreement bucket, the buckets are tallied and pinned, and a row that would land
// in none is a failure rather than a silently defaulted "other". Every bucket that
// is not an agreement requires the fixture to carry a one-sentence Divergence note,
// so a measured cost can neither appear nor disappear without being written down.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// soAgreement is how one row's two envelopes relate.
type soAgreement string

const (
	// soAgreeValue: stock emitted a value, native canonicalized the same document
	// with the same identity, and every check site agreed.
	soAgreeValue soAgreement = "agree-value"
	// soAgreeAssertFailure: stock REJECTED the node on a false @assert and native's
	// state records the same assertion, at the same level, evaluating false.
	soAgreeAssertFailure soAgreement = "agree-assert-failure"
	// soAgreeRefusal: neither engine produced a boolean — stock's evaluator or
	// coercion failed and native refused too. They agree in substance.
	soAgreeRefusal soAgreement = "agree-refusal"
	// soNativeDeclinesPredicate: stock DECIDED and native refused the PREDICATE with
	// ErrConstraintUnsupported. Safe, and the measured cost of the native profile.
	soNativeDeclinesPredicate soAgreement = "native-declines-predicate"
	// soNativeDeclinesCoercion: native's production coerce refused the VALUE with the
	// ErrDeBAMLParseUnsupported sentinel, so no state exists to compare. Fail-closed
	// by construction: the same sentinel is what makes the serving path fall back.
	soNativeDeclinesCoercion soAgreement = "native-declines-coercion"
	// soNativeDeclinesExtraction: native's extraction stage found no cleanly-claimable
	// candidate in the assistant text, so it never reached coercion. Stock's
	// recovery is broader. Also a decline, also fail-closed.
	soNativeDeclinesExtraction soAgreement = "native-declines-extraction"
	// soCollectorRefuses: the TEST-ONLY collector could not mirror production's
	// coercion (an unmodelled shape, or a traversal that did not serialize to
	// production's bytes) and refused to report a state. It is a limitation of the
	// witness rather than of either engine, and it is recorded as such.
	soCollectorRefuses soAgreement = "collector-refuses"
	// soStateDiverges: the two legs canonicalized the same raw text into different
	// values. Never served, because the row is constraint-bearing.
	soStateDiverges soAgreement = "state-divergence"
	// soEventShapeDiverges: the two legs agree on every boolean but not on the SHAPE
	// of the check collection — stock repeated a check, or reported none where
	// native ran one and declined it.
	soEventShapeDiverges soAgreement = "event-shape-divergence"
	// soStockUnobservable: stock aborts or hangs, so there is no envelope to agree
	// with and no boolean may be fabricated.
	soStockUnobservable soAgreement = "stock-unobservable"
)

// soIsAgreement reports whether a bucket is an AGREEMENT, i.e. one that requires
// no Divergence note.
func soIsAgreement(a soAgreement) bool {
	return a == soAgreeValue || a == soAgreeAssertFailure || a == soAgreeRefusal
}

// soMismatchKind names WHERE two envelopes parted company, so a failure report
// says what kind of disagreement it is rather than only that there was one.
type soMismatchKind string

const (
	soMismatchState    soMismatchKind = "state"
	soMismatchEvent    soMismatchKind = "event"
	soMismatchBoundary soMismatchKind = "boundary"
)

// soMismatch is one way the two envelopes parted company.
//
// Violation says whether it BREAKS the contract. A documented state or
// check-collection-shape difference does not: it defines the row's bucket, is
// pinned byte-for-byte by the recorded envelopes, and is required to carry a
// Divergence note. A boolean disagreement, an admitted constraint-bearing bundle,
// or a native answer where stock produced none always does.
type soMismatch struct {
	Kind      soMismatchKind
	Violation bool
	Detail    string
}

func (m soMismatch) String() string {
	tag := "RECORDED"
	if m.Violation {
		tag = "VIOLATION"
	}
	return tag + " " + string(m.Kind) + ": " + m.Detail
}

// soViolations filters a mismatch list down to the contract-breaking entries.
func soViolations(ms []soMismatch) []soMismatch {
	var out []soMismatch
	for _, m := range ms {
		if m.Violation {
			out = append(out, m)
		}
	}
	return out
}

// soCheckSites returns native's CHECK-level sites — the ones stock can report back
// inside a value. @assert sites are handled separately, because stock never
// carries a holding assertion in the value and reports a failing one as a
// rejection.
func soCheckSites(env soNativeEnvelope) []soNativeSite {
	var out []soNativeSite
	for _, s := range env.Sites {
		if s.Level == schema.ConstraintCheck {
			out = append(out, s)
		}
	}
	return out
}

// soCollapseRepeats folds CONSECUTIVE IDENTICAL stock sites into one.
//
// Stock repeats a check when the value reached its node more than once — a
// single-value-into-list wrap, or a list re-coerced after an element was dropped —
// and reports the same (path, label, expression, status) twice. That is a fact
// about the check COLLECTION's shape, not about the predicate: the result is
// identical, so folding cannot hide a differing boolean.
//
// It is deliberately narrow. Two checks sharing a label with DIFFERENT expressions
// (the duplicate-label asymmetry) are not consecutive-identical and survive; so
// does the same expression with a different status. TestServingOracleDuplicateLabel
// is the control that proves the fold does not swallow that case.
func soCollapseRepeats(sites []soStockSite) []soStockSite {
	var out []soStockSite
	for _, s := range sites {
		if n := len(out); n > 0 && out[n-1] == s {
			continue
		}
		out = append(out, s)
	}
	return out
}

// soDecidedSites are the native sites that produced a boolean.
func soDecidedSites(sites []soNativeSite) []soNativeSite {
	var out []soNativeSite
	for _, s := range sites {
		if s.Outcome == constraintOutcomeTrue || s.Outcome == constraintOutcomeFalse {
			out = append(out, s)
		}
	}
	return out
}

// soCompare applies the contract and returns every way the two legs disagree,
// together with the agreement bucket the row lands in.
func soCompare(f servingOracleFixture, stock soStockEnvelope, native soNativeEnvelope) (soAgreement, []soMismatch) {
	var out []soMismatch
	add := func(k soMismatchKind, format string, args ...any) {
		out = append(out, soMismatch{Kind: k, Violation: true, Detail: fmt.Sprintf(format, args...)})
	}
	record := func(k soMismatchKind, format string, args ...any) {
		out = append(out, soMismatch{Kind: k, Detail: fmt.Sprintf(format, args...)})
	}

	// ---- boundary -------------------------------------------------------
	//
	// Recorded for EVERY row, on every run: the support verdict the collector read
	// off production's own checkSupported. A constraint-bearing row that became
	// admitted is a boundary mismatch here as well as a failure of the dedicated
	// boundary test, so the differential cannot go green on an admitted fixture.
	if f.Unconstrained {
		if native.Support != nil {
			add(soMismatchBoundary, "the UNCONSTRAINED control was DECLINED by checkSupported (%v); the "+
				"decline must be caused by constraints, not by the shape", native.Support)
		}
	} else {
		switch {
		case native.Support == nil:
			add(soMismatchBoundary, "checkSupported ADMITTED a constraint-bearing bundle; native would serve a "+
				"value BAML computes differently")
		case !errors.Is(native.Support, bamlutils.ErrDeBAMLParseUnsupported):
			add(soMismatchBoundary, "checkSupported declined with an error that does not wrap "+
				"ErrDeBAMLParseUnsupported: %v", native.Support)
		}
	}

	if f.Fatal {
		// Stock is unobservable. The only claim available is that native refuses,
		// asserted here and again, with the live subprocess evidence, by
		// TestServingOracleFatalRowIsUnobservable.
		for _, s := range native.Sites {
			if s.Outcome != constraintOutcomeUnsupported {
				add(soMismatchEvent, "stock is process-fatal for this row, but native DECIDED %s for %s — "+
					"no boolean may be produced where the oracle cannot be observed", s.Outcome, s.render())
			}
		}
		return soStockUnobservable, out
	}

	// ---- native produced no state at all --------------------------------
	//
	// Three different reasons, kept apart. Two are production DECLINES and are
	// fail-closed by construction; the third is the test-only collector refusing,
	// which is a limitation of the witness and is recorded as one.
	switch native.Kind {
	case soNativeNoCandidate:
		return soNativeDeclinesExtraction, out
	case soNativeUnmodelled, soNativeCollectorDiverged:
		return soCollectorRefuses, out
	case soNativeCoercionError:
		if stock.Kind == soStockUnrecognisedError {
			add(soMismatchState, "stock returned an unrecognised error shape: %s",
				strings.Join(stock.Reasons, " | "))
			return soAgreement("unrecognised-stock-error"), out
		}
		// The two axes are independent and both matter: did native fall back with the
		// supported sentinel or fail outright, and did stock DECIDE something or refuse
		// the value too.
		//
		// A stock assertion-failure is a DECISION — stock named an assertion and
		// rejected the node on it — so it can never be a shared refusal. Folding it into
		// agree-refusal was calling a real difference an agreement.
		sentinel := errors.Is(native.Err, bamlutils.ErrDeBAMLParseUnsupported)
		switch {
		case sentinel && stock.Kind == soStockValue:
			return soNativeDeclinesCoercion, out
		case sentinel && stock.Kind == soStockAssertFailed:
			// Stock decided; native declined to coerce at all. Safe (the caller falls
			// back to BAML) but a measured cost, not an agreement.
			return soNativeDeclinesCoercion, out
		case sentinel:
			// Stock refused the value too, so neither engine produced anything.
			return soAgreeRefusal, out
		case stock.Kind == soStockValue:
			add(soMismatchState, "stock produced a value (%s) but native's coercion FAILED without the "+
				"unsupported sentinel: %v", stock.Identity, native.Err)
			return soStateDiverges, out
		case stock.Kind == soStockAssertFailed:
			add(soMismatchState, "stock REJECTED the node on a named assertion (%s) but native's coercion "+
				"FAILED without the unsupported sentinel (%v); that is a claimed native parse failure, not a "+
				"fall back to BAML, and the two legs do not agree",
				strings.Join(soFailedAssert(stock.Reasons), " | "), native.Err)
			return soStateDiverges, out
		default:
			// Stock's evaluator or coercion failed and native failed too.
			return soAgreeRefusal, out
		}
	}

	// ---- state ----------------------------------------------------------
	stateDiverged := false
	if stock.Kind == soStockValue {
		if stock.Identity != native.Identity {
			record(soMismatchState, "canonical identity differs:\n      stock  %s\n      native %s",
				stock.Identity, native.Identity)
			stateDiverged = true
		} else if diff, ok := constraintStateJSONEquivalent([]byte(native.JSON), []byte(stock.JSON)); !ok {
			// Exact, order-sensitive, big.Rat over numbers — never float64.
			record(soMismatchState, "canonical document differs at %s:\n      stock  %s\n      native %s",
				diff, stock.JSON, native.JSON)
			stateDiverged = true
		}
	}

	// ---- events ---------------------------------------------------------
	drops := soDropsByPrefix(native)
	nativeChecks := soCheckSites(native)
	stockSites := soCollapseRepeats(stock.Sites)
	eventShapeDiverged := len(stockSites) != len(stock.Sites)
	if eventShapeDiverged {
		record(soMismatchEvent, "stock REPEATED a check: %d entries collapse to %d distinct ones:\n      %s",
			len(stock.Sites), len(stockSites), soRenderStockSites(stock.Sites))
	}

	switch stock.Kind {
	case soStockValue:
		// ORDER IS ONLY CLAIMED WHERE IT WAS OBSERVED. A site read from the raw CFFI
		// tree carries stock's own order; one recovered from a folded ROOT collection
		// does not (see soFoldedSites), so those are compared as an unordered multiset
		// per path and the unavailability is recorded on the row rather than papered
		// over with the schema's order.
		certified, uncertified := 0, 0
		for _, ss := range stockSites {
			if ss.Certified {
				certified++
			} else {
				uncertified++
			}
		}
		if certified > 0 && uncertified > 0 {
			add(soMismatchEvent, "the row mixes %d CERTIFIED and %d UNCERTIFIED check site(s); the comparator "+
				"models one or the other, not both, so this shape is unmodelled rather than silently compared "+
				"at the weaker standard", certified, uncertified)
			break
		}
		if uncertified > 0 {
			record(soMismatchEvent, "root-envelope certification is UNAVAILABLE for %d site(s): baml_go folds "+
				"a root Checked's collection into a map before this oracle can see it, so stock's ORDER and "+
				"MULTIPLICITY there are unobservable. Label, expression, path and result ARE compared; order "+
				"is not claimed.", uncertified)
			out = append(out, soCompareUncertifiedSites(stockSites, nativeChecks)...)
			break
		}
		if len(stockSites) != len(nativeChecks) {
			// The ONE tolerated shape difference: stock reported nothing (its optional
			// coercion swallowed the failure) and native decided nothing either.
			// Anything else is a real disagreement.
			if len(stockSites) == 0 && len(soDecidedSites(nativeChecks)) == 0 {
				record(soMismatchEvent, "stock reported NO check (its optional coercion swallowed the "+
					"failure) where native ran %d and decided none of them: %s",
					len(nativeChecks), soRenderNativeSites(nativeChecks))
				eventShapeDiverged = true
				break
			}
			add(soMismatchEvent, "stock ran %d check(s) and native ran %d:\n      stock  %s\n      native %s",
				len(stockSites), len(nativeChecks), soRenderStockSites(stockSites), soRenderNativeSites(nativeChecks))
			// The differential fails on the violation above, but the BUCKET must move too:
			// leaving it at agree-value would let the tally and the note contract call a
			// check-collection mismatch an agreement.
			eventShapeDiverged = true
			break
		}
		for i, ss := range stockSites {
			ns := nativeChecks[i]
			if ss.Expression != ns.Expression {
				add(soMismatchEvent, "check %d: stock evaluated %q and native evaluated %q",
					i, ss.Expression, ns.Expression)
				continue
			}
			if ss.Label != ns.Label {
				add(soMismatchEvent, "check %d (%q): stock labelled it %q and native %q",
					i, ss.Expression, ss.Label, ns.Label)
			}
			// EVERY site's path is compared, root ones included. A root Checked's
			// collection reaches this code folded (see soChecked), but its path and
			// order are re-derived from the schema and its contents verified exactly,
			// so there is nothing to exempt.
			if want := soAlignNativePath(ns.Path, drops); want != ss.Path {
				add(soMismatchEvent, "check %d (%q): stock ran it at %s, native at %s (aligned %s)",
					i, ss.Expression, ss.Path, ns.Path, want)
			}
			switch ns.Outcome {
			case constraintOutcomeUnsupported:
				// Native declined this predicate: safe, and counted as a cost below.
			case constraintOutcomeTrue:
				if ss.Status != "succeeded" {
					add(soMismatchEvent, "check %d (%q): native answered TRUE where stock reported %q",
						i, ss.Expression, ss.Status)
				}
			case constraintOutcomeFalse:
				if ss.Status != "failed" {
					add(soMismatchEvent, "check %d (%q): native answered FALSE where stock reported %q",
						i, ss.Expression, ss.Status)
				}
			default:
				add(soMismatchEvent, "check %d (%q): native produced the unrecognised outcome %q",
					i, ss.Expression, ns.Outcome)
			}
		}
		// A holding @assert leaves no trace in stock's value, so every native assert
		// site must have held (or been declined). One that evaluated FALSE would mean
		// native rejects a node stock served.
		for _, s := range native.Sites {
			if s.Level == schema.ConstraintAssert && s.Outcome == constraintOutcomeFalse {
				add(soMismatchEvent, "stock SERVED the value, but native's @assert %s evaluated false", s.render())
			}
		}

	case soStockAssertFailed:
		// Stock named the assertion that rejected the node. Native must have found
		// the same one false — or have declined it.
		failed := soFailedAssert(stock.Reasons)
		if len(failed) == 0 {
			add(soMismatchEvent, "stock reported an assertion failure but named no assertion: %v", stock.Reasons)
			break
		}
		for _, want := range failed {
			matched, declined := false, false
			for _, s := range native.Sites {
				if s.Level != schema.ConstraintAssert {
					continue
				}
				// Stock renders the pair as "<label> <expression>".
				if want != strings.TrimSpace(s.Label+" "+s.Expression) {
					continue
				}
				switch s.Outcome {
				case constraintOutcomeFalse:
					matched = true
				case constraintOutcomeUnsupported:
					declined = true
				case constraintOutcomeTrue:
					add(soMismatchEvent, "stock REJECTED the node on %q, but native evaluated that assertion TRUE",
						want)
				}
			}
			if !matched && !declined {
				add(soMismatchEvent, "stock rejected the node on the assertion %q, which native's state does not "+
					"record as false or declined:\n      native %s", want, soRenderNativeSites(native.Sites))
			}
		}
		// Stock rejected the node, so it emitted no value and reported no check. The
		// ONLY predicates native may have decided are the assertions stock itself
		// named; anything else is a boolean stock did not produce.
		for _, s := range soDecidedSites(native.Sites) {
			named := false
			for _, want := range failed {
				if want == strings.TrimSpace(s.Label+" "+s.Expression) {
					named = true
				}
			}
			if !named {
				add(soMismatchEvent, "stock REJECTED the node on %v and reported no check, but native decided "+
					"%s for %s, which stock never named", failed, s.Outcome, s.render())
			}
		}

	case soStockEvaluatorError, soStockCoercionError:
		// Stock produced NO VALUE at all, so it produced no boolean either. Native
		// deciding ANY predicate here is a boolean stock did not produce — the exact
		// thing the fail-closed rule forbids — and it is a violation whether stock's
		// message happens to name that predicate or not.
		//
		// This used to be guarded only for soStockEvaluatorError, and only for
		// predicates stock's message named. That accepted the counterexample
		// "stock returned a coercion error while native returned a canonical value
		// and a true/false check", which is what
		// TestServingOracleContractRejectsNativeDecisionWithoutStock now pins.
		for _, s := range soDecidedSites(native.Sites) {
			add(soMismatchEvent, "stock produced NO value (%s: %s) but native DECIDED %s for %s; native may "+
				"never produce a boolean stock did not produce",
				stock.Kind, strings.Join(stock.Reasons, " | "), s.Outcome, s.render())
		}
		// Native canonicalizing a value where stock produced none is a STATE
		// difference. It is not by itself a contract violation — native serves
		// nothing, because the bundle declines — but it must be recorded and noted
		// rather than silently read as agreement.
		if native.Identity != "" {
			record(soMismatchState, "stock produced no value (%s) but native canonicalized %s",
				stock.Kind, native.Identity)
			stateDiverged = true
		}

	case soStockUnrecognisedError:
		add(soMismatchState, "stock returned an error shape the envelope vocabulary does not recognise, so "+
			"there is nothing to compare against: %s", strings.Join(stock.Reasons, " | "))
	}

	// ---- bucket ---------------------------------------------------------
	//
	// Priority order, most specific first. A row lands in exactly one bucket and the
	// tally pins the population of each.
	// Gate on the actual UNDECIDED population, at any level. Gating on nativeChecks
	// missed a value whose only native site is a declined @assert: it has no
	// check-level site, so the row landed in agree-value with no divergence note even
	// though native refused to decide something.
	declinedPredicate := len(soDecidedSites(native.Sites)) < len(native.Sites)
	switch {
	case stateDiverged:
		return soStateDiverges, out
	case eventShapeDiverged:
		return soEventShapeDiverges, out
	case declinedPredicate && stock.Kind == soStockValue:
		return soNativeDeclinesPredicate, out
	case stock.Kind == soStockValue:
		return soAgreeValue, out
	case stock.Kind == soStockAssertFailed:
		return soAgreeAssertFailure, out
	case stock.Kind == soStockEvaluatorError, stock.Kind == soStockCoercionError:
		return soAgreeRefusal, out
	case stock.Kind == soStockUnrecognisedError:
		return soAgreement("unrecognised-stock-error"), out
	}
	out = append(out, soMismatch{Kind: soMismatchEvent,
		Detail: "the row landed in no agreement bucket; stock kind " + string(stock.Kind)})
	return soAgreement("unbucketed"), out
}

// soStockNamesExpression reports whether stock's reason chain mentions this
// expression, i.e. whether the evaluator failure was about THIS predicate.
//
// Without it, a class whose evaluator failed on one predicate would forbid native
// from deciding a DIFFERENT, unrelated predicate on the same node — which stock
// never claimed anything about.
func soStockNamesExpression(reasons []string, expr string) bool {
	for _, r := range reasons {
		if strings.Contains(r, expr) {
			return true
		}
	}
	return false
}

func soRenderStockSites(sites []soStockSite) string {
	parts := make([]string, len(sites))
	for i, s := range sites {
		parts[i] = s.render()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func soRenderNativeSites(sites []soNativeSite) string {
	parts := make([]string, len(sites))
	for i, s := range sites {
		parts[i] = s.render()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// soReport is the PER-CASE report. Every failure in this package carries one, so no
// assertion is ever satisfied by an aggregate: the row names its own .baml source
// and method, the raw text both legs were given, the schema in play, and both
// envelopes in full.
func soReport(f servingOracleFixture, stock soStockEnvelope, native soNativeEnvelope, problems []soMismatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  fixture   %s (family %s)", f.Name, f.Family)
	fmt.Fprintf(&b, "\n  purpose   %s", f.Doc)
	fmt.Fprintf(&b, "\n  source    %s", f.source())
	fmt.Fprintf(&b, "\n  raw       %q", f.Raw)
	fmt.Fprintf(&b, "\n  schema    %s", soTypeExpr(f.Bundle.Target))
	fmt.Fprintf(&b, "\n  stock     %s", stock.render())
	fmt.Fprintf(&b, "\n  native    %s", native.render())
	fmt.Fprintf(&b, "\n  support   %v", native.Support)
	if f.Divergence != "" {
		fmt.Fprintf(&b, "\n  divergence %s", f.Divergence)
	}
	for _, p := range problems {
		fmt.Fprintf(&b, "\n  MISMATCH  %s", p)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The contract, driven directly.
// ---------------------------------------------------------------------------

// soContractFixture is a minimal constraint-bearing fixture for the direct
// contract tests. It is a real bundle, so the boundary arm of the contract sees
// the same decline it sees for a corpus row.
func soContractFixture() servingOracleFixture {
	return servingOracleFixture{
		Name: "contract-probe", Family: "scalar", Doc: "direct contract test",
		Bundle: soOneFieldBundle("SoContractProbe", soWith(intType(), soCheck("gt", "this > 0"))),
		Raw:    `{"v":5}`,
	}
}

// TestServingOracleContractRejectsNativeDecisionWithoutStock is the direct proof
// of the fail-closed rule's hardest edge: stock produced NO value, so it produced
// no boolean, and native must not produce one either.
//
// It is driven over SYNTHETIC envelopes rather than through the CFFI, because the
// point is the comparator's behaviour on a combination the current corpus does not
// contain — a combination that was silently ACCEPTED before this test existed.
func TestServingOracleContractRejectsNativeDecisionWithoutStock(t *testing.T) {
	f := soContractFixture()
	decided := func(outcome constraintStateOutcome) soNativeEnvelope {
		return soNativeEnvelope{
			Kind: soNativeValue, Identity: "class:SoContractProbe{v=int:5}", JSON: `{"v":5}`,
			Support: checkSupported(f.Bundle),
			Sites: []soNativeSite{{
				Path: "$.v", Origin: constraintOriginTypeMeta, Level: schema.ConstraintCheck,
				Labeled: true, Label: "gt", Expression: "this > 0", Outcome: outcome,
			}},
		}
	}

	cases := []struct {
		name  string
		stock soStockEnvelope
		// wantViolation is whether the contract must REJECT the pair.
		wantViolation bool
		// declineIsClean says whether the SAME pair with native DECLINING must carry
		// no violation at all. It is false only for the assertion row, where stock
		// names an assertion native's state does not record — a separate, legitimate
		// violation that has nothing to do with the decided predicate under test, and
		// one TestServingOracleContractAcceptsStockNamedAssertion covers directly.
		declineIsClean bool
	}{
		{
			name: "stock coercion error, native decides TRUE",
			stock: soStockEnvelope{Kind: soStockCoercionError,
				Reasons: []string{"Failed while parsing required fields: missing=0, unparsed=1"}},
			wantViolation: true, declineIsClean: true,
		},
		{
			name: "stock evaluator error, native decides TRUE",
			stock: soStockEnvelope{Kind: soStockEvaluatorError,
				Reasons: []string{"Failed to evaluate constraints: unknown filter: filter nope is unknown"}},
			wantViolation: true, declineIsClean: true,
		},
		{
			name: "stock evaluator error naming a DIFFERENT expression, native still decides",
			stock: soStockEnvelope{Kind: soStockEvaluatorError,
				Reasons: []string{"Failed to evaluate constraints: unknown filter: something else entirely"}},
			wantViolation: true, declineIsClean: true,
		},
		{
			name: "stock assertion failure, native decides an assertion stock did NOT name",
			stock: soStockEnvelope{Kind: soStockAssertFailed,
				Reasons: []string{"Assertions failed.", "Failed: other this > 100"}},
			wantViolation: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, outcome := range []constraintStateOutcome{constraintOutcomeTrue, constraintOutcomeFalse} {
				_, problems := soCompare(f, tc.stock, decided(outcome))
				got := len(soViolations(problems)) > 0
				if got != tc.wantViolation {
					t.Fatalf("native decided %s where stock produced no value: violation=%v, want %v\n  %v",
						outcome, got, tc.wantViolation, problems)
				}
			}
			// The DECLINED variant of the very same pair must be accepted: refusing to
			// decide is exactly what native is allowed to do.
			if !tc.declineIsClean {
				return
			}
			_, problems := soCompare(f, tc.stock, decided(constraintOutcomeUnsupported))
			for _, v := range soViolations(problems) {
				if v.Kind == soMismatchEvent {
					t.Fatalf("native DECLINING the predicate must not be a violation; got %v", v)
				}
			}
		})
	}
}

// TestServingOracleContractRejectsUnnamedDecisionBesideANamedAssertion is the
// narrow case only the "stock never named it" guard can catch.
//
// Stock rejects the node on an assertion native DOES record as false — so the
// matching arm is satisfied — and native ALSO decides an unrelated @check. Stock
// emitted no value and reported no check, so that second boolean is one stock never
// produced.
func TestServingOracleContractRejectsUnnamedDecisionBesideANamedAssertion(t *testing.T) {
	ty := soWith(intType(), soAssert("gt", "this > 100"), soCheck("pos", "this > 0"))
	f := servingOracleFixture{
		Name: "contract-mixed-probe", Family: "scalar", Doc: "direct contract test",
		Bundle: soOneFieldBundle("SoContractMixed", ty), Raw: `{"v":5}`,
	}
	stock := soStockEnvelope{Kind: soStockAssertFailed,
		Reasons: []string{"Assertions failed.", "Failed: gt this > 100"}}
	site := func(level schema.ConstraintLevel, label, expr string, o constraintStateOutcome) soNativeSite {
		return soNativeSite{Path: "$.v", Origin: constraintOriginTypeMeta, Level: level,
			Labeled: true, Label: label, Expression: expr, Outcome: o}
	}
	native := soNativeEnvelope{
		Kind: soNativeAssertFailed, Identity: "class:SoContractMixed{v=int:5}", JSON: `{"v":5}`,
		Support: checkSupported(f.Bundle),
		Sites: []soNativeSite{
			site(schema.ConstraintAssert, "gt", "this > 100", constraintOutcomeFalse),
			site(schema.ConstraintCheck, "pos", "this > 0", constraintOutcomeTrue),
		},
	}
	if _, problems := soCompare(f, stock, native); len(soViolations(problems)) == 0 {
		t.Fatal("native decided a @check stock never reported, beside an assertion stock DID name; the " +
			"unnamed decision is a boolean stock did not produce and must be a violation")
	}
	// CONTROL: the same pair with the unrelated check DECLINED is accepted, so the
	// guard is about deciding rather than about having a second constraint at all.
	native.Sites[1].Outcome = constraintOutcomeUnsupported
	if _, problems := soCompare(f, stock, native); len(soViolations(problems)) > 0 {
		t.Fatalf("declining the unrelated predicate must be accepted; got %v", soViolations(problems))
	}
}

// TestServingOracleContractAcceptsStockNamedAssertion is the other direction: the
// one decided predicate a stock no-value outcome DOES evidence is the assertion
// stock's own message names.
//
// Without it the rule above would be a blanket ban, and every assertion-failure
// row in the corpus would be a violation — a test that forbids everything proves
// nothing about what is allowed.
func TestServingOracleContractAcceptsStockNamedAssertion(t *testing.T) {
	f := servingOracleFixture{
		Name: "contract-assert-probe", Family: "scalar", Doc: "direct contract test",
		Bundle: soOneFieldBundle("SoContractAssert", soWith(intType(), soAssert("gt", "this > 100"))),
		Raw:    `{"v":5}`,
	}
	stock := soStockEnvelope{Kind: soStockAssertFailed,
		Reasons: []string{"Assertions failed.", "Failed: gt this > 100"}}
	native := soNativeEnvelope{
		Kind: soNativeAssertFailed, Identity: "class:SoContractAssert{v=int:5}", JSON: `{"v":5}`,
		Support: checkSupported(f.Bundle),
		Sites: []soNativeSite{{
			Path: "$.v", Origin: constraintOriginTypeMeta, Level: schema.ConstraintAssert,
			Labeled: true, Label: "gt", Expression: "this > 100", Outcome: constraintOutcomeFalse,
		}},
	}
	bucket, problems := soCompare(f, stock, native)
	if v := soViolations(problems); len(v) > 0 {
		t.Fatalf("the assertion stock itself named must be allowed to be decided false: %v", v)
	}
	if bucket != soAgreeAssertFailure {
		t.Fatalf("bucket = %s, want %s", bucket, soAgreeAssertFailure)
	}
	// And the same envelope with the assertion evaluating TRUE is a violation: stock
	// rejected the node on it.
	native.Sites[0].Outcome = constraintOutcomeTrue
	if _, problems := soCompare(f, stock, native); len(soViolations(problems)) == 0 {
		t.Fatal("native evaluating stock's REJECTING assertion true must be a violation")
	}
}

// TestServingOracleContractRejectsUnrecognisedStockError proves the vocabulary has
// no default bucket: an error shape it does not know is a harness failure, never
// an agreement.
func TestServingOracleContractRejectsUnrecognisedStockError(t *testing.T) {
	f := soContractFixture()
	stock := soClassifyStockError(errors.New(`ParsingError { scope: [], reason: "a brand new BAML failure mode", causes: [] }`))
	if stock.Kind != soStockUnrecognisedError {
		t.Fatalf("an unknown reason chain classified as %s; it must be %s", stock.Kind, soStockUnrecognisedError)
	}
	if len(stock.Reasons) != 1 || stock.Reasons[0] != "a brand new BAML failure mode" {
		t.Fatalf("the unknown reason was not retained verbatim: %v", stock.Reasons)
	}
	bucket, problems := soCompare(f, stock, soNativeEnvelope{
		Kind: soNativeValue, Identity: "class:SoContractProbe{v=int:5}", JSON: `{"v":5}`,
		Support: checkSupported(f.Bundle),
	})
	if len(soViolations(problems)) == 0 {
		t.Fatal("an unrecognised stock error must be a violation, not a silently accepted refusal")
	}
	if soIsAgreement(bucket) {
		t.Fatalf("an unrecognised stock error landed in the agreeing bucket %s", bucket)
	}
	// CONTROL: each known shape still classifies, so the vocabulary is not simply
	// rejecting everything.
	if len(soKnownStockErrorShapes) == 0 {
		t.Fatal("soKnownStockErrorShapes is empty; the classifier would reject every error")
	}
	for _, shape := range soKnownStockErrorShapes {
		got := soClassifyStockError(errors.New(`ParsingError { scope: [], reason: "` + shape.Fragment + ` x", causes: [] }`))
		if got.Kind != shape.Kind {
			t.Errorf("the reason fragment %q classified as %s, want %s", shape.Fragment, got.Kind, shape.Kind)
		}
	}
}

// soCompareUncertifiedSites compares a root check collection whose ORDER stock no
// longer carries.
//
// It matches by (path, label, expression) as a MULTISET and compares the result of
// each matched pair. Everything the folded map genuinely evidences is still checked
// exactly — which labels ran, at which node, over which expression text, and with
// which result — and the one thing it cannot evidence, their relative order, is not
// asserted. A native site with no stock counterpart, or a stock site with no native
// one, is a violation either way.
func soCompareUncertifiedSites(stock []soStockSite, native []soNativeSite) []soMismatch {
	type key struct{ path, label, expr string }
	remaining := map[key][]soNativeSite{}
	for _, ns := range native {
		k := key{soUnionArmRe.ReplaceAllString(ns.Path, ""), ns.Label, ns.Expression}
		remaining[k] = append(remaining[k], ns)
	}
	var out []soMismatch
	violation := func(format string, args ...any) {
		out = append(out, soMismatch{Kind: soMismatchEvent, Violation: true,
			Detail: fmt.Sprintf(format, args...)})
	}
	for _, ss := range stock {
		k := key{ss.Path, ss.Label, ss.Expression}
		pool := remaining[k]
		if len(pool) == 0 {
			violation("stock ran %s but native records no such predicate at that node:\n      native %s",
				ss.render(), soRenderNativeSites(native))
			continue
		}
		ns := pool[0]
		remaining[k] = pool[1:]
		switch ns.Outcome {
		case constraintOutcomeUnsupported:
			// Declined: safe, and counted as a cost by the bucket logic.
		case constraintOutcomeTrue:
			if ss.Status != "succeeded" {
				violation("%s: native answered TRUE where stock reported %q", ss.render(), ss.Status)
			}
		case constraintOutcomeFalse:
			if ss.Status != "failed" {
				violation("%s: native answered FALSE where stock reported %q", ss.render(), ss.Status)
			}
		default:
			violation("%s: native produced the unrecognised outcome %q", ss.render(), ns.Outcome)
		}
	}
	for k, pool := range remaining {
		for _, ns := range pool {
			if ns.Outcome == constraintOutcomeUnsupported {
				continue
			}
			violation("native DECIDED %s at %s, which stock's root collection does not contain",
				ns.render(), k.path)
		}
	}
	return out
}

// TestServingOracleBucketsDoNotCallDifferencesAgreement drives the three
// classification holes CodeRabbit found, over synthetic envelopes.
//
// Each was independently real: a row that genuinely differed could land in an
// AGREEING bucket, where the note contract requires no explanation and the tally
// records it as parity. None of them was reachable from the current corpus, which
// is exactly why they needed direct tests rather than a re-pinned count.
func TestServingOracleBucketsDoNotCallDifferencesAgreement(t *testing.T) {
	fixture := func(ty schema.Type) servingOracleFixture {
		return servingOracleFixture{
			Name: "bucket-probe", Family: "scalar", Doc: "direct bucket test",
			Bundle: soOneFieldBundle("SoBucketProbe", ty), Raw: `{"v":5}`,
		}
	}
	site := func(level schema.ConstraintLevel, label, expr string, o constraintStateOutcome) soNativeSite {
		return soNativeSite{Path: "$.v", Origin: constraintOriginTypeMeta, Level: level,
			Labeled: true, Label: label, Expression: expr, Outcome: o}
	}

	t.Run("stock decided an assertion, native could not coerce at all", func(t *testing.T) {
		f := fixture(soWith(intType(), soAssert("gt", "this > 100")))
		stock := soStockEnvelope{Kind: soStockAssertFailed,
			Reasons: []string{"Assertions failed.", "Failed: gt this > 100"}}

		// The SUPPORTED fallback: native declined to coerce. Stock still decided, so
		// this is a measured cost, never a shared refusal.
		declined := soNativeEnvelope{Kind: soNativeCoercionError, Support: checkSupported(f.Bundle),
			Err: unsupported("probe"), Message: "declined"}
		if bucket, _ := soCompare(f, stock, declined); bucket != soNativeDeclinesCoercion {
			t.Errorf("a sentinel decline against a stock ASSERTION FAILURE landed in %s; stock decided a "+
				"named assertion false, so the two legs did not agree", bucket)
		}
		// A NON-sentinel coercion failure is a claimed native parse failure, not a
		// fall back — a different thing again, and also not an agreement.
		failed := soNativeEnvelope{Kind: soNativeCoercionError, Support: checkSupported(f.Bundle),
			Err: errors.New("native blew up"), Message: "boom"}
		bucket, problems := soCompare(f, stock, failed)
		if soIsAgreement(bucket) {
			t.Errorf("a NON-sentinel coercion failure against a stock assertion failure landed in the "+
				"agreeing bucket %s", bucket)
		}
		if len(soViolations(problems)) == 0 {
			t.Error("a claimed native parse failure where stock produced a definite verdict must be a violation")
		}
		// CONTROL: where stock ALSO refused the value, a sentinel decline IS an
		// agreement — so the rule above is about stock having decided, not about
		// declines in general.
		refused := soStockEnvelope{Kind: soStockEvaluatorError,
			Reasons: []string{"Failed to evaluate constraints: unknown filter: nope"}}
		if bucket, _ := soCompare(f, refused, declined); bucket != soAgreeRefusal {
			t.Errorf("both legs refusing the value must agree; got %s", bucket)
		}
	})

	t.Run("the only declined native site is an @assert", func(t *testing.T) {
		f := fixture(soWith(intType(), soAssert("gt", "this > 0")))
		stock := soStockEnvelope{Kind: soStockValue, Identity: "class:SoBucketProbe{v=int:5}",
			JSON: `{"v":5}`}
		native := soNativeEnvelope{Kind: soNativeUnsupported, Identity: "class:SoBucketProbe{v=int:5}",
			JSON: `{"v":5}`, Support: checkSupported(f.Bundle),
			Sites: []soNativeSite{site(schema.ConstraintAssert, "gt", "this > 0", constraintOutcomeUnsupported)},
		}
		bucket, _ := soCompare(f, stock, native)
		if bucket != soNativeDeclinesPredicate {
			t.Errorf("native REFUSED to decide the only predicate on the row and it landed in %s; a declined "+
				"@assert has no check-level site, which is how this reached an agreeing bucket with no "+
				"divergence note", bucket)
		}
		// CONTROL: the same row with the assertion DECIDED is a genuine agreement.
		native.Kind = soNativeValue
		native.Sites[0].Outcome = constraintOutcomeTrue
		if bucket, _ := soCompare(f, stock, native); bucket != soAgreeValue {
			t.Errorf("a decided assertion that holds must agree; got %s", bucket)
		}
	})

	t.Run("a check-count mismatch is not an agreement", func(t *testing.T) {
		f := fixture(soWith(intType(), soCheck("a", "this > 0")))
		stock := soStockEnvelope{Kind: soStockValue, Identity: "class:SoBucketProbe{v=int:5}", JSON: `{"v":5}`,
			Sites: []soStockSite{
				{Path: "$.v", Label: "a", Expression: "this > 0", Status: "succeeded", Certified: true},
				{Path: "$.v", Label: "b", Expression: "this > 1", Status: "succeeded", Certified: true},
			}}
		native := soNativeEnvelope{Kind: soNativeValue, Identity: "class:SoBucketProbe{v=int:5}",
			JSON: `{"v":5}`, Support: checkSupported(f.Bundle),
			Sites: []soNativeSite{site(schema.ConstraintCheck, "a", "this > 0", constraintOutcomeTrue)},
		}
		bucket, problems := soCompare(f, stock, native)
		if soIsAgreement(bucket) {
			t.Errorf("stock ran 2 checks and native ran 1, and the row landed in the agreeing bucket %s; the "+
				"differential fails it separately, but the tally and the note contract would call it parity",
				bucket)
		}
		if len(soViolations(problems)) == 0 {
			t.Error("a check-count mismatch must be a violation")
		}
	})
}
