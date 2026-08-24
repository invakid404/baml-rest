// Command testbucket splits a Go repository's unit tests into K
// time-balanced buckets and keeps the split honest as the tests change.
//
// It is the mechanism behind a bucketed unit-test workflow: `plan` turns a
// rolling timing store plus the live package set into a GitHub-Actions
// matrix, and `ingest` folds each run's `go test -json` output back into the
// store so the next split is better than the last.
//
//	testbucket plan   --k 6 --store test-timings.json --json
//	testbucket ingest --in events.ndjson --store test-timings.json
//
// Three properties are load-bearing:
//
//   - NEVER DROP A TEST. `plan` enumerates the LIVE tree (`go list ./...`
//     intersected with the module set), not the store, and refuses to emit a
//     matrix unless every live package — and every test func of every
//     name-sliced package — lands in exactly one bucket. A balanced but
//     incomplete split is the one failure mode worse than an imbalanced one,
//     because nothing goes red.
//
//   - COLD START IS NORMAL, NOT AN ERROR. The store is a rolling CI cache,
//     not a committed file, so a miss is routine. Any package without a
//     recorded weight gets the mean weight and is scheduled immediately; its
//     real weight lands on the next master record.
//
//   - STALENESS IS NEVER SILENT. Every `plan` prints a loaded-vs-missing
//     summary — how many units carry real timings, how much measured
//     wall-time they account for, how far the store has drifted from the
//     tree — so an expired or mis-keyed cache shows up as numbers in the job
//     log instead of as a quietly worse split.
//
// The balancer is Karmarkar-Karp (largest differencing), deterministic down
// to the tie-break, so the same store and the same K always produce the same
// buckets.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const usage = `testbucket — time-balanced unit-test bucketing

usage:
  testbucket plan   [flags]   compute K buckets and emit a GH-Actions matrix
  testbucket ingest [flags]   fold go test -json timings back into the store

