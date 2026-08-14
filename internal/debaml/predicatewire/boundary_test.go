//go:build integration

package predicatewire

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
)

// The DIRECT-i64 BOUNDARY MATRIX.
//
// # What it is for
//
// 7.2c-3 would admit a schema whose `confidence` is any i64 the strict extractor can
// produce. The scope's non-negotiable preclaim rule is that an admitted schema must have
// a total, byte-proven native outcome for EVERY such value — so the two things that have
// to be known before 7.2c-2 can be designed are:
//
//  1. what stock ANSWERS across the whole i64 range, including around ±2^53 — the
//     magnitude the native guard refuses from, one step below ±(2^53+1) where float64
//     actually stops being exact — and at the i64 endpoints; and
//  2. exactly where native's CURRENT generic evaluator guard REFUSES rather than answers.
//
// Both are measured here. The first is a stock capture. The second is an INDEPENDENT
// native evaluation of the same (expression, value) pair — native's answer is compared
// against the recorded stock answer in Go, and is never submitted back to the CFFI.
//
// # The contract, restated at the boundary
//
// internal/debaml.EvaluateConstraint promises either a boolean byte-identical to stock's
// or an error wrapping ErrConstraintUnsupported. This matrix re-asserts that at the one
// place it is most likely to break, and a native answer stock did not produce is a
// FAILURE, never a recorded difference.

// pwBoundaryOutcome is one row of the matrix: what stock said and what native did.
type pwBoundaryOutcome struct {
	threshold pwBoundaryThreshold
	value     int64
	operator  pwOperator
	stock     bool
	// nativeErr is non-nil when the native evaluator refused. The invariant is that it
	// wraps ErrConstraintUnsupported; anything else is a contract violation.
	nativeErr error
	native    bool
}

// pwStatusBool converts stock's status string to the boolean the evaluator contract is
// stated in, refusing anything that is neither.
func pwStatusBool(t *testing.T, what, status string) bool {
	t.Helper()
	switch status {
	case "succeeded":
		return true
	case "failed":
		return false
	default:
		t.Fatalf("%s: stock reported status %q, which is neither succeeded nor failed", what, status)
		return false
	}
}

