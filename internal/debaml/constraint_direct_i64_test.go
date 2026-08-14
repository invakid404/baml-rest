package debaml

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// De-BAML Slice 7.2c-2 — the TOTALITY proof for the exact signed-i64 capability.
//
// This file is the front-loaded proof-integrity half of the slice. Its central
// claim is the one the 7.2c scope makes a hard precondition for any direct-int
// admission:
//
//	"An admitted schema must have a total, byte-proven native outcome for every
//	 value that native coercion can produce. If the evaluator may return
//	 unsupported for such a value, the schema declines before a native socket
//	 opens."
//
// So the matrix below drives all six direct operators over the full i64 range and
// requires, for EVERY row, a correct boolean and NEVER an error. Its expected
// answers come from math/big — not from the int64 comparison under test — so a
// wrong comparison cannot agree with itself. And the assertions are proven to BITE
// by re-driving the same matrix against three mutants that reproduce the exact
// defects this slice closes.
//
// AUTHORITY. Stock v0.223 CFFI, banked by Slice 7.2c-1 in
// internal/debaml/predicatewire: the six-operator wire/error captures, and the
// 13-threshold × 6-operator boundary matrix over which stock answered all 222 rows
// exactly — distinguishing 2^53 from 2^53+1 and evaluating correctly at MinInt64.
// Nothing here re-feeds native output to the CFFI; the captures are one-way.

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

// directI64ProofLiterals are the thresholds the matrix compares against.
//
// They are the thirteen the 7.2c scope names by value — zero, ±1, both sides of
// ±2^53, and both i64 endpoints with their inward neighbours — written out here
// independently of any production table, so the matrix cannot shrink to whatever
// the implementation happens to find easy.
func directI64ProofLiterals() []int64 {
	const exact = int64(1) << 53
	return []int64{
		0,
		1, -1,
		exact - 1, -(exact - 1),
		exact, -exact,
		exact + 1, -(exact + 1),
		math.MaxInt64 - 1, math.MaxInt64,
		math.MinInt64 + 1, math.MinInt64,
	}
}

// directI64ProofValues are the `this` values every literal is driven against, on
// top of that literal's own immediate neighbourhood.
//
// Zero, both signs, both endpoints and both sides of ±2^53 appear for EVERY
// literal, not only for the literal that happens to sit near them — so a
// capability that were exact only "near" its threshold would still be caught.
func directI64ProofValues() []int64 {
	const exact = int64(1) << 53
	return []int64{
		0, 1, -1,
		exact, exact + 1, -exact, -(exact + 1),
		math.MinInt64, math.MaxInt64,
	}
}

// directI64ProofRowCount is the MEASURED size of the matrix, pinned so a row set
// that quietly shrank would fail rather than pass smaller.
//
// It is 6 operators × 133 (literal, value) pairs — every literal driven against
// [directI64ProofValues] plus its own value and whichever of its two immediate
// neighbours exist inside i64 (MinInt64 and MaxInt64 each contribute one
// neighbour rather than two), deduplicated per literal.
const directI64ProofRowCount = 798

