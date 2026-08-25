package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Invocation is one concrete `go test` call a bucket must make.
type Invocation struct {
	// Dir is the directory to run from, relative to the repo root.
	Dir string `json:"dir"`
	// Env holds the resolution-mode envelope (GOWORK=off for modules that
	// are not go.work members).
	Env  map[string]string `json:"env,omitempty"`
	Args []string          `json:"args"`
	Desc string            `json:"desc"`
}

type planUnit struct {
	ID   string   `json:"id"`
	Kind unitKind `json:"kind"`
	// Packages is what this unit actually runs. It is spelled out rather
	// than left implicit in the ID because a module-atom unit covers
	// several packages, and a reader auditing the plan artifact should not
	// have to re-derive that.
	Packages  []string `json:"packages"`
	Seconds   float64  `json:"est_seconds"`
	Estimated bool     `json:"estimated,omitempty"`
}

type planBucket struct {
	Index       int          `json:"bucket"`
	Name        string       `json:"name"`
	Seconds     float64      `json:"est_seconds"`
	NeedsNode   bool         `json:"needs_node"`
	Units       []planUnit   `json:"units"`
	Invocations []Invocation `json:"invocations"`
	Script      string       `json:"script"`
}

// planSummary is the loaded-vs-missing report required by owner decision 3.
// It exists so that a store that has silently expired, been keyed wrong, or
// drifted away from the tree shows up in the job log as numbers rather than
// as a mysteriously slow matrix three weeks later.
type planSummary struct {
	LivePackages     int      `json:"live_packages"`
	Loaded           int      `json:"loaded"`
	Missing          int      `json:"missing"`
	MissingPackages  []string `json:"missing_packages,omitempty"`
	MeasuredSeconds  float64  `json:"measured_seconds"`
	EstimatedSeconds float64  `json:"estimated_seconds"`
	MeanSeconds      float64  `json:"mean_seconds"`
	TotalSeconds     float64  `json:"total_seconds"`
	ScheduledUnits   int      `json:"scheduled_units"`
	StaleRows        []string `json:"stale_rows,omitempty"`
	DriftAdded       []string `json:"drift_added,omitempty"`
	DriftRemoved     []string `json:"drift_removed,omitempty"`
	ColdStart        bool     `json:"cold_start"`
	ColdStartReason  string   `json:"cold_start_reason,omitempty"`
	StoreAge         string   `json:"store_age,omitempty"`
	Stale            bool     `json:"stale,omitempty"`
	IdealSeconds     float64  `json:"ideal_seconds"`
	MakespanSeconds  float64  `json:"makespan_seconds"`
	LightestSeconds  float64  `json:"lightest_seconds"`
	ImbalancePct     float64  `json:"imbalance_pct"`
}

type planDocument struct {
	K         int          `json:"k"`
	Flags     string       `json:"flags"`
	Algorithm string       `json:"algorithm"`
	StorePath string       `json:"store"`
	UpdatedAt string       `json:"store_updated_at,omitempty"`
	Summary   planSummary  `json:"summary"`
	Buckets   []planBucket `json:"buckets"`
	Notes     []string     `json:"notes,omitempty"`
}

type planOptions struct {
	K            int
	StorePath    string
	Race         bool
	Count        int
	Timeout      string
	NodePrefixes []string
	EventsDir    string
	StaleAfter   time.Duration
	Now          time.Time
	Live         []LivePackage
	Runnables    runnableNamer
}

// validate rejects settings that would emit an invalid or meaningless
// matrix. A non-positive -count is the sharp one: `go test -count=0` runs
// nothing at all, so a plan built from it would be a complete, balanced,
// gate-passing matrix that executes zero tests.
func (o planOptions) validate() error {
	switch {
	case o.K < 1:
		return fmt.Errorf("--k must be >= 1, got %d", o.K)
	case o.Count < 1:
		return fmt.Errorf("--count must be >= 1, got %d", o.Count)
	case o.StaleAfter < 0:
		return fmt.Errorf("--stale-after must be >= 0, got %v", o.StaleAfter)
	}
	// The timeout is spliced verbatim into every emitted invocation, so an
	// unparsable value fails every bucket of the matrix at once — far from
	// where the typo is.
	if o.Timeout != "" {
		d, err := time.ParseDuration(o.Timeout)
		if err != nil {
			return fmt.Errorf("--timeout %q is not a Go duration: %w", o.Timeout, err)
		}
		if d < 0 {
			return fmt.Errorf("--timeout must be >= 0, got %v", d)
		}
	}
	return nil
}

