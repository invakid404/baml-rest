package buildrequest

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

func TestResolveFallbackChain_AllSupported(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}
	if providers["ClientA"] != "openai" || providers["ClientB"] != "anthropic" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected empty legacyChildren for all-supported chain, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_MixedSupport(t *testing.T) {
	// Chain [openai, aws-bedrock, anthropic] with only openai + anthropic
	// supported by BuildRequest. Expect a 3-element chain back with
	// legacyChildren marking the aws-bedrock child.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB", "ClientC"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "aws-bedrock", // unsupported
		"ClientC":    "anthropic",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 3 {
		t.Fatalf("expected chain length 3 for mixed-mode, got %d", len(chain))
	}
	if providers["ClientA"] != "openai" || providers["ClientB"] != "aws-bedrock" || providers["ClientC"] != "anthropic" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if !legacyChildren["ClientB"] {
		t.Errorf("expected ClientB to be marked legacy, got legacyChildren=%v", legacyChildren)
	}
	if legacyChildren["ClientA"] || legacyChildren["ClientC"] {
		t.Errorf("expected only ClientB to be legacy, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_AllUnsupported(t *testing.T) {
	// When every child is unsupported, ResolveFallbackChain returns nil so
	// the caller routes the whole chain through the existing legacy path.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "aws-bedrock",
		"ClientB":    "aws-bedrock",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil results when every child is unsupported, got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_DuplicateLegacyChildren(t *testing.T) {
	// A chain listing the same unsupported client twice is still "all
	// legacy" from a routing standpoint — there is no supported child for
	// BuildRequest to drive, so the resolver must return nil so the whole
	// chain routes through the legacy path. The previous implementation
	// compared len(legacyChildren) (a set, 1) against len(chain)
	// (positions, 2) and incorrectly reported this as a mixed chain.
	fallbackChains := map[string][]string{
		"MyFallback": {"BedrockA", "BedrockA"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"BedrockA":   "aws-bedrock",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil results when every chain position is legacy (duplicates included), got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_EmptyProviderFallsBack(t *testing.T) {
	// A child with a missing provider is fatal — we can't route an unknown
	// provider through either BuildRequest or legacy reliably, so the
	// whole chain should fall back to the legacy path.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		// ClientB intentionally missing
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil results when a child's provider is empty, got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeOverrideChild(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	// Introspected: ClientA=openai, ClientB=anthropic
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	// Runtime override changes ClientB from anthropic to google-ai
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "ClientB", Provider: "google-ai"},
			},
		},
	}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" || p == "google-ai" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}
	// ClientB should use the runtime override, not the introspected value
	if providers["ClientB"] != "google-ai" {
		t.Errorf("expected ClientB provider 'google-ai' (runtime override), got %q", providers["ClientB"])
	}
	if providers["ClientA"] != "openai" {
		t.Errorf("expected ClientA provider 'openai' (introspected), got %q", providers["ClientA"])
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeOverrideMakesLegacy(t *testing.T) {
	// All introspected providers are supported; runtime override flips one
	// child to aws-bedrock, turning this into a mixed-mode chain rather
	// than falling back entirely to the legacy path.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "ClientB", Provider: "aws-bedrock"},
			},
		},
	}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2 for mixed-mode, got %d", len(chain))
	}
	if !legacyChildren["ClientB"] {
		t.Errorf("expected ClientB to be legacy after runtime override, got %v", legacyChildren)
	}
	if legacyChildren["ClientA"] {
		t.Errorf("expected ClientA to remain BuildRequest-driven, got %v", legacyChildren)
	}
	if providers["ClientB"] != "aws-bedrock" {
		t.Errorf("expected ClientB provider 'aws-bedrock' in providers map, got %q", providers["ClientB"])
	}
}

