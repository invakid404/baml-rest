package buildrequest

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func TestParseDisableCallBuildRequestEnv(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"empty", "", false},
		{"true", "true", true},
		{"True", "True", true},
		{"1", "1", true},
		{"on", "on", true},
		{"yes", "yes", true},
		{"false", "false", false},
		{"0", "0", false},
		{"off", "off", false},
		{"garbage", "garbage", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BAML_REST_DISABLE_CALL_BUILD_REQUEST", tc.value)
			if got := parseDisableCallBuildRequestEnv(); got != tc.want {
				t.Errorf("value=%q: got %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestResolveProviderWithReason_BuildRequestSupported(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	res := ResolveProviderWithReason(adapter, "MyClient", "openai", IsProviderSupported)
	if res.Path != "buildrequest" {
		t.Errorf("path: got %q, want buildrequest", res.Path)
	}
	if res.PathReason != "" {
		t.Errorf("reason should be empty for buildrequest path; got %q", res.PathReason)
	}
	if res.Provider != "openai" {
		t.Errorf("provider: got %q, want openai", res.Provider)
	}
}

func TestResolveProviderWithReason_LegacyUnsupported(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	res := ResolveProviderWithReason(adapter, "MyClient", "aws-bedrock", IsProviderSupported)
	if res.Path != "legacy" {
		t.Errorf("path: got %q, want legacy", res.Path)
	}
	if res.PathReason != PathReasonUnsupportedProvider {
		t.Errorf("reason: got %q, want %q", res.PathReason, PathReasonUnsupportedProvider)
	}
}

func TestResolveProviderWithReason_LegacyEmpty(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	res := ResolveProviderWithReason(adapter, "MyClient", "", IsProviderSupported)
	if res.Path != "legacy" {
		t.Errorf("path: got %q, want legacy", res.Path)
	}
	if res.PathReason != PathReasonEmptyProvider {
		t.Errorf("reason: got %q, want %q", res.PathReason, PathReasonEmptyProvider)
	}
}

func TestResolveProviderWithReason_RoundRobin(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	res := ResolveProviderWithReason(adapter, "MyClient", "baml-roundrobin", IsProviderSupported)
	if res.Path != "legacy" {
		t.Errorf("path: got %q, want legacy", res.Path)
	}
	if res.PathReason != PathReasonRoundRobin {
		t.Errorf("reason: got %q, want %q", res.PathReason, PathReasonRoundRobin)
	}
	if res.Strategy != "baml-roundrobin" {
		t.Errorf("strategy: got %q, want baml-roundrobin", res.Strategy)
	}
}

func TestResolveFallbackChainWithReason_AllLegacy(t *testing.T) {
	primary := "Strategy"
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &primary,
		},
	}
	chains := map[string][]string{"Strategy": {"Bedrock1", "Bedrock2"}}
	providers := map[string]string{
		"Strategy": "baml-fallback",
		"Bedrock1": "aws-bedrock",
		"Bedrock2": "aws-bedrock",
	}
	chain, _, _, reason := ResolveFallbackChainWithReason(adapter, "Strategy", chains, providers, IsProviderSupported)
	if chain != nil {
		t.Errorf("expected nil chain for all-legacy; got %v", chain)
	}
	if reason != PathReasonFallbackAllLegacy {
		t.Errorf("reason: got %q, want %q", reason, PathReasonFallbackAllLegacy)
	}
}

func TestResolveFallbackChainWithReason_MixedDrivable(t *testing.T) {
	primary := "Strategy"
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &primary,
		},
	}
	chains := map[string][]string{"Strategy": {"Primary", "Backup"}}
	providers := map[string]string{
		"Strategy": "baml-fallback",
		"Primary":  "openai",
		"Backup":   "aws-bedrock",
	}
	chain, _, legacy, reason := ResolveFallbackChainWithReason(adapter, "Strategy", chains, providers, IsProviderSupported)
	if reason != "" {
		t.Errorf("expected empty reason for drivable mixed chain; got %q", reason)
	}
	if len(chain) != 2 {
		t.Errorf("expected 2-element chain; got %d", len(chain))
	}
	if !legacy["Backup"] {
		t.Errorf("Backup should be marked as legacy child")
	}
	if legacy["Primary"] {
		t.Errorf("Primary should not be marked as legacy child")
	}
}

