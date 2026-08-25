package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

// event renders one `go test -json` line.
func event(action, pkg, test string, elapsed float64) string {
	if test == "" {
		return fmt.Sprintf(`{"Time":"2026-08-25T00:00:00Z","Action":%q,"Package":%q,"Elapsed":%g}`, action, pkg, elapsed)
	}
	return fmt.Sprintf(`{"Time":"2026-08-25T00:00:00Z","Action":%q,"Package":%q,"Test":%q,"Elapsed":%g}`, action, pkg, test, elapsed)
}

func stream(lines ...string) *bytes.Reader {
	return bytes.NewReader([]byte(strings.Join(lines, "\n") + "\n"))
}

// mustIngest runs a merge that is expected to be well-formed.
func mustIngest(t *testing.T, st *Store, sum *eventSummary, opt ingestOptions) *ingestReport {
	t.Helper()
	rep, err := applyIngest(st, sum, opt)
	if err != nil {
		t.Fatalf("applyIngest: %v", err)
	}
	return rep
}

func defaultIngestOptions() ingestOptions {
	return ingestOptions{
		Alpha:  0.5,
		Race:   true,
		Count:  100,
		WhaleK: 6,
		Now:    time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
	}
}

func TestParseEventsAggregatesTheStream(t *testing.T) {
	sum, err := parseEvents(stream(
		// One package split across two count-shards: its weight is the sum.
		event("pass", repoPrefix+"bamlutils/llmhttp", "", 450),
		event("pass", repoPrefix+"bamlutils/llmhttp", "", 450),
		// A parent and its subtest. The parent's Elapsed already covers
		// the child, so only the parent may be weighed.
		event("pass", repoPrefix+"internal/debaml", "TestAlpha", 200),
		event("pass", repoPrefix+"internal/debaml", "TestAlpha/sub", 15),
		event("pass", repoPrefix+"internal/debaml", "TestBeta", 100),
		event("pass", repoPrefix+"internal/debaml", "", 420),
		// A failed package: no fresh weight may be taken from it.
		event("fail", repoPrefix+"pool", "", 12),
		// A package with no test files.
		event("skip", repoPrefix+"cmd/embed", "", 0),
		// Chatter that is not an event at all.
		event("run", repoPrefix+"worker", "TestWorker", 0),
		event("output", repoPrefix+"worker", "TestWorker", 0),
		event("pass", repoPrefix+"worker", "", 110),
	), stream(
		// A second file, as the record job would concatenate bucket artifacts.
		event("pass", repoPrefix+"dynclient", "", 80),
	))
	if err != nil {
		t.Fatalf("parseEvents: %v", err)
	}

	if got := sum.PackageSeconds[repoPrefix+"bamlutils/llmhttp"]; got != 900 {
		t.Errorf("llmhttp = %v, want the 900s sum of both shards", got)
	}
	if got := sum.PackageRuns[repoPrefix+"bamlutils/llmhttp"]; got != 2 {
		t.Errorf("llmhttp ran %d times, want 2", got)
	}
	if got := sum.TestSeconds[repoPrefix+"internal/debaml"]["TestAlpha"]; got != 200 {
		t.Errorf("TestAlpha = %v, want the parent's own 200 (its pass event already covers the subtest)", got)
	}
	if _, ok := sum.TestSeconds[repoPrefix+"internal/debaml"]["TestAlpha/sub"]; ok {
		t.Error("a subtest was weighed as if it were a top-level runnable")
	}
	if sum.Subtests != 1 {
		t.Errorf("counted %d subtest events, want 1", sum.Subtests)
	}
	if !sum.Failed[repoPrefix+"pool"] {
		t.Error("the failed package was not recorded as failed")
	}
	if _, ok := sum.PackageSeconds[repoPrefix+"pool"]; ok {
		t.Error("a failed package contributed a weight")
	}
	if !sum.NoTests[repoPrefix+"cmd/embed"] {
		t.Error("the test-free package was not recorded")
	}
	if got := sum.PackageSeconds[repoPrefix+"dynclient"]; got != 80 {
		t.Errorf("second stream not ingested: dynclient = %v", got)
	}
}

func TestParseEventsToleratesJunkButNotSilence(t *testing.T) {
	sum, err := parseEvents(stream(
		"go: downloading github.com/example/thing v1.2.3",
		"",
		"{not json at all",
		event("pass", repoPrefix+"pool", "", 120),
	))
	if err != nil {
		t.Fatalf("a stray toolchain line cost the whole run's timings: %v", err)
	}
	if sum.Malformed != 2 {
		t.Errorf("counted %d unparsable lines, want 2", sum.Malformed)
	}
	if sum.PackageSeconds[repoPrefix+"pool"] != 120 {
		t.Error("the usable event was lost")
	}

	// A stream with nothing usable means the capture is broken. Writing an
	// unchanged store and exiting 0 would hide that indefinitely.
	if _, err := parseEvents(stream("not json", "still not json")); err == nil {
		t.Error("an unusable stream was accepted")
	}
	// Well-formed chatter with no package result is just as broken — this
	// is what a truncated or mis-redirected capture actually looks like.
	_, err = parseEvents(stream(
		event("run", repoPrefix+"pool", "TestPool", 0),
		event("output", repoPrefix+"pool", "TestPool", 0),
	))
	if err == nil {
		t.Error("a stream with no package results was accepted")
	}
}

func TestApplyIngestSmoothsInsteadOfOverwriting(t *testing.T) {
	st := syntheticStore()
	before := st.Units[repoPrefix+"pool"].Seconds
	sum, err := parseEvents(stream(event("pass", repoPrefix+"pool", "", 200)))
	if err != nil {
		t.Fatal(err)
	}
	rep := mustIngest(t, st, sum, defaultIngestOptions())

	row := st.Units[repoPrefix+"pool"]
	if want := 0.5*200 + 0.5*before; row.Seconds != want {
		t.Errorf("pool = %v, want the EWMA %v", row.Seconds, want)
	}
	if row.Samples != 13 {
		t.Errorf("samples = %d, want 13", row.Samples)
	}
	if len(rep.Updated) != 1 || rep.Updated[0] != repoPrefix+"pool" {
		t.Errorf("report updated = %v", rep.Updated)
	}
	if st.UpdatedAt == "" {
		t.Error("the store was not stamped")
	}
}

