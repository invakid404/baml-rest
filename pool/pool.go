package pool

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/metrics"
	"github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

var tokioWorkerThreads = fmt.Sprintf("TOKIO_WORKER_THREADS=%d", runtime.NumCPU()*2)

// WorkerRestartReason represents why a worker was restarted
type WorkerRestartReason string

const (
	RestartReasonHung      WorkerRestartReason = "hung"
	RestartReasonUnhealthy WorkerRestartReason = "unhealthy"
)

type workerRestartLabels struct {
	Reason WorkerRestartReason `label:"reason"`
}

var workerRestarts = metrics.CounterWith[workerRestartLabels](
	"worker_restarts_total",
	"Total number of worker restarts",
)

// priorityFields are fields to show before the message in pretty mode
var priorityFields = [...]string{"worker", "hclog_name"}

// formatPrepareWithPriorityFields moves priority fields before the message
func formatPrepareWithPriorityFields(m map[string]interface{}) error {
	// Fast path: check if any priority fields exist
	var count int
	for i := range priorityFields {
		if _, ok := m[priorityFields[i]]; ok {
			count++
		}
	}
	if count == 0 {
		return nil
	}

	msg, _ := m[zerolog.MessageFieldName].(string)

	var b strings.Builder
	b.Grow(count*20 + len(msg))

	for i := range priorityFields {
		if v, ok := m[priorityFields[i]]; ok {
			b.WriteString(priorityFields[i])
			b.WriteByte('=')
			fmt.Fprint(&b, v)
			b.WriteByte(' ')
			delete(m, priorityFields[i])
		}
	}
	b.WriteString(msg)
	m[zerolog.MessageFieldName] = b.String()
	return nil
}

// Config holds the configuration for the worker pool
type Config struct {
	// PoolSize is the number of workers to maintain
	PoolSize int
	// MaxRetries is the maximum number of retries for a failed call
	MaxRetries int
	// HealthCheckInterval is how often to check worker health
	HealthCheckInterval time.Duration
	// FirstByteTimeout is how long to wait for first response before considering worker hung
	FirstByteTimeout time.Duration
	// LogOutput is the output writer for logs (defaults to os.Stdout)
	LogOutput io.Writer
	// PrettyLogs enables human-readable console logging
	PrettyLogs bool
	// WorkerPath is the path to the worker binary (required)
	WorkerPath string
	// WorkerMemLimit is the GOMEMLIMIT for each worker process (0 = no limit)
	WorkerMemLimit int64
}

// DefaultConfig returns a default configuration
func DefaultConfig() *Config {
	return &Config{
		PoolSize:            4,
		MaxRetries:          2,
		HealthCheckInterval: 10 * time.Second,
		FirstByteTimeout:    120 * time.Second,
	}
}

// inFlightRequest tracks an active request on a worker
type inFlightRequest struct {
	id           uint64
	startedAt    time.Time
	gotFirstByte atomic.Bool
	cancel       context.CancelFunc
}

// inFlightRequestPool reduces allocations for request tracking
var inFlightRequestPool = bamlutils.NewPool(func() *inFlightRequest {
	return &inFlightRequest{}
})

// trackRequest registers an in-flight request and returns the request and a cleanup function.
// The cleanup function unregisters the request, cancels the context, and returns the request to the pool.
func (p *Pool) trackRequest(handle *workerHandle, cancel context.CancelFunc) (*inFlightRequest, func()) {
	reqID := p.requestID.Add(1)

	req := inFlightRequestPool.Get()
	req.id = reqID
	req.startedAt = time.Now()
	req.gotFirstByte.Store(false)
	req.cancel = cancel

	handle.inFlightMu.Lock()
	handle.inFlightReq[reqID] = req
	handle.inFlightMu.Unlock()

	cleanup := func() {
		handle.inFlightMu.Lock()
		delete(handle.inFlightReq, reqID)
		handle.inFlightMu.Unlock()
		cancel()
		*req = inFlightRequest{}
		inFlightRequestPool.Put(req)
	}

	return req, cleanup
}

// Pool manages a pool of worker processes
type Pool struct {
	config    *Config
	logger    zerolog.Logger
	workers   []*workerHandle
	mu        sync.RWMutex
	next      atomic.Uint64
	requestID atomic.Uint64
	closed    atomic.Bool
	draining  atomic.Bool
	wg        sync.WaitGroup
	done      chan struct{}
}

