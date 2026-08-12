package debaml

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/bytedance/sonic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// De-BAML Slice 7.2b-2 — the PRODUCTION mapper's proof.
//
// Everything here runs in the ordinary, CGO-free `go test ./internal/debaml/...`. The
// bytes it compares against are STOCK's, captured from the real BAML v0.223.0 CFFI by
// internal/debaml/checkedwire and copied here as literals;
// [TestStaticCheckedStockAuthorityAgrees] parses that package's source and proves each
// copy is byte-identical to the constant it came from, so the untagged proof cannot
// drift away from the tagged capture.
//
// Native output is NEVER re-fed to the CFFI. The comparison is one-directional: stock
// produced these bytes, and the mapper must reproduce them.

// The stock authority. Each literal is the constant of the same meaning in
// internal/debaml/checkedwire, and the agreement guard names the pairing.
const (
	// `{"answer": "sunny", "confidence": 9}` against
	// `class { answer string; confidence int @check(positive, {{ this > 0 }}) }`.
	staticCheckedWireNestedPass = `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`
	// The same declaration with `confidence: -1`, so the predicate is FALSE. A false
	// check is DATA: the value is still emitted.
	staticCheckedWireNestedFail = `{"answer":"sunny","confidence":{"value":-1,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"failed"}}}}`
	// The @assert twin with the predicate HOLDING: no wrapper, an ordinary int field.
	staticCheckedWireAssertPass = `{"answer":"sunny","confidence":9}`
	// A DUPLICATE canonical key (`answer` twice): stock keeps the FIRST occurrence.
	// This is a COERCION fact the mapper inherits, not a constraint fact.
	staticCheckedWireDuplicateKey = `{"answer":"first","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`
	// The @assert twin with the predicate FALSE: no value at all, and this exact
	// error text.
	staticCheckedAssertFailBytes = `Failed to coerce value: ParsingError { scope: [], reason: "Failed while parsing required fields: missing=0, unparsed=1", causes: [ParsingError { scope: [], reason: "Failed to parse field confidence: <root>: Assertions failed.\n  - <root>: Failed: positive this > 0", causes: [ParsingError { scope: [], reason: "Assertions failed.", causes: [ParsingError { scope: [], reason: "Failed: positive this > 0", causes: [] }] }] }] }`
)

// staticCheckedAuthorityPairs names, for each literal above, the checkedwire constant
// it must equal. Declared as data so the agreement guard cannot be satisfied by a
// pairing that quietly went missing.
var staticCheckedAuthorityPairs = map[string]string{
	"staticCheckedWireNestedPass":   "wireNestedCheck",
	"staticCheckedWireNestedFail":   "wireNestedCheckFail",
	"staticCheckedWireAssertPass":   "wireNestedAssertPass",
	"staticCheckedAssertFailBytes":  "errNestedAssertFail",
	"staticCheckedWireDuplicateKey": "wireNestedCheckDuplicateKey",
}

// staticCheckedNativeAnswer is the GENERATED static return type for the check
// fixture, as the codegen must emit it: the carrier is bamlutils' (whose bytes and
// deterministic key order are proven) rather than stock baml_go's.
type staticCheckedNativeAnswer struct {
	Answer     string                   `json:"answer"`
	Confidence bamlutils.Checked[int64] `json:"confidence"`
}

// staticCheckedNativeAssertAnswer is the GENERATED static return type for the assert
// fixture. `confidence` stays an ORDINARY int64: `as_check()` excludes an assert from
// the CFFI check list, so a passing assert produces no wrapper at all.
type staticCheckedNativeAssertAnswer struct {
	Answer     string `json:"answer"`
	Confidence int64  `json:"confidence"`
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// staticCheckedBundle builds the admitted fingerprint for one level/label/expression.
func staticCheckedBundle(level schema.ConstraintLevel, label, expr string) *schema.Bundle {
	c := schema.Constraint{Level: level, Expression: expr}
	if label != "" {
		l := label
		c.Label = &l
	}
	confidence := intType()
	confidence.Meta.Constraints = []schema.Constraint{c}
	name := "StaticCheckedAnswer"
	if level == schema.ConstraintAssert {
		name = "StaticAssertAnswer"
	}
	b := &schema.Bundle{
		Target: schema.Type{Kind: schema.TypeClass, Name: name, Mode: schema.NonStreaming},
		Classes: []schema.ClassDef{{
			Name: schema.Name{Name: name},
			Mode: schema.NonStreaming,
			Fields: []schema.ClassField{
				scalarField("answer", stringType()),
				scalarField("confidence", confidence),
			},
		}},
	}
	if err := b.RebuildIndexes(); err != nil {
		panic("static checked fixture: " + err.Error())
	}
	return b
}

// staticCheckedRow is one admitted-fingerprint row driven end to end.
type staticCheckedRow struct {
	name  string
	level schema.ConstraintLevel
	label string
	expr  string
	raw   string
	// wantJSON is the exact canonical output; empty for the assert-failure row,
	// which must emit NO value.
	wantJSON string
	// wantErr is the exact err.Error() for the assert-failure row; empty otherwise.
	wantErr string
	// wantStatus is the carrier status for a @check row; empty for @assert rows.
	wantStatus string
	// wantOutcome is the predicate result the INDEPENDENT #662 collector must
	// separately reach for the constrained node.
	wantOutcome constraintStateOutcome
}

// staticCheckedRows are the four serving-shaped outcomes of the two narrow fixtures —
// the same four the #665 companion rows name, and the same raw texts checkedwire
// handed the real CFFI.
func staticCheckedRows() []staticCheckedRow {
	return []staticCheckedRow{{
		name: "check_pass", level: schema.ConstraintCheck, label: "positive", expr: "this > 0",
		raw:      `{"answer": "sunny", "confidence": 9}`,
		wantJSON: staticCheckedWireNestedPass, wantStatus: bamlutils.CheckSucceeded,
		wantOutcome: constraintOutcomeTrue,
	}, {
		name: "check_fail", level: schema.ConstraintCheck, label: "positive", expr: "this > 0",
		raw:      `{"answer": "sunny", "confidence": -1}`,
		wantJSON: staticCheckedWireNestedFail, wantStatus: bamlutils.CheckFailed,
		wantOutcome: constraintOutcomeFalse,
	}, {
		name: "assert_pass", level: schema.ConstraintAssert, label: "positive", expr: "this > 0",
		raw:      `{"answer": "sunny", "confidence": 9}`,
		wantJSON: staticCheckedWireAssertPass,
		// A HOLDING assert leaves no trace in the value, so there is no status.
		wantOutcome: constraintOutcomeTrue,
	}, {
		name: "assert_fail", level: schema.ConstraintAssert, label: "positive", expr: "this > 0",
		raw:         `{"answer": "sunny", "confidence": -1}`,
		wantErr:     staticCheckedAssertFailBytes,
		wantOutcome: constraintOutcomeFalse,
	}}
}

func (r staticCheckedRow) bundle() *schema.Bundle {
	return staticCheckedBundle(r.level, r.label, r.expr)
}

// ---------------------------------------------------------------------------
// The mapper
// ---------------------------------------------------------------------------

// TestStaticCheckedMapperProducesStockBytes is the acceptance comparison: for each of
// the four serving-shaped outcomes, the production mapper's RAW bytes (or its exact
// error text) equal what stock v0.223.0 produced for the same declaration and the same
// assistant text.
func TestStaticCheckedMapperProducesStockBytes(t *testing.T) {
	for _, r := range staticCheckedRows() {
		t.Run(r.name, func(t *testing.T) {
			b := r.bundle()
			prof, ok := staticCheckedProfileOf(b)
			if !ok {
				t.Fatal("the admitted fingerprint did not classify its own fixture")
			}
			res, err := staticCheckedMap(b, prof, r.raw)

			if r.wantErr != "" {
				if err == nil {
					t.Fatalf("a FALSE @assert produced a value (%s); stock emits none", res.JSON)
				}
				if len(res.JSON) != 0 {
					t.Fatalf("a FALSE @assert produced BOTH an error and %s bytes", res.JSON)
				}
				if !staticCheckedIsAssertFailure(err) {
					t.Fatalf("a FALSE @assert produced %T (%v), not the rendered stock assertion failure", err, err)
				}
				// A CLAIMED parse failure, never a decline: a decline would send the
				// request back to BAML and hide the disagreement.
				if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
					t.Fatalf("the assertion failure carries the DECLINE sentinel: %v", err)
				}
				if got := err.Error(); got != r.wantErr {
					t.Fatalf("assertion error bytes:\n got %s\nwant %s", strconv.Quote(got), strconv.Quote(r.wantErr))
				}
				return
			}

			if err != nil {
				t.Fatalf("mapper returned %v, want the canonical bytes", err)
			}
			if got := string(res.JSON); got != r.wantJSON {
				t.Fatalf("canonical bytes:\n got %s\nwant %s", got, r.wantJSON)
			}
		})
	}
}

