package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// testEvent is the subset of `go test -json`'s TestEvent this tool reads.
// The stream is the native Go equivalent of the JUnit XML every other
// splitter consumes, which is why no third-party runner is needed.
type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
}

// eventSummary is one batch of `go test -json` output reduced to weights.
type eventSummary struct {
	// PackageSeconds sums the package-level Elapsed across every
	// invocation in the batch. Summing (rather than taking a max) is what
	// makes a split package reconstitute correctly: the S count-shards or
	// -run slices of one package each report their own Elapsed, and their
	// sum is the whole-package weight the next plan wants.
	//
	// A batch missing one bucket's artifact therefore under-measures the
	// packages that bucket held. EWMA keeps that from rewriting the split,
	// and the drop shows up in the next plan's measured wall-time.
	PackageSeconds map[string]float64
	PackageRuns    map[string]int
	// TestSeconds holds TOP-LEVEL runnable weights only. A parent's pass
	// event already reports the elapsed time of everything it ran, subtests
	// included, so folding child events in would count that time twice.
	TestSeconds map[string]map[string]float64
	// Failed packages contribute no fresh weight: a package that aborted
	// under the race detector or hit its -timeout reports a wall time that
	// measures the failure, not the work.
	Failed  map[string]bool
	NoTests map[string]bool
	Lines   int
	Events  int
	// Subtests counts child pass events seen and deliberately not weighed.
	Subtests  int
	Malformed int
}

func newEventSummary() *eventSummary {
	return &eventSummary{
		PackageSeconds: map[string]float64{},
		PackageRuns:    map[string]int{},
		TestSeconds:    map[string]map[string]float64{},
		Failed:         map[string]bool{},
		NoTests:        map[string]bool{},
	}
}

// parseEvents folds one or more NDJSON streams into an eventSummary.
// Non-JSON lines are tolerated and counted, not fatal: a stray `go` warning
// on stdout must not cost a whole run's timings. A stream with no usable
// events at all IS fatal — that means the capture is broken and silently
// writing an unchanged store would hide it.
func parseEvents(readers ...io.Reader) (*eventSummary, error) {
	sum := newEventSummary()
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			sum.Lines++
			if !strings.HasPrefix(line, "{") {
				sum.Malformed++
				continue
			}
			var ev testEvent
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				sum.Malformed++
				continue
			}
			if ev.Package == "" {
				continue
			}
			sum.Events++
			switch {
			case ev.Test == "" && ev.Action == "pass":
				sum.PackageSeconds[ev.Package] += ev.Elapsed
				sum.PackageRuns[ev.Package]++
			case ev.Test == "" && ev.Action == "fail":
				sum.Failed[ev.Package] = true
			case ev.Test == "" && ev.Action == "skip":
				// "no test files" — nothing to weigh, nothing to schedule.
				sum.NoTests[ev.Package] = true
			case ev.Test != "" && ev.Action == "pass":
				// -run slicing operates on top-level names, and a top-level
				// pass event's Elapsed ALREADY includes every subtest it
				// ran. Adding the "TestX/sub" events on top would inflate
				// the parent — by a lot, in a package that leans on t.Run —
				// which in turn skews the slice packing and can push a
				// package over the run-upgrade threshold on time that was
				// only ever counted once in reality.
				if strings.ContainsRune(ev.Test, '/') {
					sum.Subtests++
					continue
				}
				if sum.TestSeconds[ev.Package] == nil {
					sum.TestSeconds[ev.Package] = map[string]float64{}
				}
				sum.TestSeconds[ev.Package][ev.Test] += ev.Elapsed
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read go test -json stream: %w", err)
		}
	}
	// "Usable" means package RESULTS, not merely well-formed lines: a
	// capture that recorded only `run`/`output` chatter is broken, and
	// silently writing an unchanged store would hide that indefinitely.
	if len(sum.PackageSeconds) == 0 && len(sum.Failed) == 0 && len(sum.NoTests) == 0 {
		return nil, fmt.Errorf("no `go test -json` package results found (%d lines, %d events, %d unparsable)",
			sum.Lines, sum.Events, sum.Malformed)
	}
	return sum, nil
}

