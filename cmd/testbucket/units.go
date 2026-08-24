package main

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// LivePackage is one package as the TREE sees it right now — the output of
// `go list ./...` over the module set. This, not the store, is the authority
// on what must run: the store only ever supplies weights.
type LivePackage struct {
	// ImportPath is the store key and the identity `go test -json` reports.
	ImportPath string `json:"import_path"`
	// Dir is the package directory relative to the repo root (informational
	// and used to build the ./... pattern relative to Module).
	Dir string `json:"dir"`
	// Module is the module directory relative to the repo root ("." for the
	// root module); invocations are issued from there.
	Module string `json:"module"`
	// Mode is "work" (go.work active) or "off" (GOWORK=off). Packages of
	// different modes can never share one `go test` invocation.
	Mode string `json:"mode"`
	// Atomic marks a module that must be packed whole — the soft module
	// boundary made hard where correctness needs it (see moduleAtoms).
	Atomic bool `json:"atomic,omitempty"`
	// HasTests is false for packages with no _test.go files. They are
	// excluded from bucketing (running them is a no-op) but they are also
	// excluded from the coverage gate, so a package that GAINS a test file
	// is picked up by the next `go list` and scheduled immediately.
	HasTests bool `json:"has_tests"`
}

const (
	modeWork = "work"
	modeOff  = "off"
)

// unitKind is the granularity tier a scheduled unit sits at.
type unitKind string

const (
	// kindPackage is tier 1: one whole package, the default.
	kindPackage unitKind = "package"
	// kindCountShard is tier 2a: the package run whole but with -count
	// divided S ways. Needs no per-test data, which is what makes it the
	// day-one harpoon for a whale the store knows nothing about internally.
	kindCountShard unitKind = "count-shard"
	// kindRunSlice is tier 2b: a -run name subset of the package. Needs
	// per-test weights, gives a genuinely finer cut than count-sharding.
	kindRunSlice unitKind = "run-slice"
	// kindModuleAtom is a whole module packed as one unit because its
	// packages cannot be mixed into a shared invocation.
	kindModuleAtom unitKind = "module-atom"
)

// Unit is one schedulable thing: exactly one `go test` invocation.
type Unit struct {
	ID       string        `json:"id"`
	Kind     unitKind      `json:"kind"`
	Seconds  float64       `json:"seconds"`
	Estimate bool          `json:"estimated,omitempty"`
	Packages []LivePackage `json:"packages"`
	Module   string        `json:"module"`
	Mode     string        `json:"mode"`
	Count    int           `json:"count"`
	Run      []string      `json:"run,omitempty"`
	Shard    int           `json:"shard,omitempty"`
	Shards   int           `json:"shards,omitempty"`
}

func (u Unit) item() Item { return Item{ID: u.ID, Weight: u.Seconds} }

// countShardID / runSliceID / moduleAtomID render the unit grammar from the
// brief: `pkg`, `pkg#shardI`, `pkg[TestA|TestB]`, plus `mod:<dir>` for a
// whole-module atom. IDs are stable identifiers, not display strings — the
// human summary shortens them.
func countShardID(importPath string, shard int) string {
	return fmt.Sprintf("%s#shard%d", importPath, shard)
}

func runSliceID(importPath string, names []string) string {
	return fmt.Sprintf("%s[%s]", importPath, strings.Join(names, "|"))
}

func moduleAtomID(moduleDir string) string { return "mod:" + moduleDir }

// runnableNamer resolves a package's complete top-level RUNNABLE set — every
// name the emitted `-run` alternation can select: tests, examples and fuzz
// targets alike. It is an injected dependency so the whole planner is
// testable against a synthetic tree; the real implementation shells out to
// `go test -list` (see listRunnableNames for why the universe must be the
// full one and not just `Test*`).
type runnableNamer func(p LivePackage) ([]string, error)

type expandOptions struct {
	// K is the bucket count, used only to bound how far a whale may be split.
	K int
	// BaseCount is the -count the un-split flake sweep uses; count-shards
	// divide it.
	BaseCount int
	// MeanSeconds is the cold-start weight handed to any live package the
	// store has no measurement for. Never zero: an unweighted unit would
	// sink to whichever bucket happens to be lightest and, worse, would
	// make the plan's estimates lie.
	MeanSeconds float64
	Runnables   runnableNamer
}

// expansion is the result of turning the live package set plus the store
// into schedulable units.
type expansion struct {
	Units []Unit
	// Runnables records the live runnable-name list used for each run-sliced
	// package, so the coverage gate can check the slices against the same
	// universe the slicer saw.
	Runnables map[string][]string
	Notes     []string
	// Loaded / Missing count PACKAGES (not units) whose weight came from a
	// real measurement vs the cold-start mean.
	Loaded           []string
	Missing          []string
	MeasuredSeconds  float64
	EstimatedSeconds float64
}

