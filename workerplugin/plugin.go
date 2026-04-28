package workerplugin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
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
	// Planned and Outcome carry routing/retry metadata for the request.
	// Both BuildRequest and legacy paths now synthesize planned + outcome
	// events, so the bytes are typically populated for any successful call;
	// either may still be nil if no metadata plan was wired (e.g. tests
	// invoking the worker directly without a planner) or if the worker
	// errored before emitting the relevant event. Both are JSON-encoded
	// bamlutils.Metadata payloads forwarded verbatim from the worker.
	Planned []byte
	Outcome []byte
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
	StreamResultKindMetadata
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
	// SharedStateImpl hosts a pb.SharedStateServer on the plugin broker
	// so workers can dial back for cross-request round-robin counters.
	// Nil in standalone/test setups — the worker then keeps its in-process
	// Coordinator. Production is pool-managed and injects a store-backed
	// server via pool.Config.SharedStateSeeds.
	SharedStateImpl pb.SharedStateServer
	// SharedStateAttachTimeout bounds the initial AttachSharedState RPC
	// from the host to the worker. Non-positive (<= 0) uses the 10s
	// default — see the resolution at the call site (verdict-36 F3).
	SharedStateAttachTimeout time.Duration
	// AttachSharedState is set by the worker side in GRPCServer and
	// invoked on AttachSharedState requests. Keeps the plumbing in the
	// plugin package rather than spreading gRPC details into cmd/worker.
	onAttachSharedState func(ctx context.Context, client pb.SharedStateClient) error
}

// SetAttachSharedStateHandler installs the worker-side callback invoked
// when the host dials AttachSharedState. The callback receives a
// pb.SharedStateClient already connected to the host broker socket;
// the worker is expected to hold onto it for the lifetime of the process.
//
// This stays on WorkerPlugin (rather than hanging off the Worker
// interface) because the Worker interface is pure per-request RPC; the
// shared-state client is a process-level handle the worker gets once,
// at plugin-handshake time.
func (p *WorkerPlugin) SetAttachSharedStateHandler(fn func(ctx context.Context, client pb.SharedStateClient) error) {
	p.onAttachSharedState = fn
}

const (
	// Number of retries per extra brokered connection dial (in addition to
	// the initial attempt). This smooths over transient broker startup races.
	extraGRPCDialRetries = 2
	// Linear backoff base between retries.
	extraGRPCDialRetryBackoff = 100 * time.Millisecond
	// Per-attempt budget for a single broker.DialWithOptions call.
	// go-plugin's GRPCBroker.DialWithOptions takes no context and its
	// non-muxer dialer ignores any context-based cancellation
	// (see github.com/hashicorp/go-plugin v1.7.0 grpc_broker.go), so
	// without a wall-clock bound a stuck dial would pin the caller's
	// results-channel goroutine indefinitely. 5s is well above a
	// healthy local-broker dial (<10ms) but below the pool init wall
	// the parent ctx already enforces. CodeRabbit verdict-39 finding
	// F10; absolute-path scrub per verdict-41 finding F3.
	extraGRPCDialAttemptTimeout = 5 * time.Second
)

