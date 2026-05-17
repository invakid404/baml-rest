package hacks

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
)

// upstreamBAMLModulePath is the module path of upstream BAML that every
// internal Go import inside a patched fork must be rewritten away from.
const upstreamBAMLModulePath = "github.com/boundaryml/baml"

// patchedModuleSubtree enumerates the relative paths copied out of the
// upstream BAML module into a patched fork. The subtree is large enough
// to keep the cgo wrapper, headers, and helper tests intact, but skips
// every directory that has no bearing on a Go-only consumer (docs, the
// Rust crates, the TypeScript packages, the generators tree, etc.).
//
// engine/language_client_go is the load-bearing piece; root LICENSE
// and go.sum come along so the resulting module preserves attribution
// and the module's transitive sum data.
var patchedModuleSubtree = []string{
	"LICENSE",
	"go.sum",
	"engine/language_client_go",
}

// GeneratePatchedBAMLModule produces a checked-in patched BAML fork at
// outDir from the upstream sources rooted at srcDir. The fork's
// `go.mod` declares modulePath, every internal `github.com/boundaryml/baml`
// import is rewritten to the new path, and the PR #3185 deadlock-fix
// patch (selected by version) is applied to the copied tree. Marker
// files in outDir record the source version, the embedded patch's
// checksum, and the rewrite target so re-runs can diff against the
// recorded provenance.
//
// outDir is removed and recreated each invocation: upstream may delete
// or rename files between BAML releases, and a stale copy of those
// files would silently linger inside the fork otherwise.
func GeneratePatchedBAMLModule(srcDir, outDir, modulePath, version string) error {
	srcDir = strings.TrimSpace(srcDir)
	outDir = strings.TrimSpace(outDir)
	modulePath = strings.TrimSpace(modulePath)
	version = strings.TrimSpace(version)
	if srcDir == "" {
		return fmt.Errorf("source directory is required to generate the patched BAML module")
	}
	if outDir == "" {
		return fmt.Errorf("output directory is required to generate the patched BAML module")
	}
	if modulePath == "" {
		return fmt.Errorf("patched module path is required")
	}
	if version == "" {
		return fmt.Errorf("baml version is required to record provenance and select the deadlock-fix patch")
	}
	normalizedVersion := bamlutils.NormalizeVersion(version)

	if err := os.RemoveAll(outDir); err != nil {
		return fmt.Errorf("removing existing patched module dir: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating patched module dir: %w", err)
	}

	for _, rel := range patchedModuleSubtree {
		srcPath := filepath.Join(srcDir, rel)
		dstPath := filepath.Join(outDir, rel)
		info, err := os.Lstat(srcPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspecting upstream %s: %w", rel, err)
		}
		if info.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return fmt.Errorf("copying upstream %s: %w", rel, err)
			}
			continue
		}
		if err := copyFile(srcPath, dstPath, info.Mode()); err != nil {
			return fmt.Errorf("copying upstream %s: %w", rel, err)
		}
	}

	if err := writePatchedGoMod(srcDir, outDir, modulePath); err != nil {
		return err
	}

	if err := rewriteBAMLImportsInTree(outDir, upstreamBAMLModulePath, modulePath); err != nil {
		return err
	}

	if err := ApplyRuntimeDeadlockFixToDir(normalizedVersion, outDir); err != nil {
		return err
	}

	if err := writePatchedProvenance(outDir, normalizedVersion, modulePath); err != nil {
		return err
	}

	fmt.Printf("  Generated patched BAML module at %s (version %s, module path %s)\n", outDir, normalizedVersion, modulePath)
	return nil
}

// writePatchedGoMod copies the upstream module's go.mod (used only to
// preserve the require / replace block shape) and rewrites the leading
// `module` directive to modulePath. The Go version line and every
// other directive carry through unchanged.
func writePatchedGoMod(srcDir, outDir, modulePath string) error {
	upstream, err := os.ReadFile(filepath.Join(srcDir, "go.mod"))
	if err != nil {
		return fmt.Errorf("reading upstream go.mod: %w", err)
	}
	lines := strings.Split(string(upstream), "\n")
	rewritten := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "module ") {
			lines[i] = "module " + modulePath
			rewritten = true
			break
		}
	}
	if !rewritten {
		return fmt.Errorf("upstream go.mod has no module directive to rewrite")
	}
	contents := strings.Join(lines, "\n")
	if err := os.WriteFile(filepath.Join(outDir, "go.mod"), []byte(contents), 0o644); err != nil {
		return fmt.Errorf("writing patched go.mod: %w", err)
	}
	return nil
}

