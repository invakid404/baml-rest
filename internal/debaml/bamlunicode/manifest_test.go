package bamlunicode

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// TestManifestMatchesVersions asserts the generated Manifest agrees with the
// hand-maintained compatibility key in versions.go. If a BAML bump updates one
// but not the other, this fails before anything ships.
func TestManifestMatchesVersions(t *testing.T) {
	checks := []struct {
		name      string
		got, want string
	}{
		{"BAMLVersion", Manifest.BAMLVersion, BAMLVersion},
		{"BAMLSourceCommit", Manifest.BAMLSourceCommit, BAMLSourceCommit},
		{"RustcVersion", Manifest.RustcVersion, RustcVersion},
		{"RustcCommit", Manifest.RustcCommit, RustcCommit},
		{"RustStdUnicodeVersion", Manifest.RustStdUnicodeVersion, RustStdUnicodeVersion},
		{"NormalizationCrate", Manifest.NormalizationCrate, NormalizationCrate},
		{"NormalizationCrateVersion", Manifest.NormalizationCrateVersion, NormalizationCrateVersion},
		{"NormalizationCrateChecksum", Manifest.NormalizationCrateChecksum, NormalizationCrateChecksum},
		{"NormalizationUnicodeVersion", Manifest.NormalizationUnicodeVersion, NormalizationUnicodeVersion},
		{"MatchStringFingerprint", Manifest.MatchStringFingerprint, MatchStringFingerprint},
		{"MatchStringPath", Manifest.MatchStringPath, MatchStringPath},
		{"ReleaseWorkflowPath", Manifest.ReleaseWorkflowPath, ReleaseWorkflowPath},
		{"ReleaseWorkflowFingerprint", Manifest.ReleaseWorkflowFingerprint, ReleaseWorkflowFingerprint},
		{"SetupRustActionPath", Manifest.SetupRustActionPath, SetupRustActionPath},
		{"SetupRustActionFingerprint", Manifest.SetupRustActionFingerprint, SetupRustActionFingerprint},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("Manifest.%s = %q, versions.go = %q", c.name, c.got, c.want)
		}
	}
}

// TestManifestInputHashes extracts every pinned input from its verified UCD
// archive and asserts each file's SHA-256 still matches the manifest. This ties
// the committed archives to the recorded file digests: corruption of either the
// archive or the manifest fails here.
func TestManifestInputHashes(t *testing.T) {
	capStress(t) // extracts + hashes every input from the multi-MB archives
	if len(Manifest.Inputs) == 0 {
		t.Fatal("manifest has no inputs")
	}
	for _, in := range Manifest.Inputs {
		sum := sha256.Sum256(extractUCD(t, in.UnicodeVersion, in.Name))
		if got := hex.EncodeToString(sum[:]); got != in.SHA256 {
			t.Errorf("%s/%s: extracted sha256 %s, manifest %s", in.UnicodeVersion, in.Name, got, in.SHA256)
		}
	}
}

// TestManifestOutputHashes re-hashes every committed generated table and asserts
// it matches the manifest. Combined with `cmd/gen -check` in CI, this makes the
// committed tables tamper-evident.
func TestManifestOutputHashes(t *testing.T) {
	if len(Manifest.Outputs) == 0 {
		t.Fatal("manifest has no outputs")
	}
	for _, out := range Manifest.Outputs {
		if got := sha256File(t, out.Name); got != out.SHA256 {
			t.Errorf("%s: sha256 %s, manifest %s", out.Name, got, out.SHA256)
		}
	}
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
