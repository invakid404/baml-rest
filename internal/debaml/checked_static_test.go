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
	"reflect"
	"runtime"
	"sort"
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
	// An EMPTY label argument means ABSENT — a nil pointer. That collapse is a
	// convenience of this builder and NOT a property of the schema: a
	// pointer-to-empty-string is a DIFFERENT, present-but-empty label, which
	// [staticCheckedBundleLabelPtr] builds and the sibling corpus drives. Keeping the
	// two constructible separately is what makes the presence rule testable at all —
	// while this builder was the only one, the present-empty case could not be written.
	if label == "" {
		return staticCheckedBundleLabelPtr(level, nil, expr)
	}
	l := label
	return staticCheckedBundleLabelPtr(level, &l, expr)
}

// staticCheckedBundleLabelPtr builds the fingerprint with the constraint label given as
// a POINTER, so an absent label (nil) and a present-but-empty one (&"") are distinct
// inputs rather than the same one.
func staticCheckedBundleLabelPtr(level schema.ConstraintLevel, label *string, expr string) *schema.Bundle {
	c := schema.Constraint{Level: level, Expression: expr, Label: label}
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
			// The collector records production's verdict verbatim (checkSupported), and
			// since the 7.2b-3 cutover that gate answers the ONE fingerprint like every
			// other schema gate — so an admitted row must be ADMITTED here. A decline
			// would mean the collector is witnessing a schema production no longer
			// classifies the way the mapper does.
			if run.ProductionSupport != nil {
				t.Fatalf("the collector recorded ProductionSupport=%v for an admitted row; the schema "+
					"gates share one fingerprint, so this must be nil", run.ProductionSupport)
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

// staticCheckedSibling is one ONE-PROPERTY sibling of the admitted fingerprint: a
// bundle that differs from an accepted one in exactly ONE property, so a rejection is
// attributable to that property rather than to the fixture being generally malformed.
type staticCheckedSibling struct {
	name string
	b    *schema.Bundle
}

// staticCheckedSiblings is the shared sibling corpus.
//
// It is shared DELIBERATELY: [TestStaticCheckedProfileAdmitsOnlyTheFingerprint] proves
// the classifier rejects each one, and [TestStaticCheckedGatesShareOneFingerprint]
// proves EVERY production gate does too. Two separate corpora would let the second
// claim drift into covering fewer shapes than the first.
func staticCheckedSiblings() []staticCheckedSibling {
	label := func(s string) *string { return &s }
	accept := func(level schema.ConstraintLevel, l, e string) *schema.Bundle {
		return staticCheckedBundle(level, l, e)
	}
	mutate := func(fn func(*schema.Bundle)) *schema.Bundle {
		b := accept(schema.ConstraintCheck, "positive", "this > 0")
		fn(b)
		return b
	}
	// NOTE: a nil bundle is NOT here. It is a fingerprint-classifier case
	// (TestStaticCheckedProfileAdmitsOnlyTheFingerprint asserts it directly); the
	// package-internal shape gates take a non-nil bundle by contract and would panic,
	// which would witness a crash rather than a decline.
	siblings := []staticCheckedSibling{
		{name: "a second class", b: mutate(func(b *schema.Bundle) {
			b.Classes = append(b.Classes, schema.ClassDef{Name: schema.Name{Name: "Other"}, Mode: schema.NonStreaming,
				Fields: []schema.ClassField{scalarField("s", stringType())}})
		})},
		{name: "an enum in the bundle", b: mutate(func(b *schema.Bundle) {
			b.Enums = []schema.EnumDef{{Name: schema.Name{Name: "E"},
				Values: []schema.EnumValue{{Name: schema.Name{Name: "A"}}}}}
		})},
		{name: "a recursive-class marker", b: mutate(func(b *schema.Bundle) { b.RecursiveClasses = []string{"StaticCheckedAnswer"} })},
		{name: "a structural recursive alias", b: mutate(func(b *schema.Bundle) {
			b.StructuralRecursiveAliases = []schema.RecursiveAliasDef{{Name: "J", Target: stringType()}}
		})},
		{name: "a target-level constraint", b: mutate(func(b *schema.Bundle) {
			b.Target.Meta.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck,
				Expression: "this > 0", Label: label("t")}}
		})},
		{name: "a streaming target", b: mutate(func(b *schema.Bundle) { b.Target.Mode = schema.Streaming })},
		{name: "a scalar target", b: mutate(func(b *schema.Bundle) { b.Target = intType() })},
		{name: "a class-level constraint", b: mutate(func(b *schema.Bundle) {
			b.Classes[0].Constraints = []schema.Constraint{{Level: schema.ConstraintCheck,
				Expression: "this.confidence > 0", Label: label("c")}}
		})},
		{name: "a third field", b: mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields = append(b.Classes[0].Fields, scalarField("extra", stringType()))
		})},
		{name: "the two fields in the other order", b: mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[0], b.Classes[0].Fields[1] = b.Classes[0].Fields[1], b.Classes[0].Fields[0]
		})},
		{name: "an aliased field", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Name.Alias = label("score") })},
		{name: "an aliased class", b: mutate(func(b *schema.Bundle) { b.Classes[0].Name.Alias = label("Answer") })},
		{name: "a described field", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[0].Description = label("the answer") })},
		{name: "a @stream annotation on the field", b: mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Type.Meta.Stream.Done = true
		})},
		// Type.Dynamic is documented as meaningful for enums/classes, but it is a field
		// of EVERY Type and ValidateOutput does not reject it on a primitive, so the
		// fingerprint must refuse it on BOTH fields rather than rely on lowering.
		{name: "a dynamic answer field", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[0].Type.Dynamic = true })},
		{name: "a dynamic confidence field", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Dynamic = true })},
		{name: "a constraint on the OTHER field", b: mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[0].Type.Meta.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck,
				Expression: "this > 0", Label: label("a")}}
		})},
		{name: "a second constraint", b: mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Type.Meta.Constraints = append(b.Classes[0].Fields[1].Type.Meta.Constraints,
				schema.Constraint{Level: schema.ConstraintCheck, Expression: "this > 1", Label: label("other")})
		})},
		{name: "duplicate check labels", b: mutate(func(b *schema.Bundle) {
			b.Classes[0].Fields[1].Type.Meta.Constraints = append(b.Classes[0].Fields[1].Type.Meta.Constraints,
				schema.Constraint{Level: schema.ConstraintCheck, Expression: "this > 1", Label: label("positive")})
		})},
		{name: "a check with no label", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Meta.Constraints[0].Label = nil })},
		// PRESENT-BUT-EMPTY labels. `&""` is not `nil`: it is a label that is THERE and
		// is empty, which internal/bamlprofile rejects as an invalid BAML identifier and
		// which no stock capture covers. The ASSERT row is the one that matters — an
		// assert may legitimately omit its label, so a predicate that read the
		// NORMALISED string admitted this as "absent" and rendered the unlabelled error.
		{name: "an assert with a present-but-EMPTY label",
			b: staticCheckedBundleLabelPtr(schema.ConstraintAssert, strPtr(""), "this > 0")},
		{name: "a check with a present-but-EMPTY label",
			b: staticCheckedBundleLabelPtr(schema.ConstraintCheck, strPtr(""), "this > 0")},
		{name: "a non-ASCII label", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Meta.Constraints[0].Label = label("positif\u00e9") })},
		// The constraint sits on the union's NON-NULL ARM rather than on the field's own
		// node. checkSupportedType's `if u.Nullable { return nil }` fast path returns
		// before recursing, so this sibling used to slip the top-down walk entirely and
		// be left for coerce to refuse at value time — a genuine gate disagreement.
		// Deciding constraints over the whole subtree closed it, and this row is what
		// keeps it closed.
		{name: "a nullable confidence", b: mutate(func(b *schema.Bundle) {
			inner := b.Classes[0].Fields[1].Type
			b.Classes[0].Fields[1].Type = schema.Type{Kind: schema.TypeUnion,
				Union: &schema.UnionType{Variants: []schema.Type{inner}, Nullable: true}}
		})},
		{name: "a float confidence", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Primitive = schema.PrimitiveFloat })},
		{name: "a bool confidence", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Primitive = schema.PrimitiveBool })},
		{name: "a string confidence", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Type.Primitive = schema.PrimitiveString })},
		// The COLLECTION families. Each keeps the constraint on the field's own type
		// node — the position the fingerprint admits for an `int` — and changes only the
		// KIND, so the rejection is the kind's and not the constraint's placement.
		{name: "a list confidence", b: mutate(func(b *schema.Bundle) {
			elem := intType()
			t := &b.Classes[0].Fields[1].Type
			t.Kind, t.Primitive, t.Elem = schema.TypeList, "", &elem
		})},
		{name: "a map confidence", b: mutate(func(b *schema.Bundle) {
			key, val := stringType(), intType()
			t := &b.Classes[0].Fields[1].Type
			t.Kind, t.Primitive, t.Key, t.Value = schema.TypeMap, "", &key, &val
		})},
		{name: "a multi-arm union confidence", b: mutate(func(b *schema.Bundle) {
			t := &b.Classes[0].Fields[1].Type
			t.Kind, t.Primitive = schema.TypeUnion, ""
			t.Union = &schema.UnionType{Variants: []schema.Type{intType(), stringType()}}
		})},
		{name: "an enum confidence", b: func() *schema.Bundle {
			b := accept(schema.ConstraintCheck, "positive", "this > 0")
			t := &b.Classes[0].Fields[1].Type
			t.Kind, t.Primitive, t.Name, t.Mode = schema.TypeEnum, "", "Level", schema.NonStreaming
			b.Enums = []schema.EnumDef{{Name: schema.Name{Name: "Level"},
				Values: []schema.EnumValue{{Name: schema.Name{Name: "Low"}}, {Name: schema.Name{Name: "High"}}}}}
			if err := b.RebuildIndexes(); err != nil {
				panic("static checked sibling: " + err.Error())
			}
			return b
		}()},
		{name: "a renamed field", b: mutate(func(b *schema.Bundle) { b.Classes[0].Fields[1].Name.Name = "score" })},
		// The two FIXTURE-IDENTITY siblings: the right shape under the wrong name, and
		// the right name under the wrong constraint level. Neither has a stock capture.
		{name: "a renamed class", b: func() *schema.Bundle {
			b := accept(schema.ConstraintCheck, "positive", "this > 0")
			b.Classes[0].Name.Name, b.Target.Name = "SomeOtherAnswer", "SomeOtherAnswer"
			if err := b.RebuildIndexes(); err != nil {
				panic("static checked sibling: " + err.Error())
			}
			return b
		}()},
		{name: "the assert level on the check class", b: func() *schema.Bundle {
			b := accept(schema.ConstraintAssert, "positive", "this > 0")
			b.Classes[0].Name.Name, b.Target.Name = staticCheckedCheckClass, staticCheckedCheckClass
			if err := b.RebuildIndexes(); err != nil {
				panic("static checked sibling: " + err.Error())
			}
			return b
		}()},
	}
	// STRAY TYPE PAYLOADS. schema.Type carries kind-selected payloads that
	// Bundle.ValidateOutput ignores when they are irrelevant to the selected kind, and
	// both SupportsNativeFinalBundle and ParseStaticBundleUnaryCall accept a PRE-LOWERED
	// bundle — so each of these is a hand-constructible, ValidateOutput-valid input that
	// used to pass the "exact" fingerprint. One row per payload per position, generated
	// from staticCheckedStrayTypePayloads so a new schema.Type field cannot be forgotten
	// (TestStaticCheckedCanonicalTypeCoversEveryField fails if one appears).
	for _, pos := range []struct {
		what string
		set  func(*schema.Bundle, func(*schema.Type))
	}{
		{"the answer field", func(b *schema.Bundle, f func(*schema.Type)) { f(&b.Classes[0].Fields[0].Type) }},
		{"the confidence field", func(b *schema.Bundle, f func(*schema.Type)) { f(&b.Classes[0].Fields[1].Type) }},
		{"the target", func(b *schema.Bundle, f func(*schema.Type)) { f(&b.Target) }},
	} {
		for _, payload := range staticCheckedStrayTypePayloads() {
			siblings = append(siblings, staticCheckedSibling{
				name: payload.name + " on " + pos.what,
				b: mutate(func(b *schema.Bundle) {
					pos.set(b, payload.set)
				}),
			})
			if payload.setEmpty == nil {
				continue
			}
			// The NON-NIL EMPTY form of the same payload. It is a distinct sibling, not
			// a duplicate: a length-based check admitted it while rejecting the
			// non-empty one, which is how `Items: []schema.Type{}` slipped the
			// "canonical" fingerprint.
			siblings = append(siblings, staticCheckedSibling{
				name: payload.name + " (non-nil EMPTY) on " + pos.what,
				b: mutate(func(b *schema.Bundle) {
					pos.set(b, payload.setEmpty)
				}),
			})
		}
	}
	// REQUIRED-ABSENT COLLECTIONS, in their NON-NIL EMPTY form. Each is a
	// hand-constructible, ValidateOutput-valid pre-lowered Bundle whose absence test, if
	// written with len(), admits it — the class that produced a finding in four
	// consecutive review rounds, most recently hidden inside schema.TypeMeta.IsZero.
	for _, c := range staticCheckedRequiredAbsentCollections() {
		siblings = append(siblings, staticCheckedSibling{
			name: c.name,
			b:    mutate(c.setEmpty),
		})
	}
	for _, expr := range []string{
		"this >= 0", "this>0", "this > 0.0", "this > +5", "this > 007",
		"this > 1_000", "this != 0", "this|length > 0", "this > 9223372036854775808", "this > ",
		// PADDING past the admitted one ASCII space each side, and padding that is not
		// an ASCII space at all. strings.TrimSpace would have accepted every one of
		// these — including the NO-BREAK SPACE, which unicode.IsSpace reports true for —
		// so the canonicaliser counts 0x20 bytes itself rather than calling it.
		"  this > 0", "this > 0  ", "  this > 0  ",
		"\tthis > 0", "this > 0\t", "\nthis > 0", "this > 0\n",
		"\u00a0this > 0", "this > 0\u00a0",
		// All padding and no expression.
		" ", "  ",
	} {
		siblings = append(siblings, staticCheckedSibling{
			name: "expression " + strconv.Quote(expr),
			b:    accept(schema.ConstraintCheck, "positive", expr),
		})
	}
	return siblings
}

