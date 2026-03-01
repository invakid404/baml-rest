//go:build integration

package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

func TestStreamEndpoint(t *testing.T) {
	t.Run("multiple_sse_events", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"name": "John Doe", "age": 30, "tags": []}`
		opts := setupScenario(t, "test-stream-person", content)

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "John, 30 years old"},
			Options: opts,
		})

		var eventCount int
		var lastEvent testutil.StreamEvent

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++
				lastEvent = event
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		// Should have received multiple events (chunked streaming)
		if eventCount < 2 {
			t.Errorf("Expected multiple events, got %d", eventCount)
		}

		// Last event should have the complete data
		var person struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}
		if err := json.Unmarshal(lastEvent.Data, &person); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if person.Name != "John Doe" || person.Age != 30 {
			t.Errorf("Expected John Doe (30), got %s (%d)", person.Name, person.Age)
		}
	})

	t.Run("streaming_array", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `[{"name": "Alice", "age": 25, "tags": []}, {"name": "Bob", "age": 35, "tags": []}]`
		opts := setupScenario(t, "test-stream-array", content)

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPeople",
			Input:   map[string]any{"descriptions": "Alice and Bob"},
			Options: opts,
		})

		var eventCount int
		var lastEvent testutil.StreamEvent

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++
				lastEvent = event
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		// Verify final result is complete array
		var people []struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}
		if err := json.Unmarshal(lastEvent.Data, &people); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if len(people) != 2 {
			t.Errorf("Expected 2 people, got %d", len(people))
		}
	})
}

func TestStreamMidStreamFailure(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	// This test suite verifies what happens when something goes wrong mid-stream.
	//
	// IMPORTANT CONTEXT (based on code analysis of pool/pool.go):
	//
	// 1. The hung detection (firstByteTimeout) ONLY monitors requests where gotFirstByte=false
	//    See pool.go:330: "if !req.gotFirstByte.Load() && now.Sub(req.startedAt) > p.config.FirstByteTimeout"
	//
	// 2. Pool-level retries happen in two scenarios:
	//    a) When handle.worker.CallStream() returns an error (before first result)
	//    b) When a retryable infrastructure error occurs mid-stream (worker death, gRPC Unavailable)
	//
	// 3. The gRPC client CallStream() blocks until the server sends response headers
	//    Headers are sent when the first stream.Send() is called (see workerplugin/grpc.go:49-77)
	//
	// 4. Mid-stream INFRASTRUCTURE failures (worker crash, gRPC Unavailable) ARE retried:
	//    - Pool detects isRetryableWorkerError() and retries on a new worker
	//    - A "reset" message is injected to tell clients to discard partial state
	//    - See TestWorkerDeathMidStream for tests of this behavior
	//
	// 5. Mid-stream APPLICATION failures (LLM disconnect, timeout) are NOT retried:
	//    - These come from the upstream LLM, not our infrastructure
	//    - The error is passed through to the client
	//    - This test suite covers these scenarios
	//
	// 6. For BAML-internal retries (rate limits, API errors), reset messages ARE sent correctly
	//    via IncrementalExtractor detecting callCount changes.

	t.Run("disconnect_after_first_byte", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Create a scenario that will disconnect after 3 chunks
		// Content is long enough to have multiple chunks
		content := `{"name": "John Doe", "age": 30, "tags": ["developer", "golang"]}`
		scenarioID := "test-midstream-disconnect"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      10, // Small chunks so we get multiple before failure
			InitialDelayMs: 10,
			ChunkDelayMs:   50, // Slow enough to observe partial data
			FailAfter:      3,  // Fail after 3 chunks
			FailureMode:    "disconnect",
		}

		if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "John, 30 years old"},
			Options: opts,
		})

		var receivedEvents []testutil.StreamEvent
		var streamErr error
		var sawResetEvent bool
		var sawErrorEvent bool

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents = append(receivedEvents, event)
				t.Logf("Received event %d: type=%s, data_len=%d", len(receivedEvents), event.Event, len(event.Data))
				if event.Event == "reset" {
					sawResetEvent = true
				}
				if event.Event == "error" {
					sawErrorEvent = true
				}
			case err := <-errs:
				if err != nil {
					streamErr = err
					t.Logf("Received stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		t.Logf("Total events received: %d", len(receivedEvents))
		t.Logf("Stream error: %v", streamErr)
		t.Logf("Saw reset event: %v", sawResetEvent)
		t.Logf("Saw error event: %v", sawErrorEvent)

		for i, event := range receivedEvents {
			t.Logf("Event %d: type=%q, data=%s", i, event.Event, string(event.Data))
		}

		// EXPECTED BEHAVIOR (based on code analysis):
		// - We should receive SOME events (partial data before disconnect)
		// - We should get an error (SSE error event or stream error)
		// - We should NOT see a reset event (no retry happens for mid-stream failures)
		// - The request is NOT retried because first byte was received

		if len(receivedEvents) == 0 && streamErr == nil {
			t.Error("Expected to receive some events or an error, got neither")
		}

		// Document: mid-stream failures don't trigger pool-level retries
		// A reset event here would indicate either a BAML-internal retry or a bug
		if sawResetEvent {
			t.Log("NOTE: Saw reset event - this indicates a BAML-internal retry occurred")
		}
	})

	t.Run("timeout_after_first_byte", func(t *testing.T) {
		// Use a shorter timeout since we expect this to hang
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Create a scenario that will timeout (hang) after 3 chunks
		content := `{"name": "Jane Doe", "age": 25, "tags": ["engineer"]}`
		scenarioID := "test-midstream-timeout"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      10,
			InitialDelayMs: 10,
			ChunkDelayMs:   50,
			FailAfter:      3,
			FailureMode:    "timeout", // Will hang after 3 chunks
		}

		if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Jane, 25 years old"},
			Options: opts,
		})

		var receivedEvents []testutil.StreamEvent
		var streamErr error
		startTime := time.Now()

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents = append(receivedEvents, event)
				t.Logf("Received event %d: type=%s", len(receivedEvents), event.Event)
			case err := <-errs:
				if err != nil {
					streamErr = err
					t.Logf("Received stream error: %v", err)
				}
			case <-ctx.Done():
				t.Logf("Context cancelled after %v", time.Since(startTime))
				streamErr = ctx.Err()
				goto done
			}
		}
	done:

		t.Logf("Total events received: %d", len(receivedEvents))
		t.Logf("Stream error: %v", streamErr)
		t.Logf("Duration: %v", time.Since(startTime))

		// Should have received some events before timeout
		// The client-side timeout (10s) should trigger before the server timeout
	})
}

func TestWorkerDeathMidStream(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	// This test verifies what happens when a WORKER PROCESS dies mid-stream,
	// as opposed to just the upstream LLM failing.
	//
	// This is testing infrastructure-level failures, not application-level failures.
	// Uses the /_debug/kill-worker endpoint to kill a worker mid-request.
	//
	// EXPECTED BEHAVIOR (after mid-stream retry implementation):
	// - Worker is killed after first byte is received
	// - Pool detects the retryable error (gRPC Unavailable/EOF)
	// - Pool gets a new worker and retries the request
	// - A "reset" message is injected to tell client to discard partial state
	// - Request completes successfully on the new worker

	t.Run("worker_killed_after_first_byte_retries_with_reset", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Create a slow streaming scenario so we have time to kill the worker
		content := `{"name": "John Doe", "age": 30, "tags": ["developer", "golang", "testing"]}`
		scenarioID := "test-worker-death-retry"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      5,   // Very small chunks
			InitialDelayMs: 100, // Some initial delay
			ChunkDelayMs:   500, // Slow chunking so we have time to kill worker
		}

		if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		// Start streaming request
		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "John, 30 years old"},
			Options: opts,
		})

		var receivedEvents []testutil.StreamEvent
		var streamErr error
		var sawResetEvent bool
		var sawErrorEvent bool
		var killedWorker bool
		var eventsBeforeKill int

		// Process events
		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents = append(receivedEvents, event)
				t.Logf("Received event %d: type=%s, data_len=%d", len(receivedEvents), event.Event, len(event.Data))

				if event.Event == "reset" {
					sawResetEvent = true
					t.Log(">>> SAW RESET EVENT - retry with reset working!")
				}
				if event.Event == "error" {
					sawErrorEvent = true
				}

				// After receiving the first event (first byte), kill the worker
				if len(receivedEvents) == 1 && !killedWorker {
					eventsBeforeKill = len(receivedEvents)
					t.Log("First event received, killing worker...")
					killCtx, killCancel := context.WithTimeout(ctx, 5*time.Second)
					result, err := BAMLClient.KillWorker(killCtx)
					killCancel()

					if err != nil {
						t.Logf("Failed to kill worker: %v", err)
					} else {
						t.Logf("Killed worker %d with %d in-flight requests, gotFirstByte=%v",
							result.WorkerID, result.InFlightCount, result.GotFirstByte)
						killedWorker = true
					}
				}

			case err := <-errs:
				if err != nil {
					streamErr = err
					t.Logf("Received stream error: %v", err)
				}
			case <-ctx.Done():
				t.Logf("Context cancelled")
				streamErr = ctx.Err()
				goto done
			}
		}
	done:

		t.Logf("=== RESULTS ===")
		t.Logf("Total events received: %d", len(receivedEvents))
		t.Logf("Events before kill: %d", eventsBeforeKill)
		t.Logf("Stream error: %v", streamErr)
		t.Logf("Worker killed: %v", killedWorker)
		t.Logf("Saw reset event: %v", sawResetEvent)
		t.Logf("Saw error event: %v", sawErrorEvent)

		for i, event := range receivedEvents {
			t.Logf("Event %d: type=%q, data=%s", i, event.Event, string(event.Data))
		}

		// Verify test setup worked
		if !killedWorker {
			t.Error("Failed to kill worker - test setup issue")
		}

		if eventsBeforeKill == 0 {
			t.Error("Expected to receive at least one event before worker death")
		}

		// VERIFY EXPECTED BEHAVIOR:

		// 1. Should have seen a reset event (indicating retry happened)
		if !sawResetEvent {
			t.Error("Expected reset event after worker death - mid-stream retry should inject reset")
		}

		// 2. Should have completed successfully (no error event, stream closed gracefully)
		if sawErrorEvent {
			t.Error("Did not expect error event - request should succeed after retry")
		}

		if streamErr != nil {
			t.Errorf("Did not expect stream error - got: %v", streamErr)
		}

		// 3. Should have received more events after reset (from the retry)
		// Total should be: events before kill + reset + events from retry
		if len(receivedEvents) <= eventsBeforeKill+1 {
			t.Errorf("Expected more events after reset, got %d total with %d before kill",
				len(receivedEvents), eventsBeforeKill)
		}

		// 4. Verify the last event has correct final data
		// (graceful stream termination means last event is the final result)
		if len(receivedEvents) > 0 {
			// Find the last non-reset data event
			var lastDataEvent testutil.StreamEvent
			for i := len(receivedEvents) - 1; i >= 0; i-- {
				if receivedEvents[i].Event != "reset" && receivedEvents[i].Event != "error" {
					lastDataEvent = receivedEvents[i]
					break
				}
			}
			var person struct {
				Name string   `json:"name"`
				Age  int      `json:"age"`
				Tags []string `json:"tags"`
			}
			if err := json.Unmarshal(lastDataEvent.Data, &person); err != nil {
				t.Fatalf("Failed to unmarshal last event data: %v", err)
			}
			if person.Name != "John Doe" || person.Age != 30 {
				t.Errorf("Final data incorrect: got %+v", person)
			}
			t.Logf("Final data verified: %+v", person)
		}
	})

	t.Run("worker_killed_before_first_byte_retries_without_reset", func(t *testing.T) {
		// This test verifies pre-first-byte retry behavior:
		// - Worker is killed BEFORE any data is sent to client
		// - Pool retries on another worker
		// - NO reset message should be sent (nothing to discard)
		// - Request completes successfully

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Create a scenario with LONG initial delay so we can kill before first byte
		content := `{"name": "Jane Smith", "age": 25, "tags": ["tester"]}`
		scenarioID := "test-worker-death-pre-first-byte"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      10,
			InitialDelayMs: 3000, // 3 second initial delay - plenty of time to kill worker
			ChunkDelayMs:   50,
		}

		if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		// Start streaming request
		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Jane, 25 years old"},
			Options: opts,
		})

		var receivedEvents []testutil.StreamEvent
		var streamErr error
		var sawResetEvent bool
		var sawErrorEvent bool
		var killedWorker bool

		// Kill the worker immediately after starting the request (before first byte)
		// We do this in a goroutine so we don't block receiving events
		go func() {
			// Small delay to ensure the request is in-flight
			time.Sleep(500 * time.Millisecond)

			killCtx, killCancel := context.WithTimeout(ctx, 5*time.Second)
			defer killCancel()

			result, err := BAMLClient.KillWorker(killCtx)
			if err != nil {
				t.Logf("Failed to kill worker: %v", err)
				return
			}
			t.Logf("Killed worker %d with %d in-flight requests, gotFirstByte=%v",
				result.WorkerID, result.InFlightCount, result.GotFirstByte)
			killedWorker = true

			// Verify gotFirstByte is false (we killed before first byte)
			for _, got := range result.GotFirstByte {
				if got {
					t.Logf("WARNING: gotFirstByte was true, timing may be off")
				}
			}
		}()

		// Process events
		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents = append(receivedEvents, event)
				t.Logf("Received event %d: type=%s, data_len=%d", len(receivedEvents), event.Event, len(event.Data))

				if event.Event == "reset" {
					sawResetEvent = true
					t.Log("WARNING: Saw reset event - this should NOT happen for pre-first-byte retry!")
				}
				if event.Event == "error" {
					sawErrorEvent = true
				}

			case err := <-errs:
				if err != nil {
					streamErr = err
					t.Logf("Received stream error: %v", err)
				}
			case <-ctx.Done():
				t.Logf("Context cancelled")
				streamErr = ctx.Err()
				goto done
			}
		}
	done:

		t.Logf("=== RESULTS ===")
		t.Logf("Total events received: %d", len(receivedEvents))
		t.Logf("Stream error: %v", streamErr)
		t.Logf("Worker killed: %v", killedWorker)
		t.Logf("Saw reset event: %v", sawResetEvent)
		t.Logf("Saw error event: %v", sawErrorEvent)

		for i, event := range receivedEvents {
			t.Logf("Event %d: type=%q, data=%s", i, event.Event, string(event.Data))
		}

		// VERIFY EXPECTED BEHAVIOR:

		// 1. Should NOT have seen a reset event (no data was sent before retry)
		if sawResetEvent {
			t.Error("Did not expect reset event - worker was killed before first byte, nothing to reset")
		}

		// 2. Should have completed successfully (no error)
		if sawErrorEvent {
			t.Error("Did not expect error event - request should succeed after retry")
		}

		if streamErr != nil {
			t.Errorf("Did not expect stream error - got: %v", streamErr)
		}

		// 3. Should have received events (from the retry)
		if len(receivedEvents) == 0 {
			t.Error("Expected to receive events from retry")
		}

		// 4. Verify the final data is correct
		if len(receivedEvents) > 0 {
			var lastDataEvent testutil.StreamEvent
			for i := len(receivedEvents) - 1; i >= 0; i-- {
				if receivedEvents[i].Event != "reset" && receivedEvents[i].Event != "error" {
					lastDataEvent = receivedEvents[i]
					break
				}
			}
			var person struct {
				Name string   `json:"name"`
				Age  int      `json:"age"`
				Tags []string `json:"tags"`
			}
			if err := json.Unmarshal(lastDataEvent.Data, &person); err != nil {
				t.Fatalf("Failed to unmarshal last event data: %v", err)
			}
			if person.Name != "Jane Smith" || person.Age != 25 {
				t.Errorf("Final data incorrect: got %+v", person)
			}
			t.Logf("Final data verified: %+v", person)
		}
	})

	t.Run("hung_detection_kills_worker_and_retries", func(t *testing.T) {
		// This test verifies that the automatic hung detection mechanism fires correctly
		// and successfully recovers on retry.
		//
		// Uses a stateful scenario where:
		// - First request: 5 second delay (triggers hung detection)
		// - Retry: 0ms delay (succeeds immediately)
		//
		// What this test verifies:
		// - Hung detection fires when FirstByteTimeout is exceeded
		// - Request is retried after hung detection kills worker
		// - Retry succeeds on the fast second attempt
		// - No reset event (hung detection kills pre-first-byte)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Set a short FirstByteTimeout for this test (2 seconds)
		shortTimeout := int64(2000)
		configResult, err := BAMLClient.SetFirstByteTimeout(ctx, shortTimeout)
		if err != nil {
			t.Fatalf("Failed to set FirstByteTimeout: %v", err)
		}
		t.Logf("Set FirstByteTimeout to %dms (was: inferred from response)", configResult.FirstByteTimeoutMs)

		// Restore default timeout after test
		defer func() {
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer restoreCancel()
			// Restore to a long timeout (120 seconds = default)
			_, _ = BAMLClient.SetFirstByteTimeout(restoreCtx, 120000)
		}()

		// Create a STATEFUL scenario:
		// - Request 0 (first): 5000ms delay (triggers hung detection)
		// - Request 1+ (retries): 0ms delay (succeeds immediately)
		content := `{"name": "Hung Test Recovery", "age": 42, "tags": ["recovered"]}`
		scenarioID := "test-hung-detection-recovery"

		scenario := &mockllm.Scenario{
			ID:        scenarioID,
			Provider:  "openai",
			Content:   content,
			ChunkSize: 20,
			// InitialDelayMsPerRequest makes the scenario stateful:
			// [0]: first request uses 5000ms delay (triggers hung detection)
			// [1]: second request (retry) uses 0ms delay (succeeds)
			InitialDelayMsPerRequest: []int{5000, 0},
			ChunkDelayMs:             50,
		}

		if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		// Start streaming request
		t.Log("Starting streaming request (expecting hung detection to fire, then retry succeeds)...")
		startTime := time.Now()

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Hung test recovery person"},
			Options: opts,
		})

		var receivedEvents []testutil.StreamEvent
		var streamErr error
		var sawResetEvent bool
		var sawErrorEvent bool

		// Process events
		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents = append(receivedEvents, event)
				elapsed := time.Since(startTime)
				t.Logf("Received event %d at %v: type=%s, data_len=%d",
					len(receivedEvents), elapsed.Round(time.Millisecond), event.Event, len(event.Data))

				if event.Event == "reset" {
					sawResetEvent = true
				}
				if event.Event == "error" {
					sawErrorEvent = true
				}

			case err := <-errs:
				if err != nil {
					streamErr = err
					t.Logf("Received stream error: %v", err)
				}
			case <-ctx.Done():
				t.Logf("Context cancelled")
				streamErr = ctx.Err()
				goto done
			}
		}
	done:

		elapsed := time.Since(startTime)
		t.Logf("=== RESULTS ===")
		t.Logf("Total time: %v", elapsed.Round(time.Millisecond))
		t.Logf("Total events received: %d", len(receivedEvents))
		t.Logf("Stream error: %v", streamErr)
		t.Logf("Saw reset event: %v", sawResetEvent)
		t.Logf("Saw error event: %v", sawErrorEvent)

		for i, event := range receivedEvents {
			t.Logf("Event %d: type=%q, data=%s", i, event.Event, string(event.Data))
		}

		// VERIFY EXPECTED BEHAVIOR:

		// 1. Should NOT have seen a reset event (hung detection kills pre-first-byte)
		if sawResetEvent {
			t.Error("Did not expect reset event - hung detection should kill before first byte")
		}

		// 2. Should NOT have error event (retry should succeed)
		if sawErrorEvent {
			t.Error("Did not expect error event - retry should succeed with fast second attempt")
		}

		// 3. Should have completed without stream error
		if streamErr != nil {
			t.Errorf("Did not expect stream error - got: %v", streamErr)
		}

		// 4. Timing should show hung detection fired (at least 2s) but not multiple failures
		// With 2s timeout for first attempt + immediate success on retry, expect ~2-4s total
		expectedMinTime := 2 * time.Second // At least one timeout worth
		expectedMaxTime := 8 * time.Second // Not multiple full timeouts
		if elapsed < expectedMinTime {
			t.Errorf("Request completed too fast (%v) - hung detection may not have fired", elapsed)
		}
		if elapsed > expectedMaxTime {
			t.Errorf("Request took too long (%v) - retry may have also timed out", elapsed)
		}
		t.Logf("Timing indicates hung detection fired and retry succeeded (elapsed: %v)", elapsed)

		// 5. Should have received data events (from successful retry)
		if len(receivedEvents) == 0 {
			t.Error("Expected to receive events from successful retry")
		}

		// 6. Verify the final data is correct
		if len(receivedEvents) > 0 {
			var lastDataEvent testutil.StreamEvent
			for i := len(receivedEvents) - 1; i >= 0; i-- {
				if receivedEvents[i].Event != "reset" && receivedEvents[i].Event != "error" {
					lastDataEvent = receivedEvents[i]
					break
				}
			}
			var person struct {
				Name string   `json:"name"`
				Age  int      `json:"age"`
				Tags []string `json:"tags"`
			}
			if err := json.Unmarshal(lastDataEvent.Data, &person); err != nil {
				t.Fatalf("Failed to unmarshal last event data: %v", err)
			}
			if person.Name != "Hung Test Recovery" || person.Age != 42 {
				t.Errorf("Final data incorrect: got %+v", person)
			}
			t.Logf("Final data verified: %+v", person)
		}
	})
}

func TestStreamNDJSONEndpoint(t *testing.T) {
	t.Run("multiple_ndjson_events", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"name": "John Doe", "age": 30, "tags": []}`
		opts := setupScenario(t, "test-stream-ndjson-person", content)

		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "John, 30 years old"},
			Options: opts,
		})

		var eventCount int
		var lastEvent testutil.StreamEvent

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++
				lastEvent = event
				t.Logf("Event %d: type=%s, data_len=%d", eventCount, event.Event, len(event.Data))
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		// Should have received multiple events (chunked streaming)
		if eventCount < 2 {
			t.Errorf("Expected multiple events, got %d", eventCount)
		}

		// All events should be data events
		if !lastEvent.IsData() {
			t.Errorf("Expected data event, got type=%s", lastEvent.Event)
		}

		// Last event should have the complete data
		var person struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}
		if err := json.Unmarshal(lastEvent.Data, &person); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if person.Name != "John Doe" || person.Age != 30 {
			t.Errorf("Expected John Doe (30), got %s (%d)", person.Name, person.Age)
		}
	})

	t.Run("streaming_array_ndjson", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `[{"name": "Alice", "age": 25, "tags": []}, {"name": "Bob", "age": 35, "tags": []}]`
		opts := setupScenario(t, "test-stream-ndjson-array", content)

		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method:  "GetPeople",
			Input:   map[string]any{"descriptions": "Alice and Bob"},
			Options: opts,
		})

		var eventCount int
		var lastEvent testutil.StreamEvent

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++
				lastEvent = event
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		// Verify final result is complete array
		var people []struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}
		if err := json.Unmarshal(lastEvent.Data, &people); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if len(people) != 2 {
			t.Errorf("Expected 2 people, got %d", len(people))
		}
	})

}

