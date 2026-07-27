//go:build integration

// Package constraintoracle is the de-BAML constraint-evaluator differential
// oracle. It proves that the native constraint evaluator
// (internal/debaml.EvaluateConstraint / RenderConstraintExpression, plus the
// native->minijinja value model) reproduces BAML v0.223.0's @assert/@check
// semantics, and — where it does not — pins the exact divergence.
//
// # What is under test, and what is NOT
//
// This is the EVALUATOR slice only. Nothing in internal/debaml's admission path
// changed: every reachable @assert/@check still declines to BAML
// (internal/debaml.checkSupported, pinned by
// TestNativeDeclines_Constraints). No Checked[T] carrier, no wire emission, no
// assert-error rendering — those are the separate serving slice. What this
// package establishes is the foundation that slice stands on: given the same
// JinjaExpression and the same value, do the two engines agree?
//
// # Why a separate package
//
// Same reason as internal/nativeprompt/staticoracle: this test binary links the
// STOCK upstream github.com/boundaryml/baml v0.223.0 through a generated client,
// while other de-BAML tests link the PATCHED dynclient fork. Keeping them in
// separate binaries avoids two BAML runtimes in one process and makes the stock
// provenance obvious. All files are behind //go:build integration and nothing
// here is reachable from a production build.
//
// # How it works
//
//   - corpus_test.go enumerates 449 (expression, `this`) cases. The expression
//     surface is enumerated from the minijinja-go/v2 v2.16.0 API — every
//     registered filter, test, global function and operator — NOT inferred from
//     what the prompt renderer happens to use, plus the operator/value cases the
//     scope calls for (arithmetic, membership, length, regex_match, sum,
//     Python-style format, float/int mixing, unicode, non-boolean results,
//     unknown filter/method).
//   - fixture.go renders those cases into baml_src/constraints.baml (batched per
//     `this` value, isolated where stock errors — see fixture.go).
//   - The STOCK leg drives Parse.<Method> on the checked-in generated client and
//     reads the resulting Checked.Checks[label].Status, or the coercion error.
//   - The NATIVE leg evaluates the SAME expression — specifically the
//     JinjaExpression stock reports back in Check.Expression, not the .baml
//     attribute text, because BAML's attribute lexer doubles backslashes — over
//     the corpus's ConstraintValue.
//   - TestConstraintProfileIsFailClosed then asserts the CONTRACT, live: for
//     every case, native must reproduce stock exactly or refuse with
//     ErrConstraintUnsupported. There is no allowance to widen and no bucket for
//     "known difference" — a native answer stock did not produce is a failure.
//     Both outcomes are also pinned per case, so a change on either side has to
//     be acknowledged rather than absorbed.
//
// # Regenerating
//
// The .baml fixture is generated from the corpus, and the client from the .baml:
//
//	BAML_CONSTRAINT_FIXTURE_WRITE=1 go test -tags integration \
//	  ./internal/debaml/constraintoracle -run TestConstraintFixtureDrift
//	cd internal/debaml/testdata/constraint_oracle && \
//	  npx --offline @boundaryml/baml@0.223.0 generate && \
//	  goimports -w baml_client && gofmt -w baml_client
//
// # Running
//
//	CGO_ENABLED=1 go test -tags integration ./internal/debaml/constraintoracle
//
// Requires CGO and the stock BAML v0.223.0 CFFI library (auto-located under the
// user BAML cache dir), exactly like the #597/#603 oracles.
//
// ARCHITECTURE MATTERS HERE. Go's out-of-range int64(float64) conversion is
// implementation-defined: arm64 saturates, amd64 does not. Two of the
// divergences this corpus pins — the round-3 `op_i64max` case and the round-10
// integral-float `2.0 ** 63` promotion, where minijinja-Go's Pow reads its
// operands through AsInt and hands int64() a value at 2^63 — therefore only
// SHOW on linux/amd64. The dedicated workflow pins that runner for exactly this
// reason, so a local darwin/arm64 pass is a weaker signal than a green CI job.
//
// UNLIKE those, this package is NOT a hand-run artifact: .github/workflows/
// constraint-oracle.yml runs exactly this command on every change under
// internal/debaml/**, because the fail-closed contract is only worth as much as
// its enforcement. (The general integration matrix runs
// `go test -tags=integration ./integration/...`, which does not reach here —
// nor does `go build -tags integration ./...`, which skips _test.go files and
// testdata directories.) A local run and a CI run are not interchangeable: the
// 64-bit numeric boundary this evaluator guards produced DIFFERENT answers on
// darwin/arm64 and linux/amd64 before the guard existed, so the CI lane is the
// authority.
package constraintoracle