// strPtr returns a pointer to s, so a PRESENT label (including an empty one) can be
// written distinctly from an absent one.
func strPtr(s string) *string { return &s }

// staticCheckedStrayTypePayload is one schema.Type field the fingerprint's kinds do NOT
// select, with a mutator that populates it.
type staticCheckedStrayTypePayload struct {
	// field is the schema.Type field name, so the completeness guard can check this
	// list against the struct by reflection rather than by memory.
	field string
	name  string
	set   func(*schema.Type)
	// setEmpty populates the field with its NON-NIL, ZERO-LENGTH / pointer-to-zero
	// form, for the reference kinds (slice, map, pointer) that have one.
	//
	// It exists because "populated" and "non-empty" are different questions, and only
	// the first is what canonicality is about. `Items: []schema.Type{}` has length zero
	// yet is not the zero value, and a length-based check admitted it — the exact hole
	// the round-3 review found. Every reference-kind field must therefore be driven in
	// BOTH forms, and [TestStaticCheckedCanonicalTypeCoversEveryField] fails if a
	// reference-kind field arrives without one.
	setEmpty func(*schema.Type)
}

// staticCheckedStrayTypePayloads is every payload [staticCheckedCanonicalType] must
// require to be zero, plus the two that are kind-selected for ONE of the fingerprint's
// kinds and therefore stray on the other (Name/Mode belong to a class reference, so they
// are stray on a primitive field; Primitive belongs to a primitive, so it is stray on the
// class target).
//
// It is the DATA behind both the sibling corpus and the completeness guard, so the two
// cannot describe different sets.
func staticCheckedStrayTypePayloads() []staticCheckedStrayTypePayload {
	return []staticCheckedStrayTypePayload{
		{field: "Media", name: "a stray Media",
			set: func(t *schema.Type) { t.Media = schema.MediaImage }},
		{field: "Literal", name: "a stray Literal",
			set: func(t *schema.Type) {
				t.Literal = &schema.LiteralValue{Kind: schema.LiteralString, String: "x"}
			},
			setEmpty: func(t *schema.Type) { t.Literal = &schema.LiteralValue{} }},
		{field: "Elem", name: "a stray Elem",
			set:      func(t *schema.Type) { e := stringType(); t.Elem = &e },
			setEmpty: func(t *schema.Type) { t.Elem = &schema.Type{} }},
		{field: "Key", name: "a stray Key",
			set:      func(t *schema.Type) { k := stringType(); t.Key = &k },
			setEmpty: func(t *schema.Type) { t.Key = &schema.Type{} }},
		{field: "Value", name: "a stray Value",
			set:      func(t *schema.Type) { v := stringType(); t.Value = &v },
			setEmpty: func(t *schema.Type) { t.Value = &schema.Type{} }},
		{field: "Items", name: "a stray Items",
			set:      func(t *schema.Type) { t.Items = []schema.Type{stringType()} },
			setEmpty: func(t *schema.Type) { t.Items = []schema.Type{} }},
		{field: "Union", name: "a stray Union",
			set: func(t *schema.Type) {
				t.Union = &schema.UnionType{Variants: []schema.Type{stringType()}}
			},
			setEmpty: func(t *schema.Type) { t.Union = &schema.UnionType{} }},
		{field: "Arrow", name: "a stray Arrow",
			set:      func(t *schema.Type) { t.Arrow = &schema.ArrowType{Return: stringType()} },
			setEmpty: func(t *schema.Type) { t.Arrow = &schema.ArrowType{} }},
		{field: "Name", name: "a stray Name",
			set: func(t *schema.Type) { t.Name = "Stray" }},
		{field: "Mode", name: "a stray Mode",
			set: func(t *schema.Type) { t.Mode = schema.Streaming }},
		{field: "Primitive", name: "a stray Primitive",
			set: func(t *schema.Type) { t.Primitive = schema.PrimitiveFloat }},
		{field: "Dynamic", name: "a stray Dynamic",
			set: func(t *schema.Type) { t.Dynamic = true }},
	}
}