// expandUnits turns the live package set into the units `plan` partitions.
//
// The traversal is over the LIVE set, never over the store. That ordering is
// the structural half of the never-drop-a-test invariant: a package the
// store has never heard of still gets a unit (on the mean weight), and a
// store row with no live package simply never gets looked at.
func expandUnits(live []LivePackage, st *Store, opt expandOptions) (*expansion, error) {
	ex := &expansion{Runnables: map[string][]string{}}

	testable := make([]LivePackage, 0, len(live))
	for _, p := range live {
		if p.HasTests {
			testable = append(testable, p)
		}
	}
	sort.Slice(testable, func(i, j int) bool { return testable[i].ImportPath < testable[j].ImportPath })

	atoms, loose := moduleAtoms(testable)

	weightOf := func(p LivePackage) (float64, bool) {
		if row := st.Units[p.ImportPath]; row.measured() {
			return row.Seconds, false
		}
		return opt.MeanSeconds, true
	}
	account := func(p LivePackage, sec float64, estimated bool) {
		if estimated {
			ex.Missing = append(ex.Missing, p.ImportPath)
			ex.EstimatedSeconds += sec
			return
		}
		ex.Loaded = append(ex.Loaded, p.ImportPath)
		ex.MeasuredSeconds += sec
	}

	// Whole-module atoms first, so their notes read before the per-package
	// ones in the summary.
	for _, moduleDir := range sortedKeys(atoms) {
		pkgs := atoms[moduleDir]
		total := 0.0
		estimated := false
		for _, p := range pkgs {
			sec, est := weightOf(p)
			account(p, sec, est)
			total += sec
			estimated = estimated || est
			if st.Units[p.ImportPath].splitPolicy() != splitNone {
				ex.Notes = append(ex.Notes, fmt.Sprintf(
					"split of %s suppressed: module %s must be packed whole (mode=%s)",
					p.ImportPath, moduleDir, pkgs[0].Mode))
			}
		}
		ex.Units = append(ex.Units, Unit{
			ID:       moduleAtomID(moduleDir),
			Kind:     kindModuleAtom,
			Seconds:  total,
			Estimate: estimated,
			Packages: pkgs,
			Module:   moduleDir,
			Mode:     pkgs[0].Mode,
			Count:    opt.BaseCount,
		})
	}

	for _, p := range loose {
		sec, estimated := weightOf(p)
		account(p, sec, estimated)
		row := st.Units[p.ImportPath]

		policy := row.splitPolicy()
		if opt.K < 2 && policy != splitNone {
			// With a single bucket there is nothing to balance, and each
			// extra slice pays another compile of the package. Splitting
			// here would be strictly worse than not.
			ex.Notes = append(ex.Notes, fmt.Sprintf("split of %s suppressed: K=%d leaves nothing to balance", p.ImportPath, opt.K))
			policy = splitNone
		}

		switch policy {
		case splitCount:
			shards := clampShards(row.SplitInto, opt.K)
			per := sec / float64(shards)
			count := ceilDiv(opt.BaseCount, shards)
			for i := 1; i <= shards; i++ {
				ex.Units = append(ex.Units, Unit{
					ID:       countShardID(p.ImportPath, i),
					Kind:     kindCountShard,
					Seconds:  per,
					Estimate: estimated,
					Packages: []LivePackage{p},
					Module:   p.Module,
					Mode:     p.Mode,
					Count:    count,
					Shard:    i,
					Shards:   shards,
				})
			}
			ex.Notes = append(ex.Notes, fmt.Sprintf(
				"count-shard %s into %d x -count=%d (aggregate %d >= %d)",
				p.ImportPath, shards, count, count*shards, opt.BaseCount))

		case splitRun:
			if opt.Runnables == nil {
				return nil, fmt.Errorf("%s is flagged split=run but no runnable-name resolver is configured", p.ImportPath)
			}
			names, err := opt.Runnables(p)
			if err != nil {
				// Loud, not silent: falling back to a whole-package run
				// here would quietly undo the harpoon and blow the
				// makespan without anyone noticing.
				return nil, fmt.Errorf("resolve runnable names for %s (flagged split=run): %w", p.ImportPath, err)
			}
			names = dedupeSorted(names)
			ex.Runnables[p.ImportPath] = names
			slices := sliceByName(p, names, row, sec, clampShards(row.SplitInto, opt.K), opt.BaseCount, estimated)
			ex.Units = append(ex.Units, slices...)
			ex.Notes = append(ex.Notes, fmt.Sprintf(
				"run-slice %s into %d slices over %d live runnables (tests, examples and fuzz targets)",
				p.ImportPath, len(slices), len(names)))

		default:
			ex.Units = append(ex.Units, Unit{
				ID:       p.ImportPath,
				Kind:     kindPackage,
				Seconds:  sec,
				Estimate: estimated,
				Packages: []LivePackage{p},
				Module:   p.Module,
				Mode:     p.Mode,
				Count:    opt.BaseCount,
			})
		}
	}

	sort.Slice(ex.Units, func(i, j int) bool { return ex.Units[i].ID < ex.Units[j].ID })
	sort.Strings(ex.Loaded)
	sort.Strings(ex.Missing)
	return ex, nil
}

