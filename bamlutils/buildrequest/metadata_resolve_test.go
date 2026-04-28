package buildrequest

import (
	"context"
	"encoding/json"
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

// TestBuildFallbackChainPlan_RRChildPathReasonPlumbed pins that the
// resolver reason (PathReasonFallbackRoundRobinChildLegacy, emitted
// by ResolveFallbackChainForClientWithReason when a baml-roundrobin
// child appears in the chain) reaches the emitted plan. Plug the
// reason value into BuildFallbackChainPlan and assert it surfaces on
// plan.PathReason. A refactor that drops the arg or stops
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

// invalidStrategyOverrides enumerates the runtime client_registry shapes
// that ParseStrategyOption refuses (per BAML upstream's ensure_strategy
// contract). Reused across the fallback-chain and round-robin tests
// below so both resolvers stay in lockstep on which inputs are
// considered "present-but-invalid".
//
// Cold-review-2 finding 1: each of these must surface as
// PathReasonInvalidStrategyOverride (or the resolver-level equivalent
// for RR — ErrInvalidStrategyOverride) rather than silently using the
// introspected chain.
func invalidStrategyOverrides() []struct {
	name string
	raw  any
} {
	return []struct {
		name string
		raw  any
	}{
		{"empty []string", []string{}},
		{"empty []any", []any{}},
		{"empty bracket string", "[]"},
		{"prefix + empty brackets", "strategy []"},
		{"half-bracketed string", "strategy [A"},
		{"bare token string", "ClientA"},
		{"heterogeneous []any", []any{"A", 42}},
		{"only-blank []string", []string{"", "  "}},
	}
}

func TestResolveProviderWithReason_FallbackInvalidOverride(t *testing.T) {
	for _, tc := range invalidStrategyOverrides() {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &mockAdapter{
				Context: context.Background(),
				originalRegistry: &bamlutils.ClientRegistry{
					Clients: []*bamlutils.ClientProperty{
						{
							Name:     "MyFallback",
							Provider: "baml-fallback",
							Options:  map[string]any{"strategy": tc.raw},
						},
					},
				},
			}
			res := ResolveProviderWithReason(adapter, "MyFallback", "baml-fallback", IsProviderSupported)
			if res.Path != "legacy" {
				t.Errorf("path: got %q, want legacy", res.Path)
			}
			if res.PathReason != PathReasonInvalidStrategyOverride {
				t.Errorf("reason: got %q, want %q", res.PathReason, PathReasonInvalidStrategyOverride)
			}
			if res.Strategy != "baml-fallback" {
				t.Errorf("strategy: got %q, want baml-fallback", res.Strategy)
			}
		})
	}
}

func TestResolveProviderWithReason_RoundRobinInvalidOverride(t *testing.T) {
	for _, tc := range invalidStrategyOverrides() {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &mockAdapter{
				Context: context.Background(),
				originalRegistry: &bamlutils.ClientRegistry{
					Clients: []*bamlutils.ClientProperty{
						{
							Name:     "MyRR",
							Provider: "baml-roundrobin",
							Options:  map[string]any{"strategy": tc.raw},
						},
					},
				},
			}
			res := ResolveProviderWithReason(adapter, "MyRR", "baml-roundrobin", IsProviderSupported)
			if res.Path != "legacy" {
				t.Errorf("path: got %q, want legacy", res.Path)
			}
			if res.PathReason != PathReasonInvalidStrategyOverride {
				t.Errorf("reason: got %q, want %q", res.PathReason, PathReasonInvalidStrategyOverride)
			}
			if res.Strategy != "baml-roundrobin" {
				t.Errorf("strategy: got %q, want baml-roundrobin", res.Strategy)
			}
		})
	}
}

func TestResolveProviderWithReason_RoundRobinAbsentStrategyKeepsRoundRobinReason(t *testing.T) {
	// A runtime registry entry without a strategy key (or no entry at
	// all) must still surface as PathReasonRoundRobin — the
	// invalid-strategy detection must not regress the RR-legacy
	// classification for valid configurations.
	adapter := &mockAdapter{Context: context.Background()}
	res := ResolveProviderWithReason(adapter, "MyRR", "baml-roundrobin", IsProviderSupported)
	if res.PathReason != PathReasonRoundRobin {
		t.Errorf("absent override must yield RR reason; got %q", res.PathReason)
	}
}

func TestResolveFallbackChainForClientWithReason_InvalidOverride(t *testing.T) {
	for _, tc := range invalidStrategyOverrides() {
		t.Run(tc.name, func(t *testing.T) {
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{
						Name:     "MyFallback",
						Provider: "baml-fallback",
						Options:  map[string]any{"strategy": tc.raw},
					},
				},
			}
			chains := map[string][]string{"MyFallback": {"A", "B"}}
			providers := map[string]string{
				"MyFallback": "baml-fallback",
				"A":          "openai",
				"B":          "anthropic",
			}
			chain, _, _, reason := ResolveFallbackChainForClientWithReason(reg, "MyFallback", chains, providers, IsProviderSupported)
			if chain != nil {
				t.Errorf("expected nil chain for invalid override; got %v", chain)
			}
			if reason != PathReasonInvalidStrategyOverride {
				t.Errorf("reason: got %q, want %q", reason, PathReasonInvalidStrategyOverride)
			}
		})
	}
}

