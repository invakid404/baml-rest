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

// TestResolveProviderWithReason_BareFallbackAlias covers PR #192
// cold-review-2 finding 2: BAML upstream's clientspec.rs:139-140
// accepts both "fallback" and "baml-fallback" as the fallback strategy
// provider, and the BAML CLI init template emits the bare "fallback"
// form. Without alias normalisation a `provider fallback` client would
// fall through to the default switch arm and get classified as an
// unsupported single-provider, losing every BuildRequest-only
// capability for that client.
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
// codegen-side metadata seam (codegen.go:2631-2643) for PR #192
// cold-review-2 finding 1. The generated dispatcher reaches the
// legacy branch with __effective == the un-unwrapped RR client when
// ResolveEffectiveClient short-circuited an invalid strategy override
// (see ResolveEffectiveClient at orchestrator.go:344-365 and the
// translation of ErrInvalidStrategyOverride). The plan emitted there
// must surface PathReasonInvalidStrategyOverride so operators
// reading X-BAML-Path-Reason see the actual classification — without
// this, the request is correctly routed but the metadata reports the
// generic PathReasonRoundRobin "roundrobin-legacy-only" reason and
// hides the operator typo.
//
// Codex sign-off-5 explicitly called out the missing helper-level
// regression for this seam: ResolveProviderWithReason is correctly
// patched, but the generated legacy dispatch uses
// BuildLegacyMetadataPlanForClient which has its own RR arm.
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
