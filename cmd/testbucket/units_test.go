package main

import (
	"math"
	"sort"
	"strings"
	"testing"
)

func expandFor(t *testing.T, st *Store, opt expandOptions) *expansion {
	t.Helper()
	live := syntheticLive()
	if opt.K == 0 {
		opt.K = 6
	}
	if opt.BaseCount == 0 {
		opt.BaseCount = 100
	}
	if opt.MeanSeconds == 0 {
		opt.MeanSeconds, _, _ = st.meanWeight(live)
	}
	ex, err := expandUnits(live, st, opt)
	if err != nil {
		t.Fatalf("expandUnits: %v", err)
	}
	return ex
}

func unitByID(ex *expansion, id string) (Unit, bool) {
	for _, u := range ex.Units {
		if u.ID == id {
			return u, true
		}
	}
	return Unit{}, false
}

func unitsForPackage(ex *expansion, imp string) []Unit {
	var out []Unit
	for _, u := range ex.Units {
		for _, p := range u.Packages {
			if p.ImportPath == imp {
				out = append(out, u)
				break
			}
		}
	}
	return out
}

func TestExpandUnitsDefaultsToWholePackages(t *testing.T) {
	ex := expandFor(t, syntheticStore(), expandOptions{})

	// Every testable package is scheduled exactly once, and the one package
	// without test files is not scheduled at all.
	for _, p := range syntheticLive() {
		got := unitsForPackage(ex, p.ImportPath)
		switch {
		case !p.HasTests && len(got) != 0:
			t.Errorf("%s has no test files but got %d units", p.ImportPath, len(got))
		case p.HasTests && len(got) != 1:
			t.Errorf("%s scheduled by %d units, want 1", p.ImportPath, len(got))
		}
	}
	u, ok := unitByID(ex, repoPrefix+"pool")
	if !ok {
		t.Fatal("pool has no unit")
	}
	if u.Kind != kindPackage || u.Seconds != 120 || u.Count != 100 || u.Estimate {
		t.Errorf("pool unit = %+v, want a measured 120s whole package at -count=100", u)
	}
	if len(ex.Missing) != 0 {
		t.Errorf("nothing should be estimated against a full store, got %v", ex.Missing)
	}
	if ex.MeasuredSeconds != sumWeights() {
		t.Errorf("measured total %v, want %v", ex.MeasuredSeconds, sumWeights())
	}
}

func TestExpandUnitsColdStartUsesTheMeanWeight(t *testing.T) {
	// THE INVARIANT, cold-start half: a package the store has never heard
	// of is scheduled immediately on the mean weight. It does not wait for
	// a record job, and it is never left out.
	cases := []struct {
		name        string
		store       *Store
		wantMissing []string
		wantMean    float64
	}{
		{
			name:        "empty store: everything is estimated",
			store:       newStore(canonicalFlags(true, 100)),
			wantMissing: allTestablePackages(),
			wantMean:    defaultColdSeconds,
		},
		{
			name:        "two brand-new packages inherit the mean of the rest",
			store:       syntheticStore("pool", "internal/schema"),
			wantMissing: []string{repoPrefix + "internal/schema", repoPrefix + "pool"},
			wantMean:    (sumWeights() - 120 - 25) / float64(len(syntheticWeights)-2),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mean, _, _ := tc.store.meanWeight(syntheticLive())
			if math.Abs(mean-tc.wantMean) > 1e-9 {
				t.Fatalf("mean weight %v, want %v", mean, tc.wantMean)
			}
			ex := expandFor(t, tc.store, expandOptions{MeanSeconds: mean})

			sort.Strings(tc.wantMissing)
			if strings.Join(ex.Missing, ",") != strings.Join(tc.wantMissing, ",") {
				t.Errorf("estimated packages\n got  %v\n want %v", ex.Missing, tc.wantMissing)
			}
			for _, imp := range tc.wantMissing {
				units := unitsForPackage(ex, imp)
				if len(units) == 0 {
					t.Fatalf("%s has no recorded timing and was NOT scheduled — the invariant is broken", imp)
				}
				for _, u := range units {
					if !u.Estimate {
						t.Errorf("%s unit %s is not marked estimated", imp, u.ID)
					}
				}
			}
			// Every live testable package is still covered.
			if got := len(ex.Loaded) + len(ex.Missing); got != len(allTestablePackages()) {
				t.Errorf("accounted for %d packages, want %d", got, len(allTestablePackages()))
			}
		})
	}
}