func TestResolveFallbackChainForClientWithReason_AbsentOverrideKeepsIntrospected(t *testing.T) {
	// No runtime override → introspected chain is honoured. Regression
	// guard: the invalid-override detection must not trip on a registry
	// entry that lacks the strategy key (or for a client missing from
	// the registry entirely).
	cases := []struct {
		name string
		reg  *bamlutils.ClientRegistry
	}{
		{
			name: "no registry",
			reg:  nil,
		},
		{
			name: "registry without entry for client",
			reg: &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{Name: "Other", Provider: "openai"},
				},
			},
		},
		{
			name: "registry with entry but no strategy key",
			reg: &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{Name: "MyFallback", Options: map[string]any{"temperature": 0.5}},
				},
			},
		},
	}
	chains := map[string][]string{"MyFallback": {"A", "B"}}
	providers := map[string]string{
		"MyFallback": "baml-fallback",
		"A":          "openai",
		"B":          "anthropic",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain, _, _, reason := ResolveFallbackChainForClientWithReason(tc.reg, "MyFallback", chains, providers, IsProviderSupported)
			if reason != "" {
				t.Errorf("reason: got %q, want empty (drivable chain)", reason)
			}
			if len(chain) != 2 {
				t.Errorf("chain: got %v, want [A B]", chain)
			}
		})
	}
}

func TestResolveFallbackChainForClientWithReason_ValidOverrideHonoured(t *testing.T) {
	// A parseable override must take precedence over the introspected
	// chain. Regression guard: the invalid-override gate must only fire
	// when the parser refused to accept the value.
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{
				Name:     "MyFallback",
				Provider: "baml-fallback",
				Options:  map[string]any{"strategy": []string{"X", "Y"}},
			},
		},
	}
	chains := map[string][]string{"MyFallback": {"A", "B"}}
	providers := map[string]string{
		"MyFallback": "baml-fallback",
		"X":          "openai",
		"Y":          "anthropic",
		"A":          "openai",
		"B":          "anthropic",
	}
	chain, _, _, reason := ResolveFallbackChainForClientWithReason(reg, "MyFallback", chains, providers, IsProviderSupported)
	if reason != "" {
		t.Errorf("reason: got %q, want empty (valid override)", reason)
	}
	if len(chain) != 2 || chain[0] != "X" || chain[1] != "Y" {
		t.Errorf("chain: got %v, want [X Y]", chain)
	}
}

// TestResolveProviderWithReason_BareFallbackAlias verifies that
// BAML upstream's clientspec.rs:139-140 alias contract is honoured:
// both "fallback" and "baml-fallback" are accepted as the fallback
// strategy provider, and the BAML CLI init template emits the bare
// "fallback" form. Without alias normalisation a `provider fallback`
// client would fall through to the default switch arm and get
// classified as an unsupported single-provider, losing every
// BuildRequest-only capability for that client.
//
// The static introspector also folds the alias (canonicaliseProvider in
// cmd/introspect/main.go), but we test runtime classification here
// because the runtime path is what flips behaviour for
// client_registry overrides — exercising both seams individually.
func TestResolveProviderWithReason_BareFallbackAlias(t *testing.T) {
	t.Run("static introspected provider", func(t *testing.T) {
		// In production, the introspector's canonicaliseProvider folds
		// the alias before the runtime ever sees it. The
		// ResolveProviderWithReason classifier still normalises
		// defensively in case a code path bypasses introspection (e.g.
		// hand-built test maps, or a future static-config layer).
		adapter := &mockAdapter{Context: context.Background()}
		res := ResolveProviderWithReason(adapter, "MyFallback", "fallback", IsProviderSupported)
		if res.Strategy != "baml-fallback" {
			t.Errorf("strategy: got %q, want baml-fallback", res.Strategy)
		}
		if res.Path != "legacy" {
			t.Errorf("path: got %q, want legacy", res.Path)
		}
		if res.PathReason != "" {
			t.Errorf("reason: got %q, want empty (drivable fallback)", res.PathReason)
		}
	})
	t.Run("runtime registry override provider", func(t *testing.T) {
		adapter := &mockAdapter{
			Context: context.Background(),
			originalRegistry: &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{Name: "MyFallback", Provider: "fallback"},
				},
			},
		}
		res := ResolveProviderWithReason(adapter, "MyFallback", "openai", IsProviderSupported)
		if res.Strategy != "baml-fallback" {
			t.Errorf("strategy: got %q, want baml-fallback (runtime alias must normalise)", res.Strategy)
		}
		if res.Path != "legacy" {
			t.Errorf("path: got %q, want legacy", res.Path)
		}
	})
}