type workerHandle struct {
	id          int
	logger      zerolog.Logger // pre-bound with worker id
	client      *plugin.Client
	worker      workerplugin.Worker
	mu          sync.RWMutex
	healthy     bool
	restarting  atomic.Bool // CAS guard: only one goroutine starts a replacement
	restartMu   sync.Mutex  // held by waiters checking restarting
	restartCond *sync.Cond  // signaled when restart completes (success or failure)
	lastUsed    time.Time
	inFlightMu  sync.RWMutex
	inFlightReq map[uint64]*inFlightRequest
}

// kill closes brokered gRPC connections and kills the plugin process.
func (h *workerHandle) kill() {
	if closer, ok := h.worker.(io.Closer); ok {
		closer.Close()
	}
	h.client.Kill()
}

// New creates a new worker pool
func New(config *Config) (*Pool, error) {
	if config.WorkerPath == "" {
		return nil, fmt.Errorf("WorkerPath is required")
	}
	if config.PoolSize <= 0 {
		config.PoolSize = 1
	}
	if config.LogOutput == nil {
		config.LogOutput = os.Stdout
	}

	// Create logger with appropriate output format
	var output io.Writer = config.LogOutput
	if config.PrettyLogs {
		output = zerolog.ConsoleWriter{
			Out:           config.LogOutput,
			FormatPrepare: formatPrepareWithPriorityFields,
		}
	}
	logger := zerolog.New(output).With().Timestamp().Logger()

	p := &Pool{
		config:  config,
		logger:  logger,
		workers: make([]*workerHandle, config.PoolSize),
		done:    make(chan struct{}),
	}

	// Initialize worker restart counter labels
	workerRestarts.Add(0, workerRestartLabels{Reason: RestartReasonHung})
	workerRestarts.Add(0, workerRestartLabels{Reason: RestartReasonUnhealthy})

	// Start workers
	for i := 0; i < config.PoolSize; i++ {
		handle, err := p.startWorker(i)
		if err != nil {
			// Clean up already started workers
			p.Close()
			return nil, fmt.Errorf("failed to start worker %d: %w", i, err)
		}
		p.workers[i] = handle
	}

	// Start health checker
	if config.HealthCheckInterval > 0 {
		p.wg.Add(1)
		go p.healthChecker()
	}

	return p, nil
}

func (p *Pool) startWorker(id int) (*workerHandle, error) {
	cmd := exec.Command(p.config.WorkerPath)

	cmd.Env = append(os.Environ(), tokioWorkerThreads)

	// Set GOMEMLIMIT for worker if configured
	if p.config.WorkerMemLimit > 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("GOMEMLIMIT=%d", p.config.WorkerMemLimit))
	}

	// If BAML logging is enabled (not "off"), configure LSP mode so logs go through
	// go-plugin's hclog instead of stdout (which would corrupt the gRPC protocol)
	if bamlLog := os.Getenv("BAML_LOG"); bamlLog != "" && bamlLog != "off" {
		cmd.Env = append(cmd.Env, "BAML_LOG_LSP=true", "BAML_INTERNAL_LOG=info")
	}

	// Create a zerolog-backed hclog for plugin communication
	pluginLogger := newHclogZerolog(p.logger.With().Int("worker", id).Logger())

	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: workerplugin.Handshake,
		Plugins:         workerplugin.PluginMap,
		Cmd:             cmd,
		AllowedProtocols: []plugin.Protocol{
			plugin.ProtocolGRPC,
		},
		Logger: pluginLogger,
		GRPCDialOptions: []grpc.DialOption{
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:                30 * time.Second, // Ping after 30s idle — detect dead connections quickly
				Timeout:             10 * time.Second, // Wait 10s for ping ack
				PermitWithoutStream: true,             // Keep pinging between request bursts
			}),
		},
	})

	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("failed to create RPC client: %w", err)
	}

	// Dispense returns a multiConnWorker (via GRPCClient) that distributes
	// RPCs across 1 primary + N brokered gRPC connections, preventing
	// HTTP/2 head-of-line blocking at high concurrency.
	raw, err := rpcClient.Dispense("worker")
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("failed to dispense worker plugin: %w", err)
	}

	worker, ok := raw.(workerplugin.Worker)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("unexpected plugin type: %T", raw)
	}

	workerLogger := p.logger.With().Int("worker", id).Logger()
	workerLogger.Info().
		Int("grpc_conns_target", 1+workerplugin.ExtraGRPCConns).
		Msg("Started worker")

	h := &workerHandle{
		id:          id,
		logger:      workerLogger,
		client:      client,
		worker:      worker,
		healthy:     true,
		lastUsed:    time.Now(),
		inFlightReq: make(map[uint64]*inFlightRequest),
	}
	h.restartCond = sync.NewCond(&h.restartMu)
	return h, nil
}

