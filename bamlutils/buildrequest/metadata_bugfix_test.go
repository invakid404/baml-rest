package buildrequest

import (
	"context"
	"sync"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// setBuildRequestForTest forces UseBuildRequest() to return a fixed value for
// the duration of the test. The production cache is a sync.Once + bool pair;
// we replace the Once with a fresh one and pre-fire it so the cached bool is
// authoritative without re-running parseBuildRequestEnv. Restoration runs in
// the test cleanup so neighbouring tests are unaffected.
//
// Tests that call this helper must NOT use t.Parallel — they share the
// process-global cache.
func setBuildRequestForTest(t *testing.T, v bool) {
	t.Helper()
	prevOnce := useBuildRequestOnce
	prevCached := useBuildRequestCached
	useBuildRequestOnce = sync.Once{}
	useBuildRequestCached = v
	useBuildRequestOnce.Do(func() {})
	t.Cleanup(func() {
		useBuildRequestOnce = prevOnce
		useBuildRequestCached = prevCached
	})
}

// TestBuildLegacyMetadataPlan_ValidChainBuildRequestDisabled exercises Bug 1.
// When the request goes legacy because BuildRequest is disabled (not because
// the chain is malformed), PathReason must NOT be PathReasonFallbackEmptyChain.
// That value is a placeholder seeded by ResolveProviderWithReason and should
// be overwritten by the real classification.
func TestBuildLegacyMetadataPlan_ValidChainBuildRequestDisabled(t *testing.T) {
	setBuildRequestForTest(t, false)

	adapter := &mockAdapter{Context: context.Background()}
	chains := map[string][]string{
		"Strategy": {"Primary", "Backup"},
	}
	providers := map[string]string{
		"Strategy": "baml-fallback",
		"Primary":  "openai",
		"Backup":   "anthropic",
	}

	plan := BuildLegacyMetadataPlan(adapter, "Strategy", "baml-fallback", chains, providers, IsProviderSupported, nil)

	if plan.PathReason == PathReasonFallbackEmptyChain {
		t.Fatalf("BUG 1: valid chain with BuildRequest disabled reports fallback-empty-chain; want buildrequest-disabled. plan=%+v", plan)
	}
	if plan.PathReason != PathReasonBuildRequestDisabled {
		t.Errorf("PathReason: got %q, want %q", plan.PathReason, PathReasonBuildRequestDisabled)
	}
	// The chain should still be enumerated for observability.
	if got, want := plan.Chain, []string{"Primary", "Backup"}; !equalStringSlice(got, want) {
		t.Errorf("Chain: got %v, want %v", got, want)
	}
}

// TestBuildLegacyMetadataPlan_PrimaryOverrideChain exercises Bug 2. A primary-
// client override should make the metadata plan describe the OVERRIDDEN
// client's chain, not the introspected default's chain. Today the helper
// looks up fallbackChains[defaultClientName] before falling through to
// resolution.Client, so an override that lands on a different all-legacy
// strategy reports the wrong children.
func TestBuildLegacyMetadataPlan_PrimaryOverrideChain(t *testing.T) {
	override := "OverrideStrategy"
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &override,
		},
	}
	chains := map[string][]string{
		"DefaultStrategy":  {"DefaultA", "DefaultB"},
		"OverrideStrategy": {"OverrideX", "OverrideY"},
	}
	providers := map[string]string{
		"DefaultStrategy":  "baml-fallback",
		"OverrideStrategy": "baml-fallback",
		"DefaultA":         "aws-bedrock",
		"DefaultB":         "aws-bedrock",
		"OverrideX":        "aws-bedrock",
		"OverrideY":        "aws-bedrock",
	}

	plan := BuildLegacyMetadataPlan(adapter, "DefaultStrategy", "baml-fallback", chains, providers, IsProviderSupported, nil)

	wantChain := []string{"OverrideX", "OverrideY"}
	if !equalStringSlice(plan.Chain, wantChain) {
		t.Fatalf("BUG 2: chain reflects defaultClientName, not the runtime-resolved override. got %v, want %v", plan.Chain, wantChain)
	}
}

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

// TestBuildLegacyMetadataPlan_RuntimeStrategyOverrideChain exercises the
// CodeRabbit finding about `fallbackChains[...]` lookups in the rebuild path.
// A runtime `strategy` option on the fallback client replaces the introspected
// chain; the metadata plan must reflect that override. Today the rebuild
// reads `fallbackChains[resolution.Client]` / `fallbackChains[defaultClientName]`
// directly, bypassing resolveFallbackStrategyChain — the authoritative
// resolver that accounts for runtime strategy overrides.
func TestBuildLegacyMetadataPlan_RuntimeStrategyOverrideChain(t *testing.T) {
	// Runtime registry configures the fallback client with a custom chain,
	// AND overrides all children to aws-bedrock so the chain falls to
	// legacy (triggering the rebuild path).
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "Strategy",
					Provider: "baml-fallback",
					Options:  map[string]any{"strategy": []string{"OverrideX", "OverrideY"}},
				},
				{Name: "OverrideX", Provider: "aws-bedrock"},
				{Name: "OverrideY", Provider: "aws-bedrock"},
			},
		},
	}
	chains := map[string][]string{
		"Strategy": {"IntrospectedA", "IntrospectedB"}, // what introspection sees
	}
	providers := map[string]string{
		"Strategy":      "baml-fallback",
		"IntrospectedA": "openai",
		"IntrospectedB": "anthropic",
	}

	plan := BuildLegacyMetadataPlan(adapter, "Strategy", "baml-fallback", chains, providers, IsProviderSupported, nil)

	wantChain := []string{"OverrideX", "OverrideY"}
	if !equalStringSlice(plan.Chain, wantChain) {
		t.Fatalf("chain should reflect runtime strategy override; got %v, want %v", plan.Chain, wantChain)
	}
}

