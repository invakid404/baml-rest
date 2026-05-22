package testharness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WriteTempBamlSrc lays out a synthetic `baml_src/` directory under
// `dir`. `files` maps file paths relative to `baml_src/` (e.g.
// "main.baml", "shared/types.baml") to their textual content.
// Subdirectories implied by the relative path are created on demand.
//
// File names must be relative paths that stay under the baml_src
// root after cleaning: absolute paths and `..` traversal are
// rejected with t.Fatalf. The synthesis path runs against test-
// authored input only, but defensive containment keeps the failure
// mode crisp if a future generator emits a bad name. See
// CheckBamlSrcName for the validation contract exposed to tests.
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
		if err := CheckBamlSrcName(root, name); err != nil {
			t.Fatalf("WriteTempBamlSrc: %v", err)
		}
		full := filepath.Join(root, filepath.Clean(name))
		if parent := filepath.Dir(full); parent != root {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", parent, err)
			}
		}
		WriteFile(t, full, content)
	}
}

// CheckBamlSrcName reports whether `name` is a safe relative path
// underneath `root`. Returns a non-nil error for absolute paths,
// `.` / `..` segments, or any cleaned path that escapes root.
// Exposed so unit tests can drive the rejection rules directly.
func CheckBamlSrcName(root, name string) error {
	if filepath.IsAbs(name) {
		return fmt.Errorf("absolute file name %q not allowed", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." {
		return fmt.Errorf("invalid file name %q", name)
	}
	full := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("file name %q escapes baml_src root", name)
	}
	return nil
}
