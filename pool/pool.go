package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/hashicorp/go-plugin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
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

var workerRestarts = newWorkerRestartCounter()

func newWorkerRestartCounter() *prometheus.CounterVec {
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "worker_restarts_total",
			Help: "Total number of worker restarts",
		},
		[]string{"reason"},
	)

	if err := prometheus.Register(counter); err != nil {
		if alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
		panic(err)
	}

	return counter
}

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
	// WorkerStartTimeout bounds worker startup and restart handshake work (0 = no timeout)
	WorkerStartTimeout time.Duration
	// SharedStateSeeds seeds per-key initial values for the host-side
	// SharedState counters. Populate from introspected.RoundRobinStart so
	// baml-roundrobin clients that declared a `start N` option honour it.
	// Keys not in the map start at a uniformly-random offset — same fresh-
	// fleet behaviour as the legacy in-process Coordinator.
	//
	// Leaving this nil disables the reverse shared-state channel: workers
	// never receive AttachSharedState and fall back to their in-process
	// coordinators. Intended for tests and single-worker configurations
	// where per-worker counters are acceptable.
	SharedStateSeeds map[string]int
}

// DefaultConfig returns a default configuration
func DefaultConfig() *Config {
	return &Config{
		PoolSize:            4,
		MaxRetries:          2,
		HealthCheckInterval: 10 * time.Second,
		FirstByteTimeout:    120 * time.Second,
		WorkerStartTimeout:  30 * time.Second,
	}
}

// inFlightRequest tracks an active request on a worker
type inFlightRequest struct {
	id           uint64
	startedAt    time.Time
	gotFirstByte atomic.Bool
	cancel       context.CancelFunc
}

type hungRequestSnapshot struct {
	id        uint64
	startedAt time.Time
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
	config             *Config
	logger             zerolog.Logger
	workers            []*workerHandle
	mu                 sync.RWMutex
	restartMu          sync.Mutex
	next               atomic.Uint64
	requestID          atomic.Uint64
	closed             atomic.Bool
	draining           atomic.Bool
	drainOnce          sync.Once
	drainCh            chan struct{} // closed when pool begins draining; selectable unlike atomic bool
	wg                 sync.WaitGroup
	restartWG          sync.WaitGroup
	done               chan struct{}
	shutdownCtx        context.Context                     // cancelled on Close(); health RPCs derive from this
	shutdownCancel     context.CancelFunc                  // cancels shutdownCtx
	newWorker          func(id int) (*workerHandle, error) // override for testing; nil in production
	beforeRestartStart func()

	// sharedStateStore hosts the per-key atomic counters that drive
	// cross-worker round-robin rotation. Nil when SharedStateSeeds was
	// not configured (test harness / single-worker setups). When non-nil,
	// every worker handshake dials back via AttachSharedState and
	// subsequent RR decisions ride through it.
	sharedStateStore *workerplugin.SharedStateStore
}

type workerHandle struct {
	id             int
	logger         zerolog.Logger // pre-bound with worker id
	client         *plugin.Client
	worker         workerplugin.Worker
	healthy        atomic.Bool
	restartPending atomic.Int32  // async restart dispatches published before restartDone is visible
	restarting     atomic.Bool   // CAS guard: only one goroutine starts a replacement
	restartMu      sync.Mutex    // protects restartDone; also used by restartCond
	restartCond    *sync.Cond    // signaled when restart completes (legacy CAS-loser path)
	restartDone    chan struct{} // closed when the current restart cycle completes; nil when idle
	inFlightMu     sync.RWMutex
	inFlightReq    map[uint64]*inFlightRequest
}

// kill closes brokered gRPC connections and kills the plugin process.
func (h *workerHandle) kill() {
	if closer, ok := h.worker.(io.Closer); ok {
		closer.Close()
	}
	if h.client != nil {
		h.client.Kill()
	}
}

// workerSnapshot returns a point-in-time copy of all non-nil worker handles.
// The returned handles are safe to use after the pool lock is released
// because handles are never freed — only replaced in their slot.
func (p *Pool) workerSnapshot() []*workerHandle {
	p.mu.RLock()
	defer p.mu.RUnlock()

	handles := make([]*workerHandle, 0, len(p.workers))
	for _, h := range p.workers {
		if h != nil {
			handles = append(handles, h)
		}
	}
	return handles
}

// finishRestart clears the CAS guard and wakes all goroutines waiting
// for this restart to complete. Must be called exactly once on every
// code path after a successful CompareAndSwap(false, true).
//
// The mutex is acquired internally so callers cannot forget it —
// this was the source of repeated missed-wakeup bugs when the
// Lock/Store/Broadcast/Unlock sequence was inlined at each call site.
func (h *workerHandle) finishRestart() {
	h.restartMu.Lock()
	h.restarting.Store(false)
	if h.restartDone != nil {
		close(h.restartDone)
		h.restartDone = nil
	}
	h.restartCond.Broadcast()
	h.restartMu.Unlock()
}

// snapshotRestartState returns the currently published restart-completion
// channel, if any, plus whether any restart work is visible for this handle.
// restartPending covers the async dispatch window before restartDone exists.
func snapshotRestartState(h *workerHandle) (chan struct{}, bool) {
	h.restartMu.Lock()
	defer h.restartMu.Unlock()

	ch := h.restartDone
	visible := h.restartPending.Load() > 0 || h.restarting.Load() || ch != nil
	return ch, visible
}

