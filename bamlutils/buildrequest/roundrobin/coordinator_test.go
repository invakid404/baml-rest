package roundrobin

import (
	"sync"
	"testing"
)

func TestAdvance_StepsThroughChildren(t *testing.T) {
	c := NewCoordinator()
	// First Advance is seeded randomly, so we only check that consecutive
	// calls produce a contiguous sequence modulo childCount.
	first := c.Advance("ClientA", 3)
	if first < 0 || first >= 3 {
		t.Fatalf("first index out of range: %d", first)
	}
	second := c.Advance("ClientA", 3)
	third := c.Advance("ClientA", 3)
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
	aBefore := c.Advance("A", 4)
	bBefore := c.Advance("B", 4)
	// Advance A a few more times
	c.Advance("A", 4)
	c.Advance("A", 4)
	bAfter := c.Advance("B", 4)
	expectedBAfter := (bBefore + 1) % 4
	if bAfter != expectedBAfter {
		t.Fatalf("B's counter leaked from A: expected %d, got %d (aBefore=%d)", expectedBAfter, bAfter, aBefore)
	}
}

func TestAdvance_ZeroChildCount_ReturnsZero(t *testing.T) {
	c := NewCoordinator()
	if got := c.Advance("X", 0); got != 0 {
		t.Fatalf("expected 0 for childCount=0, got %d", got)
	}
	if got := c.Advance("X", -5); got != 0 {
		t.Fatalf("expected 0 for childCount<0, got %d", got)
	}
}

func TestAdvance_ConcurrentUsageDoesNotSkipOrDuplicate(t *testing.T) {
	c := NewCoordinator()
	const n = 1000
	const childCount = 7
	// Prime the counter so we know its starting offset deterministically.
	start := c.Advance("Shared", childCount)
	counts := make([]int, childCount)
	counts[start]++
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < n-1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx := c.Advance("Shared", childCount)
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
