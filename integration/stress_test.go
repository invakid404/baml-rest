//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// ---------------------------------------------------------------------------
// Stress test telemetry helpers
// ---------------------------------------------------------------------------

// errorSampler collects the first N unique error messages per category (endpoint
// type) and counts total occurrences of each unique error. This lets us see
// *what* is failing without flooding logs.
type errorSampler struct {
	mu       sync.Mutex
	maxUniq  int                       // max unique error strings to keep per category
	errors   map[string]map[string]int // category -> error string -> count
	ordering map[string][]string       // category -> insertion-ordered unique errors
}

func newErrorSampler(maxUniq int) *errorSampler {
	return &errorSampler{
		maxUniq:  maxUniq,
		errors:   make(map[string]map[string]int),
		ordering: make(map[string][]string),
	}
}

func (s *errorSampler) record(category string, err error, extra string) {
	msg := err.Error()
	if extra != "" {
		msg = extra + ": " + msg
	}
	// Truncate very long messages for readability.
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.errors[category] == nil {
		s.errors[category] = make(map[string]int)
	}
	s.errors[category][msg]++
	// Track insertion order, capped at maxUniq.
	if s.errors[category][msg] == 1 && len(s.ordering[category]) < s.maxUniq {
		s.ordering[category] = append(s.ordering[category], msg)
	}
}

// recordHTTP records a non-2xx HTTP status as an error.
func (s *errorSampler) recordHTTP(category string, statusCode int, errBody string) {
	msg := fmt.Sprintf("HTTP %d", statusCode)
	if errBody != "" {
		body := errBody
		if len(body) > 200 {
			body = body[:200] + "..."
		}
		msg += ": " + body
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.errors[category] == nil {
		s.errors[category] = make(map[string]int)
	}
	s.errors[category][msg]++
	if s.errors[category][msg] == 1 && len(s.ordering[category]) < s.maxUniq {
		s.ordering[category] = append(s.ordering[category], msg)
	}
}

func (s *errorSampler) dump(t *testing.T) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for category, order := range s.ordering {
		if len(order) == 0 {
			continue
		}
		t.Logf("  [%s] %d unique error(s):", category, len(s.errors[category]))
		for i, msg := range order {
			count := s.errors[category][msg]
			t.Logf("    #%d (%dx): %s", i+1, count, msg)
		}
		// If there are more unique errors than we sampled, note it.
		total := len(s.errors[category])
		if total > len(order) {
			t.Logf("    ... and %d more unique error(s) not shown", total-len(order))
		}
	}
}

func (s *errorSampler) totalErrors(category string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, count := range s.errors[category] {
		total += count
	}
	return total
}

// latencyTracker records request durations and computes percentiles.
// It uses reservoir sampling to bound memory for large request volumes.
type latencyTracker struct {
	mu        sync.Mutex
	durations []time.Duration
	count     int64
	maxStore  int // max samples to keep in reservoir
}

func newLatencyTracker(maxStore int) *latencyTracker {
	return &latencyTracker{
		durations: make([]time.Duration, 0, maxStore),
		maxStore:  maxStore,
	}
}

func (lt *latencyTracker) record(d time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.count++
	if len(lt.durations) < lt.maxStore {
		lt.durations = append(lt.durations, d)
	} else {
		// Reservoir sampling: replace a random element with decreasing probability.
		// For simplicity, we keep all samples up to maxStore, then keep every Nth.
		// This is biased but good enough for diagnostics.
		if lt.count%int64(lt.maxStore/1000+1) == 0 {
			idx := lt.count % int64(lt.maxStore)
			lt.durations[idx] = d
		}
	}
}

func (lt *latencyTracker) percentiles() (p50, p95, p99, max time.Duration, count int64) {
	lt.mu.Lock()
	sorted := make([]time.Duration, len(lt.durations))
	copy(sorted, lt.durations)
	count = lt.count
	lt.mu.Unlock()

	if len(sorted) == 0 {
		return
	}

	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	pct := func(p float64) time.Duration {
		idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}

	p50 = pct(50)
	p95 = pct(95)
	p99 = pct(99)
	max = sorted[len(sorted)-1]
	return
}

