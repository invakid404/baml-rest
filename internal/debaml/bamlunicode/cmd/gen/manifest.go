package main

import (
	"fmt"
	"go/format"
	"strings"
)

// Generator-local mirror of bamlunicode.ProfileManifest. Kept separate so the
// generator never imports the package it generates (a broken table must not
// block regeneration). The field set and JSON tags match manifest.go exactly;
// emitManifestGo renders the committed Go literal and json.Marshal renders the
// mirrored testdata/manifest.json.
type ProfileManifest struct {
	BAMLVersion                 string            `json:"baml_version"`
	BAMLSourceCommit            string            `json:"baml_source_commit"`
	RustcVersion                string            `json:"rustc_version"`
	RustcCommit                 string            `json:"rustc_commit"`
	RustStdUnicodeVersion       string            `json:"rust_std_unicode_version"`
	NormalizationCrate          string            `json:"normalization_crate"`
	NormalizationCrateVersion   string            `json:"normalization_crate_version"`
	NormalizationCrateChecksum  string            `json:"normalization_crate_checksum"`
	NormalizationUnicodeVersion string            `json:"normalization_unicode_version"`
	MatchStringPath             string            `json:"match_string_path"`
	MatchStringFingerprint      string            `json:"match_string_fingerprint"`
	ReleaseWorkflowPath         string            `json:"release_workflow_path"`
	ReleaseWorkflowFingerprint  string            `json:"release_workflow_fingerprint"`
	SetupRustActionPath         string            `json:"setup_rust_action_path"`
	SetupRustActionFingerprint  string            `json:"setup_rust_action_fingerprint"`
	GeneratorRevision           string            `json:"generator_revision"`
	Inputs                      []ManifestFile    `json:"inputs"`
	Archives                    []ManifestArchive `json:"archives"`
	Outputs                     []ManifestFile    `json:"outputs"`
}

type ManifestFile struct {
	UnicodeVersion string `json:"unicode_version,omitempty"`
	Name           string `json:"name"`
	URL            string `json:"url,omitempty"`
	SHA256         string `json:"sha256"`
}

type ManifestArchive struct {
	UnicodeVersion string `json:"unicode_version"`
	URL            string `json:"url"`
	SHA256         string `json:"sha256"`
}

func emitManifestGo(m ProfileManifest) ([]byte, error) {
	var b strings.Builder
	b.WriteString(fileHeader("cmd/gen (compatibility profile + UCD input/output digests)"))
	b.WriteString("// Manifest is the machine-readable provenance record for the generated\n")
	b.WriteString("// tables; the mirrored testdata/manifest.json carries the same data. The CI\n")
	b.WriteString("// drift guard re-derives the discovered fields from BAML source and re-hashes\n")
	b.WriteString("// the inputs/outputs, failing on any mismatch.\n")
	b.WriteString("var Manifest = ProfileManifest{\n")
	fmt.Fprintf(&b, "\tBAMLVersion: %q,\n", m.BAMLVersion)
	fmt.Fprintf(&b, "\tBAMLSourceCommit: %q,\n", m.BAMLSourceCommit)
	fmt.Fprintf(&b, "\tRustcVersion: %q,\n", m.RustcVersion)
	fmt.Fprintf(&b, "\tRustcCommit: %q,\n", m.RustcCommit)
	fmt.Fprintf(&b, "\tRustStdUnicodeVersion: %q,\n", m.RustStdUnicodeVersion)
	fmt.Fprintf(&b, "\tNormalizationCrate: %q,\n", m.NormalizationCrate)
	fmt.Fprintf(&b, "\tNormalizationCrateVersion: %q,\n", m.NormalizationCrateVersion)
	fmt.Fprintf(&b, "\tNormalizationCrateChecksum: %q,\n", m.NormalizationCrateChecksum)
	fmt.Fprintf(&b, "\tNormalizationUnicodeVersion: %q,\n", m.NormalizationUnicodeVersion)
	fmt.Fprintf(&b, "\tMatchStringPath: %q,\n", m.MatchStringPath)
	fmt.Fprintf(&b, "\tMatchStringFingerprint: %q,\n", m.MatchStringFingerprint)
	fmt.Fprintf(&b, "\tReleaseWorkflowPath: %q,\n", m.ReleaseWorkflowPath)
	fmt.Fprintf(&b, "\tReleaseWorkflowFingerprint: %q,\n", m.ReleaseWorkflowFingerprint)
	fmt.Fprintf(&b, "\tSetupRustActionPath: %q,\n", m.SetupRustActionPath)
	fmt.Fprintf(&b, "\tSetupRustActionFingerprint: %q,\n", m.SetupRustActionFingerprint)
	fmt.Fprintf(&b, "\tGeneratorRevision: %q,\n", m.GeneratorRevision)

	b.WriteString("\tInputs: []ManifestFile{\n")
	for _, f := range m.Inputs {
		fmt.Fprintf(&b, "\t\t{UnicodeVersion: %q, Name: %q, URL: %q, SHA256: %q},\n", f.UnicodeVersion, f.Name, f.URL, f.SHA256)
	}
	b.WriteString("\t},\n")

	b.WriteString("\tArchives: []ManifestArchive{\n")
	for _, a := range m.Archives {
		fmt.Fprintf(&b, "\t\t{UnicodeVersion: %q, URL: %q, SHA256: %q},\n", a.UnicodeVersion, a.URL, a.SHA256)
	}
	b.WriteString("\t},\n")

	b.WriteString("\tOutputs: []ManifestFile{\n")
	for _, f := range m.Outputs {
		fmt.Fprintf(&b, "\t\t{Name: %q, SHA256: %q},\n", f.Name, f.SHA256)
	}
	b.WriteString("\t},\n")
	b.WriteString("}\n")
	return format.Source([]byte(b.String()))
}
