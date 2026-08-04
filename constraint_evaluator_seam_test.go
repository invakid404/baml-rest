package baml_rest

// The ONE-EVALUATOR SEAM guard.
//
// Two packages in this repo can evaluate a BAML constraint predicate, and they
// arrived by different routes:
//
//   - internal/debaml — ConstraintValue (a BamlValue-shaped, insertion-ordered
//     value model) plus EvaluateConstraint, behind a fail-closed profile that
//     returns ErrConstraintUnsupported for anything it has not proven against
//     stock BAML v0.223 (internal/debaml/constraintoracle);
//   - internal/bamlprofile — EvaluateConstraints over resolved
//     Constraint/ConstraintRequest values, a byte-faithful transcription of
//     run_user_checks + evaluate_predicate, proven by its own stock-CFFI
//     differential (internal/bamlprofile/profileoracle).
//
// THE DECISION, and it is a decision rather than an accident: internal/debaml is
// the SINGLE production evaluator/value seam. bamlprofile's constraint façade
// stays an ORACLE/TEST facade — it may be evaluated against, compared with, and
// used to derive fixtures, but it must not become a second, independently
// evolving runtime evaluator that serving code can reach.
//
// WHY IT MATTERS ENOUGH TO GUARD. Two evaluators means two admission profiles,
// two fail-closed contracts, and two differentials that can drift apart while
// both stay green — and the failure mode is not a broken build. It is a serving
// path that admits a constraint one evaluator proved and the other did not,
// which is exactly the over-claim the whole slice exists to prevent. The seam
// decision is only worth as much as its enforcement, so this test is the
// enforcement: today bamlprofile's constraint API genuinely has no production
// caller, and this is what keeps that true when Slice 7.2 starts wiring the
// serving path.
//
// NOTE ON SCOPE. bamlprofile as a PACKAGE is production — Slice 7.1a wired its
// prompt/host half into internal/nativeprompt's render seam, and
// bamlprofile_embed_test.go pins that its files ship. This guard is narrower: it
// is about the CONSTRAINT symbols only, so the prompt half is untouched.
//
// It runs in the default, CGO-free `go test`.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	// debamlDir holds the designated production evaluator seam.
	debamlDir = "internal/debaml"
	// constraintOracleSubdir is internal/debaml's own stock differential.
	constraintOracleSubdir = "internal/debaml/constraintoracle"
)

// bamlprofileConstraintAPI is the exported surface of bamlprofile's constraint
// façade — the identifiers a caller would have to name to evaluate a constraint
// through it. A production reference to any of them is a second evaluator
// appearing on a serving path.
//
// The list is asserted to be COMPLETE against the package's own source by
// TestBamlprofileConstraintAPIListIsComplete, so a newly exported constraint
// symbol cannot slip past this guard by not being listed here.
var bamlprofileConstraintAPI = map[string]bool{
	"EvaluateConstraints":     true,
	"Constraint":              true,
	"ConstraintRequest":       true,
	"ConstraintReport":        true,
	"ConstraintResult":        true,
	"ConstraintLevel":         true,
	"ConstraintCheck":         true,
	"ConstraintAssert":        true,
	"ConstraintError":         true,
	"ConstraintStage":         true,
	"ConstraintStageValidate": true,
	"ConstraintStageProject":  true,
	"ConstraintStageCompile":  true,
	"ConstraintStageRender":   true,
	"ConstraintStageClassify": true,
}

// bamlprofileImportPath is the package whose constraint half must stay
// test-only.
const bamlprofileImportPath = "github.com/invakid404/baml-rest/internal/bamlprofile"

