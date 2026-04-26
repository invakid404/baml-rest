package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/workerplugin"
)

// ---------------------------------------------------------------------------
// Mock worker
// ---------------------------------------------------------------------------

// mockWorker implements workerplugin.Worker and io.Closer for unit tests.
type mockWorker struct {
	parseFn      func(ctx context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error)
	callStreamFn func(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error)
	healthFn     func(ctx context.Context) (bool, error)
	closeFn      func() error

	closeMu sync.Mutex
	closed  bool
}

type manualCancelContext struct {
	mu   sync.RWMutex
	err  error
	done chan struct{}
	once sync.Once
}

func newManualCancelContext() *manualCancelContext {
	return &manualCancelContext{done: make(chan struct{})}
}

func (c *manualCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *manualCancelContext) Done() <-chan struct{}       { return c.done }
func (c *manualCancelContext) Value(any) any               { return nil }

func (c *manualCancelContext) Err() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.err
}

func (c *manualCancelContext) cancel(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
	c.once.Do(func() { close(c.done) })
}

func newMockWorker() *mockWorker { return &mockWorker{} }

func (m *mockWorker) Parse(ctx context.Context, methodName string, inputJSON []byte) (*workerplugin.ParseResult, error) {
	if m.parseFn != nil {
		return m.parseFn(ctx, methodName, inputJSON)
	}
	return &workerplugin.ParseResult{Data: []byte(`"ok"`)}, nil
}

func (m *mockWorker) CallStream(ctx context.Context, methodName string, inputJSON []byte, streamMode bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
	if m.callStreamFn != nil {
		return m.callStreamFn(ctx, methodName, inputJSON, streamMode)
	}
	ch := make(chan *workerplugin.StreamResult, 1)
	r := workerplugin.GetStreamResult()
	r.Kind = workerplugin.StreamResultKindFinal
	r.Data = []byte(`"stream_ok"`)
	ch <- r
	close(ch)
	return ch, nil
}

func (m *mockWorker) Health(ctx context.Context) (bool, error) {
	if m.healthFn != nil {
		return m.healthFn(ctx)
	}
	return true, nil
}

func (m *mockWorker) GetMetrics(context.Context) ([][]byte, error) { return nil, nil }
func (m *mockWorker) TriggerGC(context.Context) (*workerplugin.GCResult, error) {
	return &workerplugin.GCResult{}, nil
}
func (m *mockWorker) GetGoroutines(context.Context, string) (*workerplugin.GoroutinesResult, error) {
	return &workerplugin.GoroutinesResult{}, nil
}

func (m *mockWorker) Close() error {
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	m.closed = true
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newMockHandle(id int, w *mockWorker) *workerHandle {
	h := &workerHandle{
		id:          id,
		logger:      zerolog.Nop(),
		worker:      w,
		inFlightReq: make(map[uint64]*inFlightRequest),
	}
	h.restartCond = sync.NewCond(&h.restartMu)
	h.healthy.Store(true)
	return h
}

// newTestPool creates a Pool backed by mock workers.
// The factory is invoked for every startWorker call (initial + replacements).
// The health checker is NOT started.
func newTestPool(t testing.TB, size int, factory func(id int) (*workerHandle, error)) *Pool {
	t.Helper()
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	p := &Pool{
		config: &Config{
			PoolSize:         size,
			MaxRetries:       2,
			FirstByteTimeout: 5 * time.Second,
		},
		logger:         zerolog.Nop(),
		workers:        make([]*workerHandle, size),
		done:           make(chan struct{}),
		drainCh:        make(chan struct{}),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
		newWorker:      factory,
	}
	for i := 0; i < size; i++ {
		h, err := p.startWorker(i)
		if err != nil {
			t.Fatalf("failed to start initial worker %d: %v", i, err)
		}
		p.workers[i] = h
	}
	return p
}

// goodFactory always returns a healthy mock handle.
func goodFactory(id int) (*workerHandle, error) {
	return newMockHandle(id, newMockWorker()), nil
}

// unavailableErr returns a gRPC Unavailable error (retryable).
func unavailableErr() error {
	return status.Error(codes.Unavailable, "worker crashed")
}

// requireCompleteWithin fails if f does not return within d.
func requireCompleteWithin(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not complete within %v", d)
	}
}

// ---------------------------------------------------------------------------
// Tests: restartWorker internals
// ---------------------------------------------------------------------------

// TestRestartSingleWorker verifies basic hot-swap: one worker is replaced
// and the new one ends up in the slot.
func TestRestartSingleWorker(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	old := p.workers[0]
	p.restartWorker(0, old)

	if p.workers[0] == old {
		t.Fatal("old handle still in slot after restart")
	}
	if !p.workers[0].healthy.Load() {
		t.Fatal("replacement worker should be healthy")
	}
}

// TestRestartConcurrentCallers verifies that when many goroutines trigger
// restartWorker simultaneously for the same handle, exactly one replacement
// is created (CAS guard) and all waiters complete (sync.Cond).
func TestRestartConcurrentCallers(t *testing.T) {
	var factoryCalls atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		factoryCalls.Add(1)
		// Simulate a non-trivial startup time so concurrent callers
		// actually queue up on the Cond.
		time.Sleep(50 * time.Millisecond)
		return newMockHandle(id, newMockWorker()), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	// Reset counter after the initial worker was created.
	factoryCalls.Store(0)
	failed := p.workers[0]

	const goroutines = 50
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p.restartWorker(0, failed)
		}()
	}

	requireCompleteWithin(t, 10*time.Second, func() {
		close(start)
		wg.Wait()
	})

	if calls := factoryCalls.Load(); calls != 1 {
		t.Errorf("expected 1 replacement, factory called %d times", calls)
	}
	if p.workers[0] == failed {
		t.Error("old handle still in slot")
	}
	if !p.workers[0].healthy.Load() {
		t.Error("replacement should be healthy")
	}
}

// TestRestartWaitersWakeOnFailure verifies that when startWorker fails,
// all waiting goroutines still wake up (no stuck goroutines).
func TestRestartWaitersWakeOnFailure(t *testing.T) {
	var failFactory atomic.Bool

	factory := func(id int) (*workerHandle, error) {
		if failFactory.Load() {
			time.Sleep(50 * time.Millisecond) // simulate attempt
			return nil, fmt.Errorf("simulated startup failure")
		}
		return newMockHandle(id, newMockWorker()), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	failFactory.Store(true)
	failed := p.workers[0]

	const goroutines = 20
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p.restartWorker(0, failed)
		}()
	}

	requireCompleteWithin(t, 10*time.Second, func() {
		close(start)
		wg.Wait()
	})

	// Worker should be marked unhealthy.
	if failed.healthy.Load() {
		t.Error("worker should be unhealthy after failed restart")
	}
}

// TestRestartFailureSkipsToHealthyWorker verifies that after a failed
// restart marks a worker unhealthy, getWorker routes to the other worker.
func TestRestartFailureSkipsToHealthyWorker(t *testing.T) {
	var failFactory atomic.Bool

	factory := func(id int) (*workerHandle, error) {
		if failFactory.Load() {
			return nil, fmt.Errorf("simulated startup failure")
		}
		return newMockHandle(id, newMockWorker()), nil
	}

	p := newTestPool(t, 2, factory)
	defer p.Close()

	failFactory.Store(true)
	p.restartWorker(0, p.workers[0])

	// getWorker should skip worker 0 (unhealthy) and return worker 1.
	handle, err := p.getWorker()
	if err != nil {
		t.Fatalf("getWorker: %v", err)
	}
	if handle.id == 0 {
		t.Error("getWorker returned the unhealthy worker")
	}
}

