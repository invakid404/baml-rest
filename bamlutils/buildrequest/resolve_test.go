package buildrequest

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/llmhttp"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// mockAdapter implements the subset of bamlutils.Adapter needed by the resolver tests.
type mockAdapter struct {
	context.Context
	retryConfig            *bamlutils.RetryConfig
	includeReasoning       bool
	clientRegistryProvider string
	originalRegistry       *bamlutils.ClientRegistry
	roundRobinAdvancer     bamlutils.RoundRobinAdvancer
}

func (m *mockAdapter) SetClientRegistry(_ *bamlutils.ClientRegistry) error { return nil }
func (m *mockAdapter) SetTypeBuilder(_ *bamlutils.TypeBuilder) error       { return nil }
func (m *mockAdapter) SetStreamMode(_ bamlutils.StreamMode)                {}
func (m *mockAdapter) StreamMode() bamlutils.StreamMode                    { return 0 }
func (m *mockAdapter) SetLogger(_ bamlutils.Logger)                        {}
func (m *mockAdapter) Logger() bamlutils.Logger                            { return nil }
func (m *mockAdapter) NewMediaFromURL(_ bamlutils.MediaKind, _ string, _ *string) (any, error) {
	return nil, nil
}
func (m *mockAdapter) NewMediaFromBase64(_ bamlutils.MediaKind, _ string, _ *string) (any, error) {
	return nil, nil
}
func (m *mockAdapter) SetRetryConfig(rc *bamlutils.RetryConfig) { m.retryConfig = rc }
func (m *mockAdapter) RetryConfig() *bamlutils.RetryConfig      { return m.retryConfig }
func (m *mockAdapter) SetIncludeReasoning(v bool)               { m.includeReasoning = v }
func (m *mockAdapter) IncludeReasoning() bool                   { return m.includeReasoning }
func (m *mockAdapter) ClientRegistryProvider() string           { return m.clientRegistryProvider }
func (m *mockAdapter) HTTPClient() *llmhttp.Client              { return nil }
func (m *mockAdapter) OriginalClientRegistry() *bamlutils.ClientRegistry {
	return m.originalRegistry
}
func (m *mockAdapter) SetRoundRobinAdvancer(a bamlutils.RoundRobinAdvancer) {
	m.roundRobinAdvancer = a
}
func (m *mockAdapter) RoundRobinAdvancer() bamlutils.RoundRobinAdvancer { return m.roundRobinAdvancer }

// ============================================================================
// ResolveProvider tests
// ============================================================================

func TestResolveProvider_PrimaryOverride(t *testing.T) {
	adapter := &mockAdapter{
		Context:                context.Background(),
		clientRegistryProvider: "anthropic",
	}
	got := ResolveProvider(adapter, "MyClient", "openai")
	if got != "anthropic" {
		t.Errorf("expected primary override 'anthropic', got %q", got)
	}
}

func TestResolveProvider_NamedClientOverride(t *testing.T) {
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "MyClient", Provider: "google-ai"},
			},
		},
	}
	got := ResolveProvider(adapter, "MyClient", "openai")
	if got != "google-ai" {
		t.Errorf("expected named-client override 'google-ai', got %q", got)
	}
}

func TestResolveProvider_NamedClientNotInRegistry(t *testing.T) {
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "OtherClient", Provider: "anthropic"},
			},
		},
	}
	got := ResolveProvider(adapter, "MyClient", "openai")
	if got != "openai" {
		t.Errorf("expected introspected fallback 'openai', got %q", got)
	}
}

func TestResolveProvider_NoRegistryNoDefault(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	got := ResolveProvider(adapter, "MyClient", "vertex-ai")
	if got != "vertex-ai" {
		t.Errorf("expected introspected fallback 'vertex-ai', got %q", got)
	}
}

func TestResolveProvider_PrimaryOverridesNamedClient(t *testing.T) {
	// When both primary and named client match, primary wins.
	primary := "PrimaryClient"
	adapter := &mockAdapter{
		Context:                context.Background(),
		clientRegistryProvider: "anthropic", // from primary
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &primary,
			Clients: []*bamlutils.ClientProperty{
				{Name: "PrimaryClient", Provider: "anthropic"},
				{Name: "MyClient", Provider: "google-ai"},
			},
		},
	}
	got := ResolveProvider(adapter, "MyClient", "openai")
	if got != "anthropic" {
		t.Errorf("expected primary 'anthropic' to win, got %q", got)
	}
}

