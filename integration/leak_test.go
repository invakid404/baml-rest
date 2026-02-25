//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// countTotalMatches counts total matching goroutines across main process and all workers.
func countTotalMatches(result *testutil.GoroutinesResult) int {
	total := result.MatchCount
	for _, w := range result.Workers {
		total += w.MatchCount
	}
	return total
}

// TestGoroutineLeaks verifies that successful operations through all major endpoints
// don't leak goroutines. This complements the cancellation leak tests by ensuring
// the happy path is also clean.
func TestGoroutineLeaks(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	t.Run("call_endpoint_no_leak", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Let things settle and capture baseline
		time.Sleep(500 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(ctx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Baseline: %d total goroutines, %d matching filter", baselineResult.TotalCount, baselineMatches)

		// Run multiple successful call requests
		const numRequests = 10
		for i := 0; i < numRequests; i++ {
			scenarioID := "leak-test-call"
			content := `{"message": "test message"}`

			scenario := &mockllm.Scenario{
				ID:             scenarioID,
				Provider:       "openai",
				Content:        content,
				ChunkSize:      100,
				InitialDelayMs: 0,
				ChunkDelayMs:   0,
			}

			if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
				t.Fatalf("Failed to register scenario: %v", err)
			}

			opts := &testutil.BAMLOptions{
				ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
			}

			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetSimple",
				Input:   map[string]any{"input": "test"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call %d failed: %v", i, err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Call %d: expected status 200, got %d", i, resp.StatusCode)
			}
		}
		t.Logf("Completed %d successful /call requests", numRequests)

		// Wait for cleanup using polling with backoff
		finalResult, cleanedUp := waitForGoroutineCleanup(ctx, t, baselineMatches, 3*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Logf("Final: %d total goroutines, %d matching filter", finalResult.TotalCount, finalMatches)
			goroutineDelta := finalMatches - baselineMatches
			t.Errorf("Goroutine leak detected after %d /call requests: %d new goroutines", numRequests, goroutineDelta)
			logLeakedStacks(t, finalResult)
		}
	})

	t.Run("call_with_raw_endpoint_no_leak", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Capture baseline
		time.Sleep(500 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(ctx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Baseline: %d total goroutines, %d matching filter", baselineResult.TotalCount, baselineMatches)

		// Run multiple successful call/raw requests
		const numRequests = 10
		for i := 0; i < numRequests; i++ {
			scenarioID := "leak-test-call-raw"
			content := `{"message": "raw test"}`

			scenario := &mockllm.Scenario{
				ID:             scenarioID,
				Provider:       "openai",
				Content:        content,
				ChunkSize:      100,
				InitialDelayMs: 0,
				ChunkDelayMs:   0,
			}

			if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
				t.Fatalf("Failed to register scenario: %v", err)
			}

			opts := &testutil.BAMLOptions{
				ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
			}

			resp, err := BAMLClient.CallWithRaw(ctx, testutil.CallRequest{
				Method:  "GetSimple",
				Input:   map[string]any{"input": "test"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("CallWithRaw %d failed: %v", i, err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("CallWithRaw %d: expected status 200, got %d", i, resp.StatusCode)
			}
			if resp.Raw == "" {
				t.Errorf("CallWithRaw %d: expected raw content, got empty", i)
			}
		}
		t.Logf("Completed %d successful /call/raw requests", numRequests)

		// Wait for cleanup using polling with backoff
		finalResult, cleanedUp := waitForGoroutineCleanup(ctx, t, baselineMatches, 3*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Logf("Final: %d total goroutines, %d matching filter", finalResult.TotalCount, finalMatches)
			goroutineDelta := finalMatches - baselineMatches
			t.Errorf("Goroutine leak detected after %d /call/raw requests: %d new goroutines", numRequests, goroutineDelta)
			logLeakedStacks(t, finalResult)
		}
	})

	t.Run("stream_endpoint_no_leak", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Capture baseline
		time.Sleep(500 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(ctx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Baseline: %d total goroutines, %d matching filter", baselineResult.TotalCount, baselineMatches)

		// Run multiple successful stream requests
		const numRequests = 10
		for i := 0; i < numRequests; i++ {
			scenarioID := "leak-test-stream"
			content := `{"name": "Stream Test", "age": 42, "tags": ["streaming"]}`

			scenario := &mockllm.Scenario{
				ID:             scenarioID,
				Provider:       "openai",
				Content:        content,
				ChunkSize:      20,
				InitialDelayMs: 0,
				ChunkDelayMs:   10,
			}

			if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
				t.Fatalf("Failed to register scenario: %v", err)
			}

			opts := &testutil.BAMLOptions{
				ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
			}

			events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": "streaming test"},
				Options: opts,
			})

			var eventCount int
			var lastEvent testutil.StreamEvent
		streamLoop:
			for {
				select {
				case event, ok := <-events:
					if !ok {
						break streamLoop
					}
					eventCount++
					lastEvent = event
				case err := <-errs:
					if err != nil {
						t.Fatalf("Stream %d error: %v", i, err)
					}
				case <-ctx.Done():
					t.Fatalf("Stream %d: context cancelled", i)
				}
			}

			if eventCount == 0 {
				t.Fatalf("Stream %d: received no events", i)
			}

			// Verify final event has correct data
			var person struct {
				Name string `json:"name"`
				Age  int    `json:"age"`
			}
			if err := json.Unmarshal(lastEvent.Data, &person); err != nil {
				t.Fatalf("Stream %d: failed to unmarshal: %v", i, err)
			}
			if person.Name != "Stream Test" {
				t.Errorf("Stream %d: expected 'Stream Test', got '%s'", i, person.Name)
			}
		}
		t.Logf("Completed %d successful /stream requests", numRequests)

		// Wait for cleanup using polling with backoff
		finalResult, cleanedUp := waitForGoroutineCleanup(ctx, t, baselineMatches, 3*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Logf("Final: %d total goroutines, %d matching filter", finalResult.TotalCount, finalMatches)
			goroutineDelta := finalMatches - baselineMatches
			t.Errorf("Goroutine leak detected after %d /stream requests: %d new goroutines", numRequests, goroutineDelta)
			logLeakedStacks(t, finalResult)
		}
	})

	t.Run("stream_with_raw_endpoint_no_leak", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Capture baseline
		time.Sleep(500 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(ctx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Baseline: %d total goroutines, %d matching filter", baselineResult.TotalCount, baselineMatches)

		// Run multiple successful stream/raw requests
		const numRequests = 10
		for i := 0; i < numRequests; i++ {
			scenarioID := "leak-test-stream-raw"
			content := `{"message": "stream raw test"}`

			scenario := &mockllm.Scenario{
				ID:             scenarioID,
				Provider:       "openai",
				Content:        content,
				ChunkSize:      20,
				InitialDelayMs: 0,
				ChunkDelayMs:   10,
			}

			if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
				t.Fatalf("Failed to register scenario: %v", err)
			}

			opts := &testutil.BAMLOptions{
				ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
			}

			events, errs := BAMLClient.StreamWithRaw(ctx, testutil.CallRequest{
				Method:  "GetSimple",
				Input:   map[string]any{"input": "test"},
				Options: opts,
			})

			var eventCount int
			var lastRaw string
		streamLoop:
			for {
				select {
				case event, ok := <-events:
					if !ok {
						break streamLoop
					}
					eventCount++
					if event.Raw != "" {
						lastRaw = event.Raw
					}
				case err := <-errs:
					if err != nil {
						t.Fatalf("StreamWithRaw %d error: %v", i, err)
					}
				case <-ctx.Done():
					t.Fatalf("StreamWithRaw %d: context cancelled", i)
				}
			}

			if eventCount == 0 {
				t.Fatalf("StreamWithRaw %d: received no events", i)
			}
			if lastRaw == "" {
				t.Errorf("StreamWithRaw %d: expected raw content, got empty", i)
			}
		}
		t.Logf("Completed %d successful /stream/raw requests", numRequests)

		// Wait for cleanup using polling with backoff
		finalResult, cleanedUp := waitForGoroutineCleanup(ctx, t, baselineMatches, 3*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Logf("Final: %d total goroutines, %d matching filter", finalResult.TotalCount, finalMatches)
			goroutineDelta := finalMatches - baselineMatches
			t.Errorf("Goroutine leak detected after %d /stream/raw requests: %d new goroutines", numRequests, goroutineDelta)
			logLeakedStacks(t, finalResult)
		}
	})

	t.Run("parse_endpoint_no_leak", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Capture baseline
		time.Sleep(500 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(ctx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Baseline: %d total goroutines, %d matching filter", baselineResult.TotalCount, baselineMatches)

		// Run multiple successful parse requests
		// Parse doesn't require mock LLM - it just parses raw text
		const numRequests = 10
		for i := 0; i < numRequests; i++ {
			resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
				Method: "GetPerson",
				Raw:    `{"name": "Parse Test", "age": 25, "tags": ["parsing"]}`,
			})
			if err != nil {
				t.Fatalf("Parse %d failed: %v", i, err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Parse %d: expected status 200, got %d: %s", i, resp.StatusCode, resp.Error)
			}

			var person struct {
				Name string `json:"name"`
				Age  int    `json:"age"`
			}
			if err := json.Unmarshal(resp.Data, &person); err != nil {
				t.Fatalf("Parse %d: failed to unmarshal: %v", i, err)
			}
			if person.Name != "Parse Test" {
				t.Errorf("Parse %d: expected 'Parse Test', got '%s'", i, person.Name)
			}
		}
		t.Logf("Completed %d successful /parse requests", numRequests)

		// Wait for cleanup using polling with backoff
		finalResult, cleanedUp := waitForGoroutineCleanup(ctx, t, baselineMatches, 3*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Logf("Final: %d total goroutines, %d matching filter", finalResult.TotalCount, finalMatches)
			goroutineDelta := finalMatches - baselineMatches
			t.Errorf("Goroutine leak detected after %d /parse requests: %d new goroutines", numRequests, goroutineDelta)
			logLeakedStacks(t, finalResult)
		}
	})

	t.Run("mixed_endpoints_no_leak", func(t *testing.T) {
		// This test runs a mix of all endpoints to simulate real-world usage patterns
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		// Capture baseline after letting things settle
		time.Sleep(500 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(ctx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Baseline: %d total goroutines, %d matching filter", baselineResult.TotalCount, baselineMatches)

		// Run mixed requests
		const iterations = 5
		for iter := 0; iter < iterations; iter++ {
			// Call endpoint
			callScenario := &mockllm.Scenario{
				ID:       "leak-test-mixed-call",
				Provider: "openai",
				Content:  `{"message": "mixed test"}`,
			}
			if err := MockClient.RegisterScenario(ctx, callScenario); err != nil {
				t.Fatalf("Failed to register call scenario: %v", err)
			}
			callOpts := &testutil.BAMLOptions{
				ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, "leak-test-mixed-call"),
			}
			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetSimple",
				Input:   map[string]any{"input": "test"},
				Options: callOpts,
			})
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("Mixed iter %d: call failed: %v (status=%d)", iter, err, resp.StatusCode)
			}

			// Stream endpoint
			streamScenario := &mockllm.Scenario{
				ID:           "leak-test-mixed-stream",
				Provider:     "openai",
				Content:      `{"name": "Mixed", "age": 1, "tags": []}`,
				ChunkSize:    50,
				ChunkDelayMs: 5,
			}
			if err := MockClient.RegisterScenario(ctx, streamScenario); err != nil {
				t.Fatalf("Failed to register stream scenario: %v", err)
			}
			streamOpts := &testutil.BAMLOptions{
				ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, "leak-test-mixed-stream"),
			}
			events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": "mixed test"},
				Options: streamOpts,
			})
			drainEvents(events, errs, ctx, t)

			// Parse endpoint
			parseResp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
				Method: "GetSimple",
				Raw:    `{"message": "parsed"}`,
			})
			if err != nil || parseResp.StatusCode != 200 {
				t.Fatalf("Mixed iter %d: parse failed: %v (status=%d)", iter, err, parseResp.StatusCode)
			}
		}
		t.Logf("Completed %d iterations of mixed endpoint requests", iterations)

		// Wait for cleanup using polling with backoff (more robust than fixed sleep)
		finalResult, cleanedUp := waitForGoroutineCleanup(ctx, t, baselineMatches, 5*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Logf("Final: %d total goroutines, %d matching filter", finalResult.TotalCount, finalMatches)
			goroutineDelta := finalMatches - baselineMatches
			t.Errorf("Goroutine leak detected after mixed requests: %d new goroutines", goroutineDelta)
			logLeakedStacks(t, finalResult)
		}
	})
}