// New creates a new worker pool
func New(config *Config) (*Pool, error) {
	if config.WorkerPath == "" {
		return nil, fmt.Errorf("WorkerPath is required")
	}
	if config.PoolSize <= 0 {
		config.PoolSize = 4 // match DefaultConfig
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

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	p := &Pool{
		config:         config,
		logger:         logger,
		workers:        make([]*workerHandle, config.PoolSize),
		done:           make(chan struct{}),
		drainCh:        make(chan struct{}),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}

	// Build the shared-state store when seeds were configured. Workers
	// dial back to the host-side server registered inside WorkerPlugin
	// during GRPCClient handshake; see startWorker below for the
	// per-worker wiring.
	if config.SharedStateSeeds != nil {
		// Snapshot the caller's map so post-construction mutations can't
		// rewrite seeds mid-flight.
		//
		// Negative `start` values are clamped to zero. Upstream BAML
		// computes `(start as usize) % strategy.len()` — sign-extension
		// + bitcast, so -1i32 becomes 0xFFFFFFFFFFFFFFFF on 64-bit and
		// the modulo result depends on the chain length rather than
		// expressing a deliberate "wrap to last child" semantic. We
		// intentionally diverge by clamping to a safe deterministic
		// value rather than mirroring the cast artifact; matches the
		// in-process Coordinator's NewCoordinatorWithStarts contract.
		// See PR #192 cold-review-2 finding 3.
		seeds := make(map[string]uint64, len(config.SharedStateSeeds))
		for k, v := range config.SharedStateSeeds {
			if v < 0 {
				v = 0
			}
			seeds[k] = uint64(v)
		}
		p.sharedStateStore = workerplugin.NewSharedStateStore(func(key string) uint64 {
			if seed, ok := seeds[key]; ok {
				return seed
			}
			// Unseeded keys get a random offset so a fresh fleet does
			// not lockstep on child 0. Matches roundrobin.counterFor.
			return uint64(rand.Uint32())
		})
	}

	// Initialize worker restart counter labels
	workerRestarts.WithLabelValues(string(RestartReasonHung)).Add(0)
	workerRestarts.WithLabelValues(string(RestartReasonUnhealthy)).Add(0)

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

// pluginMap returns the go-plugin dispenser map for this pool. A fresh
// WorkerPlugin is constructed per call so each worker connection carries
// a plugin instance whose SharedStateImpl points at this pool's store —
// tests that spin up multiple pools in the same process would otherwise
// share the package-level workerplugin.PluginMap and bleed state.
func (p *Pool) pluginMap() map[string]plugin.Plugin {
	wp := &workerplugin.WorkerPlugin{}
	if p.sharedStateStore != nil {
		wp.SharedStateImpl = workerplugin.NewSharedStateServer(p.sharedStateStore)
	}
	return map[string]plugin.Plugin{"worker": wp}
}

var errWorkerStartAborted = fmt.Errorf("worker startup aborted")

type workerStartResult struct {
	handle *workerHandle
	err    error
}

func (p *Pool) startWorker(id int) (*workerHandle, error) {
	return p.startWorkerWithStop(id, nil)
}

func (p *Pool) startWorkerWithStop(id int, stop <-chan struct{}) (*workerHandle, error) {
	select {
	case <-stop:
		return nil, errWorkerStartAborted
	default:
	}

	if p.newWorker != nil {
		return p.startTestWorker(id, stop)
	}

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
		Plugins:         p.pluginMap(),
		Cmd:             cmd,
		AllowedProtocols: []plugin.Protocol{
			plugin.ProtocolGRPC,
		},
		Logger:          pluginLogger,
		GRPCDialOptions: workerplugin.GRPCDialOptions(),
	})

	select {
	case <-stop:
		client.Kill()
		return nil, errWorkerStartAborted
	default:
	}

	resultCh := make(chan workerStartResult, 1)
	go func() {
		handle, err := p.finishWorkerStartup(id, client)
		resultCh <- workerStartResult{handle: handle, err: err}
	}()

	if p.config.WorkerStartTimeout > 0 {
		timer := time.NewTimer(p.config.WorkerStartTimeout)
		defer timer.Stop()

		select {
		case result := <-resultCh:
			return result.handle, result.err
		case <-timer.C:
			client.Kill()
			go reapWorkerStartResult(resultCh)
			return nil, fmt.Errorf("worker startup timed out after %s", p.config.WorkerStartTimeout)
		case <-stop:
			client.Kill()
			go reapWorkerStartResult(resultCh)
			return nil, errWorkerStartAborted
		}
	}

	select {
	case result := <-resultCh:
		return result.handle, result.err
	case <-stop:
		client.Kill()
		go reapWorkerStartResult(resultCh)
		return nil, errWorkerStartAborted
	}
}

func (p *Pool) startTestWorker(id int, stop <-chan struct{}) (*workerHandle, error) {
	select {
	case <-stop:
		return nil, errWorkerStartAborted
	default:
	}

	resultCh := make(chan workerStartResult, 1)
	go func() {
		handle, err := p.newWorker(id)
		resultCh <- workerStartResult{handle: handle, err: err}
	}()

	select {
	case result := <-resultCh:
		return result.handle, result.err
	case <-stop:
		go reapWorkerStartResult(resultCh)
		return nil, errWorkerStartAborted
	}
}

func reapWorkerStartResult(resultCh <-chan workerStartResult) {
	result := <-resultCh
	if result.handle != nil {
		result.handle.kill()
	}
}

func (p *Pool) finishWorkerStartup(id int, client *plugin.Client) (*workerHandle, error) {

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
		inFlightReq: make(map[uint64]*inFlightRequest),
	}
	h.restartCond = sync.NewCond(&h.restartMu)
	h.healthy.Store(true)
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
	timeout := p.GetFirstByteTimeout()
	now := time.Now()
	for _, handle := range p.workerSnapshot() {
		hungRequest, ok := handle.firstHungRequestSnapshot(now, timeout)
		if ok {
			confirmNow := time.Now()
			if !handle.isHungRequestStillHung(hungRequest, confirmNow, timeout) {
				continue
			}
			handle.logger.Warn().
				Uint64("request_id", hungRequest.id).
				Dur("waited", confirmNow.Sub(hungRequest.startedAt)).
				Msg("Worker appears hung, killing")
			workerRestarts.WithLabelValues(string(RestartReasonHung)).Inc()
			p.killWorkerAndRetry(handle)
		}
	}
}

func (h *workerHandle) firstHungRequestSnapshot(now time.Time, timeout time.Duration) (hungRequestSnapshot, bool) {
	h.inFlightMu.RLock()
	defer h.inFlightMu.RUnlock()

	for _, req := range h.inFlightReq {
		if !req.gotFirstByte.Load() && now.Sub(req.startedAt) > timeout {
			return hungRequestSnapshot{
				id:        req.id,
				startedAt: req.startedAt,
			}, true
		}
	}

	return hungRequestSnapshot{}, false
}

func (h *workerHandle) isHungRequestStillHung(snapshot hungRequestSnapshot, now time.Time, timeout time.Duration) bool {
	h.inFlightMu.RLock()
	defer h.inFlightMu.RUnlock()

	req, ok := h.inFlightReq[snapshot.id]
	if !ok || !req.startedAt.Equal(snapshot.startedAt) {
		return false
	}

	return !req.gotFirstByte.Load() && now.Sub(req.startedAt) > timeout
}

func (p *Pool) dispatchRestart(handle *workerHandle) {
	p.restartMu.Lock()
	if p.closed.Load() {
		p.restartMu.Unlock()
		return
	}
	p.restartWG.Add(1)
	handle.restartPending.Add(1)
	p.restartMu.Unlock()

	go func() {
		defer p.restartWG.Done()
		defer handle.restartPending.Add(-1)
		p.restartWorker(handle.id, handle)
	}()
}