// TestDirectIntBoundaryMatrix drives the whole literal x value x operator matrix through
// stock, then evaluates the SAME captured expression over the SAME value with the
// production native evaluator, and reports where the two diverge.
//
// Byte-level pinning is deliberately NOT used for these rows. Each boundary function
// carries SIX checks, so stock's public map has six keys and sonic.Marshal of a Go map
// has no stable byte order — the very instability [TestTwoCheckWireOrderIsUnstable]
// records. The assertion is therefore per-check and exact: name, expression and status,
// compared field by field.
func TestDirectIntBoundaryMatrix(t *testing.T) {
	var rows []pwBoundaryOutcome
	refusedBy := map[string]int{}

	for _, b := range pwBoundaryThresholds() {
		for _, v := range pwBoundaryValues(b.N) {
			key := pwDriveKey{Project: b.projectKey(), Func: pwBoundaryFn, Raw: strconv.FormatInt(v, 10)}
			stock := pwBareChecked(t, key)

			if stock.Value != v {
				t.Fatalf("%s: stock coerced the value to %d, want %d; the matrix would be measuring "+
					"a different number", key, stock.Value, v)
			}
			if len(stock.Checks) != len(pwOperators()) {
				t.Fatalf("%s: stock reported %d checks, want %d (one per operator): %v",
					key, len(stock.Checks), len(pwOperators()), stock.Checks)
			}

			for _, o := range pwOperators() {
				expr := fmt.Sprintf("this %s %s", o.Op, b.literal())
				got, ok := stock.Checks[o.ID]
				if !ok {
					t.Fatalf("%s: stock reported no check under %q", key, o.ID)
				}
				if got.Name != o.ID || got.Expression != expr {
					t.Fatalf("%s: stock check %q = {name:%q expression:%q}, want {name:%q expression:%q}",
						key, o.ID, got.Name, got.Expression, o.ID, expr)
				}
				stockAnswer := pwStatusBool(t, key.String()+" "+o.ID, got.Status)

				// The NATIVE leg. It is given the expression stock REPORTED — not the
				// .baml attribute text — and the same value, and it is evaluated in
				// this process without any BAML involvement.
				nativeAnswer, nativeErr := debaml.EvaluateConstraint(debaml.IntValue(v), got.Expression)
				if nativeErr != nil && !errors.Is(nativeErr, debaml.ErrConstraintUnsupported) {
					t.Fatalf("%s %s: native returned an error that is NOT ErrConstraintUnsupported, "+
						"which breaks the fail-closed contract: %v", key, o.ID, nativeErr)
				}
				if nativeErr == nil && nativeAnswer != stockAnswer {
					t.Errorf("%s %s (%q over this=%d): native answered %v where stock answered %v — "+
						"a boolean stock did not produce",
						key, o.ID, got.Expression, v, nativeAnswer, stockAnswer)
				}
				if nativeErr != nil {
					refusedBy[pwRefusalReason(b, v)]++
				}
				rows = append(rows, pwBoundaryOutcome{
					threshold: b, value: v, operator: o,
					stock: stockAnswer, native: nativeAnswer, nativeErr: nativeErr,
				})
			}
		}
	}

	// The matrix must be the size it CLAIMS to be, against a PINNED number rather than
	// one recomputed from the same tables that produced the rows. A derived expectation
	// shrinks in step with a deleted threshold and reports nothing; this one goes red.
	// [TestDirectIntBoundaryMatrixShapeIsPinned] carries the structural half.
	if len(rows) != pwBoundaryRowCount {
		t.Fatalf("the matrix produced %d rows, want the pinned %d; a threshold or a value was dropped "+
			"(or added) without the pin being updated", len(rows), pwBoundaryRowCount)
	}

	answered, refused := 0, 0
	for _, r := range rows {
		if r.nativeErr != nil {
			refused++
			continue
		}
		answered++
	}
	// A COVERAGE claim, logged so a shrunken matrix cannot read as a full one. The
	// clamped endpoints are named explicitly: math.MinInt64 and math.MaxInt64 have only
	// one neighbour inside i64, so their rows are 2 values rather than 3 by arithmetic,
	// not by omission.
	t.Logf("direct-i64 boundary matrix: %d thresholds x 6 operators = %d rows "+
		"(%d stock-answered rows native reproduces, %d native REFUSES). "+
		"math.MinInt64/math.MaxInt64 contribute 2 values each rather than 3 because one neighbour "+
		"falls outside i64.", len(pwBoundaryThresholds()), len(rows), answered, refused)
	for _, reason := range pwSortedKeys(refusedBy) {
		t.Logf("  native refuses %3d row(s): %s", refusedBy[reason], reason)
	}

	// Stock ANSWERED every row — asserted inline above, where a status that was neither
	// `succeeded` nor `failed` is fatal. If stock had refused one, that row would be a
	// value an admitted direct-int schema could not be total over, and the whole 7.2c
	// premise would need revisiting.
	//
	// AND SO DOES NATIVE, AS OF SLICE 7.2c-2. When this matrix was first written it
	// measured a 36-answered / 186-refused split across three named clauses, and that
	// split WAS the totality blocker the scope handed to 7.2c-2. Every row here is the
	// closed direct grammar `this OP <canonical i64>` over an integer `this`, which
	// internal/debaml.EvaluateConstraint now decides with an exact int64 comparison
	// (constraint_direct_i64.go) rather than routing through the generic numeric
	// whitelist. So the residual is GONE, and the assertion is inverted: a refusal here
	// would mean the totality repair had regressed on a value an admitted schema can
	// reach.
	//
	// The three clauses are NOT gone — they are unchanged, and still refuse everything
	// outside the direct grammar. [TestGenericNumericProfileStillRefusesOutsideTheDirectGrammar]
	// is what keeps that a measurement rather than an assertion, by driving the same
	// thresholds and values through expressions the direct grammar does not cover.
	if refused != 0 {
		t.Fatalf("native REFUSED %d of %d boundary rows. Every row is the closed direct grammar over "+
			"an i64 the strict extractor can produce, and Slice 7.2c-2 made that path total — a "+
			"refusal here is a post-claim unsupported an admitted direct-int schema could reach:\n  %s",
			refused, len(rows), strings.Join(pwSortedKeys(refusedBy), "\n  "))
	}
	if answered != pwBoundaryRowCount {
		t.Fatalf("native answered %d of the %d boundary rows; the matrix must be total", answered, pwBoundaryRowCount)
	}
	// NON-VACUITY of the agreement itself. Every row was compared against stock inline
	// above, and both outcomes must actually occur — a matrix in which stock said `true`
	// everywhere would be satisfied by a comparator that always answered true.
	stockTrue, stockFalse := 0, 0
	for _, r := range rows {
		if r.stock {
			stockTrue++
		} else {
			stockFalse++
		}
		if r.native != r.stock {
			t.Fatalf("row %s/%s over this=%d: native %v vs stock %v after the totality repair",
				r.threshold.ID, r.operator.ID, r.value, r.native, r.stock)
		}
	}
	if stockTrue == 0 || stockFalse == 0 {
		t.Fatalf("stock answered true on %d rows and false on %d; both outcomes must appear or the "+
			"agreement proves only one direction", stockTrue, stockFalse)
	}
	t.Logf("  native now reproduces ALL %d rows exactly (%d true, %d false), including all %d "+
		"negative-literal rows and every row past ±2^53 — the 36/186 split this matrix recorded "+
		"before Slice 7.2c-2 is CLOSED, and the three refusal clauses it named survive only outside "+
		"the direct grammar", answered, stockTrue, stockFalse, pwNegativeLiteralRows(rows))
}