type ingestOptions struct {
	Alpha float64
	Race  bool
	Count int
	// WhaleK is the K the split threshold is derived from: a package is a
	// whale once it alone exceeds total/K, because at that point it — not
	// the total work — sets the makespan.
	WhaleK int
	// WhaleSeconds overrides the derived threshold with an absolute one.
	WhaleSeconds float64
	// MinShardSeconds is the floor on a slice's wall time. Every extra
	// slice is a whole extra CI job paying checkout + setup + compile
	// (~2-3 min on this repo's runners), so slicing a unit into pieces
	// smaller than that spends a job to save less than the job costs. It is
	// the "diminishing returns" half of the K curve, enforced per unit.
	MinShardSeconds float64
	Now             time.Time
	// Live is the authoritative package set at record time, when available.
	// It is what lets `plan` report drift, and what licenses pruning rows
	// for packages that no longer exist.
	Live              []LivePackage
	LiveAuthoritative bool
}

// runUpgradeFraction is how much of a whale's wall time must be attributable
// to named top-level runnables before `ingest` promotes it from count-sharding to the
// finer -run slicing. Below it, the per-test picture is too incomplete for
// name slices to balance well and count-sharding stays the safer harpoon.
const runUpgradeFraction = 0.5

type ingestReport struct {
	Updated      []string
	New          []string
	SkippedFail  []string
	Pruned       []string
	Whales       []string
	Unflagged    []string
	TotalSeconds float64
	Threshold    float64
	Alpha        float64
	Events       int
	Malformed    int
	Coverage     int
	CoverageFrom string
	FlagsReset   string
	Subtests     int
}

// validate rejects settings that would silently corrupt the store rather
// than fail. An out-of-range alpha is the dangerous one: 0 makes the store
// stop learning forever and a negative one drives weights negative, and
// neither shows up as anything but a mysteriously bad split months later.
func (o ingestOptions) validate() error {
	switch {
	case o.Count < 1:
		return fmt.Errorf("--count must be >= 1, got %d", o.Count)
	case math.IsNaN(o.Alpha) || o.Alpha <= 0 || o.Alpha > 1:
		return fmt.Errorf("--ewma must be in (0,1], got %v", o.Alpha)
	case o.WhaleK < 1:
		return fmt.Errorf("--whale-k must be >= 1, got %d", o.WhaleK)
	case math.IsNaN(o.WhaleSeconds) || math.IsInf(o.WhaleSeconds, 0) || o.WhaleSeconds < 0:
		return fmt.Errorf("--whale-seconds must be a finite value >= 0, got %v", o.WhaleSeconds)
	case math.IsNaN(o.MinShardSeconds) || math.IsInf(o.MinShardSeconds, 0) || o.MinShardSeconds < 0:
		return fmt.Errorf("--min-shard-seconds must be a finite value >= 0, got %v", o.MinShardSeconds)
	}
	return nil
}

