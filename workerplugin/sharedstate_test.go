package workerplugin

import (
	"sync"
	"testing"
	"time"
)

func TestSharedStateStore_FetchAddAdvances(t *testing.T) {
	store := newSharedStateStoreWithTTL(nil, 0)
	defer store.Close()

	if got := store.FetchAdd("k", 1, ""); got != 0 {
		t.Fatalf("first fetch-add: got %d, want 0", got)
	}
	if got := store.FetchAdd("k", 1, ""); got != 1 {
		t.Fatalf("second fetch-add: got %d, want 1", got)
	}
	if got := store.FetchAdd("k", 3, ""); got != 2 {
		t.Fatalf("third fetch-add: got %d, want 2", got)
	}
	if got := store.FetchAdd("k", 1, ""); got != 5 {
		t.Fatalf("post-delta fetch-add: got %d, want 5", got)
	}
}

func TestSharedStateStore_InitialValueSeed(t *testing.T) {
	store := newSharedStateStoreWithTTL(func(key string) uint64 {
		if key == "seeded" {
			return 42
		}
		return 0
	}, 0)
	defer store.Close()

	if got := store.FetchAdd("seeded", 1, ""); got != 42 {
		t.Fatalf("seeded first: got %d, want 42", got)
	}
	if got := store.FetchAdd("seeded", 1, ""); got != 43 {
		t.Fatalf("seeded second: got %d, want 43", got)
	}
	if got := store.FetchAdd("unseeded", 1, ""); got != 0 {
		t.Fatalf("unseeded first: got %d, want 0", got)
	}
}

func TestSharedStateStore_IdempotencyHoldsAcrossRetries(t *testing.T) {
	store := newSharedStateStoreWithTTL(nil, 0)
	defer store.Close()

	// First call with op-id populates the cache.
	first := store.FetchAdd("k", 1, "req-1")
	if first != 0 {
		t.Fatalf("first: got %d, want 0", first)
	}
	// Replayed call with the same op-id must NOT advance the counter.
	replay := store.FetchAdd("k", 1, "req-1")
	if replay != 0 {
		t.Fatalf("replay: got %d, want 0 (cache miss — counter advanced)", replay)
	}
	// A different op-id must see the *next* counter value — the earlier
	// call advanced exactly once.
	second := store.FetchAdd("k", 1, "req-2")
	if second != 1 {
		t.Fatalf("second op: got %d, want 1", second)
	}
}

func TestSharedStateStore_DropScopeFreesIdemEntries(t *testing.T) {
	store := newSharedStateStoreWithTTL(nil, 0)
	defer store.Close()

	// Populate two entries under one request id.
	store.FetchAdd("a", 1, "req")
	store.FetchAdd("b", 1, "req")

	released := store.DropScope("req")
	if released != 2 {
		t.Fatalf("DropScope released=%d, want 2", released)
	}
	// After drop, the same op-id behaves like a fresh request — the
	// counter has already advanced, so replay sees the post-increment
	// value. This test asserts the cache was actually cleared.
	if got := store.FetchAdd("a", 1, "req"); got != 1 {
		t.Fatalf("post-drop 'a': got %d, want 1", got)
	}
}

func TestSharedStateStore_ConcurrentFetchAddAdvancesExactlyOncePerCall(t *testing.T) {
	store := newSharedStateStoreWithTTL(nil, 0)
	defer store.Close()

	const n = 1000
	counts := make([]uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			counts[i] = store.FetchAdd("shared", 1, "")
		}(i)
	}
	wg.Wait()

	// With n concurrent calls, the returned previous values should be a
	// permutation of 0..n-1.
	seen := make(map[uint64]bool, n)
	for _, c := range counts {
		if seen[c] {
			t.Fatalf("duplicate previous=%d — counter dropped an increment", c)
		}
		seen[c] = true
	}
	if len(seen) != n {
		t.Fatalf("distinct previous values: got %d, want %d", len(seen), n)
	}
}

func TestSharedStateStore_ConcurrentReplaysAdvanceCounterOnce(t *testing.T) {
	// Simulates the pool-retry race: N goroutines call FetchAdd with the
	// same (key, op-id) concurrently. The sync.Once guard in idemEntry
	// must collapse all of them onto a single counter advance, and every
	// caller must observe the same previous value. Without the guard the
	// old read-then-add-then-store sequence would let multiple advances
	// through, rotating the counter past what the request expected.
	store := newSharedStateStoreWithTTL(nil, 0)
	defer store.Close()

	const concurrency = 100
	results := make([]uint64, concurrency)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines simultaneously to maximise overlap
			results[i] = store.FetchAdd("k", 1, "req-race")
		}(i)
	}
	close(start)
	wg.Wait()

	// Every caller must see the same previous value (0 — first advance
	// on an unseeded key). A different result for any goroutine means
	// multiple advances leaked through.
	for i, got := range results {
		if got != 0 {
			t.Fatalf("goroutine %d got previous=%d, want 0 (concurrent replay advanced counter twice)", i, got)
		}
	}

	// A fresh op-id must see exactly previous=1 — the counter advanced
	// once (by the winner of the race), not N times.
	if got := store.FetchAdd("k", 1, "next"); got != 1 {
		t.Fatalf("post-race: got %d, want 1 (counter advanced %d times under concurrency)", got, got)
	}
}

func TestSharedStateStore_TTLSweepReclaimsIdemEntries(t *testing.T) {
	store := newSharedStateStoreWithTTL(nil, 50*time.Millisecond)
	defer store.Close()

	store.FetchAdd("k", 1, "orphan")

	// Wait past the TTL for the background sweep to run. The ticker fires
	// at the TTL interval, so 3x TTL is enough to cover one sweep under
	// GC pressure without making the test flaky.
	time.Sleep(200 * time.Millisecond)

	// Post-TTL, a replay with the same op-id must advance — the cached
	// entry has been reclaimed. (Before the sweep ran, a replay would
	// return 0; after, it returns the current counter value, 1.)
	if got := store.FetchAdd("k", 1, "orphan"); got != 1 {
		t.Fatalf("post-TTL replay: got %d, want 1 (sweep did not reclaim orphan entry)", got)
	}
}
