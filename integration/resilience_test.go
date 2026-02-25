//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestConcurrentCallsDuringWorkerDeath fires many concurrent Call requests
// and kills a worker mid-flight. All requests should complete successfully
// thanks to the pool's hot-swap restart and retry logic.
func TestConcurrentCallsDuringWorkerDeath(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const concurrency = 20
	content := `{"name": "ConcurrentTest", "age": 42, "tags": ["resilience"]}`
	scenarioID := "resilience-concurrent-calls"

	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        content,
		ChunkSize:      10,
		InitialDelayMs: 200, // enough time for requests to be in-flight
		ChunkDelayMs:   50,
	}
	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	opts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
	}

	var wg sync.WaitGroup
	var failures atomic.Int32

	// Fire concurrent requests.
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": fmt.Sprintf("person_%d", idx)},
				Options: opts,
			})
			if err != nil {
				t.Logf("Request %d transport error: %v", idx, err)
				failures.Add(1)
				return
			}
			if resp.StatusCode != 200 {
				t.Logf("Request %d status %d: %s", idx, resp.StatusCode, resp.Error)
				failures.Add(1)
				return
			}

			var person struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(resp.Body, &person); err != nil {
				t.Logf("Request %d unmarshal error: %v", idx, err)
				failures.Add(1)
			}
		}(i)
	}

	// Wait for some requests to be in-flight, then kill a worker.
	time.Sleep(300 * time.Millisecond)
	killCtx, killCancel := context.WithTimeout(ctx, 5*time.Second)
	result, err := BAMLClient.KillWorker(killCtx)
	killCancel()
	if err != nil {
		t.Logf("KillWorker failed (may be ok if no in-flight): %v", err)
	} else {
		t.Logf("Killed worker %d (%d in-flight)", result.WorkerID, result.InFlightCount)
	}

	wg.Wait()

	if f := failures.Load(); f > 0 {
		t.Errorf("%d/%d concurrent call requests failed during worker death", f, concurrency)
	}
}

// TestConcurrentStreamsDuringWorkerDeath fires many concurrent Stream
// requests and kills a worker mid-flight. All requests should complete
// (possibly with a reset event mid-stream).
func TestConcurrentStreamsDuringWorkerDeath(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const concurrency = 15
	content := `{"name": "StreamResilience", "age": 99, "tags": ["stream", "test"]}`
	scenarioID := "resilience-concurrent-streams"

	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        content,
		ChunkSize:      5,
		InitialDelayMs: 200,
		ChunkDelayMs:   100, // slow enough for kill to hit mid-stream
	}
	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	opts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
	}

	var wg sync.WaitGroup
	var failures atomic.Int32
	var resets atomic.Int32

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": fmt.Sprintf("person_%d", idx)},
				Options: opts,
			})

			var gotFinal bool
			var gotError bool
			for {
				select {
				case event, ok := <-events:
					if !ok {
						goto done
					}
					if event.IsReset() {
						resets.Add(1)
					}
					if event.IsFinal() {
						gotFinal = true
					}
					if event.IsError() {
						gotError = true
						t.Logf("Request %d got error event: %s", idx, event.Data)
					}
				case err := <-errs:
					if err != nil {
						t.Logf("Request %d stream error: %v", idx, err)
						gotError = true
					}
				case <-ctx.Done():
					goto done
				}
			}
		done:

			if !gotFinal || gotError {
				t.Logf("Request %d: final=%v error=%v", idx, gotFinal, gotError)
				failures.Add(1)
			}
		}(i)
	}

	// Wait for streams to start, then kill a worker.
	time.Sleep(400 * time.Millisecond)
	killCtx, killCancel := context.WithTimeout(ctx, 5*time.Second)
	result, err := BAMLClient.KillWorker(killCtx)
	killCancel()
	if err != nil {
		t.Logf("KillWorker failed: %v", err)
	} else {
		t.Logf("Killed worker %d (%d in-flight)", result.WorkerID, result.InFlightCount)
	}

	wg.Wait()

	t.Logf("Resets observed: %d", resets.Load())
	if f := failures.Load(); f > 0 {
		t.Errorf("%d/%d concurrent stream requests failed during worker death", f, concurrency)
	}
}

