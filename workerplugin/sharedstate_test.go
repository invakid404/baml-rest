package workerplugin

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
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

	// With n concurrent calls, the returned previous values should be
	// exactly the set {0, 1, ..., n-1} — counter advanced n times, each
	// caller got a distinct pre-increment value, no gaps. A raw "distinct
	// count == n" check would pass even if callers observed e.g.
	// {0..n-2, 999}; the explicit range sweep below would catch a bug
	// where the counter skipped a slot but still produced n unique ints.
	seen := make(map[uint64]bool, n)
	for _, c := range counts {
		if seen[c] {
			t.Fatalf("duplicate previous=%d — counter dropped an increment", c)
		}
		seen[c] = true
	}
	for i := 0; i < n; i++ {
		if !seen[uint64(i)] {
			t.Fatalf("missing previous=%d — expected full permutation of 0..%d", i, n-1)
		}
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
	// TTL set long enough that the background ticker won't fire during
	// the test; we drive sweepOnce manually so the assertion is
	// deterministic rather than relying on wall-clock sleeps. An
	// arbitrarily-advanced "now" simulates the scope sitting idle past
	// its TTL.
	const ttl = time.Hour
	store := newSharedStateStoreWithTTL(nil, ttl)
	defer store.Close()

	store.FetchAdd("k", 1, "orphan")

	// Advance the sweep clock past TTL. The orphan scope's lastAccessed
	// is "now"; cutoff becomes now + 2*TTL - TTL = now + TTL, which is
	// strictly after lastAccessed, so the scope evicts.
	store.sweepOnce(time.Now().Add(2 * ttl))

	// Post-sweep, a replay with the same op-id must advance — the cached
	// entry has been reclaimed. Before the sweep ran, a replay would
	// return 0; after, it returns the current counter value, 1.
	if got := store.FetchAdd("k", 1, "orphan"); got != 1 {
		t.Fatalf("post-TTL replay: got %d, want 1 (sweep did not reclaim orphan entry)", got)
	}
}