// TestStaticCheckedMapperDecodesStrictlyIntoTheGeneratedType proves the mapper's bytes
// are what the GENERATED static seam actually consumes: the per-method
// DecodeNativeStaticFinal closure is an instantiation of [bamlutils.DecodeStaticFinal]
// at the method's concrete return type, and it is STRICT.
//
// Both concrete forms are exercised, because the two fixtures differ precisely in
// which one they need: `Checked[int64]` for the check fixture and a bare `int64` for
// the assert fixture.
//
// The re-marshal is the second half: the worker serializes the DECODED value with
// sonic, so `sonic.Marshal(decode(mapperBytes))` — not merely the mapper's bytes — is
// what a caller receives, and it too must be stock's.
func TestStaticCheckedMapperDecodesStrictlyIntoTheGeneratedType(t *testing.T) {
	for _, r := range staticCheckedRows() {
		if r.wantJSON == "" {
			continue // the assert-failure row emits no value to decode
		}
		t.Run(r.name, func(t *testing.T) {
			b := r.bundle()
			prof, _ := staticCheckedProfileOf(b)
			res, err := staticCheckedMap(b, prof, r.raw)
			if err != nil {
				t.Fatalf("mapper: %v", err)
			}

			var round []byte
			if r.level == schema.ConstraintCheck {
				decoded, derr := bamlutils.DecodeStaticFinal[staticCheckedNativeAnswer](res.JSON)
				if derr != nil {
					t.Fatalf("DecodeStaticFinal[Checked[int64] carrier](%s): %v", res.JSON, derr)
				}
				// The DECODED value, field by field, so the re-marshal below cannot pass
				// on a value that merely serializes the same way.
				if decoded.Answer != "sunny" {
					t.Fatalf("decoded answer = %q, want \"sunny\"", decoded.Answer)
				}
				want := bamlutils.Check{Name: r.label, Expression: r.expr, Status: r.wantStatus}
				if got := decoded.Confidence.Checks[r.label]; got != want {
					t.Fatalf("decoded check = %+v, want %+v", got, want)
				}
				if len(decoded.Confidence.Checks) != 1 {
					t.Fatalf("decoded %d checks, want exactly 1: %v", len(decoded.Confidence.Checks), decoded.Confidence.Checks)
				}
				round, err = sonic.Marshal(decoded)
			} else {
				decoded, derr := bamlutils.DecodeStaticFinal[staticCheckedNativeAssertAnswer](res.JSON)
				if derr != nil {
					t.Fatalf("DecodeStaticFinal[int64 field](%s): %v", res.JSON, derr)
				}
				if decoded.Answer != "sunny" || decoded.Confidence != 9 {
					t.Fatalf("decoded = %+v, want {sunny 9}", decoded)
				}
				round, err = sonic.Marshal(decoded)
			}
			if err != nil {
				t.Fatalf("sonic.Marshal of the decoded value: %v", err)
			}
			if string(round) != r.wantJSON {
				t.Fatalf("decode -> re-marshal bytes:\n got %s\nwant %s", round, r.wantJSON)
			}
		})
	}
}

// TestStaticCheckedDecodeIsStrict proves the STRICTNESS the generated decoder relies
// on is real for the new carrier — the property the scope requires be shown rather
// than assumed when [bamlutils.DecodeStaticFinal] is instantiated at a nested
// `Checked[T]`.
//
// A json.Unmarshaler takes over its whole subtree, so a lenient carrier would
// silently disable the outer DisallowUnknownFields for exactly the field the
// constraint lives on. Each input below is a document stock cannot produce.
func TestStaticCheckedDecodeIsStrict(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"unknown outer field", `{"answer":"sunny","confidence":{"value":9,"checks":{}},"extra":1}`},
		{"unknown field INSIDE the carrier", `{"answer":"sunny","confidence":{"value":9,"checks":{},"extra":1}}`},
		{"unknown field inside a check", `{"answer":"sunny","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded","extra":1}}}}`},
		{"trailing second value", staticCheckedWireNestedPass + `{"answer":"x","confidence":{"value":1,"checks":{}}}`},
		{"carrier value of the wrong type", `{"answer":"sunny","confidence":{"value":"9","checks":{}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bamlutils.DecodeStaticFinal[staticCheckedNativeAnswer]([]byte(tc.doc)); err == nil {
				t.Fatalf("the strict decoder ACCEPTED a document stock cannot produce: %s", tc.doc)
			}
		})
	}
	// CONTROL: the mapper's own bytes decode cleanly, so the rejections above are
	// about the mutations rather than about the decoder refusing everything.
	if _, err := bamlutils.DecodeStaticFinal[staticCheckedNativeAnswer]([]byte(staticCheckedWireNestedPass)); err != nil {
		t.Fatalf("the strict decoder rejected the stock bytes: %v", err)
	}
}

// TestStaticCheckedMapperAgreesWithTheCollector runs the test-only #662 coercion-state
// collector BESIDE the production mapper for every admitted row and requires the two
// to agree — scope §"Independent state check".
//
// The collector is an independent implementation of the same traversal (it descends
// production coerce node by node and evaluates each attached predicate itself), so an
// agreement here is evidence the mapper read the coercion the same way rather than
// evidence that one implementation is self-consistent. It is a WITNESS, never a
// production dependency: the guard in constraint_state_seam_test.go proves no
// production file can reach it.
func TestStaticCheckedMapperAgreesWithTheCollector(t *testing.T) {
	for _, r := range staticCheckedRows() {
		t.Run(r.name, func(t *testing.T) {
			b := r.bundle()
			run, err := collectConstraintCoercionState(b, r.raw)
			if err != nil {
				t.Fatalf("collector: %v", err)
			}
			// The collector records production's verdict verbatim: while the seam is
			// closed it must still be a decline.
			if !errors.Is(run.ProductionSupport, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("the collector recorded ProductionSupport=%v; the seam is closed, so every "+
					"constraint-bearing bundle must still decline", run.ProductionSupport)
			}
			node := run.Root.find("$.confidence")
			if node == nil {
				t.Fatal("the collector produced no state for $.confidence")
			}
			if len(node.Events) != 1 {
				t.Fatalf("the collector recorded %d events at $.confidence, want exactly 1: %v", len(node.Events), node.Events)
			}
			ev := node.Events[0]
			if ev.Outcome != r.wantOutcome {
				t.Fatalf("the collector decided %s, the row expects %s", ev.Outcome, r.wantOutcome)
			}
			// The EXPRESSION the collector saw is the source text, and it is the same
			// text the mapper must have put in the carrier verbatim.
			if ev.Expression != r.expr {
				t.Fatalf("the collector saw expression %q, want %q", ev.Expression, r.expr)
			}
			if ev.Level != r.level || ev.Label != r.label || ev.Origin != constraintOriginTypeMeta {
				t.Fatalf("the collector recorded %s, which is not this row's constraint", ev.describe())
			}

			// And the MAPPER's own answer, read back out of what it produced.
			prof, _ := staticCheckedProfileOf(b)
			res, merr := staticCheckedMap(b, prof, r.raw)
			switch {
			case r.level == schema.ConstraintCheck:
				if merr != nil {
					t.Fatalf("mapper: %v", merr)
				}
				decoded, derr := bamlutils.DecodeStaticFinal[staticCheckedNativeAnswer](res.JSON)
				if derr != nil {
					t.Fatalf("decode mapper output: %v", derr)
				}
				gotOutcome := constraintOutcomeFalse
				if decoded.Confidence.Checks[r.label].Status == bamlutils.CheckSucceeded {
					gotOutcome = constraintOutcomeTrue
				}
				if gotOutcome != ev.Outcome {
					t.Fatalf("the mapper's carrier says %s but the independent collector says %s",
						gotOutcome, ev.Outcome)
				}
			case ev.Outcome == constraintOutcomeTrue:
				if merr != nil {
					t.Fatalf("a HOLDING assert must map to a value, got %v", merr)
				}
				if node.AssertFailed {
					t.Fatal("the collector reports AssertFailed for a row the mapper served")
				}
			default:
				if !staticCheckedIsAssertFailure(merr) {
					t.Fatalf("a FALSE assert must map to the rendered stock failure, got %v", merr)
				}
				if !node.AssertFailed {
					t.Fatal("the mapper rejected the node but the collector does not report AssertFailed")
				}
			}
		})
	}
}

// TestStaticCheckedMapperPreservesFieldOrderAndPlainJSON pins the two properties
// requirement 5 is about, each as a byte fact rather than as a property of the happy
// path.
func TestStaticCheckedMapperPreservesFieldOrderAndPlainJSON(t *testing.T) {
	b := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")
	prof, _ := staticCheckedProfileOf(b)

	// (1) CANONICAL FIELD ORDER, not input order. The assistant writes the two fields
	// REVERSED; the output must still be `answer` then `confidence`, because the order
	// is the SCHEMA's.
	res, err := staticCheckedMap(b, prof, `{"confidence": 9, "answer": "sunny"}`)
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	if got := string(res.JSON); got != staticCheckedWireNestedPass {
		t.Fatalf("reversed input did not produce canonical field order:\n got %s\nwant %s",
			got, staticCheckedWireNestedPass)
	}

	// (2) NORMAL JSON BEHAVIOUR for the untouched field: a value needing escaping goes
	// through unchanged from the coercion's own bytes, so the splice cannot be
	// re-encoding it.
	res, err = staticCheckedMap(b, prof, `{"answer": "a\"b\\c\nd", "confidence": 9}`)
	if err != nil {
		t.Fatalf("mapper with an escaped answer: %v", err)
	}
	const wantEscaped = `{"answer":"a\"b\\c\nd","confidence":{"value":9,"checks":{"positive":{"name":"positive","expression":"this > 0","status":"succeeded"}}}}`
	if got := string(res.JSON); got != wantEscaped {
		t.Fatalf("escaped answer:\n got %s\nwant %s", got, wantEscaped)
	}
	// And the SAME bytes come back out of a strict decode + re-marshal.
	decoded, derr := bamlutils.DecodeStaticFinal[staticCheckedNativeAnswer](res.JSON)
	if derr != nil {
		t.Fatalf("decode escaped: %v", derr)
	}
	round, merr := sonic.Marshal(decoded)
	if merr != nil {
		t.Fatalf("re-marshal escaped: %v", merr)
	}
	if string(round) != wantEscaped {
		t.Fatalf("escaped round trip:\n got %s\nwant %s", round, wantEscaped)
	}
}

// TestStaticCheckedMapperDeclinesRatherThanGuesses pins requirement 6: everything the
// mapper cannot prove returns the EXISTING unsupported sentinel, so the caller falls
// back to BAML instead of receiving a value native invented.
func TestStaticCheckedMapperDeclinesRatherThanGuesses(t *testing.T) {
	b := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")
	prof, _ := staticCheckedProfileOf(b)

	for _, tc := range []struct{ name, raw string }{
		{"no cleanly-claimable candidate", "there is no JSON here at all"},
		{"confidence is absent", `{"answer": "sunny"}`},
		{"confidence cannot be coerced to an int", `{"answer": "sunny", "confidence": "not a number"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := staticCheckedMap(b, prof, tc.raw)
			if err == nil {
				t.Fatalf("the mapper SERVED %s for input it cannot prove", res.JSON)
			}
			if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("the mapper returned %v; an unprovable input must carry the decline sentinel", err)
			}
			if len(res.JSON) != 0 {
				t.Fatalf("the mapper declined but still produced %s bytes", res.JSON)
			}
		})
	}
}