func TestStreamAcceptHeaderNegotiation(t *testing.T) {
	// Helper to make a raw HTTP request and return the Content-Type
	makeRequest := func(t *testing.T, ctx context.Context, acceptHeader string, opts *testutil.BAMLOptions) string {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"input":            "test",
			"__baml_options__": opts,
		})
		if err != nil {
			t.Fatalf("Failed to marshal request: %v", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST",
			TestEnv.BAMLRestURL+"/stream/GetSimple",
			bytes.NewReader(body))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if acceptHeader != "" {
			req.Header.Set("Accept", acceptHeader)
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()

		return resp.Header.Get("Content-Type")
	}

	t.Run("no_accept_header_defaults_to_sse", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello"}`
		opts := setupScenario(t, "test-accept-no-header", content)

		contentType := makeRequest(t, ctx, "", opts)
		if contentType != "text/event-stream" {
			t.Errorf("Expected Content-Type 'text/event-stream' with no Accept header, got '%s'", contentType)
		}
	})

	t.Run("accept_text_event_stream_returns_sse", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello"}`
		opts := setupScenario(t, "test-accept-sse", content)

		contentType := makeRequest(t, ctx, "text/event-stream", opts)
		if contentType != "text/event-stream" {
			t.Errorf("Expected Content-Type 'text/event-stream' with Accept: text/event-stream, got '%s'", contentType)
		}
	})

	t.Run("accept_ndjson_returns_ndjson", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello"}`
		opts := setupScenario(t, "test-accept-ndjson", content)

		contentType := makeRequest(t, ctx, "application/x-ndjson", opts)
		if contentType != "application/x-ndjson" {
			t.Errorf("Expected Content-Type 'application/x-ndjson' with Accept: application/x-ndjson, got '%s'", contentType)
		}
	})

	t.Run("accept_with_quality_values_prefers_ndjson", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello"}`
		opts := setupScenario(t, "test-accept-quality", content)

		// Even with quality values, if NDJSON is listed, it should be used
		contentType := makeRequest(t, ctx, "application/x-ndjson, text/event-stream;q=0.9", opts)
		if contentType != "application/x-ndjson" {
			t.Errorf("Expected Content-Type 'application/x-ndjson' with quality Accept header, got '%s'", contentType)
		}
	})

	t.Run("accept_unknown_type_defaults_to_sse", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello"}`
		opts := setupScenario(t, "test-accept-unknown", content)

		contentType := makeRequest(t, ctx, "application/json", opts)
		if contentType != "text/event-stream" {
			t.Errorf("Expected Content-Type 'text/event-stream' with unknown Accept header, got '%s'", contentType)
		}
	})

	t.Run("accept_wildcard_defaults_to_sse", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello"}`
		opts := setupScenario(t, "test-accept-wildcard", content)

		contentType := makeRequest(t, ctx, "*/*", opts)
		if contentType != "text/event-stream" {
			t.Errorf("Expected Content-Type 'text/event-stream' with wildcard Accept header, got '%s'", contentType)
		}
	})

	t.Run("stream_with_raw_accept_ndjson", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello"}`
		opts := setupScenario(t, "test-accept-raw-ndjson", content)

		// Test stream-with-raw endpoint with NDJSON
		body, err := json.Marshal(map[string]any{
			"input":            "test",
			"__baml_options__": opts,
		})
		if err != nil {
			t.Fatalf("Failed to marshal request: %v", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST",
			TestEnv.BAMLRestURL+"/stream-with-raw/GetSimple",
			bytes.NewReader(body))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/x-ndjson")

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()

		contentType := resp.Header.Get("Content-Type")
		if contentType != "application/x-ndjson" {
			t.Errorf("Expected Content-Type 'application/x-ndjson' for stream-with-raw, got '%s'", contentType)
		}
	})

	t.Run("stream_with_raw_no_accept_defaults_to_sse", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello"}`
		opts := setupScenario(t, "test-accept-raw-default", content)

		// Test stream-with-raw endpoint without Accept header
		body, err := json.Marshal(map[string]any{
			"input":            "test",
			"__baml_options__": opts,
		})
		if err != nil {
			t.Fatalf("Failed to marshal request: %v", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST",
			TestEnv.BAMLRestURL+"/stream-with-raw/GetSimple",
			bytes.NewReader(body))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()

		contentType := resp.Header.Get("Content-Type")
		if contentType != "text/event-stream" {
			t.Errorf("Expected Content-Type 'text/event-stream' for stream-with-raw without Accept, got '%s'", contentType)
		}
	})
}