func TestApplyIngestKeepsThePriorWeightOnFailure(t *testing.T) {
	// A race-detector abort or a -timeout reports a wall time that measures
	// the failure, not the work. Folding it in would poison the split.
	st := syntheticStore()
	before := st.Units[repoPrefix+"bamlutils/llmhttp"].Seconds
	sum, err := parseEvents(stream(
		event("fail", repoPrefix+"bamlutils/llmhttp", "", 1200),
		event("pass", repoPrefix+"pool", "", 120),
	))
	if err != nil {
		t.Fatal(err)
	}
	rep := mustIngest(t, st, sum, defaultIngestOptions())

	if got := st.Units[repoPrefix+"bamlutils/llmhttp"].Seconds; got != before {
		t.Errorf("llmhttp = %v, want the prior %v kept", got, before)
	}
	if len(rep.SkippedFail) != 1 || rep.SkippedFail[0] != repoPrefix+"bamlutils/llmhttp" {
		t.Errorf("report skipped = %v, want llmhttp named", rep.SkippedFail)
	}
}

func TestApplyIngestFlagsWhalesAndPicksAPolicy(t *testing.T) {
	// Automatic whale detection: a package that alone exceeds total/K sets
	// the makespan, so it must be split before K can buy anything.
	cases := []struct {
		name       string
		perTest    []string
		wantSplit  string
		wantShards int
	}{
		{
			name:       "no per-test data yet: count-shard, which needs none",
			wantSplit:  splitCount,
			wantShards: 6,
		},
		{
			name: "per-test data covering most of the wall-time, no dominant name: upgrade to name slicing",
			perTest: []string{
				// A 6-way count-shard costs 150s; every name fits under
				// that, so packing by name can actually beat it.
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestRetry", 140),
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestSSE", 140),
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestBackoff", 140),
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestParse", 140),
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestRender", 140),
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestEmit", 140),
			},
			wantSplit:  splitRun,
			wantShards: 6,
		},
		{
			name: "per-test data covering most of the wall-time but ONE name dominates: stay on count-sharding",
			perTest: []string{
				// The measured shape of both real whales. 89% of the package
				// is attributable to named tests — the old heuristic's only
				// condition — but TestRetry alone is 44% of it, so no -run
				// split can finish faster than 400s while a 6-way count-shard
				// costs 150s. Name-slicing here is not merely worse, it is
				// the wrong mechanism.
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestRetry", 400),
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestSSE", 300),
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestBackoff", 100),
			},
			wantSplit:  splitCount,
			wantShards: 6,
		},
		{
			name: "per-test data explaining only a sliver: stay on count-sharding",
			perTest: []string{
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestRetry", 20),
				event("pass", repoPrefix+"bamlutils/llmhttp", "TestSSE", 10),
			},
			wantSplit:  splitCount,
			wantShards: 6,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{}
			for dir, sec := range syntheticWeights {
				lines = append(lines, event("pass", repoPrefix+dir, "", sec))
			}
			sort.Strings(lines)
			lines = append(lines, tc.perTest...)
			sum, err := parseEvents(stream(lines...))
			if err != nil {
				t.Fatal(err)
			}
			st := newStore(canonicalFlags(true, 100))
			rep := mustIngest(t, st, sum, defaultIngestOptions())

			row := st.Units[repoPrefix+"bamlutils/llmhttp"]
			if row.Split != tc.wantSplit {
				t.Errorf("llmhttp split = %q, want %q", row.Split, tc.wantSplit)
			}
			// The shard width is K, so a shard fits any bucket by
			// construction and the width does not move when an unrelated
			// package elsewhere gets slower.
			if row.SplitInto != tc.wantShards {
				t.Errorf("llmhttp split_into = %d, want %d", row.SplitInto, tc.wantShards)
			}
			if perShard := row.Seconds / float64(row.SplitInto); perShard > rep.Threshold {
				t.Errorf("each shard is %.1fs, still above the %.1fs threshold", perShard, rep.Threshold)
			}
			if row.SplitInto != 6 {
				t.Errorf("split width %d, want K=6", row.SplitInto)
			}
			// The threshold is total/K: at 2001s over K=6 that is ~333s,
			// so llmhttp (900s) and internal/debaml (420s) are whales and
			// nothing else is.
			if math.Abs(rep.Threshold-sumWeights()/6) > 1e-6 {
				t.Errorf("threshold %v, want total/6 = %v", rep.Threshold, sumWeights()/6)
			}
			whales := map[string]bool{}
			for _, w := range rep.Whales {
				whales[strings.Fields(w)[0]] = true
			}
			if !whales[repoPrefix+"bamlutils/llmhttp"] || !whales[repoPrefix+"internal/debaml"] {
				t.Errorf("whales = %v, want both dominators", rep.Whales)
			}
			if whales[repoPrefix+"pool"] {
				t.Error("a 120s package was flagged as a whale")
			}
			// Per-test rows exist only to serve a split.
			if st.Units[repoPrefix+"pool"].Tests != nil {
				t.Error("per-test rows kept for a non-whale package")
			}
		})
	}
}

func TestApplyIngestWillNotSliceBelowAJobsFixedOverhead(t *testing.T) {
	// A unit can be over the relative threshold (total/K) and still be far
	// too small in absolute terms to slice: every extra slice is another CI
	// job paying checkout, setup and compile. Splitting a 1.5s package six
	// ways would spend minutes of runner time to save milliseconds.
	lines := []string{
		event("pass", repoPrefix+"bamlutils/retry", "", 1.5),
		event("pass", repoPrefix+"internal/apierror", "", 0.5),
		event("pass", repoPrefix+"cmd/testbucket", "", 0.2),
	}
	sum, err := parseEvents(stream(lines...))
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(canonicalFlags(false, 1))
	opt := defaultIngestOptions()
	opt.Race, opt.Count = false, 1
	opt.MinShardSeconds = 30
	rep := mustIngest(t, st, sum, opt)

	// retry IS over total/6 (~0.37s) — the relative rule alone would slice
	// it — but no slice of it could reach 30s.
	if got := st.Units[repoPrefix+"bamlutils/retry"]; got.splitPolicy() != splitNone {
		t.Errorf("a 1.5s package was sliced %q x%d", got.Split, got.SplitInto)
	}
	if len(rep.Whales) != 0 {
		t.Errorf("whales = %v, want none at this scale", rep.Whales)
	}

	// The same relative shape at a real scale still splits.
	big, err := parseEvents(stream(
		event("pass", repoPrefix+"bamlutils/llmhttp", "", 900),
		event("pass", repoPrefix+"pool", "", 300),
		event("pass", repoPrefix+"worker", "", 300),
	))
	if err != nil {
		t.Fatal(err)
	}
	st = newStore(canonicalFlags(true, 100))
	opt = defaultIngestOptions()
	opt.MinShardSeconds = 30
	mustIngest(t, st, big, opt)
	if got := st.Units[repoPrefix+"bamlutils/llmhttp"]; got.splitPolicy() == splitNone {
		t.Error("a 900s whale was left whole")
	}
}

