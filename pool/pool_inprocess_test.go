//go:build !subprocess

package pool

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/workerplugin"
)

// inprocessFactoryCall captures one observed factory invocation so
// tests can assert on the ID and shared-state store the pool threaded
// through WorkerFactoryConfig.
type inprocessFactoryCall struct {
	cfg WorkerFactoryConfig
}

// captureFactory returns a WorkerFactory that appends each invocation
// to calls and produces fresh mock workers. The handler swap is
// driven entirely through the factory — no mockHandle / newWorker
// injection — so the test exercises the real in-process startup path.
func captureFactory(calls *[]inprocessFactoryCall, mu *sync.Mutex) WorkerFactory {
	return func(cfg WorkerFactoryConfig) (workerplugin.Worker, error) {
		mu.Lock()
		*calls = append(*calls, inprocessFactoryCall{cfg: cfg})
		mu.Unlock()
		return newMockWorker(), nil
	}
}

// TestInProcessPoolDoesNotRequireWorkerPath verifies that an
// in-process pool starts cleanly with WorkerPath unset as long as
// WorkerFactory is supplied. Subprocess builds reject this; the
// in-process normalizeConfig is the only build-mode-specific
// validation difference.
func TestInProcessPoolDoesNotRequireWorkerPath(t *testing.T) {
	var calls []inprocessFactoryCall
	var mu sync.Mutex

	cfg := DefaultConfig()
	cfg.WorkerFactory = captureFactory(&calls, &mu)
	cfg.HealthCheckInterval = 0 // keep the health goroutine out of this test

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() with empty WorkerPath failed: %v", err)
	}
	defer p.Close()

	mu.Lock()
	got := len(calls)
	mu.Unlock()
	if got != 1 {
		t.Fatalf("expected 1 factory call for initial fill, got %d", got)
	}
}

// TestInProcessPoolForcesSingleWorker verifies the force-1 contract:
// even when callers request PoolSize > 1, the pool collapses to a
// single in-process handler. Multiple handlers would share an
// address space, defeating the FFI isolation that the multi-process
// subprocess pool provides.
func TestInProcessPoolForcesSingleWorker(t *testing.T) {
	var calls []inprocessFactoryCall
	var mu sync.Mutex

	cfg := DefaultConfig()
	cfg.PoolSize = 4
	cfg.WorkerFactory = captureFactory(&calls, &mu)
	cfg.HealthCheckInterval = 0

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer p.Close()

	mu.Lock()
	got := len(calls)
	mu.Unlock()
	if got != 1 {
		t.Errorf("expected 1 factory call (force-1), got %d", got)
	}
	if total := p.Stats().TotalWorkers; total != 1 {
		t.Errorf("Stats().TotalWorkers = %d, want 1", total)
	}
}

// TestInProcessFactoryReceivesSharedStateStore verifies the pool
// threads its host-side store through WorkerFactoryConfig when seeds
// are configured, and leaves it nil when seeds are absent. The
// factory does not own the store; pool.New owns lifetime so the
// store survives handler restarts.
func TestInProcessFactoryReceivesSharedStateStore(t *testing.T) {
	t.Run("with seeds", func(t *testing.T) {
		var calls []inprocessFactoryCall
		var mu sync.Mutex

		cfg := DefaultConfig()
		cfg.WorkerFactory = captureFactory(&calls, &mu)
		cfg.SharedStateSeeds = map[string]int{"clientA": 3}
		cfg.HealthCheckInterval = 0

		p, err := New(cfg)
		if err != nil {
			t.Fatalf("New() failed: %v", err)
		}
		defer p.Close()

		mu.Lock()
		defer mu.Unlock()
		if len(calls) != 1 {
			t.Fatalf("expected 1 factory call, got %d", len(calls))
		}
		if calls[0].cfg.SharedStateStore == nil {
			t.Error("expected non-nil SharedStateStore in WorkerFactoryConfig when seeds are set")
		}
	})

	t.Run("without seeds", func(t *testing.T) {
		var calls []inprocessFactoryCall
		var mu sync.Mutex

		cfg := DefaultConfig()
		cfg.WorkerFactory = captureFactory(&calls, &mu)
		cfg.HealthCheckInterval = 0

		p, err := New(cfg)
		if err != nil {
			t.Fatalf("New() failed: %v", err)
		}
		defer p.Close()

		mu.Lock()
		defer mu.Unlock()
		if len(calls) != 1 {
			t.Fatalf("expected 1 factory call, got %d", len(calls))
		}
		if calls[0].cfg.SharedStateStore != nil {
			t.Errorf("expected nil SharedStateStore in WorkerFactoryConfig when seeds are absent, got %p", calls[0].cfg.SharedStateStore)
		}
	})
}

// TestInProcessRestartRebuildsHandler verifies the factory is
// invoked again on restart and the slot is swapped to the new
// handle. Mirrors the subprocess-restart hot-swap contract from
// pool_test.go without depending on go-plugin.
func TestInProcessRestartRebuildsHandler(t *testing.T) {
	var (
		mu           sync.Mutex
		idsObserved  []int
		workers      []*mockWorker
		factoryCalls atomic.Int32
	)

	factory := func(cfg WorkerFactoryConfig) (workerplugin.Worker, error) {
		factoryCalls.Add(1)
		w := newMockWorker()
		mu.Lock()
		idsObserved = append(idsObserved, cfg.ID)
		workers = append(workers, w)
		mu.Unlock()
		return w, nil
	}

	cfg := DefaultConfig()
	cfg.WorkerFactory = factory
	cfg.HealthCheckInterval = 0
	cfg.WorkerStartTimeout = 5 * time.Second

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer p.Close()

	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("expected 1 factory call after New, got %d", got)
	}

	original := p.workers[0]
	if original == nil {
		t.Fatal("initial slot is nil")
	}

	p.restartWorker(0, original)

	if got := factoryCalls.Load(); got != 2 {
		t.Errorf("expected 2 factory calls after restart, got %d", got)
	}
	if p.workers[0] == original {
		t.Error("slot still holds original handle after restart")
	}
	if !p.workers[0].healthy.Load() {
		t.Error("replacement handle should be healthy")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(idsObserved) != 2 {
		t.Fatalf("expected 2 factory invocations recorded, got %d", len(idsObserved))
	}
	for _, id := range idsObserved {
		if id != 0 {
			t.Errorf("factory called with unexpected slot id: got %d, want 0 (force-1 pool)", id)
		}
	}
}
