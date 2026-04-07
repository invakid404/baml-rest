//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// registerFallbackScenario registers a scenario on the mock LLM for a
// fallback chain child client. The scenarioID must match the child's
// "model" field in clients.baml.
func registerFallbackScenario(t *testing.T, scenario *mockllm.Scenario) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := MockClient.RegisterScenario(ctx, scenario); err != nil {
		t.Fatalf("Failed to register scenario %q: %v", scenario.ID, err)
	}
}

// fallbackScenarioIDs lists the scenario IDs used by fallback tests. Deleting
// only these (instead of calling ClearScenarios) avoids wiping scenarios
// registered by other integration test files.
var fallbackScenarioIDs = []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"}

// clearFallbackScenarios removes the known fallback scenarios so each subtest
// starts from a clean request-count state with no stale entries.
func clearFallbackScenarios(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range fallbackScenarioIDs {
		if err := MockClient.DeleteScenario(ctx, id); err != nil {
			t.Fatalf("Failed to delete scenario %q: %v", id, err)
		}
	}
}

// assertHitCounts verifies that each scenario received exactly the expected
// number of requests.  This proves the fallback chain was traversed correctly
// rather than just checking the terminal result.
func assertHitCounts(t *testing.T, expected map[string]int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for id, want := range expected {
		got, err := MockClient.GetRequestCount(ctx, id)
		if err != nil {
			t.Fatalf("Failed to get request count for %q: %v", id, err)
		}
		if got != want {
			t.Errorf("scenario %q: expected %d requests, got %d", id, want, got)
		}
	}
}

// ============================================================
// /call endpoint — fallback chain tests
//
// These tests exercise baml-fallback client strategies across both routing
// implementations. On supported BuildRequest configurations, fallback chains
// run through the BuildRequest orchestrators; otherwise they fall back to the
// legacy CallStream+OnTick path. In either case, when a child client fails
// (HTTP 500), the next child in the strategy list should be tried.
//
// Failure scenarios use FailAfter=1, FailureMode="500", ChunkSize=0.
// The mock returns HTTP 500 before any response body for this combination,
// which works for both streaming and non-streaming requests.
// ============================================================

