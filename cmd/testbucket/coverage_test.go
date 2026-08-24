package main

import (
	"strings"
	"testing"
)

// bucketsFor runs the real expander + partitioner and hands back the buckets
// the gate would see. Tests then doctor those buckets to model each way a
// test could go missing.
func bucketsFor(t *testing.T, st *Store, opt expandOptions) ([]LivePackage, []Bucket, map[string][]string) {
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
	items := make([]Item, 0, len(ex.Units))
	byID := map[string]Unit{}
	for _, u := range ex.Units {
		items = append(items, u.item())
		byID[u.ID] = u
	}
	groups := karmarkarKarp(items, opt.K)
	buckets := make([]Bucket, opt.K)
	for i, g := range groups {
		b := Bucket{Index: i}
		for _, it := range g {
			u := byID[it.ID]
			b.Units = append(b.Units, u)
			b.Seconds += u.Seconds
		}
		buckets[i] = b
	}
	return live, buckets, ex.Runnables
}

// gate runs the real coverage gate with the synthetic tree's base -count,
// so every test exercises the aggregate-sweep check alongside the rest.
func gate(live []LivePackage, buckets []Bucket, runnables map[string][]string) error {
	return assertCoverage(gateInput{
		Live:      live,
		Buckets:   buckets,
		Runnables: runnables,
		BaseCount: 100,
	})
}

// dropUnit removes a unit from the plan — the fault the gate exists to catch.
func dropUnit(buckets []Bucket, pred func(Unit) bool) []Bucket {
	out := make([]Bucket, len(buckets))
	for i, b := range buckets {
		nb := Bucket{Index: b.Index}
		for _, u := range b.Units {
			if pred(u) {
				continue
			}
			nb.Units = append(nb.Units, u)
			nb.Seconds += u.Seconds
		}
		out[i] = nb
	}
	return out
}

func mapUnits(buckets []Bucket, f func(Unit) Unit) []Bucket {
	out := make([]Bucket, len(buckets))
	for i, b := range buckets {
		nb := Bucket{Index: b.Index}
		for _, u := range b.Units {
			nb.Units = append(nb.Units, f(u))
		}
		out[i] = nb
	}
	return out
}

func TestCoverageGatePassesOnAWellFormedPlan(t *testing.T) {
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200, "TestB": 120, "TestC": 60})
	live, buckets, names := bucketsFor(t, st, expandOptions{
		Runnables: syntheticRunnables(map[string][]string{
			repoPrefix + "internal/debaml": {"TestA", "TestB", "TestC", "TestD"},
		}),
	})
	if err := gate(live, buckets, names); err != nil {
		t.Fatalf("gate rejected a well-formed plan: %v", err)
	}
}