func TestBuildSingleProviderPlan(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	plan := BuildSingleProviderPlan(adapter, "MyClient", "openai", nil, BuildRequestAPIRequest)
	if plan.Path != "buildrequest" {
		t.Errorf("path: got %q, want buildrequest", plan.Path)
	}
	if plan.BuildRequestAPI != BuildRequestAPIRequest {
		t.Errorf("BuildRequestAPI: got %q, want %q", plan.BuildRequestAPI, BuildRequestAPIRequest)
	}
	if plan.Client != "MyClient" {
		t.Errorf("client: got %q, want MyClient", plan.Client)
	}
	if plan.Provider != "openai" {
		t.Errorf("provider: got %q, want openai", plan.Provider)
	}
	if plan.RetryMax != nil {
		t.Errorf("RetryMax should be nil when policy is nil; got %v", plan.RetryMax)
	}
}

func TestBuildSingleProviderPlan_StreamRequestAPI(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	plan := BuildSingleProviderPlan(adapter, "MyClient", "openai", nil, BuildRequestAPIStreamRequest)
	if plan.BuildRequestAPI != BuildRequestAPIStreamRequest {
		t.Errorf("BuildRequestAPI: got %q, want %q", plan.BuildRequestAPI, BuildRequestAPIStreamRequest)
	}
}

func TestBuildFallbackChainPlan_APIFieldCarriesThrough(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	chain := []string{"A", "B"}
	providers := map[string]string{"A": "openai", "B": "anthropic"}
	plan := BuildFallbackChainPlan(adapter, "Strategy", chain, providers, nil, nil, BuildRequestAPIStreamRequest, "")
	if plan.BuildRequestAPI != BuildRequestAPIStreamRequest {
		t.Errorf("BuildRequestAPI: got %q, want %q", plan.BuildRequestAPI, BuildRequestAPIStreamRequest)
	}
	if plan.Strategy != "baml-fallback" {
		t.Errorf("strategy: got %q, want baml-fallback", plan.Strategy)
	}
	if plan.PathReason != "" {
		t.Errorf("pathReason: got %q, want empty (no reason passed)", plan.PathReason)
	}
}

// TestBuildFallbackChainPlan_RRChildPathReasonPlumbed is the
// metadata-level regression for CodeRabbit finding A. The F2 resolver
// reason (PathReasonFallbackRoundRobinChildLegacy, emitted by
// ResolveFallbackChainForClientWithReason when a baml-roundrobin child
// appears in the chain) previously never reached the emitted plan:
// the BuildFallbackChainPlan builders had no PathReason parameter and
// the codegen sites called the non-reason resolver. This test bolts
// the classification to the plan: plug the reason value in, assert it
// ends up on plan.PathReason. A refactor that drops the arg or stops
// threading it in codegen fails here.
func TestBuildFallbackChainPlan_RRChildPathReasonPlumbed(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	chain := []string{"OpenAIClient", "InnerRR"}
	providers := map[string]string{
		"OpenAIClient": "openai",
		"InnerRR":      "baml-roundrobin",
	}
	legacy := map[string]bool{"InnerRR": true}

	t.Run("canonical spelling via adapter-based plan builder", func(t *testing.T) {
		plan := BuildFallbackChainPlan(adapter, "MyFallback", chain, providers, legacy, nil, BuildRequestAPIStreamRequest,
			PathReasonFallbackRoundRobinChildLegacy)
		if plan.PathReason != PathReasonFallbackRoundRobinChildLegacy {
			t.Fatalf("PathReason: got %q, want %q", plan.PathReason, PathReasonFallbackRoundRobinChildLegacy)
		}
		if plan.Strategy != "baml-fallback" {
			t.Errorf("strategy: got %q, want baml-fallback", plan.Strategy)
		}
		if len(plan.LegacyChildren) != 1 || plan.LegacyChildren[0] != "InnerRR" {
			t.Errorf("LegacyChildren: got %v, want [InnerRR]", plan.LegacyChildren)
		}
	})

	t.Run("client-name plan builder", func(t *testing.T) {
		plan := BuildFallbackChainPlanForClient("MyFallback", chain, providers, legacy, nil, BuildRequestAPIRequest,
			PathReasonFallbackRoundRobinChildLegacy)
		if plan.PathReason != PathReasonFallbackRoundRobinChildLegacy {
			t.Fatalf("PathReason: got %q, want %q", plan.PathReason, PathReasonFallbackRoundRobinChildLegacy)
		}
	})
}

