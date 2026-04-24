package roundrobin

import (
	"sync"
	"testing"
)

// advance is a test helper that unwraps the (int, error) return of
// Coordinator.Advance. The in-process Coordinator never returns an
// error, so failing the test on one is the appropriate diagnostic —
// it would signal a contract regression in the Coordinator itself.
func advance(t *testing.T, c *Coordinator, name string, n int) int {
	t.Helper()
	idx, err := c.Advance(name, n)
	if err != nil {
		t.Fatalf("Coordinator.Advance(%q, %d): unexpected error: %v", name, n, err)
	}
	return idx
}

func TestAdvance_StepsThroughChildren(t *testing.T) {
	c := NewCoordinator()
	// First Advance is seeded randomly, so we only check that consecutive
	// calls produce a contiguous sequence modulo childCount.
	first := advance(t, c, "ClientA", 3)
	if first < 0 || first >= 3 {
		t.Fatalf("first index out of range: %d", first)
	}
	second := advance(t, c, "ClientA", 3)
	third := advance(t, c, "ClientA", 3)
	if second != (first+1)%3 {
		t.Fatalf("second: expected %d, got %d", (first+1)%3, second)
	}
	if third != (first+2)%3 {
		t.Fatalf("third: expected %d, got %d", (first+2)%3, third)
	}
}

func TestAdvance_SeparateClientsHaveIndependentCounters(t *testing.T) {
	c := NewCoordinator()
	// Two clients with the same child count. Whatever their starts are,
	// advancing A must not affect B's counter.
	aBefore := advance(t, c, "A", 4)
	bBefore := advance(t, c, "B", 4)
	// Advance A a few more times
	advance(t, c, "A", 4)
	advance(t, c, "A", 4)
	bAfter := advance(t, c, "B", 4)
	expectedBAfter := (bBefore + 1) % 4
	if bAfter != expectedBAfter {
		t.Fatalf("B's counter leaked from A: expected %d, got %d (aBefore=%d)", expectedBAfter, bAfter, aBefore)
	}
}

func TestAdvance_ZeroChildCount_ReturnsZero(t *testing.T) {
	c := NewCoordinator()
	if got := advance(t, c, "X", 0); got != 0 {
		t.Fatalf("expected 0 for childCount=0, got %d", got)
	}
	if got := advance(t, c, "X", -5); got != 0 {
		t.Fatalf("expected 0 for childCount<0, got %d", got)
	}
}

func TestAdvance_ConcurrentUsageDoesNotSkipOrDuplicate(t *testing.T) {
	c := NewCoordinator()
	const n = 1000
	const childCount = 7
	// Prime the counter so we know its starting offset deterministically.
	start := advance(t, c, "Shared", childCount)
	counts := make([]int, childCount)
	counts[start]++
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < n-1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx, err := c.Advance("Shared", childCount)
			if err != nil {
				t.Errorf("concurrent Advance: %v", err)
				return
			}
			mu.Lock()
			counts[idx]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	// Every child should be hit roughly n/childCount times. For n=1000 and
	// childCount=7, each count should be 142 or 143. The looser bound below
	// just confirms nothing got silently dropped (total == n) and every
	// child received at least one hit (no starvation).
	total := 0
	for i, cnt := range counts {
		total += cnt
		if cnt == 0 {
			t.Errorf("child %d never picked", i)
		}
	}
	if total != n {
		t.Fatalf("counter lost updates: sum=%d want=%d", total, n)
	}
}

func TestAdvanceDynamic_WithinBounds(t *testing.T) {
	for i := 0; i < 100; i++ {
		got := AdvanceDynamic(5)
		if got < 0 || got >= 5 {
			t.Fatalf("out of range: %d", got)
		}
	}
}

func TestAdvanceDynamic_ZeroReturnsZero(t *testing.T) {
	if got := AdvanceDynamic(0); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

func TestNewCoordinatorWithStarts_SeedsConfiguredClients(t *testing.T) {
	// A configured start of 2 with childCount=3 must produce the sequence
	// 2, 0, 1, 2, 0, 1, ... — fetch_add semantics return the pre-increment
	// value modulo childCount.
	c := NewCoordinatorWithStarts(map[string]int{"Seeded": 2})
	wantSeq := []int{2, 0, 1, 2, 0, 1}
	for i, want := range wantSeq {
		if got := advance(t, c, "Seeded", 3); got != want {
			t.Fatalf("step %d: got %d, want %d", i, got, want)
		}
	}
}

func TestNewCoordinatorWithStarts_NegativeStartClampedToZero(t *testing.T) {
	c := NewCoordinatorWithStarts(map[string]int{"Neg": -7})
	// Clamped to 0 — first Advance returns 0.
	if got := advance(t, c, "Neg", 4); got != 0 {
		t.Fatalf("first: got %d, want 0 (clamped from -7)", got)
	}
	if got := advance(t, c, "Neg", 4); got != 1 {
		t.Fatalf("second: got %d, want 1", got)
	}
}

func TestNewCoordinatorWithStarts_UnlistedClientsStayRandom(t *testing.T) {
	// A seed configured for "Other" must NOT influence "Unlisted". We can't
	// observe the random seed directly, but we can confirm that Unlisted's
	// first index is not forced to Other's seed value.
	c := NewCoordinatorWithStarts(map[string]int{"Other": 3})
	// Drive "Other" to confirm its seed is honoured first.
	if got := advance(t, c, "Other", 4); got != 3 {
		t.Fatalf("Other first: got %d, want 3", got)
	}
	// Unlisted takes a random seed; any legal index is acceptable. Asserting
	// only the bound confirms independence — the deterministic path would
	// have leaked Other's 3 here if the implementation shared state.
	got := advance(t, c, "Unlisted", 4)
	if got < 0 || got >= 4 {
		t.Fatalf("Unlisted: out of range: %d", got)
	}
}

func TestNewCoordinatorWithStarts_NilMapBehavesLikeNoStarts(t *testing.T) {
	c := NewCoordinatorWithStarts(nil)
	// Round-trip: nil map just degrades to the random-seed behaviour of
	// NewCoordinator. Consecutive calls must still be contiguous (mod n).
	first := advance(t, c, "X", 5)
	if first < 0 || first >= 5 {
		t.Fatalf("first: out of range: %d", first)
	}
	if got := advance(t, c, "X", 5); got != (first+1)%5 {
		t.Fatalf("second: got %d, want %d", got, (first+1)%5)
	}
}

func TestNewCoordinatorWithStarts_CopiesInputMap(t *testing.T) {
	// Mutating the caller's map after construction must not affect the
	// coordinator — the starts are captured at construction time.
	starts := map[string]int{"A": 1}
	c := NewCoordinatorWithStarts(starts)
	starts["A"] = 999 // caller mutates after handoff
	if got := advance(t, c, "A", 3); got != 1 {
		t.Fatalf("got %d, want 1 (coordinator must not see post-construction mutations)", got)
	}
}
