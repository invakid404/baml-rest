package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestBuildPlanColdStartStillProducesACompleteMatrix(t *testing.T) {
	// Cold start is the NORMAL case for a rolling cache: an expired key, a
	// fork PR, a fresh repo. The matrix it produces must still be valid and
	// complete — only its balance is worse.
	cases := []struct {
		name       string
		store      *Store
		reason     string
		wantReason string
	}{
		{"store missing entirely", nil, "no store at test-timings.json", "no store at"},
		{"store present but empty of measurements", newStore(canonicalFlags(true, 100)), "", "no measurement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := syntheticLive()
			doc := mustPlan(t, tc.store, tc.reason, defaultPlanOptions(live))

			if !doc.Summary.ColdStart {
				t.Error("summary does not report a cold start")
			}
			if !strings.Contains(doc.Summary.ColdStartReason, tc.wantReason) {
				t.Errorf("cold-start reason %q does not mention %q", doc.Summary.ColdStartReason, tc.wantReason)
			}
			if doc.Summary.Loaded != 0 {
				t.Errorf("%d packages reported as loaded on a cold start", doc.Summary.Loaded)
			}
			if doc.Summary.Missing != len(allTestablePackages()) {
				t.Errorf("%d packages estimated, want all %d", doc.Summary.Missing, len(allTestablePackages()))
			}
			if doc.Summary.MeanSeconds != defaultColdSeconds {
				t.Errorf("mean weight %v, want the %v cold default", doc.Summary.MeanSeconds, defaultColdSeconds)
			}

			// Every live testable package is scheduled exactly once — the
			// gate already asserted it, this pins the observable result.
			sched := scheduledPackages(doc)
			for _, imp := range allTestablePackages() {
				if sched[imp] != 1 {
					t.Errorf("%s scheduled %d times, want 1", imp, sched[imp])
				}
			}
			if len(doc.Buckets) != 6 {
				t.Fatalf("got %d buckets, want K=6", len(doc.Buckets))
			}
			// With every weight equal the split degenerates to an
			// equal-count one, which is exactly the intended behaviour.
			counts := map[int]bool{}
			for _, b := range doc.Buckets {
				counts[len(b.Units)] = true
			}
			if len(counts) > 2 {
				t.Errorf("cold-start bucket sizes vary by more than one: %v", counts)
			}

			var out bytes.Buffer
			_ = doc.writeSummary(&out, repoPrefix)
			if !strings.Contains(out.String(), "COLD START") {
				t.Errorf("the summary does not announce the cold start:\n%s", out.String())
			}
		})
	}
}

func TestBuildPlanLoadedVsMissingSummary(t *testing.T) {
	// Owner decision 3's rot mitigation: the summary is the only thing that
	// makes a silently stale or half-populated store visible.
	live := syntheticLive()
	st := syntheticStore("pool", "internal/schema")
	st.Units[repoPrefix+"internal/deleted"] = &UnitStat{Seconds: 77, Samples: 4}
	st.Coverage = append(st.Coverage, repoPrefix+"internal/deleted")

	doc := mustPlan(t, st, "", defaultPlanOptions(live))
	s := doc.Summary

	if s.LivePackages != len(allTestablePackages()) {
		t.Errorf("live packages %d, want %d", s.LivePackages, len(allTestablePackages()))
	}
	if s.Loaded != len(syntheticWeights)-2 {
		t.Errorf("loaded %d, want %d", s.Loaded, len(syntheticWeights)-2)
	}
	if s.Missing != 2 {
		t.Errorf("missing %d, want 2", s.Missing)
	}
	wantMeasured := sumWeights() - 120 - 25
	if math.Abs(s.MeasuredSeconds-wantMeasured) > 1e-9 {
		t.Errorf("measured wall-time %v, want %v", s.MeasuredSeconds, wantMeasured)
	}
	if math.Abs(s.EstimatedSeconds-2*s.MeanSeconds) > 1e-9 {
		t.Errorf("estimated %v, want 2 x the %v mean", s.EstimatedSeconds, s.MeanSeconds)
	}
	if math.Abs(s.TotalSeconds-(s.MeasuredSeconds+s.EstimatedSeconds)) > 1e-9 {
		t.Errorf("total %v is not measured+estimated", s.TotalSeconds)
	}
	if len(s.StaleRows) != 1 || s.StaleRows[0] != repoPrefix+"internal/deleted" {
		t.Errorf("stale rows %v, want internal/deleted", s.StaleRows)
	}
	if len(s.DriftRemoved) != 1 || s.DriftRemoved[0] != repoPrefix+"internal/deleted" {
		t.Errorf("drift removed %v, want internal/deleted", s.DriftRemoved)
	}
	if len(s.DriftAdded) != 2 {
		t.Errorf("drift added %v, want the two unrecorded packages", s.DriftAdded)
	}

	var out bytes.Buffer
	_ = doc.writeSummary(&out, repoPrefix)
	text := out.String()
	for _, want := range []string{
		"loaded vs missing",
		"live test packages",
		"loaded (recorded timing)",
		"missing (mean estimate)",
		"measured wall-time",
		"total scheduled work",
		"store rows with no live package",
		"coverage drift vs store",
		"scheduled units",
		"makespan",
		"imbalance",
		"coverage gate: PASS",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary is missing %q:\n%s", want, text)
		}
	}
	// The estimated packages must be named, not just counted: knowing that
	// "2 packages were guessed" without knowing which is not actionable.
	if !strings.Contains(text, "pool") || !strings.Contains(text, "internal/schema") {
		t.Errorf("summary does not name the estimated packages:\n%s", text)
	}
}

