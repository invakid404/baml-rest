//go:build integration

package predicatewire

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"
)

// The EXPRESSION-TEXT half: what stock RETAINS in Check.Expression for each canonical
// predicate under source padding, and what it does with the non-canonical integer
// spellings the fingerprint rejects.
//
// Both questions are about the string BAML keeps, which is the string a `Check` carries
// onto the wire and the string an assertion cause quotes. The 7.2c grammar's canonical
// inner text is only meaningful if stock produces it, and only bounded if the spellings
// outside it are measured rather than assumed.

// pwPadDriveRaw is the assistant text every padding probe is driven with: a bare int,
// because these probes sit on a bare `int` target.
const pwPadDriveRaw = "5"

// pwPadDriveValue is that text as an integer, so the pinned bytes can carry it.
const pwPadDriveValue = 5

// pwPadStatus is the status stock must report for each operator at `this = 5` against
// the canonical literal `0`.
//
// It is DATA, deliberately, not a Go comparison: computing it would re-derive the answer
// with the same arithmetic the capture exists to check, and a table that agreed with a
// wrong engine would be green.
var pwPadStatus = map[string]string{
	"gt": "succeeded", // 5 > 0
	"ge": "succeeded", // 5 >= 0
	"lt": "failed",    // 5 < 0
	"le": "failed",    // 5 <= 0
	"eq": "failed",    // 5 == 0
	"ne": "succeeded", // 5 != 0
}

// pwBareWireBytes is the whole wire form of a bare checked int, assembled from the three
// independent facts a padding probe is about: the label, the expression stock RETAINED,
// and the status.
//
// Building the expectation rather than pinning fourteen near-identical literals is what
// makes the assertion discriminating: stock is handed the PADDED source and the expected
// bytes are built from the UNPADDED canonical text, so a stock that kept the padding
// fails here. [TestStockPaddingIsStrippedForEveryOperator] additionally guards that no
// expectation it builds carries padding at all.
func pwBareWireBytes(label, expression, status string) string {
	return fmt.Sprintf(`{"value":%d,"checks":{%q:{"name":%q,"expression":%q,"status":%q}}}`,
		pwPadDriveValue, label, label, expression, status)
}

// TestStockPaddingIsStrippedForEveryOperator measures what stock retains for zero, one
// and two ASCII spaces of source padding, for all six operators.
//
// checkedwire established this for the ONE-byte `>`; the open question 7.2c raises is
// whether a TWO-byte operator behaves the same, because `>=` is the first admitted
// expression that is longer than the one the current cause-length ceiling was computed
// from. The pad-2 row is measured, NOT admitted: the production fingerprint still allows
// at most one space, which internal/debaml's sibling corpus pins as a decline.
func TestStockPaddingIsStrippedForEveryOperator(t *testing.T) {
	pads := map[int]int{}
	for _, probe := range pwPadProbes() {
		t.Run(probe.Label, func(t *testing.T) {
			status, ok := pwPadStatus[probe.Op.ID]
			if !ok {
				t.Fatalf("no pinned status for operator %q", probe.Op.ID)
			}
			key := pwDriveKey{Project: pwExprTextKey, Func: "Pad_" + probe.Label, Raw: pwPadDriveRaw}
			stock := pwBareChecked(t, key)

			// (1) Check.Expression DIRECTLY. The wire comparison alone could pass on a
			// carrier that dropped the field entirely.
			got, ok := stock.Checks[probe.Label]
			if !ok {
				t.Fatalf("stock reported no check under %q: %v", probe.Label, stock.Checks)
			}
			want := shared.Check{Name: probe.Label, Expression: probe.canonical(), Status: status}
			if got != want {
				t.Fatalf("stock check = %+v, want %+v\n(the source it was given was %q)",
					got, want, probe.source())
			}

			// (2) The WIRE, built from the UNPADDED canonical text.
			pwRequireSonicBytes(t, "stock", stock,
				pwBareWireBytes(probe.Label, probe.canonical(), status))

			pads[probe.Pad]++
		})
	}

	// DISCRIMINATING: a padded source must actually differ from its canonical form, or
	// "stock strips the padding" would be unfalsifiable for the pad-0 rows.
	padded := 0
	for _, probe := range pwPadProbes() {
		if probe.source() != probe.canonical() {
			padded++
			if !strings.HasPrefix(probe.source(), " ") || !strings.HasSuffix(probe.source(), " ") {
				t.Errorf("%s claims %d spaces of padding but its source is %q",
					probe.Label, probe.Pad, probe.source())
			}
		}
	}
	if padded == 0 {
		t.Fatal("no probe carries padding at all, so this file measures nothing")
	}
	// And every expectation this test built must be free of padding, or a stock that
	// KEPT the padding could still have matched.
	for _, probe := range pwPadProbes() {
		w := pwBareWireBytes(probe.Label, probe.canonical(), pwPadStatus[probe.Op.ID])
		if strings.Contains(w, `"expression":" `) || strings.Contains(w, ` ","status"`) {
			t.Fatalf("a constructed expectation carries padding in the expression: %s", w)
		}
	}
	// COVERAGE, logged so a shrunken matrix cannot read as a full one. The pad-2 arm is
	// deliberately narrower than the other two — it exists to answer the two-byte-operator
	// question, not to propose a third admitted padding.
	t.Logf("padding measured: pad0 x%d, pad1 x%d, pad2 x%d (pad2 is the TWO-BYTE `>=` probe only, "+
		"and remains a DECLINE in the production fingerprint)", pads[0], pads[1], pads[2])
	if pads[0] != len(pwOperators()) || pads[1] != len(pwOperators()) {
		t.Errorf("pad0/pad1 do not cover all %d operators (%d/%d)", len(pwOperators()), pads[0], pads[1])
	}
	if pads[2] == 0 {
		t.Error("no pad2 probe ran, so the two-byte-operator padding question is unmeasured")
	}
}

