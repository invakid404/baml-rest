package workerplugin

import (
	"context"
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// GRPCServer is the gRPC server that the plugin runs
type GRPCServer struct {
	pb.UnimplementedWorkerServer
	Impl Worker
}

func protoToRawCollectionMode(mode pb.RawCollectionMode) bamlutils.RawCollectionMode {
	switch mode {
	case pb.RawCollectionMode_RAW_COLLECTION_FINAL_ONLY:
		return bamlutils.RawCollectionFinalOnly
	case pb.RawCollectionMode_RAW_COLLECTION_ALL:
		return bamlutils.RawCollectionAll
	default:
		return bamlutils.RawCollectionNone
	}
}

func rawCollectionModeToProto(mode bamlutils.RawCollectionMode) pb.RawCollectionMode {
	switch mode {
	case bamlutils.RawCollectionFinalOnly:
		return pb.RawCollectionMode_RAW_COLLECTION_FINAL_ONLY
	case bamlutils.RawCollectionAll:
		return pb.RawCollectionMode_RAW_COLLECTION_ALL
	default:
		return pb.RawCollectionMode_RAW_COLLECTION_NONE
	}
}

func (s *GRPCServer) CallStream(req *pb.CallRequest, stream pb.Worker_CallStreamServer) error {
	rawCollectionMode := protoToRawCollectionMode(req.RawCollectionMode)
	results, err := s.Impl.CallStream(stream.Context(), req.MethodName, req.InputJson, rawCollectionMode)
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

// GRPCClient is the gRPC client that connects to the plugin
type GRPCClient struct {
	client pb.WorkerClient
}

func (c *GRPCClient) CallStream(ctx context.Context, methodName string, inputJSON []byte, rawCollectionMode bamlutils.RawCollectionMode) (<-chan *StreamResult, error) {
	stream, err := c.client.CallStream(ctx, &pb.CallRequest{
		MethodName:        methodName,
		InputJson:         inputJSON,
		RawCollectionMode: rawCollectionModeToProto(rawCollectionMode),
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
