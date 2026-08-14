//go:build integration

package predicatewire

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
)

// The OPERATOR half: what stock v0.223.0 puts on the wire, and in an assertion error,
// for each of the six direct comparisons on BOTH name-pinned families.
//
// The comparison is on raw []byte from sonic.Marshal and on the WHOLE unmodified
// err.Error(). Nothing here is normalised, collapsed or matched by substring: a pinned
// literal is the entire output.
//
// These captures are AUTHORITY, not permission. Slice 7.2c-1 admits nothing: five of the
// six operators below are still declines at every production gate, and
// [TestPredicateWireAdmissionIsUnchanged] proves it beside the captures rather than
// leaving it to another package.

// pwOperatorCapture is the four pinned stock outputs for one operator: three wire byte
// strings and one error byte string.
type pwOperatorCapture struct {
	// checkTrue and checkFalse are sonic.Marshal of the decoded CHECK family with the
	// predicate holding and failing. Both carry the value — a false @check is DATA.
	checkTrue  string
	checkFalse string
	// assertTrue is sonic.Marshal of the decoded ASSERT family with the predicate
	// holding: an ordinary int, no wrapper, no check entry.
	assertTrue string
	// assertFail is the UNMODIFIED err.Error() of the ASSERT family with the predicate
	// FALSE: no value at all, and stock's required-field wrapper chain. The embedded
	// `\n` is stock's DEBUG escape (two bytes), never a real newline.
	assertFail string
}

// pwOperatorCaptures is the byte authority for the whole six-operator manifest.
//
// Every string was produced by driving the raw assistant text of the named isolated
// project through the stock BAML v0.223.0 CFFI and serializing the decoded value with
// sonic (or by taking err.Error() unchanged). Nothing here is derived from native output.
var pwOperatorCaptures = map[string]pwOperatorCapture{
	"gt": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":9}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this > 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this > 0", causes: [] }] }] }] }`,
	},
	"ge": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this >= 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this >= 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":0}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this >= 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this >= 0", causes: [] }] }] }] }`,
	},
	"lt": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this < 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this < 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":-1}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this < 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this < 0", causes: [] }] }] }] }`,
	},
	"le": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this <= 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this <= 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":0}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this <= 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this <= 0", causes: [] }] }] }] }`,
	},
	"eq": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this == 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this == 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":0}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this == 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this == 0", causes: [] }] }] }] }`,
	},
	"ne": {
		checkTrue:  `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this != 0","status":"succeeded"}}}}`,
		checkFalse: `{"answer":"sunny","confidence":{"value":0,"checks":{"positive":{"name":"positive","expression":"this != 0","status":"failed"}}}}`,
		assertTrue: `{"answer":"sunny","confidence":9}`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this != 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this != 0", causes: [] }] }] }] }`,
	},
}

// pwCaptureOf returns one operator's pinned capture, failing loudly if an operator was
// added to the manifest without one.
func pwCaptureOf(t *testing.T, o pwOperator) pwOperatorCapture {
	t.Helper()
	c, ok := pwOperatorCaptures[o.ID]
	if !ok {
		t.Fatalf("operator %q (%s) is in the manifest with no pinned stock capture; the widening "+
			"it proposes would rest on nothing", o.ID, o.Op)
	}
	return c
}

// pwCheckedLabel is the constraint label every operator project uses.
const pwCheckedLabel = "positive"

