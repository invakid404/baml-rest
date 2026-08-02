//go:build integration

package profileoracle

// Stock BAML v0.223.0 differential for internal/bamlprofile's CONSTRAINT lowerer.
//
// It links the UNTOUCHED github.com/boundaryml/baml@v0.223.0 runtime via CFFI and
// runs every constraint row through BamlRuntime.CallFunctionParse — the CFFI
// response-parse path (language_client_go/pkg/runtime.go:178-213). That path runs
// BAML's real coercer, so the constraints on the function's return type go
// through run_user_checks -> evaluate_predicate -> validate_asserts. Rendering a
// prompt would exercise none of it.
//
// Run:
//
//	CGO_ENABLED=1 go test -tags integration ./internal/bamlprofile/profileoracle
//
// Regenerate the recorded version/source-map fixture after an intended corpus
// change:
//
//	WRITE_CONSTRAINT_ORACLE_FIXTURE=1 CGO_ENABLED=1 go test -tags integration \
//	  ./internal/bamlprofile/profileoracle -run TestConstraintOracleFixture

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

const constraintFixturePath = "testdata/constraint_oracle.json"

// constraintParseTimeout bounds a single CallFunctionParse.
//
// A healthy parse answers in milliseconds. The deadline exists because a Rust
// panic inside BAML's parse path unwinds the tokio worker that owns the request,
// so the CFFI call never delivers a result and the Go side would block FOREVER —
// the same failure mode fault_integration_test.go documents for BuildRequest. A
// timeout is NEVER converted into a verdict here: it is a loud harness failure
// pointing at the subprocess leg, because "it hung" is not evidence of any
// particular outcome class.
const constraintParseTimeout = 60 * time.Second

// --- the stock decode type map ---------------------------------------------

// checkedTypeBinding maps a corpus return type to the CFFI checked-type NAME the
// stock client will look up, and the Go type it must find there.
//
// When a return type declares any @check, the CFFI wraps the value in a
// CffiValueChecked whose name is `CHECKED_TYPES.<inner type's union name>`
// (language_client_cffi/src/ctypes/baml_value_with_meta_encode.rs:69-101, with
// the checks popped from the inner type). The stock Go client resolves that name
// through a package-global type map and PANICS if it is missing
// (baml_go/serde/decode.go:290-312) — inside a cgo callback, which would abort
// the test binary. So the bindings are declared here and
// TestConstraintCheckedTypesAreRegistered proves the corpus never needs one that
// is absent.
//
// The union names come from ToUnionName (baml-types/src/ir_type/mod.rs:935-1002):
// a primitive is its own spelling, a class/enum is its declared name, and a list
// is `List__<element>`. The Go side of a class/enum decodes to the client's
// DYNAMIC fallback (serde/decode.go:186-220) because this harness registers no
// generated struct for the corpus types — which is fine: the differential
// compares CHECKS, not the decoded value's Go shape.
//
// "string" is registered although NO corpus row uses it: it is needed by
// TestStockSkipsConstraintsOnBareStringReturn, which parses a checked bare-string
// return precisely to prove stock reports no checks for it.
type checkedTypeBinding struct {
	name string
	typ  reflect.Type
}

func checkedTypeBindings() map[string]checkedTypeBinding {
	return map[string]checkedTypeBinding{
		"int":      {"int", reflect.TypeOf(baml.Checked[int64]{})},
		"string":   {"string", reflect.TypeOf(baml.Checked[string]{})},
		"Color":    {"Color", reflect.TypeOf(baml.Checked[baml.DynamicEnum]{})},
		"C":        {"C", reflect.TypeOf(baml.Checked[baml.DynamicClass]{})},
		"string[]": {"List__string", reflect.TypeOf(baml.Checked[[]string]{})},
	}
}

var constraintTypeMapOnce sync.Once

