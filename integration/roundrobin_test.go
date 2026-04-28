//go:build integration

package integration

import (
	"context"
	"strconv"
	"strings"
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

// skipIfNoBuildRequest skips the calling test when the container is
// running the legacy dispatch path. The centralised round-robin wiring
// (SharedState, RemoteAdvancer, pool-level request_id) only matters on
// the BuildRequest path; the legacy path delegates rotation to BAML's
// runtime and doesn't guarantee the planned-metadata headers or per-
// child hit counts these tests assert. Hoisted out of the per-test
// inline Skip so the four RR tests stay DRY.
func skipIfNoBuildRequest(t *testing.T) {
	t.Helper()
	if !ActuallyBuildRequest() {
		t.Skip("Skipping: baml-roundrobin BuildRequest-path tests require BAML >= 0.219.0 AND BAML_REST_USE_BUILD_REQUEST=true")
	}
}

// clientForContent maps a mock-LLM response payload back to the
// configured RR child that served it. registerAllGreetingScenarios and
// the streaming scenarios embed the scenario id (which equals the BAML
// client's `model` option) in their Content, so any raw / final
// response carries a stable marker. The test uses this to cross-check
// that the planned-metadata Selected field names the same child that
// actually handled the dispatch — without this check, a bug where
// metadata reported one child while BAML routed to another would pass
// every other assertion (header present, index in range, balanced hit
// counts) because hit counts alone can't bind identity to a specific
// request.
//
// Returns "" when the payload doesn't contain any known scenario id;
// callers should t.Errorf in that case rather than silently pass.
func clientForContent(payload string) string {
	switch {
	case strings.Contains(payload, "fallback-primary"):
		return "FallbackPrimary"
	case strings.Contains(payload, "fallback-secondary"):
		return "FallbackSecondary"
	case strings.Contains(payload, "fallback-tertiary"):
		return "FallbackTertiary"
	}
	return ""
}

// ============================================================
// /call endpoint — round-robin tests
// ============================================================

func TestRoundRobinCall(t *testing.T) {
	skipIfNoBuildRequest(t)
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

			// Children is the RR chain as declared in .baml; the
			// planned-metadata Index is a position in this list. Keeping
			// the expected order here lets us bind Selected ↔ Index
			// per-call and assert consecutive indices step by exactly
			// (prev+1) % len(children). aggregateBalanced alone would
			// pass even if two requests served the same child in a row
			// as long as the totals still balanced out.
			children := []string{"FallbackPrimary", "FallbackSecondary", "FallbackTertiary"}

			const runs = 6
			prevIndex := -1
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

				selected := resp.Headers.Get(testutil.HeaderBAMLRoundRobinSelected)
				indexStr := resp.Headers.Get(testutil.HeaderBAMLRoundRobinIndex)
				idx, convErr := strconv.Atoi(indexStr)
				if convErr != nil {
					t.Fatalf("Call %d: Index header %q not an integer: %v", i, indexStr, convErr)
				}
				if idx < 0 || idx >= len(children) {
					t.Fatalf("Call %d: Index %d out of range [0, %d)", i, idx, len(children))
				}
				if children[idx] != selected {
					t.Errorf("Call %d: Selected=%q but children[%d]=%q", i, selected, idx, children[idx])
				}
				// Modulo progression: each request must advance the
				// counter by exactly one. A stuck counter or a sweep
				// re-advance would manifest as a skipped or repeated
				// index here.
				if prevIndex >= 0 {
					wantIdx := (prevIndex + 1) % len(children)
					if idx != wantIdx {
						t.Errorf("Call %d: expected Index=%d (prev=%d, step=+1 mod %d), got %d",
							i, wantIdx, prevIndex, len(children), idx)
					}
				}
				prevIndex = idx
			}

			assertBalanced(t, []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"}, runs)
		})
	})
}

// ============================================================
// /call-with-raw endpoint — round-robin tests
// ============================================================

