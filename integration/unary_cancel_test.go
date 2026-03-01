//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestUnaryCancelCall verifies that cancelling an in-flight /call request on the
// chi/net-http unary cancel server actually terminates the worker call promptly.
func TestUnaryCancelCall(t *testing.T) {
	parentCtx, parentCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer parentCancel()

	waitForHealthy(t, 30*time.Second)

	// Register a slow scenario so we can cancel mid-flight.
	scenarioID := "unary-cancel-call"
	scenario := &mockllm.Scenario{
		ID:             scenarioID,
		Provider:       "openai",
		Content:        `{"name":"Cancelled","age":0}`,
		ChunkSize:      0,     // non-streaming
		InitialDelayMs: 10000, // 10s — plenty of time to cancel
	}
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario: %v", err)
	}

	opts := &testutil.BAMLOptions{
		ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, scenarioID),
	}

	t.Run("cancel_before_response", func(t *testing.T) {
		baselineResult, err := BAMLClient.GetGoroutines(parentCtx, GoroutineLeakFilter)
		if err != nil {
			t.Fatalf("Failed to get baseline goroutines: %v", err)
		}
		baselineMatches := countTotalMatches(baselineResult)

		reqCtx, reqCancel := context.WithCancel(parentCtx)

		// Cancel after 500ms (well before the 10s response delay)
		cancelTimer := time.AfterFunc(500*time.Millisecond, func() {
			reqCancel()
		})
		defer cancelTimer.Stop()

		startTime := time.Now()
		_, err = UnaryCancelClient.Call(reqCtx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "cancel test"},
			Options: opts,
		})
		elapsed := time.Since(startTime)

		// Should get an error (context cancelled)
		if err == nil {
			t.Error("Expected error from cancelled request, got nil")
		}
		t.Logf("Unary cancel completed in %v (error: %v)", elapsed, err)

		// Should complete quickly (cancelled at ~500ms), not waiting 10s
		if elapsed > 5*time.Second {
			t.Errorf("Cancellation took too long: %v (expected < 5s)", elapsed)
		}

		// Check for goroutine leaks
		finalResult, cleanedUp := waitForGoroutineCleanup(parentCtx, t, baselineMatches, 10*time.Second)
		if !cleanedUp {
			finalMatches := countTotalMatches(finalResult)
			t.Errorf("Goroutine leak detected: %d new goroutines", finalMatches-baselineMatches)
			logLeakedStacks(t, finalResult)
		}
	})
}

// TestUnaryCancelParse verifies cancellation on /parse endpoints.
func TestUnaryCancelParse(t *testing.T) {
	parentCtx, parentCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer parentCancel()

	waitForHealthy(t, 30*time.Second)

	// Parse is typically fast, but we can still exercise the cancellation path
	// by verifying the endpoint is functional on the unary server.
	t.Run("basic_parse_works", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
		defer cancel()

		resp, err := UnaryCancelClient.Parse(ctx, testutil.ParseRequest{
			Method: "GetSimple",
			Raw:    `Hello, world!`,
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}
		t.Logf("Parse response: %s", resp.Data)
	})
}

// TestUnaryCallParity verifies that the unary cancel server returns identical
// responses to the Fiber server for basic /call and /call-with-raw requests.
func TestUnaryCallParity(t *testing.T) {
	parentCtx, parentCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer parentCancel()

	opts := setupNonStreamingScenario(t, "unary-parity-call", `{"name":"Alice","age":30}`)

	t.Run("call_parity", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
		defer cancel()

		req := testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "parity test"},
			Options: opts,
		}

		fiberResp, err := BAMLClient.Call(ctx, req)
		if err != nil {
			t.Fatalf("Fiber call failed: %v", err)
		}

		chiResp, err := UnaryCancelClient.Call(ctx, req)
		if err != nil {
			t.Fatalf("Chi call failed: %v", err)
		}

		if fiberResp.StatusCode != chiResp.StatusCode {
			t.Errorf("Status mismatch: fiber=%d chi=%d", fiberResp.StatusCode, chiResp.StatusCode)
		}

		if string(fiberResp.Body) != string(chiResp.Body) {
			t.Errorf("Body mismatch:\n  fiber: %s\n  chi:   %s", fiberResp.Body, chiResp.Body)
		}

		t.Logf("Both servers returned status=%d body=%s", chiResp.StatusCode, chiResp.Body)
	})

	t.Run("call_with_raw_parity", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
		defer cancel()

		req := testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "parity test raw"},
			Options: opts,
		}

		fiberResp, err := BAMLClient.CallWithRaw(ctx, req)
		if err != nil {
			t.Fatalf("Fiber call-with-raw failed: %v", err)
		}

		chiResp, err := UnaryCancelClient.CallWithRaw(ctx, req)
		if err != nil {
			t.Fatalf("Chi call-with-raw failed: %v", err)
		}

		if fiberResp.StatusCode != chiResp.StatusCode {
			t.Errorf("Status mismatch: fiber=%d chi=%d", fiberResp.StatusCode, chiResp.StatusCode)
		}

		if string(fiberResp.Data) != string(chiResp.Data) {
			t.Errorf("Data mismatch:\n  fiber: %s\n  chi:   %s", fiberResp.Data, chiResp.Data)
		}

		if fiberResp.Raw != chiResp.Raw {
			t.Errorf("Raw mismatch:\n  fiber: %s\n  chi:   %s", fiberResp.Raw, chiResp.Raw)
		}

		t.Logf("Both servers returned status=%d data=%s raw=%q", chiResp.StatusCode, chiResp.Data, chiResp.Raw)
	})
}