// TestOperatorManifestIsTheWholeGrammar pins the manifest itself against the 7.2c scope's
// operator set, so a quietly dropped operator is a failure rather than a smaller matrix
// that still passes.
func TestOperatorManifestIsTheWholeGrammar(t *testing.T) {
	// The scope's set, written out independently of pwOperators so the two can disagree.
	want := map[string]bool{">": true, ">=": true, "<": true, "<=": true, "==": true, "!=": true}
	got := map[string]bool{}
	for _, o := range pwOperators() {
		if got[o.Op] {
			t.Fatalf("operator %q appears twice in the manifest", o.Op)
		}
		got[o.Op] = true
		if o.TrueVal == o.FalseVal {
			t.Errorf("operator %q drives the same value for both outcomes, so its two captures "+
				"cannot differ", o.Op)
		}
	}
	for op := range want {
		if !got[op] {
			t.Errorf("the manifest omits %q; 7.2c's operator set is exactly {>, >=, <, <=, ==, !=}", op)
		}
	}
	for op := range got {
		if !want[op] {
			t.Errorf("the manifest carries %q, which is not in 7.2c's operator set", op)
		}
	}
	// COVERAGE, not a claim about the table. The scope requires NESTED AND TOP-LEVEL
	// check pass/fail and assert pass/fail for every operator, so both halves are counted
	// here — a matrix that quietly dropped one of them would otherwise still pass every
	// byte assertion it did keep.
	perProject := map[string]int{}
	perTopFunc := map[string]int{}
	for _, k := range pwAllDrives() {
		perProject[k.Project]++
		if k.Project == pwTopLevelKey {
			perTopFunc[k.Func]++
		}
	}
	for _, o := range pwOperators() {
		if n := perProject[o.projectKey()]; n != 4 {
			t.Errorf("operator %q drives %d NESTED rows, want exactly 4 (check pass/fail, assert "+
				"pass/fail)", o.Op, n)
		}
		for _, fn := range []string{pwTopCheckFn(o), pwTopAssertFn(o)} {
			if n := perTopFunc[fn]; n != 2 {
				t.Errorf("operator %q drives %d rows through %s, want exactly 2 (pass and fail)",
					o.Op, n, fn)
			}
		}
	}
	if n := perProject[pwTopLevelKey]; n != len(pwOperators())*4 {
		t.Errorf("the top-level project drives %d rows, want %d (6 operators x 4 outcomes)",
			n, len(pwOperators())*4)
	}
	t.Logf("operator manifest: 6 operators x 4 outcomes x 2 positions = %d captures — %d NESTED on "+
		"the two pinned families and %d TOP-LEVEL on a bare `int` target",
		6*4*2, perProject[pwOperators()[0].projectKey()]*6, perProject[pwTopLevelKey])
}

// pwCarrierFromStock builds the NATIVE carrier from stock's OWN decoded check results.
//
// The value and every check field come from stock; the only thing this supplies is the
// declaration order, which stock's map fold has already destroyed. The label set must
// match exactly, so a check stock did not report cannot be invented and one it did report
// cannot be dropped.
func pwCarrierFromStock(t *testing.T, stock shared.Checked[int64], declared ...string) bamlutils.Checked[int64] {
	t.Helper()
	if len(declared) != len(stock.Checks) {
		t.Fatalf("the fixture declares %d check(s) but stock reported %d: %v", len(declared), len(stock.Checks), stock.Checks)
	}
	ordered := make([]bamlutils.Check, 0, len(declared))
	for _, label := range declared {
		got, ok := stock.Checks[label]
		if !ok {
			t.Fatalf("stock reported no check under the declared label %q: %v", label, stock.Checks)
		}
		ordered = append(ordered, bamlutils.Check{Name: got.Name, Expression: got.Expression, Status: got.Status})
	}
	carrier, err := bamlutils.NewChecked(stock.Value, ordered)
	if err != nil {
		t.Fatalf("NewChecked over stock's own check results: %v", err)
	}
	return carrier
}