import (
	stdjson "encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"

	"github.com/invakid404/baml-rest/internal/debaml"
	bamlclient "github.com/invakid404/baml-rest/internal/debaml/testdata/constraint_oracle/baml_client"
)

const (
	// fixtureBAMLPath is the checked-in .baml the generated client was produced
	// from; renderFixtureBAML must reproduce it byte for byte.
	fixtureBAMLPath = "../testdata/constraint_oracle/baml_src/constraints.baml"
	// writeFixtureEnv rewrites the fixture instead of comparing (see the package comment).
	writeFixtureEnv = "BAML_CONSTRAINT_FIXTURE_WRITE"
	// fixtureSrcDir holds every .baml the stock client was generated from.
	fixtureSrcDir = "../testdata/constraint_oracle/baml_src"
	// sourceMapPath is the generated client's verbatim copy of those sources.
	sourceMapPath = "../testdata/constraint_oracle/baml_client/baml_source_map.go"

	// rootGoModPath is the root module's go.mod, from this package's CWD.
	rootGoModPath = "../../../go.mod"
	// bamlModulePath / wantBAMLModuleVersion / wantBAMLRuntimeVersion pin the
	// stock oracle dependency; the CFFI reports the bare semver.
	bamlModulePath         = "github.com/boundaryml/baml"
	wantBAMLModuleVersion  = "v0.223.0"
	wantBAMLRuntimeVersion = "0.223.0"
)

// ---------------------------------------------------------------------------
// Guard: the oracle really is stock BAML v0.223.0.
// ---------------------------------------------------------------------------

// TestBAMLVersionPinned requires the LOADED CFFI runtime and the root go.mod pin
// to be exactly v0.223.0, so a green differential can never be attributed to a
// different BAML. The runtime check is the load-bearing one — it reads the
// native library that actually parsed every response in this binary.
func TestBAMLVersionPinned(t *testing.T) {
	if got := baml_go.BamlVersion(); got != wantBAMLRuntimeVersion {
		t.Fatalf("loaded BAML CFFI runtime reports version %q, want exactly %q", got, wantBAMLRuntimeVersion)
	}
	raw, err := os.ReadFile(rootGoModPath)
	if err != nil {
		t.Fatalf("read %s: %v", rootGoModPath, err)
	}
	if !strings.Contains(string(raw), bamlModulePath+" "+wantBAMLModuleVersion) {
		t.Fatalf("root go.mod does not require %s %s", bamlModulePath, wantBAMLModuleVersion)
	}
	if strings.Contains(string(raw), "=> "+bamlModulePath) {
		t.Fatalf("root go.mod replaces %s; the oracle must link the stock module", bamlModulePath)
	}
}

// ---------------------------------------------------------------------------
// Guard: the corpus and the generated client describe the same project.
// ---------------------------------------------------------------------------