// logLeakedStacks logs the stack traces of leaked goroutines for debugging
func logLeakedStacks(t *testing.T, result *testutil.GoroutinesResult) {
	t.Helper()
	if len(result.MatchedStacks) > 0 {
		t.Logf("Main process leaked stacks:")
		for i, stack := range result.MatchedStacks {
			t.Logf("Leaked goroutine %d:\n%s", i+1, stack)
		}
	}
	for _, w := range result.Workers {
		if len(w.MatchedStacks) > 0 {
			t.Logf("Worker %d leaked stacks:", w.WorkerID)
			for i, stack := range w.MatchedStacks {
				t.Logf("Leaked goroutine %d:\n%s", i+1, stack)
			}
		}
	}
}

// waitForGoroutineCleanup polls until the goroutine count stabilizes (two consecutive
// readings match), then reports whether the stable count is at or below baseline.
//
// We wait for stability rather than just "count <= baseline" because baseline
// goroutines can exit during the polling window, which would mask a real leak
// via an unrelated count drop.
func waitForGoroutineCleanup(ctx context.Context, t *testing.T, baselineMatches int, maxWait time.Duration) (*testutil.GoroutinesResult, bool) {
	t.Helper()

	const pollInterval = 500 * time.Millisecond
	deadline := time.Now().Add(maxWait)

	var finalResult *testutil.GoroutinesResult
	prevMatches := -1

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		result, err := BAMLClient.GetGoroutines(ctx, GoroutineLeakFilter)
		if err != nil {
			t.Logf("Error during goroutine polling: %v", err)
			continue
		}

		finalResult = result
		finalMatches := countTotalMatches(result)

		if finalMatches <= baselineMatches && finalMatches == prevMatches {
			// Stable at or below baseline: cleanup confirmed.
			t.Logf("Goroutines cleaned up: final=%d, baseline=%d", finalMatches, baselineMatches)
			return result, true
		}

		t.Logf("Waiting for goroutine cleanup: %d matching (baseline %d)...", finalMatches, baselineMatches)
		prevMatches = finalMatches
	}

	if finalResult == nil {
		t.Fatalf("Failed to get any goroutine snapshot during %v polling window", maxWait)
	}

	// Timed out without stabilization — never report success since we can't
	// distinguish a transient low reading from actual cleanup.
	return finalResult, false
}

