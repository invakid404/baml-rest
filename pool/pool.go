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
	mu          sync.Mutex
	healthy     bool
	lastUsed    time.Time
	inFlightMu  sync.RWMutex
	inFlightReq map[uint64]*inFlightRequest
}

// New creates a new worker pool
func New(config *Config) (*Pool, error) {
	if config.WorkerPath == "" {
		return nil, fmt.Errorf("WorkerPath is required")
	}
	if config.PoolSize <= 0 {
		config.PoolSize = 4
	}
	if config.LogOutput == nil {
		config.LogOutput = os.Stdout
	}

	// Create logger with appropriate output format
	var output io.Writer = config.LogOutput
	if config.PrettyLogs {
		output = zerolog.ConsoleWriter{
			Out: config.LogOutput,
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
	})

	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("failed to create RPC client: %w", err)
	}

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
	workerLogger.Info().Msg("Started worker")

	return &workerHandle{
		id:          id,
		logger:      workerLogger,
		client:      client,
		worker:      worker,
		healthy:     true,
		lastUsed:    time.Now(),
		inFlightReq: make(map[uint64]*inFlightRequest),
	}, nil
}

func (p *Pool) healthChecker() {
	defer p.wg.Done()

	healthTicker := time.NewTicker(p.config.HealthCheckInterval)
	defer healthTicker.Stop()

	// Check for hung requests more frequently
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

// killWorkerAndRetry kills a worker and cancels all its in-flight requests
func (p *Pool) killWorkerAndRetry(handle *workerHandle) {
	handle.mu.Lock()
	handle.healthy = false
	handle.mu.Unlock()

	// Kill the process immediately - don't wait for graceful shutdown
	// This is critical for truly hung workers that won't respond to context cancellation
	handle.client.Kill()

	// Cancel all in-flight requests - they will retry on other workers
	handle.inFlightMu.Lock()
	for _, req := range handle.inFlightReq {
		if req.cancel != nil {
			req.cancel()
		}
	}
	handle.inFlightMu.Unlock()

	// Start a new worker to replace the killed one
	go p.restartWorker(handle.id)
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
			handle.healthy = false
			handle.logger.Warn().Err(err).Msg("Worker unhealthy, restarting")
			handle.mu.Unlock()

			workerRestarts.Inc(workerRestartLabels{Reason: RestartReasonUnhealthy})
			p.restartWorker(handle.id)
		} else {
			handle.healthy = true
			handle.mu.Unlock()
		}
	}
}

func (p *Pool) restartWorker(id int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed.Load() {
		return
	}

	oldHandle := p.workers[id]
	if oldHandle != nil {
		oldHandle.client.Kill()
	}

	newHandle, err := p.startWorker(id)
	if err != nil {
		p.logger.Error().Int("worker", id).Err(err).Msg("Failed to restart worker")
		return
	}

	p.workers[id] = newHandle
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
func (p *Pool) Parse(ctx context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error) {
	handle, err := p.getWorker()
	if err != nil {
		return nil, err
	}

	handle.mu.Lock()
	handle.lastUsed = time.Now()
	handle.mu.Unlock()

	return handle.worker.Parse(ctx, methodName, inputJSON)
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

// CallStream executes a BAML method and streams results
func (p *Pool) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
	var lastErr error

	for attempt := 0; attempt <= p.config.MaxRetries; attempt++ {
		handle, err := p.getWorker()
		if err != nil {
			return nil, err
		}

		// Create cancellable context for this attempt
		attemptCtx, cancel := context.WithCancel(ctx)
		req, cleanup := p.trackRequest(handle, cancel)

		handle.mu.Lock()
		handle.lastUsed = time.Now()
		handle.mu.Unlock()

		results, err := handle.worker.CallStream(attemptCtx, methodName, inputJSON, streamMode)
		if err != nil {
			cleanup()
			lastErr = err

			// Check if the parent context is cancelled (user cancelled)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			// If attempt context was cancelled but parent wasn't, worker was killed - retry
			if attemptCtx.Err() != nil {
				handle.logger.Info().Int("attempt", attempt+1).Msg("Retrying stream after worker kill")
				continue
			}

			// Regular error - mark worker unhealthy and retry
			handle.logger.Warn().Int("attempt", attempt+1).Err(err).Msg("Worker stream call failed")

			handle.mu.Lock()
			handle.healthy = false
			handle.mu.Unlock()

			go p.restartWorker(handle.id)
			continue
		}

		// Got a results channel - wrap it to track first byte and cleanup
		wrappedResults := make(chan *workerplugin.StreamResult)
		go func() {
			defer close(wrappedResults)
			defer cleanup()

			firstByte := false
			for result := range results {
				if !firstByte {
					firstByte = true
					req.gotFirstByte.Store(true)
				}

				// Filter out heartbeat - it's only for first-byte tracking, not for consumers
				if result.Kind == workerplugin.StreamResultKindHeartbeat {
					workerplugin.ReleaseStreamResult(result)
					continue
				}

				select {
				case wrappedResults <- result:
				case <-ctx.Done():
					// Consumer cancelled, drain remaining results to unblock gRPC goroutine
					// Release the current result that wasn't sent
					workerplugin.ReleaseStreamResult(result)
					go func() {
						for r := range results {
							workerplugin.ReleaseStreamResult(r)
						}
					}()
					return
				}

				// FINAL and ERROR are terminal - drain any remaining results
				// This prevents blocking if the gRPC stream sends more after terminal
				if result.Kind == workerplugin.StreamResultKindFinal || result.Kind == workerplugin.StreamResultKindError {
					go func() {
						for r := range results {
							workerplugin.ReleaseStreamResult(r)
						}
					}()
					return
				}
			}
		}()

		return wrappedResults, nil
	}

	return nil, fmt.Errorf("all stream retries failed: %w", lastErr)
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
			handle.client.Kill()
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
	WorkerID      int
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
