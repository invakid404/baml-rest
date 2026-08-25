package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// storeSchema is the on-disk schema version of the timing store. Bump it
// only for changes that older readers cannot tolerate; `plan` cold-starts
// (rather than mis-reads) any store whose schema it does not recognise.
const storeSchema = 1

// defaultColdSeconds is the per-unit weight used when the store carries no
// measured weight at all. Its absolute value is irrelevant to the resulting
// partition — with every unit equal, any balancer degenerates to an
// equal-count split — but it keeps the reported estimates in a sane unit
// (seconds) instead of showing zeros.
const defaultColdSeconds = 60.0

// Store is the rolling timing store. Per owner decision 3 it is NOT
// committed to the repo: a master-only `record` job writes it to the Actions
// cache/artifact and PR runs restore the last-known-good copy. Nothing in
// this package assumes otherwise — a missing or unreadable store is a
// cold start, never an error.
//
// Rows are keyed by Go IMPORT PATH (the `Package` field of a `go test -json`
// event), not by directory. That is the one identifier both halves of the
// loop see natively: `ingest` reads it straight off the event stream and
// `plan` gets it from `go list`. Everything a bucket needs in order to
// actually invoke `go test` — the module directory, whether the module runs
// under GOWORK=off, whether it must be packed whole — is a property of the
// LIVE tree, so it is resolved from `go list` at plan time and deliberately
// NOT mirrored into the store, where it could go stale and silently emit a
// wrong invocation.
type Store struct {
	Schema int `json:"schema"`
	// Flags is the canonical test-flag set the weights were measured under
	// (e.g. "-race -count=100"). Weights are only comparable within one
	// flag set, so `plan` cold-starts loudly when it does not match. This
	// is the guard against CircleCI's classic "renamed job -> silently bad
	// split for a few runs" failure.
	Flags string `json:"flags"`
	// UpdatedAt is provenance only. It is never used for weighting; `plan`
	// reads it solely to warn when the restored store looks stale, so cache
	// expiry can never be silent.
	UpdatedAt string               `json:"updated_at,omitempty"`
	Units     map[string]*UnitStat `json:"units"`
	// Coverage is the live package set observed by the most recent ingest,
	// sorted. `plan` diffs the current `go list` against it to report drift
	// (packages added or deleted since the store was recorded).
	Coverage       []string `json:"coverage,omitempty"`
	CoverageSource string   `json:"coverage_source,omitempty"`
}

// UnitStat is one package's rolling weight plus its split policy.
type UnitStat struct {
	// Seconds is the EWMA-smoothed wall time of the package under Store.Flags.
	Seconds float64 `json:"seconds"`
	// Samples counts how many ingests have contributed; 0 means the row
	// exists but carries no measurement yet and must be treated as missing.
	Samples int `json:"samples"`
	// Split is the whale policy: "" / "none" (run the package whole),
	// "count" (count-shard it, needs no per-test data) or "run" (slice it by
	// test name, needs Tests). Set automatically by `ingest`.
	Split     string `json:"split,omitempty"`
	SplitInto int    `json:"split_into,omitempty"`
	// SplitReason records WHY this policy was chosen. It is provenance, not
	// input — nothing reads it back — but a store that says "count-sharded
	// because TestX alone is 50% of the package" answers the question a
	// reviewer of the emitted matrix will actually have.
	SplitReason string `json:"split_reason,omitempty"`
	// Tests holds per-test weights, recorded only for packages `ingest` has
	// flagged as split candidates. Keeping per-test rows for the whole tree
	// would bloat the store and churn on every test rename for no benefit —
	// only whales are ever sliced by name.
	Tests map[string]float64 `json:"tests,omitempty"`
}

const (
	splitNone  = "none"
	splitCount = "count"
	splitRun   = "run"
)

// splitPolicy normalises the stored policy; an unknown value degrades to
// "none" (run the package whole), which is always safe: it can cost
// wall-time, never coverage.
func (u *UnitStat) splitPolicy() string {
	if u == nil {
		return splitNone
	}
	switch u.Split {
	case splitCount, splitRun:
		if u.SplitInto >= 2 {
			return u.Split
		}
		return splitNone
	default:
		return splitNone
	}
}

// measured reports whether the row carries a usable measurement.
func (u *UnitStat) measured() bool {
	return u != nil && u.Samples > 0 && u.Seconds > 0
}

// canonicalFlags renders the flag set the weights are comparable within.
// -timeout is deliberately excluded: it bounds a run, it does not change how
// much work the run does.
func canonicalFlags(race bool, count int) string {
	var sb strings.Builder
	if race {
		sb.WriteString("-race ")
	}
	fmt.Fprintf(&sb, "-count=%d", count)
	return sb.String()
}

func newStore(flags string) *Store {
	return &Store{Schema: storeSchema, Flags: flags, Units: map[string]*UnitStat{}}
}

