package workerplugin

import (
	"context"
	"fmt"
	"time"

	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// RemoteAdvancer is a bamlutils.RoundRobinAdvancer that delegates to the
// host-side SharedState service over a brokered gRPC connection. It is
// constructed fresh per request because the operation_id is request-
// scoped — the host uses it to cache FetchAdd results so pool-level
// retries of the same request observe the same rotation index.
//
// Lives in the workerplugin package rather than bamlutils/buildrequest/
// roundrobin on purpose: RemoteAdvancer holds a pb.SharedStateClient,
// and pulling the generated gRPC types into bamlutils would force every
// version-pinned adapter (adapter_v0_204_0, adapter_v0_215_0) to add a
// workerplugin replace directive to its go.mod. Keeping the proto
// dependency here lets the bamlutils tree stay self-contained.
//
// RemoteAdvancer is not safe to reuse across requests: the embedded
// operationID must change with each request or retries of a later
// request would read a stale cached value for an earlier one.
type RemoteAdvancer struct {
	client      pb.SharedStateClient
	operationID string
	// parent is the request-scoped context used as the parent for every
	// FetchAdd RPC. Stored rather than threaded through Advance because
	// bamlutils.RoundRobinAdvancer.Advance is ctx-less — the interface
	// is shared with the in-process Coordinator (which never blocks) and
	// with the generated dispatch code, and adding ctx would ripple
	// through every call site for the sake of this one remote case. A
	// per-request advancer that remembers its request ctx is the local
	// fix.
	//
	// Propagating request cancellation matters because without it the
	// FetchAdd keeps running for up to `timeout` after the caller has
	// gone away — wasted work on a hot request path, and the counter
	// still advances on the host even though nobody is going to observe
	// the result. The P1-1 idempotency cache makes this benign for
	// correctness (a retry with the same op_id would see the cached
	// value), but the parent-ctx link is the right way to stop doing
	// the work in the first place.
	parent context.Context
	// timeout bounds each FetchAdd call. The call is a single unary RPC
	// over an in-process broker socket, so the practical cost is a
	// microsecond; the timeout is a liveness guard, not latency shaping.
	timeout time.Duration
}

// NewRemoteAdvancer constructs a RemoteAdvancer scoped to one request.
// operationID is the value the host will use as its idempotency key —
// pass the CallRequest.request_id verbatim. An empty operationID means
// no idempotency caching on the host (every call advances), which is
// safe but re-advances on pool retries; prefer passing the request id
// whenever one is available.
//
// parent is the request-scoped context. FetchAdd inherits cancellation
// from it, so if the caller disconnects the RPC is aborted rather than
// held open for the full timeout. Passing nil falls back to
// context.Background — acceptable for test harnesses but not production,
// where losing cancellation means one wasted RPC per cancelled request.
func NewRemoteAdvancer(parent context.Context, client pb.SharedStateClient, operationID string) *RemoteAdvancer {
	if parent == nil {
		parent = context.Background()
	}
	return &RemoteAdvancer{
		client:      client,
		operationID: operationID,
		parent:      parent,
		timeout:     5 * time.Second,
	}
}

// Advance calls SharedState.FetchAdd with delta=1 and applies the
// modulus locally. Transport errors are propagated to the caller rather
// than silently downgraded to random selection — a pool-managed worker
// that can't reach the host must fail the request, otherwise the fleet
// would quietly degrade to per-worker rotation exactly when central
// coordination is supposed to kick in. Callers (orchestrator) translate
// the error into a request-level failure; the next request gets a fresh
// advancer and a fresh attempt at the host.
func (r *RemoteAdvancer) Advance(clientName string, childCount int) (int, error) {
	if childCount <= 0 {
		return 0, nil
	}
	if r == nil || r.client == nil {
		// A nil advancer here is a wiring bug (cmd/worker should never
		// install a nil client); return an error rather than random so
		// the misconfiguration is visible.
		return 0, fmt.Errorf("round-robin remote advancer has no SharedState client")
	}
	parent := r.parent
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	resp, err := r.client.FetchAdd(ctx, &pb.FetchAddRequest{
		Key:         clientName,
		Delta:       1,
		OperationId: r.operationID,
	})
	if err != nil {
		return 0, fmt.Errorf("round-robin shared-state FetchAdd for %q: %w", clientName, err)
	}
	return int(resp.GetPrevious() % uint64(childCount)), nil
}