func TestApplyIngestDoesNotStoreZeroWeightTests(t *testing.T) {
	// go test reports 0.00 for sub-millisecond tests; a zero row carries no
	// information and the slicer treats it as unknown regardless, so it
	// would only grow the store.
	sum, err := parseEvents(stream(
		event("pass", repoPrefix+"bamlutils/llmhttp", "", 900),
		event("pass", repoPrefix+"bamlutils/llmhttp", "TestHeavy", 500),
		event("pass", repoPrefix+"bamlutils/llmhttp", "TestInstant", 0),
		event("pass", repoPrefix+"bamlutils/llmhttp", "TestAlsoHeavy", 200),
		event("pass", repoPrefix+"pool", "", 300),
	))
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(canonicalFlags(true, 100))
	mustIngest(t, st, sum, defaultIngestOptions())

	tests := st.Units[repoPrefix+"bamlutils/llmhttp"].Tests
	if _, ok := tests["TestInstant"]; ok {
		t.Error("a zero-weight test was stored")
	}
	if len(tests) != 2 {
		t.Errorf("stored %d per-test rows, want the 2 with real weight: %v", len(tests), tests)
	}
}

func TestApplyIngestUnflagsAPackageThatIsNoLongerAWhale(t *testing.T) {
	// Self-optimizing in both directions: a package that got faster (or a
	// tree that got slower around it) must stop paying split overhead.
	st := syntheticStore()
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200, "TestB": 120})
	sum, err := parseEvents(stream(event("pass", repoPrefix+"internal/debaml", "", 10)))
	if err != nil {
		t.Fatal(err)
	}
	// alpha=1 so the single fast measurement lands in full.
	opt := defaultIngestOptions()
	opt.Alpha = 1
	rep := mustIngest(t, st, sum, opt)

	row := st.Units[repoPrefix+"internal/debaml"]
	if row.splitPolicy() != splitNone {
		t.Errorf("still flagged %q x%d after shrinking to 10s", row.Split, row.SplitInto)
	}
	if row.Tests != nil {
		t.Error("per-test rows survived the un-flagging")
	}
	if len(rep.Unflagged) != 1 {
		t.Errorf("report unflagged = %v", rep.Unflagged)
	}
}

func TestApplyIngestPrunesOnlyAgainstAnAuthoritativeLiveSet(t *testing.T) {
	// Pruning off an event batch alone would delete every package that
	// merely was not part of this batch — e.g. a re-ingest of one bucket.
	events := stream(event("pass", repoPrefix+"pool", "", 120))

	t.Run("no live set: nothing is pruned", func(t *testing.T) {
		st := syntheticStore()
		st.Units[repoPrefix+"internal/deleted"] = &UnitStat{Seconds: 33, Samples: 3}
		sum, err := parseEvents(events)
		if err != nil {
			t.Fatal(err)
		}
		rep := mustIngest(t, st, sum, defaultIngestOptions())
		if len(rep.Pruned) != 0 {
			t.Errorf("pruned %v without an authoritative live set", rep.Pruned)
		}
		if st.Units[repoPrefix+"worker"] == nil {
			t.Error("a package absent from this batch was deleted")
		}
		if rep.CoverageFrom != "go-test-json" {
			t.Errorf("coverage source = %q", rep.CoverageFrom)
		}
	})

	t.Run("authoritative live set: dead rows go", func(t *testing.T) {
		st := syntheticStore()
		st.Units[repoPrefix+"internal/deleted"] = &UnitStat{Seconds: 33, Samples: 3}
		sum, err := parseEvents(stream(event("pass", repoPrefix+"pool", "", 120)))
		if err != nil {
			t.Fatal(err)
		}
		opt := defaultIngestOptions()
		opt.Live = syntheticLive()
		opt.LiveAuthoritative = true
		rep := mustIngest(t, st, sum, opt)

		if len(rep.Pruned) != 1 || rep.Pruned[0] != repoPrefix+"internal/deleted" {
			t.Errorf("pruned = %v, want internal/deleted", rep.Pruned)
		}
		if st.Units[repoPrefix+"worker"] == nil {
			t.Error("a live package absent from this batch was pruned")
		}
		if rep.CoverageFrom != "go-list" || rep.Coverage != len(allTestablePackages()) {
			t.Errorf("coverage = %d from %q, want %d from go-list", rep.Coverage, rep.CoverageFrom, len(allTestablePackages()))
		}
	})
}

func TestApplyIngestResetsWhenTheFlagSetChanges(t *testing.T) {
	st := syntheticStore() // recorded under -race -count=100
	sum, err := parseEvents(stream(event("pass", repoPrefix+"pool", "", 1.2)))
	if err != nil {
		t.Fatal(err)
	}
	opt := defaultIngestOptions()
	opt.Count = 1
	rep := mustIngest(t, st, sum, opt)

	if rep.FlagsReset == "" {
		t.Fatal("a flag-set change was not reported")
	}
	if st.Flags != "-race -count=1" {
		t.Errorf("store flags = %q", st.Flags)
	}
	if st.Units[repoPrefix+"bamlutils/llmhttp"] != nil {
		t.Error("incomparable weights survived the reset")
	}
	if got := st.Units[repoPrefix+"pool"]; got == nil || got.Seconds != 1.2 || got.Samples != 1 {
		t.Errorf("the new measurement was not recorded from scratch: %+v", got)
	}
	var out bytes.Buffer
	_ = rep.write(&out, repoPrefix)
	if !strings.Contains(out.String(), "FLAG SET CHANGED") {
		t.Errorf("the reset was not announced:\n%s", out.String())
	}
}

