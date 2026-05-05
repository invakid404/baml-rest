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
	// Bucket-bounds invariant: with the deterministic primed first
	// call plus n-1 concurrent advances over childCount children,
	// every bucket must hold either floor(n/k) or ceil(n/k) hits —
	// atomic increments preserve total ordering on the counter
	// regardless of goroutine interleaving, so the modular
	// distribution is exact, not "roughly". For n=1000 and k=7
	// that's 142 or 143.
	//
	// A looser assertion (total==n + cnt>0) accepts any distribution
	// that touches every bucket and preserves total —
	// including badly-skewed ones a broken modulo / ordering bug
	// could produce. The strict bounds catch that; total stays as a
	// safety net for "counter lost updates" regressions.
	total := 0
	floor := n / childCount
	ceil := floor
	if n%childCount != 0 {
		ceil = floor + 1
	}
	for i, cnt := range counts {
		total += cnt
		if cnt == 0 {
			t.Errorf("child %d never picked", i)
		}
		if cnt != floor && cnt != ceil {
			t.Errorf("child %d: count %d not in {%d, %d} (per-bucket invariant for n=%d, childCount=%d)", i, cnt, floor, ceil, n, childCount)
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

// TestAdvanceDynamic_NonPositiveReturnsZero pins the documented
// non-positive contract (coordinator.go:147-152): childCount of 0,
// negative, or any value <= 0 must yield 0 without panicking. The
// table covers both zero and negative inputs so an off-by-one
// regression that only handles one of the two would surface.
func TestAdvanceDynamic_NonPositiveReturnsZero(t *testing.T) {
	for _, k := range []int{0, -1, -100} {
		if got := AdvanceDynamic(k); got != 0 {
			t.Errorf("AdvanceDynamic(%d) = %d, want 0", k, got)
		}
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

func TestNewCoordinatorWithStarts_NegativeStartCastSemantics(t *testing.T) {
	// Negative `start` values are reinterpreted as unsigned to mirror
	// upstream BAML's `(start as usize) % strategy.len()` cast. With
	// start=-7 and childCount=4: uint64(int(-7)) = 0xFFFFFFFFFFFFFFF9
	// = 2^64 - 7. Modulo 4 wraps to 1 (2^64 ≡ 0 mod 4, -7 ≡ 1 mod 4).
	// Subsequent Advance calls walk 1 → 2 → 3 → 0 → 1 …, exactly
	// matching what BAML's per-worker runtime would produce.
	c := NewCoordinatorWithStarts(map[string]int{"Neg": -7})
	want := []int{1, 2, 3, 0, 1}
	for i, w := range want {
		if got := advance(t, c, "Neg", 4); got != w {
			t.Fatalf("call %d: got %d, want %d (cast-and-modulo from start=-7)", i+1, got, w)
		}
	}
}

func TestNewCoordinatorWithStarts_UnlistedClientsStayRandom(t *testing.T) {
	// A seed configured for "Other" must NOT influence "Unlisted".
	// With the injected random source we can assert the precise
	// value an unlisted client observes — a range-only check would
	// let a broken implementation that reused starts["Other"] for
	// every client still satisfy childCount=4. Inject a known seed
	// so the next-index calculation pins the unlisted client's
	// value independently of "Other"'s configured seed.
	c := NewCoordinatorWithStarts(map[string]int{"Other": 3})
	// Pin the random seed to a value that is NOT 3 modulo 4 — choose
	// 17 so 17 % 4 = 1, distinct from "Other"'s configured 3.
	c.SetRandSeedForTest(func() uint32 { return 17 })
	// Drive "Other" to confirm its seed is honoured first.
	if got := advance(t, c, "Other", 4); got != 3 {
		t.Fatalf("Other first: got %d, want 3", got)
	}
	// Unlisted should observe the injected random seed (17), giving
	// 17 % 4 == 1 on the first Advance.
	if got := advance(t, c, "Unlisted", 4); got != 1 {
		t.Fatalf("Unlisted first: got %d, want 1 (17 %% 4 — injected random seed must override the listed-client default)", got)
	}
	// And the unlisted client must keep advancing from the seed rather
	// than the listed-client's value — second call yields (17+1)%4 = 2.
	if got := advance(t, c, "Unlisted", 4); got != 2 {
		t.Fatalf("Unlisted second: got %d, want 2", got)
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