// ensureConstraintTypeMap installs the checked-type map on the stock client.
//
// baml.SetTypeMap is a package GLOBAL, so it is installed once per test binary.
// It cannot disturb the prompt differential: BuildRequest returns an object
// handle and never runs serde.Decode (pkg/callbacks.go:184-190,220-226).
func ensureConstraintTypeMap() {
	constraintTypeMapOnce.Do(func() {
		m := map[string]reflect.Type{}
		for _, b := range checkedTypeBindings() {
			m["CHECKED_TYPES."+b.name] = b.typ
		}
		baml.SetTypeMap(m)
	})
}

// TestConstraintCheckedTypesAreRegistered proves the corpus can never ask the
// stock client for a checked type the harness did not register.
//
// The failure it prevents is not a red test: decode.go panics inside a cgo
// callback, which ABORTS the test binary with a Rust-ish stack and no row name.
// Catching it here, in a CFFI-free assertion that runs before any parse, turns
// that into a one-line message naming the row and the missing binding.
func TestConstraintCheckedTypesAreRegistered(t *testing.T) {
	bindings := checkedTypeBindings()
	used := map[string]bool{}
	for _, r := range ConstraintCorpus() {
		if !r.DeclaresCheck() {
			continue
		}
		b, ok := bindings[r.ReturnType]
		if !ok {
			t.Errorf("row %q declares a @check on return type %q, but no CHECKED_TYPES binding is registered for it; "+
				"add one to checkedTypeBindings or the stock client's decode will panic inside a cgo callback",
				r.ID, r.ReturnType)
			continue
		}
		used[r.ReturnType] = true
		if b.typ.Kind() != reflect.Struct {
			t.Errorf("binding for %q is not a struct type", r.ReturnType)
			continue
		}
		if _, ok := b.typ.FieldByName("Value"); !ok {
			t.Errorf("binding for %q has no Value field; decodeCheckedValue sets one", r.ReturnType)
		}
		if _, ok := b.typ.FieldByName("Checks"); !ok {
			t.Errorf("binding for %q has no Checks field; decodeCheckedValue sets one", r.ReturnType)
		}
	}
	if len(used) == 0 {
		t.Fatal("no corpus row declares a @check; the Checked<T> half of the differential would prove nothing")
	}
	t.Logf("checked-type bindings exercised by the corpus: %d of %d registered", len(used), len(bindings))
}

// --- the stock leg ----------------------------------------------------------

// stockConstraintOutcome parses one row through stock BAML and classifies the
// result into the same normalized outcome the profile leg produces.
func stockConstraintOutcome(t *testing.T, rt *baml.BamlRuntime, r ConstraintRow, env map[string]string) ConstraintOutcome {
	t.Helper()

	// CallFunctionParse's kwargs are not the function's parameters: the CFFI reads
	// `text` (the raw response to parse) and `stream`
	// (language_client_cffi/src/ffi/functions.rs:163-183). Every corpus function is
	// parameterless, so these two are the whole argument set.
	args := baml.BamlFunctionArguments{
		Kwargs: map[string]any{"text": r.Raw, "stream": false},
		Env:    env,
	}
	encoded, err := args.Encode()
	if err != nil {
		// Encoding is HARNESS work, never an engine outcome.
		t.Fatalf("row %q: encode parse args: %v", r.ID, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), constraintParseTimeout)
	defer cancel()
	result, err := rt.CallFunctionParse(ctx, r.FuncName(), encoded)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// A hang, not a verdict. Stock's parse path can block forever when the
			// Rust engine panics (see fault_integration_test.go); calling that an
			// outcome would let a row match on a crash.
			t.Fatalf("row %q: stock CallFunctionParse did not return within %s.\n"+
				"That is the signature of a Rust panic unwinding the tokio worker, not a classifiable outcome. "+
				"Prove it through a subprocess leg (as fault_integration_test.go does for BuildRequest) "+
				"before declaring any class for this row.", r.ID, constraintParseTimeout)
		}
		kind, ok := classifyStockConstraintError(err)
		if !ok {
			t.Fatalf("row %q: stock BAML failed with an error this harness cannot classify as a constraint outcome: %v\n"+
				"Refusing to guess: an unrecognized parse failure (a coercion failure, a runtime setup problem) "+
				"is not evidence that the CONSTRAINT behaved any particular way.", r.ID, err)
		}
		return ConstraintOutcome{Kind: kind, Detail: err.Error()}
	}
	// The row's own declaration selects the legal result shape, so an undecodable
	// Checked[T] is reported AS one instead of surfacing later as a generic check
	// mismatch against the profile leg.
	checks, err := stockChecksOf(result, r.DeclaresCheck())
	if err != nil {
		t.Fatalf("row %q: stock BAML parsed but its result shape is not one this harness understands: %v (result %T: %+v)",
			r.ID, err, result, result)
	}
	return ConstraintOutcome{Kind: ConstraintParsed, Checks: checks}
}