func (p *Pool) healthChecker() {
	defer p.wg.Done()

	healthTicker := time.NewTicker(p.config.HealthCheckInterval)
	defer healthTicker.Stop()

	// Check for hung requests more frequently than health checks.
	// Safe at any pool size thanks to hot-swap restarts (the replacement
	// worker is started before the hung one is killed).
	hungTicker := time.NewTicker(time.Second)
	defer hungTicker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-hungTicker.C:
			if p.closed.Load() {
				return
			}
			p.checkHungRequests()
		case <-healthTicker.C:
			if p.closed.Load() {
				return
			}
			p.checkHealth()
		}
	}
}

// checkHungRequests looks for requests waiting too long for first byte
func (p *Pool) checkHungRequests() {
	p.mu.RLock()
	workers := p.workers
	p.mu.RUnlock()

	now := time.Now()
	for _, handle := range workers {
		if handle == nil {
			continue
		}

		handle.inFlightMu.RLock()
		var hungRequest *inFlightRequest
		for _, req := range handle.inFlightReq {
			if !req.gotFirstByte.Load() && now.Sub(req.startedAt) > p.config.FirstByteTimeout {
				hungRequest = req
				break
			}
		}
		handle.inFlightMu.RUnlock()

		if hungRequest != nil {
			handle.logger.Warn().
				Uint64("request_id", hungRequest.id).
				Dur("waited", now.Sub(hungRequest.startedAt)).
				Msg("Worker appears hung, killing")
			workerRestarts.Inc(workerRestartLabels{Reason: RestartReasonHung})
			p.killWorkerAndRetry(handle)
		}
	}
}

// killWorkerAndRetry replaces a broken worker via hot-swap and then cancels
// its in-flight requests so they can retry on the fresh replacement.
//
// The hot-swap inside restartWorker ensures the slot is never empty: the
// replacement is started first, swapped into the slot, and only then is the
// old process killed. In-flight requests are cancelled *after* the swap so
// their retries immediately find the new worker via getWorker().
func (p *Pool) killWorkerAndRetry(handle *workerHandle) {
	// NOTE: we intentionally do NOT mark the handle unhealthy here.
	// The old worker stays "healthy" for routing so getWorker() never
	// sees zero healthy workers. restartWorker atomically swaps the
	// new worker in and kills the old process; in-flight gRPC calls
	// on the old handle will fail with Unavailable and get retried.

	// Hot-swap: starts replacement, swaps into slot, kills old process.
	// This is synchronous so that by the time we cancel in-flight requests
	// below, the new worker is already serving.
	p.restartWorker(handle.id, handle)

	// Cancel all in-flight requests on the OLD handle. The old process is
	// already dead (killed inside restartWorker), so these contexts just
	// unblock the gRPC recv loops. The retry logic in CallStream will call
	// getWorker() and find the new healthy worker.
	handle.inFlightMu.Lock()
	for _, req := range handle.inFlightReq {
		if req.cancel != nil {
			req.cancel()
		}
	}
	handle.inFlightMu.Unlock()
}

func (p *Pool) checkHealth() {
	p.mu.RLock()
	workers := p.workers
	p.mu.RUnlock()

	for _, handle := range workers {
		if handle == nil {
			continue
		}

		handle.mu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		healthy, err := handle.worker.Health(ctx)
		cancel()

		if err != nil || !healthy {
			handle.logger.Warn().Err(err).Msg("Worker unhealthy, restarting")
			handle.mu.Unlock()

			workerRestarts.Inc(workerRestartLabels{Reason: RestartReasonUnhealthy})
			p.restartWorker(handle.id, handle)
		} else {
			handle.healthy = true
			handle.mu.Unlock()
		}
	}
}

