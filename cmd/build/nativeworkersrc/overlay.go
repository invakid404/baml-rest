package nativeworkersrc

// Build-time go.mod overlay for the isolated nanollm worker module
// (de-BAML cutover Slice 2, second review fix).
//
// The isolated internal/nativebody/nanollmprepare module is built with
// GOWORK=off, so it does NOT see the builder's go.work — which is where the
// real builder exposes the freshly generated ./baml_client module
// (`go work use ./baml_client`) and any custom/selected BAML replacement
// (`go work edit -replace github.com/boundaryml/baml=...`). Under GOWORK=off the
// isolated module's OWN go.mod is authoritative, so it must carry those itself
// or the worker cannot resolve the generated client (root's generated
// InitBamlRuntime imports github.com/invakid404/baml-rest/baml_client, a nested
// generated module the root-module replacements cannot cross) and would build
// against the wrong BAML.
//
// ApplyOverlay mutates ONLY the extracted (throwaway) copy of the isolated
// module's go.mod in the build context. It never touches root go.mod/go.work, so
// the host link graph stays zero-nanollm / CGO-free. It:
//
//  1. Drops every filesystem-path replace whose target is missing in this build
//     context (and its matching require). The server bundle strips dynclient and
//     the unselected adapters, so their `=> ../../../<dir>` replaces would
//     otherwise point at absent directories and fail module-graph loading. This
//     is detection-based (missing target), so it needs no knowledge of which
//     adapter was selected or whether dynclient is present.
//  2. Adds a local require+replace for the generated baml_client so the worker
//     resolves the freshly generated client across the nested-module boundary.
//  3. Aligns github.com/boundaryml/baml with the build's selected version, or
//     replaces it with the custom Go library path (--custom-baml-lib /
//     --baml-source builds), mirroring the workspace's custom-BAML replacement.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

// BAMLClientModulePath is the module path of the builder-generated client.
const BAMLClientModulePath = "github.com/invakid404/baml-rest/baml_client"

// bamlModulePath is the stock BAML Go module the generated client depends on.
const bamlModulePath = "github.com/boundaryml/baml"

// OverlayOptions configures ApplyOverlay.
type OverlayOptions struct {
	// BAMLClientPath is the generated baml_client directory, as a replace target
	// relative to the module dir (e.g. "../../../baml_client") or absolute. It
	// MUST exist — the real builder always generates it before the native worker
	// build.
	BAMLClientPath string
	// BAMLVersion is the build's selected stock-BAML version (e.g. "v0.223.0").
	// Applied as the isolated module's github.com/boundaryml/baml require so the
	// worker compiles the generated client against the same BAML the rest of the
	// build used. Ignored when CustomBAMLLibPath is set.
	BAMLVersion string
	// CustomBAMLLibPath, when non-empty, is the local Go library replacing
	// github.com/boundaryml/baml (a --custom-baml-go-lib / --baml-source build),
	// as a path relative to the module dir or absolute. It mirrors the builder's
	// `go work edit -replace github.com/boundaryml/baml=...`.
	CustomBAMLLibPath string
}

// isFilesystemReplace reports whether a modfile replacement targets a local
// directory (New.Version == "" and a non-empty path) rather than another module
// version. Only filesystem replaces can dangle when the build context trims a
// directory.
func isFilesystemReplace(r *modfile.Replace) bool {
	return r.New.Version == "" && r.New.Path != ""
}

// ApplyOverlay rewrites moduleDir/go.mod in place per OverlayOptions.
func ApplyOverlay(moduleDir string, opts OverlayOptions) error {
	if opts.BAMLClientPath == "" {
		return fmt.Errorf("nativeworkersrc: BAMLClientPath is required")
	}
	// The generated client must exist; the worker's transitive import of it is
	// the whole reason for the overlay. Fail loudly rather than writing a replace
	// to a phantom directory.
	clientTarget := opts.BAMLClientPath
	if !filepath.IsAbs(clientTarget) {
		clientTarget = filepath.Join(moduleDir, clientTarget)
	}
	if _, err := os.Stat(filepath.Join(clientTarget, "go.mod")); err != nil {
		return fmt.Errorf("nativeworkersrc: generated baml_client not found at %s: %w", clientTarget, err)
	}

	gomodPath := filepath.Join(moduleDir, "go.mod")
	data, err := os.ReadFile(gomodPath)
	if err != nil {
		return err
	}
	mf, err := modfile.Parse(gomodPath, data, nil)
	if err != nil {
		return fmt.Errorf("nativeworkersrc: parsing %s: %w", gomodPath, err)
	}

	// (1) Drop filesystem replaces whose target is absent in this build context,
	// plus the matching require. Collect first (mutating while ranging is unsafe).
	// Only a genuinely-absent target (os.IsNotExist) is dropped — a permission or
	// other I/O stat error is NOT evidence of absence, so it is returned rather
	// than silently dropping a still-valid replace and corrupting the module graph.
	var dropMissing []string
	for _, r := range mf.Replace {
		if !isFilesystemReplace(r) {
			continue
		}
		target := r.New.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(moduleDir, target)
		}
		if _, err := os.Stat(target); err != nil {
			if os.IsNotExist(err) {
				dropMissing = append(dropMissing, r.Old.Path)
				continue
			}
			return fmt.Errorf("nativeworkersrc: stat replace target %s for %s: %w", target, r.Old.Path, err)
		}
	}
	for _, modPath := range dropMissing {
		_ = mf.DropReplace(modPath, "")
		_ = mf.DropRequire(modPath)
	}

	// (2) Add the generated baml_client as a local require+replace.
	if err := mf.AddRequire(BAMLClientModulePath, "v0.0.0"); err != nil {
		return fmt.Errorf("nativeworkersrc: adding baml_client require: %w", err)
	}
	if err := mf.AddReplace(BAMLClientModulePath, "", opts.BAMLClientPath, ""); err != nil {
		return fmt.Errorf("nativeworkersrc: adding baml_client replace: %w", err)
	}

	// (3) Align stock BAML with the build's selection.
	if opts.CustomBAMLLibPath != "" {
		if err := mf.AddReplace(bamlModulePath, "", opts.CustomBAMLLibPath, ""); err != nil {
			return fmt.Errorf("nativeworkersrc: adding custom BAML replace: %w", err)
		}
	} else if v := strings.TrimSpace(opts.BAMLVersion); v != "" {
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		if err := mf.AddRequire(bamlModulePath, v); err != nil {
			return fmt.Errorf("nativeworkersrc: aligning BAML version: %w", err)
		}
	}

	mf.Cleanup()
	out, err := mf.Format()
	if err != nil {
		return err
	}
	return os.WriteFile(gomodPath, out, 0o644)
}
