//go:build integration

package checkedwire

import (
	stdjson "encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"
	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
)

// The WIRE half: what stock v0.223.0 puts on the wire for a checked value, and the
// proof that [bamlutils.Checked] reproduces it byte for byte.
//
// The comparison is on raw []byte from [sonic.Marshal] — the serializer worker/parse.go
// and the final stream path use, and therefore the acceptance authority. encoding/json
// is checked too, but only as a second observation: it HTML-escapes any json.Marshaler's
// output, so its bytes differ for an expression carrying `<`, `>` or `&` — which stock's
// own `this > 0` does. That divergence belongs to the encoder, not to either carrier,
// and TestStockWireEncoderFraming measures it on stock's plain struct as well as on the
// native one.

// The pinned stock wire bytes. Every one of these was produced by driving the raw
// assistant text of the named fixture through the stock BAML v0.223.0 CFFI and
// serializing the decoded value with sonic.
const (
	wireCheckPass     = `{"value":5,"checks":{"gt":{"name":"gt","expression":"this > 0","status":"succeeded"}}}`
	wireCheckFail     = `{"value":5,"checks":{"gt":{"name":"gt","expression":"this > 100","status":"failed"}}}`
	wireNestedCheck   = `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`
	wireCheckLessThan = `{"value":5,"checks":{"lt":{"name":"lt","expression":"this < 100","status":"succeeded"}}}`
	// A node carrying BOTH a check and a passing assert keeps only the check.
	wireCheckPlusAssert = `{"value":5,"checks":{"c":{"name":"c","expression":"this > 0","status":"succeeded"}}}`
	// A passing assert is not a check at all: the value stays a bare int.
	wireAssertPassNoCheck = `5`
)

// cwStockChecked pulls the decoded stock value out of a fixture and requires it to be
// the concrete generated shape, `shared.Checked[int64]`.
//
// The type assertion is the point: if stock had NOT wrapped the value, or had wrapped
// it in something else, this fails rather than silently comparing a different shape.
func cwStockChecked(t *testing.T, fixture string) shared.Checked[int64] {
	t.Helper()
	v := cwValue(t, fixture)
	c, ok := v.(shared.Checked[int64])
	if !ok {
		t.Fatalf("%s: stock decoded a %T, want shared.Checked[int64]", fixture, v)
	}
	return c
}

// cwCarrierFromStock builds the NATIVE carrier from stock's OWN decoded check
// results, in the declaration order the .baml fixture states.
//
// The value and every check field come from stock; the only thing this test supplies
// is the order, which stock's map fold has already destroyed (that loss is what
// asymmetries.md records and what the duplicate-label row measures). The label set is
// required to match exactly, so a check stock did not report cannot be invented here
// and one it did report cannot be dropped.
func cwCarrierFromStock(t *testing.T, stock shared.Checked[int64], declared ...string) bamlutils.Checked[int64] {
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

// cwRequireSonicBytes asserts sonic.Marshal(v) is exactly want.
func cwRequireSonicBytes(t *testing.T, what string, v any, want string) {
	t.Helper()
	got, err := sonic.Marshal(v)
	if err != nil {
		t.Fatalf("%s: sonic.Marshal: %v", what, err)
	}
	if string(got) != want {
		t.Fatalf("%s: sonic bytes:\n got %s\nwant %s", what, got, want)
	}
}

// TestStockCheckedWireBytes is the byte oracle for a single checked int, in both
// outcomes, and the proof that the native carrier reproduces stock's bytes exactly.
func TestStockCheckedWireBytes(t *testing.T) {
	for _, tc := range []struct {
		fixture, label, expression, status, want string
	}{
		{"CheckPass", "gt", "this > 0", "succeeded", wireCheckPass},
		{"CheckFail", "gt", "this > 100", "failed", wireCheckFail},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			stock := cwStockChecked(t, tc.fixture)

			// (1) The decoded value itself, field by field — so the bytes below cannot
			// pass on a value that merely serializes the same way.
			if stock.Value != 5 {
				t.Fatalf("stock value = %d, want 5", stock.Value)
			}
			if len(stock.Checks) != 1 {
				t.Fatalf("stock reported %d checks, want exactly 1: %v", len(stock.Checks), stock.Checks)
			}
			want := shared.Check{Name: tc.label, Expression: tc.expression, Status: tc.status}
			if got := stock.Checks[tc.label]; got != want {
				t.Fatalf("stock check = %+v, want %+v", got, want)
			}

			// (2) The WIRE: stock's own struct through the worker's serializer.
			cwRequireSonicBytes(t, "stock", stock, tc.want)

			// (3) The native carrier, built from stock's own results, must produce the
			// SAME bytes. This is the acceptance comparison.
			carrier := cwCarrierFromStock(t, stock, tc.label)
			cwRequireSonicBytes(t, "bamlutils.Checked", carrier, tc.want)
		})
	}

	// A FALSE check still carries its value. Stated as a byte fact rather than as a
	// property of the fixture name.
	if !strings.Contains(wireCheckFail, `"value":5`) || !strings.Contains(wireCheckFail, `"status":"failed"`) {
		t.Fatalf("the failed-check literal does not carry both a value and a failed status: %s", wireCheckFail)
	}
}