func TestExpandUnitsCountShardsAWhale(t *testing.T) {
	// The zero-data harpoon: divide -count instead of the test list. It
	// needs no per-test weights, which is what makes it available on day
	// one for a package the store knows nothing about internally.
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	ex := expandFor(t, st, expandOptions{})

	shards := unitsForPackage(ex, repoPrefix+"bamlutils/llmhttp")
	if len(shards) != 6 {
		t.Fatalf("got %d shards, want 6", len(shards))
	}
	seenIdx := map[int]bool{}
	total := 0.0
	for _, u := range shards {
		if u.Kind != kindCountShard {
			t.Errorf("%s kind = %s, want %s", u.ID, u.Kind, kindCountShard)
		}
		if want := countShardID(repoPrefix+"bamlutils/llmhttp", u.Shard); u.ID != want {
			t.Errorf("shard ID %q, want %q", u.ID, want)
		}
		if u.Count != 17 { // ceil(100/6)
			t.Errorf("shard %d runs -count=%d, want 17", u.Shard, u.Count)
		}
		if u.Shards != 6 {
			t.Errorf("shard %d reports %d shards, want 6", u.Shard, u.Shards)
		}
		if seenIdx[u.Shard] {
			t.Errorf("shard index %d emitted twice", u.Shard)
		}
		seenIdx[u.Shard] = true
		total += u.Seconds
	}
	for i := 1; i <= 6; i++ {
		if !seenIdx[i] {
			t.Errorf("shard index %d missing", i)
		}
	}
	if math.Abs(total-900) > 1e-9 {
		t.Errorf("shard weights sum to %v, want the package's 900s", total)
	}
	// Coverage-equivalence in aggregate: 6 x -count=17 = 102 >= 100. The
	// sweep may run slightly MORE iterations than the unsharded job, never
	// fewer — the direction that cannot lose coverage.
	if got := 6 * 17; got < 100 {
		t.Errorf("aggregate -count %d is below the un-split 100", got)
	}
	if !strings.Contains(strings.Join(ex.Notes, "\n"), "count-shard") {
		t.Errorf("no count-shard note emitted: %v", ex.Notes)
	}
}

func TestExpandUnitsRunSlicesABigPackage(t *testing.T) {
	// The principled harpoon: pack test NAMES into slices by their recorded
	// per-test weight.
	names := []string{"TestAlpha", "TestBeta", "TestGamma", "TestDelta", "TestEpsilon"}
	st := syntheticStore()
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{
		"TestAlpha": 200, "TestBeta": 100, "TestGamma": 60, "TestDelta": 40, "TestEpsilon": 20,
	})
	ex := expandFor(t, st, expandOptions{
		TestNames: syntheticTestNames(map[string][]string{repoPrefix + "internal/debaml": names}),
	})

	slices := unitsForPackage(ex, repoPrefix+"internal/debaml")
	if len(slices) != 3 {
		t.Fatalf("got %d slices, want 3", len(slices))
	}
	seen := map[string]int{}
	total := 0.0
	for _, u := range slices {
		if u.Kind != kindRunSlice {
			t.Errorf("%s kind = %s, want %s", u.ID, u.Kind, kindRunSlice)
		}
		if u.Count != 100 {
			t.Errorf("%s runs -count=%d; name slicing must not change the sweep depth", u.ID, u.Count)
		}
		if len(u.Run) == 0 {
			t.Errorf("%s has an empty -run set", u.ID)
		}
		if want := runSliceID(repoPrefix+"internal/debaml", u.Run); u.ID != want {
			t.Errorf("slice ID %q, want %q", u.ID, want)
		}
		for _, n := range u.Run {
			seen[n]++
		}
		total += u.Seconds
	}
	for _, n := range names {
		if seen[n] != 1 {
			t.Errorf("test %s appears in %d slices, want exactly 1", n, seen[n])
		}
	}
	if math.Abs(total-420) > 1e-9 {
		t.Errorf("slice weights sum to %v, want the package's 420s", total)
	}
	if ex.TestNames[repoPrefix+"internal/debaml"] == nil {
		t.Error("the live test-name list was not recorded for the gate to check")
	}
}