// TestGenericNumericProfileStillRefusesOutsideTheDirectGrammar is the other half of
// the closure recorded above, and the one that keeps it honest.
//
// Slice 7.2c-2 narrowed the numeric guard BY A GRAMMAR, not by a magnitude: the exact
// path answers `this OP <canonical i64>` and nothing else, and every other expression
// over the same oversized values keeps the existing fail-closed profile. If that were
// not so, the all-answered matrix above would be recording a loosened guard rather
// than a closed gap.
//
// It is split into TWO claims, because the two things that have to stay true are
// different in kind and only one of them is about the clause table:
//
//  1. [TestGenericNumericProfileClauseAttributionIsExact] — the three MAGNITUDE /
//     LITERAL / SIGN clauses still fire exactly where they always did. That is a claim
//     about [pwRefusalClauses], and it is asserted as an IFF on the one expression
//     shape those clauses were written to describe.
//  2. this test — expressions that are outside the direct grammar for a SHAPE reason
//     (arithmetic, an unproven filter, a compound, a stringify) are still refused, at
//     every threshold and every value. No clause label is claimed here, and that is
//     deliberate: `this + 0 == 0` is refused because `this` is not the closed numeric
//     sublanguage, not because of any magnitude — labelling such a row
//     `i64_beyond_exact` would be an attribution the profile never made.
//
// An earlier round of this file did exactly that: every probe advertised a clauseID
// while selecting rows with "any documented clause applies", so a probe labelled
// `i64_beyond_exact` could be satisfied entirely by rows where only
// `i64_negative_literal` applied — and, worse, its refusals were caused by the
// expression's shape rather than by either clause. The label was decoration. It is
// gone from the shape sweep and made an exact claim in the sibling test.
func TestGenericNumericProfileStillRefusesOutsideTheDirectGrammar(t *testing.T) {
	// Every probe is refused for a SHAPE reason that holds at every magnitude, so each
	// is driven over the WHOLE axis — no applicability predicate, and therefore no way
	// for a shrinking row population to hide a regression.
	probes := []struct {
		name string
		expr func(b pwBoundaryThreshold) string
		why  string
	}{{
		name: "arithmetic on the value",
		expr: func(b pwBoundaryThreshold) string { return "this + 0 == " + b.literal() },
		why:  "`this` is not a numeric literal, so the whole expression cannot parse as the closed sublanguage",
	}, {
		name: "an UNPROVEN filter over the value",
		expr: func(b pwBoundaryThreshold) string { return "this|round == " + b.literal() },
		why: "`round` has no declared result kind, so the operator gate cannot prove the comparison " +
			"is same-kind. (An ADMITTED filter such as `|abs` over a small value is legitimately " +
			"ANSWERED — that is the generic profile working, not a regression, and it is covered by " +
			"the answered direction of the clause test below. Driving it here would report a correct " +
			"answer as a failure, which is how this probe was wrong in an earlier round.)",
	}, {
		name: "a compound predicate",
		expr: func(b pwBoundaryThreshold) string { return "this > " + b.literal() + " and this < 0" },
		why:  "`and` is outside the closed predicate grammar (#583)",
	}, {
		name: "a stringify comparison",
		expr: func(b pwBoundaryThreshold) string { return `this|string == "` + b.literal() + `"` },
		why:  "the two engines do not render numbers alike",
	}}

	drives, refusals := 0, 0
	perProbe := map[string]int{}
	for _, p := range probes {
		for _, b := range pwBoundaryThresholds() {
			for _, v := range pwBoundaryValues(b.N) {
				drives++
				_, err := debaml.EvaluateConstraint(debaml.IntValue(v), p.expr(b))
				if err == nil {
					t.Errorf("%s: %q over this=%d was ANSWERED; only the closed direct grammar is "+
						"exact, and the generic profile must be unchanged (%s)", p.name, p.expr(b), v, p.why)
					continue
				}
				if !errors.Is(err, debaml.ErrConstraintUnsupported) {
					t.Errorf("%s: %q over this=%d returned an error that is not the decline sentinel: %v",
						p.name, p.expr(b), v, err)
					continue
				}
				refusals++
				perProbe[p.name]++
			}
		}
	}
	if drives == 0 {
		t.Fatal("the shape sweep drove no rows; it would be vacuous")
	}
	if refusals != drives {
		t.Fatalf("%d of %d shape-sweep rows were answered; every one is outside the direct grammar "+
			"and must keep the fail-closed profile", drives-refusals, drives)
	}
	for _, p := range probes {
		if perProbe[p.name] != pwBoundaryValueDrives {
			t.Errorf("probe %q drove %d rows, want the full axis of %d", p.name, perProbe[p.name],
				pwBoundaryValueDrives)
		}
	}
	t.Logf("the GENERIC profile is unchanged on SHAPE: %d probes x %d value-drives = %d rows outside "+
		"the direct grammar, all refused", len(probes), pwBoundaryValueDrives, drives)
}