// TestStockNestedCheckedWireBytes pins the nested class placement — the shape the
// first production admission fingerprint targets — and proves the native carrier
// reproduces it inside an ordinary struct.
func TestStockNestedCheckedWireBytes(t *testing.T) {
	v := cwValue(t, "NestedCheck")
	stock, ok := v.(cwStaticCheckedAnswer)
	if !ok {
		t.Fatalf("stock decoded a %T, want cwStaticCheckedAnswer", v)
	}
	if stock.Answer != "sunny" || stock.Confidence.Value != 9 {
		t.Fatalf("stock value = %+v, want answer=sunny confidence.value=9", stock)
	}
	want := shared.Check{Name: "positive", Expression: "this > 0", Status: "succeeded"}
	if got := stock.Confidence.Checks["positive"]; got != want {
		t.Fatalf("stock nested check = %+v, want %+v", got, want)
	}
	cwRequireSonicBytes(t, "stock", stock, wireNestedCheck)

	// The native twin: the same struct with the carrier swapped in.
	type nativeAnswer struct {
		Answer     string                   `json:"answer"`
		Confidence bamlutils.Checked[int64] `json:"confidence"`
	}
	native := nativeAnswer{Answer: stock.Answer, Confidence: cwCarrierFromStock(t, stock.Confidence, "positive")}
	cwRequireSonicBytes(t, "bamlutils.Checked", native, wireNestedCheck)
}

// TestStockWireEncoderFraming pins the one place the two Go encoders disagree over
// these bytes, on BOTH carriers, so it is recorded as encoder framing rather than
// discovered later as a carrier bug.
//
// sonic (the wire route) leaves `<` alone; encoding/json rewrites it. Stock's own
// canonical expression `this > 0` already contains a `>`, so this is not an exotic
// case — it is every check fixture in this package.
func TestStockWireEncoderFraming(t *testing.T) {
	stock := cwStockChecked(t, "CheckLessThan")
	cwRequireSonicBytes(t, "stock", stock, wireCheckLessThan)
	carrier := cwCarrierFromStock(t, stock, "lt")
	cwRequireSonicBytes(t, "bamlutils.Checked", carrier, wireCheckLessThan)

	escaped := strings.NewReplacer("<", cwEscapedLT, ">", cwEscapedGT, "&", cwEscapedAmp)
	wantStd := escaped.Replace(wireCheckLessThan)
	if wantStd == wireCheckLessThan {
		t.Fatal("the fixture carries no HTML-escapable character, so this test would witness nothing")
	}
	for _, tc := range []struct {
		what string
		v    any
	}{{"stock", stock}, {"bamlutils.Checked", carrier}} {
		got, err := stdjson.Marshal(tc.v)
		if err != nil {
			t.Fatalf("%s: encoding/json.Marshal: %v", tc.what, err)
		}
		if string(got) != wantStd {
			t.Fatalf("%s: encoding/json bytes:\n got %s\nwant %s", tc.what, got, wantStd)
		}
	}
	// And the canonical `>` fixture diverges the same way, so the divergence reaches
	// the shape the scope documents rather than only this one.
	pass := cwStockChecked(t, "CheckPass")
	gotStd, err := stdjson.Marshal(pass)
	if err != nil {
		t.Fatalf("encoding/json.Marshal: %v", err)
	}
	if string(gotStd) != escaped.Replace(wireCheckPass) {
		t.Fatalf("encoding/json bytes for the canonical fixture:\n got %s\nwant %s", gotStd, escaped.Replace(wireCheckPass))
	}
}