// killWorkerAndRetry fires an async replacement for a broken worker and
// then cancels its in-flight requests so they can retry on the new worker.
//
// The restart runs in the background because startWorker has no bounded
// timeout and must not block in-flight request cancellation — otherwise
// the health-check goroutine that calls this would stall as well. Retry
// loops safely wait for the replacement via getWorkerForRetry's CAS-loser
// wait path.
func (p *Pool) killWorkerAndRetry(handle *workerHandle) {
	// Mark the hung worker unhealthy so getWorker() stops routing new
	// requests to it while the replacement starts in the background.
	// Without this, new requests would also hang on the same worker
	// until FirstByteTimeout. For pool size 1, getWorkerForRetry will
	// wait for the in-progress restart before re-fetching.
	handle.healthy.Store(false)

	// Kill the hung process immediately. This breaks the gRPC transport
	// so any lingering references (stale in-flight RPCs) fail fast with
	// Unavailable instead of blocking until startWorker completes.
	// kill() is idempotent — restartWorker will call it again harmlessly.
	handle.kill()

	// Fire the restart in the background so in-flight requests are
	// cancelled immediately below — even if startWorker is slow. The
	// retry loops use getWorkerForRetry, which blocks on the CAS-loser
	// wait path until the restart completes.
	p.dispatchRestart(handle)

	// Cancel all in-flight request contexts (belt-and-suspenders: the
	// process kill above already broke transport, but this ensures the
	// context-based cleanup paths in CallStream/Parse also trigger).
	handle.inFlightMu.RLock()
	cancels := make([]context.CancelFunc, 0, len(handle.inFlightReq))
	for _, req := range handle.inFlightReq {
		if req.cancel != nil {
			cancels = append(cancels, req.cancel)
		}
	}
	handle.inFlightMu.RUnlock()

	for _, cancel := range cancels {
		cancel()
	}
}

func (p *Pool) checkHealth() {
	for _, handle := range p.workerSnapshot() {
		// Abort the sweep once shutdown begins so Close() is not blocked
		// waiting for remaining health RPCs to time out.
		if p.shutdownCtx.Err() != nil {
			return
		}

		ctx, cancel := context.WithTimeout(p.shutdownCtx, 5*time.Second)
		healthy, err := handle.worker.Health(ctx)
		cancel()

		// If the health RPC was interrupted by shutdown, don't treat it
		// as an unhealthy probe — just stop the sweep.
		if p.shutdownCtx.Err() != nil {
			return
		}

		if err != nil || !healthy {
			handle.healthy.Store(false)

			// Once a restart is already pending/in progress for this handle,
			// another failed probe adds no new information. Re-dispatching would
			// only enqueue extra async waiters behind the same restart.
			if _, restartVisible := snapshotRestartState(handle); restartVisible {
				continue
			}

			handle.logger.Warn().Err(err).Msg("Worker unhealthy, restarting")

			workerRestarts.WithLabelValues(string(RestartReasonUnhealthy)).Inc()
			p.dispatchRestart(handle)
		} else if _, restartVisible := snapshotRestartState(handle); !restartVisible {
			handle.healthy.Store(true)
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
	// Early stale-handle check: if the slot already holds a different handle
	// (a previous restart already completed), skip entirely. Without this,
	// cancelled in-flight requests from an old handle waste CPU/memory
	// spawning processes that are immediately killed.
	p.mu.RLock()
	if p.workers[id] != failed {
		p.mu.RUnlock()
		return
	}
	p.mu.RUnlock()

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

	// CAS won — create a channel that awaitRestart callers can select on
	// instead of spawning per-call goroutines. Must be set before the
	// defer so finishRestart always finds a non-nil channel to close.
	failed.restartMu.Lock()
	failed.restartDone = make(chan struct{})
	failed.restartMu.Unlock()

	// INVARIANT: every code path after this point ends with finishRestart.
	// defer guarantees this even if a new return is added later.
	defer failed.finishRestart()

	// Double-check after winning CAS — the slot could have changed between
	// the early check and the CAS, or the pool may have been closed.
	p.mu.RLock()
	stale := p.workers[id] != failed
	closed := p.closed.Load()
	p.mu.RUnlock()
	if stale || closed {
		return
	}

	// Start replacement BEFORE acquiring the pool lock or killing anything.
	// The old worker stays in the slot (possibly struggling, but better
	// than nothing) while the new one boots up (~1-2s for process start
	// and gRPC handshake).
	if p.beforeRestartStart != nil {
		p.beforeRestartStart()
	}

	newHandle, err := p.startWorkerWithStop(id, p.done)
	if err != nil {
		if err == errWorkerStartAborted || p.closed.Load() {
			return
		}

		// Mark the failed worker unhealthy so getWorker stops routing to it.
		p.mu.Lock()
		if p.workers[id] == failed {
			failed.healthy.Store(false)
		}
		p.mu.Unlock()

		// Kill the old process. For the hung-request path
		// (killWorkerAndRetry), the worker may be stuck in a syscall
		// that context cancellation cannot unblock — only process death
		// will free it. kill() is idempotent if already dead.
		failed.kill()
		p.logger.Error().Int("worker", id).Err(err).Msg("Failed to start replacement worker, killed old process")
		return
	}

	p.mu.Lock()

	if p.closed.Load() {
		p.mu.Unlock()
		newHandle.kill()
		return
	}

	// Another goroutine already restarted this worker — kill the one we
	// just started (wasteful but correct) and bail.
	if p.workers[id] != failed {
		p.mu.Unlock()
		newHandle.kill()
		return
	}

	// Hot-swap: put new worker in the slot, then kill old.
	p.workers[id] = newHandle
	p.mu.Unlock()

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
		if handle != nil && handle.healthy.Load() {
			return handle, nil
		}
	}

	return nil, fmt.Errorf("no healthy workers available")
}

// awaitRestart waits for an in-progress restart of handle to complete,
// respecting ctx cancellation.
//
// When the restart goroutine has already set up restartDone (the common
// case), this is a pure select. During the rare scheduling race where
// async dispatch has published restartPending but restartDone is not yet
// visible, poll until the channel is published or the restart disappears.
// Only if no restart state is visible at all do we fall back to kicking
// off restartWorker ourselves.
func (p *Pool) awaitRestart(ctx context.Context, handle *workerHandle) error {
	spins := 0
	sawPending := false
	for {
		ch, pending := snapshotRestartState(handle)

		if ch != nil {
			select {
			case <-ch:
				return nil
			case <-p.drainCh:
				return fmt.Errorf("pool is draining")
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if !pending {
			if sawPending {
				return nil
			}
			break
		}
		sawPending = true
		spins++

		select {
		case <-p.drainCh:
			return fmt.Errorf("pool is draining")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if spins > 10 {
			time.Sleep(time.Millisecond)
		} else {
			runtime.Gosched()
		}
	}

	select {
	case <-p.drainCh:
		return fmt.Errorf("pool is draining")
	default:
	}

	done := make(chan struct{})
	go func() {
		p.restartWorker(handle.id, handle)
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-p.drainCh:
		return fmt.Errorf("pool is draining")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// getWorkerForRetry returns a healthy worker for a retry attempt.
//
// Four cases:
//  1. getWorker() returns a different (healthy) handle → returned immediately.
//  2. getWorker() returns the same handle that just failed (pool size 1,
//     or all others also dead) → waits for the in-progress restart on
//     that handle to complete, then re-fetches.
//  3. getWorker() finds no healthy workers and lastFailed is set → waits
//     for its restart, then re-fetches. This is the pool-size-1 path
//     after killWorkerAndRetry/checkHealth marks the handle unhealthy
//     before firing the async restart.
//  4. getWorker() finds no healthy workers and lastFailed is nil (new
//     request arriving while a restart is in progress) → scans all
//     workers for one that's restarting and waits for it. This prevents
//     new requests from failing immediately with "no healthy workers"
//     during the brief window between a worker being marked unhealthy
//     and its replacement becoming ready.
//
// All waits are context-aware.
func (p *Pool) getWorkerForRetry(ctx context.Context, lastFailed *workerHandle) (*workerHandle, error) {
	handle, err := p.getWorker()
	if err != nil {
		// Fast-reject: don't wait for restarts if the pool is shutting
		// down — the caller should see the closed/draining error immediately.
		if p.closed.Load() || p.draining.Load() {
			return nil, err
		}

		// No healthy workers — wait for a restart to complete.
		if lastFailed != nil {
			// Retry path: wait for the specific handle that failed.
			if awaitErr := p.awaitRestart(ctx, lastFailed); awaitErr != nil {
				return nil, awaitErr
			}
			return p.getWorker()
		}
		// New request (not a retry): wait for ANY restarting worker
		// to finish rather than picking one that might be slow.
		// Loop because awaitAnyRestart can be woken by a failed restart
		// (restartDone is always closed, even on startWorker failure) —
		// keep waiting while restarts are still in progress.
		yielded := false
		for {
			if awaitErr := p.awaitAnyRestart(ctx); awaitErr != nil {
				if ctx.Err() != nil {
					return nil, awaitErr
				}
				if handle, retryErr := p.getWorker(); retryErr == nil {
					return handle, nil
				}
				if p.closed.Load() || p.draining.Load() {
					return nil, awaitErr
				}
				// No restarts visible. The common async-restart gap is
				// covered by restartPending, but a narrow synchronous window
				// still exists between marking a worker unhealthy and
				// publishing restart state. Yield once to let that path run.
				if !yielded && p.hasUnhealthyWorkers() {
					yielded = true
					runtime.Gosched()
					continue
				}
				return nil, awaitErr
			}
			yielded = false // reset after a successful await
			if handle, retryErr := p.getWorker(); retryErr == nil {
				return handle, nil
			}
			// Still no healthy workers. Re-check shutdown state.
			if p.closed.Load() || p.draining.Load() {
				return nil, err
			}
			// Loop: another restart may still be in progress.
		}
	}
	if handle != lastFailed {
		return handle, nil
	}

	// Same handle — wait for the in-progress restart.
	if err := p.awaitRestart(ctx, handle); err != nil {
		return nil, err
	}
	return p.getWorker()
}

// awaitAnyRestart waits for ANY worker restart to complete, returning
// as soon as the first one finishes. This is used for new requests
// (not retries) that arrive while workers are restarting — we don't
// care which worker recovers first, just that one does.
//
// Returns nil if a restart completed, ctx.Err() if the context was
// cancelled, or a generic error if no workers are restarting at all.
func (p *Pool) awaitAnyRestart(ctx context.Context) error {
	spins := 0
	sawPending := false
	prevVisible := map[*workerHandle]chan struct{}{}
	for {
		cases := []reflect.SelectCase{
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(p.drainCh)},
		}
		currentVisible := make(map[*workerHandle]chan struct{})
		hasPendingOnly := false

		for _, h := range p.workerSnapshot() {
			ch, hasRestart := snapshotRestartState(h)
			if !hasRestart {
				continue
			}
			currentVisible[h] = ch
			if ch != nil {
				cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
			} else {
				hasPendingOnly = true
			}
		}

		if sawPending {
			// A previously visible restart vanished (or a published restart was
			// replaced with a different channel), which means some restart finished
			// while we were polling through the publication gap.
			for handle, prevCh := range prevVisible {
				curCh, ok := currentVisible[handle]
				if !ok {
					return nil
				}
				if prevCh != nil && curCh != prevCh {
					return nil
				}
			}
		}

		if len(currentVisible) == 0 {
			if sawPending {
				return nil
			}
			return fmt.Errorf("no workers restarting")
		}
		sawPending = true

		if len(cases) > 2 {
			if !hasPendingOnly {
				chosen, _, _ := reflect.Select(cases)
				switch chosen {
				case 0:
					return ctx.Err()
				case 1:
					return fmt.Errorf("pool is draining")
				default:
					return nil
				}
			}

			// When any restart is still pending-only, do not block on the already
			// published channels: a faster restart could still complete before its
			// restartDone channel is visible.
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectDefault})
			chosen, _, _ := reflect.Select(cases)
			switch chosen {
			case 0:
				return ctx.Err()
			case 1:
				return fmt.Errorf("pool is draining")
			case len(cases) - 1:
			default:
				return nil
			}
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-p.drainCh:
				return fmt.Errorf("pool is draining")
			default:
			}
		}

		prevVisible = currentVisible
		spins++

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.drainCh:
			return fmt.Errorf("pool is draining")
		default:
		}

		if spins > 10 {
			time.Sleep(time.Millisecond)
		} else {
			runtime.Gosched()
		}
	}
}

// hasUnhealthyWorkers returns true if any worker is currently unhealthy.
// Used to detect the async restart dispatch gap: a worker has been marked
// unhealthy and a restart goroutine dispatched, but the goroutine hasn't
// set restarting=true yet.
func (p *Pool) hasUnhealthyWorkers() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, h := range p.workers {
		if h != nil && !h.healthy.Load() {
			return true
		}
	}
	return false
}