// TestResolveFallbackChainForClientWithReason_BareFallbackAlias verifies
// the chain-resolution seam normalises the alias too. Without this,
// the parentProvider check (`!= "baml-fallback"`) would early-return
// nil for a fallback client spelled "fallback" and the BuildRequest
// fallback chain would never be enumerated.
func TestResolveFallbackChainForClientWithReason_BareFallbackAlias(t *testing.T) {
	t.Run("static introspected", func(t *testing.T) {
		chains := map[string][]string{"MyFallback": {"A", "B"}}
		// Caller's introspected map uses the bare alias — simulates a
		// path that bypasses canonicaliseProvider (defensive
		// normalisation at the runtime seam).
		providers := map[string]string{
			"MyFallback": "fallback",
			"A":          "openai",
			"B":          "anthropic",
		}
		chain, _, _, reason := ResolveFallbackChainForClientWithReason(nil, "MyFallback", chains, providers, IsProviderSupported)
		if reason != "" {
			t.Errorf("reason: got %q, want empty (drivable chain)", reason)
		}
		if len(chain) != 2 || chain[0] != "A" || chain[1] != "B" {
			t.Errorf("chain: got %v, want [A B]", chain)
		}
	})
	t.Run("runtime registry override", func(t *testing.T) {
		reg := &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "MyFallback", Provider: "fallback"},
			},
		}
		chains := map[string][]string{"MyFallback": {"A", "B"}}
		providers := map[string]string{
			"MyFallback": "openai", // intentionally different — runtime alias must win
			"A":          "openai",
			"B":          "anthropic",
		}
		chain, _, _, reason := ResolveFallbackChainForClientWithReason(reg, "MyFallback", chains, providers, IsProviderSupported)
		if reason != "" {
			t.Errorf("reason: got %q, want empty (drivable chain)", reason)
		}
		if len(chain) != 2 {
			t.Errorf("chain: got %v, want [A B]", chain)
		}
	})
}

func TestNormalizeStrategyProvider_FoldsAliases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"baml-roundrobin", "baml-roundrobin"},
		{"baml-round-robin", "baml-roundrobin"},
		{"round-robin", "baml-roundrobin"},
		{"baml-fallback", "baml-fallback"},
		{"fallback", "baml-fallback"},
		{"openai", "openai"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeStrategyProvider(tc.in); got != tc.want {
				t.Errorf("normalizeStrategyProvider(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildLegacyMetadataPlanForClient_RRInvalidOverride covers the
// codegen-side metadata seam (codegen.go:2631-2643): the generated
// dispatcher reaches the legacy branch with __effective == the
// un-unwrapped RR client when ResolveEffectiveClient short-circuited
// an invalid strategy override (see ResolveEffectiveClient at
// orchestrator.go:344-365 and the translation of
// ErrInvalidStrategyOverride). The plan emitted there must surface
// PathReasonInvalidStrategyOverride so operators reading
// X-BAML-Path-Reason see the actual classification — without this,
// the request is correctly routed but the metadata reports the
// generic PathReasonRoundRobin "roundrobin-legacy-only" reason and
// hides the operator typo.
//
// ResolveProviderWithReason is exercised separately; this test pins
// the helper-level seam BuildLegacyMetadataPlanForClient, which has
// its own RR arm.
func TestBuildLegacyMetadataPlanForClient_RRInvalidOverride(t *testing.T) {
	for _, tc := range invalidStrategyOverrides() {
		t.Run(tc.name, func(t *testing.T) {
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{
						Name:     "MyRR",
						Provider: "baml-roundrobin",
						Options:  map[string]any{"strategy": tc.raw},
					},
				},
			}
			plan := BuildLegacyMetadataPlanForClient(
				reg,
				"MyRR",
				"baml-roundrobin",
				map[string][]string{"MyRR": {"A", "B"}},
				map[string]string{"MyRR": "baml-roundrobin", "A": "openai", "B": "anthropic"},
				IsProviderSupported,
				nil,
			)
			if plan.Path != "legacy" {
				t.Errorf("path: got %q, want legacy", plan.Path)
			}
			if plan.Strategy != "baml-roundrobin" {
				t.Errorf("strategy: got %q, want baml-roundrobin", plan.Strategy)
			}
			if plan.PathReason != PathReasonInvalidStrategyOverride {
				t.Errorf("reason: got %q, want %q (legacy plan must surface invalid-override over the RR-legacy default)", plan.PathReason, PathReasonInvalidStrategyOverride)
			}
		})
	}
}

// TestBuildLegacyMetadataPlanForClient_RRAbsentOverrideKeepsRoundRobinReason
// is the inverse-regression guard. A registry entry without a
// strategy key (or no registry at all) must continue to surface
// PathReasonRoundRobin — the invalid-override detection must not
// regress the standard RR-legacy classification. Mirrors the
// ResolveProviderWithReason guard one layer up.
func TestBuildLegacyMetadataPlanForClient_RRAbsentOverrideKeepsRoundRobinReason(t *testing.T) {
	cases := []struct {
		name string
		reg  *bamlutils.ClientRegistry
	}{
		{name: "no registry", reg: nil},
		{
			name: "registry without entry",
			reg: &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{Name: "Other", Provider: "openai"},
				},
			},
		},
		{
			name: "registry entry with no strategy key",
			reg: &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{Name: "MyRR", Options: map[string]any{"temperature": 0.5}},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := BuildLegacyMetadataPlanForClient(
				tc.reg,
				"MyRR",
				"baml-roundrobin",
				map[string][]string{"MyRR": {"A", "B"}},
				map[string]string{"MyRR": "baml-roundrobin", "A": "openai", "B": "anthropic"},
				IsProviderSupported,
				nil,
			)
			if plan.PathReason != PathReasonRoundRobin {
				t.Errorf("reason: got %q, want %q (absent override must yield RR-legacy reason)", plan.PathReason, PathReasonRoundRobin)
			}
		})
	}
}

