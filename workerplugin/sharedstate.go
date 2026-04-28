package workerplugin

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// SharedStateStore is the host-side implementation of the SharedState gRPC
// service: a flat keyspace of unsigned-64 counters plus a small
// (key, operation_id) -> previous-value idempotency cache.
//
// The store is intentionally unaware of what its counters mean. Workers
// pick keys, workers apply any modulus, workers retry. The store only
// guarantees that concurrent FetchAdd calls advance a counter atomically
// and that the same (key, op_id) pair returns the same value until its
// scope is dropped.
//
// Idempotency is keyed on the caller-supplied operation_id. For round-
// robin, the caller passes the request_id of the in-flight request so the
// pool-level retry loop resolves the same rotation index each attempt —
// without this, a pool retry would advance the counter a second time and
// the rotation would skip a child. Empty operation_id disables caching
// entirely; every call advances.
//
// DropScope(requestID) must be invoked when a request finishes so the
// idempotency entries keyed on that request_id are released. A long TTL
// sweep runs in the background purely as a safety net for operation_ids
// that never receive a DropScope call (host crashes, goroutine panics
// before the defer registers, etc.). The TTL is intentionally longer
// than any expected request lifetime — it must never sweep a live
// request's entry out from under the pool retry loop, since that would
// re-advance the counter on the retry.
type SharedStateStore struct {
	initialValue func(key string) uint64

	counters sync.Map // map[string]*atomic.Uint64
	idem     sync.Map // map[idemMapKey]*idemEntry — see makeIdemKey
	scopes   sync.Map // map[string]*scope — key is operation_id (aka request_id)

	ttl time.Duration

	stopCh  chan struct{}
	stopped atomic.Bool
}

// idemEntry caches a single FetchAdd response for a (key, op_id) pair.
//
// The sync.Once is load-bearing: it makes FetchAdd atomic under concurrent
// retries for the same (key, op_id). Two overlapping pool attempts both
// LoadOrStore the same entry; the winner of once.Do advances the counter
// and records previous, the loser blocks inside once.Do until the winner
// finishes and then reads the stored previous value. Without this guard
// both retries could observe a cache miss, both would Add on the counter,
// and the later storeIdem would overwrite the earlier — the rotation
// would skip a child.
//
// previous must be read only after once.Do has returned. Before that the
// field is zero; after, sync.Once provides the happens-before edge for
// the read on the slow path.
type idemEntry struct {
	once     sync.Once
	previous uint64
}

// scope holds the set of idempotency keys owned by one operation_id.
// DropScope walks this set instead of the whole idem map — O(keys touched
// by this request) rather than O(all cached entries in the process).
//
// lastAccessedNano is updated on every FetchAdd under the scope, including
// cache hits. The TTL sweeper evicts by lastAccessed rather than by a
// fixed creation time so a long-running request (legitimate retries
// spanning more than TTL) keeps its cached rotation index alive as long
// as the pool is still probing it. A createdAt-only policy would evict
// live entries mid-retry, causing the next replay to advance the counter
// a second time — exactly the race the cache exists to prevent.
//
// deleted is set (under mu) the moment the sweeper or DropScope commits
// to removing this scope. It plugs a race where a registerAndLoadIdem
// caller had already loaded the scope pointer and stored a fresh
// lastAccessed but was then blocked on the mutex while sweep proceeded
// past its atomic recheck. Post-mutex, registerAndLoadIdem sees
// deleted=true and restarts with a fresh LoadOrStore; this forces a
// fresh scope from s.scopes rather than mutating the orphan the sweeper
// is about to discard. Without the flag, a window existed where the
// caller could add keys + idem entries to a scope that had already been
// removed from s.scopes, leaving the idem entry dangling and causing
// the next FetchAdd for the same op_id to re-advance the counter.
// The full lock protocol — attach scope, recheck deleted, register key,
// create or load idem under the same sc.mu — is documented at
// registerAndLoadIdem (~line 170).
//
// atomic.Int64/Bool avoid per-call mutex cost for the hot-path fields.
// UnixNano is monotonic-enough for the "age > cutoff" comparisons here;
// the sweep cadence is minutes, not nanoseconds.
type scope struct {
	mu               sync.Mutex
	keys             map[string]struct{}
	lastAccessedNano atomic.Int64
	deleted          atomic.Bool
}