// Parse parses raw LLM output using a BAML method's schema.
// Supports automatic retry on worker infrastructure failures (crashes, network issues).
func (p *Pool) Parse(ctx context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error) {
	var lastErr error
	var lastFailed *workerHandle

	for attempt := 0; attempt <= p.config.MaxRetries; attempt++ {
		handle, err := p.getWorkerForRetry(ctx, lastFailed)
		if err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("retry failed, no workers available: %w (previous: %v)", err, lastErr)
			}
			return nil, err
		}

		attemptCtx, cancel := context.WithCancel(ctx)
		req, cleanup := p.trackRequest(handle, cancel)
		req.gotFirstByte.Store(true)
		result, err := handle.worker.Parse(attemptCtx, methodName, inputJSON)
		cleanup()
		if err == nil {
			return result, nil
		}

		// Don't retry if the caller cancelled
		if ctx.Err() != nil {
			return nil, err
		}

		if isRetryableWorkerError(err) {
			handle.healthy.Store(false)

			handle.logger.Info().
				Int("attempt", attempt+1).
				Err(err).
				Msg("Retrying parse after worker failure")
			p.dispatchRestart(handle)
			lastFailed = handle
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

	// Accumulate metadata events during the drain. Pool retries may emit
	// multiple planned/outcome events (one pair per attempt); the unary
	// handler only cares about the last attempt's routing, so keep the
	// most recent of each phase.
	var plannedMetadata, outcomeMetadata []byte

	// Consume stream waiting for final result
	for result := range results {
		switch result.Kind {
		case workerplugin.StreamResultKindError:
			err := workerplugin.NewErrorWithStack(result.Error, result.Stacktrace)
			workerplugin.ReleaseStreamResult(result)
			// Return a CallResult carrying any accumulated metadata
			// alongside the error so the unary handler can still set
			// X-BAML-Path / X-BAML-Path-Reason / X-BAML-Client headers
			// for observability. Data/Raw stay empty — the caller's
			// `if err != nil` branch should not read them. The streaming
			// path already forwards metadata frames eagerly to the
			// client; this preserves the same observability for the
			// unary path on the error tail.
			//
			// Callers that previously assumed `err != nil → result == nil`
			// continue to work: nil-checking the error first remains
			// the correct pattern; what changes is that handlers that
			// want to surface routing metadata on error can read it from
			// the now-non-nil result before returning. See PR #192
			// verdict-15 follow-up.
			return metadataOnlyCallResult(plannedMetadata, outcomeMetadata), err
		case workerplugin.StreamResultKindFinal:
			callResult := &workerplugin.CallResult{
				Data:    result.Data,
				Raw:     result.Raw,
				Planned: plannedMetadata,
				Outcome: outcomeMetadata,
			}
			workerplugin.ReleaseStreamResult(result)
			return callResult, nil
		case workerplugin.StreamResultKindMetadata:
			// The pool's forward loop has already rewritten Attempt. We
			// only need to inspect the Phase to decide which slot the
			// payload belongs in, then copy the bytes (the underlying
			// buffer is about to be released back to the pool).
			if phase := metadataPhase(result.Data); phase == string(bamlutils.MetadataPhaseOutcome) {
				outcomeMetadata = append(outcomeMetadata[:0], result.Data...)
			} else {
				plannedMetadata = append(plannedMetadata[:0], result.Data...)
			}
		}
		// StreamResultKindStream / Metadata / others - continue waiting for final
		workerplugin.ReleaseStreamResult(result)
	}

	// Stream closed without a final result. If the context was cancelled
	// (client disconnect, deadline exceeded), surface that so callers can
	// distinguish cancellation from unexpected stream termination.
	// Preserve accumulated metadata on these tails too — same rationale as
	// the explicit-Error case above.
	if ctx.Err() != nil {
		return metadataOnlyCallResult(plannedMetadata, outcomeMetadata), ctx.Err()
	}
	return metadataOnlyCallResult(plannedMetadata, outcomeMetadata), fmt.Errorf("no final result received")
}

