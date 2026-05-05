package adapter

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// SetClientRegistry must not panic on a nil registry, must skip nil
// client entries inside Clients, and must clear clientRegistryProvider
// before scanning so a second call with no matching primary doesn't
// leak the prior cached value. These tests pin the v0.219 parity
// behaviour now that v0.215's adapter has been ported.

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
// covers the case where a runtime registry names a
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
// the foundPrimary-flag invariant: a matching primary
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
	// Also pin that the entry itself made it into BAML's
	// BuildRequest-safe registry view with the empty provider
	// preserved verbatim. See v0.219 sibling for full rationale.
	provider, _, ok := buildRequestClientEntrySnapshot(a, "X")
	if !ok {
		t.Fatalf("BuildRequest registry should retain the present-empty entry; got missing")
	}
	if provider != "" {
		t.Errorf("BuildRequest registry provider for X: got %q, want \"\" (present-empty must not be materialised at the registry seam either)", provider)
	}
	// SetClientRegistry materialises both views; the legacy view is
	// the seam BAML reads on the legacy fallthrough path. A regression
	// that dropped the present-empty entry from one view but not the
	// other would fall through to the function's compiled default
	// instead of letting BAML emit its canonical missing-provider
	// error against the operator-typo'd entry.
	legacyProvider, _, ok := legacyClientEntrySnapshot(a, "X")
	if !ok {
		t.Fatalf("legacy registry should retain the present-empty entry; got missing")
	}
	if legacyProvider != "" {
		t.Errorf("legacy registry provider for X: got %q, want \"\" (present-empty must be preserved verbatim in the legacy view)", legacyProvider)
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
// load-bearing assertion for the dual-view registry split — see
// the v0.219 adapter test file for the full rationale.
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
				Name:     "AliasRR",
				Provider: "round-robin",
				Options:  map[string]any{"strategy": []any{"A", "B"}},
			},
			expectedProvider: "round-robin",
		},
		{
			name: "explicit fallback alias",
			client: &bamlutils.ClientProperty{
				Name:     "AliasFb",
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
					"AliasRR":          "baml-roundrobin",
					"AliasFb":          "baml-fallback",
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

			// Pin (provider, options) passed to BAML, not just the
			// name. v0.215 forwards options unwrapped, so direct
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
// primary-cache rule across both registry views
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
// sibling: a present-empty primary must be a no-op at the registry
// seam. See the v0.219 test for the full rationale.
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

// TestSetClientRegistry_RejectsDuplicateClientName mirrors v0.219; see
// the v0.219 adapter test for the BAML-last-wins / baml-rest-first-
// match divergence rationale.
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
// regression control for the duplicate-rejection test — mirrors
// v0.219.
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
	legacyGot := legacyUpstreamClientNamesSnapshot(a)
	if !reflect.DeepEqual(legacyGot, want) {
		t.Errorf("legacyUpstreamClientNamesSnapshot: got %v, want %v (legacy view must mirror BuildRequest view for distinct-name entries)", legacyGot, want)
	}
}

// TestSetClientRegistry_RejectsEmptyClientName mirrors v0.219; see
// the v0.219 adapter test for the empty-Name rejection rationale
// (centralized validator closes the original-registry-leak vector
// that a forward-loop skip alone wouldn't).
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