func TestIngestThenPlanClosesTheLoop(t *testing.T) {
	// End to end, against a synthetic store the whole way: measure, record,
	// re-plan. The second plan must know what the first run learned — that
	// is the entire self-optimizing claim.
	live := syntheticLive()

	// 1. Cold start: no store at all. Everything is estimated, but the
	//    matrix is complete.
	cold := mustPlan(t, nil, "no store at test-timings.json", defaultPlanOptions(live))
	if cold.Summary.Loaded != 0 || cold.Summary.Missing != len(allTestablePackages()) {
		t.Fatalf("cold plan is not fully estimated: %+v", cold.Summary)
	}

	// 2. That run reports its timings.
	var lines []string
	for dir, sec := range syntheticWeights {
		lines = append(lines, event("pass", repoPrefix+dir, "", sec))
	}
	sort.Strings(lines)
	lines = append(lines,
		event("pass", repoPrefix+"bamlutils/llmhttp", "TestRetry", 500),
		event("pass", repoPrefix+"bamlutils/llmhttp", "TestSSE", 300),
	)
	sum, err := parseEvents(stream(lines...))
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(canonicalFlags(true, 100))
	opt := defaultIngestOptions()
	opt.Live = live
	opt.LiveAuthoritative = true
	mustIngest(t, st, sum, opt)

	// 3. The next plan is warm, and the whale has been harpooned.
	planOpt := defaultPlanOptions(live)
	planOpt.Now = time.Date(2026, 8, 25, 13, 0, 0, 0, time.UTC)
	planOpt.Runnables = syntheticRunnables(map[string][]string{
		repoPrefix + "bamlutils/llmhttp": {"TestRetry", "TestSSE"},
		repoPrefix + "internal/debaml":   {"TestParse", "TestRender", "TestEmit"},
	})
	warm := mustPlan(t, st, "", planOpt)

	if warm.Summary.ColdStart {
		t.Errorf("the plan after a record still reports a cold start: %q", warm.Summary.ColdStartReason)
	}
	if warm.Summary.Loaded != len(allTestablePackages()) || warm.Summary.Missing != 0 {
		t.Errorf("warm plan: %d loaded / %d missing, want all loaded", warm.Summary.Loaded, warm.Summary.Missing)
	}
	if math.Abs(warm.Summary.MeasuredSeconds-sumWeights()) > 1e-9 {
		t.Errorf("measured wall-time %v, want %v", warm.Summary.MeasuredSeconds, sumWeights())
	}
	if warm.Summary.ScheduledUnits <= cold.Summary.ScheduledUnits {
		t.Errorf("no whale was split: %d units warm vs %d cold", warm.Summary.ScheduledUnits, cold.Summary.ScheduledUnits)
	}
	// And the makespan actually improved over running the whale whole.
	if warm.Summary.MakespanSeconds >= 900 {
		t.Errorf("makespan %.1fs did not beat the un-split 900s whale", warm.Summary.MakespanSeconds)
	}
	sched := scheduledPackages(warm)
	for _, imp := range allTestablePackages() {
		if sched[imp] == 0 {
			t.Errorf("%s vanished from the warm plan", imp)
		}
	}
	t.Logf("cold makespan %.1fs over %d units -> warm makespan %.1fs over %d units",
		cold.Summary.MakespanSeconds, cold.Summary.ScheduledUnits,
		warm.Summary.MakespanSeconds, warm.Summary.ScheduledUnits)
}

func TestIngestReportNamesWhatItDid(t *testing.T) {
	st := syntheticStore()
	sum, err := parseEvents(stream(
		event("pass", repoPrefix+"pool", "", 130),
		event("pass", repoPrefix+"internal/brandnew", "", 40),
	))
	if err != nil {
		t.Fatal(err)
	}
	rep := mustIngest(t, st, sum, defaultIngestOptions())
	var out bytes.Buffer
	_ = rep.write(&out, repoPrefix)
	text := out.String()
	for _, want := range []string{"packages updated", "packages new", "internal/brandnew", "total measured work", "whale threshold", "coverage recorded"} {
		if !strings.Contains(text, want) {
			t.Errorf("ingest report omits %q:\n%s", want, text)
		}
	}
}

func TestParentAndSubtestTimingIsCountedOnce(t *testing.T) {
	// P1-2 regression, on a realistic `go test -json` fixture: a top-level
	// test that runs three subtests. The parent's own pass event already
	// reports 12.0s covering all of them, so weighing the children on top
	// would record 21.0s for work that took 12.0s.
	//
	// Both whales lean on t.Run, so the inflation is not hypothetical: it
	// distorts slice packing and can push a package over the 50% threshold
	// that promotes count-sharding to -run slicing on time counted twice.
	const fixture = `{"Time":"2026-08-25T00:00:00Z","Action":"run","Package":"example.com/pkg","Test":"TestParent"}
{"Time":"2026-08-25T00:00:00Z","Action":"run","Package":"example.com/pkg","Test":"TestParent/alpha"}
{"Time":"2026-08-25T00:00:03Z","Action":"pass","Package":"example.com/pkg","Test":"TestParent/alpha","Elapsed":3}
{"Time":"2026-08-25T00:00:03Z","Action":"run","Package":"example.com/pkg","Test":"TestParent/beta"}
{"Time":"2026-08-25T00:00:07Z","Action":"pass","Package":"example.com/pkg","Test":"TestParent/beta","Elapsed":4}
{"Time":"2026-08-25T00:00:07Z","Action":"run","Package":"example.com/pkg","Test":"TestParent/beta/nested"}
{"Time":"2026-08-25T00:00:09Z","Action":"pass","Package":"example.com/pkg","Test":"TestParent/beta/nested","Elapsed":2}
{"Time":"2026-08-25T00:00:12Z","Action":"pass","Package":"example.com/pkg","Test":"TestParent","Elapsed":12}
{"Time":"2026-08-25T00:00:12Z","Action":"pass","Package":"example.com/pkg","Test":"ExampleThing","Elapsed":1}
{"Time":"2026-08-25T00:00:13Z","Action":"pass","Package":"example.com/pkg","Elapsed":13}
`
	sum, err := parseEvents(bytes.NewReader([]byte(fixture)))
	if err != nil {
		t.Fatalf("parseEvents: %v", err)
	}
	tests := sum.TestSeconds["example.com/pkg"]
	if got := tests["TestParent"]; got != 12 {
		t.Errorf("TestParent = %v, want the parent's own 12s counted once", got)
	}
	if len(tests) != 2 {
		t.Errorf("weighed %d runnables, want just TestParent and ExampleThing: %v", len(tests), tests)
	}
	if got := tests["ExampleThing"]; got != 1 {
		t.Errorf("ExampleThing = %v, want 1 — examples are runnables too", got)
	}
	if sum.Subtests != 3 {
		t.Errorf("counted %d subtest events, want 3", sum.Subtests)
	}

	// The parent's weight must survive the merge intact, so slice packing
	// balances on real seconds.
	st := newStore(canonicalFlags(true, 100))
	opt := defaultIngestOptions()
	opt.WhaleSeconds = 1 // force the whale path so per-test rows are kept
	mustIngest(t, st, sum, opt)
	row := st.Units["example.com/pkg"]
	if got := row.Tests["TestParent"]; got != 12 {
		t.Errorf("stored TestParent = %v, want 12", got)
	}
	// Named time must not exceed the package's own elapsed; that is the
	// invariant double-counting breaks, and the run-upgrade threshold is a
	// ratio of exactly these two numbers.
	named := 0.0
	for _, v := range row.Tests {
		named += v
	}
	if named > row.Seconds+1e-9 {
		t.Errorf("named time %.2fs exceeds the package's %.2fs — subtest time is being counted twice", named, row.Seconds)
	}
}

