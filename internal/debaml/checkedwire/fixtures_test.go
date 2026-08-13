//go:build integration

package checkedwire

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

// The fixture table. Every declaration stock compiles, and every byte stock is given,
// is here.
//
// Nothing in this table is derived from native output. Each row's Raw is a fixed
// assistant string; what comes back is compared against a pinned literal in the byte
// tests.

// cwProjectSHA256 pins the rendered project. See TestCheckedWireProjectDrift.
const cwProjectSHA256 = "7a458c8d20c02f83f95fe81b8f47cfbcee4a13fd69fbfe369bf30124c3ae5a8a"

// The two cause-length probes. stock's validate_asserts measures the WHOLE cause —
// `Failed: ` + label + ' ' + expression — with Rust's String::len(), i.e. in BYTES,
// and truncates it to 100 before appending `...`. These two expressions are sized so
// the cause lands exactly ON the limit and exactly one byte OVER it.
//
// The label is one ASCII byte, so the fixed prefix is `Failed: t ` = 10 bytes and the
// expression carries the remaining 90 / 91.
//
// ASCII ON PURPOSE. Rust's String::truncate PANICS if the index is not on a UTF-8
// character boundary, and a panic inside the CFFI is a dead process rather than a
// failed test — so a cause whose 100th byte falls INSIDE a multi-byte character is
// deliberately not driven from here. That case is recorded as an unmeasured hazard in
// asymmetries.md rather than guessed at.
const cwCauseLabel = "t"

func cwCauseExpr(totalCauseLen int) string {
	const head = `this > 100 and "`
	const tail = `" != ""`
	prefix := len("Failed: ") + len(cwCauseLabel) + len(" ")
	pad := totalCauseLen - prefix - len(head) - len(tail)
	if pad < 1 {
		panic("checkedwire: cause length too small to pad")
	}
	return head + strings.Repeat("P", pad) + tail
}

// cwCauseText is the cause string stock must produce for an expression of this size,
// BEFORE truncation.
func cwCauseText(expr string) string {
	return "Failed: " + cwCauseLabel + " " + expr
}

var (
	cwExpr100 = cwCauseExpr(100)
	cwExpr101 = cwCauseExpr(101)
)

// cwManyAsserts renders n failing @assert attributes with distinct expressions, so
// the ORDER of the causes stock reports is observable rather than a coincidence of
// identical text.
func cwManyAsserts(n int) string {
	var b strings.Builder
	b.WriteString("int")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, " @assert(a%d, {{ this > %d }})", i, 100+i)
	}
	return b.String()
}

const (
	cwStaticCheckedAnswerClass = `class CW_StaticCheckedAnswer {
  answer string
  confidence int @check(positive, {{ this > 0 }})
}
`
	cwRequiredAssertClass = `class CW_RequiredAssert {
  v int @assert(gt, {{ this > 100 }})
}
`
	cwAliasedCheckedClass = `class CW_AliasedChecked {
  qty int @alias("amount") @check(positive, {{ this > 0 }})
}
`
	// De-BAML Slice 7.2b-2 — the ASSERT half of the two narrow production fixtures.
	// Structurally identical to CW_StaticCheckedAnswer (same field names, same order,
	// same types, same predicate) with `@check` swapped for `@assert`, so every byte
	// difference between the two families is attributable to the level and to nothing
	// else. The two outcomes are driven from the SAME declaration by two raw texts.
	cwStaticAssertAnswerClass = `class CW_StaticAssertAnswer {
  answer string
  confidence int @assert(positive, {{ this > 0 }})
}
`
)