// TestBuildLegacyMetadataPlanForClient_FallbackInvalidOverride covers
// the same emitted-metadata seam for fallback strategies. The fallback
// arm delegates to ResolveFallbackChainForClientWithReason which
// already returns PathReasonInvalidStrategyOverride; this regression
// test pins that contract end-to-end through the legacy plan builder
// so a future refactor can't accidentally drop the reason on the
// floor (e.g. by overwriting plan.PathReason after the chain returns).
func TestBuildLegacyMetadataPlanForClient_FallbackInvalidOverride(t *testing.T) {
	for _, tc := range invalidStrategyOverrides() {
		t.Run(tc.name, func(t *testing.T) {
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{
						Name:     "MyFallback",
						Provider: "baml-fallback",
						Options:  map[string]any{"strategy": tc.raw},
					},
				},
			}
			plan := BuildLegacyMetadataPlanForClient(
				reg,
				"MyFallback",
				"baml-fallback",
				map[string][]string{"MyFallback": {"A", "B"}},
				map[string]string{"MyFallback": "baml-fallback", "A": "openai", "B": "anthropic"},
				IsProviderSupported,
				nil,
			)
			if plan.Strategy != "baml-fallback" {
				t.Errorf("strategy: got %q, want baml-fallback", plan.Strategy)
			}
			if plan.PathReason != PathReasonInvalidStrategyOverride {
				t.Errorf("reason: got %q, want %q", plan.PathReason, PathReasonInvalidStrategyOverride)
			}
		})
	}
}

// TestBuildLegacyMetadataPlan_RuntimeClientWithoutChainDoesNotLeakDefaultChain
// pins the metadata contract: when a primary override points the
// request at a fallback client whose chain cannot be resolved
// (invalid strategy override or empty introspected chain), the
// rebuild block in BuildLegacyMetadataPlan must NOT fall through to
// `defaultClientName`'s introspected chain. Doing so would emit
// metadata with `Client=<runtime client>` but
// `Chain=<default client's chain>` — a mismatch that misleads
// operators reading X-BAML-Path-Reason / metadata payloads when
// debugging a runtime override.
//
// The plan leaves Chain empty when the runtime-resolved client has
// no chain, so Client and Chain always describe the same client.
func TestBuildLegacyMetadataPlan_RuntimeClientWithoutChainDoesNotLeakDefaultChain(t *testing.T) {
	cases := []struct {
		name           string
		overrideClient *bamlutils.ClientProperty
	}{
		{
			name: "invalid strategy override on runtime client",
			overrideClient: &bamlutils.ClientProperty{
				Name:     "OverrideStrategy",
				Provider: "baml-fallback",
				Options:  map[string]any{"strategy": "[]"},
			},
		},
		{
			name: "runtime client with no chain in introspected map",
			overrideClient: &bamlutils.ClientProperty{
				Name:     "OverrideStrategy",
				Provider: "baml-fallback",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primary := "OverrideStrategy"
			adapter := &mockAdapter{
				Context: context.Background(),
				originalRegistry: &bamlutils.ClientRegistry{
					Primary: &primary,
					Clients: []*bamlutils.ClientProperty{tc.overrideClient},
				},
			}
			// DefaultStrategy has a chain in the introspected map;
			// OverrideStrategy does not (or has an invalid override).
			// The previous behaviour leaked DefaultA/DefaultB into the
			// emitted plan even though the request resolved to
			// OverrideStrategy.
			fallbackChains := map[string][]string{
				"DefaultStrategy": {"DefaultA", "DefaultB"},
			}
			providers := map[string]string{
				"DefaultStrategy":  "baml-fallback",
				"OverrideStrategy": "baml-fallback",
				"DefaultA":         "openai",
				"DefaultB":         "anthropic",
			}
			plan := BuildLegacyMetadataPlan(
				adapter,
				"DefaultStrategy",
				"baml-fallback",
				fallbackChains,
				providers,
				IsProviderSupported,
				nil,
			)
			if plan.Client != "OverrideStrategy" {
				t.Errorf("client: got %q, want OverrideStrategy", plan.Client)
			}
			if len(plan.Chain) != 0 {
				t.Errorf("chain: got %v, want empty (default client's chain must not leak when runtime client has none)", plan.Chain)
			}
			if len(plan.LegacyChildren) != 0 {
				t.Errorf("legacyChildren: got %v, want empty (must not be populated from leaked chain)", plan.LegacyChildren)
			}
		})
	}
}

