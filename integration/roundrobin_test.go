//go:build integration

package integration

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// ============================================================
// baml-roundrobin integration tests
//
// These reuse the fallback-{primary,secondary,tertiary} mock scenarios
// (each addressable via its model name on the BAML client side) and
// count per-scenario hits to verify the rotation.
//
// The coordinator starts at a random offset per process, so absolute
// first-child identity is not predictable. Tests instead verify:
//   - consecutive requests hit distinct children (contiguous rotation);
//   - over 2N requests, a 2-child RR hits each child exactly N times;
//   - metadata (X-BAML-RoundRobin-* headers + streaming metadata event)
//     surfaces the selected child and its position in the child list.
// ============================================================

// roundRobinScenarioIDs lists the scenario IDs used by RR tests. Deleting
// only these keeps scenarios registered by other tests untouched.
var roundRobinScenarioIDs = []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"}

func clearRoundRobinScenarios(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range roundRobinScenarioIDs {
		if err := MockClient.DeleteScenario(ctx, id); err != nil {
			t.Fatalf("Failed to delete scenario %q: %v", id, err)
		}
	}
}

func registerRoundRobinScenario(t *testing.T, s *mockllm.Scenario) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := MockClient.RegisterScenario(ctx, s); err != nil {
		t.Fatalf("Failed to register scenario %q: %v", s.ID, err)
	}
}

// registerAllGreetingScenarios registers a content-returning scenario for
// each RR child so any child hit produces a valid response. The returned
// content is distinct so callers can verify which child handled the call.
func registerAllGreetingScenarios(t *testing.T, ids []string) {
	t.Helper()
	for _, id := range ids {
		registerRoundRobinScenario(t, &mockllm.Scenario{
			ID:             id,
			Provider:       "openai",
			Content:        "Hello from " + id + "!",
			ChunkSize:      0,
			InitialDelayMs: 10,
		})
	}
}

// assertBalanced verifies that total requests across RR children equals
// the expected total and that the per-child counts differ by at most 1.
// This is the correct invariant for round-robin with an unknown starting
// offset: across N requests the load is floor(N/k) or ceil(N/k) per child.
func assertBalanced(t *testing.T, ids []string, total int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	counts := make([]int, len(ids))
	sum := 0
	for i, id := range ids {
		got, err := MockClient.GetRequestCount(ctx, id)
		if err != nil {
			t.Fatalf("Failed to get request count for %q: %v", id, err)
		}
		counts[i] = got
		sum += got
	}
	if sum != total {
		t.Fatalf("total requests across %v: got %d, want %d (per-child: %v)", ids, sum, total, counts)
	}
	// Round-robin invariant: max-min <= 1.
	lo, hi := counts[0], counts[0]
	for _, c := range counts[1:] {
		if c < lo {
			lo = c
		}
		if c > hi {
			hi = c
		}
	}
	if hi-lo > 1 {
		t.Errorf("round-robin imbalance across %v: per-child counts %v (expected max-min <= 1)", ids, counts)
	}
}

// ============================================================
// /call endpoint — round-robin tests
// ============================================================

