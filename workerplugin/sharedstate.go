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
// idempotency entries keyed on that request_id are released. A 60s TTL
// sweeper runs in the background as a safety net for callers that fail
// to drop explicitly (e.g. the pool process crashing mid-request).
type SharedStateStore struct {
	initialValue func(key string) uint64

	counters sync.Map // map[string]*atomic.Uint64
	idem     sync.Map // map[string]*idemEntry — key is fmt.Sprint(key, "\x00", opID)
	scopes   sync.Map // map[string]*scope — key is operation_id (aka request_id)

	ttl time.Duration

	stopCh chan struct{}
	stopped atomic.Bool
}

// idemEntry caches a single FetchAdd response for a (key, op_id) pair.
// createdAt drives the TTL sweeper so orphaned entries (the owning
// request never called DropScope) are reclaimed without per-call
// bookkeeping.
type idemEntry struct {
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

// NewSharedStateStore returns a store whose counters are initialised by
// the caller-supplied callback on first touch. Pass nil to default new
// counters to 0; production callers should wire this to the introspected
// RoundRobinStart map so configured BAML `start N` values are honoured
// and unseeded keys get a random offset (preserving the fresh-fleet
// behaviour of the legacy in-process Coordinator).
func NewSharedStateStore(initialValue func(key string) uint64) *SharedStateStore {
	return newSharedStateStoreWithTTL(initialValue, 60*time.Second)
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
// without advancing the counter.
func (s *SharedStateStore) FetchAdd(key string, delta uint64, opID string) uint64 {
	if opID != "" {
		if prev, ok := s.loadIdem(key, opID); ok {
			return prev
		}
	}
	counter := s.counterFor(key)
	prev := counter.Add(delta) - delta
	if opID != "" {
		s.storeIdem(key, opID, prev)
	}
	return prev
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

func (s *SharedStateStore) loadIdem(key, opID string) (uint64, bool) {
	v, ok := s.idem.Load(idemKey(key, opID))
	if !ok {
		return 0, false
	}
	return v.(*idemEntry).previous, true
}

func (s *SharedStateStore) storeIdem(key, opID string, previous uint64) {
	entry := &idemEntry{previous: previous, createdAt: time.Now()}
	s.idem.Store(idemKey(key, opID), entry)

	// Track the key under its scope so DropScope can find it without a full
	// idem-map walk. LoadOrStore handles the first-touch race.
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
	t := time.NewTicker(s.ttl)
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
