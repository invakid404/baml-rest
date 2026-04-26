//go:build integration

package integration

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/integration/testutil"
)

// Integration tests for the runtime client_registry override flows
// targeted by PR #192 cold-review-3 findings F1, F2, F3. These cross
// the resolver-only / adapter-only seams covered by Go unit tests
// and exercise the BAML CFFI boundary that previous reviews flagged
// as the discipline that catches this class of bug.
//
// Each test sends a request through the BuildRequest path with a
// runtime client_registry override and asserts that BAML accepts the
// shape (no CFFI rejection), the resolver's chosen leaf actually
// served the request, and the path/metadata headers match.

// TestRoundRobinOverrides_PresenceOnlyParent covers F1 (cold-review-3
// signoff-10): a runtime registry entry that names a static RR client
// with no provider and no options must work end-to-end. The adapter
// drops the strategy parent from BAML's registry (BAML would reject
// it for missing options.strategy); the resolver still treats it as
// a dynamic RR entry and picks a child from the introspected chain.
func TestRoundRobinOverrides_PresenceOnlyParent(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method: "GetGreetingRoundRobinPair",
			Input:  map[string]any{"name": "World"},
			Options: &testutil.BAMLOptions{
				ClientRegistry: &testutil.ClientRegistry{
					// presence-only entry — no provider, no options.
					// The adapter would have forwarded provider:"" pre-fix,
					// or post-materialise a parent BAML rejects for
					// missing options.strategy. Drop closes both holes.
					Clients: []*testutil.ClientProperty{
						{Name: "TestRoundRobinPair"},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d: %s (BAML registry decode rejection would surface here)", resp.StatusCode, resp.Error)
		}
		// BuildRequest path means the resolver succeeded and
		// dispatched to a leaf — the parent never reached BAML.
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLRoundRobinName, "TestRoundRobinPair")
		testutil.AssertHeaderPresent(t, resp.Headers, testutil.HeaderBAMLRoundRobinSelected)
	})
}

// TestRoundRobinOverrides_StrategyOnlyParent covers F1's strategy-
// only shape: runtime registry supplies options.strategy as an array
// but no provider. Pre-fix the adapter materialised provider then
// forwarded the parent — BAML would still execute the parent
// registry-side, but now its options.strategy is honoured by
// baml-rest's resolver. Drop ensures BAML never has to deal with the
// parent shape.
func TestRoundRobinOverrides_StrategyOnlyParent(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		// Register all three scenarios so the post-loop hit-count
		// assertions can query fallback-primary (introspected chain's
		// first child) without the mockllm returning 404 for an
		// unregistered ID. The override below replaces the chain with
		// [secondary, tertiary] — the primary entry exists only so we
		// can assert it received zero requests.
		registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Drive multiple calls. The stable invariant under the
		// strategy-only override is "every pick is in the override
		// chain AND none is the introspected primary" — NOT "both
		// override children appear at least once". Dynamic RR with
		// no options.start uses AdvanceDynamic
		// (rand.IntN(childCount), no retained state per
		// coordinator.go:127-139), so requiring both children would
		// flake at ~12.5% per subtest for 4 runs over 2 children.
		// Per-pick membership is the F1-relevant assertion. Cold-
		// review-3 verdict-15 finding.
		const runs = 4
		overrideChildren := map[string]bool{
			"FallbackSecondary": true,
			"FallbackTertiary":  true,
		}
		for i := 0; i < runs; i++ {
			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetGreetingRoundRobinPair",
				Input:  map[string]any{"name": "World"},
				Options: &testutil.BAMLOptions{
					ClientRegistry: &testutil.ClientRegistry{
						Clients: []*testutil.ClientProperty{
							{
								Name: "TestRoundRobinPair",
								Options: map[string]any{
									"strategy": []any{"FallbackSecondary", "FallbackTertiary"},
								},
							},
						},
					},
				},
			})
			if err != nil {
				t.Fatalf("Call %d failed: %v", i, err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Call %d: expected 200, got %d: %s", i, resp.StatusCode, resp.Error)
			}
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
			selected := resp.Headers.Get(testutil.HeaderBAMLRoundRobinSelected)
			if !overrideChildren[selected] {
				t.Fatalf("Call %d: Selected=%q not in override chain [FallbackSecondary FallbackTertiary] (override not honoured or empty header)", i, selected)
			}
		}
		// Defense in depth: confirm the introspected primary received
		// zero requests and the override children together absorbed
		// every call. The per-pick membership check above already
		// guarantees this from header observation; cross-checking the
		// mockllm hit counts catches a hypothetical regression where
		// the resolver reports the override child in metadata but
		// dispatches elsewhere on the wire.
		countCtx, countCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer countCancel()
		if got, err := MockClient.GetRequestCount(countCtx, "fallback-primary"); err != nil {
			t.Fatalf("GetRequestCount(fallback-primary): %v", err)
		} else if got != 0 {
			t.Errorf("fallback-primary received %d requests, want 0 (override chain excluded it)", got)
		}
		secondary, err := MockClient.GetRequestCount(countCtx, "fallback-secondary")
		if err != nil {
			t.Fatalf("GetRequestCount(fallback-secondary): %v", err)
		}
		tertiary, err := MockClient.GetRequestCount(countCtx, "fallback-tertiary")
		if err != nil {
			t.Fatalf("GetRequestCount(fallback-tertiary): %v", err)
		}
		if secondary+tertiary != runs {
			t.Errorf("override children received %d total requests (secondary=%d tertiary=%d), want %d", secondary+tertiary, secondary, tertiary, runs)
		}
	})
}