// rewriteBAMLImportsInTree walks every .go file under root and rewrites
// import paths that begin with oldModule (or are exactly oldModule) to
// the new module path. The Go AST is parsed and re-printed so that
// comments and formatting are preserved alongside the rewritten
// import string literals.
func rewriteBAMLImportsInTree(root, oldModule, newModule string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		return rewriteBAMLImportsInFile(path, oldModule, newModule)
	})
}

func rewriteBAMLImportsInFile(path, oldModule, newModule string) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	changed := false
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		raw, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		var rewritten string
		switch {
		case raw == oldModule:
			rewritten = newModule
		case strings.HasPrefix(raw, oldModule+"/"):
			rewritten = newModule + strings.TrimPrefix(raw, oldModule)
		default:
			continue
		}
		imp.Path.Value = strconv.Quote(rewritten)
		changed = true
	}
	if !changed {
		return nil
	}
	ast.SortImports(fset, file)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-baml-rewrite-*.go")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := printer.Fprint(tmp, fset, file); err != nil {
		tmp.Close()
		return fmt.Errorf("writing rewritten %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming rewritten %s into place: %w", path, err)
	}
	return nil
}

// writePatchedProvenance records the BAML version, patched module
// path, and the deadlock-fix patch sha256 so a re-run of
// GeneratePatchedBAMLModule can be diffed against the previous output
// without re-reading upstream. Versions at or above
// pr3185UpstreamMergedFloor ship the deadlock fix natively, so no
// patch is recorded and PATCHES.md notes the upstream provenance
// instead.
func writePatchedProvenance(outDir, version, modulePath string) error {
	versionPath := filepath.Join(outDir, "BAML_REST_SOURCE_VERSION")

	var (
		patchPath string
		checksum  string
	)
	upstreamMerged := bamlutils.CompareVersions(version, pr3185UpstreamMergedFloor) >= 0
	if !upstreamMerged {
		patchPath = pr3185PatchV218BackportPath
		if bamlutils.CompareVersions(version, "v0.219.0") >= 0 {
			patchPath = pr3185PatchV219Path
		}
		patchData, err := readEmbeddedPatch(patchPath)
		if err != nil {
			return err
		}
		checksum = fmt.Sprintf("%x", sha256.Sum256(patchData))
	}

	patchField := "none (PR #3185 merged upstream)"
	if !upstreamMerged {
		patchField = patchPath
	}
	checksumField := "none"
	if !upstreamMerged {
		checksumField = checksum
	}
	versionBody := fmt.Sprintf(
		"upstream_module=%s\nupstream_version=%s\nmodule_path=%s\npatch=%s\npatch_sha256=%s\n",
		upstreamBAMLModulePath,
		version,
		modulePath,
		patchField,
		checksumField,
	)
	if err := os.WriteFile(versionPath, []byte(versionBody), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(versionPath), err)
	}

	var patchesBody string
	if upstreamMerged {
		patchesBody = strings.Join([]string{
			"# Patches applied to this BAML fork",
			"",
			"This directory holds a copy of `" + upstreamBAMLModulePath + "` (version " + version + ")",
			"rewritten to module path `" + modulePath + "`. PR #3185 (runtime",
			"deadlock fix) is merged upstream at or above " + pr3185UpstreamMergedFloor + ", so no",
			"embedded patch is applied to this tree.",
			"",
			"- Internal `" + upstreamBAMLModulePath + "/...` imports are rewritten to `" + modulePath + "/...`.",
			"",
			"Generated by cmd/hacks (see `GeneratePatchedBAMLModule`).",
			"",
		}, "\n")
	} else {
		patchesBody = strings.Join([]string{
			"# Patches applied to this BAML fork",
			"",
			"This directory holds a copy of `" + upstreamBAMLModulePath + "` (version " + version + ")",
			"rewritten to module path `" + modulePath + "` and patched for the",
			"runtime-deadlock fix backported from BoundaryML/baml PR #3185.",
			"",
			"- `" + filepath.Base(patchPath) + "` — sha256 " + checksum + ".",
			"- Internal `" + upstreamBAMLModulePath + "/...` imports are rewritten to `" + modulePath + "/...`.",
			"",
			"Generated by cmd/hacks (see `GeneratePatchedBAMLModule`).",
			"",
		}, "\n")
	}
	if err := os.WriteFile(filepath.Join(outDir, "PATCHES.md"), []byte(patchesBody), 0o644); err != nil {
		return fmt.Errorf("writing PATCHES.md: %w", err)
	}
	return nil
}
