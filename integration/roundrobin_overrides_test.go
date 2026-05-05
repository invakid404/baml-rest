//go:build integration

package integration

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/integration/testutil"
)

// Integration tests for the runtime client_registry override flows.
// These cross the resolver-only / adapter-only seams covered by Go
// unit tests and exercise the BAML CFFI boundary as the discipline
// that catches this class of bug.
//
// Each test sends a request through the BuildRequest path with a
// runtime client_registry override and asserts that BAML accepts the
// shape (no CFFI rejection), the resolver's chosen leaf actually
// served the request, and the path/metadata headers match.

// TestRoundRobinOverrides_PresenceOnlyParent pins that a runtime
// registry entry naming a static RR client with no provider and no
// options must work end-to-end. The adapter drops the strategy parent
// from BAML's registry (BAML would reject it for missing
// options.strategy); the resolver still treats it as a dynamic RR
// entry and picks a child from the introspected chain.
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
					// Pin Primary to present-empty (`"primary":""` in
					// the JSON payload) to preserve integration coverage
					// of the BuildRequest gate's present-empty no-op
					// contract — the adapter must NOT propagate "" to
					// BAML's SetPrimaryClient or PromptRenderer would
					// fail on the empty-name lookup. Other tests in this
					// file leave Primary nil (omitted) since they don't
					// intend to assert anything about it.
					Primary: testutil.StringPtr(""),
					// presence-only entry — no provider, no options.
					// Without dropping the parent, the adapter would
					// forward provider:"" or materialise a parent BAML
					// rejects for missing options.strategy. The drop
					// closes both holes.
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

// TestRoundRobinOverrides_StrategyOnlyParent pins the strategy-only
// runtime override shape: runtime registry supplies options.strategy
// as an array but no provider. The adapter must drop the parent so
// BAML never parses the parent shape, while baml-rest's resolver
// honors options.strategy.
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

		// Drive multiple calls. The stable invariant under the
		// strategy-only override is "every pick is in the override
		// chain AND none is the introspected primary" — NOT "both
		// override children appear at least once". Dynamic RR with
		// no options.start uses AdvanceDynamic (rand.IntN(childCount),
		// no retained state), so requiring both children would flake
		// at ~12.5% per subtest for 4 runs over 2 children. Per-pick
		// membership is the relevant assertion.
		//
		// Per-iteration context: a single shared deadline across
		// every call would let a slow first call consume budget
		// later calls inherit, so a deadline-induced failure could
		// look like an application regression. Fresh deadline per
		// iteration isolates that.
		const runs = 4
		overrideChildren := map[string]bool{
			"FallbackSecondary": true,
			"FallbackTertiary":  true,
		}
		for i := 0; i < runs; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			cancel()
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

// TestRoundRobinOverrides_CanonicalSpellingRecognized pins
// BuildRequest-path recognition of the canonical "baml-roundrobin"
// spelling: an operator-supplied parent provider in the canonical
// form must drive baml-rest's resolver (parent dropped, leaf
// dispatched, RR headers populated) without any upstream BAML error.
//
// This test does NOT prove the upstream-translation seam — the
// BuildRequest-safe registry view drops the strategy parent before
// BAML parses anything, so BAML never sees "baml-roundrobin" via this
// path. The TranslateUpstreamProvider seam (which folds aliases for
// the LEGACY-bound registry) is covered by adapter-level unit tests
// and translation-table tests in bamlutils.
func TestRoundRobinOverrides_CanonicalSpellingRecognized(t *testing.T) {
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
			t.Fatalf("expected canonical spelling recognition without BAML rejecting 'baml-roundrobin'; got %d: %s", resp.StatusCode, resp.Error)
		}
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLRoundRobinName, "TestRoundRobinPair")
	})
}