// TestResolveFallbackChainWithReason_EmptyPrimaryIgnored exercises
// CodeRabbit's second finding. A ClientRegistry with Primary pointing to
// an empty string should be treated as "no primary override" — matching
// ResolveProviderWithReason's behaviour. Today
// ResolveFallbackChainWithReason treats Primary: ptr("") as an override,
// so it looks up an empty client name and reaches the wrong conclusion.
func TestResolveFallbackChainWithReason_EmptyPrimaryIgnored(t *testing.T) {
	emptyPrimary := ""
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &emptyPrimary,
		},
	}
	chains := map[string][]string{
		"DefaultStrategy": {"Primary", "Backup"},
	}
	providers := map[string]string{
		"DefaultStrategy": "baml-fallback",
		"Primary":         "openai",
		"Backup":          "anthropic",
	}

	chain, _, _, reason := ResolveFallbackChainWithReason(adapter, "DefaultStrategy", chains, providers, IsProviderSupported)
	if reason != "" {
		t.Fatalf("empty primary should not block chain resolution; got reason %q", reason)
	}
	wantChain := []string{"Primary", "Backup"}
	if !equalStringSlice(chain, wantChain) {
		t.Fatalf("empty primary should be treated as no override; got chain %v, want %v", chain, wantChain)
	}
}

// TestResolveFallbackChain_EmptyPrimaryIgnored mirrors the above for the
// non-reason sibling (same bug, same fix surface).
func TestResolveFallbackChain_EmptyPrimaryIgnored(t *testing.T) {
	emptyPrimary := ""
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &emptyPrimary,
		},
	}
	chains := map[string][]string{
		"DefaultStrategy": {"Primary", "Backup"},
	}
	providers := map[string]string{
		"DefaultStrategy": "baml-fallback",
		"Primary":         "openai",
		"Backup":          "anthropic",
	}

	chain, _, _ := ResolveFallbackChain(adapter, "DefaultStrategy", chains, providers, IsProviderSupported)
	wantChain := []string{"Primary", "Backup"}
	if !equalStringSlice(chain, wantChain) {
		t.Fatalf("empty primary should be treated as no override; got chain %v, want %v", chain, wantChain)
	}
}

// TestBuildLegacyMetadataPlan_RuntimeOverrideRespectedInLegacyChain
// exercises Codex's third finding. When a chain falls to legacy because
// every child's *runtime* provider is unsupported (introspected providers
// would have been supported), the metadata plan must still mark each
// child as legacy. Today the rebuild path consults the static
// clientProviders map directly instead of going through resolveChildProvider,
// so it loses sight of the runtime override.
func TestBuildLegacyMetadataPlan_RuntimeOverrideRespectedInLegacyChain(t *testing.T) {
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			// Override the child's provider to one that's not supported by
			// BuildRequest. The introspected static map still says openai.
			Clients: []*bamlutils.ClientProperty{
				{Name: "Child", Provider: "aws-bedrock"},
			},
		},
	}
	chains := map[string][]string{
		"Strategy": {"Child"},
	}
	providers := map[string]string{
		"Strategy": "baml-fallback",
		"Child":    "openai", // static — but runtime says aws-bedrock
	}

	plan := BuildLegacyMetadataPlan(adapter, "Strategy", "baml-fallback", chains, providers, IsProviderSupported, nil)

	// PathReason should reflect that the chain is fully legacy under the
	// runtime override. ResolveFallbackChainWithReason already accounts
	// for runtime overrides, so this part works today.
	if plan.PathReason != PathReasonFallbackAllLegacy {
		t.Fatalf("PathReason: got %q, want %q (runtime override flips child to unsupported)", plan.PathReason, PathReasonFallbackAllLegacy)
	}
	// The bug: the rebuild loop used clientProviders["Child"] (= "openai")
	// directly, missed the runtime override, and left LegacyChildren empty.
	if len(plan.LegacyChildren) != 1 || plan.LegacyChildren[0] != "Child" {
		t.Fatalf("BUG: rebuild ignored runtime provider override; LegacyChildren=%v, want [Child]", plan.LegacyChildren)
	}
}