// TestStockAssertIsNotACheck pins the three halves of the @assert/@check split, each
// as a byte or type fact.
func TestStockAssertIsNotACheck(t *testing.T) {
	// (1) A PASSING assert creates no check entry, so no wrapper exists at all: the
	// decoded value is a bare int64, not a Checked.
	v := cwValue(t, "AssertPassNoCheck")
	if _, wrapped := v.(shared.Checked[int64]); wrapped {
		t.Fatalf("a passing @assert produced a Checked wrapper: %#v", v)
	}
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("stock decoded a %T, want a bare int64", v)
	}
	if n != 5 {
		t.Fatalf("stock value = %d, want 5", n)
	}
	cwRequireSonicBytes(t, "assert-only", v, wireAssertPassNoCheck)

	// (2) A node carrying a check AND a passing assert keeps ONLY the check. The
	// assert's label is absent from the map, which is the discriminating fact.
	both := cwStockChecked(t, "CheckPlusPassingAssert")
	if len(both.Checks) != 1 {
		t.Fatalf("stock reported %d checks for a check+assert node, want exactly 1: %v", len(both.Checks), both.Checks)
	}
	if _, present := both.Checks["a"]; present {
		t.Fatalf("the passing @assert produced a check entry: %v", both.Checks)
	}
	cwRequireSonicBytes(t, "stock", both, wireCheckPlusAssert)
	cwRequireSonicBytes(t, "bamlutils.Checked", cwCarrierFromStock(t, both, "c"), wireCheckPlusAssert)

	// (3) A FAILING assert emits no success value at all — not a value with a failed
	// status, which is what a @check would have produced.
	f := cwFixtureNamed(t, "AssertFailLabelled")
	r := cwDrive(t, f)
	if r.err == nil {
		t.Fatalf("a failing @assert produced a value: %#v", r.value)
	}
	if r.value != nil {
		t.Fatalf("a failing @assert produced BOTH an error and a value (%#v)", r.value)
	}
}

