package worker

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// fakeSharedStateHook is a test double that records calls to
// NewRoundRobinAdvancer and returns a fixed sentinel advancer so the
// assertions can verify the handler's wiring without depending on
// the gRPC RemoteAdvancer or an in-memory store.
type fakeSharedStateHook struct {
	calls       atomic.Int64
	lastReqID   atomic.Value // string
	returnValue bamlutils.RoundRobinAdvancer
}

func (f *fakeSharedStateHook) NewRoundRobinAdvancer(_ context.Context, requestID string) bamlutils.RoundRobinAdvancer {
	f.calls.Add(1)
	f.lastReqID.Store(requestID)
	return f.returnValue
}

// stubAdvancer is a sentinel RoundRobinAdvancer the fake hook can
// return so equality assertions on the handler's output work even
// though bamlutils.RoundRobinAdvancer is an interface.
type stubAdvancer struct{}

func (stubAdvancer) Advance(string, int) (int, error) { return 0, nil }

// TestHandlerSharedStateHook_UsesRequestID asserts that
// roundRobinAdvancerFor delegates to the installed hook when the
// inbound context carries a non-empty request id. This is the load-
// bearing seam: subprocess builds wire grpcSharedStateHook through
// here, and PR 2's in-process build will swap in a direct-call store
// adapter — both depend on the request id being threaded through.
func TestHandlerSharedStateHook_UsesRequestID(t *testing.T) {
	t.Parallel()

	expected := stubAdvancer{}
	hook := &fakeSharedStateHook{returnValue: expected}
	h, err := New(Config{SharedState: hook})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const reqID = "test-request-id"
	ctx := workerplugin.WithRequestID(context.Background(), reqID)

	got := h.roundRobinAdvancerFor(ctx)
	if got != expected {
		t.Fatalf("advancer: got %#v, want %#v", got, expected)
	}
	if calls := hook.calls.Load(); calls != 1 {
		t.Fatalf("hook calls: got %d, want 1", calls)
	}
	if id, _ := hook.lastReqID.Load().(string); id != reqID {
		t.Fatalf("hook saw request id %q, want %q", id, reqID)
	}
}

// TestHandlerSharedStateHook_MissingRequestIDFallsBack asserts that
// roundRobinAdvancerFor returns nil (and does NOT call the hook) when
// the inbound context has no request id installed. SharedStateStore's
// idempotency cache bypasses empty operation ids — falling back to the
// in-process Coordinator preserves "same logical request observes the
// same rotation".
func TestHandlerSharedStateHook_MissingRequestIDFallsBack(t *testing.T) {
	t.Parallel()

	hook := &fakeSharedStateHook{returnValue: stubAdvancer{}}
	h, err := New(Config{SharedState: hook})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := h.roundRobinAdvancerFor(context.Background())
	if got != nil {
		t.Fatalf("advancer: got %#v, want nil (no request id should bypass the hook)", got)
	}
	if calls := hook.calls.Load(); calls != 0 {
		t.Fatalf("hook calls: got %d, want 0 (missing request id must not invoke the hook)", calls)
	}
}

// TestHandlerSharedStateHook_NilHookFallsBack covers the "no hook
// attached" branch — the handler must return nil without panicking,
// which is the same fall-through behavior the subprocess binary
// observed before AttachSharedState completed (and what standalone
// test harnesses see throughout their run).
func TestHandlerSharedStateHook_NilHookFallsBack(t *testing.T) {
	t.Parallel()

	h, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := workerplugin.WithRequestID(context.Background(), "req-id")
	got := h.roundRobinAdvancerFor(ctx)
	if got != nil {
		t.Fatalf("advancer: got %#v, want nil (no hook installed)", got)
	}
}

// fakeFetchAddStore records FetchAdd invocations and returns
// caller-controlled previous values. Used to verify
// storeRoundRobinAdvancer forwards keys, deltas, and operation ids
// without depending on the real SharedStateStore.
type fakeFetchAddStore struct {
	mu         atomic.Int64 // monotonic call counter doubles as "next return"
	lastKey    atomic.Value // string
	lastDelta  atomic.Uint64
	lastOpID   atomic.Value // string
	nextReturn uint64       // value returned by FetchAdd
}

func (s *fakeFetchAddStore) FetchAdd(key string, delta uint64, operationID string) uint64 {
	s.mu.Add(1)
	s.lastKey.Store(key)
	s.lastDelta.Store(delta)
	s.lastOpID.Store(operationID)
	return s.nextReturn
}

// TestStoreSharedStateHook_AdvanceUsesFetchAddIdempotency asserts that
// the direct-call adapter forwards the operation id to FetchAdd
// (idempotency cache key on the store side) and applies the modulus
// locally, matching workerplugin.RemoteAdvancer's contract. PR 2's
// in-process build relies on this surface — without operation id
// forwarding, retries of the same logical request would re-advance
// the counter.
func TestStoreSharedStateHook_AdvanceUsesFetchAddIdempotency(t *testing.T) {
	t.Parallel()

	store := &fakeFetchAddStore{nextReturn: 7}
	hook := NewStoreSharedStateHook(store)
	const reqID = "op-uuid-xyz"

	advancer := hook.NewRoundRobinAdvancer(context.Background(), reqID)
	if advancer == nil {
		t.Fatal("NewRoundRobinAdvancer returned nil")
	}

	// 7 % 3 = 1; verifies modulo math runs on the worker side.
	idx, err := advancer.Advance("client-a", 3)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if idx != 1 {
		t.Fatalf("Advance: got %d, want 1 (7 mod 3)", idx)
	}

	if key, _ := store.lastKey.Load().(string); key != "client-a" {
		t.Fatalf("FetchAdd key: got %q, want client-a", key)
	}
	if delta := store.lastDelta.Load(); delta != 1 {
		t.Fatalf("FetchAdd delta: got %d, want 1", delta)
	}
	if id, _ := store.lastOpID.Load().(string); id != reqID {
		t.Fatalf("FetchAdd operation id: got %q, want %q (must forward request id for SharedStateStore idempotency)", id, reqID)
	}
}

// TestStoreSharedStateHook_AdvanceZeroChildCount mirrors the existing
// RemoteAdvancer contract: childCount <= 0 short-circuits to (0, nil)
// without touching the store, so an introspected client with zero
// configured children doesn't generate a FetchAdd RPC.
func TestStoreSharedStateHook_AdvanceZeroChildCount(t *testing.T) {
	t.Parallel()

	store := &fakeFetchAddStore{nextReturn: 99}
	hook := NewStoreSharedStateHook(store)
	advancer := hook.NewRoundRobinAdvancer(context.Background(), "req-id")

	idx, err := advancer.Advance("client-a", 0)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if idx != 0 {
		t.Fatalf("Advance: got %d, want 0", idx)
	}
	if calls := store.mu.Load(); calls != 0 {
		t.Fatalf("store FetchAdd calls: got %d, want 0 (childCount=0 must not touch the store)", calls)
	}
}