func TestResolveFallbackChain_RuntimeStrategyOverrideReordersChildren(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "MyFallback",
					Provider: "baml-fallback",
					Options: map[string]any{
						"strategy": []any{"ClientB", "ClientA"},
					},
				},
			},
		},
	}

	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}
	if chain[0] != "ClientB" || chain[1] != "ClientA" {
		t.Fatalf("expected runtime strategy order [ClientB ClientA], got %v", chain)
	}
	if providers["ClientB"] != "anthropic" || providers["ClientA"] != "openai" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children for all-supported chain, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeStrategyOverrideWithoutIntrospectedChain(t *testing.T) {
	fallbackChains := map[string][]string{}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "MyFallback",
					Provider: "baml-fallback",
					Options: map[string]any{
						"strategy": "strategy [ClientB, ClientA]",
					},
				},
			},
		},
	}

	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected runtime-defined chain length 2, got %d", len(chain))
	}
	if chain[0] != "ClientB" || chain[1] != "ClientA" {
		t.Fatalf("expected runtime strategy order [ClientB ClientA], got %v", chain)
	}
	if providers["ClientB"] != "anthropic" || providers["ClientA"] != "openai" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children for all-supported chain, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeQuotedStringStrategyOverride(t *testing.T) {
	fallbackChains := map[string][]string{}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{
					Name:     "MyFallback",
					Provider: "baml-fallback",
					Options: map[string]any{
						"strategy": `strategy ["ClientB", "ClientA"]`,
					},
				},
			},
		},
	}

	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected runtime-defined chain length 2, got %d", len(chain))
	}
	if chain[0] != "ClientB" || chain[1] != "ClientA" {
		t.Fatalf("expected runtime strategy order [ClientB ClientA], got %v", chain)
	}
	if providers["ClientB"] != "anthropic" || providers["ClientA"] != "openai" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children for all-supported chain, got %v", legacyChildren)
	}
}

func TestResolveFallbackChain_RuntimeOverrideMakesAllLegacy(t *testing.T) {
	// Runtime override flips every child to an unsupported provider →
	// full legacy fallback (nil results), matching the all-unsupported case.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "ClientA", Provider: "aws-bedrock"},
				{Name: "ClientB", Provider: "aws-bedrock"},
			},
		},
	}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil when every child is legacy, got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_NotFallback(t *testing.T) {
	fallbackChains := map[string][]string{}
	clientProviders := map[string]string{"GPT4": "openai"}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "GPT4", fallbackChains, clientProviders,
		func(p string) bool { return true },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil for non-fallback client, got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_RoundRobinChildSkipped(t *testing.T) {
	// ResolveFallbackChain only classifies baml-fallback parents. A
	// baml-roundrobin parent returns nil,nil,nil — not because RR is
	// "legacy-only" globally (top-level RR is resolved upstream by
	// ResolveEffectiveClient for modern adapters with SupportsWithClient),
	// but because this helper is the fallback-chain classifier. RR is
	// handled either by the upstream resolver (modern) or by BAML's
	// runtime under the legacy path (older adapters / nested RR children
	// inside a fallback chain).
	fallbackChains := map[string][]string{
		"MyRoundRobin": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyRoundRobin": "baml-roundrobin",
		"ClientA":      "openai",
		"ClientB":      "anthropic",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyRoundRobin", fallbackChains, clientProviders,
		func(p string) bool { return true },
	)

	if chain != nil || providers != nil || legacyChildren != nil {
		t.Errorf("expected nil for baml-roundrobin parent (classified elsewhere), got chain=%v providers=%v legacy=%v", chain, providers, legacyChildren)
	}
}

func TestResolveFallbackChain_FallbackWithRRChild_SurfacesReason(t *testing.T) {
	// A fallback chain whose immediate baml-roundrobin child has a
	// BR-supported selected leaf is centrally unwrapped to that leaf
	// (issue #237 PR 2). The 4-tuple wrapper surfaces the change as
	// (a) the RR wrapper child REMOVED from legacyChildren, (b)
	// providers[child] rewritten to the leaf provider so the
	// orchestrator's IsProviderSupported gate sees the leaf, and (c)
	// reason == PathReasonFallbackRoundRobinChildBuildRequest. The
	// per-child Targets / NestedRoundRobin information requires the
	// typed ResolveFallbackChainPlanForClient helper — the 4-tuple
	// wrapper deliberately loses it for codegen-compat.
	fallbackChains := map[string][]string{
		"MyFallback": {"OpenAIClient", "InnerRR"},
		"InnerRR":    {"GoogleA", "GoogleB"},
	}
	clientProviders := map[string]string{
		"MyFallback":   "baml-fallback",
		"OpenAIClient": "openai",
		"InnerRR":      "baml-roundrobin",
		"GoogleA":      "google-ai",
		"GoogleB":      "google-ai",
	}

	chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
		nil, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool {
			return p == "openai" || p == "google-ai" || p == "anthropic"
		},
	)

	if chain == nil {
		t.Fatalf("expected chain to resolve, got nil with reason=%q", reason)
	}
	if reason != PathReasonFallbackRoundRobinChildBuildRequest {
		t.Fatalf("reason: got %q, want %q (centralized RR child should surface ChildBuildRequest)",
			reason, PathReasonFallbackRoundRobinChildBuildRequest)
	}
	if legacy["InnerRR"] {
		t.Errorf("InnerRR must NOT be marked legacy after centralization, legacyChildren=%v", legacy)
	}
	if legacy["OpenAIClient"] {
		t.Errorf("OpenAIClient is a supported provider — must not be on legacy list, legacyChildren=%v", legacy)
	}
	if providers["InnerRR"] != "google-ai" {
		t.Errorf("providers[InnerRR]: got %q, want google-ai (leaf provider after centralization)", providers["InnerRR"])
	}
}

