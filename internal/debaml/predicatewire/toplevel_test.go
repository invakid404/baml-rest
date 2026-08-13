//go:build integration

package predicatewire

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
)

// The TOP-LEVEL half of the operator matrix: what stock puts on the wire, and in an
// assertion error, when the constraint sits on the RETURN TYPE itself rather than on a
// class field.
//
// # Why this is a separate capture and not an inference from the nested one
//
// The scope requires nested AND top-level check pass/fail and assert pass/fail for every
// direct operator, and the two are different outcomes:
//
//   - a top-level check emits the carrier as the WHOLE response. Its bytes are not the
//     nested object minus a wrapper; there is no enclosing object at all, so a mapper
//     that produced the nested form here would be wrong in a way the nested fixtures
//     cannot see.
//   - a top-level FAILING assert carries NO required-field wrapper. The nested error's
//     entire `Failed while parsing required fields: missing=0, unparsed=1` /
//     `Failed to parse field confidence: ...` chain comes from the FIELD POSITION. Strip
//     it and what remains is a two-level `Assertions failed.` / `Failed: <label> <expr>`
//     tree — which is what these rows pin, and which no amount of reasoning about the
//     nested capture would have established.
//
// Everything here is still a DECLINE: the admitted fingerprint requires the two-field
// name-pinned class, so a bare constrained target is refused before any socket.
// [TestTopLevelOperatorFormsAreDeclined] proves it and is proven to bite.

// pwTopLevelCaptures is the byte authority for the six operators at TOP LEVEL.
//
// Same provenance as pwOperatorCaptures: each string is stock v0.223.0's own output for
// the named drive, through sonic (values) or unchanged (errors).
var pwTopLevelCaptures = map[string]pwOperatorCapture{
	"gt": {
		checkTrue:  `{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}`,
		checkFalse: `{"value":-1,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"failed"}}}`,
		assertTrue: `9`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this > 0", causes: [] }] }`,
	},
	"ge": {
		checkTrue:  `{"value":0,"checks":{"positive":{"name":"positive","expression":"this >= 0","status":"succeeded"}}}`,
		checkFalse: `{"value":-1,"checks":{"positive":{"name":"positive","expression":"this >= 0","status":"failed"}}}`,
		assertTrue: `0`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this >= 0", causes: [] }] }`,
	},
	"lt": {
		checkTrue:  `{"value":-1,"checks":{"positive":{"name":"positive","expression":"this < 0","status":"succeeded"}}}`,
		checkFalse: `{"value":9,"checks":{"positive":{"name":"positive","expression":"this < 0","status":"failed"}}}`,
		assertTrue: `-1`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this < 0", causes: [] }] }`,
	},
	"le": {
		checkTrue:  `{"value":0,"checks":{"positive":{"name":"positive","expression":"this <= 0","status":"succeeded"}}}`,
		checkFalse: `{"value":9,"checks":{"positive":{"name":"positive","expression":"this <= 0","status":"failed"}}}`,
		assertTrue: `0`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this <= 0", causes: [] }] }`,
	},
	"eq": {
		checkTrue:  `{"value":0,"checks":{"positive":{"name":"positive","expression":"this == 0","status":"succeeded"}}}`,
		checkFalse: `{"value":9,"checks":{"positive":{"name":"positive","expression":"this == 0","status":"failed"}}}`,
		assertTrue: `0`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this == 0", causes: [] }] }`,
	},
	"ne": {
		checkTrue:  `{"value":9,"checks":{"positive":{"name":"positive","expression":"this != 0","status":"succeeded"}}}`,
		checkFalse: `{"value":0,"checks":{"positive":{"name":"positive","expression":"this != 0","status":"failed"}}}`,
		assertTrue: `9`,
		assertFail: `Failed to coerce value: ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this != 0", causes: [] }] }`,
	},
}

// pwTopCaptureOf returns one operator's pinned TOP-LEVEL capture.
func pwTopCaptureOf(t *testing.T, o pwOperator) pwOperatorCapture {
	t.Helper()
	c, ok := pwTopLevelCaptures[o.ID]
	if !ok {
		t.Fatalf("operator %q (%s) has no pinned TOP-LEVEL capture; the scope requires nested AND "+
			"top-level rows for every direct operator", o.ID, o.Op)
	}
	return c
}