// stock error needles. They are matched, not compared: the differential's
// contract is the outcome CLASS, and these are only how the class is READ off
// stock's message.
//
// Both come straight from BAML v0.223's source and are the two distinct ways a
// constrained parse can fail:
//
//   - validate_asserts builds "Assertions failed." and carries the BAML comment
//     "IMPORTANT: DO NOT CHANGE THIS MESSAGE"
//     (jsonish/src/deserializer/coercer/field_type.rs:272-287);
//   - a predicate that could not be evaluated is wrapped as
//     "Failed to evaluate constraints: {e:?}" BEFORE validate_asserts is reached
//     (field_type.rs:191-199), which is why an evaluator error beats a failing
//     assert in the same batch.
//
// Anything else is deliberately NOT classified — see the caller.
const (
	stockAssertNeedle    = "Assertions failed"
	stockEvalErrorNeedle = "Failed to evaluate constraints"
)

func classifyStockConstraintError(err error) (ConstraintOutcomeKind, bool) {
	msg := err.Error()
	// The evaluator-error needle is checked FIRST. run_user_checks aborts before
	// validate_asserts runs, so the two can never both be genuine — but stock
	// nests the inner error's Debug rendering into the outer message, and an
	// evaluator error raised on a value that ALSO has failing asserts must still
	// read as an evaluator error.
	if strings.Contains(msg, stockEvalErrorNeedle) {
		return ConstraintEvalError, true
	}
	if strings.Contains(msg, stockAssertNeedle) {
		return ConstraintAssertFailed, true
	}
	return "", false
}

// --- the differential -------------------------------------------------------