// TestBamlprofileConstraintAPIHasNoProductionCaller is the seam itself.
//
// It parses every non-test .go file in the repo, finds the ones that import
// internal/bamlprofile, and fails if any of them names a constraint symbol.
// Importing the package is fine and expected — that is the prompt/host half.
func TestBamlprofileConstraintAPIHasNoProductionCaller(t *testing.T) {
	root := repoRoot(t)
	files := allProductionGoFiles(t, root)
	if len(files) == 0 {
		t.Fatal("no production .go files found; this guard would be vacuous")
	}

	scanned := 0
	for _, rel := range files {
		// bamlprofile owns its own constraint code, and profileoracle is the
		// test-only differential that is ALLOWED to drive it — that is precisely
		// the oracle role the seam decision assigns it. Its files are non-test
		// .go by construction (corpus tables the _test.go files consume), so they
		// have to be named here rather than filtered by suffix.
		if strings.HasPrefix(rel, profileDir+"/") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			// A file this walker cannot parse is not evidence of compliance.
			t.Errorf("parsing %s: %v", rel, err)
			continue
		}
		imports, violations := scanSeamViolations(f)
		if !imports {
			continue
		}
		scanned++
		for _, v := range violations {
			t.Errorf("%s: %s", rel, v)
		}
	}

	// Non-vacuity: if nothing outside bamlprofile imports it at all, this guard
	// proves nothing and the reader should know.
	if scanned == 0 {
		t.Logf("note: no production file outside %s imports it; the guard is currently "+
			"forward-looking rather than load-bearing", profileDir)
	}
}

// TestDebamlIsTheProductionEvaluatorSeam pins the OTHER half of the decision:
// the designated seam has to actually exist, and it has to be production code
// rather than something behind a test tag. Without this, the guard above could
// be satisfied by deleting both evaluators.
func TestDebamlIsTheProductionEvaluatorSeam(t *testing.T) {
	root := repoRoot(t)
	want := map[string]bool{
		"EvaluateConstraint":         false,
		"RenderConstraintExpression": false,
		"ConstraintValue":            false,
		"ErrConstraintUnsupported":   false,
	}
	for _, rel := range productionGoFiles(t, debamlDir, constraintOracleSubdir) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Errorf("parsing %s: %v", rel, err)
			continue
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					if _, tracked := want[d.Name.Name]; tracked {
						want[d.Name.Name] = true
					}
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if _, tracked := want[s.Name.Name]; tracked {
							want[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if _, tracked := want[n.Name]; tracked {
								want[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s.%s is not declared in production code; it is the designated "+
				"single production constraint evaluator seam", debamlDir, name)
		}
	}
}

// TestBamlprofileConstraintAPIListIsComplete keeps the guard above honest. It
// re-derives the exported constraint surface from bamlprofile's own constraint
// sources and fails if anything is missing from bamlprofileConstraintAPI — so a
// new exported constraint symbol cannot become a silently unguarded second
// evaluator entry point.
func TestBamlprofileConstraintAPIListIsComplete(t *testing.T) {
	root := repoRoot(t)
	// The two files the seam decision is about. project.go is the constraint-side
	// serde projection of `this`; constraints.go is the evaluator and its types.
	sources := []string{
		filepath.Join(root, profileDir, "constraints.go"),
		filepath.Join(root, profileDir, "project.go"),
	}
	found := 0
	for _, path := range sources {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s not found (%v); the completeness check would be vacuous", path, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			for _, name := range exportedTopLevelNames(decl) {
				found++
				if !bamlprofileConstraintAPI[name] {
					t.Errorf("bamlprofile exports %s from %s, but bamlprofileConstraintAPI does not list it; "+
						"add it so TestBamlprofileConstraintAPIHasNoProductionCaller actually guards it",
						name, filepath.Base(path))
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no exported constraint symbols discovered; the completeness check would be vacuous")
	}
}

// exportedTopLevelNames returns the exported package-level names a declaration
// introduces (functions without a receiver, types, constants and variables).
func exportedTopLevelNames(decl ast.Decl) []string {
	var out []string
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil && d.Name.IsExported() {
			out = append(out, d.Name.Name)
		}
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				if s.Name.IsExported() {
					out = append(out, s.Name.Name)
				}
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if n.IsExported() {
						out = append(out, n.Name)
					}
				}
			}
		}
	}
	return out
}