func TestRoundRobinCall(t *testing.T) {
	if !ActuallyBuildRequest() {
		// The centralised round-robin wiring (SharedState, RemoteAdvancer,
		// the pool's request_id plumbing) only matters on the BuildRequest
		// path. The legacy path delegates RR to BAML's runtime, which has
		// its own rotation and is observable only via outcome metadata —
		// these tests assert planned-metadata headers and per-child hit
		// counts that the legacy path doesn't guarantee. Skip on legacy
		// runs rather than flake.
		t.Skip("Skipping: baml-roundrobin BuildRequest-path tests require BAML >= 0.219.0 AND BAML_REST_USE_BUILD_REQUEST=true")
	}
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		t.Run("two_client_rotation", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)
			clearRoundRobinScenarios(t)
			registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary"})

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			const runs = 4
			for i := 0; i < runs; i++ {
				resp, err := client.Call(ctx, testutil.CallRequest{
					Method: "GetGreetingRoundRobinPair",
					Input:  map[string]any{"name": "World"},
				})
				if err != nil {
					t.Fatalf("Call %d failed: %v", i, err)
				}
				if resp.StatusCode != 200 {
					t.Fatalf("Call %d: expected 200, got %d: %s", i, resp.StatusCode, resp.Error)
				}
			}

			assertBalanced(t, []string{"fallback-primary", "fallback-secondary"}, runs)
		})

		t.Run("round_robin_metadata_headers", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)
			clearRoundRobinScenarios(t)
			registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary"})

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetGreetingRoundRobinPair",
				Input:  map[string]any{"name": "World"},
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Expected 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			// Planned metadata: X-BAML-Client is the effective leaf (a child
			// of the RR strategy), and the RoundRobin-* headers describe the
			// decision.
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLRoundRobinName, "TestRoundRobinPair")
			testutil.AssertHeaderPresent(t, resp.Headers, testutil.HeaderBAMLRoundRobinSelected)
			testutil.AssertHeaderPresent(t, resp.Headers, testutil.HeaderBAMLRoundRobinIndex)
		})

		t.Run("three_client_rotation", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)
			clearRoundRobinScenarios(t)
			registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"})

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			const runs = 6
			for i := 0; i < runs; i++ {
				resp, err := client.Call(ctx, testutil.CallRequest{
					Method: "GetGreetingRoundRobinChain",
					Input:  map[string]any{"name": "World"},
				})
				if err != nil {
					t.Fatalf("Call %d failed: %v", i, err)
				}
				if resp.StatusCode != 200 {
					t.Fatalf("Call %d: expected 200, got %d: %s", i, resp.StatusCode, resp.Error)
				}
			}

			assertBalanced(t, []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"}, runs)
		})
	})
}

// ============================================================
// /call-with-raw endpoint — round-robin tests
// ============================================================

func TestRoundRobinCallWithRaw(t *testing.T) {
	if !ActuallyBuildRequest() {
		// The centralised round-robin wiring (SharedState, RemoteAdvancer,
		// the pool's request_id plumbing) only matters on the BuildRequest
		// path. The legacy path delegates RR to BAML's runtime, which has
		// its own rotation and is observable only via outcome metadata —
		// these tests assert planned-metadata headers and per-child hit
		// counts that the legacy path doesn't guarantee. Skip on legacy
		// runs rather than flake.
		t.Skip("Skipping: baml-roundrobin BuildRequest-path tests require BAML >= 0.219.0 AND BAML_REST_USE_BUILD_REQUEST=true")
	}
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		t.Run("raw_propagates_with_rotation", func(t *testing.T) {
			waitForHealthy(t, 30*time.Second)
			clearRoundRobinScenarios(t)
			registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary"})

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			const runs = 2
			seenRaws := map[string]bool{}
			seenSelected := map[string]bool{}
			seenIndices := map[string]bool{}
			wantChildren := map[string]bool{
				"FallbackPrimary":   true,
				"FallbackSecondary": true,
			}
			for i := 0; i < runs; i++ {
				resp, err := client.CallWithRaw(ctx, testutil.CallRequest{
					Method: "GetGreetingRoundRobinPair",
					Input:  map[string]any{"name": "World"},
				})
				if err != nil {
					t.Fatalf("CallWithRaw %d failed: %v", i, err)
				}
				if resp.StatusCode != 200 {
					t.Fatalf("CallWithRaw %d: expected 200, got %d: %s", i, resp.StatusCode, resp.Error)
				}
				seenRaws[resp.Raw] = true

				// Full RR metadata: match what the /call test asserts so
				// we catch regressions where one endpoint drops part of
				// the planned metadata the other still surfaces.
				testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLRoundRobinName, "TestRoundRobinPair")
				testutil.AssertHeaderPresent(t, resp.Headers, testutil.HeaderBAMLRoundRobinSelected)
				testutil.AssertHeaderPresent(t, resp.Headers, testutil.HeaderBAMLRoundRobinIndex)

				selected := resp.Headers.Get(testutil.HeaderBAMLRoundRobinSelected)
				if !wantChildren[selected] {
					t.Errorf("CallWithRaw %d: Selected=%q is not in the configured children (FallbackPrimary, FallbackSecondary)", i, selected)
				}
				seenSelected[selected] = true

				indexStr := resp.Headers.Get(testutil.HeaderBAMLRoundRobinIndex)
				idx, err := strconv.Atoi(indexStr)
				if err != nil {
					t.Errorf("CallWithRaw %d: Index header %q not an integer: %v", i, indexStr, err)
				} else if idx < 0 || idx >= 2 {
					t.Errorf("CallWithRaw %d: Index %d out of range [0, 2)", i, idx)
				}
				seenIndices[indexStr] = true
			}

			// Consecutive requests on a 2-child RR must rotate, so both
			// Selected values and both Index values must appear across
			// the runs. This is the same consistency invariant the
			// streaming tests check via RoundRobinInfo.
			if len(seenSelected) != 2 {
				t.Errorf("expected both children to be selected across %d runs, got selected set %v", runs, seenSelected)
			}
			if len(seenIndices) != 2 {
				t.Errorf("expected both indices to appear across %d runs, got index set %v", runs, seenIndices)
			}

			// Two consecutive runs must hit two distinct children, so two
			// distinct raw responses.
			if len(seenRaws) != 2 {
				t.Errorf("expected 2 distinct raw responses across %d runs, got %d (%v)", runs, len(seenRaws), seenRaws)
			}

			assertBalanced(t, []string{"fallback-primary", "fallback-secondary"}, runs)
		})
	})
}

