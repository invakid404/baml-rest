package adapter

import (
	"context"
	"reflect"
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
			if got := upstreamClientNamesSnapshot(a); len(got) != 0 {
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
	if names := upstreamClientNamesSnapshot(a); len(names) != 0 {
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
	got := upstreamClientNamesSnapshot(a)
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
	// CodeRabbit verdict-39 finding F4: also pin that the entry
	// itself made it into BAML's BuildRequest-safe registry view with
	// the empty provider preserved verbatim. The cache check above
	// only proves the adapter shortcut returns "", not that BAML
	// actually received the entry. A regression that silently
	// dropped present-empty entries from the registry while leaving
	// the cache empty would slip past the cache assertion alone.
	provider, _, ok := buildRequestClientEntrySnapshot(a, "MyClient")
	if !ok {
		t.Fatalf("BuildRequest registry should retain the present-empty entry; got missing")
	}
	if provider != "" {
		t.Errorf("BuildRequest registry provider for MyClient: got %q, want \"\" (present-empty must not be materialised at the registry seam either)", provider)
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
// Also pins upstreamClientNames clearing on BOTH views so a stale
// list from a previous registry call cannot survive across a nil
// reset (cold-review-4 + Option C).
func TestSetClientRegistry_NilRegistryClearsState(t *testing.T) {
	a := &BamlAdapter{Context: context.Background()}
	// Seed both upstream lists so we can verify the nil branch
	// clears them. Without seed an unconditional check would
	// pass trivially.
	if err := a.SetClientRegistry(&bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{{Name: "Seed", Provider: "openai"}},
	}); err != nil {
		t.Fatalf("seed call: unexpected error: %v", err)
	}
	if names := upstreamClientNamesSnapshot(a); len(names) != 1 {
		t.Fatalf("seed: UpstreamClientNames len = %d, want 1", len(names))
	}
	if names := legacyUpstreamClientNamesSnapshot(a); len(names) != 1 {
		t.Fatalf("seed: LegacyUpstreamClientNames len = %d, want 1", len(names))
	}

	if err := a.SetClientRegistry(nil); err != nil {
		t.Fatalf("SetClientRegistry(nil): unexpected error: %v", err)
	}
	if a.ClientRegistry != nil {
		t.Errorf("ClientRegistry: got %v, want nil", a.ClientRegistry)
	}
	if a.LegacyClientRegistry != nil {
		t.Errorf("LegacyClientRegistry: got %v, want nil", a.LegacyClientRegistry)
	}
	if a.OriginalClientRegistry() != nil {
		t.Errorf("OriginalClientRegistry(): got %v, want nil", a.OriginalClientRegistry())
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("ClientRegistryProvider(): got %q, want empty", got)
	}
	if names := upstreamClientNamesSnapshot(a); len(names) != 0 {
		t.Errorf("UpstreamClientNames(): got %v, want empty (stale list leaked across nil reset)", names)
	}
	if names := legacyUpstreamClientNamesSnapshot(a); len(names) != 0 {
		t.Errorf("LegacyUpstreamClientNames(): got %v, want empty (stale legacy list leaked across nil reset)", names)
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
	got := upstreamClientNamesSnapshot(a)
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

// TestSetClientRegistry_StaticPrimary_PopulatesProviderFromIntrospected
// covers verdict-13 findings 1, 2, 5: a runtime registry that names a
// static primary by name without redefining it in clients[] (a normal
// operator-facing way to switch to an existing introspected client)
// must populate the cache from the introspected map, not leave it ""
// where ResolveProvider's adapter shortcut would short-circuit to the
// function default.
func TestSetClientRegistry_StaticPrimary_PopulatesProviderFromIntrospected(t *testing.T) {
	primary := "StaticOpenAI"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{}, // empty — pure static-primary selection
	}
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"StaticOpenAI": "openai",
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Errorf("ClientRegistryProvider(): got %q, want openai (synthesized from introspected map for unrebound primary)", got)
	}
}

// TestSetClientRegistry_PresentEmptyMatchingPrimary_StaysEmpty pins
// the foundPrimary-flag invariant from verdict-13: a matching primary
// entry that intentionally produces "" (operator typo'd provider:"")
// must NOT be overwritten by introspected materialisation. Without
// the explicit foundPrimary gate, a "cache is empty" trigger would
// clobber this case.
func TestSetClientRegistry_PresentEmptyMatchingPrimary_StaysEmpty(t *testing.T) {
	primary := "X"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "X", Provider: "", ProviderSet: true},
		},
	}
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"X": "openai", // would have been materialised on the synthesis path
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("ClientRegistryProvider(): got %q, want empty (present-empty primary must not be synthesized over)", got)
	}
}