var cwFixtures = []cwFixture{{
	Name:   "CheckPass",
	Doc:    "WIRE: one checked int whose predicate holds. Stock emits status \"succeeded\".",
	Target: `int @check(gt, {{ this > 0 }})`,
	Raw:    "5",
}, {
	Name:   "CheckFail",
	Doc:    "WIRE: the same shape with a FALSE check. The value is still emitted; only the status differs.",
	Target: `int @check(gt, {{ this > 100 }})`,
	Raw:    "5",
}, {
	Name:    "NestedCheck",
	Doc:     "WIRE: the nested class placement — the carrier sits on the `confidence` field.",
	Classes: []string{cwStaticCheckedAnswerClass},
	Target:  "CW_StaticCheckedAnswer",
	Raw:     `{"answer": "sunny", "confidence": 9}`,
}, {
	Name:    "NestedCheckFail",
	Doc:     "WIRE: the SAME nested class with a FALSE check. The value is still emitted, nested, with status \"failed\".",
	Classes: []string{cwStaticCheckedAnswerClass},
	Target:  "CW_StaticCheckedAnswer",
	Raw:     `{"answer": "sunny", "confidence": -1}`,
}, {
	Name:    "NestedCheckDuplicateKey",
	Doc:     "WIRE: the same nested class given `answer` TWICE. Stock keeps the FIRST occurrence, and the check still runs on the single `confidence`.",
	Classes: []string{cwStaticCheckedAnswerClass},
	Target:  "CW_StaticCheckedAnswer",
	Raw:     `{"answer": "first", "answer": "second", "confidence": 9}`,
}, {
	Name:    "NestedAssertPass",
	Doc:     "WIRE: the ASSERT twin of the nested class, predicate HOLDING — no wrapper, no check entry, an ordinary int field.",
	Classes: []string{cwStaticAssertAnswerClass},
	Target:  "CW_StaticAssertAnswer",
	Raw:     `{"answer": "sunny", "confidence": 9}`,
}, {
	Name:    "NestedAssertFail",
	Doc:     "ERROR BYTES: the same declaration with the predicate FALSE — no value at all, and the required-field wrapper chain around the two-field class.",
	Classes: []string{cwStaticAssertAnswerClass},
	Target:  "CW_StaticAssertAnswer",
	Raw:     `{"answer": "sunny", "confidence": -1}`,
}, {
	Name:   "CheckLessThan",
	Doc:    "WIRE: an expression carrying `<`, which encoding/json HTML-escapes and sonic does not.",
	Target: `int @check(lt, {{ this < 100 }})`,
	Raw:    "5",
}, {
	Name:   "AssertPassNoCheck",
	Doc:    "@assert IS NOT A CHECK: a PASSING assert creates no check entry, so the value stays a bare int.",
	Target: `int @assert(ok, {{ this > 0 }})`,
	Raw:    "5",
}, {
	Name:   "CheckPlusPassingAssert",
	Doc:    "@assert IS NOT A CHECK: a node carrying both keeps only the CHECK in the wrapper.",
	Target: `int @check(c, {{ this > 0 }}) @assert(a, {{ this > 0 }})`,
	Raw:    "5",
}, {
	Name:   "AssertFailLabelled",
	Doc:    "ERROR BYTES: one LABELLED failing assert — the `Failed: <label> <expr>` cause form.",
	Target: `int @assert(gt, {{ this > 100 }})`,
	Raw:    "5",
}, {
	Name:   "AssertFailUnlabelled",
	Doc:    "ERROR BYTES: one UNLABELLED failing assert — the same cause form with the label part empty.",
	Target: `int @assert({{ this > 100 }})`,
	Raw:    "5",
}, {
	Name:    "AssertFailRequiredField",
	Doc:     "ERROR BYTES: the same failure on a REQUIRED class field, which adds stock's wrapper chain.",
	Classes: []string{cwRequiredAssertClass},
	Target:  "CW_RequiredAssert",
	Raw:     `{"v": 5}`,
}, {
	Name:   "AssertFailFive",
	Doc:    "ERROR BYTES: exactly MAX_CAUSES (5) failures — the top reason must carry no truncation count.",
	Target: cwManyAsserts(5),
	Raw:    "5",
}, {
	Name:   "AssertFailSix",
	Doc:    "ERROR BYTES: six failures — the top reason gains `{total} (truncated to first 5)`.",
	Target: cwManyAsserts(6),
	Raw:    "5",
}, {
	Name:   "AssertFailCause100",
	Doc:    "ERROR BYTES: a cause of exactly 100 bytes — at the limit, so NOT truncated.",
	Target: `int @assert(` + cwCauseLabel + `, {{ ` + cwExpr100 + ` }})`,
	Raw:    "5",
}, {
	Name:   "AssertFailCause101",
	Doc:    "ERROR BYTES: a cause of exactly 101 bytes — one over, so truncated to 100 plus `...`.",
	Target: `int @assert(` + cwCauseLabel + `, {{ ` + cwExpr101 + ` }})`,
	Raw:    "5",
}, {
	Name:   "CauseExpr100Probe",
	Doc:    "The same expression as a @check, so the expression text BAML RETAINED is readable and the cause length above is measured rather than assumed.",
	Target: `int @check(` + cwCauseLabel + `, {{ ` + cwExpr100 + ` }})`,
	Raw:    "5",
}, {
	Name:   "CauseExpr101Probe",
	Doc:    "As above, for the one-byte-over expression.",
	Target: `int @check(` + cwCauseLabel + `, {{ ` + cwExpr101 + ` }})`,
	Raw:    "5",
}, {
	// De-BAML Slice 7.2b-3 — the EXPRESSION-TEXT normalisation probes.
	//
	// A `@check(l, {{ EXPR }})` attribute stores EXPR with the `{{`/`}}` delimiters
	// stripped but the PADDING kept: the descriptor a generated static method carries
	// for `{{ this > 0 }}` is literally " this > 0 " (one space each side). What stock
	// puts in Check.Expression is a DIFFERENT string, and these three rows measure the
	// relationship for zero, one and two spaces of padding instead of assuming it.
	//
	// The admitted fingerprint accepts only the paddings measured here.
	Name:   "ExprPadNone",
	Doc:    "EXPRESSION TEXT: `{{this > 0}}` — no padding at all.",
	Target: `int @check(pad0, {{this > 0}})`,
	Raw:    "5",
}, {
	Name:   "ExprPadOne",
	Doc:    "EXPRESSION TEXT: `{{ this > 0 }}` — the ordinary one-space form every fixture writes.",
	Target: `int @check(pad1, {{ this > 0 }})`,
	Raw:    "5",
}, {
	Name:   "ExprPadTwo",
	Doc:    "EXPRESSION TEXT: `{{  this > 0  }}` — two spaces each side, the padding the fingerprint DECLINES.",
	Target: `int @check(pad2, {{  this > 0  }})`,
	Raw:    "5",
}, {
	Name:   "EvaluatorError",
	Doc:    "ERROR BYTES: an expression that fails to EVALUATE. Stock reports `Failed to evaluate constraints:` and must NOT turn it into a failed check.",
	Target: `int @check(ev, {{ this|urlencode }})`,
	Raw:    "5",
}, {
	Name:   "DuplicateLabels",
	Doc:    "ASYMMETRY: two @check attributes under ONE label. The raw CFFI list keeps both; baml_go's map fold keeps one.",
	Target: `int @check(dup, {{ this > 0 }}) @check(dup, {{ this > 1 }})`,
	Raw:    "5",
}, {
	Name:   "BareStringAssertSkipped",
	Doc:    "ASYMMETRY: a bare top-level `string` return SKIPS its constraints — even a false assert.",
	Target: `string @assert(never, {{ this == "definitely-not-this" }})`,
	Raw:    "hello",
}, {
	Name:    "AliasIngress",
	Doc:     "ASYMMETRY: alias ingress produces CANONICAL output — `amount` arrives and stock emits `qty`. This scalar predicate is NOT a sequencing witness.",
	Classes: []string{cwAliasedCheckedClass},
	Target:  "CW_AliasedChecked",
	Raw:     `{"amount": 7}`,
}}