// TestResolveProvider_NormalisesStrategyAliases pins that every
// non-empty return path runs through normalizeStrategyProvider.
// ResolveProviderWithReason and ResolveClientProvider already
// normalise; ResolveProvider previously leaked raw aliases on the
// adapter-cache, primary-override, named-client, and introspected-
// fallback paths, leaving downstream classification dependent on
// which seam happened to answer first. The four cases below cover
// each return path with each aliased spelling so a regression to
// raw aliases trips at exactly the seam it broke at.
func TestResolveProvider_NormalisesStrategyAliases(t *testing.T) {
	type call struct {
		seam     string
		adapter  *mockAdapter
		spelling string
	}

	cases := []struct {
		name string
		calls []call
		want  string
	}{
		{
			name: "fallback-aliases-collapse-to-baml-fallback",
			want: "baml-fallback",
			calls: []call{
				// adapter-cache: cached pre-resolved primary-provider
				// path. spelling=openai diverges from the override so
				// a regression that bypassed the cache and reached the
				// introspected fallback would surface as want="openai"
				// (i.e. the test would fail) rather than coincide on
				// the same normalised result.
				{
					seam: "adapter-cache",
					adapter: &mockAdapter{
						Context:                context.Background(),
						clientRegistryProvider: "fallback",
					},
					spelling: "openai",
				},
				// primary-override: registry.Primary points at a
				// runtime entry whose Provider is the fallback
				// override. clientRegistryProvider stays empty so
				// ResolveProvider exercises the direct registry
				// lookup branch (NOT the cache). spelling=openai
				// disambiguates from the introspected fallback the
				// same way as adapter-cache above.
				{
					seam: "primary-override",
					adapter: func() *mockAdapter {
						primary := "PrimaryClient"
						return &mockAdapter{
							Context: context.Background(),
							originalRegistry: &bamlutils.ClientRegistry{
								Primary: &primary,
								Clients: []*bamlutils.ClientProperty{
									{Name: "PrimaryClient", Provider: "fallback"},
								},
							},
						}
					}(),
					spelling: "openai",
				},
				// named-client-override: registry has an entry for
				// the named default client with the fallback
				// override; clientRegistryProvider empty + Primary
				// nil so the direct named-lookup branch fires.
				{
					seam: "named-client-override",
					adapter: &mockAdapter{
						Context: context.Background(),
						originalRegistry: &bamlutils.ClientRegistry{
							Clients: []*bamlutils.ClientProperty{
								{Name: "MyClient", Provider: "fallback"},
							},
						},
					},
					spelling: "openai",
				},
				// introspected-fallback: bare adapter, no runtime
				// registry. spelling=fallback so the introspected
				// path normalises and produces the expected
				// canonical "baml-fallback".
				{
					seam:     "introspected-fallback",
					adapter:  &mockAdapter{Context: context.Background()},
					spelling: "fallback",
				},
			},
		},
		{
			name: "round-robin-aliases-collapse-to-baml-roundrobin",
			want: "baml-roundrobin",
			calls: []call{
				// adapter-cache: cached pre-resolved provider path.
				// spelling=openai diverges from the override so a
				// regression that bypassed the cache and reached the
				// introspected fallback would surface as want=openai
				// (i.e. test fails) rather than coincide on the same
				// normalised result.
				{
					seam: "adapter-cache",
					adapter: &mockAdapter{
						Context:                context.Background(),
						clientRegistryProvider: "round-robin",
					},
					spelling: "openai",
				},
				// primary-override: registry.Primary points at a
				// runtime entry whose Provider is an RR alias.
				// clientRegistryProvider stays empty so the direct
				// registry-Primary lookup branch fires (NOT the
				// cache). spelling=openai disambiguates as above.
				{
					seam: "primary-override",
					adapter: func() *mockAdapter {
						primary := "PrimaryClient"
						return &mockAdapter{
							Context: context.Background(),
							originalRegistry: &bamlutils.ClientRegistry{
								Primary: &primary,
								Clients: []*bamlutils.ClientProperty{
									{Name: "PrimaryClient", Provider: "round-robin"},
								},
							},
						}
					}(),
					spelling: "openai",
				},
				// named-client-override: registry has an entry for
				// the named default client with an RR alias as the
				// override; clientRegistryProvider empty + Primary
				// nil so the direct named-lookup branch fires.
				{
					seam: "named-client-override",
					adapter: &mockAdapter{
						Context: context.Background(),
						originalRegistry: &bamlutils.ClientRegistry{
							Clients: []*bamlutils.ClientProperty{
								{Name: "MyClient", Provider: "baml-round-robin"},
							},
						},
					},
					spelling: "openai",
				},
				// introspected-* rows: bare adapter, no runtime
				// registry. The static spelling argument is what
				// gets normalised — three rows cover the three
				// accepted RR spellings.
				{
					seam:     "introspected-bare-round-robin",
					adapter:  &mockAdapter{Context: context.Background()},
					spelling: "round-robin",
				},
				{
					seam:     "introspected-baml-round-robin",
					adapter:  &mockAdapter{Context: context.Background()},
					spelling: "baml-round-robin",
				},
				{
					seam:     "introspected-canonical-baml-roundrobin",
					adapter:  &mockAdapter{Context: context.Background()},
					spelling: "baml-roundrobin",
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range tc.calls {
				// `introspected-*` cases use a mock adapter with no
				// runtime registry / provider entry, so ResolveProvider
				// falls back to the static spelling argument and the
				// normalisation under test fires on that path.
				got := ResolveProvider(c.adapter, "MyClient", c.spelling)
				if got != tc.want {
					t.Errorf("[%s] ResolveProvider with spelling %q: got %q, want %q",
						c.seam, c.spelling, got, tc.want)
				}
			}
		})
	}
}

// ============================================================================
// ResolveRetryPolicy tests
// ============================================================================

func TestResolveRetryPolicy_PerRequestOverride(t *testing.T) {
	adapter := &mockAdapter{
		Context:     context.Background(),
		retryConfig: &bamlutils.RetryConfig{MaxRetries: 5, Strategy: "constant_delay", DelayMs: 100},
	}
	policies := map[string]*retry.Policy{
		"IntrospectedPolicy": {MaxRetries: 2},
	}

	got := ResolveRetryPolicy(adapter, "MyClient", "IntrospectedPolicy", policies)
	if got == nil {
		t.Fatal("expected non-nil policy")
	}
	if got.MaxRetries != 5 {
		t.Errorf("expected per-request MaxRetries=5, got %d", got.MaxRetries)
	}
}

func TestResolveRetryPolicy_ClientRegistryOverride(t *testing.T) {
	policyName := "ClientRetryPolicy"
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "MyClient", Provider: "openai", RetryPolicy: &policyName},
			},
		},
	}
	policies := map[string]*retry.Policy{
		"ClientRetryPolicy":  {MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 50}},
		"IntrospectedPolicy": {MaxRetries: 1},
	}

	got := ResolveRetryPolicy(adapter, "MyClient", "IntrospectedPolicy", policies)
	if got == nil {
		t.Fatal("expected non-nil policy from client_registry")
	}
	if got.MaxRetries != 3 {
		t.Errorf("expected client-registry MaxRetries=3, got %d", got.MaxRetries)
	}
}