// TestBuildLegacyMetadataPlan_RuntimeClientWithChainStillEmitsChain is
// the inverse-regression guard for runtime clients with their own
// resolvable chain. When the runtime client DOES have a resolvable
// chain, the plan must still describe it — dropping the
// defaultClientName fallback must not regress the common "primary
// override points at a different valid fallback client" case.
func TestBuildLegacyMetadataPlan_RuntimeClientWithChainStillEmitsChain(t *testing.T) {
	primary := "OverrideStrategy"
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &primary,
		},
	}
	fallbackChains := map[string][]string{
		"DefaultStrategy":  {"DefaultA", "DefaultB"},
		"OverrideStrategy": {"OverrideA", "OverrideB"},
	}
	providers := map[string]string{
		"DefaultStrategy":  "baml-fallback",
		"OverrideStrategy": "baml-fallback",
		"DefaultA":         "openai",
		"DefaultB":         "anthropic",
		"OverrideA":        "openai",
		"OverrideB":        "anthropic",
	}
	plan := BuildLegacyMetadataPlan(
		adapter,
		"DefaultStrategy",
		"baml-fallback",
		fallbackChains,
		providers,
		IsProviderSupported,
		nil,
	)
	if plan.Client != "OverrideStrategy" {
		t.Errorf("client: got %q, want OverrideStrategy", plan.Client)
	}
	if len(plan.Chain) != 2 || plan.Chain[0] != "OverrideA" || plan.Chain[1] != "OverrideB" {
		t.Errorf("chain: got %v, want [OverrideA OverrideB]", plan.Chain)
	}
}

// TestResolveProviderWithReason_PresentEmptyProvider pins that a
// runtime client_registry entry which explicitly sends
// "provider":"" must surface PathReasonInvalidProviderOverride and
// route to legacy. BAML upstream's ClientProvider::from_str rejects
// empty provider strings (clientspec.rs:119-144); silently falling
// through to the introspected provider would hide the malformed
// override from BAML and from operators reading X-BAML-Path-Reason.
func TestResolveProviderWithReason_PresentEmptyProvider(t *testing.T) {
	t.Run("named-client override", func(t *testing.T) {
		adapter := &mockAdapter{
			Context: context.Background(),
			originalRegistry: &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{Name: "MyClient", Provider: "", ProviderSet: true},
				},
			},
		}
		res := ResolveProviderWithReason(adapter, "MyClient", "openai", IsProviderSupported)
		if res.Path != "legacy" {
			t.Errorf("path: got %q, want legacy", res.Path)
		}
		if res.PathReason != PathReasonInvalidProviderOverride {
			t.Errorf("reason: got %q, want %q", res.PathReason, PathReasonInvalidProviderOverride)
		}
	})
	t.Run("primary override", func(t *testing.T) {
		primary := "MyPrimary"
		adapter := &mockAdapter{
			Context: context.Background(),
			originalRegistry: &bamlutils.ClientRegistry{
				Primary: &primary,
				Clients: []*bamlutils.ClientProperty{
					{Name: "MyPrimary", Provider: "", ProviderSet: true},
				},
			},
		}
		res := ResolveProviderWithReason(adapter, "MyClient", "openai", IsProviderSupported)
		if res.PathReason != PathReasonInvalidProviderOverride {
			t.Errorf("reason: got %q, want %q", res.PathReason, PathReasonInvalidProviderOverride)
		}
	})
}