// restartWorker replaces the worker at the given slot using a hot-swap
// strategy: the replacement is started *before* the old worker is killed,
// so the slot is never empty and getWorker() always finds a live worker.
//
// The caller must pass the handle it observed the failure on; if the slot
// already holds a different handle (i.e. a concurrent restart already
// completed), the call is a no-op. This prevents a thundering-herd of
// restart calls from repeatedly killing freshly-started workers.
func (p *Pool) restartWorker(id int, failed *workerHandle) {
	// CAS guard: only one goroutine spawns a replacement process per worker.
	// Without this, N concurrent retries (e.g. 100 in-flight requests that
	// all hit gRPC Unavailable) would each call startWorker and spin up a
	// new OS process, only to have N-1 of them killed by the thundering-herd
	// check below. On resource-constrained CI runners this is enough to
	// stall the whole system.
	//
	// If another goroutine is already restarting, we wait for it to finish
	// rather than returning immediately. This prevents retry loops from
	// burning through MaxRetries against the same failing handle while a
	// replacement is booting.
	if !failed.restarting.CompareAndSwap(false, true) {
		// Wait for the in-progress restart to complete. The Cond.Wait()
		// loop re-checks the predicate under the lock, so there is no
		// TOCTOU race: even if a new restart begins between Broadcast
		// and our re-check, we simply wait again.
		failed.restartMu.Lock()
		for failed.restarting.Load() {
			failed.restartCond.Wait()
		}
		failed.restartMu.Unlock()
		return
	}

	// Start replacement BEFORE acquiring the lock or killing anything.
	// The old worker stays in the slot (possibly struggling, but better
	// than nothing) while the new one boots up (~1-2s for process start
	// and gRPC handshake).
	newHandle, err := p.startWorker(id)
	if err != nil {
		// Mark the failed worker unhealthy so getWorker stops routing to it.
		// Without this, requests keep hitting a known-bad worker and
		// triggering restart storms until a later attempt succeeds.
		p.mu.Lock()
		if p.workers[id] == failed {
			failed.mu.Lock()
			failed.healthy = false
			failed.mu.Unlock()
		}
		p.mu.Unlock()

		// Clear CAS and wake waiters so they can proceed (they'll find
		// the worker unhealthy and fail gracefully). The mutex must be
		// held when storing + broadcasting to avoid missed wakeups: a
		// waiter that checked the predicate but hasn't called Wait()
		// yet would miss a bare Broadcast.
		failed.restartMu.Lock()
		failed.restarting.Store(false)
		failed.restartCond.Broadcast()
		failed.restartMu.Unlock()

		p.logger.Error().Int("worker", id).Err(err).Msg("Failed to start replacement worker, marked unhealthy")
		return
	}

	p.mu.Lock()

	if p.closed.Load() {
		p.mu.Unlock()
		newHandle.kill()
		failed.restartMu.Lock()
		failed.restarting.Store(false)
		failed.restartCond.Broadcast()
		failed.restartMu.Unlock()
		return
	}

	// Another goroutine already restarted this worker — kill the one we
	// just started (wasteful but correct) and bail.
	if p.workers[id] != failed {
		p.mu.Unlock()
		newHandle.kill()
		failed.restartMu.Lock()
		failed.restarting.Store(false)
		failed.restartCond.Broadcast()
		failed.restartMu.Unlock()
		return
	}

	// Hot-swap: put new worker in the slot, then kill old.
	p.workers[id] = newHandle
	p.mu.Unlock()

	// Clear CAS and wake waiters. The mutex must be held around
	// Store + Broadcast to prevent missed wakeups (see waiter loop).
	failed.restartMu.Lock()
	failed.restarting.Store(false)
	failed.restartCond.Broadcast()
	failed.restartMu.Unlock()

	failed.kill()
}

// getWorker returns a worker using round-robin selection
func (p *Pool) getWorker() (*workerHandle, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.closed.Load() {
		return nil, fmt.Errorf("pool is closed")
	}

	if p.draining.Load() {
		return nil, fmt.Errorf("pool is draining, not accepting new requests")
	}

	// Round-robin selection
	start := p.next.Add(1) % uint64(len(p.workers))
	for i := 0; i < len(p.workers); i++ {
		idx := (start + uint64(i)) % uint64(len(p.workers))
		handle := p.workers[idx]
		if handle != nil && handle.healthy {
			return handle, nil
		}
	}

	return nil, fmt.Errorf("no healthy workers available")
}