// defaultIdemTTL is the default time-to-live for idempotency entries
// whose DropScope never fires (crash / panic safety net). It must be
// larger than any in-flight request can plausibly take — otherwise the
// sweep would evict a live request's cached rotation index before the
// pool finished retrying, causing a second counter advance.
//
// Pool FirstByteTimeout is 120s by default, requests can hold for longer
// with retries, and nothing about this value is latency-sensitive — the
// sweep is pure memory hygiene for orphans. 15 minutes is well clear of
// any realistic request lifetime without letting orphans accumulate
// indefinitely.
const defaultIdemTTL = 15 * time.Minute

// NewSharedStateStore returns a store whose counters are initialised by
// the caller-supplied callback on first touch. Pass nil to default new
// counters to 0; production callers should wire this to the introspected
// RoundRobinStart map so configured BAML `start N` values are honoured
// and unseeded keys get a random offset (preserving the fresh-fleet
// behaviour of the legacy in-process Coordinator).
func NewSharedStateStore(initialValue func(key string) uint64) *SharedStateStore {
	return newSharedStateStoreWithTTL(initialValue, defaultIdemTTL)
}

func newSharedStateStoreWithTTL(initialValue func(key string) uint64, ttl time.Duration) *SharedStateStore {
	if initialValue == nil {
		initialValue = func(string) uint64 { return 0 }
	}
	s := &SharedStateStore{
		initialValue: initialValue,
		ttl:          ttl,
		stopCh:       make(chan struct{}),
	}
	go s.sweeper()
	return s
}

// Close stops the background sweeper. Safe to call multiple times.
func (s *SharedStateStore) Close() {
	if s.stopped.CompareAndSwap(false, true) {
		close(s.stopCh)
	}
}

// FetchAdd atomically adds delta to the counter named by key, returning
// the pre-increment value. When opID is non-empty the (key, opID) result
// is cached: subsequent calls with the same pair return the cached value
// without advancing the counter. Concurrent calls for the same (key,
// opID) collapse onto a single counter advance — see idemEntry for the
// once.Do mechanics.
func (s *SharedStateStore) FetchAdd(key string, delta uint64, opID string) uint64 {
	if opID == "" {
		// No idempotency requested — single atomic advance, no cache.
		counter := s.counterFor(key)
		return counter.Add(delta) - delta
	}
	entry := s.registerAndLoadIdem(key, opID, time.Now())
	entry.once.Do(func() {
		counter := s.counterFor(key)
		// Assignment is inside Do so concurrent readers blocked on the
		// same Once observe this write under the happens-before edge
		// that Once establishes.
		entry.previous = counter.Add(delta) - delta
	})
	return entry.previous
}

