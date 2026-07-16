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
	for full := range tarFullNames(t, data) {
		names[strings.TrimPrefix(full, ModuleRelPath+"/")] = true
	}
	return names
}

// tarFullNames returns the FULL (module-prefixed) entry names carried by an
// archive, without stripping any module prefix — the form that must survive a
// `tar -xf` at the repo root, and the one a multi-module archive must be asserted
// on (each module's files under its own prefix).
func tarFullNames(t *testing.T, data []byte) map[string]bool {
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
		names[h.Name] = true
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

	// Package a single synthetic module (this test fabricates only ModuleRelPath),
	// so drive BuildTarForModules directly rather than BuildTar (which packages the
	// full production module set).
	data, err := BuildTarForModules(root, []string{ModuleRelPath})
	if err != nil {
		t.Fatalf("BuildTarForModules: %v", err)
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

// TestBuildTarForModules_MultiModulePrefixes proves the ModuleRelPaths
// generalization (de-BAML #624): BuildTarForModules packs EVERY module's files
// under its OWN repo-root-relative prefix, so `tar -xf` at the repo root restores
// each module in place. It fabricates BOTH production module paths — the nanollm
// worker module and the nativeserve serve core — and asserts the exact prefixed
// entry names, so a regression that drops a module (or collapses prefixes) fails
// here. The allowlist still applies per module (test files never ship).
func TestBuildTarForModules_MultiModulePrefixes(t *testing.T) {
	root := t.TempDir()
	mods := []string{ModuleRelPath, NativeServeModuleRelPath}
	for _, m := range mods {
		md := filepath.Join(root, filepath.FromSlash(m))
		writeAt(t, filepath.Join(md, "go.mod"), "module m\n\ngo 1.26.5\n")
		writeAt(t, filepath.Join(md, "go.sum"), "")
		writeAt(t, filepath.Join(md, "core.go"), "package m\n")
		writeAt(t, filepath.Join(md, "core_test.go"), "package m\n") // must be excluded
	}

	data, err := BuildTarForModules(root, mods)
	if err != nil {
		t.Fatalf("BuildTarForModules: %v", err)
	}
	names := tarFullNames(t, data)

	for _, want := range []string{
		"internal/nativebody/nanollmprepare/go.mod",
		"internal/nativebody/nanollmprepare/go.sum",
		"internal/nativebody/nanollmprepare/core.go",
		"nativeserve/go.mod",
		"nativeserve/go.sum",
		"nativeserve/core.go",
	} {
		if !names[want] {
			t.Errorf("expected prefixed entry %q in tar, missing", want)
		}
	}
	for _, deny := range []string{
		"internal/nativebody/nanollmprepare/core_test.go",
		"nativeserve/core_test.go",
	} {
		if names[deny] {
			t.Errorf("test file %q must not ship in the opaque worker tar", deny)
		}
	}
}
