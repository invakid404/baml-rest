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
	// The native split is then non-vacuous in BOTH directions. (An `answered + refused ==
	// len(rows)` check would be arithmetic about a two-way branch, not a claim about the
	// matrix, so it is deliberately not written.)
	if refused == 0 {
		t.Fatal("native refused NOTHING across the whole boundary matrix; the 2^53 guard this slice " +
			"documents as 7.2c-2's blocker would then not exist, and the exact-int work would be " +
			"unmotivated — that contradiction must be resolved rather than passed over")
	}
	if answered == 0 {
		t.Fatal("native refused EVERY row; the matrix would prove nothing about where the guard's " +
			"edge actually is")
	}

	// THE FRONTIER — reported over NON-NEGATIVE literals only, and with the sign clause
	// stated separately.
	//
	// A single magnitude frontier over all thirteen thresholds would be a MISSTATEMENT,
	// not just an imprecision: `-1` and `+1` have the same magnitude and opposite
	// dispositions, because a negative literal is refused by the arithmetic-byte clause
	// whatever its size. "Answers up to 1 and refuses from 1 upward" is therefore not a
	// partition of anything. Restricting the frontier to non-negative literals makes the
	// magnitude claim well-formed; the sign clause is reported beside it as its own fact.
	var maxAnswered, minRefused uint64
	var sawAnswered, sawRefused bool
	negativeAnswered := 0
	for _, r := range rows {
		if r.threshold.N < 0 {
			if r.nativeErr == nil {
				negativeAnswered++
			}
			continue
		}
		mag := absInt64(r.threshold.N)
		if r.nativeErr == nil {
			if !sawAnswered || mag > maxAnswered {
				maxAnswered, sawAnswered = mag, true
			}
			continue
		}
		if !sawRefused || mag < minRefused {
			minRefused, sawRefused = mag, true
		}
	}
	if !sawAnswered || !sawRefused {
		t.Fatalf("the non-negative frontier is undefined: answered=%v refused=%v", sawAnswered, sawRefused)
	}
	if maxAnswered >= minRefused {
		t.Fatalf("the non-negative frontier is not ordered: native answers at magnitude %d and refuses "+
			"at %d, so the two clauses overlap and the log below would misstate the residual",
			maxAnswered, minRefused)
	}
	// The SIGN clause, as its own measured statement rather than folded into the number
	// above. Every negative threshold is refused at every value, so the magnitude frontier
	// simply does not apply on that side.
	if negativeAnswered != 0 {
		t.Errorf("native answered %d row(s) on a NEGATIVE literal; the recorded sign clause says every "+
			"negative literal is refused, so either the clause or this count is wrong", negativeAnswered)
	}
	t.Logf("  native's frontier over NON-NEGATIVE literals: it answers at threshold magnitudes up to "+
		"%d and refuses from %d upward. The axis samples no threshold between those two, so the exact "+
		"crossing is not measured here — the >15-digit clause places it at 10^15, BELOW 2^53.",
		maxAnswered, minRefused)
	t.Logf("  the SIGN clause is separate and absolute: all %d negative-literal rows are refused "+
		"regardless of magnitude, because `-` makes the whole expression arithmetic and `this ...` "+
		"never parses as the closed numeric sublanguage", pwNegativeLiteralRows(rows))
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

// TestAdmittedGreaterThanReachesPostClaimUnsupported records a hazard the boundary matrix
// exposes on the CURRENTLY ADMITTED predicate, not on a proposed one.
//
// `staticCheckedThreshold` admits any literal that round-trips through
// strconv.FormatInt — math.MinInt64 and math.MaxInt64 included — and the strict extractor
// can produce any i64 value. But the generic evaluator refuses most of that range. So a
// bundle the production gates admit TODAY can reach ErrConstraintUnsupported AFTER the
// admission decision, which is exactly the "no postclaim unsupported" rule the scope
// states and the totality blocker it hands to 7.2c-2.
//
// This test CHANGES NOTHING. It records how wide the gap is on the one admitted operator,
// so 7.2c-2 is sized against a measurement instead of an estimate, and so a later slice
// that closes the gap has a row to turn green.
func TestAdmittedGreaterThanReachesPostClaimUnsupported(t *testing.T) {
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
					examples = append(examples, fmt.Sprintf("%q over this=%d", expr, v))
				}
			}
		}
	}
	if admittedLiterals == 0 {
		t.Fatal("the production gates admitted NONE of the boundary literals on `this > I`; the " +
			"7.2b fingerprint is supposed to accept every canonical i64, so either the gates or this " +
			"expectation has changed")
	}
	t.Logf("RECORDED (7.2c-2 blocker, measured on the ADMITTED predicate): the gates admit "+
		"`this > I` for %d of %d boundary literals; of the %d (literal, value) pairs that follow, "+
		"the production evaluator REFUSES %d after the admission decision. Examples: %s",
		admittedLiterals, len(pwBoundaryThresholds()), total, reachable, strings.Join(examples, "; "))
	if reachable == 0 {
		t.Fatal("no admitted row reached an unsupported evaluator result, which would mean the " +
			"totality blocker the scope hands to 7.2c-2 does not exist; that contradiction must be " +
			"resolved rather than passed over")
	}
	// The hazard is POST-CLAIM by construction: the gate said yes before the evaluator
	// was ever consulted. Stating it as an assertion keeps the row from being read as a
	// pre-socket decline.
	worst := pwBundleFor(schema.ConstraintCheck, pwCheckedLabel, "this > "+strconv.FormatInt(math.MaxInt64, 10))
	if !pwAdmits(t, "this > math.MaxInt64", worst) {
		t.Fatal("`this > math.MaxInt64` is no longer admitted; the recorded hazard above is stale")
	}
	if _, err := debaml.EvaluateConstraint(debaml.IntValue(math.MaxInt64), "this > "+strconv.FormatInt(math.MaxInt64, 10)); err == nil {
		t.Fatal("the production evaluator now answers at math.MaxInt64; the recorded hazard above is stale")
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
// nobody has written down, and 7.2c-2 would be closing a gap it had not located. This is
// also the assertion that makes [pwRefusalReason] falsifiable rather than decorative.
func TestDirectIntBoundaryRefusalsAreAttributed(t *testing.T) {
	unattributed := 0
	for _, b := range pwBoundaryThresholds() {
		for _, v := range pwBoundaryValues(b.N) {
			for _, o := range pwOperators() {
				expr := fmt.Sprintf("this %s %s", o.Op, b.literal())
				_, err := debaml.EvaluateConstraint(debaml.IntValue(v), expr)
				if err == nil {
					continue
				}
				if reason := pwRefusalReason(b, v); strings.HasPrefix(reason, "UNATTRIBUTED") {
					unattributed++
					t.Errorf("native refused %q over this=%d for no documented reason: %v", expr, v, err)
				}
			}
		}
	}
	if unattributed != 0 {
		t.Errorf("%d refusal(s) are outside the documented numeric profile", unattributed)
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
