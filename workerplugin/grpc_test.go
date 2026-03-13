package workerplugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	pb "github.com/invakid404/baml-rest/workerplugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

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