// TestStaticCheckedCanonicalReadersAreStrict drives the three readers the mapper uses
// on its OWN canonical output directly, because the fingerprint keeps most of their
// failure modes unreachable through [staticCheckedMap] — an unreachable guard that is
// never asserted is a guard nobody can rely on.
//
// Each returns the decline sentinel rather than a guessed value, and the join/split
// pair is proven to round-trip byte-exactly (which is what makes the mapper's
// byte-parity check a real proof rather than a re-serialization agreeing with itself).
func TestStaticCheckedCanonicalReadersAreStrict(t *testing.T) {
	t.Run("split", func(t *testing.T) {
		for _, tc := range []struct{ name, doc string }{
			{"not an object", `[1,2]`},
			{"a bare scalar", `5`},
			{"truncated", `{"answer":`},
			{"trailing value", `{"answer":"a"}{"answer":"b"}`},
			{"trailing garbage", `{"answer":"a"} nope`},
		} {
			if _, err := staticCheckedSplit([]byte(tc.doc)); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("split(%s) returned %v, want the decline sentinel", tc.doc, err)
			}
		}
	})

	t.Run("int", func(t *testing.T) {
		for _, tc := range []string{`1.5`, `"9"`, `null`, `true`, `9223372036854775808`, `1e3`, `9 9`} {
			if n, err := staticCheckedInt([]byte(tc)); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("int(%s) returned (%d, %v), want the decline sentinel", tc, n, err)
			}
		}
		for _, tc := range []struct {
			doc  string
			want int64
		}{{`0`, 0}, {`9`, 9}, {`-1`, -1}, {`9223372036854775807`, 9223372036854775807}} {
			got, err := staticCheckedInt([]byte(tc.doc))
			if err != nil {
				t.Errorf("int(%s): %v", tc.doc, err)
				continue
			}
			if got != tc.want {
				t.Errorf("int(%s) = %d, want %d", tc.doc, got, tc.want)
			}
		}
	})

	t.Run("split then join is byte-exact", func(t *testing.T) {
		for _, doc := range []string{
			`{}`,
			`{"answer":"sunny","confidence":9}`,
			`{"answer":"a\"b\\c\nd","confidence":-1}`,
			`{"b":2,"a":1}`,
			`{"answer":"é","confidence":0}`,
		} {
			members, err := staticCheckedSplit([]byte(doc))
			if err != nil {
				t.Errorf("split(%s): %v", doc, err)
				continue
			}
			rebuilt, jerr := staticCheckedJoin(members)
			if jerr != nil {
				t.Errorf("join(%s): %v", doc, jerr)
				continue
			}
			if got := string(rebuilt); got != doc {
				t.Errorf("split -> join is not byte-exact:\n got %s\nwant %s", got, doc)
			}
		}
	})
}

// TestStaticCheckedMapperUsesTheProductionEvaluator pins requirement 1 STRUCTURALLY:
// the mapper's file names internal/debaml's own evaluator and nothing else.
//
// The repo-root #649 guard already proves no production file anywhere calls
// bamlprofile's constraint façade. This is the positive half for this file
// specifically — that the mapper evaluates AT ALL, through the designated seam,
// rather than re-deciding the predicate from its statically parsed threshold.
func TestStaticCheckedMapperUsesTheProductionEvaluator(t *testing.T) {
	file := staticCheckedParseSource(t, staticCheckedSourcePath(t, "checked_static.go"))
	calls := map[string]int{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			calls[id.Name]++
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok {
				calls[pkg.Name+"."+sel.Sel.Name]++
			}
		}
		return true
	})
	if calls["EvaluateConstraint"] != 1 {
		t.Fatalf("checked_static.go calls EvaluateConstraint %d time(s), want exactly 1; the mapper must "+
			"decide every predicate through the production evaluator", calls["EvaluateConstraint"])
	}
	for name := range calls {
		if strings.HasPrefix(name, "bamlprofile.") {
			t.Errorf("checked_static.go calls %s; internal/debaml is the single production evaluator seam", name)
		}
	}
	// The mapper must also reuse the SHARED extract/coerce pipeline rather than a
	// private one, which is what makes canonical field order inherited (requirement 5).
	for _, want := range []string{"coerce", "extractCandidateMode", "stripJSONComments"} {
		if calls[want] == 0 {
			t.Errorf("checked_static.go never calls %s; the mapper is not consuming the shared "+
				"canonical coercion pipeline", want)
		}
	}
	// AND it imports NOTHING from stock BAML. The scope's hard rule is that the mapper
	// consumes the native canonical coercion output and never serializes native JSON to
	// hand back to the CFFI as a validation step; an import is the only way it could,
	// so its absence is the structural half of that claim.
	imported := 0
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("unparsable import %s", imp.Path.Value)
		}
		imported++
		if strings.Contains(path, "boundaryml") || strings.Contains(path, "cffi") {
			t.Errorf("checked_static.go imports %q; native output must never be re-fed to the CFFI", path)
		}
	}
	if imported == 0 {
		t.Fatal("checked_static.go imports nothing; the import scan would be vacuous")
	}
}

// ---------------------------------------------------------------------------
// The fingerprint
// ---------------------------------------------------------------------------

