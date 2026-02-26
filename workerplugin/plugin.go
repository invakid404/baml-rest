package workerplugin

import (
	"context"
	"log"
	"runtime"
	"time"

	"github.com/hashicorp/go-plugin"
	"github.com/invakid404/baml-rest/bamlutils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

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

// ParseResult represents the result of parsing raw LLM output
type ParseResult struct {
	Data []byte // JSON-encoded parsed result
}

// GoroutinesResult contains goroutine pprof data from a worker
type GoroutinesResult struct {
	TotalCount    int32    // Total number of goroutines
	MatchCount    int32    // Number of goroutines matching filter
	MatchedStacks []string // Stack traces of matching goroutines
}

// Worker is the interface that the plugin implements.
type Worker interface {
	// CallStream executes a BAML method and streams results.
	// Used for both streaming and non-streaming calls - non-streaming callers
	// simply wait for the final result while benefiting from hung detection.
	CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *StreamResult, error)

	// Health checks if the worker is healthy
	Health(ctx context.Context) (bool, error)

	// GetMetrics returns Prometheus metrics from the worker process.
	// Returns serialized prometheus MetricFamily protos.
	GetMetrics(ctx context.Context) ([][]byte, error)

	// TriggerGC forces garbage collection and releases memory to OS
	TriggerGC(ctx context.Context) (*GCResult, error)

	// Parse parses raw LLM output into structured data using a BAML method's schema
	Parse(ctx context.Context, methodName string, inputJSON []byte) (*ParseResult, error)

	// GetGoroutines returns goroutine pprof data from the worker process
	// filter is a comma-separated list of patterns to match (case-insensitive)
	GetGoroutines(ctx context.Context, filter string) (*GoroutinesResult, error)
}

// WorkerPlugin is the implementation of plugin.GRPCPlugin
type WorkerPlugin struct {
	plugin.Plugin
	// Impl is the concrete implementation, used by the server side
	Impl Worker
}

const (
	// Number of retries per extra brokered connection dial (in addition to
	// the initial attempt). This smooths over transient broker startup races.
	extraGRPCDialRetries = 2
	// Linear backoff base between retries.
	extraGRPCDialRetryBackoff = 100 * time.Millisecond
)

func (p *WorkerPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	impl := &GRPCServer{Impl: p.Impl}
	pb.RegisterWorkerServer(s, impl)

	// Start extra gRPC servers via the broker for multi-connection support.
	// Each brokered connection gets its own HTTP/2 transport, preventing
	// head-of-line blocking when many concurrent RPCs share a connection.
	for i := uint32(1); i <= ExtraGRPCConns; i++ {
		id := i
		go broker.AcceptAndServe(id, func(opts []grpc.ServerOption) *grpc.Server {
			opts = append(opts, GRPCServerOptions()...)
			srv := grpc.NewServer(opts...)
			pb.RegisterWorkerServer(srv, impl)
			return srv
		})
	}

	return nil
}

// brokerDialResult collects the outcome of a single broker dial goroutine.
type brokerDialResult struct {
	id   uint32
	conn *grpc.ClientConn
	err  error
}

func (p *WorkerPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	primary := &GRPCClient{client: pb.NewWorkerClient(c)}

	allWorkers := []Worker{primary}
	var conns []*grpc.ClientConn

	// Extra connections are best-effort — the primary connection is always
	// sufficient. If a broker dial fails (transient timeout, etc.), we
	// continue with however many connections succeeded.
	//
	// Dials run in parallel so a single slow/stuck broker stream doesn't
	// delay the others. Worst-case latency is max(per-conn) instead of
	// sum(per-conn).
	results := make(chan brokerDialResult, ExtraGRPCConns)
	for i := uint32(1); i <= ExtraGRPCConns; i++ {
		go func(id uint32) {
			conn, err := dialBrokerConnWithRetry(ctx, broker, id)
			results <- brokerDialResult{id: id, conn: conn, err: err}
		}(i)
	}

	var connected int
	for range ExtraGRPCConns {
		r := <-results
		if r.err != nil {
			if ctx != nil && ctx.Err() != nil {
				// Context cancelled — clean up any connections we already
				// collected before returning.
				for _, c := range conns {
					c.Close()
				}
				return nil, ctx.Err()
			}
			log.Printf("[WARN] failed to dial extra gRPC connection %d after %d attempts: %v", r.id, extraGRPCDialRetries+1, r.err)
			continue
		}
		conns = append(conns, r.conn)
		allWorkers = append(allWorkers, NewGRPCClient(r.conn))
		connected++
	}

	if connected < ExtraGRPCConns {
		log.Printf("[INFO] using %d/%d extra gRPC connections", connected, ExtraGRPCConns)
	}

	return &multiConnWorker{
		workers: allWorkers,
		conns:   conns,
	}, nil
}

func dialBrokerConnWithRetry(ctx context.Context, broker *plugin.GRPCBroker, id uint32) (*grpc.ClientConn, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	var lastErr error
	for attempt := 0; attempt <= extraGRPCDialRetries; attempt++ {
		conn, err := broker.DialWithOptions(id, GRPCDialOptions()...)
		if err == nil {
			return conn, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt == extraGRPCDialRetries {
			break
		}

		backoff := time.Duration(attempt+1) * extraGRPCDialRetryBackoff
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}

	return nil, lastErr
}

// GRPCDialOptions returns gRPC dial options shared by all connections
// (primary go-plugin connection and brokered extra connections).
func GRPCDialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second, // Ping after 30s idle — detect dead connections quickly
			Timeout:             10 * time.Second, // Wait 10s for ping ack
			PermitWithoutStream: true,             // Keep pinging between request bursts
		}),
		grpc.WithInitialWindowSize(4 << 20),     // 4 MB per-stream flow control window (default 64KB)
		grpc.WithInitialConnWindowSize(4 << 20), // 4 MB per-connection flow control window (default 64KB)
	}
}

// GRPCServerOptions returns gRPC server options shared by all connections
// (primary go-plugin server and brokered extra servers).
func GRPCServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second, // Must be <= client Time
			PermitWithoutStream: true,
		}),
		grpc.NumStreamWorkers(uint32(runtime.NumCPU())),
		grpc.InitialWindowSize(4 << 20),     // 4 MB per-stream flow control window (default 64KB)
		grpc.InitialConnWindowSize(4 << 20), // 4 MB per-connection flow control window (default 64KB)
	}
}