// buildPlan is the whole planner as a pure function of (live tree, store,
// options): no I/O, no clock beyond opt.Now. Everything the CLI does around
// it is reading the store, resolving the live set, and printing.
func buildPlan(st *Store, reason string, opt planOptions) (*planDocument, error) {
	if err := opt.validate(); err != nil {
		return nil, err
	}
	flags := canonicalFlags(opt.Race, opt.Count)

	coldStart := false
	coldReason := reason
	if st == nil {
		coldStart = true
		st = newStore(flags)
	} else if st.Flags != "" && st.Flags != flags {
		// Weights measured under a different flag set are not comparable;
		// using them would produce a confidently wrong split. This is the
		// guard against the "renamed job, silently bad split" trap.
		coldStart = true
		coldReason = fmt.Sprintf("store was recorded under flags %q but this plan runs %q", st.Flags, flags)
		st = newStore(flags)
	}
	if coldReason != "" {
		coldStart = true
	}

	mean, measuredCount, _ := st.meanWeight(opt.Live)
	ex, err := expandUnits(opt.Live, st, expandOptions{
		K:           opt.K,
		BaseCount:   opt.Count,
		MeanSeconds: mean,
		Runnables:   opt.Runnables,
	})
	if err != nil {
		return nil, err
	}

	items := make([]Item, 0, len(ex.Units))
	byID := make(map[string]Unit, len(ex.Units))
	for _, u := range ex.Units {
		items = append(items, u.item())
		byID[u.ID] = u
	}
	groups := karmarkarKarp(items, opt.K)

	buckets := make([]Bucket, opt.K)
	for i, g := range groups {
		b := Bucket{Index: i}
		for _, it := range g {
			u := byID[it.ID]
			b.Units = append(b.Units, u)
			b.Seconds += u.Seconds
		}
		buckets[i] = b
	}

	// The gate runs on the FINAL buckets, after partitioning — the point is
	// to prove what will actually be executed, not what was intended.
	if err := assertCoverage(gateInput{
		Live:      opt.Live,
		Buckets:   buckets,
		Runnables: ex.Runnables,
		BaseCount: opt.Count,
	}); err != nil {
		return nil, err
	}

	doc := &planDocument{
		K:         opt.K,
		Flags:     flags,
		Algorithm: "karmarkar-karp",
		StorePath: storeName(opt.StorePath),
		UpdatedAt: st.UpdatedAt,
		Notes:     ex.Notes,
	}
	for _, b := range buckets {
		doc.Buckets = append(doc.Buckets, renderBucket(b, opt))
	}

	stale, staleOK := st.age(opt.Now)
	added, removed := st.coverageDrift(opt.Live)
	total := ex.MeasuredSeconds + ex.EstimatedSeconds
	ideal := total / float64(opt.K)
	maxSec, minSec := 0.0, 0.0
	for i, b := range buckets {
		if i == 0 || b.Seconds > maxSec {
			maxSec = b.Seconds
		}
		if i == 0 || b.Seconds < minSec {
			minSec = b.Seconds
		}
	}
	imbalance := 0.0
	if ideal > 0 {
		imbalance = (maxSec - ideal) / ideal * 100
	}

	doc.Summary = planSummary{
		LivePackages:     len(ex.Loaded) + len(ex.Missing),
		Loaded:           len(ex.Loaded),
		Missing:          len(ex.Missing),
		MissingPackages:  ex.Missing,
		MeasuredSeconds:  ex.MeasuredSeconds,
		EstimatedSeconds: ex.EstimatedSeconds,
		MeanSeconds:      mean,
		TotalSeconds:     total,
		ScheduledUnits:   len(ex.Units),
		StaleRows:        st.staleRows(opt.Live),
		DriftAdded:       added,
		DriftRemoved:     removed,
		ColdStart:        coldStart || measuredCount == 0,
		ColdStartReason:  coldReason,
		IdealSeconds:     ideal,
		MakespanSeconds:  maxSec,
		LightestSeconds:  minSec,
		ImbalancePct:     imbalance,
	}
	if staleOK {
		doc.Summary.StoreAge = stale.Round(time.Minute).String()
		doc.Summary.Stale = opt.StaleAfter > 0 && stale > opt.StaleAfter
	}
	if doc.Summary.ColdStart && doc.Summary.ColdStartReason == "" && measuredCount == 0 {
		doc.Summary.ColdStartReason = "store carries no measurement for any live package"
	}

	if doc.Summary.LivePackages == 0 {
		doc.Notes = append(doc.Notes, "no live package in the module set has test files — the matrix is empty")
	}
	empty := 0
	for _, b := range buckets {
		if len(b.Units) == 0 {
			empty++
		}
	}
	if empty > 0 {
		// Not an error — the matrix is still correct — but K buckets for
		// fewer than K units means paying a job's fixed overhead for
		// nothing.
		doc.Notes = append(doc.Notes, fmt.Sprintf(
			"%d of %d buckets are empty: only %d schedulable units exist, so K=%d is more lanes than there is work",
			empty, opt.K, len(ex.Units), opt.K))
	}
	return doc, nil
}