// applyIngest merges a batch of measurements into the store and re-derives
// the split policy. It is the entire self-optimizing half of the loop: every
// master run rewrites the weights that shape the next PR's matrix.
func applyIngest(st *Store, sum *eventSummary, opt ingestOptions) (*ingestReport, error) {
	if err := opt.validate(); err != nil {
		return nil, err
	}
	flags := canonicalFlags(opt.Race, opt.Count)
	rep := &ingestReport{Alpha: opt.Alpha, Events: sum.Events, Malformed: sum.Malformed, Subtests: sum.Subtests}

	if st.Flags != "" && st.Flags != flags {
		// Weights from a different flag set cannot be blended with these.
		rep.FlagsReset = fmt.Sprintf("%s -> %s", st.Flags, flags)
		st.Units = map[string]*UnitStat{}
		st.Coverage = nil
	}
	st.Flags = flags
	if st.Units == nil {
		st.Units = map[string]*UnitStat{}
	}

	for _, pkg := range sortedKeys(sum.PackageSeconds) {
		measured := sum.PackageSeconds[pkg]
		if sum.Failed[pkg] {
			rep.SkippedFail = append(rep.SkippedFail, pkg)
			continue
		}
		if measured <= 0 {
			continue
		}
		row := st.Units[pkg]
		if row == nil {
			row = &UnitStat{}
			st.Units[pkg] = row
			rep.New = append(rep.New, pkg)
		} else {
			rep.Updated = append(rep.Updated, pkg)
		}
		row.Seconds = ewma(row.Seconds, row.Samples, measured, opt.Alpha)
		row.Samples++
	}
	for _, pkg := range sortedKeys(sum.Failed) {
		if _, ok := sum.PackageSeconds[pkg]; !ok {
			rep.SkippedFail = append(rep.SkippedFail, pkg)
		}
	}
	sort.Strings(rep.SkippedFail)
	rep.SkippedFail = dedupe(rep.SkippedFail)

	// Prune rows for packages that no longer exist, but only when the live
	// set is authoritative. Pruning off an event batch alone would delete
	// every package that simply was not part of this batch.
	if opt.LiveAuthoritative {
		liveSet := map[string]bool{}
		for _, p := range opt.Live {
			if p.HasTests {
				liveSet[p.ImportPath] = true
			}
		}
		for _, pkg := range sortedKeys(st.Units) {
			if !liveSet[pkg] {
				delete(st.Units, pkg)
				rep.Pruned = append(rep.Pruned, pkg)
			}
		}
	}

	// Reduced over sorted keys and in integer microseconds: map iteration
	// order is randomised per process and float addition is not associative,
	// so a plain `for range st.Units` sum can land on either side of
	// total/K for byte-identical inputs — and the whale threshold derived
	// from it decides whether a package is split at all.
	measured := make([]float64, 0, len(st.Units))
	for _, pkg := range sortedKeys(st.Units) {
		if row := st.Units[pkg]; row.measured() {
			measured = append(measured, row.Seconds)
		}
	}
	rep.TotalSeconds = sumSeconds(measured)
	rep.Threshold = whaleThreshold(rep.TotalSeconds, opt)

	for _, pkg := range sortedKeys(st.Units) {
		row := st.Units[pkg]
		if !row.measured() || rep.Threshold <= 0 || row.Seconds <= rep.Threshold {
			if row.splitPolicy() != splitNone {
				rep.Unflagged = append(rep.Unflagged, pkg)
			}
			row.Split = ""
			row.SplitInto = 0
			// Per-test rows exist only to serve a split; drop them with it
			// so the store does not accrete a per-test index of the tree.
			row.Tests = nil
			continue
		}
		shards := clampShards(int(math.Ceil(row.Seconds/rep.Threshold)), opt.WhaleK)
		if opt.MinShardSeconds > 0 {
			if affordable := int(row.Seconds / opt.MinShardSeconds); affordable < shards {
				shards = affordable
			}
		}
		if shards < 2 {
			// Above the relative threshold but too small in absolute terms
			// for slicing to pay for itself. Leave it whole.
			if row.splitPolicy() != splitNone {
				rep.Unflagged = append(rep.Unflagged, pkg)
			}
			row.Split = ""
			row.SplitInto = 0
			row.Tests = nil
			continue
		}
		row.SplitInto = shards

		// Fold this batch's per-test weights in before deciding whether the
		// package can be name-sliced, so a package that becomes a whale on
		// the same run it was measured can go straight to -run slicing.
		if fresh := sum.TestSeconds[pkg]; len(fresh) > 0 {
			if row.Tests == nil {
				row.Tests = map[string]float64{}
			}
			for name, sec := range fresh {
				if sec <= 0 {
					// A sub-millisecond test reports 0.00; a zero row
					// carries no weight information and is treated as
					// unknown by the slicer anyway, so storing it would
					// only grow the store.
					continue
				}
				row.Tests[name] = ewma(row.Tests[name], boolToInt(row.Tests[name] > 0), sec, opt.Alpha)
			}
			// A test that no longer reports has been renamed or deleted;
			// keeping its weight would misdirect a future slice.
			for name := range row.Tests {
				if _, ok := fresh[name]; !ok {
					delete(row.Tests, name)
				}
			}
		}

		// Same fixed-order, integer reduction: this sum decides count-shard
		// versus -run slicing, and the two are not interchangeable.
		perTest := make([]float64, 0, len(row.Tests))
		for _, name := range sortedKeys(row.Tests) {
			perTest = append(perTest, row.Tests[name])
		}
		named := sumSeconds(perTest)
		if len(row.Tests) >= 2 && row.Seconds > 0 && named/row.Seconds >= runUpgradeFraction {
			row.Split = splitRun
		} else {
			row.Split = splitCount
		}
		rep.Whales = append(rep.Whales, fmt.Sprintf("%s %.1fs -> split=%s x%d", pkg, row.Seconds, row.Split, shards))
	}

	// Record the coverage snapshot `plan` diffs against.
	cov := map[string]bool{}
	src := "go-test-json"
	if opt.LiveAuthoritative {
		src = "go-list"
		for _, p := range opt.Live {
			if p.HasTests {
				cov[p.ImportPath] = true
			}
		}
	} else {
		for pkg := range sum.PackageSeconds {
			cov[pkg] = true
		}
		for pkg := range sum.Failed {
			cov[pkg] = true
		}
	}
	st.Coverage = sortedKeys(cov)
	st.CoverageSource = src
	rep.Coverage = len(st.Coverage)
	rep.CoverageFrom = src
	st.stamp(opt.Now)
	return rep, nil
}