func TestCoverageGateCatchesEveryWayATestCanVanish(t *testing.T) {
	// THE INVARIANT. Each case injects one concrete way a live test could
	// end up unscheduled and asserts the gate refuses to emit a matrix and
	// names the casualty. A balanced-but-incomplete split is the one
	// failure mode that never goes red on its own.
	cases := []struct {
		name    string
		store   func() *Store
		names   map[string][]string
		doctor  func([]Bucket) []Bucket
		wantIn  []string
		wantNot []string
	}{
		{
			name:   "a whole package is assigned to no bucket",
			store:  func() *Store { return syntheticStore() },
			doctor: func(b []Bucket) []Bucket { return dropUnit(b, func(u Unit) bool { return u.ID == repoPrefix+"pool" }) },
			wantIn: []string{"assigned to no bucket", repoPrefix + "pool"},
		},
		{
			name:  "a GOWORK=off module atom is dropped, taking both its packages",
			store: func() *Store { return syntheticStore() },
			doctor: func(b []Bucket) []Bucket {
				return dropUnit(b, func(u Unit) bool { return u.Kind == kindModuleAtom })
			},
			wantIn: []string{repoPrefix + "adapters/common", repoPrefix + "adapters/common/codegen"},
		},
		{
			name: "one count-shard of a whale goes missing",
			store: func() *Store {
				st := syntheticStore()
				harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
				return st
			},
			doctor: func(b []Bucket) []Bucket {
				return dropUnit(b, func(u Unit) bool { return u.Kind == kindCountShard && u.Shard == 3 })
			},
			wantIn: []string{"missing shard 3"},
			// The package itself is still covered by its other five shards,
			// so only the shard-level check can catch this.
			wantNot: []string{"assigned to no bucket"},
		},
		{
			name: "a count-shard is scheduled twice",
			store: func() *Store {
				st := syntheticStore()
				harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
				return st
			},
			doctor: func(b []Bucket) []Bucket {
				for i := range b {
					for _, u := range b[i].Units {
						if u.Kind == kindCountShard && u.Shard == 2 {
							b[(i+1)%len(b)].Units = append(b[(i+1)%len(b)].Units, u)
							return b
						}
					}
				}
				return b
			},
			wantIn: []string{"more than one bucket"},
		},
		{
			name: "a -run slice quietly loses a test name",
			store: func() *Store {
				st := syntheticStore()
				harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200, "TestB": 120, "TestC": 60})
				return st
			},
			names: map[string][]string{repoPrefix + "internal/debaml": {"TestA", "TestB", "TestC"}},
			doctor: func(b []Bucket) []Bucket {
				return mapUnits(b, func(u Unit) Unit {
					if u.Kind == kindRunSlice {
						var kept []string
						for _, n := range u.Run {
							if n != "TestB" {
								kept = append(kept, n)
							}
						}
						u.Run = kept
					}
					return u
				})
			},
			wantIn: []string{"in no -run slice", "TestB"},
		},
		{
			name: "a test name lands in two -run slices",
			store: func() *Store {
				st := syntheticStore()
				harpoon(st, "internal/debaml", splitRun, 2, map[string]float64{"TestA": 200, "TestB": 120})
				return st
			},
			names: map[string][]string{repoPrefix + "internal/debaml": {"TestA", "TestB"}},
			doctor: func(b []Bucket) []Bucket {
				return mapUnits(b, func(u Unit) Unit {
					if u.Kind == kindRunSlice {
						u.Run = []string{"TestA", "TestB"}
					}
					return u
				})
			},
			wantIn: []string{"more than one -run slice"},
		},
		{
			name: "a -run slice ends up empty",
			store: func() *Store {
				st := syntheticStore()
				harpoon(st, "internal/debaml", splitRun, 2, map[string]float64{"TestA": 200, "TestB": 120})
				return st
			},
			names: map[string][]string{repoPrefix + "internal/debaml": {"TestA", "TestB"}},
			doctor: func(b []Bucket) []Bucket {
				return mapUnits(b, func(u Unit) Unit {
					if u.Kind == kindRunSlice {
						u.Run = nil
					}
					return u
				})
			},
			wantIn: []string{"empty -run set"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt := expandOptions{}
			if tc.names != nil {
				opt.Runnables = syntheticRunnables(tc.names)
			}
			live, buckets, names := bucketsFor(t, tc.store(), opt)
			if err := gate(live, buckets, names); err != nil {
				t.Fatalf("the undoctored plan already fails the gate: %v", err)
			}

			err := gate(live, tc.doctor(buckets), names)
			if err == nil {
				t.Fatal("the gate PASSED a plan that drops a live test — the invariant is not enforced")
			}
			msg := err.Error()
			for _, want := range tc.wantIn {
				if !strings.Contains(msg, want) {
					t.Errorf("gate message does not mention %q:\n%s", want, msg)
				}
			}
			for _, notWant := range tc.wantNot {
				if strings.Contains(msg, notWant) {
					t.Errorf("gate message unexpectedly mentions %q:\n%s", notWant, msg)
				}
			}
			if !strings.Contains(msg, "coverage gate FAILED") {
				t.Errorf("gate message is not self-identifying:\n%s", msg)
			}
		})
	}
}