// TestConstraintFixtureDrift pins baml_src/constraints.baml as a pure function of
// the corpus. Without it, a corpus edit would silently stop matching the
// generated client and the differential would quietly test the wrong thing.
func TestConstraintFixtureDrift(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range constraintCases {
		if seen[c.Label] {
			t.Fatalf("duplicate case label %q", c.Label)
		}
		seen[c.Label] = true
		if _, ok := groupByName(c.Group); !ok {
			t.Fatalf("case %q references unknown group %q", c.Label, c.Group)
		}
		// A Note is required exactly where the profile COSTS something: native
		// refused an expression stock actually decided. Where stock also refused
		// (outError / outNoChecks), the two agree in substance and a Note would
		// be noise. A Note anywhere else is stale.
		stockDecided := c.Stock == outTrue || c.Stock == outFalse
		wantNote := c.Native == outUnsupported && stockDecided
		if wantNote && c.Note == "" {
			t.Fatalf("case %q: native refuses an expression stock decided (stock=%s) but carries no Note naming the guard",
				c.Label, c.Stock)
		}
		if !wantNote && c.Note != "" {
			t.Fatalf("case %q carries a Note but is not a profile cost (stock=%s native=%s)",
				c.Label, c.Stock, c.Native)
		}
	}

	want := renderFixtureBAML()
	if os.Getenv(writeFixtureEnv) != "" {
		if err := os.WriteFile(fixtureBAMLPath, []byte(want), 0o644); err != nil {
			t.Fatalf("write %s: %v", fixtureBAMLPath, err)
		}
		t.Logf("%s rewritten from the corpus; regenerate the stock client (see the package comment)", fixtureBAMLPath)
		return
	}
	got, err := os.ReadFile(fixtureBAMLPath)
	if err != nil {
		t.Fatalf("read %s: %v", fixtureBAMLPath, err)
	}
	if string(got) != want {
		t.Fatalf("%s is stale: it no longer matches renderFixtureBAML() over the corpus.\n"+
			"Regenerate with %s=1 and re-run the stock BAML generator (see the package comment).",
			fixtureBAMLPath, writeFixtureEnv)
	}
}

// TestGeneratedClientIsFresh proves the checked-in stock client was generated
// from the checked-in .baml sources, and not from an older revision of them.
//
// This is not paranoia. TestConstraintFixtureDrift only relates the corpus to
// baml_src; on its own, an isolated case could keep its label and its pinned
// outcome while the client the stock leg actually drives still carries the OLD
// expression — and the differential would quietly compare native against the
// wrong oracle. The generated client embeds every source file verbatim in
// baml_source_map.go's file_map, so comparing that map against the files on
// disk closes the loop: source -> client -> test.
func TestGeneratedClientIsFresh(t *testing.T) {
	embedded := parseGeneratedSourceMap(t)
	if len(embedded) == 0 {
		t.Fatal("generated client embeds no source map")
	}
	entries, err := os.ReadDir(fixtureSrcDir)
	if err != nil {
		t.Fatalf("read %s: %v", fixtureSrcDir, err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".baml" {
			continue
		}
		seen++
		onDisk, err := os.ReadFile(filepath.Join(fixtureSrcDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		inClient, ok := embedded[e.Name()]
		if !ok {
			t.Errorf("%s is not in the generated client's source map: the client is stale, regenerate it (see the package comment)", e.Name())
			continue
		}
		if inClient != string(onDisk) {
			t.Errorf("%s differs from the copy embedded in the generated client: the client is STALE and the stock leg is driving a different project.\n"+
				"Regenerate it (see the package comment).\n  on disk: %d bytes\n  in client: %d bytes",
				e.Name(), len(onDisk), len(inClient))
		}
	}
	if seen != len(embedded) {
		t.Errorf("baml_src has %d .baml files but the generated client embeds %d; the client is stale", seen, len(embedded))
	}
}

// parseGeneratedSourceMap reads baml_source_map.go's file_map literal.
//
// The map and its accessor are unexported in the generated package, so the
// contents are recovered from the syntax tree rather than called. Parsing beats
// string matching here: the generated literal's escaping is BAML's choice, and
// strconv.Unquote gives the bytes it actually denotes.
func parseGeneratedSourceMap(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, sourceMapPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", sourceMapPath, err)
	}
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "file_map" || len(vs.Values) != 1 {
			return true
		}
		lit, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, kerr := strconv.Unquote(kv.Key.(*ast.BasicLit).Value)
			val, verr := strconv.Unquote(kv.Value.(*ast.BasicLit).Value)
			if kerr != nil || verr != nil {
				t.Fatalf("unquote %s entry: %v / %v", sourceMapPath, kerr, verr)
			}
			out[key] = val
		}
		return false
	})
	return out
}

// ---------------------------------------------------------------------------
// The stock leg.
// ---------------------------------------------------------------------------

// stockResult is one stock parse of one fixture function.
type stockResult struct {
	// valueJSON is encoding/json over the decoded Checked.Value — the BAML value
	// the predicate ran against, as BAML itself modelled it.
	valueJSON string
	checks    map[string]shared.Check
	err       error
}

// stockCache memoizes one parse per fixture method: a group batch is driven once
// and read by every case in it. No mutex — every subtest here is sequential (the
// CFFI runtime is process-global and the suite deliberately does not parallelise
// over it).
var stockCache = map[string]stockResult{}