func TestSumSecondsIsOrderIndependent(t *testing.T) {
	// P2-1. Float addition is not associative, and the two reductions this
	// helper replaced ran over Go maps, whose iteration order is randomised
	// per process. Near total/K or the 50% upgrade boundary that is enough
	// to choose a different split for byte-identical inputs.
	values := []float64{0.1, 0.2, 0.3}

	// The naive sum really is order-dependent — otherwise this helper would
	// be solving nothing.
	forward := values[0] + values[1] + values[2]
	backward := values[2] + values[1] + values[0]
	if forward == backward {
		t.Fatal("the fixture is not order-sensitive in float; pick different values")
	}

	want := sumSeconds(values)
	if want != 0.6 {
		t.Errorf("sumSeconds = %v, want an exact 0.6", want)
	}
	perms := [][]float64{
		{0.1, 0.2, 0.3}, {0.1, 0.3, 0.2}, {0.2, 0.1, 0.3},
		{0.2, 0.3, 0.1}, {0.3, 0.1, 0.2}, {0.3, 0.2, 0.1},
	}
	for _, p := range perms {
		if got := sumSeconds(p); got != want {
			t.Errorf("sumSeconds(%v) = %v, want %v", p, got, want)
		}
	}

	// Non-finite values cannot poison a whole reduction.
	if got := sumSeconds([]float64{1.5, math.NaN(), math.Inf(1), 2.5}); got != 4.0 {
		t.Errorf("sumSeconds with junk = %v, want 4.0", got)
	}
}

func TestApplyIngestIsStableAcrossRuns(t *testing.T) {
	// The end-to-end form of P2-1: identical events must produce a
	// byte-identical store every time, including the split policy and shard
	// counts that ingest derives from summed weights. Go randomises map
	// iteration order per process AND per range, so repeating the merge
	// inside one test genuinely exercises it.
	var lines []string
	for i := 0; i < 40; i++ {
		// Two-decimal weights, the precision go test -json reports, chosen
		// so the total lands near a shard-count boundary.
		lines = append(lines, event("pass", fmt.Sprintf("%sp%02d", repoPrefix, i), "", 0.07*float64(i%7)+1.13))
	}
	lines = append(lines,
		event("pass", repoPrefix+"whale", "", 121.5),
		event("pass", repoPrefix+"whale", "TestA", 40.5),
		event("pass", repoPrefix+"whale", "TestB", 40.5),
		event("pass", repoPrefix+"whale", "TestC", 20.25),
	)

	var first string
	for run := 0; run < 200; run++ {
		sum, err := parseEvents(stream(lines...))
		if err != nil {
			t.Fatal(err)
		}
		st := newStore(canonicalFlags(true, 100))
		opt := defaultIngestOptions()
		opt.MinShardSeconds = 1
		mustIngest(t, st, sum, opt)
		st.UpdatedAt = "" // provenance only; deliberately wall-clock
		blob, err := json.MarshalIndent(st, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			first = string(blob)
			continue
		}
		if string(blob) != first {
			t.Fatalf("run %d produced a different store:\n--- first ---\n%s\n--- run %d ---\n%s", run, first, run, blob)
		}
	}
}

func TestIngestOptionsValidation(t *testing.T) {
	// P3-1. An out-of-range alpha is the dangerous one: 0 makes the store
	// stop learning forever, negative drives weights negative, and neither
	// surfaces as anything but a mysteriously bad split much later.
	cases := []struct {
		name   string
		mutate func(*ingestOptions)
		wantIn string
	}{
		{"zero count", func(o *ingestOptions) { o.Count = 0 }, "--count"},
		{"negative count", func(o *ingestOptions) { o.Count = -2 }, "--count"},
		{"alpha of zero never learns", func(o *ingestOptions) { o.Alpha = 0 }, "--ewma"},
		{"negative alpha", func(o *ingestOptions) { o.Alpha = -0.5 }, "--ewma"},
		{"alpha above one", func(o *ingestOptions) { o.Alpha = 1.5 }, "--ewma"},
		{"alpha not a number", func(o *ingestOptions) { o.Alpha = math.NaN() }, "--ewma"},
		{"zero whale-k", func(o *ingestOptions) { o.WhaleK = 0 }, "--whale-k"},
		{"negative whale-seconds", func(o *ingestOptions) { o.WhaleSeconds = -1 }, "--whale-seconds"},
		{"infinite whale-seconds", func(o *ingestOptions) { o.WhaleSeconds = math.Inf(1) }, "--whale-seconds"},
		{"negative min-shard-seconds", func(o *ingestOptions) { o.MinShardSeconds = -30 }, "--min-shard-seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sum, err := parseEvents(stream(event("pass", repoPrefix+"pool", "", 120)))
			if err != nil {
				t.Fatal(err)
			}
			opt := defaultIngestOptions()
			tc.mutate(&opt)
			st := syntheticStore()
			before, _ := json.Marshal(st)
			if _, err := applyIngest(st, sum, opt); err == nil {
				t.Fatal("the setting was accepted")
			} else if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not name %s", err.Error(), tc.wantIn)
			}
			after, _ := json.Marshal(st)
			if !bytes.Equal(before, after) {
				t.Error("the store was mutated before the settings were rejected")
			}
		})
	}

	// Alpha of exactly 1 is legal: take the latest measurement verbatim.
	sum, err := parseEvents(stream(event("pass", repoPrefix+"pool", "", 120)))
	if err != nil {
		t.Fatal(err)
	}
	opt := defaultIngestOptions()
	opt.Alpha = 1
	if _, err := applyIngest(syntheticStore(), sum, opt); err != nil {
		t.Errorf("alpha=1 was rejected: %v", err)
	}
}

