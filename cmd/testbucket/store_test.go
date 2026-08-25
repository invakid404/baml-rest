package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadStoreTreatsAbsenceAsColdStartNotError(t *testing.T) {
	// The store is a rolling CI cache, not a committed file (owner decision
	// 3): a miss is routine — an expired key, a fork PR, a brand-new repo —
	// and must never fail the job.
	cases := []struct {
		name    string
		write   string
		writeIt bool
		want    string
	}{
		{name: "no file at all", want: "no store at"},
		{name: "empty file", writeIt: true, write: "", want: "is empty"},
		{name: "whitespace only", writeIt: true, write: "\n  \n", want: "is empty"},
		{name: "future schema", writeIt: true, write: `{"schema":99,"units":{}}`, want: "schema 99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test-timings.json")
			if tc.writeIt {
				if err := os.WriteFile(path, []byte(tc.write), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			st, reason, err := loadStore(path)
			if err != nil {
				t.Fatalf("loadStore returned an error for a cold start: %v", err)
			}
			if st != nil {
				t.Errorf("expected no store, got %+v", st)
			}
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason %q does not mention %q", reason, tc.want)
			}
		})
	}
}

func TestLoadStoreRejectsCorruptJSON(t *testing.T) {
	// A truncated or corrupt store is NOT a cold start: silently discarding
	// it would hide a broken cache write behind a merely-worse split.
	path := filepath.Join(t.TempDir(), "test-timings.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"units":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadStore(path); err == nil {
		t.Fatal("expected an error for a corrupt store")
	}
}

func TestStoreSaveIsAtomicAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "test-timings.json")
	st := syntheticStore()
	if err := st.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, reason, err := loadStore(path)
	if err != nil || got == nil {
		t.Fatalf("reload: err=%v reason=%q", err, reason)
	}
	if !reflect.DeepEqual(got.Units, st.Units) {
		t.Errorf("units did not round-trip")
	}
	if got.Flags != st.Flags {
		t.Errorf("flags %q, want %q", got.Flags, st.Flags)
	}

	// No temp file may survive a successful save.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "test-timings.json" {
			t.Errorf("stray file left behind: %s", e.Name())
		}
	}

	// Map keys marshal sorted, so two saves of the same store are
	// byte-identical — the store diffs cleanly across runs.
	first, _ := os.ReadFile(path)
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Error("two saves of the same store produced different bytes")
	}
	var raw map[string]any
	if err := json.Unmarshal(second, &raw); err != nil {
		t.Fatalf("saved store is not valid JSON: %v", err)
	}
}

func TestEWMASmoothsInsteadOfOverwriting(t *testing.T) {
	cases := []struct {
		name     string
		old      float64
		samples  int
		measured float64
		alpha    float64
		want     float64
	}{
		{"first sample is taken verbatim", 0, 0, 900, 0.5, 900},
		{"zero prior with samples still takes measured", 0, 4, 900, 0.5, 900},
		{"half and half", 800, 4, 900, 0.5, 850},
		{"slow alpha barely moves", 800, 4, 900, 0.1, 810},
		{"one slow runner does not rewrite the weight", 900, 12, 1800, 0.5, 1350},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ewma(tc.old, tc.samples, tc.measured, tc.alpha); got != tc.want {
				t.Errorf("ewma(%v,%d,%v,%v) = %v, want %v", tc.old, tc.samples, tc.measured, tc.alpha, got, tc.want)
			}
		})
	}
}

func TestMeanWeightIgnoresDeadRows(t *testing.T) {
	live := syntheticLive()
	st := syntheticStore()
	// A pile of stale rows for deleted packages must not drag the estimate
	// a new package inherits.
	for i := 0; i < 50; i++ {
		st.Units[repoPrefix+"deleted/pkg"+string(rune('a'+i%26))+string(rune('a'+i/26))] = &UnitStat{Seconds: 5000, Samples: 9}
	}
	mean, count, total := st.meanWeight(live)
	if count != len(syntheticWeights) {
		t.Errorf("counted %d measured live packages, want %d", count, len(syntheticWeights))
	}
	wantTotal := 0.0
	for _, v := range syntheticWeights {
		wantTotal += v
	}
	if total != wantTotal {
		t.Errorf("measured total %v, want %v", total, wantTotal)
	}
	if want := wantTotal / float64(len(syntheticWeights)); mean != want {
		t.Errorf("mean %v, want %v", mean, want)
	}
}

func TestMeanWeightFallsBackWhenNothingIsMeasured(t *testing.T) {
	st := newStore(canonicalFlags(true, 100))
	mean, count, total := st.meanWeight(syntheticLive())
	if count != 0 || total != 0 {
		t.Errorf("expected nothing measured, got count=%d total=%v", count, total)
	}
	if mean != defaultColdSeconds {
		t.Errorf("cold mean %v, want %v", mean, defaultColdSeconds)
	}
	// A row that exists but has never been sampled is not a measurement.
	st.Units[repoPrefix+"pool"] = &UnitStat{Seconds: 120, Samples: 0}
	if _, count, _ := st.meanWeight(syntheticLive()); count != 0 {
		t.Errorf("an unsampled row counted as measured")
	}
}

func TestStaleRowsAndCoverageDrift(t *testing.T) {
	live := syntheticLive()
	st := syntheticStore()
	st.Units[repoPrefix+"internal/gone"] = &UnitStat{Seconds: 33, Samples: 3}
	// The store was recorded before internal/schema existed and while
	// internal/removed still did.
	st.Coverage = nil
	for _, p := range live {
		if p.HasTests && p.ImportPath != repoPrefix+"internal/schema" {
			st.Coverage = append(st.Coverage, p.ImportPath)
		}
	}
	st.Coverage = append(st.Coverage, repoPrefix+"internal/removed")

	if got := st.staleRows(live); len(got) != 1 || got[0] != repoPrefix+"internal/gone" {
		t.Errorf("staleRows = %v, want just internal/gone", got)
	}
	added, removed := st.coverageDrift(live)
	if len(added) != 1 || added[0] != repoPrefix+"internal/schema" {
		t.Errorf("added = %v, want internal/schema", added)
	}
	if len(removed) != 1 || removed[0] != repoPrefix+"internal/removed" {
		t.Errorf("removed = %v, want internal/removed", removed)
	}

	// An empty coverage record means "never recorded", not "everything is new".
	fresh := newStore(canonicalFlags(true, 100))
	if a, r := fresh.coverageDrift(live); a != nil || r != nil {
		t.Errorf("unrecorded coverage reported drift: +%v -%v", a, r)
	}
}

func TestCanonicalFlagsIsTheComparabilityKey(t *testing.T) {
	cases := []struct {
		race  bool
		count int
		want  string
	}{
		{true, 100, "-race -count=100"},
		{false, 100, "-count=100"},
		{true, 1, "-race -count=1"},
	}
	for _, tc := range cases {
		if got := canonicalFlags(tc.race, tc.count); got != tc.want {
			t.Errorf("canonicalFlags(%v,%d) = %q, want %q", tc.race, tc.count, got, tc.want)
		}
	}
}

func TestStoreAge(t *testing.T) {
	st := syntheticStore()
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	age, ok := st.age(now)
	if !ok || age != 72*time.Hour {
		t.Errorf("age = %v (ok=%v), want 72h", age, ok)
	}
	st.UpdatedAt = "not a timestamp"
	if _, ok := st.age(now); ok {
		t.Error("unparsable timestamp reported an age")
	}
	st.UpdatedAt = ""
	if _, ok := st.age(now); ok {
		t.Error("missing timestamp reported an age")
	}
}
