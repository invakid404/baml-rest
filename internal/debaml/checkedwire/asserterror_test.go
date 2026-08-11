//go:build integration

package checkedwire

import (
	"strconv"
	"strings"
	"testing"
)

// The ERROR half: the UNMODIFIED `err.Error()` bytes stock v0.223.0 hands back when an
// @assert fails, and when a constraint fails to EVALUATE.
//
// These are the acceptance oracle for the assertion renderer 7.2b-2 will write. They
// are deliberately NOT the #665 serving oracle's collapsed `reason` strings: that
// collapse folds newlines and normalises whitespace, which is exactly the information
// a byte-for-byte error claim has to preserve. What is pinned below is the whole
// string, escaping included.
//
// # The shape stock actually produces
//
// The CFFI hands back Rust's `{:?}` (Debug) rendering of a ParsingError, prefixed by
// `Failed to coerce value: `. Nested causes appear as a `causes: [...]` list, and every
// `reason` is a Debug-escaped Rust string — so a newline inside a reason appears as the
// two characters backslash-n, and an embedded quote as backslash-quote. That escaping
// is part of the bytes and is asserted rather than normalised away.

// The pinned stock error strings. Each is the exact `err.Error()` of the named fixture.
const (
	errAssertFailLabelled = `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: gt this > 100", causes: [] }] }`

	errAssertFailUnlabelled = `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: this > 100", causes: [] }] }`

	// The REQUIRED-FIELD wrapper chain: stock's required-fields summary, then the
	// per-field coercion error whose reason embeds the rendered assertion block
	// (complete with its escaped newline and two-space indent), then the assertion
	// error itself as a nested cause.
	errAssertFailRequiredField = `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field v: <root>: Assertions failed.\n  - <root>: Failed: gt this > 100", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: gt this > 100", causes: [] }] }] }] }`

	errAssertFailFive = `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: a1 this > 101", causes: [] }, ParsingError { scope: [], reason: "Failed: a2 this > 102", causes: [] }, ParsingError { scope: [], reason: "Failed: a3 this > 103", causes: [] }, ParsingError { scope: [], reason: "Failed: a4 this > 104", causes: [] }, ParsingError { scope: [], reason: "Failed: a5 this > 105", causes: [] }] }`

	errAssertFailSix = `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed. 6 (truncated to first 5)", causes: [ParsingError { scope: [], reason: "Failed: a1 this > 101", causes: [] }, ParsingError { scope: [], reason: "Failed: a2 this > 102", causes: [] }, ParsingError { scope: [], reason: "Failed: a3 this > 103", causes: [] }, ParsingError { scope: [], reason: "Failed: a4 this > 104", causes: [] }, ParsingError { scope: [], reason: "Failed: a5 this > 105", causes: [] }] }`

	errAssertFailCause100 = `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: t this > 100 and \"PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP\" != \"\"", causes: [] }] }`

	errAssertFailCause101 = `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: t this > 100 and \"PPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPPP\" != \"...", causes: [] }] }`

	// The EVALUATOR failure. A distinct form: `Failed to evaluate constraints:` with NO
	// causes, produced BEFORE validate_asserts runs.
	errEvaluatorError = `Failed to coerce value: ParsingError { scope: [], reason: "Failed to evaluate constraints: unknown filter: filter urlencode is unknown (in <string>:1)", causes: [] }`
)

// The two top-reason forms validate_asserts chooses between, and the per-cause prefix.
const (
	topReasonPlain     = "Assertions failed."
	topReasonTruncated = "Assertions failed. 6 (truncated to first 5)"
	causePrefix        = "Failed: "
	evaluatorPrefix    = "Failed to evaluate constraints: "
	// MAX_CAUSES / MAX_CAUSE_LEN, as v0.223's validate_asserts declares them.
	maxCauses   = 5
	maxCauseLen = 100
)

// cwDebugEscape mirrors Rust's `{:?}` escaping of a String for the characters these
// fixtures contain, so a cause can be located inside the pinned message by its own
// bytes rather than by a loose substring.
//
// It is not a general Rust escaper and does not pretend to be: the fixtures are ASCII
// with quotes and newlines, and TestStockAssertErrorCauseForm proves the composed
// result really does appear in stock's output.
func cwDebugEscape(s string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	).Replace(s)
}

