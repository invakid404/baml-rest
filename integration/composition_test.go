//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/invakid404/baml-rest/integration/mockllm"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// ============================================================
// fallback[rr[A,B], C] static composition — centralised RR-child
// dispatch through BuildRequest.
//
// The fixture client `TestFallbackRoundRobinPair` in clients.baml
// has shape:
//
//   baml-fallback
//     |__ TestRoundRobinPair (baml-roundrobin)
//     |     |__ FallbackPrimary  (openai)
//     |     |__ FallbackSecondary (openai)
//     |__ FallbackTertiary (openai)
//
// The static composition is BR-eligible: every RR leaf is a non-
// strategy openai client whose provider is BuildRequest-supported.
// The resolver centralises the immediate RR child to one of its
// leaves and dispatches via the BuildRequest path; the same
// SharedState advancer that drives top-level RR also drives this
// rotation, so per-leaf hits balance across pool workers identically
// to a top-level baml-roundrobin client. FallbackTertiary only sees
// traffic if the RR child fails on a given attempt.
//
// Tests deliberately stay agnostic to per-REST-request RR cadence
// (baml-rest advances once per REST request, BAML upstream advances
// per LLM call). Per-leaf rotation order is NOT pinned; per-leaf hit
// counts are summed against the fallback-tertiary baseline so the
// invariant is "both RR leaves saw traffic and tertiary didn't",
// not "leaf X served run N".
// ============================================================

// compositionScenarioIDs lists the mock scenario IDs the composition
// fixture exercises: the two RR-leaf scenarios plus the fallback
// sibling that catches RR-leaf failures.
var compositionScenarioIDs = []string{"fallback-primary", "fallback-secondary", "fallback-tertiary"}

func clearCompositionScenarios(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range compositionScenarioIDs {
		if err := MockClient.DeleteScenario(ctx, id); err != nil {
			t.Fatalf("Failed to delete scenario %q: %v", id, err)
		}
	}
}