// TestRoundRobinOverrides_DeterministicStart pins deterministic
// start: runtime options.start makes the dynamic RR resolution
// deterministic at index 0/1/2. We use the three-child chain
// (TestRoundRobinChain) and pin start=1 → FallbackSecondary.
func TestRoundRobinOverrides_DeterministicStart(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"})

		// Multiple requests, each with start:1, all must select
		// FallbackSecondary (index 1 in the override chain). Fresh
		// per-iteration deadline; see
		// TestRoundRobinOverrides_StrategyOnlyParent above for the
		// rationale.
		const runs = 5
		for i := 0; i < runs; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			cancel()
			if err != nil {
				t.Fatalf("Call %d failed: %v", i, err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Call %d: expected 200, got %d: %s", i, resp.StatusCode, resp.Error)
			}
			// Pin the routing path before checking the deterministic
			// selection — a regression that fell back to legacy could
			// still emit headers but the start=1 contract is a
			// BuildRequest-path-only guarantee.
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
			selected := resp.Headers.Get(testutil.HeaderBAMLRoundRobinSelected)
			if selected != "FallbackSecondary" {
				t.Errorf("Call %d: Selected=%q, want FallbackSecondary (start=1 must be deterministic)", i, selected)
			}
			// Fail loudly on missing/malformed RR-Index header rather
			// than swallowing the parse error and silently using the
			// zero-value, which would surface a wrong-index regression
			// only when the expected index happened to be 0.
			indexStr := resp.Headers.Get(testutil.HeaderBAMLRoundRobinIndex)
			if indexStr == "" {
				t.Fatalf("Call %d: %s header absent (deterministic start should always emit it)", i, testutil.HeaderBAMLRoundRobinIndex)
			}
			idx, err := strconv.Atoi(indexStr)
			if err != nil {
				t.Fatalf("Call %d: %s header %q failed to parse as int: %v", i, testutil.HeaderBAMLRoundRobinIndex, indexStr, err)
			}
			if idx != 1 {
				t.Errorf("Call %d: Index=%d, want 1", i, idx)
			}
		}
	})
}

// TestRoundRobinOverrides_InvalidStartRoutesToLegacy pins that a
// runtime options.start as a string fails BAML's i32 ensure_int
// contract: resolver returns ErrInvalidStartOverride; dispatcher
// falls through to legacy with
// PathReasonInvalidRoundRobinStartOverride so operators can spot the
// malformed input via X-BAML-Path-Reason.
//
// Header-only assertion (path + path-reason). The body-level
// invariant (BAML emits its canonical ensure_int error rather than
// silently using static config) is asserted by
// TestInvalidRuntimeRoundRobinStartReturnsLegacyError below.
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
		if resp.StatusCode == 200 {
			t.Fatalf("expected non-200 from BAML's canonical start error; got 200 (regression — invalid-start parent silently used static config). Body: %s", string(resp.Body))
		}
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "legacy")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPathReason, "invalid-round-robin-start-override")
	})
}

// TestRoundRobinOverrides_ExplicitProviderWithoutStrategyRoutesToLegacy
// pins the missing-strategy contract end-to-end: a runtime
// client_registry entry that supplies an explicit RR provider but
// no `options.strategy` must short-circuit to top-level legacy
// with PathReasonInvalidStrategyOverride. Falling back to the
// introspected chain would silently dispatch a static child while
// BAML's eager parse rejects the missing-strategy shape — the
// validation suppression class the resolver's contract rules out.
//
// Asserts both the path/path-reason headers (the observability
// surface operators actually consume) and a non-200 status (the
// load-bearing body-level signal that BAML emitted its canonical
// ensure_strategy error rather than serving from static config).
func TestRoundRobinOverrides_ExplicitProviderWithoutStrategyRoutesToLegacy(t *testing.T) {
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
						// Explicit RR provider on TestRoundRobinPair
						// (a static RR client) with no
						// options.strategy. The introspected chain
						// for TestRoundRobinPair is non-empty —
						// the resolver's contract is that the
						// explicit-provider override preempts the
						// static-chain fallback regardless, so the
						// request routes to top-level legacy.
						{
							Name:     "TestRoundRobinPair",
							Provider: testutil.StringPtr("round-robin"),
							Options:  map[string]any{},
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		// BAML's eager parse should reject the missing-strategy
		// shape with its canonical ensure_strategy error. A 200
		// here means the legacy fallthrough still drove dispatch
		// to a static child despite the routing path-reason
		// header reporting invalid-strategy-override — the
		// validation suppression class this verdict closed.
		// Without this assertion a regression that emitted the
		// observability headers but still served from static
		// config could pass.
		if resp.StatusCode == 200 {
			t.Fatalf("expected non-200 from BAML's canonical missing-strategy error; got 200. Body: %s", string(resp.Body))
		}
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "legacy")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPathReason, "invalid-strategy-override")
	})
}