// TestStockOperatorWireBytes is the byte oracle for all six direct comparisons on the
// name-pinned CHECK family, in both outcomes.
//
// Three assertions per row, in order of strength: the decoded value field by field (so
// the bytes cannot pass on a value that merely serializes the same way), the wire bytes
// themselves, and the native carrier built from stock's own results reproducing them.
func TestStockOperatorWireBytes(t *testing.T) {
	for _, o := range pwOperators() {
		cap := pwCaptureOf(t, o)
		for _, tc := range []struct {
			outcome string
			value   int64
			status  string
			want    string
		}{
			{"true", o.TrueVal, "succeeded", cap.checkTrue},
			{"false", o.FalseVal, "failed", cap.checkFalse},
		} {
			t.Run(o.ID+"/check_"+tc.outcome, func(t *testing.T) {
				key := pwDriveKey{Project: o.projectKey(), Func: pwFnChecked, Raw: pwNestedRaw(tc.value)}
				stock := pwCheckedValue(t, key)

				if stock.Answer != "sunny" {
					t.Fatalf("stock answer = %q, want %q", stock.Answer, "sunny")
				}
				if stock.Confidence.Value != tc.value {
					t.Fatalf("stock confidence.value = %d, want %d", stock.Confidence.Value, tc.value)
				}
				if len(stock.Confidence.Checks) != 1 {
					t.Fatalf("stock reported %d checks, want exactly 1: %v",
						len(stock.Confidence.Checks), stock.Confidence.Checks)
				}
				want := shared.Check{Name: pwCheckedLabel, Expression: o.expr(), Status: tc.status}
				if got := stock.Confidence.Checks[pwCheckedLabel]; got != want {
					t.Fatalf("stock check = %+v, want %+v", got, want)
				}

				pwRequireSonicBytes(t, "stock", stock, tc.want)

				// The native carrier, built from stock's OWN results, must produce the
				// SAME bytes. This is the acceptance comparison the scope names.
				type nativeAnswer struct {
					Answer     string                   `json:"answer"`
					Confidence bamlutils.Checked[int64] `json:"confidence"`
				}
				native := nativeAnswer{
					Answer:     stock.Answer,
					Confidence: pwCarrierFromStock(t, stock.Confidence, pwCheckedLabel),
				}
				pwRequireSonicBytes(t, "bamlutils.Checked", native, tc.want)
			})
		}
	}
}

// TestStockOperatorAssertBytes is the ASSERT twin: a HOLDING assert leaves an ordinary
// int on the wire, and a FALSE one emits no value at all and its exact wrapper chain.
//
// The class differs from the check family in ONE token, so every difference below is
// attributable to the level and to nothing else.
func TestStockOperatorAssertBytes(t *testing.T) {
	for _, o := range pwOperators() {
		cap := pwCaptureOf(t, o)

		t.Run(o.ID+"/assert_true", func(t *testing.T) {
			key := pwDriveKey{Project: o.projectKey(), Func: pwFnAssert, Raw: pwNestedRaw(o.TrueVal)}
			stock := pwAssertValue(t, key)
			if stock.Answer != "sunny" || stock.Confidence != o.TrueVal {
				t.Fatalf("stock value = %+v, want answer=sunny confidence=%d", stock, o.TrueVal)
			}
			pwRequireSonicBytes(t, "stock", stock, cap.assertTrue)
			// DISCRIMINATING: the bytes carry no wrapper at all, so an implementation
			// that wrapped a passing assert would fail here rather than pass on a
			// superset.
			if strings.Contains(cap.assertTrue, `"checks"`) || strings.Contains(cap.assertTrue, `"value"`) {
				t.Fatalf("the passing-assert literal carries wrapper keys: %s", cap.assertTrue)
			}
		})

		t.Run(o.ID+"/assert_false", func(t *testing.T) {
			key := pwDriveKey{Project: o.projectKey(), Func: pwFnAssert, Raw: pwNestedRaw(o.FalseVal)}
			err := pwError(t, key)
			if got := err.Error(); got != cap.assertFail {
				t.Fatalf("stock assertion error bytes:\n got %s\nwant %s",
					strconv.Quote(got), strconv.Quote(cap.assertFail))
			}
			// The wrapper chain is the point of driving this on a CLASS FIELD rather
			// than a bare target: it names the field, counts the required-field
			// outcome, and keeps the inner tree beside the flattened display text.
			cause := "Failed: " + pwCheckedLabel + " " + o.expr()
			for _, want := range []string{
				`reason: "Failed while parsing required fields: missing=0, unparsed=1"`,
				`reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: ` + cause + `"`,
				`reason: "Assertions failed."`,
				`reason: "` + cause + `"`,
			} {
				if !strings.Contains(cap.assertFail, want) {
					t.Errorf("the pinned assertion error does not carry %s", want)
				}
			}
			// The embedded newline is DEBUG-ESCAPED (two bytes), never a real one —
			// the single most likely way a renderer gets this wrong.
			if strings.Contains(cap.assertFail, "\n") {
				t.Fatal("the pinned assertion error carries a REAL newline; stock's `{:?}` rendering escapes it")
			}
			if !strings.Contains(cap.assertFail, `\n  - <root>: `) {
				t.Fatalf("the pinned assertion error does not carry the escaped display separator: %s",
					cap.assertFail)
			}
		})
	}
}