// TestStaticCheckedProfileAdmitsOnlyTheFingerprint drives the classifier over the two
// admitted shapes and over a ONE-PROPERTY sibling of each rejection reason.
//
// Every negative row differs from an accepted bundle in exactly one property, so a
// rejection is attributable to that property rather than to the fixture being
// generally malformed.
func TestStaticCheckedProfileAdmitsOnlyTheFingerprint(t *testing.T) {
	label := func(s string) *string { return &s }
	accept := func(level schema.ConstraintLevel, l, e string) *schema.Bundle {
		return staticCheckedBundle(level, l, e)
	}
	// The positives first: without them the negatives could all be satisfied by a
	// classifier that rejects everything.
	for _, p := range []struct {
		name  string
		b     *schema.Bundle
		level schema.ConstraintLevel
		label string
	}{
		{"check", accept(schema.ConstraintCheck, "positive", "this > 0"), schema.ConstraintCheck, "positive"},
		{"assert", accept(schema.ConstraintAssert, "positive", "this > 0"), schema.ConstraintAssert, "positive"},
		{"assert without a label", accept(schema.ConstraintAssert, "", "this > 100"), schema.ConstraintAssert, ""},
		{"negative threshold", accept(schema.ConstraintCheck, "gt", "this > -5"), schema.ConstraintCheck, "gt"},
	} {
		prof, ok := staticCheckedProfileOf(p.b)
		if !ok {
			t.Fatalf("%s: the admitted fingerprint was REJECTED", p.name)
		}
		if prof.level != p.level || prof.label != p.label {
			t.Fatalf("%s: classified as %+v", p.name, prof)
		}
	}

	mutate := func(fn func(*schema.Bundle)) *schema.Bundle {
		b := accept(schema.ConstraintCheck, "positive", "this > 0")
		fn(b)
		return b
	}
	negatives := []struct {
		name string
		b    *schema.Bundle
	}{
		{"nil bundle", nil},
		{"a second class", mutate(func(b *schema.Bundle) {
			b.Classes = append(b.Classes, schema.ClassDef{Name: schema.Name{Name: "Other"}, Mode: schema.NonStreaming,
				Fields: []schema.ClassField{scalarField("s", stringType())}})
		})},
		{"an enum in the bundle", mutate(func(b *schema.Bundle) {
			b.Enums = []schema.EnumDef{{Name: schema.Name{Name: "E"},
				Values: []schema.EnumValue{{Name: schema.Name{Name: "A"}}}}}
		})},
		{"a recursive-class marker", mutate(func(b *schema.Bundle) { b.RecursiveClasses = []string{"StaticCheckedAnswer"} })},
		{"a structural recursive alias", mutate(func(b *schema.Bundle) {
			b.StructuralRecursiveAliases = []schema.RecursiveAliasDef{{Name: "J", Target: stringType()}}
		})},
		{"a target-level constraint", mutate(func(b *schema.Bundle) {
			b.Target.Meta.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck,
				Expression: "this > 0", Label: label("t")}}
		})},
		{"a streaming target", mutate(func(b *schema.Bundle) { b.Target.Mode = schema.Streaming })},
		{"a scalar target", mutate(func(b *schema.Bundle) { b.Target = intType() })},
		{"a class-level constraint", mutate(func(b *schema.Bundle) {
			b.Classes[0].Constraints = []schema.Constraint{{Level: schema.ConstraintCheck,
				Expression: "this.confidence > 0", Label: label("c")}}
		})},
		{"a third field", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields = append(b.Classes[0].Fields, scalarField("extra", stringType()))
		})},
		{"the two fields in the other order", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[0], b.Classes[0].Fields[1] = b.Classes[0].Fields[1], b.Classes[0].Fields[0]
		})},
		{"an aliased field", mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Name.Alias = label("score") })},
		{"an aliased class", mutate(func(b *schema.Bundle) { b.Classes[0].Name.Alias = label("Answer") })},
		{"a described field", mutate(func(b *schema.Bundle) { b.Classes[0].Fields[0].Description = label("the answer") })},
		{"a @stream annotation on the field", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Type.Meta.Stream.Done = true
		})},
		// Type.Dynamic is documented as meaningful for enums/classes, but it is a field
		// of EVERY Type and ValidateOutput does not reject it on a primitive, so the
		// fingerprint must refuse it on BOTH fields rather than rely on lowering.
		{"a dynamic answer field", mutate(func(b *schema.Bundle) { b.Classes[0].Fields[0].Type.Dynamic = true })},
		{"a dynamic confidence field", mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Dynamic = true })},
		{"a constraint on the OTHER field", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[0].Type.Meta.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck,
				Expression: "this > 0", Label: label("a")}}
		})},
		{"a second constraint", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Type.Meta.Constraints = append(b.Classes[0].Fields[1].Type.Meta.Constraints,
				schema.Constraint{Level: schema.ConstraintCheck, Expression: "this > 1", Label: label("other")})
		})},
		{"duplicate check labels", mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Type.Meta.Constraints = append(b.Classes[0].Fields[1].Type.Meta.Constraints,
				schema.Constraint{Level: schema.ConstraintCheck, Expression: "this > 1", Label: label("positive")})
		})},
		{"a check with no label", mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Meta.Constraints[0].Label = nil })},
		{"a non-ASCII label", mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Meta.Constraints[0].Label = label("positif\u00e9") })},
		{"a nullable confidence", mutate(func(b *schema.Bundle) {
			inner := b.Classes[0].Fields[1].Type
			b.Classes[0].Fields[1].Type = schema.Type{Kind: schema.TypeUnion,
				Union: &schema.UnionType{Variants: []schema.Type{inner}, Nullable: true}}
		})},
		{"a float confidence", mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Primitive = schema.PrimitiveFloat })},
		{"a renamed field", mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Name.Name = "score" })},
	}
	for _, expr := range []string{
		"this >= 0", "this > 0 ", " this > 0", "this>0", "this > 0.0", "this > +5", "this > 007",
		"this > 1_000", "this != 0", "this|length > 0", "this > 9223372036854775808", "this > ",
	} {
		negatives = append(negatives, struct {
			name string
			b    *schema.Bundle
		}{"expression " + strconv.Quote(expr), accept(schema.ConstraintCheck, "positive", expr)})
	}
	for _, n := range negatives {
		if _, ok := staticCheckedProfileOf(n.b); ok {
			t.Errorf("the fingerprint ADMITTED a one-property sibling: %s", n.name)
		}
	}
	if len(negatives) < 20 {
		t.Fatalf("only %d negative rows; the fingerprint's narrowness would be barely witnessed", len(negatives))
	}
}

// TestStaticCheckedThresholdIsAProof pins the statically proven expression profile
// directly, including the round-trip rule that rejects a non-canonical literal.
func TestStaticCheckedThresholdIsAProof(t *testing.T) {
	for expr, want := range map[string]int64{
		"this > 0": 0, "this > 100": 100, "this > -5": -5,
		"this > 9223372036854775807":  9223372036854775807,
		"this > -9223372036854775808": -9223372036854775808,
	} {
		got, ok := staticCheckedThreshold(expr)
		if !ok {
			t.Errorf("%q was rejected by the proven expression profile", expr)
			continue
		}
		if got != want {
			t.Errorf("%q parsed to %d, want %d", expr, got, want)
		}
	}
	for _, expr := range []string{"", "this", "this > ", "this > x", "this > 0x10", "this > 1e3",
		"this > +1", "this > 01", "this > -0", "this > 9223372036854775808", "this < 0"} {
		if n, ok := staticCheckedThreshold(expr); ok {
			t.Errorf("the proven expression profile ADMITTED %q (as %d)", expr, n)
		}
	}
}

// TestStaticCheckedCauseStaysInsideTheTruncationBoundary pins the one length rule the
// assertion renderer has to respect.
//
// Stock's validate_asserts truncates a cause above 100 BYTES and appends `...`
// (internal/debaml/checkedwire's AssertFailCause100 / AssertFailCause101 rows measure
// the boundary). This renderer does NOT reproduce that truncation — the rule interacts
// with Rust's UTF-8-boundary panic, which checkedwire records as an unmeasured hazard —
// so the fingerprint must make an over-long cause unreachable, and the renderer must
// decline if one reaches it anyway.
func TestStaticCheckedCauseStaysInsideTheTruncationBoundary(t *testing.T) {
	// The longest cause the fingerprint can admit is exactly at the boundary, never past
	// it. Stated as arithmetic over the same constants the code uses, so a change to
	// either side is caught here rather than in a fixture nobody re-derives.
	const longestExpr = "this > -9223372036854775808"
	if _, ok := staticCheckedThreshold(longestExpr); !ok {
		t.Fatalf("%q is not admitted, so the bound below is derived from the wrong expression", longestExpr)
	}
	maxLabel := strings.Repeat("z", staticCheckedMaxLabelLen)
	worst := "Failed: " + maxLabel + " " + longestExpr
	if len(worst) != staticCheckedMaxCauseLen {
		t.Fatalf("the longest admissible cause is %d bytes, want exactly %d; the label bound is off",
			len(worst), staticCheckedMaxCauseLen)
	}

	// A label at the bound is admitted and renders; one byte more is DECLINED at the
	// fingerprint, before admission.
	atBound := staticCheckedBundle(schema.ConstraintAssert, maxLabel, longestExpr)
	if _, ok := staticCheckedProfileOf(atBound); !ok {
		t.Fatalf("a %d-byte label was rejected, but its cause is exactly at the boundary", len(maxLabel))
	}
	overBound := staticCheckedBundle(schema.ConstraintAssert, maxLabel+"z", longestExpr)
	if _, ok := staticCheckedProfileOf(overBound); ok {
		t.Fatal("the fingerprint ADMITTED a label whose rendered cause stock would truncate")
	}
	// The same bound applies to a @check label, whose profile shares the ASCII rule.
	if _, ok := staticCheckedProfileOf(staticCheckedBundle(schema.ConstraintCheck, maxLabel+"z", "this > 0")); ok {
		t.Fatal("the fingerprint ADMITTED an over-long @check label")
	}

	// The renderer's BACKSTOP: reached directly, an over-long cause declines rather
	// than being emitted untruncated.
	rendered, err := staticCheckedAssertFailure(staticCheckedConfidenceField, maxLabel+"z", longestExpr)
	if err == nil {
		t.Fatalf("the renderer emitted an over-long cause: %v", rendered)
	}
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("the renderer returned %v; an unprovable cause length must carry the decline sentinel", err)
	}
	// CONTROL: the boundary-length cause renders, so the rejection above is about the
	// extra byte rather than about the renderer refusing long input generally.
	ok, rerr := staticCheckedAssertFailure(staticCheckedConfidenceField, maxLabel, longestExpr)
	if rerr != nil {
		t.Fatalf("the renderer refused a cause exactly at the boundary: %v", rerr)
	}
	if !strings.Contains(ok.Error(), "Failed: "+maxLabel+" "+longestExpr) {
		t.Fatalf("the boundary-length cause was not rendered verbatim: %v", ok)
	}
}

