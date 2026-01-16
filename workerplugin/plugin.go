package workerplugin

import (
	"context"

	"github.com/hashicorp/go-plugin"
	"github.com/invakid404/baml-rest/bamlutils"
	"google.golang.org/grpc"

	pb "github.com/invakid404/baml-rest/workerplugin/proto"
)

// streamResultPool is a pool of StreamResult structs to reduce allocations.
var streamResultPool = bamlutils.NewPool(func() *StreamResult {
	return &StreamResult{}
})

// GetStreamResult retrieves a StreamResult from the pool.
// The struct is already zeroed by ReleaseStreamResult before being returned to pool.
func GetStreamResult() *StreamResult {
	return streamResultPool.Get()
}

// ReleaseStreamResult returns a StreamResult to the pool for reuse.
// After calling this, the StreamResult should not be accessed.
func ReleaseStreamResult(r *StreamResult) {
	if r == nil {
		return
	}
	// Reset entire struct before returning to pool to avoid memory retention
	// and ensure future-proofing if fields are added
	*r = StreamResult{}
	streamResultPool.Put(r)
}

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

// ErrorWithStack wraps an error with an optional stacktrace.
// Used to propagate stacktraces from worker panics through the pool to the server.
type ErrorWithStack struct {
	Err        error
	Stacktrace string
}

func (e *ErrorWithStack) Error() string {
	return e.Err.Error()
}

func (e *ErrorWithStack) Unwrap() error {
	return e.Err
}

// GetStacktrace returns the stacktrace, or empty string if none.
func (e *ErrorWithStack) GetStacktrace() string {
	return e.Stacktrace
}

// NewErrorWithStack creates an error that carries a stacktrace.
func NewErrorWithStack(err error, stacktrace string) error {
	if stacktrace == "" {
		return err
	}
	return &ErrorWithStack{Err: err, Stacktrace: stacktrace}
}

// StreamResult represents a streaming result from a BAML method
type StreamResult struct {
	Kind       StreamResultKind
	Data       []byte // JSON-encoded data
	Raw        string // Raw LLM response (populated on Final)
	Error      error  // Error (populated on Error kind)
	Stacktrace string // Stacktrace (populated on Error kind, when available)
	Reset      bool   // When true, client should discard accumulated state (retry occurred)
}

type StreamResultKind int

const (
	StreamResultKindStream StreamResultKind = iota
	StreamResultKindFinal
	StreamResultKindError
	StreamResultKindHeartbeat
)

// GCResult contains memory stats from a GC operation
type GCResult struct {
	HeapAllocBefore uint64
	HeapAllocAfter  uint64
	HeapReleased    uint64
}

// Worker is the interface that the plugin implements.
type Worker interface {
	// CallStream executes a BAML method and streams results.
	// Used for both streaming and non-streaming calls - non-streaming callers
	// simply wait for the final result while benefiting from hung detection.
	CallStream(ctx context.Context, methodName string, inputJSON []byte, enableRawCollection bool) (<-chan *StreamResult, error)

	// Health checks if the worker is healthy
	Health(ctx context.Context) (bool, error)

	// GetMetrics returns Prometheus metrics from the worker process.
	// Returns serialized prometheus MetricFamily protos.
	GetMetrics(ctx context.Context) ([][]byte, error)

	// TriggerGC forces garbage collection and releases memory to OS
	TriggerGC(ctx context.Context) (*GCResult, error)
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