// TestConstraintDifferential is the constraint proof: every corpus row parsed
// through stock BAML v0.223 lands in the same outcome class as the pure-Go leaf,
// with the same evaluated checks.
//
// Both halves of every row are asserted, in this order:
//
//  1. stock BAML really produces the row's DECLARED class (so a declaration
//     cannot rot into a fiction, exactly as TestProfileFaultDifferential does);
//  2. the profile produces the same class stock did — never a conservative parse
//     where stock failed, and never a decline where stock succeeded;
//  3. for a parsed row, the evaluated CHECK sets match exactly: label,
//     BARE expression, and pass state.
//
// (3) is stronger than it looks. Stock's ResponseCheck.expression is the bare
// `expression.0` BAML's parser stored, so comparing it to the profile's
// Constraint.Expression proves the corpus's bare spelling really is what BAML
// records — the round trip behind the "Expression is already bare" contract.
func TestConstraintDifferential(t *testing.T) {
	assertBAMLAuthority(t)
	ensureConstraintTypeMap()

	rows := ConstraintCorpus()
	env := envVars()
	rt, err := baml.CreateRuntime("./baml_src", GenerateConstraintBAMLSource(rows), env)
	if err != nil {
		t.Fatalf("CreateRuntime from the in-memory constraint corpus: %v\n"+
			"Every predicate must survive BAML's jinja PARSER (a parse error is a hard CreateRuntime error, "+
			"while a type error is only a warning), so a malformed expression takes the whole project down.", err)
	}

	for _, r := range rows {
		t.Run(r.ID, func(t *testing.T) {
			if r.Expect == ConstraintPanic {
				t.Fatalf("row declares Expect=%s, but this harness has no subprocess stock leg. "+
					"A panic hangs CallFunctionParse forever in-process; wire the child-process machinery from "+
					"fault_integration_test.go before declaring the panic class here.", r.Expect)
			}

			stock := stockConstraintOutcome(t, &rt, r, env)

			want := r.Expect
			if want == "" {
				want = ConstraintParsed
			}
			if stock.Kind != want {
				t.Fatalf("row declares stock BAML %s, but stock BAML actually produced %s.\n"+
					"The declaration is stale — fix the row, not this assertion.", want, stock)
			}

			prof := ConstraintOutcomeProfile(r)
			if prof.Kind != stock.Kind {
				t.Fatalf("outcome-class mismatch\nstock BAML: %s\nprofile:    %s\n"+
					"A row is green ONLY when the profile lands in stock's class; parsing where stock fails "+
					"(or declining where stock parses) is the parity-decline rule's out-do.", stock, prof)
			}
			if stock.Kind != ConstraintParsed {
				t.Logf("both legs %s — stock: %s | profile: %s", stock.Kind, stock.Detail, prof.Detail)
				return
			}
			if !equalChecks(prof.Checks, stock.Checks) {
				t.Errorf("check mismatch\nstock BAML: %#v\nprofile:    %#v", stock.Checks, prof.Checks)
			}
		})
	}
}

func equalChecks(a, b []CheckOutcome) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestConstraintDuplicateLabelCollapse is scope open question #2, measured rather
// than assumed.
//
// BAML evaluates duplicate-labelled checks in order and the CFFI carries them as
// an ordered list, but its Go client folds that list into a map keyed by label
// (serde/decode.go:297-303), so only one survives the response representation.
// This records WHICH, so Slice 7.2 can document a stable policy instead of
// inheriting an accident. It asserts the collapse is real (two declared checks,
// one reported) and that the profile's identically-collapsed view agrees; it
// deliberately does NOT assert a policy, because PR-3 does not own one.
func TestConstraintDuplicateLabelCollapse(t *testing.T) {
	assertBAMLAuthority(t)
	ensureConstraintTypeMap()

	const probeID = "levels_duplicate_label_probe"
	var probe ConstraintRow
	for _, r := range ConstraintCorpus() {
		if r.ID == probeID {
			probe = r
		}
	}
	if probe.ID == "" {
		t.Fatalf("the duplicate-label probe row %q is gone; scope open question #2 would go unmeasured", probeID)
	}
	declared := 0
	labels := map[string]int{}
	for _, c := range probe.Constraints {
		if c.IsCheck() && c.Label != nil {
			declared++
			labels[*c.Label]++
		}
	}
	if declared < 2 || len(labels) != 1 {
		t.Fatalf("the probe row no longer declares two checks under ONE label (%d checks, %d labels)", declared, len(labels))
	}

	env := envVars()
	rt, err := baml.CreateRuntime("./baml_src", GenerateConstraintBAMLSource(ConstraintCorpus()), env)
	if err != nil {
		t.Fatalf("CreateRuntime: %v", err)
	}
	stock := stockConstraintOutcome(t, &rt, probe, env)
	if stock.Kind != ConstraintParsed {
		t.Fatalf("the probe row did not parse on stock BAML: %s", stock)
	}
	if len(stock.Checks) != 1 {
		t.Fatalf("stock reported %d checks for %d duplicate-labelled declarations; "+
			"the collapse this probe exists to measure did not happen: %#v", len(stock.Checks), declared, stock.Checks)
	}
	prof := ConstraintOutcomeProfile(probe)
	if !equalChecks(prof.Checks, stock.Checks) {
		t.Errorf("duplicate-label collapse differs\nstock BAML: %#v\nprofile:    %#v", stock.Checks, prof.Checks)
	}
	t.Logf("MEASURED (scope open question #2): %d checks declared under label %q collapse to ONE in the response "+
		"representation, retaining {expression=%q passed=%v}. PR-3 still returns both, in declared order; "+
		"Slice 7.2 owns the documented policy.",
		declared, stock.Checks[0].Label, stock.Checks[0].Expression, stock.Checks[0].Passed)
}