// Parse parses raw LLM output using a BAML method's schema.
// Supports automatic retry on worker infrastructure failures (crashes, network issues).
func (p *Pool) Parse(ctx context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error) {
	var lastErr error

	for attempt := 0; attempt <= p.config.MaxRetries; attempt++ {
		handle, err := p.getWorker()
		if err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("retry failed, no workers available: %w (previous: %v)", err, lastErr)
			}
			return nil, err
		}

		handle.mu.Lock()
		handle.lastUsed = time.Now()
		handle.mu.Unlock()

		result, err := handle.worker.Parse(ctx, methodName, inputJSON)
		if err == nil {
			return result, nil
		}

		// Don't retry if the caller cancelled
		if ctx.Err() != nil {
			return nil, err
		}

		if isRetryableWorkerError(err) {
			handle.logger.Info().
				Int("attempt", attempt+1).
				Err(err).
				Msg("Retrying parse after worker failure")
			p.restartWorker(handle.id, handle)
			lastErr = err
			continue
		}

		// Non-retryable error
		return nil, err
	}

	return nil, fmt.Errorf("all parse retries exhausted: %w", lastErr)
}

// Call executes a BAML method and returns the final result.
// Internally uses CallStream to benefit from hung detection on first partial result.
func (p *Pool) Call(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (*workerplugin.CallResult, error) {
	results, err := p.CallStream(ctx, methodName, inputJSON, streamMode)
	if err != nil {
		return nil, err
	}

	// Consume stream waiting for final result
	for result := range results {
		switch result.Kind {
		case workerplugin.StreamResultKindError:
			err := workerplugin.NewErrorWithStack(result.Error, result.Stacktrace)
			workerplugin.ReleaseStreamResult(result)
			return nil, err
		case workerplugin.StreamResultKindFinal:
			callResult := &workerplugin.CallResult{
				Data: result.Data,
				Raw:  result.Raw,
			}
			workerplugin.ReleaseStreamResult(result)
			return callResult, nil
		}
		// StreamResultKindStream - continue waiting for final
		workerplugin.ReleaseStreamResult(result)
	}

	return nil, fmt.Errorf("no final result received")
}

// CallStream executes a BAML method and streams results.
// Supports automatic retry on worker infrastructure failures (crashes, network issues),
// both before first byte and mid-stream. On mid-stream retry, a reset message is injected
// so clients can discard accumulated partial state.
func (p *Pool) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
	// Verify we can get at least one worker before starting
	handle, err := p.getWorker()
	if err != nil {
		return nil, err
	}

	wrappedResults := make(chan *workerplugin.StreamResult)

	go func() {
		defer close(wrappedResults)

		currentHandle := handle
		var sentAnyResults bool

		for attempt := 0; attempt <= p.config.MaxRetries; attempt++ {
			// For retry attempts, get a new worker
			if attempt > 0 {
				var err error
				currentHandle, err = p.getWorker()
				if err != nil {
					errResult := workerplugin.GetStreamResult()
					errResult.Kind = workerplugin.StreamResultKindError
					errResult.Error = fmt.Errorf("retry failed, no workers available: %w", err)
					select {
					case wrappedResults <- errResult:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(errResult)
					}
					return
				}
			}

			// Create cancellable context for this attempt
			attemptCtx, cancel := context.WithCancel(ctx)
			req, cleanup := p.trackRequest(currentHandle, cancel)

			currentHandle.mu.Lock()
			currentHandle.lastUsed = time.Now()
			currentHandle.mu.Unlock()

			results, err := currentHandle.worker.CallStream(attemptCtx, methodName, inputJSON, streamMode)
			if err != nil {
				cleanup()

				// Check if parent context was cancelled (user cancelled)
				if ctx.Err() != nil {
					return
				}

				// If attempt context was cancelled (hung detection killed worker) or
				// this is a retryable worker error, retry with another worker
				if attemptCtx.Err() != nil || isRetryableWorkerError(err) {
					currentHandle.logger.Info().
						Int("attempt", attempt+1).
						Err(err).
						Msg("Retrying stream after worker failure")
					// Synchronous hot-swap: starts replacement, swaps into
					// slot, kills old process. The CAS guard ensures only one
					// goroutine spawns a replacement. We do NOT mark the old
					// handle unhealthy — it stays routable until the swap so
					// other callers never see zero healthy workers.
					p.restartWorker(currentHandle.id, currentHandle)
					continue
				}

				// Non-retryable error - send to client
				currentHandle.logger.Warn().
					Int("attempt", attempt+1).
					Err(err).
					Msg("Worker stream call failed (non-retryable)")
				errResult := workerplugin.GetStreamResult()
				errResult.Kind = workerplugin.StreamResultKindError
				errResult.Error = err
				select {
				case wrappedResults <- errResult:
				case <-ctx.Done():
					workerplugin.ReleaseStreamResult(errResult)
				}
				return
			}

			// Got results channel - forward results with retry support
			needsReset := sentAnyResults // Need to inject reset if this is a retry after sending data
			var injectedReset bool
			var shouldRetry bool

			for result := range results {
				// Mark first byte received (disables hung detection for this request)
				req.gotFirstByte.Store(true)

				// Filter heartbeat - only for first-byte tracking
				if result.Kind == workerplugin.StreamResultKindHeartbeat {
					workerplugin.ReleaseStreamResult(result)
					continue
				}

				// Check for retryable worker errors mid-stream
				if result.Kind == workerplugin.StreamResultKindError && isRetryableWorkerError(result.Error) {
					currentHandle.logger.Info().
						Int("attempt", attempt+1).
						Err(result.Error).
						Msg("Retryable error mid-stream, will retry")
					workerplugin.ReleaseStreamResult(result)
					shouldRetry = true
					break
				}

				// Inject reset message before first result after retry
				if needsReset && !injectedReset {
					resetResult := workerplugin.GetStreamResult()
					resetResult.Kind = workerplugin.StreamResultKindStream
					resetResult.Reset = true
					select {
					case wrappedResults <- resetResult:
						injectedReset = true
						currentHandle.logger.Info().Msg("Injected reset message for mid-stream retry")
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(resetResult)
						workerplugin.ReleaseStreamResult(result)
						drainResults(results)
						cleanup()
						return
					}
				}

				// Forward the result to the client
				select {
				case wrappedResults <- result:
					sentAnyResults = true
				case <-ctx.Done():
					workerplugin.ReleaseStreamResult(result)
					drainResults(results)
					cleanup()
					return
				}

				// Terminal states - we're done (successfully or with app error)
				if result.Kind == workerplugin.StreamResultKindFinal ||
					result.Kind == workerplugin.StreamResultKindError {
					drainResults(results)
					cleanup()
					return
				}
			}

			cleanup()

			// If we need to retry, trigger hot-swap and continue
			if shouldRetry {
				p.restartWorker(currentHandle.id, currentHandle)
				continue
			}

			// Channel closed without terminal - unexpected EOF, treat as retryable
			if attempt < p.config.MaxRetries {
				currentHandle.logger.Warn().
					Int("attempt", attempt+1).
					Msg("Stream ended unexpectedly without terminal result, retrying")
				p.restartWorker(currentHandle.id, currentHandle)
				continue
			}
		}

		// All retries exhausted
		errResult := workerplugin.GetStreamResult()
		errResult.Kind = workerplugin.StreamResultKindError
		errResult.Error = fmt.Errorf("all stream retries exhausted")
		select {
		case wrappedResults <- errResult:
		case <-ctx.Done():
			workerplugin.ReleaseStreamResult(errResult)
		}
	}()

	return wrappedResults, nil
}