func TestRoundRobinCallWithRaw(t *testing.T) {
	skipIfNoBuildRequest(t)
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
				} else {
					// Pin the header consistency invariant: Selected
					// must equal Children[Index]. The chain order
					// matches TestRoundRobinPair's strategy in
					// clients.baml (`[FallbackPrimary,
					// FallbackSecondary]`). A range-only check would
					// accept a swapped-header bug where Selected and
					// Index pointed at different children.
					children := []string{"FallbackPrimary", "FallbackSecondary"}
					if got := children[idx]; got != selected {
						t.Errorf("CallWithRaw %d: Selected=%q but Index=%d points at %q (header desync)", i, selected, idx, got)
					}
				}
				seenIndices[indexStr] = true

				// Identity cross-check: the raw payload carries the
				// scenario id of whichever child actually served this
				// request. Assert the header's Selected names that
				// same child. A metadata/dispatch desync — header
				// reports FallbackPrimary while BAML routed to
				// FallbackSecondary — would fail here, while the
				// per-child hit counts in assertBalanced below would
				// still pass if the overall distribution happened to
				// balance out.
				actualChild := clientForContent(resp.Raw)
				if actualChild == "" {
					t.Errorf("CallWithRaw %d: raw %q doesn't identify a known child", i, resp.Raw)
				} else if actualChild != selected {
					t.Errorf("CallWithRaw %d: raw was served by %q but Selected header says %q", i, actualChild, selected)
				}
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
	skipIfNoBuildRequest(t)

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

		// runs=4 covers at least one wrap-around on a 2-child RR:
		// indices must step 0,1,0,1 or 1,0,1,0. runs=2 was too short
		// to catch a counter stuck at its initial offset — the two
		// observed indices could be adjacent by chance without the
		// counter ever completing a cycle.
		const runs = 4
		seenFinals := map[string]bool{}
		prevIndex := -1
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

			// Identity cross-check: the mock's streamed content contains
			// the scenario id, so the unmarshalled final string names
			// the child that actually served this stream. Assert the
			// planned-metadata Selected matches — a desync between what
			// we report and what BAML routed to would fail here.
			actualChild := clientForContent(result)
			if actualChild == "" {
				t.Errorf("Stream %d: final %q doesn't identify a known child", i, result)
			} else if actualChild != tracker.planned.RoundRobin.Selected {
				t.Errorf("Stream %d: final was served by %q but RoundRobin.Selected says %q", i, actualChild, tracker.planned.RoundRobin.Selected)
			}

			// Modulo progression: each stream must advance the counter
			// by exactly one. A stuck counter or a sweep-induced re-
			// advance would show up as a skipped or repeated index.
			// Validated against the observed Children list from the
			// first iteration's metadata, which equality-check above
			// already asserts matches the expected order.
			if prevIndex >= 0 {
				wantIdx := (prevIndex + 1) % len(tracker.planned.RoundRobin.Children)
				if tracker.planned.RoundRobin.Index != wantIdx {
					t.Errorf("Stream %d: expected Index=%d (prev=%d, step=+1 mod %d), got %d",
						i, wantIdx, prevIndex, len(tracker.planned.RoundRobin.Children), tracker.planned.RoundRobin.Index)
				}
			}
			prevIndex = tracker.planned.RoundRobin.Index
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
	skipIfNoBuildRequest(t)

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

		// runs=4 covers at least one wrap-around on a 2-child RR —
		// indices must step 0,1,0,1 or 1,0,1,0. runs=2 was too short
		// to catch a counter stuck at its initial offset. Matches
		// what /stream already asserts after CR-26.
		const runs = 4
		seenRaws := map[string]bool{}
		prevIndex := -1
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

			// Full RR metadata consistency check — mirrors the /stream
			// test. Without the full comparison a regression that drops
			// Children, skews Index, or desyncs Selected from Children
			// on the raw path alone would slip through.
			if tracker.planned == nil || tracker.planned.RoundRobin == nil {
				t.Fatalf("StreamWithRaw %d: planned metadata missing RoundRobin info: %+v", i, tracker.planned)
			}
			if tracker.planned.RoundRobin.Name != "TestRoundRobinPair" {
				t.Errorf("StreamWithRaw %d: RoundRobin.Name = %q, want TestRoundRobinPair", i, tracker.planned.RoundRobin.Name)
			}
			if got := tracker.planned.RoundRobin.Selected; got == "" {
				t.Errorf("StreamWithRaw %d: RoundRobin.Selected is empty", i)
			}
			wantChildren := []string{"FallbackPrimary", "FallbackSecondary"}
			if !equalSliceStrings(tracker.planned.RoundRobin.Children, wantChildren) {
				t.Errorf("StreamWithRaw %d: RoundRobin.Children = %v, want %v", i, tracker.planned.RoundRobin.Children, wantChildren)
			}
			if idx := tracker.planned.RoundRobin.Index; idx < 0 || idx >= len(wantChildren) {
				t.Errorf("StreamWithRaw %d: RoundRobin.Index %d out of range", i, idx)
			} else if wantChildren[idx] != tracker.planned.RoundRobin.Selected {
				t.Errorf("StreamWithRaw %d: Selected=%q does not match Children[%d]=%q", i, tracker.planned.RoundRobin.Selected, idx, wantChildren[idx])
			}

			// Identity cross-check: the final raw payload embeds the
			// scenario id of whichever child actually served this
			// stream. Assert planned-metadata Selected matches — a
			// dispatch/metadata desync would fail here while the
			// distinct-payloads and balanced-hit-count checks above
			// would still pass.
			actualChild := clientForContent(finalRaw)
			if actualChild == "" {
				t.Errorf("StreamWithRaw %d: finalRaw %q doesn't identify a known child", i, finalRaw)
			} else if actualChild != tracker.planned.RoundRobin.Selected {
				t.Errorf("StreamWithRaw %d: raw was served by %q but RoundRobin.Selected says %q", i, actualChild, tracker.planned.RoundRobin.Selected)
			}

			// Modulo progression: each raw stream must advance the
			// counter by exactly one. Matches the /stream test's
			// CR-26 check — a stuck or double-advanced counter would
			// manifest as a skipped or repeated index here even when
			// the distinct-payloads assertion below still passed.
			if prevIndex >= 0 {
				wantIdx := (prevIndex + 1) % len(tracker.planned.RoundRobin.Children)
				if tracker.planned.RoundRobin.Index != wantIdx {
					t.Errorf("StreamWithRaw %d: expected Index=%d (prev=%d, step=+1 mod %d), got %d",
						i, wantIdx, prevIndex, len(tracker.planned.RoundRobin.Children), tracker.planned.RoundRobin.Index)
				}
			}
			prevIndex = tracker.planned.RoundRobin.Index
		}

		if len(seenRaws) != 2 {
			t.Errorf("expected 2 distinct raw payloads across %d runs, got %d (%v)", runs, len(seenRaws), seenRaws)
		}

		assertBalanced(t, []string{"fallback-primary", "fallback-secondary"}, runs)
	})
}