// TestRestartFailureKillsOldWorker verifies that when startWorker fails,
// the old process is killed. This is critical for the hung-request path
// where context cancellation may not unblock a stuck RPC.
func TestRestartFailureKillsOldWorker(t *testing.T) {
	var failFactory atomic.Bool

	factory := func(id int) (*workerHandle, error) {
		if failFactory.Load() {
			return nil, fmt.Errorf("simulated startup failure")
		}
		return newMockHandle(id, newMockWorker()), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	oldWorker := p.workers[0].worker.(*mockWorker)
	failFactory.Store(true)
	p.restartWorker(0, p.workers[0])

	// The old worker's Close() should have been called by kill().
	oldWorker.closeMu.Lock()
	closed := oldWorker.closed
	oldWorker.closeMu.Unlock()
	if !closed {
		t.Error("old worker should be killed (Close called) when replacement fails")
	}
}

// TestKillWorkerAndRetryKillsImmediately verifies that killWorkerAndRetry
// kills the old process synchronously (before the async restart starts),
// so lingering gRPC references fail fast instead of blocking on a hung
// transport while startWorker runs.
func TestKillWorkerAndRetryKillsImmediately(t *testing.T) {
	slowFactory := func(id int) (*workerHandle, error) {
		time.Sleep(time.Second)
		return newMockHandle(id, newMockWorker()), nil
	}

	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	oldWorker := p.workers[0].worker.(*mockWorker)
	p.newWorker = slowFactory

	p.killWorkerAndRetry(p.workers[0])

	// kill() must have been called synchronously — the slow replacement
	// hasn't started yet but the old process is already dead.
	oldWorker.closeMu.Lock()
	killed := oldWorker.closed
	oldWorker.closeMu.Unlock()
	if !killed {
		t.Error("old worker should be killed immediately, before replacement starts")
	}
}

// TestKillWorkerAndRetryCancelsOutsideInFlightLock verifies that request
// cancellation runs after the in-flight lock is released.
func TestKillWorkerAndRetryCancelsOutsideInFlightLock(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	handle := p.workers[0]
	cancelCalled := make(chan struct{})

	handle.inFlightMu.Lock()
	handle.inFlightReq[1] = &inFlightRequest{
		cancel: func() {
			close(cancelCalled)
			handle.inFlightMu.Lock()
			handle.inFlightMu.Unlock()
		},
	}
	handle.inFlightMu.Unlock()

	done := make(chan struct{})
	go func() {
		p.killWorkerAndRetry(handle)
		close(done)
	}()

	select {
	case <-cancelCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel was not invoked")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("killWorkerAndRetry blocked while cancelling requests")
	}
}

// TestFirstHungRequestSnapshotCopiesState verifies that hung-request detection
// copies request fields while holding the in-flight lock instead of retaining a
// pointer to pooled request state after unlock.
func TestFirstHungRequestSnapshotCopiesState(t *testing.T) {
	handle := newMockHandle(0, newMockWorker())
	startedAt := time.Now().Add(-10 * time.Second)
	req := &inFlightRequest{
		id:        42,
		startedAt: startedAt,
	}
	handle.inFlightReq[req.id] = req

	snapshot, ok := handle.firstHungRequestSnapshot(time.Now(), time.Second)
	if !ok {
		t.Fatal("expected hung request snapshot")
	}

	// Simulate request cleanup and object reuse after the lock is released.
	*req = inFlightRequest{}
	req.id = 99
	req.startedAt = time.Now()
	req.gotFirstByte.Store(true)

	if snapshot.id != 42 {
		t.Fatalf("snapshot id = %d, want 42", snapshot.id)
	}
	if !snapshot.startedAt.Equal(startedAt) {
		t.Fatalf("snapshot startedAt = %v, want %v", snapshot.startedAt, startedAt)
	}
}

// TestHungRequestConfirmationSkipsProgressedRequest verifies that once a
// request receives its first byte after the initial hung snapshot, the worker
// is no longer considered hung for that request.
func TestHungRequestConfirmationSkipsProgressedRequest(t *testing.T) {
	handle := newMockHandle(0, newMockWorker())
	req := &inFlightRequest{
		id:        42,
		startedAt: time.Now().Add(-10 * time.Second),
	}
	handle.inFlightReq[req.id] = req

	snapshot, ok := handle.firstHungRequestSnapshot(time.Now(), time.Second)
	if !ok {
		t.Fatal("expected hung request snapshot")
	}

	req.gotFirstByte.Store(true)

	if handle.isHungRequestStillHung(snapshot, time.Now(), time.Second) {
		t.Fatal("request should not still be treated as hung after first byte")
	}
}

// TestCheckHungRequestsRestartsHungWorker verifies the end-to-end hung path:
// a request that has exceeded the first-byte timeout causes the worker to be
// killed and replaced.
func TestCheckHungRequestsRestartsHungWorker(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	old := p.workers[0]
	oldWorker := old.worker.(*mockWorker)
	closed := make(chan struct{}, 1)
	oldWorker.closeFn = func() error {
		select {
		case closed <- struct{}{}:
		default:
		}
		return nil
	}

	p.SetFirstByteTimeout(time.Second)

	old.inFlightMu.Lock()
	old.inFlightReq[1] = &inFlightRequest{
		id:        1,
		startedAt: time.Now().Add(-10 * time.Second),
	}
	old.inFlightMu.Unlock()

	p.checkHungRequests()

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("hung worker was not killed")
	}

	requireCompleteWithin(t, 2*time.Second, func() {
		for {
			p.mu.RLock()
			current := p.workers[0]
			p.mu.RUnlock()
			if current != old {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	p.mu.RLock()
	replacement := p.workers[0]
	p.mu.RUnlock()
	if !replacement.healthy.Load() {
		t.Fatal("replacement worker should be healthy")
	}
}

// TestCheckHungRequestsSkipsRequestWithFirstByte verifies the end-to-end
// negative path: once first byte has been received, hung detection should not
// kill or replace the worker even if the request is old.
func TestCheckHungRequestsSkipsRequestWithFirstByte(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	old := p.workers[0]
	oldWorker := old.worker.(*mockWorker)
	closed := make(chan struct{}, 1)
	oldWorker.closeFn = func() error {
		select {
		case closed <- struct{}{}:
		default:
		}
		return nil
	}

	p.SetFirstByteTimeout(time.Second)

	req := &inFlightRequest{
		id:        1,
		startedAt: time.Now().Add(-10 * time.Second),
	}
	req.gotFirstByte.Store(true)

	old.inFlightMu.Lock()
	old.inFlightReq[1] = req
	old.inFlightMu.Unlock()

	p.checkHungRequests()

	select {
	case <-closed:
		t.Fatal("worker should not be killed after first byte")
	case <-time.After(100 * time.Millisecond):
	}

	p.mu.RLock()
	current := p.workers[0]
	p.mu.RUnlock()
	if current != old {
		t.Fatal("worker should not be replaced after first byte")
	}
}

// TestRestartStaleHandleNoop verifies that restarting with a handle that
// has already been replaced is a no-op (prevents killing fresh workers).
func TestRestartStaleHandleNoop(t *testing.T) {
	var factoryCalls atomic.Int32

	factory := func(id int) (*workerHandle, error) {
		factoryCalls.Add(1)
		return newMockHandle(id, newMockWorker()), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()
	factoryCalls.Store(0)

	stale := p.workers[0]

	// First restart replaces the worker.
	p.restartWorker(0, stale)
	if factoryCalls.Load() != 1 {
		t.Fatalf("expected 1 factory call, got %d", factoryCalls.Load())
	}

	current := p.workers[0]

	// Second restart with the stale handle should be a no-op —
	// the early ownership check prevents even spawning a process.
	factoryCalls.Store(0)
	p.restartWorker(0, stale)
	if p.workers[0] != current {
		t.Error("stale restart should not replace the current worker")
	}
	if factoryCalls.Load() != 0 {
		t.Errorf("stale restart should not spawn a process, got %d factory calls", factoryCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// Tests: Parse retry
// ---------------------------------------------------------------------------

// TestParseRetryAfterRestart verifies that a Parse call that hits a dead
// worker transparently retries on a fresh replacement.
func TestParseRetryAfterRestart(t *testing.T) {
	var parseOKCalls atomic.Int32

	failWorker := newMockWorker()
	failWorker.parseFn = func(context.Context, string, []byte) (*workerplugin.ParseResult, error) {
		return nil, unavailableErr()
	}

	goodWorker := newMockWorker()
	goodWorker.parseFn = func(context.Context, string, []byte) (*workerplugin.ParseResult, error) {
		parseOKCalls.Add(1)
		return &workerplugin.ParseResult{Data: []byte(`"parsed"`)}, nil
	}

	var n atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		if n.Add(1) == 1 {
			return newMockHandle(id, failWorker), nil
		}
		return newMockHandle(id, goodWorker), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	result, err := p.Parse(context.Background(), "Test", []byte(`{}`))
	if err != nil {
		t.Fatalf("Parse should succeed after retry: %v", err)
	}
	if string(result.Data) != `"parsed"` {
		t.Errorf("unexpected data: %s", result.Data)
	}
	if parseOKCalls.Load() != 1 {
		t.Errorf("good worker should be called once, got %d", parseOKCalls.Load())
	}
}

// TestParseExhaustsRetries verifies that Parse returns an error after
// MaxRetries when every worker fails.
func TestParseExhaustsRetries(t *testing.T) {
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.parseFn = func(context.Context, string, []byte) (*workerplugin.ParseResult, error) {
			return nil, unavailableErr()
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	_, err := p.Parse(context.Background(), "Test", []byte(`{}`))
	if err == nil {
		t.Fatal("Parse should fail after exhausting retries")
	}
}

// TestParsePoolSizeOne exercises the full failure→restart→success cycle
// with a single-worker pool (the hardest configuration).
func TestParsePoolSizeOne(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	// Poison the current worker.
	initial := p.workers[0].worker.(*mockWorker)
	initial.parseFn = func(context.Context, string, []byte) (*workerplugin.ParseResult, error) {
		return nil, unavailableErr()
	}

	// Parse should restart the poisoned worker and succeed on the
	// replacement (which uses the default success parseFn).
	result, err := p.Parse(context.Background(), "Test", []byte(`{}`))
	if err != nil {
		t.Fatalf("Parse should recover on pool-size-1: %v", err)
	}
	if string(result.Data) != `"ok"` {
		t.Errorf("unexpected data: %s", result.Data)
	}
}

// TestParseTracksInFlightRequests verifies that Parse registers and cleans up
// in-flight work so shutdown accounting sees unary parse requests.
func TestParseTracksInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.parseFn = func(context.Context, string, []byte) (*workerplugin.ParseResult, error) {
			close(started)
			<-release
			return &workerplugin.ParseResult{Data: []byte(`"parsed"`)}, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	parseDone := make(chan error, 1)
	go func() {
		_, err := p.Parse(context.Background(), "Test", []byte(`{}`))
		parseDone <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Parse did not start")
	}

	requireCompleteWithin(t, time.Second, func() {
		for p.totalInFlight() != 1 {
			time.Sleep(10 * time.Millisecond)
		}
	})

	close(release)

	select {
	case err := <-parseDone:
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Parse did not finish")
	}

	if got := p.totalInFlight(); got != 0 {
		t.Fatalf("in-flight count = %d, want 0", got)
	}
}

// TestShutdownWaitsForInFlightParse verifies that graceful shutdown waits for
// Parse requests that are already running.
func TestShutdownWaitsForInFlightParse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.parseFn = func(context.Context, string, []byte) (*workerplugin.ParseResult, error) {
			close(started)
			<-release
			return &workerplugin.ParseResult{Data: []byte(`"parsed"`)}, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	parseDone := make(chan error, 1)
	go func() {
		_, err := p.Parse(context.Background(), "Test", []byte(`{}`))
		parseDone <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Parse did not start")
	}

	requireCompleteWithin(t, time.Second, func() {
		for p.totalInFlight() != 1 {
			time.Sleep(10 * time.Millisecond)
		}
	})

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownDone <- p.Shutdown(ctx)
	}()

	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before Parse completed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-parseDone:
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Parse did not finish")
	}

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not finish after Parse completed")
	}
}

// TestCheckHungRequestsIgnoresTrackedParse verifies that parse attempts are
// tracked for drain/cancel accounting without being mistaken for first-byte
// hung stream requests.
func TestCheckHungRequestsIgnoresTrackedParse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	killed := make(chan struct{}, 1)

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.parseFn = func(context.Context, string, []byte) (*workerplugin.ParseResult, error) {
			close(started)
			<-release
			return &workerplugin.ParseResult{Data: []byte(`"parsed"`)}, nil
		}
		w.closeFn = func() error {
			select {
			case killed <- struct{}{}:
			default:
			}
			return nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer func() {
		p.workers[0].worker.(*mockWorker).closeFn = nil
		_ = p.Close()
	}()
	p.SetFirstByteTimeout(time.Millisecond)

	parseDone := make(chan error, 1)
	go func() {
		_, err := p.Parse(context.Background(), "Test", []byte(`{}`))
		parseDone <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Parse did not start")
	}

	time.Sleep(20 * time.Millisecond)
	p.checkHungRequests()

	select {
	case <-killed:
		t.Fatal("parse request should not be treated as hung")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-parseDone:
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Parse did not finish")
	}
}

// ---------------------------------------------------------------------------
// Tests: CallStream / Call retry
// ---------------------------------------------------------------------------

// TestCallRetryAfterRestart verifies that Call (which wraps CallStream)
// transparently retries when the underlying worker dies.
func TestCallRetryAfterRestart(t *testing.T) {
	failWorker := newMockWorker()
	failWorker.callStreamFn = func(context.Context, string, []byte, bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
		return nil, unavailableErr()
	}

	var n atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		if n.Add(1) == 1 {
			return newMockHandle(id, failWorker), nil
		}
		return newMockHandle(id, newMockWorker()), nil // default: returns Final "stream_ok"
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	result, err := p.Call(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeCall)
	if err != nil {
		t.Fatalf("Call should succeed after retry: %v", err)
	}
	if string(result.Data) != `"stream_ok"` {
		t.Errorf("unexpected data: %s", result.Data)
	}
}

// TestCallStreamMidStreamRetry verifies retry when a worker dies after
// sending partial results: a reset event should be injected.
func TestCallStreamMidStreamRetry(t *testing.T) {
	failWorker := newMockWorker()
	failWorker.callStreamFn = func(ctx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
		ch := make(chan *workerplugin.StreamResult, 2)
		// Send one partial, then a retryable error.
		partial := workerplugin.GetStreamResult()
		partial.Kind = workerplugin.StreamResultKindStream
		partial.Data = []byte(`"partial"`)
		ch <- partial

		errResult := workerplugin.GetStreamResult()
		errResult.Kind = workerplugin.StreamResultKindError
		errResult.Error = unavailableErr()
		ch <- errResult
		close(ch)
		return ch, nil
	}

	var n atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		if n.Add(1) == 1 {
			return newMockHandle(id, failWorker), nil
		}
		return newMockHandle(id, newMockWorker()), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	var gotReset, gotFinal bool
	for r := range results {
		if r.Reset {
			gotReset = true
		}
		if r.Kind == workerplugin.StreamResultKindFinal {
			gotFinal = true
		}
		workerplugin.ReleaseStreamResult(r)
	}

	if !gotReset {
		t.Error("expected reset event after mid-stream retry")
	}
	if !gotFinal {
		t.Error("expected final result after retry")
	}
}

// ---------------------------------------------------------------------------
// Tests: sequential restarts
// ---------------------------------------------------------------------------

// TestSequentialRestarts verifies that the pool recovers from multiple
// consecutive failures (die → replace → die → replace → success).
func TestSequentialRestarts(t *testing.T) {
	var callNum atomic.Int32

	factory := func(id int) (*workerHandle, error) {
		n := callNum.Add(1)
		w := newMockWorker()
		if n <= 2 {
			// First two workers (initial + first replacement) fail on Parse.
			w.parseFn = func(context.Context, string, []byte) (*workerplugin.ParseResult, error) {
				return nil, unavailableErr()
			}
		}
		// Third worker uses default success behavior.
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	result, err := p.Parse(context.Background(), "Test", []byte(`{}`))
	if err != nil {
		t.Fatalf("Parse should succeed after sequential restarts: %v", err)
	}
	if string(result.Data) != `"ok"` {
		t.Errorf("unexpected data: %s", result.Data)
	}
	// Initial (1) + first replacement (2) + second replacement (3) = 3
	if n := callNum.Load(); n != 3 {
		t.Errorf("expected 3 factory calls (2 restarts), got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Tests: isRetryableWorkerError
// ---------------------------------------------------------------------------

func TestIsRetryableWorkerError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"Unavailable", status.Error(codes.Unavailable, "worker crashed"), true},
		{"Canceled", status.Error(codes.Canceled, "request canceled"), true},
		{"DeadlineExceeded", status.Error(codes.DeadlineExceeded, "request timed out"), true},
		{"InvalidArgument", status.Error(codes.InvalidArgument, "bad input"), false},
		{"Internal", status.Error(codes.Internal, "runtime panic"), false},
		{"NotFound", status.Error(codes.NotFound, "method missing"), false},
		{"OK", status.Error(codes.OK, ""), false},
		{"connection reset string", errors.New("connection reset by peer"), true},
		{"EOF string", errors.New("error reading from server: EOF"), true},
		{"transport closing string", errors.New("transport is closing"), true},
		{"code = Unavailable string", errors.New("rpc error: code = Unavailable desc = gone"), true},
		// Serialized cancellation errors (lost gRPC status across boundary)
		{"code = Canceled string", errors.New("rpc error: code = Canceled desc = request canceled"), true},
		{"code = DeadlineExceeded string", errors.New("rpc error: code = DeadlineExceeded desc = timeout"), true},
		// Plain cancellation strings are NOT retryable — they can come from
		// application-level errors (e.g. upstream LLM timeout). Only the
		// serialized gRPC forms above indicate transport-level cancellation.
		{"plain context.Canceled", fmt.Errorf("context canceled"), false},
		{"plain context.DeadlineExceeded", fmt.Errorf("context deadline exceeded"), false},
		{"wrapped context.Canceled", fmt.Errorf("stream failed: context canceled"), false},
		{"plain error", errors.New("something broke"), false},
		// Regression: gRPC status is authoritative — non-retryable codes must
		// not be misclassified just because the message text contains
		// cancellation strings. Infrastructure patterns ("connection reset",
		// "transport is closing") are still checked because they indicate
		// transport failures regardless of the reported gRPC code.
		{"Internal with context canceled message", status.Error(codes.Internal, "context canceled"), false},
		{"Internal with deadline exceeded message", status.Error(codes.Internal, "context deadline exceeded"), false},
		{"InvalidArgument with context canceled message", status.Error(codes.InvalidArgument, "context canceled"), false},
		// Regression: serialized gRPC errors (lost *status.Status across
		// boundary) — the top-level serialized code is authoritative.
		{"serialized Internal with context canceled", fmt.Errorf("rpc error: code = Internal desc = context canceled"), false},
		{"serialized Internal with deadline exceeded", fmt.Errorf("rpc error: code = Internal desc = context deadline exceeded"), false},
		{"serialized NotFound with context canceled", fmt.Errorf("rpc error: code = NotFound desc = context canceled"), false},
		// Regression: nested serialized gRPC errors — only the top-level code matters.
		{"nested Internal over Canceled", fmt.Errorf("rpc error: code = Internal desc = rpc error: code = Canceled desc = context canceled"), false},
		{"nested Internal over Unavailable", fmt.Errorf("rpc error: code = Internal desc = rpc error: code = Unavailable desc = connection refused"), false},
		{"nested Canceled over Internal", fmt.Errorf("rpc error: code = Canceled desc = rpc error: code = Internal desc = something"), true},
		{"nested Unavailable over Internal", fmt.Errorf("rpc error: code = Unavailable desc = rpc error: code = Internal desc = something"), true},
		{"wrapped Unavailable message", status.Error(codes.Unknown, "connection reset"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableWorkerError(tt.err); got != tt.want {
				t.Errorf("isRetryableWorkerError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: isCallerCancellationError
// ---------------------------------------------------------------------------

func TestIsCallerCancellationError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context.Canceled", context.Canceled, true},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"wrapped context.Canceled", fmt.Errorf("call failed: %w", context.Canceled), true},
		{"gRPC Canceled", status.Error(codes.Canceled, "request canceled"), true},
		{"gRPC DeadlineExceeded", status.Error(codes.DeadlineExceeded, "deadline"), true},
		{"gRPC Internal", status.Error(codes.Internal, "runtime panic"), false},
		{"gRPC Unavailable", status.Error(codes.Unavailable, "worker crashed"), false},
		// Regression: Unavailable with cancellation text in message must NOT
		// be treated as caller cancellation — it's a dead worker whose
		// transport teardown message happens to contain "context canceled".
		{"gRPC Unavailable with context canceled message", status.Error(codes.Unavailable, "transport: context canceled"), false},
		{"gRPC Unavailable with deadline exceeded message", status.Error(codes.Unavailable, "context deadline exceeded"), false},
		// String-serialized gRPC errors (lost status object across gRPC boundary,
		// but the "rpc error: code = ..." format is preserved in the text).
		{"serialized gRPC Canceled", fmt.Errorf("rpc error: code = Canceled desc = request canceled"), true},
		{"serialized gRPC DeadlineExceeded", fmt.Errorf("rpc error: code = DeadlineExceeded desc = timeout"), true},
		// Regression: serialized Unavailable with cancellation text must NOT
		// be treated as caller cancellation — the gRPC code is authoritative.
		{"serialized gRPC Unavailable with context canceled", fmt.Errorf("rpc error: code = Unavailable desc = context canceled"), false},
		{"serialized gRPC Internal with deadline text", fmt.Errorf("rpc error: code = Internal desc = context deadline exceeded"), false},
		{"wrapped serialized gRPC Unavailable", fmt.Errorf("call failed: rpc error: code = Unavailable desc = context canceled"), false},
		// Regression: nested serialized gRPC errors — only the top-level code matters.
		{"nested Internal over Canceled", fmt.Errorf("rpc error: code = Internal desc = rpc error: code = Canceled desc = context canceled"), false},
		{"nested Canceled over Internal", fmt.Errorf("rpc error: code = Canceled desc = rpc error: code = Internal desc = something"), true},
		// Plain string-serialized cancellation errors (no gRPC structure at all)
		{"string context canceled", fmt.Errorf("context canceled"), true},
		{"string context deadline exceeded", fmt.Errorf("context deadline exceeded"), true},
		{"wrapped string context canceled", fmt.Errorf("stream failed: context canceled"), true},
		{"wrapped string deadline", fmt.Errorf("call failed: context deadline exceeded"), true},
		{"plain error", fmt.Errorf("something broke"), false},
		{"unrelated cancel substring", fmt.Errorf("cancelled by admin"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCallerCancellationError(tt.err); got != tt.want {
				t.Errorf("isCallerCancellationError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: getWorker / getWorkerForRetry
// ---------------------------------------------------------------------------

// TestGetWorkerRoundRobin verifies round-robin distributes across all workers.
func TestGetWorkerRoundRobin(t *testing.T) {
	p := newTestPool(t, 3, goodFactory)
	defer p.Close()

	seen := make(map[int]int)
	for i := 0; i < 30; i++ {
		h, err := p.getWorker()
		if err != nil {
			t.Fatalf("getWorker: %v", err)
		}
		seen[h.id]++
	}

	if len(seen) != 3 {
		t.Errorf("expected 3 distinct workers, got %d", len(seen))
	}
	// Each should get exactly 10 (30 / 3).
	for id, count := range seen {
		if count != 10 {
			t.Errorf("worker %d got %d requests, expected 10", id, count)
		}
	}
}

// TestGetWorkerPoolClosedAndDraining verifies getWorker errors on closed/draining pools.
func TestGetWorkerPoolClosedAndDraining(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)

	// Draining rejects new requests.
	p.draining.Store(true)
	if _, err := p.getWorker(); err == nil {
		t.Error("getWorker should fail when draining")
	}
	p.draining.Store(false)

	// Closed pool rejects new requests.
	p.Close()
	if _, err := p.getWorker(); err == nil {
		t.Error("getWorker should fail when closed")
	}
}

// TestGetWorkerForRetryDifferentWorker verifies that with pool > 1,
// getWorkerForRetry completes quickly (returns a different worker
// via round-robin without blocking on a restart).
func TestGetWorkerForRetryDifferentWorker(t *testing.T) {
	p := newTestPool(t, 2, goodFactory)
	defer p.Close()

	failed := p.workers[0]

	requireCompleteWithin(t, time.Second, func() {
		h, err := p.getWorkerForRetry(context.Background(), failed)
		if err != nil {
			t.Errorf("getWorkerForRetry: %v", err)
			return
		}
		// With 2 workers, round-robin should hand back the other one.
		if h == failed {
			t.Log("getWorkerForRetry returned same handle (restarted) — still OK")
		}
	})
}

// TestGetWorkerForRetryAllDead verifies error propagation when every
// worker is unhealthy and replacement fails.
func TestGetWorkerForRetryAllDead(t *testing.T) {
	p := newTestPool(t, 2, goodFactory)
	defer p.Close()

	// Mark all workers unhealthy AND make the factory fail so the
	// await-restart path can't recover.
	for _, h := range p.workers {
		h.healthy.Store(false)
	}
	p.newWorker = func(id int) (*workerHandle, error) {
		return nil, fmt.Errorf("startup failure")
	}

	_, err := p.getWorkerForRetry(context.Background(), p.workers[0])
	if err == nil {
		t.Error("getWorkerForRetry should fail when all workers are dead and restart fails")
	}
}

// TestGetWorkerForRetryWaitsForRestart verifies that with pool size 1,
// if the only worker is unhealthy and a restart is in progress,
// getWorkerForRetry waits for the restart and then returns the new worker.
func TestGetWorkerForRetryWaitsForRestart(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	failed := p.workers[0]

	// Mark unhealthy (simulating what killWorkerAndRetry does).
	failed.healthy.Store(false)

	// getWorkerForRetry should wait for restart, then return the new worker.
	requireCompleteWithin(t, 2*time.Second, func() {
		h, err := p.getWorkerForRetry(context.Background(), failed)
		if err != nil {
			t.Errorf("expected recovery after restart, got: %v", err)
			return
		}
		if h == failed {
			t.Error("should return the new replacement, not the failed handle")
		}
		if !h.healthy.Load() {
			t.Error("replacement should be healthy")
		}
	})
}

// TestGetWorkerForRetryWaitsForPendingAsyncRestart verifies that a new request
// waits for an actual async restart dispatch instead of failing during the
// publication window before restartDone becomes visible.
func TestGetWorkerForRetryWaitsForPendingAsyncRestart(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	failed := p.workers[0]
	failed.healthy.Store(false)
	p.dispatchRestart(failed)

	requireCompleteWithin(t, 2*time.Second, func() {
		h, err := p.getWorkerForRetry(context.Background(), nil)
		if err != nil {
			t.Fatalf("expected recovery after published async restart, got: %v", err)
		}
		if h == failed {
			t.Error("should return the replacement, not the failed handle")
		}
		if !h.healthy.Load() {
			t.Error("replacement should be healthy")
		}
	})
}

// TestAwaitRestartPendingPathRespectsCancellation verifies the slow path does
// not spawn replacement work when only restartPending is visible.
func TestAwaitRestartPendingPathRespectsCancellation(t *testing.T) {
	var startCalls atomic.Int32
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	p.newWorker = func(id int) (*workerHandle, error) {
		startCalls.Add(1)
		return newMockHandle(id, newMockWorker()), nil
	}

	failed := p.workers[0]
	failed.healthy.Store(false)
	failed.restartPending.Add(1)
	defer failed.restartPending.Add(-1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	requireCompleteWithin(t, time.Second, func() {
		err := p.awaitRestart(ctx, failed)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context deadline exceeded, got %v", err)
		}
	})

	if got := startCalls.Load(); got != 0 {
		t.Fatalf("awaitRestart spawned %d replacement(s); want 0", got)
	}
}

// TestAwaitAnyRestartPendingPathRespectsCancellation verifies fan-in waiting
// does not fan out restart attempts when only restartPending is visible.
func TestAwaitAnyRestartPendingPathRespectsCancellation(t *testing.T) {
	var startCalls atomic.Int32
	p := newTestPool(t, 4, goodFactory)
	defer p.Close()

	p.newWorker = func(id int) (*workerHandle, error) {
		startCalls.Add(1)
		return newMockHandle(id, newMockWorker()), nil
	}

	var cleanup []*workerHandle
	for _, failed := range p.workers {
		failed.healthy.Store(false)
		failed.restartPending.Add(1)
		cleanup = append(cleanup, failed)
	}
	defer func() {
		for _, failed := range cleanup {
			failed.restartPending.Add(-1)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	requireCompleteWithin(t, time.Second, func() {
		err := p.awaitAnyRestart(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context deadline exceeded, got %v", err)
		}
	})

	if got := startCalls.Load(); got != 0 {
		t.Fatalf("awaitAnyRestart spawned %d replacement(s); want 0", got)
	}
}

// TestAwaitAnyRestartReturnsFirstCompletionAcrossMixedStates verifies that a
// pending-only restart cannot be hidden behind another worker's published
// restartDone channel.
func TestAwaitAnyRestartReturnsFirstCompletionAcrossMixedStates(t *testing.T) {
	p := newTestPool(t, 2, goodFactory)
	defer p.Close()

	slow := p.workers[0]
	fast := p.workers[1]

	slow.restartMu.Lock()
	slow.restartDone = make(chan struct{})
	slow.restartMu.Unlock()
	defer func() {
		slow.restartMu.Lock()
		if slow.restartDone != nil {
			close(slow.restartDone)
			slow.restartDone = nil
		}
		slow.restartMu.Unlock()
	}()

	fast.restartPending.Add(1)

	done := make(chan error, 1)
	go func() {
		done <- p.awaitAnyRestart(context.Background())
	}()

	// Give awaitAnyRestart time to observe both restart states. The old
	// implementation blocks on slow.restartDone here and misses fast's
	// earlier completion entirely.
	time.Sleep(50 * time.Millisecond)
	fast.restartPending.Add(-1)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("awaitAnyRestart returned error: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("awaitAnyRestart blocked on slower published restart")
	}
}

// TestAwaitRestartReturnsOnDrain verifies retry waiters stop promptly when the
// pool begins draining instead of waiting for restart completion.
func TestAwaitRestartReturnsOnDrain(t *testing.T) {
	var startCalls atomic.Int32
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	p.newWorker = func(id int) (*workerHandle, error) {
		startCalls.Add(1)
		return newMockHandle(id, newMockWorker()), nil
	}

	failed := p.workers[0]
	p.draining.Store(true)
	p.drainOnce.Do(func() { close(p.drainCh) })

	err := p.awaitRestart(context.Background(), failed)
	if err == nil || err.Error() != "pool is draining" {
		t.Fatalf("expected pool is draining error, got %v", err)
	}
	if got := startCalls.Load(); got != 0 {
		t.Fatalf("awaitRestart spawned %d replacement(s); want 0", got)
	}
}

// TestDispatchRestartPublishesPendingState verifies the actual async dispatch
// helper publishes pending restart state before handing control to the
// goroutine, so new requests do not fail during the dispatch gap.
func TestDispatchRestartPublishesPendingState(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	failed := p.workers[0]
	failed.healthy.Store(false)

	blockedFactory := make(chan struct{})
	p.newWorker = func(id int) (*workerHandle, error) {
		<-blockedFactory
		return newMockHandle(id, newMockWorker()), nil
	}

	p.dispatchRestart(failed)

	recoveryDone := make(chan struct{})
	go func() {
		defer close(recoveryDone)
		h, err := p.getWorkerForRetry(context.Background(), nil)
		if err != nil {
			t.Errorf("expected recovery after dispatchRestart, got: %v", err)
			return
		}
		if h == failed {
			t.Error("should return the replacement, not the failed handle")
		}
	}()

	select {
	case <-recoveryDone:
		t.Fatal("getWorkerForRetry returned before replacement was allowed to start")
	case <-time.After(100 * time.Millisecond):
	}

	close(blockedFactory)

	select {
	case <-recoveryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("getWorkerForRetry did not recover after replacement was released")
	}
}

// TestDispatchRestartConcurrentWaiters verifies that multiple new requests can
// wait through the async restart publication window and all recover once the
// replacement becomes available.
func TestDispatchRestartConcurrentWaiters(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	failed := p.workers[0]
	failed.healthy.Store(false)

	blockedFactory := make(chan struct{})
	p.newWorker = func(id int) (*workerHandle, error) {
		<-blockedFactory
		return newMockHandle(id, newMockWorker()), nil
	}

	p.dispatchRestart(failed)

	const waiters = 16
	errCh := make(chan error, waiters)
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := p.getWorkerForRetry(context.Background(), nil)
			if err != nil {
				errCh <- fmt.Errorf("getWorkerForRetry: %w", err)
				return
			}
			if h == failed {
				errCh <- fmt.Errorf("waiter received failed handle")
				return
			}
			if !h.healthy.Load() {
				errCh <- fmt.Errorf("waiter received unhealthy replacement")
			}
		}()
	}

	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		wg.Wait()
	}()

	select {
	case <-waitDone:
		t.Fatal("waiters returned before replacement was allowed to start")
	case <-time.After(100 * time.Millisecond):
	}

	close(blockedFactory)

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waiters did not recover after replacement was released")
	}
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestGetWorkerForRetryRespectsContext verifies that getWorkerForRetry
// returns promptly when the caller's context is cancelled, even if
// the restart would otherwise block (slow factory).
func TestGetWorkerForRetryRespectsContext(t *testing.T) {
	slowFactory := func(id int) (*workerHandle, error) {
		// Simulate a very slow worker startup.
		time.Sleep(10 * time.Second)
		return newMockHandle(id, newMockWorker()), nil
	}

	p := newTestPool(t, 1, goodFactory) // initial workers start fast
	defer p.Close()

	// Swap factory to slow one for the restart path.
	p.newWorker = slowFactory
	failed := p.workers[0]

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	requireCompleteWithin(t, 2*time.Second, func() {
		_, err := p.getWorkerForRetry(ctx, failed)
		if err == nil {
			t.Error("expected context error, got nil")
		}
		if ctx.Err() == nil {
			t.Error("context should be cancelled")
		}
	})
}

// ---------------------------------------------------------------------------
// Tests: Parse edge cases
// ---------------------------------------------------------------------------

// TestParseContextCancelled verifies that Parse returns immediately when
// the caller's context is cancelled — no retries attempted.
func TestParseContextCancelled(t *testing.T) {
	var parseCalls atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.parseFn = func(ctx context.Context, _ string, _ []byte) (*workerplugin.ParseResult, error) {
			parseCalls.Add(1)
			return nil, ctx.Err()
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := p.Parse(ctx, "Test", []byte(`{}`))
	if err == nil {
		t.Fatal("Parse should fail with cancelled context")
	}
	if parseCalls.Load() != 1 {
		t.Errorf("expected exactly 1 parse call (no retry), got %d", parseCalls.Load())
	}
}

// TestParseNonRetryableError verifies that a non-retryable error (e.g.
// InvalidArgument) is returned immediately without triggering restart.
func TestParseNonRetryableError(t *testing.T) {
	var parseCalls atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.parseFn = func(_ context.Context, _ string, _ []byte) (*workerplugin.ParseResult, error) {
			parseCalls.Add(1)
			return nil, status.Error(codes.InvalidArgument, "bad input")
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	_, err := p.Parse(context.Background(), "Test", []byte(`{}`))
	if err == nil {
		t.Fatal("Parse should fail with non-retryable error")
	}
	if parseCalls.Load() != 1 {
		t.Errorf("expected exactly 1 parse call (no retry for non-retryable), got %d", parseCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// Tests: CallStream edge cases
// ---------------------------------------------------------------------------

// TestCallStreamUnexpectedEOF verifies that when the worker channel closes
// without a terminal result (Final or Error), CallStream retries and
// eventually succeeds.
func TestCallStreamUnexpectedEOF(t *testing.T) {
	var callCount atomic.Int32

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			n := callCount.Add(1)
			ch := make(chan *workerplugin.StreamResult, 2)
			if n == 1 {
				// First attempt: partial then EOF (no terminal).
				partial := workerplugin.GetStreamResult()
				partial.Kind = workerplugin.StreamResultKindStream
				partial.Data = []byte(`"partial"`)
				ch <- partial
				close(ch)
			} else {
				// Retry: clean Final.
				final := workerplugin.GetStreamResult()
				final.Kind = workerplugin.StreamResultKindFinal
				final.Data = []byte(`"done"`)
				ch <- final
				close(ch)
			}
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	var gotFinal bool
	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			if r.Kind == workerplugin.StreamResultKindFinal {
				gotFinal = true
			}
			workerplugin.ReleaseStreamResult(r)
		}
	})

	if !gotFinal {
		t.Error("expected final result after unexpected EOF retry")
	}
}

// TestCallStreamExhaustsRetries verifies that when every attempt fails,
// the consumer receives an error result.
func TestCallStreamExhaustsRetries(t *testing.T) {
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			return nil, unavailableErr()
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup should succeed: %v", err)
	}

	var gotError bool
	requireCompleteWithin(t, 10*time.Second, func() {
		for r := range results {
			if r.Kind == workerplugin.StreamResultKindError {
				gotError = true
			}
			workerplugin.ReleaseStreamResult(r)
		}
	})

	if !gotError {
		t.Error("expected error after exhausting all retries")
	}
}

// TestCallStreamNonRetryableError verifies that a non-retryable error
// before streaming starts is forwarded to the consumer with no retry.
func TestCallStreamNonRetryableError(t *testing.T) {
	var callCounts atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			callCounts.Add(1)
			return nil, status.Error(codes.InvalidArgument, "bad request")
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup should succeed: %v", err)
	}

	var gotError bool
	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			if r.Kind == workerplugin.StreamResultKindError {
				gotError = true
			}
			workerplugin.ReleaseStreamResult(r)
		}
	})

	if !gotError {
		t.Error("expected non-retryable error forwarded to consumer")
	}
	if c := callCounts.Load(); c != 1 {
		t.Errorf("expected 1 call (no retry for non-retryable), got %d", c)
	}
}

// TestCallStreamContextCancelled verifies that when the parent context is
// cancelled, the stream goroutine terminates promptly without hanging.
func TestCallStreamContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(fnCtx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult)
			// Block until context cancelled — simulates a long-running LLM call.
			go func() {
				<-fnCtx.Done()
				close(ch)
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// Cancel immediately.
	cancel()

	// The results channel must close promptly — no hang.
	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})
}

// TestCallStreamContextCancelledStillRestartsRetryableError verifies that a
// real worker failure still schedules a restart even if the caller cancels.
func TestCallStreamContextCancelledStillRestartsRetryableError(t *testing.T) {
	ctx := newManualCancelContext()
	restarted := make(chan struct{}, 1)
	var restartSignal sync.Once
	releaseErr := make(chan struct{})
	errSent := make(chan struct{})

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 1)
			go func() {
				<-releaseErr
				errResult := workerplugin.GetStreamResult()
				errResult.Kind = workerplugin.StreamResultKindError
				errResult.Error = status.Error(codes.Unavailable, "worker died")
				ch <- errResult
				close(errSent)
				close(ch)
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	p.newWorker = func(id int) (*workerHandle, error) {
		restartSignal.Do(func() { close(restarted) })
		return newMockHandle(id, newMockWorker()), nil
	}
	defer p.Close()

	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// Buffer the error BEFORE cancelling the context so both ctx.Done()
	// and the result channel are simultaneously ready in the streamLoop
	// select. Whichever branch wins, the restart must still be dispatched.
	close(releaseErr)
	<-errSent
	ctx.cancel(context.Canceled)

	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("worker should restart after retryable failure even if caller canceled")
	}
}

// TestCallStreamMidStreamRetryableErrorWithCallerCancel verifies that when
// a retryable mid-stream error (Unavailable) wins the select race against
// ctx.Done(), the worker IS restarted but NO second attempt is started.
// Without the ctx.Err() check after restart dispatch, the cancelled caller
// would trigger a wasted retry on another worker.
func TestCallStreamMidStreamRetryableErrorWithCallerCancel(t *testing.T) {
	ctx := newManualCancelContext()
	restarted := make(chan struct{}, 1)
	var restartSignal sync.Once
	releaseErr := make(chan struct{})
	errSent := make(chan struct{})
	var attempts atomic.Int32

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			attempts.Add(1)
			ch := make(chan *workerplugin.StreamResult, 1)
			go func() {
				<-releaseErr
				errResult := workerplugin.GetStreamResult()
				errResult.Kind = workerplugin.StreamResultKindError
				errResult.Error = status.Error(codes.Unavailable, "worker died")
				ch <- errResult
				close(errSent)
				close(ch)
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	p.newWorker = func(id int) (*workerHandle, error) {
		restartSignal.Do(func() { close(restarted) })
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			attempts.Add(1)
			ch := make(chan *workerplugin.StreamResult, 1)
			close(ch)
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}
	defer p.Close()

	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// Cancel FIRST, then release the error. This guarantees ctx.Err()
	// is non-nil by the time the streamLoop processes the Unavailable
	// error, regardless of goroutine scheduling. Without this ordering,
	// the streamLoop can race through shouldRetry → cleanup →
	// dispatchRestart → ctx.Err() check before cancel() is called.
	ctx.cancel(context.Canceled)
	close(releaseErr)

	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})

	// Restart must happen (dead worker).
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("worker should restart after mid-stream Unavailable even if caller canceled")
	}

	// By the time results is drained and closed, the CallStream
	// goroutine has exited. No need for a sleep.
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt, got %d (cancelled caller triggered a wasted retry)", got)
	}
}

// TestCallStreamContextCancelledDoesNotRestartOnCanceledError verifies that a
// cancellation race with a canceled stream error also avoids a restart.
func TestCallStreamContextCancelledDoesNotRestartOnCanceledError(t *testing.T) {
	ctx := newManualCancelContext()
	restarted := make(chan struct{}, 1)
	var restartSignal sync.Once
	releaseErr := make(chan struct{})
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 1)
			go func() {
				<-releaseErr
				errResult := workerplugin.GetStreamResult()
				errResult.Kind = workerplugin.StreamResultKindError
				errResult.Error = status.Error(codes.Canceled, "request canceled")
				ch <- errResult
				close(ch)
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	original := p.workers[0]
	p.newWorker = func(id int) (*workerHandle, error) {
		restartSignal.Do(func() { close(restarted) })
		return newMockHandle(id, newMockWorker()), nil
	}
	defer p.Close()

	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// Cancel context FIRST, then release the error. This ensures
	// ctx.Err() is non-nil when the Canceled error is processed.
	// Regardless of which select branch wins (ctx.Done() or results),
	// no restart should happen: codes.Canceled + cancelled context
	// is a caller cancellation, not a worker failure.
	ctx.cancel(context.Canceled)
	close(releaseErr)

	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})

	select {
	case <-restarted:
		t.Fatal("worker should not restart after client cancellation")
	case <-time.After(200 * time.Millisecond):
	}

	if p.workers[0] != original {
		t.Fatal("worker should not be replaced after client cancellation")
	}
}

// TestCallStreamClientCancelMidStreamDoesNotRestartWorker verifies that a
// client-side cancellation surfaced as a mid-stream canceled error does not
// mark the worker unhealthy or trigger a restart.
func TestCallStreamClientCancelMidStreamDoesNotRestartWorker(t *testing.T) {
	var factoryCalls atomic.Int32
	releaseCancelErr := make(chan struct{})

	factory := func(id int) (*workerHandle, error) {
		factoryCalls.Add(1)
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 2)

			partial := workerplugin.GetStreamResult()
			partial.Kind = workerplugin.StreamResultKindStream
			partial.Data = []byte(`"partial"`)
			ch <- partial

			go func() {
				<-releaseCancelErr
				errResult := workerplugin.GetStreamResult()
				errResult.Kind = workerplugin.StreamResultKindError
				errResult.Error = status.Error(codes.Canceled, "request canceled")
				ch <- errResult
				close(ch)
			}()

			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()
	original := p.workers[0]
	factoryCalls.Store(0)

	ctx := newManualCancelContext()
	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	select {
	case result, ok := <-results:
		if !ok {
			t.Fatal("stream closed before first result")
		}
		workerplugin.ReleaseStreamResult(result)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first stream result")
	}

	ctx.cancel(context.Canceled)
	close(releaseCancelErr)

	requireCompleteWithin(t, 2*time.Second, func() {
		for result := range results {
			workerplugin.ReleaseStreamResult(result)
		}
	})
	requireCompleteWithin(t, time.Second, func() {
		p.restartWG.Wait()
	})

	if calls := factoryCalls.Load(); calls != 0 {
		t.Fatalf("expected no restart after client cancellation, got %d replacement starts", calls)
	}
	if p.workers[0] != original {
		t.Fatal("worker should not be replaced after client cancellation")
	}
	if !original.healthy.Load() {
		t.Fatal("worker should remain healthy after client cancellation")
	}
}

// TestCallStreamClientCancelUnexpectedCloseDoesNotRestartWorker verifies that
// a client-side cancellation followed by stream closure without a terminal
// result does not trigger retry/restart.
func TestCallStreamClientCancelUnexpectedCloseDoesNotRestartWorker(t *testing.T) {
	var factoryCalls atomic.Int32
	releaseClose := make(chan struct{})

	factory := func(id int) (*workerHandle, error) {
		factoryCalls.Add(1)
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 1)

			partial := workerplugin.GetStreamResult()
			partial.Kind = workerplugin.StreamResultKindStream
			partial.Data = []byte(`"partial"`)
			ch <- partial

			go func() {
				<-releaseClose
				close(ch)
			}()

			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()
	original := p.workers[0]
	factoryCalls.Store(0)

	ctx := newManualCancelContext()
	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	select {
	case result, ok := <-results:
		if !ok {
			t.Fatal("stream closed before first result")
		}
		workerplugin.ReleaseStreamResult(result)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first stream result")
	}

	ctx.cancel(context.Canceled)
	close(releaseClose)

	requireCompleteWithin(t, 2*time.Second, func() {
		for result := range results {
			workerplugin.ReleaseStreamResult(result)
		}
	})
	requireCompleteWithin(t, time.Second, func() {
		p.restartWG.Wait()
	})

	if calls := factoryCalls.Load(); calls != 0 {
		t.Fatalf("expected no restart after client cancellation, got %d replacement starts", calls)
	}
	if p.workers[0] != original {
		t.Fatal("worker should not be replaced after client cancellation")
	}
	if !original.healthy.Load() {
		t.Fatal("worker should remain healthy after client cancellation")
	}
}

// TestCallStreamCallerCancelWithNonRetryableSetupError verifies that
// a caller cancellation racing with a non-retryable setup error (like
// InvalidArgument) does NOT trigger a spurious worker restart. The
// worker is healthy — only the request was bad.
func TestCallStreamCallerCancelWithNonRetryableSetupError(t *testing.T) {
	ctx := newManualCancelContext()
	restarted := make(chan struct{}, 1)
	var restartSignal sync.Once

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(fnCtx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			// Block until context is cancelled, then return a non-retryable error.
			<-fnCtx.Done()
			return nil, status.Error(codes.InvalidArgument, "bad input")
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	original := p.workers[0]
	p.newWorker = func(id int) (*workerHandle, error) {
		restartSignal.Do(func() { close(restarted) })
		return newMockHandle(id, newMockWorker()), nil
	}
	defer p.Close()

	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// Cancel — the mock returns InvalidArgument.
	ctx.cancel(context.Canceled)

	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})

	select {
	case <-restarted:
		t.Fatal("worker should not restart after non-retryable setup error + caller cancel")
	case <-time.After(200 * time.Millisecond):
	}

	if p.workers[0] != original {
		t.Fatal("worker should not be replaced after non-retryable setup error")
	}
}

// TestCallStreamSetupUnavailableWithCallerCancel verifies that when
// worker.CallStream itself returns Unavailable (worker dead) and the
// caller's context is also cancelled, the worker is still restarted.
// This exercises the setup-path fix: restart is dispatched before the
// ctx.Err() early return.
func TestCallStreamSetupUnavailableWithCallerCancel(t *testing.T) {
	ctx := newManualCancelContext()
	restarted := make(chan struct{}, 1)
	var restartSignal sync.Once

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(fnCtx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			// Block until context is cancelled, then return Unavailable
			// as if the worker process died.
			<-fnCtx.Done()
			return nil, status.Error(codes.Unavailable, "worker died")
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	p.newWorker = func(id int) (*workerHandle, error) {
		restartSignal.Do(func() { close(restarted) })
		return newMockHandle(id, newMockWorker()), nil
	}
	defer p.Close()

	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// Cancel the context — unblocks the factory's CallStream which
	// returns Unavailable. The setup-path fix must dispatch restart
	// before the ctx.Err() early return.
	ctx.cancel(context.Canceled)

	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("worker should restart after Unavailable even when caller cancelled")
	}
}

// TestCallStreamDrainCatchesSecondInSequenceError verifies that when
// ctx.Done() wins the select and a non-error result is buffered before
// the retryable error, the restart-aware drain reads past the non-error
// result and still dispatches a restart for the retryable error.
func TestCallStreamDrainCatchesSecondInSequenceError(t *testing.T) {
	ctx := newManualCancelContext()
	restarted := make(chan struct{}, 1)
	var restartSignal sync.Once
	releaseErr := make(chan struct{})

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 2)
			go func() {
				<-releaseErr
				// Non-error result first
				partial := workerplugin.GetStreamResult()
				partial.Kind = workerplugin.StreamResultKindStream
				partial.Data = []byte(`"partial"`)
				ch <- partial
				// Retryable error second
				errResult := workerplugin.GetStreamResult()
				errResult.Kind = workerplugin.StreamResultKindError
				errResult.Error = status.Error(codes.Unavailable, "worker died")
				ch <- errResult
				close(ch)
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	p.newWorker = func(id int) (*workerHandle, error) {
		restartSignal.Do(func() { close(restarted) })
		return newMockHandle(id, newMockWorker()), nil
	}
	defer p.Close()

	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// Cancel context BEFORE releasing results. The streamLoop select
	// sees only ctx.Done() (results channel is empty) and takes it.
	// The drain goroutine then reads both the partial and the error.
	ctx.cancel(context.Canceled)
	close(releaseErr)

	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("drain should catch retryable error after non-error result")
	}
}

// TestCallStreamDrainCatchesDelayedError verifies that when ctx.Done()
// wins and the retryable error arrives after the select (not simultaneously
// buffered), the restart-aware drain goroutine still catches it.
func TestCallStreamDrainCatchesDelayedError(t *testing.T) {
	ctx := newManualCancelContext()
	restarted := make(chan struct{}, 1)
	var restartSignal sync.Once
	releaseErr := make(chan struct{})

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			// Unbuffered: the send blocks until the drain goroutine reads.
			ch := make(chan *workerplugin.StreamResult)
			go func() {
				<-releaseErr
				errResult := workerplugin.GetStreamResult()
				errResult.Kind = workerplugin.StreamResultKindError
				errResult.Error = status.Error(codes.Unavailable, "worker died")
				ch <- errResult
				close(ch)
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	p.newWorker = func(id int) (*workerHandle, error) {
		restartSignal.Do(func() { close(restarted) })
		return newMockHandle(id, newMockWorker()), nil
	}
	defer p.Close()

	results, err := p.CallStream(ctx, "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// Cancel the context — the streamLoop takes ctx.Done(), starts the
	// drain goroutine, then closes wrappedResults (via defer).
	ctx.cancel(context.Canceled)

	// Wait for wrappedResults to close — this proves the streamLoop
	// goroutine has exited and the drain goroutine is running.
	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})

	// NOW release the error. The drain goroutine reads it from the
	// unbuffered channel, detects the retryable error, and restarts.
	close(releaseErr)

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("drain should catch delayed retryable error after ctx.Done()")
	}
}

// TestCallStreamInternalCanceledErrorStillRestartsWorker verifies that a
// cancellation-shaped stream error still triggers restart when the parent
// request context is not canceled.
func TestCallStreamInternalCanceledErrorStillRestartsWorker(t *testing.T) {
	restarted := make(chan struct{}, 1)
	var restartSignal sync.Once
	releaseErr := make(chan struct{})

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 1)
			go func() {
				<-releaseErr
				errResult := workerplugin.GetStreamResult()
				errResult.Kind = workerplugin.StreamResultKindError
				errResult.Error = status.Error(codes.Canceled, "internal restart canceled stream")
				ch <- errResult
				close(ch)
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	p.newWorker = func(id int) (*workerHandle, error) {
		restartSignal.Do(func() { close(restarted) })
		return newMockHandle(id, newMockWorker()), nil
	}
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	close(releaseErr)

	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("worker should restart after internal cancellation error")
	}
}

