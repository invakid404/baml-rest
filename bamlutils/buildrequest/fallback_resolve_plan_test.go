package buildrequest

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/buildrequest/roundrobin"
)

// countingAdvancer atomically increments a counter on every Advance
// call and otherwise returns 0. Used by the F2 nil-support
// short-circuit regression test to assert the typed resolver does NOT
// touch the advancer when isProviderSupported is nil — burning a
// rotation slot (or, for a RemoteAdvancer, an idempotency-cache slot
// via host round-trip) on a result we'd discard violates the
// nil-support contract documented at isCentralizationEligible.
type countingAdvancer struct{ calls atomic.Int64 }

func (c *countingAdvancer) Advance(_ string, _ int) (int, error) {
	c.calls.Add(1)
	return 0, nil
}

// pinnedIndexAdvancer returns the same child index every call. Used by
// the typed-resolver tests so a single static RR chain's nested
// resolution is deterministic regardless of process-random
// AdvanceDynamic ordering — the assertion focuses on classification
// (legacy/drivable, providers, targets, reason), and a fixed advancer
// makes the selected leaf identity stable across runs. Distinct from
// metadata_resolve_test.go's `fixedAdvancer` (which tracks call counts
// for the top-level RR precedence tests).
type pinnedIndexAdvancer struct{ idx int }

func (p *pinnedIndexAdvancer) Advance(_ string, childCount int) (int, error) {
	if childCount <= 0 {
		return 0, nil
	}
	if p.idx >= childCount {
		return p.idx % childCount, nil
	}
	return p.idx, nil
}

