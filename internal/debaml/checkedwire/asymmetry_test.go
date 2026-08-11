//go:build integration

package checkedwire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/debaml"
	"github.com/invakid404/baml-rest/internal/schema"
)

// The three RECORDED ASYMMETRIES between stock v0.223.0 and what native could claim.
//
// Each is measured against the real CFFI here and then explicitly DECLINED at the
// native entry points, and each carries a section in asymmetries.md naming its #583
// parity-affecting residual. None of them is "fixed": stock's behaviour is the
// contract, and where native cannot reproduce it byte for byte the node stays on BAML.
//
// This slice changes no gate. The declines below are assertions about the CURRENT
// behaviour of the exported entry points, kept beside the stock measurement they are
// the consequence of; internal/debaml's own TestServingOracleBoundaryLock remains the
// full 49-row invariant and is untouched.

// The pinned stock bytes for the three rows.
const (
	// Two @check attributes under one label: baml_go folds the ordered CFFI list into
	// map[string]Check, so the SECOND declaration is what survives.
	wireDuplicateLabels = `{"value":5,"checks":{"dup":{"name":"dup","expression":"this > 1","status":"succeeded"}}}`
	// A bare top-level string return: the false assert is never evaluated and the raw
	// text comes back untouched.
	wireBareStringSkipped = `"hello"`
	// Alias ingress: `amount` arrives and stock emits the CANONICAL `qty` field.
	wireAliasIngress = `{"qty":{"value":7,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`
)

// asymmetryRow ties one stock observation to its native decline and to its section in
// asymmetries.md.
type asymmetryRow struct {
	// ID is the `### <ID>` heading in asymmetries.md.
	ID string
	// Fixtures are the checkedwire rows that measure the stock behaviour.
	Fixtures []string
	// BAMLText is attribute source that must appear VERBATIM in the compiled project,
	// so the bundle below and the .baml stock actually compiled describe one shape
	// rather than two.
	BAMLText []string
	// Bundle is the native-side equivalent, which must decline.
	Bundle func() *schema.Bundle
	// Raw is the assistant text ParseStaticBundle is given.
	Raw string
}

func label(s string) *string { return &s }

func cwIntType() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
}

func cwStringType() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
}

// cwConstraintAttr renders one constraint as the BAML attribute the project declares,
// mirroring the serving oracle's renderer. It is what ties a hand-built schema.Bundle
// to the source stock compiled.
func cwConstraintAttr(c schema.Constraint) string {
	kind := "check"
	if c.Level == schema.ConstraintAssert {
		kind = "assert"
	}
	if c.Label == nil {
		return fmt.Sprintf("@%s({{ %s }})", kind, c.Expression)
	}
	return fmt.Sprintf("@%s(%s, {{ %s }})", kind, *c.Label, c.Expression)
}

var asymmetryRows = []asymmetryRow{{
	ID:       "bare-string-return-skips-its-constraints",
	Fixtures: []string{"BareStringAssertSkipped"},
	BAMLText: []string{`string @assert(never, {{ this == "definitely-not-this" }})`},
	Raw:      "hello",
	Bundle: func() *schema.Bundle {
		t := cwStringType()
		t.Meta.Constraints = []schema.Constraint{{
			Level: schema.ConstraintAssert, Expression: `this == "definitely-not-this"`, Label: label("never"),
		}}
		return &schema.Bundle{Target: t}
	},
}, {
	ID:       "duplicate-check-labels-fold-last-write-wins",
	Fixtures: []string{"DuplicateLabels"},
	BAMLText: []string{`@check(dup, {{ this > 0 }}) @check(dup, {{ this > 1 }})`},
	Raw:      "5",
	Bundle: func() *schema.Bundle {
		t := cwIntType()
		t.Meta.Constraints = []schema.Constraint{
			{Level: schema.ConstraintCheck, Expression: "this > 0", Label: label("dup")},
			{Level: schema.ConstraintCheck, Expression: "this > 1", Label: label("dup")},
		}
		return &schema.Bundle{Target: t}
	},
}, {
	ID:       "alias-ingress-has-canonical-output",
	Fixtures: []string{"AliasIngress"},
	BAMLText: []string{`qty int @alias("amount") @check(positive, {{ this > 0 }})`},
	Raw:      `{"amount": 7}`,
	Bundle: func() *schema.Bundle {
		field := cwIntType()
		field.Meta.Constraints = []schema.Constraint{{
			Level: schema.ConstraintCheck, Expression: "this > 0", Label: label("positive"),
		}}
		cls := schema.ClassDef{
			Name: schema.Name{Name: "CW_AliasedChecked"},
			Mode: schema.NonStreaming,
			Fields: []schema.ClassField{{
				Name: schema.Name{Name: "qty", Alias: label("amount")},
				Type: field,
			}},
		}
		return &schema.Bundle{
			Target:  schema.Type{Kind: schema.TypeClass, Name: cls.Name.Name, Mode: schema.NonStreaming},
			Classes: []schema.ClassDef{cls},
		}
	},
}}

