package workerplugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	pb "github.com/invakid404/baml-rest/workerplugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type testWorkerImpl struct {
	callStream func(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *StreamResult, error)
}

func (w *testWorkerImpl) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *StreamResult, error) {
	if w.callStream != nil {
		return w.callStream(ctx, methodName, inputJSON, streamMode)
	}
	panic("unexpected CallStream call")
}

func (w *testWorkerImpl) Health(context.Context) (bool, error) { panic("unexpected Health call") }
func (w *testWorkerImpl) GetMetrics(context.Context) ([][]byte, error) {
	panic("unexpected GetMetrics call")
}
func (w *testWorkerImpl) TriggerGC(context.Context) (*GCResult, error) {
	panic("unexpected TriggerGC call")
}
func (w *testWorkerImpl) Parse(context.Context, string, []byte) (*ParseResult, error) {
	panic("unexpected Parse call")
}
func (w *testWorkerImpl) GetGoroutines(context.Context, string) (*GoroutinesResult, error) {
	panic("unexpected GetGoroutines call")
}

type testCallStreamServer struct {
	ctx  context.Context
	sent []*pb.StreamResult
}

func (s *testCallStreamServer) Send(resp *pb.StreamResult) error {
	s.sent = append(s.sent, resp)
	return nil
}

func (s *testCallStreamServer) SetHeader(metadata.MD) error  { return nil }
func (s *testCallStreamServer) SendHeader(metadata.MD) error { return nil }
func (s *testCallStreamServer) SetTrailer(metadata.MD)       {}
func (s *testCallStreamServer) Context() context.Context     { return s.ctx }
func (s *testCallStreamServer) SendMsg(any) error            { return nil }
func (s *testCallStreamServer) RecvMsg(any) error            { return nil }

type testWorkerClient struct {
	stream grpc.ServerStreamingClient[pb.StreamResult]
	err    error
}

func (c *testWorkerClient) CallStream(context.Context, *pb.CallRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.StreamResult], error) {
	return c.stream, c.err
}

func (c *testWorkerClient) Health(context.Context, *pb.Empty, ...grpc.CallOption) (*pb.HealthResponse, error) {
	panic("unexpected Health call")
}

func (c *testWorkerClient) GetMetrics(context.Context, *pb.Empty, ...grpc.CallOption) (*pb.MetricsResponse, error) {
	panic("unexpected GetMetrics call")
}

func (c *testWorkerClient) TriggerGC(context.Context, *pb.Empty, ...grpc.CallOption) (*pb.GCResponse, error) {
	panic("unexpected TriggerGC call")
}

func (c *testWorkerClient) Parse(context.Context, *pb.ParseRequest, ...grpc.CallOption) (*pb.ParseResponse, error) {
	panic("unexpected Parse call")
}

func (c *testWorkerClient) GetGoroutines(context.Context, *pb.GetGoroutinesRequest, ...grpc.CallOption) (*pb.GetGoroutinesResponse, error) {
	panic("unexpected GetGoroutines call")
}

func (c *testWorkerClient) AttachSharedState(context.Context, *pb.AttachSharedStateRequest, ...grpc.CallOption) (*pb.Empty, error) {
	panic("unexpected AttachSharedState call")
}

type testStreamEvent struct {
	resp *pb.StreamResult
	err  error
}

type testCallStreamClient struct {
	events []testStreamEvent
	index  int
}

func (c *testCallStreamClient) Recv() (*pb.StreamResult, error) {
	if c.index >= len(c.events) {
		return nil, io.EOF
	}
	event := c.events[c.index]
	c.index++
	return event.resp, event.err
}

func (c *testCallStreamClient) Header() (metadata.MD, error) { return nil, nil }
func (c *testCallStreamClient) Trailer() metadata.MD         { return nil }
func (c *testCallStreamClient) CloseSend() error             { return nil }
func (c *testCallStreamClient) Context() context.Context     { return context.Background() }
func (c *testCallStreamClient) SendMsg(any) error            { return nil }
func (c *testCallStreamClient) RecvMsg(any) error            { return nil }

