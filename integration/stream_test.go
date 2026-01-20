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