run "testbucket <subcommand> -h" for the flags of each.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "plan":
		err = runPlan(os.Args[2:])
	case "ingest":
		err = runIngest(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "testbucket: unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "testbucket: %v\n", err)
		os.Exit(1)
	}
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	k := fs.Int("k", 6, "number of buckets (the single knob: adding a lane = bumping K)")
	store := fs.String("store", "test-timings.json", "timing store path, or - for stdin; a missing store is a cold start")
	asJSON := fs.Bool("json", false, "write the fromJSON matrix to stdout (summary then goes to stderr)")
	shardPlan := fs.String("shard-plan", "", "also write the full plan (buckets, invocations, summary) as JSON to this path")
	race := fs.Bool("race", true, "weights and invocations assume -race")
	count := fs.Int("count", 100, "-count for the flake sweep; count-shards divide it")
	timeout := fs.String("timeout", "20m", "-timeout passed to each go test invocation")
	live := fs.String("live", "", "read the live package set from this JSON file instead of running go list")
	nodePrefixes := fs.String("node-prefixes", "adapters", "comma-separated package-dir prefixes whose buckets need Node set up")
	eventsDir := fs.String("events-dir", "", "if set, emitted invocations add -json and tee events into this directory")
	staleAfter := fs.Duration("stale-after", 14*24*time.Hour, "warn when the store was recorded longer ago than this (0 disables)")
	var excludes stringList
	fs.Var(&excludes, "exclude-module", "module dir (glob) to leave out of the module set; repeatable, replaces the defaults")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repoRoot, err := findRepoRoot(".")
	if err != nil && *live == "" {
		return err
	}

	var livePkgs []LivePackage
	if *live != "" {
		livePkgs, err = loadLivePackages(*live)
		if err != nil {
			return err
		}
	} else {
		ex := defaultExcludedModules
		if len(excludes) > 0 {
			ex = excludes
		}
		mods, err := discoverModules(repoRoot, ex)
		if err != nil {
			return err
		}
		livePkgs, err = listPackages(repoRoot, mods)
		if err != nil {
			return err
		}
	}

	st, reason, err := loadStore(*store)
	if err != nil {
		return err
	}

	doc, err := buildPlan(st, reason, planOptions{
		K:            *k,
		StorePath:    *store,
		Race:         *race,
		Count:        *count,
		Timeout:      *timeout,
		NodePrefixes: strings.Split(*nodePrefixes, ","),
		EventsDir:    *eventsDir,
		StaleAfter:   *staleAfter,
		Now:          time.Now(),
		Live:         livePkgs,
		TestNames:    func(p LivePackage) ([]string, error) { return listTestNames(repoRoot, p) },
	})
	if err != nil {
		return err
	}

	if *shardPlan != "" {
		if err := writeJSONFile(*shardPlan, doc); err != nil {
			return err
		}
	}

	// The summary always reaches the job log; stdout stays machine-clean
	// whenever the caller is capturing the matrix.
	summaryOut := io.Writer(os.Stdout)
	if *asJSON {
		summaryOut = os.Stderr
	}
	doc.writeSummary(summaryOut, commonImportPrefix(livePkgs))

	if *asJSON {
		matrix, err := doc.matrixJSON()
		if err != nil {
			return err
		}
		fmt.Println(string(matrix))
	}
	return nil
}

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	store := fs.String("store", "test-timings.json", "timing store path to create or update")
	alpha := fs.Float64("ewma", 0.5, "EWMA smoothing factor: new = alpha*measured + (1-alpha)*old")
	race := fs.Bool("race", true, "the measured run used -race")
	count := fs.Int("count", 100, "the -count the measured run swept at (aggregate across shards)")
	whaleK := fs.Int("whale-k", 6, "flag a package as a split candidate once it exceeds total/K")
	whaleSeconds := fs.Float64("whale-seconds", 0, "absolute split threshold in seconds; overrides --whale-k")
	minShard := fs.Float64("min-shard-seconds", 30, "never slice a unit into pieces smaller than this; each slice costs a whole CI job's fixed overhead")
	live := fs.String("live", "", "read the live package set from this JSON file instead of running go list")
	noGoList := fs.Bool("no-golist", false, "skip go list; record coverage from the observed events only (no row pruning)")
	var in stringList
	fs.Var(&in, "in", "go test -json file to ingest, or - for stdin; repeatable (extra positional args also count)")
	var excludes stringList
	fs.Var(&excludes, "exclude-module", "module dir (glob) to leave out of the module set; repeatable, replaces the defaults")
	if err := fs.Parse(args); err != nil {
		return err
	}

	inputs := append([]string(nil), in...)
	inputs = append(inputs, fs.Args()...)
	if len(inputs) == 0 {
		return fmt.Errorf("no input: pass --in <go-test-json> (or - for stdin)")
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
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("open events %s: %w", p, err)
		}
		closers = append(closers, f)
		readers = append(readers, f)
	}

	sum, err := parseEvents(readers...)
	if err != nil {
		return err
	}

	var livePkgs []LivePackage
	authoritative := false
	switch {
	case *live != "":
		livePkgs, err = loadLivePackages(*live)
		if err != nil {
			return err
		}
		authoritative = true
	case !*noGoList:
		repoRoot, err := findRepoRoot(".")
		if err != nil {
			return err
		}
		ex := defaultExcludedModules
		if len(excludes) > 0 {
			ex = excludes
		}
		mods, err := discoverModules(repoRoot, ex)
		if err != nil {
			return err
		}
		livePkgs, err = listPackages(repoRoot, mods)
		if err != nil {
			return err
		}
		authoritative = true
	}

	st, reason, err := loadStore(*store)
	if err != nil {
		return err
	}
	if st == nil {
		st = newStore(canonicalFlags(*race, *count))
		if reason != "" {
			fmt.Fprintf(os.Stderr, "testbucket ingest: starting a new store (%s)\n", reason)
		}
	}

	rep := applyIngest(st, sum, ingestOptions{
		Alpha:             *alpha,
		Race:              *race,
		Count:             *count,
		WhaleK:            *whaleK,
		WhaleSeconds:      *whaleSeconds,
		MinShardSeconds:   *minShard,
		Now:               time.Now(),
		Live:              livePkgs,
		LiveAuthoritative: authoritative,
	})
	if err := st.save(*store); err != nil {
		return err
	}
	rep.write(os.Stderr, commonImportPrefix(livePkgs))
	return nil
}

func writeJSONFile(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
