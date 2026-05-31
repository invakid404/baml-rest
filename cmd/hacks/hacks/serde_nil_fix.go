package hacks

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
)

const (
	baml3620SerdeNilV214Path            = "patches/baml3620_serde_nil_v214.diff"
	baml3620SerdeNilV214Checksum        = "eaf8d6880510dfbf71f1bf8a8b49228f6566b9f3bc564be3462c1ee15112a20d"
	baml3620SerdeNilV222Path            = "patches/baml3620_serde_nil_v222.diff"
	baml3620SerdeNilV222Checksum        = "f43c6977e017286a53dc702f03059a4cc8a76bd2ee6e0d6f853254b70343e4ec"
	baml3620SerdeNilProvenance          = "BoundaryML/baml issue #3620 — Go SDK serde nil-value panic"
	baml3620SerdeNilMinFloor            = "v0.214.0"
	baml3620SerdeNilUpstreamMergedFloor = "v999.0.0"
)

func init() {
	patchMetadataByPath[baml3620SerdeNilV214Path] = embeddedPatchMetadata{
		checksum:   baml3620SerdeNilV214Checksum,
		provenance: baml3620SerdeNilProvenance,
	}
	patchMetadataByPath[baml3620SerdeNilV222Path] = embeddedPatchMetadata{
		checksum:   baml3620SerdeNilV222Checksum,
		provenance: baml3620SerdeNilProvenance,
	}
}

// ApplyBamlSerdeNilFix is the server-build in-place wrapper that mirrors
// ApplyRuntimeDeadlockFix: resolves the BAML module, copies it out of
// GOMODCACHE when needed, applies the serde nil-value fix, and installs
// a go.work replace directive for the patched copy.
func ApplyBamlSerdeNilFix(bamlVersion string) error {
	requestedVersion := strings.TrimSpace(bamlVersion)
	if requestedVersion != "" {
		requestedVersion = bamlutils.NormalizeVersion(requestedVersion)
	}

	resolvedVersion, err := bamlModuleVersion()
	if err != nil {
		return err
	}
	moduleDir, err := bamlModuleDir()
	if err != nil {
		return err
	}

	version := resolvedVersion
	if requestedVersion != "" && bamlutils.CompareVersions(requestedVersion, resolvedVersion) != 0 {
		usesLocalReplace, err := moduleUsesLocalReplace(moduleDir)
		if err != nil {
			return err
		}
		if usesLocalReplace {
			version = requestedVersion
		}
	}

	moduleDir, usingPatchedCopy, err := preparePatchedBamlModuleDir(moduleDir, version)
	if err != nil {
		return err
	}

	if err := ApplyBamlSerdeNilFixToDir(version, moduleDir); err != nil {
		return err
	}

	if usingPatchedCopy {
		if err := setGoWorkReplace("github.com/boundaryml/baml", moduleDir); err != nil {
			return err
		}
	}
	return nil
}

// ApplyBamlSerdeNilFixToDir applies the embedded BoundaryML/baml#3620 serde
// nil-value fix to a BAML module rooted at moduleDir. The patch guards
// decodeListValue against zero reflect.Values returned by Decode (e.g. from
// null union arms), preventing a panic on reflect.Value.Set.
//
// Versions below v0.214.0 are skipped (untested decode.go shape).
// v0.214.x uses a dedicated patch (single-return Decode signature);
// v0.218.0+ uses the v222 patch (dual-return Decode signature).
func ApplyBamlSerdeNilFixToDir(version, moduleDir string) error {
	version = strings.TrimSpace(version)
	if version != "" {
		version = bamlutils.NormalizeVersion(version)
	}
	if version == "" {
		return fmt.Errorf("baml version is required to select the serde nil-value fix patch")
	}
	if moduleDir == "" {
		return fmt.Errorf("module directory is required to apply the serde nil-value fix patch")
	}

	if bamlutils.CompareVersions(version, baml3620SerdeNilMinFloor) < 0 {
		fmt.Printf("Skipping serde nil-value fix (effective version %s is below %s where the patch shape is untested)\n", version, baml3620SerdeNilMinFloor)
		return nil
	}
	if bamlutils.CompareVersions(version, baml3620SerdeNilUpstreamMergedFloor) >= 0 {
		fmt.Printf("Skipping serde nil-value fix (effective version %s is at or above %s where issue #3620 is fixed upstream)\n", version, baml3620SerdeNilUpstreamMergedFloor)
		return nil
	}

	patchPath := baml3620SerdeNilV222Path
	if bamlutils.CompareVersions(version, "v0.218.0") < 0 {
		patchPath = baml3620SerdeNilV214Path
	}

	patchData, err := readEmbeddedPatch(patchPath)
	if err != nil {
		return err
	}

	applied, _, err := applyPatch(moduleDir, patchData)
	if err != nil {
		return fmt.Errorf("applying %s in %s: %w", filepath.Base(patchPath), moduleDir, err)
	}

	if applied {
		fmt.Printf("  Applied patch: %s\n", filepath.Base(patchPath))
		return nil
	}

	fmt.Printf("  Patch already applied: %s\n", filepath.Base(patchPath))
	return nil
}
