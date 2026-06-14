package testharness

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// HeavyTestRunsEnv is the environment variable that caps how many
// times an opted-in heavy test actually executes under `go test
// -count=N`. It exists because the unit-tests workflow runs the whole
// suite with `-race -count=100` for race-stress coverage, but a
// handful of subprocess-backed codegen tests (each shelling out to an
// inner `go build` / `go test`) would dominate CI wallclock at 100
// repeats while adding little marginal race coverage.
//
// Semantics:
//   - Unset or empty: UNCAPPED. This preserves the local `go test`
//     default and the nightly fuzz workflow, neither of which should
//     silently lose `-count` iterations.
//   - Positive integer N: opted-in tests run for iterations 1..N and
//     skip on iteration N+1 and later.
//   - Invalid or non-positive: the calling test FAILS fast. A typo in
//     CI (e.g. BAML_HEAVY_TEST_RUNS=abc) must not silently remove the
//     cap.
//
// The .github/workflows/unit-tests.yml `unit-tests` job sets this to
// "5"; no other workflow sets it.
const HeavyTestRunsEnv = "BAML_HEAVY_TEST_RUNS"

// heavyTestRunCounts tracks, per logical cap key, how many times
// CapStressKey has been reached in this test process. `go test
// -count=N` reruns a package's tests within a single test process, so
// package-level state accumulates across the N iterations — that is
// the load-bearing assumption that lets this counter cap repeats.
// Counters are *atomic.Int64 because parallel tests / parallel
// subtests may reach the helper concurrently.
var heavyTestRunCounts sync.Map // map[string]*atomic.Int64

// CapStress is the ergonomic default for a top-level heavy test whose
// logical cap unit is exactly its name. Call it before expensive setup
// and before t.Parallel().
func CapStress(t *testing.T) {
	t.Helper()
	CapStressKey(t, t.Name())
}

// CapStressKey caps a heavy test keyed by an explicit, stable `key`.
// Use it for subtests / table-driven rows where the logical cap unit
// is not exactly t.Name() — the key must come from a deterministic
// case id, not map iteration order or a duplicate generated subtest
// name.
//
// If HeavyTestRunsEnv is unset/empty the call returns immediately
// (uncapped). If it is a positive integer N, the first N reaches for
// `key` proceed and every later reach calls t.Skipf. An invalid or
// non-positive value fails the test via t.Fatalf.
func CapStressKey(t *testing.T, key string) {
	t.Helper()

	limit, ok := heavyTestRunCap(t)
	if !ok {
		// Unset/empty: uncapped.
		return
	}

	counterAny, _ := heavyTestRunCounts.LoadOrStore(key, new(atomic.Int64))
	counter := counterAny.(*atomic.Int64)
	run := counter.Add(1)
	if run > int64(limit) {
		t.Skipf("%s: skipping run %d (cap %d via %s=%d); unset %s for uncapped runs",
			key, run, limit, HeavyTestRunsEnv, limit, HeavyTestRunsEnv)
	}
}

// heavyTestRunCap parses HeavyTestRunsEnv. It returns (0, false) when
// the var is unset/empty (uncapped). For a positive integer it returns
// (N, true). For any invalid or non-positive value it fails the test
// via t.Fatalf rather than silently uncapping — a CI typo must not
// quietly drop the cap.
func heavyTestRunCap(t *testing.T) (int, bool) {
	t.Helper()
	raw, set := os.LookupEnv(HeavyTestRunsEnv)
	if !set || raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a valid integer: %v", HeavyTestRunsEnv, raw, err)
	}
	if n <= 0 {
		t.Fatalf("%s=%q must be a positive integer (got %d); unset it for uncapped runs", HeavyTestRunsEnv, raw, n)
	}
	return n, true
}
