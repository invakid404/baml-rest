package worker

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/invakid404/baml-rest/workerplugin"
)

// GetMetrics returns the worker process Prometheus metrics, serialized
// as MetricFamily proto bytes so the host can wire them into its own
// /metrics endpoint without re-collecting.
func (h *Handler) GetMetrics(ctx context.Context) ([][]byte, error) {
	mfs, err := h.metricsReg.Gather()
	if err != nil {
		return nil, fmt.Errorf("failed to gather metrics: %w", err)
	}

	result := make([][]byte, 0, len(mfs))
	for _, mf := range mfs {
		data, err := proto.Marshal(mf)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal metric family %s: %w", mf.GetName(), err)
		}
		result = append(result, data)
	}
	return result, nil
}

// TriggerGC forces a Go garbage collection and releases unused heap
// memory back to the OS, returning the before/after heap snapshot so
// the host can surface the effectiveness of the GC.
func (h *Handler) TriggerGC(ctx context.Context) (*workerplugin.GCResult, error) {
	// Capture memory stats before GC
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// Force garbage collection
	runtime.GC()

	// Aggressively release memory to OS
	debug.FreeOSMemory()

	// Capture memory stats after GC
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	// HeapReleased is current bytes returned to the OS, not a cumulative
	// counter — it can decrease between snapshots when the runtime
	// reacquires previously-released memory. A naive uint64 subtraction
	// would underflow to ~18 EB in that case; clamp to zero so metrics
	// and logs report "no bytes released" instead.
	var heapReleased uint64
	if memAfter.HeapReleased >= memBefore.HeapReleased {
		heapReleased = memAfter.HeapReleased - memBefore.HeapReleased
	}

	return &workerplugin.GCResult{
		HeapAllocBefore: memBefore.HeapAlloc,
		HeapAllocAfter:  memAfter.HeapAlloc,
		HeapReleased:    heapReleased,
	}, nil
}

// GetGoroutines returns the worker process goroutine profile, optionally
// filtered by a comma-separated include/exclude pattern list. Patterns
// prefixed with `-` are exclusions; others are inclusions. Matching is
// case-insensitive substring against each goroutine's stack.
func (h *Handler) GetGoroutines(ctx context.Context, filter string) (*workerplugin.GoroutinesResult, error) {
	// Capture goroutine profile with full stacks
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		return nil, fmt.Errorf("failed to capture goroutine profile: %w", err)
	}

	stacks := buf.String()
	totalCount := int32(runtime.NumGoroutine())

	result := &workerplugin.GoroutinesResult{
		TotalCount: totalCount,
	}

	// Parse include and exclude patterns (patterns prefixed with - are exclusions)
	var includePatterns, excludePatterns []string
	if filter != "" {
		for _, pattern := range strings.Split(filter, ",") {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			if strings.HasPrefix(pattern, "-") {
				excludePatterns = append(excludePatterns, strings.ToLower(strings.TrimPrefix(pattern, "-")))
			} else {
				includePatterns = append(includePatterns, strings.ToLower(pattern))
			}
		}
	}

	// If include patterns provided, count matching goroutines (case-insensitive)
	if len(includePatterns) > 0 {
		goroutineStacks := strings.Split(stacks, "goroutine ")
		for _, stack := range goroutineStacks {
			if stack == "" {
				continue
			}
			stackLower := strings.ToLower(stack)

			// Check if stack matches any include pattern
			matched := false
			for _, pattern := range includePatterns {
				if strings.Contains(stackLower, pattern) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}

			// Check if stack matches any exclude pattern
			excluded := false
			for _, pattern := range excludePatterns {
				if strings.Contains(stackLower, pattern) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}

			result.MatchCount++
			// Truncate for readability
			if len(stack) > 1000 {
				stack = stack[:1000] + "..."
			}
			result.MatchedStacks = append(result.MatchedStacks, "goroutine "+stack)
		}
	}

	return result, nil
}