// TestStaticCheckedDuplicateKeysMatchStock pins what the mapper does when a canonical
// field arrives TWICE — the one input shape where "which occurrence wins" is decidable
// and could silently change the value and the check status a caller receives.
//
// The answer is STOCK's, measured: internal/debaml/checkedwire drives the same
// declaration and the same raw text through the real CFFI and pins
// `wireNestedCheckDuplicateKey`, which keeps the FIRST occurrence. The mapper must
// produce those exact bytes.
//
// The second half is why no mapper-local duplicate rejection was added: which
// occurrence wins is decided by the SHARED coercion, so the constrained and
// unconstrained lanes must agree for identical input. Rejecting duplicates here would
// have made them disagree.
func TestStaticCheckedDuplicateKeysMatchStock(t *testing.T) {
	const raw = `{"answer": "first", "answer": "second", "confidence": 9}`

	b := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")
	prof, _ := staticCheckedProfileOf(b)
	got, err := staticCheckedMap(b, prof, raw)
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	if string(got.JSON) != staticCheckedWireDuplicateKey {
		t.Fatalf("duplicate-key bytes:\n got %s\nwant %s", got.JSON, staticCheckedWireDuplicateKey)
	}
	// DISCRIMINATING: the losing occurrence is nowhere in the output, so a mapper that
	// took the last one would fail here rather than pass on a superset.
	if strings.Contains(string(got.JSON), "second") {
		t.Fatalf("the mapper kept the SECOND occurrence: %s", got.JSON)
	}

	// The UNCONSTRAINED twin through the real static serving entry point.
	stripped := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")
	stripped.Classes[0].Fields[1].Type.Meta.Constraints = nil
	plain, perr := ParseStaticBundle(context.Background(), stripped, raw)

	if perr != nil {
		t.Fatalf("the unconstrained lane declined (%v) but the mapper served the stock bytes; the two "+
			"lanes must agree about which occurrence wins", perr)
	}
	// The mapper's output must be the unconstrained bytes with the ONE constrained
	// member wrapped, and nothing else moved.
	members, serr := staticCheckedSplit(plain.JSON)
	if serr != nil {
		t.Fatalf("split the unconstrained output: %v", serr)
	}
	if len(members) != 2 {
		t.Fatalf("the unconstrained lane emitted %d members: %s", len(members), plain.JSON)
	}
	carrier, cerr := bamlutils.NewChecked(int64(9), []bamlutils.Check{{
		Name: "positive", Expression: "this > 0", Status: bamlutils.CheckSucceeded}})
	if cerr != nil {
		t.Fatalf("build the expected carrier: %v", cerr)
	}
	wrapped, merr := sonic.Marshal(carrier)
	if merr != nil {
		t.Fatalf("marshal the expected carrier: %v", merr)
	}
	members[1].raw = wrapped
	want, jerr := staticCheckedJoin(members)
	if jerr != nil {
		t.Fatalf("rebuild the expected output: %v", jerr)
	}
	if string(got.JSON) != string(want) {
		t.Fatalf("the mapper changed something other than the constrained member:\n got %s\nwant %s",
			got.JSON, want)
	}
}

// ---------------------------------------------------------------------------
// The non-admitting seam
// ---------------------------------------------------------------------------

// staticCheckedGate is one PRODUCTION gate the four companion rows are driven through,
// in both seam states.
//
// There is no `runExempt` twin and no stand-in: `run` is the production function, and
// the two states differ only in whether [OpenStaticCheckedSeamForTest] has been called.
// `wantOpen` records what that function is expected to do once the seam is open — which
// is not "admit" for all of them, and saying so per gate is the point:
//
//   - the SHAPE gates (checkSupported, checkSupportedFields, checkSupportedType) are the
//     generic constraint cut-line and are NOT what the checked-static seam moves; they
//     keep declining, and 7.2b-3 exempts them explicitly.
//   - the SUPPORT predicate (SupportsNativeFinalBundle) answers a question about the
//     SHAPE, so it admits — and that is what makes nativeserve's real admission gate
//     admit too, which its own package asserts.
//   - the DIRECT parse routes (ParseStaticBundle, root Parse) keep DECLINING even with
//     the seam open, because the capability is a property of the route. That is the
//     scope's "static unary /call final parsing only" boundary, measured.
//   - the STATIC UNARY /call route (ParseStaticBundleUnaryCall) SERVES.
type staticCheckedGate struct {
	name string
	// run is the production gate exactly as it ships.
	run func(*schema.Bundle) error
	// wantOpen is the disposition this gate must reach once the seam is open.
	wantOpen staticCheckedDisposition
}

// staticCheckedDisposition is what a gate did with a bundle.
type staticCheckedDisposition string

const (
	// staticCheckedDeclined: the fallback sentinel — BAML serves.
	staticCheckedDeclined staticCheckedDisposition = "declined"
	// staticCheckedAdmitted: no error, and (for a parse gate) bytes were produced.
	staticCheckedAdmitted staticCheckedDisposition = "admitted"
	// staticCheckedClaimedFailure: a non-sentinel error — a CLAIMED parse failure, which
	// is what a false @assert is.
	staticCheckedClaimedFailure staticCheckedDisposition = "claimed-failure"
)

// staticCheckedDispositionOf classifies one gate result.
func staticCheckedDispositionOf(err error) staticCheckedDisposition {
	switch {
	case err == nil:
		return staticCheckedAdmitted
	case errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported):
		return staticCheckedDeclined
	default:
		return staticCheckedClaimedFailure
	}
}

// staticCheckedParseGate drives a bundle through a parse entry point with the row's own
// raw text and reports "admitted" only when it produced bytes.
//
// The row's raw text matters: the assert_fail row's admitted outcome is a CLAIMED
// failure with no bytes, and a gate helper that hardcoded a passing input could not tell
// that apart from a decline.
func staticCheckedParseGate(
	parse func(context.Context, *schema.Bundle, string) (bamlutils.DeBAMLParseResult, error),
	raw string,
) func(*schema.Bundle) error {
	return func(b *schema.Bundle) error {
		res, err := parse(context.Background(), b, raw)
		if err != nil {
			return err
		}
		if len(res.JSON) == 0 {
			return fmt.Errorf("served no bytes and no error")
		}
		return nil
	}
}

// staticCheckedRootParse drives root [Parse] over a static descriptor — the DIRECT parse
// endpoint. It is the lane the scope requires stay declined even after the cutover, and
// it reaches [ParseStaticBundle] through the same routing production uses, which is why
// it is driven by name rather than approximated.
func staticCheckedRootParse(ctx context.Context, b *schema.Bundle, raw string) (bamlutils.DeBAMLParseResult, error) {
	fn := promptdescriptor.Function{Return: staticCheckedReturnDescriptor(b)}
	return Parse(ctx, bamlutils.DeBAMLParseRequest{StaticStreamDescriptor: &fn, Raw: raw})
}

// staticCheckedGateSet is EVERY production gate the companion rows are driven through,
// with the disposition each must reach once the seam is open.
func staticCheckedGateSet(row staticCheckedRow) []staticCheckedGate {
	typeNode := func(b *schema.Bundle) schema.Type { return b.Classes[0].Fields[1].Type }
	// The static unary /call route's open disposition follows the ROW: three rows serve
	// bytes, and the false-assert row reaches a CLAIMED failure with no value.
	unaryOpen := staticCheckedAdmitted
	if row.wantErr != "" {
		unaryOpen = staticCheckedClaimedFailure
	}
	return []staticCheckedGate{{
		name: "checkSupported",
		run:  checkSupported, wantOpen: staticCheckedDeclined,
	}, {
		name: "checkSupportedFields",
		run:  checkSupportedFields, wantOpen: staticCheckedDeclined,
	}, {
		name:     "checkSupportedType",
		run:      func(b *schema.Bundle) error { return checkSupportedType(b, typeNode(b)) },
		wantOpen: staticCheckedDeclined,
	}, {
		name: "SupportsNativeFinalBundle",
		run:  SupportsNativeFinalBundle, wantOpen: staticCheckedAdmitted,
	}, {
		name:     "ParseStaticBundle (direct)",
		run:      staticCheckedParseGate(ParseStaticBundle, row.raw),
		wantOpen: staticCheckedDeclined,
	}, {
		name:     "Parse (root, static descriptor)",
		run:      staticCheckedParseGate(staticCheckedRootParse, row.raw),
		wantOpen: staticCheckedDeclined,
	}, {
		name:     "ParseStaticBundleUnaryCall (static unary /call)",
		run:      staticCheckedParseGate(ParseStaticBundleUnaryCall, row.raw),
		wantOpen: unaryOpen,
	}}
}