// renderBucket turns a bucket's units into the concrete invocations the CI
// job will run, merging everything that legitimately shares one `go test`
// call and keeping apart everything that cannot.
func renderBucket(b Bucket, opt planOptions) planBucket {
	pb := planBucket{
		Index:   b.Index,
		Name:    fmt.Sprintf("bucket-%d", b.Index),
		Seconds: b.Seconds,
	}
	type group struct {
		key   string
		mode  string
		dir   string
		count int
		run   []string
		paths []string
		ids   []string
	}
	var order []string
	groups := map[string]*group{}

	for _, u := range b.Units {
		covered := make([]string, 0, len(u.Packages))
		for _, p := range u.Packages {
			covered = append(covered, p.ImportPath)
		}
		pb.Units = append(pb.Units, planUnit{
			ID: u.ID, Kind: u.Kind, Packages: covered, Seconds: u.Seconds, Estimated: u.Estimate,
		})
		if needsNode(u, opt.NodePrefixes) {
			pb.NeedsNode = true
		}

		// Workspace-mode packages resolve by import path from the repo
		// root, so they merge across module lines — the soft boundary.
		// GOWORK=off modules must run from their own directory with their
		// own build list, so they never merge with anything else.
		dir := "."
		if u.Mode == modeOff {
			dir = u.Module
		}
		key := fmt.Sprintf("plain|%s|%s|%d", u.Mode, dir, u.Count)
		switch u.Kind {
		case kindCountShard, kindRunSlice:
			// Each carries its own -count / -run, so it is its own call.
			key = "solo|" + u.ID
		}
		g := groups[key]
		if g == nil {
			g = &group{key: key, mode: u.Mode, dir: dir, count: u.Count, run: u.Run}
			groups[key] = g
			order = append(order, key)
		}
		g.ids = append(g.ids, u.ID)
		for _, p := range u.Packages {
			if u.Mode == modeOff {
				g.paths = append(g.paths, p.pattern())
				continue
			}
			g.paths = append(g.paths, p.ImportPath)
		}
	}

	sort.Strings(order)
	var lines []string
	for _, key := range order {
		g := groups[key]
		sort.Strings(g.paths)
		inv := Invocation{Dir: g.dir, Args: goTestArgs(opt, g.count, g.run, g.paths)}
		if g.mode == modeOff {
			inv.Env = map[string]string{"GOWORK": "off"}
		}
		sort.Strings(g.ids)
		inv.Desc = strings.Join(g.ids, " ")
		pb.Invocations = append(pb.Invocations, inv)
		lines = append(lines, shellLine(inv, opt, pb.Index, len(lines)))
	}
	pb.Script = strings.Join(append([]string{"set -euo pipefail"}, lines...), "\n")
	return pb
}

// serialPackages is the -p value every emitted invocation carries.
//
// It is 1 because the balancer's objective must be the job's wall time, and
// the weights it partitions are SUMMED package elapsed times. `go test` runs
// package test binaries in parallel by default, so a coalesced invocation
// would finish in something closer to the bucket's critical package than its
// sum — the planner would be optimising a cost function the runner does not
// have, and a bucket estimated at 400s could really take 150s while another
// estimated at 380s took 380s. Serialising the packages makes the measured
// sum the thing that actually happens, and it makes the timings ingest
// records contention-free and therefore comparable across runs.
//
// This is NOT `-parallel`: subtests inside one package still run in
// parallel, and that concurrency is already inside the package's measured
// elapsed time. Only cross-package concurrency is given up, and it is bought
// back — with far better balance — by the K buckets themselves.
const serialPackages = 1

func goTestArgs(opt planOptions, count int, run []string, paths []string) []string {
	args := []string{"go", "test"}
	if opt.Race {
		args = append(args, "-race")
	}
	args = append(args, fmt.Sprintf("-p=%d", serialPackages))
	args = append(args, fmt.Sprintf("-count=%d", count))
	if opt.Timeout != "" {
		args = append(args, "-timeout", opt.Timeout)
	}
	if len(run) > 0 {
		// Anchored alternation: without ^...$ a slice named TestFoo would
		// also pull in TestFooBar and run it twice across two slices.
		args = append(args, "-run", fmt.Sprintf("^(%s)$", strings.Join(run, "|")))
	}
	if opt.EventsDir != "" {
		args = append(args, "-json")
	}
	return append(args, paths...)
}

