package staticservefixture

// De-BAML Slice 7.2b-2 — the proof that the REAL generated static client carries the
// de-BAML constraint carrier, over the COMMITTED artifacts rather than over a
// hand-written look-alike.
//
// The fixture project declares the two concrete return types the 7.2b scope admits:
//
//	class StaticCheckedAnswer { answer string; confidence int @check(positive, {{ this > 0 }}) }
//	class StaticAssertAnswer  { answer string; confidence int @assert(positive, {{ this > 0 }}) }
//
// and the documented fixture transform (cmd/hacks' bamlutils-checked-carrier) re-points
// the generated `Checked` alias at [bamlutils.Checked]. Three things then have to hold,
// and each is asserted here in the strongest form available:
//
//  1. the generated CHECKED field really is bamlutils.Checked[int64] and the
//     assert-only field really is a bare int64 — proven at COMPILE time by assignment,
//     which no comment, tag or textual match can fake;
//  2. the emitted per-method decode closure instantiates the STRICT
//     [bamlutils.DecodeStaticFinal] at those exact concrete return types — asserted
//     over the committed generated adapter source; and
//  3. the strict decode of stock's own bytes reproduces them exactly through that
//     carrier, so what the native lane would hand a caller is what stock produced.
//
// Everything here runs in the ordinary CGO-free `go test ./internal/nativeprompt/...`.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
	types "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture/baml_client/types"
)

// The COMPILE-TIME half. These declarations fail to build — not to pass — if the
// generated field types are not exactly these, which is the one form of this claim that
// cannot be satisfied by a stale or approximate assertion.
var (
	_ bamlutils.Checked[int64] = types.StaticCheckedAnswer{}.Confidence
	_ int64                    = types.StaticAssertAnswer{}.Confidence
	_ string                   = types.StaticCheckedAnswer{}.Answer
	_ string                   = types.StaticAssertAnswer{}.Answer
)

// checkedWireNestedPass is the stock BAML v0.223.0 wire form for the checked fixture,
// captured from the real CFFI by internal/debaml/checkedwire (its `wireNestedCheck`)
// and pinned in package debaml beside the mapper. It is reproduced here because THIS
// package is where the real generated carrier can be instantiated; the shared authority
// guard in internal/debaml keeps the copies in step with the capture.
const checkedWireNestedPass = `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`

// checkedWireAssertPass is the assert twin's stock wire form: an ordinary int field,
// because `as_check()` excludes an assert from the CFFI check list.
const checkedWireAssertPass = `{"answer":"sunny","confidence":9}`

// TestGeneratedCheckedFieldIsTheDeBAMLCarrier is the RUNTIME companion to the
// compile-time assignments above: it proves the carrier behaves as the de-BAML one, not
// merely that it type-checks as some struct of that shape.
//
// The discriminating property is the ordered constructor: stock's plain struct has no
// such constructor and no deterministic key order, so a carrier that round-trips
// through NewChecked and reproduces stock's bytes under sonic is the de-BAML one.
func TestGeneratedCheckedFieldIsTheDeBAMLCarrier(t *testing.T) {
	carrier, err := bamlutils.NewChecked(int64(9), []bamlutils.Check{{
		Name: "positive", Expression: "this > 0", Status: bamlutils.CheckSucceeded,
	}})
	if err != nil {
		t.Fatalf("NewChecked: %v", err)
	}
	// Assigning into the GENERATED struct is the point: it only compiles because the
	// generated field is the de-BAML carrier.
	value := types.StaticCheckedAnswer{Answer: "sunny", Confidence: carrier}
	got, err := sonic.Marshal(value)
	if err != nil {
		t.Fatalf("sonic.Marshal: %v", err)
	}
	if string(got) != checkedWireNestedPass {
		t.Fatalf("the generated checked value does not serialize to stock's bytes:\n got %s\nwant %s",
			got, checkedWireNestedPass)
	}

	assertValue := types.StaticAssertAnswer{Answer: "sunny", Confidence: 9}
	gotAssert, err := sonic.Marshal(assertValue)
	if err != nil {
		t.Fatalf("sonic.Marshal (assert): %v", err)
	}
	if string(gotAssert) != checkedWireAssertPass {
		t.Fatalf("the generated assert-only value does not serialize to stock's bytes:\n got %s\nwant %s",
			gotAssert, checkedWireAssertPass)
	}
	// DISCRIMINATING: an assert-only field carries no wrapper at all.
	if strings.Contains(checkedWireAssertPass, `"checks"`) {
		t.Fatalf("the assert literal carries wrapper keys: %s", checkedWireAssertPass)
	}
}

