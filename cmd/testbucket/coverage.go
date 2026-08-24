package main

import (
	"fmt"
	"math"
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

	// The live set is the authority the whole grammar is checked against:
	// a unit may only name packages the tree actually has, described the
	// way the tree describes them.
	liveByPath := make(map[string]LivePackage, len(in.Live))
	for _, p := range in.Live {
		liveByPath[p.ImportPath] = p
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

			// The unit's whole grammar is checked before anything is
			// credited to a package, because a malformed unit must not be
			// allowed to mark a package scheduled: crediting it would let
			// the package look covered by an invocation that cannot
			// actually run it, which is exactly the illusion this gate
			// exists to break.
			if defects := validateUnitGrammar(u, liveByPath, in.BaseCount); len(defects) > 0 {
				cerr.MalformedUnits = append(cerr.MalformedUnits, defects...)
				continue
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
		len(cerr.ShardGaps) == 0 &&
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

// runNameForbidden lists the characters that must never appear in a name the
// renderer will splice into `-run '^(a|b|c)$'`.
//
// Go test names are identifiers, so none of these can occur legitimately —
// which is exactly why one appearing means the slice was built by something
// other than the resolver. A stray `|` adds an alternative, a `.` or `*`
// widens the match, and a `/` retargets the filter at a SUBTEST, quietly
// running one child instead of the whole top-level test.
const runNameForbidden = "|()[]{}.*+?^$\\/ \t\n\r\"'`"

// validateUnitGrammar checks every field of a final unit that the renderer
// can turn into emitted `go test` semantics, plus the fields the gate's own
// later checks depend on.
//
// The pairing with renderBucket/goTestArgs is deliberate and is asserted
// structurally by the tests: every field of Unit is classified as either
// command-changing (and validated here) or provably inert in the renderer.
// Without that, this gate keeps growing one "it never checked field X"
// hole at a time — the field is emitted, the gate ignores it, and the plan
// passes while running something other than what it claims.
//
// Defects are returned rather than thrown so one bad unit reports every
// problem it has, and so the caller can refuse to credit it.
func validateUnitGrammar(u Unit, live map[string]LivePackage, baseCount int) []string {
	var defects []string
	label := unitLabel(u)
	bad := func(format string, a ...any) {
		defects = append(defects, label+" "+fmt.Sprintf(format, a...))
	}

	// Kind first: the renderer switches on it to decide whether a unit gets
	// a solo invocation or is merged into a shared one, and it treats
	// anything it does not recognise as a mergeable whole-package unit. A
	// zero-value kind therefore does not fail loudly, it fails quietly.
	switch u.Kind {
	case kindPackage, kindModuleAtom, kindCountShard, kindRunSlice:
	default:
		bad("has unknown kind %q; the renderer merges anything it does not recognise into a shared whole-package invocation", u.Kind)
		// Every remaining rule is kind-specific, so continuing would only
		// produce noise on top of a unit that is already unschedulable.
		return defects
	}

	// Arity. A sub-package unit carries per-invocation arguments — one -run
	// regex, one divided -count — computed for exactly one package, and the
	// renderer applies them to every package in the unit. A passenger would
	// run under the FIRST package's regex; zero packages used to panic at
	// the [0] index where this gate promises an error.
	switch u.Kind {
	case kindRunSlice, kindCountShard:
		if len(u.Packages) != 1 {
			bad("is a %s over %d packages; a sub-package unit must cover exactly 1 (%s)",
				u.Kind, len(u.Packages), importPathsOf(u.Packages))
			return defects
		}
	default:
		if len(u.Packages) == 0 {
			bad("is a %s covering no package at all", u.Kind)
			return defects
		}
	}

	if strings.TrimSpace(u.ID) == "" {
		bad("has no ID; the renderer keys sub-package invocations by unit ID, so two unnamed units collapse into one command and one of them never runs")
	}

	// The resolution envelope. Mode picks the invocation's working
	// directory and whether GOWORK=off is exported; Module is that
	// directory. A unit whose envelope disagrees with its packages emits
	// `cd <wrong module>` with patterns relative to a different one, or
	// resolves an out-of-workspace package by import path from the repo
	// root, where it does not exist.
	switch u.Mode {
	case modeWork, modeOff:
	default:
		bad("has unknown resolution mode %q; the renderer only knows %q and %q", u.Mode, modeWork, modeOff)
	}
	if strings.TrimSpace(u.Module) == "" {
		bad("has no module directory; a GOWORK=off invocation would cd to an empty path")
	}

	// Packages must be live, test-bearing, and described exactly as the
	// tree describes them. Comparing the whole LivePackage — not just the
	// import path — closes the sub-grammar the renderer reads out of it
	// (Dir for module-relative patterns and for the Node prefix match,
	// Module and Mode for the envelope) in one rule, including any field
	// added to LivePackage later.
	for _, p := range u.Packages {
		lp, ok := live[p.ImportPath]
		switch {
		case !ok:
			bad("names %s, which is not in the live package set; the emitted pattern would not resolve to a package the tree has", p.ImportPath)
			continue
		case !lp.HasTests:
			bad("names %s, which has no test files", p.ImportPath)
			continue
		case lp != p:
			bad("describes %s differently from the live tree (unit has dir=%q module=%q mode=%q, tree has dir=%q module=%q mode=%q)",
				p.ImportPath, p.Dir, p.Module, p.Mode, lp.Dir, lp.Module, lp.Mode)
			continue
		}
		if p.Mode != u.Mode {
			bad("runs in %q mode but %s resolves in %q", u.Mode, p.ImportPath, p.Mode)
		}
		if p.Module != u.Module {
			bad("runs from module %q but %s lives in %q", u.Module, p.ImportPath, p.Module)
		}
	}

	// -count. The renderer emits u.Count for EVERY kind, so a zero here is
	// not an inert field: `go test -count=0` runs nothing at all and
	// reports success.
	if u.Count < 1 {
		bad("runs -count=%d; go test -count=0 executes nothing and still passes", u.Count)
	} else if baseCount > 0 {
		switch u.Kind {
		case kindPackage, kindModuleAtom, kindRunSlice:
			// These run their whole selection once per requested
			// iteration; only count-shards divide the sweep, and their
			// aggregate is checked at group level.
			if u.Count < baseCount {
				bad("runs -count=%d, weakening the requested -count=%d flake sweep", u.Count, baseCount)
			}
		}
	}

	// -run. This is the sharpest of the lot: goTestArgs emits a -run filter
	// whenever Run is non-empty, for ANY kind. A filter on a unit that is
	// not a name-slice therefore runs only the named runnables and silently
	// skips every other test, example and fuzz target in the package —
	// while the unit still looks like complete coverage of it.
	if len(u.Run) > 0 && u.Kind != kindRunSlice {
		bad("is a %s carrying a -run filter (%s); the renderer emits -run for any kind, so this would execute only those names and silently skip the rest of the package",
			u.Kind, strings.Join(u.Run, "|"))
	}
	if u.Kind == kindRunSlice && len(u.Run) == 0 {
		bad("is a run-slice with an empty -run set; the renderer would emit no -run at all and run the whole package, duplicating whatever the other slices cover")
	}
	for _, n := range u.Run {
		switch {
		case strings.TrimSpace(n) == "":
			bad("has an empty name in its -run set")
		case strings.ContainsAny(n, runNameForbidden):
			bad("has %q in its -run set; a Go test name cannot contain a regex metacharacter, so this would change what the alternation matches", n)
		}
	}

	// Shard coordinates. The renderer ignores these, but the gate's own
	// group-completeness check is derived from them, so an incoherent pair
	// would corrupt the evidence rather than the command.
	switch u.Kind {
	case kindCountShard:
		if u.Shards < 2 {
			bad("declares %d count-shards; a split must have at least 2", u.Shards)
		} else if u.Shard < 1 || u.Shard > u.Shards {
			bad("has shard %d outside the 1..%d group it declares", u.Shard, u.Shards)
		}
	default:
		if u.Shard != 0 || u.Shards != 0 {
			bad("is a %s but carries count-shard coordinates %d/%d", u.Kind, u.Shard, u.Shards)
		}
	}

	// Weight. Not a command, but it is what the balancer partitioned and
	// what the plan advertises as the bucket's cost; a non-finite or
	// negative value makes every estimate downstream meaningless.
	if math.IsNaN(u.Seconds) || math.IsInf(u.Seconds, 0) || u.Seconds < 0 {
		bad("has weight %v; a unit's estimate must be a finite, non-negative number of seconds", u.Seconds)
	}

	return defects
}

// unitLabel names a unit in a defect message, including when it has no ID.
func unitLabel(u Unit) string {
	if strings.TrimSpace(u.ID) != "" {
		return u.ID
	}
	kind := string(u.Kind)
	if kind == "" {
		kind = "kindless"
	}
	return "<unnamed " + kind + " unit>"
}
