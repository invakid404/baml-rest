package testharness

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteTempBamlSrc lays out a synthetic `baml_src/` directory under
// `dir`. `files` maps file paths relative to `baml_src/` (e.g.
// "main.baml", "shared/types.baml") to their textual content.
// Subdirectories implied by the relative path are created on demand.
//
// Static-emission fuzz cases use this helper to materialize a
// generated BAML source tree that the integration test then ingests
// through the standard server boot path. The temp directory is
// expected to be a t.TempDir() so cleanup is automatic.
func WriteTempBamlSrc(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	root := filepath.Join(dir, "baml_src")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir baml_src: %v", err)
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if parent := filepath.Dir(path); parent != root {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", parent, err)
			}
		}
		WriteFile(t, path, content)
	}
}