func TestResolveFallbackChain_FallbackWithRRChild_AliasSpellings(t *testing.T) {
	// Resolver-level coverage for the non-canonical RR spellings the
	// alias classifier must fold before deciding whether the child
	// contributes PathReasonFallbackRoundRobinChildLegacy. The
	// canonical "baml-roundrobin" form is exercised by
	// TestResolveFallbackChain_FallbackWithRRChild above; this table
	// pins the two alias forms (BAML upstream's hyphenated
	// "baml-round-robin" and the bare "round-robin" shorthand).
	// Both alias spellings must hit the resolver's RR-child path.
	// (Metadata-plan-level coverage of all three spellings exists in
	// TestBuildFallbackChainPlan_ReasonPlumbsFromResolver.)
	cases := []struct {
		name     string
		provider string
	}{
		{name: "baml-round-robin", provider: "baml-round-robin"},
		{name: "round-robin", provider: "round-robin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fallbackChains := map[string][]string{
				"MyFallback": {"A", "InnerRR"},
			}
			clientProviders := map[string]string{
				"MyFallback": "baml-fallback",
				"A":          "openai",
				"InnerRR":    tc.provider,
			}

			_, providers, _, reason := ResolveFallbackChainForClientWithReason(
				nil, "MyFallback", fallbackChains, clientProviders,
				func(p string) bool { return p == "openai" },
			)
			if reason != PathReasonFallbackRoundRobinChildLegacy {
				t.Fatalf("reason with alias %q: got %q, want %q", tc.provider, reason, PathReasonFallbackRoundRobinChildLegacy)
			}
			// providers map must surface the canonical "baml-roundrobin"
			// spelling regardless of which alias the caller's
			// clientProviders used. ResolveClientProvider applies
			// normalizeStrategyProvider, but a regression that dropped
			// the normalisation on one path could still yield the
			// right reason while leaking the alias here.
			if got := providers["InnerRR"]; got != "baml-roundrobin" {
				t.Errorf("providers[InnerRR] with alias %q: got %q, want baml-roundrobin (canonical)", tc.provider, got)
			}
		})
	}
}

