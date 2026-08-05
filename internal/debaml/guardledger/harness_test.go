//go:build integration

// Package guardledger is the de-BAML guard-removal ledger's PROOF HARNESS: the
// "rows" engine of scope §1.
//
// # What it establishes
//
// The native constraint evaluator (internal/debaml) carries a large fail-closed
// compensation layer that was written against the UPSTREAM pure-Go minijinja
// port and is now compiled against the BAML-exact fork. Fork capability is not
// removal authority: before any of those guards can be deleted, the outcome it
// currently produces has to be measured against REAL stock BAML v0.223.0.
//
// This package is that measurement. Each row is a generated .baml project method
// plus exact raw model JSON. It RECORDS THE STOCK OUTCOME ENVELOPE FIRST — pass,
// failed check, assertion error, evaluator error, no-checks, or process-fatal —
// and then asserts the native leg reproduces that envelope or declines with
// ErrConstraintUnsupported. A row never uses a fork unit test as the expected
// result, and never fabricates a boolean for a row stock cannot answer.
//
// The ledger those rows feed is internal/debaml/guard_ledger.md, rendered from
// internal/debaml/testdata/guard_ledger/ledger.json and cross-checked against
// this corpus by TestGuardLedgerCoversEveryLedgerRecord.
//
// # What is NOT under test
//
// The evaluator's admission path is unchanged and unreachable from production:
// every constraint-bearing bundle still declines at checkSupported (pinned by
// internal/debaml.TestNativeDeclines_Constraints and boundary_decline_test.go).
// A guard removal proven here widens only the internal evaluator's ANSWER
// surface, which production does not reach. That is a COVERAGE change, not a
// parity claim, and nothing here wires it to serving.
//
// # Why a separate package and a separate project
//
// Same reason as internal/debaml/constraintoracle: this test binary links the
// STOCK upstream github.com/boundaryml/baml v0.223.0 through a generated client,
// while other de-BAML tests link the PATCHED dynclient fork. It carries its own
// baml project (internal/debaml/testdata/guard_ledger) so the #649 corpus's
// generated client stays byte-untouched by this slice. All files are behind
// //go:build integration; nothing here is reachable from a production build, and
// the testdata project is excluded from the customer/container embed via
// .embedignore.
//
// # Regenerating
//
//	GUARD_LEDGER_FIXTURE_WRITE=1 go test -tags integration \
//	  ./internal/debaml/guardledger -run TestGuardLedgerFixtureDrift
//	cd internal/debaml/testdata/guard_ledger && \
//	  npx --offline @boundaryml/baml@0.223.0 generate && \
//	  goimports -w baml_client && gofmt -w baml_client
//
// # Recording
//
// GUARD_LEDGER_RECORD=1 prints the LIVE stock envelope of every instance instead
// of asserting the pin, which is how the pins in corpus_test.go were produced.
//
// # Running
//
//	CGO_ENABLED=1 go test -tags integration ./internal/debaml/guardledger
//
// Requires CGO and the stock BAML v0.223.0 CFFI library (auto-located under the
// user BAML cache dir), exactly like internal/debaml/constraintoracle.
package guardledger

import (
	stdjson "encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	baml_go "github.com/boundaryml/baml/engine/language_client_go/baml_go"
	"github.com/boundaryml/baml/engine/language_client_go/baml_go/shared"
	mj "github.com/invakid404/minijinja-go/v2"
	"golang.org/x/mod/modfile"

	"github.com/invakid404/baml-rest/internal/debaml"
	bamlclient "github.com/invakid404/baml-rest/internal/debaml/testdata/guard_ledger/baml_client"
)

const (
	// fixtureBAMLPath is the checked-in .baml the generated client was produced
	// from; renderFixtureBAML must reproduce it byte for byte.
	fixtureBAMLPath = "../testdata/guard_ledger/baml_src/rows.baml"
	// writeFixtureEnv rewrites the fixture instead of comparing.
	writeFixtureEnv = "GUARD_LEDGER_FIXTURE_WRITE"
	// recordEnv prints the live stock envelopes instead of asserting the pins.
	recordEnv = "GUARD_LEDGER_RECORD"
	// fixtureSrcDir holds every .baml the stock client was generated from.
	fixtureSrcDir = "../testdata/guard_ledger/baml_src"
	// sourceMapPath is the generated client's verbatim copy of those sources.
	sourceMapPath = "../testdata/guard_ledger/baml_client/baml_source_map.go"
	// ledgerJSONPath is the structured guard ledger internal/debaml renders its
	// markdown from; this harness reads it to prove the cited evidence exists.
	ledgerJSONPath = "../testdata/guard_ledger/ledger.json"

	// rootGoModPath is the root module's go.mod, from this package's CWD.
	rootGoModPath = "../../../go.mod"
	// bamlModulePath / wantBAMLModuleVersion / wantBAMLRuntimeVersion pin the
	// stock oracle dependency; the CFFI reports the bare semver.
	bamlModulePath         = "github.com/boundaryml/baml"
	wantBAMLModuleVersion  = "v0.223.0"
	wantBAMLRuntimeVersion = "0.223.0"
)

// ---------------------------------------------------------------------------
// Protection 1: the oracle really is stock BAML v0.223.0.
// ---------------------------------------------------------------------------

// TestGuardLedgerBAMLVersionPinned requires the LOADED CFFI runtime and the root
// go.mod pin to be exactly v0.223.0, so a recorded envelope can never be
// attributed to a different BAML. The runtime check is the load-bearing one — it
// reads the native library that actually parsed every row in this binary.
func TestGuardLedgerBAMLVersionPinned(t *testing.T) {
	if got := baml_go.BamlVersion(); got != wantBAMLRuntimeVersion {
		t.Fatalf("loaded BAML CFFI runtime reports version %q, want exactly %q", got, wantBAMLRuntimeVersion)
	}
	// THE EFFECTIVE RESOLUTION comes first, because it is the only check that
	// cannot be routed around. A manifest scan answers "what does this file
	// say"; `go list -m` answers "what will the toolchain actually link", after
	// go.mod, the active go.work and any GOFLAGS have all had their say. A
	// workspace replacement is invisible to the former and decisive for the
	// latter — and it leaves the version string reading v0.223.0 while the code
	// behind it is something else entirely.
	res, err := resolveStockModule(".", "")
	if err != nil {
		t.Fatalf("resolve the effective %s module: %v\n"+
			"The pin cannot be verified, so the recorded envelopes cannot be attributed to stock BAML.", bamlModulePath, err)
	}
	if res.Version != wantBAMLModuleVersion {
		t.Fatalf("the toolchain resolves %s to %s, want exactly %s", bamlModulePath, res.Version, wantBAMLModuleVersion)
	}
	if res.ReplacedBy != "" {
		t.Fatalf("the toolchain resolves %s through a REPLACEMENT (%s); the harness must link the stock module, "+
			"and a same-version fork would satisfy both the CFFI version string and the manifest checks below "+
			"while invalidating every recorded envelope", bamlModulePath, res.ReplacedBy)
	}

	// The manifests are then checked directly, so a red run names the FILE that
	// has to change rather than only the effect.
	assertNoBAMLReplacement(t, rootGoModPath, bamlReplacementsInGoMod)
	if work := activeWorkspaceFile(t); work != "" {
		assertNoBAMLReplacement(t, work, bamlReplacementsInGoWork)
	}

	raw, err := os.ReadFile(rootGoModPath)
	if err != nil {
		t.Fatalf("read %s: %v", rootGoModPath, err)
	}
	f, err := modfile.Parse(rootGoModPath, raw, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", rootGoModPath, err)
	}
	required := false
	for _, r := range f.Require {
		if r.Mod.Path != bamlModulePath {
			continue
		}
		required = true
		if r.Mod.Version != wantBAMLModuleVersion {
			t.Fatalf("root go.mod requires %s %s, want exactly %s", bamlModulePath, r.Mod.Version, wantBAMLModuleVersion)
		}
	}
	if !required {
		t.Fatalf("root go.mod does not require %s %s", bamlModulePath, wantBAMLModuleVersion)
	}

	// THE GENERATOR'S OWN PIN. The checked-in client was produced by the BAML
	// CLI at whatever version generators.baml declares, and nothing above reads
	// that file — so a bump of go.mod and the CFFI that left this string behind
	// (or the reverse) would stay green while the client and the runtime came
	// from different BAMLs.
	assertGeneratorVersionPinned(t)
}