// metadataOnlyCallResult wraps accumulated planned/outcome metadata into a
// CallResult so the unary handlers can surface routing observability on
// error tails. Returns nil if both byte slices are empty so callers can
// still distinguish "metadata available" from "nothing to report".
func metadataOnlyCallResult(planned, outcome []byte) *workerplugin.CallResult {
	if len(planned) == 0 && len(outcome) == 0 {
		return nil
	}
	return &workerplugin.CallResult{Planned: planned, Outcome: outcome}
}

// CallStream executes a BAML method and streams results.
// Supports automatic retry on worker infrastructure failures (crashes, network issues),
// both before first byte and mid-stream. On mid-stream retry, a reset message is injected
// so clients can discard accumulated partial state.
func (p *Pool) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
	// Verify we can get at least one worker before starting.
	// Use getWorkerForRetry(nil) so that new requests arriving during a
	// restart window wait for the replacement rather than failing immediately
	// with "no healthy workers available".
	handle, err := p.getWorkerForRetry(ctx, nil)
	if err != nil {
		return nil, err
	}

	// Generate a stable request id and thread it through every attempt.
	// The worker forwards it as CallRequest.request_id, which the host's
	// SharedState store uses as the idempotency key for FetchAdd. Pool
	// retries must see the same id so the rotation index is resolved once
	// per request — otherwise each retry would advance the counter a
	// second time and the RR ordering would skip a child.
	requestID := uuid.NewString()
	ctx = workerplugin.WithRequestID(ctx, requestID)

	wrappedResults := make(chan *workerplugin.StreamResult)

	go func() {
		defer close(wrappedResults)
		// Release any FetchAdd idempotency entries recorded under this
		// request id once the retry loop finishes (success, error, or
		// caller cancellation — doesn't matter). A TTL sweep on the host
		// side is the backstop for crashes that skip this defer.
		if p.sharedStateStore != nil {
			defer p.sharedStateStore.DropScope(requestID)
		}

		currentHandle := handle
		var lastFailed *workerHandle
		var sentAnyResults bool
		// currentAttempt is the pool-level attempt number stamped onto every
		// metadata event before forwarding. The orchestrator inside the worker
		// always emits Attempt=0; the pool owns this field because only the
		// pool knows about pool-level retries. Incremented on each retry
		// boundary (below), so the first attempt stays 0.
		var currentAttempt int

		for attempt := 0; attempt <= p.config.MaxRetries; attempt++ {
			// For retry attempts, get a new worker (may wait if pool size 1).
			if attempt > 0 {
				currentAttempt++
				var err error
				currentHandle, err = p.getWorkerForRetry(ctx, lastFailed)
				if err != nil {
					sendStreamError(ctx, wrappedResults, fmt.Errorf("retry failed, no workers available: %w", err))
					return
				}
			}

			// Create cancellable context for this attempt
			attemptCtx, cancel := context.WithCancel(ctx)
			req, cleanup := p.trackRequest(currentHandle, cancel)

			results, err := currentHandle.worker.CallStream(attemptCtx, methodName, inputJSON, streamMode)
			if err != nil {
				// Save whether the attempt was pre-cancelled (by hung
				// detection) *before* cleanup cancels the context.
				attemptCancelled := attemptCtx.Err() != nil
				cleanup()

				workerFailed := isRetryableWorkerError(err)
				callerCancelled := ctx.Err() != nil

				// attemptCancelled is only a meaningful signal when the
				// caller has NOT cancelled — because attemptCtx is derived
				// from ctx, a caller cancel always propagates, making
				// attemptCancelled true even for non-retryable errors like
				// InvalidArgument. When the caller has cancelled, only
				// workerFailed (the error itself) determines restart.
				shouldRestart := workerFailed || (!callerCancelled && attemptCancelled)

				// Restart for infrastructure failures or hung detection.
				// Skip cancellation-shaped errors when the caller also
				// cancelled — that's gRPC propagating the caller's context.
				if shouldRestart && !(callerCancelled && isCallerCancellationError(err)) {
					currentHandle.healthy.Store(false)
					p.dispatchRestart(currentHandle)
				}

				// Caller cancelled — don't retry, the caller is gone.
				// The restart above (if triggered) ensures pool health.
				if callerCancelled {
					return
				}

				if shouldRestart {
					currentHandle.logger.Info().
						Int("attempt", attempt+1).
						Err(err).
						Msg("Retrying stream after worker failure")
					lastFailed = currentHandle
					continue
				}

				// Non-retryable error - send to client
				currentHandle.logger.Warn().
					Int("attempt", attempt+1).
					Err(err).
					Msg("Worker stream call failed (non-retryable)")
				sendStreamError(ctx, wrappedResults, err)
				return
			}

			// Forward results from the worker channel with retry support.
			needsReset := sentAnyResults
			var injectedReset bool
			var shouldRetry bool

		streamLoop:
			for {
				var (
					result *workerplugin.StreamResult
					ok     bool
				)

				select {
				case <-ctx.Done():
					cleanup()
					p.drainResultsAndCheckRestart(results, currentHandle)
					return
				case result, ok = <-results:
					if !ok {
						break streamLoop
					}
				}

				// Mark first byte received (disables hung detection for this
				// request) for events that genuinely indicate progress —
				// heartbeat, stream content, final, error, and outcome
				// metadata. SKIP planned metadata: post-verdict-15 it emits
				// upfront from the orchestrator before any HTTP work, so a
				// worker that emits planned and then stalls in upstream
				// HTTP would otherwise disable FirstByteTimeout while never
				// actually progressing. See PR #192 verdict-15 follow-up.
				isPlannedMetadata := result.Kind == workerplugin.StreamResultKindMetadata &&
					metadataPhase(result.Data) != string(bamlutils.MetadataPhaseOutcome)
				if !isPlannedMetadata {
					req.gotFirstByte.Store(true)
				}

				// Filter heartbeat - only for first-byte tracking
				if result.Kind == workerplugin.StreamResultKindHeartbeat {
					workerplugin.ReleaseStreamResult(result)
					continue
				}

				// Stamp the pool-level attempt number onto metadata events.
				// The orchestrator always emits Attempt=0 (it does not know
				// about pool retries); the pool owns this field. Rewrite
				// unconditionally — first attempt is a no-op at Attempt=0.
				// Both planned and outcome events from a retried attempt
				// carry the same value, so clients never see disagreement.
				if result.Kind == workerplugin.StreamResultKindMetadata && len(result.Data) > 0 {
					if rewritten, err := rewriteMetadataAttempt(result.Data, currentAttempt); err != nil {
						currentHandle.logger.Warn().
							Err(err).
							Msg("Failed to rewrite metadata attempt; forwarding as-is")
					} else {
						result.Data = rewritten
					}
				}

				// Check for retryable worker errors mid-stream
				if result.Kind == workerplugin.StreamResultKindError && ctx.Err() != nil && isCallerCancellationError(result.Error) {
					workerplugin.ReleaseStreamResult(result)
					drainResults(results)
					cleanup()
					return
				}

				if result.Kind == workerplugin.StreamResultKindError && isRetryableWorkerError(result.Error) {
					currentHandle.logger.Info().
						Int("attempt", attempt+1).
						Err(result.Error).
						Msg("Retryable error mid-stream, will retry")
					workerplugin.ReleaseStreamResult(result)
					shouldRetry = true
					break
				}

				// Mark the first forwarded result after a retry as reset so the
				// stream writer emits a reset event immediately before this data.
				// This ties reset signaling to the first real result that makes it
				// to the client (instead of a standalone synthetic message).
				//
				// Planned metadata is excluded for the same reason it's excluded
				// from first-byte liveness: post-verdict-15 it emits upfront
				// from the orchestrator before any HTTP work, so it does not
				// represent forwarded content the client needs to discard.
				// Reset must land on the first real result (heartbeat / stream /
				// final / outcome metadata) so the streamwriter only resets
				// state when there's actually state to reset.
				if needsReset && !injectedReset && !isPlannedMetadata {
					result.Reset = true
					injectedReset = true
					currentHandle.logger.Info().Msg("Injected reset marker for mid-stream retry")
				}

				// Save kind before sending — the consumer may ReleaseStreamResult
				// immediately after receiving, zeroing the struct. Reading
				// result.Kind after the send would race with that write.
				resultKind := result.Kind

				// Forward the result to the client
				select {
				case wrappedResults <- result:
					// Planned metadata is excluded from sentAnyResults so a
					// pre-first-byte retry doesn't trigger reset injection —
					// nothing meaningful was forwarded to the client yet.
					// Symmetric with the first-byte gate above.
					if !isPlannedMetadata {
						sentAnyResults = true
					}
				case <-ctx.Done():
					workerplugin.ReleaseStreamResult(result)
					p.drainResultsAndCheckRestart(results, currentHandle)
					cleanup()
					return
				}

				// Terminal states - we're done (successfully or with app error)
				if resultKind == workerplugin.StreamResultKindFinal ||
					resultKind == workerplugin.StreamResultKindError {
					drainResults(results)
					cleanup()
					return
				}
			}

			cleanup()

			// Reaching here means either a mid-stream retryable error or an
			// unexpected EOF (channel closed without terminal). Both are
			// retryable: restart in background and let the next iteration
			// pick up a healthy worker.
			currentHandle.healthy.Store(false)

			if !shouldRetry {
				currentHandle.logger.Warn().
					Int("attempt", attempt+1).
					Msg("Stream ended unexpectedly without terminal result, retrying")
			}

			p.dispatchRestart(currentHandle)

			// If the caller cancelled while we were processing the
			// mid-stream error, don't start another attempt — the
			// caller is gone. The restart above ensures pool health.
			if ctx.Err() != nil {
				return
			}

			lastFailed = currentHandle
		}

		// All retries exhausted
		sendStreamError(ctx, wrappedResults, fmt.Errorf("all stream retries exhausted"))
	}()

	return wrappedResults, nil
}