// cwRequireErrorBytes asserts the fixture's err.Error() is EXACTLY want.
//
// strconv.Quote on failure so an invisible difference (a stray space, a real newline
// where an escaped one belongs) is readable in the output.
func cwRequireErrorBytes(t *testing.T, fixture, want string) string {
	t.Helper()
	got := cwError(t, fixture).Error()
	if got != want {
		t.Fatalf("%s: err.Error() bytes differ.\n got %s\nwant %s", fixture, strconv.Quote(got), strconv.Quote(want))
	}
	return got
}

// TestStockAssertErrorBytes pins every captured error string, whole.
//
// Nothing is normalised on the way in: the comparison is against `err.Error()` as the
// CFFI produced it.
func TestStockAssertErrorBytes(t *testing.T) {
	for _, tc := range []struct{ fixture, want string }{
		{"AssertFailLabelled", errAssertFailLabelled},
		{"AssertFailUnlabelled", errAssertFailUnlabelled},
		{"AssertFailRequiredField", errAssertFailRequiredField},
		{"AssertFailFive", errAssertFailFive},
		{"AssertFailSix", errAssertFailSix},
		{"AssertFailCause100", errAssertFailCause100},
		{"AssertFailCause101", errAssertFailCause101},
		{"EvaluatorError", errEvaluatorError},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			cwRequireErrorBytes(t, tc.fixture, tc.want)
		})
	}
}

// TestStockAssertErrorTopReason pins which of the two top-reason forms stock chooses,
// as a function of how many asserts failed.
//
// One and five failures both take the plain form; six switches to the counted one and
// still reports only MAX_CAUSES causes. That is the whole 1-5 vs >5 rule, measured.
func TestStockAssertErrorTopReason(t *testing.T) {
	countCauses := func(msg string) int { return strings.Count(msg, causePrefix) }

	one := cwRequireErrorBytes(t, "AssertFailLabelled", errAssertFailLabelled)
	if !strings.Contains(one, `reason: "`+topReasonPlain+`"`) {
		t.Errorf("a single failure did not use the plain top reason: %s", one)
	}
	if got := countCauses(one); got != 1 {
		t.Errorf("a single failure reported %d causes, want 1", got)
	}

	five := cwRequireErrorBytes(t, "AssertFailFive", errAssertFailFive)
	if !strings.Contains(five, `reason: "`+topReasonPlain+`"`) {
		t.Errorf("exactly MAX_CAUSES failures did not use the plain top reason: %s", five)
	}
	if strings.Contains(five, "truncated to first") {
		t.Errorf("exactly MAX_CAUSES failures reported a truncation count: %s", five)
	}
	if got := countCauses(five); got != maxCauses {
		t.Errorf("five failures reported %d causes, want %d", got, maxCauses)
	}

	six := cwRequireErrorBytes(t, "AssertFailSix", errAssertFailSix)
	if !strings.Contains(six, `reason: "`+topReasonTruncated+`"`) {
		t.Errorf("six failures did not use the counted top reason: %s", six)
	}
	if got := countCauses(six); got != maxCauses {
		t.Errorf("six failures reported %d causes, want %d (MAX_CAUSES)", got, maxCauses)
	}
	// The sixth assert is dropped entirely — not reported with a shorter body.
	if strings.Contains(six, "Failed: a6") {
		t.Errorf("the sixth cause survived truncation: %s", six)
	}
	if !strings.Contains(six, "Failed: a5 this > 105") {
		t.Errorf("the fifth cause is missing, so the cut is not at MAX_CAUSES: %s", six)
	}
	// The counted form names the TOTAL, not the retained count.
	if !strings.Contains(six, "failed. 6 (") {
		t.Errorf("the counted top reason does not name the total: %s", six)
	}
	if strings.Contains(six, "failed. 5 (") {
		t.Errorf("the counted top reason names the RETAINED count instead of the total: %s", six)
	}
}