func TestStreamWithRawNDJSONEndpoint(t *testing.T) {
	t.Run("raw_accumulates_ndjson", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello world"}`
		opts := setupScenario(t, "test-stream-raw-ndjson", content)

		events, errs := BAMLClient.StreamWithRawNDJSON(ctx, testutil.CallRequest{
			Method:  "GetSimple",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})

		var eventCount int
		var lastRaw string

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++
				if event.Raw != "" {
					lastRaw = event.Raw
				}
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		// Should have received events
		if eventCount == 0 {
			t.Error("Expected to receive events")
		}

		// Final raw should match the complete content
		if lastRaw != content {
			t.Errorf("Expected final raw '%s', got '%s'", content, lastRaw)
		}
	})

	t.Run("raw_grows_incrementally_ndjson", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello world"}`
		scenarioID := "test-stream-raw-incremental-ndjson"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      5, // Small chunks to see incremental growth
			InitialDelayMs: 10,
			ChunkDelayMs:   50,
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

		// Track raw lengths per segment (segments are separated by reset events)
		var segments [][]int
		var currentSegment []int
		resetCount := 0

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				if event.IsReset() {
					// Reset event: save current segment and start new one
					if len(currentSegment) > 0 {
						segments = append(segments, currentSegment)
						currentSegment = nil
					}
					resetCount++
				} else if event.Raw != "" {
					currentSegment = append(currentSegment, len(event.Raw))
				}
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		// Save final segment
		if len(currentSegment) > 0 {
			segments = append(segments, currentSegment)
		}

		// Should have at least one segment with multiple raw values
		if len(segments) == 0 {
			t.Fatal("Expected at least one segment with raw values")
		}

		totalRawValues := 0
		for _, seg := range segments {
			totalRawValues += len(seg)
		}
		if totalRawValues < 2 {
			t.Errorf("Expected multiple raw values across all segments, got %d", totalRawValues)
		}

		// Raw lengths should be non-decreasing within each segment
		for segIdx, segment := range segments {
			for i := 1; i < len(segment); i++ {
				if segment[i] < segment[i-1] {
					t.Errorf("Segment %d: Raw length decreased from %d to %d at index %d",
						segIdx, segment[i-1], segment[i], i)
				}
			}
		}

		if resetCount > 0 {
			t.Logf("Stream had %d reset(s) (BAML internal retries)", resetCount)
		}
		t.Logf("Segments: %d, Raw lengths per segment: %v", len(segments), segments)
	})
}