// TestCallStreamSerializedCanceledErrorTriggersRestart verifies that a
// cancellation error that has been serialized across the gRPC boundary
// (losing its *status.Status wrapper) still triggers a restart when the
// caller context is NOT cancelled. This simulates the production path:
// worker emits codes.Canceled → GRPCServer serializes via .Error() →
// GRPCClient reconstructs via fmt.Errorf → pool receives plain string.
func TestCallStreamSerializedCanceledErrorTriggersRestart(t *testing.T) {
	restarted := make(chan struct{}, 1)
	var restartSignal sync.Once
	releaseErr := make(chan struct{})

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 1)
			go func() {
				<-releaseErr
				// Simulate what survives the gRPC round-trip: the
				// status.Error is serialized to a plain string by
				// GRPCServer (.Error()) and reconstructed by GRPCClient
				// as fmt.Errorf — no *status.Status wrapper remains.
				errResult := workerplugin.GetStreamResult()
				errResult.Kind = workerplugin.StreamResultKindError
				errResult.Error = fmt.Errorf("rpc error: code = Canceled desc = internal restart canceled stream")
				ch <- errResult
				close(ch)
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	p.newWorker = func(id int) (*workerHandle, error) {
		restartSignal.Do(func() { close(restarted) })
		return newMockHandle(id, newMockWorker()), nil
	}
	defer p.Close()

	// Caller context is NOT cancelled — this is an internal/worker
	// cancellation, not a caller cancellation.
	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	close(releaseErr)

	requireCompleteWithin(t, 5*time.Second, func() {
		for r := range results {
			workerplugin.ReleaseStreamResult(r)
		}
	})

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("serialized Canceled error should trigger restart when caller is not cancelled")
	}
}

