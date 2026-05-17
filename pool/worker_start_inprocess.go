//go:build !subprocess

package pool

import (
	"fmt"
	"sync"
	"time"
)

// startWorkerWithStop calls the configured WorkerFactory to populate
// a slot in in-process builds. The factory may be invoked at initial
// pool fill or on restart, so it must be idempotent — the pool
// reuses the same SharedStateStore across restarts (it is owned by
// pool.New, not by individual handler instances) but lets the
// factory rebuild any per-handler state.
//
// The factory runs in a goroutine so WorkerStartTimeout and the
// stop channel can still cancel a wedged construction (e.g. a
// factory that blocks on lazy initialization). reapWorkerStartResult
// handles the late-arrival case.
func (p *Pool) startWorkerWithStop(id int, stop <-chan struct{}) (*workerHandle, error) {
	select {
	case <-stop:
		return nil, errWorkerStartAborted
	default:
	}

	if p.newWorker != nil {
		return p.startTestWorker(id, stop)
	}

	if p.config.WorkerFactory == nil {
		return nil, fmt.Errorf("in-process pool: WorkerFactory is nil")
	}

	workerLogger := p.logger.With().Int("worker", id).Logger()
	factoryLogger := newHclogZerolog(workerLogger)
	factoryCfg := WorkerFactoryConfig{
		ID:               id,
		Logger:           factoryLogger,
		SharedStateStore: p.sharedStateStore,
	}

	resultCh := make(chan workerStartResult, 1)
	go func() {
		worker, err := p.config.WorkerFactory(factoryCfg)
		if err != nil {
			resultCh <- workerStartResult{err: fmt.Errorf("in-process worker factory: %w", err)}
			return
		}
		if worker == nil {
			resultCh <- workerStartResult{err: fmt.Errorf("in-process worker factory returned nil worker")}
			return
		}
		// Wrap before storing so every code path that reaches into
		// workerHandle.worker (per-request RPC, admin, restart probes)
		// goes through the recover boundary. The wrapper is method-
		// dispatch transparent; the inner worker only sees calls that
		// pass through recovery.Call*.
		worker = newRecoveringWorker(worker, workerLogger)
		h := &workerHandle{
			id:          id,
			logger:      workerLogger,
			worker:      worker,
			inFlightReq: make(map[uint64]*inFlightRequest),
		}
		h.restartCond = sync.NewCond(&h.restartMu)
		h.healthy.Store(true)
		workerLogger.Info().Msg("Started in-process worker")
		resultCh <- workerStartResult{handle: h}
	}()

	if p.config.WorkerStartTimeout > 0 {
		timer := time.NewTimer(p.config.WorkerStartTimeout)
		defer timer.Stop()

		select {
		case result := <-resultCh:
			return result.handle, result.err
		case <-timer.C:
			go reapWorkerStartResult(resultCh)
			return nil, fmt.Errorf("worker startup timed out after %s", p.config.WorkerStartTimeout)
		case <-stop:
			go reapWorkerStartResult(resultCh)
			return nil, errWorkerStartAborted
		}
	}

	select {
	case result := <-resultCh:
		return result.handle, result.err
	case <-stop:
		go reapWorkerStartResult(resultCh)
		return nil, errWorkerStartAborted
	}
}
