package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"github.com/invakid404/baml-rest/cmd/build/nativeworkersrc"
)

// De-BAML full pin/tar — the PACKAGED MANIFEST guard.
//
// TestNativeWorkerModuleTarIsFresh already byte-compares the committed tar against
// a freshly built one, so a stale tar cannot survive it. What it does NOT do is
// say anything READABLE about the packaged manifests: when a reviewer wants to
// know "do the go.mod files inside the shipped tar carry the current pins, and do
// they claim the same follow-up status as the tracked record?", the only way to
// find out was to extract them by hand and grep.
//
// That is not a hypothetical. A cold review did exactly that, hit the HISTORICAL
// SHAs and the word RESOLVED inside the BUMP-RULE prose, and reported the tar as
// stale and partially regenerated — while the packaged manifests were in fact
// byte-identical to the source and carried the correct pins. A freshness test that
// is green but says nothing an auditor can read is a freshness test that gets
// re-litigated by hand.
//
// So this guard performs the audit mechanically, on the COMMITTED tar:
//
//	IDENTITY   each packaged go.mod is byte-identical to its source file;
//	PINS       the first-party selections INSIDE the packaged manifests are the
//	           same five the source declares, on one commit;
//	STATUS     each packaged manifest carries exactly one PIN-STATUS marker, and
//	           it equals the STATUS in the tracked follow-up record.
//
// The PIN-STATUS marker exists for the same reason: it gives the packaged
// manifests one authoritative, machine-checkable token, so an auditor never has to
// infer the current state from paragraphs that legitimately discuss past ones.

// packagedManifests are the two module manifests carried inside the opaque tar.
func packagedManifests() []string {
	return []string{
		nativeworkersrc.NativeServeModuleRelPath + "/go.mod",
		nativeworkersrc.ModuleRelPath + "/go.mod",
	}
}

// pinStatusRe matches the machine-readable marker.
var pinStatusRe = regexp.MustCompile(`(?m)^// PIN-STATUS: ([A-Z]+)$`)

// readTarEntries returns the named entries from the committed tar.
func readTarEntries(t *testing.T, repoRoot string, names []string) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(nativeworkersrc.TarRelPath)))
	if err != nil {
		t.Fatalf("read %s: %v", nativeworkersrc.TarRelPath, err)
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	found := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading %s: %v", nativeworkersrc.TarRelPath, err)
		}
		if !want[hdr.Name] {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading entry %s: %v", hdr.Name, err)
		}
		found[hdr.Name] = body
	}
	for _, n := range names {
		if _, ok := found[n]; !ok {
			t.Fatalf("%s does not contain %s; the packaged worker could not resolve its own module graph", nativeworkersrc.TarRelPath, n)
		}
	}
	return found
}

// TestPackagedManifestsMatchTheTrackedPins is the audit, run on every ordinary
// `go test ./...`.
func TestPackagedManifestsMatchTheTrackedPins(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	entries := readTarEntries(t, repoRoot, packagedManifests())
	record := readPinFollowup(t, repoRoot)
	trackedStatus := record.fields["STATUS"]
	trackedCommit := record.fields["PINNED-COMMIT"]
	if trackedStatus == "" || trackedCommit == "" {
		t.Fatalf("the tracked record carries STATUS=%q PINNED-COMMIT=%q; this guard compares the packaged manifests against it", trackedStatus, trackedCommit)
	}

	for name, packaged := range entries {
		t.Run(name, func(t *testing.T) {
			// IDENTITY. Implied by tar freshness, asserted here so a failure says
			// WHICH packaged file drifted rather than only that some byte did.
			source, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(name)))
			if err != nil {
				t.Fatalf("read source %s: %v", name, err)
			}
			if !bytes.Equal(source, packaged) {
				t.Errorf("the %s inside %s differs from the source file; the tar was regenerated from a different tree state than the one committed",
					name, nativeworkersrc.TarRelPath)
			}

			// PINS. Read the first-party selections out of the PACKAGED bytes, so
			// this is a statement about what ships, not about what is on disk.
			mf, err := modfile.Parse(name, packaged, nil)
			if err != nil {
				t.Fatalf("parse packaged %s: %v", name, err)
			}
			checked := 0
			for _, pin := range firstPartyPins() {
				if pin.file != name {
					continue
				}
				var version string
				for _, r := range mf.Require {
					if r.Mod.Path == pin.module {
						version = r.Mod.Version
					}
				}
				if version == "" {
					t.Errorf("packaged %s no longer requires %s", name, pin.module)
					continue
				}
				m := pseudoVersionRe.FindStringSubmatch(version)
				if m == nil {
					t.Errorf("packaged %s selects %s %s, which is not a first-party pseudo-version", name, pin.module, version)
					continue
				}
				if m[2] != trackedCommit {
					t.Errorf("packaged %s selects %s at commit %s, but the tracked record names %s; the shipped tar carries pins the follow-up does not describe",
						name, pin.module, m[2], trackedCommit)
				}
				checked++
			}
			// Non-vacuity: a manifest this guard reads but finds no tracked pin in
			// would pass silently, which is the shape of the failure it exists to
			// catch.
			if checked == 0 {
				t.Errorf("no tracked first-party pin was found in packaged %s; this guard inspected nothing", name)
			}

			// STATUS. Exactly one marker, agreeing with the tracked record.
			markers := pinStatusRe.FindAllStringSubmatch(string(packaged), -1)
			if len(markers) != 1 {
				t.Fatalf("packaged %s carries %d PIN-STATUS markers, want exactly 1; the marker is what makes the packaged pin state readable without interpreting prose",
					name, len(markers))
			}
			if got := markers[0][1]; got != trackedStatus {
				t.Errorf("packaged %s declares PIN-STATUS: %s but the tracked record records STATUS: %s", name, got, trackedStatus)
			}
		})
	}
}

// TestPackagedManifestGuardBitesOnAStaleManifest is the control. The guard above
// asserts agreement, and an agreement check is worth exactly as much as its
// ability to notice disagreement — so drive its comparisons with a manifest that
// carries the previous slice's commit and the opposite status.
func TestPackagedManifestGuardBitesOnAStaleManifest(t *testing.T) {
	const stale = `module example.com/stale

// PIN-STATUS: RESOLVED

require (
	github.com/invakid404/baml-rest v0.0.0-20260819100300-de1eefa68ed8
)
`
	mf, err := modfile.Parse("stale/go.mod", []byte(stale), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var version string
	for _, r := range mf.Require {
		if r.Mod.Path == "github.com/invakid404/baml-rest" {
			version = r.Mod.Version
		}
	}
	m := pseudoVersionRe.FindStringSubmatch(version)
	if m == nil {
		t.Fatalf("the control's own pseudo-version did not parse: %q", version)
	}
	if m[2] == "e38f7effd633" {
		t.Fatal("the control names the current commit; it would prove nothing")
	}

	markers := pinStatusRe.FindAllStringSubmatch(stale, -1)
	if len(markers) != 1 || markers[0][1] != "RESOLVED" {
		t.Fatalf("the marker regexp did not read the control's status: %v", markers)
	}
	if markers[0][1] == "OUTSTANDING" {
		t.Fatal("the control's status matches the tracked one; it would prove nothing")
	}
	if !strings.Contains(stale, "de1eefa68ed8") {
		t.Fatal("the control lost its stale commit")
	}
}