func TestStreamNDJSONMidStreamFailure(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	t.Run("disconnect_after_first_byte_ndjson", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Create a scenario that will disconnect after 3 chunks
		content := `{"name": "John Doe", "age": 30, "tags": ["developer", "golang"]}`
		scenarioID := "test-ndjson-midstream-disconnect"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      10, // Small chunks so we get multiple before failure
			InitialDelayMs: 10,
			ChunkDelayMs:   50,
			FailAfter:      3, // Fail after 3 chunks
			FailureMode:    "disconnect",
		}

		if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "John, 30 years old"},
			Options: opts,
		})

		var receivedEvents []testutil.StreamEvent
		var streamErr error
		var sawResetEvent bool
		var sawErrorEvent bool

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents = append(receivedEvents, event)
				t.Logf("Received event %d: type=%s, data_len=%d", len(receivedEvents), event.Event, len(event.Data))
				if event.IsReset() {
					sawResetEvent = true
				}
				if event.IsError() {
					sawErrorEvent = true
				}
			case err := <-errs:
				if err != nil {
					streamErr = err
					t.Logf("Received stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		t.Logf("Total events received: %d", len(receivedEvents))
		t.Logf("Stream error: %v", streamErr)
		t.Logf("Saw reset event: %v", sawResetEvent)
		t.Logf("Saw error event: %v", sawErrorEvent)

		// Should have received some events or an error
		if len(receivedEvents) == 0 && streamErr == nil {
			t.Error("Expected to receive some events or an error, got neither")
		}

		// Document: mid-stream failures don't trigger pool-level retries
		if sawResetEvent {
			t.Log("NOTE: Saw reset event - this indicates a BAML-internal retry occurred")
		}
	})
}