// drainResults consumes remaining results from a channel to unblock the gRPC goroutine
func drainResults(results <-chan *workerplugin.StreamResult) {
	go func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	}()
}

// totalInFlight returns the total number of in-flight requests across all workers
func (p *Pool) totalInFlight() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var total int64
	for _, handle := range p.workers {
		if handle != nil {
			handle.inFlightMu.RLock()
			total += int64(len(handle.inFlightReq))
			handle.inFlightMu.RUnlock()
		}
	}
	return total
}

// KillWorkerResult contains information about a killed worker
type KillWorkerResult struct {
	WorkerID      int
	InFlightCount int
	GotFirstByte  []bool // For each in-flight request, whether it had received first byte
	Killed        bool
	Error         string
}

// KillWorkerWithInFlight finds a worker with in-flight requests and kills it.
// This is intended for testing scenarios where we need to simulate worker death mid-request.
// Returns information about the killed worker, or an error if no suitable worker was found.
func (p *Pool) KillWorkerWithInFlight() (*KillWorkerResult, error) {
	p.mu.RLock()
	workers := p.workers
	p.mu.RUnlock()

	// Find a worker with in-flight requests
	for _, handle := range workers {
		if handle == nil {
			continue
		}

		handle.inFlightMu.RLock()
		inFlightCount := len(handle.inFlightReq)
		var gotFirstByteList []bool
		for _, req := range handle.inFlightReq {
			gotFirstByteList = append(gotFirstByteList, req.gotFirstByte.Load())
		}
		handle.inFlightMu.RUnlock()

		if inFlightCount > 0 {
			// Found a worker with in-flight requests - kill it
			p.logger.Warn().
				Int("worker_id", handle.id).
				Int("in_flight_count", inFlightCount).
				Msg("DEBUG: Killing worker with in-flight requests")

			p.killWorkerAndRetry(handle)

			return &KillWorkerResult{
				WorkerID:      handle.id,
				InFlightCount: inFlightCount,
				GotFirstByte:  gotFirstByteList,
				Killed:        true,
			}, nil
		}
	}

	return nil, fmt.Errorf("no workers with in-flight requests found")
}

