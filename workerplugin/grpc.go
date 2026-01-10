package workerplugin

import (
	"context"
	"fmt"

	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// GRPCServer is the gRPC server that the plugin runs
type GRPCServer struct {
	pb.UnimplementedWorkerServer
	Impl Worker
}

func (s *GRPCServer) Call(ctx context.Context, req *pb.CallRequest) (*pb.CallResponse, error) {
	result, err := s.Impl.Call(ctx, req.MethodName, req.InputJson, req.OptionsJson, req.EnableRawCollection)
	if err != nil {
		return &pb.CallResponse{Error: err.Error()}, nil
	}
	return &pb.CallResponse{
		DataJson: result.Data,
		Raw:      result.Raw,
	}, nil
}

func (s *GRPCServer) CallStream(req *pb.CallRequest, stream pb.Worker_CallStreamServer) error {
	results, err := s.Impl.CallStream(stream.Context(), req.MethodName, req.InputJson, req.OptionsJson, req.EnableRawCollection)
	if err != nil {
		return err
	}

	for result := range results {
		pbResult := &pb.StreamResult{
			Kind:     pb.StreamResult_Kind(result.Kind),
			DataJson: result.Data,
			Raw:      result.Raw,
		}
		if result.Error != nil {
			pbResult.Error = result.Error.Error()
		}
		if err := stream.Send(pbResult); err != nil {
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

// GRPCClient is the gRPC client that connects to the plugin
type GRPCClient struct {
	client pb.WorkerClient
}

func (c *GRPCClient) Call(ctx context.Context, methodName string, inputJSON, optionsJSON []byte, enableRawCollection bool) (*CallResult, error) {
	resp, err := c.client.Call(ctx, &pb.CallRequest{
		MethodName:          methodName,
		InputJson:           inputJSON,
		OptionsJson:         optionsJSON,
		EnableRawCollection: enableRawCollection,
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return &CallResult{
		Data: resp.DataJson,
		Raw:  resp.Raw,
	}, nil
}

func (c *GRPCClient) CallStream(ctx context.Context, methodName string, inputJSON, optionsJSON []byte, enableRawCollection bool) (<-chan *StreamResult, error) {
	stream, err := c.client.CallStream(ctx, &pb.CallRequest{
		MethodName:          methodName,
		InputJson:           inputJSON,
		OptionsJson:         optionsJSON,
		EnableRawCollection: enableRawCollection,
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
				results <- &StreamResult{
					Kind:  StreamResultKindError,
					Error: err,
				}
				return
			}

			result := &StreamResult{
				Kind: StreamResultKind(resp.Kind),
				Data: resp.DataJson,
				Raw:  resp.Raw,
			}
			if resp.Error != "" {
				result.Error = fmt.Errorf("%s", resp.Error)
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
