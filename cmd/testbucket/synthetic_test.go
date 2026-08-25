package main

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// The tests in this package run entirely against a SYNTHETIC tree and a
// SYNTHETIC store. Nothing here shells out to the Go toolchain: the planner
// takes the live package set and the test-name resolver as data, so the
// whole mechanism — including the coverage gate that is its reason for
// existing — is exercised without a build, a network, or a CI run.
//
// The shape below is baml-rest's, scaled to the design's §2.4 estimates: one
// ~900s whale (bamlutils/llmhttp), a second ~420s dominator
// (internal/debaml), a smooth long tail, one GOWORK=off module that must be
// packed whole, and one package with no test files at all.

const repoPrefix = "github.com/invakid404/baml-rest/"

func livePkg(dir, module, mode string, hasTests bool) LivePackage {
	imp := repoPrefix + dir
	if dir == "." {
		imp = strings.TrimSuffix(repoPrefix, "/")
	}
	return LivePackage{
		ImportPath: imp,
		Dir:        dir,
		Module:     module,
		Mode:       mode,
		Atomic:     mode == modeOff,
		HasTests:   hasTests,
	}
}

func syntheticLive() []LivePackage {
	live := []LivePackage{
		livePkg("bamlutils/llmhttp", "bamlutils", modeWork, true),
		livePkg("bamlutils/buildrequest", "bamlutils", modeWork, true),
		livePkg("bamlutils/sseclient", "bamlutils", modeWork, true),
		livePkg("internal/debaml", ".", modeWork, true),
		livePkg("internal/nativebody", ".", modeWork, true),
		livePkg("pool", "pool", modeWork, true),
		livePkg("worker", "worker", modeWork, true),
		livePkg("cmd/serve", ".", modeWork, true),
		livePkg("dynclient", "dynclient", modeWork, true),
		livePkg("internal/schema", ".", modeWork, true),
		// No test files: not bucketed, not gated — but the moment it gains
		// one, `go list` reports HasTests and it is scheduled.
		livePkg("cmd/embed", ".", modeWork, false),
		// GOWORK=off module: its packages must ride together.
		livePkg("adapters/common", "adapters/common", modeOff, true),
		livePkg("adapters/common/codegen", "adapters/common", modeOff, true),
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ImportPath < live[j].ImportPath })
	return live
}

// syntheticWeights are the measured seconds the synthetic store carries.
var syntheticWeights = map[string]float64{
	"bamlutils/llmhttp":       900,
	"bamlutils/buildrequest":  20,
	"bamlutils/sseclient":     15,
	"internal/debaml":         420,
	"internal/nativebody":     160,
	"pool":                    120,
	"worker":                  110,
	"cmd/serve":               95,
	"dynclient":               80,
	"internal/schema":         25,
	"adapters/common":         12,
	"adapters/common/codegen": 44,
}

// syntheticStore builds a warm store. omit lists package dirs to leave
// unmeasured, so a test can model "these packages are new".
func syntheticStore(omit ...string) *Store {
	skip := map[string]bool{}
	for _, o := range omit {
		skip[o] = true
	}
	st := newStore(canonicalFlags(true, 100))
	st.UpdatedAt = time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	for dir, sec := range syntheticWeights {
		if skip[dir] {
			continue
		}
		st.Units[repoPrefix+dir] = &UnitStat{Seconds: sec, Samples: 12}
	}
	for _, p := range syntheticLive() {
		if p.HasTests && st.Units[p.ImportPath] != nil {
			st.Coverage = append(st.Coverage, p.ImportPath)
		}
	}
	sort.Strings(st.Coverage)
	st.CoverageSource = "go-list"
	return st
}

// harpoon flags a package the way `ingest` would once it crosses the whale
// threshold.
func harpoon(st *Store, dir, policy string, into int, tests map[string]float64) {
	row := st.Units[repoPrefix+dir]
	if row == nil {
		row = &UnitStat{}
		st.Units[repoPrefix+dir] = row
	}
	row.Split = policy
	row.SplitInto = into
	row.Tests = tests
}

// syntheticRunnables stands in for `go test -list`. Packages absent from the
// map report nothing runnable, which is exactly the corruption the coverage
// gate must catch rather than paper over.
//
// Fixtures deliberately mix Test, Example and Fuzz names: `go test -run`
// selects all three, so a universe of only Test* would let the suite prove
// coverage of something narrower than the emitted command executes.
func syntheticRunnables(names map[string][]string) runnableNamer {
	return func(p LivePackage) ([]string, error) {
		return names[p.ImportPath], nil
	}
}

func defaultPlanOptions(live []LivePackage) planOptions {
	return planOptions{
		K:            6,
		StorePath:    "test-timings.json",
		Race:         true,
		Count:        100,
		Timeout:      "20m",
		NodePrefixes: []string{"adapters"},
		StaleAfter:   14 * 24 * time.Hour,
		Now:          time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC),
		Live:         live,
	}
}

// mustPlan fails the test if the plan does not build; the coverage gate is
// supposed to pass on every well-formed input.
func mustPlan(t *testing.T, st *Store, reason string, opt planOptions) *planDocument {
	t.Helper()
	doc, err := buildPlan(st, reason, opt)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	return doc
}

// scheduledPackages counts how many units cover each import path.
func scheduledPackages(doc *planDocument) map[string]int {
	out := map[string]int{}
	for _, b := range doc.Buckets {
		for _, u := range b.Units {
			for _, imp := range u.Packages {
				out[imp]++
			}
		}
	}
	return out
}