// TestStockSkipsConstraintsOnBareStringReturn pins a MEASURED stock-BAML v0.223
// asymmetry that the corpus cannot express as an ordinary differential row,
// because the profile has no way to reproduce it and must not try.
//
// jsonish::from_str short-circuits a bare string target before any coercion runs:
//
//	if matches!(target, TypeIR::Primitive(TypeValue::String, _)) {
//	    return Ok(BamlValueWithFlags::String((raw_string.to_string(), target).into()));
//	}
//	// jsonish/src/lib.rs:233-237
//
// The match ignores the type's metadata, so `-> string @assert(...) @check(...)`
// never reaches TypeCoercer::coerce, never reaches run_user_checks, and its
// constraints are NEVER EVALUATED. This test proves both halves of that against
// the live runtime: a check reports no result at all, and an assert that is
// plainly false does not reject the parse.
//
// Why it matters beyond curiosity: at Slice 7.2 a lowerer that faithfully
// evaluates the constraints it was given would REJECT responses stock BAML
// accepts — an out-do in the most damaging direction, since it turns a working
// call into a parse failure. 7.2 must reproduce the skip for a bare `string`
// return (or decline the shape); PR-3 records the fact and the leaf stays
// deliberately unwired. Ledgered as PR-3 parity debt on #583.
//
// It is a live measurement, not a golden: if a future stock version starts
// evaluating these, this test fails and the 7.2 rule has to be revisited.
func TestStockSkipsConstraintsOnBareStringReturn(t *testing.T) {
	assertBAMLAuthority(t)
	ensureConstraintTypeMap()

	const (
		checkFn  = "CStr_check_is_dropped"
		assertFn = "CStr_false_assert_is_ignored"
	)
	files := map[string]string{
		"clients.baml": clientSource(),
		"types.baml":   typesBAMLSource(),
		"strskip.baml": "function " + checkFn + "() -> string @check(never_reported, {{ this|length > 2 }}) {\n" +
			"  client " + clientName + "\n  prompt #\"\n" + constraintPromptBody + "\n\"#\n}\n\n" +
			"function " + assertFn + "() -> string @assert({{ false }}) {\n" +
			"  client " + clientName + "\n  prompt #\"\n" + constraintPromptBody + "\n\"#\n}\n",
	}
	env := envVars()
	rt, err := baml.CreateRuntime("./baml_src", files, env)
	if err != nil {
		t.Fatalf("CreateRuntime: %v", err)
	}

	parse := func(fn string) (any, error) {
		args := baml.BamlFunctionArguments{Kwargs: map[string]any{"text": "hello", "stream": false}, Env: env}
		encoded, err := args.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), constraintParseTimeout)
		defer cancel()
		return rt.CallFunctionParse(ctx, fn, encoded)
	}

	result, err := parse(checkFn)
	if err != nil {
		t.Fatalf("%s: stock BAML failed to parse a bare string return: %v", checkFn, err)
	}
	// declaresCheck is TRUE: the function DOES carry a @check, so the CFFI still
	// wraps the value in a Checked[string] — it is the CHECK LIST inside that is
	// empty, because the predicate was never evaluated. Passing true keeps the
	// shape contract enforced here too, so a future stock that stopped wrapping
	// would fail loudly rather than read as "zero checks, as expected".
	checks, err := stockChecksOf(result, true)
	if err != nil {
		t.Fatalf("%s: unexpected result shape: %v.\n"+
			"Stock no longer returns a Checked[string] for a checked bare-string return; "+
			"the measured skip below must be re-derived from what it does now.", checkFn, err)
	}
	if len(checks) != 0 {
		t.Fatalf("%s: stock reported %d check(s) on a bare `string` return: %#v.\n"+
			"The jsonish::from_str string short-circuit no longer skips coercion — "+
			"re-derive the Slice 7.2 rule from this behavior instead of the old one.", checkFn, len(checks), checks)
	}

	if _, err := parse(assertFn); err != nil {
		t.Fatalf("%s: an @assert({{ false }}) on a bare `string` return REJECTED the parse (%v).\n"+
			"Stock v0.223 skips it entirely; if that changed, the Slice 7.2 rule must change with it.", assertFn, err)
	}

	t.Logf("MEASURED: stock BAML v0.223 evaluates NO constraints on a bare `string` return type " +
		"(jsonish/src/lib.rs:233-237 returns before coercion). A @check reports nothing and an " +
		"@assert({{ false }}) does not reject. Slice 7.2 must reproduce the skip rather than evaluate; " +
		"evaluating would fail calls stock accepts. Ledgered on #583.")
}