func TestResolveRetryPolicy_IntrospectedFallback(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	policies := map[string]*retry.Policy{
		"DefaultPolicy": {MaxRetries: 2, Strategy: &retry.ConstantDelay{DelayMs: 200}},
	}

	got := ResolveRetryPolicy(adapter, "MyClient", "DefaultPolicy", policies)
	if got == nil {
		t.Fatal("expected non-nil introspected policy")
	}
	if got.MaxRetries != 2 {
		t.Errorf("expected introspected MaxRetries=2, got %d", got.MaxRetries)
	}
}

func TestResolveRetryPolicy_NoPolicy(t *testing.T) {
	adapter := &mockAdapter{Context: context.Background()}
	got := ResolveRetryPolicy(adapter, "MyClient", "", nil)
	if got != nil {
		t.Errorf("expected nil policy, got %v", got)
	}
}

func TestResolveRetryPolicy_PriorityOrder(t *testing.T) {
	// Per-request > client_registry > introspected
	policyName := "ClientPolicy"
	adapter := &mockAdapter{
		Context:     context.Background(),
		retryConfig: &bamlutils.RetryConfig{MaxRetries: 10, Strategy: "constant_delay", DelayMs: 1},
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "MyClient", Provider: "openai", RetryPolicy: &policyName},
			},
		},
	}
	policies := map[string]*retry.Policy{
		"ClientPolicy":       {MaxRetries: 5},
		"IntrospectedPolicy": {MaxRetries: 2},
	}

	got := ResolveRetryPolicy(adapter, "MyClient", "IntrospectedPolicy", policies)
	if got.MaxRetries != 10 {
		t.Errorf("expected per-request MaxRetries=10 (highest priority), got %d", got.MaxRetries)
	}

	// Remove per-request override — should fall to client_registry
	adapter.retryConfig = nil
	got = ResolveRetryPolicy(adapter, "MyClient", "IntrospectedPolicy", policies)
	if got.MaxRetries != 5 {
		t.Errorf("expected client-registry MaxRetries=5, got %d", got.MaxRetries)
	}

	// Remove client_registry — should fall to introspected
	adapter.originalRegistry = nil
	got = ResolveRetryPolicy(adapter, "MyClient", "IntrospectedPolicy", policies)
	if got.MaxRetries != 2 {
		t.Errorf("expected introspected MaxRetries=2, got %d", got.MaxRetries)
	}
}

