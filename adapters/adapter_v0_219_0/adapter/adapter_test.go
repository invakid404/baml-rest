package adapter

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestSetClientRegistry_DropsResolvedStrategyParents covers PR #192
// cold-review-3 signoff-10 F1. baml-rest-resolved RR / fallback
// strategy parent entries must not be forwarded into BAML's upstream
// registry — BAML rejects partial RR shapes at parse time
// (round_robin.rs:73-83 requires options.strategy; ensure_strategy at
// helpers.rs:790-829 requires it as an array, not a bracketed
// string), and the resolver dispatches with WithClient(leaf) so the
// parent has no role in BAML's runtime anyway.
//
// Coverage matrix:
//   - presence-only RR parent (omitted provider, no options)
//   - strategy-only RR parent (omitted provider, with strategy)
//   - explicit-provider RR parent (operator-typed canonical or alias)
//   - explicit fallback parent
//
// The original registry stays untouched in every case — baml-rest's
// resolver and metadata classifier read it verbatim from
// OriginalClientRegistry.
func TestSetClientRegistry_DropsResolvedStrategyParents(t *testing.T) {
	cases := []struct {
		name   string
		client *bamlutils.ClientProperty
	}{
		{
			name:   "presence-only RR parent (introspected provider drives classification)",
			client: &bamlutils.ClientProperty{Name: "MyRR"},
		},
		{
			name: "strategy-only RR parent (introspected provider, runtime chain)",
			client: &bamlutils.ClientProperty{
				Name:    "MyRR",
				Options: map[string]any{"strategy": []any{"A", "B"}},
			},
		},
		{
			name: "explicit baml-roundrobin (canonical baml-rest spelling)",
			client: &bamlutils.ClientProperty{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"strategy": []any{"A", "B"}},
			},
		},
		{
			name: "explicit round-robin (BAML upstream spelling)",
			client: &bamlutils.ClientProperty{
				Name:     "MyRR",
				Provider: "round-robin",
				Options:  map[string]any{"strategy": []any{"A", "B"}},
			},
		},
		{
			name: "explicit baml-fallback parent",
			client: &bamlutils.ClientProperty{
				Name:     "MyFb",
				Provider: "baml-fallback",
				Options:  map[string]any{"strategy": []any{"A", "B"}},
			},
		},
		{
			name: "explicit fallback alias",
			client: &bamlutils.ClientProperty{
				Name:     "MyFb",
				Provider: "fallback",
				Options:  map[string]any{"strategy": []any{"A", "B"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &BamlAdapter{
				Context: context.Background(),
				IntrospectedClientProvider: map[string]string{
					"MyRR": "baml-roundrobin",
					"MyFb": "baml-fallback",
				},
			}
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{tc.client},
			}
			if err := a.SetClientRegistry(reg); err != nil {
				t.Fatalf("SetClientRegistry: unexpected error: %v", err)
			}
			// BAML-bound: parent must be absent.
			if got := a.UpstreamClientNames(); len(got) != 0 {
				t.Errorf("UpstreamClientNames(): got %v, want empty (strategy parent must be dropped)", got)
			}
			// Original: parent must be intact, byte-for-byte.
			origClients := a.OriginalClientRegistry().Clients
			if len(origClients) != 1 || origClients[0] != tc.client {
				t.Errorf("OriginalClientRegistry mutated: got %+v, want %+v", origClients, []*bamlutils.ClientProperty{tc.client})
			}
		})
	}
}

// TestSetClientRegistry_PresenceOnlyRRRoundTrip is the
// presence-only round-trip test the cold-review-3 signoff-10 fix
// promises: original registry has the entry, BAML-bound registry
// drops it, and (in the integration test below) the resulting BAML
// call succeeds because BAML never sees the partial parent.
func TestSetClientRegistry_PresenceOnlyRRRoundTrip(t *testing.T) {
	primary := "MyRR"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "MyRR"}, // presence-only RR parent
		},
	}
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"MyRR": "baml-roundrobin",
			"A":    "openai",
			"B":    "openai",
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	// Original preserved verbatim.
	got := a.OriginalClientRegistry()
	if got != reg || len(got.Clients) != 1 || got.Clients[0].Name != "MyRR" {
		t.Errorf("OriginalClientRegistry not preserved; got %+v", got)
	}
	// BAML-bound: parent dropped.
	if names := a.UpstreamClientNames(); len(names) != 0 {
		t.Errorf("UpstreamClientNames(): got %v, want empty for presence-only RR", names)
	}
	// Cache for primary still reflects the materialised+translated
	// provider so ResolveProvider's shortcut returns the correct
	// classification value. (The primary itself isn't in BAML's
	// registry, but BuildRequest dispatches with WithClient(leaf)
	// after resolution, so BAML doesn't need the parent there.)
	if got := a.ClientRegistryProvider(); got != "baml-round-robin" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-round-robin", got)
	}
}

