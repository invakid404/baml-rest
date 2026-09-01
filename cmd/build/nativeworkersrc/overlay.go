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

// ApplyOverlay rewrites moduleDir/go.mod in place per OverlayOptions. This is the
// BAML (full-worker) overlay mode: it performs the generic missing-replace
// cleanup AND the two BAML operations (add the generated baml_client, select
// github.com/boundaryml/baml). It is MUTUALLY EXCLUSIVE with ApplyNativeOnlyOverlay
// — the native-only worker never links baml_client or BAML, so mixing the two
// would silently inject BAML into an artifact whose whole point is to have none.
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
	mf, err := parseModFile(gomodPath)
	if err != nil {
		return err
	}

	// (1) The generic missing-replace cleanup (shared with the native-only mode).
	if err := dropMissingFilesystemReplaces(mf, moduleDir); err != nil {
		return err
	}

	// (2) BAML operation A — add the generated baml_client as a local
	// require+replace so the worker resolves the freshly generated client across
	// the nested-module boundary.
	if err := mf.AddRequire(BAMLClientModulePath, "v0.0.0"); err != nil {
		return fmt.Errorf("nativeworkersrc: adding baml_client require: %w", err)
	}
	if err := mf.AddReplace(BAMLClientModulePath, "", opts.BAMLClientPath, ""); err != nil {
		return fmt.Errorf("nativeworkersrc: adding baml_client replace: %w", err)
	}

	// (3) BAML operation B — align/select stock BAML with the build's selection.
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

	return writeModFile(gomodPath, mf)
}

// ApplyNativeOnlyOverlay rewrites moduleDir/go.mod in place for the ExecBridge-U1b
// NATIVE-ONLY worker. It performs ONLY the generic missing-replace cleanup, and it
// deliberately SKIPS both BAML operations ApplyOverlay does: it neither adds the
// generated baml_client require/replace NOR selects github.com/boundaryml/baml. The
// native-only worker's compiled graph is BAML-free (proven by the whole-command
// dependency gate), so wiring baml_client/BAML into its module graph would be the
// exact "helpful module wiring" §8 warns against — a require that a later import
// could quietly start using.
//
// It is MUTUALLY EXCLUSIVE with ApplyOverlay; the CLI validates that the two modes
// are never both requested, so a future build-script typo cannot silently inject
// BAML into the native-only artifact.
func ApplyNativeOnlyOverlay(moduleDir string) error {
	gomodPath := filepath.Join(moduleDir, "go.mod")
	mf, err := parseModFile(gomodPath)
	if err != nil {
		return err
	}
	if err := dropMissingFilesystemReplaces(mf, moduleDir); err != nil {
		return err
	}
	return writeModFile(gomodPath, mf)
}

// parseModFile reads and parses a go.mod.
func parseModFile(gomodPath string) (*modfile.File, error) {
	data, err := os.ReadFile(gomodPath)
	if err != nil {
		return nil, err
	}
	mf, err := modfile.Parse(gomodPath, data, nil)
	if err != nil {
		return nil, fmt.Errorf("nativeworkersrc: parsing %s: %w", gomodPath, err)
	}
	return mf, nil
}

// writeModFile cleans, formats, and writes a modfile back in place.
func writeModFile(gomodPath string, mf *modfile.File) error {
	mf.Cleanup()
	out, err := mf.Format()
	if err != nil {
		return err
	}
	return os.WriteFile(gomodPath, out, 0o644)
}

// dropMissingFilesystemReplaces drops every filesystem-path replace whose target
// is absent in this build context, plus the matching require. Collect first
// (mutating while ranging is unsafe). Only a genuinely-absent target
// (os.IsNotExist) is dropped — a permission or other I/O stat error is NOT
// evidence of absence, so it is returned rather than silently dropping a
// still-valid replace and corrupting the module graph. It is the ONE overlay
// operation shared between the BAML and native-only modes.
func dropMissingFilesystemReplaces(mf *modfile.File, moduleDir string) error {
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
	return nil
}