func TestResolveRetryPolicy_ClientRegistryUnknownPolicy(t *testing.T) {
	// Client references a retry_policy name not in the introspected map
	policyName := "NonexistentPolicy"
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "MyClient", Provider: "openai", RetryPolicy: &policyName},
			},
		},
	}
	policies := map[string]*retry.Policy{
		"IntrospectedPolicy": {MaxRetries: 2},
	}

	// Unknown policy name → fall through to introspected
	got := ResolveRetryPolicy(adapter, "MyClient", "IntrospectedPolicy", policies)
	if got == nil || got.MaxRetries != 2 {
		t.Errorf("expected introspected fallback MaxRetries=2 when client policy not found, got %v", got)
	}
}

func TestResolveRetryPolicy_PrimaryClientOverride(t *testing.T) {
	// When client_registry.primary selects a different client, its retry_policy
	// must be used — not the function's default client's policy.
	primaryName := "PrimaryClient"
	primaryPolicy := "PrimaryRetry"
	defaultPolicy := "DefaultRetry"
	adapter := &mockAdapter{
		Context:                context.Background(),
		clientRegistryProvider: "anthropic",
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &primaryName,
			Clients: []*bamlutils.ClientProperty{
				{Name: "PrimaryClient", Provider: "anthropic", RetryPolicy: &primaryPolicy},
				{Name: "DefaultClient", Provider: "openai", RetryPolicy: &defaultPolicy},
			},
		},
	}
	policies := map[string]*retry.Policy{
		"PrimaryRetry":      {MaxRetries: 7, Strategy: &retry.ConstantDelay{DelayMs: 50}},
		"DefaultRetry":      {MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 200}},
		"IntrospectedRetry": {MaxRetries: 1},
	}

	got := ResolveRetryPolicy(adapter, "DefaultClient", "IntrospectedRetry", policies)
	if got == nil {
		t.Fatal("expected non-nil policy")
	}
	if got.MaxRetries != 7 {
		t.Errorf("expected primary client's retry MaxRetries=7, got %d", got.MaxRetries)
	}
}

func TestResolveRetryPolicy_PrimaryWithoutRetrySkipsDefaultClient(t *testing.T) {
	// When primary is set but has no retry_policy, the resolver must NOT
	// borrow the default client's retry_policy. It should fall through to
	// the introspected default instead, because the primary client is the
	// one actually being used for streaming.
	primaryName := "PrimaryClient"
	defaultPolicy := "DefaultRetry"
	adapter := &mockAdapter{
		Context:                context.Background(),
		clientRegistryProvider: "anthropic",
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &primaryName,
			Clients: []*bamlutils.ClientProperty{
				{Name: "PrimaryClient", Provider: "anthropic"}, // no retry_policy
				{Name: "DefaultClient", Provider: "openai", RetryPolicy: &defaultPolicy},
			},
		},
	}
	policies := map[string]*retry.Policy{
		"DefaultRetry":      {MaxRetries: 3},
		"IntrospectedRetry": {MaxRetries: 1},
	}

	// Should get the introspected default, NOT DefaultClient's policy
	got := ResolveRetryPolicy(adapter, "DefaultClient", "IntrospectedRetry", policies)
	if got == nil {
		t.Fatal("expected non-nil policy from introspected fallback")
	}
	if got.MaxRetries != 1 {
		t.Errorf("expected introspected MaxRetries=1 (not default client's 3), got %d", got.MaxRetries)
	}
}

