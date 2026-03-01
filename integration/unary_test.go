//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

func requireUnaryClient(t *testing.T) *testutil.BAMLRestClient {
	t.Helper()
	if UnaryClient == nil {
		t.Skip("unary server not enabled (set UNARY_SERVER=true)")
	}
	return UnaryClient
}

// TestUnaryCancelCall verifies that cancelling an in-flight /call request on the
// chi/net-http unary server actually terminates the worker call promptly.
func TestUnaryCancelCall(t *testing.T) {
	client := requireUnaryClient(t)

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
		_, err = client.Call(reqCtx, testutil.CallRequest{
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

// TestUnaryParse verifies /parse works on the unary server.
func TestUnaryParse(t *testing.T) {
	client := requireUnaryClient(t)

	parentCtx, parentCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer parentCancel()

	waitForHealthy(t, 30*time.Second)

	t.Run("basic_parse_works", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
		defer cancel()

		resp, err := client.Parse(ctx, testutil.ParseRequest{
			Method: "GetSimple",
			Raw:    `{"message": "hello world"}`,
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