func shellLine(inv Invocation, opt planOptions, bucket, seq int) string {
	var sb strings.Builder
	sb.WriteString("( cd ")
	sb.WriteString(shellQuote(inv.Dir))
	sb.WriteString(" && ")
	// The resolution-mode envelope is a command PREFIX, not a standalone
	// assignment: `GOWORK=off && go test` would set nothing for go test.
	for _, k := range sortedKeys(inv.Env) {
		fmt.Fprintf(&sb, "%s=%s ", k, shellQuote(inv.Env[k]))
	}
	for i, a := range inv.Args {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(shellQuote(a))
	}
	if opt.EventsDir != "" {
		fmt.Fprintf(&sb, " | tee -a %s", shellQuote(fmt.Sprintf("%s/bucket-%d-%02d.ndjson", strings.TrimSuffix(opt.EventsDir, "/"), bucket, seq)))
	}
	sb.WriteString(" )")
	return sb.String()
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r == '.' || r == '/' || r == '-' || r == '_' || r == '=' || r == ':' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsNode(u Unit, prefixes []string) bool {
	for _, p := range u.Packages {
		for _, pre := range prefixes {
			pre = strings.TrimSpace(pre)
			if pre == "" {
				continue
			}
			if p.Dir == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p.Dir, strings.TrimSuffix(pre, "/")+"/") {
				return true
			}
		}
	}
	return false
}

