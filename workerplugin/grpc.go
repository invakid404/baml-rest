package workerplugin

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/hashicorp/go-plugin"
	"github.com/invakid404/baml-rest/bamlutils"
	pb "github.com/invakid404/baml-rest/workerplugin/proto"
	"google.golang.org/grpc"
)

// NewGRPCClient creates a GRPCClient from a raw gRPC connection.
// Used by the pool to create additional connections to the same worker process.
func NewGRPCClient(conn *grpc.ClientConn) *GRPCClient {
	return &GRPCClient{client: pb.NewWorkerClient(conn)}
}

// streamModeToPb converts bamlutils.StreamMode to pb.StreamMode
func streamModeToPb(m bamlutils.StreamMode) pb.StreamMode {
	switch m {
	case bamlutils.StreamModeCall:
		return pb.StreamMode_STREAM_MODE_CALL
	case bamlutils.StreamModeStream:
		return pb.StreamMode_STREAM_MODE_STREAM
	case bamlutils.StreamModeCallWithRaw:
		return pb.StreamMode_STREAM_MODE_CALL_WITH_RAW
	case bamlutils.StreamModeStreamWithRaw:
		return pb.StreamMode_STREAM_MODE_STREAM_WITH_RAW
	default:
		return pb.StreamMode_STREAM_MODE_CALL
	}
}

// pbToStreamMode converts pb.StreamMode to bamlutils.StreamMode
func pbToStreamMode(m pb.StreamMode) bamlutils.StreamMode {
	switch m {
	case pb.StreamMode_STREAM_MODE_CALL:
		return bamlutils.StreamModeCall
	case pb.StreamMode_STREAM_MODE_STREAM:
		return bamlutils.StreamModeStream
	case pb.StreamMode_STREAM_MODE_CALL_WITH_RAW:
		return bamlutils.StreamModeCallWithRaw
	case pb.StreamMode_STREAM_MODE_STREAM_WITH_RAW:
		return bamlutils.StreamModeStreamWithRaw
	default:
		return bamlutils.StreamModeCall
	}
}

// GRPCServer is the gRPC server that the plugin runs
type GRPCServer struct {
	pb.UnimplementedWorkerServer
	Impl Worker
	// broker is captured from WorkerPlugin.GRPCServer so the AttachSharedState
	// handler can dial back to the host-side broker socket. It is set once
	// at handshake time and never mutated.
	broker *plugin.GRPCBroker
	// onAttach is invoked after the worker successfully dials back to the
	// host's SharedState broker socket. Set by WorkerPlugin.GRPCServer.
	onAttach func(ctx context.Context, client pb.SharedStateClient) error
}

// AttachSharedState is called by the host after it begins serving the
// shared-state socket on the broker id given in the request. The worker
// dials back, hands the resulting client to the installed callback, and
// returns. Any dial error is propagated; the host treats a failure here
// as fatal for that worker (without shared state, pool-wide round-robin
// collapses back to per-worker coordinators, which is the bug this RPC
// exists to fix).
func (s *GRPCServer) AttachSharedState(ctx context.Context, req *pb.AttachSharedStateRequest) (*pb.Empty, error) {
	if s.broker == nil {
		return nil, fmt.Errorf("worker: broker not available for shared-state dial-back")
	}
	if s.onAttach == nil {
		// No handler installed — return success so standalone worker
		// binaries (launched without a pool, e.g. tests) can accept the
		// RPC without side effects. Production workers always install
		// a handler; its absence here means a misconfigured test harness.
		return &pb.Empty{}, nil
	}
	conn, err := s.broker.DialWithOptions(req.GetBrokerId(), GRPCDialOptions()...)
	if err != nil {
		return nil, fmt.Errorf("worker: dial shared-state broker id=%d: %w", req.GetBrokerId(), err)
	}
	client := pb.NewSharedStateClient(conn)
	if err := s.onAttach(ctx, client); err != nil {
		conn.Close()
		return nil, fmt.Errorf("worker: install shared-state client: %w", err)
	}
	// Intentionally do NOT close conn on success. The client is meant
	// to live for the worker's lifetime; go-plugin closes the underlying
	// broker stream when the plugin is killed, which tears the conn
	// down for us.
	return &pb.Empty{}, nil
}

func (s *GRPCServer) CallStream(req *pb.CallRequest, stream pb.Worker_CallStreamServer) error {
	// Re-attach the request_id from the wire payload onto the handler
	// context so the worker's round-robin wiring can pick it up without
	// every call path threading it as an argument. Empty RequestId is
	// fine — WithRequestID short-circuits.
	ctx := WithRequestID(stream.Context(), req.GetRequestId())
	results, err := s.Impl.CallStream(ctx, req.MethodName, req.InputJson, pbToStreamMode(req.StreamMode))
	if err != nil {
		return err
	}

	sentTerminal := false
	for result := range results {
		pbResult := &pb.StreamResult{
			Kind:     pb.StreamResult_Kind(result.Kind),
			DataJson: result.Data,
			Raw:      result.Raw,
			Reset_:   result.Reset,
		}
		if result.Error != nil {
			pbResult.Error = result.Error.Error()
			// Extract stacktrace using %+v formatting (works with go-recovery and pkg/errors style errors)
			if fullErr := fmt.Sprintf("%+v", result.Error); fullErr != pbResult.Error {
				pbResult.Stacktrace = fullErr
			}
		}
		if err := stream.Send(pbResult); err != nil {
			ReleaseStreamResult(result)
			return err
		}
		if result.Kind == StreamResultKindFinal || result.Kind == StreamResultKindError {
			sentTerminal = true
		}
		// Release the StreamResult back to pool after Send completes
		ReleaseStreamResult(result)
	}
	if !sentTerminal {
		if err := stream.Context().Err(); err != nil {
			return err
		}
	}
	return nil
}