// TestFallbackRoundRobinComposition_CentralizedAcrossWorkers exercises
// the static `fallback[rr[A,B], C]` shape end-to-end on the
// BuildRequest path. Each request rotates one of the RR leaves
// through the same SharedState advancer top-level RR uses; over N
// successful requests both leaves see traffic while the fallback
// sibling stays untouched.
//
// Load-bearing assertions:
//
//   - X-BAML-Path is "buildrequest" — RR dispatch must NOT have
//     degraded to the legacy per-worker callback.
//   - X-BAML-Path-Reason is "fallback-roundrobin-child-buildrequest"
//     and never "fallback-roundrobin-child-legacy" — the centralised
//     classification is what the metadata classifier emits, not the
//     deferred-shape legacy reason.
//   - X-BAML-Client names the fallback strategy parent so operators
//     reading headers see the operator-authored client name.
//   - X-BAML-Winner-Client names the actual RR-leaf openai client
//     that served the request (one of FallbackPrimary /
//     FallbackSecondary) — the centralised wrapper-unwrap puts the
//     served leaf on outcome, not the RR wrapper name.
//   - Per-leaf hit counts: across `runs` runs the two RR-leaf scenarios
//     must sum to `runs`, fallback-tertiary must stay at 0. The
//     RR-cadence-agnostic check is the sum-and-zero invariant, not a
//     per-call rotation order.
//   - Streaming metadata event exposes FallbackTargets (wrapper→leaf
//     mapping) and FallbackRoundRobin (RR decision) on the planned
//     phase — the planned-vs-outcome shape the metadata vocabulary
//     commits to.
func TestFallbackRoundRobinComposition_CentralizedAcrossWorkers(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearCompositionScenarios(t)
		registerAllGreetingScenarios(t, compositionScenarioIDs)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Even runs across the 2-leaf RR child so an idealised cadence
		// would balance to (runs/2, runs/2). The RR-cadence-agnostic
		// assertions below tolerate any non-zero distribution as long
		// as both leaves saw traffic and the sum matches; the cadence
		// the BuildRequest path actually uses (once per REST request)
		// would in practice produce strict alternation, but tests don't
		// pin that.
		const runs = 6
		validLeaves := map[string]bool{
			"FallbackPrimary":   true,
			"FallbackSecondary": true,
		}
		for i := 0; i < runs; i++ {
			resp, err := client.Call(ctx, testutil.CallRequest{
				Method: "GetGreetingFallbackRoundRobinPair",
				Input:  map[string]any{"name": "World"},
			})
			if err != nil {
				t.Fatalf("Call %d failed: %v", i, err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("Call %d: expected 200, got %d body=%s err=%s",
					i, resp.StatusCode, string(resp.Body), resp.Error)
			}

			// Routing classification must always be the centralised
			// BuildRequest reason; a regression to the legacy callback
			// would surface "fallback-roundrobin-child-legacy" here and
			// the load-distribution invariant would still pass.
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPathReason, "fallback-roundrobin-child-buildrequest")
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLClient, "TestFallbackRoundRobinPair")

			// #543: the composition fallback[rr[openai,openai], openai] is
			// fully call-supported — every RR leaf and the fallback sibling
			// are openai, and the centralized RR child is recorded in the
			// resolution with its SELECTED-LEAF provider (openai), not
			// "baml-roundrobin", and is absent from LegacyChildren. So the
			// bridge's single resolution is fully call-supported and
			// dispatches via the non-streaming Request API. A "streamrequest"
			// here would mean this all-call-supported RR-child shape
			// needlessly bridged through SSE accumulation. The single
			// resolution is reused for both the Request-preference decision
			// and the centralized RR dispatch, so rotation still advances
			// exactly once per REST request (the leaf-balance invariant below
			// would break on a double-advance).
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLBuildRequestAPI, "request")

			// Winner-Client is the served openai leaf, not the RR
			// wrapper or fallback strategy parent — the centralised
			// dispatch surfaces the leaf on outcome metadata so the
			// header reports the actual contacted client.
			winner := resp.Headers.Get(testutil.HeaderBAMLWinnerClient)
			if !validLeaves[winner] {
				t.Errorf("Call %d: Winner-Client=%q is not a valid RR leaf (expected FallbackPrimary or FallbackSecondary)",
					i, winner)
			}
			testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLWinnerProvider, "openai")
		}

		// Per-leaf hit counts across the runs. The RR-cadence-agnostic
		// invariants are:
		//   - Both leaves saw at least one hit (rotation is active).
		//   - Combined leaf hits equal `runs` (every request landed on
		//     a leaf, none were lost).
		//   - Fallback sibling sees zero hits (RR child succeeded
		//     every time, sibling never reached).
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
		if primaryHits == 0 || secondaryHits == 0 {
			t.Errorf("RR rotation never reached one leaf: primary=%d secondary=%d (centralised RR-child dispatch is stuck on one leaf)",
				primaryHits, secondaryHits)
		}
		if primaryHits+secondaryHits != runs {
			t.Errorf("RR leaves saw %d total hits, want %d (primary=%d secondary=%d)",
				primaryHits+secondaryHits, runs, primaryHits, secondaryHits)
		}
		if tertiaryHits != 0 {
			t.Errorf("FallbackTertiary received %d hits; RR child succeeded every run so the sibling must stay at zero",
				tertiaryHits)
		}
	})
}