// TestCallStreamResetNotInjectedOnFirstAttempt verifies that the reset
// message is only injected when retrying after partial data was sent,
// not on a clean first attempt.
func TestCallStreamResetNotInjectedOnFirstAttempt(t *testing.T) {
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(_ context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 2)
			partial := workerplugin.GetStreamResult()
			partial.Kind = workerplugin.StreamResultKindStream
			partial.Data = []byte(`"data"`)
			ch <- partial

			final := workerplugin.GetStreamResult()
			final.Kind = workerplugin.StreamResultKindFinal
			final.Data = []byte(`"done"`)
			ch <- final
			close(ch)
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	var gotReset bool
	for r := range results {
		if r.Reset {
			gotReset = true
		}
		workerplugin.ReleaseStreamResult(r)
	}

	if gotReset {
		t.Error("reset should NOT be injected on a clean first attempt")
	}
}

// ---------------------------------------------------------------------------
// Tests: Pool lifecycle
// ---------------------------------------------------------------------------

// TestCloseIdempotent verifies that calling Close multiple times is safe.
func TestCloseIdempotent(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)

	if err := p.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestCloseDuringHealthCheckDoesNotDeadlock reproduces the shutdown deadlock
// where Close() waits on the health-check goroutine while still holding p.mu,
// and the health-check goroutine needs p.mu to enter restartWorker().
func TestCloseDuringHealthCheckDoesNotDeadlock(t *testing.T) {
	healthStarted := make(chan struct{})
	releaseHealth := make(chan struct{})
	closeStarted := make(chan struct{})

	var signalCloseStart sync.Once

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.healthFn = func(context.Context) (bool, error) {
			close(healthStarted)
			<-releaseHealth
			return false, nil
		}
		w.closeFn = func() error {
			signalCloseStart.Do(func() { close(closeStarted) })
			return nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.checkHealth()
	}()

	select {
	case <-healthStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("health check did not start")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- p.Close()
	}()

	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not begin worker shutdown")
	}

	close(releaseHealth)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked with concurrent health check")
	}
}

