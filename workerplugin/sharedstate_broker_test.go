package workerplugin

import (
	"context"
	"sync"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// TestSharedState_BrokerHandshakeAndFetchAdd exercises the full PR
// central-coordination wiring end-to-end in-process:
//
//   1. host builds a SharedStateStore + WorkerPlugin.SharedStateImpl,
//   2. plugin handshake runs, which triggers GRPCClient → broker.Accept
//      on the reverse SharedStateBrokerID socket + AttachSharedState
//      RPC to the worker,
//   3. worker-side AttachSharedState handler receives a usable
//      pb.SharedStateClient and stores it,
//   4. that installed client issues FetchAdd calls over the broker to
//      the host store,
//   5. the test asserts the host-side counter advanced and that
//      operation_id-based idempotency works across the broker.
//
// Guards against regressions in: SharedStateBrokerID timing on
// broker.Accept; AttachSharedState RPC wiring; pb.SharedStateClient
// usability over the reverse channel; operation_id round-trip through
// FetchAddRequest. A refactor that silently drops the reverse channel
// (e.g., GRPCClient stops calling AttachSharedState, or the handler
// receives a nil client) would fail here; unit tests of SharedStateStore
// and RemoteAdvancer against a fake client would not.
func TestSharedState_BrokerHandshakeAndFetchAdd(t *testing.T) {
	// Deterministic seed so first-touch key returns 0 (baseline for the
	// "advanced by N" assertions below) rather than a random offset.
	store := NewSharedStateStore(func(string) uint64 { return 0 })
	defer store.Close()

	var (
		installMu sync.Mutex
		installed pb.SharedStateClient
	)
	// Reuse the existing testWorkerImpl stub from grpc_test.go. The
	// broker handshake runs during Dispense regardless of whether any
	// Worker RPC fires; Worker methods panic if called, which would
	// signal that the test's scope drifted beyond broker-only coverage.
	workerPlugin := &WorkerPlugin{
		Impl:            &testWorkerImpl{},
		SharedStateImpl: NewSharedStateServer(store),
	}
	workerPlugin.SetAttachSharedStateHandler(func(_ context.Context, client pb.SharedStateClient) error {
		installMu.Lock()
		installed = client
		installMu.Unlock()
		return nil
	})

	// TestPluginGRPCConn stands up both host and worker over an
	// in-process listener. Dispense triggers the GRPCClient path on
	// the host, which runs broker.Accept(SharedStateBrokerID) + go
	// server.Serve + AttachSharedState; the worker-side handler then
	// dials back via broker.DialWithOptions and hands us the client
	// via the SetAttachSharedStateHandler callback above.
	client, _ := goplugin.TestPluginGRPCConn(t, false, map[string]goplugin.Plugin{
		"worker": workerPlugin,
	})
	// client.Close() triggers go-plugin's controller Shutdown RPC,
	// which calls GRPCServer.Stop() on the server side for us. Calling
	// server.Stop() explicitly in cleanup races that internal Stop —
	// both paths read/write the same unexported server-state fields
	// concurrently and fail under `go test -race -count=100`. Let the
	// harness own the server lifecycle; client close is enough.
	defer func() { _ = client.Close() }()

	if _, err := client.Dispense("worker"); err != nil {
		t.Fatalf("Dispense: %v", err)
	}

	installMu.Lock()
	sharedStateClient := installed
	installMu.Unlock()
	if sharedStateClient == nil {
		t.Fatal("AttachSharedState did not install a SharedState client; reverse broker channel is broken")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First FetchAdd over the broker: must reach the host store and
	// return the pre-increment value (0).
	resp1, err := sharedStateClient.FetchAdd(ctx, &pb.FetchAddRequest{
		Key:         "clientA",
		Delta:       1,
		OperationId: "req-1",
	})
	if err != nil {
		t.Fatalf("FetchAdd #1 over broker: %v", err)
	}
	if resp1.GetPrevious() != 0 {
		t.Fatalf("FetchAdd #1: got previous=%d, want 0", resp1.GetPrevious())
	}

	// Replay with the SAME op_id: the host store's idempotency cache
	// must return the cached value without advancing the counter. This
	// is the pool-retry contract — a broken request_id propagation
	// path (e.g., the worker losing the id in transit) would make the
	// host treat the replay as a new operation and double-advance.
	resp2, err := sharedStateClient.FetchAdd(ctx, &pb.FetchAddRequest{
		Key:         "clientA",
		Delta:       1,
		OperationId: "req-1",
	})
	if err != nil {
		t.Fatalf("FetchAdd #2 over broker (replay): %v", err)
	}
	if resp2.GetPrevious() != 0 {
		t.Fatalf("FetchAdd #2 replay: got previous=%d, want 0 (idempotency broken across broker)", resp2.GetPrevious())
	}

	// A different op_id advances by exactly one.
	resp3, err := sharedStateClient.FetchAdd(ctx, &pb.FetchAddRequest{
		Key:         "clientA",
		Delta:       1,
		OperationId: "req-2",
	})
	if err != nil {
		t.Fatalf("FetchAdd #3 over broker (new op): %v", err)
	}
	if resp3.GetPrevious() != 1 {
		t.Fatalf("FetchAdd #3 new op: got previous=%d, want 1", resp3.GetPrevious())
	}

	// Empty operation_id explicitly disables idempotency — every call
	// advances. This catches the silent-regression case where request_id
	// propagation breaks (worker sees empty string) and every pool
	// retry gets a fresh counter slot instead of the cached value.
	resp4, err := sharedStateClient.FetchAdd(ctx, &pb.FetchAddRequest{
		Key:         "clientA",
		Delta:       1,
		OperationId: "",
	})
	if err != nil {
		t.Fatalf("FetchAdd #4 over broker (empty op): %v", err)
	}
	if resp4.GetPrevious() != 2 {
		t.Fatalf("FetchAdd #4 empty op: got previous=%d, want 2", resp4.GetPrevious())
	}
}