// metadataPhase peeks the "phase" field out of a JSON-encoded metadata
// payload without fully decoding it. Returns "" if the payload is malformed
// or missing the field — callers treat that as "planned" by default.
func metadataPhase(data []byte) string {
	var envelope struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ""
	}
	return envelope.Phase
}

// rewriteMetadataAttempt JSON-decodes a bamlutils.Metadata payload, overrides
// the Attempt field with the pool-level attempt number, and re-encodes. This
// runs for every metadata event the pool forwards so both planned and outcome
// events of a retried attempt carry the same number.
func rewriteMetadataAttempt(data []byte, attempt int) ([]byte, error) {
	var md bamlutils.Metadata
	if err := json.Unmarshal(data, &md); err != nil {
		return nil, fmt.Errorf("decode metadata: %w", err)
	}
	md.Attempt = attempt
	encoded, err := json.Marshal(&md)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}
	return encoded, nil
}

// sendStreamError sends an error result on ch, respecting ctx cancellation.
// Used by CallStream to avoid repeating the get/send/release pattern.
func sendStreamError(ctx context.Context, ch chan<- *workerplugin.StreamResult, err error) {
	errResult := workerplugin.GetStreamResult()
	errResult.Kind = workerplugin.StreamResultKindError
	errResult.Error = err
	select {
	case ch <- errResult:
	case <-ctx.Done():
		workerplugin.ReleaseStreamResult(errResult)
	}
}