// TestBuildFallbackChainPlan_ReasonPlumbsFromResolver simulates the
// codegen call sequence: the `WithReason` resolver produces the reason,
// which codegen threads into the plan builder. This test exercises
// both halves together for the hyphenated BAML-upstream spelling
// (baml-round-robin) and the canonical one, so a regression in either
// the resolver's alias folding or the plumbing surfaces here.
func TestBuildFallbackChainPlan_ReasonPlumbsFromResolver(t *testing.T) {
	tests := []struct {
		name       string
		rrProvider string
	}{
		{name: "canonical spelling", rrProvider: "baml-roundrobin"},
		{name: "hyphenated upstream spelling", rrProvider: "baml-round-robin"},
		{name: "shorthand", rrProvider: "round-robin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fallbackChains := map[string][]string{
				"MyFallback": {"OpenAIClient", "InnerRR"},
			}
			clientProviders := map[string]string{
				"MyFallback":   "baml-fallback",
				"OpenAIClient": "openai",
				"InnerRR":      tt.rrProvider,
			}

			chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
				nil, "MyFallback", fallbackChains, clientProviders,
				func(p string) bool { return p == "openai" },
			)
			if chain == nil {
				t.Fatalf("expected chain to resolve, got nil with reason=%q", reason)
			}
			if reason != PathReasonFallbackRoundRobinChildLegacy {
				t.Fatalf("resolver reason: got %q, want %q", reason, PathReasonFallbackRoundRobinChildLegacy)
			}

			plan := BuildFallbackChainPlanForClient("MyFallback", chain, providers, legacy, nil, BuildRequestAPIStreamRequest, reason)
			if plan.PathReason != PathReasonFallbackRoundRobinChildLegacy {
				t.Fatalf("plan.PathReason: got %q, want %q — resolver reason did not reach emitted metadata", plan.PathReason, PathReasonFallbackRoundRobinChildLegacy)
			}
		})
	}
}

func TestBuildLegacyMetadataPlan_UnsupportedProvider(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	plan := BuildLegacyMetadataPlan(adapter, "MyClient", "aws-bedrock", nil, nil, IsProviderSupported, nil)
	if plan.Path != "legacy" {
		t.Errorf("path: got %q, want legacy", plan.Path)
	}
	if plan.PathReason != PathReasonUnsupportedProvider {
		t.Errorf("reason: got %q, want %q", plan.PathReason, PathReasonUnsupportedProvider)
	}
	if plan.Provider != "aws-bedrock" {
		t.Errorf("provider: got %q, want aws-bedrock", plan.Provider)
	}
}

func TestEncodeRetryPolicy_Formats(t *testing.T) {
	cases := []struct {
		name string
		mk   func() any // returns *retry.Policy or nil
		want string
	}{
		{name: "nil policy", mk: func() any { return nil }, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeRetryPolicy(nil)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
