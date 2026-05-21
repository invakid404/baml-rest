// Package testharness is the shared infrastructure backing the codegen
// compile matrix and the pool-lifecycle harness. It owns the temp-
// module synthesis, the go build / go test subprocess wrappers, and
// the cell-enumeration interface that downstream property-based
// generators can extend. The package lives under `internal/` so
// dynclient cannot import it: lifecycle infrastructure is a test-only
// seam.
package testharness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// WriteTempModule lays out a self-contained Go module under `dir` that
// compiles `rendered` (the generator's output) against bamlutils +
// fixtures via replace directives. The temp module reuses
// adapters/common's go.mod + go.sum verbatim — that module already
// pins every transitive dep bamlutils needs — and rewrites the module
// path / replace directives to point at the local sources. Without
// inheriting the transitive-dep pins `go build` would complain
// "updates to go.mod needed" and refuse to proceed without network
// access.
//
// `extraFiles` are written alongside `matrix.go` and let callers ship
// additional Go sources into the temp module — the lifecycle harness
// uses it to install a generated `lifecycle_test.go`. Keys are file
// names relative to `dir`. Pass nil for the compile-matrix case.
func WriteTempModule(t *testing.T, dir, rendered string, extraFiles map[string]string) {
	t.Helper()

	bamlutilsAbs := AbsRepoPath(t, "bamlutils")
	introspectedAbs := AbsRepoPath(t, "introspected")
	commonAbs := AbsRepoPath(t, "adapters/common")

	commonModBytes, err := os.ReadFile(filepath.Join(commonAbs, "go.mod"))
	if err != nil {
		t.Fatalf("read adapters/common go.mod: %v", err)
	}
	commonMod := string(commonModBytes)

	// Rewrite the module path so the temp module is a standalone
	// build target (`matrixtest`). Replace the original first-party
	// replaces (which used `../../...` relative paths) with absolute
	// paths so the temp dir's CWD doesn't matter. Declare the temp
	// module under the adapters/common/codegen import-path prefix so
	// the matrix file can import `.../codegen/internal/fixtures` (and
	// `.../codegen/internal/poolaudit`) — Go's `internal/`
	// visibility rule allows that from any package whose path is
	// rooted at the same parent (`adapters/common/codegen/`). Any
	// module path outside that prefix would be rejected as a
	// cross-tree internal import.
	rewritten := replaceRequired(t, commonMod,
		"module github.com/invakid404/baml-rest/adapters/common",
		"module github.com/invakid404/baml-rest/adapters/common/codegen/matrixtest",
	)
	rewritten = replaceRequired(t, rewritten,
		"github.com/invakid404/baml-rest/bamlutils => ../../bamlutils",
		"github.com/invakid404/baml-rest/bamlutils => "+bamlutilsAbs,
	)
	rewritten = replaceRequired(t, rewritten,
		"github.com/invakid404/baml-rest/introspected => ../../introspected",
		"github.com/invakid404/baml-rest/introspected => "+introspectedAbs,
	)
	// Add a require + replace for the adapters/common module so the
	// rendered file's fixtures / poolaudit imports (sub-packages of
	// adapters/common) resolve to the local source tree.
	rewritten += "\nrequire github.com/invakid404/baml-rest/adapters/common v0.0.0-00010101000000-000000000000\n"
	rewritten += "\nreplace github.com/invakid404/baml-rest/adapters/common => " + commonAbs + "\n"
	WriteFile(t, filepath.Join(dir, "go.mod"), rewritten)

	// Reuse adapters/common's go.sum verbatim so transitive dep
	// hashes don't trip the "module verification" gate.
	commonSum, err := os.ReadFile(filepath.Join(commonAbs, "go.sum"))
	if err != nil {
		t.Fatalf("read adapters/common go.sum: %v", err)
	}
	WriteFile(t, filepath.Join(dir, "go.sum"), string(commonSum))

	WriteFile(t, filepath.Join(dir, "matrix.go"), rendered)

	for name, content := range extraFiles {
		WriteFile(t, filepath.Join(dir, name), content)
	}
}

// replaceRequired is strings.Replace with count=1 that fails the test
// loudly when `old` is not present in `src`. Used for the three
// adapters/common/go.mod rewrites so a future drift in that file
// (e.g. a module-path rename or a reformatted replace directive)
// surfaces as a clear "rewrite target missing" error at the rewrite
// step itself, pinpointing which substring drifted.
func replaceRequired(t *testing.T, src, old, new string) string {
	t.Helper()
	if !strings.Contains(src, old) {
		t.Fatalf("testharness: rewrite target missing in adapters/common/go.mod: %q", old)
	}
	return strings.Replace(src, old, new, 1)
}

// AbsRepoPath returns the absolute path to a repo-relative directory.
// Used to wire the temp module's go.mod replaces against the real
// bamlutils + fixtures source trees. Uses runtime.Caller against this
// file (which is at adapters/common/codegen/internal/testharness/) to
// locate the repo root.
func AbsRepoPath(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve testharness file path via runtime.Caller")
	}
	// Walk up from adapters/common/codegen/internal/testharness/<this
	// file> to the repo root, then descend into the requested rel
	// path. The parent chain is internal -> codegen -> common ->
	// adapters -> <repo>.
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", ".."))
	return filepath.Join(repoRoot, rel)
}

// WriteFile is a t.Helper wrapper that fails the test on write error
// so a permissions / disk failure surfaces immediately with a labelled
// line. Tests use it instead of bare os.WriteFile.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
