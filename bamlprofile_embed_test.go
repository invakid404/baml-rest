package baml_rest

// de-BAML Slice 7.1a embed guard.
//
// Slice 7.1a wired internal/bamlprofile into the serving path
// (nativeserve/admission -> internal/nativeprompt -> internal/bamlprofile) and
// narrowed .embedignore from the whole tree to only
// internal/bamlprofile/profileoracle. The customer/container build materializes
// the server from this embed.FS (cmd/build/main.go copies bamlrest.Sources into
// the Docker build context), so a missing production file is not a cosmetic
// manifest issue — it is a container that cannot compile.
//
// This guard derives its expectations from the LIVE tree rather than a checked-in
// list, so adding a bamlprofile file without re-running `go run ./cmd/embed`
// fails here instead of at a customer build.
//
// It runs in the default, CGO-free `go test`.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	profileDir   = "internal/bamlprofile"
	oracleSubdir = "internal/bamlprofile/profileoracle"
	promptDir    = "internal/nativeprompt"
)

// embeddedRootPaths returns the set of slash-separated paths present in the root
// module's embed.FS.
func embeddedRootPaths(t *testing.T) map[string]bool {
	t.Helper()
	root, ok := Sources["."]
	if !ok {
		t.Fatal(`Sources["."] is missing; the root embed manifest is not registered`)
	}
	paths := map[string]bool{}
	if err := fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths[path] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walking the root embed.FS: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("the root embed.FS is empty; this guard would be vacuous")
	}
	return paths
}

// productionGoFiles lists the non-test .go files under dir (recursively),
// relative to the repo root and slash-separated, skipping the given subtrees.
func productionGoFiles(t *testing.T, dir string, skip ...string) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		slashed := filepath.ToSlash(rel)
		if d.IsDir() {
			for _, s := range skip {
				if slashed == s {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(slashed, ".go") && !strings.HasSuffix(slashed, "_test.go") {
			out = append(out, slashed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

// TestProductionBamlprofileIsEmbedded asserts every production bamlprofile file
// entered the embed graph. Deriving the want-list from disk is what makes this a
// drift guard rather than a restatement of the manifest.
func TestProductionBamlprofileIsEmbedded(t *testing.T) {
	embedded := embeddedRootPaths(t)

	want := productionGoFiles(t, profileDir, oracleSubdir)
	if len(want) == 0 {
		t.Fatalf("no production .go files found under %s; the guard would be vacuous", profileDir)
	}
	for _, f := range want {
		if !embedded[f] {
			t.Errorf("production file %s is NOT in the root embed.FS; run `go run ./cmd/embed`", f)
		}
	}

	// The nativeprompt -> bamlprofile edge only helps a customer build if BOTH
	// ends are embedded. nativeprompt already was; pin it so a future manifest
	// regression cannot half-break the path.
	for _, f := range productionGoFiles(t, promptDir, promptDir+"/staticoracle",
		promptDir+"/staticservefixture", promptDir+"/testdata") {
		if !embedded[f] {
			t.Errorf("production file %s is NOT in the root embed.FS; run `go run ./cmd/embed`", f)
		}
	}
}

// TestProfileOracleIsNotEmbedded is the other half of the narrowed .embedignore
// entry: the test-only stock-BAML differential oracle, its fixture material, and
// every bamlprofile _test.go must stay OUT of the shipped source bundle.
// profileoracle's integration test links github.com/boundaryml/baml, so
// embedding it would make the server binary ship a second BAML runtime.
func TestProfileOracleIsNotEmbedded(t *testing.T) {
	embedded := embeddedRootPaths(t)

	sawProfileFile := false
	for path := range embedded {
		if !strings.HasPrefix(path, profileDir+"/") {
			continue
		}
		sawProfileFile = true
		if strings.HasPrefix(path, oracleSubdir+"/") || path == oracleSubdir {
			t.Errorf("test-only oracle file %s is embedded; .embedignore must exclude %s", path, oracleSubdir)
		}
		if strings.HasSuffix(path, "_test.go") {
			t.Errorf("test file %s is embedded; .embedignore excludes **/*_test.go", path)
		}
	}
	if !sawProfileFile {
		t.Fatalf("no %s file is embedded at all; TestProductionBamlprofileIsEmbedded should have caught this", profileDir)
	}

	// Non-vacuity: the oracle really does exist on disk, so "nothing embedded
	// from it" is a decision rather than an empty directory.
	if _, err := os.Stat(filepath.Join(repoRoot(t), oracleSubdir)); err != nil {
		t.Fatalf("%s not found on disk (%v); this exclusion guard would be vacuous", oracleSubdir, err)
	}
}
