package codegenspine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// updateGuard regenerates guard.json from the live tree. Mirrors the repo's
// existing "-update-*-goldens" convention (cmd/introspect). Run:
//
//	go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard
//
// guardBaselineNote is the CURRENT provenance written by a regen and asserted
// against the committed guard.json (TestSourceGuard/provenance_note_current).
// Holding it in one named place — instead of hardcoding it inside
// computeLiveBaseline — stops a regen from silently rewriting the honest
// provenance back to obsolete wording (the M2 fix), and keeps the note in sync
// with guard.json. Its value here mirrors the post-#692 baseline note verbatim.
const guardBaselineNote = "Pin/tar-independence baseline for the codegen-spine slice: the packaged native-worker tar, the five first-party pseudo-version pins, and the byte content of the three collision-path trees. A codegen-spine (M/P) slice must leave every value here untouched. It is NOT a freeze on master: a sanctioned change to a guarded path — the /parse union burn-down (#689), the /parse union-residual batch 2, a post-squash first-party re-pin — legitimately moves these, and MUST re-run the update flag in the same change, or this guard stays red for everyone. First frozen at M0; re-frozen after #689 + the post-squash re-pin to 062871154d95, and again for the /parse union-residual batch 2 (branch pin at c011c7e95993, re-baselined post-squash). Regenerate with: go test ./internal/codegenspine/ -run TestSourceGuard -update-codegenspine-guard"

var updateGuard = flag.Bool("update-codegenspine-guard", false,
	"regenerate internal/codegenspine/guard.json from the live tree")

// guardBaseline is the frozen pin/tar-independence baseline. It is checked in as
// guard.json and verified byte-for-byte against the live tree on every run. An
// M0/P slice must leave every value here untouched; the guard fails loudly the
// moment a collision path, the packaged native-worker tar, or a first-party pin
// moves.
type guardBaseline struct {
	Note            string         `json:"note"`
	NativeWorkerTar tarBaseline    `json:"native_worker_tar"`
	FirstPartyPins  []pinBaseline  `json:"first_party_pins"`
	GuardedTrees    []treeBaseline `json:"guarded_trees"`
}

type tarBaseline struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type pinBaseline struct {
	File    string `json:"file"`
	Module  string `json:"module"`
	Version string `json:"version"`
}

type treeBaseline struct {
	Prefix    string `json:"prefix"`
	FileCount int    `json:"file_count"`
	SHA256    string `json:"sha256"`
}

const nativeWorkerTarRelPath = "cmd/build/nativeworker_module.tar"

// guardedTreePrefixes are the three collision paths a pin/tar-independent slice
// must not touch (codegen-spine scope §3, "P" label).
var guardedTreePrefixes = []string{
	"internal/debaml",
	"nativeserve",
	"internal/nativebody/nanollmprepare",
}

// firstPartyPinSites are the (go.mod, module) pairs whose pinned pseudo-version
// a P slice must not bump. Versions are read live at generation time and frozen
// into guard.json (nativeworker pin audit: cmd/build/nativeworker_pins_test.go).
var firstPartyPinSites = []struct{ file, module string }{
	{"nativeserve/go.mod", "github.com/invakid404/baml-rest"},
	{"nativeserve/go.mod", "github.com/invakid404/baml-rest/bamlutils"},
	{"nativeserve/go.mod", "github.com/invakid404/baml-rest/worker"},
	{"internal/nativebody/nanollmprepare/go.mod", "github.com/invakid404/baml-rest/bamlutils"},
	{"internal/nativebody/nanollmprepare/go.mod", "github.com/invakid404/baml-rest/worker"},
}

func pkgDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}

// hashFile returns the hex sha256 of a file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashTree walks a guarded prefix and returns (fileCount, combinedHash). The
// combined hash is deterministic: per regular file, in relpath-sorted order,
// the forward-slash repo-relative path and the file's content hash are folded
// into one digest. Any added, removed, renamed, or edited file changes it.
//
// ".DS_Store" is skipped so a macOS working copy and a Linux CI checkout agree;
// nothing else is excluded — the point is to catch stray files under a guarded
// path, not to hide them.
func hashTree(root, prefix string) (int, string, error) {
	base := filepath.Join(root, filepath.FromSlash(prefix))
	type entry struct{ rel, hash string }
	var entries []entry
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if d.Name() == ".DS_Store" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		fh, err := hashFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel: rel, hash: fh})
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\n%s\n", e.rel, e.hash)
	}
	return len(entries), hex.EncodeToString(h.Sum(nil)), nil
}