// TestGenericNumericProfileClauseAttributionIsExact is the CLAUSE half, and it is where
// the ledger ids in [pwRefusalClauses] are a claim rather than a label.
//
// The probe is the direct comparison written UNSPACED — `this>I`. That one byte puts it
// outside the exact path's closed grammar (which requires exactly one ASCII space on
// each side), so it goes to the generic evaluator; but the guard reads the VALUE and the
// LITERAL TEXT, neither of which the space changes. It is therefore the one expression
// for which the three clauses describe the refusal exactly, and the assertion can be an
// IFF instead of a one-way implication:
//
//	the generic evaluator refuses  <=>  at least one documented clause applies
//	the reported attribution       ==   exactly the clauses that apply, in table order
//
// That is what a mislabelled clause cannot survive. Selecting rows by a clause and then
// checking that the same clause applies would be self-satisfying; here the clause table
// has to predict the evaluator's behaviour on every row of the axis, in both directions.
func TestGenericNumericProfileClauseAttributionIsExact(t *testing.T) {
	clauses := pwRefusalClauses()
	if len(clauses) != 3 {
		t.Fatalf("the clause table has %d entries; 7.2c-1 recorded three independent clauses", len(clauses))
	}
	seen := map[string]bool{}
	for _, c := range clauses {
		if c.ledgerID == "" || c.name == "" || c.applies == nil {
			t.Fatalf("clause %q is incomplete", c.ledgerID)
		}
		if seen[c.ledgerID] {
			t.Fatalf("clause id %q appears twice; per-clause counts would be merged", c.ledgerID)
		}
		seen[c.ledgerID] = true
	}
	// The ids the residual ledger records, written out independently of the table.
	for _, id := range []string{"i64_beyond_exact", "i64_long_literal", "i64_negative_literal"} {
		if !seen[id] {
			t.Fatalf("the clause table omits %q, which residuals.md records as a row", id)
		}
	}

	answered, refused := 0, 0
	perClause, soleClause := map[string]int{}, map[string]int{}
	for _, b := range pwBoundaryThresholds() {
		for _, v := range pwBoundaryValues(b.N) {
			expr := "this>" + b.literal() // one byte outside the exact path's grammar
			var applying []pwRefusalClause
			for _, c := range clauses {
				if c.applies(b, v) {
					applying = append(applying, c)
				}
			}
			_, err := debaml.EvaluateConstraint(debaml.IntValue(v), expr)

			if len(applying) == 0 {
				// NO clause applies, so the generic profile must ANSWER. This is the
				// direction a loosened guard could never fail and a mislabelled clause
				// table always does.
				if err != nil {
					t.Errorf("%q over this=%d was REFUSED (%v) but no documented clause covers it; "+
						"the clause table no longer describes the guard", expr, v, err)
					continue
				}
				answered++
				continue
			}
			if err == nil {
				t.Errorf("%q over this=%d was ANSWERED, but %d documented clause(s) cover it (%s); "+
					"the generic guard has been loosened", expr, v, len(applying), pwRefusalReason(b, v))
				continue
			}
			if !errors.Is(err, debaml.ErrConstraintUnsupported) {
				t.Errorf("%q over this=%d returned an error that is not the decline sentinel: %v", expr, v, err)
				continue
			}
			// EXACT attribution: the reported reason must name exactly the clauses that
			// apply, in table order — not merely "some documented reason".
			var want []string
			for _, c := range applying {
				want = append(want, c.name)
			}
			if got, wantJoined := pwRefusalReason(b, v), strings.Join(want, " + "); got != wantJoined {
				t.Errorf("%q over this=%d is attributed %q, want exactly %q", expr, v, got, wantJoined)
				continue
			}
			refused++
			for _, c := range applying {
				perClause[c.ledgerID]++
			}
			if len(applying) == 1 {
				soleClause[applying[0].ledgerID]++
			}
		}
	}

	if answered == 0 || refused == 0 {
		t.Fatalf("the clause probe answered %d rows and refused %d; both directions must occur or the "+
			"IFF is one-sided", answered, refused)
	}
	// NON-VACUITY PER CLAUSE on the axis: every clause must explain at least one refusal.
	for _, c := range clauses {
		if perClause[c.ledgerID] == 0 {
			t.Errorf("clause %s (%s) explains no refusal; it is not proof material any more",
				c.ledgerID, c.name)
		}
	}
	for _, id := range pwSortedKeys(perClause) {
		t.Logf("  clause %s: explains %d refusal(s) on the axis, %d of them alone", id, perClause[id], soleClause[id])
	}
	t.Logf("clause attribution is EXACT over the %d-row axis: %d refused / %d answered, and the "+
		"reported reason equals the applying clause set on every refused row", answered+refused, refused, answered)

	// ISOLATION — the strong form, and it needs drives the CFFI axis cannot supply.
	//
	// A clause that only ever co-occurs with another is UNFALSIFIABLE: dropping it would
	// change no outcome, which is exactly how the long-literal clause could have been
	// folded into the 2^53 one and misdescribed the residual. The axis cannot settle that
	// for `i64_beyond_exact` by itself — it drives each value near its own threshold, so
	// a value past 2^53 only ever appears against a literal that is also >15 digits (the
	// `%d of them alone` counts above show it directly). The isolating pair is a SMALL
	// non-negative literal at a HUGE value, which the axis never forms.
	//
	// So each clause is isolated explicitly, and every isolating row must (a) have that
	// clause as the ONLY one applying, (b) be refused, and (c) be attributed to that
	// clause and nothing else.
	for _, iso := range []struct {
		clauseID string
		n, value int64
	}{
		// A one-digit non-negative literal: neither the length nor the sign clause can
		// apply, so only the VALUE magnitude can explain a refusal.
		{clauseID: "i64_beyond_exact", n: 0, value: maxExactInt},
		// 2^53-1 is BELOW the guard's magnitude threshold and non-negative, so only its
		// sixteen digits can explain a refusal.
		{clauseID: "i64_long_literal", n: maxExactInt - 1, value: 0},
		// A small negative literal at a small value: neither magnitude nor length
		// applies, so only the sign clause can.
		{clauseID: "i64_negative_literal", n: -1, value: 0},
	} {
		clause := pwClauseByID(t, iso.clauseID)
		b := pwBoundaryThreshold{ID: "isolate_" + iso.clauseID, N: iso.n}
		var applying []string
		for _, c := range clauses {
			if c.applies(b, iso.value) {
				applying = append(applying, c.ledgerID)
			}
		}
		if len(applying) != 1 || applying[0] != iso.clauseID {
			t.Errorf("the isolating drive for %s (literal %d, this=%d) has applying clauses %v; it must "+
				"isolate exactly that clause or it proves nothing about it", iso.clauseID, iso.n, iso.value, applying)
			continue
		}
		expr := "this>" + b.literal()
		if _, err := debaml.EvaluateConstraint(debaml.IntValue(iso.value), expr); err == nil {
			t.Errorf("clause %s (%s) is UNFALSIFIABLE: %q over this=%d is the one row where it is the "+
				"sole documented reason, and the generic profile ANSWERED it — so dropping the clause "+
				"would change no outcome", iso.clauseID, clause.name, expr, iso.value)
			continue
		} else if !errors.Is(err, debaml.ErrConstraintUnsupported) {
			t.Errorf("clause %s: %q over this=%d returned an error that is not the decline sentinel: %v",
				iso.clauseID, expr, iso.value, err)
			continue
		}
		if got := pwRefusalReason(b, iso.value); got != clause.name {
			t.Errorf("clause %s: the isolating row is attributed %q, want exactly %q",
				iso.clauseID, got, clause.name)
			continue
		}
		t.Logf("  clause %s ISOLATED: %q over this=%d is refused, and %s is the only reason",
			iso.clauseID, expr, iso.value, clause.name)
	}
}

