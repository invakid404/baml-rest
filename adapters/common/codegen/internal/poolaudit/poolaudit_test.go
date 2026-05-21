package poolaudit

import (
	"sync"
	"testing"
)

type elem struct {
	N int
	S string
}

func TestResetClearsAll(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	OnCheckout("A")
	OnRelease("A")
	CheckZeroPrePut("A", []elem{{N: 1}})
	if got := Snapshot(); got.Checkouts["A"] != 1 || got.Releases["A"] != 1 {
		t.Fatalf("pre-reset snapshot wrong: %+v", got)
	}
	if got := ZeroOnPutViolations(); len(got) != 1 {
		t.Fatalf("pre-reset violations wrong: %+v", got)
	}
	Reset()
	if got := Snapshot(); len(got.Checkouts) != 0 || len(got.Releases) != 0 {
		t.Fatalf("post-reset snapshot not empty: %+v", got)
	}
	if got := ZeroOnPutViolations(); len(got) != 0 {
		t.Fatalf("post-reset violations not empty: %+v", got)
	}
}

func TestCheckoutReleaseBalance(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	OnCheckout("X")
	OnCheckout("X")
	OnRelease("X")
	OnRelease("X")
	if got := Imbalanced(); len(got) != 0 {
		t.Fatalf("balanced X reported as imbalanced: %v", got)
	}
	OnCheckout("Y")
	got := Imbalanced()
	if len(got) != 1 {
		t.Fatalf("expected one imbalance entry for Y, got %v", got)
	}
}

func TestCheckZeroPrePutAllZero(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	CheckZeroPrePut("E", []elem{{}, {}, {}})
	if got := ZeroOnPutViolations(); len(got) != 0 {
		t.Fatalf("all-zero slice produced violations: %v", got)
	}
}

func TestCheckZeroPrePutNonZero(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	CheckZeroPrePut("E", []elem{{}, {N: 7}, {}, {S: "x"}})
	got := ZeroOnPutViolations()
	if len(got) != 2 {
		t.Fatalf("expected 2 violations, got %d: %v", len(got), got)
	}
	if got[0].Index != 1 || got[1].Index != 3 {
		t.Fatalf("violation indices wrong: %v", got)
	}
}

func TestCheckZeroPrePutEmptySlice(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	CheckZeroPrePut("E", []elem{})
	if got := ZeroOnPutViolations(); len(got) != 0 {
		t.Fatalf("empty slice produced violations: %v", got)
	}
}

func TestCheckZeroPrePutNilSlice(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	CheckZeroPrePut("E", nil)
	if got := ZeroOnPutViolations(); len(got) != 0 {
		t.Fatalf("nil produced violations: %v", got)
	}
}

func TestCheckZeroPrePutWrongKind(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	CheckZeroPrePut("E", 42)
	got := ZeroOnPutViolations()
	if len(got) != 1 || got[0].Index != -1 {
		t.Fatalf("expected kind-mismatch violation, got %v", got)
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	OnCheckout("A")
	snap := Snapshot()
	snap.Checkouts["A"] = 99
	if got := Snapshot(); got.Checkouts["A"] != 1 {
		t.Fatalf("mutating snapshot leaked into package state: %v", got)
	}
}

// TestRaceClean drives counters AND the violations slice from many
// goroutines so go test -race surfaces any missing synchronization on
// either path. Violations recording and reading share the same mutex
// as the counter maps today; the extra goroutines guard against a
// future refactor that splits the lock and lets the slice access
// race.
func TestRaceClean(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	var wg sync.WaitGroup
	const n = 50
	wg.Add(n * 5)
	for i := 0; i < n; i++ {
		go func() { defer wg.Done(); OnCheckout("R") }()
		go func() { defer wg.Done(); OnRelease("R") }()
		go func() { defer wg.Done(); _ = Snapshot() }()
		go func() { defer wg.Done(); CheckZeroPrePut("R", []elem{{N: 1}}) }()
		go func() { defer wg.Done(); _ = ZeroOnPutViolations() }()
	}
	wg.Wait()
	got := Snapshot()
	if got.Checkouts["R"] != n || got.Releases["R"] != n {
		t.Fatalf("counter loss under concurrency: %+v", got)
	}
	if v := ZeroOnPutViolations(); len(v) != n {
		t.Fatalf("violation loss under concurrency: got %d, want %d", len(v), n)
	}
}