func TestRunSliceSchedulesTestsTheStoreHasNeverSeen(t *testing.T) {
	// The subtle way a -run split can silently drop a test: a test added
	// since the last record has no per-test weight. It must still be packed
	// into a slice — weighted by the package's residual per-test average —
	// or it would simply never run, with nothing going red.
	live := []string{"TestOld1", "TestOld2", "TestBrandNew"}
	st := syntheticStore()
	harpoon(st, "internal/debaml", splitRun, 2, map[string]float64{
		"TestOld1": 200, "TestOld2": 100,
	})
	ex := expandFor(t, st, expandOptions{
		TestNames: syntheticTestNames(map[string][]string{repoPrefix + "internal/debaml": live}),
	})

	seen := map[string]int{}
	for _, u := range unitsForPackage(ex, repoPrefix+"internal/debaml") {
		for _, n := range u.Run {
			seen[n]++
		}
	}
	for _, n := range live {
		if seen[n] != 1 {
			t.Errorf("test %s scheduled %d times, want exactly 1", n, seen[n])
		}
	}
	// The unknown test carries the residual (420 - 300) rather than zero,
	// so it cannot be treated as free and stuffed anywhere.
	var newWeight float64
	for _, u := range unitsForPackage(ex, repoPrefix+"internal/debaml") {
		if len(u.Run) == 1 && u.Run[0] == "TestBrandNew" {
			newWeight = u.Seconds
		}
	}
	if newWeight != 0 && math.Abs(newWeight-120) > 1e-9 {
		t.Errorf("brand-new test weighted %v, want the 120s residual", newWeight)
	}
}

func TestRunSliceWeightingFallbacks(t *testing.T) {
	// When the recorded per-test weights already exceed the package weight
	// (drift, or a shrunk package), the residual is negative and must not
	// become a negative or zero weight for the unknown tests.
	st := syntheticStore()
	harpoon(st, "internal/debaml", splitRun, 2, map[string]float64{
		"TestHuge": 900,
	})
	ex := expandFor(t, st, expandOptions{
		TestNames: syntheticTestNames(map[string][]string{
			repoPrefix + "internal/debaml": {"TestHuge", "TestUnknown"},
		}),
	})
	for _, u := range unitsForPackage(ex, repoPrefix+"internal/debaml") {
		if u.Seconds <= 0 {
			t.Errorf("slice %s got a non-positive weight %v", u.ID, u.Seconds)
		}
	}
}

func TestExpandUnitsPacksGoworkOffModulesWhole(t *testing.T) {
	// The soft module boundary made hard where correctness needs it: a
	// GOWORK=off module resolves against a different build list, so its
	// packages cannot share an invocation with workspace-mode ones. They
	// ride together as one atom.
	ex := expandFor(t, syntheticStore(), expandOptions{})

	atom, ok := unitByID(ex, moduleAtomID("adapters/common"))
	if !ok {
		t.Fatal("adapters/common was not packed as a module atom")
	}
	if atom.Kind != kindModuleAtom || atom.Mode != modeOff {
		t.Errorf("atom = %+v, want a GOWORK=off module atom", atom)
	}
	if len(atom.Packages) != 2 {
		t.Errorf("atom covers %d packages, want 2", len(atom.Packages))
	}
	if math.Abs(atom.Seconds-56) > 1e-9 { // 12 + 44
		t.Errorf("atom weight %v, want the module's 56s", atom.Seconds)
	}
	// Workspace packages, by contrast, stay individually schedulable and
	// mix freely across module lines.
	for _, imp := range []string{repoPrefix + "bamlutils/llmhttp", repoPrefix + "pool", repoPrefix + "worker"} {
		u, ok := unitByID(ex, imp)
		if !ok {
			t.Fatalf("%s is not its own unit", imp)
		}
		if u.Mode != modeWork {
			t.Errorf("%s mode = %s, want %s", imp, u.Mode, modeWork)
		}
	}
}