// SetFirstByteTimeout updates the timeout for hung detection.
// This is primarily intended for testing purposes.
func (p *Pool) SetFirstByteTimeout(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config.FirstByteTimeout = d
}

// GetFirstByteTimeout returns the current first byte timeout.
func (p *Pool) GetFirstByteTimeout() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.config.FirstByteTimeout
}

// WorkerInFlightInfo contains in-flight information for a single worker.
type WorkerInFlightInfo struct {
	WorkerID     int
	Healthy      bool
	InFlight     int
	GotFirstByte []bool
}

// GetInFlightStatus returns the in-flight status of all workers for debugging.
func (p *Pool) GetInFlightStatus() []WorkerInFlightInfo {
	p.mu.RLock()
	workers := p.workers
	p.mu.RUnlock()

	result := make([]WorkerInFlightInfo, 0, len(workers))
	for _, handle := range workers {
		if handle == nil {
			continue
		}

		handle.mu.RLock()
		healthy := handle.healthy
		handle.mu.RUnlock()

		handle.inFlightMu.RLock()
		inFlightCount := len(handle.inFlightReq)
		var gotFirstByteList []bool
		for _, req := range handle.inFlightReq {
			gotFirstByteList = append(gotFirstByteList, req.gotFirstByte.Load())
		}
		handle.inFlightMu.RUnlock()

		result = append(result, WorkerInFlightInfo{
			WorkerID:     handle.id,
			Healthy:      healthy,
			InFlight:     inFlightCount,
			GotFirstByte: gotFirstByteList,
		})
	}
	return result
}

// Shutdown gracefully shuts down the pool, waiting for in-flight requests to complete
func (p *Pool) Shutdown(ctx context.Context) error {
	// Start draining - reject new requests
	p.draining.Store(true)
	p.logger.Info().Msg("Pool draining, waiting for in-flight requests to complete")

	// Wait for in-flight requests to complete
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		inFlight := p.totalInFlight()
		if inFlight == 0 {
			p.logger.Info().Msg("All in-flight requests completed")
			break
		}

		select {
		case <-ctx.Done():
			p.logger.Warn().Int64("in_flight", inFlight).Msg("Shutdown timeout, killing workers with in-flight requests")
			return p.Close()
		case <-ticker.C:
			p.logger.Info().Int64("count", inFlight).Msg("Waiting for in-flight requests")
		}
	}

	return p.Close()
}

// Close immediately shuts down the pool and kills all workers
func (p *Pool) Close() error {
	if p.closed.Swap(true) {
		return nil // Already closed
	}

	p.draining.Store(true)

	// Signal health checker to stop
	close(p.done)

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, handle := range p.workers {
		if handle != nil {
			handle.kill()
		}
	}

	p.wg.Wait()
	p.logger.Info().Msg("Worker pool closed")
	return nil
}

// Stats returns pool statistics
type Stats struct {
	TotalWorkers   int
	HealthyWorkers int
	InFlight       int64
}

// isRetryableWorkerError returns true if the error indicates a worker infrastructure
// failure (crash, network issue) rather than an application-level error from BAML/LLM.
// These errors should trigger automatic retry on another worker.
func isRetryableWorkerError(err error) bool {
	if err == nil {
		return false
	}

	// First, try to extract gRPC status code (preferred method)
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable:
			// Worker died, network issue, or connection refused
			return true
		case codes.Canceled:
			// Request was canceled (e.g., by hung detection)
			return true
		}

		// Also check the status message for specific infrastructure errors
		msg := st.Message()
		if strings.Contains(msg, "connection reset") ||
			strings.Contains(msg, "error reading from server: EOF") ||
			strings.Contains(msg, "transport is closing") {
			return true
		}
	}

	// Fallback: string matching for wrapped errors or non-gRPC errors
	// This catches cases where the gRPC status couldn't be extracted
	errStr := err.Error()
	if strings.Contains(errStr, "code = Unavailable") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "error reading from server: EOF") ||
		strings.Contains(errStr, "transport is closing") {
		return true
	}

	return false
}