// directI64Drives returns the deduplicated, sorted `this` values one literal is
// driven against: the global set, the literal itself, and each in-range neighbour.
func directI64Drives(literal int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	add := func(v int64) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range directI64ProofValues() {
		add(v)
	}
	add(literal)
	if literal != math.MinInt64 {
		add(literal - 1)
	}
	if literal != math.MaxInt64 {
		add(literal + 1)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// directI64Want is the INDEPENDENT oracle for one row.
//
// It decides the comparison with math/big rather than with int64 operators, so it
// shares no arithmetic with [directCompareOp.Holds]. A sign error, a swapped
// operand order or a float64 rounding step in the implementation cannot be
// reproduced here and therefore cannot cancel out.
func directI64Want(t *testing.T, op directCompareOp, this, literal int64) bool {
	t.Helper()
	c := new(big.Int).SetInt64(this).Cmp(new(big.Int).SetInt64(literal))
	switch op.ID {
	case "gt":
		return c > 0
	case "ge":
		return c >= 0
	case "lt":
		return c < 0
	case "le":
		return c <= 0
	case "eq":
		return c == 0
	case "ne":
		return c != 0
	}
	t.Fatalf("operator %q (%s) has no independent oracle; the matrix would silently skip it", op.ID, op.Token)
	return false
}

// TestDirectI64CapabilityIsTotalAcrossTheWholeRange is THE proof of this slice.
//
// For every one of the six direct operators, every literal in
// [directI64ProofLiterals] and every value in that literal's drive set, it
// requires [EvaluateConstraint] — the PRODUCTION seam, not a helper — to return
//
//	err == nil   AND   the boolean math/big independently computes.
//
// The `err == nil` half is the one the 7.2c scope calls a blocker: an admitted
// direct-i64 node that could return ErrConstraintUnsupported after its schema was
// claimed is a post-claim decline on an open socket, and 7.2c-1 measured 31 such
// pairs on the ALREADY-SERVED `this > I` fingerprint. There are none now.
func TestDirectI64CapabilityIsTotalAcrossTheWholeRange(t *testing.T) {
	literals := directI64ProofLiterals()
	if len(literals) != 13 {
		t.Fatalf("the matrix drives %d literals, want the 13 the scope names", len(literals))
	}
	seenLiteral := map[int64]bool{}
	for _, l := range literals {
		if seenLiteral[l] {
			t.Fatalf("literal %d appears twice; the row count would be inflated by a duplicate", l)
		}
		seenLiteral[l] = true
	}
	for _, want := range []int64{0, math.MinInt64, math.MaxInt64, 1 << 53, -(1 << 53), (1 << 53) + 1, -((1 << 53) + 1)} {
		if !seenLiteral[want] {
			t.Fatalf("the literal set omits %d, which the scope names explicitly", want)
		}
	}

	ops := directCompareOperators()
	if len(ops) != 6 {
		t.Fatalf("the capability carries %d operators, want the scope's 6", len(ops))
	}

	rows := 0
	trueRows := map[string]int{}
	falseRows := map[string]int{}
	// The three clauses 7.2c-1 recorded as the direct-i64 totality gap. Each must
	// be exercised, or the matrix would be green without touching the defect.
	pastExact, negativeLiteral, longLiteral := 0, 0, 0

	for _, op := range ops {
		for _, literal := range literals {
			expr := directI64Expression(op, literal)
			for _, this := range directI64Drives(literal) {
				rows++
				got, err := EvaluateConstraint(IntValue(this), expr)
				if err != nil {
					t.Fatalf("%q over this=%d returned an ERROR (%v); the direct-i64 capability must be "+
						"TOTAL — an admitted node may never reach a post-claim unsupported", expr, this, err)
				}
				if want := directI64Want(t, op, this, literal); got != want {
					t.Fatalf("%q over this=%d = %v, want %v (math/big)", expr, this, got, want)
				}
				if got {
					trueRows[op.ID]++
				} else {
					falseRows[op.ID]++
				}
				if this >= 1<<53 || this <= -(1<<53) {
					pastExact++
				}
				if literal < 0 {
					negativeLiteral++
				}
				if len(strconv.FormatInt(literal, 10)) > 15 {
					longLiteral++
				}
			}
		}
	}

	if rows != directI64ProofRowCount {
		t.Fatalf("the matrix drove %d rows, want the pinned %d", rows, directI64ProofRowCount)
	}
	for _, op := range ops {
		if trueRows[op.ID] == 0 || falseRows[op.ID] == 0 {
			t.Errorf("operator %q produced %d true and %d false rows; both outcomes must be exercised "+
				"or the operator is only half proven", op.Token, trueRows[op.ID], falseRows[op.ID])
		}
	}
	if pastExact == 0 || negativeLiteral == 0 || longLiteral == 0 {
		t.Fatalf("the matrix touched %d rows past ±2^53, %d with a negative literal and %d with a "+
			">15-digit literal; those are the THREE clauses 7.2c-1 recorded as the gap, and a matrix "+
			"that misses one proves nothing about it", pastExact, negativeLiteral, longLiteral)
	}
	t.Logf("direct-i64 totality: %d thresholds x %d operators = %d rows, 0 unsupported "+
		"(%d rows past ±2^53, %d with a negative literal, %d with a >15-digit literal)",
		len(literals), len(ops), rows, pastExact, negativeLiteral, longLiteral)
}

// ---------------------------------------------------------------------------
// Proven to BITE
// ---------------------------------------------------------------------------

// directI64Mutant is a stand-in capability the bite proof drives the matrix
// against. Each mutant reproduces one real defect class.
type directI64Mutant struct {
	name string
	why  string
	// eval mirrors [evaluateDirectI64]'s signature plus an error, so a mutant can
	// model a REFUSAL as well as a wrong answer.
	eval func(this int64, op directCompareOp, literal int64) (bool, error)
}

// directI64Mutants are the three ways this capability could be wrong, written as
// code so the matrix's assertions can be shown to catch each one.
func directI64Mutants() []directI64Mutant {
	return []directI64Mutant{{
		name: "pre-7.2c-2 whitelist",
		why: "the three clauses 7.2c-1 measured — |this| >= 2^53, a literal longer than 15 digits, " +
			"and any negative literal (a `-` is an arithmetic byte) — refusing after the claim",
		eval: func(this int64, op directCompareOp, literal int64) (bool, error) {
			if this >= 1<<53 || this <= -(1<<53) ||
				len(strconv.FormatInt(literal, 10)) > 15 || literal < 0 {
				return false, unsupportedConstraint("outside the proven numeric profile")
			}
			return op.Holds(this, literal), nil
		},
	}, {
		name: "float64 comparator",
		why:  "comparing through float64, which conflates 2^53+1 with 2^53 and both i64 endpoints with 2^63",
		eval: func(this int64, op directCompareOp, literal int64) (bool, error) {
			a, b := float64(this), float64(literal)
			switch op.ID {
			case "gt":
				return a > b, nil
			case "ge":
				return a >= b, nil
			case "lt":
				return a < b, nil
			case "le":
				return a <= b, nil
			case "eq":
				return a == b, nil
			default:
				return a != b, nil
			}
		},
	}, {
		name: "swapped operands",
		why:  "reading the literal as the left operand, which is invisible to any symmetric drive set",
		eval: func(this int64, op directCompareOp, literal int64) (bool, error) {
			return op.Holds(literal, this), nil
		},
	}}
}

// TestDirectI64TotalityIsProvenToBite re-drives the SAME matrix against each
// mutant and requires every one to be reported.
//
// Without it, the totality test is a claim about code that might have been green
// for the wrong reason — most obviously the float64 mutant, which agrees with the
// exact comparator on all but a handful of the 798 rows.
func TestDirectI64TotalityIsProvenToBite(t *testing.T) {
	for _, m := range directI64Mutants() {
		t.Run(m.name, func(t *testing.T) {
			wrong, refused := 0, 0
			for _, op := range directCompareOperators() {
				for _, literal := range directI64ProofLiterals() {
					for _, this := range directI64Drives(literal) {
						got, err := m.eval(this, op, literal)
						if err != nil {
							refused++
							continue
						}
						if got != directI64Want(t, op, this, literal) {
							wrong++
						}
					}
				}
			}
			if wrong+refused == 0 {
				t.Fatalf("the %s mutant produced no wrong answer and no refusal across %d rows; "+
					"the totality matrix cannot be detecting %s", m.name, directI64ProofRowCount, m.why)
			}
			t.Logf("%s: %d wrong answer(s) + %d refusal(s) caught — %s", m.name, wrong, refused, m.why)
		})
	}
}

// ---------------------------------------------------------------------------
// Exact int64, not float64
// ---------------------------------------------------------------------------

// TestDirectI64ComparisonsAreExactInt64NotFloat drives the specific pairs where an
// exact comparator and a float64 one give DIFFERENT answers, and states the
// non-vacuity precondition for each: the two integers really are one float64.
//
// The values go through int64 VARIABLES rather than untyped constants, because the
// compiler folds a constant float64 conversion and would turn the precondition
// into dead code — the same trap constraint_state_test.go documents.
func TestDirectI64ComparisonsAreExactInt64NotFloat(t *testing.T) {
	rows := []struct {
		this, literal int64
		op            string
		want          bool
		float64Says   bool
	}{
		// 2^53+1 and 2^53 are the same float64. Wrong on EVERY architecture.
		{this: (1 << 53) + 1, literal: 1 << 53, op: "==", want: false, float64Says: true},
		{this: (1 << 53) + 1, literal: 1 << 53, op: "!=", want: true, float64Says: false},
		{this: (1 << 53) + 1, literal: 1 << 53, op: ">", want: true, float64Says: false},
		{this: (1 << 53) + 1, literal: 1 << 53, op: "<=", want: false, float64Says: true},
		// Both i64 endpoints round to 2^63, so their inward neighbours fuse with them.
		{this: math.MaxInt64, literal: math.MaxInt64 - 1, op: ">", want: true, float64Says: false},
		{this: math.MaxInt64, literal: math.MaxInt64 - 1, op: "==", want: false, float64Says: true},
		{this: math.MinInt64, literal: math.MinInt64 + 1, op: "<", want: true, float64Says: false},
		{this: math.MinInt64, literal: math.MinInt64 + 1, op: "==", want: false, float64Says: true},
		// The negative mirror of the 2^53 case.
		{this: -((1 << 53) + 1), literal: -(1 << 53), op: "<", want: true, float64Says: false},
	}
	for _, r := range rows {
		op, ok := directI64OpByToken(r.op)
		if !ok {
			t.Fatalf("the capability does not carry %q", r.op)
		}
		// NON-VACUITY. If these two stopped being one float64 the row would no
		// longer discriminate, and the whole point of it is that it does.
		a, b := r.this, r.literal
		if a == b {
			t.Fatalf("fixture is stale: %d and %d are the same integer", a, b)
		}
		if float64(a) != float64(b) {
			t.Fatalf("fixture is stale: %d and %d are no longer one float64, so this row no longer "+
				"distinguishes an exact comparator from a float64 one", a, b)
		}
		if r.want == r.float64Says {
			t.Fatalf("row %d %s %d claims the exact and float64 answers agree; it discriminates nothing",
				r.this, r.op, r.literal)
		}
		expr := directI64Expression(op, r.literal)
		got, err := EvaluateConstraint(IntValue(r.this), expr)
		if err != nil {
			t.Fatalf("%q over this=%d was REFUSED (%v)", expr, r.this, err)
		}
		if got != r.want {
			t.Errorf("%q over this=%d = %v, want %v (a float64 comparator would say %v)",
				expr, r.this, got, r.want, r.float64Says)
		}
	}
}

// TestDirectI64OperatorsAreNotSymmetric pins the ORIENTATION of every operator:
// `this` is the left operand and the literal the right.
//
// A drive set built only from symmetric pairs would pass with `<` and `>` swapped,
// which is why each operator is checked on an ASYMMETRIC pair here rather than
// left to the matrix.
func TestDirectI64OperatorsAreNotSymmetric(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  bool
	}{
		{">", true}, {">=", true}, {"<", false}, {"<=", false}, {"==", false}, {"!=", true},
	} {
		op, ok := directI64OpByToken(tc.token)
		if !ok {
			t.Fatalf("the capability does not carry %q", tc.token)
		}
		// this = 5, literal = 3: every operator's answer differs from its answer
		// with the operands swapped, except the two symmetric ones (== and !=),
		// which are checked for value instead.
		expr := directI64Expression(op, 3)
		got, err := EvaluateConstraint(IntValue(5), expr)
		if err != nil {
			t.Fatalf("%q was refused: %v", expr, err)
		}
		if got != tc.want {
			t.Errorf("%q over this=5 = %v, want %v", expr, got, tc.want)
		}
		if swapped := op.Holds(3, 5); tc.token != "==" && tc.token != "!=" && swapped == got {
			t.Errorf("%q gives the same answer with the operands swapped; the orientation is untested",
				tc.token)
		}
	}
}

// directI64OpByToken looks one operator up in the capability table.
func directI64OpByToken(token string) (directCompareOp, bool) {
	for _, op := range directCompareOperators() {
		if op.Token == token {
			return op, true
		}
	}
	return directCompareOp{}, false
}

// ---------------------------------------------------------------------------
// The grammar is CLOSED
// ---------------------------------------------------------------------------

// TestDirectI64GrammarIsClosed pins every spelling the exact path must NOT claim.
//
// The stake is higher than tidiness. The exact path answers where the generic
// evaluator refuses, so anything it claims by mistake is a boolean served without
// the fail-closed profile ever having looked at it. Every row below must leave
// `decided == false`, i.e. must fall through to the generic evaluator untouched.
func TestDirectI64GrammarIsClosed(t *testing.T) {
	// POSITIVE CONTROL FIRST: if the grammar matched nothing at all, every negative
	// below would pass for the wrong reason.
	if held, decided := evaluateDirectI64(IntValue(1), "this > 0"); !decided || !held {
		t.Fatalf("the canonical form `this > 0` was not decided (held=%v decided=%v); every negative "+
			"row below would then be vacuous", held, decided)
	}

	for _, expr := range []string{
		// SPACING — exactly one ASCII space on each side of the operator.
		"this>0", "this >0", "this> 0", "this  > 0", "this >  0",
		" this > 0", "this > 0 ", "this\t> 0", "this >\t0", "this >\n0", "this > 0\n",
		// SUBJECT.
		"", "this", "This > 0", "THIS > 0", "that > 0", "this.x > 0", "this|abs > 0",
		"0 > this", "thisx > 0",
		// OPERATOR.
		"this = 0", "this === 0", "this <> 0", "this ! = 0", "this >> 0", "this ~ 0",
		"this >= ", "this > ",
		// LITERAL — every non-canonical spelling, including the four 7.2c-1 measured
		// as COMPILING under stock (so BAML would not reject them upstream).
		"this > +5", "this > 007", "this > -0", "this > 1_000", "this > 5.0",
		"this > 0x10", "this > 1e3", "this > 9223372036854775808",
		"this > -9223372036854775809", "this > 0.0", "this >  ", "this > x",
		// COMPOUND / TRAILING — a fragment of the grammar is not the grammar.
		"this > 0 and this < 9", "this > 0 or this < 9", "this > 0;", "this > 0 == true",
		"(this > 0)", "this > 0 if true else false",
	} {
		if held, decided := evaluateDirectI64(IntValue(1), expr); decided {
			t.Errorf("the exact path CLAIMED %q (as %v); only the closed direct grammar may bypass "+
				"the fail-closed profile", expr, held)
		}
	}

	// A NON-INT `this` is not this grammar either, whatever the expression says.
	for _, v := range []ConstraintValue{
		NullValue(), BoolValue(true), FloatValue(1), FloatValue(2.5), StringValue("1"),
		ListValue([]ConstraintValue{IntValue(1)}),
		MapValue([]ConstraintEntry{{Key: "a", Value: IntValue(1)}}),
		ClassValue("C", []ConstraintEntry{{Key: "a", Value: IntValue(1)}}),
	} {
		if held, decided := evaluateDirectI64(v, "this > 0"); decided {
			t.Errorf("the exact path claimed `this > 0` over a %v value (as %v); it is keyed on "+
				"BamlValue::Int and must route everything else to the generic evaluator", v.Kind(), held)
		}
	}
}

// TestDirectI64ManifestIsUnambiguous proves the shared-prefix operators cannot both
// match one expression, so [parseDirectI64Comparison]'s single-pass loop cannot
// commit to the wrong one.
//
// It is driven over EVERY canonical expression the whole capability can produce at
// a spread of literals, and requires exactly one operator's head to match each.
func TestDirectI64ManifestIsUnambiguous(t *testing.T) {
	ops := directCompareOperators()
	seen := map[string]string{}
	for _, op := range ops {
		if prev, dup := seen[op.Token]; dup {
			t.Fatalf("token %q is carried by both %q and %q", op.Token, prev, op.ID)
		}
		seen[op.Token] = op.ID
		if prev, dup := seen["id:"+op.ID]; dup {
			t.Fatalf("id %q is carried twice (%q)", op.ID, prev)
		}
		seen["id:"+op.ID] = op.Token
	}

	for _, literal := range []int64{0, 1, -1, math.MinInt64, math.MaxInt64} {
		for _, op := range ops {
			expr := directI64Expression(op, literal)
			var matched []string
			for _, cand := range ops {
				if cmp, ok := parseDirectI64Comparison(expr, []directCompareOp{cand}); ok {
					matched = append(matched, cand.ID)
					if cmp.literal != literal {
						t.Errorf("%q parsed to literal %d, want %d", expr, cmp.literal, literal)
					}
				}
			}
			if len(matched) != 1 || matched[0] != op.ID {
				t.Errorf("%q matched %v; exactly one operator (%s) must", expr, matched, op.ID)
			}
		}
	}

	// The specific hazard: a one-character token must not swallow the head of its
	// two-character sibling.
	for _, pair := range [][2]string{{">", ">="}, {"<", "<="}} {
		short, _ := directI64OpByToken(pair[0])
		expr := directI64Expression(mustOpByToken(t, pair[1]), 0)
		if _, ok := parseDirectI64Comparison(expr, []directCompareOp{short}); ok {
			t.Errorf("%q was matched by the %q operator; the separator must make the heads disjoint",
				expr, pair[0])
		}
	}
}

func mustOpByToken(t *testing.T, token string) directCompareOp {
	t.Helper()
	op, ok := directI64OpByToken(token)
	if !ok {
		t.Fatalf("the capability does not carry %q", token)
	}
	return op
}

// TestDirectI64ExpressionRoundTrips drives the renderer and the parser against each
// other over the whole operator set and the boundary literals, so a change to
// either spelling is caught rather than absorbed.
func TestDirectI64ExpressionRoundTrips(t *testing.T) {
	ops := directCompareOperators()
	for _, op := range ops {
		for _, literal := range append(directI64ProofLiterals(), 42, -42, 1000) {
			expr := directI64Expression(op, literal)
			// The rendered text is exactly what stock retains: `this`, one space,
			// the operator, one space, the canonical literal.
			if want := "this " + op.Token + " " + strconv.FormatInt(literal, 10); expr != want {
				t.Fatalf("directI64Expression rendered %q, want %q", expr, want)
			}
			cmp, ok := parseDirectI64Comparison(expr, ops)
			if !ok {
				t.Fatalf("%q did not round-trip through the parser", expr)
			}
			if cmp.op.ID != op.ID || cmp.literal != literal {
				t.Fatalf("%q parsed to (%s, %d), want (%s, %d)", expr, cmp.op.ID, cmp.literal, op.ID, literal)
			}
		}
	}
}

// TestDirectI64LongestLiteralIsTheMinimum measures the claim the assert-cause bound
// rests on: math.MinInt64 has the longest canonical i64 text.
//
// It is measured rather than asserted in a comment because getting it wrong makes
// every derived length bound one byte too generous, which is exactly the class of
// defect the 7.2c scope flags for `>=`/`<=`.
func TestDirectI64LongestLiteralIsTheMinimum(t *testing.T) {
	longest := len(strconv.FormatInt(math.MinInt64, 10))
	if longest != 20 {
		t.Fatalf("MinInt64 formats to %d bytes, want 20", longest)
	}
	for _, v := range []int64{
		0, 1, -1, 9, -9, math.MaxInt64, math.MaxInt64 - 1, math.MinInt64 + 1,
		1 << 53, -(1 << 53), 1e15, -1e15, 999999999999999999, -999999999999999999,
	} {
		if got := len(strconv.FormatInt(v, 10)); got > longest {
			t.Fatalf("%d formats to %d bytes, longer than MinInt64's %d", v, got, longest)
		}
	}
	// And the derived expression really is built from it.
	full := directI64LongestExpression(directCompareOperators())
	if !strings.HasSuffix(full, strconv.FormatInt(math.MinInt64, 10)) {
		t.Fatalf("the longest capability expression is %q, which does not end in MinInt64", full)
	}
	// The two-character operators are what make it longer than the `>`-only form.
	gtOnly := directI64LongestExpression([]directCompareOp{mustOpByToken(t, ">")})
	if len(full) != len(gtOnly)+1 {
		t.Fatalf("the six-operator longest expression is %d bytes and the `>`-only one %d; "+
			"the two-character operators must add exactly one byte", len(full), len(gtOnly))
	}
	// FAIL-CLOSED: an empty manifest must not yield an empty (and therefore
	// maximally permissive) bound.
	if empty := directI64LongestExpression(nil); empty != full {
		t.Fatalf("an empty manifest derived %q; it must fall back to the whole capability (%q) so a "+
			"length bound derived from it is the TIGHTEST, not the loosest", empty, full)
	}
}

// TestDirectCompareManifestResolvesOrFailsClosed pins the manifest resolver's two
// refusal modes, both of which would otherwise silently produce a SHORTER manifest
// than the caller asked for.
func TestDirectCompareManifestResolvesOrFailsClosed(t *testing.T) {
	ops, ok := directCompareManifest([]string{">", "=="})
	if !ok || len(ops) != 2 {
		t.Fatalf("a two-token manifest resolved to %v (ok=%v)", ops, ok)
	}
	// The capability's order is preserved, not the request's.
	if ops[0].Token != ">" || ops[1].Token != "==" {
		t.Errorf("manifest order = %q/%q, want the capability's own order", ops[0].Token, ops[1].Token)
	}
	if _, ok := directCompareManifest([]string{">", "<=>"}); ok {
		t.Error("a manifest naming an operator the capability cannot decide was RESOLVED; it must fail closed")
	}
	if _, ok := directCompareManifest([]string{">", ">"}); ok {
		t.Error("a manifest with a duplicate token was resolved")
	}
	all, ok := directCompareManifest([]string{">", ">=", "<", "<=", "==", "!="})
	if !ok || len(all) != 6 {
		t.Fatalf("the full six-token manifest resolved to %d operator(s) (ok=%v)", len(all), ok)
	}
	if _, ok := directCompareManifest(nil); !ok {
		t.Error("an empty manifest must resolve (to nothing), not error")
	}
}

// ---------------------------------------------------------------------------
// Scoped exactness — the generic guard is UNCHANGED
// ---------------------------------------------------------------------------

// TestDirectI64ExactnessIsScoped is the other half of the slice's contract: the
// exactness applies ONLY to the closed direct grammar, and every other generic
// expression keeps the existing fail-closed profile — the `|int| >= 2^53` refusal
// included.
//
// It is deliberately driven at values and literals the exact path DOES decide, so
// a guard that had been loosened by magnitude rather than narrowed by grammar
// would be caught here rather than looking safe.
func TestDirectI64ExactnessIsScoped(t *testing.T) {
	oversized := IntValue((1 << 53) + 1)

	// (a) GENERIC EXPRESSIONS over an oversized value: still refused.
	for _, expr := range []string{
		"this|abs == 9007199254740993",
		"this|abs > 0",
		"this + 0 == 9007199254740993",
		"this - 1 == 9007199254740992",
		"this is odd",
		"this is defined",
		"[this]|length == 1",
		"this|string == \"9007199254740993\"",
	} {
		if got, err := EvaluateConstraint(oversized, expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q over 2^53+1 answered %v (err=%v); the GENERIC guard must be unchanged",
				expr, got, err)
		}
	}

	// (b) The generic numeric whitelist itself, driven directly: unchanged.
	if !exceedsExactIntegerRange(oversized, "this|abs > 0") {
		t.Error("exceedsExactIntegerRange no longer refuses an oversized value-model integer")
	}
	if !exceedsExactIntegerRange(IntValue(1), "9007199254740993 == 9007199254740992") {
		t.Error("exceedsExactIntegerRange no longer refuses an oversized literal")
	}
	if !exceedsExactIntegerRange(IntValue(1), "this > -1") {
		t.Error("exceedsExactIntegerRange no longer treats a negative literal as an arithmetic byte; " +
			"the exact path must BYPASS this guard, never relax it")
	}
	if !exceedsExactIntegerRange(IntValue(1), "this > 9007199254740991") {
		t.Error("exceedsExactIntegerRange no longer refuses a 16-digit literal")
	}

	// (c) COMPOUND and non-comparison forms over an ordinary value: still refused,
	// so the direct grammar has not become a foothold for a wider one.
	for _, expr := range []string{
		"this > 0 and this < 10",
		"this > 0 or this < 10",
		"not (this > 0)",
		"this > 0 if true else false",
		"this in [1, 2]",
		"this ~ \"x\" == \"1x\"",
	} {
		if got, err := EvaluateConstraint(IntValue(1), expr); !errors.Is(err, ErrConstraintUnsupported) {
			t.Errorf("%q answered %v (err=%v); compound predicates stay #583 declined", expr, got, err)
		}
	}

	// (d) The rendered path is untouched: RenderConstraintExpression still refuses
	// what it always refused, including the direct grammar at an oversized value.
	// EvaluateConstraint answers it; the RENDERER does not, which is what makes the
	// exact path a bypass rather than a change to the profile.
	if _, err := RenderConstraintExpression(oversized, "this > 0"); !errors.Is(err, ErrConstraintUnsupported) {
		t.Errorf("RenderConstraintExpression now answers `this > 0` over 2^53+1; the generic renderer "+
			"must be unchanged (err=%v)", err)
	}
	if got, err := EvaluateConstraint(oversized, "this > 0"); err != nil || !got {
		t.Errorf("EvaluateConstraint refused `this > 0` over 2^53+1 (got=%v err=%v); the exact path "+
			"is what closes the post-claim hazard", got, err)
	}
}