// ---------------------------------------------------------------------------
// Driving the stock CFFI.
// ---------------------------------------------------------------------------

// cwResult is one stock parse. Both halves are retained: a fixture is either a value
// capture or an error capture, and which one it is is part of what is pinned.
type cwResult struct {
	value any
	err   error
}

// cwCache memoizes one parse per function. No mutex: every subtest here is sequential
// (the CFFI runtime is process-global and this suite deliberately does not parallelise
// over it).
var cwCache = map[string]cwResult{}

// cwDrive runs one fixture through the stock CFFI.
//
// The progress marker goes to STDERR unbuffered before the call, so if a row takes the
// process down the last marker names the row that did it.
func cwDrive(t *testing.T, f cwFixture) cwResult {
	t.Helper()
	cwEnsureRuntime(t)
	if r, ok := cwCache[f.method()]; ok {
		return r
	}
	fmt.Fprintf(os.Stderr, "checked-wire: stock BEGIN %s\n", f.method())
	args := baml.BamlFunctionArguments{
		Kwargs: map[string]any{"text": f.Raw, "stream": false},
		Env:    cwEnv,
	}
	encoded, err := args.Encode()
	if err != nil {
		t.Fatalf("%s: encode arguments: %v", f.method(), err)
	}
	value, callErr := cwRt.CallFunctionParse(context.Background(), f.method(), encoded)
	fmt.Fprintf(os.Stderr, "checked-wire: stock END   %s\n", f.method())
	r := cwResult{value: value, err: callErr}
	cwCache[f.method()] = r
	return r
}

