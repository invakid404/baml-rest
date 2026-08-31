package nativespinejsonfixture

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenDeps are import-path substrings that would mean the emitted JSON-alias
// carrier reached the generated BAML client, the BAML runtime, the patched
// CFFI-bearing fork, or a CFFI symbol. ExecBridge-U1's no-CFFI invariant requires the
// emitted native package to link none of them.
var forbiddenDeps = []string{
	"baml_client",
	"github.com/boundaryml/baml",
	"dynclient/baml-patched",
	"language_client_go",
}

// TestGeneratedPackageHasNoBAMLOrCFFI asserts the non-test import graph of this
// package (the committed generated carriers + the pure-Go runtime/adapter) contains
// no baml_client directory and no BAML/CFFI import — the crux of ExecBridge-U1's
// hermetic emitted module. It shells out to `go list -deps`.
func TestGeneratedPackageHasNoBAMLOrCFFI(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping import-graph assertion")
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))

	const pkg = "github.com/invakid404/baml-rest/internal/nativespinejsonfixture"
	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Dir = repoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\nstderr: %s", pkg, err, stderr.String())
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		t.Fatal("go list returned no dependencies")
	}
	deps := strings.Split(trimmed, "\n")
	for _, dep := range deps {
		for _, bad := range forbiddenDeps {
			if strings.Contains(dep, bad) {
				t.Errorf("emitted JSON-alias package depends on %q (matches forbidden %q) — it must link no baml_client/BAML/CFFI", dep, bad)
			}
		}
	}
}