// TestStockOperatorCapturesAreProvenToBite is the anti-false-green control for this file.
//
// Each mutant is a byte string a WRONG implementation would produce; none may equal a
// pinned literal, or the corresponding assertion would pass on the wrong bytes. The
// control at the end rebuilds a real literal from the same pieces, so the inequalities
// are about the mutations rather than about the formatter.
func TestStockOperatorCapturesAreProvenToBite(t *testing.T) {
	// (1) The six operators must be DISTINGUISHABLE from one another. Without this,
	// a capture harness that silently drove `>` six times would still be green.
	seen := map[string]string{}
	for _, o := range pwOperators() {
		cap := pwCaptureOf(t, o)
		if prev, dup := seen[cap.checkTrue]; dup {
			t.Errorf("operators %s and %s pin the SAME check-true bytes; one of them is not being "+
				"driven with its own predicate", prev, o.Op)
		}
		seen[cap.checkTrue] = o.Op
		if cap.checkTrue == cap.checkFalse {
			t.Errorf("operator %s pins identical true and false bytes", o.Op)
		}
		if !strings.Contains(cap.checkTrue, `"expression":"`+o.expr()+`"`) {
			t.Errorf("operator %s's check-true literal does not carry its own expression %q: %s",
				o.Op, o.expr(), cap.checkTrue)
		}
		if !strings.Contains(cap.checkTrue, `"status":"succeeded"`) ||
			!strings.Contains(cap.checkFalse, `"status":"failed"`) {
			t.Errorf("operator %s's literals do not carry the two distinct statuses", o.Op)
		}
		// A FALSE check still carries its value, stated as a byte fact.
		if !strings.Contains(cap.checkFalse, fmt.Sprintf(`"value":%d`, o.FalseVal)) {
			t.Errorf("operator %s's failed-check literal drops its value: %s", o.Op, cap.checkFalse)
		}
	}

	// (2) The TWO-BYTE operators must not have collapsed to their one-byte prefixes.
	// `this >= 0` sharing a capture with `this > 0` is the exact failure a substring
	// assertion would miss, and it is the reason 7.2c has to recompute its cause-length
	// ceiling rather than inherit it.
	for _, pair := range []struct{ two, one string }{{"ge", "gt"}, {"le", "lt"}} {
		twoCap, oneCap := pwOperatorCaptures[pair.two], pwOperatorCaptures[pair.one]
		for _, oneByte := range []string{`"expression":"this > 0"`, `"expression":"this < 0"`} {
			if strings.Contains(twoCap.checkTrue, oneByte) {
				t.Errorf("the %s capture carries the one-byte expression %s", pair.two, oneByte)
			}
		}
		if len(twoCap.assertFail) <= len(oneCap.assertFail) {
			t.Errorf("the %s assertion error is not longer than the %s one, yet its expression is "+
				"one byte longer; the cause-length arithmetic 7.2c-3 must redo rests on this",
				pair.two, pair.one)
		}
	}

	// (3) Byte-level mutants of one row, none of which may equal the pinned literal.
	gt := pwOperatorCaptures["gt"]
	const (
		valuePart  = `"value":9`
		checksPart = `"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}`
	)
	inner := "{" + valuePart + "," + checksPart + "}"
	if want := `{"answer":"sunny","confidence":` + inner + `}`; want != gt.checkTrue {
		t.Fatalf("the mutation harness cannot rebuild the pinned literal, so its inequalities prove "+
			"nothing:\n got %s\nwant %s", want, gt.checkTrue)
	}
	for name, mutant := range map[string]string{
		"checks before value":     `{"answer":"sunny","confidence":{` + checksPart + "," + valuePart + `}}`,
		"check fields permuted":   `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"status":"succeeded","name":"positive","expression":"this > 0"}}}}`,
		"value dropped":           `{"answer":"sunny","confidence":{` + checksPart + `}}`,
		"answer after confidence": `{"confidence":` + inner + `,"answer":"sunny"}`,
		"status flipped":          strings.Replace(gt.checkTrue, "succeeded", "failed", 1),
		// encoding/json rewrites `>` in ANY json.Marshaler's output and sonic does not.
		// Stock's own canonical expression carries one, so this mutant is the whole
		// reason sonic — not encoding/json — is the wire authority here.
		"expression HTML-escaped": strings.Replace(gt.checkTrue, ">", `\u003e`, 1),
		"operator widened to >=":  strings.Replace(gt.checkTrue, "this > 0", "this >= 0", 1),
	} {
		if mutant == gt.checkTrue {
			t.Errorf("the %q mutant equals the pinned literal, so no assertion distinguishes them", name)
		}
	}
	// (4) And the same for the ERROR bytes: a renderer that unescaped the newline, or
	// dropped the wrapper, must not produce the pinned string.
	for name, mutant := range map[string]string{
		"newline unescaped": strings.ReplaceAll(gt.assertFail, `\n`, "\n"),
		"wrapper dropped":   `Failed to coerce value: ParsingError { scope: [], reason: "Failed: positive this > 0", causes: [] }`,
		"label dropped":     strings.ReplaceAll(gt.assertFail, "positive this > 0", "this > 0"),
	} {
		if mutant == gt.assertFail {
			t.Errorf("the %q error mutant equals the pinned literal", name)
		}
	}
}

