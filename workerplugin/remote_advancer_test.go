package workerplugin

import (
	"context"
	"errors"
	"testing"

	pb "github.com/invakid404/baml-rest/workerplugin/proto"
	"google.golang.org/grpc"
)

// fakeSharedStateClient lets tests observe the context passed to FetchAdd
// and decide the response. The hook signature mirrors the generated
// interface so a test can assert cancellation without a real gRPC server.
type fakeSharedStateClient struct {
	fetchAdd func(ctx context.Context, req *pb.FetchAddRequest) (*pb.FetchAddResponse, error)
}

func (f *fakeSharedStateClient) FetchAdd(ctx context.Context, req *pb.FetchAddRequest, _ ...grpc.CallOption) (*pb.FetchAddResponse, error) {
	return f.fetchAdd(ctx, req)
}

func TestRemoteAdvancer_AdvanceReturnsPreviousModuloChildCount(t *testing.T) {
	// Sanity path: happy FetchAdd returns the modded pre-increment value.
	// If this regresses the whole pool-wide rotation is off, so guard it
	// with an explicit test rather than relying on integration coverage.
	fake := &fakeSharedStateClient{
		fetchAdd: func(_ context.Context, req *pb.FetchAddRequest) (*pb.FetchAddResponse, error) {
			if req.GetKey() != "Strat" {
				t.Fatalf("key: got %q, want Strat", req.GetKey())
			}
			if req.GetDelta() != 1 {
				t.Fatalf("delta: got %d, want 1", req.GetDelta())
			}
			if req.GetOperationId() != "op-1" {
				t.Fatalf("operation_id: got %q, want op-1", req.GetOperationId())
			}
			return &pb.FetchAddResponse{Previous: 7}, nil
		},
	}
	r := NewRemoteAdvancer(context.Background(), fake, "op-1")
	idx, err := r.Advance("Strat", 3)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// 7 % 3 == 1
	if idx != 1 {
		t.Fatalf("idx: got %d, want 1", idx)
	}
}

func TestRemoteAdvancer_PropagatesParentCancellation(t *testing.T) {
	// The RPC must inherit cancellation from the per-request parent
	// context rather than always using Background. Otherwise a
	// disconnected caller would leave the FetchAdd running for up to
	// r.timeout (5s) before the deadline fired.
	parent, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled so FetchAdd sees ctx.Err() immediately

	var observedErr error
	fake := &fakeSharedStateClient{
		fetchAdd: func(ctx context.Context, _ *pb.FetchAddRequest) (*pb.FetchAddResponse, error) {
			// The real gRPC client checks ctx before sending; simulate
			// that so the test remains meaningful without a network hop.
			if err := ctx.Err(); err != nil {
				observedErr = err
				return nil, err
			}
			t.Fatalf("FetchAdd invoked with live ctx — parent cancellation did not propagate")
			return nil, nil
		},
	}
	r := NewRemoteAdvancer(parent, fake, "op")
	_, err := r.Advance("Strat", 2)
	if err == nil {
		t.Fatal("Advance: expected error from cancelled parent ctx, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Advance: expected wrapped context.Canceled, got %v", err)
	}
	if !errors.Is(observedErr, context.Canceled) {
		t.Fatalf("FetchAdd: expected ctx.Err to be context.Canceled, got %v", observedErr)
	}
}

func TestRemoteAdvancer_NilParentFallsBackToBackground(t *testing.T) {
	// A nil parent ctx must not panic — the constructor and Advance both
	// fall back to Background. Relevant for test harnesses that don't
	// plumb a request ctx; production cmd/worker always passes one.
	fake := &fakeSharedStateClient{
		fetchAdd: func(ctx context.Context, _ *pb.FetchAddRequest) (*pb.FetchAddResponse, error) {
			if ctx == nil {
				t.Fatal("ctx is nil — WithTimeout was not applied")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("ctx has no deadline — WithTimeout was skipped")
			}
			return &pb.FetchAddResponse{Previous: 0}, nil
		},
	}
	//lint:ignore SA1012 deliberately passing nil to exercise the guard.
	r := NewRemoteAdvancer(nil, fake, "op")
	if _, err := r.Advance("Strat", 2); err != nil {
		t.Fatalf("Advance with nil parent: %v", err)
	}
}