// TestStockAssertErrorCauseForm pins the per-cause text: `Failed: ` + optional
// label + ' ' + expression, with the label part absent (and NO extra space) when the
// assert is unlabelled.
func TestStockAssertErrorCauseForm(t *testing.T) {
	labelled := cwRequireErrorBytes(t, "AssertFailLabelled", errAssertFailLabelled)
	if want := causePrefix + "gt " + "this > 100"; !strings.Contains(labelled, cwDebugEscape(want)) {
		t.Errorf("labelled cause %q not present in %s", want, labelled)
	}

	unlabelled := cwRequireErrorBytes(t, "AssertFailUnlabelled", errAssertFailUnlabelled)
	if want := causePrefix + "this > 100"; !strings.Contains(unlabelled, cwDebugEscape(want)) {
		t.Errorf("unlabelled cause %q not present in %s", want, unlabelled)
	}
	// The discriminating half: an unlabelled cause must NOT carry the extra space a
	// naive `label + " "` would leave behind.
	if strings.Contains(unlabelled, causePrefix+" ") {
		t.Errorf("the unlabelled cause carries a stray separator space: %s", unlabelled)
	}
	// And the two forms really are different, so neither test could pass on the other.
	if labelled == unlabelled {
		t.Fatal("the labelled and unlabelled captures are identical")
	}
}

// TestStockAssertErrorCauseOrder pins that causes appear in DECLARATION order.
//
// The five expressions are distinct, so this is an order claim over five
// distinguishable strings rather than over repeated text.
func TestStockAssertErrorCauseOrder(t *testing.T) {
	msg := cwRequireErrorBytes(t, "AssertFailFive", errAssertFailFive)
	prev := -1
	for i := 1; i <= maxCauses; i++ {
		want := cwDebugEscape(causePrefix + "a" + strconv.Itoa(i) + " this > " + strconv.Itoa(100+i))
		at := strings.Index(msg, want)
		if at < 0 {
			t.Fatalf("cause %q is absent from %s", want, msg)
		}
		if at <= prev {
			t.Fatalf("cause %d appears at %d, before the previous cause at %d; the causes are not in "+
				"declaration order:\n%s", i, at, prev, msg)
		}
		prev = at
	}
}

// TestStockAssertErrorCauseTruncation is the 100-byte rule, measured on both sides of
// the boundary.
//
// Rust's validate_asserts tests String::len() — BYTES — and truncates to 100 before
// appending `...`. The two fixtures are sized so one cause is exactly 100 bytes and the
// other exactly 101, which is the only pair that distinguishes 100 from 99 or 101. The
// expression each cause is built from is read back from stock's OWN retained
// Check.Expression on the twin @check probe, so the arithmetic rests on what BAML kept
// rather than on what the .baml source said.
func TestStockAssertErrorCauseTruncation(t *testing.T) {
	// The expression BAML retained, byte for byte.
	retained := func(fixture string) string {
		t.Helper()
		stock := cwStockChecked(t, fixture)
		if len(stock.Checks) != 1 {
			t.Fatalf("%s: probe reported %d checks, want 1", fixture, len(stock.Checks))
		}
		return stock.Checks[cwCauseLabel].Expression
	}

	expr100 := retained("CauseExpr100Probe")
	if expr100 != cwExpr100 {
		t.Fatalf("BAML retained a different expression than the source declared:\n got %q\nwant %q", expr100, cwExpr100)
	}
	expr101 := retained("CauseExpr101Probe")
	if expr101 != cwExpr101 {
		t.Fatalf("BAML retained a different expression than the source declared:\n got %q\nwant %q", expr101, cwExpr101)
	}

	cause100 := cwCauseText(expr100)
	cause101 := cwCauseText(expr101)
	if len(cause100) != maxCauseLen || len(cause101) != maxCauseLen+1 {
		t.Fatalf("the causes are %d and %d bytes, want %d and %d", len(cause100), len(cause101), maxCauseLen, maxCauseLen+1)
	}

	// AT the limit: kept whole, no ellipsis.
	at := cwRequireErrorBytes(t, "AssertFailCause100", errAssertFailCause100)
	if !strings.Contains(at, cwDebugEscape(cause100)) {
		t.Errorf("the 100-byte cause was not kept whole:\n%s", at)
	}
	if strings.Contains(at, cwDebugEscape(cause100)+"...") {
		t.Errorf("the 100-byte cause was truncated even though it is AT the limit:\n%s", at)
	}

	// ONE OVER: truncated to exactly 100 bytes, then `...`.
	over := cwRequireErrorBytes(t, "AssertFailCause101", errAssertFailCause101)
	if want := cwDebugEscape(cause101[:maxCauseLen]) + "..."; !strings.Contains(over, want) {
		t.Errorf("the 101-byte cause was not truncated at %d bytes:\n got %s\nwant a substring %s", maxCauseLen, over, want)
	}
	if strings.Contains(over, cwDebugEscape(cause101)) {
		t.Errorf("the 101-byte cause survived whole:\n%s", over)
	}
	// PROVEN TO BITE: the neighbouring cut points must NOT appear. Without these the
	// assertion above would also pass for a 99- or 101-byte truncation.
	for _, n := range []int{maxCauseLen - 1, maxCauseLen + 1} {
		if n > len(cause101) {
			continue
		}
		if wrong := cwDebugEscape(cause101[:n]) + "..."; strings.Contains(over, wrong) {
			t.Errorf("stock's output also matches a truncation at %d bytes, so the %d-byte claim is not "+
				"discriminating:\n%s", n, maxCauseLen, over)
		}
	}
	// The truncation drops the cause's final byte, which here is a closing quote — so
	// the escaped tail differs between the two captures. Stated so the pair cannot both
	// pass on identical text.
	if errAssertFailCause100 == errAssertFailCause101 {
		t.Fatal("the two truncation captures are identical")
	}
}