// registerAndLoadIdem atomically (with respect to DropScope and sweepOnce)
// registers the (key, opID) pair under its scope and returns the canonical
// idempotency entry. Scope attachment and idem map creation happen under
// the same scope mutex, which closes the orphan race the previous
// split-call version had.
//
// Earlier revisions ran `touchScope` and `loadOrStoreIdem` as separate
// calls. A concurrent DropScope firing between them could delete the
// scope + iterate its keys (finding no idem entry because the caller
// hadn't created one yet), then the caller would resume and create the
// idem entry after DropScope had already committed. That entry was
// orphaned: the sweeper only ranges s.scopes, so it could never reclaim
// it. Comments in the prior revision claimed the race was closed by the
// `deleted` flag, but the flag only covered the in-touchScope window,
// not the post-touch-pre-create window.
//
// The fix is structural: do both steps while holding sc.mu. A DropScope
// racing with this helper either (a) wins — we see deleted=true after
// acquiring mu and restart against a fresh scope, creating the entry
// against the new scope; or (b) loses — we create the entry and register
// the key, and DropScope's subsequent sc.mu.Lock sees our entry in its
// iteration and deletes it cleanly. No orphan in either ordering.
//
// Long-running-request survival: lastAccessedNano is bumped on every
// call, so the sweeper never evicts a scope that's actively being
// probed by pool retries.
func (s *SharedStateStore) registerAndLoadIdem(key, opID string, now time.Time) *idemEntry {
	for {
		var sc *scope
		if v, ok := s.scopes.Load(opID); ok {
			sc = v.(*scope)
		} else {
			fresh := &scope{keys: make(map[string]struct{})}
			actual, _ := s.scopes.LoadOrStore(opID, fresh)
			sc = actual.(*scope)
		}
		// Store before Lock so a sweeper that has not yet acquired the
		// mutex sees the fresh timestamp in its DCL recheck and bails
		// out. A sweeper that has already passed its recheck will
		// proceed to delete under the mutex; we then see deleted=true
		// once we acquire it and restart the loop.
		sc.lastAccessedNano.Store(now.UnixNano())
		sc.mu.Lock()
		if sc.deleted.Load() {
			sc.mu.Unlock()
			// Brief scheduler hint; avoids a tight spin if the sweeper
			// or DropScope is slow to publish the deletion. A Gosched
			// is sufficient — both sites hold a bounded critical section.
			runtime.Gosched()
			continue
		}
		if sc.keys == nil {
			sc.keys = make(map[string]struct{})
		}
		sc.keys[key] = struct{}{}
		// Create or load the idem entry UNDER the same scope mutex so
		// a racing DropScope's key iteration either finds our entry
		// and deletes it, or commits its deletion before we ever saw
		// the scope as live (in which case we restarted above).
		k := makeIdemKey(key, opID)
		var entry *idemEntry
		if v, ok := s.idem.Load(k); ok {
			entry = v.(*idemEntry)
		} else {
			fresh := &idemEntry{}
			actual, _ := s.idem.LoadOrStore(k, fresh)
			entry = actual.(*idemEntry)
		}
		sc.mu.Unlock()
		return entry
	}
}

// DropScope releases every idempotency entry recorded for opID. Returns
// the number of entries released (useful for tests and metrics). Calling
// DropScope with an unknown opID is a no-op.
//
// Lock-ordering contract with registerAndLoadIdem:
//
//   - DropScope LoadAndDeletes the scope from s.scopes, then takes the
//     scope mutex, marks deleted=true, and walks sc.keys to delete the
//     matching idem entries under the same lock.
//   - registerAndLoadIdem does scope attach + idem map creation under
//     that same mutex. If it holds mu before DropScope, DropScope's
//     key iteration sees the idem entry we just created and cleans it
//     up. If DropScope holds mu first, registerAndLoadIdem observes
//     deleted=true and restarts with a fresh scope.
//
// Either ordering leaves the invariant `s.idem[k,op] exists implies
// s.scopes[op] exists` intact. A previous revision split scope attach
// and idem creation across two calls; DropScope could land between
// them and leave an orphan idem entry that the sweeper (which only
// ranges s.scopes) could never reclaim. The fuse into
// registerAndLoadIdem closes that window.
func (s *SharedStateStore) DropScope(opID string) int {
	if opID == "" {
		return 0
	}
	v, ok := s.scopes.LoadAndDelete(opID)
	if !ok {
		return 0
	}
	sc := v.(*scope)
	sc.mu.Lock()
	sc.deleted.Store(true)
	released := len(sc.keys)
	for k := range sc.keys {
		s.idem.Delete(makeIdemKey(k, opID))
	}
	sc.keys = nil
	sc.mu.Unlock()
	return released
}

func (s *SharedStateStore) counterFor(key string) *atomic.Uint64 {
	if v, ok := s.counters.Load(key); ok {
		return v.(*atomic.Uint64)
	}
	fresh := &atomic.Uint64{}
	fresh.Store(s.initialValue(key))
	actual, _ := s.counters.LoadOrStore(key, fresh)
	return actual.(*atomic.Uint64)
}

// idemMapKey is the structured (key, opID) tuple used to address the
// per-(client, request) idempotency entry on s.idem. CodeRabbit
// verdict-38 finding F6: the previous shape concatenated the two halves
// with a NUL byte (`key + "\x00" + opID`), which collapses two distinct
// inputs onto the same string when either half contains a NUL.
// FetchAdd / DropScope accept arbitrary caller-supplied strings, so a
// caller passing `("a\x00b", "c")` and another passing `("a", "b\x00c")`
// would have collided and shared an idempotency entry — silently
// returning a stale rotation index for one of them. A struct with two
// string fields is comparable (sync.Map keys must be comparable) and
// has no separator-encoding to confuse.
type idemMapKey struct {
	Key  string
	OpID string
}