// TestStockDuplicateLabelFoldIsLastWriteWins is the measurement that makes the
// carrier's duplicate-label REJECTION necessary rather than defensive.
//
// The raw CFFI check list carries both declarations; baml_go's decodeCheckedValue
// writes them into a map[string]Check in list order, so exactly one survives and it is
// the LAST. The surviving entry's expression is the discriminating byte: it is the
// second declaration's, not the first's.
func TestStockDuplicateLabelFoldIsLastWriteWins(t *testing.T) {
	stock := cwStockChecked(t, "DuplicateLabels")
	if len(stock.Checks) != 1 {
		t.Fatalf("stock reported %d check entries for two declarations, want 1 (the fold): %v", len(stock.Checks), stock.Checks)
	}
	want := shared.Check{Name: "dup", Expression: "this > 1", Status: "succeeded"}
	if got := stock.Checks["dup"]; got != want {
		t.Fatalf("the surviving entry is %+v, want %+v (the SECOND declaration)", got, want)
	}
	// Discriminating: the FIRST declaration's expression is gone. Without this the row
	// would be satisfied by a fold that kept either one.
	if stock.Checks["dup"].Expression == "this > 0" {
		t.Fatal("the FIRST declaration survived; the fold is first-write-wins, not last")
	}
	cwRequireSonicBytes(t, "stock", stock, wireDuplicateLabels)

	// The native carrier cannot be built from the DECLARED pair at all — which is the
	// point: a duplicate cannot be reproduced byte for byte, so the node must decline
	// before it is ever mapped.
	_, err := bamlutils.NewChecked(int64(5), []bamlutils.Check{
		{Name: "dup", Expression: "this > 0", Status: bamlutils.CheckSucceeded},
		{Name: "dup", Expression: "this > 1", Status: bamlutils.CheckSucceeded},
	})
	if !errors.Is(err, bamlutils.ErrCheckedMalformed) {
		t.Fatalf("NewChecked accepted the duplicate pair (err=%v)", err)
	}
	// Feeding it only what SURVIVED the fold does produce stock's bytes — so the
	// refusal above is about the declaration, not about the carrier being unable to
	// represent stock's result.
	folded := cwCarrierFromStock(t, stock, "dup")
	cwRequireSonicBytes(t, "bamlutils.Checked", folded, wireDuplicateLabels)
}

// TestStockBareStringReturnSkipsConstraints measures the skip: a bare top-level
// `string` return does not evaluate its constraints at all, so even an assert that
// must fail produces the raw text and no error.
func TestStockBareStringReturnSkipsConstraints(t *testing.T) {
	f := cwFixtureNamed(t, "BareStringAssertSkipped")
	r := cwDrive(t, f)
	if r.err != nil {
		t.Fatalf("a bare-string return with a FALSE assert errored, so the constraint was not skipped: %v", r.err)
	}
	s, ok := r.value.(string)
	if !ok {
		t.Fatalf("stock decoded a %T, want a bare string", r.value)
	}
	if s != "hello" {
		t.Fatalf("stock value = %q, want %q", s, "hello")
	}
	cwRequireSonicBytes(t, "stock", r.value, wireBareStringSkipped)

	// Discriminating: the SAME predicate on an int target DOES fail, so the skip is a
	// property of the bare-string position rather than of the predicate.
	if err := cwError(t, "AssertFailLabelled"); !strings.Contains(err.Error(), topReasonPlain) {
		t.Fatalf("the control assert did not fail: %v", err)
	}
}