// TestSetClientRegistry_PresentNonEmptyMatchingPrimary_UsesOverride
// is the inverse-regression guard: a matching primary entry with a
// concrete provider must populate the cache from that override, not
// from the introspected map. Without this guard, a future refactor
// of the foundPrimary logic could swap precedence by accident.
func TestSetClientRegistry_PresentNonEmptyMatchingPrimary_UsesOverride(t *testing.T) {
	primary := "X"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "X", Provider: "openai"},
		},
	}
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"X": "anthropic", // override must win over introspected
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Errorf("ClientRegistryProvider(): got %q, want openai (operator override must win over introspected)", got)
	}
}

// TestSetClientRegistryKeepsExplicitStrategyParentForLegacyOnly is the
// load-bearing assertion for PR #192 cold-review-4 + Option C: an
// explicit runtime strategy-parent override is hidden from the
// BuildRequest-safe registry view (so leaf calls aren't poisoned by
// to_clients eagerly parsing every entry) but kept in the legacy
// view (so BAML can honour the override on legacy fallthrough).
func TestSetClientRegistryKeepsExplicitStrategyParentForLegacyOnly(t *testing.T) {
	cases := []struct {
		name   string
		client *bamlutils.ClientProperty
	}{
		{
			name: "explicit baml-fallback parent with valid array strategy",
			client: &bamlutils.ClientProperty{
				Name:     "TestFallbackPair",
				Provider: "baml-fallback",
				Options:  map[string]any{"strategy": []any{"RuntimePrimary", "RuntimeSecondary"}},
			},
		},
		{
			name: "explicit baml-roundrobin parent with valid array strategy",
			client: &bamlutils.ClientProperty{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"strategy": []any{"A", "B"}, "start": 1},
			},
		},
		{
			name: "RR parent with invalid string start (BAML must see it to emit canonical error)",
			client: &bamlutils.ClientProperty{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"strategy": []any{"A", "B"}, "start": "not-an-int"},
			},
		},
		{
			name: "strategy-only RR override on a static parent (introspected provider drives classification)",
			client: &bamlutils.ClientProperty{
				Name:    "StaticRR",
				Options: map[string]any{"strategy": []any{"A", "B"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &BamlAdapter{
				Context: context.Background(),
				IntrospectedClientProvider: map[string]string{
					"StaticRR":         "baml-roundrobin",
					"TestFallbackPair": "baml-fallback",
				},
			}
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{tc.client},
			}
			if err := a.SetClientRegistry(reg); err != nil {
				t.Fatalf("SetClientRegistry: unexpected error: %v", err)
			}
			// BuildRequest view: parent dropped (cold-review-3 F1
			// invariant preserved — to_clients is shielded from
			// rejected shapes when leaf calls drive dispatch).
			if names := upstreamClientNamesSnapshot(a); len(names) != 0 {
				t.Errorf("UpstreamClientNames(): got %v, want empty (BuildRequest view must drop explicit strategy parent)", names)
			}
			// Legacy view: parent kept (Option C — BAML executes
			// the override or emits canonical error on legacy
			// fallthrough).
			legacyNames := legacyUpstreamClientNamesSnapshot(a)
			if len(legacyNames) != 1 || legacyNames[0] != tc.client.Name {
				t.Errorf("LegacyUpstreamClientNames(): got %v, want [%s] (legacy view must preserve explicit strategy parent)", legacyNames, tc.client.Name)
			}

			// CodeRabbit verdict-38 finding F1: pin provider+options
			// passed to BAML, not just the name. v0.219 forwards
			// options unwrapped, so direct DeepEqual works.
			provider, options, ok := legacyClientEntrySnapshot(a, tc.client.Name)
			if !ok {
				t.Fatalf("legacyClientEntrySnapshot: entry %q missing from BAML's legacy registry", tc.client.Name)
			}
			wantProvider := bamlutils.UpstreamClientRegistryProvider(tc.client, a.IntrospectedClientProvider)
			if provider != wantProvider {
				t.Errorf("legacy provider for %q: got %q, want %q", tc.client.Name, provider, wantProvider)
			}
			if want, ok := tc.client.Options["strategy"]; ok {
				if !reflect.DeepEqual(options["strategy"], want) {
					t.Errorf("legacy options[strategy] for %q: got %v, want %v", tc.client.Name, options["strategy"], want)
				}
			}
			if want, ok := tc.client.Options["start"]; ok {
				if !reflect.DeepEqual(options["start"], want) {
					t.Errorf("legacy options[start] for %q: got %v, want %v", tc.client.Name, options["start"], want)
				}
			}
		})
	}
}