func makeIdemKey(key, opID string) idemMapKey {
	return idemMapKey{Key: key, OpID: opID}
}

// sweeper reclaims idempotency entries that exceeded the TTL. The pool
// calls DropScope in a defer for every request, so under normal load
// this sweep finds nothing; it exists purely to bound memory if a request
// crashes after populating the cache but before calling DropScope.
func (s *SharedStateStore) sweeper() {
	if s.ttl <= 0 {
		return
	}
	// Sweep at half the TTL so a crashed entry is reclaimed within [TTL,
	// 1.5*TTL). Ticker cadence smaller than TTL means we don't scan the
	// map more often than necessary while still bounding orphan latency.
	interval := s.ttl / 2
	if interval <= 0 {
		interval = s.ttl
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-t.C:
			s.sweepOnce(now)
		}
	}
}

// sweepOnce performs one eviction pass for scopes idle past the TTL.
// Exported to the package for tests that want a deterministic sweep
// rather than relying on the background ticker.
//
// The deletions — s.scopes.Delete, marking deleted=true, and clearing
// the idem entries — all happen under the scope mutex. Holding the
// mutex across the full commit is what closes the race where a
// concurrent registerAndLoadIdem, having already captured the scope
// pointer, would otherwise store a fresh lastAccessed and add a new
// key + idem entry to a scope that's about to be removed from s.scopes.
// With the mutex held, any such call blocks, acquires the mutex after
// sweep unlocks, sees deleted=true on its post-Lock check (~line 247
// in registerAndLoadIdem), and retries with a fresh LoadOrStore.
// The current end-to-end lock protocol — store lastAccessedNano, lock
// sc.mu, check deleted, register key, create/load idem under that
// same lock — is documented at registerAndLoadIdem (~line 170).
func (s *SharedStateStore) sweepOnce(now time.Time) {
	cutoffNano := now.Add(-s.ttl).UnixNano()
	s.scopes.Range(func(k, v any) bool {
		sc := v.(*scope)
		// Cheap pre-check: atomic read avoids the mutex for the common
		// "not stale" case. Scopes that look fresh here are skipped
		// without any lock contention.
		if sc.lastAccessedNano.Load() >= cutoffNano {
			return true
		}
		sc.mu.Lock()
		// Recheck under the lock. A registerAndLoadIdem caller whose
		// atomic Store on lastAccessedNano landed between the pre-check
		// above and here will make this check pass, and we bail without
		// evicting.
		if sc.lastAccessedNano.Load() >= cutoffNano {
			sc.mu.Unlock()
			return true
		}
		opID := k.(string)
		// Mark and remove atomically with respect to registerAndLoadIdem.
		// A concurrent caller either ran entirely before this mark (and
		// kept the scope alive via the recheck above) or runs entirely
		// after and observes deleted=true on its post-Lock check,
		// restarting with a fresh scope.
		sc.deleted.Store(true)
		s.scopes.Delete(opID)
		for kk := range sc.keys {
			s.idem.Delete(makeIdemKey(kk, opID))
		}
		sc.keys = nil
		sc.mu.Unlock()
		return true
	})
}

// sharedStateServer adapts SharedStateStore to the generated gRPC server
// interface. Separated from the store so the store can be unit-tested
// without a gRPC fixture.
type sharedStateServer struct {
	pb.UnimplementedSharedStateServer
	store *SharedStateStore
}

// NewSharedStateServer returns a pb.SharedStateServer backed by store.
// Exported so plugin.go can register it on the broker socket.
func NewSharedStateServer(store *SharedStateStore) pb.SharedStateServer {
	return &sharedStateServer{store: store}
}

func (s *sharedStateServer) FetchAdd(_ context.Context, req *pb.FetchAddRequest) (*pb.FetchAddResponse, error) {
	prev := s.store.FetchAdd(req.GetKey(), req.GetDelta(), req.GetOperationId())
	return &pb.FetchAddResponse{Previous: prev}, nil
}