// TestInvalidRuntimeRoundRobinStartReturnsLegacyError is the body-
// level companion to TestRoundRobinOverrides_InvalidStartRoutesToLegacy.
//
// The legacy registry view preserves explicit `start` overrides on
// RR parents, so BAML's `ensure_int` rejects a non-integer value and
// surfaces its canonical error. The non-200 status is the load-
// bearing assertion: a registry view that dropped invalid-shape
// parents would let BAML silently fall through to static config
// (which has no start override) and return 200, with headers saying
// one thing and body another. The serve layer wraps the underlying
// BAML message with a generic "failed to process request" error
// response, so we don't pin the upstream wording in the body — the
// path-reason header is the observability surface.
func TestInvalidRuntimeRoundRobinStartReturnsLegacyError(t *testing.T) {
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
							Provider: testutil.StringPtr("round-robin"),
							Options: map[string]any{
								"strategy": []any{"FallbackPrimary", "FallbackSecondary"},
								// Numeric string — rejected by BAML's
								// ensure_int (helpers.rs:917-940 only
								// accepts numeric values).
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
		if resp.StatusCode == 200 {
			t.Fatalf("expected non-200 from BAML's canonical start error; got 200 (regression — invalid-start parent silently used static config). Body: %s", string(resp.Body))
		}
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "legacy")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPathReason, "invalid-round-robin-start-override")
	})
}