func TestBuildPlanColdStartsWhenTheFlagSetChanged(t *testing.T) {
	// Weights measured under -count=100 mean nothing for a -count=1 run.
	// Blending them would produce a confidently wrong split — the
	// "renamed job, silently bad split" trap.
	live := syntheticLive()
	opt := defaultPlanOptions(live)
	opt.Count = 1

	doc := mustPlan(t, syntheticStore(), "", opt)
	if !doc.Summary.ColdStart {
		t.Fatal("a flag-set change did not force a cold start")
	}
	if !strings.Contains(doc.Summary.ColdStartReason, "-count=100") || !strings.Contains(doc.Summary.ColdStartReason, "-count=1") {
		t.Errorf("reason %q does not name both flag sets", doc.Summary.ColdStartReason)
	}
	if doc.Summary.Loaded != 0 {
		t.Errorf("%d packages loaded from an incomparable store", doc.Summary.Loaded)
	}
	if doc.Flags != "-race -count=1" {
		t.Errorf("plan flags %q, want the run's own flags", doc.Flags)
	}
}

func TestBuildPlanAnnouncesAStaleStore(t *testing.T) {
	live := syntheticLive()
	opt := defaultPlanOptions(live)
	opt.Now = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) // 38 days after the store

	doc := mustPlan(t, syntheticStore(), "", opt)
	if !doc.Summary.Stale {
		t.Fatal("a 38-day-old store was not reported stale against a 14-day threshold")
	}
	var out bytes.Buffer
	_ = doc.writeSummary(&out, repoPrefix)
	if !strings.Contains(out.String(), "STALE STORE") {
		t.Errorf("summary does not announce staleness:\n%s", out.String())
	}

	// A fresh store must not cry wolf.
	opt.Now = time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	if fresh := mustPlan(t, syntheticStore(), "", opt); fresh.Summary.Stale {
		t.Error("a one-day-old store was reported stale")
	}
}

func TestBuildPlanIsDeterministic(t *testing.T) {
	live := syntheticLive()
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200, "TestB": 120, "TestC": 60})
	opt := defaultPlanOptions(live)
	opt.Runnables = syntheticRunnables(map[string][]string{
		repoPrefix + "internal/debaml": {"TestA", "TestB", "TestC", "TestD"},
	})

	first, err := json.Marshal(mustPlan(t, st, "", opt))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		again, err := json.Marshal(mustPlan(t, syntheticStoreWithSameHarpoons(), "", opt))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("plan %d differs from plan 0", i)
		}
	}
}

func syntheticStoreWithSameHarpoons() *Store {
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200, "TestB": 120, "TestC": 60})
	return st
}