// TestCloseInterruptsSlowHealthChecks verifies that Close() returns promptly
// even when a health-check sweep is blocked on slow/hanging health RPCs.
// Before the fix, checkHealth used context.Background() so each health RPC
// could block for up to 5s regardless of shutdown.
func TestCloseInterruptsSlowHealthChecks(t *testing.T) {
	healthStarted := make(chan struct{}, 4)
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.healthFn = func(ctx context.Context) (bool, error) {
			select {
			case healthStarted <- struct{}{}:
			default:
			}
			// Block until the context is cancelled (simulates a slow/stuck RPC).
			<-ctx.Done()
			return false, ctx.Err()
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 4, factory)

	// Start a health check sweep in the background.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.checkHealth()
	}()

	// Wait for at least one health RPC to start.
	select {
	case <-healthStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("health check did not start")
	}

	// Close must return promptly — not blocked for 5s * pool_size.
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- p.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked waiting for slow health checks to time out")
	}
}

// TestCheckHealthDispatchesRestartAsync verifies that an unhealthy worker does
// not block checkHealth() while replacement startup is stalled.
func TestCheckHealthDispatchesRestartAsync(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	failed := p.workers[0]
	failed.worker.(*mockWorker).healthFn = func(context.Context) (bool, error) {
		return false, nil
	}

	blockedFactory := make(chan struct{})
	restartStarted := make(chan struct{})
	var signalRestartStart sync.Once
	p.newWorker = func(id int) (*workerHandle, error) {
		signalRestartStart.Do(func() { close(restartStarted) })
		<-blockedFactory
		return newMockHandle(id, newMockWorker()), nil
	}

	checkDone := make(chan struct{})
	go func() {
		defer close(checkDone)
		p.checkHealth()
	}()

	select {
	case <-restartStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement startup did not begin")
	}

	select {
	case <-checkDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("checkHealth blocked on replacement startup")
	}

	close(blockedFactory)
}