// ---------------------------------------------------------------------------
// Canonical-literal discriminators.
// ---------------------------------------------------------------------------

// pwLiteralCapture is what stock did with one non-canonical integer spelling.
type pwLiteralCapture struct {
	// rejected is true when BAML's own parser refused the project. Then wire is empty
	// and the recorded fact is the refusal itself.
	rejected bool
	// wire is sonic.Marshal of the decoded nested value when the project compiled.
	wire string
	// retained is the string stock put in Check.Expression, quoted here so the
	// relationship to the .baml source text is visible in review.
	retained string
	// status is the outcome stock reached, which says how BAML INTERPRETED the literal.
	status string
	// note records what the outcome tells us about the interpretation.
	note string
}

// pwCompileRefusedErr is the WHOLE Go-side error baml.CreateRuntime returns when stock's
// parser refuses a project. The detailed diagnostic goes to the CFFI's own stderr, so
// this is the entire observable, and it is pinned rather than merely checked non-empty.
const pwCompileRefusedErr = "failed to create BAML runtime"

// pwLiteralCaptures pins what stock v0.223.0 does with each spelling the 7.2b/7.2c
// canonical grammar rejects.
//
// This is the evidence that the fingerprint's literal rule is a bounded OVER-DECLINE and
// not a guess. Four of the five spellings COMPILE and evaluate — BAML's Jinja layer reads
// `007` as 7, `1_000` as 1000 and `5.0` as a float — so nothing upstream would have
// stopped them; the native rule has to reject them itself. The fifth is refused by BAML's
// own parser, which is a different and stronger fact, recorded as such.
var pwLiteralCaptures = map[string]pwLiteralCapture{
	"plus5": {
		rejected: true,
		note: "BAML's Jinja parser REFUSES this spelling outright — the CFFI reports " +
			"'syntax error: unexpected +' and the project does not compile — so it can never reach " +
			"a native gate from a real project",
	},
	"leading_zeros": {
		wire:     `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 007","status":"succeeded"}}}}`,
		retained: "this > 007",
		status:   "succeeded",
		note:     "compiles; `007` is read as 7, and 9 > 7 holds. The retained text keeps the leading zeros",
	},
	"underscore": {
		wire:     `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 1_000","status":"failed"}}}}`,
		retained: "this > 1_000",
		status:   "failed",
		note:     "compiles; `1_000` is read as 1000, and 9 > 1000 fails. The retained text keeps the separator",
	},
	"float": {
		wire:     `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 5.0","status":"succeeded"}}}}`,
		retained: "this > 5.0",
		status:   "succeeded",
		note:     "compiles; a float threshold compares against an int `this` without complaint",
	},
	"overflow": {
		wire:     `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 9223372036854775808","status":"failed"}}}}`,
		retained: "this > 9223372036854775808",
		status:   "failed",
		note: "compiles; one past math.MaxInt64 does NOT wrap — 9 > 2^63 fails, so BAML carried the " +
			"magnitude rather than truncating it to i64",
	},
}

