//go:build !inprocess

package pool

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/hashicorp/go-plugin"

	"github.com/invakid404/baml-rest/workerplugin"
)

// tokioWorkerThreads pins the spawned worker's tokio runtime to a
// reasonable thread count for the host. Derived from GOMAXPROCS so
// the worker honours the Go scheduler's effective parallelism —
// recent Go versions auto-tune GOMAXPROCS from cgroup CPU quota,
// while runtime.NumCPU() reports physical host CPUs and would
// over-provision tokio threads inside a CPU-limited container.
// Worker subprocesses share the server's cgroup, so the budget is
// shared too.
var tokioWorkerThreads = fmt.Sprintf("TOKIO_WORKER_THREADS=%d", runtime.GOMAXPROCS(0)*2)

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

// startWorkerWithStop launches a subprocess worker, performs the
// go-plugin handshake, dispenses the workerplugin.Worker, and returns
// the populated workerHandle. The stop channel lets in-flight pool
// shutdown cancel an in-progress start.
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
		killFn:      client.Kill,
		worker:      worker,
		inFlightReq: make(map[uint64]*inFlightRequest),
	}
	if reattach := client.ReattachConfig(); reattach != nil {
		h.nativePid = reattach.Pid
	}
	h.restartCond = sync.NewCond(&h.restartMu)
	h.healthy.Store(true)
	return h, nil
}