// TestStaticCheckedProfileAdmitsOnlyTheFingerprint drives the classifier over the two
// admitted shapes and over a ONE-PROPERTY sibling of each rejection reason.
func TestStaticCheckedProfileAdmitsOnlyTheFingerprint(t *testing.T) {
	// The positives first: without them the negatives could all be satisfied by a
	// classifier that rejects everything.
	for _, p := range []struct {
		name  string
		b     *schema.Bundle
		level schema.ConstraintLevel
		label string
	}{
		{"check", staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0"), schema.ConstraintCheck, "positive"},
		{"assert", staticCheckedBundle(schema.ConstraintAssert, "positive", "this > 0"), schema.ConstraintAssert, "positive"},
		{"assert without a label", staticCheckedBundle(schema.ConstraintAssert, "", "this > 100"), schema.ConstraintAssert, ""},
		{"negative threshold", staticCheckedBundle(schema.ConstraintCheck, "gt", "this > -5"), schema.ConstraintCheck, "gt"},
	} {
		prof, ok := staticCheckedProfileOf(p.b)
		if !ok {
			t.Fatalf("%s: the admitted fingerprint was REJECTED", p.name)
		}
		if prof.level != p.level || prof.label != p.label {
			t.Fatalf("%s: classified as %+v", p.name, prof)
		}
	}

	// The nil bundle, asserted here rather than in the shared corpus: it is a classifier
	// case, and the package-internal shape gates would panic on it rather than decline.
	if _, ok := staticCheckedProfileOf(nil); ok {
		t.Error("the fingerprint ADMITTED a nil bundle")
	}

	siblings := staticCheckedSiblings()
	for _, n := range siblings {
		if _, ok := staticCheckedProfileOf(n.b); ok {
			t.Errorf("the fingerprint ADMITTED a one-property sibling: %s", n.name)
		}
	}
	if len(siblings) < 20 {
		t.Fatalf("only %d negative rows; the fingerprint's narrowness would be barely witnessed", len(siblings))
	}
}

// TestStaticCheckedProductionPaddingIsCanonicalised pins the ONE normalisation the
// fingerprint performs, in both directions and as BYTES.
//
// It exists because the production text is not the one the byte captures show. A
// generated static method's descriptor carries a `@check(l, {{ EXPR }})` attribute with
// the delimiters stripped and the PADDING kept — the staticserve fixture's descriptor
// for `{{ this > 0 }}` is literally " this > 0 " — while stock puts `this > 0` in
// Check.Expression and in the assertion cause. internal/debaml/checkedwire's
// ExprPadNone / ExprPadOne / ExprPadTwo rows measured that against the real CFFI; this
// is the production side of the same fact.
//
// Every admitted padding must produce the SAME stock bytes as the unpadded form — so a
// route whose descriptor carries the padded text serves exactly what the byte authority
// captured, and the mapper cannot be leaking a padded string into the wire or the cause.
func TestStaticCheckedProductionPaddingIsCanonicalised(t *testing.T) {
	const raw = `{"answer": "sunny", "confidence": 9}`
	for _, src := range []string{"this > 0", " this > 0", "this > 0 ", " this > 0 "} {
		t.Run(strconv.Quote(src), func(t *testing.T) {
			b := staticCheckedBundle(schema.ConstraintCheck, "positive", src)
			prof, ok := staticCheckedProfileOf(b)
			if !ok {
				t.Fatalf("the fingerprint rejected the padded production form %s", strconv.Quote(src))
			}
			// The CANONICAL text, not the source: this is what reaches the carrier.
			if prof.expression != "this > 0" {
				t.Fatalf("profile expression = %s, want %s (stock's unpadded text)",
					strconv.Quote(prof.expression), strconv.Quote("this > 0"))
			}
			// The bundle itself still carries the SOURCE text, so this is a real
			// normalisation rather than a fixture that was never padded.
			if got := b.Classes[0].Fields[1].Type.Meta.Constraints[0].Expression; got != src {
				t.Fatalf("the fixture bundle carries %s, not the source %s", strconv.Quote(got), strconv.Quote(src))
			}
			res, err := staticCheckedMap(b, prof, raw)
			if err != nil {
				t.Fatalf("mapper: %v", err)
			}
			if got := string(res.JSON); got != staticCheckedWireNestedPass {
				t.Fatalf("padded source produced different bytes:\n got %s\nwant %s",
					got, staticCheckedWireNestedPass)
			}

			// The ASSERT twin's rendered cause must be the unpadded one too — the
			// error bytes are where a leaked space would be hardest to notice.
			ab := staticCheckedBundle(schema.ConstraintAssert, "positive", src)
			aprof, aok := staticCheckedProfileOf(ab)
			if !aok {
				t.Fatalf("the assert twin rejected the padded production form %s", strconv.Quote(src))
			}
			_, aerr := staticCheckedMap(ab, aprof, `{"answer": "sunny", "confidence": -1}`)
			if !staticCheckedIsAssertFailure(aerr) {
				t.Fatalf("the assert twin returned %v, want the rendered stock assertion failure", aerr)
			}
			if got := aerr.Error(); got != staticCheckedAssertFailBytes {
				t.Fatalf("assertion error bytes for a padded source:\n got %s\nwant %s",
					strconv.Quote(got), strconv.Quote(staticCheckedAssertFailBytes))
			}
		})
	}
	// The canonicaliser directly, so its boundary is asserted rather than inferred from
	// the four bundles above.
	for _, tc := range []struct {
		src  string
		want string
		ok   bool
	}{
		{"this > 0", "this > 0", true},
		{" this > 0 ", "this > 0", true},
		{" this > -9223372036854775808 ", "this > -9223372036854775808", true},
		{"  this > 0", "", false},
		{"this > 0  ", "", false},
		{"\tthis > 0", "", false},
		{"\u00a0this > 0", "", false},
		{"this > 0\u00a0", "", false},
		{" this>0 ", "", false},
		{"this > 0 ", "this > 0", true},
		{" ", "", false},
		{"", "", false},
	} {
		got, ok := staticCheckedCanonicalExpression(tc.src)
		if ok != tc.ok || got != tc.want {
			t.Errorf("canonicalise(%s) = (%s, %v), want (%s, %v)",
				strconv.Quote(tc.src), strconv.Quote(got), ok, strconv.Quote(tc.want), tc.ok)
		}
	}
}

// TestStaticCheckedCanonicalTypeCoversEveryField is the COMPLETENESS guard for the
// canonical-payload rule, and it is written against the struct rather than against a
// remembered list.
//
// [schema.Bundle.ValidateOutput] validates the SELECTED kind and ignores irrelevant
// populated payloads, while [SupportsNativeFinalBundle] and [ParseStaticBundleUnaryCall]
// accept a PRE-LOWERED bundle — so every schema.Type field the fingerprint's kinds do not
// select is a hand-constructible representation with no byte capture behind it. This
// reflects over schema.Type and requires each such field to be REJECTED, and it FAILS if
// a field exists that is neither kind-selected nor covered by
// staticCheckedStrayTypePayloads. A new payload on schema.Type therefore cannot slip
// through unnoticed.
func TestStaticCheckedCanonicalTypeCoversEveryField(t *testing.T) {
	// The fields each admitted kind SELECTS. Meta is listed for both: it is governed
	// separately (it carries the one admitted constraint and the stream flags
	// staticCheckedProfileOf / staticCheckedPlainField check), not by the payload rule.
	selected := map[schema.TypeKind]map[string]bool{
		schema.TypePrimitive: {"Kind": true, "Meta": true, "Primitive": true},
		schema.TypeClass:     {"Kind": true, "Meta": true, "Name": true, "Mode": true},
	}
	canonical := map[schema.TypeKind]schema.Type{
		schema.TypePrimitive: {Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString},
		schema.TypeClass:     {Kind: schema.TypeClass, Name: "StaticCheckedAnswer", Mode: schema.NonStreaming},
	}
	// CONTROL: the canonical forms are accepted, so every rejection below is about the
	// one payload added rather than about a predicate that refuses everything.
	for kind, base := range canonical {
		if !staticCheckedCanonicalType(base, kind) {
			t.Fatalf("the canonical %s type is REJECTED; every assertion below is vacuous", kind)
		}
	}

	covered := map[string]bool{}
	hasEmptyForm := map[string]bool{}
	for _, p := range staticCheckedStrayTypePayloads() {
		covered[p.field] = true
		hasEmptyForm[p.field] = p.setEmpty != nil
		for kind, base := range canonical {
			if selected[kind][p.field] {
				continue // this kind selects it; it is not stray here
			}
			// BOTH presence forms. A reference-kind payload has two non-canonical
			// states — populated, and NON-NIL but empty — and only the first is what a
			// length check sees. Driving both is what makes "canonical" mean "the zero
			// value" rather than "nothing interesting in it".
			forms := []struct {
				what string
				set  func(*schema.Type)
			}{{"", p.set}}
			if p.setEmpty != nil {
				forms = append(forms, struct {
					what string
					set  func(*schema.Type)
				}{" (non-nil EMPTY)", p.setEmpty})
			}
			for _, f := range forms {
				mutated := base
				f.set(&mutated)
				if staticCheckedCanonicalType(mutated, kind) {
					t.Errorf("staticCheckedCanonicalType ADMITTED %s%s on a %s node; that payload has "+
						"no stock byte capture behind it", p.name, f.what, kind)
				}
			}
		}
	}

	// COMPLETENESS: every field of schema.Type is either kind-selected for BOTH admitted
	// kinds, or covered above — and every REFERENCE-KIND field (slice, map, pointer)
	// must be covered in BOTH presence forms, because those are the fields where
	// "empty" and "absent" are different states.
	ty := reflect.TypeOf(schema.Type{})
	scanned, refKinds := 0, 0
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		name := f.Name
		scanned++
		if !covered[name] {
			if selected[schema.TypePrimitive][name] && selected[schema.TypeClass][name] {
				continue // Kind / Meta: selected everywhere, governed elsewhere
			}
			t.Errorf("schema.Type.%s is neither selected by both admitted kinds nor covered by "+
				"staticCheckedStrayTypePayloads; it is a payload the fingerprint has never been "+
				"asked about, so the canonical rule is incomplete", name)
			continue
		}
		switch f.Type.Kind() {
		case reflect.Slice, reflect.Map, reflect.Pointer:
			refKinds++
			if !hasEmptyForm[name] {
				t.Errorf("schema.Type.%s is a %s — its zero value is nil, so a NON-NIL EMPTY value "+
					"is a distinct non-canonical state — but staticCheckedStrayTypePayloads gives it "+
					"no setEmpty form, so nothing proves that state declines", name, f.Type.Kind())
			}
		}
	}
	if refKinds == 0 {
		t.Fatal("no reference-kind field was scanned; the nil-versus-empty half of this guard is vacuous")
	}
	if scanned == 0 {
		t.Fatal("schema.Type has no fields; the completeness scan is vacuous")
	}
	if len(covered) < scanned-2 {
		t.Fatalf("only %d of %d schema.Type fields are covered as stray payloads; the two "+
			"uncovered ones must be exactly Kind and Meta", len(covered), scanned)
	}
}

// staticCheckedRequiredAbsentCollection is one COLLECTION the fingerprint requires to be
// ABSENT, expressed so the three proofs that need it cannot describe different sets: the
// sibling corpus, the lowering-nil measurement, and the structural completeness guard.
//
// "Absent" means NIL, never "length zero". A non-nil empty slice is a populated field
// that ordinary lowering does not produce, and a length test admits it — which is how the
// same class of over-claim was found in four consecutive review rounds, most recently
// hidden inside [schema.TypeMeta.IsZero], whose body is `len(m.Constraints) == 0 && …`.
type staticCheckedRequiredAbsentCollection struct {
	// owner/field name the reflection guard matches against, e.g. "Bundle"/"Enums".
	owner, field string
	// name is the human label used in sibling rows and failures.
	name string
	// setEmpty populates it with the NON-NIL, zero-length form.
	setEmpty func(*schema.Bundle)
	// isNil reads it back from a bundle, for the lowering measurement.
	isNil func(*schema.Bundle) bool
}

// staticCheckedRequiredAbsentCollections is every one of them.
//
// Type.Items is NOT here: it is a [schema.Type] payload, governed by
// [staticCheckedCanonicalType] and covered — in both presence forms — by
// staticCheckedStrayTypePayloads at all three positions.
func staticCheckedRequiredAbsentCollections() []staticCheckedRequiredAbsentCollection {
	return []staticCheckedRequiredAbsentCollection{{
		owner: "Bundle", field: "Enums", name: "a non-nil EMPTY Bundle.Enums",
		setEmpty: func(b *schema.Bundle) { b.Enums = []schema.EnumDef{} },
		isNil:    func(b *schema.Bundle) bool { return b.Enums == nil },
	}, {
		owner: "Bundle", field: "RecursiveClasses", name: "a non-nil EMPTY Bundle.RecursiveClasses",
		setEmpty: func(b *schema.Bundle) { b.RecursiveClasses = []string{} },
		isNil:    func(b *schema.Bundle) bool { return b.RecursiveClasses == nil },
	}, {
		owner: "Bundle", field: "StructuralRecursiveAliases",
		name:     "a non-nil EMPTY Bundle.StructuralRecursiveAliases",
		setEmpty: func(b *schema.Bundle) { b.StructuralRecursiveAliases = []schema.RecursiveAliasDef{} },
		isNil:    func(b *schema.Bundle) bool { return b.StructuralRecursiveAliases == nil },
	}, {
		owner: "ClassDef", field: "Constraints", name: "a non-nil EMPTY ClassDef.Constraints",
		setEmpty: func(b *schema.Bundle) { b.Classes[0].Constraints = []schema.Constraint{} },
		isNil:    func(b *schema.Bundle) bool { return b.Classes[0].Constraints == nil },
	}, {
		owner: "TypeMeta", field: "Constraints",
		name:     "a non-nil EMPTY Meta.Constraints on the TARGET",
		setEmpty: func(b *schema.Bundle) { b.Target.Meta.Constraints = []schema.Constraint{} },
		isNil:    func(b *schema.Bundle) bool { return b.Target.Meta.Constraints == nil },
	}, {
		owner: "TypeMeta", field: "Constraints",
		name:     "a non-nil EMPTY Meta.Constraints on the ANSWER field",
		setEmpty: func(b *schema.Bundle) { b.Classes[0].Fields[0].Type.Meta.Constraints = []schema.Constraint{} },
		isNil:    func(b *schema.Bundle) bool { return b.Classes[0].Fields[0].Type.Meta.Constraints == nil },
	}}
}

