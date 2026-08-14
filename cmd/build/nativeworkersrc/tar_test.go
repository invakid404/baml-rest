package nativeworkersrc

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
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

// TestBuildTarExcludesGatedTestSupport proves the de-BAML Slice 7.2c-3 refinement of
// the allowlist: a NON-test Go file under a gated test-support subtree still does not
// ship, while an ordinary non-test Go file at the same depth does.
//
// The refinement was forced by the cutover's live proof, which had to be split across
// six test binaries and therefore needed a shared harness in ordinary `.go` files
// (Go cannot import another package's test files). Before that, `staticserve` held only
// `_test.go` files and the subtree was excluded by accident; the rule now says so
// explicitly, and this is what keeps it from being either forgotten or over-applied.
//
// Both directions are driven. A rule that excluded everything would pass a
// one-directional test.
func TestBuildTarExcludesGatedTestSupport(t *testing.T) {
	if len(gatedTestSupportPrefixes) == 0 {
		t.Fatal("no gated test-support prefix is declared; this test would be vacuous")
	}
	root := t.TempDir()
	mod := filepath.Join(root, filepath.FromSlash(ModuleRelPath))
	writeAt(t, filepath.Join(mod, "go.mod"), "module m\n\ngo 1.26.5\n")
	writeAt(t, filepath.Join(mod, "go.sum"), "")

	// The gated subtree: NON-test Go files that must still be excluded.
	writeAt(t, filepath.Join(mod, "staticserve", "opharness", "harness.go"), "package opharness\n")
	writeAt(t, filepath.Join(mod, "staticserve", "opharness", "rows.go"), "package opharness\n")
	writeAt(t, filepath.Join(mod, "staticserve", "helper.go"), "package staticserve\n")

	// A sibling that must STILL ship, at the same depth — otherwise "excluded" could
	// be true for the wrong reason (a prefix that swallowed the whole module).
	writeAt(t, filepath.Join(mod, "staticserveutil", "util.go"), "package staticserveutil\n")
	writeAt(t, filepath.Join(mod, "cmd", "worker", "main.go"), "package main\n")

	data, err := BuildTarForModules(root, []string{ModuleRelPath})
	if err != nil {
		t.Fatalf("BuildTarForModules: %v", err)
	}
	names := tarEntryNames(t, data)
	for _, deny := range []string{
		"staticserve/opharness/harness.go", "staticserve/opharness/rows.go", "staticserve/helper.go",
	} {
		if names[deny] {
			t.Errorf("gated test-support file %q was shipped in the opaque worker tar", deny)
		}
	}
	for _, want := range []string{"go.mod", "go.sum", "cmd/worker/main.go", "staticserveutil/util.go"} {
		if !names[want] {
			t.Errorf("expected allowlisted file %q in tar, missing — the exclusion prefix is matching "+
				"more than the named subtree", want)
		}
	}
}

// TestGatedTestSupportPrefixesMatchTheLiveTree keeps the exclusion honest against the
// real module rather than against a fabricated one.
//
// A prefix that no longer names an existing directory would be a rule with nothing
// behind it, and a subtree that stopped being test-only would be shipped-by-omission —
// so every declared prefix must exist in the live module AND must contain only files
// whose build constraint keeps them out of an ordinary build.
func TestGatedTestSupportPrefixesMatchTheLiveTree(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	for _, prefix := range gatedTestSupportPrefixes {
		dir := filepath.Join(repoRoot, filepath.FromSlash(ModuleRelPath), filepath.FromSlash(prefix))
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("gated test-support prefix %q names %s, which does not exist: %v", prefix, dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("gated test-support prefix %q names a file, not a directory", prefix)
			continue
		}
		nonTestGo := 0
		err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			nonTestGo++
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			// Every non-test file under the excluded subtree must be gated out of an
			// ordinary build. Without this, a file that genuinely belonged in the
			// worker could be added here and silently stop shipping.
			if !strings.Contains(string(b), "//go:build integration && nanollm_integration") {
				t.Errorf("%s is under the excluded subtree %q but is NOT gated by "+
					"`//go:build integration && nanollm_integration`; it would silently stop shipping",
					path, prefix)
			}
			return nil
		})
		if err != nil {
			t.Errorf("walking %s: %v", dir, err)
		}
		if nonTestGo == 0 {
			t.Errorf("the excluded subtree %q contains no non-test Go file, so the exclusion is "+
				"redundant with the `!_test.go` rule and should be removed", prefix)
		}
	}
}
