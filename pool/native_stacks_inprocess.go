//go:build !subprocess

package pool

import (
	"context"
	"fmt"
)

// GetAllWorkersNativeStacks returns an explanatory per-handle error
// in in-process builds. The worker handler runs in the server process,
// so attaching gdb to it would freeze the server itself — a debugging
// tool that produces a deadlock is worse than no tool at all.
//
// The /_debug/native-stacks endpoint still calls this so it stays
// compiled; clients receive a result with a populated Error field.
func (p *Pool) GetAllWorkersNativeStacks(_ context.Context) []WorkerNativeStacksResult {
	var results []WorkerNativeStacksResult
	for _, handle := range p.workerSnapshot() {
		results = append(results, WorkerNativeStacksResult{
			WorkerID: handle.id,
			Error:    fmt.Errorf("native worker stacks are unavailable in in-process builds"),
		})
	}
	return results
}