// generatorVersionPattern matches the `version "X.Y.Z"` line of a BAML generator
// block.
var generatorVersionPattern = regexp.MustCompile(`(?m)^\s*version\s+"([^"]+)"`)

// assertGeneratorVersionPinned requires baml_src/generators.baml to declare the
// same version the loaded CFFI reports.
func assertGeneratorVersionPinned(t *testing.T) {
	t.Helper()
	path := filepath.Join(fixtureSrcDir, "generators.baml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := generatorVersionPattern.FindAllStringSubmatch(string(raw), -1)
	if len(m) == 0 {
		t.Fatalf("%s declares no generator version; the client's provenance is then unpinned", path)
	}
	for _, got := range m {
		if got[1] != wantBAMLRuntimeVersion {
			t.Fatalf("%s generates with BAML %s, but this binary links the %s runtime; the checked-in client "+
				"and the CFFI that drives it would come from different BAMLs", path, got[1], wantBAMLRuntimeVersion)
		}
	}
}

// stockModuleFacts is what the toolchain ACTUALLY resolves the stock BAML module
// to, as opposed to what any one manifest asks for.
type stockModuleFacts struct {
	Version string
	// ReplacedBy is empty when the module is unreplaced, and otherwise names the
	// path (and version, for a module replacement) it resolves through.
	ReplacedBy string
}

// resolveStockModule asks the go command what [bamlModulePath] resolves to from
// dir, optionally under an explicit GOWORK.
//
// It shells out on purpose: reimplementing module resolution would reproduce
// exactly the blind spot this check exists to close, and the go command is the
// same one that built this test binary.
func resolveStockModule(dir, gowork string) (stockModuleFacts, error) {
	cmd := exec.Command("go", "list", "-m", "-f",
		"{{.Version}}\t{{with .Replace}}{{.Path}}@{{.Version}}{{end}}", bamlModulePath)
	cmd.Dir = dir
	if gowork != "" {
		cmd.Env = append(os.Environ(), "GOWORK="+gowork)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return stockModuleFacts{}, fmt.Errorf("go list -m %s: %w (%s)", bamlModulePath, err, strings.TrimSpace(stderr.String()))
	}
	// Trim only the line terminator: an UNREPLACED module leaves the field after
	// the tab empty, and a general whitespace trim would eat the separator with
	// it and make the line unreadable.
	version, replaced, found := strings.Cut(strings.TrimRight(string(out), "\r\n"), "\t")
	if !found {
		return stockModuleFacts{}, fmt.Errorf("go list -m %s produced an unreadable line %q", bamlModulePath, string(out))
	}
	// A DIRECTORY replacement carries no version, so the raw form is `path@`;
	// normalise that to the bare path rather than reporting a stray separator.
	return stockModuleFacts{Version: version, ReplacedBy: strings.TrimSuffix(replaced, "@")}, nil
}

// activeWorkspaceFile is the go.work in force for this build, or "" when the
// build is not in a workspace.
func activeWorkspaceFile(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOWORK").Output()
	if err != nil {
		t.Fatalf("go env GOWORK: %v", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" || path == "off" {
		return ""
	}
	return path
}

// bamlReplacementsInGoMod / bamlReplacementsInGoWork report every replace
// directive in a manifest that names the stock module on EITHER side.
//
// Both positions matter and both are parsed rather than pattern-matched, so the
// block form is caught as readily as the single-line one. The module as the
// SOURCE (`replace github.com/boundaryml/baml => ...`) swaps the stock runtime
// out while leaving every version string intact; the module as the TARGET makes
// some other path resolve to it.
func bamlReplacementsInGoMod(path string, content []byte) ([]string, error) {
	f, err := modfile.Parse(path, content, nil)
	if err != nil {
		return nil, err
	}
	return describeBAMLReplacements(f.Replace), nil
}

func bamlReplacementsInGoWork(path string, content []byte) ([]string, error) {
	f, err := modfile.ParseWork(path, content, nil)
	if err != nil {
		return nil, err
	}
	return describeBAMLReplacements(f.Replace), nil
}

func describeBAMLReplacements(replaces []*modfile.Replace) []string {
	var out []string
	for _, r := range replaces {
		if r.Old.Path != bamlModulePath && r.New.Path != bamlModulePath {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s => %s %s", r.Old.Path, r.Old.Version, r.New.Path, r.New.Version))
	}
	return out
}

// assertNoBAMLReplacement fails when a manifest replaces the stock module.
func assertNoBAMLReplacement(t *testing.T, path string, scan func(string, []byte) ([]string, error)) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	found, err := scan(path, content)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(found) > 0 {
		t.Fatalf("%s replaces %s (%s); the harness must link the stock module, and a same-version fork would "+
			"satisfy the CFFI version string and the require pin while invalidating every recorded envelope",
			path, bamlModulePath, strings.Join(found, "; "))
	}
}

// ---------------------------------------------------------------------------
// Protection 2: the corpus, the .baml and the generated client describe the
// same project.
// ---------------------------------------------------------------------------

// TestGuardLedgerFixtureDrift pins baml_src/rows.baml as a pure function of the
// corpus. Without it a corpus edit would silently stop matching the generated
// client and the harness would quietly measure the wrong expressions.
func TestGuardLedgerFixtureDrift(t *testing.T) {
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

// TestGuardLedgerGeneratedClientIsFresh proves the checked-in stock client was
// generated from the checked-in .baml sources, and not from an older revision of
// them.
//
// TestGuardLedgerFixtureDrift only relates the corpus to baml_src; on its own, a
// row could keep its label and its pinned envelope while the client the stock leg
// actually drives still carries the OLD expression. The generated client embeds
// every source file verbatim in baml_source_map.go's file_map, so comparing that
// map against the files on disk closes the loop: corpus -> source -> client.
func TestGuardLedgerGeneratedClientIsFresh(t *testing.T) {
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
			// The generator emits string literals on both sides. Asserting that
			// rather than type-asserting blind means a generator that changes
			// shape — a concatenation, a constant reference — fails with a
			// message naming the file and the entry, instead of panicking with a
			// bare interface-conversion error that says nothing about which
			// artefact drifted.
			keyLit, keyOK := kv.Key.(*ast.BasicLit)
			valLit, valOK := kv.Value.(*ast.BasicLit)
			if !keyOK || !valOK {
				t.Fatalf("%s: a file_map entry is not a pair of string literals (%T: %T); the stock generator's "+
					"source-map shape changed and this reader must be updated before the freshness check means anything",
					sourceMapPath, kv.Key, kv.Value)
			}
			key, kerr := strconv.Unquote(keyLit.Value)
			val, verr := strconv.Unquote(valLit.Value)
			if kerr != nil || verr != nil {
				t.Fatalf("unquote %s entry: %v / %v", sourceMapPath, kerr, verr)
			}
			// A duplicate filename would silently overwrite, and the freshness
			// check would then compare disk against whichever copy came last —
			// so a stale embedded source could pass by being listed twice.
			if _, seen := out[key]; seen {
				t.Fatalf("%s embeds %q more than once; the freshness check cannot say which copy the client "+
					"was generated from", sourceMapPath, key)
			}
			out[key] = val
		}
		return false
	})
	return out
}