// TestSequentialWorkerDeaths kills a worker, waits for recovery, fires
// requests to verify it works, then kills again. Verifies the pool can
// recover from repeated failures.
func TestSequentialWorkerDeaths(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const rounds = 3
	content := `{"name": "Sequential", "age": 1, "tags": ["recovery"]}`
	scenarioID := "resilience-sequential-deaths"

	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        content,
		ChunkSize:      10,
		InitialDelayMs: 200,
		ChunkDelayMs:   100,
	}
	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	opts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
	}

	for round := 1; round <= rounds; round++ {
		t.Logf("=== Round %d/%d ===", round, rounds)

		// Start a stream request (creates in-flight work).
		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": fmt.Sprintf("round_%d", round)},
			Options: opts,
		})

		// Wait for first event.
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("Round %d: stream closed before first event", round)
			}
			t.Logf("Round %d: got first event: type=%s", round, event.Event)
		case err := <-errs:
			t.Fatalf("Round %d: stream error before first event: %v", round, err)
		case <-time.After(30 * time.Second):
			t.Fatalf("Round %d: timeout waiting for first event", round)
		}

		// Kill a worker.
		killCtx, killCancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := BAMLClient.KillWorker(killCtx)
		killCancel()
		if err != nil {
			t.Logf("Round %d: KillWorker failed: %v", round, err)
		} else {
			t.Logf("Round %d: killed worker %d", round, result.WorkerID)
		}

		// Drain remaining events — expect recovery (reset + final).
		var gotFinal bool
		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto roundDone
				}
				if event.IsFinal() {
					gotFinal = true
				}
			case err := <-errs:
				if err != nil {
					t.Logf("Round %d: stream error during drain: %v", round, err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("Round %d: timeout draining stream", round)
			}
		}
	roundDone:

		if !gotFinal {
			t.Errorf("Round %d: never got final result after worker death", round)
		}

		// Verify a simple call works after recovery.
		nonStreamOpts := setupNonStreamingScenario(t, fmt.Sprintf("resilience-verify-%d", round), `"recovered"`)
		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetGreeting",
			Input:   map[string]any{"name": "test"},
			Options: nonStreamOpts,
		})
		if err != nil {
			t.Fatalf("Round %d: post-recovery call failed: %v", round, err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Round %d: post-recovery call status %d: %s", round, resp.StatusCode, resp.Error)
		}
		t.Logf("Round %d: recovery verified", round)
	}
}

// TestConcurrentParsesDuringWorkerDeath fires many concurrent Parse
// requests and kills a worker. Parse uses a simpler retry path (no
// streaming), so this tests the Parse-specific retry logic.
func TestConcurrentParsesDuringWorkerDeath(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const concurrency = 30

	var wg sync.WaitGroup
	var failures atomic.Int32

	// Start a long-running stream to create in-flight work for the kill.
	scenarioID := "resilience-parse-bg-stream"
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        `{"name": "Background", "age": 1, "tags": []}`,
		ChunkSize:      5,
		InitialDelayMs: 100,
		ChunkDelayMs:   500, // slow stream keeps worker busy
	}
	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register background scenario: %v", err)
	}

	bgOpts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
	}
	// Fire a background stream so there's an in-flight request to kill.
	bgEvents, _ := BAMLClient.Stream(ctx, testutil.CallRequest{
		Method:  "GetPerson",
		Input:   map[string]any{"description": "background"},
		Options: bgOpts,
	})

	// Wait for the background stream to start.
	select {
	case _, ok := <-bgEvents:
		if !ok {
			t.Fatal("Background stream closed immediately")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for background stream")
	}

	// Fire concurrent parse requests.
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
				Method: "GetPerson",
				Raw:    fmt.Sprintf(`{"name": "Parse%d", "age": %d, "tags": []}`, idx, idx),
			})
			if err != nil {
				t.Logf("Parse %d transport error: %v", idx, err)
				failures.Add(1)
				return
			}
			if resp.StatusCode != 200 {
				t.Logf("Parse %d status %d: %s", idx, resp.StatusCode, resp.Error)
				failures.Add(1)
			}
		}(i)
	}

	// Kill a worker while parses are in flight.
	time.Sleep(50 * time.Millisecond)
	killCtx, killCancel := context.WithTimeout(ctx, 5*time.Second)
	result, err := BAMLClient.KillWorker(killCtx)
	killCancel()
	if err != nil {
		t.Logf("KillWorker failed: %v", err)
	} else {
		t.Logf("Killed worker %d (%d in-flight)", result.WorkerID, result.InFlightCount)
	}

	wg.Wait()

	// Drain the background stream.
	go func() {
		for range bgEvents {
		}
	}()

	if f := failures.Load(); f > 0 {
		t.Errorf("%d/%d concurrent parse requests failed during worker death", f, concurrency)
	}
}