// TestStockEvaluatorErrorIsDistinct pins the evaluator failure as its OWN form.
//
// v0.223 formats `Failed to evaluate constraints: {e:?}` before validate_asserts runs.
// It must not be converted into a failed check (there is no checks map at all) and not
// into a friendly assertion error (the message never says "Assertions failed."). Both
// halves are asserted, and the fixture is a @check — the level that WOULD have produced
// a status had the predicate merely been false.
func TestStockEvaluatorErrorIsDistinct(t *testing.T) {
	msg := cwRequireErrorBytes(t, "EvaluatorError", errEvaluatorError)
	if !strings.Contains(msg, evaluatorPrefix) {
		t.Fatalf("the evaluator failure does not carry its own prefix: %s", msg)
	}
	if strings.Contains(msg, topReasonPlain) {
		t.Errorf("an evaluator failure was rendered as an assertion failure: %s", msg)
	}
	if strings.Contains(msg, causePrefix) {
		t.Errorf("an evaluator failure produced an assertion cause: %s", msg)
	}
	if !strings.Contains(msg, "causes: []") {
		t.Errorf("the evaluator failure reports causes: %s", msg)
	}
	// No value was produced, so it cannot have become a failed check.
	f := cwFixtureNamed(t, "EvaluatorError")
	if r := cwDrive(t, f); r.value != nil {
		t.Errorf("the evaluator failure ALSO produced a value (%#v), so it could be mistaken for a failed check", r.value)
	}
}

