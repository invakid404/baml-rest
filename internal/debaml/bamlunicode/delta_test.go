package bamlunicode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

// Delta manifests: the scalars where the repository's OLD ambient Unicode path
// (Go stdlib / golang.org/x/text at Unicode 15.0.0) disagrees with the pinned
// BAML target (Rust std Unicode 17.0.0 for case/properties, unicode-normalization
// Unicode 16.0.0 for NFKD/marks). These are exactly the scalars #555 was filed
// about; committing them and asserting each as mandatory test input keeps the
// proof from being dominated by the ~1.1M unchanged scalars.
//
// The `new` column is bamlunicode's output, which the exhaustive Rust reference
// (rustref_test.go) independently proves byte-for-byte. This always-on test
// asserts (a) bamlunicode still produces `new` (freshness), (b) new != old (a
// genuine version delta), and, on the pinned Unicode-15 baseline, that the full
// delta SET is complete (nothing silently dropped).
//
// Regenerate with BAMLUNICODE_UPDATE_DELTAS=1.

const baselineUnicodeVersion = "15.0.0"

type scalarDelta struct {
	CP  rune   `json:"cp"`
	Old string `json:"old"` // hex UTF-8 of the ambient Go/x-text result
	New string `json:"new"` // hex UTF-8 of the bamlunicode result
}

type boolDelta struct {
	CP  rune `json:"cp"`
	Old bool `json:"old"`
	New bool `json:"new"`
}

func everyScalar(fn func(r rune)) {
	for cp := rune(0); cp <= 0x10FFFF; cp++ {
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue
		}
		fn(cp)
	}
}

func computeLowercaseDeltas() []scalarDelta {
	caser := cases.Lower(language.Und)
	out := []scalarDelta{}
	everyScalar(func(r rune) {
		old := caser.String(string(r))
		nw := LowerString(string(r))
		if old != nw {
			out = append(out, scalarDelta{r, hexOf(old), hexOf(nw)})
		}
	})
	return out
}

func computeNFKDDeltas() []scalarDelta {
	out := []scalarDelta{}
	everyScalar(func(r rune) {
		old := norm.NFKD.String(string(r))
		nw := NFKD(string(r))
		if old != nw {
			out = append(out, scalarDelta{r, hexOf(old), hexOf(nw)})
		}
	})
	return out
}

func computeBoolDeltas(oldFn, newFn func(rune) bool) []boolDelta {
	out := []boolDelta{}
	everyScalar(func(r rune) {
		o, n := oldFn(r), newFn(r)
		if o != n {
			out = append(out, boolDelta{r, o, n})
		}
	})
	return out
}

// oldAlphanumeric is the predicate parse.go uses today (Go stdlib, Unicode 15).
func oldAlphanumeric(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }
func oldWhitespace(r rune) bool   { return unicode.IsSpace(r) }
func oldCombiningMark(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Mc, unicode.Me)
}

func deltaPath(name string) string { return filepath.Join("testdata", "deltas", name) }