// --- the fixture ------------------------------------------------------------

// TestConstraintOracleFixture is the constraint corpus's version + source-map
// drift guard, a SIBLING of profile_oracle.json rather than extra fields in it:
// the two corpora generate two different .baml projects through two different
// runtime entry points, and one file per project keeps a prompt-corpus edit from
// invalidating the constraint record and vice versa.
func TestConstraintOracleFixture(t *testing.T) {
	// Guard the authority FIRST — before either branch — so a regen in a bad
	// environment (wrong CFFI runtime, replaced/mispinned module) fails fast
	// instead of baking bad values into the fixture.
	assertBAMLAuthority(t)

	observedModule, _, _ := bamlPinFromGoMod(t, rootGoModPath)
	files := GenerateConstraintBAMLSource(ConstraintCorpus())
	live := guardFixture{
		BAMLRuntimeVersion: baml_go.BamlVersion(),
		BAMLModuleVersion:  observedModule,
		SourceSHA256:       sourceSHA256(files),
		RowCount:           len(ConstraintCorpus()),
		Note:               "stock BAML v0.223.0 CallFunctionParse constraint differential for internal/bamlprofile (Slice 2 PR-3)",
	}

	if os.Getenv("WRITE_CONSTRAINT_ORACLE_FIXTURE") == "1" {
		if err := os.MkdirAll(filepath.Dir(constraintFixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := stdjson.MarshalIndent(live, "", "  ")
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		if err := os.WriteFile(constraintFixturePath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", constraintFixturePath)
		return
	}

	data, err := os.ReadFile(constraintFixturePath)
	if err != nil {
		t.Fatalf("read fixture %s: %v (regenerate with WRITE_CONSTRAINT_ORACLE_FIXTURE=1)", constraintFixturePath, err)
	}
	var want guardFixture
	if err := stdjson.Unmarshal(data, &want); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if live.BAMLRuntimeVersion != want.BAMLRuntimeVersion || live.BAMLModuleVersion != want.BAMLModuleVersion {
		t.Fatalf("version drift: live %+v, fixture %+v", live, want)
	}
	if live.SourceSHA256 != want.SourceSHA256 || live.RowCount != want.RowCount {
		t.Fatalf("constraint corpus source drift: live sha=%s rows=%d, fixture sha=%s rows=%d.\n"+
			"If the corpus change is intended, regenerate:\n"+
			"  WRITE_CONSTRAINT_ORACLE_FIXTURE=1 CGO_ENABLED=1 go test -tags integration ./internal/bamlprofile/profileoracle -run TestConstraintOracleFixture",
			live.SourceSHA256, live.RowCount, want.SourceSHA256, want.RowCount)
	}
}