// sumSeconds adds durations in the order given and in integer microseconds,
// so the result is exactly reproducible: integer addition is associative
// where float addition is not, and callers always pass a value list built
// from sorted keys rather than a map range.
func sumSeconds(values []float64) float64 {
	var micros int64
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		micros += int64(math.Round(v * 1e6))
	}
	return float64(micros) / 1e6
}

// whaleThreshold is total/K — the point above which a single package alone
// sets the makespan and no value of K can help until it is split.
func whaleThreshold(total float64, opt ingestOptions) float64 {
	if opt.WhaleSeconds > 0 {
		return opt.WhaleSeconds
	}
	if opt.WhaleK <= 0 {
		return 0
	}
	return total / float64(opt.WhaleK)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func (r *ingestReport) write(w io.Writer, prefix string) {
	fmt.Fprintf(w, "testbucket ingest — %d events (%d unparsable lines), alpha=%.2f\n", r.Events, r.Malformed, r.Alpha)
	if r.Subtests > 0 {
		fmt.Fprintf(w, "  subtest events seen %d (not weighed: already inside their parent's elapsed)\n", r.Subtests)
	}
	if r.FlagsReset != "" {
		fmt.Fprintf(w, "*** FLAG SET CHANGED (%s): previous weights discarded, store cold-starts. ***\n", r.FlagsReset)
	}
	fmt.Fprintf(w, "  packages updated   %d\n", len(r.Updated))
	fmt.Fprintf(w, "  packages new       %d%s\n", len(r.New), listSuffix(r.New, prefix))
	if len(r.SkippedFail) > 0 {
		fmt.Fprintf(w, "  failed (no fresh weight, prior kept) %d%s\n", len(r.SkippedFail), listSuffix(r.SkippedFail, prefix))
	}
	if len(r.Pruned) > 0 {
		fmt.Fprintf(w, "  rows pruned (package gone) %d%s\n", len(r.Pruned), listSuffix(r.Pruned, prefix))
	}
	fmt.Fprintf(w, "  total measured work %s\n", humanSeconds(r.TotalSeconds))
	fmt.Fprintf(w, "  whale threshold     %.1fs\n", r.Threshold)
	if len(r.Whales) > 0 {
		fmt.Fprintf(w, "  split candidates:\n")
		for _, wl := range r.Whales {
			fmt.Fprintf(w, "    - %s\n", shortenID(wl, prefix))
		}
	}
	if len(r.Unflagged) > 0 {
		fmt.Fprintf(w, "  no longer whales   %d%s\n", len(r.Unflagged), listSuffix(r.Unflagged, prefix))
	}
	fmt.Fprintf(w, "  coverage recorded  %d packages (source: %s)\n", r.Coverage, r.CoverageFrom)
}

func listSuffix(items []string, prefix string) string {
	if len(items) == 0 {
		return ""
	}
	short := make([]string, 0, len(items))
	for _, i := range items {
		short = append(short, shortenID(i, prefix))
	}
	return "  " + truncList(short, 5)
}
