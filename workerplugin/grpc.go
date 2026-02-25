package workerplugin

import (
	"context"
	"fmt"

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
}

func (s *GRPCServer) CallStream(req *pb.CallRequest, stream pb.Worker_CallStreamServer) error {
	results, err := s.Impl.CallStream(stream.Context(), req.MethodName, req.InputJson, pbToStreamMode(req.StreamMode))
	if err != nil {
		return err
	}

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
		// Release the StreamResult back to pool after Send completes
		ReleaseStreamResult(result)
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
	})
	if err != nil {
		return nil, err
	}

	results := make(chan *StreamResult)
	go func() {
		defer close(results)
		for {
			resp, err := stream.Recv()
			if err != nil {
				// EOF means stream ended normally
				if err.Error() == "EOF" {
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