// pwClauseByID resolves one documented refusal clause by its ledger id, FAILING if the
// id is unknown.
//
// Failing is the point: a caller that named a renamed or deleted clause would otherwise
// select nothing, count nothing, and pass — the exact shape of a proof that quietly
// stops proving anything.
func pwClauseByID(t *testing.T, id string) pwRefusalClause {
	t.Helper()
	for _, c := range pwRefusalClauses() {
		if c.ledgerID == id {
			return c
		}
	}
	t.Fatalf("no documented refusal clause carries the ledger id %q; a caller names a clause the "+
		"table no longer has", id)
	return pwRefusalClause{}
}

// pwNegativeLiteralRows counts the matrix rows whose THRESHOLD is negative.
func pwNegativeLiteralRows(rows []pwBoundaryOutcome) int {
	n := 0
	for _, r := range rows {
		if r.threshold.N < 0 {
			n++
		}
	}
	return n
}

// The PINNED shape of the boundary matrix.
//
// These are written out rather than derived from pwBoundaryThresholds()/pwBoundaryValues()
// so that deleting a threshold — or quietly clamping one — fails instead of shrinking the
// proof in step with its own expectation. The arithmetic is stated once here and checked
// against the tables by [TestDirectIntBoundaryMatrixShapeIsPinned]:
//
//	11 interior thresholds x 3 values (n-1, n, n+1)      = 33
//	 2 i64 endpoints      x 2 values (one neighbour only) =  4
//	                                                        37 value-drives
//	37 value-drives x 6 operators                         = 222 rows
const (
	pwBoundaryThresholdCount = 13
	pwBoundaryValueDrives    = 37
	pwBoundaryRowCount       = 222
)