func TestResolveRetryPolicy_PrimaryWithoutRetryNoIntrospected(t *testing.T) {
	// When primary is set with no retry_policy and there's no introspected
	// default either, the result should be nil (no retries).
	primaryName := "PrimaryClient"
	defaultPolicy := "DefaultRetry"
	adapter := &mockAdapter{
		Context:                context.Background(),
		clientRegistryProvider: "anthropic",
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &primaryName,
			Clients: []*bamlutils.ClientProperty{
				{Name: "PrimaryClient", Provider: "anthropic"}, // no retry_policy
				{Name: "DefaultClient", Provider: "openai", RetryPolicy: &defaultPolicy},
			},
		},
	}
	policies := map[string]*retry.Policy{
		"DefaultRetry": {MaxRetries: 3},
	}

	// No introspected policy name → nil
	got := ResolveRetryPolicy(adapter, "DefaultClient", "", policies)
	if got != nil {
		t.Errorf("expected nil policy when primary has no retry and no introspected default, got MaxRetries=%d", got.MaxRetries)
	}
}

// ============================================================================
// ResolveStrategyAwareRetryPolicy tests
// ============================================================================
//
// These pin the wrapper-first / leaf-fallback contract that mirrors
// BAML's LLMStrategyProvider::WithRetryPolicy semantics. Each table
// row builds an adapter + introspected policies map and asserts the
// resolved MaxRetries (or nil) for a (strategyClient, effectiveClient)
// pair.

