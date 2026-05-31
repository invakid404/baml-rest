package hacks

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils"
)

const (
	baml3620SerdeNilV222Path            = "patches/baml3620_serde_nil_v222.diff"
	baml3620SerdeNilV222Checksum        = "f43c6977e017286a53dc702f03059a4cc8a76bd2ee6e0d6f853254b70343e4ec"
	baml3620SerdeNilV222Provenance      = "BoundaryML/baml issue #3620 — Go SDK serde nil-value panic"
	baml3620SerdeNilUpstreamMergedFloor = "v999.0.0"
)

func init() {
	patchMetadataByPath[baml3620SerdeNilV222Path] = embeddedPatchMetadata{
		checksum:   baml3620SerdeNilV222Checksum,
		provenance: baml3620SerdeNilV222Provenance,
	}
}

// ApplyBamlSerdeNilFixToDir applies the embedded BoundaryML/baml#3620 serde
// nil-value fix to a BAML module rooted at moduleDir. The patch guards
// decodeListValue against zero reflect.Values returned by Decode (e.g. from
// null union arms), preventing a panic on reflect.Value.Set.
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

	if bamlutils.CompareVersions(version, baml3620SerdeNilUpstreamMergedFloor) >= 0 {
		fmt.Printf("Skipping serde nil-value fix (effective version %s is at or above %s where issue #3620 is fixed upstream)\n", version, baml3620SerdeNilUpstreamMergedFloor)
		return nil
	}

	patchData, err := readEmbeddedPatch(baml3620SerdeNilV222Path)
	if err != nil {
		return err
	}

	applied, _, err := applyPatch(moduleDir, patchData)
	if err != nil {
		return fmt.Errorf("applying %s in %s: %w", filepath.Base(baml3620SerdeNilV222Path), moduleDir, err)
	}

	if applied {
		fmt.Printf("  Applied patch: %s\n", filepath.Base(baml3620SerdeNilV222Path))
		return nil
	}

	fmt.Printf("  Patch already applied: %s\n", filepath.Base(baml3620SerdeNilV222Path))
	return nil
}