// matrixJSON renders the GitHub-Actions matrix, ready for
// `matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}`.
func (d *planDocument) matrixJSON() ([]byte, error) {
	type entry struct {
		Bucket      int          `json:"bucket"`
		Name        string       `json:"name"`
		Seconds     float64      `json:"est_seconds"`
		NeedsNode   bool         `json:"needs_node"`
		Units       []string     `json:"units"`
		Invocations []Invocation `json:"invocations"`
		Script      string       `json:"script"`
	}
	out := struct {
		Include []entry `json:"include"`
	}{}
	for _, b := range d.Buckets {
		e := entry{
			Bucket:      b.Index,
			Name:        b.Name,
			Seconds:     round1(b.Seconds),
			NeedsNode:   b.NeedsNode,
			Invocations: b.Invocations,
			Script:      b.Script,
		}
		for _, u := range b.Units {
			e.Units = append(e.Units, u.ID)
		}
		out.Include = append(out.Include, e)
	}
	return json.Marshal(out)
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// errWriter records the first write failure and swallows the rest, so a
// report built from dozens of Fprintf calls can be checked once at the end
// instead of threading an error through every line.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		// Report the length as written: the caller is mid-report and the
		// first error is the one worth keeping.
		return len(p), nil
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// writeSummary prints the human report. It goes to the job log (stderr when
// the matrix is on stdout) precisely so that staleness is never silent: the
// loaded-vs-missing block is the whole early-warning system for a store
// that expired out of the Actions cache — which is why a failure to write it
// is returned rather than dropped.
func (d *planDocument) writeSummary(out io.Writer, shortenPrefix string) error {
	ew := &errWriter{w: out}
	w := io.Writer(ew)
	s := d.Summary
	fmt.Fprintf(w, "testbucket plan — K=%d, algorithm=%s, flags %q\n", d.K, d.Algorithm, d.Flags)
	fmt.Fprintf(w, "store: %s", d.StorePath)
	if d.UpdatedAt != "" {
		fmt.Fprintf(w, " (recorded %s", d.UpdatedAt)
		if s.StoreAge != "" {
			fmt.Fprintf(w, ", %s ago", s.StoreAge)
		}
		fmt.Fprint(w, ")")
	}
	fmt.Fprintln(w)

	if s.ColdStart {
		fmt.Fprintf(w, "\n*** COLD START: %s ***\n", firstNonEmpty(s.ColdStartReason, "no usable weights"))
		fmt.Fprintf(w, "    Every unweighted unit gets the mean weight (%.1fs). The matrix is valid and\n", s.MeanSeconds)
		fmt.Fprintf(w, "    complete, but only count-balanced until the next master `record` lands.\n")
	}
	if s.Stale {
		fmt.Fprintf(w, "\n*** STALE STORE: last recorded %s ago — the split is running on old timings. ***\n", s.StoreAge)
	}

	fmt.Fprintf(w, "\nloaded vs missing\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  live test packages\t%d\t\n", s.LivePackages)
	fmt.Fprintf(tw, "  loaded (recorded timing)\t%d\tmeasured wall-time %s\n", s.Loaded, humanSeconds(s.MeasuredSeconds))
	fmt.Fprintf(tw, "  missing (mean estimate)\t%d\testimated %s @ mean %.1fs\n", s.Missing, humanSeconds(s.EstimatedSeconds), s.MeanSeconds)
	fmt.Fprintf(tw, "  scheduled units\t%d\ttotal scheduled work %s\n", s.ScheduledUnits, humanSeconds(s.TotalSeconds))
	if len(s.StaleRows) > 0 {
		fmt.Fprintf(tw, "  store rows with no live package\t%d\t%s\n", len(s.StaleRows), truncList(s.StaleRows, 3))
	}
	if len(s.DriftAdded) > 0 || len(s.DriftRemoved) > 0 {
		fmt.Fprintf(tw, "  coverage drift vs store\t+%d / -%d\t%s\n", len(s.DriftAdded), len(s.DriftRemoved), truncList(append(append([]string{}, s.DriftAdded...), s.DriftRemoved...), 3))
	}
	_ = tw.Flush()
	if len(s.MissingPackages) > 0 {
		fmt.Fprintf(w, "  estimated packages: %s\n", truncList(s.MissingPackages, 8))
	}

	fmt.Fprintf(w, "\nbalance\n")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  ideal (total/K)\t%s\n", humanSeconds(s.IdealSeconds))
	fmt.Fprintf(tw, "  makespan (heaviest)\t%s\n", humanSeconds(s.MakespanSeconds))
	fmt.Fprintf(tw, "  lightest\t%s\n", humanSeconds(s.LightestSeconds))
	fmt.Fprintf(tw, "  imbalance over ideal\t%.1f%%\n", s.ImbalancePct)
	_ = tw.Flush()

	fmt.Fprintf(w, "\nbuckets\n")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  bucket\test\tnode\tunits\n")
	for _, b := range d.Buckets {
		names := make([]string, 0, len(b.Units))
		for _, u := range b.Units {
			names = append(names, displayID(u.ID, shortenPrefix))
		}
		node := "-"
		if b.NeedsNode {
			node = "node"
		}
		fmt.Fprintf(tw, "  %d\t%.1fs\t%s\t%s\n", b.Index, b.Seconds, node, truncList(names, 6))
	}
	_ = tw.Flush()

	if len(d.Notes) > 0 {
		fmt.Fprintf(w, "\nsplit notes\n")
		for _, n := range d.Notes {
			fmt.Fprintf(w, "  - %s\n", shortenID(n, shortenPrefix))
		}
	}
	_, _ = fmt.Fprintf(w, "\ncoverage gate: PASS — every live package, every runnable (test, example\n")
	_, _ = fmt.Fprintf(w, "or fuzz target) of every name-sliced package, and every count-shard of\n")
	_, _ = fmt.Fprintf(w, "every sharded package is assigned to exactly one bucket; each sharded\n")
	_, _ = fmt.Fprintf(w, "package's shards add back up to the requested -count.\n")
	_, _ = fmt.Fprintf(w, "execution model: -p=%d, so a bucket's estimate is its serial wall time.\n", serialPackages)
	return ew.err
}

// shortenID trims the repo's import-path prefix for display only. The
// canonical IDs in the matrix and the store stay fully qualified.
func shortenID(id, prefix string) string {
	if prefix == "" {
		return id
	}
	return strings.ReplaceAll(id, prefix, "")
}

// displayID additionally collapses a run-slice's test-name alternation,
// which can run to hundreds of characters, down to its size. The full list
// stays in the matrix and the --shard-plan artifact.
func displayID(id, prefix string) string {
	short := shortenID(id, prefix)
	open := strings.IndexByte(short, '[')
	if open < 0 || !strings.HasSuffix(short, "]") {
		return short
	}
	n := strings.Count(short[open+1:len(short)-1], "|") + 1
	return fmt.Sprintf("%s[%d tests]", short[:open], n)
}

// commonImportPrefix finds the longest shared import-path prefix ending at a
// path separator, used purely to keep the human table readable.
func commonImportPrefix(live []LivePackage) string {
	prefix := ""
	for i, p := range live {
		if i == 0 {
			prefix = p.ImportPath
			continue
		}
		prefix = sharedPrefix(prefix, p.ImportPath)
	}
	if idx := strings.LastIndex(prefix, "/"); idx >= 0 {
		return prefix[:idx+1]
	}
	return ""
}

func sharedPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

func truncList(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, … (+%d more)", strings.Join(items[:limit], ", "), len(items)-limit)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
