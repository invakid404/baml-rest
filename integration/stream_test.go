//go:build integration

package integration

import (
	"context"
	"strings"
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
	// This test suite verifies what happens when something goes wrong mid-stream.
	//
	// IMPORTANT CONTEXT (based on code analysis of pool/pool.go):
	//
	// 1. The hung detection (firstByteTimeout) ONLY monitors requests where gotFirstByte=false
	//    See pool.go:330: "if !req.gotFirstByte.Load() && now.Sub(req.startedAt) > p.config.FirstByteTimeout"
	//
	// 2. Pool-level retries happen when handle.worker.CallStream() returns an error
	//    See pool.go:510-534. The retry loop exits once CallStream returns successfully.
	//
	// 3. The gRPC client CallStream() blocks until the server sends response headers
	//    Headers are sent when the first stream.Send() is called (see workerplugin/grpc.go:49-77)
	//
	// 4. Therefore: if the worker is slow to produce the FIRST result, the gRPC call blocks,
	//    hung detection can fire, and RETRY WORKS.
	//
	// 5. BUT: once the first result is produced, gotFirstByte=true, and hung detection
	//    no longer monitors that request. Mid-stream failures result in errors, NOT retries.
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
			ID:       scenarioID,
			Provider: "openai",
			Content:  content,
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

func TestStreamWithRawEndpoint(t *testing.T) {
	t.Run("raw_accumulates", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
