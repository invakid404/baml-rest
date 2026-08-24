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
			doc.writeSummary(&out, repoPrefix)
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
	doc.writeSummary(&out, repoPrefix)
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
	doc.writeSummary(&out, repoPrefix)
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
	opt.TestNames = syntheticTestNames(map[string][]string{
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
	opt.TestNames = syntheticTestNames(map[string][]string{
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
	// name-sliced, but the tree reports no test funcs for it (a build-tag
	// gated file set, a resolver returning nothing). The slicer produces no
	// units, and `plan` must refuse rather than emit a matrix that quietly
	// never runs internal/debaml.
	live := syntheticLive()
	st := syntheticStore()
	harpoon(st, "internal/debaml", splitRun, 3, map[string]float64{"TestA": 200})
	opt := defaultPlanOptions(live)
	opt.TestNames = syntheticTestNames(map[string][]string{}) // resolves to nothing

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