// drainEvents consumes all events from a stream, failing the test on error
func drainEvents(events <-chan testutil.StreamEvent, errs <-chan error, ctx context.Context, t *testing.T) {
	t.Helper()
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case err := <-errs:
			if err != nil {
				t.Fatalf("Stream error while draining: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("Context cancelled while draining events")
		}
	}
}

// TestNDJSONGoroutineLeaks verifies that NDJSON streaming operations don't leak goroutines.
func TestNDJSONGoroutineLeaks(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	t.Run("stream_ndjson_endpoint_no_leak", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Capture baseline
		time.Sleep(500 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(ctx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Baseline: %d total goroutines, %d matching filter", baselineResult.TotalCount, baselineMatches)

		// Run multiple successful NDJSON stream requests
		const numRequests = 10
		for i := 0; i < numRequests; i++ {
			scenarioID := "leak-test-stream-ndjson"
			content := `{"name": "NDJSON Test", "age": 42, "tags": ["streaming"]}`

			scenario := &mockllm.Scenario{
				ID:             scenarioID,
				Provider:       "openai",
				Content:        content,
				ChunkSize:      20,
				InitialDelayMs: 0,
				ChunkDelayMs:   10,
			}

			if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
				t.Fatalf("Failed to register scenario: %v", err)
			}

			opts := &testutil.BAMLOptions{
				ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
			}

			events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": "ndjson streaming test"},
				Options: opts,
			})

			var eventCount int
			var lastEvent testutil.StreamEvent
		streamLoop:
			for {
				select {
				case event, ok := <-events:
					if !ok {
						break streamLoop
					}
					eventCount++
					lastEvent = event
				case err := <-errs:
					if err != nil {
						t.Fatalf("StreamNDJSON %d error: %v", i, err)
					}
				case <-ctx.Done():
					t.Fatalf("StreamNDJSON %d: context cancelled", i)
				}
			}

			if eventCount == 0 {
				t.Fatalf("StreamNDJSON %d: received no events", i)
			}

			// Verify final event has correct data
			var person struct {
				Name string `json:"name"`
				Age  int    `json:"age"`
			}
			if err := json.Unmarshal(lastEvent.Data, &person); err != nil {
				t.Fatalf("StreamNDJSON %d: failed to unmarshal: %v", i, err)
			}
			if person.Name != "NDJSON Test" {
				t.Errorf("StreamNDJSON %d: expected 'NDJSON Test', got '%s'", i, person.Name)
			}
		}
		t.Logf("Completed %d successful NDJSON /stream requests", numRequests)

		// Wait for cleanup using polling with backoff
		finalResult, cleanedUp := waitForGoroutineCleanup(ctx, t, baselineMatches, 3*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Logf("Final: %d total goroutines, %d matching filter", finalResult.TotalCount, finalMatches)
			goroutineDelta := finalMatches - baselineMatches
			t.Errorf("Goroutine leak detected after %d NDJSON /stream requests: %d new goroutines", numRequests, goroutineDelta)
			logLeakedStacks(t, finalResult)
		}
	})

	t.Run("stream_with_raw_ndjson_endpoint_no_leak", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Capture baseline
		time.Sleep(500 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(ctx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Baseline: %d total goroutines, %d matching filter", baselineResult.TotalCount, baselineMatches)

		// Run multiple successful NDJSON stream/raw requests
		const numRequests = 10
		for i := 0; i < numRequests; i++ {
			scenarioID := "leak-test-stream-raw-ndjson"
			content := `{"message": "ndjson stream raw test"}`

			scenario := &mockllm.Scenario{
				ID:             scenarioID,
				Provider:       "openai",
				Content:        content,
				ChunkSize:      20,
				InitialDelayMs: 0,
				ChunkDelayMs:   10,
			}

			if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
				t.Fatalf("Failed to register scenario: %v", err)
			}

			opts := &testutil.BAMLOptions{
				ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
			}

			events, errs := BAMLClient.StreamWithRawNDJSON(ctx, testutil.CallRequest{
				Method:  "GetSimple",
				Input:   map[string]any{"input": "test"},
				Options: opts,
			})

			var eventCount int
			var lastRaw string
		streamLoop:
			for {
				select {
				case event, ok := <-events:
					if !ok {
						break streamLoop
					}
					eventCount++
					if event.Raw != "" {
						lastRaw = event.Raw
					}
				case err := <-errs:
					if err != nil {
						t.Fatalf("StreamWithRawNDJSON %d error: %v", i, err)
					}
				case <-ctx.Done():
					t.Fatalf("StreamWithRawNDJSON %d: context cancelled", i)
				}
			}

			if eventCount == 0 {
				t.Fatalf("StreamWithRawNDJSON %d: received no events", i)
			}
			if lastRaw == "" {
				t.Errorf("StreamWithRawNDJSON %d: expected raw content, got empty", i)
			}
		}
		t.Logf("Completed %d successful NDJSON /stream-with-raw requests", numRequests)

		// Wait for cleanup using polling with backoff
		finalResult, cleanedUp := waitForGoroutineCleanup(ctx, t, baselineMatches, 3*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Logf("Final: %d total goroutines, %d matching filter", finalResult.TotalCount, finalMatches)
			goroutineDelta := finalMatches - baselineMatches
			t.Errorf("Goroutine leak detected after %d NDJSON /stream-with-raw requests: %d new goroutines", numRequests, goroutineDelta)
			logLeakedStacks(t, finalResult)
		}
	})
}