func (p *WorkerPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	impl := &GRPCServer{
		Impl:     p.Impl,
		broker:   broker,
		onAttach: p.onAttachSharedState,
	}
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
	// go-plugin does not contract ctx as non-nil across all call paths;
	// older consumers invoke GRPCClient with a nil context during
	// handshake, which would panic the later WithTimeout call. A nil ctx
	// has no deadline or cancellation to propagate — Background is the
	// correct replacement.
	if ctx == nil {
		ctx = context.Background()
	}
	primaryClient := pb.NewWorkerClient(c)
	primary := &GRPCClient{client: primaryClient}

	// Stand up the reverse shared-state server before returning, so the
	// worker side can dial back as soon as AttachSharedState arrives.
	// If SharedStateImpl is nil the host isn't hosting a store (e.g.
	// standalone harness) and we skip the reverse channel entirely; the
	// worker falls back to its in-process Coordinator.
	//
	// cleanupSharedState stops the reverse server AND closes its
	// listener on every error path between Accept and the successful
	// return. CodeRabbit verdict-25 finding F8: previously
	// AttachSharedState failure called srv.Stop() but left the
	// listener open, and the post-attach context-cancelled extra-
	// connection path (below) returned without cleaning up either.
	// Both leaked the reverse server and listener for the lifetime
	// of the host process. Successful return intentionally keeps the
	// reverse server alive — go-plugin teardown closes the listener.
	cleanupSharedState := func() {}
	if p.SharedStateImpl != nil {
		// broker.Accept publishes the id over the handshake stream
		// synchronously before returning a listener, so the worker's
		// subsequent Dial is guaranteed to find the id ready. No retry
		// loop needed.
		listener, err := broker.Accept(SharedStateBrokerID)
		if err != nil {
			return nil, fmt.Errorf("host: accept shared-state broker id=%d: %w", SharedStateBrokerID, err)
		}
		srv := grpc.NewServer(GRPCServerOptions()...)
		pb.RegisterSharedStateServer(srv, p.SharedStateImpl)
		cleanupSharedState = func() {
			srv.Stop()
			_ = listener.Close()
		}
		go func() {
			// Serve returns when listener is closed (plugin teardown).
			// Filter out the expected shutdown signals — without this,
			// every clean teardown emits a [WARN] line that operators
			// have to learn to ignore, training them to ignore real
			// problems too. CodeRabbit verdict-34 finding F6.
			err := srv.Serve(listener)
			if err == nil {
				return
			}
			if errors.Is(err, grpc.ErrServerStopped) ||
				errors.Is(err, net.ErrClosed) ||
				errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("[WARN] shared-state server: %v", err)
		}()

		// Resolve the attach timeout once: caller-supplied value if
		// positive, otherwise the 10s default. Both arms previously
		// wired the same parent ctx + cancel + defer pattern;
		// CodeRabbit verdict-36 finding F3 collapsed them.
		attachTimeout := p.SharedStateAttachTimeout
		if attachTimeout <= 0 {
			attachTimeout = 10 * time.Second
		}
		attachCtx, cancel := context.WithTimeout(ctx, attachTimeout)
		defer cancel()
		if _, err := primaryClient.AttachSharedState(attachCtx, &pb.AttachSharedStateRequest{
			BrokerId: SharedStateBrokerID,
		}); err != nil {
			cleanupSharedState()
			return nil, fmt.Errorf("host: AttachSharedState failed: %w", err)
		}
	}

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
	var received uint32
	for range ExtraGRPCConns {
		r := <-results
		received++
		if r.err != nil {
			if ctx != nil && ctx.Err() != nil {
				// Context cancelled — clean up any connections we
				// already collected AND tear down the reverse
				// shared-state server before returning. CodeRabbit
				// verdict-25 finding F8 + verdict-38 finding F9: drain
				// the remaining in-flight dials too, closing any
				// late-arriving successful conn so we don't leak the
				// FD on a goroutine that lost the cancellation race.
				for j := received; j < ExtraGRPCConns; j++ {
					late := <-results
					if late.err == nil {
						late.conn.Close()
					}
				}
				for _, c := range conns {
					c.Close()
				}
				cleanupSharedState()
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
		// CodeRabbit verdict-40 finding F4: short-circuit before
		// dialBrokerOnce when the parent ctx is already cancelled.
		// dialBrokerOnce itself selects on ctx.Done so this isn't a
		// correctness bug for the result channel — but spawning a
		// goroutine + drain path for a doomed dial is wasted work,
		// and avoids transiently holding broker resources during
		// teardown. The outer caller's "exactly one brokerDialResult
		// per pending dial" contract (see plugin.go ~line 327 and
		// the verdict-38 F9 cancellation drain) is preserved: this
		// branch returns once with the cancel error.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dialBrokerOnce(ctx, broker, id)
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

// dialBrokerOnce wraps a single broker.DialWithOptions call with a
// per-attempt timeout (extraGRPCDialAttemptTimeout) and parent-ctx
// cancellation. CodeRabbit verdict-39 finding F10: go-plugin's
// GRPCBroker.DialWithOptions has no context parameter and its
// non-muxer dialer ignores any context-based cancellation, so a stuck
// dial cannot be aborted by passing a child context. The helper
// goroutine pattern below lets us bound the wait without changing
// go-plugin's API: we kick off the dial on a background goroutine and
// race it against the per-attempt context.
//
// Goroutine-collapse design (CodeRabbit verdict-42): the dial
// goroutine itself owns late-success cleanup via a select on
// done <- res vs ctx.Done(). The `done` channel is UNBUFFERED — load-
// bearing for correctness. With a buffered channel, the inner send
// would always succeed (cap=1 is always ready for the first send),
// even after the outer select returned on ctx.Done(); the result
// would orphan in the channel and the conn would leak. The
// unbuffered channel pairs send with receive: if the outer takes
// the ctx.Done branch first, the inner's select observes the same
// ctx already cancelled and falls through to the cleanup arm —
// closing any successfully-dialled-but-late conn.
//
// dialBrokerConnWithRetry's outer caller writes exactly one
// brokerDialResult to the `results` channel for each connection ID
// (see plugin.go ~line 327), and the cancellation drain at
// plugin.go:340 depends on receiving exactly one result per pending
// dial. dialBrokerOnce ALWAYS returns either (conn, nil) or
// (nil, err) — never blocks indefinitely — preserving that invariant.
func dialBrokerOnce(parent context.Context, broker *plugin.GRPCBroker, id uint32) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(parent, extraGRPCDialAttemptTimeout)
	defer cancel()

	type result struct {
		conn *grpc.ClientConn
		err  error
	}
	done := make(chan result) // unbuffered — see doc comment above for the cleanup-correctness reason
	go func() {
		conn, err := broker.DialWithOptions(id, GRPCDialOptions()...)
		res := result{conn: conn, err: err}
		select {
		case done <- res:
		case <-ctx.Done():
			// Outer already returned on ctx.Done. Close any conn the
			// dial produced so we don't leak the FD on the late path.
			if err == nil && conn != nil {
				conn.Close()
			}
		}
	}()

	select {
	case r := <-done:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