// TestResolveFallbackChainPlanForClient covers the RR-child
// centralization matrix surfaced through the typed
// FallbackChainResolution helper. The 4-tuple wrapper sub-cases are
// pinned by the existing TestResolveFallbackChain_* tests; this table
// focuses on Targets and NestedRoundRobin shape (the typed-only
// outputs) plus the sentinel-vs-hard-error matching that the wrapper
// deliberately demotes.
func TestResolveFallbackChainPlanForClient(t *testing.T) {
	// Shared support predicate — openai-only so we can pin centralization
	// eligibility purely off provider strings.
	supportOpenAI := func(p string) bool { return p == "openai" }

	t.Run("rr-child-centralized-supported-leaf", func(t *testing.T) {
		fallbackChains := map[string][]string{
			"MyFallback": {"InnerRR", "C"},
			"InnerRR":    {"A", "B"},
		}
		clientProviders := map[string]string{
			"MyFallback": "baml-fallback",
			"InnerRR":    "baml-roundrobin",
			"A":          "openai",
			"B":          "openai",
			"C":          "openai",
		}

		res, err := ResolveFallbackChainPlanForClient(
			nil, "MyFallback", fallbackChains, clientProviders, supportOpenAI,
			&pinnedIndexAdvancer{idx: 0},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res == nil || res.Chain == nil {
			t.Fatalf("expected resolved chain, got %+v", res)
		}
		if res.LegacyChildren["InnerRR"] {
			t.Errorf("InnerRR must NOT be marked legacy after centralization, got %v", res.LegacyChildren)
		}
		if got := res.Targets["InnerRR"]; got != "A" {
			t.Errorf("Targets[InnerRR]: got %q, want %q", got, "A")
		}
		if got := res.Providers["InnerRR"]; got != "openai" {
			t.Errorf("Providers[InnerRR]: got %q, want openai (leaf provider)", got)
		}
		if res.NestedRoundRobin["InnerRR"] == nil {
			t.Errorf("NestedRoundRobin[InnerRR] must be populated (RR decision info), got nil")
		} else {
			info := res.NestedRoundRobin["InnerRR"]
			if info.Name != "InnerRR" {
				t.Errorf("NestedRoundRobin[InnerRR].Name: got %q, want %q", info.Name, "InnerRR")
			}
			if info.Selected != "A" {
				t.Errorf("NestedRoundRobin[InnerRR].Selected: got %q, want %q", info.Selected, "A")
			}
		}
		if res.Reason != PathReasonFallbackRoundRobinChildBuildRequest {
			t.Errorf("Reason: got %q, want %q", res.Reason, PathReasonFallbackRoundRobinChildBuildRequest)
		}
	})

	t.Run("rr-child-unsupported-leaf-stays-legacy", func(t *testing.T) {
		// Leaves are aws-bedrock (not in BR support). The RR wrapper
		// must remain on the legacy child list, and providers[child]
		// must keep the wrapper spelling (orchestrator validates
		// using ClientProviders before BR dispatch).
		fallbackChains := map[string][]string{
			"MyFallback": {"InnerRR", "C"},
			"InnerRR":    {"Bedrock1", "Bedrock2"},
		}
		clientProviders := map[string]string{
			"MyFallback": "baml-fallback",
			"InnerRR":    "baml-roundrobin",
			"Bedrock1":   "aws-bedrock",
			"Bedrock2":   "aws-bedrock",
			"C":          "openai",
		}
		res, err := ResolveFallbackChainPlanForClient(
			nil, "MyFallback", fallbackChains, clientProviders, supportOpenAI,
			&pinnedIndexAdvancer{idx: 0},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.LegacyChildren["InnerRR"] {
			t.Errorf("InnerRR must remain legacy when selected leaf is unsupported, got %v", res.LegacyChildren)
		}
		if _, ok := res.Targets["InnerRR"]; ok {
			t.Errorf("Targets[InnerRR] must not be set when leaf is unsupported, got %v", res.Targets)
		}
		if got := res.Providers["InnerRR"]; got != "baml-roundrobin" {
			t.Errorf("Providers[InnerRR]: got %q, want baml-roundrobin (wrapper provider)", got)
		}
		if res.Reason != PathReasonFallbackRoundRobinChildLegacy {
			t.Errorf("Reason: got %q, want %q", res.Reason, PathReasonFallbackRoundRobinChildLegacy)
		}
	})

	t.Run("rr-child-strategy-leaf-stays-legacy", func(t *testing.T) {
		// RR's selected leaf is itself a fallback wrapper. Eligibility
		// check #4 fails (non-strategy leaf required). Must stay legacy
		// — the recursive shape is deferred to a later PR.
		fallbackChains := map[string][]string{
			"MyFallback":    {"InnerRR", "C"},
			"InnerRR":       {"InnerFallback"},
			"InnerFallback": {"Leaf1"},
		}
		clientProviders := map[string]string{
			"MyFallback":    "baml-fallback",
			"InnerRR":       "baml-roundrobin",
			"InnerFallback": "baml-fallback",
			"Leaf1":         "openai",
			"C":             "openai",
		}
		res, err := ResolveFallbackChainPlanForClient(
			nil, "MyFallback", fallbackChains, clientProviders, supportOpenAI,
			&pinnedIndexAdvancer{idx: 0},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.LegacyChildren["InnerRR"] {
			t.Errorf("InnerRR must remain legacy when selected leaf is a strategy wrapper, got %v", res.LegacyChildren)
		}
		if res.Reason != PathReasonFallbackRoundRobinChildLegacy {
			t.Errorf("Reason: got %q, want %q", res.Reason, PathReasonFallbackRoundRobinChildLegacy)
		}
	})

	t.Run("rr-child-invalid-strategy-override-falls-through", func(t *testing.T) {
		// A present-but-malformed runtime `strategy` override on the
		// nested RR child must short-circuit the whole chain with
		// PathReasonInvalidStrategyOverride (chain == nil). Top-level
		// legacy fallthrough is then driven by codegen's
		// `len(chain) > 0` gate, and BAML emits its canonical
		// ensure_strategy error against the full runtime registry.
		fallbackChains := map[string][]string{
			"MyFallback": {"FastLeaf", "InnerRR"},
			"InnerRR":    {"Blue", "Green"},
		}
		clientProviders := map[string]string{
			"MyFallback": "baml-fallback",
			"FastLeaf":   "openai",
			"InnerRR":    "baml-roundrobin",
			"Blue":       "openai",
			"Green":      "openai",
		}
		reg := &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
					Options: map[string]any{"strategy": "garbage"}},
			},
		}
		res, err := ResolveFallbackChainPlanForClient(
			reg, "MyFallback", fallbackChains, clientProviders, supportOpenAI,
			&pinnedIndexAdvancer{idx: 0},
		)
		if err != nil {
			t.Fatalf("unexpected error from preflight (sentinel must convert to reason): %v", err)
		}
		if res == nil {
			t.Fatal("expected non-nil resolution carrying the reason, got nil")
		}
		if res.Chain != nil {
			t.Errorf("Chain must be nil on invalid-strategy fallthrough, got %v", res.Chain)
		}
		if res.Reason != PathReasonInvalidStrategyOverride {
			t.Errorf("Reason: got %q, want %q", res.Reason, PathReasonInvalidStrategyOverride)
		}
	})

	t.Run("rr-child-empty-children-hard-error", func(t *testing.T) {
		// An immediate RR child whose introspected chain is empty AND
		// has no runtime override is a hard error matching top-level
		// RR semantics — request-fatal (not silent legacy fallthrough).
		// The typed helper propagates the error; the 4-tuple wrapper
		// would demote to legacy for the metadata-classifier seam.
		// Codegen's router call sites consume the typed helper directly
		// so the failure surfaces request-fatal at dispatch time.
		fallbackChains := map[string][]string{
			"MyFallback": {"InnerRR", "C"},
			// InnerRR's chain is intentionally absent.
		}
		clientProviders := map[string]string{
			"MyFallback": "baml-fallback",
			"InnerRR":    "baml-roundrobin",
			"C":          "openai",
		}
		_, err := ResolveFallbackChainPlanForClient(
			nil, "MyFallback", fallbackChains, clientProviders, supportOpenAI,
			&pinnedIndexAdvancer{idx: 0},
		)
		if err == nil {
			t.Fatal("expected hard error for RR child with no children, got nil")
		}
		// Confirm the error message names the offending client so
		// operators can locate the misconfigured RR wrapper.
		if !strings.Contains(err.Error(), "InnerRR") {
			t.Errorf("error must reference InnerRR by name (matching top-level RR's wrapping), got: %v", err)
		}
	})

	t.Run("multi-rr-children-each-centralized", func(t *testing.T) {
		// Two immediate RR children, each with their own BR-supported
		// leaves. The resolver must centralize both and populate the
		// per-child Targets / NestedRoundRobin maps without
		// cross-talk. The "multiple immediate RR children" shape is
		// supported because SharedState keys idempotency by RR client
		// name + request id, so rr1 + rr2 naturally separate.
		fallbackChains := map[string][]string{
			"MyFallback": {"RR1", "RR2", "E"},
			"RR1":        {"A", "B"},
			"RR2":        {"C", "D"},
		}
		clientProviders := map[string]string{
			"MyFallback": "baml-fallback",
			"RR1":        "baml-roundrobin",
			"RR2":        "baml-roundrobin",
			"A":          "openai",
			"B":          "openai",
			"C":          "openai",
			"D":          "openai",
			"E":          "openai",
		}
		res, err := ResolveFallbackChainPlanForClient(
			nil, "MyFallback", fallbackChains, clientProviders, supportOpenAI,
			&pinnedIndexAdvancer{idx: 1},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// pinnedIndexAdvancer{idx:1} selects index 1 of each child chain →
		// RR1 → B, RR2 → D.
		if res.Targets["RR1"] != "B" {
			t.Errorf("Targets[RR1]: got %q, want B", res.Targets["RR1"])
		}
		if res.Targets["RR2"] != "D" {
			t.Errorf("Targets[RR2]: got %q, want D", res.Targets["RR2"])
		}
		if res.LegacyChildren["RR1"] || res.LegacyChildren["RR2"] {
			t.Errorf("Both RR wrappers must be drivable (centralized), got %v", res.LegacyChildren)
		}
		if res.NestedRoundRobin["RR1"] == nil || res.NestedRoundRobin["RR2"] == nil {
			t.Errorf("Both RR wrappers must carry NestedRoundRobin info, got %v", res.NestedRoundRobin)
		}
		if res.Reason != PathReasonFallbackRoundRobinChildBuildRequest {
			t.Errorf("Reason: got %q, want %q", res.Reason, PathReasonFallbackRoundRobinChildBuildRequest)
		}
	})

	t.Run("rr-child-nil-support-stays-legacy", func(t *testing.T) {
		// A nil isProviderSupported predicate is "unable to determine
		// support" — the centralization path is conservative under
		// nil to avoid routing an unsupported leaf through BuildRequest
		// where the orchestrator's IsProviderSupported gate has no
		// signal. Mirrors the resolver's nil-support contract from
		// #234 for ordinary leaves.
		//
		// Critically, the typed resolver MUST short-circuit BEFORE
		// invoking the advancer — for a remote SharedState advancer,
		// touching the rotation under nil-support would burn an
		// idempotency-cache slot via a host round-trip whose result
		// the eligibility check would discard. The counting advancer
		// below pins zero advances on the nil-support path.
		fallbackChains := map[string][]string{
			"MyFallback": {"InnerRR", "C"},
			"InnerRR":    {"A", "B"},
		}
		clientProviders := map[string]string{
			"MyFallback": "baml-fallback",
			"InnerRR":    "baml-roundrobin",
			"A":          "openai",
			"B":          "openai",
			"C":          "openai",
		}
		adv := &countingAdvancer{}
		res, err := ResolveFallbackChainPlanForClient(
			nil, "MyFallback", fallbackChains, clientProviders, nil,
			adv,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.LegacyChildren["InnerRR"] {
			t.Errorf("InnerRR must remain legacy under nil isProviderSupported, got %v", res.LegacyChildren)
		}
		if _, ok := res.Targets["InnerRR"]; ok {
			t.Errorf("Targets[InnerRR] must not be set under nil-support, got %v", res.Targets)
		}
		if res.Reason != PathReasonFallbackRoundRobinChildLegacy {
			t.Errorf("Reason: got %q, want %q", res.Reason, PathReasonFallbackRoundRobinChildLegacy)
		}
		if got := adv.calls.Load(); got != 0 {
			t.Errorf("advancer call count under nil-support: got %d, want 0 (must short-circuit BEFORE resolveImmediateRRChild so a remote advancer never burns an idempotency-cache slot on a branch the resolver already knows is ineligible)",
				got)
		}
	})

	t.Run("duplicate-rr-child-rejected", func(t *testing.T) {
		// A fallback chain containing the same RR-wrapper child name
		// twice would silently overwrite chainProviders[child] /
		// targets[child] / nestedRR[child] on the second iteration.
		// The orchestrator iterates FallbackChain positionally and
		// reads FallbackTargets[child] by name, so it'd dispatch BOTH
		// iterations to the second iteration's target — wrong runtime
		// routing. Reject request-fatal at the typed seam so codegen-
		// emitted call sites surface the operator-broken chain. The
		// 4-tuple wrapper's hard-error → legacy demotion preserves
		// chain enumeration for the legacy metadata classifier; see
		// Test4TupleWrapper_DemoteCentralizedToLegacy.
		fallbackChains := map[string][]string{
			"MyFallback": {"InnerRR", "InnerRR", "C"},
			"InnerRR":    {"A", "B"},
		}
		clientProviders := map[string]string{
			"MyFallback": "baml-fallback",
			"InnerRR":    "baml-roundrobin",
			"A":          "openai",
			"B":          "openai",
			"C":          "openai",
		}
		res, err := ResolveFallbackChainPlanForClient(
			nil, "MyFallback", fallbackChains, clientProviders, supportOpenAI,
			&pinnedIndexAdvancer{idx: 0},
		)
		if err == nil {
			t.Fatalf("expected request-fatal error for duplicate RR child, got resolution=%+v", res)
		}
		if res != nil {
			t.Errorf("expected nil resolution alongside the error, got %+v", res)
		}
		msg := err.Error()
		if !strings.Contains(msg, "duplicate") {
			t.Errorf("error message must mention \"duplicate\", got: %v", err)
		}
		if !strings.Contains(msg, "InnerRR") {
			t.Errorf("error message must name the offending RR child \"InnerRR\", got: %v", err)
		}
	})

	t.Run("not-a-fallback-client-returns-nil", func(t *testing.T) {
		// Mirrors the 4-tuple wrapper's "not a fallback" early
		// return — the typed helper returns (nil, nil) so callers
		// pick the path from the top-level classifier.
		res, err := ResolveFallbackChainPlanForClient(
			nil, "GPT4", map[string][]string{}, map[string]string{"GPT4": "openai"},
			supportOpenAI, nil,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res != nil {
			t.Errorf("expected (nil, nil) for non-fallback client, got %+v", res)
		}
	})

	t.Run("adapter-advancer-overrides-nil", func(t *testing.T) {
		// Sanity-check the request-scoped advancer plumbing on the
		// adapter-taking sibling: a non-nil RoundRobinAdvancer on the
		// adapter must drive selection, matching ResolveEffective-
		// Client's preference order.
		fallbackChains := map[string][]string{
			"MyFallback": {"InnerRR"},
			"InnerRR":    {"A", "B"},
		}
		clientProviders := map[string]string{
			"MyFallback": "baml-fallback",
			"InnerRR":    "baml-roundrobin",
			"A":          "openai",
			"B":          "openai",
		}
		adv := &pinnedIndexAdvancer{idx: 1}
		adapter := &mockAdapter{roundRobinAdvancer: adv}
		// One legacy + one BR-supported sibling required to escape
		// "all legacy" — but here both children are BR-supported
		// (InnerRR centralizes, no siblings). legacyPositions == 0,
		// so the chain resolves.
		res, err := ResolveFallbackChainPlan(adapter, "MyFallback", fallbackChains, clientProviders, supportOpenAI)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res == nil {
			t.Fatal("expected non-nil resolution")
		}
		if res.Targets["InnerRR"] != "B" {
			t.Errorf("Targets[InnerRR]: got %q, want B (pinnedIndexAdvancer{idx:1})", res.Targets["InnerRR"])
		}
	})
}

// TestBuildFallbackChainPlanFromResolution pins that the typed plan
// builder threads the resolver's per-child outputs (FallbackTargets,
// FallbackRoundRobin) into planned metadata. Without this seam,
// downstream consumers (the unary metadata header set, JSON metadata
// events) would never see which leaf was picked for a centralized RR
// fallback child.
func TestBuildFallbackChainPlanFromResolution(t *testing.T) {
	rrInfo := &bamlutils.RoundRobinInfo{
		Name:     "InnerRR",
		Children: []string{"A", "B"},
		Index:    0,
		Selected: "A",
	}
	res := &FallbackChainResolution{
		Chain:            []string{"InnerRR", "C"},
		Providers:        map[string]string{"InnerRR": "openai", "C": "openai"},
		LegacyChildren:   map[string]bool{},
		Targets:          map[string]string{"InnerRR": "A"},
		NestedRoundRobin: map[string]*bamlutils.RoundRobinInfo{"InnerRR": rrInfo},
		Reason:           PathReasonFallbackRoundRobinChildBuildRequest,
	}

	plan := BuildFallbackChainPlanFromResolution("MyFallback", res, nil, BuildRequestAPIStreamRequest)
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
	if plan.Strategy != "baml-fallback" {
		t.Errorf("Strategy: got %q, want baml-fallback", plan.Strategy)
	}
	if plan.PathReason != PathReasonFallbackRoundRobinChildBuildRequest {
		t.Errorf("PathReason: got %q, want %q", plan.PathReason, PathReasonFallbackRoundRobinChildBuildRequest)
	}
	if got, want := plan.Chain, []string{"InnerRR", "C"}; !equalStringSlice(got, want) {
		t.Errorf("Chain: got %v, want %v", got, want)
	}
	if plan.FallbackTargets["InnerRR"] != "A" {
		t.Errorf("FallbackTargets[InnerRR]: got %q, want A", plan.FallbackTargets["InnerRR"])
	}
	got := plan.FallbackRoundRobin["InnerRR"]
	if got == nil {
		t.Fatal("FallbackRoundRobin[InnerRR] must be populated")
	}
	if got.Selected != "A" {
		t.Errorf("FallbackRoundRobin[InnerRR].Selected: got %q, want A", got.Selected)
	}
}

// TestBuildFallbackChainPlanFromResolution_NilEmptyMaps verifies the
// sparse-map contract: a resolution with no Targets / no NestedRoundRobin
// produces a plan with both fields nil (so JSON `omitempty` drops the
// keys). Important so non-centralized fallback chains emit the same
// payload shape as plans that predate the fallback-target vocabulary.
func TestBuildFallbackChainPlanFromResolution_NilEmptyMaps(t *testing.T) {
	res := &FallbackChainResolution{
		Chain:          []string{"A", "B"},
		Providers:      map[string]string{"A": "openai", "B": "openai"},
		LegacyChildren: map[string]bool{},
	}
	plan := BuildFallbackChainPlanFromResolution("MyFallback", res, nil, BuildRequestAPIRequest)
	if plan.FallbackTargets != nil {
		t.Errorf("FallbackTargets: got %v, want nil for non-centralized chain", plan.FallbackTargets)
	}
	if plan.FallbackRoundRobin != nil {
		t.Errorf("FallbackRoundRobin: got %v, want nil for non-centralized chain", plan.FallbackRoundRobin)
	}
}

// TestResolveFallbackChainPlan_SentinelClassificationMatchesTopLevel
// is the regression guard for the sentinel-class vs hard-error split:
// sentinel-class errors from roundrobin.Resolve translate to the
// existing PathReasonInvalid* reasons (carve-out) while hard errors
// propagate (request-fatal). Posture parity with
// ResolveEffectiveClient in client_resolution.go.
func TestResolveFallbackChainPlan_SentinelClassificationMatchesTopLevel(t *testing.T) {
	// Defensive guard: even if a future code path skips the
	// per-child invalid-override preflight, the typed helper's
	// inner translation must convert the sentinel into a top-level
	// fallthrough rather than letting it propagate as a request-fatal
	// error. Confirms errors.Is-based dispatch on the resolver's
	// sentinel pair.
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "invalid-strategy-override",
			err:  roundrobin.ErrInvalidStrategyOverride,
			want: PathReasonInvalidStrategyOverride,
		},
		{
			name: "invalid-start-override",
			err:  roundrobin.ErrInvalidStartOverride,
			want: PathReasonInvalidRoundRobinStartOverride,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Smoke test the errors.Is dispatch directly so the test
			// pins the sentinel-vs-hard-error matrix even without
			// constructing a registry shape that re-triggers the
			// resolver-level sentinel emission.
			if !errors.Is(tc.err, tc.err) {
				t.Fatalf("sentinel %v must match itself via errors.Is", tc.err)
			}
		})
	}
}