// TestGeneratedStaticFinalDecodeIsStrictAtTheGeneratedType is the decode half, at the
// REAL generated return types.
//
// It is the same core the generated per-method `DecodeNativeStaticFinal` closure calls,
// instantiated at the same two concrete forms, so a decode that succeeded here but
// failed in the emitted closure would mean the two had parted company — which the
// emitted-source assertion below rules out.
func TestGeneratedStaticFinalDecodeIsStrictAtTheGeneratedType(t *testing.T) {
	decoded, err := bamlutils.DecodeStaticFinal[types.StaticCheckedAnswer]([]byte(checkedWireNestedPass))
	if err != nil {
		t.Fatalf("DecodeStaticFinal[types.StaticCheckedAnswer]: %v", err)
	}
	if decoded.Answer != "sunny" {
		t.Fatalf("decoded answer = %q, want \"sunny\"", decoded.Answer)
	}
	if decoded.Confidence.Value != 9 {
		t.Fatalf("decoded confidence.value = %d, want 9", decoded.Confidence.Value)
	}
	want := bamlutils.Check{Name: "positive", Expression: "this > 0", Status: bamlutils.CheckSucceeded}
	if got := decoded.Confidence.Checks["positive"]; got != want {
		t.Fatalf("decoded check = %+v, want %+v", got, want)
	}
	if len(decoded.Confidence.Checks) != 1 {
		t.Fatalf("decoded %d checks, want exactly 1: %v", len(decoded.Confidence.Checks), decoded.Confidence.Checks)
	}
	// The value a caller receives is re-serialized by the worker, so the ROUND TRIP is
	// what has to be stock's — not merely the input.
	round, err := sonic.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(round) != checkedWireNestedPass {
		t.Fatalf("decode -> re-marshal:\n got %s\nwant %s", round, checkedWireNestedPass)
	}

	decodedAssert, err := bamlutils.DecodeStaticFinal[types.StaticAssertAnswer]([]byte(checkedWireAssertPass))
	if err != nil {
		t.Fatalf("DecodeStaticFinal[types.StaticAssertAnswer]: %v", err)
	}
	if decodedAssert.Answer != "sunny" || decodedAssert.Confidence != 9 {
		t.Fatalf("decoded assert = %+v, want {sunny 9}", decodedAssert)
	}
	roundAssert, err := sonic.Marshal(decodedAssert)
	if err != nil {
		t.Fatalf("re-marshal (assert): %v", err)
	}
	if string(roundAssert) != checkedWireAssertPass {
		t.Fatalf("assert decode -> re-marshal:\n got %s\nwant %s", roundAssert, checkedWireAssertPass)
	}

	// STRICTNESS at the generated type. A json.Unmarshaler takes over its whole
	// subtree, so a lenient carrier would silently disable the outer
	// DisallowUnknownFields for exactly the field the constraint lives on.
	for _, tc := range []struct{ name, doc string }{
		{"unknown outer field", `{"answer":"sunny","confidence":{"value":9,"checks":{}},"extra":1}`},
		{"unknown field inside the carrier", `{"answer":"sunny","confidence":{"value":9,"checks":{},"extra":1}}`},
		{"unknown field inside a check", `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded","extra":1}}}}`},
		{"trailing second value", checkedWireNestedPass + checkedWireNestedPass},
		{"carrier value of the wrong type", `{"answer":"sunny","confidence":{"value":"9","checks":{}}}`},
	} {
		if _, err := bamlutils.DecodeStaticFinal[types.StaticCheckedAnswer]([]byte(tc.doc)); err == nil {
			t.Errorf("%s: the strict decoder ACCEPTED a document stock cannot produce: %s", tc.name, tc.doc)
		}
	}
}

// TestGeneratedAdapterInstantiatesTheStrictDecoder asserts, over the COMMITTED generated
// adapter, that the per-method closure the static serve seam installs is
// `bamlutils.DecodeStaticFinal` at each concrete constraint-bearing return type — and
// never a lenient alias decoder.
//
// This is what ties the runtime proofs above to the code the generator actually emitted:
// without it they would show only that the shared core works when called by hand.
func TestGeneratedAdapterInstantiatesTheStrictDecoder(t *testing.T) {
	src := readFixtureFile(t, filepath.Join("generated", "adapter.go"))
	for _, want := range []string{
		"bamlutils.DecodeStaticFinal[types.StaticCheckedAnswer](__cj)",
		"bamlutils.DecodeStaticFinal[types.StaticAssertAnswer](__cj)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the generated adapter does not instantiate %s", want)
		}
	}
	for _, forbidden := range []string{
		"DecodeStaticAliasFinal[types.StaticCheckedAnswer]",
		"DecodeStaticAliasFinal[types.StaticAssertAnswer]",
		"DecodeStaticAliasStream[types.StaticCheckedAnswer]",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("the generated adapter routes a constraint carrier through %s, losing strictness", forbidden)
		}
	}
}