// TestMixedRequestsDuringWorkerDeath fires a mix of Call, Stream, and
// Parse requests concurrently, then kills a worker. Verifies that all
// three request types recover.
func TestMixedRequestsDuringWorkerDeath(t *testing.T) {
	waitForHealthy(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Register scenarios.
	callContent := `"mixed_call_ok"`
	callScenario := "resilience-mixed-call"
	callOpts := setupNonStreamingScenario(t, callScenario, callContent)

	streamContent := `{"name": "MixedStream", "age": 77, "tags": ["mixed"]}`
	streamScenarioID := "resilience-mixed-stream"
	streamScenario := &mockllm.Scenario{
		ID:             streamScenarioID,
		Provider:       "openai",
		Content:        streamContent,
		ChunkSize:      10,
		InitialDelayMs: 200,
		ChunkDelayMs:   100,
	}
	if err := MockClient.RegisterScenario(ctx, streamScenario); err != nil {
		t.Fatalf("Failed to register stream scenario: %v", err)
	}
	streamOpts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, streamScenarioID),
	}

	var wg sync.WaitGroup
	var callFailures, streamFailures, parseFailures atomic.Int32
	const perType = 10

	// Call requests.
	for i := 0; i < perType; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetGreeting",
				Input:   map[string]any{"name": fmt.Sprintf("caller_%d", idx)},
				Options: callOpts,
			})
			if err != nil || resp.StatusCode != 200 {
				callFailures.Add(1)
			}
		}(i)
	}

	// Stream requests.
	for i := 0; i < perType; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": fmt.Sprintf("streamer_%d", idx)},
				Options: streamOpts,
			})
			var gotFinal, gotError bool
			for {
				select {
				case event, ok := <-events:
					if !ok {
						goto done
					}
					if event.IsFinal() {
						gotFinal = true
					}
					if event.IsError() {
						gotError = true
					}
				case err := <-errs:
					if err != nil {
						gotError = true
					}
				case <-ctx.Done():
					goto done
				}
			}
		done:
			if !gotFinal || gotError {
				streamFailures.Add(1)
			}
		}(i)
	}

	// Parse requests.
	for i := 0; i < perType; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
				Method: "GetGreeting",
				Raw:    fmt.Sprintf(`"greeting_%d"`, idx),
			})
			if err != nil || resp.StatusCode != 200 {
				parseFailures.Add(1)
			}
		}(i)
	}

	// Kill a worker after requests have started.
	time.Sleep(300 * time.Millisecond)
	killCtx, killCancel := context.WithTimeout(ctx, 5*time.Second)
	result, err := BAMLClient.KillWorker(killCtx)
	killCancel()
	if err != nil {
		t.Logf("KillWorker failed: %v", err)
	} else {
		t.Logf("Killed worker %d (%d in-flight)", result.WorkerID, result.InFlightCount)
	}

	wg.Wait()

	if f := callFailures.Load(); f > 0 {
		t.Errorf("Call: %d/%d failed", f, perType)
	}
	if f := streamFailures.Load(); f > 0 {
		t.Errorf("Stream: %d/%d failed", f, perType)
	}
	if f := parseFailures.Load(); f > 0 {
		t.Errorf("Parse: %d/%d failed", f, perType)
	}
}