func TestCoverageGateIgnoresPackagesWithNoTestFiles(t *testing.T) {
	// A package with no _test.go files is not a test unit; demanding a
	// bucket for it would make every plan fail for no reason.
	live, buckets, names := bucketsFor(t, syntheticStore(), expandOptions{})
	if err := gate(live, buckets, names); err != nil {
		t.Fatalf("gate rejected a plan over a tree containing a test-free package: %v", err)
	}
	for _, b := range buckets {
		for _, u := range b.Units {
			for _, p := range u.Packages {
				if p.ImportPath == repoPrefix+"cmd/embed" {
					t.Error("a package with no test files was scheduled")
				}
			}
		}
	}
}

func TestCoverageGateReportsEveryCasualtyNotJustTheFirst(t *testing.T) {
	// A gate that names one victim costs the reader a bisect.
	live, buckets, names := bucketsFor(t, syntheticStore(), expandOptions{})
	broken := dropUnit(buckets, func(u Unit) bool {
		return u.ID == repoPrefix+"pool" || u.ID == repoPrefix+"worker" || u.ID == repoPrefix+"dynclient"
	})
	err := gate(live, broken, names)
	if err == nil {
		t.Fatal("gate passed a plan missing three packages")
	}
	for _, want := range []string{"pool", "worker", "dynclient", "3 live package(s)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("gate message omits %q:\n%s", want, err.Error())
		}
	}
}

func TestCoverageGateCatchesAPackageScheduledTwice(t *testing.T) {
	// The mirror image of a dropped test: a package covered by two tiers at
	// once runs twice. It never goes red, it just silently costs the
	// wall-time the whole exercise exists to save.
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	live, buckets, names := bucketsFor(t, st, expandOptions{})

	whole := Unit{
		ID:       repoPrefix + "bamlutils/llmhttp",
		Kind:     kindPackage,
		Seconds:  900,
		Packages: []LivePackage{livePkg("bamlutils/llmhttp", "bamlutils", modeWork, true)},
		Module:   "bamlutils",
		Mode:     modeWork,
		Count:    100,
	}
	buckets[0].Units = append(buckets[0].Units, whole)

	err := gate(live, buckets, names)
	if err == nil {
		t.Fatal("the gate passed a plan that runs llmhttp both whole and sharded")
	}
	for _, want := range []string{"would run twice", "bamlutils/llmhttp", "count-shard", "package"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("gate message omits %q:\n%s", want, err.Error())
		}
	}
}

func TestCoverageGateCatchesADuplicatedWholePackage(t *testing.T) {
	live, buckets, names := bucketsFor(t, syntheticStore(), expandOptions{})
	for _, u := range buckets[0].Units {
		if u.Kind == kindPackage {
			dup := u
			dup.ID = u.ID + " (copy)" // a distinct ID, so only the per-package check can see it
			buckets[1].Units = append(buckets[1].Units, dup)
			break
		}
	}
	if err := gate(live, buckets, names); err == nil {
		t.Fatal("the gate passed a plan running one package as two separate units")
	}
}

func TestDisplayIDCollapsesLongRunSlices(t *testing.T) {
	id := runSliceID(repoPrefix+"internal/debaml", []string{"TestA", "TestB", "TestC"})
	if got := displayID(id, repoPrefix); got != "internal/debaml[3 tests]" {
		t.Errorf("displayID = %q", got)
	}
	if got := displayID(repoPrefix+"pool", repoPrefix); got != "pool" {
		t.Errorf("displayID mangled a plain package: %q", got)
	}
	if got := displayID(moduleAtomID("adapters/common"), repoPrefix); got != "mod:adapters/common" {
		t.Errorf("displayID mangled a module atom: %q", got)
	}
}

