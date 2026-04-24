package workerplugin

import (
	"context"
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
	idem     sync.Map // map[string]*idemEntry — key is fmt.Sprint(key, "\x00", opID)
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
	once      sync.Once
	previous  uint64
	createdAt time.Time
}

// scope holds the set of idempotency keys owned by one operation_id.
// DropScope walks this set instead of the whole idem map — O(keys touched
// by this request) rather than O(all cached entries in the process).
type scope struct {
	mu        sync.Mutex
	keys      map[string]struct{}
	createdAt time.Time
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
	entry := s.loadOrStoreIdem(key, opID)
	entry.once.Do(func() {
		counter := s.counterFor(key)
		// Assignment is inside Do so concurrent readers blocked on the
		// same Once observe this write under the happens-before edge
		// that Once establishes.
		entry.previous = counter.Add(delta) - delta
		entry.createdAt = time.Now()
		s.attachToScope(key, opID)
	})
	return entry.previous
}

// loadOrStoreIdem returns the canonical idempotency entry for (key,
// opID), creating it on first touch. Concurrent callers for the same
// pair all receive the same *idemEntry; exactly one of them will win
// the once.Do race inside FetchAdd.
func (s *SharedStateStore) loadOrStoreIdem(key, opID string) *idemEntry {
	k := idemKey(key, opID)
	if v, ok := s.idem.Load(k); ok {
		return v.(*idemEntry)
	}
	fresh := &idemEntry{}
	actual, _ := s.idem.LoadOrStore(k, fresh)
	return actual.(*idemEntry)
}

// attachToScope registers (key, opID) under opID's scope so DropScope
// can find every entry belonging to a request without walking the whole
// idem map. Called only on the once.Do winner path; losers already
// observe a registered scope through their blocking Do.
func (s *SharedStateStore) attachToScope(key, opID string) {
	var sc *scope
	if v, ok := s.scopes.Load(opID); ok {
		sc = v.(*scope)
	} else {
		fresh := &scope{keys: make(map[string]struct{}), createdAt: time.Now()}
		actual, _ := s.scopes.LoadOrStore(opID, fresh)
		sc = actual.(*scope)
	}
	sc.mu.Lock()
	if sc.keys == nil {
		sc.keys = make(map[string]struct{})
	}
	sc.keys[key] = struct{}{}
	sc.mu.Unlock()
}

// DropScope releases every idempotency entry recorded for opID. Returns
// the number of entries released (useful for tests and metrics). Calling
// DropScope with an unknown opID is a no-op.
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
	released := len(sc.keys)
	for k := range sc.keys {
		s.idem.Delete(idemKey(k, opID))
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

// idemKey composes the two-level key with a byte that cannot appear in
// either half (BAML client names are identifiers; request_ids are UUIDs
// or fiber request-ids — both ASCII-only).
func idemKey(key, opID string) string {
	return key + "\x00" + opID
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

func (s *SharedStateStore) sweepOnce(now time.Time) {
	cutoff := now.Add(-s.ttl)
	s.scopes.Range(func(k, v any) bool {
		sc := v.(*scope)
		sc.mu.Lock()
		expired := sc.createdAt.Before(cutoff)
		var keys []string
		if expired {
			keys = make([]string, 0, len(sc.keys))
			for kk := range sc.keys {
				keys = append(keys, kk)
			}
		}
		sc.mu.Unlock()
		if !expired {
			return true
		}
		opID := k.(string)
		s.scopes.Delete(opID)
		for _, kk := range keys {
			s.idem.Delete(idemKey(kk, opID))
		}
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