// TestStockTopLevelOperatorWireBytes is the byte oracle for the six comparisons on a bare
// `int` target, in both check outcomes, with the native carrier reproducing them.
func TestStockTopLevelOperatorWireBytes(t *testing.T) {
	for _, o := range pwOperators() {
		capture := pwTopCaptureOf(t, o)
		for _, tc := range []struct {
			outcome string
			value   int64
			status  string
			want    string
		}{
			{"true", o.TrueVal, "succeeded", capture.checkTrue},
			{"false", o.FalseVal, "failed", capture.checkFalse},
		} {
			t.Run(o.ID+"/check_"+tc.outcome, func(t *testing.T) {
				key := pwDriveKey{Project: pwTopLevelKey, Func: pwTopCheckFn(o), Raw: strconv.FormatInt(tc.value, 10)}
				stock := pwBareChecked(t, key)

				if stock.Value != tc.value {
					t.Fatalf("stock value = %d, want %d", stock.Value, tc.value)
				}
				if len(stock.Checks) != 1 {
					t.Fatalf("stock reported %d checks, want exactly 1: %v", len(stock.Checks), stock.Checks)
				}
				want := shared.Check{Name: pwCheckedLabel, Expression: o.expr(), Status: tc.status}
				if got := stock.Checks[pwCheckedLabel]; got != want {
					t.Fatalf("stock check = %+v, want %+v", got, want)
				}
				pwRequireSonicBytes(t, "stock", stock, tc.want)

				// The native carrier, built from stock's OWN results, must produce the
				// SAME bytes — here UNENCLOSED, which is the whole point of the row.
				pwRequireSonicBytes(t, "bamlutils.Checked",
					pwCarrierFromStock(t, stock, pwCheckedLabel), tc.want)
			})
		}
	}
}

// TestStockTopLevelOperatorAssertBytes pins the ASSERT twin at top level: a holding
// assert leaves a BARE int with no wrapper at all, and a failing one produces the
// unwrapped two-level assertion tree.
func TestStockTopLevelOperatorAssertBytes(t *testing.T) {
	for _, o := range pwOperators() {
		capture := pwTopCaptureOf(t, o)

		t.Run(o.ID+"/assert_true", func(t *testing.T) {
			key := pwDriveKey{Project: pwTopLevelKey, Func: pwTopAssertFn(o), Raw: strconv.FormatInt(o.TrueVal, 10)}
			v := pwValue(t, key)
			// A passing assert creates no check entry, so no wrapper exists at all: the
			// decoded value is a bare int64, not a Checked. Asserted as a TYPE fact
			// first, so the bytes below cannot pass on a wrapper that happens to
			// serialize to a number.
			if _, wrapped := v.(shared.Checked[int64]); wrapped {
				t.Fatalf("a passing top-level @assert produced a Checked wrapper: %#v", v)
			}
			n, ok := v.(int64)
			if !ok {
				t.Fatalf("stock decoded a %T, want a bare int64", v)
			}
			if n != o.TrueVal {
				t.Fatalf("stock value = %d, want %d", n, o.TrueVal)
			}
			pwRequireSonicBytes(t, "stock", v, capture.assertTrue)
			if strings.Contains(capture.assertTrue, `"checks"`) || strings.Contains(capture.assertTrue, `"value"`) {
				t.Fatalf("the passing top-level assert literal carries wrapper keys: %s", capture.assertTrue)
			}
		})

		t.Run(o.ID+"/assert_false", func(t *testing.T) {
			key := pwDriveKey{Project: pwTopLevelKey, Func: pwTopAssertFn(o), Raw: strconv.FormatInt(o.FalseVal, 10)}
			err := pwError(t, key)
			if got := err.Error(); got != capture.assertFail {
				t.Fatalf("stock top-level assertion error bytes:\n got %s\nwant %s",
					strconv.Quote(got), strconv.Quote(capture.assertFail))
			}
			cause := "Failed: " + pwCheckedLabel + " " + o.expr()
			for _, want := range []string{`reason: "Assertions failed."`, `reason: "` + cause + `"`} {
				if !strings.Contains(capture.assertFail, want) {
					t.Errorf("the pinned top-level assertion error does not carry %s", want)
				}
			}
			// THE DISCRIMINATING HALF: the required-field wrapper the NESTED error carries
			// is ABSENT here. That is the fact this row exists to establish, and it is
			// what makes the top-level capture irreducible to the nested one.
			for _, absent := range []string{
				"Failed while parsing required fields",
				"Failed to parse field confidence",
				`\n  - <root>: `,
			} {
				if strings.Contains(capture.assertFail, absent) {
					t.Errorf("the pinned top-level assertion error carries the NESTED wrapper fragment "+
						"%q; a bare target has no field position for it to come from", absent)
				}
			}
			// And it carries a REAL newline nowhere — the nested form's escaped separator
			// is absent because the whole display line is absent, not because it was
			// unescaped.
			if strings.Contains(capture.assertFail, "\n") {
				t.Fatal("the pinned top-level assertion error carries a REAL newline")
			}
		})
	}
}