// TestDirectIntBoundaryMatrixShapeIsPinned is the structural half of the size claim: it
// checks the TABLES against the pinned numbers, independently of the matrix run, and
// checks the thresholds are the distinct, deliberately chosen set the scope names.
//
// Together with the pinned row count in [TestDirectIntBoundaryMatrix], this is what makes
// "222 rows" a claim rather than a restatement of whatever the tables happened to hold.
func TestDirectIntBoundaryMatrixShapeIsPinned(t *testing.T) {
	thresholds := pwBoundaryThresholds()
	if len(thresholds) != pwBoundaryThresholdCount {
		t.Fatalf("the threshold axis has %d entries, want the pinned %d", len(thresholds), pwBoundaryThresholdCount)
	}
	seenID := map[string]bool{}
	seenN := map[int64]bool{}
	drives := 0
	for _, b := range thresholds {
		if seenID[b.ID] {
			t.Errorf("threshold id %q appears twice", b.ID)
		}
		seenID[b.ID] = true
		if seenN[b.N] {
			t.Errorf("threshold value %d appears twice, so one of its rows is a duplicate rather than "+
				"a distinct boundary", b.N)
		}
		seenN[b.N] = true

		values := pwBoundaryValues(b.N)
		wantValues := 3
		if b.N == math.MinInt64 || b.N == math.MaxInt64 {
			wantValues = 2 // one neighbour falls outside i64
		}
		if len(values) != wantValues {
			t.Errorf("threshold %s (%d) drives %d values, want %d", b.ID, b.N, len(values), wantValues)
		}
		// The values really are the neighbourhood of the threshold, not an arbitrary
		// triple: the threshold itself must be among them.
		found := false
		for _, v := range values {
			if v == b.N {
				found = true
			}
		}
		if !found {
			t.Errorf("threshold %s (%d) is not among the values driven against it", b.ID, b.N)
		}
		drives += len(values)
	}
	if drives != pwBoundaryValueDrives {
		t.Fatalf("the threshold axis produces %d value-drives, want the pinned %d", drives, pwBoundaryValueDrives)
	}
	if got := drives * len(pwOperators()); got != pwBoundaryRowCount {
		t.Fatalf("%d value-drives x %d operators = %d, but the pinned row count is %d",
			drives, len(pwOperators()), got, pwBoundaryRowCount)
	}
	// And the drive table — the INDEPENDENT path the CFFI is actually driven through —
	// must agree with the same number.
	boundaryDrives := 0
	for _, k := range pwAllDrives() {
		if strings.HasPrefix(k.Project, "bound_") {
			boundaryDrives++
		}
	}
	if boundaryDrives != pwBoundaryValueDrives {
		t.Fatalf("the drive table performs %d boundary parses but the threshold axis describes %d; the "+
			"matrix and the rows actually driven have diverged", boundaryDrives, pwBoundaryValueDrives)
	}
	// The scope names these specific boundaries; a matrix that dropped one would still be
	// "13 thresholds" if a filler replaced it.
	for _, want := range []int64{
		0, 1, -1,
		maxExactInt - 1, maxExactInt, maxExactInt + 1,
		-(maxExactInt - 1), -maxExactInt, -(maxExactInt + 1),
		math.MaxInt64 - 1, math.MaxInt64, math.MinInt64 + 1, math.MinInt64,
	} {
		if !seenN[want] {
			t.Errorf("the threshold axis omits %d, which the scope names explicitly", want)
		}
	}
}

// TestAdmittedGreaterThanNeverReachesPostClaimUnsupported is the row Slice 7.2c-1
// wrote to be turned green, and Slice 7.2c-2 turned it.
//
// WHAT IT USED TO RECORD. `staticCheckedThreshold` admits any literal that round-trips
// through strconv.FormatInt — math.MinInt64 and math.MaxInt64 included — and the strict
// extractor can produce any i64 value, while the generic evaluator refused most of that
// range. So a bundle the production gates admitted could reach
// ErrConstraintUnsupported AFTER the admission decision. Measured on the SHIPPED
// `this > I` fingerprint, that was 31 of 37 (literal, value) pairs: not a proposed
// hazard, a live one.
//
// WHAT IT RECORDS NOW. The same drive, over the same admitted literals, with zero
// post-claim refusals — because [debaml.EvaluateConstraint] decides the closed direct
// grammar exactly. The admission side is asserted FIRST and unchanged: if the gates
// stopped admitting the boundary literals, the zero below would be vacuous rather than
// a repair, so a narrowed fingerprint fails here instead of passing quietly.
func TestAdmittedGreaterThanNeverReachesPostClaimUnsupported(t *testing.T) {
	admittedLiterals, reachable, total := 0, 0, 0
	var examples []string
	for _, b := range pwBoundaryThresholds() {
		expr := "this > " + b.literal()
		bundle := pwBundleFor(schema.ConstraintCheck, pwCheckedLabel, expr)
		if !pwAdmits(t, expr, bundle) {
			continue
		}
		admittedLiterals++
		for _, v := range pwBoundaryValues(b.N) {
			total++
			if _, err := debaml.EvaluateConstraint(debaml.IntValue(v), expr); err != nil {
				reachable++
				if len(examples) < 3 {
					examples = append(examples, fmt.Sprintf("%q over this=%d: %v", expr, v, err))
				}
			}
		}
	}
	if admittedLiterals != len(pwBoundaryThresholds()) {
		t.Fatalf("the production gates admit `this > I` for %d of %d boundary literals; the 7.2b "+
			"fingerprint accepts every canonical i64, and a narrower one would make the zero below "+
			"vacuous rather than a repair", admittedLiterals, len(pwBoundaryThresholds()))
	}
	if total == 0 {
		t.Fatal("no (literal, value) pair was driven; the claim below would be about nothing")
	}
	if reachable != 0 {
		t.Fatalf("POST-CLAIM UNSUPPORTED: %d of %d (literal, value) pairs on the ADMITTED `this > I` "+
			"predicate still refuse AFTER the admission decision. Slice 7.2c-2 closed this; examples:\n  %s",
			reachable, total, strings.Join(examples, "\n  "))
	}
	t.Logf("RECORDED (7.2c-1's blocker, CLOSED by 7.2c-2): the gates admit `this > I` for %d of %d "+
		"boundary literals; all %d (literal, value) pairs that follow are now decided by the exact "+
		"direct-i64 path. 0 post-claim refusals, down from the 31 of 37 7.2c-1 measured",
		admittedLiterals, len(pwBoundaryThresholds()), total)

	// The two endpoints, named explicitly, because they are the values a float64 core
	// loses and the ones a "mostly total" repair would still miss.
	for _, v := range []int64{math.MaxInt64, math.MinInt64} {
		expr := "this > " + strconv.FormatInt(v, 10)
		if !pwAdmits(t, expr, pwBundleFor(schema.ConstraintCheck, pwCheckedLabel, expr)) {
			t.Fatalf("%q is no longer admitted; the claim above is stale", expr)
		}
		for _, drive := range []int64{math.MinInt64, 0, math.MaxInt64} {
			got, err := debaml.EvaluateConstraint(debaml.IntValue(drive), expr)
			if err != nil {
				t.Fatalf("the production evaluator refused %q at this=%d: %v", expr, drive, err)
			}
			if want := drive > v; got != want {
				t.Errorf("%q at this=%d = %v, want %v", expr, drive, got, want)
			}
		}
	}
}