// TestStockAliasIngressHasCanonicalOutput measures ONLY what this fixture can
// distinguish: the assistant text carries the ALIAS, and stock emits the CANONICAL
// field name with its checked value and status.
//
// It deliberately makes NO predicate-sequencing claim. `this > 0` has the same result
// whether it ran before or after the field-name rewrite, so a scalar row like this one
// cannot witness the ORDER of canonicalisation and evaluation — only their combined
// output. Widening the claim to sequencing would need a predicate whose result differs
// across the rewrite, which this slice does not build and does not need: the disposition
// is unaffected either way, since every alias-bearing candidate stays declined
// (TestAsymmetriesRemainDeclined).
func TestStockAliasIngressHasCanonicalOutput(t *testing.T) {
	v := cwValue(t, "AliasIngress")
	stock, ok := v.(cwAliasedChecked)
	if !ok {
		t.Fatalf("stock decoded a %T, want cwAliasedChecked", v)
	}
	if stock.Qty.Value != 7 {
		t.Fatalf("stock qty = %d, want 7 (the value arrived as `amount`)", stock.Qty.Value)
	}
	want := shared.Check{Name: "positive", Expression: "this > 0", Status: "succeeded"}
	if got := stock.Qty.Checks["positive"]; got != want {
		t.Fatalf("stock alias check = %+v, want %+v", got, want)
	}
	// The check result is part of the observed canonical OUTPUT. It is not a sequencing
	// witness: this predicate succeeds on either side of alias canonicalisation.
	cwRequireSonicBytes(t, "stock", stock, wireAliasIngress)
	// The wire carries the CANONICAL name, not the alias — the fact that makes an
	// alias-bearing candidate a distinct parity claim rather than a renaming.
	if strings.Contains(wireAliasIngress, "amount") {
		t.Fatalf("the wire carries the alias: %s", wireAliasIngress)
	}
}

// TestAsymmetriesRemainDeclined drives the native entry points for each row and
// requires the fallback sentinel from every one.
//
// Two gates are driven — the admission predicate and the static-final serving entry
// point — with a constraint-free, alias-free control proving the gate is not simply
// refusing everything.
func TestAsymmetriesRemainDeclined(t *testing.T) {
	declined := 0
	for _, row := range asymmetryRows {
		t.Run(row.ID, func(t *testing.T) {
			b := row.Bundle()
			if err := debaml.SupportsNativeFinalBundle(b); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("SupportsNativeFinalBundle returned %v, want ErrDeBAMLParseUnsupported", err)
			}
			if _, err := debaml.ParseStaticBundle(context.Background(), b, row.Raw); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("ParseStaticBundle returned %v, want ErrDeBAMLParseUnsupported", err)
			}
			declined++
		})
	}
	if declined != len(asymmetryRows) {
		t.Fatalf("%d of %d rows declined", declined, len(asymmetryRows))
	}

	// The NON-VACUITY control: a flat class with no constraints and no aliases is
	// admitted, so the declines above are about what the rows carry.
	control := &schema.Bundle{
		Target: schema.Type{Kind: schema.TypeClass, Name: "CW_Control", Mode: schema.NonStreaming},
		Classes: []schema.ClassDef{{
			Name: schema.Name{Name: "CW_Control"},
			Mode: schema.NonStreaming,
			Fields: []schema.ClassField{
				{Name: schema.Name{Name: "answer"}, Type: cwStringType()},
				{Name: schema.Name{Name: "confidence"}, Type: cwIntType()},
			},
		}},
	}
	if err := debaml.SupportsNativeFinalBundle(control); err != nil {
		t.Fatalf("the constraint-free control was DECLINED (%v); every decline above would then be vacuous", err)
	}
	res, err := debaml.ParseStaticBundle(context.Background(), control, `{"answer": "sunny", "confidence": 9}`)
	if err != nil {
		t.Fatalf("the constraint-free control failed to parse: %v", err)
	}
	if len(res.JSON) == 0 {
		t.Fatal("the constraint-free control produced no bytes; the control would not prove the lane serves")
	}
}

