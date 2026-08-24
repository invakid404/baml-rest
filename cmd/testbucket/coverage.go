package main

import (
	"fmt"
	"sort"
	"strings"
)

// Bucket is one lane of the generated matrix.
type Bucket struct {
	Index   int     `json:"bucket"`
	Units   []Unit  `json:"units"`
	Seconds float64 `json:"est_seconds"`
}

// coverageError is the never-drop-a-test gate firing. It is a distinct type
// so callers can tell "the plan is wrong" apart from "the tool could not
// run", and it renders every offending name — a gate that says only "1
// package missing" costs the reader a bisect.
type coverageError struct {
	MissingPackages []string
	MissingTests    map[string][]string
	DuplicateTests  map[string][]string
	DuplicateUnits  []string
	EmptySlices     []string
	ShardGaps       []string
	MixedCoverage   []string
}

func (e *coverageError) Error() string {
	var b strings.Builder
	b.WriteString("coverage gate FAILED: the plan does not schedule every live test")
	if len(e.MissingPackages) > 0 {
		fmt.Fprintf(&b, "\n  %d live package(s) assigned to no bucket:", len(e.MissingPackages))
		for _, p := range e.MissingPackages {
			fmt.Fprintf(&b, "\n    - %s", p)
		}
	}
	for _, pkg := range sortedKeys(e.MissingTests) {
		fmt.Fprintf(&b, "\n  %s: %d test func(s) in no -run slice:", pkg, len(e.MissingTests[pkg]))
		for _, t := range e.MissingTests[pkg] {
			fmt.Fprintf(&b, "\n    - %s", t)
		}
	}
	for _, pkg := range sortedKeys(e.DuplicateTests) {
		fmt.Fprintf(&b, "\n  %s: test func(s) in more than one -run slice: %s",
			pkg, strings.Join(e.DuplicateTests[pkg], ", "))
	}
	if len(e.DuplicateUnits) > 0 {
		fmt.Fprintf(&b, "\n  unit(s) assigned to more than one bucket: %s", strings.Join(e.DuplicateUnits, ", "))
	}
	if len(e.EmptySlices) > 0 {
		fmt.Fprintf(&b, "\n  run-slice unit(s) with an empty -run set: %s", strings.Join(e.EmptySlices, ", "))
	}
	if len(e.ShardGaps) > 0 {
		fmt.Fprintf(&b, "\n  count-shard gaps: %s", strings.Join(e.ShardGaps, ", "))
	}
	if len(e.MixedCoverage) > 0 {
		fmt.Fprintf(&b, "\n  package(s) covered by incompatible units (would run twice): %s",
			strings.Join(e.MixedCoverage, ", "))
	}
	b.WriteString("\n\nThis is THE invariant: a balanced-but-incomplete split is the one\n" +
		"failure mode worse than an imbalanced one, because it is silent.\n" +
		"Refusing to emit a matrix is the correct outcome.")
	return b.String()
}