func TestWorkerDeathMidStreamNDJSON(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	t.Run("worker_killed_after_first_byte_retries_with_reset_ndjson", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Create a slow streaming scenario so we have time to kill the worker
		content := `{"name": "John Doe", "age": 30, "tags": ["developer", "golang", "testing"]}`
		scenarioID := "test-worker-death-retry-ndjson"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      5,   // Very small chunks
			InitialDelayMs: 100, // Some initial delay
			ChunkDelayMs:   500, // Slow chunking so we have time to kill worker
		}

		if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		// Start streaming request with NDJSON
		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "John, 30 years old"},
			Options: opts,
		})

		var receivedEvents []testutil.StreamEvent
		var streamErr error
		var sawResetEvent bool
		var sawErrorEvent bool
		var killedWorker bool
		var eventsBeforeKill int

		// Process events
		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents = append(receivedEvents, event)
				t.Logf("Received event %d: type=%s, data_len=%d", len(receivedEvents), event.Event, len(event.Data))

				if event.IsReset() {
					sawResetEvent = true
					t.Log(">>> SAW RESET EVENT - retry with reset working in NDJSON!")
				}
				if event.IsError() {
					sawErrorEvent = true
				}

				// After receiving the first event (first byte), kill the worker
				if len(receivedEvents) == 1 && !killedWorker {
					eventsBeforeKill = len(receivedEvents)
					t.Log("First event received, killing worker...")
					killCtx, killCancel := context.WithTimeout(ctx, 5*time.Second)
					result, err := BAMLClient.KillWorker(killCtx)
					killCancel()

					if err != nil {
						t.Logf("Failed to kill worker: %v", err)
					} else {
						t.Logf("Killed worker %d with %d in-flight requests, gotFirstByte=%v",
							result.WorkerID, result.InFlightCount, result.GotFirstByte)
						killedWorker = true
					}
				}

			case err := <-errs:
				if err != nil {
					streamErr = err
					t.Logf("Received stream error: %v", err)
				}
			case <-ctx.Done():
				t.Logf("Context cancelled")
				streamErr = ctx.Err()
				goto done
			}
		}
	done:

		t.Logf("=== RESULTS ===")
		t.Logf("Total events received: %d", len(receivedEvents))
		t.Logf("Events before kill: %d", eventsBeforeKill)
		t.Logf("Stream error: %v", streamErr)
		t.Logf("Worker killed: %v", killedWorker)
		t.Logf("Saw reset event: %v", sawResetEvent)
		t.Logf("Saw error event: %v", sawErrorEvent)

		// Verify test setup worked
		if !killedWorker {
			t.Error("Failed to kill worker - test setup issue")
		}

		if eventsBeforeKill == 0 {
			t.Error("Expected to receive at least one event before worker death")
		}

		// VERIFY EXPECTED BEHAVIOR:

		// 1. Should have seen a reset event (indicating retry happened)
		if !sawResetEvent {
			t.Error("Expected reset event after worker death - mid-stream retry should inject reset")
		}

		// 2. Should have completed successfully (no error event, stream closed gracefully)
		if sawErrorEvent {
			t.Error("Did not expect error event - request should succeed after retry")
		}

		if streamErr != nil {
			t.Errorf("Did not expect stream error - got: %v", streamErr)
		}

		// 3. Should have received more events after reset (from the retry)
		if len(receivedEvents) <= eventsBeforeKill+1 {
			t.Errorf("Expected more events after reset, got %d total with %d before kill",
				len(receivedEvents), eventsBeforeKill)
		}

		// 4. Verify the last data event has correct final data
		if len(receivedEvents) > 0 {
			var lastDataEvent testutil.StreamEvent
			for i := len(receivedEvents) - 1; i >= 0; i-- {
				if receivedEvents[i].IsData() {
					lastDataEvent = receivedEvents[i]
					break
				}
			}
			var person struct {
				Name string   `json:"name"`
				Age  int      `json:"age"`
				Tags []string `json:"tags"`
			}
			if err := json.Unmarshal(lastDataEvent.Data, &person); err != nil {
				t.Fatalf("Failed to unmarshal last event data: %v", err)
			}
			if person.Name != "John Doe" || person.Age != 30 {
				t.Errorf("Final data incorrect: got %+v", person)
			}
			t.Logf("Final data verified: %+v", person)
		}
	})
}

