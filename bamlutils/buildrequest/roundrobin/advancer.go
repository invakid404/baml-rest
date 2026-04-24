package roundrobin

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// Advancer decides the next child index for a static round-robin client.
// The interface is defined in the bamlutils root package so the Adapter
// interface (which is passed through generated code) can declare a
// RoundRobinAdvancer accessor without introducing an import cycle. The
// alias here keeps existing call sites that spell it "roundrobin.Advancer"
// working unchanged.
//
// Two implementations exist:
//
//   - *Coordinator: in-process counters. Used by standalone worker
//     binaries (no host shared state), tests, and any callsite that
//     predates the brokered SharedState wiring. Each process rotates
//     independently — with N workers you get N counters.
//   - *RemoteAdvancer: calls FetchAdd on the host-side SharedState
//     service. Used by pool-managed workers so every worker in the
//     pool observes a single rotation. This is what makes round-robin
//     actually behave like round-robin across a multi-worker pool.
//
// Transient gRPC failures on the remote path fall back to AdvanceDynamic
// so a broken host socket degrades to per-request random selection rather
// than wedging the entire worker.
type Advancer = bamlutils.RoundRobinAdvancer

// Compile-time assertion that the in-process Coordinator satisfies
// Advancer. Keeps the two implementations contractually aligned.
var _ Advancer = (*Coordinator)(nil)

// RemoteAdvancer is an Advancer that delegates to the host-side
// SharedState service over a brokered gRPC connection. It is constructed
// fresh per request because the operation_id is request-scoped — the
// host uses it to cache FetchAdd results so pool-level retries of the
// same request observe the same rotation index.
//
// RemoteAdvancer is not safe to reuse across requests: the embedded
// operationID must change with each request or retries of a later
// request would read a stale cached value for an earlier one.
type RemoteAdvancer struct {
	client      pb.SharedStateClient
	operationID string
	// timeout bounds each FetchAdd call. The call is a single unary RPC
	// over an in-process broker socket, so the practical cost is a
	// microsecond; we set a generous ceiling purely as a liveness guard.
	timeout time.Duration
}

// NewRemoteAdvancer constructs a RemoteAdvancer scoped to one request.
// operationID is the value the host will use as its idempotency key —
// pass the CallRequest.request_id verbatim. An empty operationID means
// no idempotency caching on the host (every call advances), which is
// safe but re-advances on pool retries; prefer passing the request id
// whenever one is available.
func NewRemoteAdvancer(client pb.SharedStateClient, operationID string) *RemoteAdvancer {
	return &RemoteAdvancer{
		client:      client,
		operationID: operationID,
		timeout:     5 * time.Second,
	}
}

var remoteAdvancerFailures atomic.Uint64

// Advance calls SharedState.FetchAdd with delta=1 and applies the
// modulus locally. On transport error it logs and falls back to a fresh
// random index via AdvanceDynamic — the host being unreachable must not
// block the request path, and a per-request random pick is a strictly
// weaker but still correct rotation.
func (r *RemoteAdvancer) Advance(clientName string, childCount int) int {
	if childCount <= 0 {
		return 0
	}
	if r == nil || r.client == nil {
		return AdvanceDynamic(childCount)
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	resp, err := r.client.FetchAdd(ctx, &pb.FetchAddRequest{
		Key:         clientName,
		Delta:       1,
		OperationId: r.operationID,
	})
	if err != nil {
		// Rate-limit noisy logging under sustained host outages: log on
		// transitions (first failure per 256 errors) rather than every
		// call. An atomic counter is sufficient — operators just need to
		// notice the degradation, not get stacks on every attempt.
		if n := remoteAdvancerFailures.Add(1); n == 1 || n%256 == 0 {
			log.Printf("[WARN] round-robin remote advancer fetch-add failed for %q (failure #%d): %v", clientName, n, err)
		}
		return AdvanceDynamic(childCount)
	}
	return int(resp.GetPrevious() % uint64(childCount))
}