// runStock drives Parse.<method>(text) on the generated client by reflection.
//
// Reflection rather than a generated dispatch table because every fixture
// function returns a DIFFERENT class type (and a differently-instantiated
// Checked[T] inside it), which Go generics cannot abstract over — while the
// shape this test needs (field V, then Value and the non-generic
// map[string]shared.Check) is uniform across all of them.
func runStock(t *testing.T, method, text string) stockResult {
	t.Helper()
	if r, ok := stockCache[method]; ok {
		return r
	}
	fn := reflect.ValueOf(bamlclient.Parse).MethodByName(method)
	if !fn.IsValid() {
		t.Fatalf("generated client has no Parse.%s; the client is stale relative to the corpus", method)
	}
	out := fn.Call([]reflect.Value{reflect.ValueOf(text)})
	if errAny := out[1].Interface(); errAny != nil {
		r := stockResult{err: errAny.(error)}
		stockCache[method] = r
		return r
	}
	field := out[0].FieldByName("V")
	if !field.IsValid() {
		t.Fatalf("Parse.%s returned no V field", method)
	}
	valueJSON, err := stdjson.Marshal(field.FieldByName("Value").Interface())
	if err != nil {
		t.Fatalf("marshal Parse.%s value: %v", method, err)
	}
	checks, _ := field.FieldByName("Checks").Interface().(map[string]shared.Check)
	r := stockResult{valueJSON: string(valueJSON), checks: checks}
	stockCache[method] = r
	return r
}

// stockOutcome maps one case's stock observation onto a legOutcome.
func stockOutcome(r stockResult, label string) (legOutcome, shared.Check) {
	if r.err != nil {
		return outError, shared.Check{}
	}
	check, ok := r.checks[label]
	if !ok {
		return outNoChecks, shared.Check{}
	}
	switch check.Status {
	case "succeeded":
		return outTrue, check
	case "failed":
		return outFalse, check
	default:
		return legOutcome(check.Status), check
	}
}

// nativeOutcome evaluates the case natively.
func nativeOutcome(this debaml.ConstraintValue, expr string) (legOutcome, error) {
	ok, err := debaml.EvaluateConstraint(this, expr)
	if err != nil {
		return outUnsupported, err
	}
	if ok {
		return outTrue, nil
	}
	return outFalse, nil
}

// ---------------------------------------------------------------------------
// (1) Value-model parity.
// ---------------------------------------------------------------------------

// TestConstraintValueModelParity proves the native ConstraintValue models the
// SAME document BAML's coercion produced for each group's `this`.
//
// The authority is stock's own decoded Checked.Value: whatever BAML built from
// the assistant text is what its predicate ran against, so if native's value
// marshals to the same JSON document, the two predicates saw the same data.
// The comparison is order-insensitive (both sides are re-decoded into `any`)
// because stock's side goes through Go's encoding/json, which sorts map keys —
// key ORDER is proven separately, and more directly, by the ordering
// expressions in the differential (this_map_order, this_cls_order,
// this_map_keys), which observe the order INSIDE the engine.
func TestConstraintValueModelParity(t *testing.T) {
	for _, g := range constraintGroups {
		t.Run(g.Name, func(t *testing.T) {
			method, ok := anyBatchMethod(g.Name)
			if !ok {
				t.Skipf("group %s has no batch function (every case isolated)", g.Name)
			}
			r := runStock(t, method, g.Input)
			if r.err != nil {
				t.Fatalf("stock parse of %s failed: %v", method, r.err)
			}
			nativeJSON, err := stdjson.Marshal(g.This)
			if err != nil {
				t.Fatalf("marshal native value: %v", err)
			}
			if !sameJSONDocument(t, r.valueJSON, string(nativeJSON)) {
				t.Fatalf("value-model mismatch for group %s:\n  stock  %s\n  native %s",
					g.Name, r.valueJSON, nativeJSON)
			}
		})
	}
}

func anyBatchMethod(group string) (string, bool) {
	for _, c := range constraintCases {
		if c.Group == group && !c.isolated() {
			return c.bamlMethod(), true
		}
	}
	return "", false
}