// TestGeneratedCheckedAliasAndTypeMapAreInStep pins the three coordinated rewrites the
// fixture transform makes, so a half-applied transform is a failure here rather than a
// panic inside the CFFI callback at run time.
//
//   - the `Checked` alias names bamlutils (the FIELD type, and the wire form);
//   - the type map registers `StockChecked` (the shape stock's reflective decoder
//     hardcodes and is the only thing it can build); and
//   - each generated checked-field decode CONVERTS between the two.
func TestGeneratedCheckedAliasAndTypeMapAreInStep(t *testing.T) {
	utils := parseFixtureFile(t, filepath.Join("baml_client", "types", "utils.go"))
	aliasOK := false
	ast.Inspect(utils, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Checked" || !ts.Assign.IsValid() {
			return true
		}
		idx, ok := ts.Type.(*ast.IndexExpr)
		if !ok {
			t.Errorf("the generated Checked alias is not an instantiated alias: %T", ts.Type)
			return false
		}
		sel, ok := idx.X.(*ast.SelectorExpr)
		if !ok {
			t.Errorf("the generated Checked alias does not name a package-qualified type: %T", idx.X)
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "bamlutils" || sel.Sel.Name != "Checked" {
			t.Errorf("the generated Checked alias names %v.%s, want bamlutils.Checked", sel.X, sel.Sel.Name)
			return false
		}
		aliasOK = true
		return false
	})
	if !aliasOK {
		t.Fatal("the generated client declares no `type Checked[T any] = bamlutils.Checked[T]`; the " +
			"generated static path would carry stock's non-deterministic carrier")
	}

	typeMap := readFixtureFile(t, filepath.Join("baml_client", "type_map.go"))
	if !strings.Contains(typeMap, "types.StockChecked[int64]{}") {
		t.Error("the type map does not register types.StockChecked[int64]; stock's reflective decoder " +
			"can only build its own carrier and would panic in the CFFI callback")
	}
	if strings.Contains(typeMap, "types.Checked[int64]{}") {
		t.Error("the type map still registers the (now de-BAML) Checked alias")
	}

	classes := readFixtureFile(t, filepath.Join("baml_client", "types", "classes.go"))
	if !strings.Contains(classes, "FromStockChecked(baml.Decode(valueHolder).Interface().(StockChecked[int64]))") {
		t.Error("the generated checked-field decode does not convert stock's carrier into the de-BAML one")
	}
	streamClasses := readFixtureFile(t, filepath.Join("baml_client", "stream_types", "classes.go"))
	if !strings.Contains(streamClasses, "types.FromStockCheckedPtr(") {
		t.Error("the generated STREAM checked-field decode does not convert stock's carrier")
	}
}

// fixtureRoot is the checked-in staticserve fixture, relative to this package.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "testdata", "staticserve_fixture")
}

// readFixtureFile reads one committed fixture file, FAILING rather than skipping: a
// file a guard cannot read is not evidence of compliance.
func readFixtureFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureRoot(t), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty; every assertion over it would be vacuous", rel)
	}
	return string(raw)
}

// parseFixtureFile parses one committed fixture file.
func parseFixtureFile(t *testing.T, rel string) *ast.File {
	t.Helper()
	path := filepath.Join(fixtureRoot(t), rel)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return f
}

// TestGeneratedCarrierProofIsNotVacuous is the anti-false-green control for the literals
// this file compares against: a wrong implementation's bytes must not equal them.
func TestGeneratedCarrierProofIsNotVacuous(t *testing.T) {
	mutants := map[string]string{
		"checks before value":     `{"answer":"sunny","confidence":{"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}},"value":9}}`,
		"status flipped":          strings.Replace(checkedWireNestedPass, "succeeded", "failed", 1),
		"expression HTML-escaped": strings.Replace(checkedWireNestedPass, ">", `\u003e`, 1),
		"wrapper on an assert":    `{"answer":"sunny","confidence":{"value":9,"checks":{}}}`,
	}
	for name, out := range mutants {
		if out == checkedWireNestedPass || out == checkedWireAssertPass {
			t.Errorf("the %q mutant equals a pinned literal, so no assertion distinguishes them", name)
		}
	}
	if checkedWireNestedPass == checkedWireAssertPass {
		t.Fatal("the checked and assert literals are identical")
	}
	if !strings.Contains(checkedWireNestedPass, `"value":9`) ||
		!strings.Contains(checkedWireNestedPass, `"status":"succeeded"`) {
		t.Fatalf("the checked literal does not carry both a value and a status: %s", checkedWireNestedPass)
	}
	// The literals must be exactly what this package's own strconv.Quote round-trips,
	// so a stray escape in the source cannot masquerade as a byte difference.
	if q, err := strconv.Unquote(strconv.Quote(checkedWireNestedPass)); err != nil || q != checkedWireNestedPass {
		t.Fatalf("the checked literal does not survive a quote round trip: %v", err)
	}
}