// TestGRPCRoundTripMetadataKind walks a metadata frame through both server
// and client halves of the gRPC bridge, verifying the new METADATA enum
// value is preserved end-to-end (kind cast at server, decoded at client)
// and the JSON payload survives intact.
func TestGRPCRoundTripMetadataKind(t *testing.T) {
	payload := []byte(`{"phase":"planned","attempt":2,"path":"buildrequest","client":"X"}`)

	// Server side: workerImpl emits a metadata StreamResult; GRPCServer.CallStream
	// must convert it into the right pb.StreamResult kind.
	worker := &testWorkerImpl{
		callStream: func(ctx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *StreamResult, error) {
			ch := make(chan *StreamResult, 2)
			r := GetStreamResult()
			r.Kind = StreamResultKindMetadata
			r.Data = payload
			ch <- r
			final := GetStreamResult()
			final.Kind = StreamResultKindFinal
			final.Data = []byte(`{"ok":true}`)
			ch <- final
			close(ch)
			return ch, nil
		},
	}

	server := &GRPCServer{Impl: worker}
	stream := &testCallStreamServer{ctx: context.Background()}
	if err := server.CallStream(&pb.CallRequest{}, stream); err != nil {
		t.Fatalf("server.CallStream returned error: %v", err)
	}

	if len(stream.sent) != 2 {
		t.Fatalf("expected 2 frames sent server-side, got %d", len(stream.sent))
	}
	if stream.sent[0].Kind != pb.StreamResult_METADATA {
		t.Errorf("server sent first frame as %v, want METADATA", stream.sent[0].Kind)
	}
	if string(stream.sent[0].DataJson) != string(payload) {
		t.Errorf("server payload corrupted: got %q, want %q", stream.sent[0].DataJson, payload)
	}

	// Client side: feed the wire frames back through testCallStreamClient and
	// verify GRPCClient.CallStream returns matching StreamResultKindMetadata.
	events := make([]testStreamEvent, 0, len(stream.sent))
	for _, frame := range stream.sent {
		events = append(events, testStreamEvent{resp: frame})
	}
	client := &GRPCClient{client: &testWorkerClient{stream: &testCallStreamClient{events: events}}}
	out, err := client.CallStream(context.Background(), "TestMethod", nil, bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("client.CallStream: %v", err)
	}

	var receivedKinds []StreamResultKind
	var firstPayload []byte
	for r := range out {
		receivedKinds = append(receivedKinds, r.Kind)
		if r.Kind == StreamResultKindMetadata && firstPayload == nil {
			firstPayload = append(firstPayload[:0], r.Data...)
		}
		ReleaseStreamResult(r)
	}

	if len(receivedKinds) != 2 {
		t.Fatalf("expected 2 frames received client-side, got %d", len(receivedKinds))
	}
	if receivedKinds[0] != StreamResultKindMetadata {
		t.Errorf("client received first frame as %d, want StreamResultKindMetadata (%d)", receivedKinds[0], StreamResultKindMetadata)
	}
	if string(firstPayload) != string(payload) {
		t.Errorf("client payload corrupted: got %q, want %q", firstPayload, payload)
	}
}

func TestGRPCClientCallStreamEOFHandling(t *testing.T) {
	tests := []struct {
		name      string
		recvErr   error
		wantKinds []StreamResultKind
		wantErr   error
	}{
		{
			name:      "exact eof ends stream cleanly",
			recvErr:   io.EOF,
			wantKinds: []StreamResultKind{StreamResultKindStream},
		},
		{
			name:      "wrapped eof ends stream cleanly",
			recvErr:   fmt.Errorf("stream closed: %w", io.EOF),
			wantKinds: []StreamResultKind{StreamResultKindStream},
		},
		{
			name:      "non eof becomes stream error",
			recvErr:   fmt.Errorf("stream failed: %w", io.ErrUnexpectedEOF),
			wantKinds: []StreamResultKind{StreamResultKindStream, StreamResultKindError},
			wantErr:   io.ErrUnexpectedEOF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &GRPCClient{client: &testWorkerClient{stream: &testCallStreamClient{events: []testStreamEvent{
				{resp: &pb.StreamResult{Kind: pb.StreamResult_STREAM, DataJson: []byte(`"chunk"`)}},
				{err: tt.recvErr},
			}}}}

			results, err := client.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
			if err != nil {
				t.Fatalf("CallStream() error = %v", err)
			}

			var got []*StreamResult
			for result := range results {
				got = append(got, result)
			}

			if len(got) != len(tt.wantKinds) {
				t.Fatalf("len(results) = %d, want %d", len(got), len(tt.wantKinds))
			}

			for i, result := range got {
				if result.Kind != tt.wantKinds[i] {
					t.Fatalf("result[%d].Kind = %v, want %v", i, result.Kind, tt.wantKinds[i])
				}
			}

			if tt.wantErr == nil {
				if got[len(got)-1].Error != nil {
					t.Fatalf("last result error = %v, want nil", got[len(got)-1].Error)
				}
			} else if !errors.Is(got[len(got)-1].Error, tt.wantErr) {
				t.Fatalf("last result error = %v, want error matching %v", got[len(got)-1].Error, tt.wantErr)
			}

			for _, result := range got {
				ReleaseStreamResult(result)
			}
		})
	}
}