// TestResolveFallbackChain_NestedInvalidStrategyOverride pins the
// invalid-nested preflight: a fallback chain whose nested RR child
// has a present-but-malformed runtime `strategy` override must abort
// chain resolution with PathReasonInvalidStrategyOverride. The caller
// (codegen `len(chain) > 0` gate) then routes the whole request to
// top-level legacy, where BAML's eager registry parse emits the
// canonical ensure_strategy error against the full runtime registry.
//
// Without this preflight, the mixed-mode legacy callback for InnerRR
// would forward the invalid entry into BAML inside the per-child
// callback, BAML's rejection would be re-classified as an ordinary
// fallback child failure, and a later successful sibling could
// silently mask the operator's misconfiguration — exactly the
// validation suppression the top-level invalid-override fallthrough
// was designed to prevent.
func TestResolveFallbackChain_NestedInvalidStrategyOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"FastLeaf", "InnerRR"},
		"InnerRR":    {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"FastLeaf":   "openai",
		"InnerRR":    "baml-roundrobin",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// "garbage" parses to an empty chain — present but invalid.
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": "garbage"}},
		},
	}

	chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain for invalid nested strategy override, got %v", chain)
	}
	if reason != PathReasonInvalidStrategyOverride {
		t.Errorf("reason: got %q, want %q", reason, PathReasonInvalidStrategyOverride)
	}
	if providers != nil {
		t.Errorf("providers must be nil when chain is rejected, got %v", providers)
	}
	if legacy != nil {
		t.Errorf("legacyChildren must be nil when chain is rejected, got %v", legacy)
	}
}

// TestResolveFallbackChain_NestedInvalidStartOverride pins the same
// invalid-nested preflight for the `start` option. Mirrors the
// top-level invalid-start route (TestRoundRobinOverrides_InvalidStart-
// RoutesToLegacy) so the contract is consistent between top-level RR
// and nested-RR-inside-fallback compositions: an invalid `start`
// surfaces upstream's canonical ensure_int error rather than silently
// using the static child config.
func TestResolveFallbackChain_NestedInvalidStartOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"FastLeaf", "InnerRR"},
		"InnerRR":    {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"FastLeaf":   "openai",
		"InnerRR":    "baml-roundrobin",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// String "1" — present-but-invalid for a numeric option.
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{
					"strategy": []any{"StaticBlue", "StaticGreen"},
					"start":    "1",
				}},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain for invalid nested start override, got %v", chain)
	}
	if reason != PathReasonInvalidRoundRobinStartOverride {
		t.Errorf("reason: got %q, want %q", reason, PathReasonInvalidRoundRobinStartOverride)
	}
}

// TestResolveFallbackChain_NestedInvalidProviderOverride pins the
// preflight's provider-precedence path: a present-empty `provider`
// on a nested strategy-parent child surfaces
// PathReasonInvalidProviderOverride, mirroring the top-level
// classifier's switch precedence. Operators reading
// X-BAML-Path-Reason then see the same reason whether the invalid
// override targets the request's top-level client or a nested
// fallback chain child.
func TestResolveFallbackChain_NestedInvalidProviderOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"FastLeaf", "InnerRR"},
		"InnerRR":    {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"FastLeaf":   "openai",
		"InnerRR":    "baml-roundrobin",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// Provider explicitly set to "" — operator typo or hostile
			// input. Forwarding "" into BAML's CFFI is rejected
			// in ClientProvider::from_str.
			{Name: "InnerRR", Provider: "", ProviderSet: true},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain for invalid nested provider override, got %v", chain)
	}
	if reason != PathReasonInvalidProviderOverride {
		t.Errorf("reason: got %q, want %q", reason, PathReasonInvalidProviderOverride)
	}
}