// TestFallbackRoundRobinComposition_StreamingMetadataShape exercises
// the streaming-path planned-metadata event for the same composition.
// The streaming path is the only path that surfaces FallbackTargets
// and FallbackRoundRobin on the wire (the unary header surface is
// intentionally narrow and doesn't expose chain-level details), so
// this is the load-bearing assertion for the new metadata vocabulary
// reaching real consumers.
func TestFallbackRoundRobinComposition_StreamingMetadataShape(t *testing.T) {
	skipIfNoBuildRequest(t)
	waitForHealthy(t, 30*time.Second)
	clearCompositionScenarios(t)
	registerAllGreetingScenarios(t, compositionScenarioIDs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
		Method: "GetGreetingFallbackRoundRobinPair",
		Input:  map[string]any{"name": "World"},
	})

	var planned *testutil.StreamMetadata
	var outcome *testutil.StreamMetadata
	for ev := range events {
		if !ev.IsMetadata() {
			continue
		}
		md, err := ev.ParseMetadata()
		if err != nil {
			t.Fatalf("parse metadata event: %v", err)
		}
		switch md.Phase {
		case "planned":
			planned = md
		case "outcome":
			outcome = md
		}
	}
	if err := <-errs; err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if planned == nil {
		t.Fatal("no planned metadata event observed")
	}
	if planned.Path != "buildrequest" {
		t.Errorf("planned.path = %q, want buildrequest", planned.Path)
	}
	if planned.PathReason != "fallback-roundrobin-child-buildrequest" {
		t.Errorf("planned.path_reason = %q, want fallback-roundrobin-child-buildrequest", planned.PathReason)
	}
	if planned.Client != "TestFallbackRoundRobinPair" {
		t.Errorf("planned.client = %q, want TestFallbackRoundRobinPair", planned.Client)
	}
	// Chain stays as configured (the operator-authored child list);
	// the centralised wrapper-unwrap is reflected in FallbackTargets,
	// not by mutating Chain. Sibling FallbackTertiary still appears
	// in the chain so observers see the fallback shape.
	wantChain := []string{"TestRoundRobinPair", "FallbackTertiary"}
	if !equalStringSlice(planned.Chain, wantChain) {
		t.Errorf("planned.chain = %v, want %v", planned.Chain, wantChain)
	}
	// FallbackTargets: the RR wrapper TestRoundRobinPair must have an
	// entry pointing at the centrally-selected leaf. The selected leaf
	// depends on the advancer's per-request decision; both
	// FallbackPrimary and FallbackSecondary are valid here.
	if planned.FallbackTargets == nil {
		t.Fatal("planned.fallback_targets is nil; expected entry for TestRoundRobinPair")
	}
	target, ok := planned.FallbackTargets["TestRoundRobinPair"]
	if !ok {
		t.Errorf("planned.fallback_targets missing TestRoundRobinPair key; got %v", planned.FallbackTargets)
	}
	if target != "FallbackPrimary" && target != "FallbackSecondary" {
		t.Errorf("planned.fallback_targets[TestRoundRobinPair] = %q; want FallbackPrimary or FallbackSecondary", target)
	}
	// FallbackRoundRobin: the per-child RR decision for the centralised
	// wrapper. Selected must match the FallbackTargets entry above so
	// the two views agree on which leaf the resolver picked.
	if planned.FallbackRoundRobin == nil {
		t.Fatal("planned.fallback_round_robin is nil; expected entry for TestRoundRobinPair")
	}
	rrInfo, ok := planned.FallbackRoundRobin["TestRoundRobinPair"]
	if !ok || rrInfo == nil {
		t.Fatalf("planned.fallback_round_robin missing TestRoundRobinPair entry; got %v", planned.FallbackRoundRobin)
	}
	if rrInfo.Name != "TestRoundRobinPair" {
		t.Errorf("FallbackRoundRobin[TestRoundRobinPair].Name = %q, want TestRoundRobinPair", rrInfo.Name)
	}
	if rrInfo.Selected != target {
		t.Errorf("FallbackRoundRobin[TestRoundRobinPair].Selected = %q != FallbackTargets entry %q (resolver views disagree)",
			rrInfo.Selected, target)
	}
	wantRRChildren := []string{"FallbackPrimary", "FallbackSecondary"}
	if !equalStringSlice(rrInfo.Children, wantRRChildren) {
		t.Errorf("FallbackRoundRobin[TestRoundRobinPair].Children = %v, want %v", rrInfo.Children, wantRRChildren)
	}

	// Outcome metadata: the orchestrator clears the planned-only
	// FallbackTargets / FallbackRoundRobin fields on outcome (the
	// realised winner is encoded in WinnerClient instead). Pin that
	// here so a regression that leaks planned-shape data into outcome
	// fails fast.
	if outcome == nil {
		t.Fatal("no outcome metadata event observed")
	}
	if outcome.FallbackTargets != nil {
		t.Errorf("outcome.fallback_targets must be nil; got %v", outcome.FallbackTargets)
	}
	if outcome.FallbackRoundRobin != nil {
		t.Errorf("outcome.fallback_round_robin must be nil; got %v", outcome.FallbackRoundRobin)
	}
	if outcome.WinnerClient != target {
		t.Errorf("outcome.winner_client = %q, want %q (centralised dispatch must name the served leaf)",
			outcome.WinnerClient, target)
	}
}