func TestStreamWithRawEndpoint(t *testing.T) {
	t.Run("raw_accumulates", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		content := `{"message": "hello world"}`
		opts := setupScenario(t, "test-stream-raw", content)

		events, errs := BAMLClient.StreamWithRaw(ctx, testutil.CallRequest{
			Method:  "GetSimple",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})

		var eventCount int
		var lastRaw string

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++
				if event.Raw != "" {
					lastRaw = event.Raw
				}
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		// Final raw should match the complete content
		if lastRaw != content {
			t.Errorf("Expected final raw '%s', got '%s'", content, lastRaw)
		}
	})
}

func TestRequestCancellation(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	// This test verifies that cancelling a request properly cleans up resources
	// in the baml-rest SERVER (not the test client) and doesn't leave the system
	// in a bad state.
	//
	// We fetch pprof data from baml-rest via /_debug/goroutines endpoint to detect
	// leaks in the actual server process AND worker processes, filtering for
	// goroutines in baml-rest and baml (boundaryml) code.

	t.Run("cancel_mid_stream_cleans_up", func(t *testing.T) {
		parentCtx, parentCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer parentCancel()

		// Let things settle and capture baseline from the SERVER
		time.Sleep(100 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(parentCtx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Server baseline: %d total goroutines, %d matching filter (main: %d, workers: %d)",
			baselineResult.TotalCount, baselineMatches, baselineResult.MatchCount, baselineMatches-baselineResult.MatchCount)

		// Create a slow streaming scenario
		content := `{"name": "Cancel Test", "age": 99, "tags": ["should", "be", "cancelled"]}`
		scenarioID := "test-cancel-midstream"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      5,   // Small chunks
			InitialDelayMs: 100, // Some initial delay
			ChunkDelayMs:   500, // Slow chunking so we have time to cancel
		}

		if err := MockClient.RegisterScenario(parentCtx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		// Create a cancellable context for the request
		reqCtx, reqCancel := context.WithCancel(parentCtx)
		defer reqCancel()

		// Start streaming request
		t.Log("Starting streaming request...")
		events, errs := BAMLClient.Stream(reqCtx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Cancel test person"},
			Options: opts,
		})

		var receivedEvents int
		var cancelledAt time.Time

		// Process events until we receive at least one, then cancel
		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents++
				t.Logf("Received event %d: type=%s, data_len=%d", receivedEvents, event.Event, len(event.Data))

				// After receiving the first event, cancel the request
				if receivedEvents == 1 {
					t.Log("Cancelling request after first event...")
					cancelledAt = time.Now()
					reqCancel()
				}

			case err := <-errs:
				if err != nil {
					t.Logf("Received error (expected after cancel): %v", err)
				}
			case <-parentCtx.Done():
				t.Fatal("Parent context cancelled unexpectedly")
			}
		}
	done:

		cancelDuration := time.Since(cancelledAt)
		t.Logf("Stream closed %v after cancellation", cancelDuration)
		t.Logf("Total events received: %d", receivedEvents)

		// Verify cancellation was prompt (should be much faster than waiting for all chunks)
		if cancelDuration > 2*time.Second {
			t.Errorf("Cancellation took too long: %v (expected < 2s)", cancelDuration)
		}

		// Check for goroutine leaks
		finalResult, cleanedUp := waitForGoroutineCleanup(parentCtx, t, baselineMatches, 5*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Errorf("Goroutine leak detected: %d new goroutines in baml-rest/baml code", finalMatches-baselineMatches)
			logLeakedStacks(t, finalResult)
		}

		// CRITICAL: Verify subsequent requests still work (pool not corrupted)
		t.Log("Verifying subsequent request works...")
		verifyCtx, verifyCancel := context.WithTimeout(parentCtx, 10*time.Second)
		defer verifyCancel()

		// Use a fast scenario for verification
		verifyContent := `{"name": "Verify OK", "age": 1, "tags": []}`
		verifyScenarioID := "test-cancel-verify"
		verifyScenario := &mockllm.Scenario{
			ID:             verifyScenarioID,
			Provider:       "openai",
			Content:        verifyContent,
			ChunkSize:      100, // Large chunks for fast completion
			InitialDelayMs: 0,
			ChunkDelayMs:   0,
		}

		if err := MockClient.RegisterScenario(verifyCtx, verifyScenario); err != nil {
			t.Fatalf("Failed to register verify scenario: %v", err)
		}

		verifyOpts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, verifyScenarioID),
		}

		verifyEvents, verifyErrs := BAMLClient.Stream(verifyCtx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Verify request works"},
			Options: verifyOpts,
		})

		var verifyEventCount int
		var verifyErr error

		for {
			select {
			case event, ok := <-verifyEvents:
				if !ok {
					goto verifyDone
				}
				verifyEventCount++
				t.Logf("Verify event %d: type=%s", verifyEventCount, event.Event)
			case err := <-verifyErrs:
				if err != nil {
					verifyErr = err
				}
			case <-verifyCtx.Done():
				t.Fatal("Verify context cancelled")
			}
		}
	verifyDone:

		if verifyErr != nil {
			t.Errorf("Subsequent request failed: %v", verifyErr)
		}
		if verifyEventCount == 0 {
			t.Error("Subsequent request received no events")
		}
		t.Logf("Subsequent request succeeded with %d events", verifyEventCount)
	})

	t.Run("cancel_before_first_byte_cleans_up", func(t *testing.T) {
		// This tests cancellation during the "waiting for first byte" phase

		parentCtx, parentCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer parentCancel()

		// Capture baseline from the SERVER
		time.Sleep(100 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(parentCtx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Server baseline: %d total goroutines, %d matching filter (main: %d, workers: %d)",
			baselineResult.TotalCount, baselineMatches, baselineResult.MatchCount, baselineMatches-baselineResult.MatchCount)

		// Create a scenario with long initial delay
		content := `{"name": "Pre-Cancel", "age": 0, "tags": []}`
		scenarioID := "test-cancel-pre-first-byte"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      100,
			InitialDelayMs: 10000, // 10 second delay - we'll cancel before this
			ChunkDelayMs:   0,
		}

		if err := MockClient.RegisterScenario(parentCtx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		// Create a cancellable context
		reqCtx, reqCancel := context.WithCancel(parentCtx)

		// Start streaming request
		t.Log("Starting streaming request with long initial delay...")
		startTime := time.Now()
		events, errs := BAMLClient.Stream(reqCtx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Pre-cancel test"},
			Options: opts,
		})

		// Cancel after a short delay (before first byte would arrive)
		go func() {
			time.Sleep(500 * time.Millisecond)
			t.Log("Cancelling request before first byte...")
			reqCancel()
		}()

		var receivedEvents int
		var streamErr error

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents++
				t.Logf("Received event: type=%s", event.Event)
			case err := <-errs:
				if err != nil {
					streamErr = err
					t.Logf("Received error: %v", err)
				}
			case <-parentCtx.Done():
				t.Fatal("Parent context cancelled unexpectedly")
			}
		}
	done:

		elapsed := time.Since(startTime)
		t.Logf("Request completed in %v", elapsed)
		t.Logf("Events received: %d", receivedEvents)
		t.Logf("Stream error: %v", streamErr)

		// Should complete quickly (cancelled at ~500ms, not waiting 10s)
		if elapsed > 3*time.Second {
			t.Errorf("Cancellation took too long: %v (expected < 3s)", elapsed)
		}

		// Check for goroutine leaks
		finalResult, cleanedUp := waitForGoroutineCleanup(parentCtx, t, baselineMatches, 5*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Errorf("Goroutine leak detected: %d new goroutines in baml-rest/baml code", finalMatches-baselineMatches)
			logLeakedStacks(t, finalResult)
		}
	})
}