// TestDirectI64ClosesTheRecordedPostClaimHazard drives the exact rows Slice 7.2c-1
// recorded as reachable on the ALREADY-SERVED `this > I` fingerprint, and requires
// every one of them to be decided now.
//
// 7.2c-1's report: "the gates admit `this > I` for 13 of 13 boundary literals; of
// the 37 (literal, value) pairs that follow, the production evaluator REFUSES 31
// after the admission decision. Examples: `this > -1` over this=-2; `this > -1`
// over this=-1; `this > -1` over this=0."
//
// This test drives a SUPERSET of that recording against the production gates —
// the same 13 admitted literals, each against its own neighbourhood plus the
// global value set, which is 133 pairs rather than 37. Every literal must still be
// ADMITTED by the unchanged fingerprint (a narrower fingerprint would make the
// measurement vacuous, so that is asserted first), and every pair must now
// evaluate. Zero post-claim refusals.
func TestDirectI64ClosesTheRecordedPostClaimHazard(t *testing.T) {
	gt := mustOpByToken(t, ">")
	pairs, refused, admitted := 0, 0, 0
	for _, literal := range directI64ProofLiterals() {
		expr := directI64Expression(gt, literal)
		// The SHIPPED fingerprint, unchanged, still admits this literal.
		if _, ok := staticCheckedThreshold(expr); !ok {
			t.Fatalf("the production fingerprint no longer admits %q; the hazard this test measures "+
				"was recorded ON the admitted set, so a narrower set would make it vacuous", expr)
		}
		admitted++
		for _, this := range directI64Drives(literal) {
			pairs++
			if _, err := EvaluateConstraint(IntValue(this), expr); err != nil {
				refused++
				t.Errorf("POST-CLAIM REFUSAL: %q over this=%d returned %v; an admitted direct-i64 row "+
					"may never reach ErrConstraintUnsupported", expr, this, err)
			}
		}
	}
	if admitted != 13 {
		t.Fatalf("%d of 13 boundary literals are admitted by `this > I`", admitted)
	}
	if refused != 0 {
		t.Fatalf("%d of %d admitted (literal, value) pairs still refuse after the claim", refused, pairs)
	}
	t.Logf("post-claim hazard CLOSED: %d literals admitted by the shipped `this > I` fingerprint, "+
		"%d (literal, value) pairs driven, 0 post-claim refusals (7.2c-1 recorded 31 of 37)",
		admitted, pairs)
}

// ---------------------------------------------------------------------------
// Non-vacuity of the whole file
// ---------------------------------------------------------------------------

// TestDirectI64ProofSurfaceIsComplete requires every operator in the capability to
// appear in every proof dimension this file claims to cover, so an operator added
// later cannot inherit a green matrix it was never driven through.
func TestDirectI64ProofSurfaceIsComplete(t *testing.T) {
	want := map[string]string{"gt": ">", "ge": ">=", "lt": "<", "le": "<=", "eq": "==", "ne": "!="}
	got := map[string]string{}
	for _, op := range directCompareOperators() {
		got[op.ID] = op.Token
		if op.Holds == nil {
			t.Fatalf("operator %q carries no decision procedure", op.ID)
		}
	}
	if fmt.Sprint(want) != fmt.Sprint(got) {
		t.Fatalf("the capability is %v, want the scope's exact set %v", got, want)
	}
	// The independent oracle must know all six, or the matrix would t.Fatal partway
	// through instead of covering them.
	for _, op := range directCompareOperators() {
		_ = directI64Want(t, op, 1, 0)
	}
}