// requireEntryVersion returns the version if entry is a "<module> <version>"
// require entry for module, else "".
func requireEntryVersion(entry, module string) string {
	fields := strings.Fields(entry)
	if len(fields) >= 2 && fields[0] == module && strings.HasPrefix(fields[1], "v") {
		return fields[1]
	}
	return ""
}

// readPinVersion returns the version a go.mod REQUIRE directive pins for module.
// It parses only require directives — the single-line form
// ("require <module> <version>") and entries inside a "require (" ... ")" block —
// and deliberately ignores replace/exclude directives. Those can also carry a
// versioned "<module> <version>" entry (e.g. "replace <module> <v> => ../path",
// or a version inside a "replace (" / "exclude (" block), which is NOT the
// module's require pin and must not be mistaken for one. Returns "" if module has
// no require pin.
func readPinVersion(gomodPath, module string) (string, error) {
	data, err := os.ReadFile(gomodPath)
	if err != nil {
		return "", err
	}
	inRequireBlock := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if inRequireBlock {
			if line == ")" {
				inRequireBlock = false
				continue
			}
			if v := requireEntryVersion(line, module); v != "" {
				return v, nil
			}
			continue
		}
		if strings.HasPrefix(line, "require") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, "require"))
			if rest == "(" {
				inRequireBlock = true
				continue
			}
			// Single-line require: rest == "<module> <version>".
			if v := requireEntryVersion(rest, module); v != "" {
				return v, nil
			}
		}
		// Any other line (replace/exclude directives and their block bodies) is
		// never a require pin: it is not inside a require block and does not
		// begin with "require", so no match branch above fires for it.
	}
	return "", nil
}

// computeLiveBaseline reads the current tree into a guardBaseline.
func computeLiveBaseline(t *testing.T, root string) guardBaseline {
	t.Helper()
	b := guardBaseline{Note: guardBaselineNote}

	tarPath := filepath.Join(root, filepath.FromSlash(nativeWorkerTarRelPath))
	fi, err := os.Stat(tarPath)
	if err != nil {
		t.Fatalf("stat native worker tar: %v", err)
	}
	tarHash, err := hashFile(tarPath)
	if err != nil {
		t.Fatalf("hash native worker tar: %v", err)
	}
	b.NativeWorkerTar = tarBaseline{Path: nativeWorkerTarRelPath, SizeBytes: fi.Size(), SHA256: tarHash}

	for _, site := range firstPartyPinSites {
		v, err := readPinVersion(filepath.Join(root, filepath.FromSlash(site.file)), site.module)
		if err != nil {
			t.Fatalf("read pin %s in %s: %v", site.module, site.file, err)
		}
		if v == "" {
			t.Fatalf("no version pin found for %s in %s", site.module, site.file)
		}
		b.FirstPartyPins = append(b.FirstPartyPins, pinBaseline{File: site.file, Module: site.module, Version: v})
	}

	for _, prefix := range guardedTreePrefixes {
		n, hash, err := hashTree(root, prefix)
		if err != nil {
			t.Fatalf("hash tree %s: %v", prefix, err)
		}
		b.GuardedTrees = append(b.GuardedTrees, treeBaseline{Prefix: prefix, FileCount: n, SHA256: hash})
	}
	return b
}

func loadGuardBaseline(t *testing.T) guardBaseline {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(pkgDir(t), "guard.json"))
	if err != nil {
		t.Fatalf("read guard.json: %v", err)
	}
	var b guardBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("parse guard.json: %v", err)
	}
	return b
}