func TestPerTestRowsSurviveAFailedOrPartialCapture(t *testing.T) {
	// Per-test weights are what `-run` slicing balances on, and a DELETION
	// is not smoothed by EWMA the way a weight is. One aborted or
	// half-uploaded run must not erase them and silently demote a
	// name-sliced whale back to count-sharding on the next plan.
	warm := func() *Store {
		st := newStore(canonicalFlags(true, 100))
		st.Units[repoPrefix+"whale"] = &UnitStat{
			Seconds: 900, Samples: 6, Split: splitRun, SplitInto: 3,
			Tests: map[string]float64{"TestA": 400, "TestB": 300, "TestC": 200},
		}
		st.Units[repoPrefix+"pool"] = &UnitStat{Seconds: 120, Samples: 6}
		return st
	}
	names := func(st *Store) []string {
		return sortedKeys(st.Units[repoPrefix+"whale"].Tests)
	}

	t.Run("a failed package contributes no per-test weight at all", func(t *testing.T) {
		// The package aborted under -race after two of its three tests
		// finished. Its wall time already measures the failure, not the
		// work; its partial test list is no better.
		sum, err := parseEvents(stream(
			event("fail", repoPrefix+"whale", "", 1200),
			event("pass", repoPrefix+"whale", "TestA", 9999),
			event("pass", repoPrefix+"pool", "", 120),
		))
		if err != nil {
			t.Fatal(err)
		}
		st := warm()
		mustIngest(t, st, sum, defaultIngestOptions())

		row := st.Units[repoPrefix+"whale"]
		if got := row.Tests["TestA"]; got != 400 {
			t.Errorf("TestA = %v, want the prior 400 kept; a failed run must not reweight it", got)
		}
		if want := []string{"TestA", "TestB", "TestC"}; strings.Join(names(st), ",") != strings.Join(want, ",") {
			t.Errorf("per-test rows = %v, want %v kept intact", names(st), want)
		}
	})

	t.Run("a partial capture updates weights but prunes nothing", func(t *testing.T) {
		// Only one of the whale's three -run slices uploaded its artifact.
		// The names it did not carry are not deleted tests, they are
		// unreported ones.
		sum, err := parseEvents(stream(
			event("pass", repoPrefix+"whale", "", 300),
			event("pass", repoPrefix+"whale", "TestA", 300),
			event("pass", repoPrefix+"pool", "", 120),
		))
		if err != nil {
			t.Fatal(err)
		}
		st := warm()
		rep := mustIngest(t, st, sum, defaultIngestOptions())

		if want := []string{"TestA", "TestB", "TestC"}; strings.Join(names(st), ",") != strings.Join(want, ",") {
			t.Fatalf("per-test rows = %v, want %v — a partial capture deleted the unreported tests", names(st), want)
		}
		if got := st.Units[repoPrefix+"whale"].Tests["TestA"]; got != 350 { // 0.5*300 + 0.5*400
			t.Errorf("TestA = %v, want the EWMA 350; reported weights should still merge", got)
		}
		if len(rep.PartialCaptures) != 1 {
			t.Fatalf("partial captures = %v, want the shortfall reported", rep.PartialCaptures)
		}
		if !strings.Contains(rep.PartialCaptures[0], "reported 1 of 3") {
			t.Errorf("report does not say how short the batch was: %q", rep.PartialCaptures[0])
		}
		var out bytes.Buffer
		_ = rep.write(&out, repoPrefix)
		if !strings.Contains(out.String(), "partial captures") {
			t.Errorf("the shortfall is not visible in the job log:\n%s", out.String())
		}
	})

	t.Run("a complete capture does prune a deleted test", func(t *testing.T) {
		// All three slices reported and TestC is gone: that is a real
		// deletion or rename, and keeping its weight would misdirect a
		// future slice.
		sum, err := parseEvents(stream(
			event("pass", repoPrefix+"whale", "", 300),
			event("pass", repoPrefix+"whale", "TestA", 300),
			event("pass", repoPrefix+"whale", "", 300),
			event("pass", repoPrefix+"whale", "TestB", 300),
			event("pass", repoPrefix+"whale", "", 300),
			event("pass", repoPrefix+"pool", "", 120),
		))
		if err != nil {
			t.Fatal(err)
		}
		st := warm()
		rep := mustIngest(t, st, sum, defaultIngestOptions())

		if want := []string{"TestA", "TestB"}; strings.Join(names(st), ",") != strings.Join(want, ",") {
			t.Errorf("per-test rows = %v, want %v — a complete capture must prune the deleted test", names(st), want)
		}
		if len(rep.PartialCaptures) != 0 {
			t.Errorf("a complete capture was reported as partial: %v", rep.PartialCaptures)
		}
	})

	t.Run("an un-split package needs only one invocation to count as covered", func(t *testing.T) {
		st := newStore(canonicalFlags(true, 100))
		st.Units[repoPrefix+"whale"] = &UnitStat{
			Seconds: 900, Samples: 6,
			Tests: map[string]float64{"TestA": 400, "TestGone": 300},
		}
		sum, err := parseEvents(stream(
			event("pass", repoPrefix+"whale", "", 900),
			event("pass", repoPrefix+"whale", "TestA", 500),
			event("pass", repoPrefix+"whale", "TestB", 300),
			event("pass", repoPrefix+"pool", "", 120),
		))
		if err != nil {
			t.Fatal(err)
		}
		mustIngest(t, st, sum, defaultIngestOptions())
		if want := []string{"TestA", "TestB"}; strings.Join(names(st), ",") != strings.Join(want, ",") {
			t.Errorf("per-test rows = %v, want %v", names(st), want)
		}
	})
}

func TestExpectedRunsPerSplitPolicy(t *testing.T) {
	cases := []struct {
		policy string
		into   int
		want   int
	}{
		{splitNone, 0, 1},
		{splitNone, 6, 1},
		{splitCount, 6, 6},
		{splitRun, 3, 3},
		// A policy recorded as split but with an incoherent width still
		// expects at least one invocation, never zero — otherwise every
		// batch would count as complete.
		{splitRun, 1, 1},
		{splitCount, 0, 1},
	}
	for _, tc := range cases {
		if got := expectedRuns(tc.policy, tc.into); got != tc.want {
			t.Errorf("expectedRuns(%q,%d) = %d, want %d", tc.policy, tc.into, got, tc.want)
		}
	}
}

