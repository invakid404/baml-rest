package pool

import (
	"context"
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

	closeMu sync.Mutex
	closed  bool
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
		lastUsed:    time.Now(),
		inFlightReq: make(map[uint64]*inFlightRequest),
	}
	h.restartCond = sync.NewCond(&h.restartMu)
	h.healthy.Store(true)
	return h
}

// newTestPool creates a Pool backed by mock workers.
// The factory is invoked for every startWorker call (initial + replacements).
// The health checker is NOT started.
func newTestPool(t *testing.T, size int, factory func(id int) (*workerHandle, error)) *Pool {
	t.Helper()
	p := &Pool{
		config: &Config{
			PoolSize:         size,
			MaxRetries:       2,
			FirstByteTimeout: 5 * time.Second,
		},
		logger:    zerolog.Nop(),
		workers:   make([]*workerHandle, size),
		done:      make(chan struct{}),
		drainCh:   make(chan struct{}),
		newWorker: factory,
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
		switch {
		case r.Reset:
			gotReset = true
		case r.Kind == workerplugin.StreamResultKindFinal:
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
		{"InvalidArgument", status.Error(codes.InvalidArgument, "bad input"), false},
		{"Internal", status.Error(codes.Internal, "runtime panic"), false},
		{"NotFound", status.Error(codes.NotFound, "method missing"), false},
		{"OK", status.Error(codes.OK, ""), false},
		{"connection reset string", errors.New("connection reset by peer"), true},
		{"EOF string", errors.New("error reading from server: EOF"), true},
		{"transport closing string", errors.New("transport is closing"), true},
		{"code = Unavailable string", errors.New("rpc error: code = Unavailable desc = gone"), true},
		{"plain error", errors.New("something broke"), false},
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