// TestCheckHealthSkipsDuplicateRestartDispatch verifies repeated unhealthy
// probes do not queue extra async restart waiters for the same handle while a
// replacement is already pending.
func TestCheckHealthSkipsDuplicateRestartDispatch(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	failed := p.workers[0]
	failed.worker.(*mockWorker).healthFn = func(context.Context) (bool, error) {
		return false, nil
	}

	blockedFactory := make(chan struct{})
	restartStarted := make(chan struct{})
	var signalRestartStart sync.Once
	p.newWorker = func(id int) (*workerHandle, error) {
		signalRestartStart.Do(func() { close(restartStarted) })
		<-blockedFactory
		return newMockHandle(id, newMockWorker()), nil
	}

	for i := 0; i < 4; i++ {
		p.checkHealth()
	}

	select {
	case <-restartStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement startup did not begin")
	}

	if got := failed.restartPending.Load(); got != 1 {
		t.Fatalf("expected exactly 1 pending restart, got %d", got)
	}

	close(blockedFactory)
}

// TestCheckHealthDoesNotReHealthyRestartingWorker verifies a stale successful
// probe does not mark a handle healthy again once restart has been queued.
func TestCheckHealthDoesNotReHealthyRestartingWorker(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)
	defer p.Close()

	handle := p.workers[0]
	handle.healthy.Store(false)
	handle.restartPending.Add(1)
	defer handle.restartPending.Add(-1)
	handle.worker.(*mockWorker).healthFn = func(context.Context) (bool, error) {
		return true, nil
	}

	p.checkHealth()

	if handle.healthy.Load() {
		t.Fatal("worker should remain unhealthy while restart is pending")
	}
}