func TestFallbackCall(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		t.Run("primary_succeeds", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)
			clearFallbackScenarios(t)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			registerFallbackScenario(t, &mockllm.Scenario{
				ID:             "fallback-primary",
				Provider:       "openai",
				Content:        "Hello from primary!",
				ChunkSize:      0,
				InitialDelayMs: 50,
			})
			// Register secondary to reset its counter even though it shouldn't be hit.
			registerFallbackScenario(t, &mockllm.Scenario{
				ID: "fallback-secondary", Provider: "openai", Content: "unused",
			})

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetGreetingFallbackPair",
				Input:  map[string]any{"name": "World"},
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result string
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}
			if result != "Hello from primary!" {
				t.Errorf("Expected 'Hello from primary!', got %q", result)
			}

			assertHitCounts(t, map[string]int{"fallback-primary": 1, "fallback-secondary": 0})
		})

		t.Run("primary_fails_secondary_succeeds", func(t *testing.T) {
			if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
				t.Skip("Skipping: baml-fallback client retry requires BAML >= 0.219.0")
			}

			waitForHealthy(t, 30*time.Second)
			clearFallbackScenarios(t)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Primary returns 500 immediately
			registerFallbackScenario(t, &mockllm.Scenario{
				ID:          "fallback-primary",
				Provider:    "openai",
				Content:     "should not see this",
				FailAfter:   1,
				FailureMode: "500",
			})
			// Secondary succeeds
			registerFallbackScenario(t, &mockllm.Scenario{
				ID:             "fallback-secondary",
				Provider:       "openai",
				Content:        "Hello from secondary!",
				ChunkSize:      20,
				InitialDelayMs: 50,
				ChunkDelayMs:   5,
			})

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetGreetingFallbackPair",
				Input:  map[string]any{"name": "World"},
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result string
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}
			if result != "Hello from secondary!" {
				t.Errorf("Expected 'Hello from secondary!', got %q", result)
			}

			assertHitCounts(t, map[string]int{"fallback-primary": 1, "fallback-secondary": 1})
		})

		t.Run("three_client_chain_first_two_fail", func(t *testing.T) {
			if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
				t.Skip("Skipping: baml-fallback client retry requires BAML >= 0.219.0")
			}

			waitForHealthy(t, 30*time.Second)
			clearFallbackScenarios(t)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Primary and secondary return 500
			registerFallbackScenario(t, &mockllm.Scenario{
				ID:          "fallback-primary",
				Provider:    "openai",
				Content:     "nope",
				FailAfter:   1,
				FailureMode: "500",
			})
			registerFallbackScenario(t, &mockllm.Scenario{
				ID:          "fallback-secondary",
				Provider:    "openai",
				Content:     "nope",
				FailAfter:   1,
				FailureMode: "500",
			})
			// Tertiary succeeds
			registerFallbackScenario(t, &mockllm.Scenario{
				ID:             "fallback-tertiary",
				Provider:       "openai",
				Content:        "Hello from tertiary!",
				ChunkSize:      20,
				InitialDelayMs: 50,
				ChunkDelayMs:   5,
			})

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetGreetingFallbackChain",
				Input:  map[string]any{"name": "World"},
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result string
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}
			if result != "Hello from tertiary!" {
				t.Errorf("Expected 'Hello from tertiary!', got %q", result)
			}

			assertHitCounts(t, map[string]int{"fallback-primary": 1, "fallback-secondary": 1, "fallback-tertiary": 1})
		})

		t.Run("all_clients_fail", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)
			clearFallbackScenarios(t)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			// All return 500
			for _, id := range []string{"fallback-primary", "fallback-secondary"} {
				registerFallbackScenario(t, &mockllm.Scenario{
					ID:          id,
					Provider:    "openai",
					Content:     "nope",
					FailAfter:   1,
					FailureMode: "500",
				})
			}

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetGreetingFallbackPair",
				Input:  map[string]any{"name": "World"},
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			// Should get an error status (retries exhausted)
			if resp.StatusCode == 200 {
				t.Fatalf("Expected non-200 status when all fallback clients fail, got 200")
			}

			if bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
				// BuildRequest round-robins children: 2 children × 2 cycles = 2 per child.
				// Legacy runtime applies its own per-child retry, doubling the count.
				if UseBuildRequest {
					assertHitCounts(t, map[string]int{"fallback-primary": 2, "fallback-secondary": 2})
				} else {
					assertHitCounts(t, map[string]int{"fallback-primary": 4, "fallback-secondary": 4})
				}
			}
		})

		t.Run("object_output_through_fallback", func(t *testing.T) {
			if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
				t.Skip("Skipping: baml-fallback client retry requires BAML >= 0.219.0")
			}

			waitForHealthy(t, 30*time.Second)
			clearFallbackScenarios(t)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Primary returns 500, secondary returns structured output
			registerFallbackScenario(t, &mockllm.Scenario{
				ID:          "fallback-primary",
				Provider:    "openai",
				Content:     "nope",
				FailAfter:   1,
				FailureMode: "500",
			})
			registerFallbackScenario(t, &mockllm.Scenario{
				ID:             "fallback-secondary",
				Provider:       "openai",
				Content:        `{"message": "fallback structured"}`,
				ChunkSize:      20,
				InitialDelayMs: 50,
				ChunkDelayMs:   5,
			})

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetSimpleFallback",
				Input:  map[string]any{"input": "test"},
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}
			if result.Message != "fallback structured" {
				t.Errorf("Expected 'fallback structured', got %q", result.Message)
			}

			assertHitCounts(t, map[string]int{"fallback-primary": 1, "fallback-secondary": 1})
		})
	})
}

// ============================================================
// /call-with-raw endpoint — fallback chain tests
// ============================================================

func TestFallbackCallWithRaw(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		t.Run("fallback_returns_raw", func(t *testing.T) {
			if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
				t.Skip("Skipping: baml-fallback client retry requires BAML >= 0.219.0")
			}
			if !UseBuildRequest {
				t.Skip("Skipping: Raw propagation through fallback chains is a BuildRequest-only feature")
			}

			waitForHealthy(t, 30*time.Second)
			clearFallbackScenarios(t)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			registerFallbackScenario(t, &mockllm.Scenario{
				ID:          "fallback-primary",
				Provider:    "openai",
				Content:     "nope",
				FailAfter:   1,
				FailureMode: "500",
			})
			registerFallbackScenario(t, &mockllm.Scenario{
				ID:             "fallback-secondary",
				Provider:       "openai",
				Content:        "raw fallback response",
				ChunkSize:      20,
				InitialDelayMs: 50,
				ChunkDelayMs:   5,
			})

			resp, err := client.CallWithRaw(ctx, testutil.CallRequest{
				Method: "GetGreetingFallbackPair",
				Input:  map[string]any{"name": "World"},
			})
			if err != nil {
				t.Fatalf("CallWithRaw failed: %v", err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}
			if resp.Raw != "raw fallback response" {
				t.Fatalf("Expected Raw()='raw fallback response', got %q", resp.Raw)
			}

			assertHitCounts(t, map[string]int{"fallback-primary": 1, "fallback-secondary": 1})
		})
	})
}