// TestResolveFallbackChain_NestedValidOverrideStillResolves pins
// negative coverage: a nested strategy-parent child WITH a valid
// runtime override (changed strategy children, valid start, valid
// provider) must resolve normally. Issue #237 PR 2 makes the valid
// override eligible for centralization — TenantLeaf is BR-supported,
// so the RR wrapper is centrally unwrapped to it and the reason
// reports ChildBuildRequest. The preflight's "any nested override →
// legacy" alternative would lose mixed-mode orchestration entirely
// (centralized or otherwise) for legitimate runtime overrides; this
// test still guards against that drift.
func TestResolveFallbackChain_NestedValidOverrideStillResolves(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"FastLeaf", "InnerRR"},
		"InnerRR":    {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"FastLeaf":   "openai",
		"InnerRR":    "baml-roundrobin",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// Valid: redirects InnerRR's children at runtime.
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{
					"strategy": []any{"TenantLeaf"},
					"start":    0,
				}},
			{Name: "TenantLeaf", Provider: "openai", ProviderSet: true},
		},
	}

	chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain == nil {
		t.Fatalf("expected chain to resolve for valid nested override, got nil with reason=%q", reason)
	}
	if reason != PathReasonFallbackRoundRobinChildBuildRequest {
		t.Errorf("reason: got %q, want %q (valid override with BR-supported leaf should centralize)",
			reason, PathReasonFallbackRoundRobinChildBuildRequest)
	}
	if legacy["InnerRR"] {
		t.Errorf("InnerRR must NOT be marked legacy after centralization, legacyChildren=%v", legacy)
	}
	if legacy["FastLeaf"] {
		t.Errorf("FastLeaf is a supported leaf; it must NOT be misclassified as legacy, legacyChildren=%v", legacy)
	}
	if providers["InnerRR"] != "openai" {
		t.Errorf("providers[InnerRR]: got %q, want openai (leaf provider after centralization to TenantLeaf)", providers["InnerRR"])
	}
}

// TestResolveFallbackChain_TransitiveInvalidStartOverride pins the
// transitive scope of the invalid-nested preflight: a fallback chain
// whose immediate strategy-parent child is itself a fallback that
// transitively reaches an RR with an invalid runtime `start` must
// short-circuit to top-level legacy with
// PathReasonInvalidRoundRobinStartOverride. Without transitive scope,
// the deep RR's malformed entry would be carried into the mixed-mode
// per-child legacy callback's scoped registry, BAML's eager parse
// would reject it inside InnerFallback's callback, RunStream-/
// CallOrchestration would re-classify the rejection as an ordinary
// fallback child failure, and the LaterLeaf sibling could silently
// answer the request with 200 — masking the misconfiguration.
//
// The LaterLeaf sibling is load-bearing: it's the trailing valid
// child that would have absorbed the deep failure under the
// immediate-only preflight. Its presence forces this test to depend
// on transitive walking specifically.
func TestResolveFallbackChain_TransitiveInvalidStartOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"InnerFallback", "LaterLeaf"},
		"InnerFallback":   {"DeeperRR"},
		"DeeperRR":        {"LeafX", "LeafY"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"InnerFallback":   "baml-fallback",
		"DeeperRR":        "baml-roundrobin",
		"LeafX":           "openai",
		"LeafY":           "openai",
		"LaterLeaf":       "openai",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// DeeperRR is two strategy-parent levels below the
			// outer chain's immediate child (InnerFallback).
			// Numeric string fails BAML's i32 ensure_int contract.
			{Name: "DeeperRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{
					"strategy": []any{"LeafX", "LeafY"},
					"start":    "1",
				}},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain (deep invalid start should short-circuit), got %v", chain)
	}
	if reason != PathReasonInvalidRoundRobinStartOverride {
		t.Errorf("reason: got %q, want %q (transitive walk should catch deep invalid start)",
			reason, PathReasonInvalidRoundRobinStartOverride)
	}
}