// TestGeneratedCarrierShapeMatchesStockCodegen grounds the hand-written
// [cwStaticCheckedAnswer] in what BAML's Go generator actually emits.
//
// The nested wire bytes are only stock's if the Go struct they came from is the one
// stock's generator would have produced. That claim is checked against the CHECKED-IN
// stock-generated client under internal/debaml/testdata/constraint_oracle, which
// BAML v0.223.0 generated for a class carrying a checked int field: the field type
// must be the generated `Checked[int64]` (an alias for baml_go's shared.Checked) and
// the JSON tag must be the BAML field name.
func TestGeneratedCarrierShapeMatchesStockCodegen(t *testing.T) {
	const generatedClasses = "../testdata/constraint_oracle/baml_client/types/classes.go"
	const generatedAliases = "../testdata/constraint_oracle/baml_client/types/utils.go"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, generatedClasses, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", generatedClasses, err)
	}
	found := 0
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, f := range st.Fields.List {
			idx, ok := f.Type.(*ast.IndexExpr)
			if !ok {
				continue
			}
			base, ok := idx.X.(*ast.Ident)
			if !ok || base.Name != "Checked" {
				continue
			}
			arg, ok := idx.Index.(*ast.Ident)
			if !ok || arg.Name != "int64" {
				continue
			}
			if f.Tag == nil || len(f.Names) != 1 {
				continue
			}
			if !strings.Contains(f.Tag.Value, `json:"`) {
				t.Errorf("%s.%s has a Checked[int64] field with no json tag: %s", ts.Name.Name, f.Names[0].Name, f.Tag.Value)
			}
			found++
		}
		return true
	})
	if found == 0 {
		t.Fatal("the checked-in stock-generated client declares no Checked[int64] field, so the hand-written " +
			"mirror in this package is ungrounded")
	}

	// And the generated `Checked` is baml_go's, not a private redefinition.
	aliases, err := parser.ParseFile(fset, generatedAliases, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", generatedAliases, err)
	}
	aliasOK := false
	ast.Inspect(aliases, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Checked" || !ts.Assign.IsValid() {
			return true
		}
		// `type Checked[T any] = baml.Checked[T]`
		idx, ok := ts.Type.(*ast.IndexExpr)
		if !ok {
			t.Errorf("generated Checked is not an instantiated alias: %T", ts.Type)
			return false
		}
		sel, ok := idx.X.(*ast.SelectorExpr)
		if !ok {
			t.Errorf("generated Checked aliases a local type, not baml_go's: %T", idx.X)
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "baml" || sel.Sel.Name != "Checked" {
			t.Errorf("generated Checked aliases %v.%s, want baml.Checked", sel.X, sel.Sel.Name)
			return false
		}
		aliasOK = true
		return false
	})
	if !aliasOK {
		t.Fatal("the checked-in stock-generated client does not alias Checked to baml.Checked[T]")
	}
	t.Logf("stock codegen shape confirmed: %d Checked[int64] field(s) in the generated client", found)
}

// TestStockWireBytesAreProvenToBite is the anti-false-green control for this file.
//
// Each mutant below is a byte string a WRONG implementation would produce; none of
// them may equal a pinned literal, or the corresponding assertion would pass on the
// wrong bytes. The control at the end rebuilds the real literal from the same pieces,
// so the inequalities are about the mutations rather than about the formatter.
func TestStockWireBytesAreProvenToBite(t *testing.T) {
	const (
		valuePart  = `"value":5`
		checksPart = `"checks":{"gt":{"name":"gt","expression":"this > 0","status":"succeeded"}}`
	)
	mutants := map[string]string{
		"checks before value":   "{" + checksPart + "," + valuePart + "}",
		"check fields permuted": `{"value":5,"checks":{"gt":{"status":"succeeded","name":"gt","expression":"this > 0"}}}`,
		"value dropped":         "{" + checksPart + "}",
		"status flipped":        strings.Replace(wireCheckPass, "succeeded", "failed", 1),
		"expression escaped":    strings.Replace(wireCheckPass, ">", cwEscapedGT, 1),
	}
	for name, out := range mutants {
		if out == wireCheckPass {
			t.Errorf("the %q mutant equals the pinned literal, so no assertion distinguishes them", name)
		}
	}
	if want := "{" + valuePart + "," + checksPart + "}"; want != wireCheckPass {
		t.Fatalf("the mutation harness cannot rebuild the pinned literal, so its inequalities prove nothing:\n got %s\nwant %s",
			want, wireCheckPass)
	}
	// The failed-check literal must differ from the succeeded one in BOTH the status
	// and the expression, so neither fixture can stand in for the other.
	if wireCheckPass == wireCheckFail {
		t.Fatal("the pass and fail literals are identical")
	}
	if !strings.Contains(wireCheckFail, `"status":"failed"`) {
		t.Fatalf("the fail literal does not carry a failed status: %s", wireCheckFail)
	}
}