// loadStore reads a store from path. path may be "-" for stdin. A missing
// file is NOT an error — it is the cold start — and is reported through the
// returned reason so the caller can say so out loud.
func loadStore(path string) (st *Store, reason string, err error) {
	var data []byte
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, "", fmt.Errorf("read store from stdin: %w", err)
		}
	} else {
		data, err = os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil, fmt.Sprintf("no store at %s", path), nil
		}
		if err != nil {
			return nil, "", fmt.Errorf("read store %s: %w", path, err)
		}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Sprintf("store %s is empty", storeName(path)), nil
	}
	st = &Store{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, "", fmt.Errorf("parse store %s: %w", storeName(path), err)
	}
	if st.Units == nil {
		st.Units = map[string]*UnitStat{}
	}
	if st.Schema != storeSchema {
		return nil, fmt.Sprintf("store %s has schema %d, this tool speaks schema %d", storeName(path), st.Schema, storeSchema), nil
	}
	return st, "", nil
}

func storeName(path string) string {
	if path == "-" {
		return "<stdin>"
	}
	return path
}

// save writes the store atomically (temp file + rename) so a killed or
// concurrent CI step can never leave a half-written store behind for the
// next run to restore.
func (s *Store) save(path string) error {
	s.Schema = storeSchema
	sort.Strings(s.Coverage)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal store: %w", err)
	}
	data = append(data, '\n')
	if path == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create store dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".test-timings-*.json")
	if err != nil {
		return fmt.Errorf("create temp store: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp store: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename store into place: %w", err)
	}
	return nil
}

// stamp records the wall-clock time of this ingest. Provenance only.
func (s *Store) stamp(now time.Time) {
	s.UpdatedAt = now.UTC().Format(time.RFC3339)
}

// ewma folds a fresh measurement into a rolling weight. The first sample is
// taken verbatim; after that new = alpha*measured + (1-alpha)*old, so a
// single slow runner nudges the split instead of rewriting it.
func ewma(old float64, samples int, measured, alpha float64) float64 {
	if samples <= 0 || old <= 0 {
		return measured
	}
	return alpha*measured + (1-alpha)*old
}

// meanWeight is the cold-start weight for a package with no measurement: the
// mean of the measured weights of the packages that ARE live. Restricting
// the mean to live packages keeps a pile of stale rows for deleted packages
// from dragging the estimate. Returns defaultColdSeconds when nothing at all
// is measured (the true cold start).
func (s *Store) meanWeight(live []LivePackage) (mean float64, measuredCount int, measuredTotal float64) {
	for _, p := range live {
		if !p.HasTests {
			continue
		}
		if row := s.Units[p.ImportPath]; row.measured() {
			measuredCount++
			measuredTotal += row.Seconds
		}
	}
	if measuredCount == 0 {
		return defaultColdSeconds, 0, 0
	}
	return measuredTotal / float64(measuredCount), measuredCount, measuredTotal
}

// staleRows lists store rows that no longer correspond to a live package.
// They are harmless to `plan` (it iterates the live set, never the store),
// but reporting them makes a rename or deletion visible instead of silent.
func (s *Store) staleRows(live []LivePackage) []string {
	liveSet := make(map[string]bool, len(live))
	for _, p := range live {
		liveSet[p.ImportPath] = true
	}
	var stale []string
	for path, row := range s.Units {
		if !row.measured() {
			continue
		}
		if !liveSet[path] {
			stale = append(stale, path)
		}
	}
	sort.Strings(stale)
	return stale
}

// coverageDrift diffs the live package set against the set recorded by the
// last ingest. Neither direction is fatal — a new package is scheduled on
// the mean weight and a deleted one simply never appears — but both are
// reported so a store that has silently drifted away from the tree is
// visible in the job log.
func (s *Store) coverageDrift(live []LivePackage) (added, removed []string) {
	if len(s.Coverage) == 0 {
		return nil, nil
	}
	recorded := make(map[string]bool, len(s.Coverage))
	for _, p := range s.Coverage {
		recorded[p] = true
	}
	liveSet := make(map[string]bool, len(live))
	for _, p := range live {
		if !p.HasTests {
			continue
		}
		liveSet[p.ImportPath] = true
		if !recorded[p.ImportPath] {
			added = append(added, p.ImportPath)
		}
	}
	for _, p := range s.Coverage {
		if !liveSet[p] {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// age returns how long ago the store was recorded. ok is false when the
// store carries no parsable timestamp.
func (s *Store) age(now time.Time) (d time.Duration, ok bool) {
	if s.UpdatedAt == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s.UpdatedAt)
	if err != nil {
		return 0, false
	}
	return now.Sub(t), true
}

func humanSeconds(sec float64) string {
	if math.IsNaN(sec) || math.IsInf(sec, 0) {
		return "?"
	}
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	return fmt.Sprintf("%.1fs (%s)", sec, d)
}
