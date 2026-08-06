package debaml

// The TEST-ONLY guard for the Slice 7.2a-2 coercion-state collector.
//
// THE CLAIM. The collector, its model, and every helper they need exist only in
// _test.go files, so nothing in it can be linked into a production binary,
// reached from Parse / ParseStaticBundle / request routing, or imported by any
// production package. Production internal/debaml is byte-identical to the base
// revision.
//
// WHY IT IS PROVEN STRUCTURALLY, AND WHY IT IS NOT KEYED ON FILENAMES. A
// source-text grep for "the collector is not called anywhere" survives exactly
// the change it is supposed to catch: rename the symbol, or move the file, and
// the grep keeps passing over a tree that no longer contains what it was
// checking. So does an AST guard that DISCOVERS its own subject from files
// matching `*_test.go` — moving the collector to a non-test filename would drop
// it out of the discovered set and leave the scan with nothing to match.
//
// The guard therefore has three rules, over the AST:
//
//  1. ANCHORS. [constraintStateRequiredAnchors] names the collector's
//     load-bearing identifiers explicitly. Each must be DECLARED somewhere in
//     internal/debaml, and every file declaring one must be a _test.go. Moving
//     the collector into a production file fails here, whatever the file is
//     called; deleting or renaming an anchor also fails, forcing a human to
//     re-state what the collector is instead of letting the guard empty itself.
//  2. NAMESPACE. [constraintStateReservedPrefixes] is the identifier namespace
//     the collector owns. No production file anywhere may DECLARE a top-level
//     name inside it — which catches any PART of the collector appearing in
//     non-test code, including helpers that are not anchors and names that never
//     existed before.
//  3. NO PRODUCTION CALLER. No production file may MENTION a collector
//     identifier, as a call, a type reference or anything else.
//
// It runs in the default, CGO-free `go test`, and mirrors the #649
// one-evaluator seam guard at the repo root.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// constraintStateGuardedNameMarker is the substring every collector identifier
// must contain. It keeps the reference scan PRECISE: a guarded name that was
// also an ordinary English word would report unrelated production code and the
// guard would be silenced rather than fixed.
const constraintStateGuardedNameMarker = "onstraint"

// constraintStateReservedPrefixes is the identifier NAMESPACE the test-only
// collector owns.
//
// THIS IS THE RULE THAT SURVIVES A FILE MOVE. Deriving the guarded set from
// files whose NAME says "_test.go" is circular: renaming
// constraint_state_collect_test.go to constraint_state_collect.go would drop its
// declarations out of the set and leave the reference scan with nothing to
// match, so the guard would pass over the exact regression it exists to catch.
// A namespace reservation does not care what the file is called: no PRODUCTION
// file may DECLARE a top-level name with any of these prefixes, so any part of
// the collector appearing in non-test code is caught by its NAME.
var constraintStateReservedPrefixes = []string{
	"constraintState",
	"constraintCoercion",
	"constraintDisposition",
	"constraintOutcome",
	"constraintOrigin",
	"constraintPath",
	"collectConstraintCoercion",
	"errConstraintState",
}

// constraintStateRequiredAnchors are the collector's load-bearing identifiers.
//
// They are named EXPLICITLY rather than discovered, so the guard has a fixed
// point that a rename cannot silently erode: each anchor must be declared
// somewhere in internal/debaml, and every file declaring one must be a
// _test.go. Deleting or renaming an anchor fails the guard and forces a human to
// re-state what the collector is — which is the correct outcome, because a guard
// that discovers its own subject can always be emptied by moving the subject.
var constraintStateRequiredAnchors = []string{
	"constraintCoercionState",
	"constraintCoercionRun",
	"constraintStateCollector",
	"collectConstraintCoercionState",
	"constraintStateEvent",
	"constraintStateSkipped",
	"constraintStateDisposition",
	"constraintStateOutcome",
	"constraintStatePath",
	"constraintStateRawMetadata",
	"constraintStateJSONEquivalent",
	"constraintDispositionSkipBareStringReturn",
	"errConstraintStateUnmodelled",
	"errConstraintStateDiverged",
}

// constraintStateHasReservedPrefix reports whether name is inside the reserved
// collector namespace.
func constraintStateHasReservedPrefix(name string) bool {
	for _, p := range constraintStateReservedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// constraintStateRepoRoot walks up from this source file to the workspace root
// (the directory holding go.work).
func constraintStateRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.work found above %s", filepath.Dir(file))
		}
		dir = parent
	}
}

