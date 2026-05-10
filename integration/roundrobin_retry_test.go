//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// ============================================================
// baml-roundrobin wrapper retry_policy integration test
//
// Pins that the wrapper's retry_policy is honored even after
// ResolveEffectiveClient unwraps the strategy to a leaf for
// dispatch. Pre-fix the codegen keyed retry on the post-unwrap
// leaf and silently dropped any retry_policy declared on the RR
// wrapper itself; BAML's runtime
// (LLMStrategyProvider::WithRetryPolicy) actually applies it
// AROUND the strategy iteration, so the dropped policy was a
// genuine semantic regression on the v0.219+ BuildRequest path.
//
// The test deliberately decouples its assertion from baml-rest's
// once-per-REST-request RR cadence: it counts TOTAL hits across
// the leaves rather than asserting that each retry hits a
// different leaf. baml-rest intentionally diverges from upstream
// here (advancing the RR coordinator once per request rather
// than once per attempt); locking either rotation pattern into
// the assertion would couple this fix to the separate cadence-
// alignment work tracked in #167.
// ============================================================

func TestRoundRobinWrapperRetry(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		t.Run("wrapper_retry_policy_applies_after_rr_unwrap", func(t *testing.T) {
			if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
				t.Skip("Skipping: BuildRequest RR unwrap requires BAML >= 0.219.0")
			}
			waitForHealthy(t, 30*time.Second)
			clearRoundRobinScenarios(t)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			// Both leaves fail every request. The wrapper has
			// retry_policy max_retries=2 (3 attempts total). With
			// the fix, the wrapper's policy applies despite the
			// RR-unwrap reassigning __effective to a leaf with no
			// retry_policy of its own. Without the fix the leaf
			// is keyed for retry resolution and yields no retries
			// → 1 hit total, 1 attempt, request fails immediately.
			for _, id := range []string{"fallback-primary", "fallback-secondary"} {
				registerRoundRobinScenario(t, &mockllm.Scenario{
					ID:          id,
					Provider:    "openai",
					Content:     "unused",
					FailAfter:   1,
					FailureMode: "500",
				})
			}

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetGreetingRoundRobinWithRetry",
				Input:  map[string]any{"name": "World"},
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			// All RR children fail every attempt; baml-rest must
			// surface the error rather than 200ing.
			if resp.StatusCode == 200 {
				t.Fatalf("Expected non-200 status when all RR children fail, got 200")
			}

			// Total hits == 1 initial + 2 retries from the
			// wrapper's retry_policy (max_retries=2). A regression
			// to leaf-keyed retry resolution would surface as
			// total=1. We sum across both leaves rather than
			// asserting a per-leaf count to remain agnostic to
			// baml-rest's once-per-REST-request RR cadence (an
			// intentional divergence from upstream's per-attempt
			// rotation, tracked separately in #167).
			ctxCount, cancelCount := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCount()
			primaryHits, err := MockClient.GetRequestCount(ctxCount, "fallback-primary")
			if err != nil {
				t.Fatalf("Failed to get primary hit count: %v", err)
			}
			secondaryHits, err := MockClient.GetRequestCount(ctxCount, "fallback-secondary")
			if err != nil {
				t.Fatalf("Failed to get secondary hit count: %v", err)
			}
			total := primaryHits + secondaryHits
			if total != 3 {
				t.Errorf("Expected 3 total hits across RR leaves (1 initial + 2 retries from wrapper retry_policy max_retries=2), got %d (primary=%d, secondary=%d). Total of 1 indicates the wrapper's retry_policy was dropped by leaf-keyed retry resolution.", total, primaryHits, secondaryHits)
			}

			// X-BAML-Retry-Max must reflect the wrapper's
			// retry_policy. This is a direct positive check on
			// the metadata pipeline: even if the orchestrator
			// somehow attempted 3 calls without honoring the
			// wrapper's policy, the header would still expose the
			// regression.
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLRetryMax, "2")

			// X-BAML-RoundRobin-Name pins the strategy identity
			// on the response — the planned-metadata pipeline
			// must still report the wrapper as the routing
			// origin, even though dispatch landed on a leaf.
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLRoundRobinName, "TestRoundRobinWithRetry")
		})
	})
}