// ---------------------------------------------------------------------------
// NO ADMISSION FLIP.
// ---------------------------------------------------------------------------

// pwBundleFor builds the schema.Bundle that describes exactly what the named isolated
// project declares for one family, so the native gates are asked about the SAME shape
// stock was driven with.
func pwBundleFor(level schema.ConstraintLevel, label, expr string) *schema.Bundle {
	l := label
	confidence := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
	confidence.Meta.Constraints = []schema.Constraint{{Level: level, Expression: expr, Label: &l}}
	name := pwCheckedClass
	if level == schema.ConstraintAssert {
		name = pwAssertClass
	}
	b := &schema.Bundle{
		Target: schema.Type{Kind: schema.TypeClass, Name: name, Mode: schema.NonStreaming},
		Classes: []schema.ClassDef{{
			Name: schema.Name{Name: name},
			Mode: schema.NonStreaming,
			Fields: []schema.ClassField{
				{Name: schema.Name{Name: "answer"}, Type: schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}},
				{Name: schema.Name{Name: "confidence"}, Type: confidence},
			},
		}},
	}
	if err := b.RebuildIndexes(); err != nil {
		panic("predicatewire fixture: " + err.Error())
	}
	return b
}

// pwAdmits reports whether the production entry points ADMIT a bundle, and fails if they
// disagree with one another.
//
// Disagreement is checked rather than assumed because the whole 7.2c discipline is that
// the shape decision has ONE owner: a package that read only SupportsNativeFinalBundle
// could not tell an unchanged fingerprint from a nativeserve delegate that had drifted.
func pwAdmits(t *testing.T, what string, b *schema.Bundle) bool {
	t.Helper()
	supportErr := debaml.SupportsNativeFinalBundle(b)
	switch {
	case supportErr == nil:
	case errors.Is(supportErr, bamlutils.ErrDeBAMLParseUnsupported):
	default:
		t.Fatalf("%s: SupportsNativeFinalBundle returned a non-sentinel error: %v", what, supportErr)
	}
	supported := supportErr == nil
	delegate := debaml.IsAdmittedStaticCheckedFamily(b)
	if supported != delegate {
		t.Fatalf("%s: SupportsNativeFinalBundle says admitted=%v but nativeserve's "+
			"IsAdmittedStaticCheckedFamily delegate says %v; the gates no longer share one fingerprint",
			what, supported, delegate)
	}
	return supported
}

// pwAdmissionDisagreements compares an EXPECTATION against the production gates for
// every operator on both families, and returns one line per disagreement.
//
// It is factored out so it can be driven with a DELIBERATELY WRONG expectation and shown
// to report the mismatch — the mutation proof for a suite whose assertions are all
// "this still declines".
func pwAdmissionDisagreements(t *testing.T, admits func(o pwOperator) bool) []string {
	t.Helper()
	var out []string
	for _, o := range pwOperators() {
		for _, fam := range []struct {
			what  string
			level schema.ConstraintLevel
		}{{"check", schema.ConstraintCheck}, {"assert", schema.ConstraintAssert}} {
			name := fmt.Sprintf("%s %s", fam.what, o.expr())
			got := pwAdmits(t, name, pwBundleFor(fam.level, pwCheckedLabel, o.expr()))
			want := admits(o)
			if got != want {
				out = append(out, fmt.Sprintf("%s: gates say admitted=%v, expected %v", name, got, want))
			}
		}
	}
	return out
}