func TestPlanNotesWhenKExceedsTheWork(t *testing.T) {
	// K=6 lanes for 3 units means three jobs paying checkout and setup to
	// run nothing. Still a valid plan — but say so.
	live := []LivePackage{
		livePkg("pool", "pool", modeWork, true),
		livePkg("worker", "worker", modeWork, true),
	}
	opt := defaultPlanOptions(live)
	doc, err := buildPlan(newStore(canonicalFlags(true, 100)), "", opt)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	joined := strings.Join(doc.Notes, "\n")
	if !strings.Contains(joined, "empty") {
		t.Errorf("no empty-bucket note:\n%s", joined)
	}
	if len(doc.Buckets) != 6 {
		t.Errorf("got %d buckets, want K=6 regardless", len(doc.Buckets))
	}
}

// mixedRunnables is the universe of a package that has more than just
// Test funcs. `go test -run` selects tests, examples AND fuzz targets, so
// all of these must land in a slice.
var mixedRunnables = []string{
	"ExampleClient",
	"ExampleClient_stream",
	"FuzzDecode",
	"TestRetry",
	"TestSSE",
}

func TestRunSlicingCoversExamplesAndFuzzTargetsNotJustTests(t *testing.T) {
	// P0-1 regression. `-run` selects tests, examples and fuzz targets
	// alike. If the runnable universe is enumerated as only `Test*`, a
	// package's ExampleXxx lands in no slice, no slice names it, and it
	// silently never runs behind a completely green matrix.
	st := syntheticStore()
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{
		"TestRetry": 200, "TestSSE": 120,
	})
	live, buckets, runnables := bucketsFor(t, st, expandOptions{
		Runnables: syntheticRunnables(map[string][]string{
			repoPrefix + "internal/debaml": mixedRunnables,
		}),
	})

	if err := gate(live, buckets, runnables); err != nil {
		t.Fatalf("gate rejected a plan covering the full runnable set: %v", err)
	}

	// Every runnable — Example and Fuzz included — is in exactly one slice.
	seen := map[string]int{}
	for _, b := range buckets {
		for _, u := range b.Units {
			for _, n := range u.Run {
				seen[n]++
			}
		}
	}
	for _, n := range mixedRunnables {
		if seen[n] != 1 {
			t.Errorf("%s is in %d slices, want exactly 1", n, seen[n])
		}
	}
	if len(seen) != len(mixedRunnables) {
		t.Errorf("slices name %d runnables, want %d: %v", len(seen), len(mixedRunnables), seen)
	}

	// And the gate must refuse a plan that covers only the Test* half —
	// the exact shape a `-list ^Test` universe would have produced.
	testsOnly := mapUnits(buckets, func(u Unit) Unit {
		if u.Kind != kindRunSlice {
			return u
		}
		var kept []string
		for _, n := range u.Run {
			if strings.HasPrefix(n, "Test") {
				kept = append(kept, n)
			}
		}
		u.Run = kept
		return u
	})
	err := gate(live, testsOnly, runnables)
	if err == nil {
		t.Fatal("the gate passed a plan whose -run slices skip every Example and Fuzz target")
	}
	for _, want := range []string{"ExampleClient", "ExampleClient_stream", "FuzzDecode", "in no -run slice"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("gate message omits %q:\n%s", want, err.Error())
		}
	}
}