// staticCheckedReturnDescriptor lowers one of the two narrow fixture bundles back into
// the descriptor shape a generated static method carries, so root [Parse] can be driven
// over the SAME shape the other gates see.
//
// It mirrors only what the fingerprint admits — a two-field class with one direct
// constraint — and FAILS on anything else rather than silently producing a different
// shape, which would make the direct-parse assertion about a bundle nobody else drove.
func staticCheckedReturnDescriptor(b *schema.Bundle) schemadescriptor.Bundle {
	if len(b.Classes) != 1 || len(b.Classes[0].Fields) != 2 {
		panic("static checked descriptor: not the narrow two-field fixture shape")
	}
	cls := b.Classes[0]
	fields := make([]schemadescriptor.ClassField, 0, len(cls.Fields))
	for _, f := range cls.Fields {
		constraints := make([]schemadescriptor.Constraint, 0, len(f.Type.Meta.Constraints))
		for _, c := range f.Type.Meta.Constraints {
			constraints = append(constraints, schemadescriptor.Constraint{
				Level:      schemadescriptor.ConstraintLevel(c.Level),
				Expression: c.Expression,
				Label:      c.Label,
			})
		}
		fields = append(fields, schemadescriptor.ClassField{
			Name: schemadescriptor.Name{Name: f.Name.Name},
			Type: schemadescriptor.Type{
				Kind:      schemadescriptor.TypePrimitive,
				Primitive: schemadescriptor.PrimitiveKind(f.Type.Primitive),
				// Dynamic is carried, not dropped. Silently losing a schema fact here
				// would make the root-Parse arm parse a DIFFERENT (clean) shape than the
				// one every other gate sees, and a decline it produced would then be
				// attributable to the wrong thing.
				Dynamic: f.Type.Dynamic,
				Meta:    schemadescriptor.TypeMeta{Constraints: constraints},
			},
		})
	}
	return schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "StaticCheckedFixture",
		Target: schemadescriptor.Type{
			Kind: schemadescriptor.TypeClass, Name: cls.Name.Name,
			Mode: schemadescriptor.StreamingMode(schema.NonStreaming),
		},
		Classes: []schemadescriptor.ClassDef{{
			Name:   schemadescriptor.Name{Name: cls.Name.Name},
			Mode:   schemadescriptor.StreamingMode(schema.NonStreaming),
			Fields: fields,
		}},
	}
}

// TestStaticCheckedSeamStillDeclines is the load-bearing invariant of Slice 7.2b-2:
// the mapper and the generated types exist, and NOTHING admits them.
//
// Every gate is the PRODUCTION function, driven in the DEFAULT (closed) state.
func TestStaticCheckedSeamStillDeclines(t *testing.T) {
	if staticCheckedAdmitsConstraints {
		t.Fatal("the non-admitting seam constant is OPEN; that is the 7.2b-3 cutover, not this slice")
	}
	if staticCheckedSeamOpen.Load() {
		t.Fatal("the descriptor-specific test seam is open at the start of the run; the default must be DENY")
	}
	// The capability half: the ONE granting constructor grants nothing by default, so no
	// route — including the static unary /call route — can claim anything.
	if staticCheckedGrantStaticUnaryCall().admits() {
		t.Fatal("the static-unary-call capability is granted by default; nothing may admit in this slice")
	}
	if staticCheckedDirect().admits() {
		t.Fatal("the DIRECT capability admits; direct routes must never claim the fingerprint")
	}

	rows := staticCheckedRows()
	if len(rows) != 4 {
		t.Fatalf("%d companion rows, want the 4 named by the scope", len(rows))
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			b := r.bundle()
			// The row IS the admitted fingerprint — otherwise the decline below would be
			// witnessing a shape the cutover does not target.
			if _, ok := staticCheckedProfileOf(b); !ok {
				t.Fatal("the row is not the admitted fingerprint; its decline would witness nothing")
			}
			gates := staticCheckedGateSet(r)
			if len(gates) < 7 {
				t.Fatalf("%d gates are driven; every named production gate must be covered", len(gates))
			}
			for _, g := range gates {
				if got := staticCheckedDispositionOf(g.run(b)); got != staticCheckedDeclined {
					t.Errorf("%s: %s with the seam CLOSED, want declined", g.name, got)
				}
			}
			// ATTRIBUTION: the constraint is what causes it. A FRESH twin of the same
			// class with the constraint removed must be ADMITTED, or the decline would
			// be about the shape.
			stripped := r.bundle()
			stripped.Classes[0].Fields[1].Type.Meta.Constraints = nil
			if err := checkSupported(stripped); err != nil {
				t.Fatalf("the constraint-stripped twin is ALSO declined (%v); this row's decline is not "+
					"attributable to its constraint", err)
			}
		})
	}
}

// TestStaticCheckedSeamOpenAdmitsThroughTheRealGates is the anti-false-green control for
// the test above, and it executes PRODUCTION code in both states.
//
// It opens the descriptor-specific seam and re-runs the SAME production gates over the
// SAME four rows. Each must reach the disposition its gate declares:
//
//   - SupportsNativeFinalBundle ADMITS (which is what makes nativeserve's real admission
//     gate admit — asserted in that module's own package);
//   - ParseStaticBundleUnaryCall SERVES stock's bytes, or reaches the claimed assertion
//     failure for the false-assert row;
//   - ParseStaticBundle, root Parse and the three shape gates KEEP DECLINING.
//
// The last point is not a weaker result — it is the scope's `/call`-only boundary, and
// proving it under an OPEN seam is strictly stronger than proving it under a closed one,
// where everything declines for free.
func TestStaticCheckedSeamOpenAdmitsThroughTheRealGates(t *testing.T) {
	restore := OpenStaticCheckedSeamForTest()
	defer restore()

	if !staticCheckedGrantStaticUnaryCall().admits() {
		t.Fatal("the seam is open but the static-unary-call capability is still denied; the open state " +
			"would prove nothing")
	}
	if staticCheckedDirect().admits() {
		t.Fatal("opening the seam granted the DIRECT capability; the /call-only boundary is gone")
	}

	moved := 0
	for _, r := range staticCheckedRows() {
		t.Run(r.name, func(t *testing.T) {
			b := r.bundle()
			for _, g := range staticCheckedGateSet(r) {
				got := staticCheckedDispositionOf(g.run(b))
				if got != g.wantOpen {
					t.Errorf("%s: %s with the seam OPEN, want %s", g.name, got, g.wantOpen)
					continue
				}
				if g.wantOpen != staticCheckedDeclined {
					moved++
				}
			}

			// The bytes the OPEN static unary route serves are stock's, so the open state
			// is a real serve rather than merely a different error.
			res, err := ParseStaticBundleUnaryCall(context.Background(), b, r.raw)
			if r.wantErr != "" {
				if !staticCheckedIsAssertFailure(err) {
					t.Fatalf("the open unary route returned %v, want the rendered stock assertion failure", err)
				}
				if got := err.Error(); got != r.wantErr {
					t.Fatalf("assertion error bytes:\n got %s\nwant %s", strconv.Quote(got), strconv.Quote(r.wantErr))
				}
				if len(res.JSON) != 0 {
					t.Fatalf("the open unary route produced %s bytes for a false @assert", res.JSON)
				}
				return
			}
			if err != nil {
				t.Fatalf("the open unary route did not serve: %v", err)
			}
			if got := string(res.JSON); got != r.wantJSON {
				t.Fatalf("the open unary route's bytes:\n got %s\nwant %s", got, r.wantJSON)
			}
		})
	}
	if moved == 0 {
		t.Fatal("no gate changed disposition when the seam opened; TestStaticCheckedSeamStillDeclines " +
			"proves nothing")
	}

	// NEGATIVE control: a bundle OUTSIDE the fingerprint stays declined by EVERY gate
	// even with the seam open, so the movement above is caused by the fingerprint rather
	// than by a seam that lifts the constraint cut-line generally.
	outside := staticCheckedBundle(schema.ConstraintCheck, "positive", "this|length > 0")
	for _, g := range staticCheckedGateSet(staticCheckedRows()[0]) {
		if got := staticCheckedDispositionOf(g.run(outside)); got != staticCheckedDeclined {
			t.Errorf("%s: %s for a bundle OUTSIDE the fingerprint with the seam open, want declined",
				g.name, got)
		}
	}
}

// staticCheckedDynamicVariants are the two one-property siblings of the admitted
// fingerprint that carry [schema.Type.Dynamic] on a primitive field.
func staticCheckedDynamicVariants() []struct {
	name  string
	field int
} {
	return []struct {
		name  string
		field int
	}{{"dynamic answer", 0}, {"dynamic confidence", 1}}
}

