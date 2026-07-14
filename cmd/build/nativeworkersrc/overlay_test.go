package nativeworkersrc

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/modfile"
)

// writeGoMod writes a go.mod at dir/go.mod, creating dir.
func writeGoMod(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// parseResult reads back the overlaid module go.mod.
func parseResult(t *testing.T, moduleDir string) *modfile.File {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mf, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return mf
}

func hasReplace(mf *modfile.File, old string) (*modfile.Replace, bool) {
	for _, r := range mf.Replace {
		if r.Old.Path == old {
			return r, true
		}
	}
	return nil, false
}

func hasRequire(mf *modfile.File, path string) bool {
	for _, r := range mf.Require {
		if r.Mod.Path == path {
			return true
		}
	}
	return false
}

// setupContext lays out a synthetic build context: a module dir with a go.mod
// carrying present-target and missing-target filesystem replaces, plus a
// generated baml_client and the "present" sibling dirs.
func setupContext(t *testing.T) (root, moduleDir string) {
	t.Helper()
	root = t.TempDir()
	moduleDir = filepath.Join(root, "internal", "nativebody", "nanollmprepare")

	// Present targets (relative to moduleDir via ../../../<name>).
	for _, name := range []string{"bamlutils", "worker", filepath.Join("adapters", "adapter_v0_219_0"), "baml_client"} {
		writeGoMod(t, filepath.Join(root, name), "module github.com/invakid404/baml-rest/"+filepath.ToSlash(name)+"\n\ngo 1.26.5\n")
	}
	// Root module itself (../../../ target).
	writeGoMod(t, root, "module github.com/invakid404/baml-rest\n\ngo 1.26.5\n")

	// Isolated module go.mod: keep present replaces, plus dynclient + an
	// unselected adapter whose dirs are ABSENT (simulating the trimmed bundle).
	writeGoMod(t, moduleDir, `module github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare

go 1.26.5

require (
	github.com/boundaryml/baml v0.223.0
	github.com/invakid404/baml-rest v0.0.48
	github.com/invakid404/baml-rest/bamlutils v0.0.48
	github.com/invakid404/baml-rest/worker v0.0.48
	github.com/invakid404/baml-rest/adapters/adapter_v0_204_0 v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/adapters/adapter_v0_219_0 v0.0.48
	github.com/invakid404/baml-rest/dynclient v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/dynclient/baml-patched v0.0.48
)

replace (
	github.com/invakid404/baml-rest => ../../../
	github.com/invakid404/baml-rest/bamlutils => ../../../bamlutils
	github.com/invakid404/baml-rest/worker => ../../../worker
	github.com/invakid404/baml-rest/adapters/adapter_v0_204_0 => ../../../adapters/adapter_v0_204_0
	github.com/invakid404/baml-rest/adapters/adapter_v0_219_0 => ../../../adapters/adapter_v0_219_0
	github.com/invakid404/baml-rest/dynclient => ../../../dynclient
	github.com/invakid404/baml-rest/dynclient/baml-patched => ../../../dynclient/baml-patched
)
`)
	return root, moduleDir
}

func TestApplyOverlayDefaultBAML(t *testing.T) {
	_, moduleDir := setupContext(t)

	if err := ApplyOverlay(moduleDir, OverlayOptions{
		BAMLClientPath: "../../../baml_client",
		BAMLVersion:    "v0.223.0",
	}); err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	mf := parseResult(t, moduleDir)

	// baml_client wired in as a local require + replace.
	if !hasRequire(mf, BAMLClientModulePath) {
		t.Error("baml_client require missing")
	}
	if r, ok := hasReplace(mf, BAMLClientModulePath); !ok {
		t.Error("baml_client replace missing")
	} else if r.New.Path != "../../../baml_client" || r.New.Version != "" {
		t.Errorf("baml_client replace = %q@%q, want ../../../baml_client (local)", r.New.Path, r.New.Version)
	}

	// Missing-target replaces (dynclient, baml-patched, unselected adapter) dropped.
	for _, gone := range []string{
		"github.com/invakid404/baml-rest/dynclient",
		"github.com/invakid404/baml-rest/dynclient/baml-patched",
		"github.com/invakid404/baml-rest/adapters/adapter_v0_204_0",
	} {
		if _, ok := hasReplace(mf, gone); ok {
			t.Errorf("expected replace for missing target %s to be dropped", gone)
		}
		if hasRequire(mf, gone) {
			t.Errorf("expected require for missing target %s to be dropped", gone)
		}
	}

	// Present-target replaces (root, bamlutils, worker, selected adapter) kept.
	for _, kept := range []string{
		"github.com/invakid404/baml-rest",
		"github.com/invakid404/baml-rest/bamlutils",
		"github.com/invakid404/baml-rest/worker",
		"github.com/invakid404/baml-rest/adapters/adapter_v0_219_0",
	} {
		if _, ok := hasReplace(mf, kept); !ok {
			t.Errorf("expected replace for present target %s to be kept", kept)
		}
	}

	// No custom replace for stock BAML; version stays aligned.
	if r, ok := hasReplace(mf, bamlModulePath); ok {
		t.Errorf("unexpected BAML replace in default build: => %s", r.New.Path)
	}
}

func TestApplyOverlayCustomBAML(t *testing.T) {
	root, moduleDir := setupContext(t)
	// A custom BAML Go lib dir in the context.
	customLib := filepath.Join(root, "custom_baml_go_lib")
	writeGoMod(t, customLib, "module github.com/boundaryml/baml\n\ngo 1.26.5\n")

	if err := ApplyOverlay(moduleDir, OverlayOptions{
		BAMLClientPath:    "../../../baml_client",
		BAMLVersion:       "v0.999.0", // must be ignored in favour of the custom replace
		CustomBAMLLibPath: customLib,
	}); err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	mf := parseResult(t, moduleDir)

	r, ok := hasReplace(mf, bamlModulePath)
	if !ok {
		t.Fatal("expected a custom BAML replace")
	}
	if r.New.Path != customLib || r.New.Version != "" {
		t.Errorf("BAML replace = %q@%q, want %s (local)", r.New.Path, r.New.Version, customLib)
	}
}

func TestApplyOverlayMissingClientFails(t *testing.T) {
	_, moduleDir := setupContext(t)
	if err := ApplyOverlay(moduleDir, OverlayOptions{
		BAMLClientPath: "../../../nonexistent_client",
		BAMLVersion:    "v0.223.0",
	}); err == nil {
		t.Fatal("expected ApplyOverlay to fail when baml_client is absent")
	}
}

// TestApplyOverlayNonENOENTStatErrorReturns proves a replace target that is
// unstattable for a reason OTHER than absence is surfaced as an error, not
// silently dropped (which would corrupt the worker module graph). A replace
// target whose parent is a regular file yields ENOTDIR (not ENOENT).
func TestApplyOverlayNonENOENTStatErrorReturns(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, "internal", "nativebody", "nanollmprepare")

	// baml_client must exist so we reach the replace scan, not the early guard.
	writeGoMod(t, filepath.Join(root, "baml_client"),
		"module github.com/invakid404/baml-rest/baml_client\n\ngo 1.26.5\n")
	// A regular file that a replace target sits UNDER: os.Stat("<file>/child")
	// returns ENOTDIR, which os.IsNotExist rejects.
	if err := os.WriteFile(filepath.Join(root, "blocker"), []byte("i am a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, moduleDir, `module github.com/invakid404/baml-rest/internal/nativebody/nanollmprepare

go 1.26.5

require github.com/example/enotdir v0.0.0-00010101000000-000000000000

replace github.com/example/enotdir => ../../../blocker/child
`)

	err := ApplyOverlay(moduleDir, OverlayOptions{
		BAMLClientPath: "../../../baml_client",
		BAMLVersion:    "v0.223.0",
	})
	if err == nil {
		t.Fatal("expected ApplyOverlay to return the non-ENOENT stat error, got nil")
	}

	// ApplyOverlay returned before writing, so the still-valid replace must not
	// have been dropped from the on-disk go.mod.
	mf := parseResult(t, moduleDir)
	if _, ok := hasReplace(mf, "github.com/example/enotdir"); !ok {
		t.Error("overlay must not drop a replace on a non-ENOENT stat error")
	}
}