// TestCloseDuringHealthRestartDoesNotBlock verifies that Close() is not held
// open by a health check that dispatches a restart whose startup is stalled.
func TestCloseDuringHealthRestartDoesNotBlock(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)

	failed := p.workers[0]
	failed.worker.(*mockWorker).healthFn = func(context.Context) (bool, error) {
		return false, nil
	}

	blockedFactory := make(chan struct{})
	restartStarted := make(chan struct{})
	var signalRestartStart sync.Once
	p.newWorker = func(id int) (*workerHandle, error) {
		signalRestartStart.Do(func() { close(restartStarted) })
		<-blockedFactory
		return newMockHandle(id, newMockWorker()), nil
	}

	checkDone := make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(checkDone)
		p.checkHealth()
	}()

	select {
	case <-restartStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement startup did not begin")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- p.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on health-check restart")
	}

	select {
	case <-checkDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("health check did not exit promptly during Close")
	}

	close(blockedFactory)
}

// TestCloseCancelsBlockedAsyncRestart verifies that Close cancels an async
// restart that is stalled in worker startup and returns without installing a
// replacement afterward.
func TestCloseCancelsBlockedAsyncRestart(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)

	failed := p.workers[0]
	failed.healthy.Store(false)

	blockedFactory := make(chan struct{})
	restartStarted := make(chan struct{})
	var signalRestartStart sync.Once
	p.newWorker = func(id int) (*workerHandle, error) {
		signalRestartStart.Do(func() { close(restartStarted) })
		<-blockedFactory
		return newMockHandle(id, newMockWorker()), nil
	}

	p.dispatchRestart(failed)

	select {
	case <-restartStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement startup did not begin")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- p.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on async restart")
	}

	close(blockedFactory)
	time.Sleep(100 * time.Millisecond)

	p.mu.RLock()
	current := p.workers[0]
	p.mu.RUnlock()
	if current != failed {
		t.Fatal("replacement should not be installed after Close")
	}
}

// TestClosePreventsReplacementStart verifies that once Close begins, a queued
// async restart cannot start a new replacement worker afterward.
func TestClosePreventsReplacementStart(t *testing.T) {
	p := newTestPool(t, 1, goodFactory)

	failed := p.workers[0]
	failed.healthy.Store(false)

	hookStarted := make(chan struct{})
	releaseHook := make(chan struct{})
	var signalHook sync.Once
	p.beforeRestartStart = func() {
		signalHook.Do(func() { close(hookStarted) })
		<-releaseHook
	}
	defer func() { p.beforeRestartStart = nil }()

	var startCalls atomic.Int32
	p.newWorker = func(id int) (*workerHandle, error) {
		startCalls.Add(1)
		return newMockHandle(id, newMockWorker()), nil
	}

	p.dispatchRestart(failed)

	select {
	case <-hookStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("restart did not reach pre-start hook")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- p.Close()
	}()

	select {
	case <-closeDone:
		t.Fatal("Close returned before blocked restart path was released")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseHook)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after releasing restart hook")
	}

	if calls := startCalls.Load(); calls != 0 {
		t.Fatalf("replacement started %d time(s) after Close began, want 0", calls)
	}
}

// ---------------------------------------------------------------------------
// Tests: Concurrent multi-worker
// ---------------------------------------------------------------------------

// TestConcurrentParseMultiWorker exercises many goroutines calling Parse
// on a 2-worker pool where one worker always fails. All calls should
// succeed (via the healthy worker or a restarted replacement).
func TestConcurrentParseMultiWorker(t *testing.T) {
	var callNum atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		n := callNum.Add(1)
		if n == 1 {
			// First worker always fails on Parse.
			w.parseFn = func(_ context.Context, _ string, _ []byte) (*workerplugin.ParseResult, error) {
				return nil, unavailableErr()
			}
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 2, factory)
	defer p.Close()

	const goroutines = 20
	var wg sync.WaitGroup
	var successCount atomic.Int32
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := p.Parse(context.Background(), "Test", []byte(`{}`))
			if err == nil && result != nil {
				successCount.Add(1)
			}
		}()
	}

	requireCompleteWithin(t, 10*time.Second, func() {
		close(start)
		wg.Wait()
	})

	if s := successCount.Load(); s != int32(goroutines) {
		t.Errorf("expected all %d goroutines to succeed, got %d", goroutines, s)
	}
}

// ---------------------------------------------------------------------------
// Tests: nil / zero-value Pool safety
// ---------------------------------------------------------------------------

func TestCloseNilPool(t *testing.T) {
	var p *Pool
	if err := p.Close(); err != nil {
		t.Fatalf("Close on nil *Pool returned error: %v", err)
	}
}

func TestCloseZeroValuePool(t *testing.T) {
	var p Pool
	// Both calls must succeed without panic.
	if err := p.Close(); err != nil {
		t.Fatalf("first Close on zero-value Pool returned error: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close on zero-value Pool returned error: %v", err)
	}
}

func TestShutdownNilPool(t *testing.T) {
	var p *Pool
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on nil *Pool returned error: %v", err)
	}
}

func TestShutdownZeroValuePool(t *testing.T) {
	var p Pool
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown on zero-value Pool returned error: %v", err)
	}
}

// TestCallStreamSlowLegacyHeartbeatPreventsHungKill verifies that a stream
// which emits a heartbeat before any "real" result — as mixed-mode legacy
// children do via the orchestrator's sendHeartbeat closure on BAML's first
// FunctionLog tick — keeps the worker alive even if the overall call takes
// longer than FirstByteTimeout. This is the positive counterpart to the
// hung-detection mechanism: anything that looks like upstream activity
// (heartbeat included) disables hung-kill for the request.
func TestCallStreamSlowLegacyHeartbeatPreventsHungKill(t *testing.T) {
	// Heartbeat arrives within FirstByteTimeout (simulating a legacy
	// child whose first BAML FunctionLog tick lands before the deadline);
	// the final trails well past the deadline so the hung check has the
	// opportunity to kill the worker if gotFirstByte isn't being set by
	// the heartbeat.
	firstByteTimeout := 40 * time.Millisecond
	heartbeatDelay := 15 * time.Millisecond
	finalDelay := 60 * time.Millisecond

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(ctx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 2)
			go func() {
				defer close(ch)
				select {
				case <-time.After(heartbeatDelay):
				case <-ctx.Done():
					return
				}
				hb := workerplugin.GetStreamResult()
				hb.Kind = workerplugin.StreamResultKindHeartbeat
				select {
				case ch <- hb:
				case <-ctx.Done():
					workerplugin.ReleaseStreamResult(hb)
					return
				}
				select {
				case <-time.After(finalDelay):
				case <-ctx.Done():
					return
				}
				final := workerplugin.GetStreamResult()
				final.Kind = workerplugin.StreamResultKindFinal
				final.Data = []byte(`"mixed-mode-final"`)
				select {
				case ch <- final:
				case <-ctx.Done():
					workerplugin.ReleaseStreamResult(final)
				}
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()
	p.SetFirstByteTimeout(firstByteTimeout)

	// workerClosed is closed (not sent-to) so multiple readers all see the
	// signal. closeFn is guarded by sync.Once because the pool may call
	// Close more than once over the worker's lifecycle (hung kill + pool
	// shutdown), and close(workerClosed) would panic on the second call.
	workerClosed := make(chan struct{})
	var closeOnce sync.Once
	p.workers[0].worker.(*mockWorker).closeFn = func() error {
		closeOnce.Do(func() { close(workerClosed) })
		return nil
	}

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// Keep probing hung detection on a fine cadence throughout the stream
	// so any window where gotFirstByte=false overlapping the timeout
	// elapses would surface as a kill. stopChecker is closed after the
	// main loop drains `results`, letting the checker exit cleanly —
	// previously the checker competed with the main loop on `case
	// <-results:` and could steal a heartbeat or final.
	stopChecker := make(chan struct{})
	checkerDone := make(chan struct{})
	go func() {
		defer close(checkerDone)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopChecker:
				return
			case <-tick.C:
				p.checkHungRequests()
			}
		}
	}()

	var gotFinal bool
	var gotData string
	for r := range results {
		if r.Kind == workerplugin.StreamResultKindFinal {
			gotFinal = true
			gotData = string(r.Data)
		}
		workerplugin.ReleaseStreamResult(r)
	}
	close(stopChecker)
	<-checkerDone

	if !gotFinal {
		t.Fatalf("expected a final result, stream terminated without one")
	}
	if gotData != `"mixed-mode-final"` {
		t.Errorf("expected final data %q, got %q", `"mixed-mode-final"`, gotData)
	}

	select {
	case <-workerClosed:
		t.Fatal("worker should not have been killed when heartbeat arrived within FirstByteTimeout")
	case <-time.After(20 * time.Millisecond):
	}
}

// TestCallStreamSilentLegacyGetsKilled is the negative-path counterpart:
// a legacy callback that neither fires sendHeartbeat nor produces a final
// should still be killed by hung detection after FirstByteTimeout. This
// proves the mixed-mode path hasn't broken the pool's liveness guarantee
// for genuinely hung upstreams.
func TestCallStreamSilentLegacyGetsKilled(t *testing.T) {
	firstByteTimeout := 30 * time.Millisecond

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(ctx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			// Worker "hangs" — hold the channel open, send nothing, and
			// only close on context cancellation so hung-kill cleanly
			// terminates the goroutine.
			ch := make(chan *workerplugin.StreamResult)
			go func() {
				<-ctx.Done()
				close(ch)
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()
	p.SetFirstByteTimeout(firstByteTimeout)

	// workerClosed is closed (not sent-to) so both the checker goroutine
	// below and the main assertion see the signal. closeFn is guarded by
	// sync.Once because the pool can call Close more than once (hung kill
	// + pool shutdown), and close() on an already-closed channel panics.
	workerClosed := make(chan struct{})
	var closeOnce sync.Once
	p.workers[0].worker.(*mockWorker).closeFn = func() error {
		closeOnce.Do(func() { close(workerClosed) })
		return nil
	}

	// Disable retries so the test observes the first-attempt hung kill
	// directly without the pool silently retrying on a fresh worker.
	p.config.MaxRetries = 0

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	// stopChecker lets the main body stop the ticker cleanly after it has
	// asserted on workerClosed. The checker never reads from workerClosed
	// itself; closing stopChecker is the sole exit path.
	stopChecker := make(chan struct{})
	checkerDone := make(chan struct{})
	go func() {
		defer close(checkerDone)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopChecker:
				return
			case <-tick.C:
				p.checkHungRequests()
			}
		}
	}()

	select {
	case <-workerClosed:
	case <-time.After(500 * time.Millisecond):
		close(stopChecker)
		<-checkerDone
		t.Fatal("worker was not killed when legacy callback produced no heartbeat or final")
	}
	close(stopChecker)
	<-checkerDone

	// Drain the stream so the CallStream goroutine finishes — the pool
	// restarts the hung worker and propagates the error. Each result
	// must be released back to the pool (CodeRabbit verdict-21 finding
	// 9): pool/pool.go:1487-1493 transfers ownership to the consumer
	// on send, so a blind drain leaks pooled StreamResult objects.
	for r := range results {
		workerplugin.ReleaseStreamResult(r)
	}
}