// TestStockAssertErrorWrapperChain pins the required-class-field wrapper stock adds
// around the very same assertion failure.
//
// The point of the row is that the inner bytes are unchanged while three outer layers
// appear: the required-fields summary, the per-field coercion reason (which embeds the
// rendered block, escaped newline and two-space indent included), and the assertion
// error itself as a nested cause.
func TestStockAssertErrorWrapperChain(t *testing.T) {
	wrapped := cwRequireErrorBytes(t, "AssertFailRequiredField", errAssertFailRequiredField)

	for _, want := range []string{
		`reason: "Failed while parsing required fields: missing=0, unparsed=1"`,
		`reason: "Failed to parse field v: <root>: ` + topReasonPlain,
		cwDebugEscape("\n  - <root>: " + causePrefix + "gt this > 100"),
		`reason: "` + topReasonPlain + `"`,
		cwDebugEscape(causePrefix + "gt this > 100"),
	} {
		if !strings.Contains(wrapped, want) {
			t.Errorf("the wrapper chain is missing %q:\n%s", want, wrapped)
		}
	}

	// The INNER assertion error is byte-identical to the unwrapped fixture's: the
	// wrapper adds layers, it does not rewrite the assertion.
	inner := strings.TrimPrefix(errAssertFailLabelled, "Failed to coerce value: ")
	if !strings.Contains(wrapped, inner) {
		t.Errorf("the wrapped chain does not contain the unwrapped assertion error verbatim.\n"+
			"  inner: %s\nwrapped: %s", inner, wrapped)
	}
	// And it really is a WRAPPER: the two captures differ.
	if wrapped == errAssertFailLabelled {
		t.Fatal("the wrapped and unwrapped captures are identical")
	}
	// The newline inside the reason is ESCAPED, not literal — that is the property a
	// collapsed reason string would have destroyed.
	if strings.Contains(wrapped, "\n") {
		t.Errorf("err.Error() carries a LITERAL newline; the ParsingError debug rendering escapes it:\n%q", wrapped)
	}
}

// TestStockAssertErrorPinsAreProvenToBite is the anti-false-green control for this
// file: each near-miss below must NOT equal the capture it mutates, or the
// corresponding assertion would pass on the wrong bytes.
func TestStockAssertErrorPinsAreProvenToBite(t *testing.T) {
	for _, m := range []struct{ name, from, mutant string }{{
		// What using the #665 collapsed `reason` chain as the acceptance assertion
		// would compare against: the same information, none of the bytes.
		name: "collapsed to the #665 reason string", from: errAssertFailLabelled,
		mutant: topReasonPlain + " / " + causePrefix + "gt this > 100",
	}, {
		name: "escaped newline turned into a real one", from: errAssertFailRequiredField,
		mutant: strings.ReplaceAll(errAssertFailRequiredField, cwDebugEscape("\n"), "\n"),
	}, {
		name: "cause prefix dropped", from: errAssertFailLabelled,
		mutant: strings.Replace(errAssertFailLabelled, causePrefix, "", 1),
	}, {
		name: "label separator doubled", from: errAssertFailLabelled,
		mutant: strings.Replace(errAssertFailLabelled, "gt this", "gt  this", 1),
	}, {
		name: "truncation cut one byte early", from: errAssertFailCause101,
		mutant: strings.Replace(errAssertFailCause101, `!= \"...`, `!= \...`, 1),
	}} {
		if m.mutant == m.from {
			t.Errorf("the %q mutant is identical to the capture it mutates, so it proves nothing", m.name)
		}
	}

	// The five- and six-failure captures differ ONLY in the top reason: putting the
	// counted form onto the five-failure message reproduces the six-failure one
	// exactly. That is what makes TestStockAssertErrorTopReason's top-reason assertion
	// the single thing separating the two rows, and it has to be exact.
	if got := strings.Replace(errAssertFailFive, topReasonPlain, topReasonTruncated, 1); got != errAssertFailSix {
		t.Fatalf("the five- and six-failure captures differ by more than the top reason; the truncation "+
			"claim is then not isolated:\n got %s\nwant %s", strconv.Quote(got), strconv.Quote(errAssertFailSix))
	}

	// Every pinned capture is distinct, so no fixture's assertion can be satisfied by
	// another fixture's bytes.
	pinned := map[string]string{
		"AssertFailLabelled":      errAssertFailLabelled,
		"AssertFailUnlabelled":    errAssertFailUnlabelled,
		"AssertFailRequiredField": errAssertFailRequiredField,
		"AssertFailFive":          errAssertFailFive,
		"AssertFailSix":           errAssertFailSix,
		"AssertFailCause100":      errAssertFailCause100,
		"AssertFailCause101":      errAssertFailCause101,
		"EvaluatorError":          errEvaluatorError,
	}
	seen := map[string]string{}
	for name, p := range pinned {
		if prev, dup := seen[p]; dup {
			t.Errorf("the %s and %s captures are identical", prev, name)
		}
		seen[p] = name
	}
	if len(pinned) != 8 {
		t.Fatalf("expected 8 pinned error captures, have %d", len(pinned))
	}
}