// staticCheckedDynamicBundle builds the admitted fingerprint with Dynamic set on one
// field.
func staticCheckedDynamicBundle(field int) *schema.Bundle {
	b := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")
	b.Classes[0].Fields[field].Type.Dynamic = true
	return b
}

// TestStaticCheckedDynamicFieldDeclinesAtTheBundleIngress closes the one hole the
// fingerprint had, at the ingress the guard actually governs.
//
// [schema.Type.Dynamic] is documented as meaningful for enums and classes, and ordinary
// static-descriptor lowering rejects it outright on a primitive — but it is a field of
// every `Type` and [schema.Bundle.ValidateOutput] does not reject it, so a caller holding
// a hand-built Bundle can present it at the root-owned [SupportsNativeFinalBundle] /
// [ParseStaticBundle] boundary. Nothing has measured what stock does for that variant, so
// it has no byte capture behind it and must not be inside the fingerprint.
//
// SCOPE, stated rather than implied. This test is about the BUNDLE ingress. The gates
// whose answer the `!f.Type.Dynamic` guard actually decides are the three named in
// staticCheckedDynamicAttributableGates; the DESCRIPTOR ingress declines for an entirely
// separate reason (lowering refuses `dynamic` as a stray payload on a primitive) and is
// proven on its own below, so it is deliberately not claimed here.
func TestStaticCheckedDynamicFieldDeclinesAtTheBundleIngress(t *testing.T) {
	// CONTROL: the same bundle WITHOUT the dynamic bit is the admitted fingerprint, so a
	// rejection below is attributable to that one bit and not to a malformed fixture.
	if _, ok := staticCheckedProfileOf(staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")); !ok {
		t.Fatal("the non-dynamic control is not the admitted fingerprint; every rejection below is vacuous")
	}

	row := staticCheckedRows()[0]
	assertDeclined := func(t *testing.T, state string) {
		t.Helper()
		for _, v := range staticCheckedDynamicVariants() {
			b := staticCheckedDynamicBundle(v.field)
			if _, ok := staticCheckedProfileOf(b); ok {
				t.Errorf("%s: the fingerprint ADMITTED a %s field; that variant has no byte capture behind it",
					state, v.name)
			}
			// The ATTRIBUTABLE gates: these are the ones whose disposition the field
			// guard decides, and the ones that reopen if it is removed.
			for _, g := range staticCheckedDynamicAttributableGates() {
				if got := staticCheckedDispositionOf(g.run(b)); got != staticCheckedDeclined {
					t.Errorf("%s: %s: %s from %s, want declined", state, v.name, got, g.name)
				}
			}
			// The REMAINING production gates must decline too — they just do so for
			// reasons the guard does not own (the generic constraint cut-line, the route
			// capability, descriptor lowering), so they are checked without being
			// claimed as evidence for the guard.
			for _, g := range staticCheckedGateSet(row) {
				if got := staticCheckedDispositionOf(g.run(b)); got != staticCheckedDeclined {
					t.Errorf("%s: %s: %s from %s, want declined", state, v.name, got, g.name)
				}
			}
		}
	}

	assertDeclined(t, "seam closed")

	restore := OpenStaticCheckedSeamForTest()
	defer restore()
	// Non-vacuity: with the seam open the NON-dynamic twin DOES move through every
	// attributable gate, so the declines above are the dynamic bit's doing rather than a
	// seam that opened nothing.
	clean := row.bundle()
	for _, g := range staticCheckedDynamicAttributableGates() {
		if got := staticCheckedDispositionOf(g.run(clean)); got == staticCheckedDeclined {
			t.Fatalf("with the seam open, %s still declined the NON-dynamic fingerprint; the open-state "+
				"assertions below would be vacuous", g.name)
		}
	}
	assertDeclined(t, "seam OPEN")
}

// staticCheckedDynamicAttributableGates are the production gates whose answer the
// `!f.Type.Dynamic` guard decides — the ones that admit the variant if it is removed.
//
// They are named explicitly, and separately from [staticCheckedGateSet], because the
// other gates decline a dynamic bundle for reasons the guard does not own: the three
// shape gates on the constraint itself, the direct parse routes on the route capability,
// and root Parse on descriptor lowering. Counting those as evidence would let an
// unrelated guard produce a false green for this one.
func staticCheckedDynamicAttributableGates() []staticCheckedGate {
	return []staticCheckedGate{{
		name: "SupportsNativeFinalBundle",
		run:  SupportsNativeFinalBundle,
	}, {
		name: "ParseStaticBundleUnaryCall (static unary /call)",
		run: staticCheckedParseGate(ParseStaticBundleUnaryCall,
			`{"answer": "sunny", "confidence": 9}`),
	}}
}

// TestStaticCheckedDynamicFieldDeclinesAtTheDescriptorIngress is the OTHER ingress, and
// it is proven separately because it declines for a different reason.
//
// A generated static method arrives as a [schemadescriptor.Bundle], and
// [schema.FromStaticDescriptor] refuses `dynamic` as a stray payload on a primitive — so
// root [Parse] never reaches the fingerprint at all. That is a real and independent
// defence, but it is NOT the `!f.Type.Dynamic` guard, so it is asserted here on its own
// terms rather than folded into the bundle-ingress bite.
//
// Both seam states are driven: the seam cannot matter for a descriptor that never lowers,
// and showing that is the point.
func TestStaticCheckedDynamicFieldDeclinesAtTheDescriptorIngress(t *testing.T) {
	assert := func(t *testing.T, state string) {
		t.Helper()
		for _, v := range staticCheckedDynamicVariants() {
			b := staticCheckedDynamicBundle(v.field)
			desc := staticCheckedReturnDescriptor(b)
			// The descriptor really carries the bit — otherwise this test would drive a
			// clean shape and prove nothing about the dynamic one.
			if !desc.Classes[0].Fields[v.field].Type.Dynamic {
				t.Fatalf("%s: the %s descriptor does not carry Dynamic; the assertion below would be "+
					"about a different shape", state, v.name)
			}
			if _, lerr := schema.FromStaticDescriptor(desc); lerr == nil {
				t.Fatalf("%s: %s: descriptor lowering ACCEPTED a dynamic primitive; this ingress's "+
					"defence would be gone", state, v.name)
			}
			res, err := staticCheckedRootParse(context.Background(), b, `{"answer": "sunny", "confidence": 9}`)
			if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Errorf("%s: %s: root Parse returned %v, want the decline sentinel", state, v.name, err)
			}
			if len(res.JSON) != 0 {
				t.Errorf("%s: %s: root Parse declined but produced %s bytes", state, v.name, res.JSON)
			}
		}
		// CONTROL: the NON-dynamic descriptor lowers cleanly, so the failures above are
		// the dynamic bit's and not a descriptor helper that produces junk.
		clean := staticCheckedReturnDescriptor(staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0"))
		if _, lerr := schema.FromStaticDescriptor(clean); lerr != nil {
			t.Fatalf("%s: the non-dynamic control descriptor does not lower (%v); every assertion above "+
				"is vacuous", state, lerr)
		}
	}

	assert(t, "seam closed")
	restore := OpenStaticCheckedSeamForTest()
	defer restore()
	assert(t, "seam OPEN")
}

// TestStaticCheckedOpenSeamNeverFallsThroughToTheBlindPath pins the safety property the
// open seam creates and the direct routes must not lose.
//
// Once the seam is open the support predicate answers "supported" for the fingerprint on
// EVERY route, so a direct route that merely failed to claim would fall through to the
// ordinary extract → coerce path — which knows nothing about constraints and would serve
// `{"answer":…,"confidence":9}` with no carrier and no assertion. That is the one way
// this seam could turn into an over-claim, so it is asserted as bytes rather than as a
// disposition.
func TestStaticCheckedOpenSeamNeverFallsThroughToTheBlindPath(t *testing.T) {
	restore := OpenStaticCheckedSeamForTest()
	defer restore()

	b := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")
	const raw = `{"answer": "sunny", "confidence": 9}`
	// The support predicate DOES admit, which is the precondition that makes the
	// fall-through possible.
	if err := SupportsNativeFinalBundle(b); err != nil {
		t.Fatalf("the open seam did not admit support (%v); this hazard would be unreachable and the "+
			"assertion below vacuous", err)
	}
	for _, tc := range []struct {
		name  string
		parse func(context.Context, *schema.Bundle, string) (bamlutils.DeBAMLParseResult, error)
	}{
		{"ParseStaticBundle", ParseStaticBundle},
		{"root Parse", staticCheckedRootParse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.parse(context.Background(), b, raw)
			if err == nil {
				t.Fatalf("the direct route SERVED %s with the seam open; a constraint-blind serve of a "+
					"checked shape is an over-claim", res.JSON)
			}
			if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("the direct route returned %v; it must DECLINE, not fail", err)
			}
			if len(res.JSON) != 0 {
				t.Fatalf("the direct route declined but produced %s bytes", res.JSON)
			}
		})
	}
}

