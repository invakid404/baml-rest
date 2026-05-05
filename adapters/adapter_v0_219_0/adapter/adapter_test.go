package adapter

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestSetClientRegistry_DropsResolvedStrategyParents pins that
// baml-rest-resolved RR / fallback strategy parent entries must not
// be forwarded into BAML's upstream registry — BAML rejects partial
// RR shapes at parse time
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
			name: "explicit baml-fallback parent",
			client: &bamlutils.ClientProperty{
				Name:     "MyFb",
				Provider: "baml-fallback",
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

// TestSetClientRegistry_PresenceOnlyRRRoundTrip is the presence-only
// round-trip: original registry has the entry, BAML-bound registry
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
	// BAML-bound: parent dropped from BOTH views. Presence-only RR
	// is inert (no provider, no options.strategy), so neither the
	// BuildRequest-safe view nor the legacy view should forward it
	// — BAML would reject the bare entry on either side. A regression
	// that left the parent in the legacy registry would slip past a
	// BuildRequest-only assertion, so check both views explicitly.
	if names := upstreamClientNamesSnapshot(a); len(names) != 0 {
		t.Errorf("UpstreamClientNames(): got %v, want empty for presence-only RR", names)
	}
	if names := legacyUpstreamClientNamesSnapshot(a); len(names) != 0 {
		t.Errorf("legacyUpstreamClientNamesSnapshot after presence-only RR: got %v, want empty", names)
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
	// Legacy view must also forward the non-strategy entries; unlike
	// the BuildRequest view, the legacy view retains the strategy
	// parent ("MyRR") because legacy dispatch reaches into it
	// directly. Asserting exact equality pins both invariants: leaves
	// flow through AND the strategy parent is preserved (not dropped
	// by the legacy path).
	legacyGot := legacyUpstreamClientNamesSnapshot(a)
	legacyWant := []string{"ClientA", "ClientB", "MyRR"}
	if !reflect.DeepEqual(legacyGot, legacyWant) {
		t.Errorf("legacyUpstreamClientNamesSnapshot: got %v, want %v (legacy view forwards non-strategy entries and retains strategy parents)", legacyGot, legacyWant)
	}
}

// TestSetClientRegistry_TranslatesCanonicalRoundRobinSpelling pins
// canonical round-robin spelling translation at the adapter seam.
// The operator-supplied
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
// boundary between two adjacent cases: present-empty routes to
// legacy with PathReasonInvalidProviderOverride, while
// omitted-provider gets materialised from the introspected map.
// Present-empty is not the same as omitted — the operator explicitly
// cleared the provider, so we must NOT materialise; let BAML's CFFI
// emit its canonical "Invalid client provider:" error.
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
	// Also pin that the entry itself made it into BAML's
	// BuildRequest-safe registry view with the empty provider
	// preserved verbatim. The cache check above only proves the
	// adapter shortcut returns "", not that BAML actually received
	// the entry. A regression that silently dropped present-empty
	// entries from the registry while leaving
	// the cache empty would slip past the cache assertion alone.
	provider, _, ok := buildRequestClientEntrySnapshot(a, "MyClient")
	if !ok {
		t.Fatalf("BuildRequest registry should retain the present-empty entry; got missing")
	}
	if provider != "" {
		t.Errorf("BuildRequest registry provider for MyClient: got %q, want \"\" (present-empty must not be materialised at the registry seam either)", provider)
	}
	// SetClientRegistry materialises both views; the legacy view is
	// the seam BAML reads on the legacy fallthrough path. A regression
	// that dropped the present-empty entry from one view but not the
	// other would fall through to the function's compiled default
	// instead of letting BAML emit its canonical missing-provider
	// error against the operator-typo'd entry.
	legacyProvider, _, ok := legacyClientEntrySnapshot(a, "MyClient")
	if !ok {
		t.Fatalf("legacy registry should retain the present-empty entry; got missing")
	}
	if legacyProvider != "" {
		t.Errorf("legacy registry provider for MyClient: got %q, want \"\" (present-empty must be preserved verbatim in the legacy view)", legacyProvider)
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
// reset.
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
// *ClientProperty slice and must not panic.
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
// reused adapter.
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
// covers the case where a runtime registry names a static primary by
// name without redefining it in clients[] (a normal operator-facing
// way to switch to an existing introspected client): the cache must
// populate from the introspected map, not leave it "" where
// ResolveProvider's adapter shortcut would short-circuit to the
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
// the foundPrimary-flag invariant: a matching primary entry that
// intentionally produces "" (operator typo'd provider:"") must NOT
// be overwritten by introspected materialisation. Without the
// explicit foundPrimary gate, a "cache is empty" trigger would
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
// load-bearing assertion for the dual-view registry split: an
// explicit runtime strategy-parent override is hidden from the
// BuildRequest-safe registry view (so leaf calls aren't poisoned by
// to_clients eagerly parsing every entry) but kept in the legacy
// view (so BAML can honour the override on legacy fallthrough).
func TestSetClientRegistryKeepsExplicitStrategyParentForLegacyOnly(t *testing.T) {
	// expectedProvider is the upstream BAML registry's spelling for
	// the row, baked in as a literal so the assertion does not share
	// an oracle with production. SetClientRegistry calls
	// bamlutils.UpstreamClientRegistryProvider internally; computing
	// the expectation via the same helper would mask any drift in
	// canonicalisation (e.g. an alias accidentally folded onto the
	// wrong upstream spelling). Per
	// bamlutils.TranslateUpstreamProvider, only "baml-roundrobin"
	// gets rewritten to "baml-round-robin"; every other RR/fallback
	// alias passes through verbatim.
	cases := []struct {
		name             string
		client           *bamlutils.ClientProperty
		expectedProvider string
	}{
		{
			name: "explicit baml-fallback parent with valid array strategy",
			client: &bamlutils.ClientProperty{
				Name:     "TestFallbackPair",
				Provider: "baml-fallback",
				Options:  map[string]any{"strategy": []any{"RuntimePrimary", "RuntimeSecondary"}},
			},
			expectedProvider: "baml-fallback",
		},
		{
			name: "explicit baml-roundrobin parent with valid array strategy",
			client: &bamlutils.ClientProperty{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"strategy": []any{"A", "B"}, "start": 1},
			},
			expectedProvider: "baml-round-robin",
		},
		{
			name: "RR parent with invalid string start (BAML must see it to emit canonical error)",
			client: &bamlutils.ClientProperty{
				Name:     "MyRR",
				Provider: "baml-roundrobin",
				Options:  map[string]any{"strategy": []any{"A", "B"}, "start": "not-an-int"},
			},
			expectedProvider: "baml-round-robin",
		},
		{
			name: "strategy-only RR override on a static parent (introspected provider drives classification)",
			client: &bamlutils.ClientProperty{
				Name:    "StaticRR",
				Options: map[string]any{"strategy": []any{"A", "B"}},
			},
			expectedProvider: "baml-round-robin",
		},
		{
			name: "explicit round-robin alias (BAML upstream spelling)",
			client: &bamlutils.ClientProperty{
				Name:     "MyRR",
				Provider: "round-robin",
				Options:  map[string]any{"strategy": []any{"A", "B"}},
			},
			expectedProvider: "round-robin",
		},
		{
			name: "explicit fallback alias",
			client: &bamlutils.ClientProperty{
				Name:     "MyFb",
				Provider: "fallback",
				Options:  map[string]any{"strategy": []any{"A", "B"}},
			},
			expectedProvider: "fallback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &BamlAdapter{
				Context: context.Background(),
				IntrospectedClientProvider: map[string]string{
					"StaticRR":         "baml-roundrobin",
					"TestFallbackPair": "baml-fallback",
					"MyRR":             "baml-roundrobin",
					"MyFb":             "baml-fallback",
				},
			}
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{tc.client},
			}
			if err := a.SetClientRegistry(reg); err != nil {
				t.Fatalf("SetClientRegistry: unexpected error: %v", err)
			}
			// BuildRequest view: parent dropped — to_clients is
			// shielded from rejected shapes when leaf calls drive
			// dispatch.
			if names := upstreamClientNamesSnapshot(a); len(names) != 0 {
				t.Errorf("UpstreamClientNames(): got %v, want empty (BuildRequest view must drop explicit strategy parent)", names)
			}
			// Legacy view: parent kept — BAML executes the override
			// or emits canonical error on legacy fallthrough.
			legacyNames := legacyUpstreamClientNamesSnapshot(a)
			if len(legacyNames) != 1 || legacyNames[0] != tc.client.Name {
				t.Errorf("LegacyUpstreamClientNames(): got %v, want [%s] (legacy view must preserve explicit strategy parent)", legacyNames, tc.client.Name)
			}

			// Pin (provider, options) passed to BAML, not just the
			// name. v0.219 forwards options unwrapped, so direct
			// DeepEqual works.
			provider, options, ok := legacyClientEntrySnapshot(a, tc.client.Name)
			if !ok {
				t.Fatalf("legacyClientEntrySnapshot: entry %q missing from BAML's legacy registry", tc.client.Name)
			}
			if provider != tc.expectedProvider {
				t.Errorf("legacy provider for %q: got %q, want %q", tc.client.Name, provider, tc.expectedProvider)
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
	// Pin the absence at the BAML-registry seam itself, not just the
	// adapter-side name lists. The snapshot-name lists are populated
	// from the same code path that calls AddLlmClient, so a regression
	// that bypassed the drop predicate AND populated the name list
	// would still pass the length checks above. Direct registry probes
	// pin "the entry never reached BAML's registry under this name" in
	// both views.
	if _, _, ok := buildRequestClientEntrySnapshot(a, "TestFallbackPair"); ok {
		t.Errorf("BuildRequest registry should drop inert presence-only parent %q; entry present", "TestFallbackPair")
	}
	if _, _, ok := legacyClientEntrySnapshot(a, "TestFallbackPair"); ok {
		t.Errorf("legacy registry should drop inert presence-only parent %q; entry present", "TestFallbackPair")
	}
}

// TestSetClientRegistry_DualViewPrimaryCacheUnderInertParent pins the
// primary-cache rule across both registry views.
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

// TestSetClientRegistry_PresentEmptyPrimaryIsNoOp pins that a runtime
// client_registry payload with `"primary": ""` (Primary != nil but
// *Primary == "") must NOT propagate the empty string to BAML's
// registry. BAML stores "" verbatim and PromptRenderer fails on the
// resulting empty-name lookup. Most baml-rest helpers already treat
// empty primary as a no-op; the adapter must too.
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
	// Guard the registry refs are still attached. clientRegistry-
	// PrimarySnapshot returns nil for a nil *baml.ClientRegistry,
	// which is also the value expected for "primary unset" — so a
	// regression that erroneously cleared a.ClientRegistry /
	// a.LegacyClientRegistry on present-empty primary would let the
	// downstream snapshot/provider checks pass via the same nil
	// signal. Pin non-nil here before those checks.
	if a.ClientRegistry == nil {
		t.Fatal("present-empty primary should not clear ClientRegistry")
	}
	if a.LegacyClientRegistry == nil {
		t.Fatal("present-empty primary should not clear LegacyClientRegistry")
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("present-empty primary leaked stale provider: ClientRegistryProvider() = %q, want \"\"", got)
	}
	// Sanity: original is preserved verbatim (caller's contract).
	if orig := a.OriginalClientRegistry(); orig == nil || orig.Primary == nil || *orig.Primary != "" {
		t.Errorf("OriginalClientRegistry should preserve the present-empty primary verbatim; got %+v", orig)
	}
	// See v0.204 sibling for rationale. Length checks because the
	// implementation reuses slices via `[:0]`.
	if got := upstreamClientNamesSnapshot(a); len(got) != 0 {
		t.Errorf("upstreamClientNamesSnapshot after present-empty primary: got %v, want empty", got)
	}
	if got := legacyUpstreamClientNamesSnapshot(a); len(got) != 0 {
		t.Errorf("legacyUpstreamClientNamesSnapshot after present-empty primary: got %v, want empty", got)
	}
	// Pin the BAML-private `primary *string` field directly. Length-
	// only checks above can't catch a regression that calls
	// SetPrimaryClient("") while still forwarding zero clients —
	// BAML would store "" verbatim and PromptRenderer would fail on the
	// empty-name lookup downstream.
	if got := clientRegistryPrimarySnapshot(a.ClientRegistry); got != nil {
		t.Errorf("ClientRegistry primary after present-empty: got %q, want nil", *got)
	}
	if got := clientRegistryPrimarySnapshot(a.LegacyClientRegistry); got != nil {
		t.Errorf("LegacyClientRegistry primary after present-empty: got %q, want nil", *got)
	}
	// Pin that the primed entry actually disappeared from the BAML
	// registry's internal client map. SetClientRegistry creates fresh
	// BuildRequest + legacy registries on every non-nil call, so the
	// follow-up call with `Clients: nil` should leave both views with
	// no `"GoodClient"` entry. The name-list / primary-snapshot checks
	// above would still pass under a regression that reused the prior
	// registry's clients map verbatim across the reset; only a direct
	// per-name probe pins the BAML registry's internal map state.
	if _, _, ok := buildRequestClientEntrySnapshot(a, "GoodClient"); ok {
		t.Errorf("BuildRequest registry should drop primed entry %q after reset; entry present", "GoodClient")
	}
	if _, _, ok := legacyClientEntrySnapshot(a, "GoodClient"); ok {
		t.Errorf("legacy registry should drop primed entry %q after reset; entry present", "GoodClient")
	}
}

// TestSetClientRegistry_RejectsDuplicateClientName pins that runtime
// registries with operator-typed duplicate Names are rejected before
// any adapter state mutates. BAML's AddLlmClient is map-backed (last-
// wins on duplicate names); every baml-rest lookup is slice-forward
// (first-match). Letting both halves classify against different
// definitions silently desyncs route classification, support gating,
// retry policy, RR inspection, and X-BAML-Path-Reason from what BAML
// actually executes. Validate at the entry-point surfaces the
// operator's ambiguous input as a hard error.
//
// Each row:
//  1. Seeds a valid prior registry so the post-rejection state check
//     can compare against a known baseline.
//  2. Calls SetClientRegistry with the duplicate fixture and asserts
//     the error matches ErrDuplicateClientName via errors.Is.
//  3. Asserts no half-apply: ClientRegistry / LegacyClientRegistry /
//     OriginalClientRegistry / clientRegistryProvider must equal
//     their post-seed values (the rejected call must not clobber
//     them).
func TestSetClientRegistry_RejectsDuplicateClientName(t *testing.T) {
	cases := []struct {
		name    string
		clients []*bamlutils.ClientProperty
	}{
		{
			name: "two entries same name, different providers",
			clients: []*bamlutils.ClientProperty{
				{Name: "Tenant", Provider: "openai"},
				{Name: "Tenant", Provider: "anthropic"},
			},
		},
		{
			name: "two entries same name, same provider — still reject",
			clients: []*bamlutils.ClientProperty{
				{Name: "Tenant", Provider: "openai"},
				{Name: "Tenant", Provider: "openai"},
			},
		},
		{
			name: "two entries same name, second is present-empty (the load-bearing case)",
			clients: []*bamlutils.ClientProperty{
				{Name: "Tenant", Provider: "openai"},
				{Name: "Tenant", Provider: "", ProviderSet: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &BamlAdapter{
				Context: context.Background(),
				IntrospectedClientProvider: map[string]string{
					"SeedClient": "openai",
				},
			}
			seedPrimary := "SeedClient"
			seed := &bamlutils.ClientRegistry{
				Primary: &seedPrimary,
				Clients: []*bamlutils.ClientProperty{
					{Name: "SeedClient", Provider: "openai"},
				},
			}
			if err := a.SetClientRegistry(seed); err != nil {
				t.Fatalf("seed SetClientRegistry: %v", err)
			}
			seedClientRegistry := a.ClientRegistry
			seedLegacyRegistry := a.LegacyClientRegistry
			seedOriginal := a.OriginalClientRegistry()
			seedProvider := a.ClientRegistryProvider()
			seedUpstream := upstreamClientNamesSnapshot(a)
			seedLegacyUpstream := legacyUpstreamClientNamesSnapshot(a)

			err := a.SetClientRegistry(&bamlutils.ClientRegistry{
				Clients: tc.clients,
			})
			if err == nil {
				t.Fatalf("SetClientRegistry: want ErrDuplicateClientName, got nil")
			}
			if !errors.Is(err, bamlutils.ErrDuplicateClientName) {
				t.Errorf("SetClientRegistry: want errors.Is(err, ErrDuplicateClientName) true, got err=%v", err)
			}

			// State preservation: rejected call must not half-apply.
			if a.ClientRegistry != seedClientRegistry {
				t.Errorf("ClientRegistry mutated after rejected call; want preserved seed pointer")
			}
			if a.LegacyClientRegistry != seedLegacyRegistry {
				t.Errorf("LegacyClientRegistry mutated after rejected call; want preserved seed pointer")
			}
			if a.OriginalClientRegistry() != seedOriginal {
				t.Errorf("OriginalClientRegistry mutated after rejected call; want preserved seed pointer")
			}
			if got := a.ClientRegistryProvider(); got != seedProvider {
				t.Errorf("ClientRegistryProvider mutated after rejected call: got %q, want %q", got, seedProvider)
			}
			if got := upstreamClientNamesSnapshot(a); !reflect.DeepEqual(got, seedUpstream) {
				t.Errorf("upstreamClientNamesSnapshot mutated after rejected call: got %v, want %v", got, seedUpstream)
			}
			if got := legacyUpstreamClientNamesSnapshot(a); !reflect.DeepEqual(got, seedLegacyUpstream) {
				t.Errorf("legacyUpstreamClientNamesSnapshot mutated after rejected call: got %v, want %v", got, seedLegacyUpstream)
			}
		})
	}
}

// TestSetClientRegistry_AcceptsDistinctClientNames is the inverse-
// regression control for TestSetClientRegistry_RejectsDuplicateClientName:
// a registry with multiple entries that all carry distinct Names must
// still flow through unchanged. Without this control, an over-broad
// validation refactor (e.g. rejecting >1 client uniformly) could
// silently break valid multi-entry registries and the rejection test
// alone wouldn't catch it.
func TestSetClientRegistry_AcceptsDistinctClientNames(t *testing.T) {
	a := &BamlAdapter{
		Context:                    context.Background(),
		IntrospectedClientProvider: map[string]string{},
	}
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "ClientA", Provider: "openai"},
			{Name: "ClientB", Provider: "anthropic"},
			{Name: "ClientC", Provider: "google-ai"},
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	got := upstreamClientNamesSnapshot(a)
	want := []string{"ClientA", "ClientB", "ClientC"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upstreamClientNamesSnapshot: got %v, want %v", got, want)
	}
}

// TestSetClientRegistry_RejectsEmptyClientName pins that runtime
// registries with operator-typed empty Names are rejected before any
// adapter state mutates. Empty Name is a typo that BAML's
// AddLlmClient would index under "" while every baml-rest lookup
// keys on Name — the entry becomes a nameless ghost the resolver /
// classifier can't reach but BAML may still execute. Worse, a
// forward-loop skip in the BAML-bound views alone wouldn't close the
// vector: OriginalClientRegistry is captured at the top of
// SetClientRegistry, and generated scoped-child registries
// (codegen.go BuildLegacyChildRegistryEntries) read the original, so
// the empty-Name entry could re-enter through that seam. Rejecting
// at Validate() is the only spot that closes both.
//
// Each row mirrors the shape of TestSetClientRegistry_RejectsDuplicate-
// ClientName: seed valid prior state, attempt the empty-Name fixture,
// assert errors.Is matches ErrEmptyClientName AND adapter state
// preserved (no half-apply).
func TestSetClientRegistry_RejectsEmptyClientName(t *testing.T) {
	cases := []struct {
		name    string
		clients []*bamlutils.ClientProperty
	}{
		{
			name: "empty Name first entry",
			clients: []*bamlutils.ClientProperty{
				{Name: "", Provider: "openai"},
			},
		},
		{
			name: "empty Name later in slice (validation scans whole slice)",
			clients: []*bamlutils.ClientProperty{
				{Name: "GoodClient", Provider: "openai"},
				{Name: "", Provider: "anthropic"},
			},
		},
		{
			name: "empty Name with present-empty Provider (operator double-typo)",
			clients: []*bamlutils.ClientProperty{
				{Name: "", Provider: "", ProviderSet: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &BamlAdapter{
				Context: context.Background(),
				IntrospectedClientProvider: map[string]string{
					"SeedClient": "openai",
				},
			}
			seedPrimary := "SeedClient"
			seed := &bamlutils.ClientRegistry{
				Primary: &seedPrimary,
				Clients: []*bamlutils.ClientProperty{
					{Name: "SeedClient", Provider: "openai"},
				},
			}
			if err := a.SetClientRegistry(seed); err != nil {
				t.Fatalf("seed SetClientRegistry: %v", err)
			}
			seedClientRegistry := a.ClientRegistry
			seedLegacyRegistry := a.LegacyClientRegistry
			seedOriginal := a.OriginalClientRegistry()
			seedProvider := a.ClientRegistryProvider()
			seedUpstream := upstreamClientNamesSnapshot(a)
			seedLegacyUpstream := legacyUpstreamClientNamesSnapshot(a)

			err := a.SetClientRegistry(&bamlutils.ClientRegistry{
				Clients: tc.clients,
			})
			if err == nil {
				t.Fatalf("SetClientRegistry: want ErrEmptyClientName, got nil")
			}
			if !errors.Is(err, bamlutils.ErrEmptyClientName) {
				t.Errorf("SetClientRegistry: want errors.Is(err, ErrEmptyClientName) true, got err=%v", err)
			}

			// State preservation: rejected call must not half-apply.
			if a.ClientRegistry != seedClientRegistry {
				t.Errorf("ClientRegistry mutated after rejected call; want preserved seed pointer")
			}
			if a.LegacyClientRegistry != seedLegacyRegistry {
				t.Errorf("LegacyClientRegistry mutated after rejected call; want preserved seed pointer")
			}
			if a.OriginalClientRegistry() != seedOriginal {
				t.Errorf("OriginalClientRegistry mutated after rejected call; want preserved seed pointer")
			}
			if got := a.ClientRegistryProvider(); got != seedProvider {
				t.Errorf("ClientRegistryProvider mutated after rejected call: got %q, want %q", got, seedProvider)
			}
			if got := upstreamClientNamesSnapshot(a); !reflect.DeepEqual(got, seedUpstream) {
				t.Errorf("upstreamClientNamesSnapshot mutated after rejected call: got %v, want %v", got, seedUpstream)
			}
			if got := legacyUpstreamClientNamesSnapshot(a); !reflect.DeepEqual(got, seedLegacyUpstream) {
				t.Errorf("legacyUpstreamClientNamesSnapshot mutated after rejected call: got %v, want %v", got, seedLegacyUpstream)
			}
		})
	}
}