func TestIsRunnableMatchesGoTestRunSemantics(t *testing.T) {
	// The universe filter must mirror `go test -run`, which selects tests,
	// examples and fuzz targets — and NOT benchmarks (-bench does those).
	// A Benchmark name inside a slice's alternation would cover nothing
	// while claiming a weight the slicer balances around.
	cases := []struct {
		name string
		want bool
	}{
		{"TestRetry", true},
		{"ExampleClient", true},
		{"ExampleClient_stream", true},
		{"FuzzDecode", true},
		{"BenchmarkEncode", false},
		{"helper", false},
	}
	for _, tc := range cases {
		if got := isRunnable(tc.name); got != tc.want {
			t.Errorf("isRunnable(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCountShardGateCatchesTheLostFinalShard(t *testing.T) {
	// P0-2 regression. Deriving the group size from the highest index
	// PRESENT cannot notice that the last shard is gone: 1..5 of six is
	// contiguous, the package is still "scheduled", and 85 of the requested
	// 100 iterations quietly never run.
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	live, buckets, runnables := bucketsFor(t, st, expandOptions{})
	if err := gate(live, buckets, runnables); err != nil {
		t.Fatalf("the undoctored plan already fails: %v", err)
	}

	highest := 0
	for _, b := range buckets {
		for _, u := range b.Units {
			if u.Kind == kindCountShard && u.Shard > highest {
				highest = u.Shard
			}
		}
	}
	if highest != 6 {
		t.Fatalf("expected a six-shard group, highest index is %d", highest)
	}
	broken := dropUnit(buckets, func(u Unit) bool { return u.Kind == kindCountShard && u.Shard == highest })

	err := gate(live, broken, runnables)
	if err == nil {
		t.Fatal("the gate passed a six-shard group missing its last shard")
	}
	msg := err.Error()
	// Both independent witnesses must fire: the index check against the
	// shards' own claimed width, and the aggregate sweep against -count.
	for _, want := range []string{"missing shard 6 of 6", "below the requested -count=100", "85 iterations"} {
		if !strings.Contains(msg, want) {
			t.Errorf("gate message omits %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "assigned to no bucket") {
		t.Errorf("the package itself is still covered by its other shards; only the shard checks should fire:\n%s", msg)
	}
}

func TestCountShardGateCatchesAnUndersizedSweep(t *testing.T) {
	// A shard group can be structurally perfect — 1..N, each once — and
	// still run a fraction of the requested flake sweep if the per-shard
	// -count is wrong. Coverage of the PACKAGE is not coverage of the SWEEP.
	cases := []struct {
		name   string
		doctor func(Unit) Unit
		wantIn []string
	}{
		{
			name: "each shard runs too few iterations",
			doctor: func(u Unit) Unit {
				if u.Kind == kindCountShard {
					u.Count = 5 // 6 x 5 = 30, far short of 100
				}
				return u
			},
			wantIn: []string{"30 iterations in aggregate", "-count=100"},
		},
		{
			name: "a shard runs zero iterations",
			doctor: func(u Unit) Unit {
				if u.Kind == kindCountShard && u.Shard == 2 {
					u.Count = 0
				}
				return u
			},
			wantIn: []string{"-count=0"},
		},
		{
			name: "the shards disagree about how many there are",
			doctor: func(u Unit) Unit {
				if u.Kind == kindCountShard && u.Shard == 4 {
					u.Shards = 9
				}
				return u
			},
			wantIn: []string{"disagree on the group size"},
		},
		{
			name: "a shard index sits outside the group",
			doctor: func(u Unit) Unit {
				if u.Kind == kindCountShard && u.Shard == 4 {
					u.Shard = 11
				}
				return u
			},
			wantIn: []string{"missing shard 4 of 6", "shard 11 outside the 1..6 group"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := syntheticStore()
			harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
			live, buckets, runnables := bucketsFor(t, st, expandOptions{})
			err := gate(live, mapUnits(buckets, tc.doctor), runnables)
			if err == nil {
				t.Fatal("the gate passed a shard group that does not run the requested sweep")
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("gate message omits %q:\n%s", want, err.Error())
				}
			}
		})
	}
}

func TestCountShardAggregateSweepIsAtLeastTheBaseCount(t *testing.T) {
	// The healthy direction: S x ceil(base/S) >= base for every S, so the
	// sharded sweep may run slightly MORE iterations than the un-split job,
	// never fewer.
	for _, shards := range []int{2, 3, 4, 5, 6, 7, 8} {
		st := syntheticStore()
		harpoon(st, "bamlutils/llmhttp", splitCount, shards, nil)
		live, buckets, runnables := bucketsFor(t, st, expandOptions{K: 8})
		if err := assertCoverage(gateInput{Live: live, Buckets: buckets, Runnables: runnables, BaseCount: 100}); err != nil {
			t.Errorf("shards=%d: %v", shards, err)
		}
		aggregate := 0
		for _, b := range buckets {
			for _, u := range b.Units {
				if u.Kind == kindCountShard {
					aggregate += u.Count
				}
			}
		}
		if aggregate < 100 {
			t.Errorf("shards=%d: aggregate -count %d is below 100", shards, aggregate)
		}
	}
}

func TestGateSeparatesStoreMissFromPlanMiss(t *testing.T) {
	// P0-3, stated as one executable contract so the two can never be
	// conflated again:
	//
	//   missing from the STORE -> scheduled on the cold-start mean weight,
	//                             no error. This is required by the brief.
	//   missing from the PLAN  -> hard error. This is THE invariant.
	live := syntheticLive()
	storeMissing := syntheticStore("pool", "internal/schema")

	doc, err := buildPlan(storeMissing, "", defaultPlanOptions(live))
	if err != nil {
		t.Fatalf("a store miss must not be an error, the brief requires it be scheduled on the mean: %v", err)
	}
	sched := scheduledPackages(doc)
	for _, imp := range []string{repoPrefix + "pool", repoPrefix + "internal/schema"} {
		if sched[imp] != 1 {
			t.Errorf("%s is absent from the store and was scheduled %d times, want 1", imp, sched[imp])
		}
	}

	// The same two packages, now missing from the emitted plan, must error.
	_, buckets, runnables := bucketsFor(t, storeMissing, expandOptions{})
	broken := dropUnit(buckets, func(u Unit) bool {
		return u.ID == repoPrefix+"pool" || u.ID == repoPrefix+"internal/schema"
	})
	if err := gate(live, broken, runnables); err == nil {
		t.Fatal("a package missing from the FINAL PLAN did not error")
	}
}

// findUnit returns the first unit in the plan matching pred, so a test can
// doctor a REAL unit rather than hand-build one that may not resemble what
// the expander actually emits.
func findUnit(buckets []Bucket, pred func(Unit) bool) (int, Unit) {
	for i, b := range buckets {
		for _, u := range b.Units {
			if pred(u) {
				return i, u
			}
		}
	}
	return -1, Unit{}
}

func replaceUnit(buckets []Bucket, id string, with Unit) []Bucket {
	return mapUnits(buckets, func(u Unit) Unit {
		if u.ID == id {
			return with
		}
		return u
	})
}

func TestCoverageGateRejectsMalformedFinalUnits(t *testing.T) {
	// P0-4 regression. The gate is the declared backstop for a defect in
	// the expander or the partitioner, so it must REJECT shapes that normal
	// expansion happens not to produce today — the whole point of a
	// backstop is that it does not trust the thing it backs up.
	//
	// The common thread: a sub-package unit carries per-invocation
	// arguments (one -run regex, one divided -count) computed for exactly
	// one package. renderBucket applies them to every package in the unit,
	// so any other shape emits a command that does not run what the unit
	// claims to cover.
	cases := []struct {
		name    string
		store   func() *Store
		names   map[string][]string
		doctor  func(*testing.T, []Bucket, []LivePackage) []Bucket
		wantIn  []string
		wantNot []string
	}{
		{
			name:  "an ordinary package is converted into a run-slice",
			store: func() *Store { return syntheticStore() },
			doctor: func(t *testing.T, b []Bucket, live []LivePackage) []Bucket {
				// pool was never name-sliced, so nothing ever resolved its
				// runnable set. Emitting -run '^(TestOne)$' for it would
				// silently skip every other test, example and fuzz target
				// it has — and the package still looks "scheduled".
				_, u := findUnit(b, func(u Unit) bool { return u.ID == repoPrefix+"pool" })
				if u.ID == "" {
					t.Fatal("no pool unit to doctor")
				}
				u.Kind = kindRunSlice
				u.Run = []string{"TestOne"}
				return replaceUnit(b, repoPrefix+"pool", u)
			},
			wantIn: []string{
				"name-sliced with no resolved runnable universe",
				"never resolved its runnable set",
				repoPrefix + "pool",
				"cannot be proved complete",
			},
		},
		{
			name: "a second package is folded into a legitimate run-slice",
			store: func() *Store {
				st := syntheticStore()
				harpoon(st, "internal/debaml", splitRun, 2, map[string]float64{"TestA": 200, "TestB": 120})
				return st
			},
			names: map[string][]string{repoPrefix + "internal/debaml": {"TestA", "TestB"}},
			doctor: func(t *testing.T, b []Bucket, live []LivePackage) []Bucket {
				// worker rides along inside a debaml slice. Only the first
				// package's names are checked, and the first package's
				// regex is what actually gets passed — so worker would run
				// whichever of its own tests happen to be named TestA/TestB,
				// and nothing else.
				idx, slice := findUnit(b, func(u Unit) bool { return u.Kind == kindRunSlice })
				if idx < 0 {
					t.Fatal("no run-slice to doctor")
				}
				var worker LivePackage
				for _, p := range live {
					if p.ImportPath == repoPrefix+"worker" {
						worker = p
					}
				}
				slice.Packages = append(append([]LivePackage(nil), slice.Packages...), worker)
				// Drop worker's own unit, so it is covered ONLY by the
				// malformed slice: exactly the expander bug being modelled.
				withoutWorker := dropUnit(b, func(u Unit) bool { return u.ID == repoPrefix+"worker" })
				return replaceUnit(withoutWorker, slice.ID, slice)
			},
			wantIn: []string{
				"over 2 packages",
				"must cover exactly 1",
				repoPrefix + "worker",
				// And because a malformed unit credits nothing, worker is
				// correctly reported as unscheduled rather than silently
				// counted as covered.
				"assigned to no bucket",
			},
		},
		{
			name: "a count-shard carries two packages",
			store: func() *Store {
				st := syntheticStore()
				harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
				return st
			},
			doctor: func(t *testing.T, b []Bucket, live []LivePackage) []Bucket {
				idx, shard := findUnit(b, func(u Unit) bool { return u.Kind == kindCountShard })
				if idx < 0 {
					t.Fatal("no count-shard to doctor")
				}
				var pool LivePackage
				for _, p := range live {
					if p.ImportPath == repoPrefix+"pool" {
						pool = p
					}
				}
				shard.Packages = append(append([]LivePackage(nil), shard.Packages...), pool)
				return replaceUnit(b, shard.ID, shard)
			},
			wantIn: []string{"count-shard over 2 packages", "must cover exactly 1"},
		},
		{
			name:  "a run-slice carries no package at all",
			store: func() *Store { return syntheticStore() },
			doctor: func(t *testing.T, b []Bucket, live []LivePackage) []Bucket {
				_, u := findUnit(b, func(u Unit) bool { return u.ID == repoPrefix+"pool" })
				u.Kind = kindRunSlice
				u.Run = []string{"TestOne"}
				u.Packages = nil
				return replaceUnit(b, repoPrefix+"pool", u)
			},
			// This is the case that used to panic at Packages[0] instead of
			// returning the coverage error the gate promises.
			wantIn: []string{"run-slice over 0 packages", "must cover exactly 1", "assigned to no bucket", repoPrefix + "pool"},
		},
		{
			name:  "a count-shard carries no package at all",
			store: func() *Store { return syntheticStore() },
			doctor: func(t *testing.T, b []Bucket, live []LivePackage) []Bucket {
				_, u := findUnit(b, func(u Unit) bool { return u.ID == repoPrefix+"pool" })
				u.Kind = kindCountShard
				u.Shard, u.Shards, u.Count = 1, 2, 50
				u.Packages = nil
				return replaceUnit(b, repoPrefix+"pool", u)
			},
			wantIn: []string{"count-shard over 0 packages", "must cover exactly 1"},
		},
		{
			name:  "an ordinary package unit carries no package at all",
			store: func() *Store { return syntheticStore() },
			doctor: func(t *testing.T, b []Bucket, live []LivePackage) []Bucket {
				_, u := findUnit(b, func(u Unit) bool { return u.ID == repoPrefix+"pool" })
				u.Packages = nil
				return replaceUnit(b, repoPrefix+"pool", u)
			},
			wantIn: []string{"covering no package at all", "assigned to no bucket"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt := expandOptions{}
			if tc.names != nil {
				opt.Runnables = syntheticRunnables(tc.names)
			}
			live, buckets, runnables := bucketsFor(t, tc.store(), opt)
			if err := gate(live, buckets, runnables); err != nil {
				t.Fatalf("the undoctored plan already fails the gate: %v", err)
			}

			// The gate must return an error, never panic, on any of these.
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("the gate PANICKED instead of reporting a coverage error: %v", r)
					}
				}()
				err = gate(live, tc.doctor(t, buckets, live), runnables)
			}()
			if err == nil {
				t.Fatal("the gate PASSED a malformed final unit — the backstop is not a backstop")
			}
			msg := err.Error()
			for _, want := range tc.wantIn {
				if !strings.Contains(msg, want) {
					t.Errorf("gate message omits %q:\n%s", want, msg)
				}
			}
			for _, notWant := range tc.wantNot {
				if strings.Contains(msg, notWant) {
					t.Errorf("gate message unexpectedly mentions %q:\n%s", notWant, msg)
				}
			}
			if !strings.Contains(msg, "coverage gate FAILED") {
				t.Errorf("gate message is not self-identifying:\n%s", msg)
			}
		})
	}
}

func TestMalformedUnitCreditsNothingAsScheduled(t *testing.T) {
	// The mechanism behind the P0-4 fix, stated on its own: a unit the gate
	// cannot vouch for must not mark its packages covered. Crediting it
	// would let a package look scheduled by a unit that cannot actually run
	// it — the exact illusion this gate exists to break.
	live := syntheticLive()
	pool := livePkg("pool", "pool", modeWork, true)
	worker := livePkg("worker", "worker", modeWork, true)

	// One bucket, one unit, claiming to cover both packages under a single
	// -run regex computed for whichever one sorts first.
	buckets := []Bucket{{
		Index: 0,
		Units: []Unit{{
			ID:       runSliceID(pool.ImportPath, []string{"TestA"}),
			Kind:     kindRunSlice,
			Packages: []LivePackage{pool, worker},
			Run:      []string{"TestA"},
			Count:    100,
		}},
	}}
	err := assertCoverage(gateInput{
		Live:      live,
		Buckets:   buckets,
		Runnables: map[string][]string{pool.ImportPath: {"TestA"}},
		BaseCount: 100,
	})
	if err == nil {
		t.Fatal("the gate passed a two-package run-slice")
	}
	// Both packages must be reported unscheduled: neither is credited by a
	// unit the gate refused.
	for _, want := range []string{pool.ImportPath, worker.ImportPath, "assigned to no bucket", "must cover exactly 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("gate message omits %q:\n%s", want, err.Error())
		}
	}
}

func TestRunSliceOverAnEmptyRunnableSetIsRejected(t *testing.T) {
	// "No evidence" must not read as "passed": a slice held to an empty
	// universe has nothing to be proved complete against.
	live := syntheticLive()
	pool := livePkg("pool", "pool", modeWork, true)
	buckets := []Bucket{{
		Index: 0,
		Units: []Unit{{
			ID:       runSliceID(pool.ImportPath, []string{"TestA"}),
			Kind:     kindRunSlice,
			Packages: []LivePackage{pool},
			Run:      []string{"TestA"},
			Count:    100,
		}},
	}}
	err := assertCoverage(gateInput{
		Live:      live,
		Buckets:   buckets,
		Runnables: map[string][]string{pool.ImportPath: nil}, // resolved, but empty
		BaseCount: 100,
	})
	if err == nil {
		t.Fatal("the gate passed a run-slice with nothing to check it against")
	}
	if !strings.Contains(err.Error(), "empty runnable set") {
		t.Errorf("gate message omits the empty-universe reason:\n%s", err.Error())
	}
}