// TestResolveProviderWithReason_PrimarySetDefaultClientPresentEmpty
// pins the classifier's defaultClientName checkpoint: when
// reg.Primary is set but has no `clients[]` entry (or the entry
// omits the provider key) and the function's defaultClientName entry
// carries an explicit "provider":"", ResolveProvider falls through
// to the default-client lookup and returns "". The classifier must
// recognise this as the invalid-override case rather than reporting
// PathReasonEmptyProvider — without the defaultClientName checkpoint
// both other checkpoints reduce to the primary key, which has no
// entry.
func TestResolveProviderWithReason_PrimarySetDefaultClientPresentEmpty(t *testing.T) {
	cases := []struct {
		name    string
		clients []*bamlutils.ClientProperty
	}{
		{
			name: "no primary entry at all",
			clients: []*bamlutils.ClientProperty{
				{Name: "DefaultClient", Provider: "", ProviderSet: true},
			},
		},
		{
			name: "primary entry with provider omitted",
			clients: []*bamlutils.ClientProperty{
				{Name: "PrimaryClient"}, // presence-only, no provider key
				{Name: "DefaultClient", Provider: "", ProviderSet: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primary := "PrimaryClient"
			adapter := &mockAdapter{
				Context: context.Background(),
				originalRegistry: &bamlutils.ClientRegistry{
					Primary: &primary,
					Clients: tc.clients,
				},
			}
			res := ResolveProviderWithReason(adapter, "DefaultClient", "openai", IsProviderSupported)
			if res.Path != "legacy" {
				t.Errorf("path: got %q, want legacy", res.Path)
			}
			if res.PathReason != PathReasonInvalidProviderOverride {
				t.Errorf("reason: got %q, want %q", res.PathReason, PathReasonInvalidProviderOverride)
			}
		})
	}
}

// TestBuildLegacyMetadataPlan_PrimarySetDefaultClientPresentEmpty pins
// the same fix end-to-end through BuildLegacyMetadataPlan, which is
// the helper the verdict identified as the affected exported API
// (BuildLegacyMetadataPlanForClient is the codegen-side path and
// receives the resolved client name directly, so it doesn't have the
// primary-vs-default ambiguity). Belt-and-suspenders coverage.
func TestBuildLegacyMetadataPlan_PrimarySetDefaultClientPresentEmpty(t *testing.T) {
	primary := "PrimaryClient"
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &primary,
			Clients: []*bamlutils.ClientProperty{
				{Name: "DefaultClient", Provider: "", ProviderSet: true},
			},
		},
	}
	plan := BuildLegacyMetadataPlan(adapter, "DefaultClient", "openai", nil, nil, IsProviderSupported, nil)
	if plan.Path != "legacy" {
		t.Errorf("path: got %q, want legacy", plan.Path)
	}
	if plan.PathReason != PathReasonInvalidProviderOverride {
		t.Errorf("reason: got %q, want %q", plan.PathReason, PathReasonInvalidProviderOverride)
	}
}

// TestResolveProviderWithReason_AbsentProviderKeepsIntrospected is
// the inverse-regression guard. A registry entry that omits the
// provider key (strategy-only override, presence-only dynamic entry)
// must keep using the introspected provider — preserving the
// existing flows that drive RR strategy-only and presence-only
// behaviour. Without this guard the present-empty handling could
// regress TestResolve_StrategyOnlyOverride_IsDynamic and
// TestResolve_RegistryPresenceWithoutOverride_IsDynamic in the RR
// package by routing absent-provider entries to legacy alongside
// present-empty entries.
func TestResolveProviderWithReason_AbsentProviderKeepsIntrospected(t *testing.T) {
	cases := []struct {
		name     string
		client   *bamlutils.ClientProperty
		wantPath string
	}{
		{
			name:     "presence-only entry",
			client:   &bamlutils.ClientProperty{Name: "MyClient"},
			wantPath: "buildrequest",
		},
		{
			name: "strategy-only override",
			client: &bamlutils.ClientProperty{
				Name:    "MyClient",
				Options: map[string]any{"strategy": []any{"A", "B"}},
			},
			wantPath: "buildrequest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &mockAdapter{
				Context: context.Background(),
				originalRegistry: &bamlutils.ClientRegistry{
					Clients: []*bamlutils.ClientProperty{tc.client},
				},
			}
			res := ResolveProviderWithReason(adapter, "MyClient", "openai", IsProviderSupported)
			if res.Path != tc.wantPath {
				t.Errorf("path: got %q, want %q", res.Path, tc.wantPath)
			}
			if res.Provider != "openai" {
				t.Errorf("provider: got %q, want openai (introspected fallback)", res.Provider)
			}
		})
	}
}

// TestResolveClientProvider_PresentEmptyReturnsEmpty pins the resolver
// helper behaviour the codegen-side BuildRequest gate depends on. A
// present-empty override must surface as "" so IsProviderSupported
// returns false and the dispatcher falls through to legacy. An absent
// override must continue to fall through to the introspected provider
// so strategy-only and presence-only flows keep working.
func TestResolveClientProvider_PresentEmptyReturnsEmpty(t *testing.T) {
	intro := map[string]string{"MyClient": "openai"}

	t.Run("present-empty surfaces as empty", func(t *testing.T) {
		reg := &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "MyClient", Provider: "", ProviderSet: true},
			},
		}
		got := ResolveClientProvider(reg, "MyClient", intro)
		if got != "" {
			t.Errorf("present-empty: got %q, want empty (introspected must NOT leak in)", got)
		}
	})

	t.Run("absent provider falls through to introspected", func(t *testing.T) {
		reg := &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "MyClient"}, // no provider, no ProviderSet
			},
		}
		got := ResolveClientProvider(reg, "MyClient", intro)
		if got != "openai" {
			t.Errorf("absent: got %q, want openai (introspected fallback)", got)
		}
	})

	t.Run("present non-empty wins", func(t *testing.T) {
		reg := &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "MyClient", Provider: "anthropic"},
			},
		}
		got := ResolveClientProvider(reg, "MyClient", intro)
		if got != "anthropic" {
			t.Errorf("present non-empty: got %q, want anthropic", got)
		}
	})

	t.Run("no registry entry falls through to introspected", func(t *testing.T) {
		reg := &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "OtherClient", Provider: "anthropic"},
			},
		}
		got := ResolveClientProvider(reg, "MyClient", intro)
		if got != "openai" {
			t.Errorf("no entry: got %q, want openai", got)
		}
	})
}