func TestBuildPlanEmitsAFromJSONReadyMatrix(t *testing.T) {
	live := syntheticLive()
	doc := mustPlan(t, syntheticStore(), "", defaultPlanOptions(live))

	raw, err := doc.matrixJSON()
	if err != nil {
		t.Fatalf("matrixJSON: %v", err)
	}
	var matrix struct {
		Include []struct {
			Bucket      int          `json:"bucket"`
			Name        string       `json:"name"`
			Seconds     float64      `json:"est_seconds"`
			NeedsNode   bool         `json:"needs_node"`
			Units       []string     `json:"units"`
			Invocations []Invocation `json:"invocations"`
			Script      string       `json:"script"`
		} `json:"include"`
	}
	if err := json.Unmarshal(raw, &matrix); err != nil {
		t.Fatalf("matrix is not valid JSON: %v\n%s", err, raw)
	}
	if len(matrix.Include) != 6 {
		t.Fatalf("matrix has %d entries, want K=6", len(matrix.Include))
	}
	for i, e := range matrix.Include {
		if e.Bucket != i {
			t.Errorf("entry %d has bucket %d", i, e.Bucket)
		}
		if e.Name != "bucket-"+string(rune('0'+i)) {
			t.Errorf("entry %d name %q", i, e.Name)
		}
		if len(e.Units) == 0 || len(e.Invocations) == 0 || e.Script == "" {
			t.Errorf("entry %d is empty: %+v", i, e)
		}
		if !strings.HasPrefix(e.Script, "set -euo pipefail") {
			t.Errorf("entry %d script does not fail fast:\n%s", i, e.Script)
		}
	}
	// Only the bucket holding the adapters module needs Node set up; the
	// pure-Go lanes must skip that setup, as bamlutils-race already does.
	nodeBuckets := 0
	for _, e := range matrix.Include {
		if e.NeedsNode {
			nodeBuckets++
		}
	}
	if nodeBuckets != 1 {
		t.Errorf("%d buckets flagged needs_node, want exactly the adapters one", nodeBuckets)
	}
}

func TestBuildPlanInvocationEnvelopes(t *testing.T) {
	live := syntheticLive()
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200, "TestB": 120, "TestC": 60})
	opt := defaultPlanOptions(live)
	opt.Runnables = syntheticRunnables(map[string][]string{
		repoPrefix + "internal/debaml": {"TestA", "TestB", "TestC"},
	})
	doc := mustPlan(t, st, "", opt)

	var sawOff, sawShard, sawSlice, sawWorkspace bool
	for _, b := range doc.Buckets {
		for _, inv := range b.Invocations {
			args := strings.Join(inv.Args, " ")
			switch {
			case inv.Env["GOWORK"] == "off":
				sawOff = true
				if inv.Dir != "adapters/common" {
					t.Errorf("GOWORK=off invocation runs from %q, want the module dir", inv.Dir)
				}
				// Its packages must be addressed relative to that dir, not
				// by import path: with GOWORK=off the workspace is gone.
				if !strings.Contains(args, " . ") && !strings.HasSuffix(args, " .") {
					t.Errorf("GOWORK=off args do not use module-relative patterns: %s", args)
				}
				if strings.Contains(args, repoPrefix) {
					t.Errorf("GOWORK=off args use import paths: %s", args)
				}
			case strings.Contains(args, "-count=17"):
				sawShard = true
				if !strings.Contains(args, repoPrefix+"bamlutils/llmhttp") {
					t.Errorf("count-shard invocation does not target llmhttp: %s", args)
				}
			case strings.Contains(args, "-run"):
				sawSlice = true
				// Anchored: an unanchored TestA would also match TestAlpha
				// and run it in two slices.
				if !strings.Contains(args, "^(") || !strings.Contains(args, ")$") {
					t.Errorf("-run pattern is not anchored: %s", args)
				}
				if !strings.Contains(args, "-count=100") {
					t.Errorf("name slicing changed the sweep depth: %s", args)
				}
			default:
				if inv.Dir == "." && strings.Contains(args, repoPrefix) {
					sawWorkspace = true
					if strings.Contains(args, "GOWORK") {
						t.Errorf("workspace invocation carries a GOWORK override: %+v", inv)
					}
				}
			}
			if !strings.Contains(args, "-race") || !strings.Contains(args, "-timeout 20m") {
				t.Errorf("invocation lost its flag envelope: %s", args)
			}
		}
	}
	for name, ok := range map[string]bool{
		"GOWORK=off module":  sawOff,
		"count-shard":        sawShard,
		"run-slice":          sawSlice,
		"workspace multipkg": sawWorkspace,
	} {
		if !ok {
			t.Errorf("no %s invocation was emitted", name)
		}
	}
}