func (s *GRPCServer) Health(ctx context.Context, req *pb.Empty) (*pb.HealthResponse, error) {
	healthy, err := s.Impl.Health(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.HealthResponse{Healthy: healthy}, nil
}

func (s *GRPCServer) GetMetrics(ctx context.Context, req *pb.Empty) (*pb.MetricsResponse, error) {
	metricFamilies, err := s.Impl.GetMetrics(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.MetricsResponse{MetricFamilies: metricFamilies}, nil
}

func (s *GRPCServer) TriggerGC(ctx context.Context, req *pb.Empty) (*pb.GCResponse, error) {
	result, err := s.Impl.TriggerGC(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.GCResponse{
		HeapAllocBefore: result.HeapAllocBefore,
		HeapAllocAfter:  result.HeapAllocAfter,
		HeapReleased:    result.HeapReleased,
	}, nil
}

func (s *GRPCServer) Parse(ctx context.Context, req *pb.ParseRequest) (*pb.ParseResponse, error) {
	result, err := s.Impl.Parse(ctx, req.MethodName, req.InputJson)
	if err != nil {
		resp := &pb.ParseResponse{Error: err.Error()}
		// Extract stacktrace using %+v formatting (works with go-recovery and pkg/errors style errors)
		if fullErr := fmt.Sprintf("%+v", err); fullErr != resp.Error {
			resp.Stacktrace = fullErr
		}
		return resp, nil
	}
	return &pb.ParseResponse{DataJson: result.Data}, nil
}

func (s *GRPCServer) GetGoroutines(ctx context.Context, req *pb.GetGoroutinesRequest) (*pb.GetGoroutinesResponse, error) {
	result, err := s.Impl.GetGoroutines(ctx, req.Filter)
	if err != nil {
		return nil, err
	}
	return &pb.GetGoroutinesResponse{
		TotalCount:    result.TotalCount,
		MatchCount:    result.MatchCount,
		MatchedStacks: result.MatchedStacks,
	}, nil
}

// GRPCClient is the gRPC client that connects to the plugin
type GRPCClient struct {
	client pb.WorkerClient
}

func (c *GRPCClient) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *StreamResult, error) {
	stream, err := c.client.CallStream(ctx, &pb.CallRequest{
		MethodName: methodName,
		InputJson:  inputJSON,
		StreamMode: streamModeToPb(streamMode),
		RequestId:  RequestIDFromContext(ctx),
	})
	if err != nil {
		return nil, err
	}

	results := make(chan *StreamResult, 1)
	go func() {
		defer close(results)
		for {
			resp, err := stream.Recv()
			if err != nil {
				// EOF means stream ended normally
				if errors.Is(err, io.EOF) {
					return
				}
				errResult := GetStreamResult()
				errResult.Kind = StreamResultKindError
				errResult.Error = err
				results <- errResult
				return
			}

			result := GetStreamResult()
			result.Kind = StreamResultKind(resp.Kind)
			result.Data = resp.DataJson
			result.Raw = resp.Raw
			result.Reset = resp.GetReset_()
			if resp.Error != "" {
				result.Error = fmt.Errorf("%s", resp.Error)
				result.Stacktrace = resp.Stacktrace
			}
			results <- result
		}
	}()

	return results, nil
}

func (c *GRPCClient) Health(ctx context.Context) (bool, error) {
	resp, err := c.client.Health(ctx, &pb.Empty{})
	if err != nil {
		return false, err
	}
	return resp.Healthy, nil
}

func (c *GRPCClient) GetMetrics(ctx context.Context) ([][]byte, error) {
	resp, err := c.client.GetMetrics(ctx, &pb.Empty{})
	if err != nil {
		return nil, err
	}
	return resp.MetricFamilies, nil
}

func (c *GRPCClient) TriggerGC(ctx context.Context) (*GCResult, error) {
	resp, err := c.client.TriggerGC(ctx, &pb.Empty{})
	if err != nil {
		return nil, err
	}
	return &GCResult{
		HeapAllocBefore: resp.HeapAllocBefore,
		HeapAllocAfter:  resp.HeapAllocAfter,
		HeapReleased:    resp.HeapReleased,
	}, nil
}

func (c *GRPCClient) Parse(ctx context.Context, methodName string, inputJSON []byte) (*ParseResult, error) {
	resp, err := c.client.Parse(ctx, &pb.ParseRequest{
		MethodName: methodName,
		InputJson:  inputJSON,
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, NewErrorWithStack(fmt.Errorf("%s", resp.Error), resp.Stacktrace)
	}
	return &ParseResult{Data: resp.DataJson}, nil
}

func (c *GRPCClient) GetGoroutines(ctx context.Context, filter string) (*GoroutinesResult, error) {
	resp, err := c.client.GetGoroutines(ctx, &pb.GetGoroutinesRequest{
		Filter: filter,
	})
	if err != nil {
		return nil, err
	}
	return &GoroutinesResult{
		TotalCount:    resp.TotalCount,
		MatchCount:    resp.MatchCount,
		MatchedStacks: resp.MatchedStacks,
	}, nil
}