func TestResolveStrategyAwareRetryPolicy(t *testing.T) {
	// Shared introspected map used across cases. Case-specific overrides
	// rebind locally.
	commonPolicies := map[string]*retry.Policy{
		"WrapperRetry": {MaxRetries: 7, Strategy: &retry.ConstantDelay{DelayMs: 50}},
		"LeafRetry":    {MaxRetries: 3, Strategy: &retry.ConstantDelay{DelayMs: 200}},
	}

	type testCase struct {
		name                string
		adapter             *mockAdapter
		strategyClient      string
		effectiveClient     string
		strategyPolicyName  string
		effectivePolicyName string
		policies            map[string]*retry.Policy
		wantNil             bool
		wantMaxRetries      int
	}

	cases := []testCase{
		{
			// Wrapper has a static introspected policy, leaf has none.
			// BAML semantics: wrapper retry is applied around the
			// strategy iteration.
			name:                "wrapper-static-leaf-none",
			adapter:             &mockAdapter{Context: context.Background()},
			strategyClient:      "MyRR",
			effectiveClient:     "Leaf",
			strategyPolicyName:  "WrapperRetry",
			effectivePolicyName: "",
			policies:            commonPolicies,
			wantMaxRetries:      7,
		},
		{
			// Both wrapper and leaf have policies. Wrapper wins.
			name:                "wrapper-static-leaf-static",
			adapter:             &mockAdapter{Context: context.Background()},
			strategyClient:      "MyRR",
			effectiveClient:     "Leaf",
			strategyPolicyName:  "WrapperRetry",
			effectivePolicyName: "LeafRetry",
			policies:            commonPolicies,
			wantMaxRetries:      7,
		},
		{
			// Wrapper has none, leaf has policy. Leaf-fallback fires.
			name:                "wrapper-none-leaf-static",
			adapter:             &mockAdapter{Context: context.Background()},
			strategyClient:      "MyRR",
			effectiveClient:     "Leaf",
			strategyPolicyName:  "",
			effectivePolicyName: "LeafRetry",
			policies:            commonPolicies,
			wantMaxRetries:      3,
		},
		{
			// Runtime client_registry policy on the wrapper wins over a
			// leaf static introspected policy.
			name: "wrapper-runtime-registry-beats-leaf-static",
			adapter: func() *mockAdapter {
				name := "WrapperRetry"
				return &mockAdapter{
					Context: context.Background(),
					originalRegistry: &bamlutils.ClientRegistry{
						Clients: []*bamlutils.ClientProperty{
							{Name: "MyRR", Provider: "baml-roundrobin", RetryPolicy: &name},
						},
					},
				}
			}(),
			strategyClient:      "MyRR",
			effectiveClient:     "Leaf",
			strategyPolicyName:  "",
			effectivePolicyName: "LeafRetry",
			policies:            commonPolicies,
			wantMaxRetries:      7,
		},
		{
			// Per-request __baml_options__.retry override wins
			// regardless of which client name is keyed. Verify that
			// the override short-circuits the wrapper-first call AND
			// is not double-applied (it's the same value either way).
			name: "per-request-override-wins-over-everything",
			adapter: &mockAdapter{
				Context:     context.Background(),
				retryConfig: &bamlutils.RetryConfig{MaxRetries: 11, Strategy: "constant_delay", DelayMs: 1},
			},
			strategyClient:      "MyRR",
			effectiveClient:     "Leaf",
			strategyPolicyName:  "WrapperRetry",
			effectivePolicyName: "LeafRetry",
			policies:            commonPolicies,
			wantMaxRetries:      11,
		},
		{
			// strategyClient == effectiveClient (no RR unwrap
			// happened). Behavior must match a single
			// ResolveRetryPolicy call — leaf-fallback path is
			// suppressed by the equality guard, so a nil wrapper
			// resolution yields nil even if a separate "leaf" entry
			// existed in the policies map.
			name:                "no-unwrap-equal-names-matches-single-call",
			adapter:             &mockAdapter{Context: context.Background()},
			strategyClient:      "MyClient",
			effectiveClient:     "MyClient",
			strategyPolicyName:  "WrapperRetry",
			effectivePolicyName: "WrapperRetry",
			policies:            commonPolicies,
			wantMaxRetries:      7,
		},
		{
			// Both nil → returns nil (no retries).
			name:                "both-nil",
			adapter:             &mockAdapter{Context: context.Background()},
			strategyClient:      "MyRR",
			effectiveClient:     "Leaf",
			strategyPolicyName:  "",
			effectivePolicyName: "",
			policies:            commonPolicies,
			wantNil:             true,
		},
		{
			// Equal-name no-unwrap with no policies → nil. Pins that
			// the equal-name short-circuit doesn't accidentally
			// double-resolve.
			name:                "no-unwrap-no-policy-nil",
			adapter:             &mockAdapter{Context: context.Background()},
			strategyClient:      "Solo",
			effectiveClient:     "Solo",
			strategyPolicyName:  "",
			effectivePolicyName: "",
			policies:            commonPolicies,
			wantNil:             true,
		},
		{
			// Equal-name no-unwrap WITH a populated effective fallback
			// policy. This is the load-bearing pin on the
			// `strategyClient != effectiveClient` guard inside
			// ResolveStrategyAwareRetryPolicy.
			//
			// Trace WITH the guard: wrapper-first call sees
			// strategyPolicyName="" and no matching runtime registry
			// entry → returns nil. The guard then suppresses the
			// leaf-fallback because strategyClient == effectiveClient,
			// so the helper returns nil. wantNil=true holds.
			//
			// Trace WITHOUT the guard: wrapper-first returns nil as
			// before, but then the leaf-fallback fires unconditionally
			// → resolveRetryPolicyForClient picks up
			// effectivePolicyName="LeafRetry" → returns the LeafRetry
			// policy (MaxRetries=3). The test would fail with
			// "expected nil, got MaxRetries=3", catching the
			// regression.
			//
			// This sub-case differs from no-unwrap-no-policy-nil
			// (where effectivePolicyName="" makes the leaf-fallback
			// also return nil and the guard becomes a no-op) and from
			// no-unwrap-equal-names-matches-single-call (where the
			// wrapper-first call returns non-nil and short-circuits
			// before the guard matters).
			name:                "no-unwrap-equal-names-suppresses-leaf-fallback",
			adapter:             &mockAdapter{Context: context.Background()},
			strategyClient:      "Solo",
			effectiveClient:     "Solo",
			strategyPolicyName:  "",
			effectivePolicyName: "LeafRetry",
			policies:            commonPolicies,
			wantNil:             true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveStrategyAwareRetryPolicy(
				tc.adapter,
				tc.strategyClient,
				tc.effectiveClient,
				tc.strategyPolicyName,
				tc.effectivePolicyName,
				tc.policies,
			)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil policy, got MaxRetries=%d", got.MaxRetries)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil policy with MaxRetries=%d, got nil", tc.wantMaxRetries)
			}
			if got.MaxRetries != tc.wantMaxRetries {
				t.Errorf("expected MaxRetries=%d, got %d", tc.wantMaxRetries, got.MaxRetries)
			}
		})
	}
}

