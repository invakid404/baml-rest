package parity

import (
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestJSONSemEqual_NumberValueEquality pins that the structured comparison treats
// JSON numbers as scalar-by-VALUE (not lexical spelling) with no float64 precision
// loss: equivalent spellings compare equal, genuinely-distinct large integers
// beyond float64's mantissa stay unequal, object key order is insignificant, array
// order is significant, and bad/mismatched input returns false without panicking.
func TestJSONSemEqual_NumberValueEquality(t *testing.T) {
	equal := []struct{ a, b string }{
		{`{"x":1.50}`, `{"x":1.5}`},     // trailing zero
		{`{"x":1e2}`, `{"x":100}`},      // exponent vs integer
		{`{"x":100}`, `{"x":1e2}`},      // symmetric
		{`{"x":1.0}`, `{"x":1}`},        // decimal vs integer
		{`{"x":2.500}`, `{"x":2.5}`},    // trailing zeros
		{`[1.50, 2.500]`, `[1.5, 2.5]`}, // array of numbers, order preserved
		{`{"x":-0}`, `{"x":0}`},         // signed zero
		{`{"x":1.5e1}`, `{"x":15}`},     // fractional exponent
	}
	for _, tc := range equal {
		if !jsonSemEqual([]byte(tc.a), []byte(tc.b)) {
			t.Errorf("jsonSemEqual(%s, %s) = false, want true (numerically equal)", tc.a, tc.b)
		}
	}

	notEqual := []struct {
		name, a, b string
	}{
		{"different values", `{"x":1.5}`, `{"x":1.6}`},
		// Distinct integers beyond float64's 53-bit mantissa MUST stay unequal —
		// float64 coercion would collapse these consecutive values to the same
		// number; big.Rat keeps them exact.
		{"distinct large ints", `{"x":9007199254740993}`, `{"x":9007199254740992}`},
		{"small ints", `{"x":1}`, `{"x":2}`},
		{"number vs string", `{"x":1}`, `{"x":"1"}`},
		{"array order significant", `[1,2]`, `[2,1]`},
	}
	for _, tc := range notEqual {
		if jsonSemEqual([]byte(tc.a), []byte(tc.b)) {
			t.Errorf("%s: jsonSemEqual(%s, %s) = true, want false", tc.name, tc.a, tc.b)
		}
	}

	// Object key order is insignificant.
	if !jsonSemEqual([]byte(`{"a":1,"b":2}`), []byte(`{"b":2,"a":1}`)) {
		t.Error("object key order must be insignificant")
	}
	// Invalid JSON returns false (no panic).
	if jsonSemEqual([]byte(`{"x":1}`), []byte(`{not json`)) {
		t.Error("invalid JSON must compare unequal")
	}
}

// TestJSONNumberEqual_BoundedAgainstOversized pins the DoS hardening: a
// byte-identical oversized token compares equal via the fast path (no big.Rat
// blowup), a non-identical out-of-budget token fails closed as a mismatch (never
// parsed into a giant rational), and normal equivalent spellings + the
// distinct-large-integer guard still hold under exact big.Rat comparison.
func TestJSONNumberEqual_BoundedAgainstOversized(t *testing.T) {
	// Identical oversized token -> equal via the fast path (no parse/blowup).
	if !jsonNumberEqual(stdjson.Number("1e1000000"), stdjson.Number("1e1000000")) {
		t.Error("byte-identical oversized token must be equal via the fast path")
	}
	// Non-identical out-of-budget operands -> fail closed (mismatch), never parsed.
	if jsonNumberEqual(stdjson.Number("1e1000000"), stdjson.Number("1e2")) {
		t.Error("oversized non-identical token must fail closed (mismatch)")
	}
	if jsonNumberEqual(stdjson.Number("1e1000000"), stdjson.Number("1e2000000")) {
		t.Error("two distinct oversized tokens must fail closed (mismatch)")
	}
	// Giant mantissa (out of budget) non-identical -> fail closed.
	giantMantissa := "1" + strings.Repeat("0", 5000)
	if jsonNumberEqual(stdjson.Number(giantMantissa), stdjson.Number("1e5000")) {
		t.Error("giant-mantissa non-identical token must fail closed")
	}

	// Bounded exact comparison still holds.
	if !jsonNumberEqual(stdjson.Number("1.50"), stdjson.Number("1.5")) {
		t.Error("1.50 must equal 1.5")
	}
	if !jsonNumberEqual(stdjson.Number("1e2"), stdjson.Number("100")) {
		t.Error("1e2 must equal 100")
	}
	// Distinct large integers within budget stay unequal (guard intact).
	if jsonNumberEqual(stdjson.Number("9007199254740993"), stdjson.Number("9007199254740992")) {
		t.Error("distinct large integers within budget must stay unequal")
	}

	// numberWithinBudget direct coverage.
	if numberWithinBudget("1e1000000") {
		t.Error("1e1000000 must be out of budget")
	}
	if numberWithinBudget("1e99999999999999999999") {
		t.Error("overflowing exponent must be out of budget")
	}
	// Signed-min-int exponent bypass guard: the digit-by-digit capped accumulator
	// (no strconv.Atoi + signed negation, which overflows on math.MinInt64) must
	// reject |exponent| far beyond the cap.
	if numberWithinBudget("1e-9223372036854775808") {
		t.Error("signed-min-int exponent must be out of budget (accumulator, not Atoi+negation)")
	}
	if jsonNumberEqual(stdjson.Number("1e-9223372036854775808"), stdjson.Number("1e2")) {
		t.Error("signed-min-int exponent (nonidentical) must fail closed, never reach big.Rat")
	}
	if numberWithinBudget("1e+9223372036854775807") {
		t.Error("signed-max-int exponent must be out of budget")
	}
	if numberWithinBudget(giantMantissa) {
		t.Error("a 5001-digit mantissa must be out of budget")
	}
	for _, ok := range []string{"1e2", "1.50", "-1.5e1", "9007199254740993", "0", "-0"} {
		if !numberWithinBudget(ok) {
			t.Errorf("numberWithinBudget(%q) = false, want true (normal number)", ok)
		}
	}
}

// TestJSONNumberEqual_BadInput pins the false-on-unparseable contract directly,
// including the IDENTICAL-malformed-token case: the fast path validates the token
// before accepting it, so equal garbage is NOT equal.
func TestJSONNumberEqual_BadInput(t *testing.T) {
	if jsonNumberEqual("1", "not-a-number") {
		t.Error("unparseable number must return false")
	}
	if jsonNumberEqual("not-a-number", "1") {
		t.Error("unparseable number must return false")
	}
	// Identical malformed tokens: the fast path must NOT return true (it validates
	// well-formedness first), so this falls through to false-on-unparseable.
	if jsonNumberEqual("not-a-number", "not-a-number") {
		t.Error("identical malformed tokens must NOT be equal (fast path validates)")
	}
	if jsonNumberEqual("", "") {
		t.Error("identical empty tokens must NOT be equal")
	}
	if jsonNumberEqual("1.2.3", "1.2.3") {
		t.Error("identical malformed numeric-ish tokens must NOT be equal")
	}
	if !jsonNumberEqual("1.50", "1.5") {
		t.Error("1.50 must equal 1.5")
	}

	// validJSONNumber direct coverage.
	for _, ok := range []string{"0", "-0", "1", "-1", "1.5", "1e2", "-1.5e-10", "1E+3", "1e1000000", "9007199254740993"} {
		if !validJSONNumber(ok) {
			t.Errorf("validJSONNumber(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "-", "not-a-number", "1.", ".5", "1e", "1e+", "01", "1.2.3", "+1", "1 ", " 1", "0x1", "1/2"} {
		if validJSONNumber(bad) {
			t.Errorf("validJSONNumber(%q) = true, want false", bad)
		}
	}
}

// TestDecodeJSONNumber_RejectsTrailingContent pins the false-on-bad-input contract
// for a valid FIRST document followed by trailing content: a second JSON value or
// trailing non-whitespace garbage must NOT decode as success (only the first value
// would otherwise reach the comparator and could report a spurious match).
func TestDecodeJSONNumber_RejectsTrailingContent(t *testing.T) {
	for _, bad := range []string{
		`{"x":1}{"ignored":2}`,  // trailing JSON value
		`{"x":1} {"ignored":2}`, // trailing value after whitespace
		`{"x":1}[1,2]`,          // trailing array
		`{"x":1} garbage`,       // trailing non-whitespace garbage
		`1 2`,                   // two scalars
	} {
		if _, ok := decodeJSONNumber([]byte(bad)); ok {
			t.Errorf("decodeJSONNumber(%q) = ok, want false (trailing content is malformed)", bad)
		}
	}
	// A single document with only trailing WHITESPACE is still valid.
	if _, ok := decodeJSONNumber([]byte(`{"x":1}` + "\n\t ")); !ok {
		t.Error("trailing whitespace after a single document must remain valid")
	}
}

// TestJSONSemEqual_RejectsTrailingContent proves the trailing-content rejection
// flows through jsonSemEqual in BOTH operand directions, and that CompareStructured
// reports (structuredMatch=false, orderMatch=false) for such a malformed payload
// (an empty-root schema's InjectAbsentOptionals fast path preserves the raw bytes,
// so the malformed input reaches the comparator).
func TestJSONSemEqual_RejectsTrailingContent(t *testing.T) {
	const malformed = `{"x":1}{"ignored":2}`
	const clean = `{"x":1}`

	if jsonSemEqual([]byte(malformed), []byte(clean)) {
		t.Error("jsonSemEqual(malformed, clean) = true, want false")
	}
	if jsonSemEqual([]byte(clean), []byte(malformed)) {
		t.Error("jsonSemEqual(clean, malformed) = true, want false")
	}

	// CompareStructured on an empty-root schema (raw bytes preserved) must reject
	// the malformed payload on BOTH facets.
	emptyRoot := &bamlutils.DynamicOutputSchema{}
	sm, om := CompareStructured([]byte(malformed), []byte(clean), emptyRoot)
	if sm || om {
		t.Errorf("CompareStructured(malformed, clean, empty-root) = (structured=%v, order=%v), want (false, false)", sm, om)
	}
}