func (lt *latencyTracker) report(t *testing.T, label string) {
	p50, p95, p99, max, count := lt.percentiles()
	if count == 0 {
		t.Logf("  [%s] latency: no samples", label)
		return
	}
	t.Logf("  [%s] latency (%d samples): p50=%s p95=%s p99=%s max=%s",
		label, count, p50.Round(time.Millisecond), p95.Round(time.Millisecond),
		p99.Round(time.Millisecond), max.Round(time.Millisecond))
}

// dumpDiagnostics calls the /_debug endpoints and logs server-side state.
// This is the key function that gives us visibility when things go wrong.
func dumpDiagnostics(t *testing.T, ctx context.Context, reason string) {
	t.Logf("=== DIAGNOSTIC DUMP (%s) ===", reason)

	// Dump recent container logs (last 30s) to see BAML-side instrumentation.
	if TestEnv != nil && TestEnv.BAMLRest != nil {
		logCtx, logCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer logCancel()
		since := time.Now().Add(-30 * time.Second)
		reader, err := TestEnv.BAMLRest.Logs(logCtx)
		if err != nil {
			t.Logf("  [container-logs] ERROR: %v", err)
		} else {
			logBytes, _ := io.ReadAll(reader)
			_ = reader.Close()
			logStr := string(logBytes)
			// Only show baml-tagged lines to reduce noise
			var filtered []string
			for _, line := range strings.Split(logStr, "\n") {
				if strings.Contains(line, "[baml-") {
					filtered = append(filtered, line)
				}
			}
			if len(filtered) > 100 {
				filtered = filtered[len(filtered)-100:] // last 100 lines
			}
			t.Logf("  [container-logs] %d baml-tagged lines (last 100):", len(filtered))
			for _, line := range filtered {
				t.Logf("    %s", line)
			}
			_ = since // suppress unused warning
		}
	}

	// Use a short timeout so diagnostics don't hang if the server is stuck.
	diagCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// 1. In-flight request status
	inFlight, err := BAMLClient.GetInFlightStatus(diagCtx)
	if err != nil {
		t.Logf("  [in-flight] ERROR: %v", err)
	} else {
		t.Logf("  [in-flight] status=%s", inFlight.Status)
		for _, w := range inFlight.Workers {
			firstByteStr := ""
			if len(w.GotFirstByte) > 0 {
				trueCount := 0
				for _, b := range w.GotFirstByte {
					if b {
						trueCount++
					}
				}
				firstByteStr = fmt.Sprintf(" (got_first_byte: %d/%d)", trueCount, len(w.GotFirstByte))
			}
			t.Logf("    worker[%d]: healthy=%v in_flight=%d%s",
				w.WorkerID, w.Healthy, w.InFlight, firstByteStr)
		}
	}

	// 2. Goroutine dump — filter to pool/workerplugin stacks to keep output manageable.
	goroutines, err := BAMLClient.GetGoroutines(diagCtx, "pool.,workerplugin.,grpc,net/http")
	if err != nil {
		t.Logf("  [goroutines] ERROR: %v", err)
	} else {
		t.Logf("  [goroutines] total=%d matched=%d", goroutines.TotalCount, goroutines.MatchCount)
		for _, stack := range goroutines.MatchedStacks {
			// Indent each line of the stack for readability.
			for _, line := range strings.Split(stack, "\n") {
				if line != "" {
					t.Logf("    %s", line)
				}
			}
			t.Logf("    ---")
		}
		for _, w := range goroutines.Workers {
			if w.Error != "" {
				t.Logf("  [goroutines] worker[%d] ERROR: %s", w.WorkerID, w.Error)
			} else {
				t.Logf("  [goroutines] worker[%d]: total=%d matched=%d", w.WorkerID, w.TotalCount, w.MatchCount)
				for _, stack := range w.MatchedStacks {
					for _, line := range strings.Split(stack, "\n") {
						if line != "" {
							t.Logf("      %s", line)
						}
					}
					t.Logf("      ---")
				}
			}
		}
	}

	// 3. Native thread backtraces via gdb — shows where Rust/C threads are blocked.
	nativeStacks, err := BAMLClient.GetNativeStacks(diagCtx)
	if err != nil {
		t.Logf("  [native-stacks] ERROR: %v", err)
	} else {
		for _, w := range nativeStacks.Workers {
			if w.Error != "" {
				t.Logf("  [native-stacks] worker[%d] (pid=%d) ERROR: %s", w.WorkerID, w.Pid, w.Error)
			} else {
				lines := strings.Split(w.Output, "\n")
				t.Logf("  [native-stacks] worker[%d] (pid=%d): %d lines of gdb output", w.WorkerID, w.Pid, len(lines))
				for _, line := range lines {
					if line != "" {
						t.Logf("    %s", line)
					}
				}
			}
		}
	}

	t.Logf("=== END DIAGNOSTIC DUMP ===")
}

