package pool

import "fmt"

// errWorkerStartAborted is returned by the worker-start codepath when
// the pool shuts down (or otherwise cancels) while a worker is still
// coming up. Callers treat it as a benign "give up cleanly" signal.
var errWorkerStartAborted = fmt.Errorf("worker startup aborted")

// workerStartResult is the value flowing through the per-start
// goroutine channel for both startup and restart paths. It lets the
// caller select between the start completing, the configured start
// timeout firing, and an external stop signal — without leaking the
// goroutine if a deadline or stop wins.
type workerStartResult struct {
	handle *workerHandle
	err    error
}

// reapWorkerStartResult drains a workerStartResult channel after the
// caller has already given up on the start (timeout / abort / etc.).
// If the start eventually completes successfully, the resulting handle
// is killed so we don't leak workers — important for the subprocess
// path where a late-arriving handle still has a live plugin process.
func reapWorkerStartResult(resultCh <-chan workerStartResult) {
	result := <-resultCh
	if result.handle != nil {
		result.handle.kill()
	}
}

// startWorker is the shared entry point for both initial pool fill
// and restart. The tag-specific implementations live in
// worker_start_subprocess.go and worker_start_inprocess.go and share
// the workerStartResult plumbing above plus the test-injection seam
// driven by Pool.newWorker.
func (p *Pool) startWorker(id int) (*workerHandle, error) {
	return p.startWorkerWithStop(id, nil)
}

// startTestWorker is the shared test-injection codepath. Both
// subprocess and inprocess builds short-circuit through here when
// Pool.newWorker is non-nil so pool tests can stay independent of the
// real worker construction path. Kept in the common file so the test
// seam exists regardless of build tag.
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