// TestResolveFallbackChain_TransitiveInvalidStrategyOverride is the
// strategy-side counterpart to TransitiveInvalidStartOverride. Same
// composition (Outer → InnerFallback → DeeperRR + LaterLeaf), but the
// deep invalid override is a malformed `strategy` value rather than
// `start`. Asserts the transitive walk catches it with
// PathReasonInvalidStrategyOverride.
func TestResolveFallbackChain_TransitiveInvalidStrategyOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"InnerFallback", "LaterLeaf"},
		"InnerFallback":   {"DeeperRR"},
		"DeeperRR":        {"LeafX", "LeafY"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"InnerFallback":   "baml-fallback",
		"DeeperRR":        "baml-roundrobin",
		"LeafX":           "openai",
		"LeafY":           "openai",
		"LaterLeaf":       "openai",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// "garbage" parses to an empty chain — present but invalid
			// per InspectStrategyOverride's three-state classification.
			{Name: "DeeperRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"strategy": "garbage"}},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain (deep invalid strategy should short-circuit), got %v", chain)
	}
	if reason != PathReasonInvalidStrategyOverride {
		t.Errorf("reason: got %q, want %q (transitive walk should catch deep invalid strategy)",
			reason, PathReasonInvalidStrategyOverride)
	}
}

// TestResolveFallbackChain_TransitivePresentEmptyProviderOverride
// pins the provider-side transitive case. The deep override is a
// present-empty `provider:""` — operator typo or hostile input that
// BAML's CFFI rejects in ClientProvider::from_str. Same Outer →
// InnerFallback → DeeperRR + LaterLeaf composition; LaterLeaf would
// mask the failure under immediate-only scope.
//
// Per-node precedence in the transitive walk surfaces this case as
// PathReasonInvalidProviderOverride (provider checked before
// strategy/start at each visited node), matching
// ResolveProviderWithReason's top-level switch.
func TestResolveFallbackChain_TransitivePresentEmptyProviderOverride(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"InnerFallback", "LaterLeaf"},
		"InnerFallback":   {"DeeperRR"},
		"DeeperRR":        {"LeafX", "LeafY"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"InnerFallback":   "baml-fallback",
		"DeeperRR":        "baml-roundrobin",
		"LeafX":           "openai",
		"LeafY":           "openai",
		"LaterLeaf":       "openai",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// Provider explicitly set to empty string — present but
			// invalid. Without the runtime override the introspected
			// provider (baml-roundrobin) would classify normally.
			{Name: "DeeperRR", Provider: "", ProviderSet: true},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain (deep invalid provider should short-circuit), got %v", chain)
	}
	if reason != PathReasonInvalidProviderOverride {
		t.Errorf("reason: got %q, want %q (transitive walk should catch deep present-empty provider)",
			reason, PathReasonInvalidProviderOverride)
	}
}

// TestResolveFallbackChain_TransitiveCycleSafe pins that a self-
// cyclic strategy graph (InnerFallback → InnerFallback) does not
// infinite-loop the transitive walk. The visited set in
// FindInvalidReachableStrategyOverride is the guard. With no
// invalid override anywhere in the graph the chain must resolve
// normally — empty reason and a non-empty chain.
//
// Mixed-mode composition (FastLeaf + InnerFallback) keeps the chain
// drivable: InnerFallback is a strategy parent so it lands on
// legacyChildren, but a chain with at least one BR-supported
// sibling escapes the legacyPositions == len(chain) short-circuit
// and is returned to the caller.
func TestResolveFallbackChain_TransitiveCycleSafe(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"FastLeaf", "InnerFallback"},
		"InnerFallback":   {"InnerFallback"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"FastLeaf":        "openai",
		"InnerFallback":   "baml-fallback",
	}

	chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
		nil, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected len=2 chain (FastLeaf, InnerFallback), got %d: %v", len(chain), chain)
	}
	if reason != "" {
		t.Errorf("reason: got %q, want empty (cycle without invalid overrides should not surface a path reason)", reason)
	}
	if providers["FastLeaf"] != "openai" {
		t.Errorf("FastLeaf provider: got %q, want openai", providers["FastLeaf"])
	}
	if !legacy["InnerFallback"] {
		t.Errorf("InnerFallback must be on the mixed-mode legacy child list (strategy parent), legacyChildren=%v", legacy)
	}
	if legacy["FastLeaf"] {
		t.Errorf("FastLeaf is a supported leaf; it must NOT be misclassified as legacy, legacyChildren=%v", legacy)
	}
}