// ---------------------------------------------------------------------------
// TestStressNoDeadlock
// ---------------------------------------------------------------------------

// TestStressNoDeadlock fires 100k+ parallel requests through the server (which now
// defaults to a single worker) to verify that BAML's Rust runtime and Tokio don't
// deadlock under extreme concurrency.
//
// Telemetry: logs first 10 unique errors per endpoint, tracks latency percentiles,
// detects stalls (zero progress for 30s) and dumps server-side diagnostics.
func TestStressNoDeadlock(t *testing.T) {
	const (
		callRequests   = 40_000
		streamRequests = 20_000
		parseRequests  = 40_000
		maxConcurrency = 100
		minSuccessRate = 0.95

		// Stall detection: if no requests complete in this window, dump diagnostics.
		stallWindow = 30 * time.Second
	)

	totalRequests := callRequests + streamRequests + parseRequests
	t.Logf("Stress test: %d total requests (%d call, %d stream, %d parse) with max concurrency %d",
		totalRequests, callRequests, streamRequests, parseRequests, maxConcurrency)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Register scenarios with zero delay for maximum throughput.
	callScenarioID := "stress-call"
	callScenario := &mockllm.Scenario{
		ID:             callScenarioID,
		Provider:       "openai",
		Content:        `{"message": "stress test response"}`,
		ChunkSize:      0,
		InitialDelayMs: 0,
	}
	if err := MockClient.RegisterScenario(ctx, callScenario); err != nil {
		t.Fatalf("Failed to register call scenario: %v", err)
	}

	streamScenarioID := "stress-stream"
	streamScenario := &mockllm.Scenario{
		ID:             streamScenarioID,
		Provider:       "openai",
		Content:        `{"name": "Stress", "age": 1, "tags": ["test"]}`,
		ChunkSize:      100,
		InitialDelayMs: 0,
		ChunkDelayMs:   0,
	}
	if err := MockClient.RegisterScenario(ctx, streamScenario); err != nil {
		t.Fatalf("Failed to register stream scenario: %v", err)
	}

	callOpts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, callScenarioID),
	}
	streamOpts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, streamScenarioID),
	}

	sem := make(chan struct{}, maxConcurrency)

	var (
		wg         sync.WaitGroup
		callOK     atomic.Int64
		callFail   atomic.Int64
		streamOK   atomic.Int64
		streamFail atomic.Int64
		parseOK    atomic.Int64
		parseFail  atomic.Int64
	)

	// Telemetry
	errSampler := newErrorSampler(10)
	callLatency := newLatencyTracker(10_000)
	streamLatency := newLatencyTracker(10_000)
	parseLatency := newLatencyTracker(10_000)

	done := make(chan struct{})
	start := time.Now()

	// Progress reporter with stall detection.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		var lastCompleted int64
		lastProgress := time.Now()
		stallDumped := false

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				completed := callOK.Load() + callFail.Load() +
					streamOK.Load() + streamFail.Load() +
					parseOK.Load() + parseFail.Load()
				elapsed := time.Since(start).Round(time.Second)
				rate := float64(0)
				if elapsed.Seconds() > 0 {
					rate = float64(completed) / elapsed.Seconds()
				}

				t.Logf("[%s] progress: %d/%d (%.0f req/s) call=%d+%d stream=%d+%d parse=%d+%d",
					elapsed, completed, totalRequests, rate,
					callOK.Load(), callFail.Load(),
					streamOK.Load(), streamFail.Load(),
					parseOK.Load(), parseFail.Load())

				// Stall detection
				if completed > lastCompleted {
					lastCompleted = completed
					lastProgress = time.Now()
					stallDumped = false
				} else if time.Since(lastProgress) > stallWindow && !stallDumped {
					t.Logf("WARNING: no progress for %s (stuck at %d/%d)",
						time.Since(lastProgress).Round(time.Second), completed, totalRequests)
					dumpDiagnostics(t, ctx, fmt.Sprintf("stall at %d/%d after %s", completed, totalRequests, elapsed))
					stallDumped = true
					// Dump again if still stuck after another window
					lastProgress = time.Now()
				}
			}
		}
	}()

	// --- /call launcher ---
	wg.Add(callRequests)
	go func() {
		for i := 0; i < callRequests; i++ {
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				reqStart := time.Now()
				resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
					Method:  "GetSimple",
					Input:   map[string]any{"input": "stress"},
					Options: callOpts,
				})
				elapsed := time.Since(reqStart)

				if err != nil {
					callFail.Add(1)
					errSampler.record("call", err, "")
					return
				}
				if resp.StatusCode != 200 {
					callFail.Add(1)
					errSampler.recordHTTP("call", resp.StatusCode, resp.Error)
					return
				}
				callLatency.record(elapsed)
				callOK.Add(1)
			}()
		}
	}()

	// --- /stream launcher ---
	wg.Add(streamRequests)
	go func() {
		for i := 0; i < streamRequests; i++ {
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				reqStart := time.Now()
				events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
					Method:  "GetPerson",
					Input:   map[string]any{"description": "stress"},
					Options: streamOpts,
				})

				var gotEvents bool
			loop:
				for {
					select {
					case _, ok := <-events:
						if !ok {
							break loop
						}
						gotEvents = true
					case err := <-errs:
						if err != nil {
							streamFail.Add(1)
							errSampler.record("stream", err, "")
							return
						}
					case <-ctx.Done():
						streamFail.Add(1)
						errSampler.record("stream", ctx.Err(), "context")
						return
					}
				}

				elapsed := time.Since(reqStart)
				if gotEvents {
					streamLatency.record(elapsed)
					streamOK.Add(1)
				} else {
					streamFail.Add(1)
					errSampler.record("stream", fmt.Errorf("stream completed with zero events"), "")
				}
			}()
		}
	}()

	// --- /parse launcher ---
	wg.Add(parseRequests)
	go func() {
		for i := 0; i < parseRequests; i++ {
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				reqStart := time.Now()
				resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
					Method: "GetPerson",
					Raw:    `{"name": "Stress Test", "age": 1, "tags": ["parse"]}`,
				})
				elapsed := time.Since(reqStart)

				if err != nil {
					parseFail.Add(1)
					errSampler.record("parse", err, "")
					return
				}
				if resp.StatusCode != 200 {
					parseFail.Add(1)
					errSampler.recordHTTP("parse", resp.StatusCode, resp.Error)
					return
				}
				parseLatency.record(elapsed)
				parseOK.Add(1)
			}()
		}
	}()

	// Wait for all requests with deadlock detection.
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		// All requests completed.
	case <-ctx.Done():
		close(done)
		t.Logf("DEADLOCK DETECTED: context expired after %s", time.Since(start).Round(time.Second))
		dumpDiagnostics(t, context.Background(), "deadlock/timeout")
		t.Logf("Error details:")
		errSampler.dump(t)
		t.Logf("Latency percentiles:")
		callLatency.report(t, "call")
		streamLatency.report(t, "stream")
		parseLatency.report(t, "parse")
		t.Fatalf("DEADLOCK: requests still in flight (call=%d/%d stream=%d/%d parse=%d/%d)",
			callOK.Load()+callFail.Load(), callRequests,
			streamOK.Load()+streamFail.Load(), streamRequests,
			parseOK.Load()+parseFail.Load(), parseRequests)
	}

	close(done)
	elapsed := time.Since(start).Round(time.Millisecond)

	cOK, cFail := callOK.Load(), callFail.Load()
	sOK, sFail := streamOK.Load(), streamFail.Load()
	pOK, pFail := parseOK.Load(), parseFail.Load()

	t.Logf("Stress test completed in %s", elapsed)
	t.Logf("  /call:   %d OK, %d failed (of %d) — %.1f%% success",
		cOK, cFail, callRequests, float64(cOK)/float64(callRequests)*100)
	t.Logf("  /stream: %d OK, %d failed (of %d) — %.1f%% success",
		sOK, sFail, streamRequests, float64(sOK)/float64(streamRequests)*100)
	t.Logf("  /parse:  %d OK, %d failed (of %d) — %.1f%% success",
		pOK, pFail, parseRequests, float64(pOK)/float64(parseRequests)*100)
	t.Logf("  throughput: %.0f req/s", float64(totalRequests)/elapsed.Seconds())

	// Latency percentiles
	t.Logf("Latency percentiles:")
	callLatency.report(t, "call")
	streamLatency.report(t, "stream")
	parseLatency.report(t, "parse")

	// Error details (always dump if there were any failures)
	if cFail > 0 || sFail > 0 || pFail > 0 {
		t.Logf("Error details:")
		errSampler.dump(t)

		// Dump full container logs so we can see worker-side errors
		t.Logf("Dumping container logs due to %d total failure(s)...", cFail+sFail+pFail)
		dumpContainerLogs("BAML REST", TestEnv.BAMLRest)
		dumpContainerLogs("Mock LLM", TestEnv.MockLLM)
	}

	callRate := float64(cOK) / float64(callRequests)
	streamRate := float64(sOK) / float64(streamRequests)
	parseRate := float64(pOK) / float64(parseRequests)

	if callRate < minSuccessRate {
		t.Errorf("/call success rate %.1f%% is below minimum %.0f%%", callRate*100, minSuccessRate*100)
	}
	if streamRate < minSuccessRate {
		t.Errorf("/stream success rate %.1f%% is below minimum %.0f%%", streamRate*100, minSuccessRate*100)
	}
	if parseRate < minSuccessRate {
		t.Errorf("/parse success rate %.1f%% is below minimum %.0f%%", parseRate*100, minSuccessRate*100)
	}
}