// pwOnlyGreaterThan is the CURRENT admitted predicate, and the only one 7.2c-1 leaves
// admitted: `this > I`.
func pwOnlyGreaterThan(o pwOperator) bool { return o.Op == ">" }

// TestPredicateWireAdmissionIsUnchanged is the NO-FLIP invariant, stated where the
// captures are.
//
// Slice 7.2c-1 banks authority for six operators and admits ONE. Every other operator —
// on both families, in both levels — must still be refused by the production gates, and
// the two gates driven here must agree with each other on every row.
func TestPredicateWireAdmissionIsUnchanged(t *testing.T) {
	if got := pwAdmissionDisagreements(t, pwOnlyGreaterThan); len(got) != 0 {
		t.Fatalf("the admitted predicate is no longer exactly `this > I`:\n  %s", strings.Join(got, "\n  "))
	}
	// And the route boundary: even the ADMITTED operator declines on the direct parse
	// endpoint, which is the scope's /call-only rule. A capture package that let this
	// slip would be evidence for a route it never measured.
	for _, o := range pwOperators() {
		b := pwBundleFor(schema.ConstraintCheck, pwCheckedLabel, o.expr())
		if _, err := debaml.ParseStaticBundle(context.Background(), b, pwNestedRaw(o.TrueVal)); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("%s: ParseStaticBundle did not decline the checked family on the direct route: %v",
				o.expr(), err)
		}
	}
	t.Logf("admission unchanged: 1 admitted predicate (`this > I`), %d captured-but-declined operator "+
		"rows across both families", (len(pwOperators())-1)*2)
}

// TestPredicateWireAdmissionIsProvenToBite feeds the SAME comparison two deliberately
// wrong expectations and requires each to be REPORTED.
//
// Without it, [TestPredicateWireAdmissionIsUnchanged] could be green because the
// comparison never fires — the classic false green for a suite of declines. The mutants
// are stand-in EXPECTATIONS; the production gates are untouched.
func TestPredicateWireAdmissionIsProvenToBite(t *testing.T) {
	// (1) An expectation that every operator is admitted — the 7.2c-3 cutover, arriving
	// early. It must be reported for the five that are not.
	all := func(pwOperator) bool { return true }
	got := pwAdmissionDisagreements(t, all)
	if len(got) == 0 {
		t.Error("an expectation that ALL six operators are admitted produced no disagreement; this " +
			"file cannot detect a widened fingerprint")
	}
	if want := (len(pwOperators()) - 1) * 2; len(got) != want {
		t.Errorf("the widened expectation was reported %d times, want %d (five operators x two "+
			"families); the comparison is not covering every row", len(got), want)
	}
	// (2) An expectation that NOTHING is admitted. It must be reported for `>`, so a
	// gate that stopped admitting the 7.2b fingerprint is caught too — an under-claim is
	// a failure in its own right.
	none := func(pwOperator) bool { return false }
	if got := pwAdmissionDisagreements(t, none); len(got) != 2 {
		t.Errorf("an expectation that NOTHING is admitted was reported %d times, want exactly 2 "+
			"(the `this > I` check and assert families); the admitted row is not being driven", len(got))
	}
	// (3) A per-operator mutant: admitting exactly ONE new operator must be reported for
	// exactly that operator's two families and nothing else. This is the realistic
	// mistake — a fingerprint widened one token at a time.
	for _, mutant := range pwOperators() {
		if mutant.Op == ">" {
			continue
		}
		admits := func(o pwOperator) bool { return o.Op == ">" || o.Op == mutant.Op }
		got := pwAdmissionDisagreements(t, admits)
		if len(got) != 2 {
			t.Errorf("admitting %q alone was reported %d times, want exactly 2:\n  %s",
				mutant.Op, len(got), strings.Join(got, "\n  "))
		}
		for _, line := range got {
			if !strings.Contains(line, mutant.expr()) {
				t.Errorf("admitting %q alone was reported against a different row: %s", mutant.Op, line)
			}
		}
	}
}