func TestSharedStateStore_CacheHoldsUnderConcurrentSweepPressure(t *testing.T) {
	// Regression for CR-10/CR-15: concurrent FetchAdd callers racing
	// against a sweep goroutine must never observe a torn state where
	// the cache is present for one caller but gone for the next. The
	// sweep runs with a real-time cutoff so it never actually evicts
	// (lastAccessed updates win the recheck every time) — the point
	// here is to exercise the Load → touchScope → mu.Lock → deleted-
	// flag path under -race detection, not to test eviction itself.
	const ttl = time.Hour
	store := newSharedStateStoreWithTTL(nil, ttl)
	defer store.Close()

	first := store.FetchAdd("k", 1, "racy-op")
	if first != 0 {
		t.Fatalf("first: got %d, want 0", first)
	}

	stop := make(chan struct{})
	var sweeperDone sync.WaitGroup
	sweeperDone.Add(1)
	go func() {
		defer sweeperDone.Done()
		for {
			select {
			case <-stop:
				return
			default:
				// Real-time sweep clock: cutoff = real_now - TTL;
				// every fresh touch keeps the scope above cutoff.
				store.sweepOnce(time.Now())
			}
		}
	}()

	const concurrency = 8
	const iterations = 2000
	var wg sync.WaitGroup
	var fail atomic.Bool
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				got := store.FetchAdd("k", 1, "racy-op")
				if got != first {
					if fail.CompareAndSwap(false, true) {
						t.Errorf("FetchAdd: got %d, want %d (cache torn under sweep pressure)", got, first)
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	sweeperDone.Wait()
}

func TestSharedStateStore_ConcurrentSweepEvictionIsIdempotent(t *testing.T) {
	// CR-15 regression: the deleted flag and mu-guarded deletion must
	// ensure that a touchScope racing with a concurrent evicting sweep
	// either (a) refreshes the scope before sweep commits, keeping it
	// alive, or (b) sees deleted=true, restarts with a fresh Load, and
	// registers under a brand-new scope. It must never leak a key into
	// the scope sync.Map orphan (the scope that was removed from
	// s.scopes but still holds the mutex briefly).
	//
	// Asserts post-state: after many iterations of "FetchAdd, then
	// force-evict via sweepOnce with a future clock", the next
	// FetchAdd always advances (because the cache was truly evicted),
	// and the counter matches the number of distinct op_ids used.
	const ttl = time.Hour
	store := newSharedStateStoreWithTTL(nil, ttl)
	defer store.Close()

	const iterations = 200
	for i := 0; i < iterations; i++ {
		opID := "ep-" + strconv.Itoa(i)
		store.FetchAdd("k", 1, opID)
		// Force-evict by advancing the sweep clock past TTL.
		store.sweepOnce(time.Now().Add(2 * ttl))
	}

	// After N force-evictions, the counter should have advanced
	// exactly N times (one per op_id). A fresh op_id observes
	// previous=iterations.
	if got := store.FetchAdd("k", 1, "final"); got != iterations {
		t.Fatalf("final: got %d, want %d (sweep dropped or duplicated advances)", got, iterations)
	}
}

func TestSharedStateStore_ActiveScopeSurvivesPastCreationTTL(t *testing.T) {
	// Regression for the long-running-request eviction bug: before, the
	// sweep evicted by scope.createdAt, so a request that kept retrying
	// past the TTL would have its cached rotation index reclaimed mid-
	// flight, and the next retry would advance the counter a second time
	// (losing the request's RR slot). With lastAccessed tracking, every
	// touch bumps the scope and the sweep leaves it alone as long as
	// the pool is still probing.
	//
	// Driven with explicit virtual timestamps on registerAndLoadIdem +
	// sweepOnce rather than real sleeps, so the assertion is
	// deterministic. The virtual clock advances by 9*TTL over ten
	// iterations — any single idle window longer than TTL would evict
	// — but each touch refreshes lastAccessed just before the sweep,
	// so the scope stays alive.
	const ttl = time.Hour
	store := newSharedStateStoreWithTTL(nil, ttl)
	defer store.Close()

	first := store.FetchAdd("k", 1, "long-running")
	if first != 0 {
		t.Fatalf("first: got %d, want 0", first)
	}

	virtualNow := time.Now()
	for i := 0; i < 10; i++ {
		// Advance virtual time by 90% of TTL. Under old createdAt
		// semantics, after i=2 iterations the createdAt would be far
		// behind the sweep cutoff and the scope would evict.
		virtualNow = virtualNow.Add(ttl * 9 / 10)
		// Refresh the scope with an explicit timestamp. Calling
		// registerAndLoadIdem is equivalent to a FetchAdd for this
		// key/opID minus the counter advance; it's what exercises the
		// lastAccessed bump the sweeper cares about.
		store.registerAndLoadIdem("k", "long-running", virtualNow)
		// Sweep one nanosecond after the touch: cutoff =
		// virtualNow - TTL + 1ns; lastAccessed = virtualNow; fresh.
		store.sweepOnce(virtualNow.Add(time.Nanosecond))
	}

	// If any iteration evicted the scope, the idem entry would be gone
	// and the next FetchAdd would miss the cache and advance the
	// counter (returning 1, not 0).
	if got := store.FetchAdd("k", 1, "long-running"); got != 0 {
		t.Fatalf("post loop: got %d, want 0 — scope was evicted despite fresh touches", got)
	}
}

func TestSharedStateStore_FetchAddDoesNotOrphanUnderConcurrentDropScope(t *testing.T) {
	// Scope attach and idem creation must happen under the same scope
	// mutex inside registerAndLoadIdem; otherwise a DropScope firing
	// between a separate touchScope and loadOrStoreIdem could delete
	// the scope and iterate its (key-set-of-one) before the idem entry
	// existed, then the caller would resume and create the entry
	// orphaned. The sweeper only ranges s.scopes, so the orphan would
	// be unreclaimable. This test drives the exact interleaving and
	// asserts the structural invariant: every live idem entry has a
	// live scope entry, regardless of which side won the race.
	store := newSharedStateStoreWithTTL(nil, time.Hour)
	defer store.Close()

	const iterations = 2000
	for i := 0; i < iterations; i++ {
		opID := fmt.Sprintf("op-%d", i)

		// Launch FetchAdd and DropScope concurrently. The goal is
		// maximal overlap on the scope-attach / drop decision, which
		// is what the old split-call version was vulnerable to.
		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() {
			defer wg.Done()
			<-start
			store.FetchAdd("k", 1, opID)
		}()
		go func() {
			defer wg.Done()
			<-start
			store.DropScope(opID)
		}()
		close(start)
		wg.Wait()

		// Invariant check: if s.idem still holds the entry, s.scopes
		// must still hold its scope. An orphan (idem present, scope
		// missing) is the exact condition the old ordering could
		// produce.
		if _, idemOK := store.idem.Load(makeIdemKey("k", opID)); idemOK {
			if _, scopeOK := store.scopes.Load(opID); !scopeOK {
				t.Fatalf("op-%d: idem entry exists but scope was dropped — orphan leak", i)
			}
		}
	}
}

func TestSharedStateStore_FetchAddAfterDropScopeRestartsCleanly(t *testing.T) {
	// Direct interleaving test: a FetchAdd observing its scope marked
	// deleted mid-call must restart against a fresh scope so the
	// returned entry is attached to a live scope. The deleted flag
	// retry loop inside registerAndLoadIdem guarantees this.
	store := newSharedStateStoreWithTTL(nil, time.Hour)
	defer store.Close()

	// Prime: create a scope then drop it.
	store.FetchAdd("k", 1, "op")
	released := store.DropScope("op")
	if released != 1 {
		t.Fatalf("DropScope released=%d, want 1", released)
	}

	// A subsequent FetchAdd with the same op_id must:
	//   - find no surviving scope (LoadOrStore creates a fresh one),
	//   - find no surviving idem entry (creates a fresh one),
	//   - advance the counter (because it's a cache miss).
	got := store.FetchAdd("k", 1, "op")
	if got != 1 {
		t.Fatalf("got previous=%d, want 1 (post-drop FetchAdd must advance against a fresh scope+entry)", got)
	}
	// And the idem/scope must now be paired again.
	if _, idemOK := store.idem.Load(makeIdemKey("k", "op")); !idemOK {
		t.Fatal("post-drop FetchAdd did not leave an idem entry")
	}
	if _, scopeOK := store.scopes.Load("op"); !scopeOK {
		t.Fatal("post-drop FetchAdd did not leave a scope entry")
	}
}

// TestSharedStateStore_NULBytesInKeyDoNotCollide pins that the
// structured idemMapKey{Key, OpID} addresses entries without
// separator-encoding ambiguity: a NUL byte inside either half cannot
// collapse two distinct caller inputs onto the same map slot.
//
// The discriminating pair is ("foo", "bar\x00baz") vs
// ("foo\x00bar", "baz"). A naive NUL-joined string key would alias
// both onto "foo\x00bar\x00baz" — under such a scheme, the second
// FetchAdd would hit the first's cached idemEntry and short-circuit
// via sync.Once.Do, leaving the second pair's COUNTER (keyed on
// "foo\x00bar") at 0. A third FetchAdd under a fresh op_id probes
// that counter; if it's still at 0, the second pair was aliased.
func TestSharedStateStore_NULBytesInKeyDoNotCollide(t *testing.T) {
	t.Run("counter advancement proves distinct entries", func(t *testing.T) {
		store := newSharedStateStoreWithTTL(nil, time.Hour)
		defer store.Close()

		// Pair A: key="foo", opID="bar\x00baz".
		// Pair B: key="foo\x00bar", opID="baz".
		// Old-scheme joined string for both: "foo\x00bar\x00baz".
		// Pair A advances counter "foo" 0→1, returns previous=0.
		if got := store.FetchAdd("foo", 1, "bar\x00baz"); got != 0 {
			t.Fatalf("pair A first call: got previous=%d, want 0", got)
		}

		// Pair B should be a cache miss under the new scheme: distinct
		// idemMapKey, distinct counter "foo\x00bar". Returns previous=0
		// from advancing that counter 0→1. Under the old scheme it
		// would hit pair A's cache and return 0 WITHOUT advancing
		// "foo\x00bar". Both return 0, so the FetchAdd return is not
		// itself the discriminator — see the next call.
		if got := store.FetchAdd("foo\x00bar", 1, "baz"); got != 0 {
			t.Fatalf("pair B first call: got previous=%d, want 0", got)
		}

		// Discriminator: a fresh op_id under pair B's key
		// ("foo\x00bar"). If pair B genuinely advanced its counter
		// (new code), this advances it again — counter "foo\x00bar"
		// goes 1→2, previous=1. If pair B was aliased onto pair A's
		// idemEntry under the old NUL-concat scheme, the
		// "foo\x00bar" counter is still 0 because Once.Do
		// short-circuited the counter.Add — this call would advance
		// 0→1, previous=0.
		got := store.FetchAdd("foo\x00bar", 1, "fresh-op")
		if got != 1 {
			t.Errorf("discriminator FetchAdd on key %q: got previous=%d, want 1 — pair B was aliased onto pair A's entry under the NUL-concat scheme", "foo\\x00bar", got)
		}
	})

	t.Run("DropScope on one pair does not affect the other", func(t *testing.T) {
		store := newSharedStateStoreWithTTL(nil, time.Hour)
		defer store.Close()

		// Same colliding pair shape, fresh store. Each pair lives in
		// its own scope (scopes are keyed on opID alone), so dropping
		// one must not touch the other's idem entry.
		store.FetchAdd("foo", 1, "bar\x00baz")  // pair A, scope "bar\x00baz"
		store.FetchAdd("foo\x00bar", 1, "baz")  // pair B, scope "baz"

		// Drop pair A's scope. Walks sc.keys = {"foo"} and removes
		// idem[{Key:"foo", OpID:"bar\x00baz"}] only. Pair B's
		// idem[{Key:"foo\x00bar", OpID:"baz"}] must remain.
		if released := store.DropScope("bar\x00baz"); released != 1 {
			t.Fatalf("DropScope released=%d, want 1 (pair A's single key)", released)
		}

		// Pair B's idempotency must survive: re-issuing it returns
		// the SAME cached previous it returned before (0). Under a
		// regression where DropScope walked the old NUL-concat
		// alias, pair B's entry would also have been deleted —
		// re-issuing would observe a cache miss, advance the counter
		// 1→2, and return previous=1.
		if got := store.FetchAdd("foo\x00bar", 1, "baz"); got != 0 {
			t.Errorf("pair B post-DropScope-A: got previous=%d, want 0 (cache hit) — DropScope leaked across the collision boundary", got)
		}

		// And pair A is genuinely gone: re-issuing observes a fresh
		// scope + fresh idem entry, advances counter "foo" again.
		// Counter "foo" was at 1 from pair A's first call; this
		// advances to 2. previous=1.
		if got := store.FetchAdd("foo", 1, "bar\x00baz"); got != 1 {
			t.Errorf("pair A post-drop re-issue: got previous=%d, want 1 (counter %q kept its value across the drop, idem entry was fresh)", got, "foo")
		}
	})
}