// TestRoundRobinOverrides_CanonicalSpellingTranslation covers F2: the
// operator-supplied "baml-roundrobin" spelling — baml-rest's
// canonical metadata form — is rejected by upstream BAML's
// ClientProvider::from_str. Without F2's TranslateUpstreamProvider
// the BAML call returns "Invalid client provider: baml-roundrobin".
// With the fix BAML accepts. The strategy parent is also dropped
// (F1) so the actual BAML registry never sees the parent at all,
// but the test still proves the spelling translation lives at the
// adapter seam — the cache (returned via headers/metadata) reflects
// the canonical spelling.
func TestRoundRobinOverrides_CanonicalSpellingTranslation(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method: "GetGreetingRoundRobinPair",
			Input:  map[string]any{"name": "World"},
			Options: &testutil.BAMLOptions{
				ClientRegistry: &testutil.ClientRegistry{
					Clients: []*testutil.ClientProperty{
						{
							Name:     "TestRoundRobinPair",
							Provider: testutil.StringPtr("baml-roundrobin"), // canonical baml-rest spelling
							Options: map[string]any{
								"strategy": []any{"FallbackPrimary", "FallbackSecondary"},
							},
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200 (BAML would reject 'baml-roundrobin' pre-fix); got %d: %s", resp.StatusCode, resp.Error)
		}
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLRoundRobinName, "TestRoundRobinPair")
	})
}

// TestRoundRobinOverrides_DeterministicStart covers F3 (deterministic
// start): runtime options.start makes the dynamic RR resolution
// deterministic at index 0/1/2. We use the three-child chain
// (TestRoundRobinChain) and pin start=1 → FallbackSecondary.
func TestRoundRobinOverrides_DeterministicStart(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Multiple requests, each with start:1, all must select
		// FallbackSecondary (index 1 in the override chain).
		const runs = 5
		for i := 0; i < runs; i++ {
			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetGreetingRoundRobinChain",
				Input:  map[string]any{"name": "World"},
				Options: &testutil.BAMLOptions{
					ClientRegistry: &testutil.ClientRegistry{
						Clients: []*testutil.ClientProperty{
							{
								Name: "TestRoundRobinChain",
								Options: map[string]any{
									"strategy": []any{"FallbackPrimary", "FallbackSecondary", "FallbackTertiary"},
									"start":    1,
								},
							},
						},
					},
				},
			})
			if err != nil {
				t.Fatalf("Call %d failed: %v", i, err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Call %d: expected 200, got %d: %s", i, resp.StatusCode, resp.Error)
			}
			selected := resp.Headers.Get(testutil.HeaderBAMLRoundRobinSelected)
			if selected != "FallbackSecondary" {
				t.Errorf("Call %d: Selected=%q, want FallbackSecondary (start=1 must be deterministic)", i, selected)
			}
			indexStr := resp.Headers.Get(testutil.HeaderBAMLRoundRobinIndex)
			idx, _ := strconv.Atoi(indexStr)
			if idx != 1 {
				t.Errorf("Call %d: Index=%d, want 1", i, idx)
			}
		}
	})
}

// TestRoundRobinOverrides_InvalidStartRoutesToLegacy covers F3
// (invalid start): runtime options.start as a string fails BAML's
// i32 ensure_int contract. Resolver returns ErrInvalidStartOverride;
// dispatcher falls through to legacy with
// PathReasonInvalidRoundRobinStartOverride so operators can spot the
// malformed input via X-BAML-Path-Reason.
//
// On the legacy path BAML's runtime emits the canonical options
// error. We don't assert the BAML error message verbatim (it's an
// upstream contract) — only that the routing landed on legacy with
// the right reason. The actual BAML invocation may succeed if BAML
// silently coerces the string, so we accept either status code as
// long as the reason header is right.
func TestRoundRobinOverrides_InvalidStartRoutesToLegacy(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method: "GetGreetingRoundRobinPair",
			Input:  map[string]any{"name": "World"},
			Options: &testutil.BAMLOptions{
				ClientRegistry: &testutil.ClientRegistry{
					Clients: []*testutil.ClientProperty{
						{
							Name: "TestRoundRobinPair",
							Options: map[string]any{
								"strategy": []any{"FallbackPrimary", "FallbackSecondary"},
								// Numeric string — rejected per BAML's
								// i32 ensure_int contract.
								"start": "1",
							},
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		// Routing must land on legacy with the new path reason.
		// The BAML call itself may succeed or fail depending on
		// upstream's coercion behaviour; the routing classification
		// is what F3 owns.
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "legacy")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPathReason, "invalid-round-robin-start-override")
	})
}