// scanSeamViolations reports every way this parsed file breaks the one-evaluator
// seam. imports says whether it imports internal/bamlprofile at all, which the
// caller uses for its non-vacuity check.
//
// It is a pure function of the AST, separated from the file walk on purpose:
// that is what lets TestSeamGuardRejectsADotImportBypass drive the guard's own
// logic over a SYNTHETIC file, including the bypass shape, without adding a real
// violating file to the tree.
//
// TWO VIOLATION SHAPES, because one of them cannot be caught by scanning
// references at all:
//
//  1. A QUALIFIED reference — `bamlprofile.EvaluateConstraints(...)`. Found by
//     walking selector expressions whose receiver is the import's local name.
//
//  2. A DOT IMPORT — `import . ".../internal/bamlprofile"`. This is rejected on
//     sight, before any reference scan, and it is the round-5 fix. A dot import
//     binds every exported name of the package into file scope, so a call to
//     the constraint façade is written as a BARE identifier —
//     `EvaluateConstraints(r)` — and produces no selector expression for rule 1
//     to find. The previous cut looked only at selectors and returned "." as an
//     ordinary local name, so the scan simply never matched and the guard stayed
//     green over a genuine second evaluator on a production path. Rejecting the
//     import itself is the only reliable rule: it does not depend on
//     recognising the call, so it holds for every present and future symbol of
//     the façade, including ones nobody has added to
//     [bamlprofileConstraintAPI] yet.
//
// A dot import is refused whatever the file goes on to do with it. That is
// deliberate and costs nothing: no production file in this repo dot-imports
// anything, and the prompt/host half is equally usable through a normal
// qualified import.
func scanSeamViolations(f *ast.File) (imports bool, violations []string) {
	local := ""
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != bamlprofileImportPath {
			continue
		}
		imports = true
		switch {
		case imp.Name == nil:
			local = "bamlprofile"
		case imp.Name.Name == ".":
			// RULE 2. Refuse and stop: with the package dot-imported there is no
			// qualified reference left to look for.
			return true, []string{
				"dot-imports " + bamlprofileImportPath + " from PRODUCTION code.\n" +
					"A dot import binds every exported name into file scope, so a call to the " +
					"constraint façade is written unqualified (`EvaluateConstraints(...)`) and no " +
					"qualified-reference check can see it. internal/debaml (ConstraintValue + " +
					"EvaluateConstraint) is the single production constraint evaluator; import " +
					"bamlprofile normally and use its prompt/host half, or move the caller behind " +
					"a build tag.",
			}
		case imp.Name.Name == "_":
			// A blank import cannot name a symbol.
			return true, nil
		default:
			local = imp.Name.Name
		}
		break
	}
	if !imports || local == "" {
		return imports, nil
	}

	// RULE 1.
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != local {
			return true
		}
		if bamlprofileConstraintAPI[sel.Sel.Name] {
			violations = append(violations, "references "+local+"."+sel.Sel.Name+
				" from PRODUCTION code.\n"+
				"internal/debaml (ConstraintValue + EvaluateConstraint) is the single production "+
				"constraint evaluator; internal/bamlprofile's constraint façade is an oracle/test "+
				"facade. Route this through internal/debaml, or move the caller behind a build tag.")
		}
		return true
	})
	return imports, violations
}