func TestImplausibleElapsedIsRejectedNotAbsorbed(t *testing.T) {
	// Elapsed comes from NDJSON that a corrupt or truncated upload can
	// write, so it is untrusted input. A value like 1e300 survives the
	// NaN/Inf filter, and `int64(math.Round(v*1e6))` on it is
	// implementation-defined in Go — which would defeat the exact
	// reproducibility sumSeconds exists to provide and can drive the total,
	// and the whale threshold derived from it, negative.
	t.Run("sumSeconds bounds what it will believe", func(t *testing.T) {
		cases := []struct {
			name   string
			values []float64
			want   float64
		}{
			{"a huge finite value is dropped", []float64{1.5, 1e300, 2.5}, 4.0},
			{"a huge negative value is dropped", []float64{1.5, -1e300, 2.5}, 4.0},
			{"NaN and Inf are still dropped", []float64{1.5, math.NaN(), math.Inf(1), math.Inf(-1), 2.5}, 4.0},
			{"the largest float is dropped", []float64{10, math.MaxFloat64}, 10},
			{"ordinary durations are kept", []float64{900.25, 420.5}, 1320.75},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := sumSeconds(tc.values)
				if got != tc.want {
					t.Errorf("sumSeconds(%v) = %v, want %v", tc.values, got, tc.want)
				}
				if got < 0 {
					t.Errorf("sumSeconds went negative: %v", got)
				}
			})
		}
	})

	t.Run("a corrupt Elapsed never reaches the store", func(t *testing.T) {
		sum, err := parseEvents(stream(
			event("pass", repoPrefix+"pool", "", 1e300),
			event("pass", repoPrefix+"worker", "", 110),
		))
		if err != nil {
			t.Fatal(err)
		}
		if sum.Implausible != 1 {
			t.Errorf("counted %d implausible events, want 1", sum.Implausible)
		}
		if _, ok := sum.PackageSeconds[repoPrefix+"pool"]; ok {
			t.Error("an implausible Elapsed was recorded as a package weight")
		}
		if got := sum.PackageSeconds[repoPrefix+"worker"]; got != 110 {
			t.Errorf("the healthy event was lost: worker = %v", got)
		}

		st := newStore(canonicalFlags(true, 100))
		rep := mustIngest(t, st, sum, defaultIngestOptions())
		if st.Units[repoPrefix+"pool"] != nil {
			t.Errorf("the corrupt package reached the store: %+v", st.Units[repoPrefix+"pool"])
		}
		if rep.TotalSeconds < 0 {
			t.Errorf("total measured work went negative: %v", rep.TotalSeconds)
		}
		if rep.Threshold < 0 {
			t.Errorf("whale threshold went negative: %v", rep.Threshold)
		}
		// The corruption must be visible, not silently swallowed.
		if rep.Implausible != 1 {
			t.Errorf("report implausible = %d, want 1", rep.Implausible)
		}
		var out bytes.Buffer
		if err := rep.write(&out, repoPrefix); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "implausible Elapsed") {
			t.Errorf("the corrupt capture is not visible in the job log:\n%s", out.String())
		}
	})
}

func TestChooseSplitPolicyComparesTheTwoMechanisms(t *testing.T) {
	// The decision the Phase B measurement forced. Name-slicing's makespan can
	// never fall below the single heaviest runnable — pack the other names
	// however you like, the slice holding the dominant one still has to run
	// it. Count-sharding divides ITERATIONS and does not care about the
	// package's internal shape at all. So the only honest test is the two
	// costs against each other, not a coverage percentage on its own.
	cases := []struct {
		name       string
		pkg        float64
		named      float64
		heaviest   float64
		namedCount int
		shards     int
		want       string
		reasonHas  string
	}{
		{
			name: "bamlutils/llmhttp as measured: 96% named, but one name is 50%",
			// A -run split floors at 407s; a 3-way count-shard costs 271s.
			pkg: 814, named: 782, heaviest: 407.2, namedCount: 221, shards: 3,
			want: splitCount, reasonHas: "dominated by one runnable",
		},
		{
			name: "internal/debaml as measured: 92% named, but one name is 44%",
			pkg:  822, named: 754, heaviest: 361.1, namedCount: 457, shards: 3,
			want: splitCount, reasonHas: "dominated by one runnable",
		},
		{
			name: "genuinely name-divisible: a long tail with no dominant name",
			pkg:  900, named: 850, heaviest: 250, namedCount: 4, shards: 3,
			want: splitRun, reasonHas: "name-divisible",
		},
		{
			name: "exactly at the boundary: the heaviest name equals the count-shard cost",
			// Ties go to name-slicing: it is no worse here and it avoids
			// repeating the package's fixed per-binary setup S times.
			pkg: 900, named: 850, heaviest: 300, namedCount: 4, shards: 3,
			want: splitRun, reasonHas: "name-divisible",
		},
		{
			name: "one iota over the boundary flips it",
			pkg:  900, named: 850, heaviest: 300.01, namedCount: 4, shards: 3,
			want: splitCount, reasonHas: "dominated by one runnable",
		},
		{
			name: "too little per-test data to pack with, however flat it looks",
			pkg:  900, named: 100, heaviest: 30, namedCount: 5, shards: 3,
			want: splitCount, reasonHas: "explain only",
		},
		{
			name: "a single named runnable is not a test list",
			pkg:  900, named: 880, heaviest: 880, namedCount: 1, shards: 3,
			want: splitCount, reasonHas: "fewer than two",
		},
		{
			name: "more shards make the count-shard cheaper and raise the bar for slicing",
			// The SAME package that sliced at S=3 no longer does at S=6: a
			// 6-way count-shard costs 150s, under the 250s dominant name.
			pkg: 900, named: 850, heaviest: 250, namedCount: 4, shards: 6,
			want: splitCount, reasonHas: "dominated by one runnable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := chooseSplitPolicy(tc.pkg, tc.named, tc.heaviest, tc.namedCount, tc.shards)
			if got != tc.want {
				t.Errorf("policy = %q, want %q (reason: %s)", got, tc.want, reason)
			}
			if !strings.Contains(reason, tc.reasonHas) {
				t.Errorf("reason %q does not mention %q", reason, tc.reasonHas)
			}
		})
	}
}

