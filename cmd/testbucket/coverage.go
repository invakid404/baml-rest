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
	MissingPackages   []string
	MissingRunnables  map[string][]string
	DuplicateRunnable map[string][]string
	DuplicateUnits    []string
	MalformedUnits    []string
	UngatedSlices     []string
	EmptySlices       []string
	ShardGaps         []string
	ShortSweeps       []string
	MixedCoverage     []string
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
	for _, pkg := range sortedKeys(e.MissingRunnables) {
		fmt.Fprintf(&b, "\n  %s: %d runnable(s) in no -run slice:", pkg, len(e.MissingRunnables[pkg]))
		for _, t := range e.MissingRunnables[pkg] {
			fmt.Fprintf(&b, "\n    - %s", t)
		}
	}
	for _, pkg := range sortedKeys(e.DuplicateRunnable) {
		fmt.Fprintf(&b, "\n  %s: runnable(s) in more than one -run slice: %s",
			pkg, strings.Join(e.DuplicateRunnable[pkg], ", "))
	}
	if len(e.DuplicateUnits) > 0 {
		fmt.Fprintf(&b, "\n  unit(s) assigned to more than one bucket: %s", strings.Join(e.DuplicateUnits, ", "))
	}
	if len(e.MalformedUnits) > 0 {
		fmt.Fprintf(&b, "\n  malformed unit(s) — the emitted invocation would not run what the unit claims:")
		for _, m := range e.MalformedUnits {
			fmt.Fprintf(&b, "\n    - %s", m)
		}
	}
	if len(e.UngatedSlices) > 0 {
		fmt.Fprintf(&b, "\n  package(s) name-sliced with no resolved runnable universe to check against:")
		for _, m := range e.UngatedSlices {
			fmt.Fprintf(&b, "\n    - %s", m)
		}
	}
	if len(e.EmptySlices) > 0 {
		fmt.Fprintf(&b, "\n  run-slice unit(s) with an empty -run set: %s", strings.Join(e.EmptySlices, ", "))
	}
	if len(e.ShardGaps) > 0 {
		fmt.Fprintf(&b, "\n  count-shard gaps: %s", strings.Join(e.ShardGaps, ", "))
	}
	if len(e.ShortSweeps) > 0 {
		fmt.Fprintf(&b, "\n  count-shard group(s) below the requested aggregate sweep: %s",
			strings.Join(e.ShortSweeps, ", "))
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

// gateInput is everything the never-drop-a-test gate compares. It is a
// struct rather than a positional list because the gate keeps growing checks
// and each one needs its own independently-sourced fact.
type gateInput struct {
	// Live is the authority: `go list ./...` intersected with the module set.
	Live []LivePackage
	// Buckets is what will actually be executed.
	Buckets []Bucket
	// Runnables maps a run-sliced package to the complete top-level runnable
	// universe the slicer saw — tests, examples and fuzz targets, i.e.
	// everything `go test -run` selects. Packages absent from the map are
	// not name-sliced and are covered by the package-level check alone.
	Runnables map[string][]string
	// BaseCount is the -count the un-split flake sweep asks for. It is the
	// independent yardstick every count-shard group must add back up to.
	BaseCount int
}

// assertCoverage is the never-drop-a-test gate.
//
// The rule it enforces is about the FINAL PLAN, not about the store: a live
// package with no recorded timing is scheduled on the cold-start mean weight
// and is perfectly legal (that is the brief's explicit requirement), whereas
// a live package missing from the emitted buckets is a hard error. Those two
// are easy to conflate and only one of them is a bug.
//
// It deliberately re-derives everything from the buckets rather than
// trusting the expander's bookkeeping: it is the backstop for a bug in the
// expander or the partitioner, so sharing their state would defeat it.
func assertCoverage(in gateInput) error {
	cerr := &coverageError{
		MissingRunnables:  map[string][]string{},
		DuplicateRunnable: map[string][]string{},
	}

	scheduled := map[string]bool{}          // import path -> covered by some unit
	kinds := map[string]map[unitKind]int{}  // import path -> unit kinds covering it
	unitSeen := map[string]int{}            // unit ID -> number of buckets holding it
	shards := map[string]map[int]int{}      // import path -> shard index -> times seen
	shardWidth := map[string]map[int]bool{} // import path -> the N values its shards claim
	sweep := map[string][]int{}             // import path -> each shard's -count
	runNames := map[string]map[string]int{}

	for _, b := range in.Buckets {
		for _, u := range b.Units {
			unitSeen[u.ID]++

			// Arity first, before anything is credited to a package.
			//
			// A sub-package unit carries per-invocation arguments — one
			// -run regex, one divided -count — that are only meaningful for
			// the single package they were computed from. renderBucket
			// applies those arguments to every package in the unit, so a
			// second package riding along would be run under the FIRST
			// package's regex: scheduled on paper, and in reality executing
			// whichever of its own tests happen to share a name. Zero
			// packages is worse still — the old code indexed [0] and would
			// have panicked where it promised a coverage error.
			//
			// A malformed unit is not allowed to mark anything scheduled.
			// Crediting it would let a package look covered by a unit that
			// cannot actually run it, which is precisely the shape this
			// gate exists to refuse.
			switch u.Kind {
			case kindRunSlice, kindCountShard:
				if len(u.Packages) != 1 {
					cerr.MalformedUnits = append(cerr.MalformedUnits, fmt.Sprintf(
						"%s is a %s over %d packages; a sub-package unit must cover exactly 1 (%s)",
						u.ID, u.Kind, len(u.Packages), importPathsOf(u.Packages)))
					continue
				}
			default:
				if len(u.Packages) == 0 {
					cerr.MalformedUnits = append(cerr.MalformedUnits, fmt.Sprintf(
						"%s is a %s covering no package at all", u.ID, u.Kind))
					continue
				}
			}

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
					shardWidth[pkg] = map[int]bool{}
				}
				shards[pkg][u.Shard]++
				shardWidth[pkg][u.Shards] = true
				sweep[pkg] = append(sweep[pkg], u.Count)
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

	// A package is only allowed to be name-sliced if there is a resolved
	// runnable universe to hold the slices to. Checking completeness only
	// for packages ALREADY in in.Runnables makes the check vacuous for any
	// package the expander never chose to slice: turn an ordinary package
	// unit into a run-slice naming one test and it would sail through —
	// scheduled, single-kind, and never compared against anything — while
	// the emitted `-run '^(TestOne)$'` silently skips every other test,
	// example and fuzz target in it.
	//
	// Requiring the universe to EXIST closes that, and it is the honest
	// condition: without it the gate has no evidence either way, and "no
	// evidence" must not read as "passed".
	for _, pkg := range sortedKeys(runNames) {
		universe, ok := in.Runnables[pkg]
		if !ok {
			cerr.UngatedSlices = append(cerr.UngatedSlices, fmt.Sprintf(
				"%s is run-sliced in the final plan but the expander never resolved its runnable set, "+
					"so the slices cannot be proved complete", pkg))
			continue
		}
		if len(universe) == 0 {
			cerr.UngatedSlices = append(cerr.UngatedSlices, fmt.Sprintf(
				"%s is run-sliced against an empty runnable set", pkg))
		}
	}

	for _, p := range in.Live {
		if !p.HasTests {
			continue
		}
		if !scheduled[p.ImportPath] {
			cerr.MissingPackages = append(cerr.MissingPackages, p.ImportPath)
		}
	}

	for id, n := range unitSeen {
		if n > 1 {
			cerr.DuplicateUnits = append(cerr.DuplicateUnits, fmt.Sprintf("%s (x%d)", id, n))
		}
	}

	// Count-shards must form a complete, non-overlapping 1..N run AND add
	// back up to the requested sweep depth.
	//
	// N comes from the shards' own claimed width, NOT from the highest index
	// present — deriving it from what is there cannot notice that the last
	// shard is gone, which is precisely the boundary that loses a sixth of
	// the flake sweep in silence. The aggregate -count check is the second,
	// independent witness: at -count=100 over six shards, losing #shard6
	// runs 85 iterations instead of 102 and nothing else in the system would
	// ever say so.
	for _, pkg := range sortedKeys(shards) {
		seen := shards[pkg]
		widths := sortedInts(setOfKeys(shardWidth[pkg]))
		if len(widths) != 1 {
			cerr.ShardGaps = append(cerr.ShardGaps, fmt.Sprintf(
				"%s shards disagree on the group size: %v", pkg, widths))
			continue
		}
		want := widths[0]
		if want < 2 {
			cerr.ShardGaps = append(cerr.ShardGaps, fmt.Sprintf(
				"%s claims %d count-shards; a split must have at least 2", pkg, want))
			continue
		}
		for i := 1; i <= want; i++ {
			switch seen[i] {
			case 1:
			case 0:
				cerr.ShardGaps = append(cerr.ShardGaps, fmt.Sprintf("%s missing shard %d of %d", pkg, i, want))
			default:
				cerr.ShardGaps = append(cerr.ShardGaps, fmt.Sprintf("%s shard %d scheduled %d times", pkg, i, seen[i]))
			}
		}
		for _, idx := range sortedInts(setOfKeys(seen)) {
			if idx < 1 || idx > want {
				cerr.ShardGaps = append(cerr.ShardGaps, fmt.Sprintf(
					"%s has shard %d outside the 1..%d group", pkg, idx, want))
			}
		}
		if in.BaseCount > 0 {
			aggregate := 0
			for _, c := range sweep[pkg] {
				if c < 1 {
					cerr.ShortSweeps = append(cerr.ShortSweeps, fmt.Sprintf(
						"%s has a shard with -count=%d", pkg, c))
					aggregate = -1
					break
				}
				aggregate += c
			}
			if aggregate >= 0 && aggregate < in.BaseCount {
				cerr.ShortSweeps = append(cerr.ShortSweeps, fmt.Sprintf(
					"%s runs %d iterations in aggregate, below the requested -count=%d",
					pkg, aggregate, in.BaseCount))
			}
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

	// Every runnable `go test -run` could select must be in exactly one
	// slice. The universe is the full one — tests, examples and fuzz
	// targets — because that is what the emitted -run selects; gating a
	// narrower set would prove coverage of something the command does not
	// execute.
	for _, pkg := range sortedKeys(in.Runnables) {
		seen := runNames[pkg]
		for _, n := range in.Runnables[pkg] {
			switch seen[n] {
			case 1:
			case 0:
				cerr.MissingRunnables[pkg] = append(cerr.MissingRunnables[pkg], n)
			default:
				cerr.DuplicateRunnable[pkg] = append(cerr.DuplicateRunnable[pkg], n)
			}
		}
		// A slice naming something outside the universe is not a coverage
		// loss, but it means the slicer and the resolver disagree about
		// what exists, so the gate's own evidence is unreliable.
		universe := map[string]bool{}
		for _, n := range in.Runnables[pkg] {
			universe[n] = true
		}
		for _, n := range sortedKeys(seen) {
			if !universe[n] {
				cerr.DuplicateRunnable[pkg] = append(cerr.DuplicateRunnable[pkg],
					fmt.Sprintf("%s (not in the package's runnable set)", n))
			}
		}
	}

	sort.Strings(cerr.MissingPackages)
	sort.Strings(cerr.DuplicateUnits)
	sort.Strings(cerr.MalformedUnits)
	sort.Strings(cerr.UngatedSlices)
	sort.Strings(cerr.EmptySlices)
	sort.Strings(cerr.ShardGaps)
	sort.Strings(cerr.ShortSweeps)
	sort.Strings(cerr.MixedCoverage)
	for k := range cerr.MissingRunnables {
		sort.Strings(cerr.MissingRunnables[k])
	}
	for k := range cerr.DuplicateRunnable {
		sort.Strings(cerr.DuplicateRunnable[k])
	}

	if len(cerr.MissingPackages) == 0 && len(cerr.MissingRunnables) == 0 &&
		len(cerr.DuplicateRunnable) == 0 && len(cerr.DuplicateUnits) == 0 &&
		len(cerr.MalformedUnits) == 0 && len(cerr.UngatedSlices) == 0 &&
		len(cerr.EmptySlices) == 0 && len(cerr.ShardGaps) == 0 &&
		len(cerr.ShortSweeps) == 0 && len(cerr.MixedCoverage) == 0 {
		return nil
	}
	return cerr
}

// sortedInts is the integer twin of sortedKeys, so every reduction in this
// package runs in a fixed, value-derived order.
func sortedInts(in []int) []int {
	out := append([]int(nil), in...)
	sort.Ints(out)
	return out
}

func setOfKeys[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// importPathsOf renders a unit's packages for an error message.
func importPathsOf(pkgs []LivePackage) string {
	if len(pkgs) == 0 {
		return "none"
	}
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, p.ImportPath)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