// TestStockNonCanonicalLiteralDispositions pins what stock does with every spelling the
// canonical grammar rejects, so the rejection is measured rather than assumed.
func TestStockNonCanonicalLiteralDispositions(t *testing.T) {
	compiled, rejected := 0, 0
	for _, probe := range pwLiteralProbes() {
		t.Run(probe.ID, func(t *testing.T) {
			capture, ok := pwLiteralCaptures[probe.ID]
			if !ok {
				t.Fatalf("literal probe %q has no pinned capture", probe.ID)
			}
			if capture.note == "" {
				t.Fatal("a literal capture with no note records an outcome without recording what it means")
			}
			if capture.rejected {
				rejected++
				// The RECORDED FACT is the refusal. The Go-side text is pinned exactly
				// even though it is generic: the CFFI writes its detailed diagnostic to
				// stderr, so this string is the whole of what a caller can observe, and
				// pinning it is what makes a CHANGE in that observable visible.
				err := pwCompileError(t, probe.projectKey())
				if got := err.Error(); got != pwCompileRefusedErr {
					t.Fatalf("stock's refusal error = %s, want %s",
						strconv.Quote(got), strconv.Quote(pwCompileRefusedErr))
				}
				t.Logf("stock REFUSED %q at parse time: %v", probe.expr(), err)
				return
			}
			compiled++
			key := pwDriveKey{Project: probe.projectKey(), Func: pwFnChecked, Raw: pwNestedRaw(probe.Confidence)}
			stock := pwCheckedValue(t, key)
			want := shared.Check{Name: pwCheckedLabel, Expression: capture.retained, Status: capture.status}
			if got := stock.Confidence.Checks[pwCheckedLabel]; got != want {
				t.Fatalf("stock check = %+v, want %+v", got, want)
			}
			pwRequireSonicBytes(t, "stock", stock, capture.wire)

			// DISCRIMINATING: the retained text is the SOURCE spelling, not a
			// canonicalised one. A stock that normalised `007` to `7` would fail here
			// rather than pass on a string that merely evaluates the same.
			if capture.retained != probe.expr() {
				t.Errorf("the pinned retained text %q is not the source spelling %q",
					capture.retained, probe.expr())
			}
			if !strings.Contains(capture.wire, `"expression":`+strconv.Quote(probe.expr())) {
				t.Errorf("the pinned wire bytes do not carry the source spelling: %s", capture.wire)
			}
		})
	}
	if compiled == 0 || rejected == 0 {
		t.Fatalf("the literal matrix landed %d compiled and %d rejected; both dispositions must be "+
			"witnessed or one of them is an untested branch", compiled, rejected)
	}
	t.Logf("non-canonical literals: %d COMPILE and evaluate (so the native fingerprint must reject "+
		"them itself), %d refused by BAML's own parser", compiled, rejected)
}

// TestNonCanonicalLiteralsAreNotTheCanonicalOne is the discriminating control: every
// spelling captured above must actually differ from the canonical `strconv.FormatInt`
// form of the value it denotes, or the file would be measuring the admitted grammar.
func TestNonCanonicalLiteralsAreNotTheCanonicalOne(t *testing.T) {
	canonical := map[string]bool{}
	for _, o := range pwOperators() {
		canonical[o.expr()] = true
	}
	for _, probe := range pwLiteralProbes() {
		if canonical[probe.expr()] {
			t.Errorf("literal probe %q spells a CANONICAL expression (%q); it discriminates nothing",
				probe.ID, probe.expr())
		}
		// The literal's canonical form, where it has one, must be a different string.
		if n, err := strconv.ParseInt(probe.Literal, 10, 64); err == nil {
			if strconv.FormatInt(n, 10) == probe.Literal {
				t.Errorf("literal %q round-trips through FormatInt, so it is CANONICAL and belongs in "+
					"the admitted grammar rather than in this table", probe.Literal)
			}
		}
	}
}