// allProductionGoFiles lists every non-test .go file in the root module, skipping
// vendored/generated trees that are not this repo's own production code.
func allProductionGoFiles(t *testing.T, root string) []string {
	t.Helper()
	skipDirs := map[string]bool{
		".git": true, ".jj": true, "node_modules": true,
		// Generated stock BAML clients and fixture projects: not hand-written
		// production code, and they never reference bamlprofile.
		"baml_client": true, "baml-patched": true,
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		slashed := filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(slashed, ".go") && !strings.HasSuffix(slashed, "_test.go") {
			out = append(out, slashed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repo: %v", err)
	}
	return out
}

// TestSeamGuardRejectsADotImportBypass is the round-5 finding: the seam guard
// had a hole exactly the size of a dot import.
//
// THE BYPASS. TestBamlprofileConstraintAPIHasNoProductionCaller looked only for
// QUALIFIED references (`bamlprofile.EvaluateConstraints`) via selector
// expressions. A dot import binds every exported name into file scope, so the
// same call is written BARE:
//
//	import . "github.com/invakid404/baml-rest/internal/bamlprofile"
//	func evaluateViaWrongSeam(r ConstraintRequest) (ConstraintReport, error) {
//	    return EvaluateConstraints(r)
//	}
//
// That is production code, outside bamlprofile, genuinely calling its evaluator
// — the second production evaluator this PR expressly prohibits — and it
// produced no selector for the scan to match, so the guard stayed green. The
// helper even returned "." as an ordinary local name while its comment claimed
// dot imports were "rejected outright", so the guard documented a protection it
// did not have.
//
// The fix rejects the IMPORT rather than trying to recognise the call, which is
// why it holds for every symbol of the façade rather than only the ones listed
// in bamlprofileConstraintAPI.
//
// This drives [scanSeamViolations] over synthetic sources rather than adding a
// real violating file to the tree: the bypass has to be exercised, not shipped.
func TestSeamGuardRejectsADotImportBypass(t *testing.T) {
	const imp = `"github.com/invakid404/baml-rest/internal/bamlprofile"`

	for name, tc := range map[string]struct {
		src           string
		wantImports   bool
		wantViolation bool
		wantSubstr    string
	}{
		// THE BYPASS, verbatim from the review's repro.
		"dot import calling the evaluator unqualified": {
			src: `package nativeprompt
import . ` + imp + `
func evaluateViaWrongSeam(r ConstraintRequest) (ConstraintReport, error) {
	return EvaluateConstraints(r)
}`,
			wantImports: true, wantViolation: true, wantSubstr: "dot-imports",
		},
		// A dot import is refused even when nothing obviously constraint-shaped
		// is called: the point is that no reference check can see through it.
		"dot import used only for the prompt half": {
			src: `package nativeprompt
import . ` + imp + `
func render() *Environment { return New(Config{}) }`,
			wantImports: true, wantViolation: true, wantSubstr: "dot-imports",
		},
		"dot import with no use at all": {
			src:         `package nativeprompt` + "\n" + `import . ` + imp,
			wantImports: true, wantViolation: true, wantSubstr: "dot-imports",
		},
		// RULE 1 must keep working — the qualified shape the guard always caught.
		"qualified reference to the constraint API": {
			src: `package nativeprompt
import bp ` + imp + `
func f(r bp.ConstraintRequest) (bp.ConstraintReport, error) { return bp.EvaluateConstraints(r) }`,
			wantImports: true, wantViolation: true, wantSubstr: "EvaluateConstraints",
		},
		"qualified reference under the default local name": {
			src: `package nativeprompt
import ` + imp + `
func f() { _ = bamlprofile.ConstraintCheck }`,
			wantImports: true, wantViolation: true, wantSubstr: "ConstraintCheck",
		},
		// CONTROLS. The prompt/host half is production and must stay allowed.
		"qualified use of the prompt half only": {
			src: `package nativeprompt
import ` + imp + `
func render() *bamlprofile.Environment { return bamlprofile.New(bamlprofile.Config{}) }`,
			wantImports: true, wantViolation: false,
		},
		"blank import": {
			src:         `package nativeprompt` + "\n" + `import _ ` + imp,
			wantImports: true, wantViolation: false,
		},
		"no bamlprofile import": {
			src: `package nativeprompt
import "strings"
func f() string { return strings.TrimSpace("x") }`,
			wantImports: false, wantViolation: false,
		},
		// A same-named identifier that is not the import must not false-positive.
		"unrelated selector with a colliding receiver name": {
			src: `package nativeprompt
import "strings"
func f(bamlprofile struct{ EvaluateConstraints string }) string {
	return strings.TrimSpace(bamlprofile.EvaluateConstraints)
}`,
			wantImports: false, wantViolation: false,
		},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "synthetic.go", tc.src, 0)
		if err != nil {
			t.Fatalf("%s: parsing the synthetic source: %v", name, err)
		}
		imports, violations := scanSeamViolations(f)
		if imports != tc.wantImports {
			t.Errorf("%s: imports = %v, want %v", name, imports, tc.wantImports)
		}
		if got := len(violations) > 0; got != tc.wantViolation {
			t.Errorf("%s: violation = %v, want %v (violations: %q)", name, got, tc.wantViolation, violations)
			continue
		}
		if tc.wantSubstr != "" && !strings.Contains(strings.Join(violations, "\n"), tc.wantSubstr) {
			t.Errorf("%s: violation text %q does not mention %q", name, violations, tc.wantSubstr)
		}
	}
}