// ============================================================
// /stream endpoint — round-robin tests
//
// Streaming is only exposed on the Fiber backend; chi unary does not
// serve /stream endpoints. Tests use BAMLClient directly.
// ============================================================

func TestRoundRobinStream(t *testing.T) {
	if !ActuallyBuildRequest() {
		// The centralised round-robin wiring (SharedState, RemoteAdvancer,
		// the pool's request_id plumbing) only matters on the BuildRequest
		// path. The legacy path delegates RR to BAML's runtime, which has
		// its own rotation and is observable only via outcome metadata —
		// these tests assert planned-metadata headers and per-child hit
		// counts that the legacy path doesn't guarantee. Skip on legacy
		// runs rather than flake.
		t.Skip("Skipping: baml-roundrobin BuildRequest-path tests require BAML >= 0.219.0 AND BAML_REST_USE_BUILD_REQUEST=true")
	}

	t.Run("rotates_across_streams", func(t *testing.T) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		for _, id := range []string{"fallback-primary", "fallback-secondary"} {
			registerRoundRobinScenario(t, &mockllm.Scenario{
				ID:             id,
				Provider:       "openai",
				Content:        "Streaming from " + id + "!",
				ChunkSize:      10,
				InitialDelayMs: 20,
				ChunkDelayMs:   5,
			})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		const runs = 2
		seenFinals := map[string]bool{}
		for i := 0; i < runs; i++ {
			partials, errc := BAMLClient.Stream(ctx, testutil.CallRequest{
				Method: "GetGreetingRoundRobinPair",
				Input:  map[string]any{"name": "World"},
			})

			tracker := newMetadataTracker(t)
			var finalData json.RawMessage
			for ev := range partials {
				if ev.IsMetadata() {
					tracker.record(ev)
					continue
				}
				if ev.IsFinal() {
					finalData = ev.Data
					tracker.markFinal()
				}
			}
			if streamErr := <-errc; streamErr != nil {
				t.Fatalf("Stream %d error: %v", i, streamErr)
			}

			var result string
			if err := json.Unmarshal(finalData, &result); err != nil {
				t.Fatalf("Stream %d: failed to unmarshal final: %v", i, err)
			}
			seenFinals[result] = true

			// Planned metadata must carry the RR decision.
			if tracker.planned == nil {
				t.Fatalf("Stream %d: expected planned metadata event", i)
			}
			if tracker.planned.RoundRobin == nil {
				t.Fatalf("Stream %d: planned metadata missing RoundRobin info: %+v", i, tracker.planned)
			}
			if tracker.planned.RoundRobin.Name != "TestRoundRobinPair" {
				t.Errorf("Stream %d: RoundRobin.Name = %q, want TestRoundRobinPair", i, tracker.planned.RoundRobin.Name)
			}
			if got := tracker.planned.RoundRobin.Selected; got == "" {
				t.Errorf("Stream %d: RoundRobin.Selected is empty", i)
			}
			wantChildren := []string{"FallbackPrimary", "FallbackSecondary"}
			if !equalSliceStrings(tracker.planned.RoundRobin.Children, wantChildren) {
				t.Errorf("Stream %d: RoundRobin.Children = %v, want %v", i, tracker.planned.RoundRobin.Children, wantChildren)
			}
			if idx := tracker.planned.RoundRobin.Index; idx < 0 || idx >= len(wantChildren) {
				t.Errorf("Stream %d: RoundRobin.Index %d out of range", i, idx)
			} else if wantChildren[idx] != tracker.planned.RoundRobin.Selected {
				t.Errorf("Stream %d: Selected=%q does not match Children[%d]=%q", i, tracker.planned.RoundRobin.Selected, idx, wantChildren[idx])
			}
		}

		if len(seenFinals) != 2 {
			t.Errorf("expected 2 distinct streamed results across %d runs, got %d (%v)", runs, len(seenFinals), seenFinals)
		}

		assertBalanced(t, []string{"fallback-primary", "fallback-secondary"}, runs)
	})
}