// TestResolveFallbackChain_NestedExplicitRRProviderWithoutStrategy
// pins the chain-preflight contract for the missing-strategy case:
// a nested RR child whose runtime override carries an explicit RR
// provider but no `options.strategy` must route the whole request
// to top-level legacy with PathReasonInvalidStrategyOverride.
// Falling back to the introspected chain would silently dispatch
// a static child while BAML's eager parse rejects the shape — the
// validation-suppression class the preflight rules out.
//
// LaterLeaf is load-bearing: it's the trailing valid sibling that
// would have masked the deep failure under per-child legacy
// dispatch. Its presence means this test depends specifically on
// the transitive walk catching the missing-strategy case.
func TestResolveFallbackChain_NestedExplicitRRProviderWithoutStrategy(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyOuterFallback": {"InnerRR", "LaterLeaf"},
		"InnerRR":         {"StaticBlue", "StaticGreen"},
	}
	clientProviders := map[string]string{
		"MyOuterFallback": "baml-fallback",
		"InnerRR":         "baml-roundrobin",
		"StaticBlue":      "openai",
		"StaticGreen":     "openai",
		"LaterLeaf":       "openai",
	}

	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			// Explicit RR provider, no options.strategy — runtime
			// shape that BAML's eager parse rejects with
			// ensure_strategy.
			{Name: "InnerRR", Provider: "round-robin", ProviderSet: true,
				Options: map[string]any{"temperature": 0.7}},
		},
	}

	chain, _, _, reason := ResolveFallbackChainForClientWithReason(
		reg, "MyOuterFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" },
	)

	if chain != nil {
		t.Fatalf("expected nil chain (explicit RR provider w/ no strategy should short-circuit), got %v", chain)
	}
	if reason != PathReasonInvalidStrategyOverride {
		t.Errorf("reason: got %q, want %q (transitive walk should catch explicit-provider+no-strategy)",
			reason, PathReasonInvalidStrategyOverride)
	}
}