func sameJSONDocument(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv any
	if err := stdjson.Unmarshal([]byte(a), &av); err != nil {
		t.Fatalf("decode %q: %v", a, err)
	}
	if err := stdjson.Unmarshal([]byte(b), &bv); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}

// ---------------------------------------------------------------------------
// (2) The expression differential.
// ---------------------------------------------------------------------------

// TestConstraintExpressionDifferential is the proof: for every case, drive the
// expression through stock BAML v0.223.0 and through the native evaluator and
// require BOTH to produce their pinned outcome.
//
// Pinning both legs (rather than only asserting they match) makes this a
// regression pin as well as a differential: a case where the two currently
// AGREE cannot silently start diverging, and a case where they currently
// DIVERGE cannot silently change shape — either would fail here and force the
// Note to be revisited.
//
// It also pins, per case, the JinjaExpression stock retained
// (Check.Expression). That is what the native leg is fed, and it is where a
// genuinely surprising BAML behaviour lives: the @check attribute lexer DOUBLES
// backslashes, so a BAML constraint cannot express a regex escape at all
// (f_regex_class, f_regex_word, f_regex_unicode, f_lines, py_str_splitlines all
// retain `\\` where the source wrote `\`).
func TestConstraintExpressionDifferential(t *testing.T) {
	for _, c := range constraintCases {
		t.Run(c.Label, func(t *testing.T) {
			g, ok := groupByName(c.Group)
			if !ok {
				t.Fatalf("unknown group %q", c.Group)
			}

			r := runStock(t, c.bamlMethod(), g.Input)
			gotStock, check := stockOutcome(r, c.Label)
			if gotStock != c.Stock {
				t.Errorf("stock outcome for %q: got %s, want %s (stock err: %v)",
					c.Expr, gotStock, c.Stock, r.err)
			}
			if check.Expression != "" && check.Expression != c.retainedExpr() {
				t.Errorf("stock retained a different JinjaExpression:\n  got  %q\n  want %q",
					check.Expression, c.retainedExpr())
			}

			gotNative, nativeErr := nativeOutcome(g.This, c.retainedExpr())
			if gotNative != c.Native {
				t.Errorf("native outcome for %q: got %s, want %s (native err: %v)",
					c.retainedExpr(), gotNative, c.Native, nativeErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (3) The divergence inventory.
// ---------------------------------------------------------------------------

// wantInventory is the measured shape of the corpus, pinned so that a NEW
// divergence cannot appear without someone editing this number and writing the
// Note that goes with it.
//
// The two buckets, and what separates them:
//
//   - agree — native produced exactly what stock produced (the same boolean, or
//     both refused). This is proven parity.
//   - unsupported — native refused with ErrConstraintUnsupported. SAFE by
//     construction: the caller declines to BAML, so no wrong result can be
//     served. This is the measured COST of the profile, not a divergence.
//
// There is deliberately no third bucket. A case where native answers something
// stock did not is a defect, and TestConstraintProfileIsFailClosed fails on it.
const (
	wantAgree       = 267
	wantUnsupported = 182
)

// TestConstraintProfileIsFailClosed is the load-bearing assertion of this
// package, and the reason the evaluator can be handed to a serving slice.
//
// For EVERY case it requires, live against stock BAML v0.223.0:
//
//	native outcome == stock outcome   OR   native outcome == outUnsupported
//
// i.e. native either reproduces BAML exactly or declines to decide. Any other
// combination — native answering true where stock answered false, native
// answering at all where stock refused the value — fails here. Nothing about
// this test can be satisfied by recording a known difference: there is no
// allowance to widen.
//
// It runs over the LIVE legs, not the pinned columns, so it cannot pass because
// the corpus was edited to match.
func TestConstraintProfileIsFailClosed(t *testing.T) {
	var violations []string
	for _, c := range constraintCases {
		g, ok := groupByName(c.Group)
		if !ok {
			t.Fatalf("case %q references unknown group %q", c.Label, c.Group)
		}
		gotStock, _ := stockOutcome(runStock(t, c.bamlMethod(), g.Input), c.Label)
		gotNative, nativeErr := nativeOutcome(g.This, c.retainedExpr())

		if gotNative == outUnsupported {
			if !errors.Is(nativeErr, debaml.ErrConstraintUnsupported) {
				violations = append(violations, fmt.Sprintf(
					"%s: native refused with an error that does not wrap ErrConstraintUnsupported: %v",
					c.Label, nativeErr))
			}
			continue
		}
		if gotNative != gotStock {
			violations = append(violations, fmt.Sprintf(
				"%s: native answered %s where stock produced %s — expr %q",
				c.Label, gotNative, gotStock, c.retainedExpr()))
		}
	}
	if len(violations) > 0 {
		t.Fatalf("the evaluator is NOT fail-closed; %d case(s) produce a result stock does not:\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestConstraintProfileCost pins how much the fail-closed profile gives up, so
// a change that quietly widens or narrows it has to be acknowledged.
func TestConstraintProfileCost(t *testing.T) {
	var agree, unsupported int
	for _, c := range constraintCases {
		switch {
		case c.Native == outUnsupported:
			unsupported++
		case c.Stock == c.Native:
			agree++
		default:
			t.Fatalf("case %q pins native=%s against stock=%s; the corpus may only pin agreement or outUnsupported",
				c.Label, c.Native, c.Stock)
		}
	}
	if agree != wantAgree || unsupported != wantUnsupported {
		t.Fatalf("profile cost changed: agree=%d (want %d), unsupported=%d (want %d)",
			agree, wantAgree, unsupported, wantUnsupported)
	}
	if agree+unsupported != len(constraintCases) {
		t.Fatalf("cost covers %d of %d cases", agree+unsupported, len(constraintCases))
	}
}

// ---------------------------------------------------------------------------
// (4) The jinja_helpers.rs source tests, driven through the real oracle.
// ---------------------------------------------------------------------------

// TestJinjaHelpersSourceTests reproduces BAML's OWN unit tests for the
// constraint environment (jinja_helpers.rs:102-218) against the native
// evaluator. They exercise render_expression directly rather than through a
// @check attribute, which matters: the attribute lexer's backslash doubling
// makes the regex_match cases unreachable from .baml, so this is the only place
// the pinned regex behaviour can be checked at all.
func TestJinjaHelpersSourceTests(t *testing.T) {
	list := debaml.ListValue([]debaml.ConstraintValue{
		debaml.IntValue(1), debaml.IntValue(2), debaml.IntValue(3),
	})
	phone := debaml.StringValue("(123)456-7890")

	cases := []struct {
		name string
		this debaml.ConstraintValue
		expr string
		want string
	}{
		// jinja_helpers.rs:122-133 (render_expression).
		{"literal", list, "1", "1"},
		{"arithmetic", list, "1 + 1", "2"},
		{"length", list, "this|length > 2", "true"},
		// jinja_helpers.rs:136-173 (regex_match).
		{"regex_substring", phone, `this|regex_match("123")`, "true"},
		// jinja_helpers.rs:207-218 (the BAML sum filter).
		{"sum_ints", list, "[1,2]|sum", "3"},
		{"sum_mixed", list, "[1,2.5]|sum", "3.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := debaml.RenderConstraintExpression(tc.this, tc.expr)
			if err != nil {
				t.Fatalf("render %q: %v", tc.expr, err)
			}
			if got != tc.want {
				t.Fatalf("render %q = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}

	// jinja_helpers.rs:136-173's phone-number regex is NOT reproducible either,
	// for a different reason: it carries a `-` inside a character class, and the
	// round-6 arithmetic gate does not skip string literals — skipping them would
	// need string lexing that could itself fail open, which is the mistake the
	// whitelist exists to undo. Assert the refusal so the cost stays visible.
	if _, err := debaml.RenderConstraintExpression(phone,
		`this|regex_match("\\(?\\d{3}\\)?[-.\\s]?\\d{3}[-.\\s]?\\d{4}")`); err == nil {
		t.Error("the phone regex carries an arithmetic byte and is expected to be refused")
	}

	// jinja_helpers.rs:176-204 (Python-style str.format) is NOT reproducible:
	// it needs the pycompat unknown-method callback, which minijinja-Go v2.16.0
	// has no hook for. Assert the fail-closed shape instead, so a future
	// minijinja-Go that grows the hook is noticed here.
	if _, err := debaml.RenderConstraintExpression(list, `"{:,}".format(1234567)`); err == nil {
		t.Fatal("pycompat str.format now evaluates natively; the pycompat gap has closed — re-check the corpus notes")
	}
}