// drainResultsAndCheckRestart drains all remaining results from the channel
// in a background goroutine, releasing each one. If any result is a retryable
// infrastructure error (not a caller cancellation), it marks the worker
// unhealthy and dispatches a restart. This catches errors that arrive at any
// point during the drain — not just those that were simultaneously buffered
// when ctx.Done() won the select.
func (p *Pool) drainResultsAndCheckRestart(results <-chan *workerplugin.StreamResult, handle *workerHandle) {
	go func() {
		for r := range results {
			if r.Kind == workerplugin.StreamResultKindError &&
				isRetryableWorkerError(r.Error) && !isCallerCancellationError(r.Error) {
				handle.healthy.Store(false)
				p.dispatchRestart(handle)
			}
			workerplugin.ReleaseStreamResult(r)
		}
	}()
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
	var total int64
	for _, handle := range p.workerSnapshot() {
		handle.inFlightMu.RLock()
		total += int64(len(handle.inFlightReq))
		handle.inFlightMu.RUnlock()
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
	type inFlightSnapshot struct {
		id           uint64
		startedAt    time.Time
		gotFirstByte bool
	}

	// Prefer the worker with the most recently started in-flight request.
	// This makes debug kills deterministic when a previous test leaves a stale
	// in-flight request while a new stream has just started.
	var selected *workerHandle
	var selectedCount int
	var selectedGotFirstByte []bool
	var selectedNewestStartedAt time.Time

	for _, handle := range p.workerSnapshot() {
		handle.inFlightMu.RLock()
		inFlightCount := len(handle.inFlightReq)
		if inFlightCount == 0 {
			handle.inFlightMu.RUnlock()
			continue
		}

		snapshots := make([]inFlightSnapshot, 0, inFlightCount)
		var newestStartedAt time.Time
		for _, req := range handle.inFlightReq {
			snapshot := inFlightSnapshot{
				id:           req.id,
				startedAt:    req.startedAt,
				gotFirstByte: req.gotFirstByte.Load(),
			}
			snapshots = append(snapshots, snapshot)
			if snapshot.startedAt.After(newestStartedAt) {
				newestStartedAt = snapshot.startedAt
			}
		}
		handle.inFlightMu.RUnlock()

		sort.Slice(snapshots, func(i, j int) bool {
			if snapshots[i].startedAt.Equal(snapshots[j].startedAt) {
				return snapshots[i].id < snapshots[j].id
			}
			return snapshots[i].startedAt.Before(snapshots[j].startedAt)
		})

		gotFirstByteList := make([]bool, 0, len(snapshots))
		for _, snapshot := range snapshots {
			gotFirstByteList = append(gotFirstByteList, snapshot.gotFirstByte)
		}

		if selected == nil || newestStartedAt.After(selectedNewestStartedAt) {
			selected = handle
			selectedCount = inFlightCount
			selectedGotFirstByte = gotFirstByteList
			selectedNewestStartedAt = newestStartedAt
		}
	}

	if selected == nil {
		return nil, fmt.Errorf("no workers with in-flight requests found")
	}

	p.logger.Warn().
		Int("worker_id", selected.id).
		Int("in_flight_count", selectedCount).
		Msg("DEBUG: Killing worker with in-flight requests")

	p.killWorkerAndRetry(selected)

	return &KillWorkerResult{
		WorkerID:      selected.id,
		InFlightCount: selectedCount,
		GotFirstByte:  selectedGotFirstByte,
		Killed:        true,
	}, nil
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
	workers := p.workerSnapshot()
	result := make([]WorkerInFlightInfo, 0, len(workers))
	for _, handle := range workers {
		healthy := handle.healthy.Load()

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
	if p == nil {
		return nil
	}

	// Start draining - reject new requests
	p.draining.Store(true)
	if p.drainCh != nil {
		p.drainOnce.Do(func() { close(p.drainCh) })
	}
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

// Close immediately shuts down the pool and kills all workers.
//
// Contract (CodeRabbit verdict-21 finding 10):
//
//   - Cancels the pool-level shutdown context so in-progress health
//     RPCs abort instead of blocking for their full timeout.
//   - Closes the drain channel so callers stop waiting on it.
//   - Snapshots and kills every worker plugin handle (worker
//     subprocesses).
//   - Waits on `restartWG` (so any in-flight restart goroutine has
//     returned) and `p.wg` (which currently tracks only the health
//     checker goroutine — see pool.go:373/555).
//   - Closes the SharedStateStore, which only stops the background
//     sweeper goroutine; remaining map operations stay safe after
//     Close (workerplugin/sharedstate.go:140).
//
// What Close does **not** wait for: in-flight CallStream goroutines
// (pool.go:1306). These goroutines are not registered with `p.wg` —
// the design assumes Close is called when the caller wants every
// worker subprocess killed *now*, not a graceful drain. Use
// Shutdown(ctx) for a drain that waits for in-flight requests to
// finish (or the context to expire), then calls Close.
//
// Race-safety with concurrent CallStream goroutines:
// SharedStateStore methods (FetchAdd, DropScope, etc.) remain safe to
// invoke after Close — Close only closes the sweeper's stopCh; the
// underlying maps are not invalidated. So a CallStream goroutine
// that defers DropScope(requestID) is fine even if Close has
// returned by the time the defer fires. If a future change adds
// state that *would* be unsafe to access post-Close, this contract
// will need to change to track CallStream goroutines explicitly.
func (p *Pool) Close() error {
	if p == nil {
		return nil
	}

	if p.closed.Swap(true) {
		return nil // Already closed
	}

	p.draining.Store(true)
	if p.drainCh != nil {
		p.drainOnce.Do(func() { close(p.drainCh) })
	}

	// Cancel pool-level context so in-progress health RPCs abort immediately
	// instead of blocking for their full timeout.
	if p.shutdownCancel != nil {
		p.shutdownCancel()
	}

	// Signal health checker to stop
	if p.done != nil {
		close(p.done)
	}

	// Prevent any new restart goroutine from registering with restartWG while
	// Close begins waiting below.
	p.restartMu.Lock()
	p.restartMu.Unlock()

	// Snapshot current workers and release the pool lock before doing any
	// potentially blocking work. Holding p.mu across kill()/wg.Wait() can
	// deadlock with the health-check goroutine if it is concurrently trying
	// to enter restartWorker(), which needs p.mu before the goroutine can exit.
	handles := p.workerSnapshot()

	for _, handle := range handles {
		handle.kill()
	}

	p.restartWG.Wait()
	p.wg.Wait()
	if p.sharedStateStore != nil {
		p.sharedStateStore.Close()
	}
	p.logger.Info().Msg("Worker pool closed")
	return nil
}

// Stats returns pool statistics
type Stats struct {
	TotalWorkers   int
	HealthyWorkers int
	InFlight       int64
}

// parseSerializedGRPCCode extracts the top-level gRPC status code name from
// a serialized gRPC error string. gRPC errors serialize as
// "rpc error: code = <Code> desc = <Message>". This function parses only
// the FIRST (top-level) code, not any codes that may appear in nested error
// descriptions like "rpc error: code = Internal desc = rpc error: code = Canceled ...".
// Returns the code name (e.g. "Internal", "Canceled") and true, or ("", false)
// if the string doesn't match the format.
func parseSerializedGRPCCode(errStr string) (string, bool) {
	const prefix = "rpc error: code = "
	idx := strings.Index(errStr, prefix)
	if idx < 0 {
		return "", false
	}
	start := idx + len(prefix)
	if start >= len(errStr) {
		return "", false
	}
	rest := errStr[start:]
	end := strings.IndexByte(rest, ' ')
	if end <= 0 {
		if len(rest) > 0 {
			return rest, true
		}
		return "", false
	}
	return rest[:end], true
}

// isRetryableWorkerError returns true if the error indicates a worker infrastructure
// failure (crash, network issue) rather than an application-level error from BAML/LLM.
// These errors should trigger automatic retry on another worker.
func isRetryableWorkerError(err error) bool {
	if err == nil {
		return false
	}

	// First, try to extract gRPC status code (preferred method).
	// When a gRPC status is present, it is authoritative — do NOT fall
	// through to substring matching. An error like codes.Internal with
	// "context canceled" in its message is an application error, not a
	// retryable infrastructure failure.
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable:
			// Worker died, network issue, or connection refused
			return true
		case codes.Canceled:
			// Request was canceled (e.g., by hung detection)
			return true
		case codes.DeadlineExceeded:
			// Request timed out (e.g., hung detection deadline)
			return true
		}

		// Also check the status message for specific infrastructure errors
		msg := st.Message()
		if strings.Contains(msg, "connection reset") ||
			strings.Contains(msg, "error reading from server: EOF") ||
			strings.Contains(msg, "transport is closing") {
			return true
		}

		// Any other gRPC status code is not retryable.
		return false
	}

	// No gRPC status object — the error may be a string-serialized gRPC
	// error from the boundary (GRPCServer serializes via .Error(),
	// GRPCClient reconstructs with fmt.Errorf), or a plain Go error.
	errStr := err.Error()

	// Serialized gRPC errors: parse the top-level code and use it as the
	// authoritative signal. This correctly handles nested errors like
	// "rpc error: code = Internal desc = rpc error: code = Canceled ..."
	// by only looking at the first (top-level) code.
	if code, ok := parseSerializedGRPCCode(errStr); ok {
		switch code {
		case "Unavailable", "Canceled", "DeadlineExceeded":
			return true
		}
		return false
	}

	// Plain error without gRPC structure — only match transport-level
	// infrastructure patterns. Plain "context canceled" / "context deadline
	// exceeded" strings are NOT treated as retryable here because they can
	// come from application-level errors (e.g. upstream LLM timeout via the
	// BAML adapter). Transport-level cancellations are already caught above
	// via the serialized gRPC code ("rpc error: code = Canceled ...").
	if strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "error reading from server: EOF") ||
		strings.Contains(errStr, "transport is closing") {
		return true
	}

	return false
}