func TestSplitOfAnAtomicModuleIsSuppressedLoudly(t *testing.T) {
	// An atomic module cannot be sub-split without breaking its invocation
	// envelope. Suppressing the split is correct; suppressing it SILENTLY
	// would hide a whale that no K can then beat.
	st := syntheticStore()
	harpoon(st, "adapters/common/codegen", splitCount, 4, nil)
	ex := expandFor(t, st, expandOptions{})

	if units := unitsForPackage(ex, repoPrefix+"adapters/common/codegen"); len(units) != 1 || units[0].Kind != kindModuleAtom {
		t.Errorf("codegen was split despite living in an atomic module: %+v", units)
	}
	joined := strings.Join(ex.Notes, "\n")
	if !strings.Contains(joined, "suppressed") || !strings.Contains(joined, "adapters/common") {
		t.Errorf("no suppression note emitted:\n%s", joined)
	}
}

func TestExpandUnitsFailsLoudlyWhenTestNamesCannotBeResolved(t *testing.T) {
	// If the store says "slice this by name" and the name list cannot be
	// obtained, falling back to a whole-package run would quietly undo the
	// harpoon and blow the makespan. Refusing is the correct outcome.
	st := syntheticStore()
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 100})

	if _, err := expandUnits(syntheticLive(), st, expandOptions{K: 6, BaseCount: 100, MeanSeconds: 60}); err == nil {
		t.Error("expected an error when no test-name resolver is configured")
	}

	failing := func(p LivePackage) ([]string, error) { return nil, errBoom }
	_, err := expandUnits(syntheticLive(), st, expandOptions{K: 6, BaseCount: 100, MeanSeconds: 60, TestNames: failing})
	if err == nil || !strings.Contains(err.Error(), "internal/debaml") {
		t.Errorf("err = %v, want a loud failure naming internal/debaml", err)
	}
}

func TestClampShardsAndCeilDiv(t *testing.T) {
	cases := []struct{ want, into, k int }{
		{2, 0, 6}, {2, 1, 6}, {3, 3, 6}, {6, 6, 6}, {6, 12, 6}, {8, 8, 8},
	}
	for _, tc := range cases {
		if got := clampShards(tc.into, tc.k); got != tc.want {
			t.Errorf("clampShards(%d,%d) = %d, want %d", tc.into, tc.k, got, tc.want)
		}
	}
	div := []struct{ a, b, want int }{{100, 6, 17}, {100, 4, 25}, {100, 1, 100}, {100, 0, 100}, {0, 6, 1}}
	for _, tc := range div {
		if got := ceilDiv(tc.a, tc.b); got != tc.want {
			t.Errorf("ceilDiv(%d,%d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSplittingIsSuppressedAtKOfOne(t *testing.T) {
	// One bucket means no parallelism to buy, so every extra slice is pure
	// added compile time. Suppress, and say so.
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	ex := expandFor(t, st, expandOptions{K: 1})

	units := unitsForPackage(ex, repoPrefix+"bamlutils/llmhttp")
	if len(units) != 1 || units[0].Kind != kindPackage {
		t.Errorf("llmhttp split at K=1: %+v", units)
	}
	if !strings.Contains(strings.Join(ex.Notes, "\n"), "suppressed") {
		t.Errorf("no suppression note: %v", ex.Notes)
	}
}

func TestPackagePatternIsRelativeToItsModule(t *testing.T) {
	cases := []struct {
		pkg  LivePackage
		want string
	}{
		{livePkg("adapters/common/codegen", "adapters/common", modeOff, true), "./codegen"},
		{livePkg("adapters/common", "adapters/common", modeOff, true), "."},
		{livePkg("internal/debaml", ".", modeWork, true), "./internal/debaml"},
		{livePkg("bamlutils/llmhttp", "bamlutils", modeWork, true), "./llmhttp"},
	}
	for _, tc := range cases {
		if got := tc.pkg.pattern(); got != tc.want {
			t.Errorf("%s pattern = %q, want %q", tc.pkg.ImportPath, got, tc.want)
		}
	}
}

func sumWeights() float64 {
	t := 0.0
	for _, v := range syntheticWeights {
		t += v
	}
	return t
}

func allTestablePackages() []string {
	var out []string
	for _, p := range syntheticLive() {
		if p.HasTests {
			out = append(out, p.ImportPath)
		}
	}
	sort.Strings(out)
	return out
}

var errBoom = errTestOnly("go test -list exploded")

type errTestOnly string

func (e errTestOnly) Error() string { return string(e) }