func TestBuildPlanMixesWorkspaceModulesWithinOneBucket(t *testing.T) {
	// The soft module boundary: workspace-mode packages from different
	// modules may share one invocation, because go.work resolves them all.
	live := syntheticLive()
	doc := mustPlan(t, syntheticStore(), "", defaultPlanOptions(live))
	mixed := false
	for _, b := range doc.Buckets {
		for _, inv := range b.Invocations {
			if inv.Env["GOWORK"] == "off" {
				continue
			}
			mods := map[string]bool{}
			for _, a := range inv.Args {
				if strings.HasPrefix(a, repoPrefix) {
					mods[strings.SplitN(strings.TrimPrefix(a, repoPrefix), "/", 2)[0]] = true
				}
			}
			if len(mods) > 1 {
				mixed = true
			}
		}
	}
	if !mixed {
		t.Error("no invocation mixed packages across workspace modules; the boundary is being treated as hard")
	}
}

func TestBuildPlanEventsCaptureWiring(t *testing.T) {
	// --events-dir is what turns a bucket into a timing source for the next
	// `ingest`; without it the loop cannot close.
	live := syntheticLive()
	opt := defaultPlanOptions(live)
	opt.EventsDir = "/tmp/events"
	doc := mustPlan(t, syntheticStore(), "", opt)

	for _, b := range doc.Buckets {
		for _, inv := range b.Invocations {
			if !containsArg(inv.Args, "-json") {
				t.Errorf("bucket %d invocation has no -json: %v", b.Index, inv.Args)
			}
		}
		if !strings.Contains(b.Script, "tee -a /tmp/events/bucket-") {
			t.Errorf("bucket %d script does not capture events:\n%s", b.Index, b.Script)
		}
	}

	// Without the flag, nothing is captured and nothing is piped.
	plainDoc := mustPlan(t, syntheticStore(), "", defaultPlanOptions(live))
	for _, b := range plainDoc.Buckets {
		if strings.Contains(b.Script, "tee") || strings.Contains(b.Script, "-json") {
			t.Errorf("bucket %d captures events without --events-dir:\n%s", b.Index, b.Script)
		}
	}
}

func TestBuildPlanCoverageGateFiresEndToEnd(t *testing.T) {
	// The reachable path to a dropped test: the store insists a package be
	// name-sliced, but the tree reports no runnables for it (a build-tag
	// gated file set, a resolver returning nothing). The slicer produces no
	// units, and `plan` must refuse rather than emit a matrix that quietly
	// never runs internal/debaml.
	live := syntheticLive()
	st := syntheticStore()
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200})
	opt := defaultPlanOptions(live)
	opt.Runnables = syntheticRunnables(map[string][]string{}) // resolves to nothing

	_, err := buildPlan(st, "", opt)
	if err == nil {
		t.Fatal("plan emitted a matrix that never runs internal/debaml")
	}
	if !strings.Contains(err.Error(), "coverage gate FAILED") || !strings.Contains(err.Error(), "internal/debaml") {
		t.Errorf("error does not identify the gate or the casualty: %v", err)
	}
}

func TestBuildPlanRejectsANonsenseK(t *testing.T) {
	live := syntheticLive()
	for _, k := range []int{0, -1} {
		opt := defaultPlanOptions(live)
		opt.K = k
		if _, err := buildPlan(syntheticStore(), "", opt); err == nil {
			t.Errorf("K=%d was accepted", k)
		}
	}
}

func TestBuildPlanKIsTheOnlyKnob(t *testing.T) {
	// Owner decision 1: adding a lane is bumping K and nothing else.
	live := syntheticLive()
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	for _, k := range []int{1, 2, 4, 6, 8, 10} {
		opt := defaultPlanOptions(live)
		opt.K = k
		doc := mustPlan(t, st, "", opt)
		if len(doc.Buckets) != k {
			t.Errorf("K=%d produced %d buckets", k, len(doc.Buckets))
		}
		sched := scheduledPackages(doc)
		for _, imp := range allTestablePackages() {
			if sched[imp] == 0 {
				t.Errorf("K=%d dropped %s", k, imp)
			}
		}
		t.Logf("K=%2d makespan %7.1fs (ideal %7.1fs, imbalance %5.1f%%) over %d units",
			k, doc.Summary.MakespanSeconds, doc.Summary.IdealSeconds, doc.Summary.ImbalancePct, doc.Summary.ScheduledUnits)
	}
}