func isCallerCancellationError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	if st, ok := status.FromError(err); ok {
		// gRPC status is available — use it as the authoritative signal.
		// Do NOT fall through to string matching: a non-cancellation status
		// like Unavailable can contain "context canceled" in its message
		// text (e.g. transport teardown) without being a caller cancellation.
		// Falling through would misclassify a dead worker as a cancelled
		// caller and suppress restart.
		switch st.Code() {
		case codes.Canceled, codes.DeadlineExceeded:
			return true
		default:
			return false
		}
	}

	// No gRPC status object — the error may be a string-serialized gRPC
	// error from the boundary (GRPCServer serializes via .Error(),
	// GRPCClient reconstructs with fmt.Errorf), or a plain Go error.
	errStr := err.Error()

	// Serialized gRPC errors: parse the top-level code. This correctly
	// handles nested errors by only looking at the first code.
	if code, ok := parseSerializedGRPCCode(errStr); ok {
		return code == "Canceled" || code == "DeadlineExceeded"
	}

	// Plain error without gRPC structure (e.g. fmt.Errorf("context canceled")
	// from boundary serialization of a non-gRPC error). Use simple substring
	// matching for the canonical Go error messages.
	if strings.Contains(errStr, context.Canceled.Error()) ||
		strings.Contains(errStr, context.DeadlineExceeded.Error()) {
		return true
	}

	return false
}

func (p *Pool) Stats() Stats {
	stats := Stats{
		TotalWorkers: p.config.PoolSize,
	}

	for _, handle := range p.workerSnapshot() {
		if handle.healthy.Load() {
			stats.HealthyWorkers++
		}
		handle.inFlightMu.RLock()
		stats.InFlight += int64(len(handle.inFlightReq))
		handle.inFlightMu.RUnlock()
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
	var results []WorkerMetrics

	for _, handle := range p.workerSnapshot() {
		if !handle.healthy.Load() {
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
	var results []WorkerGCResult

	for _, handle := range p.workerSnapshot() {
		if !handle.healthy.Load() {
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
	var results []WorkerGoroutinesResult

	for _, handle := range p.workerSnapshot() {
		if !handle.healthy.Load() {
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

// WorkerNativeStacksResult contains native (OS-level) thread backtraces for a worker.
type WorkerNativeStacksResult struct {
	WorkerID int
	Pid      int
	Output   string // gdb output (thread backtraces)
	Error    error
}

// GetAllWorkersNativeStacks uses gdb to capture native thread backtraces from all
// worker processes. This reveals where Rust/C threads are blocked — information
// that Go's pprof goroutine dump cannot provide.
//
// Requires: gdb installed in the container, SYS_PTRACE capability.
func (p *Pool) GetAllWorkersNativeStacks(ctx context.Context) []WorkerNativeStacksResult {
	var results []WorkerNativeStacksResult

	for _, handle := range p.workerSnapshot() {
		result := WorkerNativeStacksResult{WorkerID: handle.id}

		// Get PID from go-plugin's ReattachConfig
		reattach := handle.client.ReattachConfig()
		if reattach == nil || reattach.Pid == 0 {
			result.Error = fmt.Errorf("could not determine worker PID")
			results = append(results, result)
			continue
		}
		result.Pid = reattach.Pid

		// Run gdb with a short timeout to avoid blocking if gdb hangs
		gdbCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(gdbCtx, "gdb",
			"-batch",
			"-ex", "set pagination off",
			"-ex", "set print thread-events off",
			"-ex", "thread apply all bt",
			"-p", fmt.Sprintf("%d", reattach.Pid),
		)

		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		cancel()

		if err != nil {
			// gdb often exits non-zero even on success (e.g. detach warnings).
			// If we got stdout output, treat it as success with a warning.
			if stdout.Len() > 0 {
				result.Output = stdout.String()
				p.logger.Warn().Int("worker", handle.id).Err(err).
					Msg("gdb exited non-zero but produced output")
			} else {
				result.Error = fmt.Errorf("gdb failed: %w (stderr: %s)", err, stderr.String())
			}
		} else {
			result.Output = stdout.String()
		}

		results = append(results, result)
	}

	return results
}
