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
	// ModuleRelPath is the isolated nanollm WORKER module's path relative to the
	// repo root — the module whose ./cmd/worker is the native-serve binary. It is
	// the first entry of ModuleRelPaths and is kept as a named constant because the
	// generator / freshness test / build.sh overlay validate against it directly.
	ModuleRelPath = "internal/nativebody/nanollmprepare"

	// NativeServeModuleRelPath is the PUBLIC nanollm-linked serve-core module the
	// worker imports (de-BAML #624). It is out-of-go.work + .embedignore'd (like the
	// worker module) so it never enters the root/host link graph, so it too must
	// ride along in the opaque tar for the isolated worker build to resolve the
	// `=> ../../../nativeserve` replace.
	NativeServeModuleRelPath = "nativeserve"

	// TarRelPath is where the committed opaque tar lives relative to the repo
	// root. It sits under cmd/build (already embedded), so it ships in the
	// standard source bundle without any change to the embed manifest.
	TarRelPath = "cmd/build/nativeworker_module.tar"
)

// ModuleRelPaths lists EVERY out-of-go.work module packaged into the opaque worker
// tar, in a fixed order. The isolated worker module (cmd/worker) plus every sibling
// module it links that is excluded from the root embed bundle (the nanollm serve
// core) must all be restored into the build context so the isolated GOWORK=off
// build resolves their filesystem replaces. Entry names are prefixed with each
// module's rel path, so `tar -xf` at the repo root drops every module back in place.
var ModuleRelPaths = []string{
	ModuleRelPath,
	NativeServeModuleRelPath,
}

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

// BuildTar packages every module in ModuleRelPaths rooted at repoRoot into a
// single deterministic tar. Entry names are <moduleRelPath> + "/" +
// <module-relative path>, so `tar -xf` at the repo root restores every module in
// place.
func BuildTar(repoRoot string) ([]byte, error) {
	return BuildTarForModules(repoRoot, ModuleRelPaths)
}

// tarEntry is one file destined for the opaque tar: its full (module-prefixed)
// entry name and the absolute source path to read.
type tarEntry struct {
	name    string
	absPath string
}

// BuildTarForModules packages the given modules (each a repo-root-relative module
// path) into a single deterministic tar. Entries across all modules are sorted by
// their full entry name so the archive is byte-reproducible regardless of module
// order or checkout mtimes. Exposed (beyond BuildTar) so unit tests can exercise a
// single synthetic module without materializing every production module.
func BuildTarForModules(repoRoot string, modules []string) ([]byte, error) {
	var entries []tarEntry
	for _, modRel := range modules {
		moduleDir := filepath.Join(repoRoot, filepath.FromSlash(modRel))
		files, err := collectFiles(moduleDir)
		if err != nil {
			return nil, err
		}
		for _, rel := range files {
			entries = append(entries, tarEntry{
				name:    modRel + "/" + rel,
				absPath: filepath.Join(moduleDir, filepath.FromSlash(rel)),
			})
		}
	}
	// Global sort by entry name keeps the archive byte-identical regardless of the
	// module ordering passed in.
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		data, err := os.ReadFile(e.absPath)
		if err != nil {
			return nil, err
		}
		hdr := &tar.Header{
			Name:    e.name,
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