// assertCoverage is the never-drop-a-test gate. It compares the emitted
// buckets against the LIVE package set — `go list ./...` intersected with
// the module set — and refuses the plan if anything live is unscheduled.
//
// It deliberately re-derives everything from the buckets rather than
// trusting the expander's bookkeeping: it is the backstop for a bug in the
// expander or the partitioner, so sharing their state would defeat it.
//
// liveTestNames maps a run-sliced package to the test-func list the slicer
// saw; packages absent from that map are not name-sliced and are covered by
// the package-level check alone.
func assertCoverage(live []LivePackage, buckets []Bucket, liveTestNames map[string][]string) error {
	cerr := &coverageError{
		MissingTests:   map[string][]string{},
		DuplicateTests: map[string][]string{},
	}

	scheduled := map[string]bool{}         // import path -> covered by some unit
	kinds := map[string]map[unitKind]int{} // import path -> unit kinds covering it
	unitSeen := map[string]int{}           // unit ID -> number of buckets holding it
	shards := map[string]map[int]int{}
	runNames := map[string]map[string]int{}

	for _, b := range buckets {
		for _, u := range b.Units {
			unitSeen[u.ID]++
			for _, p := range u.Packages {
				scheduled[p.ImportPath] = true
				if kinds[p.ImportPath] == nil {
					kinds[p.ImportPath] = map[unitKind]int{}
				}
				kinds[p.ImportPath][u.Kind]++
			}
			switch u.Kind {
			case kindCountShard:
				pkg := u.Packages[0].ImportPath
				if shards[pkg] == nil {
					shards[pkg] = map[int]int{}
				}
				shards[pkg][u.Shard]++
			case kindRunSlice:
				pkg := u.Packages[0].ImportPath
				if len(u.Run) == 0 {
					cerr.EmptySlices = append(cerr.EmptySlices, u.ID)
				}
				if runNames[pkg] == nil {
					runNames[pkg] = map[string]int{}
				}
				for _, n := range u.Run {
					runNames[pkg][n]++
				}
			}
		}
	}

	for _, p := range live {
		if !p.HasTests {
			continue
		}
		if !scheduled[p.ImportPath] {
			cerr.MissingPackages = append(cerr.MissingPackages, p.ImportPath)
		}
	}

	// The mirror image of a dropped test: a package covered by two
	// different tiers at once (a whole-package unit AND its shards, say)
	// would run twice. That costs wall-time rather than coverage, but it is
	// just as much a bug in the expander and just as invisible from a green
	// matrix.
	for _, imp := range sortedKeys(kinds) {
		seen := kinds[imp]
		if len(seen) > 1 {
			var names []string
			for _, k := range []unitKind{kindPackage, kindModuleAtom, kindCountShard, kindRunSlice} {
				if seen[k] > 0 {
					names = append(names, fmt.Sprintf("%s x%d", k, seen[k]))
				}
			}
			cerr.MixedCoverage = append(cerr.MixedCoverage, fmt.Sprintf("%s (%s)", imp, strings.Join(names, " + ")))
			continue
		}
		for _, k := range []unitKind{kindPackage, kindModuleAtom} {
			if seen[k] > 1 {
				cerr.MixedCoverage = append(cerr.MixedCoverage, fmt.Sprintf("%s (%s x%d)", imp, k, seen[k]))
			}
		}
	}

	for id, n := range unitSeen {
		if n > 1 {
			cerr.DuplicateUnits = append(cerr.DuplicateUnits, fmt.Sprintf("%s (x%d)", id, n))
		}
	}

	// Count-shards must form a complete, non-overlapping 1..N run: a gap
	// means a slice of the flake sweep silently went missing, which at
	// -count=100/N is exactly the kind of loss that never shows up as a
	// failing test.
	for _, pkg := range sortedKeys(shards) {
		seen := shards[pkg]
		highest := 0
		for idx := range seen {
			if idx > highest {
				highest = idx
			}
		}
		for i := 1; i <= highest; i++ {
			switch seen[i] {
			case 1:
			case 0:
				cerr.ShardGaps = append(cerr.ShardGaps, fmt.Sprintf("%s missing shard %d of %d", pkg, i, highest))
			default:
				cerr.ShardGaps = append(cerr.ShardGaps, fmt.Sprintf("%s shard %d scheduled %d times", pkg, i, seen[i]))
			}
		}
	}

	for pkg, names := range liveTestNames {
		seen := runNames[pkg]
		for _, n := range names {
			switch seen[n] {
			case 1:
			case 0:
				cerr.MissingTests[pkg] = append(cerr.MissingTests[pkg], n)
			default:
				cerr.DuplicateTests[pkg] = append(cerr.DuplicateTests[pkg], n)
			}
		}
	}

	sort.Strings(cerr.MissingPackages)
	sort.Strings(cerr.DuplicateUnits)
	sort.Strings(cerr.EmptySlices)
	sort.Strings(cerr.ShardGaps)
	sort.Strings(cerr.MixedCoverage)
	for k := range cerr.MissingTests {
		sort.Strings(cerr.MissingTests[k])
	}
	for k := range cerr.DuplicateTests {
		sort.Strings(cerr.DuplicateTests[k])
	}

	if len(cerr.MissingPackages) == 0 && len(cerr.MissingTests) == 0 &&
		len(cerr.DuplicateTests) == 0 && len(cerr.DuplicateUnits) == 0 &&
		len(cerr.EmptySlices) == 0 && len(cerr.ShardGaps) == 0 &&
		len(cerr.MixedCoverage) == 0 {
		return nil
	}
	return cerr
}