func TestDeltaManifests(t *testing.T) {
	capStress(t) // 1.1M-scalar scan; deterministic — cap under -race -count=100
	type scalarManifest struct {
		name    string
		compute func() []scalarDelta
	}
	type boolManifest struct {
		name       string
		compute    func() []boolDelta
		allowEmpty bool
	}
	scalarManifests := []scalarManifest{
		{"lowercase.json", computeLowercaseDeltas},
		{"nfkd.json", computeNFKDDeltas},
	}
	boolManifests := []boolManifest{
		{"alphanumeric.json", func() []boolDelta { return computeBoolDeltas(oldAlphanumeric, IsAlphanumeric) }, false},
		// White_Space is identical across Unicode 15..17, so this delta set is
		// legitimately empty: it is committed as positive evidence that the trim
		// path is version-stable, and the baseline completeness check below keeps
		// it that way (a future divergence would repopulate it and fail).
		{"whitespace.json", func() []boolDelta { return computeBoolDeltas(oldWhitespace, IsWhitespace) }, true},
		{"marks.json", func() []boolDelta { return computeBoolDeltas(oldCombiningMark, IsCombiningMark) }, false},
	}

	update := os.Getenv("BAMLUNICODE_UPDATE_DELTAS") == "1"
	baseline := unicode.Version == baselineUnicodeVersion
	if !baseline {
		t.Logf("host go unicode.Version=%s (not the pinned %s baseline): "+
			"skipping delta-set completeness; freshness/non-triviality still enforced",
			unicode.Version, baselineUnicodeVersion)
	}

	for _, m := range scalarManifests {
		fresh := m.compute()
		if update {
			writeJSON(t, deltaPath(m.name), fresh)
			continue
		}
		committed := readScalarDeltas(t, deltaPath(m.name))
		if len(committed) == 0 {
			t.Errorf("%s: empty delta manifest (expected genuine 15->target differences)", m.name)
		}
		for _, d := range committed {
			if d.Old == d.New {
				t.Errorf("%s: U+%04X is not a genuine delta (old==new)", m.name, d.CP)
			}
			if got := hexOf(applyScalar(m.name, d.CP)); got != d.New {
				t.Errorf("%s: U+%04X regressed: bamlunicode now %s, manifest %s", m.name, d.CP, got, d.New)
			}
		}
		if baseline && !equalScalarDeltas(fresh, committed) {
			t.Errorf("%s: delta set changed on the %s baseline; regenerate with BAMLUNICODE_UPDATE_DELTAS=1 (fresh=%d committed=%d)",
				m.name, baselineUnicodeVersion, len(fresh), len(committed))
		}
	}

	for _, m := range boolManifests {
		fresh := m.compute()
		if update {
			writeJSON(t, deltaPath(m.name), fresh)
			continue
		}
		committed := readBoolDeltas(t, deltaPath(m.name))
		if len(committed) == 0 && !m.allowEmpty {
			t.Errorf("%s: empty delta manifest", m.name)
		}
		for _, d := range committed {
			if d.Old == d.New {
				t.Errorf("%s: U+%04X is not a genuine delta (old==new)", m.name, d.CP)
			}
			if got := applyBool(m.name, d.CP); got != d.New {
				t.Errorf("%s: U+%04X regressed: bamlunicode now %v, manifest %v", m.name, d.CP, got, d.New)
			}
		}
		if baseline && !equalBoolDeltas(fresh, committed) {
			t.Errorf("%s: delta set changed on the %s baseline; regenerate with BAMLUNICODE_UPDATE_DELTAS=1 (fresh=%d committed=%d)",
				m.name, baselineUnicodeVersion, len(fresh), len(committed))
		}
	}

	if !update {
		// The #555 headline sentinel must live in the lowercase delta set.
		assertContainsCP(t, deltaPath("lowercase.json"), 0xA7DC)
	}
}

func applyScalar(manifest string, r rune) string {
	if manifest == "nfkd.json" {
		return NFKD(string(r))
	}
	return LowerString(string(r))
}

func applyBool(manifest string, r rune) bool {
	switch manifest {
	case "alphanumeric.json":
		return IsAlphanumeric(r)
	case "whitespace.json":
		return IsWhitespace(r)
	default:
		return IsCombiningMark(r)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
}

func readScalarDeltas(t *testing.T, path string) []scalarDelta {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with BAMLUNICODE_UPDATE_DELTAS=1)", path, err)
	}
	out := []scalarDelta{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

func readBoolDeltas(t *testing.T, path string) []boolDelta {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with BAMLUNICODE_UPDATE_DELTAS=1)", path, err)
	}
	out := []boolDelta{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

func equalScalarDeltas(a, b []scalarDelta) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalBoolDeltas(a, b []boolDelta) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertContainsCP(t *testing.T, path string, cp rune) {
	t.Helper()
	for _, d := range readScalarDeltas(t, path) {
		if d.CP == cp {
			return
		}
	}
	t.Errorf("%s: expected sentinel U+%04X in delta set", path, cp)
}