// TestStaticCheckedRequiredAbsentCollectionsAreStructurallyComplete is the guard that
// closes the nil-versus-empty CLASS rather than its latest instance.
//
// It reflects over every struct the fingerprint reads and requires each slice/map/pointer
// field to be CLASSIFIED: covered by [staticCheckedRequiredAbsentCollections] (required
// absent, so it has an empty-form sibling and a lowering measurement), or by
// staticCheckedStrayTypePayloads (a Type payload), or listed as exactly-N / governed
// elsewhere with a stated reason. An unclassified collection FAILS — so a newly added
// field cannot reach the fingerprint without someone deciding what absence means for it.
func TestStaticCheckedRequiredAbsentCollectionsAreStructurallyComplete(t *testing.T) {
	absent := map[string]bool{}
	for _, c := range staticCheckedRequiredAbsentCollections() {
		absent[c.owner+"."+c.field] = true
	}
	typePayload := map[string]bool{}
	for _, p := range staticCheckedStrayTypePayloads() {
		typePayload["Type."+p.field] = true
	}
	// Collections the fingerprint pins to an EXACT count, or that are governed by a
	// named predicate — each with the reason, so "not required absent" is a decision
	// rather than an omission.
	governed := map[string]string{
		"Bundle.Classes":         "pinned to EXACTLY one class",
		"ClassDef.Fields":        "pinned to EXACTLY two fields, in order",
		"Type.Meta":              "a struct, not a collection; its own fields are classified below",
		"ClassField.Type":        "a struct; its payloads are staticCheckedCanonicalType's",
		"ClassDef.Name":          "a struct; its Alias pointer is tested for presence",
		"ClassField.Name":        "a struct; its Alias pointer is tested for presence",
		"ClassDef.Description":   "a pointer, tested for presence",
		"ClassField.Description": "a pointer, tested for presence",
		"ClassDef.Stream":        "a struct of three bools; no collection, so IsZero is safe",
		"TypeMeta.Stream":        "a struct of three bools; no collection, so IsZero is safe",
		"Bundle.Target":          "a struct; its payloads are staticCheckedCanonicalType's",
	}
	for _, owner := range []struct {
		name string
		typ  reflect.Type
	}{
		{"Bundle", reflect.TypeOf(schema.Bundle{})},
		{"ClassDef", reflect.TypeOf(schema.ClassDef{})},
		{"ClassField", reflect.TypeOf(schema.ClassField{})},
		{"TypeMeta", reflect.TypeOf(schema.TypeMeta{})},
	} {
		scanned := 0
		for i := 0; i < owner.typ.NumField(); i++ {
			f := owner.typ.Field(i)
			if !f.IsExported() {
				continue // unexported index maps, rebuilt by RebuildIndexes
			}
			scanned++
			key := owner.name + "." + f.Name
			switch f.Type.Kind() {
			case reflect.Slice, reflect.Map, reflect.Pointer:
			default:
				continue // scalars have no nil-versus-empty distinction
			}
			if absent[key] || typePayload[key] || governed[key] != "" {
				continue
			}
			t.Errorf("%s is a %s the fingerprint reads but nothing classifies: it is neither in "+
				"staticCheckedRequiredAbsentCollections (with an empty-form sibling and a lowering "+
				"measurement) nor declared exactly-N/governed. A collection whose ABSENCE is tested "+
				"with a length check admits its non-nil empty form", key, f.Type.Kind())
		}
		if scanned == 0 {
			t.Fatalf("%s has no exported fields; this scan is vacuous", owner.name)
		}
	}
	if len(absent) == 0 {
		t.Fatal("no required-absent collection is declared; the class guard is vacuous")
	}
}

// TestStaticCheckedLoweringProducesNilAbsences is what makes the PRESENCE rule safe to
// require rather than merely correct in principle.
//
// [staticCheckedProfileOf] demands `nil` — not "length zero" — for every collection it
// requires to be ABSENT, because a non-nil empty slice is a populated field that
// ValidateOutput does not traverse and that a length check therefore admitted. That is
// only a free tightening if the REAL descriptor-lowering path produces nil for each of
// them; if lowering ever produced a non-nil empty slice instead, the fingerprint would
// stop matching its own generated fixture and this test says so directly rather than
// leaving it to a live-socket failure.
func TestStaticCheckedLoweringProducesNilAbsences(t *testing.T) {
	for _, level := range []schema.ConstraintLevel{schema.ConstraintCheck, schema.ConstraintAssert} {
		t.Run(string(level), func(t *testing.T) {
			b, err := schema.FromStaticDescriptor(
				bundleDescriptorFor(staticCheckedBundle(level, "positive", "this > 0")))
			if err != nil {
				t.Fatalf("lower the fixture descriptor: %v", err)
			}
			// NON-VACUITY: this really is the admitted fingerprint, so the nil checks
			// below are about the shape the cutover serves.
			if _, ok := staticCheckedProfileOf(b); !ok {
				t.Fatal("the lowered fixture is not the admitted fingerprint; the assertions below " +
					"would be about some other bundle")
			}
			// EVERY required-absent collection, driven from the same registry the
			// siblings and the completeness guard use, so the three cannot diverge.
			for _, c := range staticCheckedRequiredAbsentCollections() {
				if !c.isNil(b) {
					t.Errorf("descriptor lowering produced a NON-NIL %s.%s (%s); the fingerprint "+
						"requires nil there, so the generated fixture would no longer match its own "+
						"fingerprint", c.owner, c.field, c.name)
				}
			}
			// …plus the Type payload with the same hazard, at all three positions.
			for _, tc := range []struct {
				what  string
				isNil bool
			}{
				{"answer Type.Items", b.Classes[0].Fields[0].Type.Items == nil},
				{"confidence Type.Items", b.Classes[0].Fields[1].Type.Items == nil},
				{"target Type.Items", b.Target.Items == nil},
			} {
				if !tc.isNil {
					t.Errorf("descriptor lowering produced a NON-NIL %s; the fingerprint requires nil "+
						"there", tc.what)
				}
			}
		})
	}
}

// TestStaticCheckedAssertLabelPresenceIsTested pins the distinction the profile used to
// erase: an ABSENT assert label and a PRESENT-but-empty one are different schemas.
//
// The scope permits an absent assert label or an ASCII one. It does not permit a present
// empty label — internal/bamlprofile rejects it as an invalid BAML identifier, and
// schema.lowerConstraints deliberately preserves nil-versus-present, so the descriptor
// really can carry it. A predicate that read the NORMALISED string (`label != "" && …`)
// treated `&""` exactly as `nil`, admitted it through every gate, and would have rendered
// its false assert as the UNLABELLED error — a byte shape no capture establishes for that
// source.
func TestStaticCheckedAssertLabelPresenceIsTested(t *testing.T) {
	// ABSENT is admitted, and its profile carries the empty label that makes the
	// renderer emit `Failed: <expr>`.
	absent := staticCheckedBundleLabelPtr(schema.ConstraintAssert, nil, "this > 0")
	prof, ok := staticCheckedProfileOf(absent)
	if !ok {
		t.Fatal("an assert with an ABSENT label is not admitted; the rejection below would be vacuous")
	}
	if prof.label != "" {
		t.Fatalf("an absent label produced profile label %q, want empty", prof.label)
	}

	// PRESENT-but-empty is a DIFFERENT bundle, and it declines.
	present := staticCheckedBundleLabelPtr(schema.ConstraintAssert, strPtr(""), "this > 0")
	if _, ok := staticCheckedProfileOf(present); ok {
		t.Fatal("an assert with a PRESENT-but-empty label was admitted as if the label were absent")
	}
	// The two really differ only in that one property, so the decline is attributable.
	if a, b := bundleDescriptorFor(absent), bundleDescriptorFor(present); soSameExceptLabel(a, b) != true {
		t.Fatal("the absent and present-empty bundles differ in more than the label pointer; the " +
			"comparison above would not be a one-property proof")
	}
	// A CHECK with a present-empty label declines too — for the check the normalised
	// rule already caught it (an empty label is not an ASCII identifier), so this is the
	// unchanged half and is asserted so the two levels cannot drift.
	if _, ok := staticCheckedProfileOf(staticCheckedBundleLabelPtr(schema.ConstraintCheck, strPtr(""), "this > 0")); ok {
		t.Fatal("a check with a PRESENT-but-empty label was admitted")
	}

	// EVERY named gate refuses it, before transport.
	const raw = `{"answer": "sunny", "confidence": 9}`
	for _, g := range staticCheckedGateSet(staticCheckedRow{raw: raw}) {
		err := g.run(present)
		if errors.Is(err, errStaticCheckedGateNotApplicable) {
			continue
		}
		if got := staticCheckedDispositionOf(err); got != staticCheckedDeclined {
			t.Errorf("%s: %s for a present-but-empty assert label, want declined", g.name, got)
		}
	}
}