// TestBuildLegacyMetadataPlanForClient_PresentEmptyProvider covers the
// codegen-side metadata seam. The generated dispatcher reaches
// BuildLegacyMetadataPlanForClient when the BuildRequest gate failed,
// which for a present-empty override happens because
// ResolveClientProvider now returns "" instead of falling through to
// the introspected provider. The emitted plan must report
// PathReasonInvalidProviderOverride so operators reading
// X-BAML-Path-Reason see the actual cause.
func TestBuildLegacyMetadataPlanForClient_PresentEmptyProvider(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "MyClient", Provider: "", ProviderSet: true},
		},
	}
	plan := BuildLegacyMetadataPlanForClient(
		reg,
		"MyClient",
		"openai",
		nil,
		map[string]string{"MyClient": "openai"},
		IsProviderSupported,
		nil,
	)
	if plan.Path != "legacy" {
		t.Errorf("path: got %q, want legacy", plan.Path)
	}
	if plan.PathReason != PathReasonInvalidProviderOverride {
		t.Errorf("reason: got %q, want %q", plan.PathReason, PathReasonInvalidProviderOverride)
	}
	if plan.Provider != "" {
		t.Errorf("provider: got %q, want empty (introspected must not leak in)", plan.Provider)
	}
}

// TestBuildLegacyMetadataPlanForClient_AbsentProviderKeepsEmptyProviderReason
// is the inverse-regression guard. A client with no provider configured
// anywhere must continue to report PathReasonEmptyProvider, not the
// new PathReasonInvalidProviderOverride. This test is the boundary
// between the two reasons.
func TestBuildLegacyMetadataPlanForClient_AbsentProviderKeepsEmptyProviderReason(t *testing.T) {
	plan := BuildLegacyMetadataPlanForClient(
		nil,
		"MyClient",
		"",
		nil,
		nil,
		IsProviderSupported,
		nil,
	)
	if plan.PathReason != PathReasonEmptyProvider {
		t.Errorf("reason: got %q, want %q", plan.PathReason, PathReasonEmptyProvider)
	}
}

// invalidStartOverrides enumerates the runtime client_registry shapes
// that inspectStartOverride refuses (per BAML upstream's ensure_int
// contract). Reused across the resolver-level (already in
// resolver_test.go) and the orchestrator-level metadata tests below
// so both classifications stay in lockstep on which inputs are
// considered "present-but-invalid".
//
// Cold-review-3 finding 3: each of these must surface as
// PathReasonInvalidRoundRobinStartOverride from the metadata
// classifier — the resolver-side counterpart returns
// ErrInvalidStartOverride.
func invalidStartOverrides() []struct {
	name string
	raw  any
} {
	return []struct {
		name string
		raw  any
	}{
		{"numeric string", "1"},
		{"empty string", ""},
		{"fractional float", 1.5},
		{"boolean true", true},
		{"slice", []any{1, 2}},
		{"map", map[string]any{"x": 1}},
		// Align this metadata-side table with the resolver-side
		// rejection matrix (resolver_test.go:912-920) — unsigned
		// ints and json.Number must surface the same canonical
		// PathReasonInvalidRoundRobinStartOverride classification,
		// not silently fall back through.
		{"uint", uint(1)},
		{"json.Number numeric", json.Number("5")},
		{"json.Number malformed", json.Number("abc")},
	}
}

// TestResolveProviderWithReason_RoundRobinInvalidStartOverride pins
// the metadata classifier contract for invalid `options.start`. A
// runtime client_registry entry on a baml-roundrobin client whose
// start value is not parseable as an integer must surface
// PathReasonInvalidRoundRobinStartOverride so operators reading
// X-BAML-Path-Reason can distinguish a malformed start option from a
// malformed strategy option (PathReasonInvalidStrategyOverride) or
// the generic RR-legacy reason.
func TestResolveProviderWithReason_RoundRobinInvalidStartOverride(t *testing.T) {
	for _, tc := range invalidStartOverrides() {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &mockAdapter{
				Context: context.Background(),
				originalRegistry: &bamlutils.ClientRegistry{
					Clients: []*bamlutils.ClientProperty{
						{
							Name:     "MyRR",
							Provider: "baml-roundrobin",
							Options:  map[string]any{"start": tc.raw},
						},
					},
				},
			}
			res := ResolveProviderWithReason(adapter, "MyRR", "baml-roundrobin", IsProviderSupported)
			if res.Path != "legacy" {
				t.Errorf("path: got %q, want legacy", res.Path)
			}
			if res.PathReason != PathReasonInvalidRoundRobinStartOverride {
				t.Errorf("reason: got %q, want %q", res.PathReason, PathReasonInvalidRoundRobinStartOverride)
			}
			if res.Strategy != "baml-roundrobin" {
				t.Errorf("strategy: got %q, want baml-roundrobin", res.Strategy)
			}
		})
	}
}

