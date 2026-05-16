package worker

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// SharedStateHook is the worker-facing seam that produces a per-request
// round-robin advancer. The subprocess binary wraps the existing
// pb.SharedStateClient (which talks to the host over the go-plugin broker
// socket); the inprocess server wiring in cmd/serve installs the
// store-backed hook returned by NewStoreSharedStateHook so the worker
// reaches the host's SharedStateStore directly without crossing gRPC.
//
// The shape is intentionally narrow — Advance is the only round-robin
// operation the handler needs, and DropScope remains the pool's
// responsibility. Returning a bamlutils.RoundRobinAdvancer rather than
// the underlying client keeps protobuf/gRPC types out of internal/worker.
type SharedStateHook interface {
	// NewRoundRobinAdvancer returns the Advancer the generated dispatch
	// path should use for the given request. Implementations may return
	// nil to signal "no advancer for this request"; callers treat nil as
	// fall-through to the introspected default Coordinator.
	NewRoundRobinAdvancer(ctx context.Context, requestID string) bamlutils.RoundRobinAdvancer
}

// FetchAddStore is the minimal store-side surface a direct-call
// SharedStateHook needs. *workerplugin.SharedStateStore satisfies this
// without importing protobuf or gRPC types into internal/worker.
type FetchAddStore interface {
	FetchAdd(key string, delta uint64, operationID string) uint64
}

// NewStoreSharedStateHook returns a SharedStateHook backed by an
// in-memory FetchAdd store. Used by inprocess builds where the worker
// shares the host's SharedStateStore directly rather than going through
// the brokered gRPC client. The subprocess binary does not invoke this —
// the wire layer continues to use workerplugin.NewRemoteAdvancer via
// the cmd/worker grpcSharedStateHook wrapper, preserving byte-identical
// IPC behavior.
func NewStoreSharedStateHook(store FetchAddStore) SharedStateHook {
	return &storeSharedStateHook{store: store}
}

type storeSharedStateHook struct {
	store FetchAddStore
}

func (h *storeSharedStateHook) NewRoundRobinAdvancer(_ context.Context, requestID string) bamlutils.RoundRobinAdvancer {
	return &storeRoundRobinAdvancer{store: h.store, operationID: requestID}
}

// storeRoundRobinAdvancer is the direct-call counterpart to
// workerplugin.RemoteAdvancer: it advances counters by calling
// SharedStateStore.FetchAdd in-process rather than over gRPC. The
// timeout machinery RemoteAdvancer needs for the brokered RPC is
// unnecessary here — FetchAdd is an atomic in-memory operation.
type storeRoundRobinAdvancer struct {
	store       FetchAddStore
	operationID string
}

func (a *storeRoundRobinAdvancer) Advance(clientName string, childCount int) (int, error) {
	if childCount <= 0 {
		return 0, nil
	}
	if a == nil || a.store == nil {
		// A nil store here is a wiring bug — return an error rather than
		// silently downgrading to per-worker rotation.
		return 0, fmt.Errorf("round-robin shared-state hook has no store")
	}
	prev := a.store.FetchAdd(clientName, 1, a.operationID)
	return int(prev % uint64(childCount)), nil
}

// sharedStateHookHolder wraps the interface value so atomic.Pointer can
// store concrete types that differ between subprocess (grpcSharedStateHook)
// and direct-call (storeSharedStateHook) builds. atomic.Value would panic on
// the second Store with a different concrete type.
type sharedStateHookHolder struct {
	hook SharedStateHook
}

// SetSharedStateHook installs the per-request advancer factory. The
// subprocess binary calls this from the AttachSharedState callback
// after handshake; tests and in-process callers can call it any time
// before the first request lands. Passing nil clears the hook.
func (h *Handler) SetSharedStateHook(hook SharedStateHook) {
	if hook == nil {
		h.sharedStateHook.Store(nil)
		return
	}
	h.sharedStateHook.Store(&sharedStateHookHolder{hook: hook})
}

// roundRobinAdvancerFor returns the Advancer the generated dispatch path
// should use for this request. When a shared-state hook is installed AND
// the inbound context carries a non-empty request id it builds a
// request-scoped advancer via the hook; otherwise it returns nil so
// orchestrator.ResolveEffectiveClient falls back to the package-level
// Coordinator compiled into introspected.
func (h *Handler) roundRobinAdvancerFor(ctx context.Context) bamlutils.RoundRobinAdvancer {
	holder := h.sharedStateHook.Load()
	if holder == nil || holder.hook == nil {
		h.noSharedStateWarnOnce.Do(func() {
			if h.logger != nil {
				h.logger.Warn(
					"round-robin falling back to in-process coordinator; " +
						"host did not complete AttachSharedState handshake. " +
						"Pool-managed workers should always attach — this " +
						"usually indicates a broken handshake or a standalone " +
						"test harness. baml-roundrobin rotations will be " +
						"per-worker instead of pool-wide for the rest of " +
						"this process's lifetime.")
			}
		})
		return nil
	}
	requestID := workerplugin.RequestIDFromContext(ctx)
	if requestID == "" {
		h.missingRequestIDWarnOnce.Do(func() {
			if h.logger != nil {
				h.logger.Warn(
					"round-robin falling back to in-process coordinator; " +
						"SharedState is attached but the inbound context " +
						"carries no request id. SharedStateStore.FetchAdd " +
						"bypasses its idempotency cache for empty operation " +
						"ids, so retries of the same logical request would " +
						"advance the counter twice. Pool-managed callers " +
						"always install a UUID — this usually indicates a " +
						"standalone / non-pool harness that wired SharedState " +
						"without threading request ids.")
			}
		})
		return nil
	}
	return holder.hook.NewRoundRobinAdvancer(ctx, requestID)
}

// hookStorage is the atomic field type for a Handler's shared-state
// hook slot. Encapsulated so callers can swap implementations without
// re-deriving the holder pattern.
type hookStorage struct {
	atomic.Pointer[sharedStateHookHolder]
}