func TestResolveFallbackChain_FallbackAllowed(t *testing.T) {
	// baml-fallback SHOULD use the BuildRequest path.
	fallbackChains := map[string][]string{
		"MyFallback": {"ClientA", "ClientB"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"ClientA":    "openai",
		"ClientB":    "anthropic",
	}

	adapter := &mockAdapter{Context: context.Background()}
	chain, providers, legacyChildren := ResolveFallbackChain(
		adapter, "MyFallback", fallbackChains, clientProviders,
		func(p string) bool { return p == "openai" || p == "anthropic" },
	)

	if len(chain) != 2 {
		t.Fatalf("expected chain length 2 for baml-fallback, got %d", len(chain))
	}
	if providers["ClientA"] != "openai" || providers["ClientB"] != "anthropic" {
		t.Errorf("unexpected providers: %v", providers)
	}
	if len(legacyChildren) != 0 {
		t.Errorf("expected no legacy children for all-supported chain, got %v", legacyChildren)
	}
}

// TestResolveFallbackChainForClientWithReason_NilSupportCheck_StrategyChildrenLegacy
// pins the nil-isProviderSupported contract for strategy-provider
// children: BuildRequest cannot drive a strategy wrapper as a leaf,
// so RR / fallback children must always land on the legacy child
// list regardless of whether the caller passed a support predicate.
//
// Ordinary leaf providers in the same chain stay drivable (unknown
// support → not legacy) under nil-support, so the chain itself
// resolves with at least one supported sibling.
//
// PathReasonFallbackRoundRobinChildLegacy must continue to fire
// only for RR children — fallback children become legacy children
// without the RR-specific informational reason.
func TestResolveFallbackChainForClientWithReason_NilSupportCheck_StrategyChildrenLegacy(t *testing.T) {
	cases := []struct {
		name           string
		outerClient    string
		fallbackChains map[string][]string
		providers      map[string]string
		legacyChild    string
		drivableChild  string
		wantReason     string
	}{
		{
			name:        "roundrobin-child",
			outerClient: "MyFallback",
			fallbackChains: map[string][]string{
				"MyFallback": {"FastLeaf", "InnerRR"},
			},
			providers: map[string]string{
				"MyFallback": "baml-fallback",
				"FastLeaf":   "openai",
				"InnerRR":    "baml-roundrobin",
			},
			legacyChild:   "InnerRR",
			drivableChild: "FastLeaf",
			wantReason:    PathReasonFallbackRoundRobinChildLegacy,
		},
		{
			name:        "fallback-child",
			outerClient: "MyOuterFallback",
			fallbackChains: map[string][]string{
				"MyOuterFallback": {"FastLeaf", "InnerFallback"},
			},
			providers: map[string]string{
				"MyOuterFallback": "baml-fallback",
				"FastLeaf":        "openai",
				"InnerFallback":   "baml-fallback",
			},
			legacyChild:   "InnerFallback",
			drivableChild: "FastLeaf",
			wantReason:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain, providers, legacy, reason := ResolveFallbackChainForClientWithReason(
				nil, tc.outerClient, tc.fallbackChains, tc.providers,
				nil,
			)
			if chain == nil {
				t.Fatalf("expected chain to resolve under nil isProviderSupported (mixed-mode strategy child + drivable leaf), got nil with reason=%q", reason)
			}
			if len(chain) != 2 {
				t.Fatalf("expected len=2 chain, got %d: %v", len(chain), chain)
			}
			if reason != tc.wantReason {
				t.Errorf("reason: got %q, want %q", reason, tc.wantReason)
			}
			if !legacy[tc.legacyChild] {
				t.Errorf("%s must be classified legacy under nil-support (strategy wrapper), legacyChildren=%v", tc.legacyChild, legacy)
			}
			if legacy[tc.drivableChild] {
				t.Errorf("%s is an ordinary leaf provider — must not be legacy under nil-support, legacyChildren=%v", tc.drivableChild, legacy)
			}
			if providers[tc.legacyChild] == "" {
				t.Errorf("providers[%s] must be populated even when child is legacy, providers=%v", tc.legacyChild, providers)
			}
		})
	}
}

// TestResolveFallbackChainForClient_NilSupportCheck_StrategyChildClassified
// pins that the no-reason sibling helper returns the same
// legacyChildren map after discarding reason. Regression guard
// against a future divergence between the two exported entry
// points' nil-support handling.
func TestResolveFallbackChainForClient_NilSupportCheck_StrategyChildClassified(t *testing.T) {
	fallbackChains := map[string][]string{
		"MyFallback": {"FastLeaf", "InnerRR"},
	}
	clientProviders := map[string]string{
		"MyFallback": "baml-fallback",
		"FastLeaf":   "openai",
		"InnerRR":    "baml-roundrobin",
	}

	chain, providers, legacy := ResolveFallbackChainForClient(
		nil, "MyFallback", fallbackChains, clientProviders, nil,
	)

	if len(chain) != 2 {
		t.Fatalf("expected len=2 chain (mixed-mode RR child + drivable leaf) under nil-support, got %d: %v", len(chain), chain)
	}
	if !legacy["InnerRR"] {
		t.Errorf("InnerRR must be classified legacy via the no-reason sibling, legacyChildren=%v", legacy)
	}
	if legacy["FastLeaf"] {
		t.Errorf("FastLeaf must not be misclassified as legacy under nil-support, legacyChildren=%v", legacy)
	}
	if providers["InnerRR"] != "baml-roundrobin" {
		t.Errorf("providers[InnerRR]: got %q, want baml-roundrobin", providers["InnerRR"])
	}
}