// constraintStateTopLevelNames returns the package-level names a file declares:
// functions WITHOUT a receiver, types, constants and variables.
//
// METHODS ARE EXCLUDED DELIBERATELY. A method is reachable only through its
// receiver type, which is already in the set, and method names (String, describe,
// walk, find) are common enough that including them would make the reference scan
// report every unrelated production file.
func constraintStateTopLevelNames(f *ast.File) []string {
	var out []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				out = append(out, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					out = append(out, s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						out = append(out, n.Name)
					}
				}
			}
		}
	}
	return out
}

// constraintStateGoFiles lists the .go files under root, split into production
// (non-_test.go) and test files, skipping trees that are not this repo's own
// hand-written code.
func constraintStateGoFiles(t *testing.T, root string) (production, tests []string) {
	t.Helper()
	skipDirs := map[string]bool{
		".git": true, ".jj": true, "node_modules": true,
		// Generated stock BAML clients and fixture projects.
		"baml_client": true, "baml-patched": true,
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		slashed := filepath.ToSlash(rel)
		if !strings.HasSuffix(slashed, ".go") {
			return nil
		}
		if strings.HasSuffix(slashed, "_test.go") {
			tests = append(tests, slashed)
		} else {
			production = append(production, slashed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repo: %v", err)
	}
	return production, tests
}

// constraintStateParse parses one file, FAILING rather than skipping on a parse
// error: a file this walker cannot read is not evidence of compliance.
func constraintStateParse(t *testing.T, root, rel string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", rel, err)
	}
	return f
}

// constraintStateDeclarationIndex maps every top-level name declared under
// internal/debaml to the files that declare it, test and production alike.
//
// It is built from the AST of the DIRECTORY, not from a filename pattern, so a
// collector file renamed or moved within the package is still seen.
func constraintStateDeclarationIndex(t *testing.T, root string) map[string][]string {
	t.Helper()
	dir := filepath.Join("internal", "debaml")
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	index := map[string][]string{}
	files := 0
	for _, e := range entries {
		base := e.Name()
		if e.IsDir() || !strings.HasSuffix(base, ".go") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(dir, base))
		files++
		for _, n := range constraintStateTopLevelNames(constraintStateParse(t, root, rel)) {
			index[n] = append(index[n], rel)
		}
	}
	if files == 0 {
		t.Fatalf("no .go files found in %s; the guard would be vacuous", dir)
	}
	return index
}

// constraintStateDeclaredByProduction reports whether name has at least one
// declaration in a NON-test file.
//
// [constraintStateDeclarationIndex] deliberately indexes test and production
// files alike — rule 1 needs both, so it can tell a test-only anchor from one
// that leaked into production. Any check that means "this is production code"
// must therefore filter, not just test for presence.
func constraintStateDeclaredByProduction(index map[string][]string, name string) bool {
	for _, rel := range index[name] {
		if !strings.HasSuffix(rel, "_test.go") {
			return true
		}
	}
	return false
}

// constraintStateGuardedNames re-derives the guarded identifier set.
//
// It takes every top-level name in internal/debaml that sits in the reserved
// collector namespace, WHATEVER FILE IT IS IN. A collector file renamed to a
// non-test name therefore stays in the set (and is separately reported as a
// production declaration), instead of vanishing from it.
func constraintStateGuardedNames(t *testing.T, index map[string][]string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for n := range index {
		if constraintStateHasReservedPrefix(n) {
			names[n] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("no identifiers in the reserved collector namespace; the collector was deleted or renamed out of its namespace, and this guard would be vacuous")
	}
	return names
}

// constraintStateScanReferences reports every guarded identifier this file
// mentions, with its position. It is a pure function of the AST so
// [TestConstraintStateSeamGuardBitesOnASyntheticProductionCaller] can drive it
// over a synthetic file without adding a violating file to the tree.
//
// It walks EVERY *ast.Ident, which covers a declaration, a call, a type
// reference and a selector's field name alike. Over-reporting is the safe
// direction — it fails a guard and asks a human — and the marker check on the
// guarded set keeps it from happening in practice.
func constraintStateScanReferences(f *ast.File, guarded map[string]bool, fset *token.FileSet) []string {
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || !guarded[id.Name] {
			return true
		}
		hits = append(hits, id.Name+" at "+fset.Position(id.Pos()).String())
		return true
	})
	return hits
}

