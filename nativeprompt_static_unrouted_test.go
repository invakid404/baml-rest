package baml_rest

// De-BAML Phase 8A unrouted guard. The emitted static prompt-descriptor surface
// (introspected.StaticPromptDescriptors / StaticPromptDeclines /
// StaticPromptDescriptor) is runtime REPRESENTATION ONLY: in Phase 8A NOTHING
// consumes it on any serving, admission, or socket path. It exists so a later
// native static-serving slice (8B/8C) can transport the already-proven descriptor
// into runtime — but until that slice lands behind the umbrella flag, a
// production reference to any of these symbols outside the generated introspected
// package breaks the "no consumer" invariant.
//
// This is a source-level structural guard that runs in the default, CGO-free
// `go test`. It parses the AST of every production (non-_test.go) .go file in the
// repo and asserts the three guarded names are referenced ONLY inside the
// generated introspected package files (root + dynclient). Because no route reads
// the metadata, flag-on and flag-off public behavior are byte-identical by
// construction — this guard is the mechanical proof of that no-behavior claim.
//
// cmd/introspect/main.go EMITS these names as string literals (jen.Id("Static…")),
// which are *ast.BasicLit, not *ast.Ident references, so the emitter is not a
// consumer and is not flagged. The checked-in fixture lives under testdata and is
// skipped by the walk.

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

// repoRoot resolves the repository root from this test file's own location (the
// unrouted guards live at the repo root). It previously lived in the Phase 7B/7C
// nativestream_unrouted_test.go, which Phase 7D removed when it wired the native
// stream lane behind the umbrella flag; the helper moved here to its remaining
// consumer.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}

func TestStaticPromptDescriptorUnrouted(t *testing.T) {
	root := repoRoot(t)

	// The guarded emitted symbols.
	guarded := map[string]bool{
		"StaticPromptDescriptors": true,
		"StaticPromptDeclines":    true,
		"StaticPromptDescriptor":  true,
	}

	// The generated introspected package files that legitimately DECLARE and
	// cross-reference these symbols. Any reference from any OTHER production file
	// is a consumer and breaks the Phase 8A unrouted invariant. (The stock-oracle
	// fixture at internal/nativeprompt/testdata/... is skipped by the testdata
	// SkipDir below, so it is never scanned.)
	allowed := map[string]bool{
		filepath.FromSlash("introspected/introspected.go"):                              true,
		filepath.FromSlash("dynclient/internal/generated/introspected/introspected.go"): true,
	}

	// declSeen tracks that each guarded name was actually DECLARED in an allowed
	// generated file, so a rename can never silently make the guard vacuous.
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

		file, perr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if perr != nil {
			// A non-parseable production file: fall back to a substring guard so a
			// reference is never silently missed (allowed generated files exempt).
			if !allowed[rel] {
				for name := range guarded {
					if strings.Contains(string(src), name) {
						t.Errorf("emitted static descriptor symbol %q appears in unparseable production file %q (parse error: %v); Phase 8A must stay UNROUTED", name, rel, perr)
					}
				}
			}
			return nil
		}

		// Record declarations found in allowed generated files (for non-vacuity),
		// then exempt those files from the reference scan entirely.
		if allowed[rel] {
			for _, decl := range file.Decls {
				switch dcl := decl.(type) {
				case *ast.FuncDecl:
					if dcl.Name != nil && guarded[dcl.Name.Name] {
						declSeen[dcl.Name.Name] = true
					}
				case *ast.GenDecl:
					for _, spec := range dcl.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for _, name := range vs.Names {
							if guarded[name.Name] {
								declSeen[name.Name] = true
							}
						}
					}
				}
			}
			return nil
		}

		// Any Ident carrying a guarded name in a non-allowed production file is a
		// consumer — a bare identifier or the Sel of a selector like
		// introspected.StaticPromptDescriptor.
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if !guarded[id.Name] {
				return true
			}
			line := fset.Position(id.Pos()).Line
			t.Errorf("emitted static descriptor symbol %q is referenced from production code at %s:%d; Phase 8A adds NO consumer — this surface must stay UNROUTED (a later 8B/8C slice will wire it behind the umbrella flag and update this guard)", id.Name, rel, line)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}

	for name := range guarded {
		if !declSeen[name] {
			t.Errorf("guard is vacuous for %q: no declaration found in the generated introspected package files (rename or missing regen?)", name)
		}
	}
}
