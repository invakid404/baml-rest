package bamlunicode

// ProfileManifest is the machine-readable provenance record for the generated
// tables. Its authoritative value lives in the generated manifest_gen.go (var
// Manifest) and a mirrored testdata/manifest.json; both are produced by
// ./cmd/gen from the same pinned inputs. The CI drift guard re-derives the
// discovered fields (BAML/rustc/crate/match_string) from BAML source and
// re-hashes the inputs/outputs, and fails on any mismatch.
type ProfileManifest struct {
	// Compatibility key (mirrors the consts in versions.go).
	BAMLVersion                 string `json:"baml_version"`
	BAMLSourceCommit            string `json:"baml_source_commit"`
	RustcVersion                string `json:"rustc_version"`
	RustcCommit                 string `json:"rustc_commit"`
	RustStdUnicodeVersion       string `json:"rust_std_unicode_version"`
	NormalizationCrate          string `json:"normalization_crate"`
	NormalizationCrateVersion   string `json:"normalization_crate_version"`
	NormalizationCrateChecksum  string `json:"normalization_crate_checksum"`
	NormalizationUnicodeVersion string `json:"normalization_unicode_version"`
	MatchStringPath             string `json:"match_string_path"`
	MatchStringFingerprint      string `json:"match_string_fingerprint"`
	ReleaseWorkflowPath         string `json:"release_workflow_path"`
	ReleaseWorkflowFingerprint  string `json:"release_workflow_fingerprint"`
	SetupRustActionPath         string `json:"setup_rust_action_path"`
	SetupRustActionFingerprint  string `json:"setup_rust_action_fingerprint"`

	// GeneratorRevision identifies the ./cmd/gen logic that produced the
	// outputs; a change here forces regeneration/review even if the UCD inputs
	// are unchanged.
	GeneratorRevision string `json:"generator_revision"`

	// Inputs are the exact UCD text files consumed (or, for
	// NormalizationTest.txt, pinned as a conformance corpus), each SHA-256
	// verified before use.
	Inputs []ManifestFile `json:"inputs"`
	// Archives are the upstream UCD.zip provenance (recorded, not re-downloaded
	// by the generator or the freshness check).
	Archives []ManifestArchive `json:"archives"`
	// Outputs are the generated Go table files with their committed digests.
	Outputs []ManifestFile `json:"outputs"`
}

// ManifestFile records one file's provenance and SHA-256.
type ManifestFile struct {
	UnicodeVersion string `json:"unicode_version,omitempty"`
	Name           string `json:"name"`
	URL            string `json:"url,omitempty"`
	SHA256         string `json:"sha256"`
}

// ManifestArchive records one upstream UCD.zip.
type ManifestArchive struct {
	UnicodeVersion string `json:"unicode_version"`
	URL            string `json:"url"`
	SHA256         string `json:"sha256"`
}
