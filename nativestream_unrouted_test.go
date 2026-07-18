package baml_rest

// De-BAML Phase 7B unrouted guard. The native OpenAI stream plan/execution lane
// (nativeserve/admission.AdmitStreamClaim + nativeserve/execute.RunStream) is a
// TEST-HARNESS API only: it must be UNREACHABLE from RunStreamOrchestration, the
// generated adapter, workers, and every public route until Phase 7D wires the
// proven lane behind the umbrella flag.
//
// This is a source-level structural guard (it runs in the default, CGO-free
// `go test`, complementing scripts/check-host-zero-nanollm.sh, which proves the
// host link graph carries no nanollm at all): it parses the AST of every
// production .go file in the repository and asserts that the two lane entrypoints
// are REFERENCED only by their own declarations. A production (non-_test.go)
// reference anywhere — a call, a method value, an address-of, a selector, even one
// split by an intervening comment (`a.AdmitStreamClaim /* x */ (…)`) — fails this
// test, the earliest, cheapest signal that the unrouted invariant broke.
//
// Parsing the AST (rather than substring-matching a call form) is deliberate: a
// comment-obfuscated call or a `f := execute.RunStream` method value that a plain
// `RunStream(` scan would miss is a real identifier reference in the AST, so it is
// caught here.

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

func TestNativeStreamLaneUnrouted(t *testing.T) {
	root := repoRoot(t)

	// The lane entrypoints, mapped to the single repo-relative file allowed to
	// DECLARE (and thus reference) each — its definition site. Any other reference
	// from a production .go file breaks the unrouted invariant.
	defFile := map[string]string{
		"AdmitStreamClaim": filepath.FromSlash("nativeserve/admission/stream.go"),
		"RunStream":        filepath.FromSlash("nativeserve/execute/stream.go"),
	}

	// declSeen tracks that each guarded name's declaration was actually located in
	// its def file, so a typo'd name/path can never silently make the guard vacuous.
	declSeen := map[string]bool{}

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".jj", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		// ParseFile ignores build constraints, so build-tagged production files
		// (e.g. //go:build subprocess) are checked too — a tagged reference still
		// breaks unroutedness.
		file, perr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if perr != nil {
			// A non-parseable .go file is not expected in the production tree; fall
			// back to a substring guard so a reference is never silently missed, and
			// keep walking rather than failing on an odd file.
			for name, def := range defFile {
				if rel != def && strings.Contains(string(src), name) {
					t.Errorf("native stream entrypoint %q appears in unparseable production file %q (parse error: %v); the Phase 7B lane must stay UNROUTED", name, rel, perr)
				}
			}
			return nil
		}

		// The declaration Ident of a guarded name in its own def file is the ONE
		// allowed occurrence; record its position so the AST walk below does not flag
		// it as a reference.
		declPos := map[token.Pos]bool{}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				continue
			}
			if def, guarded := defFile[fn.Name.Name]; guarded && rel == def {
				declPos[fn.Name.Pos()] = true
				declSeen[fn.Name.Name] = true
			}
		}

		// Every Ident carrying a guarded name is a reference — a bare identifier, or
		// the Sel of a selector like `execute.RunStream` / `a.AdmitStreamClaim`
		// (comments between the selector and the call do not change the AST). The
		// sole exemption is each entrypoint's own declaration Ident.
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			def, guarded := defFile[id.Name]
			if !guarded || declPos[id.Pos()] {
				return true
			}
			line := fset.Position(id.Pos()).Line
			t.Errorf("native stream entrypoint %q is referenced from production code at %s:%d (only its declaration in %q may reference it); the Phase 7B lane must stay UNROUTED", id.Name, rel, line, def)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}

	// Non-vacuity: each guarded entrypoint's declaration MUST have been located in
	// its def file. If one is missing, the def-file path or the name is wrong and
	// the guard silently stopped guarding that symbol.
	for name, def := range defFile {
		if !declSeen[name] {
			t.Errorf("guard is vacuous for %q: no declaration found in %q (wrong name or def-file path?)", name, def)
		}
	}
}

// repoRoot resolves the repository root from this test file's own location
// (nativestream_unrouted_test.go lives at the repo root).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}
