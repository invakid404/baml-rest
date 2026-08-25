package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// runAudit reconstructs, from the captured events, that a bucketed run
// executed exactly what its plan said it would.
//
// The coverage gate inside `plan` proves the MATRIX is complete before
// anything runs. This proves the RUN was: it is the after-the-fact half, and
// it catches what the gate structurally cannot — a bucket whose job never
// produced events, an artifact that failed to upload, a shard that died
// before reporting. To the gate those are invisible; the plan it approved was
// complete, and it has no view of what happened next.
//
// The check is semantics-aware, because "exactly once" means different things
// per tier and a naive per-package count would be wrong for two of the three:
//
//   - a whole package or module atom runs in ONE invocation;
//   - a count-shard package runs in S invocations, each a slice of the sweep
//     rather than a repeat of the package, so S package-level results is
//     correct and one would mean five shards vanished;
//   - a run-sliced package also runs in S invocations, and additionally its
//     slices' name sets must union to the package's whole runnable set —
//     disjointness and completeness are the property there, not arity.
//
// So the expectation is derived per package from the plan itself: how many
// units cover it, and which names those units name.
func runAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	planPath := fs.String("shard-plan", "", "the plan artifact the run was fanned out from (required)")
	var in stringList
	fs.Var(&in, "in", "go test -json file to audit, or - for stdin; repeatable (extra positional args also count)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *planPath == "" {
		return fmt.Errorf("--shard-plan is required: the audit compares what ran against what was planned")
	}

	inputs := append([]string(nil), in...)
	inputs = append(inputs, fs.Args()...)
	if len(inputs) == 0 {
		return fmt.Errorf("no input: pass the captured event files")
	}

	planned, err := loadPlannedCoverage(*planPath)
	if err != nil {
		return err
	}

	var readers []io.Reader
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	for _, p := range inputs {
		if p == "-" {
			readers = append(readers, os.Stdin)
			continue
		}
		f, ferr := os.Open(p)
		if ferr != nil {
			return fmt.Errorf("open events %s: %w", p, ferr)
		}
		closers = append(closers, f)
		readers = append(readers, f)
	}
	sum, err := parseEvents(readers...)
	if err != nil {
		return err
	}

	return auditCoverage(os.Stdout, planned, sum)
}

// plannedCoverage is what the plan promised, per package.
type plannedCoverage struct {
	// Invocations is how many separate `go test` calls should report this
	// package: 1 for a whole package or atom, S for a sharded or sliced one.
	Invocations map[string]int
	// Runnables is the union of the -run names across a package's slices,
	// present only for run-sliced packages.
	Runnables map[string][]string
	Units     int
}

func loadPlannedCoverage(path string) (*plannedCoverage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read shard plan: %w", err)
	}
	var doc planDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse shard plan %s: %w", path, err)
	}
	out := &plannedCoverage{Invocations: map[string]int{}, Runnables: map[string][]string{}}
	for _, b := range doc.Buckets {
		for _, u := range b.Units {
			out.Units++
			for _, p := range u.Packages {
				out.Invocations[p]++
			}
			if u.Kind == kindRunSlice && len(u.Packages) == 1 {
				// The names are in the unit ID (pkg[A|B|C]) — the same text
				// the emitted -run regex is built from.
				if open := strings.Index(u.ID, "["); open >= 0 && strings.HasSuffix(u.ID, "]") {
					names := strings.Split(u.ID[open+1:len(u.ID)-1], "|")
					out.Runnables[u.Packages[0]] = append(out.Runnables[u.Packages[0]], names...)
				}
			}
		}
	}
	return out, nil
}

// auditCoverage compares the plan against what the events show actually ran.
func auditCoverage(w io.Writer, planned *plannedCoverage, sum *eventSummary) error {
	observed := map[string]int{}
	for pkg, n := range sum.PackageRuns {
		observed[pkg] += n
	}
	for pkg := range sum.Failed {
		if _, ok := sum.PackageRuns[pkg]; !ok {
			// A package that only failed still ran; it reported a result.
			observed[pkg]++
		}
	}

	var missing, short, extra, unplanned []string
	for _, pkg := range sortedKeys(planned.Invocations) {
		want := planned.Invocations[pkg]
		got := observed[pkg]
		switch {
		case got == 0:
			missing = append(missing, fmt.Sprintf("%s (planned %d invocation(s), reported none)", pkg, want))
		case got < want:
			short = append(short, fmt.Sprintf("%s reported %d of %d planned invocation(s)", pkg, got, want))
		case got > want:
			extra = append(extra, fmt.Sprintf("%s reported %d invocation(s), %d were planned", pkg, got, want))
		}
	}
	for _, pkg := range sortedKeys(observed) {
		if _, ok := planned.Invocations[pkg]; !ok {
			unplanned = append(unplanned, fmt.Sprintf("%s reported %d result(s) but was in no bucket", pkg, observed[pkg]))
		}
	}

	// Run-sliced packages: the slices' names must be exactly the top-level
	// runnables the package actually reported. A name planned but never
	// reported means a slice did not run it; one reported but never planned
	// means the -run regex reached past its slice.
	var sliceGaps []string
	for _, pkg := range sortedKeys(planned.Runnables) {
		want := map[string]bool{}
		for _, n := range planned.Runnables[pkg] {
			want[n] = true
		}
		got := sum.TestSeconds[pkg]
		for _, n := range sortedKeys(want) {
			if _, ok := got[n]; !ok {
				sliceGaps = append(sliceGaps, fmt.Sprintf("%s: %s was in a -run slice but never reported", pkg, n))
			}
		}
		for _, n := range sortedKeys(got) {
			if !want[n] {
				sliceGaps = append(sliceGaps, fmt.Sprintf("%s: %s reported but was in no -run slice", pkg, n))
			}
		}
	}

	fmt.Fprintf(w, "testbucket audit — %d planned unit(s) over %d package(s)\n", planned.Units, len(planned.Invocations))
	fmt.Fprintf(w, "  packages that reported a result   %d\n", len(observed))
	fmt.Fprintf(w, "  run-sliced packages name-checked  %d\n", len(planned.Runnables))

	problems := len(missing) + len(short) + len(extra) + len(unplanned) + len(sliceGaps)
	if problems == 0 {
		fmt.Fprintf(w, "\nPASS — every planned package reported exactly the invocations the plan\n")
		fmt.Fprintf(w, "scheduled for it, counting a count-shard group as the one logical package\n")
		fmt.Fprintf(w, "it is, and every -run slice's names are accounted for.\n")
		return nil
	}

	var b strings.Builder
	b.WriteString("coverage audit FAILED: the run did not execute what the plan scheduled")
	for _, group := range []struct {
		label string
		items []string
	}{
		{"package(s) that never reported — a bucket produced no events", missing},
		{"package(s) that reported fewer invocations than planned — a shard or slice is missing", short},
		{"package(s) that reported more invocations than planned", extra},
		{"package(s) that ran but were in no bucket", unplanned},
		{"-run slice discrepancies", sliceGaps},
	} {
		if len(group.items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n  %s:", group.label)
		for _, it := range group.items {
			fmt.Fprintf(&b, "\n    - %s", it)
		}
	}
	b.WriteString("\n\nThe plan's coverage gate proves the MATRIX is complete before anything\n" +
		"runs. This proves the RUN was. A bucket that produced no events is\n" +
		"invisible to the gate and is exactly what this catches.")
	fmt.Fprintln(w)
	return fmt.Errorf("%s", b.String())
}