func TestShellQuoting(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{".", "."},
		{"adapters/common", "adapters/common"},
		{"-count=100", "-count=100"},
		{"^(TestA|TestB)$", `'^(TestA|TestB)$'`},
		{"a b", "'a b'"},
		{"it's", `'it'\''s'`},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestEmittedExecutionMatchesTheBalancersCostModel(t *testing.T) {
	// P1-1 regression, and the most load-bearing assumption in the tool.
	//
	// KK partitions SUMMED package elapsed times, and `ingest` records
	// package elapsed by summing. That objective is the job's wall time
	// only if the packages in a bucket actually run one after another. Left
	// at the default -p, `go test pkgA pkgB pkgC` runs the three binaries
	// concurrently, so a bucket estimated at 400s might finish in 150s
	// while its neighbour estimated at 380s really takes 380s — the
	// balancer would be optimising a cost function the runner does not have.
	//
	// This pins the topology, not just the flag.
	live := syntheticLive()
	st := syntheticStore()
	harpoon(st, "bamlutils/llmhttp", splitCount, 6, nil)
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200, "TestB": 120, "TestC": 60})
	opt := defaultPlanOptions(live)
	opt.Runnables = syntheticRunnables(map[string][]string{
		repoPrefix + "internal/debaml": {"TestA", "TestB", "TestC", "ExampleD"},
	})
	doc := mustPlan(t, st, "", opt)

	multiPackage := 0
	for _, b := range doc.Buckets {
		for _, inv := range b.Invocations {
			if got := argValue(inv.Args, "-p"); got != "1" {
				t.Errorf("bucket %d invocation runs packages concurrently (-p=%q): %v", b.Index, got, inv.Args)
			}
			if countPackageArgs(inv.Args) > 1 {
				multiPackage++
			}
		}

		// The estimate a bucket advertises must be exactly the serial cost
		// of what it will execute — that is the number the matrix, the
		// summary and the balancer all quote.
		sum := 0.0
		for _, u := range b.Units {
			sum += u.Seconds
		}
		if math.Abs(b.Seconds-sum) > 1e-9 {
			t.Errorf("bucket %d advertises %.3fs but its units sum to %.3fs", b.Index, b.Seconds, sum)
		}
	}
	if multiPackage == 0 {
		t.Fatal("no invocation coalesced several packages; this test would not be proving anything")
	}

	// And the makespan the summary reports is the heaviest bucket's serial
	// cost, i.e. the predicted wall time of the matrix.
	heaviest := 0.0
	for _, b := range doc.Buckets {
		if b.Seconds > heaviest {
			heaviest = b.Seconds
		}
	}
	if math.Abs(doc.Summary.MakespanSeconds-heaviest) > 1e-9 {
		t.Errorf("summary makespan %.3fs, want the heaviest bucket's %.3fs", doc.Summary.MakespanSeconds, heaviest)
	}
}

func TestPlanOptionsValidation(t *testing.T) {
	// P3-1. `go test -count=0` runs nothing at all, so a plan built from it
	// would be a complete, balanced, gate-passing matrix that executes zero
	// tests — a green CI lane proving nothing.
	live := syntheticLive()
	cases := []struct {
		name   string
		mutate func(*planOptions)
		wantIn string
	}{
		{"zero K", func(o *planOptions) { o.K = 0 }, "--k"},
		{"negative K", func(o *planOptions) { o.K = -3 }, "--k"},
		{"zero count", func(o *planOptions) { o.Count = 0 }, "--count"},
		{"negative count", func(o *planOptions) { o.Count = -1 }, "--count"},
		{"negative stale-after", func(o *planOptions) { o.StaleAfter = -time.Hour }, "--stale-after"},
		// The timeout is spliced verbatim into every emitted invocation, so
		// a typo fails every bucket of the matrix at once, far from its
		// cause.
		{"unparsable timeout", func(o *planOptions) { o.Timeout = "20 minutes" }, "--timeout"},
		{"timeout with no unit", func(o *planOptions) { o.Timeout = "20" }, "--timeout"},
		{"negative timeout", func(o *planOptions) { o.Timeout = "-5m" }, "--timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt := defaultPlanOptions(live)
			tc.mutate(&opt)
			_, err := buildPlan(syntheticStore(), "", opt)
			if err == nil {
				t.Fatal("the setting was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not name %s", err.Error(), tc.wantIn)
			}
		})
	}

	// The healthy settings still build, including the legal timeout
	// spellings: a Go duration, or empty to omit -timeout altogether.
	for _, timeout := range []string{"20m", "1h30m", "90s", ""} {
		opt := defaultPlanOptions(live)
		opt.Timeout = timeout
		if _, err := buildPlan(syntheticStore(), "", opt); err != nil {
			t.Errorf("valid options with --timeout %q were rejected: %v", timeout, err)
		}
	}
}