// TestResolveStrategyAwareRetryPolicy_LeafFallbackUsesEffectiveName
// pins the priority-3 path: wrapper has no policy in either runtime
// registry or introspected map, but the leaf does. The helper must
// invoke the runtime-registry lookup with the EFFECTIVE name on the
// second call so it keys off the leaf, not the wrapper. A regression
// that mistakenly passed strategyClient on the fallback call would
// miss the leaf's runtime-registry retry_policy.
//
// This test exercises the primary-nil registry shape; the
// reg.Primary-set companion lives in
// TestResolveStrategyAwareRetryPolicy_LeafFallbackUnshadowedByPrimary.
func TestResolveStrategyAwareRetryPolicy_LeafFallbackUsesEffectiveName(t *testing.T) {
	leafPolicy := "LeafRetry"
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Clients: []*bamlutils.ClientProperty{
				{Name: "Leaf", Provider: "openai", RetryPolicy: &leafPolicy},
			},
		},
	}
	policies := map[string]*retry.Policy{
		"LeafRetry": {MaxRetries: 4, Strategy: &retry.ConstantDelay{DelayMs: 25}},
	}

	got := ResolveStrategyAwareRetryPolicy(adapter, "MyRR", "Leaf", "", "", policies)
	if got == nil {
		t.Fatal("expected leaf-fallback to find the runtime-registry policy on Leaf, got nil")
	}
	if got.MaxRetries != 4 {
		t.Errorf("expected leaf MaxRetries=4, got %d", got.MaxRetries)
	}
}

// TestResolveStrategyAwareRetryPolicy_LeafFallbackUnshadowedByPrimary
// pins the production-shaped scenario CodeRabbit verdict-1 surfaced:
// when the strategy wrapper IS the runtime registry primary, the
// inner ResolveRetryPolicy short-circuits to a primary-only scan
// (retry_policy.go: the `reg.Primary != nil && *reg.Primary != ""`
// branch), so the leaf-fallback's second call — even with the
// leaf's effective name passed in — never reaches the leaf's entry.
// The fix is to use a primary-ignoring helper for the leaf-fallback
// call so the leaf's runtime-registry retry_policy is honored.
//
// Failure mode without the fix: ResolveStrategyAwareRetryPolicy
// returns nil even though the leaf's runtime-registry retry_policy
// is fully populated. The first-call wrapper path returns nil
// (primary entry exists but has no RetryPolicy and no introspected
// fallback), and the second-call leaf path is shadowed by the same
// primary-only scan and also returns nil.
//
// The fix path: leaf-fallback uses resolveRetryPolicyForClient, which
// scans by direct client name instead of *reg.Primary. The wrapper-
// first call still uses ResolveRetryPolicy so its primary-aware
// behavior + per-request override priority are unchanged.
func TestResolveStrategyAwareRetryPolicy_LeafFallbackUnshadowedByPrimary(t *testing.T) {
	primary := "MyRR"
	leafPolicy := "LeafRetry"
	adapter := &mockAdapter{
		Context: context.Background(),
		originalRegistry: &bamlutils.ClientRegistry{
			Primary: &primary,
			Clients: []*bamlutils.ClientProperty{
				// Wrapper is the registry primary but carries no
				// retry policy of its own — and has no introspected
				// fallback either (strategyPolicyName="").
				{Name: "MyRR", Provider: "baml-roundrobin"},
				// Leaf has a runtime-registry retry policy. This is
				// what the strategy-aware helper must surface via
				// the leaf-fallback path.
				{Name: "Leaf", Provider: "openai", RetryPolicy: &leafPolicy},
			},
		},
	}
	policies := map[string]*retry.Policy{
		"LeafRetry": {MaxRetries: 9, Strategy: &retry.ConstantDelay{DelayMs: 75}},
	}

	got := ResolveStrategyAwareRetryPolicy(adapter, "MyRR", "Leaf", "", "", policies)
	if got == nil {
		t.Fatal("expected leaf-fallback to find Leaf's runtime-registry retry_policy despite MyRR being the registry primary; got nil (leaf-fallback shadowed by primary-only scan in inner ResolveRetryPolicy)")
	}
	if got.MaxRetries != 9 {
		t.Errorf("expected leaf MaxRetries=9, got %d", got.MaxRetries)
	}
}
