package bamlunicode

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The committed UCD data lives ONLY as the two SHA-256-pinned UCD.zip archives
// under testdata/ucd/archives (no raw .txt is committed). These helpers give the
// always-on tests the same verified-extract path the generator uses.

func archivePath(ver string) string {
	return filepath.Join("testdata", "ucd", "archives", "UCD-"+ver+".zip")
}

// verifiedArchive reads and SHA-256-verifies the committed UCD archive for ver
// against the manifest, returning its bytes.
func verifiedArchive(t *testing.T, ver string) []byte {
	t.Helper()
	raw, err := os.ReadFile(archivePath(ver))
	if err != nil {
		t.Fatalf("read UCD archive %s: %v", ver, err)
	}
	var want string
	for _, a := range Manifest.Archives {
		if a.UnicodeVersion == ver {
			want = a.SHA256
		}
	}
	if want == "" {
		t.Fatalf("manifest has no archive entry for Unicode %s", ver)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("UCD archive %s sha256 %s != manifest %s", ver, got, want)
	}
	return raw
}

// extractUCD extracts a named root file from the verified archive for ver.
func extractUCD(t *testing.T, ver, name string) []byte {
	t.Helper()
	raw := verifiedArchive(t, ver)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open archive %s: %v", ver, err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("%s not found at root of archive %s", name, ver)
	return nil
}

// TestArchiveHashes enforces the pinned UCD.zip archive digests (drift-guard
// check B / P2). Combined with cmd/gen -check (which extracts from and re-hashes
// these archives), the whole provenance chain archive -> file -> table is
// tamper-evident.
func TestArchiveHashes(t *testing.T) {
	capStress(t) // ~18MB sha256 across two archives; deterministic
	if len(Manifest.Archives) == 0 {
		t.Fatal("manifest has no archives")
	}
	for _, a := range Manifest.Archives {
		verifiedArchive(t, a.UnicodeVersion) // fatals on any mismatch
	}
}
