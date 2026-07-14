package main

// Freshness guard for the committed opaque nanollm-worker source tar
// (de-BAML cutover Slice 2). Runs in the DEFAULT build (no nanollm, no CGO): it
// only reads the live module tree and rebuilds the deterministic tar in memory,
// so it catches a stale/committed-out-of-date asset before the production
// builder ships one. It does NOT build or link nanollm.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/invakid404/baml-rest/cmd/build/nativeworkersrc"
)

func TestNativeWorkerModuleTarIsFresh(t *testing.T) {
	// go test runs with CWD = this package dir (cmd/build); the repo root is two
	// levels up. Resolve + validate against the isolated module's go.mod.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, nativeworkersrc.ModuleRelPath, "go.mod")); err != nil {
		t.Fatalf("isolated module not found at %s (repoRoot=%s): %v", nativeworkersrc.ModuleRelPath, repoRoot, err)
	}

	want, err := nativeworkersrc.BuildTar(repoRoot)
	if err != nil {
		t.Fatalf("rebuilding tar from live module tree: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repoRoot, nativeworkersrc.TarRelPath))
	if err != nil {
		t.Fatalf("reading committed %s: %v", nativeworkersrc.TarRelPath, err)
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("committed %s is stale (%d bytes) vs the live module tree (%d bytes).\n"+
			"Regenerate it: go run ./cmd/build/gen-nativeworker-src",
			nativeworkersrc.TarRelPath, len(got), len(want))
	}
}