// soSameExceptLabel reports whether two descriptors differ ONLY in the constraint label
// pointer of the confidence field.
func soSameExceptLabel(a, b schemadescriptor.Bundle) bool {
	if len(a.Classes) != 1 || len(b.Classes) != 1 ||
		len(a.Classes[0].Fields) != 2 || len(b.Classes[0].Fields) != 2 {
		return false
	}
	ac := a.Classes[0].Fields[1].Type.Meta.Constraints
	bc := b.Classes[0].Fields[1].Type.Meta.Constraints
	if len(ac) != 1 || len(bc) != 1 {
		return false
	}
	a.Classes[0].Fields[1].Type.Meta.Constraints[0].Label = nil
	b.Classes[0].Fields[1].Type.Meta.Constraints[0].Label = nil
	return reflect.DeepEqual(a, b)
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
// The cutover
// ---------------------------------------------------------------------------

// staticCheckedGate is one PRODUCTION gate the corpus is driven through.
//
// There is no stand-in: `run` is the production function exactly as it ships.
type staticCheckedGate struct {
	name string
	// run is the production gate exactly as it ships.
	run func(*schema.Bundle) error
	// want is the disposition this gate must reach for a bundle that IS the admitted
	// fingerprint.
	want staticCheckedDisposition
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

// errStaticCheckedGateNotApplicable marks a gate that was not asked about this bundle.
// It is deliberately NOT the decline sentinel: counting "never asked" as "declined" is
// the exact shape of a false green.
var errStaticCheckedGateNotApplicable = errors.New("gate not applicable to this bundle")

// ---------------------------------------------------------------------------
// The SCHEMA gates — one fingerprint, no exemptions
// ---------------------------------------------------------------------------

// staticCheckedSchemaGates is EVERY named gate that answers "may this SHAPE be claimed".
//
// The scope names six and requires them to use the SAME single fingerprint: a schema that
// passes one and fails another is a bug. They are listed here as ONE set, driven in BOTH
// directions over the same corpus, with NO gate exempted and no asymmetry — see
// [TestStaticCheckedGatesShareOneFingerprint].
//
//   - checkSupported / checkSupportedFields / checkSupportedType are the generic cut-line
//     the dynamic and stream lanes run. Since Slice 7.2b-3 their constraint reject
//     consults [staticCheckedAdmittedConstraintNode], so they answer the fingerprint too.
//   - SupportsNativeFinalBundle is the admission predicate.
//   - IsAdmittedStaticCheckedFamily is its exported twin, and the ONE thing nativeserve's
//     admission return-shape gate consults. It is driven here, beside the predicate it
//     mirrors, so the two cannot part company in the root package and be caught only in
//     the isolated module.
//
// checkSupportedType is driven over EVERY constrained type node rather than one hard-coded
// field index: a sibling that MOVES the constraint (the reordered pair, the constraint on
// `answer`) would otherwise be asked at a node carrying no constraint at all, which the
// function correctly admits — a false green for the one gate that arm is about.
func staticCheckedSchemaGates() []staticCheckedGate {
	return []staticCheckedGate{{
		name: "checkSupported", run: checkSupported, want: staticCheckedAdmitted,
	}, {
		name: "checkSupportedFields", run: checkSupportedFields, want: staticCheckedAdmitted,
	}, {
		name: "checkSupportedType (every constrained node)",
		run: func(b *schema.Bundle) error {
			nodes := bundleConstrainedTypeNodes(b)
			if len(nodes) == 0 {
				// No constrained TYPE node: the constraint lives on a class/enum
				// DECLARATION, which this function never sees. Saying so is not a
				// decline — reporting one would be a false green for a gate that was
				// never asked.
				return errStaticCheckedGateNotApplicable
			}
			for _, n := range nodes {
				if err := checkSupportedType(b, n); err != nil {
					return err
				}
			}
			return nil
		},
		want: staticCheckedAdmitted,
	}, {
		name: "SupportsNativeFinalBundle", run: SupportsNativeFinalBundle, want: staticCheckedAdmitted,
	}, {
		name: "IsAdmittedStaticCheckedFamily (nativeserve's return-shape delegate)",
		run: func(b *schema.Bundle) error {
			if IsAdmittedStaticCheckedFamily(b) {
				return nil
			}
			return unsupported("not the admitted checked-static family")
		},
		want: staticCheckedAdmitted,
	}}
}

// ---------------------------------------------------------------------------
// The ROUTE gates — which caller may claim a claimable shape
// ---------------------------------------------------------------------------

// staticCheckedRouteGates is every production ENTRY POINT the corpus is driven through,
// with the disposition each must reach for the admitted fingerprint.
//
// A route gate answers a different question from a schema gate: not "is this shape
// claimable" but "may THIS caller claim it". The scope admits the fingerprint on the
// static unary /call route ONLY, so the direct parse endpoints, the dynamic final lane
// and both stream lanes DECLINE it — and, now that the shape gates agree, they have to
// say so themselves rather than inheriting a blanket constraint reject.
//
// The `raw` a parse gate is driven with belongs to the ROW, so the assert-failure row's
// CLAIMED failure (no bytes, a non-sentinel error) is distinguishable from a decline.
func staticCheckedRouteGates(row staticCheckedRow) []staticCheckedGate {
	// The static unary /call route's disposition follows the ROW: three rows serve bytes,
	// and the false-assert row reaches a CLAIMED failure with no value.
	unary := staticCheckedAdmitted
	if row.wantErr != "" {
		unary = staticCheckedClaimedFailure
	}
	return []staticCheckedGate{{
		name: "ParseStaticBundle (direct)",
		run:  staticCheckedParseGate(ParseStaticBundle, row.raw),
		want: staticCheckedDeclined,
	}, {
		name: "Parse (root, static descriptor)",
		run:  staticCheckedParseGate(staticCheckedRootParse, row.raw),
		want: staticCheckedDeclined,
	}, {
		name: "Parse (root, DYNAMIC final lane)",
		run:  staticCheckedParseGate(staticCheckedDynamicParse, row.raw),
		want: staticCheckedDeclined,
	}, {
		name: "parseStream (the /stream runtime lane)",
		run:  staticCheckedParseGate(staticCheckedStreamParse, row.raw),
		want: staticCheckedDeclined,
	}, {
		name: "SupportsNativeStreamBundle (the /stream admission predicate)",
		run:  SupportsNativeStreamBundle, want: staticCheckedDeclined,
	}, {
		name: "SupportsNativeStaticStreamBundle (the static /stream admission predicate)",
		run:  SupportsNativeStaticStreamBundle, want: staticCheckedDeclined,
	}, {
		name: "ParseStaticBundleUnaryCall (static unary /call)",
		run:  staticCheckedParseGate(ParseStaticBundleUnaryCall, row.raw),
		want: unary,
	}}
}

// staticCheckedGateSet is both sets together — every named production gate.
func staticCheckedGateSet(row staticCheckedRow) []staticCheckedGate {
	return append(staticCheckedSchemaGates(), staticCheckedRouteGates(row)...)
}

// staticCheckedDynamicParse drives root [Parse]'s DYNAMIC final lane over a bundle.
//
// The dynamic TypeBuilder has no constraint channel (the #572 ceiling), so a constrained
// bundle cannot be built through the public DynamicOutputSchema API. The lane is therefore
// driven by handing Parse the bundle DIRECTLY through the same internal entry the dynamic
// branch reaches after lowering — which is what makes the route boundary in that branch a
// tested fact rather than an unreachable comment.
func staticCheckedDynamicParse(ctx context.Context, b *schema.Bundle, raw string) (bamlutils.DeBAMLParseResult, error) {
	_ = ctx
	if err := checkSupported(b); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	if err := staticCheckedRouteBoundary(b, "the dynamic final route"); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	parsed, ok := extractCandidate(stripJSONComments(raw))
	if !ok {
		return bamlutils.DeBAMLParseResult{}, unsupported("no cleanly-claimable JSON candidate")
	}
	out, err := coerce(b, b.Target, parsed, nil, &coerceCtx{})
	if err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	return bamlutils.DeBAMLParseResult{JSON: out}, nil
}

// staticCheckedStreamParse drives the /stream runtime lane over a bundle, for the same
// reason and by the same construction as [staticCheckedDynamicParse].
func staticCheckedStreamParse(ctx context.Context, b *schema.Bundle, raw string) (bamlutils.DeBAMLParseResult, error) {
	_ = ctx
	if err := checkSupported(b); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	if err := staticCheckedRouteBoundary(b, "the stream route"); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	if err := checkNoStreamAnnotations(b); err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	v, ok := streamExtractCandidate(stripJSONComments(raw))
	if !ok {
		return bamlutils.DeBAMLParseResult{}, unsupported("stream: no candidate")
	}
	out, err := coerceStream(b, b.Target, v)
	if err != nil {
		return bamlutils.DeBAMLParseResult{}, err
	}
	return bamlutils.DeBAMLParseResult{JSON: out}, nil
}

// TestStaticCheckedDynamicAndStreamLanesMirrorProduction is what makes the two drivers
// above evidence about PRODUCTION rather than about themselves.
//
// Each reproduces one production lane's prologue, and each must be a byte-for-byte copy of
// the guard sequence that lane runs. The structural comparison is over the SOURCE: the
// production function must call checkSupported and then staticCheckedRouteBoundary, in that
// order, and the driver must call the same two.
func TestStaticCheckedDynamicAndStreamLanesMirrorProduction(t *testing.T) {
	prologue := func(file *ast.File, fn string) []string {
		var calls []string
		ast.Inspect(file, func(n ast.Node) bool {
			decl, ok := n.(*ast.FuncDecl)
			if !ok || decl.Name.Name != fn {
				return true
			}
			ast.Inspect(decl.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok {
					switch id.Name {
					case "checkSupported", "staticCheckedRouteBoundary":
						calls = append(calls, id.Name)
					}
				}
				return true
			})
			return false
		})
		return calls
	}
	production := staticCheckedParseSource(t, staticCheckedSourcePath(t, "parse.go"))
	driver := staticCheckedParseSource(t, staticCheckedSourcePath(t, "checked_static_test.go"))
	for _, tc := range []struct{ prod, drive string }{
		{"Parse", "staticCheckedDynamicParse"},
		{"parseStream", "staticCheckedStreamParse"},
	} {
		got, want := prologue(driver, tc.drive), prologue(production, tc.prod)
		if len(want) != 2 || want[0] != "checkSupported" || want[1] != "staticCheckedRouteBoundary" {
			t.Fatalf("%s no longer runs checkSupported then staticCheckedRouteBoundary (got %v); the "+
				"route boundary this slice added is gone from production", tc.prod, want)
		}
		if len(got) != len(want) {
			t.Fatalf("%s drives %v but %s runs %v; the driver is not the production prologue",
				tc.drive, got, tc.prod, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s drives %v but %s runs %v", tc.drive, got, tc.prod, want)
			}
		}
	}
}

// staticCheckedParseGate drives a bundle through a parse entry point with the row's own
// raw text and reports "admitted" only when it produced bytes.
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
	fn := promptdescriptor.Function{Method: "StaticCheckedFixture", Return: staticCheckedReturnDescriptor(b)}
	return Parse(ctx, bamlutils.DeBAMLParseRequest{StaticStreamDescriptor: &fn, Raw: raw})
}

// staticCheckedReturnDescriptor lowers a bundle back into the descriptor shape a
// generated static method carries, so root [Parse] can be driven over the SAME shape the
// other gates see.
//
// It is the FAITHFUL mirror [bundleDescriptorFor] — not a narrow fixture-shaped builder.
// The sibling sweep drives shapes with a second class, a third field, a nullable field
// and an enum through this gate, and a builder that could only express the two-field
// fixture would either panic on them or, worse, silently hand root Parse a DIFFERENT
// schema and let its decline be attributed to the wrong thing.
func staticCheckedReturnDescriptor(b *schema.Bundle) schemadescriptor.Bundle {
	return bundleDescriptorFor(b)
}

// TestStaticCheckedCutoverAdmitsThroughTheRealGates is the load-bearing invariant of
// Slice 7.2b-3: the four companion rows move decline → admit through every SCHEMA gate,
// and the ROUTE gates place them on exactly one route.
//
// Every gate is the PRODUCTION function, driven exactly as it ships.
func TestStaticCheckedCutoverAdmitsThroughTheRealGates(t *testing.T) {
	if !staticCheckedAdmitsConstraints {
		t.Fatal("the cutover constant is still false; this slice is the flip, so every admit assertion " +
			"below would be about code nothing reaches")
	}
	// The capability halves: only the static unary /call route may claim.
	if !staticCheckedGrantStaticUnaryCall().admits() {
		t.Fatal("the static-unary-call capability is denied after the cutover; nothing would serve")
	}
	if staticCheckedDirect().admits() {
		t.Fatal("the DIRECT capability admits; direct routes must never claim the fingerprint")
	}

	rows := staticCheckedRows()
	if len(rows) != 4 {
		t.Fatalf("%d companion rows, want the 4 named by the scope", len(rows))
	}
	admittedGates, declinedGates := 0, 0
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			b := r.bundle()
			// The row IS the admitted fingerprint — otherwise the dispositions below
			// would be witnessing a shape the cutover does not target.
			if _, ok := staticCheckedProfileOf(b); !ok {
				t.Fatal("the row is not the admitted fingerprint; its disposition would witness nothing")
			}
			gates := staticCheckedGateSet(r)
			if len(gates) < 11 {
				t.Fatalf("%d gates are driven; every named production gate must be covered", len(gates))
			}
			for _, g := range gates {
				got := staticCheckedDispositionOf(g.run(b))
				if got != g.want {
					t.Errorf("%s: %s, want %s", g.name, got, g.want)
					continue
				}
				if g.want == staticCheckedDeclined {
					declinedGates++
					continue
				}
				admittedGates++
			}

			// The bytes the static unary route serves are STOCK's, so this is a real
			// serve rather than merely a different error.
			res, err := ParseStaticBundleUnaryCall(context.Background(), b, r.raw)
			if r.wantErr != "" {
				if !staticCheckedIsAssertFailure(err) {
					t.Fatalf("the unary route returned %v, want the rendered stock assertion failure", err)
				}
				if got := err.Error(); got != r.wantErr {
					t.Fatalf("assertion error bytes:\n got %s\nwant %s", strconv.Quote(got), strconv.Quote(r.wantErr))
				}
				if len(res.JSON) != 0 {
					t.Fatalf("the unary route produced %s bytes for a false @assert", res.JSON)
				}
				return
			}
			if err != nil {
				t.Fatalf("the unary route did not serve: %v", err)
			}
			if got := string(res.JSON); got != r.wantJSON {
				t.Fatalf("the unary route's bytes:\n got %s\nwant %s", got, r.wantJSON)
			}
		})
	}
	// Both populations must be non-empty, or the table would be asserting only one half
	// of the boundary.
	if admittedGates == 0 {
		t.Fatal("no gate admitted any row; the cutover is not reachable from these gates")
	}
	if declinedGates == 0 {
		t.Fatal("no gate declined any row; the /call-only boundary would be unwitnessed")
	}
}