func (p *Pool) Stats() Stats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := Stats{
		TotalWorkers: len(p.workers),
	}

	for _, handle := range p.workers {
		if handle != nil {
			if handle.healthy {
				stats.HealthyWorkers++
			}
			handle.inFlightMu.RLock()
			stats.InFlight += int64(len(handle.inFlightReq))
			handle.inFlightMu.RUnlock()
		}
	}

	return stats
}

// WorkerMetrics holds metrics from a single worker
type WorkerMetrics struct {
	WorkerID       int
	MetricFamilies [][]byte
}

// GatherWorkerMetrics collects Prometheus metrics from all healthy workers.
// Returns a slice of worker metrics, one per worker.
func (p *Pool) GatherWorkerMetrics(ctx context.Context) []WorkerMetrics {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var results []WorkerMetrics

	for _, handle := range p.workers {
		if handle == nil || !handle.healthy {
			continue
		}

		// Use a short timeout for metrics collection
		metricsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		metricFamilies, err := handle.worker.GetMetrics(metricsCtx)
		cancel()

		if err != nil {
			p.logger.Warn().Int("worker", handle.id).Err(err).Msg("Failed to collect metrics from worker")
			continue
		}

		results = append(results, WorkerMetrics{
			WorkerID:       handle.id,
			MetricFamilies: metricFamilies,
		})
	}

	return results
}

// WorkerGCResult contains GC results for a single worker
type WorkerGCResult struct {
	WorkerID        int
	HeapAllocBefore uint64
	HeapAllocAfter  uint64
	HeapReleased    uint64
	Error           error
}

// TriggerAllWorkersGC triggers garbage collection on all healthy workers.
// Returns results for each worker.
func (p *Pool) TriggerAllWorkersGC(ctx context.Context) []WorkerGCResult {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var results []WorkerGCResult

	for _, handle := range p.workers {
		if handle == nil || !handle.healthy {
			continue
		}

		// Use a short timeout for GC
		gcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		gcResult, err := handle.worker.TriggerGC(gcCtx)
		cancel()

		result := WorkerGCResult{
			WorkerID: handle.id,
			Error:    err,
		}
		if gcResult != nil {
			result.HeapAllocBefore = gcResult.HeapAllocBefore
			result.HeapAllocAfter = gcResult.HeapAllocAfter
			result.HeapReleased = gcResult.HeapReleased
		}

		if err != nil {
			p.logger.Warn().Int("worker", handle.id).Err(err).Msg("Failed to trigger GC on worker")
		}

		results = append(results, result)
	}

	return results
}

// WorkerGoroutinesResult contains goroutine pprof data for a single worker
type WorkerGoroutinesResult struct {
	WorkerID      int
	TotalCount    int32
	MatchCount    int32
	MatchedStacks []string
	Error         error
}

// GetAllWorkersGoroutines fetches goroutine pprof data from all healthy workers.
// filter is a comma-separated list of patterns to match (case-insensitive).
// Returns results for each worker.
func (p *Pool) GetAllWorkersGoroutines(ctx context.Context, filter string) []WorkerGoroutinesResult {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var results []WorkerGoroutinesResult

	for _, handle := range p.workers {
		if handle == nil || !handle.healthy {
			continue
		}

		// Use a short timeout for getting goroutines
		goroutineCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		goroutineResult, err := handle.worker.GetGoroutines(goroutineCtx, filter)
		cancel()

		result := WorkerGoroutinesResult{
			WorkerID: handle.id,
			Error:    err,
		}
		if goroutineResult != nil {
			result.TotalCount = goroutineResult.TotalCount
			result.MatchCount = goroutineResult.MatchCount
			result.MatchedStacks = goroutineResult.MatchedStacks
		}

		if err != nil {
			p.logger.Warn().Int("worker", handle.id).Err(err).Msg("Failed to get goroutines from worker")
		}

		results = append(results, result)
	}

	return results
}