// ============================================================
// /stream-with-raw endpoint — round-robin tests
// ============================================================

func TestRoundRobinStreamWithRaw(t *testing.T) {
	if !ActuallyBuildRequest() {
		// The centralised round-robin wiring (SharedState, RemoteAdvancer,
		// the pool's request_id plumbing) only matters on the BuildRequest
		// path. The legacy path delegates RR to BAML's runtime, which has
		// its own rotation and is observable only via outcome metadata —
		// these tests assert planned-metadata headers and per-child hit
		// counts that the legacy path doesn't guarantee. Skip on legacy
		// runs rather than flake.
		t.Skip("Skipping: baml-roundrobin BuildRequest-path tests require BAML >= 0.219.0 AND BAML_REST_USE_BUILD_REQUEST=true")
	}

	t.Run("raw_propagates_across_streams", func(t *testing.T) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		for _, id := range []string{"fallback-primary", "fallback-secondary"} {
			registerRoundRobinScenario(t, &mockllm.Scenario{
				ID:             id,
				Provider:       "openai",
				Content:        "Raw stream from " + id,
				ChunkSize:      8,
				InitialDelayMs: 20,
				ChunkDelayMs:   5,
			})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		const runs = 2
		seenRaws := map[string]bool{}
		for i := 0; i < runs; i++ {
			partials, errc := BAMLClient.StreamWithRaw(ctx, testutil.CallRequest{
				Method: "GetGreetingRoundRobinPair",
				Input:  map[string]any{"name": "World"},
			})

			tracker := newMetadataTracker(t)
			var finalRaw string
			for ev := range partials {
				if ev.IsMetadata() {
					tracker.record(ev)
					continue
				}
				if ev.IsFinal() {
					finalRaw = ev.Raw
					tracker.markFinal()
				}
			}
			if streamErr := <-errc; streamErr != nil {
				t.Fatalf("StreamWithRaw %d error: %v", i, streamErr)
			}

			if finalRaw == "" {
				t.Fatalf("StreamWithRaw %d: final event missing raw payload", i)
			}
			seenRaws[finalRaw] = true

			if tracker.planned == nil || tracker.planned.RoundRobin == nil {
				t.Fatalf("StreamWithRaw %d: planned metadata missing RoundRobin info: %+v", i, tracker.planned)
			}
			if tracker.planned.RoundRobin.Name != "TestRoundRobinPair" {
				t.Errorf("StreamWithRaw %d: RoundRobin.Name = %q, want TestRoundRobinPair", i, tracker.planned.RoundRobin.Name)
			}
		}

		if len(seenRaws) != 2 {
			t.Errorf("expected 2 distinct raw payloads across %d runs, got %d (%v)", runs, len(seenRaws), seenRaws)
		}

		assertBalanced(t, []string{"fallback-primary", "fallback-secondary"}, runs)
	})
}