// TestSetClientRegistry_NonStrategyEntriesForwarded is the inverse
// regression guard. Non-strategy entries (concrete providers, leaf
// clients) must continue to reach BAML's registry — only the strategy
// parents get dropped.
func TestSetClientRegistry_NonStrategyEntriesForwarded(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "ClientA", Provider: "openai"},
			{Name: "ClientB", Provider: "anthropic"},
			{Name: "MyRR", Provider: "baml-roundrobin", Options: map[string]any{"strategy": []any{"ClientA", "ClientB"}}},
		},
	}
	a := &BamlAdapter{
		Context:                    context.Background(),
		IntrospectedClientProvider: map[string]string{},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	got := a.UpstreamClientNames()
	want := []string{"ClientA", "ClientB"}
	if len(got) != len(want) {
		t.Fatalf("UpstreamClientNames(): got %v, want %v", got, want)
	}
	for i, n := range want {
		if got[i] != n {
			t.Errorf("UpstreamClientNames()[%d]: got %q, want %q (strategy parent must be dropped, leaves preserved)", i, got[i], n)
		}
	}
}

// TestSetClientRegistry_TranslatesCanonicalRoundRobinSpelling covers
// finding 2 at the adapter seam. The operator-supplied
// "baml-roundrobin" spelling — the canonical baml-rest form — must
// be translated to "baml-round-robin" before forwarding to BAML's
// upstream CFFI, which rejects "baml-roundrobin" in
// ClientProvider::from_str (clientspec.rs:119-144).
func TestSetClientRegistry_TranslatesCanonicalRoundRobinSpelling(t *testing.T) {
	primary := "MyRR"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"strategy": []any{"A", "B"}},
			},
		},
	}
	a := &BamlAdapter{
		Context:                    context.Background(),
		IntrospectedClientProvider: map[string]string{},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "baml-round-robin" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-round-robin (operator baml-roundrobin → upstream-translated)", got)
	}
	// Original is preserved for the resolver — operator typed
	// "baml-roundrobin" and the resolver classification works on
	// that exact spelling via roundrobin.IsRoundRobinProvider, which
	// accepts all three forms.
	if a.OriginalClientRegistry().Clients[0].Provider != "baml-roundrobin" {
		t.Errorf("operator's Provider was mutated; original must be preserved verbatim")
	}
}

// TestSetClientRegistry_PresentEmptyProviderStaysEmpty covers the
// boundary between cold-review-2 verdict-8 (present-empty routes to
// legacy with PathReasonInvalidProviderOverride) and cold-review-3
// finding 1 (omitted-provider gets materialised from the introspected
// map). Present-empty is not the same as omitted: the operator
// explicitly cleared the provider, so we must NOT materialise — let
// BAML's CFFI emit its canonical "Invalid client provider:" error.
func TestSetClientRegistry_PresentEmptyProviderStaysEmpty(t *testing.T) {
	primary := "MyClient"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "MyClient", Provider: "", ProviderSet: true},
		},
	}
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"MyClient": "openai", // would have been materialised if absent
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("ClientRegistryProvider(): got %q, want empty (present-empty must not be materialised)", got)
	}
}