// TestCallStreamPlannedMetadataDoesNotDisableHungDetection verifies the
// post-verdict-15 invariant: a worker that emits planned metadata
// upfront and then stalls in upstream HTTP must still be killed by
// FirstByteTimeout. Planned metadata is now emitted before any HTTP
// work (verdict-15), so it cannot count as upstream-progress evidence
// the way heartbeat / stream / final / outcome metadata do. Without
// this guard, planned-first emission would silently disable hung
// detection and a hung upstream would never get retried/killed.
func TestCallStreamPlannedMetadataDoesNotDisableHungDetection(t *testing.T) {
	firstByteTimeout := 30 * time.Millisecond

	plannedPayload, err := json.Marshal(&bamlutils.Metadata{
		Phase:      bamlutils.MetadataPhasePlanned,
		Path:       "legacy",
		PathReason: "invalid-round-robin-start-override",
		Client:     "TestClient",
	})
	if err != nil {
		t.Fatalf("marshal planned metadata: %v", err)
	}

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(ctx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 1)
			go func() {
				defer close(ch)
				// Emit planned metadata immediately, mirroring what the
				// orchestrator now does.
				md := workerplugin.GetStreamResult()
				md.Kind = workerplugin.StreamResultKindMetadata
				md.Data = append(md.Data[:0], plannedPayload...)
				select {
				case ch <- md:
				case <-ctx.Done():
					workerplugin.ReleaseStreamResult(md)
					return
				}
				// Then stall — never produce a real first byte. The
				// hung detector must still fire.
				<-ctx.Done()
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()
	p.SetFirstByteTimeout(firstByteTimeout)

	workerClosed := make(chan struct{})
	var closeOnce sync.Once
	p.workers[0].worker.(*mockWorker).closeFn = func() error {
		closeOnce.Do(func() { close(workerClosed) })
		return nil
	}

	// Disable retries so the test observes the first-attempt hung kill
	// directly without the pool silently retrying on a fresh worker.
	p.config.MaxRetries = 0

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	stopChecker := make(chan struct{})
	checkerDone := make(chan struct{})
	go func() {
		defer close(checkerDone)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopChecker:
				return
			case <-tick.C:
				p.checkHungRequests()
			}
		}
	}()

	select {
	case <-workerClosed:
	case <-time.After(500 * time.Millisecond):
		close(stopChecker)
		<-checkerDone
		t.Fatal("worker was not killed when planned metadata arrived but upstream stalled — first-byte gate must skip planned metadata")
	}
	close(stopChecker)
	<-checkerDone

	// Drain + release pooled results — see drain at pool_test.go:3192
	// for rationale (CodeRabbit verdict-21 finding 9).
	for r := range results {
		workerplugin.ReleaseStreamResult(r)
	}
}

// TestCallStreamOutcomeMetadataPreventsHungKill is the inverse-regression
// guard: outcome metadata indicates the request actually progressed
// (BAML produced a winning attempt), so it MUST count toward first-byte
// liveness. Without distinguishing planned from outcome in the
// first-byte gate, a fix that simply skipped all metadata would also
// disable liveness for a request that completed successfully — wrong.
func TestCallStreamOutcomeMetadataPreventsHungKill(t *testing.T) {
	firstByteTimeout := 30 * time.Millisecond
	outcomeDelay := 10 * time.Millisecond
	finalDelay := 80 * time.Millisecond

	outcomePayload, err := json.Marshal(&bamlutils.Metadata{
		Phase:          bamlutils.MetadataPhaseOutcome,
		Path:           "legacy",
		Client:         "TestClient",
		WinnerProvider: "openai",
	})
	if err != nil {
		t.Fatalf("marshal outcome metadata: %v", err)
	}

	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(ctx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			ch := make(chan *workerplugin.StreamResult, 2)
			go func() {
				defer close(ch)
				select {
				case <-time.After(outcomeDelay):
				case <-ctx.Done():
					return
				}
				md := workerplugin.GetStreamResult()
				md.Kind = workerplugin.StreamResultKindMetadata
				md.Data = append(md.Data[:0], outcomePayload...)
				select {
				case ch <- md:
				case <-ctx.Done():
					workerplugin.ReleaseStreamResult(md)
					return
				}
				select {
				case <-time.After(finalDelay):
				case <-ctx.Done():
					return
				}
				final := workerplugin.GetStreamResult()
				final.Kind = workerplugin.StreamResultKindFinal
				final.Data = []byte(`"done"`)
				select {
				case ch <- final:
				case <-ctx.Done():
					workerplugin.ReleaseStreamResult(final)
				}
			}()
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()
	p.SetFirstByteTimeout(firstByteTimeout)

	workerClosed := make(chan struct{})
	var closeOnce sync.Once
	p.workers[0].worker.(*mockWorker).closeFn = func() error {
		closeOnce.Do(func() { close(workerClosed) })
		return nil
	}

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	stopChecker := make(chan struct{})
	checkerDone := make(chan struct{})
	go func() {
		defer close(checkerDone)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopChecker:
				return
			case <-tick.C:
				p.checkHungRequests()
			}
		}
	}()

	for r := range results {
		workerplugin.ReleaseStreamResult(r)
	}
	close(stopChecker)
	<-checkerDone

	select {
	case <-workerClosed:
		t.Fatal("worker should not have been killed when outcome metadata arrived within FirstByteTimeout")
	case <-time.After(20 * time.Millisecond):
	}
}

// TestCallStreamPlannedMetadataDoesNotTriggerResetOnRetry verifies the
// post-verdict-15 invariant that planned metadata, emitted upfront from
// the orchestrator before any HTTP work, does not count as "forwarded
// content the client must discard on retry". The pool's reset-injection
// path was previously gated on sentAnyResults flipping for any
// successful forward; with planned-first emission, that meant a
// pre-first-byte retry would inject Reset onto the next attempt's
// planned metadata even though no real content had reached the client.
// Integration tests TestWorkerDeathMidStream/* depend on this guard.
func TestCallStreamPlannedMetadataDoesNotTriggerResetOnRetry(t *testing.T) {
	plannedPayload, err := json.Marshal(&bamlutils.Metadata{
		Phase:      bamlutils.MetadataPhasePlanned,
		Path:       "legacy",
		PathReason: "invalid-round-robin-start-override",
		Client:     "TestClient",
	})
	if err != nil {
		t.Fatalf("marshal planned metadata: %v", err)
	}

	var callCount atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(ctx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			n := callCount.Add(1)
			ch := make(chan *workerplugin.StreamResult, 2)
			if n == 1 {
				// First attempt: emit planned, then die without
				// producing any real content. Mirrors a worker
				// that crashed (or was hung-killed) after the
				// orchestrator's upfront planned emit but before
				// BAML's first FunctionLog tick.
				go func() {
					defer close(ch)
					md := workerplugin.GetStreamResult()
					md.Kind = workerplugin.StreamResultKindMetadata
					md.Data = append(md.Data[:0], plannedPayload...)
					select {
					case ch <- md:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(md)
					}
					// Simulate worker death by returning a
					// retryable RPC error.
					errR := workerplugin.GetStreamResult()
					errR.Kind = workerplugin.StreamResultKindError
					errR.Error = unavailableErr()
					select {
					case ch <- errR:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(errR)
					}
				}()
			} else {
				// Retry: emit planned + clean Final. Reset must
				// NOT be injected — first attempt only forwarded
				// planned metadata, which has no client-visible
				// state to reset.
				go func() {
					defer close(ch)
					md := workerplugin.GetStreamResult()
					md.Kind = workerplugin.StreamResultKindMetadata
					md.Data = append(md.Data[:0], plannedPayload...)
					select {
					case ch <- md:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(md)
						return
					}
					final := workerplugin.GetStreamResult()
					final.Kind = workerplugin.StreamResultKindFinal
					final.Data = []byte(`"done"`)
					select {
					case ch <- final:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(final)
					}
				}()
			}
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	var sawReset bool
	var sawFinal bool
	for r := range results {
		if r.Reset {
			sawReset = true
		}
		if r.Kind == workerplugin.StreamResultKindFinal {
			sawFinal = true
		}
		workerplugin.ReleaseStreamResult(r)
	}

	if !sawFinal {
		t.Fatal("expected a Final result after retry")
	}
	if sawReset {
		t.Error("Reset must NOT be injected when only planned metadata was forwarded pre-retry")
	}
}

// TestCallStreamRealContentTriggersResetOnRetry is the inverse-regression
// guard. When real content (a stream partial) was forwarded before the
// worker died, the retry MUST inject Reset on the next forwarded result
// so the streamwriter discards accumulated state. Without this
// distinction, a fix that blanket-skipped Reset on metadata frames
// would also break the legitimate mid-stream-retry case.
func TestCallStreamRealContentTriggersResetOnRetry(t *testing.T) {
	plannedPayload, err := json.Marshal(&bamlutils.Metadata{
		Phase:  bamlutils.MetadataPhasePlanned,
		Path:   "buildrequest",
		Client: "TestClient",
	})
	if err != nil {
		t.Fatalf("marshal planned metadata: %v", err)
	}

	var callCount atomic.Int32
	factory := func(id int) (*workerHandle, error) {
		w := newMockWorker()
		w.callStreamFn = func(ctx context.Context, _ string, _ []byte, _ bamlutils.StreamMode) (<-chan *workerplugin.StreamResult, error) {
			n := callCount.Add(1)
			ch := make(chan *workerplugin.StreamResult, 4)
			if n == 1 {
				// First attempt: planned + real partial content,
				// then die. Real content went to the client →
				// retry must inject Reset.
				go func() {
					defer close(ch)
					md := workerplugin.GetStreamResult()
					md.Kind = workerplugin.StreamResultKindMetadata
					md.Data = append(md.Data[:0], plannedPayload...)
					select {
					case ch <- md:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(md)
						return
					}
					partial := workerplugin.GetStreamResult()
					partial.Kind = workerplugin.StreamResultKindStream
					partial.Data = []byte(`"part"`)
					select {
					case ch <- partial:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(partial)
						return
					}
					errR := workerplugin.GetStreamResult()
					errR.Kind = workerplugin.StreamResultKindError
					errR.Error = unavailableErr()
					select {
					case ch <- errR:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(errR)
					}
				}()
			} else {
				// Retry: planned + Final. The first non-planned
				// forwarded result must carry Reset=true.
				go func() {
					defer close(ch)
					md := workerplugin.GetStreamResult()
					md.Kind = workerplugin.StreamResultKindMetadata
					md.Data = append(md.Data[:0], plannedPayload...)
					select {
					case ch <- md:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(md)
						return
					}
					final := workerplugin.GetStreamResult()
					final.Kind = workerplugin.StreamResultKindFinal
					final.Data = []byte(`"done"`)
					select {
					case ch <- final:
					case <-ctx.Done():
						workerplugin.ReleaseStreamResult(final)
					}
				}()
			}
			return ch, nil
		}
		return newMockHandle(id, w), nil
	}

	p := newTestPool(t, 1, factory)
	defer p.Close()

	results, err := p.CallStream(context.Background(), "Test", []byte(`{}`), bamlutils.StreamModeStream)
	if err != nil {
		t.Fatalf("CallStream setup failed: %v", err)
	}

	var sawReset bool
	var sawFinal bool
	for r := range results {
		if r.Reset {
			sawReset = true
		}
		if r.Kind == workerplugin.StreamResultKindFinal {
			sawFinal = true
		}
		workerplugin.ReleaseStreamResult(r)
	}

	if !sawFinal {
		t.Fatal("expected a Final result after retry")
	}
	if !sawReset {
		t.Error("Reset must be injected when real partial content was forwarded pre-retry")
	}
}