// TestConstraintStateCollectorIsTestOnly is the guard. It has three rules, and
// each closes a different way the property could be broken.
func TestConstraintStateCollectorIsTestOnly(t *testing.T) {
	root := constraintStateRepoRoot(t)
	index := constraintStateDeclarationIndex(t, root)
	guarded := constraintStateGuardedNames(t, index)

	// NON-VACUITY OF THE GUARD'S OWN INPUTS. Both rule tables are size-checked
	// before they are used. Without this, emptying either list would make its rule
	// pass over everything: an empty anchor list satisfies the
	// `requiredAnchors != len(...)` tally trivially (0 == 0), and an empty prefix
	// list makes constraintStateHasReservedPrefix always return false. Those are
	// the never-fires arms this suite forbids elsewhere, and the guard must not be
	// the one place they live.
	if got, want := len(constraintStateRequiredAnchors), 14; got != want {
		t.Fatalf("constraintStateRequiredAnchors lists %d anchors, want %d; update this count "+
			"deliberately when the collector's load-bearing surface changes", got, want)
	}
	if got, want := len(constraintStateReservedPrefixes), 8; got != want {
		t.Fatalf("constraintStateReservedPrefixes lists %d prefixes, want %d; update this count "+
			"deliberately when the collector's namespace changes", got, want)
	}
	// And the namespace rule must actually classify: a collector name inside it,
	// a production evaluator name outside it.
	if !constraintStateHasReservedPrefix("constraintCoercionState") {
		t.Fatal("the reserved namespace does not cover the collector's own root type; rule 2 is dead")
	}
	if constraintStateHasReservedPrefix("EvaluateConstraint") {
		t.Fatal("the reserved namespace covers the production evaluator; rule 2 would refuse the seam")
	}

	// RULE 1 — ANCHORS. The collector's load-bearing identifiers must exist, and
	// every file declaring one must be a _test.go. This is what catches the
	// collector being MOVED into production: the check is on the symbol, not on a
	// filename pattern, so renaming constraint_state_collect_test.go to
	// constraint_state_collect.go fails here rather than emptying the guard.
	requiredAnchors := 0
	for _, anchor := range constraintStateRequiredAnchors {
		files := index[anchor]
		if len(files) == 0 {
			t.Errorf("collector anchor %q is declared nowhere in internal/debaml; it was deleted or "+
				"renamed — re-state what the collector is in constraintStateRequiredAnchors", anchor)
			continue
		}
		requiredAnchors++
		for _, rel := range files {
			if !strings.HasSuffix(rel, "_test.go") {
				t.Errorf("%s is a PRODUCTION file and declares the collector anchor %q; the "+
					"coercion-state collector must stay test-only", rel, anchor)
			}
		}
	}
	if requiredAnchors != len(constraintStateRequiredAnchors) {
		t.Errorf("found %d of %d collector anchors; the guard is not covering the whole collector",
			requiredAnchors, len(constraintStateRequiredAnchors))
	}

	// PRECISION. Every guarded name must carry the marker, so the reference scan
	// below cannot start reporting unrelated production code (which would get the
	// guard weakened instead of the violation fixed).
	for name := range guarded {
		if !strings.Contains(name, constraintStateGuardedNameMarker) {
			t.Errorf("collector declares %q, which does not contain %q; rename it so the "+
				"repo-wide reference scan stays precise", name, constraintStateGuardedNameMarker)
		}
	}

	production, tests := constraintStateGoFiles(t, root)
	if len(production) == 0 {
		t.Fatal("no production .go files found; this guard would be vacuous")
	}
	if len(tests) == 0 {
		t.Fatal("no _test.go files found; the walker is not seeing the tree")
	}

	// RULE 2 — NAMESPACE RESERVATION. No production file anywhere may DECLARE a
	// top-level name inside the collector's reserved namespace. This catches any
	// PART of the collector appearing in non-test code, including a helper that
	// is not an anchor and a name that was never in the guarded set.
	fset := token.NewFileSet()
	scanned := 0
	for _, rel := range production {
		path := filepath.Join(root, rel)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Errorf("parsing %s: %v", rel, err)
			continue
		}
		scanned++
		for _, n := range constraintStateTopLevelNames(f) {
			if constraintStateHasReservedPrefix(n) {
				t.Errorf("%s declares %q, which is inside the TEST-ONLY coercion-state collector's "+
					"reserved identifier namespace", rel, n)
			}
		}
		// RULE 3 — NO PRODUCTION CALLER. No production file may MENTION a collector
		// identifier, as a call, a type reference or anything else.
		for _, hit := range constraintStateScanReferences(f, guarded, fset) {
			t.Errorf("%s references the TEST-ONLY coercion-state collector: %s", rel, hit)
		}
	}
	if scanned != len(production) {
		t.Errorf("scanned %d of %d production files; an unparsed file is not evidence of compliance", scanned, len(production))
	}
	t.Logf("guarded %d collector identifiers (%d anchors, %d reserved prefixes) against %d production .go files",
		len(guarded), len(constraintStateRequiredAnchors), len(constraintStateReservedPrefixes), scanned)
}

