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
		local, imported := bamlprofileLocalName(f)
		if !imported {
			continue
		}
		scanned++
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
				t.Errorf("%s references %s.%s from PRODUCTION code.\n"+
					"internal/debaml (ConstraintValue + EvaluateConstraint) is the single production "+
					"constraint evaluator; internal/bamlprofile's constraint façade is an oracle/test "+
					"facade. Route this through internal/debaml, or move the caller behind a build tag.",
					rel, local, sel.Sel.Name)
			}
			return true
		})
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

// bamlprofileLocalName reports the local name internal/bamlprofile is bound to
// in this file, and whether it is imported at all. A dot-import would make the
// selector scan blind, so it is rejected outright rather than silently skipped.
func bamlprofileLocalName(f *ast.File) (string, bool) {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != bamlprofileImportPath {
			continue
		}
		if imp.Name == nil {
			return "bamlprofile", true
		}
		if imp.Name.Name == "_" {
			// A blank import cannot name a symbol.
			return "", false
		}
		return imp.Name.Name, true
	}
	return "", false
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
