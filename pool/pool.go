package pool

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-plugin"

	"github.com/invakid404/baml-rest/workerplugin"
)

// Config holds the configuration for the worker pool
type Config struct {
	// PoolSize is the number of workers to maintain
	PoolSize int
	// MaxRetries is the maximum number of retries for a failed call
	MaxRetries int
	// HealthCheckInterval is how often to check worker health
	HealthCheckInterval time.Duration
	// Logger for pool operations
	Logger *slog.Logger
}

// DefaultConfig returns a default configuration
func DefaultConfig() *Config {
	return &Config{
		PoolSize:            4,
		MaxRetries:          2,
		HealthCheckInterval: 10 * time.Second,
	}
}

// Pool manages a pool of worker processes
type Pool struct {
	config   *Config
	execPath string // path to current executable
	workers  []*workerHandle
	mu       sync.RWMutex
	next     atomic.Uint64
	closed   atomic.Bool
	draining atomic.Bool
	wg       sync.WaitGroup
	done     chan struct{}
}

type workerHandle struct {
	id       int
	client   *plugin.Client
	worker   workerplugin.Worker
	mu       sync.Mutex
	healthy  bool
	lastUsed time.Time
	inFlight atomic.Int64
}

// New creates a new worker pool
func New(config *Config) (*Pool, error) {
	if config.PoolSize <= 0 {
		config.PoolSize = 4
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}

	// Get the path to the current executable
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable path: %w", err)
	}

	p := &Pool{
		config:   config,
		execPath: execPath,
		workers:  make([]*workerHandle, config.PoolSize),
		done:     make(chan struct{}),
	}

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
	// Spawn self with "worker" subcommand
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: workerplugin.Handshake,
		Plugins:         workerplugin.PluginMap,
		Cmd:             exec.Command(p.execPath, "worker"),
		AllowedProtocols: []plugin.Protocol{
			plugin.ProtocolGRPC,
		},
		Logger: nil, // Use go-plugin's default hclog
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

	p.config.Logger.Info("Started worker", "id", id)

	return &workerHandle{
		id:       id,
		client:   client,
		worker:   worker,
		healthy:  true,
		lastUsed: time.Now(),
	}, nil
}

func (p *Pool) healthChecker() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			if p.closed.Load() {
				return
			}
			p.checkHealth()
		}
	}
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
			p.config.Logger.Warn("Worker unhealthy, restarting", "id", handle.id, "error", err)
			handle.mu.Unlock()

			// Restart worker
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
		p.config.Logger.Error("Failed to restart worker", "id", id, "error", err)
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

// Call executes a BAML method and returns the final result
func (p *Pool) Call(ctx context.Context, methodName string, inputJSON, optionsJSON []byte) (*workerplugin.CallResult, error) {
	var lastErr error

	for attempt := 0; attempt <= p.config.MaxRetries; attempt++ {
		handle, err := p.getWorker()
		if err != nil {
			return nil, err
		}

		// Track in-flight request
		handle.inFlight.Add(1)
		defer handle.inFlight.Add(-1)

		handle.mu.Lock()
		handle.lastUsed = time.Now()
		handle.mu.Unlock()

		result, err := handle.worker.Call(ctx, methodName, inputJSON, optionsJSON)
		if err == nil {
			return result, nil
		}

		lastErr = err
		p.config.Logger.Warn("Worker call failed", "worker", handle.id, "attempt", attempt+1, "error", err)

		// Check if context is cancelled
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Mark worker as unhealthy and restart
		handle.mu.Lock()
		handle.healthy = false
		handle.mu.Unlock()

		go p.restartWorker(handle.id)
	}

	return nil, fmt.Errorf("all retries failed: %w", lastErr)
}

// CallStream executes a BAML method and streams results
func (p *Pool) CallStream(ctx context.Context, methodName string, inputJSON, optionsJSON []byte) (<-chan *workerplugin.StreamResult, error) {
	handle, err := p.getWorker()
	if err != nil {
		return nil, err
	}

	// Track in-flight request
	handle.inFlight.Add(1)

	handle.mu.Lock()
	handle.lastUsed = time.Now()
	handle.mu.Unlock()

	results, err := handle.worker.CallStream(ctx, methodName, inputJSON, optionsJSON)
	if err != nil {
		handle.inFlight.Add(-1)

		// Mark worker as unhealthy
		handle.mu.Lock()
		handle.healthy = false
		handle.mu.Unlock()

		go p.restartWorker(handle.id)
		return nil, err
	}

	// Wrap the results channel to decrement inFlight when done
	wrappedResults := make(chan *workerplugin.StreamResult)
	go func() {
		defer close(wrappedResults)
		defer handle.inFlight.Add(-1)

		for result := range results {
			wrappedResults <- result
		}
	}()

	return wrappedResults, nil
}

// totalInFlight returns the total number of in-flight requests across all workers
func (p *Pool) totalInFlight() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var total int64
	for _, handle := range p.workers {
		if handle != nil {
			total += handle.inFlight.Load()
		}
	}
	return total
}

// Shutdown gracefully shuts down the pool, waiting for in-flight requests to complete
func (p *Pool) Shutdown(ctx context.Context) error {
	// Start draining - reject new requests
	p.draining.Store(true)
	p.config.Logger.Info("Pool draining, waiting for in-flight requests to complete")

	// Wait for in-flight requests to complete
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		inFlight := p.totalInFlight()
		if inFlight == 0 {
			p.config.Logger.Info("All in-flight requests completed")
			break
		}

		select {
		case <-ctx.Done():
			p.config.Logger.Warn("Shutdown timeout, killing workers with in-flight requests", "in_flight", inFlight)
			return p.Close()
		case <-ticker.C:
			p.config.Logger.Info("Waiting for in-flight requests", "count", inFlight)
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
	p.config.Logger.Info("Worker pool closed")
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
			stats.InFlight += handle.inFlight.Load()
		}
	}

	return stats
}
