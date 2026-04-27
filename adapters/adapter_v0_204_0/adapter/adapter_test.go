package adapter

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// Verdict-11 findings 3 + 4: SetClientRegistry must not panic on a
// nil registry, must skip nil client entries inside Clients, and must
// clear clientRegistryProvider before scanning so a second call with
// no matching primary doesn't leak the prior cached value. These
// tests pin the v0.219 parity behaviour now that v0.204's adapter
// has been ported.
//
// NOTE: v0.204 has a pre-existing compile failure in dynamic_value.go
// against the v0.204 BAML CFFI types (CFFIValueHolder/BamlObjectHandle
// API mismatch); this test file is committed for parity coverage and
// will run once the unrelated build issue is resolved.

// TestSetClientRegistry_NilRegistryClearsState verifies the nil-input
// path: the adapter must clear every registry view (BuildRequest,
// legacy, original, primary cache, both upstreamClientNames lists)
// and return without panicking.
func TestSetClientRegistry_NilRegistryClearsState(t *testing.T) {
	a := &BamlAdapter{Context: context.Background()}
	// Seed stale state so the test catches a regression where
	// subsequent nil clears stop wiping.
	primary := "OldClient"
	if err := a.SetClientRegistry(&bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{{Name: "OldClient", Provider: "openai"}},
	}); err != nil {
		t.Fatalf("seed call: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Fatalf("seed: ClientRegistryProvider got %q, want openai", got)
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
		t.Errorf("ClientRegistryProvider(): got %q, want empty (stale value not cleared)", got)
	}
	if names := upstreamClientNamesSnapshot(a); len(names) != 0 {
		t.Errorf("UpstreamClientNames(): got %v, want empty (stale list leaked across nil reset)", names)
	}
	if names := legacyUpstreamClientNamesSnapshot(a); len(names) != 0 {
		t.Errorf("LegacyUpstreamClientNames(): got %v, want empty (stale legacy list leaked across nil reset)", names)
	}
}

// TestSetClientRegistry_SkipsNilClientEntries verifies the nil-client
// skip in both loops (forward + primary scan). A JSON-shaped
// `clients: [null]` is allowed by the *ClientProperty slice and must
// not panic.
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
		t.Errorf("UpstreamClientNames(): got %v, want [RealClient] (nil entries must be skipped)", got)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Errorf("ClientRegistryProvider(): got %q, want openai", got)
	}
}

// TestSetClientRegistry_ClearsStaleProviderCache verifies that a
// second call without a matching primary does not retain the prior
// provider in clientRegistryProvider. Without the explicit clear at
// the start of SetClientRegistry, the second call would return the
// stale value from the first.
func TestSetClientRegistry_ClearsStaleProviderCache(t *testing.T) {
	a := &BamlAdapter{
		Context:                    context.Background(),
		IntrospectedClientProvider: map[string]string{},
	}
	// First call: primary set, cache populated.
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
	// Second call: no primary, cache must clear.
	if err := a.SetClientRegistry(&bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{{Name: "OtherClient", Provider: "anthropic"}},
	}); err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("second call: ClientRegistryProvider got %q, want empty (stale cache from first call leaked)", got)
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
// load-bearing assertion for PR #192 cold-review-4 + Option C — see
// the v0.219 adapter test file for the full rationale.
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
			if names := upstreamClientNamesSnapshot(a); len(names) != 0 {
				t.Errorf("UpstreamClientNames(): got %v, want empty (BuildRequest view must drop explicit strategy parent)", names)
			}
			legacyNames := legacyUpstreamClientNamesSnapshot(a)
			if len(legacyNames) != 1 || legacyNames[0] != tc.client.Name {
				t.Errorf("LegacyUpstreamClientNames(): got %v, want [%s] (legacy view must preserve explicit strategy parent)", legacyNames, tc.client.Name)
			}
		})
	}
}

// TestSetClientRegistryDropsInertPresenceOnlyParentInBothBamlViews
// pins the no-op invariant for inert presence-only static parents —
// see v0.219 adapter test file for the rationale.
func TestSetClientRegistryDropsInertPresenceOnlyParentInBothBamlViews(t *testing.T) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "TestFallbackPair"},
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
// fix for CodeRabbit verdict-21 findings 1+2 generalised across views
// — see v0.219 adapter test file for the rationale.
func TestSetClientRegistry_DualViewPrimaryCacheUnderInertParent(t *testing.T) {
	primary := "TestFallbackPair"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "TestFallbackPair"},
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
	if got := a.ClientRegistryProvider(); got != "baml-fallback" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-fallback (introspected synthesis under dropped primary)", got)
	}
}

// TestSetClientRegistry_DualViewPrimaryCacheUnderExplicitParent —
// see v0.219 adapter test file for the rationale.
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
			"MyParent": "baml-roundrobin",
		},
	}
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "baml-fallback" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-fallback (operator runtime override must win over introspected)", got)
	}
}

// TestSetClientRegistry_PresentEmptyPrimaryIsNoOp mirrors the v0.219
// regression test for CodeRabbit verdict-31 finding F1. See the
// v0.219 sibling for the full rationale; same shape applies here.
func TestSetClientRegistry_PresentEmptyPrimaryIsNoOp(t *testing.T) {
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"GoodClient": "openai",
		},
	}

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
	if orig := a.OriginalClientRegistry(); orig == nil || orig.Primary == nil || *orig.Primary != "" {
		t.Errorf("OriginalClientRegistry should preserve the present-empty primary verbatim; got %+v", orig)
	}
}