// TestStaticCheckedSeamHasNoProductionWriter is the structural half of "the seam is DENY
// by default": no production file may write it, so no request can open it.
func TestStaticCheckedSeamHasNoProductionWriter(t *testing.T) {
	root := staticCheckedSourcePath(t, "")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	scanned, writers := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		file := staticCheckedParseSource(t, filepath.Join(root, name))
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "staticCheckedSeamOpen" {
				return true
			}
			// Reads are expected (the grant consults it). Only the mutators matter.
			switch sel.Sel.Name {
			case "Store", "Swap", "CompareAndSwap":
				// The ONE permitted writer is the exported test opener itself.
				if name == "checked_static.go" {
					return true
				}
				writers++
				t.Errorf("%s writes the checked-static seam via %s; a production writer could open it "+
					"for a real request", name, sel.Sel.Name)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no production files were scanned; this guard would be vacuous")
	}
	if writers != 0 {
		t.Fatalf("%d production writer(s) found", writers)
	}
	// And the opener is loud about nesting, so one test's closer cannot end another's
	// open state.
	restore := OpenStaticCheckedSeamForTest()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a nested OpenStaticCheckedSeamForTest did not panic")
			}
		}()
		_ = OpenStaticCheckedSeamForTest()
	}()
	restore()
	if staticCheckedSeamOpen.Load() {
		t.Fatal("the closer did not close the seam")
	}
}

// TestStaticCheckedFingerprintPinsTheFixtureIdentity pins that the admitted shape is the
// two CONCRETE generated fixture return types, not any class with the same two fields.
//
// The byte proof is per-fixture: the stock captures in internal/debaml/checkedwire were
// taken from these exact declarations, so a differently-named class has no capture
// behind it and must decline. The level pairing is part of the identity too.
func TestStaticCheckedFingerprintPinsTheFixtureIdentity(t *testing.T) {
	// The two names are the ones the generated fixture project declares.
	if staticCheckedCheckClass != "StaticCheckedAnswer" || staticCheckedAssertClass != "StaticAssertAnswer" {
		t.Fatalf("the pinned identities are %q/%q; they must name the generated fixture return types",
			staticCheckedCheckClass, staticCheckedAssertClass)
	}
	rename := func(level schema.ConstraintLevel, name string) *schema.Bundle {
		b := staticCheckedBundle(level, "positive", "this > 0")
		b.Classes[0].Name.Name = name
		b.Target.Name = name
		if err := b.RebuildIndexes(); err != nil {
			t.Fatalf("rebuild indexes: %v", err)
		}
		return b
	}
	for _, tc := range []struct {
		what  string
		level schema.ConstraintLevel
		name  string
	}{
		{"a renamed check class", schema.ConstraintCheck, "SomeOtherAnswer"},
		{"a renamed assert class", schema.ConstraintAssert, "SomeOtherAnswer"},
		// The CROSSED pairing: the check fixture's shape under the assert fixture's
		// name and vice versa. Neither has a capture behind it.
		{"the check level on the assert class", schema.ConstraintCheck, staticCheckedAssertClass},
		{"the assert level on the check class", schema.ConstraintAssert, staticCheckedCheckClass},
	} {
		if _, ok := staticCheckedProfileOf(rename(tc.level, tc.name)); ok {
			t.Errorf("the fingerprint ADMITTED %s (%q)", tc.what, tc.name)
		}
	}
	// CONTROL: the two correctly-named shapes are admitted, so the rejections above are
	// about the name and not about a fingerprint that matches nothing.
	for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
		if _, ok := staticCheckedProfileOf(staticCheckedBundle(level, "positive", "this > 0")); !ok {
			t.Fatalf("the correctly-named %s fixture was rejected; every rejection above is vacuous", level)
		}
	}
}

// TestStaticCheckedSeamIsTheOnlySwitch pins STRUCTURALLY that the cutover cannot be
// made by a runtime assignment: the seam is an untyped boolean CONSTANT and it is false.
func TestStaticCheckedSeamIsTheOnlySwitch(t *testing.T) {
	file := staticCheckedParseSource(t, staticCheckedSourcePath(t, "checked_static.go"))
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		gen, ok := n.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			return true
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "staticCheckedAdmitsConstraints" {
				continue
			}
			found = true
			if len(vs.Values) != 1 {
				t.Fatal("the seam constant has no single initialiser")
			}
			lit, ok := vs.Values[0].(*ast.Ident)
			if !ok || lit.Name != "false" {
				t.Fatalf("the seam constant is initialised to %v, want the literal false", vs.Values[0])
			}
		}
		return true
	})
	if !found {
		t.Fatal("staticCheckedAdmitsConstraints is not declared as a CONSTANT in checked_static.go; a " +
			"mutable switch could be flipped by production code at runtime")
	}
	// The DIRECT parse entry point must pass the DIRECT capability, and NO entry point
	// reached by a direct route may name the granting constructor. That is the structural
	// half of "the cutover is not a constant flip": the only production caller of the
	// grant is the static-unary /call route.
	callsIn := func(rel string) map[string]int {
		src := staticCheckedParseSource(t, staticCheckedSourcePath(t, rel))
		out := map[string]int{}
		ast.Inspect(src, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					out[id.Name]++
				}
			}
			return true
		})
		return out
	}
	direct := callsIn("parse_static.go")
	if direct["staticCheckedDirect"] != 1 {
		t.Errorf("parse_static.go passes the DIRECT capability %d time(s), want exactly 1",
			direct["staticCheckedDirect"])
	}
	if direct["staticCheckedGrantStaticUnaryCall"] != 0 {
		t.Errorf("parse_static.go grants the static-unary-call capability; ParseStaticBundle is reached " +
			"by root Parse, the shadow comparator and the stream-final lane")
	}
	// The one production route that DOES grant it is the static unary /call route, and it
	// lives beside the seam it opens.
	seam := callsIn("checked_static.go")
	if seam["staticCheckedGrantStaticUnaryCall"] == 0 {
		t.Error("no production route calls staticCheckedGrantStaticUnaryCall; the capability would be " +
			"inert and the open-state proof would be about code nothing uses")
	}
}

// ---------------------------------------------------------------------------
// The stock-authority agreement guard
// ---------------------------------------------------------------------------

// TestStaticCheckedStockAuthorityAgrees proves each literal this file compares against
// is byte-identical to the constant internal/debaml/checkedwire captured from the real
// CFFI.
//
// Without it the untagged proof above would be self-referential: it would show the
// mapper reproduces bytes THIS FILE declares, not bytes stock produced. The guard runs
// untagged, so a CGO-free run still fails if the two copies part company.
func TestStaticCheckedStockAuthorityAgrees(t *testing.T) {
	authority := map[string]string{}
	for _, name := range []string{"wire_test.go"} {
		file := staticCheckedParseSource(t, staticCheckedSourcePath(t, filepath.Join("checkedwire", name)))
		ast.Inspect(file, func(n ast.Node) bool {
			gen, ok := n.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				return true
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				lit, ok := vs.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				authority[vs.Names[0].Name] = unquoted
			}
			return true
		})
	}
	if len(authority) == 0 {
		t.Fatal("no string constants were read from internal/debaml/checkedwire; this guard would be vacuous")
	}
	mine := map[string]string{
		"staticCheckedWireNestedPass":   staticCheckedWireNestedPass,
		"staticCheckedWireNestedFail":   staticCheckedWireNestedFail,
		"staticCheckedWireAssertPass":   staticCheckedWireAssertPass,
		"staticCheckedAssertFailBytes":  staticCheckedAssertFailBytes,
		"staticCheckedWireDuplicateKey": staticCheckedWireDuplicateKey,
	}
	if len(mine) != len(staticCheckedAuthorityPairs) {
		t.Fatalf("%d literals are compared but %d pairings are declared; a literal would be unguarded",
			len(mine), len(staticCheckedAuthorityPairs))
	}
	for local, remote := range staticCheckedAuthorityPairs {
		want, ok := authority[remote]
		if !ok {
			t.Errorf("internal/debaml/checkedwire no longer declares %s, so %s has no stock authority",
				remote, local)
			continue
		}
		got, ok := mine[local]
		if !ok {
			t.Errorf("%s is paired with %s but is not among the compared literals", local, remote)
			continue
		}
		if got != want {
			t.Errorf("%s has drifted from the stock capture %s:\n got %s\nwant %s",
				local, remote, strconv.Quote(got), strconv.Quote(want))
		}
	}
	// Non-vacuity: the guard must actually be able to tell them apart.
	if staticCheckedWireNestedPass == staticCheckedWireNestedFail {
		t.Fatal("the pass and fail literals are identical")
	}
}

// staticCheckedSourcePath resolves a path relative to this package's directory.
func staticCheckedSourcePath(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), rel)
}

// staticCheckedParseSource parses one file, FAILING rather than skipping on an error:
// a file a guard cannot read is not evidence of compliance. Build tags do not affect
// the parser, so the integration-tagged capture is readable from an untagged run.
func staticCheckedParseSource(t *testing.T, path string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return f
}