// ============================================================
// /stream endpoint — fallback chain tests
//
// Streaming is only available on the Fiber backend; the chi unary
// server does not expose /stream endpoints. Therefore these tests
// use BAMLClient directly instead of iterating with forEachUnaryClient.
// ============================================================

func TestFallbackStream(t *testing.T) {
	t.Run("primary_succeeds_stream", func(t *testing.T) {
		waitForHealthy(t, 30*time.Second)
		clearFallbackScenarios(t)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		registerFallbackScenario(t, &mockllm.Scenario{
			ID:             "fallback-primary",
			Provider:       "openai",
			Content:        "Streaming from primary!",
			ChunkSize:      10,
			InitialDelayMs: 50,
			ChunkDelayMs:   10,
		})
		registerFallbackScenario(t, &mockllm.Scenario{
			ID: "fallback-secondary", Provider: "openai", Content: "unused",
		})

		partials, errc := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method: "GetGreetingFallbackPair",
			Input:  map[string]any{"name": "World"},
		})

		var finalData json.RawMessage
		for ev := range partials {
			if ev.IsFinal() {
				finalData = ev.Data
			}
		}
		if streamErr := <-errc; streamErr != nil {
			t.Fatalf("Stream error: %v", streamErr)
		}

		var result string
		if err := json.Unmarshal(finalData, &result); err != nil {
			t.Fatalf("Failed to unmarshal final: %v", err)
		}
		if result != "Streaming from primary!" {
			t.Errorf("Expected 'Streaming from primary!', got %q", result)
		}

		// BAML < 0.219.0 uses the legacy streaming fallback path, which may
		// contact the secondary child even when the primary succeeds. Unary does
		// not share that behavior, so only assert the exact streaming hit counts
		// on versions that use the BuildRequest fallback orchestrator.
		if bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
			assertHitCounts(t, map[string]int{"fallback-primary": 1, "fallback-secondary": 0})
		}
	})

	t.Run("primary_fails_secondary_succeeds_stream", func(t *testing.T) {
		if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
			t.Skip("Skipping: baml-fallback client retry requires BAML >= 0.219.0")
		}

		waitForHealthy(t, 30*time.Second)
		clearFallbackScenarios(t)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Primary returns 500
		registerFallbackScenario(t, &mockllm.Scenario{
			ID:          "fallback-primary",
			Provider:    "openai",
			Content:     "nope",
			FailAfter:   1,
			FailureMode: "500",
		})
		// Secondary streams successfully
		registerFallbackScenario(t, &mockllm.Scenario{
			ID:             "fallback-secondary",
			Provider:       "openai",
			Content:        "Streaming from secondary!",
			ChunkSize:      10,
			InitialDelayMs: 50,
			ChunkDelayMs:   10,
		})

		partials, errc := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method: "GetGreetingFallbackPair",
			Input:  map[string]any{"name": "World"},
		})

		var finalData json.RawMessage
		for ev := range partials {
			if ev.IsFinal() {
				finalData = ev.Data
			}
		}
		if streamErr := <-errc; streamErr != nil {
			t.Fatalf("Stream error: %v", streamErr)
		}

		var result string
		if err := json.Unmarshal(finalData, &result); err != nil {
			t.Fatalf("Failed to unmarshal final: %v", err)
		}
		if result != "Streaming from secondary!" {
			t.Errorf("Expected 'Streaming from secondary!', got %q", result)
		}

		assertHitCounts(t, map[string]int{"fallback-primary": 1, "fallback-secondary": 1})
	})

	t.Run("all_clients_fail_stream", func(t *testing.T) {
		waitForHealthy(t, 30*time.Second)
		clearFallbackScenarios(t)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		for _, id := range []string{"fallback-primary", "fallback-secondary"} {
			registerFallbackScenario(t, &mockllm.Scenario{
				ID:          id,
				Provider:    "openai",
				Content:     "nope",
				FailAfter:   1,
				FailureMode: "500",
			})
		}

		partials, errc := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method: "GetGreetingFallbackPair",
			Input:  map[string]any{"name": "World"},
		})

		var gotError bool
		for ev := range partials {
			if ev.IsError() {
				gotError = true
			}
		}
		// Drain the error channel
		if streamErr := <-errc; streamErr != nil {
			t.Logf("Stream error channel: %v", streamErr)
		}

		if !gotError {
			t.Error("Expected an error event when all fallback clients fail")
		}

		if bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
			if UseBuildRequest {
				assertHitCounts(t, map[string]int{"fallback-primary": 2, "fallback-secondary": 2})
			} else {
				assertHitCounts(t, map[string]int{"fallback-primary": 4, "fallback-secondary": 4})
			}
		}
	})
}