// TestStaticCheckedRouteBoundaryKeepsTheDynamicAndStreamLanesClosed is the OTHER half of
// the single-fingerprint decision, and the reason the shape gates could be made to agree.
//
// Once checkSupported answers the fingerprint, "claimable shape" no longer implies "this
// caller may claim it" — so the lanes the scope leaves on BAML have to decline it
// THEMSELVES. Each is driven with a raw text it would otherwise coerce cleanly, and the
// decline is required to be the ROUTE's (it names the route) rather than a coercion
// failure or a leftover shape reject.
func TestStaticCheckedRouteBoundaryKeepsTheDynamicAndStreamLanesClosed(t *testing.T) {
	const raw = `{"answer": "sunny", "confidence": 9}`
	b := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")

	// PRECONDITION: the shape gates DO admit it, so a decline below is the route's.
	for _, g := range staticCheckedSchemaGates() {
		if got := staticCheckedDispositionOf(g.run(b)); got != staticCheckedAdmitted {
			t.Fatalf("%s: %s for the admitted fingerprint; the route assertions below would be "+
				"witnessing a shape reject instead of a route boundary", g.name, got)
		}
	}

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"root Parse, dynamic final lane", func() error {
			_, err := staticCheckedDynamicParse(context.Background(), b, raw)
			return err
		}},
		{"parseStream, the /stream runtime lane", func() error {
			_, err := staticCheckedStreamParse(context.Background(), b, raw)
			return err
		}},
		{"SupportsNativeStreamBundle, the /stream admission predicate", func() error {
			return SupportsNativeStreamBundle(b)
		}},
		{"ParseStaticBundle, the direct endpoint", func() error {
			_, err := ParseStaticBundle(context.Background(), b, raw)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
				t.Fatalf("returned %v; the route must DECLINE the fingerprint", err)
			}
			if !strings.Contains(err.Error(), "may not claim it") {
				t.Fatalf("declined with %q, which does not name the ROUTE; a shape-reject message here "+
					"would mean the boundary is not the thing refusing", err)
			}
		})
	}

	// NON-VACUITY: the CONSTRAINT-STRIPPED twin passes every one of those lanes, so the
	// declines above are about the fingerprint and not about the two-field class.
	stripped := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")
	stripped.Classes[0].Fields[1].Type.Meta.Constraints = nil
	if _, err := staticCheckedDynamicParse(context.Background(), stripped, raw); err != nil {
		t.Fatalf("the constraint-stripped twin was refused by the dynamic lane (%v); every decline "+
			"above is vacuous", err)
	}
	if err := SupportsNativeStreamBundle(stripped); err != nil {
		t.Fatalf("the constraint-stripped twin was refused by the stream predicate (%v); every decline "+
			"above is vacuous", err)
	}
}

// TestDynamicLoweringCannotExpressAConstraint pins the #572 ceiling, which is the reason
// the DYNAMIC admission predicate ([SupportsNativeFinal]) carries no checked-static route
// boundary while root [Parse]'s dynamic BRANCH does.
//
// A DynamicOutputSchema has no constraint channel, so the lowering cannot produce a
// constrained bundle and a guard on the predicate could never execute — an unverifiable
// boundary. What CAN be verified is the ceiling itself, and this is it: a schema built
// with every shape the public dynamic API can express lowers to a bundle with ZERO
// constrained type nodes and no class/enum declaration constraints.
//
// If the dynamic API ever gains a constraint channel this test fails, which is exactly
// when the predicate must grow the boundary.
func TestDynamicLoweringCannotExpressAConstraint(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("root", &bamlutils.DynamicProperty{Ref: "StaticCheckedAnswer"})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("StaticCheckedAnswer", &bamlutils.DynamicClass{
				Properties: props(kv("answer", strProp()), kv("confidence", intProp())),
			}),
		),
	}
	b, err := schema.FromDynamicOutputSchema(s, schema.BuildOptions{})
	if err != nil {
		t.Fatalf("lower the richest dynamic schema: %v", err)
	}
	// NON-VACUITY: the lowering really produced the shape, so "no constraints" is a fact
	// about a real bundle rather than about an empty one.
	named := 0
	for _, c := range b.Classes {
		if c.Name.Name == "StaticCheckedAnswer" && len(c.Fields) == 2 {
			named++
		}
	}
	if named != 1 {
		t.Fatalf("the dynamic lowering did not produce the two-field class (%d matches); the constraint "+
			"scan below would be vacuous", named)
	}
	if nodes := bundleConstrainedTypeNodes(b); len(nodes) != 0 {
		t.Fatalf("the DYNAMIC lowering produced %d constrained type node(s); the #572 ceiling is gone, "+
			"so SupportsNativeFinal now needs the checked-static route boundary root Parse's dynamic "+
			"branch already carries", len(nodes))
	}
	for _, c := range b.Classes {
		if len(c.Constraints) != 0 {
			t.Fatalf("the DYNAMIC lowering produced a class-level constraint on %q; see above", c.Name.Name)
		}
	}
	for _, e := range b.Enums {
		if len(e.Constraints) != 0 {
			t.Fatalf("the DYNAMIC lowering produced an enum-level constraint on %q; see above", e.Name.Name)
		}
	}
	// …and the lowered shape is NOT the fingerprint, so the dynamic lane cannot present
	// it even by name.
	if _, ok := staticCheckedProfileOf(b); ok {
		t.Fatal("a DYNAMICALLY-lowered bundle matched the checked-static fingerprint; the dynamic lane " +
			"could then claim it")
	}
}

// TestStaticCheckedOnePropertySiblingsDeclineEverywhere is the guard the scope requires
// beside the four-row flip: a bundle that differs from the admitted fingerprint in
// exactly ONE property still declines, BEFORE transport, at EVERY named gate — schema
// gates and route gates alike, with no exemption.
func TestStaticCheckedOnePropertySiblingsDeclineEverywhere(t *testing.T) {
	// A raw text every sibling's shape can coerce, so a decline is the GATE's decision
	// rather than a coercion failure.
	const raw = `{"answer": "sunny", "confidence": 9}`
	gates := staticCheckedGateSet(staticCheckedRow{raw: raw})

	siblings := staticCheckedSiblings()
	if len(siblings) < 20 {
		t.Fatalf("only %d siblings; this guard would be barely witnessed", len(siblings))
	}
	for _, s := range siblings {
		t.Run(s.name, func(t *testing.T) {
			// The fingerprint itself rejects it — otherwise the declines below could be
			// about something else entirely.
			if _, ok := staticCheckedProfileOf(s.b); ok {
				t.Fatal("the fingerprint ADMITTED this sibling; it is not a sibling")
			}
			for _, g := range gates {
				err := g.run(s.b)
				if errors.Is(err, errStaticCheckedGateNotApplicable) {
					continue
				}
				if got := staticCheckedDispositionOf(err); got != staticCheckedDeclined {
					t.Errorf("%s: %s for a one-property sibling, want declined", g.name, got)
				}
			}
		})
	}

	// NON-VACUITY: the same gate set, over the ADMITTED fingerprint and the same raw
	// text, really does admit — so the declines above are the siblings' doing rather
	// than a gate set that refuses everything.
	admitted := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")
	moved := 0
	for _, g := range gates {
		if staticCheckedDispositionOf(g.run(admitted)) == staticCheckedAdmitted {
			moved++
		}
	}
	if moved < len(staticCheckedSchemaGates()) {
		t.Fatalf("only %d gates admitted the fingerprint itself, want at least the %d schema gates; "+
			"the sibling declines above would be vacuous", moved, len(staticCheckedSchemaGates()))
	}
}

// ---------------------------------------------------------------------------
// One fingerprint, shared by every gate
// ---------------------------------------------------------------------------

// staticCheckedCorpusEntry is one bundle the gate-agreement sweep drives, with whether
// it IS the admitted fingerprint.
type staticCheckedCorpusEntry struct {
	name        string
	b           *schema.Bundle
	fingerprint bool
}

// staticCheckedAgreementCorpus is the four admitted rows plus every one-property
// sibling — the smallest set over which "these gates share one fingerprint" is a claim
// about both answers rather than about one.
func staticCheckedAgreementCorpus() []staticCheckedCorpusEntry {
	var out []staticCheckedCorpusEntry
	for _, r := range staticCheckedRows() {
		out = append(out, staticCheckedCorpusEntry{name: r.name, b: r.bundle(), fingerprint: true})
	}
	for _, s := range staticCheckedSiblings() {
		out = append(out, staticCheckedCorpusEntry{name: s.name, b: s.b})
	}
	return out
}

// staticCheckedGateDisagreements is the comparison itself, factored out so it can be
// driven with a DELIBERATELY WRONG fingerprint or a DELIBERATELY WRONG gate and shown to
// report the mismatch. It returns one line per disagreement.
//
// The rule is SYMMETRIC and has no exemption: for every corpus bundle, every gate must
// admit exactly when the fingerprint does. A gate that CLAIMS what the fingerprint
// rejects is an over-claim (native would serve a schema with no stock byte capture behind
// it); a gate that DECLINES what the fingerprint admits is the gates disagreeing about
// one schema, which the scope calls a bug in its own right. Both are reported.
//
// It is for SCHEMA gates only. A route gate answers a different question — "may this
// caller claim it" — and its decline is the /call-only boundary rather than a
// disagreement; those are asserted by
// TestStaticCheckedRouteBoundaryKeepsTheDynamicAndStreamLanesClosed.
func staticCheckedGateDisagreements(
	fingerprint func(*schema.Bundle) bool,
	gates []staticCheckedGate,
	corpus []staticCheckedCorpusEntry,
) []string {
	var out []string
	for _, e := range corpus {
		want := fingerprint(e.b)
		for _, g := range gates {
			err := g.run(e.b)
			if errors.Is(err, errStaticCheckedGateNotApplicable) {
				continue
			}
			claimed := staticCheckedDispositionOf(err) != staticCheckedDeclined
			switch {
			case claimed && !want:
				out = append(out, fmt.Sprintf("%s CLAIMED %q, which the fingerprint rejects", g.name, e.name))
			case !claimed && want:
				out = append(out, fmt.Sprintf("%s DECLINED %q, which the fingerprint admits", g.name, e.name))
			}
		}
	}
	return out
}

// TestStaticCheckedGatesShareOneFingerprint is the scope's single-fingerprint invariant,
// as a measured fact over EVERY named schema gate, in BOTH directions, with no exemption.
func TestStaticCheckedGatesShareOneFingerprint(t *testing.T) {
	corpus := staticCheckedAgreementCorpus()
	admitted := 0
	for _, e := range corpus {
		if e.fingerprint {
			admitted++
		}
	}
	if admitted != 4 || len(corpus) < 24 {
		t.Fatalf("the corpus has %d admitted rows in %d entries, want the 4 companion rows and a "+
			"substantial sibling set", admitted, len(corpus))
	}
	gates := staticCheckedSchemaGates()
	if len(gates) < 5 {
		t.Fatalf("only %d schema gates are driven; the scope names checkSupported, "+
			"checkSupportedFields, checkSupportedType, SupportsNativeFinalBundle and the "+
			"nativeserve return-shape delegate", len(gates))
	}
	if got := staticCheckedGateDisagreements(staticCheckedFingerprintAdmits, gates, corpus); len(got) != 0 {
		t.Fatalf("the production gates do not share one fingerprint:\n  %s", strings.Join(got, "\n  "))
	}
}

// staticCheckedFingerprintAdmits is [staticCheckedProfileOf] as a bare predicate.
func staticCheckedFingerprintAdmits(b *schema.Bundle) bool {
	_, ok := staticCheckedProfileOf(b)
	return ok
}