func TestRequestCancellationNDJSON(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	t.Run("cancel_mid_stream_cleans_up_ndjson", func(t *testing.T) {
		parentCtx, parentCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer parentCancel()

		// Let things settle and capture baseline from the SERVER
		time.Sleep(100 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(parentCtx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Server baseline: %d total goroutines, %d matching filter (main: %d, workers: %d)",
			baselineResult.TotalCount, baselineMatches, baselineResult.MatchCount, baselineMatches-baselineResult.MatchCount)

		// Create a slow streaming scenario
		content := `{"name": "Cancel NDJSON Test", "age": 99, "tags": ["should", "be", "cancelled"]}`
		scenarioID := "test-cancel-midstream-ndjson"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      5,
			InitialDelayMs: 100,
			ChunkDelayMs:   500,
		}

		if err := MockClient.RegisterScenario(parentCtx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		// Create a cancellable context for the request
		reqCtx, reqCancel := context.WithCancel(parentCtx)
		defer reqCancel()

		// Start streaming request
		t.Log("Starting NDJSON streaming request...")
		events, errs := BAMLClient.StreamNDJSON(reqCtx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Cancel NDJSON test person"},
			Options: opts,
		})

		var receivedEvents int
		var cancelledAt time.Time

		// Process events until we receive at least one, then cancel
		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents++
				t.Logf("Received NDJSON event %d: type=%s, data_len=%d", receivedEvents, event.Event, len(event.Data))

				// After receiving the first event, cancel the request
				if receivedEvents == 1 {
					t.Log("Cancelling NDJSON request after first event...")
					cancelledAt = time.Now()
					reqCancel()
				}

			case err := <-errs:
				if err != nil {
					t.Logf("Received NDJSON error (expected after cancel): %v", err)
				}
			case <-parentCtx.Done():
				t.Fatal("Parent context cancelled unexpectedly")
			}
		}
	done:
		if cancelledAt.IsZero() {
			t.Fatalf("Did not reach cancellation point before stream ended (received_events=%d)", receivedEvents)
		}

		cancelDuration := time.Since(cancelledAt)
		t.Logf("NDJSON stream closed %v after cancellation", cancelDuration)
		t.Logf("Total NDJSON events received: %d", receivedEvents)

		// Verify cancellation was prompt (should be much faster than waiting for all chunks)
		if cancelDuration > 2*time.Second {
			t.Errorf("Cancellation took too long: %v (expected < 2s)", cancelDuration)
		}

		// Check for goroutine leaks
		finalResult, cleanedUp := waitForGoroutineCleanup(parentCtx, t, baselineMatches, 5*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Errorf("Goroutine leak detected: %d new goroutines in baml-rest/baml code", finalMatches-baselineMatches)
			logLeakedStacks(t, finalResult)
		}

		// CRITICAL: Verify subsequent requests still work (pool not corrupted)
		t.Log("Verifying subsequent NDJSON request works...")
		verifyCtx, verifyCancel := context.WithTimeout(parentCtx, 10*time.Second)
		defer verifyCancel()

		verifyContent := `{"name": "Verify NDJSON OK", "age": 1, "tags": []}`
		verifyScenarioID := "test-cancel-verify-ndjson"
		verifyScenario := &mockllm.Scenario{
			ID:             verifyScenarioID,
			Provider:       "openai",
			Content:        verifyContent,
			ChunkSize:      100,
			InitialDelayMs: 0,
			ChunkDelayMs:   0,
		}

		if err := MockClient.RegisterScenario(verifyCtx, verifyScenario); err != nil {
			t.Fatalf("Failed to register verify scenario: %v", err)
		}

		verifyOpts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, verifyScenarioID),
		}

		verifyEvents, verifyErrs := BAMLClient.StreamNDJSON(verifyCtx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Verify NDJSON request works"},
			Options: verifyOpts,
		})

		var verifyEventCount int
		var verifyErr error

		for {
			select {
			case event, ok := <-verifyEvents:
				if !ok {
					goto verifyDone
				}
				verifyEventCount++
				t.Logf("Verify NDJSON event %d: type=%s", verifyEventCount, event.Event)
			case err := <-verifyErrs:
				if err != nil {
					verifyErr = err
				}
			case <-verifyCtx.Done():
				t.Fatal("Verify context cancelled")
			}
		}
	verifyDone:

		if verifyErr != nil {
			t.Errorf("Subsequent NDJSON request failed: %v", verifyErr)
		}
		if verifyEventCount == 0 {
			t.Error("Subsequent NDJSON request received no events")
		}
		t.Logf("Subsequent NDJSON request succeeded with %d events", verifyEventCount)
	})

	t.Run("cancel_before_first_byte_cleans_up_ndjson", func(t *testing.T) {
		parentCtx, parentCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer parentCancel()

		// Capture baseline from the SERVER
		time.Sleep(100 * time.Millisecond)
		baselineResult, err := BAMLClient.GetGoroutines(parentCtx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)
		t.Logf("Server baseline: %d total goroutines, %d matching filter (main: %d, workers: %d)",
			baselineResult.TotalCount, baselineMatches, baselineResult.MatchCount, baselineMatches-baselineResult.MatchCount)

		// Create a scenario with long initial delay
		content := `{"name": "Pre-Cancel NDJSON", "age": 0, "tags": []}`
		scenarioID := "test-cancel-pre-first-byte-ndjson"

		scenario := &mockllm.Scenario{
			ID:             scenarioID,
			Provider:       "openai",
			Content:        content,
			ChunkSize:      100,
			InitialDelayMs: 10000, // 10 second delay - we'll cancel before this
			ChunkDelayMs:   0,
		}

		if err := MockClient.RegisterScenario(parentCtx, scenario); err != nil {
			t.Fatalf("Failed to register scenario: %v", err)
		}

		opts := &testutil.BAMLOptions{
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
		}

		// Create a cancellable context
		reqCtx, reqCancel := context.WithCancel(parentCtx)

		// Start streaming request
		t.Log("Starting NDJSON streaming request with long initial delay...")
		startTime := time.Now()
		events, errs := BAMLClient.StreamNDJSON(reqCtx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "Pre-cancel NDJSON test"},
			Options: opts,
		})

		// Cancel after a short delay (before first byte would arrive)
		go func() {
			time.Sleep(500 * time.Millisecond)
			t.Log("Cancelling NDJSON request before first byte...")
			reqCancel()
		}()

		var receivedEvents int
		var streamErr error

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				receivedEvents++
				t.Logf("Received NDJSON event: type=%s", event.Event)
			case err := <-errs:
				if err != nil {
					streamErr = err
					t.Logf("Received NDJSON error: %v", err)
				}
			case <-parentCtx.Done():
				t.Fatal("Parent context cancelled unexpectedly")
			}
		}
	done:

		elapsed := time.Since(startTime)
		t.Logf("NDJSON request completed in %v", elapsed)
		t.Logf("NDJSON events received: %d", receivedEvents)
		t.Logf("NDJSON stream error: %v", streamErr)

		if receivedEvents != 0 {
			t.Errorf("Expected zero NDJSON events before cancellation, got %d", receivedEvents)
		}

		// Should complete quickly (cancelled at ~500ms, not waiting 10s)
		if elapsed > 3*time.Second {
			t.Errorf("Cancellation took too long: %v (expected < 3s)", elapsed)
		}

		// Check for goroutine leaks
		finalResult, cleanedUp := waitForGoroutineCleanup(parentCtx, t, baselineMatches, 5*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Errorf("Goroutine leak detected: %d new goroutines in baml-rest/baml code", finalMatches-baselineMatches)
			logLeakedStacks(t, finalResult)
		}
	})
}
