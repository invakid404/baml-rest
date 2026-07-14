// Package nativeworkersrc packages the out-of-go.work
// internal/nativebody/nanollmprepare module's source into a single
// deterministic tar so the production builder can carry it as an OPAQUE build
// asset (de-BAML cutover Slice 2).
//
// Why an opaque tar and not the normal embed path: nanollmprepare is a separate
// Go module (its own go.mod, out of go.work) that requires nanollm-ffi. It is
// deliberately excluded from the root embed manifest (.embedignore) — if
// cmd/embed discovered it, it would add a nanollmprepare *import* to the root
// embed.go, dragging the module (and transitively nanollm) into the root link
// graph. `//go:embed` also refuses to cross a module boundary ("in different
// module"). So the only way to ship the source without importing/linking it is
// as opaque bytes: this tar is committed at TarRelPath, embedded via the
// already-embedded cmd/build directory, laid into the build context by
// cmd/build like any other source file, and extracted by build.sh only when
// NATIVE_WORKER=true. The isolation boundary stays at the BUILD/link level (the
// host never imports nanollm); the source bytes riding along in the bundle are
// inert.
//
// The tar is deterministic (sorted entries, fixed epoch mtime, fixed mode) so a
// freshness test can byte-compare it against the live module tree.
package nativeworkersrc

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// ModuleRelPath is the isolated module's path relative to the repo root.
	// Tar entries are prefixed with it so extraction at the repo root drops the
	// module back into place.
	ModuleRelPath = "internal/nativebody/nanollmprepare"

	// TarRelPath is where the committed opaque tar lives relative to the repo
	// root. It sits under cmd/build (already embedded), so it ships in the
	// standard source bundle without any change to the embed manifest.
	TarRelPath = "cmd/build/nativeworker_module.tar"
)

// fixedModTime is the single timestamp stamped on every tar entry so the
// archive is byte-reproducible regardless of the checkout's file mtimes.
var fixedModTime = time.Unix(0, 0).UTC()

// includeFile reports whether a module-relative file path belongs in the
// packaged worker source. It is an explicit ALLOWLIST — NOT a deny-only filter —
// because the resulting tar is an opaque, embedded artifact later extracted into
// the isolated worker build: only what `go build ./cmd/worker` needs may ship,
// so a stray secret/.env/editor-artifact/fixture/non-Go file cannot ride along.
// The allowed set is exactly the module manifests (go.mod, go.sum) and non-test
// Go sources (*.go except *_test.go — the gated Phase-5/6 oracle + boot smoke
// tests are not part of the customer build).
func includeFile(relPath string) bool {
	base := relPath
	if i := strings.LastIndexByte(relPath, '/'); i >= 0 {
		base = relPath[i+1:]
	}
	switch base {
	case "go.mod", "go.sum":
		return true
	}
	return strings.HasSuffix(base, ".go") && !strings.HasSuffix(base, "_test.go")
}

// collectFiles returns the sorted set of module-relative file paths to package,
// rooted at moduleDir (an absolute or CWD-relative path to the module).
func collectFiles(moduleDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(moduleDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// No nested modules live inside nanollmprepare, so there is nothing
			// to prune here; keep walking.
			return nil
		}
		rel, err := filepath.Rel(moduleDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if includeFile(rel) {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// BuildTar packages the isolated module rooted at repoRoot into a deterministic
// tar. Entry names are ModuleRelPath + "/" + <module-relative path>, so `tar -xf`
// at the repo root restores the module in place.
func BuildTar(repoRoot string) ([]byte, error) {
	moduleDir := filepath.Join(repoRoot, filepath.FromSlash(ModuleRelPath))
	files, err := collectFiles(moduleDir)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(moduleDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		hdr := &tar.Header{
			Name:    ModuleRelPath + "/" + rel,
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: fixedModTime,
			Format:  tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