// TestStaticCheckedGateAgreementIsProvenToBite is the mutation proof for the assertion
// above: it feeds the SAME comparison a fingerprint that has been broadened to admit a
// sibling, and gates mutated in BOTH directions, and requires each to be REPORTED — for
// EVERY named schema gate, not for a privileged subset.
//
// Without it, TestStaticCheckedGatesShareOneFingerprint could be green because the
// comparison never fires. The mutants are stand-ins fed to the real comparison — the
// production gates and the production fingerprint are untouched.
func TestStaticCheckedGateAgreementIsProvenToBite(t *testing.T) {
	corpus := staticCheckedAgreementCorpus()
	gates := staticCheckedSchemaGates()

	// (1) A BROADENED fingerprint: one that also admits every sibling.
	broadened := func(*schema.Bundle) bool { return true }
	if got := staticCheckedGateDisagreements(broadened, gates, corpus); len(got) == 0 {
		t.Error("a fingerprint broadened to admit EVERY sibling produced no disagreement; the " +
			"agreement assertion cannot detect a widened fingerprint")
	}
	// (2) A NARROWED fingerprint: one that admits nothing.
	narrowed := func(*schema.Bundle) bool { return false }
	if got := staticCheckedGateDisagreements(narrowed, gates, corpus); len(got) == 0 {
		t.Error("a fingerprint narrowed to admit NOTHING produced no disagreement; the agreement " +
			"assertion cannot detect a gate that kept claiming")
	}
	// (3) A fingerprint broadened by exactly ONE property — the realistic mistake. Each
	// mutant drops one clause of the real fingerprint, and the sibling it would let
	// through must be reported.
	for _, m := range []struct {
		name    string
		sibling string
		admits  func(*schema.Bundle) bool
	}{{
		name: "the class-name pin dropped", sibling: "a renamed class",
		admits: func(b *schema.Bundle) bool {
			return staticCheckedFingerprintAdmits(b) || staticCheckedRenamedTwinAdmits(b)
		},
	}, {
		name: "the one-constraint clause dropped", sibling: "a second constraint",
		admits: func(b *schema.Bundle) bool {
			return staticCheckedFingerprintAdmits(b) || staticCheckedExtraConstraintTwinAdmits(b)
		},
	}} {
		t.Run(m.name, func(t *testing.T) {
			got := staticCheckedGateDisagreements(m.admits, gates, corpus)
			found := false
			for _, line := range got {
				if strings.Contains(line, m.sibling) {
					found = true
				}
			}
			if !found {
				t.Errorf("a fingerprint broadened by one property did not produce a disagreement naming "+
					"%q; got %v", m.sibling, got)
			}
		})
	}

	// (4) EVERY named gate, mutated in BOTH directions, one at a time. Replacing gate i
	// with an over-claiming or under-claiming stand-in must be reported AND must name
	// that gate — so no gate can be silently exempt from the agreement rule.
	for i := range gates {
		name := gates[i].name
		for _, m := range []struct {
			dir string
			run func(*schema.Bundle) error
		}{
			{"over-claims", func(*schema.Bundle) error { return nil }},
			{"under-claims", func(*schema.Bundle) error { return unsupported("mutant") }},
		} {
			t.Run(name+" "+m.dir, func(t *testing.T) {
				mutated := append([]staticCheckedGate(nil), gates...)
				mutated[i] = staticCheckedGate{name: name, run: m.run, want: gates[i].want}
				got := staticCheckedGateDisagreements(staticCheckedFingerprintAdmits, mutated, corpus)
				found := false
				for _, line := range got {
					if strings.HasPrefix(line, name+" ") {
						found = true
					}
				}
				if !found {
					t.Errorf("mutating %s to %s produced no disagreement naming it; that gate is "+
						"exempt from the single-fingerprint rule. got %v", name, m.dir, got)
				}
			})
		}
	}
}

// staticCheckedRenamedTwinAdmits / staticCheckedExtraConstraintTwinAdmits recognise the
// two one-property siblings the broadened-fingerprint mutants above let through. They are
// written as SHAPE predicates, so each mutant really is "the production fingerprint minus
// one clause" rather than a name match on the corpus row.
func staticCheckedRenamedTwinAdmits(b *schema.Bundle) bool {
	if b == nil || len(b.Classes) != 1 || len(b.Classes[0].Fields) != 2 {
		return false
	}
	renamed := *b
	renamed.Classes = []schema.ClassDef{b.Classes[0]}
	renamed.Classes[0].Name.Name = staticCheckedCheckClass
	renamed.Target.Name = staticCheckedCheckClass
	if err := renamed.RebuildIndexes(); err != nil {
		return false
	}
	return staticCheckedFingerprintAdmits(&renamed)
}

func staticCheckedExtraConstraintTwinAdmits(b *schema.Bundle) bool {
	if b == nil || len(b.Classes) != 1 || len(b.Classes[0].Fields) != 2 {
		return false
	}
	cs := b.Classes[0].Fields[1].Type.Meta.Constraints
	if len(cs) < 2 {
		return false
	}
	trimmed := *b
	trimmed.Classes = []schema.ClassDef{b.Classes[0]}
	trimmed.Classes[0].Fields = append([]schema.ClassField(nil), b.Classes[0].Fields...)
	trimmed.Classes[0].Fields[1].Type.Meta.Constraints = cs[:1]
	if err := trimmed.RebuildIndexes(); err != nil {
		return false
	}
	return staticCheckedFingerprintAdmits(&trimmed)
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
	for _, v := range staticCheckedDynamicVariants() {
		b := staticCheckedDynamicBundle(v.field)
		if _, ok := staticCheckedProfileOf(b); ok {
			t.Errorf("the fingerprint ADMITTED a %s field; that variant has no byte capture behind it", v.name)
		}
		// The ATTRIBUTABLE gates: these are the ones whose disposition the field
		// guard decides, and the ones that reopen if it is removed.
		for _, g := range staticCheckedDynamicAttributableGates() {
			if got := staticCheckedDispositionOf(g.run(b)); got != staticCheckedDeclined {
				t.Errorf("%s: %s from %s, want declined", v.name, got, g.name)
			}
		}
		// The REMAINING production gates must decline too — they just do so for
		// reasons the guard does not own (the generic constraint cut-line, the route
		// capability, descriptor lowering), so they are checked without being
		// claimed as evidence for the guard.
		for _, g := range staticCheckedGateSet(row) {
			if got := staticCheckedDispositionOf(g.run(b)); got != staticCheckedDeclined {
				t.Errorf("%s: %s from %s, want declined", v.name, got, g.name)
			}
		}
	}

	// NON-VACUITY: after the cutover the NON-dynamic twin DOES move through every
	// attributable gate, so the declines above are the dynamic bit's doing rather than a
	// gate set that refuses everything.
	clean := row.bundle()
	for _, g := range staticCheckedDynamicAttributableGates() {
		if got := staticCheckedDispositionOf(g.run(clean)); got == staticCheckedDeclined {
			t.Fatalf("%s declined the NON-dynamic fingerprint; the assertions above would be vacuous", g.name)
		}
	}
}

// staticCheckedDynamicAttributableGates are the production gates whose answer the
// `!f.Type.Dynamic` guard decides — the ones that admit the variant if it is removed.
//
// They are named explicitly, and separately from [staticCheckedGateSet], because the
// other gates decline a dynamic bundle for reasons the guard does not own: the direct
// parse routes on the route capability, the /stream lanes on the route boundary, and
// root Parse on descriptor lowering. Counting those as evidence would let an unrelated
// guard produce a false green for this one.
//
// (The three generic shape gates ARE attributable now — since the cutover they consult
// the same fingerprint, and the `dynamic` bit is one of the properties
// staticCheckedCanonicalType refuses — but they are left out of this list because
// [TestStaticCheckedOnePropertySiblingsDeclineEverywhere] already drives them over the
// dynamic siblings alongside every other stray Type payload.)
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
func TestStaticCheckedDynamicFieldDeclinesAtTheDescriptorIngress(t *testing.T) {
	for _, v := range staticCheckedDynamicVariants() {
		b := staticCheckedDynamicBundle(v.field)
		desc := staticCheckedReturnDescriptor(b)
		// The descriptor really carries the bit — otherwise this test would drive a
		// clean shape and prove nothing about the dynamic one.
		if !desc.Classes[0].Fields[v.field].Type.Dynamic {
			t.Fatalf("the %s descriptor does not carry Dynamic; the assertion below would be about a "+
				"different shape", v.name)
		}
		if _, lerr := schema.FromStaticDescriptor(desc); lerr == nil {
			t.Fatalf("%s: descriptor lowering ACCEPTED a dynamic primitive; this ingress's defence "+
				"would be gone", v.name)
		}
		res, err := staticCheckedRootParse(context.Background(), b, `{"answer": "sunny", "confidence": 9}`)
		if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
			t.Errorf("%s: root Parse returned %v, want the decline sentinel", v.name, err)
		}
		if len(res.JSON) != 0 {
			t.Errorf("%s: root Parse declined but produced %s bytes", v.name, res.JSON)
		}
	}
	// CONTROL: the NON-dynamic descriptor lowers cleanly, so the failures above are
	// the dynamic bit's and not a descriptor helper that produces junk.
	clean := staticCheckedReturnDescriptor(staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0"))
	if _, lerr := schema.FromStaticDescriptor(clean); lerr != nil {
		t.Fatalf("the non-dynamic control descriptor does not lower (%v); every assertion above is "+
			"vacuous", lerr)
	}
}