func TestMatrixIsTimeIndependentEvenThoughTheSummaryIsNot(t *testing.T) {
	// P2-1's second half. The matrix that fans the jobs out must be a pure
	// function of (store, K) so the same commit always runs the same split;
	// the summary deliberately carries wall-clock facts (store age,
	// staleness) because their whole job is to make expiry visible.
	live := syntheticLive()
	early := defaultPlanOptions(live)
	early.Now = time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	late := defaultPlanOptions(live)
	late.Now = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	a := mustPlan(t, syntheticStore(), "", early)
	b := mustPlan(t, syntheticStore(), "", late)

	ma, err := a.matrixJSON()
	if err != nil {
		t.Fatal(err)
	}
	mb, err := b.matrixJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ma, mb) {
		t.Error("the matrix changed with the clock; the split must depend only on the store and K")
	}
	if a.Summary.StoreAge == b.Summary.StoreAge {
		t.Error("the summary did not track the clock; staleness would be invisible")
	}
	if a.Summary.Stale || !b.Summary.Stale {
		t.Errorf("staleness did not flip across the threshold: early=%v late=%v", a.Summary.Stale, b.Summary.Stale)
	}
}

// argValue returns the value of a flag in an argv, accepting both `-p=1` and
// `-p 1` spellings.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// countPackageArgs counts the trailing package patterns of a go test argv.
func countPackageArgs(args []string) int {
	n := 0
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if i > 0 && (args[i-1] == "-run" || args[i-1] == "-timeout" || args[i-1] == "-p") {
			continue
		}
		if a == "go" || a == "test" {
			continue
		}
		n++
	}
	return n
}

// failingWriter fails after letting n bytes through, modelling a full pipe
// or a closed redirect target.
type failingWriter struct {
	budget int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.budget <= 0 {
		return 0, errTestOnly("write: no space left on device")
	}
	if len(p) > f.budget {
		n := f.budget
		f.budget = 0
		return n, errTestOnly("write: no space left on device")
	}
	f.budget -= len(p)
	return len(p), nil
}

func TestReportWritesPropagateFailure(t *testing.T) {
	// A summary that could not be written must not be reported as written.
	// The loaded-vs-missing block is the only early warning for a store
	// that expired out of the cache, so losing it silently defeats the
	// mitigation it exists to provide.
	live := syntheticLive()
	doc := mustPlan(t, syntheticStore(), "", defaultPlanOptions(live))

	for _, budget := range []int{0, 40, 400} {
		if err := doc.writeSummary(&failingWriter{budget: budget}, repoPrefix); err == nil {
			t.Errorf("budget=%d: a failed summary write was reported as success", budget)
		}
	}

	var ok bytes.Buffer
	if err := doc.writeSummary(&ok, repoPrefix); err != nil {
		t.Errorf("a healthy write returned an error: %v", err)
	}
	if ok.Len() == 0 {
		t.Error("the healthy write produced nothing")
	}
}

func TestIngestReportWritePropagatesFailure(t *testing.T) {
	sum, err := parseEvents(stream(event("pass", repoPrefix+"pool", "", 120)))
	if err != nil {
		t.Fatal(err)
	}
	rep := mustIngest(t, syntheticStore(), sum, defaultIngestOptions())
	if err := rep.write(&failingWriter{budget: 10}, repoPrefix); err == nil {
		t.Error("a failed ingest-report write was reported as success")
	}
	var ok bytes.Buffer
	if err := rep.write(&ok, repoPrefix); err != nil {
		t.Errorf("a healthy write returned an error: %v", err)
	}
}