// TestTopLevelCapturesAreIrreducibleToTheNestedOnes is the control that makes the whole
// file non-redundant: every top-level row must DIFFER from its nested twin, or the scope's
// "nested and top-level" requirement would be satisfied by one capture written twice.
func TestTopLevelCapturesAreIrreducibleToTheNestedOnes(t *testing.T) {
	for _, o := range pwOperators() {
		top, nested := pwTopCaptureOf(t, o), pwCaptureOf(t, o)
		for _, tc := range []struct{ what, top, nested string }{
			{"check-true", top.checkTrue, nested.checkTrue},
			{"check-false", top.checkFalse, nested.checkFalse},
			{"assert-true", top.assertTrue, nested.assertTrue},
			{"assert-false", top.assertFail, nested.assertFail},
		} {
			if tc.top == tc.nested {
				t.Errorf("%s %s: the top-level and nested literals are IDENTICAL, so one of them is "+
					"not being driven from its own fixture", o.Op, tc.what)
			}
		}
		// The nested check literal ENCLOSES the top-level one; the top-level one is not a
		// standalone slice of the nested bytes plus an object, which is the shape a
		// careless implementation would assume.
		if !strings.Contains(nested.checkTrue, top.checkTrue) {
			t.Errorf("%s: the nested check literal does not contain the top-level carrier bytes; the "+
				"two captures disagree about the carrier itself, which is a real divergence rather "+
				"than a nesting difference:\n  nested: %s\n  top:    %s",
				o.Op, nested.checkTrue, top.checkTrue)
		}
		// The nested ASSERT error, by contrast, is NOT an extension of the top-level one:
		// stock re-renders the inner tree rather than nesting the outer error's text. This
		// is recorded because it is the assumption a renderer is most likely to make.
		if strings.Contains(nested.assertFail, top.assertFail) {
			t.Errorf("%s: the nested assertion error CONTAINS the top-level one verbatim; the recorded "+
				"relationship between the two error shapes is wrong", o.Op)
		}
	}
	t.Logf("top-level captures: 6 operators x 4 outcomes = %d rows, every one distinct from its "+
		"nested twin", len(pwOperators())*4)
}

// pwTopLevelBundle builds the bare constrained-target bundle: the return type IS the
// constrained `int`, with no class anywhere.
func pwTopLevelBundle(level schema.ConstraintLevel, label, expr string) *schema.Bundle {
	l := label
	target := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
	target.Meta.Constraints = []schema.Constraint{{Level: level, Expression: expr, Label: &l}}
	b := &schema.Bundle{Target: target}
	if err := b.RebuildIndexes(); err != nil {
		panic("predicatewire top-level fixture: " + err.Error())
	}
	return b
}

// pwTopLevelDeclineReport returns one line per top-level form whose gate disposition
// disagrees with the expectation, so the comparison can be driven with a wrong one.
func pwTopLevelDeclineReport(t *testing.T, declines func(o pwOperator) bool) []string {
	t.Helper()
	var out []string
	for _, o := range pwOperators() {
		for _, fam := range []struct {
			what  string
			level schema.ConstraintLevel
		}{{"check", schema.ConstraintCheck}, {"assert", schema.ConstraintAssert}} {
			name := "top-level " + fam.what + " " + o.expr()
			admitted := pwAdmits(t, name, pwTopLevelBundle(fam.level, pwCheckedLabel, o.expr()))
			if admitted == declines(o) {
				out = append(out, name+": gates admitted="+strconv.FormatBool(admitted)+
					", expected declined="+strconv.FormatBool(declines(o)))
			}
		}
	}
	return out
}

// TestTopLevelOperatorFormsAreDeclined proves the newly captured top-level forms are ALL
// still refused — including `this > I`, which is admitted in its nested two-field form
// and must not become claimable just because its bytes are now pinned.
func TestTopLevelOperatorFormsAreDeclined(t *testing.T) {
	always := func(pwOperator) bool { return true }
	if got := pwTopLevelDeclineReport(t, always); len(got) != 0 {
		t.Fatalf("a top-level constrained target is no longer declined:\n  %s", strings.Join(got, "\n  "))
	}
	for _, o := range pwOperators() {
		b := pwTopLevelBundle(schema.ConstraintCheck, pwCheckedLabel, o.expr())
		if _, err := debaml.ParseStaticBundle(context.Background(), b, strconv.FormatInt(o.TrueVal, 10)); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("%s: ParseStaticBundle did not decline the bare constrained target: %v", o.expr(), err)
		}
	}
	t.Logf("all %d top-level operator forms (6 operators x 2 levels) DECLINE at the production "+
		"gates, `this > I` included", len(pwOperators())*2)
}

// TestTopLevelDeclinesAreProvenToBite feeds the same comparison the opposite expectation
// and requires every row to be reported.
func TestTopLevelDeclinesAreProvenToBite(t *testing.T) {
	never := func(pwOperator) bool { return false }
	got := pwTopLevelDeclineReport(t, never)
	if want := len(pwOperators()) * 2; len(got) != want {
		t.Fatalf("an expectation that NO top-level form declines was reported %d times, want %d "+
			"(six operators x two levels); the comparison is not covering every row", len(got), want)
	}
}