// sliceByName packs a package's live runnables — tests, examples and fuzz
// targets alike — into up to `shards` -run slices, weighting each name by
// its recorded per-name time and giving names the store has never seen the
// package's residual per-name average. Unrecorded names are packed exactly
// like recorded ones: that is what keeps a brand-new test (or a newly added
// Example) inside a harpooned whale from vanishing.
func sliceByName(p LivePackage, names []string, row *UnitStat, pkgSeconds float64, shards, baseCount int, estimated bool) []Unit {
	if len(names) == 0 {
		// Deliberately returns nothing rather than an empty -run (which
		// would match everything and duplicate the package across slices).
		// The coverage gate is the backstop that turns this into a loud
		// failure, because `go list` says this package HAS tests.
		return nil
	}
	known := 0.0
	knownCount := 0
	for _, n := range names {
		if w, ok := row.Tests[n]; ok && w > 0 {
			known += w
			knownCount++
		}
	}
	unknownCount := len(names) - knownCount
	perUnknown := 0.0
	switch {
	case unknownCount == 0:
	case pkgSeconds-known > 0:
		perUnknown = (pkgSeconds - known) / float64(unknownCount)
	case knownCount > 0:
		perUnknown = known / float64(knownCount)
	default:
		perUnknown = pkgSeconds / float64(len(names))
	}

	items := make([]Item, 0, len(names))
	for _, n := range names {
		w := perUnknown
		if v, ok := row.Tests[n]; ok && v > 0 {
			w = v
		}
		items = append(items, Item{ID: n, Weight: w})
	}

	groups := karmarkarKarp(items, shards)
	units := make([]Unit, 0, shards)
	for _, g := range groups {
		if len(g) == 0 {
			// Fewer live tests than requested slices; an empty slice would
			// be an invocation that runs nothing.
			continue
		}
		sliceNames := make([]string, 0, len(g))
		total := 0.0
		for _, it := range g {
			sliceNames = append(sliceNames, it.ID)
			total += it.Weight
		}
		sort.Strings(sliceNames)
		units = append(units, Unit{
			ID:       runSliceID(p.ImportPath, sliceNames),
			Kind:     kindRunSlice,
			Seconds:  total,
			Estimate: estimated,
			Packages: []LivePackage{p},
			Module:   p.Module,
			Mode:     p.Mode,
			Count:    baseCount,
			Run:      sliceNames,
		})
	}
	return units
}

// moduleAtoms splits the live set into modules that must be packed whole and
// packages that may mix freely.
//
// This is the "module boundary is a SOFT factor" rule: honoured only where
// correctness needs it. A module that resolves with GOWORK=off cannot share
// an invocation with workspace-mode packages (different build list), so its
// packages ride together; everything inside go.work packs purely for
// balance, across module lines.
func moduleAtoms(live []LivePackage) (map[string][]LivePackage, []LivePackage) {
	atoms := map[string][]LivePackage{}
	var loose []LivePackage
	for _, p := range live {
		if p.Atomic || p.Mode == modeOff {
			atoms[p.Module] = append(atoms[p.Module], p)
			continue
		}
		loose = append(loose, p)
	}
	return atoms, loose
}

// pattern renders the package pattern to pass to `go test`, relative to the
// module directory the invocation runs from.
func (p LivePackage) pattern() string {
	rel := relDir(p.Module, p.Dir)
	if rel == "." {
		return "."
	}
	return "./" + rel
}

func relDir(moduleDir, pkgDir string) string {
	moduleDir = path.Clean(moduleDir)
	pkgDir = path.Clean(pkgDir)
	if moduleDir == pkgDir {
		return "."
	}
	if moduleDir == "." {
		return pkgDir
	}
	if strings.HasPrefix(pkgDir, moduleDir+"/") {
		return strings.TrimPrefix(pkgDir, moduleDir+"/")
	}
	return pkgDir
}

// clampShards bounds a whale's slice count: at least 2 (a "split" into one
// is not a split) and never more than the bucket count, since slices beyond
// K only add compile cost without adding parallelism. Callers must not reach
// it with k < 2; expandUnits suppresses splitting entirely there.
func clampShards(want, k int) int {
	if want < 2 {
		want = 2
	}
	if k >= 2 && want > k {
		return k
	}
	return want
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return a
	}
	if a <= 0 {
		return 1
	}
	n := (a + b - 1) / b
	if n < 1 {
		return 1
	}
	return n
}

// dedupeSorted sorts and de-duplicates a resolver's names. A duplicate would
// otherwise be packed into two slices and run twice.
func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return dedupe(out)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
