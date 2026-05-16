//go:build !inprocess

package pool

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// GetAllWorkersNativeStacks uses gdb to capture native thread backtraces from all
// worker processes. This reveals where Rust/C threads are blocked — information
// that Go's pprof goroutine dump cannot provide.
//
// Requires: gdb installed in the container, SYS_PTRACE capability.
func (p *Pool) GetAllWorkersNativeStacks(ctx context.Context) []WorkerNativeStacksResult {
	var results []WorkerNativeStacksResult

	for _, handle := range p.workerSnapshot() {
		result := WorkerNativeStacksResult{WorkerID: handle.id}

		if handle.nativePid == 0 {
			result.Error = fmt.Errorf("could not determine worker PID")
			results = append(results, result)
			continue
		}
		result.Pid = handle.nativePid

		// Run gdb with a short timeout to avoid blocking if gdb hangs
		gdbCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(gdbCtx, "gdb",
			"-batch",
			"-ex", "set pagination off",
			"-ex", "set print thread-events off",
			"-ex", "thread apply all bt",
			"-p", fmt.Sprintf("%d", handle.nativePid),
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
