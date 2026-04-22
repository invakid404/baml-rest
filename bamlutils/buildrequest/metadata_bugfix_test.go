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