// cwFixtureNamed looks a fixture up by name, failing loudly on an unknown one so a
// renamed row cannot silently stop being driven.
func cwFixtureNamed(t *testing.T, name string) cwFixture {
	t.Helper()
	for _, f := range cwFixtures {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no fixture named %q", name)
	return cwFixture{}
}

// cwDriveAll drives every fixture once, so a whole-table guard sees every row.
func cwDriveAll(t *testing.T) {
	t.Helper()
	for _, f := range cwFixtures {
		cwDrive(t, f)
	}
}

// cwValue returns the decoded value for a fixture that must SUCCEED.
func cwValue(t *testing.T, name string) any {
	t.Helper()
	f := cwFixtureNamed(t, name)
	r := cwDrive(t, f)
	if r.err != nil {
		t.Fatalf("%s: stock parse failed, but this fixture is a VALUE capture: %v", f.method(), r.err)
	}
	return r.value
}

// cwError returns the UNMODIFIED error for a fixture that must FAIL.
func cwError(t *testing.T, name string) error {
	t.Helper()
	f := cwFixtureNamed(t, name)
	r := cwDrive(t, f)
	if r.err == nil {
		t.Fatalf("%s: stock parse SUCCEEDED (%#v), but this fixture is an ERROR capture", f.method(), r.value)
	}
	return r.err
}

// TestCheckedWireCauseLengthsAreExact pins the arithmetic the two truncation fixtures
// rest on, before any of stock's bytes are consulted. If the padding were off by one
// the whole 100-vs-101 claim would be about the wrong boundary.
func TestCheckedWireCauseLengthsAreExact(t *testing.T) {
	if got := len(cwCauseText(cwExpr100)); got != 100 {
		t.Fatalf("the 100-byte cause is %d bytes: %q", got, cwCauseText(cwExpr100))
	}
	if got := len(cwCauseText(cwExpr101)); got != 101 {
		t.Fatalf("the 101-byte cause is %d bytes: %q", got, cwCauseText(cwExpr101))
	}
	// ASCII, so byte length and rune count agree here; the note on cwCauseLabel
	// explains why the non-ASCII boundary is deliberately not driven.
	for _, e := range []string{cwExpr100, cwExpr101} {
		if len(e) != len([]rune(e)) {
			t.Fatalf("cause expression %q is not ASCII; its truncation boundary would be unmeasured", e)
		}
	}
}