// TestAsymmetryRowsMatchTheCompiledProject ties each row's hand-built schema.Bundle to
// the .baml stock actually compiled.
//
// Without it a row could decline one shape natively while measuring a different one
// through the CFFI, and both halves would still be green.
func TestAsymmetryRowsMatchTheCompiledProject(t *testing.T) {
	cwEnsureRuntime(t)
	for _, row := range asymmetryRows {
		t.Run(row.ID, func(t *testing.T) {
			if len(row.BAMLText) == 0 {
				t.Fatal("row declares no BAML text, so it is untied to the compiled project")
			}
			for _, want := range row.BAMLText {
				if !strings.Contains(cwSource, want) {
					t.Errorf("the compiled project does not contain %q", want)
				}
			}
			for _, name := range row.Fixtures {
				f := cwFixtureNamed(t, name)
				if !strings.Contains(cwSource, "function "+f.method()+"(") {
					t.Errorf("fixture %s is not in the compiled project", name)
				}
			}
			// The bundle's own constraints must render to attribute text the project
			// carries, so the two descriptions cannot drift apart silently.
			for _, c := range row.Bundle().Target.Meta.Constraints {
				if attr := cwConstraintAttr(c); !strings.Contains(cwSource, attr) {
					t.Errorf("the bundle declares %s, which the compiled project does not carry", attr)
				}
			}
			for _, cls := range row.Bundle().Classes {
				for _, fld := range cls.Fields {
					for _, c := range fld.Type.Meta.Constraints {
						if attr := cwConstraintAttr(c); !strings.Contains(cwSource, attr) {
							t.Errorf("field %s declares %s, which the compiled project does not carry", fld.Name.Name, attr)
						}
					}
					if fld.Name.Alias != nil {
						if attr := fmt.Sprintf("@alias(%q)", *fld.Name.Alias); !strings.Contains(cwSource, attr) {
							t.Errorf("field %s declares %s, which the compiled project does not carry", fld.Name.Name, attr)
						}
					}
				}
			}
		})
	}
}

// TestAsymmetriesLedgerCoversEveryRow ties the rows to asymmetries.md, so the recorded
// residuals cannot drift from the tests that measure them.
func TestAsymmetriesLedgerCoversEveryRow(t *testing.T) {
	const ledgerPath = "asymmetries.md"
	const deferralRecord = "https://github.com/invakid404/baml-rest/issues/583"

	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read %s: %v", ledgerPath, err)
	}
	doc := string(raw)
	sections := map[string]string{}
	for _, part := range strings.Split(doc, "\n### ")[1:] {
		id, body, _ := strings.Cut(part, "\n")
		sections[strings.TrimSpace(id)] = body
	}
	if len(sections) != len(asymmetryRows) {
		t.Fatalf("%s has %d row section(s) but there are %d rows: %v", ledgerPath, len(sections), len(asymmetryRows), sections)
	}
	for _, row := range asymmetryRows {
		body, ok := sections[row.ID]
		if !ok {
			t.Errorf("%s has no `### %s` section", ledgerPath, row.ID)
			continue
		}
		for _, name := range row.Fixtures {
			if !strings.Contains(body, name) {
				t.Errorf("%s section %q does not name its fixture %s", ledgerPath, row.ID, name)
			}
		}
		if !strings.Contains(body, deferralRecord) {
			t.Errorf("%s section %q carries no #583 deferral record", ledgerPath, row.ID)
		}
	}
	// The non-ASCII truncation hazard is recorded here too, because it is the one
	// stock behaviour this package deliberately does not drive.
	if !strings.Contains(doc, "String::truncate") {
		t.Errorf("%s does not record the non-ASCII cause-truncation hazard", ledgerPath)
	}
}