// pwRefusalReason names WHICH clause of the generic numeric profile refuses a row.
//
// The two clauses are independent and both reachable here, so a single "native refused"
// tally would hide which one is doing the work — and 7.2c-2 has to close BOTH for the
// direct grammar, not just the one that happens to fire most often:
//
//   - the VALUE clause: |this| >= 2^53 is refused before evaluation, whatever the
//     expression (constraint_profile.go's maxAbsInt check); and
//   - the EXPRESSION clause: a numeric token longer than 15 digits is not provably small,
//     and any `-` byte makes the whole expression arithmetic that must parse as the closed
//     numeric sublanguage — which `this ...` never does.
func pwRefusalReason(b pwBoundaryThreshold, value int64) string {
	var reasons []string
	for _, c := range pwRefusalClauses() {
		if c.applies(b, value) {
			reasons = append(reasons, c.name)
		}
	}
	if len(reasons) == 0 {
		return "UNATTRIBUTED — no clause of the documented profile explains this refusal"
	}
	return strings.Join(reasons, " + ")
}

// pwRefusalClause is one named clause of the generic numeric profile, with the ledger row
// that records it as a residual.
//
// The clause set is DATA so that [TestRefusalClausesEachHaveALedgerRow] can enumerate it.
// The three clauses are independent, and the third is the one it is easiest to leave
// unrecorded: `9007199254740991` is 2^53-1 — BELOW the exactness bound — yet it is
// sixteen digits, so it is refused by the literal-length clause alone. Folding it into
// the 2^53 row would misdescribe the residual and understate what 7.2c-2 has to close.
type pwRefusalClause struct {
	// name is the string pwRefusalReason emits.
	name string
	// ledgerID is the residuals.md row that records this clause.
	ledgerID string
	applies  func(b pwBoundaryThreshold, value int64) bool
}

func pwRefusalClauses() []pwRefusalClause {
	return []pwRefusalClause{{
		name:     "VALUE |this| >= 2^53",
		ledgerID: "i64_beyond_exact",
		applies:  func(_ pwBoundaryThreshold, value int64) bool { return absInt64(value) >= uint64(maxExactInt) },
	}, {
		name:     "LITERAL has more than 15 digits",
		ledgerID: "i64_long_literal",
		applies: func(b pwBoundaryThreshold, _ int64) bool {
			return len(strings.TrimPrefix(b.literal(), "-")) > 15
		},
	}, {
		name:     "LITERAL is negative, so the expression carries an arithmetic byte",
		ledgerID: "i64_negative_literal",
		applies:  func(b pwBoundaryThreshold, _ int64) bool { return strings.HasPrefix(b.literal(), "-") },
	}}
}

// absInt64 is |v| as a uint64, which is what the profile's own magnitude test uses:
// math.MinInt64 has no positive int64 counterpart.
func absInt64(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}

func pwSortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDirectIntBoundaryRefusalsAreAttributed requires every native refusal in the matrix
// to be explained by a NAMED clause of the documented profile.
//
// An unattributed refusal is the dangerous kind: it means native declines for a reason
// nobody has written down. As of Slice 7.2c-2 the direct grammar produces no refusals at
// all, so this now asserts the stronger form — none, attributed or otherwise — and the
// attribution machinery it was written to police is exercised on expressions OUTSIDE the
// direct grammar by
// [TestGenericNumericProfileStillRefusesOutsideTheDirectGrammar]. Keeping both is what
// stops "no refusals" from quietly meaning "nothing was checked".
func TestDirectIntBoundaryRefusalsAreAttributed(t *testing.T) {
	drives, refusals := 0, 0
	for _, b := range pwBoundaryThresholds() {
		for _, v := range pwBoundaryValues(b.N) {
			for _, o := range pwOperators() {
				drives++
				expr := fmt.Sprintf("this %s %s", o.Op, b.literal())
				_, err := debaml.EvaluateConstraint(debaml.IntValue(v), expr)
				if err == nil {
					continue
				}
				refusals++
				t.Errorf("native refused %q over this=%d (%s): the direct grammar is total after "+
					"Slice 7.2c-2, so any refusal here is a post-claim unsupported an admitted "+
					"schema could reach — %v", expr, v, pwRefusalReason(b, v), err)
			}
		}
	}
	if drives != pwBoundaryRowCount {
		t.Fatalf("the attribution sweep drove %d rows, want the pinned %d", drives, pwBoundaryRowCount)
	}
	if refusals != 0 {
		t.Errorf("%d refusal(s) remain inside the direct grammar", refusals)
	}
	// NON-VACUITY of the attribution machinery itself: the clause table still has to be
	// able to name a reason, or the sibling test above would be reporting nothing.
	big := pwBoundaryThreshold{ID: "probe", N: math.MinInt64}
	if reason := pwRefusalReason(big, math.MinInt64); strings.HasPrefix(reason, "UNATTRIBUTED") {
		t.Fatalf("the clause table can no longer attribute a refusal at math.MinInt64 (%s); the "+
			"generic-profile probe would then report nothing", reason)
	}
}

// TestDirectIntBoundaryIsProvenToBite is the anti-false-green control for the matrix.
//
// The comparison above only bites if a WRONG native answer would actually be reported. A
// float64-based comparator is the exact wrong implementation the 2^53 guard exists to
// prevent, so it is used as the mutant: at 2^53+1 it conflates two distinct integers, and
// the matrix must disagree with it where stock does.
func TestDirectIntBoundaryIsProvenToBite(t *testing.T) {
	// (1) The float64 mutant conflates 2^53 and 2^53+1; stock does not. If these agreed,
	// the whole exactness claim would be untestable at this boundary.
	const at, above = maxExactInt, maxExactInt + 1
	if float64(at) != float64(above) {
		t.Fatal("float64 distinguishes 2^53 from 2^53+1 on this platform, so the mutant below is not " +
			"the wrong implementation this matrix is meant to exclude")
	}
	key := pwDriveKey{Project: "bound_exact_at", Func: pwBoundaryFn, Raw: strconv.FormatInt(above, 10)}
	stock := pwBareChecked(t, key)
	eq, ok := stock.Checks["eq"]
	if !ok {
		t.Fatalf("%s: stock reported no `eq` check: %v", key, stock.Checks)
	}
	// Stock says FALSE where the float64 mutant (checked above to genuinely conflate the
	// two) would have said TRUE. That gap is the whole content of this row.
	if got := pwStatusBool(t, "eq at 2^53+1", eq.Status); got {
		t.Fatalf("stock says %d == %d is TRUE; it is the float64 answer, not the exact one",
			above, at)
	}

	// (2) The endpoints must be REACHED, not approximated. A matrix that silently
	// clamped its thresholds would be green while never touching the range that
	// motivates the exact-int work.
	var sawMin, sawMax bool
	for _, b := range pwBoundaryThresholds() {
		switch b.N {
		case math.MinInt64:
			sawMin = true
		case math.MaxInt64:
			sawMax = true
		}
	}
	if !sawMin || !sawMax {
		t.Fatalf("the threshold axis reaches math.MinInt64=%v and math.MaxInt64=%v; both are required",
			sawMin, sawMax)
	}
	// (3) And stock evaluates EXACTLY at math.MinInt64 — the one literal whose magnitude
	// has no positive i64, and therefore the likeliest place a parser silently loses a
	// value.
	minKey := pwDriveKey{Project: "bound_i64min", Func: pwBoundaryFn, Raw: strconv.FormatInt(math.MinInt64, 10)}
	minStock := pwBareChecked(t, minKey)
	if got := minStock.Checks["eq"]; got.Status != "succeeded" {
		t.Fatalf("stock says math.MinInt64 == math.MinInt64 is %q; the literal did not survive parsing",
			got.Status)
	}
	if got := minStock.Checks["lt"]; got.Status != "failed" {
		t.Fatalf("stock says math.MinInt64 < math.MinInt64 is %q", got.Status)
	}
}