// TestSetClientRegistry_ExplicitProviderPassesThrough is the
// regression guard for the common-case: a fully-formed runtime entry
// with a real provider must pass through unchanged (modulo the
// upstream-spelling translation, which is a no-op for the four
// concrete provider strings tested here).
func TestSetClientRegistry_ExplicitProviderPassesThrough(t *testing.T) {
	cases := []string{"openai", "anthropic", "google-ai", "openai-generic"}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			primary := "MyClient"
			reg := &bamlutils.ClientRegistry{
				Primary: &primary,
				Clients: []*bamlutils.ClientProperty{
					{Name: "MyClient", Provider: p},
				},
			}
			a := &BamlAdapter{
				Context:                    context.Background(),
				IntrospectedClientProvider: map[string]string{},
			}
			if err := a.SetClientRegistry(reg); err != nil {
				t.Fatalf("SetClientRegistry: unexpected error: %v", err)
			}
			if got := a.ClientRegistryProvider(); got != p {
				t.Errorf("ClientRegistryProvider(): got %q, want %q (concrete provider must pass through unchanged)", got, p)
			}
		})
	}
}

// TestSetClientRegistry_NilRegistryClearsState mirrors the existing
// nil-input behaviour and is here so future refactors of the
// materialise helper can't accidentally panic on the nil-input path.
// Also pins upstreamClientNames clearing so a stale list from a
// previous registry call cannot survive across a nil reset.
func TestSetClientRegistry_NilRegistryClearsState(t *testing.T) {
	a := &BamlAdapter{Context: context.Background()}
	// Seed upstreamClientNames so we can verify the nil branch
	// clears it. Without this seed an unconditional check would
	// pass trivially.
	if err := a.SetClientRegistry(&bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{{Name: "Seed", Provider: "openai"}},
	}); err != nil {
		t.Fatalf("seed call: unexpected error: %v", err)
	}
	if names := a.UpstreamClientNames(); len(names) != 1 {
		t.Fatalf("seed: UpstreamClientNames len = %d, want 1", len(names))
	}

	if err := a.SetClientRegistry(nil); err != nil {
		t.Fatalf("SetClientRegistry(nil): unexpected error: %v", err)
	}
	if a.ClientRegistry != nil {
		t.Errorf("ClientRegistry: got %v, want nil", a.ClientRegistry)
	}
	if a.OriginalClientRegistry() != nil {
		t.Errorf("OriginalClientRegistry(): got %v, want nil", a.OriginalClientRegistry())
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("ClientRegistryProvider(): got %q, want empty", got)
	}
	if names := a.UpstreamClientNames(); len(names) != 0 {
		t.Errorf("UpstreamClientNames(): got %v, want empty (stale list leaked across nil reset)", names)
	}
}

// TestSetClientRegistry_SkipsNilClientEntries verifies the nil-client
// skip in both loops. JSON-shaped `clients: [null]` is allowed by the
// *ClientProperty slice and must not panic — verdict-11 finding 4
// (already correct on v0.219; pinned here for cross-version parity).
func TestSetClientRegistry_SkipsNilClientEntries(t *testing.T) {
	primary := "RealClient"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			nil,
			{Name: "RealClient", Provider: "openai"},
			nil,
		},
	}
	a := &BamlAdapter{
		Context:                    context.Background(),
		IntrospectedClientProvider: map[string]string{},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	got := a.UpstreamClientNames()
	if len(got) != 1 || got[0] != "RealClient" {
		t.Errorf("UpstreamClientNames(): got %v, want [RealClient]", got)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Errorf("ClientRegistryProvider(): got %q, want openai", got)
	}
}

// TestSetClientRegistry_ClearsStaleProviderCache pins the cache-
// clearing behaviour at the top of SetClientRegistry. A second call
// without a matching primary must clear the prior provider — without
// the explicit reset stale state would survive across requests on a
// reused adapter (verdict-11 finding 4 stale-cache half).
func TestSetClientRegistry_ClearsStaleProviderCache(t *testing.T) {
	a := &BamlAdapter{
		Context:                    context.Background(),
		IntrospectedClientProvider: map[string]string{},
	}
	primary := "FirstClient"
	if err := a.SetClientRegistry(&bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{{Name: "FirstClient", Provider: "openai"}},
	}); err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Fatalf("first call: ClientRegistryProvider got %q, want openai", got)
	}
	if err := a.SetClientRegistry(&bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{{Name: "OtherClient", Provider: "anthropic"}},
	}); err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("second call: ClientRegistryProvider got %q, want empty (stale cache leaked)", got)
	}
}