// TestStaticCheckedDirectRoutesNeverFallThroughToTheBlindPath pins the safety property
// the cutover creates and the direct routes must not lose.
//
// The support predicate answers "supported" for the fingerprint on EVERY route, so a
// direct route that merely failed to claim would fall through to the ordinary
// extract → coerce path — which knows nothing about constraints and would serve
// `{"answer":…,"confidence":9}` with no carrier and no assertion. That is the one way
// this cutover could turn into an over-claim, so it is asserted as bytes rather than as
// a disposition.
func TestStaticCheckedDirectRoutesNeverFallThroughToTheBlindPath(t *testing.T) {
	b := staticCheckedBundle(schema.ConstraintCheck, "positive", "this > 0")
	const raw = `{"answer": "sunny", "confidence": 9}`
	// The support predicate DOES admit, which is the precondition that makes the
	// fall-through possible.
	if err := SupportsNativeFinalBundle(b); err != nil {
		t.Fatalf("the cutover did not admit support (%v); this hazard would be unreachable and the "+
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
				t.Fatalf("the direct route SERVED %s; a constraint-blind serve of a checked shape is an "+
					"over-claim", res.JSON)
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

// TestStaticCheckedCutoverIsOneChokePoint pins the ROLLBACK property the cutover constant
// claims: flipping it back to false restores the total decline, with NO other edit.
//
// It is asserted STRUCTURALLY because the constant is a compile-time `const` and a test
// cannot flip it. The property that makes the rollback true is that every production path
// which can admit the fingerprint reads the constant, and it does so through exactly TWO
// choke points:
//
//	staticCheckedAdmittedFamily        the SHAPE side (checkSupported / checkSupportedFields /
//	                                   checkSupportedType via staticCheckedAdmittedConstraintNode,
//	                                   and IsAdmittedStaticCheckedFamily)
//	staticCheckedGrantStaticUnaryCall  the ROUTE side (the /call capability)
//
// A third reader, or a shape gate that reached [staticCheckedProfileOf] WITHOUT going
// through staticCheckedAdmittedFamily, would be a path the rollback misses. That is not a
// hypothetical: an earlier revision of this slice had exactly that hole — with the
// constant off, checkSupported and SupportsNativeFinalBundle still ADMITTED the
// fingerprint while IsAdmittedStaticCheckedFamily said false, which is both a broken
// rollback and a gate disagreement.
// staticCheckedChokePointScan is what the guard actually measures, factored out of the
// test so it can be pointed at a DIRECTORY — the real package, or a temp directory
// holding a synthetic production file — rather than only at the tree it ships in.
type staticCheckedChokePointScan struct {
	// readers[file:func] counts references to the cutover CONSTANT.
	readers map[string]int
	// refs[file:func] counts references to staticCheckedProfileOf in ANY form: a direct
	// call, or the function VALUE (assigned to a variable, passed as an argument, stored
	// in a struct). A bypass that aliased the function and invoked the alias would
	// otherwise evade a callee-only scan and reach the fingerprint without the constant.
	refs    map[string]int
	scanned int
}

// staticCheckedScanChokePoints AST-scans every non-test .go file in dir.
func staticCheckedScanChokePoints(t *testing.T, dir string) staticCheckedChokePointScan {
	t.Helper()
	out := staticCheckedChokePointScan{readers: map[string]int{}, refs: map[string]int{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out.scanned++
		file := staticCheckedParseSource(t, filepath.Join(dir, name))
		fn := ""
		// declPos marks the identifiers that DECLARE a name — a func's own name, and a
		// const/var spec's names — so a declaration is not counted as a reference to
		// itself. Without it the `const staticCheckedAdmitsConstraints = true` line would
		// be reported as a file-scope reader of the very constant it declares.
		declPos := map[token.Pos]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if spec, ok := n.(*ast.ValueSpec); ok {
				for _, id := range spec.Names {
					declPos[id.Pos()] = true
				}
				return true
			}
			if decl, ok := n.(*ast.FuncDecl); ok {
				fn = decl.Name.Name
				declPos[decl.Name.Pos()] = true
				return true
			}
			id, ok := n.(*ast.Ident)
			if !ok || declPos[id.Pos()] {
				return true
			}
			switch id.Name {
			case "staticCheckedAdmitsConstraints":
				out.readers[name+":"+fn]++
			case "staticCheckedProfileOf":
				out.refs[name+":"+fn]++
			}
			return true
		})
	}
	return out
}

// staticCheckedChokePointViolations applies the POLICY to a scan, returning one line per
// violation. Separating it from the scan is what lets the same policy be run against a
// synthetic bypass and shown to reject it.
func staticCheckedChokePointViolations(scan staticCheckedChokePointScan) []string {
	wantReaders := map[string]bool{
		"checked_static.go:staticCheckedAdmittedFamily":       true,
		"checked_static.go:staticCheckedGrantStaticUnaryCall": true,
	}
	allowedRefs := map[string]bool{
		// The choke point itself.
		"checked_static.go:staticCheckedAdmittedFamily": true,
		// The PARSE side, which needs the profile's CONTENTS (label, expression, level)
		// rather than a yes/no, and is gated by the ROUTE capability it is handed. It
		// cannot admit on its own: staticCheckedParse serves only when claim.admits(),
		// and the only granting constructor reads the constant.
		"checked_static.go:staticCheckedParse": true,
	}
	var out []string
	for where := range scan.readers {
		if !wantReaders[where] {
			out = append(out, where+" reads the cutover constant; a third reader is a path the "+
				"rollback would miss")
		}
	}
	for where := range scan.refs {
		if !allowedRefs[where] {
			out = append(out, where+" references staticCheckedProfileOf, bypassing "+
				"staticCheckedAdmittedFamily and therefore the cutover constant")
		}
	}
	sort.Strings(out)
	return out
}

// TestStaticCheckedCutoverIsOneChokePoint pins the ROLLBACK property the cutover constant
// claims: flipping it back to false restores the total decline, with NO other edit.
//
// It is asserted STRUCTURALLY because the constant is a compile-time `const` and a test
// cannot flip it. The property that makes the rollback true is that every production path
// which can admit the fingerprint reads the constant, and it does so through exactly TWO
// choke points:
//
//	staticCheckedAdmittedFamily        the SHAPE side (checkSupported / checkSupportedFields /
//	                                   checkSupportedType via staticCheckedAdmittedConstraintNode,
//	                                   and IsAdmittedStaticCheckedFamily)
//	staticCheckedGrantStaticUnaryCall  the ROUTE side (the /call capability)
//
// The scan covers EVERY non-test file in the package and every REFERENCE form, not just a
// direct call: aliasing staticCheckedProfileOf to a variable and invoking the alias would
// reach the fingerprint without the constant, so a callee-only scan would miss it.
//
// TestStaticCheckedChokePointGuardIsProvenToBite writes a real synthetic production file
// and drives this same scan+policy over it, so the guard's teeth are part of the suite
// rather than a manual experiment.
func TestStaticCheckedCutoverIsOneChokePoint(t *testing.T) {
	scan := staticCheckedScanChokePoints(t, staticCheckedSourcePath(t, ""))
	if scan.scanned < 2 {
		t.Fatalf("only %d production file(s) were scanned; the package-wide claim would be about one "+
			"file", scan.scanned)
	}
	if v := staticCheckedChokePointViolations(scan); len(v) != 0 {
		t.Fatalf("the cutover is not one choke point:\n  %s", strings.Join(v, "\n  "))
	}
	// Both halves must be PRESENT, or "exactly two readers" would be satisfied by zero.
	for _, where := range []string{
		"checked_static.go:staticCheckedAdmittedFamily",
		"checked_static.go:staticCheckedGrantStaticUnaryCall",
	} {
		if scan.readers[where] == 0 {
			t.Errorf("%s does NOT read the cutover constant; flipping it would not close that half",
				where)
		}
	}
	if len(scan.refs) == 0 {
		t.Fatal("nothing references staticCheckedProfileOf in the package; this guard is vacuous")
	}
}

// TestStaticCheckedChokePointGuardIsProvenToBite writes REAL synthetic production files
// into a temp directory and requires the scan+policy above to reject each one.
//
// It exists because the guard's value is entirely in what it REJECTS, and scanning only
// the tree it ships in can never demonstrate that. Each case is a bypass a future change
// could plausibly introduce, including the two the callee-only scan used to miss: taking
// the function VALUE rather than calling it.
func TestStaticCheckedChokePointGuardIsProvenToBite(t *testing.T) {
	const header = "package debaml\n\nimport \"github.com/invakid404/baml-rest/internal/schema\"\n\n"
	for _, tc := range []struct {
		name, body, wantSubstr string
	}{{
		name:       "a third reader of the cutover constant",
		body:       "func zzBypass(b *schema.Bundle) bool {\n\treturn staticCheckedAdmitsConstraints && b != nil\n}\n",
		wantSubstr: "reads the cutover constant",
	}, {
		name:       "a direct call to staticCheckedProfileOf",
		body:       "func zzBypass(b *schema.Bundle) bool {\n\t_, ok := staticCheckedProfileOf(b)\n\treturn ok\n}\n",
		wantSubstr: "references staticCheckedProfileOf",
	}, {
		name:       "the FUNCTION VALUE of staticCheckedProfileOf assigned to a variable",
		body:       "func zzBypass(b *schema.Bundle) bool {\n\tf := staticCheckedProfileOf\n\t_, ok := f(b)\n\treturn ok\n}\n",
		wantSubstr: "references staticCheckedProfileOf",
	}, {
		name: "the FUNCTION VALUE passed as an argument",
		body: "func zzApply(f func(*schema.Bundle) (staticCheckedProfile, bool), b *schema.Bundle) bool {\n" +
			"\t_, ok := f(b)\n\treturn ok\n}\n\n" +
			"func zzBypass(b *schema.Bundle) bool { return zzApply(staticCheckedProfileOf, b) }\n",
		wantSubstr: "references staticCheckedProfileOf",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A second, INNOCENT file, so the scan is shown to be selective rather than
			// failing on any directory it is pointed at.
			if err := os.WriteFile(filepath.Join(dir, "innocent.go"),
				[]byte(header+"func zzInnocent(b *schema.Bundle) bool { return b == nil }\n"), 0o600); err != nil {
				t.Fatalf("write innocent.go: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "zz_bypass.go"), []byte(header+tc.body), 0o600); err != nil {
				t.Fatalf("write zz_bypass.go: %v", err)
			}
			// A _test.go file carrying the SAME bypass must be IGNORED — the policy is
			// about production code, and counting tests would make it unusable.
			if err := os.WriteFile(filepath.Join(dir, "zz_ignored_test.go"), []byte(header+tc.body), 0o600); err != nil {
				t.Fatalf("write zz_ignored_test.go: %v", err)
			}

			scan := staticCheckedScanChokePoints(t, dir)
			if scan.scanned != 2 {
				t.Fatalf("scanned %d files, want exactly 2 (the _test.go one must be skipped)", scan.scanned)
			}
			v := staticCheckedChokePointViolations(scan)
			if len(v) == 0 {
				t.Fatalf("the guard accepted a synthetic production bypass; it has no teeth")
			}
			named, matched := false, false
			for _, line := range v {
				if strings.HasPrefix(line, "zz_bypass.go:zzBypass ") {
					named = true
					if strings.Contains(line, tc.wantSubstr) {
						matched = true
					}
				}
				if strings.HasPrefix(line, "innocent.go:") || strings.HasPrefix(line, "zz_ignored_test.go:") {
					t.Errorf("the guard flagged %q; it must reject only the production bypass", line)
				}
			}
			if !named {
				t.Errorf("the violations do not name zz_bypass.go:zzBypass; got %v", v)
			}
			if !matched {
				t.Errorf("the violation naming the bypass does not say %q; got %v", tc.wantSubstr, v)
			}
		})
	}
}

// TestStaticCheckedCutoverHasNoRuntimeWriter is the structural half of "the cutover is
// a compile-time decision": no file — production OR test — may reintroduce a mutable
// switch that widens the claim at runtime.
//
// Slice 7.2b-2 carried an `atomic.Bool` test seam so the CLOSED state could be shown to
// be the only thing holding the fingerprint back. It is gone with the cutover, and this
// guard is what keeps it gone: a mutable switch a test can flip is a way for a test to
// make production claim a schema no gate agreed to, which is precisely the failure mode
// the scope is about. Attribution is now carried by the one-property siblings, which
// need no switch at all.
func TestStaticCheckedCutoverHasNoRuntimeWriter(t *testing.T) {
	root := staticCheckedSourcePath(t, "")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		scanned++
		file := staticCheckedParseSource(t, filepath.Join(root, name))
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			switch id.Name {
			case "staticCheckedSeamOpen", "OpenStaticCheckedSeamForTest":
				t.Errorf("%s names %s; the runtime seam was removed by the 7.2b-3 cutover and must not "+
					"come back — a switch a test can flip lets a test widen production's claim",
					name, id.Name)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no files were scanned; this guard would be vacuous")
	}
	// NON-VACUITY: the scan really can see an identifier in these files — proven by
	// finding the constant the cutover DOES use, in the file it lives in.
	found := false
	ast.Inspect(staticCheckedParseSource(t, staticCheckedSourcePath(t, "checked_static.go")), func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "staticCheckedAdmitsConstraints" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("the scan did not find staticCheckedAdmitsConstraints in checked_static.go; it would " +
			"not have found a reintroduced seam either")
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

// TestStaticCheckedSeamIsTheOnlySwitch pins STRUCTURALLY that the cutover is a
// compile-time decision: the switch is an untyped boolean CONSTANT, and it is now true.
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
			if !ok || lit.Name != "true" {
				t.Fatalf("the cutover constant is initialised to %v, want the literal true", vs.Values[0])
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