// ---------------------------------------------------------------------------
// Protection 3: the corpus itself is well formed.
// ---------------------------------------------------------------------------

// TestGuardLedgerRowsAreWellFormed enforces the invariants the rest of the
// harness relies on, so a malformed row fails here with a specific message
// rather than as a confusing differential result.
func TestGuardLedgerRowsAreWellFormed(t *testing.T) {
	if len(guardRows) == 0 {
		t.Fatal("the corpus is empty; every per-row check in this package would pass over nothing")
	}
	seen := map[string]bool{}
	for _, r := range guardRows {
		if seen[r.ID] {
			t.Errorf("duplicate row id %q", r.ID)
		}
		seen[r.ID] = true
		if len(r.Guards) == 0 {
			t.Errorf("row %q names no ledger guard", r.ID)
		}
		if _, ok := groupByName(r.Group); !ok {
			t.Errorf("row %q references unknown group %q", r.ID, r.Group)
		}
		if r.StockCheck == "" {
			t.Errorf("row %q has no recorded @check envelope", r.ID)
		}
		if (r.StockAssert == "") == (r.AssertOmitted == "") {
			t.Errorf("row %q must carry EITHER a recorded @assert envelope OR a reason it has none (assert=%q omitted=%q)",
				r.ID, r.StockAssert, r.AssertOmitted)
		}
		// An OMITTED assert leg is admitted only where stock genuinely cannot be
		// observed at that level. There is exactly one such shape: an OPTIONAL
		// field whose predicate errors, where the optional coercion swallows the
		// failure and the node becomes null — the same observation a PASSING
		// assert produces, so the two are indistinguishable. Everywhere else the
		// assert leg is a real observation and must be recorded.
		if r.AssertOmitted != "" && r.StockCheck != envNoChecks && r.StockCheck != envSourceRejected {
			t.Errorf("row %q omits its @assert leg but stock IS observable there (check envelope %s); "+
				"record the assert envelope instead of explaining it away", r.ID, r.StockCheck)
		}
		// The INNER stock error is required exactly where the envelope is an
		// evaluator error, and forbidden elsewhere — an envelope that carries no
		// engine message has nothing to pin.
		wantsInner := r.StockCheck == envEvaluatorError || r.StockAssert == envEvaluatorError
		if wantsInner && r.StockInner == "" {
			t.Errorf("row %q records an evaluator error but pins no inner error class; "+
				"\"evaluator-error\" alone cannot tell an unknown-name failure from a type or arity one", r.ID)
		}
		if !wantsInner && r.StockInner != "" {
			t.Errorf("row %q pins an inner error class but its envelopes are check=%s assert=%s",
				r.ID, r.StockCheck, r.StockAssert)
		}
		if r.NativeGuard != "" && !knownGuardAttribution(r.NativeGuard) {
			t.Errorf("row %q pins NativeGuard %q, which attributeNativeGuard can never produce", r.ID, r.NativeGuard)
		}

		// A Note is required exactly where the profile COSTS something: native
		// refuses an expression stock DECIDED. Where stock also refused, the two
		// agree in substance and a Note would be noise; where native answered
		// there is nothing to explain. A Note anywhere else is stale.
		costly := r.NativeGuard != "" && stockDecided(r.StockCheck)
		if costly && r.Note == "" {
			t.Errorf("row %q: native refuses an expression stock decided (stock=%s) but carries no Note naming the cost",
				r.ID, r.StockCheck)
		}
		if !costly && r.Note != "" {
			t.Errorf("row %q carries a Note but is not a profile cost (stock=%s nativeGuard=%q)",
				r.ID, r.StockCheck, r.NativeGuard)
		}

		// SOURCE BYTES. BAML's @check/@assert attribute lexer doubles every
		// backslash, so Retained is a pure function of Expr — and asserting THAT
		// (rather than accepting whatever a row author typed) is what makes the
		// backslash rows a real observation instead of a hand-copied string.
		wantRetained := ""
		if strings.Contains(r.Expr, `\`) {
			wantRetained = strings.ReplaceAll(r.Expr, `\`, `\\`)
		}
		if r.Retained != wantRetained {
			t.Errorf("row %q: Retained must be Expr with every backslash doubled\n  expr     %q\n  retained %q\n  want     %q",
				r.ID, r.Expr, r.Retained, wantRetained)
		}
	}
}

// stockDecided reports whether stock produced a boolean-shaped outcome, i.e.
// whether a native refusal costs coverage.
func stockDecided(e envelope) bool {
	switch e {
	case envPass, envFailedCheck, envAssertError:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// The stock leg.
// ---------------------------------------------------------------------------

// stockObservation is one stock parse of one fixture function, kept as the RAW
// observation so classification is a separate, inspectable step.
type stockObservation struct {
	method string
	err    error
	// checked reports whether the returned node carried a Checked[T] wrapper. A
	// class with only @assert attributes does not get one, so its absence is
	// information rather than an error.
	checked   bool
	checks    map[string]shared.Check
	valueJSON string
}

// stockCache memoizes one parse per fixture method: a batch is driven once and
// read by every instance in it. No mutex — every subtest here is sequential (the
// CFFI runtime is process-global and the suite deliberately does not
// parallelise over it).
var stockCache = map[string]stockObservation{}

// stockCacheInput records the assistant text each cached method was driven with,
// so a second call with DIFFERENT text cannot silently read the first call's
// result. Every method here belongs to exactly one group today; this makes that
// an enforced property rather than a convention.
var stockCacheInput = map[string]string{}

// driveStock drives Parse.<method>(text) on the generated client by reflection.
//
// Reflection rather than a generated dispatch table because every fixture
// function returns a DIFFERENT class type (and a differently-instantiated
// Checked[T] inside it), which Go generics cannot abstract over — while the
// shape this harness needs (field V, then Value and the non-generic
// map[string]shared.Check when present) is uniform across all of them.
func driveStock(t *testing.T, method, text string) stockObservation {
	t.Helper()
	if r, ok := stockCache[method]; ok {
		if stockCacheInput[method] != text {
			t.Fatalf("Parse.%s was driven with two different inputs (%q then %q); the cached observation "+
				"would be attributed to the wrong one", method, stockCacheInput[method], text)
		}
		return r
	}
	stockCacheInput[method] = text
	fn := reflect.ValueOf(bamlclient.Parse).MethodByName(method)
	if !fn.IsValid() {
		t.Fatalf("generated client has no Parse.%s; the client is stale relative to the corpus", method)
	}
	out := fn.Call([]reflect.Value{reflect.ValueOf(text)})
	if errAny := out[1].Interface(); errAny != nil {
		r := stockObservation{method: method, err: errAny.(error)}
		stockCache[method] = r
		return r
	}
	field := out[0].FieldByName("V")
	if !field.IsValid() {
		t.Fatalf("Parse.%s returned no V field", method)
	}
	r := stockObservation{method: method}
	if field.Kind() == reflect.Struct && field.FieldByName("Checks").IsValid() {
		r.checked = true
		checks, ok := field.FieldByName("Checks").Interface().(map[string]shared.Check)
		if !ok {
			t.Fatalf("Parse.%s returned a Checked[T] whose Checks field is %T, not map[string]shared.Check; "+
				"the generated client's shape changed", method, field.FieldByName("Checks").Interface())
		}
		r.checks = checks
		field = field.FieldByName("Value")
	}
	valueJSON, err := stdjson.Marshal(field.Interface())
	if err != nil {
		t.Fatalf("marshal Parse.%s value: %v", method, err)
	}
	r.valueJSON = string(valueJSON)
	stockCache[method] = r
	return r
}

// BAML's two rejection shapes, quoted from the coercion error it raises. They
// are the whole reason an assertion failure and an evaluator failure are
// DIFFERENT envelopes rather than one "error" bucket.
const (
	stockAssertMarker    = "Assertions failed."
	stockEvaluatorMarker = "Failed to evaluate constraints: "
	// stockLocationMarker ends the engine's own message and begins the source
	// location BAML appends to it.
	stockLocationMarker = " (in <string>:"
)

// debugEscape applies Rust's string-Debug escaping, which is how BAML's
// coercion error carries the reason it quotes. Comparing against the escaped
// form is what lets a predicate containing quotes or backslashes be matched at
// all — and it is a second, independent check on the source bytes, since a row
// whose backslash was not doubled would not match here either.
func debugEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// stockInnerError recovers the ENGINE's own message out of BAML's coercion
// error: everything after "Failed to evaluate constraints: " and before the
// source location it appends.
//
// This is what keeps envEvaluatorError from being a lossy bucket. The outer
// marker only says "the predicate did not produce a boolean"; the inner text
// says WHY, and the difference between `unknown filter: filter urlencode is
// unknown` and `invalid operation: cannot calculate length of value of type
// number` is exactly the error-class discrimination a witness row exists to pin.
// A row whose inner text changed would still be an evaluator error, and would
// still be a different observation.
//
// It reports ok=false rather than guessing when the marker is absent, and the
// caller fails the row.
func stockInnerError(text string) (string, bool) {
	i := strings.Index(text, stockEvaluatorMarker)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(stockEvaluatorMarker):]
	end := len(rest)
	if j := strings.Index(rest, stockLocationMarker); j >= 0 && j < end {
		end = j
	}
	// The error arrives Rust-Debug-formatted, so the reason it sits in is a
	// QUOTED string whose own quotes are backslash-escaped. Only an UNESCAPED
	// quote ends the message.
	//
	// "Preceded by a backslash" is NOT the test, because backslashes escape each
	// other: in `…\\"` the two backslashes are one escaped backslash and the
	// quote that follows is real. Escaping is decided by the PARITY of the run —
	// odd means the quote is escaped, even means it closes the string — and
	// getting that wrong reads straight past the message boundary and folds
	// BAML's surrounding Debug scaffolding into the inner error this row pins.
	for j := 0; j < end; j++ {
		if rest[j] != '"' {
			continue
		}
		backslashes := 0
		for k := j - 1; k >= 0 && rest[k] == '\\'; k-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			end = j
			break
		}
	}
	inner := strings.TrimSpace(rest[:end])
	if inner == "" {
		return "", false
	}
	return inner, true
}

// classifyStock maps one raw observation onto an envelope for one instance.
//
// It has NO default arm. An error whose shape BAML has never produced here, or a
// check status that is neither "succeeded" nor "failed", returns ok=false and the
// caller fails the row with the raw text — a silently bucketed observation would
// turn an unknown stock behaviour into a green result.
func classifyStock(obs stockObservation, lv level, label string) (envelope, string, shared.Check, bool) {
	if obs.err != nil {
		text := obs.err.Error()
		switch {
		case strings.Contains(text, stockAssertMarker):
			// An assertion failure carries BAML's own "Failed: <label> <expr>"
			// rather than an engine message; the differential asserts that shape
			// directly rather than pinning it per row, since it is derivable.
			return envAssertError, "", shared.Check{}, true
		case strings.Contains(text, stockEvaluatorMarker):
			inner, ok := stockInnerError(text)
			if !ok {
				return "", "", shared.Check{}, false
			}
			return envEvaluatorError, inner, shared.Check{}, true
		default:
			return "", "", shared.Check{}, false
		}
	}
	if lv == levelAssert {
		// The node was emitted, so every @assert on it held. BAML records no
		// entry for a passing assert, which is why this is not a map lookup.
		return envPass, "", shared.Check{}, true
	}
	if !obs.checked {
		// A @check instance whose class produced no Checked[T] wrapper is a
		// harness bug (the fixture would have had to omit the attribute), not a
		// stock behaviour to record.
		return "", "", shared.Check{}, false
	}
	check, ok := obs.checks[label]
	if !ok {
		return envNoChecks, "", shared.Check{}, true
	}
	switch check.Status {
	case "succeeded":
		return envPass, "", check, true
	case "failed":
		return envFailedCheck, "", check, true
	default:
		return "", "", check, false
	}
}

// ---------------------------------------------------------------------------
// The native leg, and WHICH guard refused.
// ---------------------------------------------------------------------------

// guardAttributions maps a native refusal's message onto the guard that produced
// it. The order is significant: a specific marker must be matched before the
// generic engine wrappers that enclose it.
//
// Attribution is what makes a removal proof discriminating rather than a
// truthiness check. A guard may be removed only when its rows already attribute
// their refusal to a DIFFERENT guard that stays — so the pin here is the
// evidence, and it flips loudly if the surviving guard is ever narrowed.
var guardAttributions = []struct {
	marker string
	guard  string
}{
	{"media values are outside the profile", "hasMedia"},
	{"`divisibleby(0)`", "divisibleByZero"},
	{"`divisibleby` over a non-integral subject", "divisibleByNonIntegral"},
	{"`divisibleby` with a non-integral divisor", "divisibleByNonIntegral"},
	{"outside the proven numeric profile", "exceedsExactIntegerRange"},
	{"outside the proven operator profile", "operatorShapeIsProven"},
	{"result depends on how the mapping is represented", "mappingDualRender"},
	{"length of a value with no length", "lengthGuard"},
	{"`last` over a mapping", "lastMappingGuard"},
	{"`items` over a mapping", "itemsTojsonMappingGuards"},
	{"`tojson` over a value containing a mapping", "itemsTojsonMappingGuards"},
	{"returns a lazy iterator", "splitWithdrawal"},
	{"over a mapping literal", "guardForeignMapping"},
	{"`range` is outside the profile", "rangeWithdrawal"},
	{"is outside the profile: a global callable", "globalWithdrawals"},
	{"is withdrawn from the profile", "checkCallParity/withdrawn"},
	{"has no proven-identical signature", "checkCallParity/unlisted"},
	{"was given keyword arguments", "checkCallParity/kwargs"},
	{"arguments; stock accepts", "checkCallParity/arity"},
	{"is not a proven-identical conversion for it", "checkCallParity/subject-kind"},
	{"so the shape is not proven identical", "checkCallParity/arg-kind"},
	{"non-integer numeric argument", "checkCallParity/arg-integrality"},
	{"unreadable numeric argument", "checkCallParity/arg-unreadable"},
	{"was given a count of", "checkCallParity/count-defaulting"},
	{"outside the range where the two engines convert alike", "containsAsIntHazard"},
	{"produced an integer at or past 2^53", "containsInexactInteger"},
	{"unknown filter", "engine/unknown-name"},
	{"unknown test", "engine/unknown-name"},
	{"unknown function", "engine/unknown-name"},
	{"unknown method", "engine/unknown-name"},
	{"predicate did not evaluate to a boolean", "engine/non-boolean"},
	{"compile constraint expression", "engine/compile-error"},
	{"evaluate constraint expression", "engine/eval-error"},
}

// attributeNativeGuard names the guard a refusal came from, or "" when the
// message matches nothing — which callers treat as a failure, never as a pass.
func attributeNativeGuard(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	for _, a := range guardAttributions {
		if strings.Contains(text, a.marker) {
			return a.guard
		}
	}
	return ""
}

func knownGuardAttribution(name string) bool {
	for _, a := range guardAttributions {
		if a.guard == name {
			return true
		}
	}
	return false
}

// nativeLeg evaluates one instance natively and reports the envelope plus the
// guard any refusal is attributed to. The error is carried out rather than
// swallowed, so a caller can print it.
func nativeLeg(this debaml.ConstraintValue, expr string, lv level) (envelope, string, error) {
	ok, err := debaml.EvaluateConstraint(this, expr)
	if err != nil {
		return envUnsupported, attributeNativeGuard(err), err
	}
	if ok {
		return envPass, "", nil
	}
	if lv == levelAssert {
		return envAssertError, "", nil
	}
	return envFailedCheck, "", nil
}

// ---------------------------------------------------------------------------
// The differential.
// ---------------------------------------------------------------------------

// TestGuardLedgerBatchesParse fails LOUDLY and specifically when a batch class
// does not parse, which is how a mis-pinned row inside it would otherwise
// present: as a wall of unrelated envelope mismatches.
func TestGuardLedgerBatchesParse(t *testing.T) {
	seen := map[string]bool{}
	for _, i := range rowInstances() {
		if !i.batched() || i.excludedFromFixture() || seen[i.method()] {
			continue
		}
		seen[i.method()] = true
		g, ok := groupByName(i.Row.Group)
		if !ok {
			t.Fatalf("row %q references unknown group %q", i.Row.ID, i.Row.Group)
		}
		if obs := driveStock(t, i.method(), g.Input); obs.err != nil {
			t.Errorf("batch %s did not parse, so every row in it is unobservable — one of its members is pinned "+
				"as pass/failed-check but stock rejects the node:\n  input %s\n  err   %v",
				i.method(), g.Input, obs.err)
		}
	}
}

// TestGuardLedgerDifferential is the proof, one subtest per row INSTANCE.
//
// For each it drives the expression through stock BAML v0.223.0, records the
// envelope, and requires BOTH legs to produce exactly what the corpus pinned —
// including WHICH guard refused, and the exact JinjaExpression stock retained.
// Nothing is asserted in aggregate: every failure names the .baml method, the
// raw input, the source bytes, both envelopes and the mismatch kind.
func TestGuardLedgerDifferential(t *testing.T) {
	recording := os.Getenv(recordEnv) != ""
	instances := rowInstances()
	if len(instances) == 0 {
		t.Fatal("no row instances to drive; the differential would report success having compared nothing")
	}
	for _, inst := range instances {
		name := inst.Row.ID + "/" + string(inst.Level)
		t.Run(name, func(t *testing.T) {
			g, ok := groupByName(inst.Row.Group)
			if !ok {
				t.Fatalf("unknown group %q", inst.Row.Group)
			}
			if inst.excludedFromFixture() {
				// No generated method exists: BAML refuses to compile the source
				// spelling, which IS the observation. Proved by
				// TestGuardLedgerRejectedSourceSpellings; here the native leg is
				// still required to decline.
				if _, _, err := nativeLeg(g.This, inst.Row.retainedExpr(), inst.Level); !errors.Is(err, debaml.ErrConstraintUnsupported) {
					t.Errorf("%s: BAML will not compile this source spelling, so native must decline it too; got %v",
						inst.Row.ID, err)
				}
				return
			}
			obs := driveStock(t, inst.method(), g.Input)
			gotStock, gotInner, check, classified := classifyStock(obs, inst.Level, inst.label())

			gotNative, gotGuard, nativeErr := nativeLeg(g.This, inst.Row.retainedExpr(), inst.Level)

			if recording {
				t.Logf("RECORD %s level=%s stock=%s(classified=%v) inner=%q native=%s guard=%q stockErr=%v",
					inst.Row.ID, inst.Level, gotStock, classified, gotInner, gotNative, gotGuard, obs.err)
				return
			}

			report := func(kind, format string, args ...any) {
				t.Errorf("%s\n  mismatch   %s\n  baml       %s :: Parse.%s\n  input      %s\n  level      %s\n  label      %s\n"+
					"  expr       %q\n  retained   %q\n  stock      %s (pinned %s)\n  native     %s (guard %q, pinned guard %q)\n  nativeErr  %v\n  stockErr   %v",
					fmt.Sprintf(format, args...), kind, fixtureBAMLPath, inst.method(), g.Input, inst.Level, inst.label(),
					inst.Row.Expr, inst.Row.retainedExpr(), gotStock, inst.Stock, gotNative, gotGuard, inst.Row.NativeGuard,
					nativeErr, obs.err)
			}

			if !classified {
				report("envelope", "stock produced an observation this harness cannot classify; it must never be bucketed silently")
				return
			}
			if gotStock != inst.Stock {
				report("envelope", "stock envelope changed")
			}
			// THE INNER ERROR CLASS. "evaluator-error" alone would let an
			// unknown-name witness quietly become a type or arity error and stay
			// green; the engine's own message is what distinguishes them.
			if gotStock == envEvaluatorError && gotInner != inst.Row.StockInner {
				report("error-class", "stock raised a DIFFERENT error: got %q, pinned %q", gotInner, inst.Row.StockInner)
			}
			// An ASSERTION failure must name this instance's label and the exact
			// bytes stock evaluated. That is also a second, independent check on
			// the source-byte handling: a backslash row's doubled spelling has to
			// come back verbatim here.
			if gotStock == envAssertError {
				want := debugEscape("Failed: " + inst.label() + " " + inst.Row.retainedExpr())
				if !strings.Contains(obs.err.Error(), want) {
					report("event", "the assertion failure does not name this instance and its source bytes: want %q", want)
				}
			}
			if gotNative != wantNativeEnvelope(inst) {
				report("envelope", "native envelope changed")
			}
			if gotGuard != inst.Row.NativeGuard {
				report("guard", "the guard that refused changed")
			}
			if gotNative == envUnsupported {
				if !errors.Is(nativeErr, debaml.ErrConstraintUnsupported) {
					report("envelope", "native refused with an error that does not wrap ErrConstraintUnsupported")
				}
				if gotGuard == "" {
					report("guard", "native refused with a message no guard attribution matches")
				}
			}
			// The SOURCE BYTES. What stock evaluated must be exactly what the
			// native leg was fed; where the two differ, BAML's attribute lexer
			// doubled a backslash and the row pins the doubled form.
			if check.Expression != "" && check.Expression != inst.Row.retainedExpr() {
				report("event", "stock retained a different JinjaExpression: got %q", check.Expression)
			}
		})
	}
}

// wantNativeEnvelope derives the expected native envelope from the pins: a row
// that names a guard refused, and a row that names none decided exactly what
// stock decided. Deriving it (rather than pinning a third column) is what keeps
// "native answered something stock did not" impossible to write down.
func wantNativeEnvelope(inst rowInstance) envelope {
	if inst.Row.NativeGuard != "" {
		return envUnsupported
	}
	return inst.Stock
}

// TestGuardLedgerIsFailClosed is the load-bearing assertion: over the LIVE legs
// rather than the pinned columns, native must reproduce stock exactly or decline.
// It cannot pass because the corpus was edited to match.
func TestGuardLedgerIsFailClosed(t *testing.T) {
	if len(rowInstances()) == 0 {
		t.Fatal("no row instances to check; the fail-closed contract would be asserted over nothing")
	}
	var violations []string
	for _, inst := range rowInstances() {
		g, ok := groupByName(inst.Row.Group)
		if !ok {
			t.Fatalf("row %q references unknown group %q", inst.Row.ID, inst.Row.Group)
		}
		if inst.excludedFromFixture() {
			continue // no stock leg exists; see TestGuardLedgerRejectedSourceSpellings
		}
		obs := driveStock(t, inst.method(), g.Input)
		gotStock, _, _, classified := classifyStock(obs, inst.Level, inst.label())
		gotNative, _, nativeErr := nativeLeg(g.This, inst.Row.retainedExpr(), inst.Level)

		if !classified {
			violations = append(violations, fmt.Sprintf(
				"%s/%s: stock produced an unclassifiable observation (%v)", inst.Row.ID, inst.Level, obs.err))
			continue
		}
		if gotNative == envUnsupported {
			if !errors.Is(nativeErr, debaml.ErrConstraintUnsupported) {
				violations = append(violations, fmt.Sprintf(
					"%s/%s: native refused with an error that does not wrap ErrConstraintUnsupported: %v",
					inst.Row.ID, inst.Level, nativeErr))
			}
			continue
		}
		if gotNative != gotStock {
			violations = append(violations, fmt.Sprintf(
				"%s/%s: native answered %s where stock produced %s — expr %q over %s",
				inst.Row.ID, inst.Level, gotNative, gotStock, inst.Row.retainedExpr(), g.Input))
		}
	}
	if len(violations) > 0 {
		t.Fatalf("the evaluator is NOT fail-closed; %d instance(s) produce a result stock does not:\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// The tally.
// ---------------------------------------------------------------------------

// The pinned population of each agreement bucket. Each label means exactly what
// its constant says (see the agreement doc comments) and the four partition the
// instance set — asserted below rather than assumed, so a bucket cannot quietly
// absorb a row that belongs in another.
//
//	wantAgreeAnswer    stock decided and native decided the SAME thing.
//	wantAgreeRefusal   stock refused to produce a boolean and native refused too.
//	wantNativeDeclines stock decided and native refused: the measured COST.
//	                   A guard whose rows land here is NOT green.
//	wantStockFatal     stock is unobservable in-process (recorded in fatal_test.go,
//	                   never in this binary), so no in-process instance carries it.
//	wantSourceRejected BAML will not COMPILE the source spelling, so there is no
//	                   stock leg to compare against at all.
const (
	wantAgreeAnswer    = 46
	wantAgreeRefusal   = 34
	wantNativeDeclines = 188
	wantStockFatal     = 0
	wantSourceRejected = 1
)

// bucketOf classifies one instance from its pins. It has no default arm.
func bucketOf(inst rowInstance) (agreement, bool) {
	if inst.Stock == envProcessFatal {
		return agFatal, true
	}
	if inst.Stock == envSourceRejected {
		return agSourceRejected, true
	}
	native := wantNativeEnvelope(inst)
	switch {
	case native != envUnsupported && native == inst.Stock:
		return agAnswer, true
	case native == envUnsupported && stockDecided(inst.Stock):
		return agNativeDeclines, true
	case native == envUnsupported && !stockDecided(inst.Stock):
		return agRefusal, true
	default:
		return "", false
	}
}

// TestGuardLedgerTally pins how the corpus is distributed across the agreement
// buckets, so a guard removal that turned a decline into an answer — or an
// answer into a decline — has to be acknowledged rather than absorbed.
func TestGuardLedgerTally(t *testing.T) {
	counts := map[agreement]int{}
	instances := rowInstances()
	for _, inst := range instances {
		b, ok := bucketOf(inst)
		if !ok {
			t.Fatalf("instance %s/%s pins native=%s against stock=%s, which is neither agreement nor a decline",
				inst.Row.ID, inst.Level, wantNativeEnvelope(inst), inst.Stock)
		}
		counts[b]++
	}
	for _, want := range []struct {
		bucket agreement
		n      int
	}{
		{agAnswer, wantAgreeAnswer},
		{agRefusal, wantAgreeRefusal},
		{agNativeDeclines, wantNativeDeclines},
		{agFatal, wantStockFatal},
		{agSourceRejected, wantSourceRejected},
	} {
		if counts[want.bucket] != want.n {
			t.Errorf("%s = %d, want %d", want.bucket, counts[want.bucket], want.n)
		}
	}
	total := counts[agAnswer] + counts[agRefusal] + counts[agNativeDeclines] + counts[agFatal] + counts[agSourceRejected]
	if total != len(instances) {
		t.Errorf("the buckets cover %d of %d instances; they must partition the corpus", total, len(instances))
	}
}

// ---------------------------------------------------------------------------
// The two asymmetries this slice RECORDS rather than resolves.
// ---------------------------------------------------------------------------

// TestGuardLedgerDuplicateLabelIsLastWriteWins records what stock does with two
// @check attributes under one label: the Go checks map cannot hold both, and the
// LAST declaration wins. 7.2a-1 records it; 7.2b decides the wire shape.
//
// It is asserted, not merely logged, so a change in BAML's collapse rule is
// caught rather than discovered later.
func TestGuardLedgerDuplicateLabelIsLastWriteWins(t *testing.T) {
	obs := driveStock(t, dupClass+"Fn", `{"v":1}`)
	if obs.err != nil {
		t.Fatalf("stock rejected the duplicate-label fixture: %v", obs.err)
	}
	if len(obs.checks) != 1 {
		t.Fatalf("stock reported %d check entries for two attributes under one label, want exactly 1: %#v",
			len(obs.checks), obs.checks)
	}
	got, ok := obs.checks["dup"]
	if !ok {
		t.Fatalf("stock reported no entry under the duplicated label: %#v", obs.checks)
	}
	// The SECOND declaration (`this == 2`, false over v=1) is the surviving one.
	if got.Expression != "this == 2" || got.Status != "failed" {
		t.Fatalf("the surviving entry is not the LAST declaration: expression %q status %q, want %q / %q",
			got.Expression, got.Status, "this == 2", "failed")
	}
}

// TestGuardLedgerAssertAndCheckAgreeOnAnErroringPredicate proves, over EVERY row
// that carries both levels with an evaluator error, that stock produces the same
// envelope AND the same engine message at @check and at @assert.
//
// It is what licenses the one omission the corpus still makes (an optional field
// whose predicate errors, where the optional coercion makes the two levels
// genuinely indistinguishable), and it is asserted over the live legs rather
// than derived from the pins.
func TestGuardLedgerAssertAndCheckAgreeOnAnErroringPredicate(t *testing.T) {
	covered := 0
	for _, r := range guardRows {
		if r.StockCheck != envEvaluatorError || r.StockAssert != envEvaluatorError {
			continue
		}
		covered++
		g, ok := groupByName(r.Group)
		if !ok {
			t.Fatalf("row %q references unknown group %q", r.ID, r.Group)
		}
		var envs [2]envelope
		var inners [2]string
		for k, lv := range []level{levelCheck, levelAssert} {
			inst := rowInstance{Row: r, Level: lv, Stock: envEvaluatorError}
			env, inner, _, classified := classifyStock(driveStock(t, inst.method(), g.Input), lv, inst.label())
			if !classified {
				t.Errorf("row %q at %s: stock produced an unclassifiable observation", r.ID, lv)
				continue
			}
			envs[k], inners[k] = env, inner
		}
		if envs[0] != envs[1] || inners[0] != inners[1] {
			t.Errorf("row %q: stock differs by LEVEL — check=(%s, %q) assert=(%s, %q). The corpus's one "+
				"AssertOmitted reason depends on this equivalence holding.",
				r.ID, envs[0], inners[0], envs[1], inners[1])
		}
	}
	if covered == 0 {
		t.Fatal("no row carries an evaluator error at both levels, so the level-equivalence claim is untested")
	}
}

// TestGuardLedgerRejectedSourceSpellings CORROBORATES the classification of an
// [envSourceRejected] row: that the rejection is a jinja SYNTAX error rather
// than some other refusal.
//
// It is deliberately the weaker of the two proofs, and it is fork evidence:
// BAML parses a constraint attribute with minijinja's own expression parser, so
// the fork this package links rejects the same bytes the same way, but that is a
// statement about the engine family rather than about stock. The AUTHORITATIVE
// observation — that stock BAML v0.223 refuses to compile the spelling and
// accepts the alternative — is TestGuardLedgerStockRejectsTheBareSubscriptSpelling
// in sourceprobe_test.go, which asks the stock compiler itself. This test exists
// because stock's Go binding discards the diagnostic, so the error CLASS can only
// be read here.
func TestGuardLedgerRejectedSourceSpellings(t *testing.T) {
	env := mj.NewEnvironment()
	seen := 0
	for _, r := range guardRows {
		if r.StockCheck != envSourceRejected {
			continue
		}
		seen++
		if _, err := env.TemplateFromString("{{ " + r.retainedExpr() + " }}"); err == nil {
			t.Errorf("row %q is recorded as a spelling BAML refuses to compile, but the engine parses "+
				"%q happily; the record is stale", r.ID, r.retainedExpr())
		} else if !strings.Contains(err.Error(), "syntax error") {
			t.Errorf("row %q: %q was rejected, but not as a SYNTAX error: %v", r.ID, r.retainedExpr(), err)
		}
		if r.AcceptedAlternative == "" {
			t.Errorf("row %q records a rejected spelling but names no accepted alternative, so the row does "+
				"not say what the attribute language actually requires", r.ID)
			continue
		}
		if _, err := env.TemplateFromString("{{ " + r.AcceptedAlternative + " }}"); err != nil {
			t.Errorf("row %q: the accepted alternative %q does not compile either (%v), so the rejection is "+
				"about the construct rather than the spelling", r.ID, r.AcceptedAlternative, err)
		}
	}
	if seen == 0 {
		t.Fatal("no row records a rejected source spelling; the unparenthesized subscript observation is missing")
	}
}

// ---------------------------------------------------------------------------
// The corpus <-> ledger seam.
// ---------------------------------------------------------------------------

// TestGuardLedgerCoversEveryLedgerRecord proves the in-repo ledger and this
// corpus describe the same evidence: every witness row the ledger cites exists
// here, and every guard a row claims to witness has a ledger record.
func TestGuardLedgerCoversEveryLedgerRecord(t *testing.T) {
	records, err := loadLedger(ledgerJSONPath)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	byKey := map[string]bool{}
	for _, rec := range records {
		if byKey[rec.Key] {
			t.Errorf("ledger record %q appears more than once; the second would silently stand in for the "+
				"first everywhere this map is consulted", rec.Key)
		}
		byKey[rec.Key] = true
		for _, id := range rec.WitnessRows {
			if _, ok := rowByID(id); !ok {
				t.Errorf("ledger record %q cites witness row %q, which is not in the corpus (have: %s)",
					rec.Key, id, strings.Join(witnessIDs(), " "))
			}
		}
	}
	for _, r := range guardRows {
		for _, key := range r.Guards {
			if !byKey[key] {
				t.Errorf("row %q claims to witness guard %q, which has no ledger record", r.ID, key)
			}
		}
	}

	// The PER-CALLABLE inventory cites rows too, and they must be real. Its
	// completeness against the live profile tables is enforced in
	// internal/debaml (which can read them); what this package can prove is that
	// the evidence it points at exists.
	byCallable, err := loadLedgerCallableRows(ledgerJSONPath)
	if err != nil {
		t.Fatalf("load callable inventory: %v", err)
	}
	for callable, ids := range byCallable {
		for _, id := range ids {
			if _, ok := rowByID(id); !ok {
				t.Errorf("callable inventory entry %q cites witness row %q, which is not in the corpus", callable, id)
			}
		}
	}

	// The DISPOSITION obligations, re-checked from the ledger rather than from a
	// comment. Each disposition makes a different claim and owes different
	// evidence:
	//
	//	removed             every cited row is GREEN (an agreement, never a native
	//	                    decline) and a surviving guard is named.
	//	kept-inert          every cited row is GREEN too — the guard costs nothing
	//	                    — so it owes a stated reason for staying, not a
	//	                    deferral.
	//	kept-over-decline   at least one cited row is a native decline of an
	//	                    expression stock decided. That is a parity-affecting
	//	                    deferral and owes a logged record.
	//	kept-unwitnessable  stock offers no observable reference at all, so it owes
	//	                    a logged record and cites no in-process row.
	buckets := map[string][]agreement{}
	for _, inst := range rowInstances() {
		b, ok := bucketOf(inst)
		if !ok {
			continue // reported by TestGuardLedgerTally
		}
		buckets[inst.Row.ID] = append(buckets[inst.Row.ID], b)
	}
	for _, problem := range dispositionProblems(records, func(id string) (guardRow, bool) { return rowByID(id) }, buckets) {
		t.Error(problem)
	}
}

// dispositionProblems is every way a record fails the obligation its disposition
// carries, as a list of messages.
//
// It is a FUNCTION over records rather than an inline block so the obligations
// can be exercised against synthetic ledgers — see
// TestDispositionObligationsAreNotVacuous. Several of them are "the cited rows
// must all look like X", and a check of that shape passes over an empty list
// while proving nothing, so each disposition states explicitly whether it
// requires rows, forbids them, or needs at least one of a kind.
func dispositionProblems(records []ledgerRecord, lookup func(string) (guardRow, bool), buckets map[string][]agreement) []string {
	green := func(id string) bool {
		for _, b := range buckets[id] {
			if b != agAnswer && b != agRefusal {
				return false
			}
		}
		return true
	}
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	for _, rec := range records {
		switch rec.Disposition {
		case "removed":
			if len(rec.WitnessRows) == 0 {
				add("ledger record %q is REMOVED but cites no witness row", rec.Key)
			}
			if rec.SubsumedBy == "" {
				add("ledger record %q is REMOVED but names no guard that now carries its refusals", rec.Key)
			}
			for _, id := range rec.WitnessRows {
				if _, ok := lookup(id); !ok {
					continue // already reported above
				}
				if !green(id) {
					add("ledger record %q is REMOVED, but its witness %s is %v — a removal owes GREEN rows only",
						rec.Key, id, buckets[id])
				}
			}
		case "kept-inert":
			if rec.DeferralRecord != "" {
				add("ledger record %q is KEPT-INERT but links a deferral record; an inert guard costs no coverage", rec.Key)
			}
			if rec.Notes == "" {
				add("ledger record %q is KEPT-INERT but states no reason for staying", rec.Key)
			}
			// Without this the greenness loop below iterates nothing and the
			// disposition asserts itself. Inertness means "its rows agree", so
			// there have to be rows.
			if len(rec.WitnessRows) == 0 {
				add("ledger record %q is KEPT-INERT but cites no witness row; the agreement it claims would "+
					"then be checked over an empty set", rec.Key)
			}
			for _, id := range rec.WitnessRows {
				if _, ok := lookup(id); ok && !green(id) {
					add("ledger record %q claims to be INERT, but its witness %s is %v — it over-declines and must be logged as such",
						rec.Key, id, buckets[id])
				}
			}
		case "kept-unprovable":
			if rec.DeferralRecord == "" {
				add("ledger record %q is KEPT because its removal is unprovable, but links no deferral record", rec.Key)
			}
			if rec.LivenessProof == "" {
				add("ledger record %q is kept although no witness row can observe it, so it owes an in-package "+
					"liveness proof; without one, deleting the guard would leave every row in this corpus green", rec.Key)
			}
			if rec.SubsumedBy != "" {
				add("ledger record %q is kept, not removed, so it must not name a subsuming guard", rec.Key)
			}
			// UNPROVABILITY IS A CLAIM ABOUT A SET OF ROWS, so the set must be
			// non-empty. Without this the attribution loop below iterates nothing
			// and the disposition asserts itself on a deferral link and a
			// liveness string alone — the same vacuity the kept-inert arm was
			// hardened against, and the opposite of kept-unwitnessable, which
			// requires the list to be EMPTY.
			if len(rec.WitnessRows) == 0 {
				add("ledger record %q is KEPT-UNPROVABLE but cites no witness row; the claim that no row can "+
					"observe this guard's absence is a statement ABOUT rows, and there are none to check", rec.Key)
			}
			// The claim is that NO cited row can observe the guard's absence, i.e.
			// every one of them is stopped by a DIFFERENT guard. If a row were
			// attributed to this guard itself, the removal would be testable and
			// the record would be wrong.
			for _, id := range rec.WitnessRows {
				r, ok := lookup(id)
				if !ok {
					continue // already reported above
				}
				if r.NativeGuard == rec.Key {
					add("ledger record %q is filed as unprovable, but its witness %s IS attributed to it; "+
						"the removal is observable after all and the record must be re-derived", rec.Key, id)
				}
			}
		case "kept-over-decline":
			if rec.DeferralRecord == "" {
				add("ledger record %q is KEPT as an over-decline but links no deferral record", rec.Key)
			}
			costly := false
			for _, id := range rec.WitnessRows {
				for _, b := range buckets[id] {
					if b == agNativeDeclines {
						costly = true
					}
				}
			}
			if !costly {
				add("ledger record %q is filed as an over-decline, but none of its witnesses %v is a native decline; "+
					"it is inert and the tally would be mislabelled", rec.Key, rec.WitnessRows)
			}
		case "kept-unwitnessable":
			if rec.DeferralRecord == "" {
				add("ledger record %q is KEPT as unwitnessable but links no deferral record", rec.Key)
			}
			if len(rec.WitnessRows) != 0 {
				add("ledger record %q is filed as unwitnessable but cites in-process rows %v", rec.Key, rec.WitnessRows)
			}
			if rec.SubprocessWitness == "" && rec.Key != "hasMedia" {
				add("ledger record %q is unwitnessable in-process and names no subprocess witness either", rec.Key)
			}
		default:
			add("ledger record %q carries an unknown disposition %q", rec.Key, rec.Disposition)
		}
	}
	return out
}

// TestGuardLedgerEnvelopeProseCoversEveryCitedRow keeps each record's recorded
// envelope FAITHFUL to the rows it cites.
//
// A record's stockEnvelope is prose, and prose drifts: rows get added to a guard
// and the summary keeps describing the old set, so a reader cannot map every
// entry in witnessRows back to a recorded outcome — which is the one thing the
// ledger exists to let them do. The check is mechanical rather than editorial:
// every DISTINCT envelope among a record's cited rows must be named, by its
// canonical envelope word, somewhere in the summary.
//
// It does not prescribe wording beyond that. A record may group rows by
// operation, by expression or by id, as long as no outcome class it cites goes
// unmentioned.
func TestGuardLedgerEnvelopeProseCoversEveryCitedRow(t *testing.T) {
	records, err := loadLedger(ledgerJSONPath)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	assertSomeRecordCitesRows(t, records)
	for _, rec := range records {
		if len(rec.WitnessRows) == 0 {
			continue
		}
		seen := map[envelope]string{}
		for _, id := range rec.WitnessRows {
			r, ok := rowByID(id)
			if !ok {
				continue // reported by TestGuardLedgerCoversEveryLedgerRecord
			}
			if _, dup := seen[r.StockCheck]; !dup {
				seen[r.StockCheck] = id
			}
		}
		for env, example := range seen {
			if !strings.Contains(rec.StockEnvelope, string(env)) {
				t.Errorf("ledger record %q cites a row with the %s envelope (%s), but its recorded stock "+
					"envelope never mentions that outcome:\n  %s",
					rec.Key, env, example, rec.StockEnvelope)
			}
		}
	}
}

// TestGuardLedgerEnvelopeProseAccountsForEveryCitedRow is the PER-ROW half, and
// it is the one that stops this class of defect recurring.
//
// Its sibling above checks per-outcome-CLASS coverage, which is necessary and
// not sufficient: a record can cite ten rows, name nine, and still mention every
// class — the tenth's class is already covered by one of its siblings. That is
// exactly how SPLIT_LENGTH went unaccounted for in the lengthGuard record while
// the class check stayed green.
//
// So the rule here is mechanical and total: EVERY id in witnessRows must appear
// by name in BOTH the stock and the native envelope prose. A reader can then map
// every cited row to a recorded outcome on each leg, which is the only thing that
// makes the ledger evidence rather than summary.
//
// The match is word-bounded, so `MAP_STRING` is not satisfied by a mention of
// `MAP_STRING_LEN`, and `N1` is not satisfied by `N10`.
func TestGuardLedgerEnvelopeProseAccountsForEveryCitedRow(t *testing.T) {
	records, err := loadLedger(ledgerJSONPath)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	assertSomeRecordCitesRows(t, records)
	for _, rec := range records {
		for _, id := range rec.WitnessRows {
			if _, ok := rowByID(id); !ok {
				continue // reported by TestGuardLedgerCoversEveryLedgerRecord
			}
			for _, leg := range []struct{ name, prose string }{
				{"stock", rec.StockEnvelope},
				{"native", rec.NativeEnvelope},
			} {
				if mentionsRowID(leg.prose, id) {
					continue
				}
				t.Errorf("ledger record %q cites witness row %s, but its %s envelope never accounts for it:\n  %s",
					rec.Key, id, leg.name, leg.prose)
			}
		}
	}
}

// assertSomeRecordCitesRows guards the prose checks against vacuity: both of
// them iterate a record's cited rows, so a ledger in which nothing cites
// anything would satisfy them having compared nothing at all.
func assertSomeRecordCitesRows(t *testing.T, records []ledgerRecord) {
	t.Helper()
	for _, rec := range records {
		if len(rec.WitnessRows) > 0 {
			return
		}
	}
	t.Fatal("no ledger record cites a witness row; the envelope-prose checks would pass over an empty set")
}

// mentionsRowID reports whether prose names this row id as a WHOLE token.
//
// Row ids share prefixes — N1/N10/N1b, MAP_STRING/MAP_STRING_LEN — so a bare
// substring test would let one row's mention stand in for another's, which is
// the same accounting hole one level down.
func mentionsRowID(prose, id string) bool {
	for i := 0; ; {
		j := strings.Index(prose[i:], id)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(id)
		beforeOK := start == 0 || !isRowIDByte(prose[start-1])
		afterOK := end == len(prose) || !isRowIDByte(prose[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
}

func isRowIDByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// TestGuardLedgerEveryNamedWitnessIsPresent pins the row ids scope §1 names by
// hand, so a corpus edit cannot quietly drop one of the required witnesses.
func TestGuardLedgerEveryNamedWitnessIsPresent(t *testing.T) {
	required := []string{
		"N1", "N2", "N3", "N4", "N5", "N6", "N7", "N8", "N9", "N10", "N11", "N12",
		"O1", "O2", "O3", "O4", "O5", "O6", "O7", "O8", "O9",
		// the mapping / last / items / split / length / global rows
		"MAP_SUBSCRIPT", "MAP_LIST", "MAP_REVERSE_LIST", "MAP_STRING", "MAP_CONCAT",
		"MAP_EQUALITY", "CLS_FIELD", "MAP_NESTED", "CLS_NESTED_LIST",
		"LAST_CLS_VALUE", "LAST_CLS_KEY", "LAST_MAP_KEY",
		"ITEMS_MAP", "ITEMS_CLS", "ITEMS_NEST", "TOJSON_MAP", "TOJSON_NEST",
		"SPLIT_LIST", "SPLIT_ITERABLE", "SPLIT_INDEX", "SPLIT_LENGTH",
		"LEN_INT", "LEN_NULL", "LEN_BOOL", "LEN_MAP", "LEN_CLS", "LEN_LIST", "LEN_STR",
		"CNT_INT", "CNT_STR", "CNT_LIST", "CNT_MAP",
		"RANGE_LIST", "RANGE_LAST", "RANGE_STEP",
		"DICT_ARITY", "NAMESPACE_ATTR", "DEBUG_CALL",
		// the five withdrawn non-BAML builtins
		"WB_URLENCODE", "WB_CONTAINING", "WB_CYCLER", "WB_JOINER", "WB_LIPSUM",
		// the mandatory foreign-mapping negative
		"FMAP_NONSTRING_KEY",
	}
	sort.Strings(required)
	for _, id := range required {
		if _, ok := rowByID(id); !ok {
			t.Errorf("required witness row %q is missing from the corpus", id)
		}
	}
}
