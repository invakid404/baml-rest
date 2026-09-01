package spine_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenSpineDeps are import-path substrings that would mean the production
// native runtime factory (nativeserve/spine, which now imports worker to return a
// worker.Runtime) reached the generated BAML client, the BAML runtime, the patched
// CFFI-bearing fork, a CFFI symbol, dynclient, the root generated package, the
// root-runtime wrapper, introspected, or the full-BAML bootstrap. U1b's
// deletion-substrate invariant requires the spine package to link none of them —
// worker.Handler + workerplugin are BAML-free, which is what makes the native-only
// runtime constructible without BAML.
var forbiddenSpineDeps = []string{
	"baml_client",
	"github.com/boundaryml/baml",
	"dynclient/baml-patched",
	"github.com/invakid404/baml-rest/dynclient",
	"language_client_go",
	"internal/rootruntime",
	"github.com/invakid404/baml-rest/introspected",
	"internal/workerboot",
}

// positiveSpineDeps are packages that MUST be present, so an empty/wrong go-list
// output cannot pass this gate by absence: the factory returns a worker.Runtime
// and reuses the BAML-free handler transport.
var positiveSpineDeps = []string{
	"github.com/invakid404/baml-rest/worker",
	"github.com/invakid404/baml-rest/nativeserve/admission",
	"github.com/invakid404/baml-rest/internal/debaml",
}

// TestSpinePackageHasNoBAMLOrCFFI asserts the non-test import graph of
// nativeserve/spine contains no baml_client / BAML / CFFI / dynclient / rootruntime
// / introspected / workerboot dependency, while still linking the BAML-free worker
// package it returns a runtime for. It shells out to `go list -deps` (GOWORK=off,
// this module is out-of-work).
func TestSpinePackageHasNoBAMLOrCFFI(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping import-graph assertion")
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// nativeserve module root is one directory up from the spine package.
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), ".."))

	const pkg = "github.com/invakid404/baml-rest/nativeserve/spine"
	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Dir = moduleRoot
	cmd.Env = append(cmd.Environ(), "GOWORK=off")
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
	depSet := make(map[string]bool, len(deps))
	for _, d := range deps {
		depSet[strings.TrimSpace(d)] = true
		for _, bad := range forbiddenSpineDeps {
			if strings.Contains(d, bad) {
				t.Errorf("nativeserve/spine depends on %q (matches forbidden %q) — the native runtime factory must link no baml_client/BAML/CFFI/dynclient/rootruntime/introspected/workerboot", d, bad)
			}
		}
	}
	for _, want := range positiveSpineDeps {
		if !depSet[want] {
			t.Errorf("nativeserve/spine is missing expected dependency %q — an empty/wrong go-list output must not pass this gate by absence", want)
		}
	}
}