// ---------------------------------------------------------------------------
// TestStressConcurrentStreams
// ---------------------------------------------------------------------------

// TestStressConcurrentStreams focuses specifically on streaming, which historically
// was the most deadlock-prone path due to callback pressure on the Tokio runtime.
//
// Telemetry: logs first 10 unique errors, tracks stream latency percentiles,
// detects stalls and dumps server-side diagnostics.
func TestStressConcurrentStreams(t *testing.T) {
	const (
		totalStreams   = 10_000
		maxConcurrency = 50
		minSuccessRate = 0.95
		stallWindow    = 30 * time.Second
	)

	t.Logf("Concurrent streams stress test: %d streams with max concurrency %d", totalStreams, maxConcurrency)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	scenarioID := "stress-concurrent-stream"
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        `{"name": "ConcurrentStream", "age": 42, "tags": ["a", "b", "c"]}`,
		ChunkSize:      15,
		InitialDelayMs: 0,
		ChunkDelayMs:   1,
		ChunkJitterMs:  1,
	}
	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	opts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
	}

	sem := make(chan struct{}, maxConcurrency)
	var (
		wg       sync.WaitGroup
		okCount  atomic.Int64
		errCount atomic.Int64
	)

	errSampler := newErrorSampler(10)
	streamLatency := newLatencyTracker(10_000)

	start := time.Now()
	done := make(chan struct{})

	// Progress reporter with stall detection.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		var lastCompleted int64
		lastProgress := time.Now()
		stallDumped := false

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				completed := okCount.Load() + errCount.Load()
				elapsed := time.Since(start).Round(time.Second)
				rate := float64(0)
				if elapsed.Seconds() > 0 {
					rate = float64(completed) / elapsed.Seconds()
				}

				t.Logf("[%s] streams: %d/%d (%.0f/s) ok=%d failed=%d",
					elapsed, completed, totalStreams, rate,
					okCount.Load(), errCount.Load())

				if completed > lastCompleted {
					lastCompleted = completed
					lastProgress = time.Now()
					stallDumped = false
				} else if time.Since(lastProgress) > stallWindow && !stallDumped {
					t.Logf("WARNING: no progress for %s (stuck at %d/%d)",
						time.Since(lastProgress).Round(time.Second), completed, totalStreams)
					dumpDiagnostics(t, ctx, fmt.Sprintf("stall at %d/%d after %s", completed, totalStreams, elapsed))
					stallDumped = true
					lastProgress = time.Now()
				}
			}
		}
	}()

	wg.Add(totalStreams)
	go func() {
		for i := 0; i < totalStreams; i++ {
			sem <- struct{}{}
			go func(idx int) {
				defer wg.Done()
				defer func() { <-sem }()

				reqStart := time.Now()
				events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
					Method:  "GetPerson",
					Input:   map[string]any{"description": fmt.Sprintf("stream-%d", idx)},
					Options: opts,
				})

				var gotFinal bool
			loop:
				for {
					select {
					case ev, ok := <-events:
						if !ok {
							break loop
						}
						if ev.IsFinal() {
							gotFinal = true
						}
					case err := <-errs:
						if err != nil {
							errCount.Add(1)
							errSampler.record("stream", err, "")
							return
						}
					case <-ctx.Done():
						errCount.Add(1)
						errSampler.record("stream", ctx.Err(), "context")
						return
					}
				}

				elapsed := time.Since(reqStart)
				if gotFinal {
					streamLatency.record(elapsed)
					okCount.Add(1)
				} else {
					errCount.Add(1)
					errSampler.record("stream", fmt.Errorf("stream completed without final event"), "")
				}
			}(i)
		}
	}()

	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
	case <-ctx.Done():
		close(done)
		t.Logf("DEADLOCK DETECTED: context expired after %s", time.Since(start).Round(time.Second))
		dumpDiagnostics(t, context.Background(), "deadlock/timeout")
		t.Logf("Error details:")
		errSampler.dump(t)
		t.Logf("Latency:")
		streamLatency.report(t, "stream")
		t.Fatalf("DEADLOCK: streams still in flight (%d/%d completed)",
			okCount.Load()+errCount.Load(), totalStreams)
	}

	close(done)
	elapsed := time.Since(start).Round(time.Millisecond)

	ok, fail := okCount.Load(), errCount.Load()
	successRate := float64(ok) / float64(totalStreams)

	t.Logf("Concurrent streams completed in %s: %d ok, %d failed — %.1f%% success (%.0f streams/s)",
		elapsed, ok, fail, successRate*100, float64(totalStreams)/elapsed.Seconds())

	t.Logf("Latency:")
	streamLatency.report(t, "stream")

	if fail > 0 {
		t.Logf("Error details:")
		errSampler.dump(t)
	}

	if successRate < minSuccessRate {
		t.Errorf("Stream success rate %.1f%% is below minimum %.0f%%", successRate*100, minSuccessRate*100)
	}
}