// TestResolveProviderWithReason_RoundRobinInvalidStrategyTakesPrecedence
// pins the priority when BOTH `strategy` and `start` are malformed:
// strategy is checked first since BAML upstream parses it before
// start. Without this guard a future refactor could silently emit
// PathReasonInvalidRoundRobinStartOverride for a request whose
// underlying break is the strategy chain.
func TestResolveProviderWithReason_RoundRobinInvalidStrategyTakesPrecedence(t *testing.T) {
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "MyRR",
					Provider: "baml-roundrobin",
					Options: map[string]any{
						"strategy": "ClientA",  // bare token — invalid
						"start":    "not an int", // also invalid
					},
				},
			},
		},
	}
	res := ResolveProviderWithReason(adapter, "MyRR", "baml-roundrobin", IsProviderSupported)
	if res.PathReason != PathReasonInvalidStrategyOverride {
		t.Errorf("reason: got %q, want %q (strategy takes precedence over start)", res.PathReason, PathReasonInvalidStrategyOverride)
	}
}

// TestBuildLegacyMetadataPlanForClient_RRInvalidStartOverride covers
// the codegen-side metadata seam for finding 3. The generated
// dispatcher reaches BuildLegacyMetadataPlanForClient after
// ResolveEffectiveClient short-circuited an invalid start override by
// returning the un-unwrapped client name; the emitted plan must
// report PathReasonInvalidRoundRobinStartOverride.
func TestBuildLegacyMetadataPlanForClient_RRInvalidStartOverride(t *testing.T) {
	for _, tc := range invalidStartOverrides() {
		t.Run(tc.name, func(t *testing.T) {
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{
					{
						Name:     "MyRR",
						Provider: "baml-roundrobin",
						Options:  map[string]any{"start": tc.raw},
					},
				},
			}
			plan := BuildLegacyMetadataPlanForClient(
				reg,
				"MyRR",
				"baml-roundrobin",
				map[string][]string{"MyRR": {"A", "B"}},
				map[string]string{"MyRR": "baml-roundrobin", "A": "openai", "B": "anthropic"},
				IsProviderSupported,
				nil,
			)
			if plan.Path != "legacy" {
				t.Errorf("path: got %q, want legacy", plan.Path)
			}
			if plan.PathReason != PathReasonInvalidRoundRobinStartOverride {
				t.Errorf("reason: got %q, want %q", plan.PathReason, PathReasonInvalidRoundRobinStartOverride)
			}
			if plan.Strategy != "baml-roundrobin" {
				t.Errorf("strategy: got %q, want baml-roundrobin", plan.Strategy)
			}
		})
	}
}

// TestResolveEffectiveClient_InvalidStartTranslatesToLegacy verifies
// that ResolveEffectiveClient mirrors its existing
// ErrInvalidStrategyOverride handling for ErrInvalidStartOverride —
// both sentinels translate to "return the un-unwrapped client name
// with no error", letting the codegen support gate fail and the
// request fall to legacy where BAML emits the canonical options
// error.
func TestResolveEffectiveClient_InvalidStartTranslatesToLegacy(t *testing.T) {
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "MyRR",
					Provider: "baml-roundrobin",
					Options:  map[string]any{"start": "not an int"},
				},
			},
		},
	}
	effective, rrInfo, err := ResolveEffectiveClient(
		adapter,
		"MyRR",
		map[string][]string{"MyRR": {"A", "B"}},
		map[string]string{"MyRR": "baml-roundrobin", "A": "openai", "B": "anthropic"},
		nil, // advancer not consulted for invalid-start case
	)
	if err != nil {
		t.Fatalf("expected nil error (sentinel translated to legacy fallthrough), got %v", err)
	}
	if effective != "MyRR" {
		t.Errorf("effective: got %q, want MyRR (un-unwrapped strategy name)", effective)
	}
	if rrInfo != nil {
		t.Errorf("rrInfo: got %+v, want nil (no RR decision occurred)", rrInfo)
	}
}

// TestBuildLegacyMetadataPlanForClient_RRValidStartKeepsRoundRobinReason
// is the inverse-regression guard for finding 3. A valid integer
// `start` must not trip the new path reason — RR clients with valid
// runtime configuration that reach the legacy plan (older adapters,
// resolver errors) should still surface PathReasonRoundRobin.
func TestBuildLegacyMetadataPlanForClient_RRValidStartKeepsRoundRobinReason(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"start": 1},
			},
		},
	}
	plan := BuildLegacyMetadataPlanForClient(
		reg,
		"MyRR",
		"baml-roundrobin",
		map[string][]string{"MyRR": {"A", "B"}},
		map[string]string{"MyRR": "baml-roundrobin", "A": "openai", "B": "anthropic"},
		IsProviderSupported,
		nil,
	)
	if plan.PathReason != PathReasonRoundRobin {
		t.Errorf("reason: got %q, want %q", plan.PathReason, PathReasonRoundRobin)
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