// TestSetClientRegistryDropsInertPresenceOnlyParentInBothBamlViews
// pins the no-op invariant for inert presence-only static parents.
// `{Name: "TestFallbackPair"}` with no provider/strategy/start
// carries no operator intent — it's just naming a static client by
// name. Forwarding it to either BAML view would fail to_clients on
// missing required `strategy`. Both views must drop it; the static
// .baml config takes effect verbatim.
func TestSetClientRegistryDropsInertPresenceOnlyParentInBothBamlViews(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "TestFallbackPair"}, // presence-only static parent
		},
	}
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"TestFallbackPair": "baml-fallback",
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if names := upstreamClientNamesSnapshot(a); len(names) != 0 {
		t.Errorf("UpstreamClientNames(): got %v, want empty (inert presence-only parent must be dropped)", names)
	}
	if names := legacyUpstreamClientNamesSnapshot(a); len(names) != 0 {
		t.Errorf("LegacyUpstreamClientNames(): got %v, want empty (inert presence-only parent must be dropped from legacy view too)", names)
	}
}

// TestSetClientRegistry_DualViewPrimaryCacheUnderInertParent pins the
// fix for CodeRabbit verdict-21 findings 1+2 generalised across views.
// When the operator names a primary that exists only as an inert
// presence-only static parent (dropped from BOTH views), the primary
// cache must NOT observe a provider that no view exposes. Instead it
// falls through to introspected synthesis so the
// ResolveProvider adapter shortcut returns the static-config provider
// rather than "" or a wrong override.
func TestSetClientRegistry_DualViewPrimaryCacheUnderInertParent(t *testing.T) {
	primary := "TestFallbackPair"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "TestFallbackPair"}, // inert presence-only — dropped from both views
		},
	}
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"TestFallbackPair": "baml-fallback",
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	// Primary cache: must reflect the introspected provider (not
	// caching from the dropped runtime entry), translated to the
	// upstream-accepted spelling.
	if got := a.ClientRegistryProvider(); got != "baml-fallback" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-fallback (introspected synthesis under dropped primary)", got)
	}
}

// TestSetClientRegistry_DualViewPrimaryCacheUnderExplicitParent is
// the companion guard: an explicit runtime strategy-parent override
// stays in the legacy view, so the primary cache should observe its
// provider (not the introspected default). Without this, an operator
// switching to a runtime baml-fallback override on an existing static
// RR client would see ResolveProvider report the static RR provider
// instead of the runtime fallback.
func TestSetClientRegistry_DualViewPrimaryCacheUnderExplicitParent(t *testing.T) {
	primary := "MyParent"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{
				Name:     "MyParent",
				Provider: "baml-fallback",
				Options:  map[string]any{"strategy": []any{"A", "B"}},
			},
		},
	}
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"MyParent": "baml-roundrobin", // operator switched type at runtime
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "baml-fallback" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-fallback (operator runtime override must win over introspected)", got)
	}
}

// TestSetClientRegistry_PresentEmptyPrimaryIsNoOp pins CodeRabbit
// verdict-31 finding F1: a runtime client_registry payload that
// supplies `"primary": ""` (Primary != nil but *Primary == "") must
// NOT propagate the empty string to BAML's registry. BAML stores ""
// verbatim and PromptRenderer fails on the resulting empty-name
// lookup. Most baml-rest helpers already treat empty primary as a
// no-op; the adapter was the outlier.
//
// The test runs SetClientRegistry twice: first with a real primary
// (cache populated, primary forwarded), then with present-empty
// primary on the same adapter. The second call must clear the cache
// (no stale provider leakage) AND must not invoke SetPrimaryClient
// or synthesize an empty-named ClientProperty.
func TestSetClientRegistry_PresentEmptyPrimaryIsNoOp(t *testing.T) {
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"GoodClient": "openai",
		},
	}

	// Step 1: prime the cache via a normal primary so we can verify
	// it gets cleared by the present-empty call.
	primed := "GoodClient"
	if err := a.SetClientRegistry(&bamlutils.ClientRegistry{
		Primary: &primed,
		Clients: []*bamlutils.ClientProperty{
			{Name: "GoodClient", Provider: "openai"},
		},
	}); err != nil {
		t.Fatalf("priming SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Fatalf("priming setup wrong: ClientRegistryProvider() = %q, want openai", got)
	}

	// Step 2: present-empty primary. Must clear cache, no synthesis,
	// no SetPrimaryClient propagation.
	empty := ""
	if err := a.SetClientRegistry(&bamlutils.ClientRegistry{
		Primary: &empty,
		Clients: nil,
	}); err != nil {
		t.Fatalf("present-empty primary SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("present-empty primary leaked stale provider: ClientRegistryProvider() = %q, want \"\"", got)
	}
	// Sanity: original is preserved verbatim (caller's contract).
	if orig := a.OriginalClientRegistry(); orig == nil || orig.Primary == nil || *orig.Primary != "" {
		t.Errorf("OriginalClientRegistry should preserve the present-empty primary verbatim; got %+v", orig)
	}
}