// TestConstraintStateSeamGuardBitesOnASyntheticProductionCaller proves the scan
// above is not vacuously green.
//
// It runs the SAME [constraintStateScanReferences] over a synthetic file that
// does exactly what the guard forbids — a production function calling
// collectConstraintCoercionState — and requires a hit. Without this, a scan that
// silently matched nothing (a broken walker, an empty guarded set, an ast.Inspect
// that returned early) would look identical to a clean tree.
func TestConstraintStateSeamGuardBitesOnASyntheticProductionCaller(t *testing.T) {
	root := constraintStateRepoRoot(t)
	index := constraintStateDeclarationIndex(t, root)
	guarded := constraintStateGuardedNames(t, index)

	const violating = `package debaml

func serveWithConstraints(b *schemaBundle, raw string) error {
	run, err := collectConstraintCoercionState(b, raw)
	if err != nil {
		return err
	}
	_ = run.Root.Disposition
	return nil
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic_production.go", violating, 0)
	if err != nil {
		t.Fatalf("parsing the synthetic file: %v", err)
	}
	hits := constraintStateScanReferences(f, guarded, fset)
	if len(hits) == 0 {
		t.Fatal("the scan found no violation in a file that calls collectConstraintCoercionState; " +
			"TestConstraintStateCollectorIsTestOnly is passing vacuously")
	}
	found := false
	for _, h := range hits {
		if strings.HasPrefix(h, "collectConstraintCoercionState ") {
			found = true
		}
	}
	if !found {
		t.Errorf("the scan reported %v but not the collector entry point itself", hits)
	}

	// And the NEGATIVE control: a production-shaped file that touches only the
	// production evaluator seam must NOT be reported, or the guard would be a
	// blanket refusal of the word "constraint" rather than a reachability proof.
	const clean = `package debaml

func evaluate(v ConstraintValue, expr string) (bool, error) {
	return EvaluateConstraint(v, expr)
}
`
	cf, err := parser.ParseFile(fset, "clean_production.go", clean, 0)
	if err != nil {
		t.Fatalf("parsing the clean file: %v", err)
	}
	if hits := constraintStateScanReferences(cf, guarded, fset); len(hits) != 0 {
		t.Errorf("the scan reported the production evaluator seam as a collector reference: %v", hits)
	}

	// RULE 2's bite check. The reference scan only knows the names that exist
	// today; the namespace reservation is what catches a piece of the collector
	// appearing in production under a NEW name, which is the shape a file move
	// produces. Drive it over the same synthetic AST.
	const newName = `package debaml

type constraintStateFreshHelper struct{}

func constraintCoercionSomethingNew() {}
`
	nf, err := parser.ParseFile(fset, "moved_production.go", newName, 0)
	if err != nil {
		t.Fatalf("parsing the moved-collector file: %v", err)
	}
	reserved := 0
	for _, n := range constraintStateTopLevelNames(nf) {
		if constraintStateHasReservedPrefix(n) {
			reserved++
		}
		if guarded[n] {
			t.Errorf("the synthetic new name %q is already in the guarded set; the fixture no longer "+
				"tests the NAMESPACE rule", n)
		}
	}
	if reserved != 2 {
		t.Errorf("the namespace rule matched %d of 2 new collector-namespace declarations; a "+
			"file moved into production could slip past it", reserved)
	}
	// NEGATIVE control for the namespace rule: production's own constraint
	// evaluator names must NOT be inside the reserved namespace, or the rule
	// would refuse the existing seam. The list is size-checked so emptying it
	// cannot turn this control into a loop that runs zero times.
	productionNames := []string{"ConstraintValue", "EvaluateConstraint", "constraintEnv", "renderConstraint", "ErrConstraintUnsupported"}
	if len(productionNames) != 5 {
		t.Fatalf("the negative control lists %d production names, want 5", len(productionNames))
	}
	for _, n := range productionNames {
		if constraintStateHasReservedPrefix(n) {
			t.Errorf("the reserved namespace swallows the production evaluator name %q", n)
		}
		// And each really is declared by PRODUCTION, so the control is about the
		// live evaluator seam rather than about strings nobody uses.
		//
		// "declared at all" is not enough: the declaration index covers _test.go
		// files too, so a name that had moved into test-only code would keep this
		// control green while no longer representing the production seam at all —
		// which is exactly the stale-fixture false-green the rest of this suite
		// refuses. Require a NON-test declaration.
		if !constraintStateDeclaredByProduction(index, n) {
			t.Errorf("%q has no PRODUCTION declaration in internal/debaml (declared in %v); the "+
				"negative control is stale — it no longer names the production evaluator seam",
				n, index[n])
		}
	}
}