func TestBothRealWhalesSelectCountSharding(t *testing.T) {
	// End to end through applyIngest with the two whales' MEASURED shapes,
	// because the policy that matters is the one the store actually records.
	//
	// The old named-coverage heuristic chose `run` for both of these — they
	// are 96% and 92% named — and `run` is the mechanism the Phase B
	// measurement showed cannot help either of them.
	whale := func(pkg string, total float64, tests map[string]float64) []string {
		lines := []string{event("pass", repoPrefix+pkg, "", total)}
		for _, n := range sortedKeys(tests) {
			lines = append(lines, event("pass", repoPrefix+pkg, n, tests[n]))
		}
		return lines
	}
	var lines []string
	lines = append(lines, whale("bamlutils/llmhttp", 814, map[string]float64{
		"TestExactStreamNoGoroutineLeak":                 407.2,
		"TestExactStreamIdleTimeoutResetsOnEveryByte":    66.0,
		"TestIdleTimeoutNeverFalseKillsAtBoundary":       52.4,
		"TestExactStreamConcurrentCloseCancelSecondCall": 52.3,
		"TestExecuteStreamContextCancellation":           20.0,
		"TestTail":                                       182.1,
	})...)
	lines = append(lines, whale("internal/debaml", 822, map[string]float64{
		"TestConstraintStateCollectorIsTestOnly":          361.1,
		"TestPromotedIntegralFloatCompositionsAreBounded": 109.2,
		"TestServingOracleIsTestOnly":                     86.1,
		"TestStaticCheckedCutoverHasNoRuntimeWriter":      74.3,
		"TestPhase3cRegression_JSONDeepNestingUnchanged":  60.9,
		"TestTail": 62.4,
	})...)
	// Plankton, so the whale threshold (total/K) is realistic.
	for i := 0; i < 20; i++ {
		lines = append(lines, event("pass", fmt.Sprintf("%splankton%02d", repoPrefix, i), "", 30))
	}
	sum, err := parseEvents(stream(lines...))
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(canonicalFlags(true, 100))
	rep := mustIngest(t, st, sum, defaultIngestOptions())

	for _, pkg := range []string{"bamlutils/llmhttp", "internal/debaml"} {
		row := st.Units[repoPrefix+pkg]
		if row == nil {
			t.Fatalf("%s is not in the store", pkg)
		}
		if row.Split != splitCount {
			t.Errorf("%s selected %q x%d (%s), want %q — a -run split floors at its dominant name",
				pkg, row.Split, row.SplitInto, row.SplitReason, splitCount)
		}
		if row.SplitInto != 6 {
			t.Errorf("%s split into %d, want the K=6 width", pkg, row.SplitInto)
		}
		if !strings.Contains(row.SplitReason, "dominated by one runnable") {
			t.Errorf("%s reason %q does not record the dominance", pkg, row.SplitReason)
		}
		// Each shard must actually come in under what the un-split package
		// costs by the width the store chose.
		perShard := row.Seconds / float64(row.SplitInto)
		if perShard >= row.Seconds {
			t.Errorf("%s per-shard %.1fs is no better than the whole %.1fs", pkg, perShard, row.Seconds)
		}
		t.Logf("%s: %.1fs -> %s x%d, %.1fs per shard (%s)", pkg, row.Seconds, row.Split, row.SplitInto, perShard, row.SplitReason)
	}
	if len(rep.Dominated) != 2 {
		t.Errorf("report named %d dominated packages, want both whales: %v", len(rep.Dominated), rep.Dominated)
	}
	var out bytes.Buffer
	if err := rep.write(&out, repoPrefix); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "one runnable dominates") {
		t.Errorf("the dominance is not visible in the job log:\n%s", out.String())
	}
}

func TestWhaleReportReDerivesTheDominanceShares(t *testing.T) {
	// The reproducibility half of the divisibility evidence: whatever a
	// measurement report claims about a whale's internal distribution must be
	// re-derivable from the store itself, by anyone, on any machine — and in
	// particular from the store artifact a master run uploads, which is CI's
	// own numbers rather than one laptop's.
	st := newStore(canonicalFlags(true, 100))
	st.UpdatedAt = "2026-08-25T00:00:00Z"
	st.CoverageSource = "go-list"
	st.Units[repoPrefix+"bamlutils/llmhttp"] = &UnitStat{
		Seconds: 814, Samples: 3, Split: splitCount, SplitInto: 3,
		SplitReason: "dominated by one runnable (407.2s > the 271.3s a 3-way count-shard costs)",
		Tests: map[string]float64{
			"TestExactStreamNoGoroutineLeak":              407.2,
			"TestExactStreamIdleTimeoutResetsOnEveryByte": 66.0,
			"TestIdleTimeoutNeverFalseKillsAtBoundary":    52.4,
		},
	}
	for i := 0; i < 10; i++ {
		st.Units[fmt.Sprintf("%splankton%02d", repoPrefix, i)] = &UnitStat{Seconds: 40, Samples: 3}
	}

	var out bytes.Buffer
	writeWhaleReport(&out, st, 6, 3, false)
	text := out.String()

	for _, want := range []string{
		"bamlutils/llmhttp",
		"heaviest runnable",
		"TestExactStreamNoGoroutineLeak",
		// The two costs the policy turns on, side by side.
		"count-shard",
		"-run slice",
		"floor",
		"dominated by one runnable",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("whale report omits %q:\n%s", want, text)
		}
	}

	// The dominance share must be stated, not left to be computed: 407.2/814
	// is 50.0%, and that single number is what decides the mechanism.
	if !strings.Contains(text, "50.0%") {
		t.Errorf("the dominance share is not reported:\n%s", text)
	}
	// And the two mechanism costs must both appear so the comparison is
	// checkable rather than asserted: 814/3 = 271.3s against a 407.2s floor.
	if !strings.Contains(text, "271.3s") || !strings.Contains(text, "407.2s") {
		t.Errorf("the mechanism comparison is not reproducible from the output:\n%s", text)
	}

	// Plankton must not be listed: the report is about split candidates.
	if strings.Contains(text, "plankton") {
		t.Errorf("an unsplit package was reported as a whale:\n%s", text)
	}

	// --all opens it up, for auditing a store that has not flagged anything.
	var everything bytes.Buffer
	writeWhaleReport(&everything, st, 6, 3, true)
	if !strings.Contains(everything.String(), "plankton") {
		t.Errorf("--all did not widen the report:\n%s", everything.String())
	}

	// A store with nothing flagged says so rather than printing an empty table.
	var empty bytes.Buffer
	writeWhaleReport(&empty, newStore(canonicalFlags(true, 100)), 6, 3, false)
	if !strings.Contains(empty.String(), "nothing has crossed the split threshold") {
		t.Errorf("an empty store produced no explanation:\n%s", empty.String())
	}
}