func TestGRPCServerCallStreamReturnsCancellationWithoutTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	worker := &testWorkerImpl{
		callStream: func(streamCtx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *StreamResult, error) {
			ch := make(chan *StreamResult, 1)
			result := GetStreamResult()
			result.Kind = StreamResultKindStream
			result.Data = []byte(`"chunk"`)
			ch <- result
			go func() {
				<-streamCtx.Done()
				close(ch)
			}()
			return ch, nil
		},
	}
	server := &GRPCServer{Impl: worker}
	stream := &testCallStreamServer{ctx: ctx}

	cancel()
	err := server.CallStream(&pb.CallRequest{MethodName: "Test"}, stream)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CallStream() error = %v, want context canceled", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent results = %d, want 1", len(stream.sent))
	}
	if stream.sent[0].Kind != pb.StreamResult_STREAM {
		t.Fatalf("sent result kind = %v, want STREAM", stream.sent[0].Kind)
	}
}

// TestGRPCRoundTripPreservesErrorStringsForCancellation verifies that
// cancellation errors survive the server→proto→client serialization path
// in their string form, so downstream classifiers can recognize them.
func TestGRPCRoundTripPreservesErrorStringsForCancellation(t *testing.T) {
	tests := []struct {
		name    string
		origErr error
		wantSub string // substring expected in reconstructed error
	}{
		{
			name:    "context.Canceled",
			origErr: context.Canceled,
			wantSub: "context canceled",
		},
		{
			name:    "context.DeadlineExceeded",
			origErr: context.DeadlineExceeded,
			wantSub: "context deadline exceeded",
		},
		{
			name:    "wrapped cancellation",
			origErr: fmt.Errorf("stream failed: %w", context.Canceled),
			wantSub: "context canceled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Server side: produce error stream result
			worker := &testWorkerImpl{
				callStream: func(context.Context, string, []byte, bamlutils.StreamMode) (<-chan *StreamResult, error) {
					ch := make(chan *StreamResult, 1)
					result := GetStreamResult()
					result.Kind = StreamResultKindError
					result.Error = tt.origErr
					ch <- result
					close(ch)
					return ch, nil
				},
			}
			server := &GRPCServer{Impl: worker}
			stream := &testCallStreamServer{ctx: context.Background()}

			err := server.CallStream(&pb.CallRequest{MethodName: "Test"}, stream)
			if err != nil {
				t.Fatalf("server CallStream() error = %v", err)
			}
			if len(stream.sent) != 1 {
				t.Fatalf("sent results = %d, want 1", len(stream.sent))
			}

			// Client side: reconstruct error from proto
			client := &GRPCClient{client: &testWorkerClient{stream: &testCallStreamClient{events: []testStreamEvent{
				{resp: stream.sent[0]},
			}}}}

			results, err := client.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeCall)
			if err != nil {
				t.Fatalf("client CallStream() error = %v", err)
			}

			var got []*StreamResult
			for r := range results {
				got = append(got, r)
			}
			if len(got) != 1 {
				t.Fatalf("results = %d, want 1", len(got))
			}
			if got[0].Kind != StreamResultKindError {
				t.Fatalf("result kind = %v, want Error", got[0].Kind)
			}
			if got[0].Error == nil {
				t.Fatal("expected non-nil error")
			}

			// The reconstructed error must contain the cancellation string
			// even though errors.Is(err, context.Canceled) will be false.
			errStr := got[0].Error.Error()
			if !strings.Contains(errStr, tt.wantSub) {
				t.Errorf("reconstructed error %q does not contain %q", errStr, tt.wantSub)
			}

			for _, r := range got {
				ReleaseStreamResult(r)
			}
		})
	}
}

func TestGRPCServerCallStreamDoesNotOverrideTerminalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	worker := &testWorkerImpl{
		callStream: func(context.Context, string, []byte, bamlutils.StreamMode) (<-chan *StreamResult, error) {
			ch := make(chan *StreamResult, 1)
			result := GetStreamResult()
			result.Kind = StreamResultKindFinal
			result.Data = []byte(`"done"`)
			ch <- result
			close(ch)
			return ch, nil
		},
	}
	server := &GRPCServer{Impl: worker}
	stream := &testCallStreamServer{ctx: ctx}

	cancel()
	err := server.CallStream(&pb.CallRequest{MethodName: "Test"}, stream)
	if err != nil {
		t.Fatalf("CallStream() error = %v, want nil", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent results = %d, want 1", len(stream.sent))
	}
	if stream.sent[0].Kind != pb.StreamResult_FINAL {
		t.Fatalf("sent result kind = %v, want FINAL", stream.sent[0].Kind)
	}
}
