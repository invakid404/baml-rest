package bamlunicode

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// capStress mirrors adapters/common/codegen/internal/testharness.CapStress for
// the root module, which cannot import that module's internal package. The
// unit-tests workflow runs the whole suite with `-race -count=100` for race
// stress; a few heavy-but-DETERMINISTIC tests here (full 1.1M-scalar delta
// scan, the 19,965-row NormalizationTest conformance suite, multi-MB archive
// hash/unzip) would dominate CI wallclock at 100 repeats while adding zero
// marginal race coverage — they are pure functions over fixed committed data.
//
// Semantics match testharness.CapStress exactly (same env var, same behavior):
//   - BAML_HEAVY_TEST_RUNS unset/empty: UNCAPPED (local `go test` default).
//   - Positive integer N: the test runs iterations 1..N and skips N+1+ within
//     the same test process (go test -count reruns tests in one process, so the
//     package-level counter accumulates).
//   - Invalid/non-positive: FAIL fast — a CI typo must not silently uncap.
//
// The unit-tests `unit-tests` job sets BAML_HEAVY_TEST_RUNS=5.
const heavyTestRunsEnv = "BAML_HEAVY_TEST_RUNS"

var heavyTestRunCounts sync.Map // key -> *atomic.Int64

func capStress(t *testing.T) {
	t.Helper()
	raw, set := os.LookupEnv(heavyTestRunsEnv)
	if !set || raw == "" {
		return // uncapped
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a valid integer: %v", heavyTestRunsEnv, raw, err)
	}
	if n <= 0 {
		t.Fatalf("%s=%q must be a positive integer (got %d); unset it for uncapped runs", heavyTestRunsEnv, raw, n)
	}
	counterAny, _ := heavyTestRunCounts.LoadOrStore(t.Name(), new(atomic.Int64))
	if run := counterAny.(*atomic.Int64).Add(1); run > int64(n) {
		t.Skipf("%s: skipping run %d (cap %d via %s); unset %s for uncapped runs",
			t.Name(), run, n, heavyTestRunsEnv, heavyTestRunsEnv)
	}
}
