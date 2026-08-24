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
	return live, buckets, ex.TestNames
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
		TestNames: syntheticTestNames(map[string][]string{
			repoPrefix + "internal/debaml": {"TestA", "TestB", "TestC", "TestD"},
		}),
	})
	if err := assertCoverage(live, buckets, names); err != nil {
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
				opt.TestNames = syntheticTestNames(tc.names)
			}
			live, buckets, names := bucketsFor(t, tc.store(), opt)
			if err := assertCoverage(live, buckets, names); err != nil {
				t.Fatalf("the undoctored plan already fails the gate: %v", err)
			}

			err := assertCoverage(live, tc.doctor(buckets), names)
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
	if err := assertCoverage(live, buckets, names); err != nil {
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
	err := assertCoverage(live, broken, names)
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

	err := assertCoverage(live, buckets, names)
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
	if err := assertCoverage(live, buckets, names); err == nil {
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
