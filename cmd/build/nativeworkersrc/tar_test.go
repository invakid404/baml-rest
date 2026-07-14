package nativeworkersrc

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntryNames returns the module-relative names carried by a BuildTar archive
// (the ModuleRelPath prefix stripped).
func tarEntryNames(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		rel := strings.TrimPrefix(h.Name, ModuleRelPath+"/")
		names[rel] = true
	}
	return names
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBuildTarExcludesUnrelatedFiles proves the packaging allowlist ships ONLY
// go.mod/go.sum + non-test *.go, so a planted secret / stray non-Go file / test
// file / dotfile in the module tree cannot ride along in the opaque builder
// artifact.
func TestBuildTarExcludesUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	mod := filepath.Join(root, filepath.FromSlash(ModuleRelPath))

	// Allowed.
	writeAt(t, filepath.Join(mod, "go.mod"), "module m\n\ngo 1.26.5\n")
	writeAt(t, filepath.Join(mod, "go.sum"), "")
	writeAt(t, filepath.Join(mod, "doc.go"), "package m\n")
	writeAt(t, filepath.Join(mod, "sub", "thing.go"), "package sub\n")

	// Must be excluded.
	writeAt(t, filepath.Join(mod, "thing_test.go"), "package m\n")
	writeAt(t, filepath.Join(mod, "secret.env"), "TOKEN=super-secret\n")
	writeAt(t, filepath.Join(mod, "notes.txt"), "stray note\n")
	writeAt(t, filepath.Join(mod, ".hidden"), "dotfile\n")
	writeAt(t, filepath.Join(mod, "sub", "fixture.json"), "{}\n")
	writeAt(t, filepath.Join(mod, "go.mod.bak"), "editor artifact\n")

	data, err := BuildTar(root)
	if err != nil {
		t.Fatalf("BuildTar: %v", err)
	}
	names := tarEntryNames(t, data)

	for _, want := range []string{"go.mod", "go.sum", "doc.go", "sub/thing.go"} {
		if !names[want] {
			t.Errorf("expected allowlisted file %q in tar, missing", want)
		}
	}
	for _, deny := range []string{"thing_test.go", "secret.env", "notes.txt", ".hidden", "sub/fixture.json", "go.mod.bak"} {
		if names[deny] {
			t.Errorf("non-allowlisted file %q was shipped in the opaque worker tar", deny)
		}
	}
}
