package baml_rest

// De-BAML Phase 7B unrouted guard. The native OpenAI stream plan/execution lane
// (nativeserve/admission.AdmitStreamClaim + nativeserve/execute.RunStream) is a
// TEST-HARNESS API only: it must be UNREACHABLE from RunStreamOrchestration, the
// generated adapter, workers, and every public route until Phase 7D wires the
// proven lane behind the umbrella flag.
//
// This is a source-level structural guard (it runs in the default, CGO-free
// `go test`, complementing scripts/check-host-zero-nanollm.sh, which proves the
// host link graph carries no nanollm at all): it walks the whole repository and
// asserts that the two lane entrypoints are CALLED only from test files. A
// production (non-_test.go) caller anywhere would fail this test — the earliest,
// cheapest signal that the unrouted invariant was broken.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNativeStreamLaneUnrouted(t *testing.T) {
	root := repoRoot(t)

	// The lane entrypoints, as CALL forms (trailing "("), and the single file
	// allowed to contain each (its definition site). A trailing "(" excludes the
	// prose mentions in doc comments and never collides with RunStreamOrchestration
	// (whose "(" follows an "O", not "RunStream").
	guarded := []struct {
		call    string
		defFile string // repo-relative; the only non-test file allowed to contain call
	}{
		{"AdmitStreamClaim(", filepath.FromSlash("nativeserve/admission/stream.go")},
		{"RunStream(", filepath.FromSlash("nativeserve/execute/stream.go")},
	}

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
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(data)
		for _, g := range guarded {
			if strings.Contains(text, g.call) && rel != g.defFile {
				t.Errorf("native stream entrypoint %q is called from non-test production file %q; the Phase 7B lane must stay UNROUTED (only %q may contain it)", g.call, rel, g.defFile)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
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