// TestFallbackRoundRobinComposition_FailureHandoffToSibling pins the
// failure path: when the centrally-selected RR leaf returns 500, the
// orchestrator advances to the fallback sibling (FallbackTertiary)
// and reports the sibling as the winner. Without this assertion, a
// regression that wedged the RR child or skipped the fallback-sibling
// step on RR-leaf failure could ship while every other test still
// passed.
func TestFallbackRoundRobinComposition_FailureHandoffToSibling(t *testing.T) {
	skipIfNoBuildRequest(t)
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		waitForHealthy(t, 30*time.Second)
		clearCompositionScenarios(t)

		// Force every RR leaf to return 500 so whichever leaf the
		// advancer picks, the orchestrator must hand off to the
		// fallback sibling. Tertiary serves the success payload so
		// the request still returns 200.
		for _, id := range []string{"fallback-primary", "fallback-secondary"} {
			registerRoundRobinScenario(t, &mockllm.Scenario{
				ID:          id,
				Provider:    "openai",
				Content:     "nope",
				FailAfter:   1,
				FailureMode: "500",
			})
		}
		registerRoundRobinScenario(t, &mockllm.Scenario{
			ID:             "fallback-tertiary",
			Provider:       "openai",
			Content:        "Hello from fallback-tertiary!",
			ChunkSize:      0,
			InitialDelayMs: 10,
		})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := client.Call(ctx, testutil.CallRequest{
			Method: "GetGreetingFallbackRoundRobinPair",
			Input:  map[string]any{"name": "World"},
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected 200 (fallback sibling should win); got %d body=%s err=%s",
				resp.StatusCode, string(resp.Body), resp.Error)
		}

		// Routing classification: still BuildRequest, still the
		// centralised reason — the failure-handoff goes through the
		// orchestrator's chain walk, not a legacy degradation.
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPath, "buildrequest")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLPathReason, "fallback-roundrobin-child-buildrequest")
		// Winner is the fallback sibling that succeeded after the RR
		// child failed. Sibling = FallbackTertiary (declared in the
		// fixture's strategy list as the second child).
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLWinnerClient, "FallbackTertiary")
		testutil.AssertHeaderEquals(t, resp.Headers, testutil.HeaderBAMLWinnerProvider, "openai")

		// Hit-count invariant for the failure-handoff path: the RR
		// child saw exactly one failing leaf attempt (whichever the
		// advancer picked), and the fallback sibling served the
		// retry. Cadence-agnostic: the per-leaf identity isn't pinned;
		// the sum-of-RR-leaves == 1 and tertiary == 1 invariant is
		// what proves the handoff worked.
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
		if primaryHits+secondaryHits != 1 {
			t.Errorf("RR child saw %d total leaf attempts, want 1 (primary=%d secondary=%d); failure-handoff should have moved on after the first leaf failure",
				primaryHits+secondaryHits, primaryHits, secondaryHits)
		}
		if tertiaryHits != 1 {
			t.Errorf("FallbackTertiary saw %d hits, want 1; failure-handoff to fallback sibling didn't reach the leaf",
				tertiaryHits)
		}
	})
}

// equalStringSlice is a cheap deep-equality check for []string. Tests
// in this file use it for chain / children ordering pins where
// reflect.DeepEqual would also work — this helper just keeps the test
// file dependency-light.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
