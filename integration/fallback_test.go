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

// ============================================================
// /call endpoint — fallback chain tests
//
// These tests exercise baml-fallback client strategies. The BAML runtime
// handles fallback routing internally (legacy CallStream+OnTick path).
// When a child client fails (HTTP 500), the runtime retries with the
// next child in the strategy list.
//
// Failure scenarios use FailAfter=1, FailureMode="500", ChunkSize=0.
// The mock returns HTTP 500 before any response body for this combination,
// which works for both streaming and non-streaming requests.
// ============================================================

func TestFallbackCall(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		t.Run("primary_succeeds", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			registerFallbackScenario(t, &mockllm.Scenario{
				ID:             "fallback-primary",
				Provider:       "openai",
				Content:        "Hello from primary!",
				ChunkSize:      0,
				InitialDelayMs: 50,
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
		})

		t.Run("primary_fails_secondary_succeeds", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)

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
		})

		t.Run("three_client_chain_first_two_fail", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)

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
		})

		t.Run("all_clients_fail", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)

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
		})

		t.Run("object_output_through_fallback", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)

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
		})
	})
}

// ============================================================
// /stream endpoint — fallback chain tests
// ============================================================

func TestFallbackStream(t *testing.T) {
	t.Run("primary_succeeds_stream", func(t *testing.T) {
		waitForHealthy(t, 30*time.Second)

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
	})

	t.Run("primary_fails_secondary_succeeds_stream", func(t *testing.T) {
		waitForHealthy(t, 30*time.Second)

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
	})

	t.Run("all_clients_fail_stream", func(t *testing.T) {
		waitForHealthy(t, 30*time.Second)

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
		<-errc

		if !gotError {
			t.Error("Expected an error event when all fallback clients fail")
		}
	})
}

// ============================================================
// /call-with-raw endpoint — fallback chain tests
// ============================================================

func TestFallbackCallWithRaw(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		t.Run("fallback_returns_raw", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)

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
			if resp.Raw == "" {
				t.Error("Expected non-empty Raw field in call-with-raw response")
			}
		})
	})
}