// TestLegacyModeHonorsRuntimeFallbackStrategyOverride pins that when
// a request falls through to legacy (UseBuildRequest=false, or BR-
// unsupported children), the legacy registry view preserves explicit
// fallback-parent overrides. BAML must execute the runtime chain and
// the static children are NEVER hit. We swap the parent's strategy
// to point at a fully-dynamic child (RuntimePrimary, defined here in
// the registry only) pointing at the tertiary mock scenario, so a
// hit on tertiary proves the runtime override was honoured end-to-end.
//
// Skipped on the BuildRequest-mode CI leg because this test
// specifically exercises legacy dispatch — it relies on the existing
// TestEnv being legacy-mode.
func TestLegacyModeHonorsRuntimeFallbackStrategyOverride(t *testing.T) {
	if ActuallyBuildRequest() {
		t.Skip("legacy-mode regression test; relies on TestEnv being UseBuildRequest=false")
	}
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Override TestFallbackPair (statically [primary, secondary])
		// with a dynamic chain [RuntimePrimary], where RuntimePrimary
		// is a runtime-only leaf pointing at the tertiary scenario.
		// A hit on tertiary proves the override took effect; a hit on
		// primary/secondary means the static chain silently won. The
		// explicit Primary forces BAML's PromptRenderer to use the
		// override target rather than the function's static client
		// declaration — when both name a strategy parent, the runtime
		// version (with the override chain) wins via ctx.client_overrides.
		resp, err := client.Call(ctx, testutil.CallRequest{
			Method: "GetGreetingFallbackPair",
			Input:  map[string]any{"name": "World"},
			Options: &testutil.BAMLOptions{
				ClientRegistry: &testutil.ClientRegistry{
					Primary: testutil.StringPtr("TestFallbackPair"),
					Clients: []*testutil.ClientProperty{
						{
							Name:     "TestFallbackPair",
							Provider: testutil.StringPtr("baml-fallback"),
							Options: map[string]any{
								"strategy": []any{"RuntimePrimary"},
							},
						},
						{
							Name:     "RuntimePrimary",
							Provider: testutil.StringPtr("openai"),
							Options: map[string]any{
								"model":    "fallback-tertiary",
								"base_url": TestEnv.MockLLMInternal + "/v1",
								"api_key":  "test-key",
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
			t.Fatalf("expected 200 (override should resolve to RuntimePrimary→tertiary); got %d body=%s err=%s", resp.StatusCode, string(resp.Body), resp.Error)
		}
		// Mock hit counts: tertiary served the request, primary +
		// secondary never saw it. This is the load-bearing assertion —
		// without the legacy registry view preserving explicit
		// strategy-parent overrides, the override would be dropped from
		// BAML's view and the static [primary, secondary] chain would
		// handle the request silently.
		mockCtx, mockCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer mockCancel()
		primaryHits, err := MockClient.GetRequestCount(mockCtx, "fallback-primary")
		if err != nil {
			t.Fatalf("GetRequestCount fallback-primary: %v", err)
		}
		secondaryHits, err := MockClient.GetRequestCount(mockCtx, "fallback-secondary")
		if err != nil {
			t.Fatalf("GetRequestCount fallback-secondary: %v", err)
		}
		tertiaryHits, err := MockClient.GetRequestCount(mockCtx, "fallback-tertiary")
		if err != nil {
			t.Fatalf("GetRequestCount fallback-tertiary: %v", err)
		}
		if primaryHits != 0 {
			t.Errorf("static primary received %d hits; runtime fallback override was dropped", primaryHits)
		}
		if secondaryHits != 0 {
			t.Errorf("static secondary received %d hits; runtime fallback override was dropped", secondaryHits)
		}
		if tertiaryHits == 0 {
			t.Errorf("RuntimePrimary→tertiary received 0 hits; runtime override never dispatched")
		}
	})
}

// TestLegacyModeSupportsDynamicFallbackPrimary covers the purely-
// dynamic strategy parent on legacy fallthrough. Pre-Option-C the
// adapter dropped the runtime parent from BAML's view, so
// `WithClient(parent)` couldn't find it (no static definition either)
// and the request errored with client-not-found. Post-fix the legacy
// view preserves the parent and BAML executes the runtime chain.
//
// Uses GetSimple (which by default hits TestClient — a leaf client we
// override here so the request is fully driven by runtime
// definitions). The runtime registry defines a fresh fallback parent
// pointing at FallbackPrimary, with Primary set to the parent name —
// BAML resolves the primary, walks the chain, hits FallbackPrimary.
//
// Skipped on the BuildRequest-mode CI leg.
func TestLegacyModeSupportsDynamicFallbackPrimary(t *testing.T) {
	if ActuallyBuildRequest() {
		t.Skip("legacy-mode regression test; relies on TestEnv being UseBuildRequest=false")
	}
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		registerAllGreetingScenarios(t, []string{"fallback-primary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method: "GetGreeting",
			Input:  map[string]any{"name": "World"},
			Options: &testutil.BAMLOptions{
				ClientRegistry: &testutil.ClientRegistry{
					Primary: testutil.StringPtr("DynamicFallback"),
					Clients: []*testutil.ClientProperty{
						{
							Name:     "DynamicFallback",
							Provider: testutil.StringPtr("baml-fallback"),
							Options: map[string]any{
								"strategy": []any{"DynamicLeaf"},
							},
						},
						{
							Name:     "DynamicLeaf",
							Provider: testutil.StringPtr("openai"),
							Options: map[string]any{
								"model":    "fallback-primary",
								"base_url": TestEnv.MockLLMInternal + "/v1",
								"api_key":  "test-key",
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
			t.Fatalf("expected 200 (dynamic fallback parent should resolve to primary leaf); got %d body=%s err=%s", resp.StatusCode, string(resp.Body), resp.Error)
		}
		mockCtx, mockCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer mockCancel()
		primaryHits, err := MockClient.GetRequestCount(mockCtx, "fallback-primary")
		if err != nil {
			t.Fatalf("GetRequestCount fallback-primary: %v", err)
		}
		if primaryHits == 0 {
			t.Errorf("FallbackPrimary received 0 hits; dynamic fallback parent never dispatched (regression — parent was dropped before BAML could resolve it)")
		}
	})
}

// TestLegacyMode_RR_NoCentralization pins that with
// BAML_REST_USE_BUILD_REQUEST=false on a 0.219+ adapter, the flag
// is a full kill switch — baml-rest's centralised RR (resolver,
// in-process coordinator, RemoteAdvancer) must NOT engage, and
// BAML's per-worker runtime rotation owns the strategy. The
// observable contract: the request still succeeds, but the
// X-BAML-RoundRobin-* headers are absent because no baml-rest RR
// resolution happened. ResolveEffectiveClient is gated on
// `supportsWithClient && __useBuildRequest`, so with the flag off
// __rrInfo never gets populated and no RR headers leak.
//
// Skipped on the BuildRequest-mode CI leg — this tests the
// flag-off semantics specifically.
func TestLegacyMode_RR_NoCentralization(t *testing.T) {
	if ActuallyBuildRequest() {
		t.Skip("legacy-mode regression test; relies on TestEnv being UseBuildRequest=false")
	}
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
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
			t.Fatalf("expected 200 (BAML's per-worker RR should still serve the request); got %d body=%s err=%s", resp.StatusCode, string(resp.Body), resp.Error)
		}
		// The smoking-gun assertion: when the flag is off, baml-rest
		// must NOT have populated __rrInfo, so the X-BAML-RoundRobin-*
		// header set is absent. BAML's runtime rotation on each
		// worker handled the request, but its rotation is per-worker
		// and not surfaced through baml-rest's planned metadata.
		for _, name := range []string{
			testutil.HeaderBAMLRoundRobinName,
			testutil.HeaderBAMLRoundRobinSelected,
			testutil.HeaderBAMLRoundRobinIndex,
		} {
			if got := resp.Headers.Get(name); got != "" {
				t.Errorf("flag-off path leaked %s=%q — baml-rest RR engaged when it should be reverted to BAML runtime", name, got)
			}
		}
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "legacy")
	})
}

// TestNestedStrategyOverrides_ValidRRChildHonoured pins the
// per-callback scoped registry contract end-to-end: a fallback chain
// whose nested RR child has a valid runtime override
// (changed-strategy → tenant leaf) must reach BAML inside the mixed-
// mode legacy callback. The mixed-mode legacy child callback uses a
// per-callback scoped registry (makeLegacyChildOptionsFromAdapter)
// rather than the outer BuildRequest-safe options — that scope keeps
// TestRoundRobinPair plus its strategy override visible to BAML so
// BAML executes the runtime-defined leaf instead of the compiled
// static `[FallbackPrimary, FallbackSecondary]`. A regression that
// reused the outer options would silently dispatch primary/secondary
// even though the request changed the RR's strategy at runtime.
//
// The load-bearing assertion is the per-mock hit count: tertiary must
// receive the request, primary/secondary must NOT. Header presence
// alone would not catch the bug since the mixed-mode classification
// header (X-BAML-Path-Reason: fallback-round-robin-child-legacy) is
// emitted whether or not the override actually reached BAML.
func TestNestedStrategyOverrides_ValidRRChildHonoured(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method: "GetGreetingFallbackPair",
			Input:  map[string]any{"name": "World"},
			Options: &testutil.BAMLOptions{
				ClientRegistry: &testutil.ClientRegistry{
					Clients: []*testutil.ClientProperty{
						// Outer fallback re-pointed at the static RR
						// client. Single-child chain keeps the test
						// shape minimal: BAML doesn't iterate sibling
						// fallback children before reaching the RR.
						{
							Name:     "TestFallbackPair",
							Provider: testutil.StringPtr("baml-fallback"),
							Options: map[string]any{
								"strategy": []any{"TestRoundRobinPair"},
							},
						},
						// Nested RR's runtime strategy override —
						// must reach BAML for the test to pass.
						// Without the per-callback scoped registry,
						// this entry is dropped from BAML's view and
						// the compiled `[FallbackPrimary,
						// FallbackSecondary]` runs instead.
						{
							Name:     "TestRoundRobinPair",
							Provider: testutil.StringPtr("round-robin"),
							Options: map[string]any{
								"strategy": []any{"FallbackTertiary"},
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
			t.Fatalf("expected 200 (override should resolve to FallbackTertiary); got %d body=%s err=%s",
				resp.StatusCode, string(resp.Body), resp.Error)
		}

		mockCtx, mockCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer mockCancel()
		primaryHits, err := MockClient.GetRequestCount(mockCtx, "fallback-primary")
		if err != nil {
			t.Fatalf("GetRequestCount fallback-primary: %v", err)
		}
		secondaryHits, err := MockClient.GetRequestCount(mockCtx, "fallback-secondary")
		if err != nil {
			t.Fatalf("GetRequestCount fallback-secondary: %v", err)
		}
		tertiaryHits, err := MockClient.GetRequestCount(mockCtx, "fallback-tertiary")
		if err != nil {
			t.Fatalf("GetRequestCount fallback-tertiary: %v", err)
		}
		if primaryHits != 0 {
			t.Errorf("FallbackPrimary received %d hits; nested RR runtime strategy override was dropped from BAML's view "+
				"(static [FallbackPrimary, FallbackSecondary] silently won)", primaryHits)
		}
		if secondaryHits != 0 {
			t.Errorf("FallbackSecondary received %d hits; nested RR runtime strategy override was dropped from BAML's view "+
				"(static [FallbackPrimary, FallbackSecondary] silently won)", secondaryHits)
		}
		if tertiaryHits == 0 {
			t.Errorf("FallbackTertiary received 0 hits; nested RR runtime strategy override never reached BAML "+
				"(per-callback scoped registry regression)")
		}
	})
}

// TestNestedStrategyOverrides_InvalidStartReturnsCanonicalError pins
// the invalid-nested preflight: a nested RR child with a present-
// but-invalid `start` (string "1" — BAML's ensure_int wants an i32)
// must short-circuit chain resolution to top-level legacy where
// BAML's eager parse rejects the registry and surfaces the canonical
// validation error.
//
// Wiring: ResolveFallbackChainForClientWithReason inspects each
// strategy-parent child for present-but-invalid strategy/start
// overrides; on hit it returns a nil chain with
// PathReasonInvalidRoundRobinStartOverride. The codegen
// `len(chain) > 0` gate skips the BR block, the legacy path's
// metadata classifier carries the same reason out via
// X-BAML-Path-Reason, and BAML's full legacy registry view
// (LegacyClientRegistry) preserves the invalid entry so
// ensure_int rejects upstream. A regression that drops the preflight
// would silently route to mixed-mode where the per-child callback
// drops the invalid entry, BAML uses static config, and the request
// returns 200 — silent validation suppression.
func TestNestedStrategyOverrides_InvalidStartReturnsCanonicalError(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		registerAllGreetingScenarios(t, []string{"fallback-primary", "fallback-secondary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method: "GetGreetingFallbackPair",
			Input:  map[string]any{"name": "World"},
			Options: &testutil.BAMLOptions{
				ClientRegistry: &testutil.ClientRegistry{
					Clients: []*testutil.ClientProperty{
						{
							Name:     "TestFallbackPair",
							Provider: testutil.StringPtr("baml-fallback"),
							Options: map[string]any{
								"strategy": []any{"TestRoundRobinPair"},
							},
						},
						{
							Name:     "TestRoundRobinPair",
							Provider: testutil.StringPtr("round-robin"),
							Options: map[string]any{
								"strategy": []any{"FallbackPrimary", "FallbackSecondary"},
								// Numeric string — rejected by BAML's
								// ensure_int (helpers.rs only accepts
								// numeric values for the i32 option).
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
		if resp.StatusCode == 200 {
			t.Fatalf("expected non-200 from BAML's canonical start error; got 200 (regression — "+
				"invalid nested start silently fell through to static config). Body: %s", string(resp.Body))
		}
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "legacy")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPathReason, "invalid-round-robin-start-override")
	})
}

// TestNestedStrategyOverrides_DynamicNestedFallbackParent pins the
// dynamic-only nested fallback case: a runtime registry defines a
// fresh fallback parent that is NOT compiled in, exposes a runtime
// leaf, and lives only in the request's registry. The per-callback
// scoped registry built by makeLegacyChildOptionsFromAdapter is the
// only seam where BAML can see DynamicNestedFallback inside the
// mixed-mode legacy child callback — the outer BuildRequest-safe
// options drops every resolved strategy parent. A regression that
// reused the outer options would either fail
// WithClient("DynamicNestedFallback") with client-not-found (no
// compiled definition) or silently fall through to a same-named
// compiled lookalike, never executing the runtime chain.
//
// The outer fallback is composed at runtime as
// `[FallbackPrimary, DynamicNestedFallback]` so the chain is mixed-
// mode (one BR-supported child, one strategy-parent child) — the
// `legacyPositions == len(resolvedChain)` short-circuit therefore
// does NOT route the whole request to top-level legacy, and the
// dynamic parent must be resolved through the per-callback scoped
// helper. FallbackPrimary's mock scenario is intentionally not
// registered so its dispatch fails and the chain advances to the
// DynamicNestedFallback child.
func TestNestedStrategyOverrides_DynamicNestedFallbackParent(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearRoundRobinScenarios(t)
		// Only register tertiary so the dynamic-leaf dispatch can
		// succeed; FallbackPrimary's missing scenario forces BAML to
		// advance to the second child in the outer chain.
		registerAllGreetingScenarios(t, []string{"fallback-tertiary"})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method: "GetGreetingFallbackPair",
			Input:  map[string]any{"name": "World"},
			Options: &testutil.BAMLOptions{
				ClientRegistry: &testutil.ClientRegistry{
					Clients: []*testutil.ClientProperty{
						{
							Name:     "TestFallbackPair",
							Provider: testutil.StringPtr("baml-fallback"),
							Options: map[string]any{
								"strategy": []any{"FallbackPrimary", "DynamicNestedFallback"},
							},
						},
						// Dynamic-only nested fallback parent. The
						// scoped registry must include it (transitive
						// reachability from the legacyStreamChildFn
						// callback root) for BAML to resolve
						// WithClient("DynamicNestedFallback").
						{
							Name:     "DynamicNestedFallback",
							Provider: testutil.StringPtr("baml-fallback"),
							Options: map[string]any{
								"strategy": []any{"DynamicNestedLeaf"},
							},
						},
						// Dynamic-only leaf under DynamicNestedFallback.
						// Routed to the tertiary mock scenario; a hit
						// here is the smoking gun that the runtime
						// chain executed end-to-end.
						{
							Name:     "DynamicNestedLeaf",
							Provider: testutil.StringPtr("openai"),
							Options: map[string]any{
								"model":    "fallback-tertiary",
								"base_url": TestEnv.MockLLMInternal + "/v1",
								"api_key":  "test-key",
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
			t.Fatalf("expected 200 from DynamicNestedFallback → DynamicNestedLeaf dispatch; got %d body=%s err=%s",
				resp.StatusCode, string(resp.Body), resp.Error)
		}

		mockCtx, mockCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer mockCancel()
		tertiaryHits, err := MockClient.GetRequestCount(mockCtx, "fallback-tertiary")
		if err != nil {
			t.Fatalf("GetRequestCount fallback-tertiary: %v", err)
		}
		if tertiaryHits == 0 {
			t.Errorf("DynamicNestedLeaf → fallback-tertiary received 0 hits; dynamic-only nested fallback parent " +
				"never reached BAML (regression — runtime parent dropped from per-callback scoped registry)")
		}
	})
}
