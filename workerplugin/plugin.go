package workerplugin

import (
	"context"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// Handshake is used to verify the plugin is the expected plugin.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "BAML_WORKER_PLUGIN",
	MagicCookieValue: "baml-worker-v1",
}

// PluginMap is the map of plugins we can dispense.
var PluginMap = map[string]plugin.Plugin{
	"worker": &WorkerPlugin{},
}

// CallResult represents the result of a BAML method call
type CallResult struct {
	Data []byte // JSON-encoded result
	Raw  string // Raw LLM response
}

// StreamResult represents a streaming result from a BAML method
type StreamResult struct {
	Kind  StreamResultKind
	Data  []byte // JSON-encoded data
	Raw   string // Raw LLM response (populated on Final)
	Error error  // Error (populated on Error kind)
}

type StreamResultKind int

const (
	StreamResultKindStream StreamResultKind = iota
	StreamResultKindFinal
	StreamResultKindError
)

// Worker is the interface that the plugin implements.
type Worker interface {
	// Call executes a BAML method and returns the final result
	Call(ctx context.Context, methodName string, inputJSON, optionsJSON []byte, enableRawCollection bool) (*CallResult, error)

	// CallStream executes a BAML method and streams results
	CallStream(ctx context.Context, methodName string, inputJSON, optionsJSON []byte, enableRawCollection bool) (<-chan *StreamResult, error)

	// Health checks if the worker is healthy
	Health(ctx context.Context) (bool, error)
}

// WorkerPlugin is the implementation of plugin.GRPCPlugin
type WorkerPlugin struct {
	plugin.Plugin
	// Impl is the concrete implementation, used by the server side
	Impl Worker
}

func (p *WorkerPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterWorkerServer(s, &GRPCServer{Impl: p.Impl})
	return nil
}

func (p *WorkerPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &GRPCClient{client: pb.NewWorkerClient(c)}, nil
}