// TestSourceGuard proves this pin/tar-independent slice touched none of the
// collision paths: the native-worker tar is byte-identical, the first-party
// pins are unmoved, and the three guarded trees are byte-frozen.
func TestSourceGuard(t *testing.T) {
	root := repoRoot(t)
	live := computeLiveBaseline(t, root)

	if *updateGuard {
		out, err := json.MarshalIndent(live, "", "  ")
		if err != nil {
			t.Fatalf("marshal guard baseline: %v", err)
		}
		out = append(out, '\n')
		if err := os.WriteFile(filepath.Join(pkgDir(t), "guard.json"), out, 0o644); err != nil {
			t.Fatalf("write guard.json: %v", err)
		}
		t.Logf("regenerated guard.json (tar %s, %d bytes; %d pins; %d trees)",
			live.NativeWorkerTar.SHA256[:12], live.NativeWorkerTar.SizeBytes,
			len(live.FirstPartyPins), len(live.GuardedTrees))
		return
	}

	want := loadGuardBaseline(t)

	t.Run("provenance_note_current", func(t *testing.T) {
		if want.Note != live.Note {
			t.Errorf("guard.json note is stale — re-run -update-codegenspine-guard:\n frozen: %q\n want:   %q", want.Note, live.Note)
		}
	})

	t.Run("native_worker_tar_byte_identical", func(t *testing.T) {
		if live.NativeWorkerTar.SizeBytes != want.NativeWorkerTar.SizeBytes {
			t.Errorf("%s size changed: live %d, frozen %d", nativeWorkerTarRelPath, live.NativeWorkerTar.SizeBytes, want.NativeWorkerTar.SizeBytes)
		}
		if live.NativeWorkerTar.SHA256 != want.NativeWorkerTar.SHA256 {
			t.Errorf("%s sha256 changed: live %s, frozen %s — a P slice must not regenerate the native-worker tar", nativeWorkerTarRelPath, live.NativeWorkerTar.SHA256, want.NativeWorkerTar.SHA256)
		}
	})

	t.Run("first_party_pins_unmoved", func(t *testing.T) {
		frozen := map[string]string{}
		for _, p := range want.FirstPartyPins {
			frozen[p.File+" "+p.Module] = p.Version
		}
		if len(live.FirstPartyPins) != len(want.FirstPartyPins) {
			t.Errorf("pin count changed: live %d, frozen %d", len(live.FirstPartyPins), len(want.FirstPartyPins))
		}
		for _, p := range live.FirstPartyPins {
			key := p.File + " " + p.Module
			w, ok := frozen[key]
			if !ok {
				t.Errorf("unexpected pin site %s", key)
				continue
			}
			if p.Version != w {
				t.Errorf("pin moved for %s: live %s, frozen %s — a P slice must not bump a first-party pin", key, p.Version, w)
			}
		}
	})

	t.Run("guarded_trees_byte_frozen", func(t *testing.T) {
		frozen := map[string]treeBaseline{}
		for _, tr := range want.GuardedTrees {
			frozen[tr.Prefix] = tr
		}
		for _, tr := range live.GuardedTrees {
			w, ok := frozen[tr.Prefix]
			if !ok {
				t.Errorf("unexpected guarded tree %s", tr.Prefix)
				continue
			}
			if tr.FileCount != w.FileCount {
				t.Errorf("guarded tree %s file count changed: live %d, frozen %d — a P slice must not add/remove files here", tr.Prefix, tr.FileCount, w.FileCount)
			}
			if tr.SHA256 != w.SHA256 {
				t.Errorf("guarded tree %s content changed: live %s, frozen %s — a P slice must not touch %s", tr.Prefix, tr.SHA256, w.SHA256, tr.Prefix)
			}
		}
	})
}

// TestGuardArtifactsOutsideCollisionPaths is a static sanity check that the M0
// artifacts themselves live outside the guarded prefixes — the slice cannot be
// pin/tar-independent if its own files sit under a collision path.
func TestGuardArtifactsOutsideCollisionPaths(t *testing.T) {
	m0Paths := []string{
		"internal/codegenspine",
		"docs/codegen-spine",
		// M1 (first native descriptor vertical) artifacts — all pin/tar-neutral.
		"bamlutils/projectdescriptor",
		"internal/nativespine",
		"internal/nativespinefixture",
		"adapters/common/codegen/nativespine.go",
	}
	for _, p := range m0Paths {
		for _, guarded := range guardedTreePrefixes {
			if p == guarded || strings.HasPrefix(p, guarded+"/") {
				t.Errorf("M0 artifact path %q lives under guarded prefix %q", p, guarded)
			}
		}
	}
}
