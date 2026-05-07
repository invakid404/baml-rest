package testdriver

import (
	"errors"
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// runNilRegistryClearsState verifies the nil-input path: every registry
// view (BuildRequest, legacy, original, primary cache, both
// upstreamClientNames lists) is cleared and the call returns without
// panicking. The test seeds stale state first so a regression that
// stops wiping on subsequent nil clears would surface.
func runNilRegistryClearsState(t *testing.T, factory Factory) {
	a := factory(map[string]string{"OldClient": "openai"})

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
	if names := a.UpstreamClientNamesSnapshot(); len(names) != 1 {
		t.Fatalf("seed: UpstreamClientNames len = %d, want 1", len(names))
	}
	if names := a.LegacyUpstreamClientNamesSnapshot(); len(names) != 1 {
		t.Fatalf("seed: LegacyUpstreamClientNames len = %d, want 1", len(names))
	}

	if err := a.SetClientRegistry(nil); err != nil {
		t.Fatalf("SetClientRegistry(nil): unexpected error: %v", err)
	}
	if !a.BuildRequestRegistryNil() {
		t.Errorf("ClientRegistry: got non-nil, want nil")
	}
	if !a.LegacyRegistryNil() {
		t.Errorf("LegacyClientRegistry: got non-nil, want nil")
	}
	if a.OriginalClientRegistry() != nil {
		t.Errorf("OriginalClientRegistry(): got %v, want nil", a.OriginalClientRegistry())
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("ClientRegistryProvider(): got %q, want empty (stale value not cleared)", got)
	}
	if names := a.UpstreamClientNamesSnapshot(); len(names) != 0 {
		t.Errorf("UpstreamClientNames(): got %v, want empty (stale list leaked across nil reset)", names)
	}
	if names := a.LegacyUpstreamClientNamesSnapshot(); len(names) != 0 {
		t.Errorf("LegacyUpstreamClientNames(): got %v, want empty (stale legacy list leaked across nil reset)", names)
	}
}

// runSkipsNilClientEntries verifies the nil-client skip in both loops
// (forward + primary scan). A JSON-shaped `clients: [null]` is allowed
// by the *ClientProperty slice and must not panic.
func runSkipsNilClientEntries(t *testing.T, factory Factory) {
	primary := "RealClient"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			nil,
			{Name: "RealClient", Provider: "openai"},
			nil,
		},
	}
	a := factory(emptyMap())
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	got := a.UpstreamClientNamesSnapshot()
	if len(got) != 1 || got[0] != "RealClient" {
		t.Errorf("UpstreamClientNames(): got %v, want [RealClient] (nil entries must be skipped)", got)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Errorf("ClientRegistryProvider(): got %q, want openai", got)
	}
}

// runClearsStaleProviderCache verifies that a second call without a
// matching primary does not retain the prior provider in
// clientRegistryProvider. Without the explicit clear at the start of
// SetClientRegistry, the second call would return the stale value.
func runClearsStaleProviderCache(t *testing.T, factory Factory) {
	a := factory(emptyMap())
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
		t.Errorf("second call: ClientRegistryProvider got %q, want empty (stale cache from first call leaked)", got)
	}
}

// runStaticPrimaryPopulatesProviderFromIntrospected covers the case
// where a runtime registry names a static primary by name without
// redefining it in clients[]: the cache must populate from the
// introspected map, not leave it "" where ResolveProvider's adapter
// shortcut would short-circuit to the function default.
func runStaticPrimaryPopulatesProviderFromIntrospected(t *testing.T, factory Factory) {
	primary := "StaticOpenAI"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{},
	}
	a := factory(map[string]string{"StaticOpenAI": "openai"})
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Errorf("ClientRegistryProvider(): got %q, want openai (synthesized from introspected map for unrebound primary)", got)
	}
}

// runPresentEmptyMatchingPrimaryStaysEmpty pins the foundPrimary-flag
// invariant: a matching primary entry that intentionally produces ""
// (operator typo'd provider:"") must NOT be overwritten by introspected
// materialisation. Without the explicit foundPrimary gate, a "cache is
// empty" trigger would clobber this case.
func runPresentEmptyMatchingPrimaryStaysEmpty(t *testing.T, factory Factory) {
	primary := "X"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "X", Provider: "", ProviderSet: true},
		},
	}
	a := factory(map[string]string{"X": "openai"})
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("ClientRegistryProvider(): got %q, want empty (present-empty primary must not be synthesized over)", got)
	}
	provider, _, ok := a.BuildRequestClientEntrySnapshot("X")
	if !ok {
		t.Fatalf("BuildRequest registry should retain the present-empty entry; got missing")
	}
	if provider != "" {
		t.Errorf("BuildRequest registry provider for X: got %q, want \"\" (present-empty must not be materialised at the registry seam either)", provider)
	}
	legacyProvider, _, ok := a.LegacyClientEntrySnapshot("X")
	if !ok {
		t.Fatalf("legacy registry should retain the present-empty entry; got missing")
	}
	if legacyProvider != "" {
		t.Errorf("legacy registry provider for X: got %q, want \"\" (present-empty must be preserved verbatim in the legacy view)", legacyProvider)
	}
}

// runPresentNonEmptyMatchingPrimaryUsesOverride is the inverse-
// regression guard: a matching primary entry with a concrete provider
// must populate the cache from that override, not from the introspected
// map. Without this guard, a future refactor of the foundPrimary logic
// could swap precedence by accident.
func runPresentNonEmptyMatchingPrimaryUsesOverride(t *testing.T, factory Factory) {
	primary := "X"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "X", Provider: "openai"},
		},
	}
	a := factory(map[string]string{"X": "anthropic"})
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "openai" {
		t.Errorf("ClientRegistryProvider(): got %q, want openai (operator override must win over introspected)", got)
	}
}

// runKeepsExplicitStrategyParentForLegacyOnly is the load-bearing
// assertion for the dual-view registry split: an explicit runtime
// strategy-parent override is hidden from the BuildRequest-safe view
// (so leaf calls aren't poisoned by to_clients eagerly parsing every
// entry) but kept in the legacy view (so BAML can honour the override
// on legacy fallthrough).
//
// expectedProvider is the upstream BAML registry's spelling for the
// row, baked in as a literal so the assertion does not share an oracle
// with production. SetClientRegistry calls bamlutils.UpstreamClient-
// RegistryProvider internally; computing the expectation via the same
// helper would mask any drift in canonicalisation. Per
// bamlutils.TranslateUpstreamProvider, only "baml-roundrobin" gets
// rewritten to "baml-round-robin"; every other RR/fallback alias
// passes through verbatim.
func runKeepsExplicitStrategyParentForLegacyOnly(t *testing.T, factory Factory) {
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
			a := factory(map[string]string{
				"StaticRR":         "baml-roundrobin",
				"TestFallbackPair": "baml-fallback",
				"AliasRR":          "baml-roundrobin",
				"AliasFb":          "baml-fallback",
				"MyRR":             "baml-roundrobin",
				"MyFb":             "baml-fallback",
			})
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{tc.client},
			}
			if err := a.SetClientRegistry(reg); err != nil {
				t.Fatalf("SetClientRegistry: unexpected error: %v", err)
			}
			if names := a.UpstreamClientNamesSnapshot(); len(names) != 0 {
				t.Errorf("UpstreamClientNames(): got %v, want empty (BuildRequest view must drop explicit strategy parent)", names)
			}
			legacyNames := a.LegacyUpstreamClientNamesSnapshot()
			if len(legacyNames) != 1 || legacyNames[0] != tc.client.Name {
				t.Errorf("LegacyUpstreamClientNames(): got %v, want [%s] (legacy view must preserve explicit strategy parent)", legacyNames, tc.client.Name)
			}

			provider, options, ok := a.LegacyClientEntrySnapshot(tc.client.Name)
			if !ok {
				t.Fatalf("legacyClientEntrySnapshot: entry %q missing from BAML's legacy registry", tc.client.Name)
			}
			if provider != tc.expectedProvider {
				t.Errorf("legacy provider for %q: got %q, want %q", tc.client.Name, provider, tc.expectedProvider)
			}
			// ExpectedOptions translates the operator-supplied input
			// into the form BAML's internal client map stores: v0.204
			// wraps every value through DynamicValue (the older CFFI
			// shape requires it for nested-map serialisation), v0.215+
			// pass through unchanged. Comparing snapshot to the
			// translated oracle works for all three adapters.
			wantOptions := a.ExpectedOptions(tc.client.Options)
			if want, ok := wantOptions["strategy"]; ok {
				if !reflect.DeepEqual(options["strategy"], want) {
					t.Errorf("legacy options[strategy] for %q: got %v, want %v", tc.client.Name, options["strategy"], want)
				}
			}
			if want, ok := wantOptions["start"]; ok {
				if !reflect.DeepEqual(options["start"], want) {
					t.Errorf("legacy options[start] for %q: got %v, want %v", tc.client.Name, options["start"], want)
				}
			}
		})
	}
}

// runDropsInertPresenceOnlyParentInBothBamlViews pins the no-op
// invariant for inert presence-only static parents.
// `{Name: "TestFallbackPair"}` with no provider/strategy/start carries
// no operator intent — it's just naming a static client by name.
// Forwarding it to either BAML view would fail to_clients on missing
// required `strategy`. Both views must drop it.
func runDropsInertPresenceOnlyParentInBothBamlViews(t *testing.T, factory Factory) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "TestFallbackPair"},
		},
	}
	a := factory(map[string]string{"TestFallbackPair": "baml-fallback"})
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if names := a.UpstreamClientNamesSnapshot(); len(names) != 0 {
		t.Errorf("UpstreamClientNames(): got %v, want empty (inert presence-only parent must be dropped)", names)
	}
	if names := a.LegacyUpstreamClientNamesSnapshot(); len(names) != 0 {
		t.Errorf("LegacyUpstreamClientNames(): got %v, want empty (inert presence-only parent must be dropped from legacy view too)", names)
	}
	if _, _, ok := a.BuildRequestClientEntrySnapshot("TestFallbackPair"); ok {
		t.Errorf("BuildRequest registry should drop inert presence-only parent %q; entry present", "TestFallbackPair")
	}
	if _, _, ok := a.LegacyClientEntrySnapshot("TestFallbackPair"); ok {
		t.Errorf("legacy registry should drop inert presence-only parent %q; entry present", "TestFallbackPair")
	}
}

// runDualViewPrimaryCacheUnderInertParent: when the operator names a
// primary that exists only as an inert presence-only static parent
// (dropped from BOTH views), the primary cache must NOT observe a
// provider that no view exposes. Falls through to introspected
// synthesis so ResolveProvider's shortcut returns the static-config
// provider rather than "" or a wrong override.
func runDualViewPrimaryCacheUnderInertParent(t *testing.T, factory Factory) {
	primary := "TestFallbackPair"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "TestFallbackPair"},
		},
	}
	a := factory(map[string]string{"TestFallbackPair": "baml-fallback"})
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "baml-fallback" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-fallback (introspected synthesis under dropped primary)", got)
	}
}

// runDualViewPrimaryCacheUnderExplicitParent is the companion guard:
// an explicit runtime strategy-parent override stays in the legacy
// view, so the primary cache should observe its provider (not the
// introspected default). Without this, an operator switching to a
// runtime baml-fallback override on an existing static RR client would
// see ResolveProvider report the static RR provider instead of the
// runtime fallback.
func runDualViewPrimaryCacheUnderExplicitParent(t *testing.T, factory Factory) {
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
	a := factory(map[string]string{"MyParent": "baml-roundrobin"})
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "baml-fallback" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-fallback (operator runtime override must win over introspected)", got)
	}
}

// runPresentEmptyPrimaryIsNoOp pins that a runtime client_registry
// payload with `"primary": ""` must NOT propagate the empty string to
// BAML's registry. BAML stores "" verbatim and PromptRenderer fails on
// the resulting empty-name lookup.
//
// Runs SetClientRegistry twice: first with a real primary (cache
// populated, primary forwarded), then with present-empty primary on
// the same adapter. The second call must clear the cache and must not
// invoke SetPrimaryClient or synthesize an empty-named ClientProperty.
func runPresentEmptyPrimaryIsNoOp(t *testing.T, factory Factory) {
	a := factory(map[string]string{"GoodClient": "openai"})

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
	// Guard the registry refs are still attached. Primary snapshot
	// returns nil for a nil registry pointer, which is also the value
	// expected for "primary unset" — so a regression that erroneously
	// cleared the registries on present-empty primary would let the
	// downstream snapshot/provider checks pass via the same nil signal.
	if a.BuildRequestRegistryNil() {
		t.Fatal("present-empty primary should not clear ClientRegistry")
	}
	if a.LegacyRegistryNil() {
		t.Fatal("present-empty primary should not clear LegacyClientRegistry")
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("present-empty primary leaked stale provider: ClientRegistryProvider() = %q, want \"\"", got)
	}
	if orig := a.OriginalClientRegistry(); orig == nil || orig.Primary == nil || *orig.Primary != "" {
		t.Errorf("OriginalClientRegistry should preserve the present-empty primary verbatim; got %+v", orig)
	}
	if got := a.UpstreamClientNamesSnapshot(); len(got) != 0 {
		t.Errorf("upstreamClientNamesSnapshot after present-empty primary: got %v, want empty", got)
	}
	if got := a.LegacyUpstreamClientNamesSnapshot(); len(got) != 0 {
		t.Errorf("legacyUpstreamClientNamesSnapshot after present-empty primary: got %v, want empty", got)
	}
	if got := a.BuildRequestPrimarySnapshot(); got != nil {
		t.Errorf("ClientRegistry primary after present-empty: got %q, want nil", *got)
	}
	if got := a.LegacyPrimarySnapshot(); got != nil {
		t.Errorf("LegacyClientRegistry primary after present-empty: got %q, want nil", *got)
	}
	if _, _, ok := a.BuildRequestClientEntrySnapshot("GoodClient"); ok {
		t.Errorf("BuildRequest registry should drop primed entry %q after reset; entry present", "GoodClient")
	}
	if _, _, ok := a.LegacyClientEntrySnapshot("GoodClient"); ok {
		t.Errorf("legacy registry should drop primed entry %q after reset; entry present", "GoodClient")
	}
}

// runRejectsDuplicateClientName pins that runtime registries with
// operator-typed duplicate Names are rejected before any adapter state
// mutates. BAML's AddLlmClient is map-backed (last-wins on duplicate
// names); every baml-rest lookup is slice-forward (first-match).
// Letting both halves classify against different definitions silently
// desyncs route classification, support gating, retry policy, RR
// inspection, and X-BAML-Path-Reason from what BAML actually executes.
//
// Each row seeds a valid prior registry, then attempts the duplicate
// fixture and asserts the error matches ErrDuplicateClientName via
// errors.Is plus full state preservation (no half-apply).
func runRejectsDuplicateClientName(t *testing.T, factory Factory) {
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
			runRegistryRejectionCase(t, factory, tc.clients, bamlutils.ErrDuplicateClientName)
		})
	}
}

// runAcceptsDistinctClientNames is the inverse-regression control for
// runRejectsDuplicateClientName: a registry with multiple entries that
// all carry distinct Names must still flow through unchanged. Without
// this control, an over-broad validation refactor (e.g. rejecting >1
// client uniformly) could silently break valid multi-entry registries
// and the rejection test alone wouldn't catch it.
func runAcceptsDistinctClientNames(t *testing.T, factory Factory) {
	a := factory(emptyMap())
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
	got := a.UpstreamClientNamesSnapshot()
	want := []string{"ClientA", "ClientB", "ClientC"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upstreamClientNamesSnapshot: got %v, want %v", got, want)
	}
	legacyGot := a.LegacyUpstreamClientNamesSnapshot()
	if !reflect.DeepEqual(legacyGot, want) {
		t.Errorf("legacyUpstreamClientNamesSnapshot: got %v, want %v (legacy view must mirror BuildRequest view for distinct-name entries)", legacyGot, want)
	}
}

// runRejectsEmptyClientName pins that runtime registries with
// operator-typed empty Names are rejected before any adapter state
// mutates. Empty Name is a typo that BAML's AddLlmClient would index
// under "" while every baml-rest lookup keys on Name — the entry
// becomes a nameless ghost the resolver / classifier can't reach but
// BAML may still execute. A forward-loop skip in the BAML-bound views
// alone wouldn't close the vector: OriginalClientRegistry is captured
// at the top of SetClientRegistry, and generated scoped-child
// registries (codegen.go BuildLegacyChildRegistryEntries) read the
// original, so the empty-Name entry could re-enter through that seam.
// Rejecting at Validate() is the only spot that closes both.
func runRejectsEmptyClientName(t *testing.T, factory Factory) {
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
			runRegistryRejectionCase(t, factory, tc.clients, bamlutils.ErrEmptyClientName)
		})
	}
}

// runRegistryRejectionCase is the shared body for the duplicate /
// empty-name rejection tests. Both shapes seed a valid prior registry,
// run the rejected fixture, and assert the same set of state-
// preservation invariants. Centralising the body keeps the contract in
// one place — a regression that half-applies one rejection branch but
// not the other can't slip past via copy-paste drift.
func runRegistryRejectionCase(t *testing.T, factory Factory, clients []*bamlutils.ClientProperty, want error) {
	t.Helper()
	a := factory(map[string]string{"SeedClient": "openai"})
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
	seedClientRegistry := a.BuildRequestRegistry()
	seedLegacyRegistry := a.LegacyRegistry()
	seedOriginal := a.OriginalClientRegistry()
	seedProvider := a.ClientRegistryProvider()
	seedUpstream := a.UpstreamClientNamesSnapshot()
	seedLegacyUpstream := a.LegacyUpstreamClientNamesSnapshot()

	err := a.SetClientRegistry(&bamlutils.ClientRegistry{Clients: clients})
	if err == nil {
		t.Fatalf("SetClientRegistry: want %v, got nil", want)
	}
	if !errors.Is(err, want) {
		t.Errorf("SetClientRegistry: want errors.Is(err, %v) true, got err=%v", want, err)
	}

	if a.BuildRequestRegistry() != seedClientRegistry {
		t.Errorf("ClientRegistry mutated after rejected call; want preserved seed pointer")
	}
	if a.LegacyRegistry() != seedLegacyRegistry {
		t.Errorf("LegacyClientRegistry mutated after rejected call; want preserved seed pointer")
	}
	if a.OriginalClientRegistry() != seedOriginal {
		t.Errorf("OriginalClientRegistry mutated after rejected call; want preserved seed pointer")
	}
	if got := a.ClientRegistryProvider(); got != seedProvider {
		t.Errorf("ClientRegistryProvider mutated after rejected call: got %q, want %q", got, seedProvider)
	}
	if got := a.UpstreamClientNamesSnapshot(); !reflect.DeepEqual(got, seedUpstream) {
		t.Errorf("upstreamClientNamesSnapshot mutated after rejected call: got %v, want %v", got, seedUpstream)
	}
	if got := a.LegacyUpstreamClientNamesSnapshot(); !reflect.DeepEqual(got, seedLegacyUpstream) {
		t.Errorf("legacyUpstreamClientNamesSnapshot mutated after rejected call: got %v, want %v", got, seedLegacyUpstream)
	}
}

// runDropsResolvedStrategyParents pins that baml-rest-resolved RR /
// fallback strategy parent entries must not be forwarded into BAML's
// upstream registry — BAML rejects partial RR shapes at parse time and
// the resolver dispatches with WithClient(leaf) so the parent has no
// role in BAML's runtime anyway. Original registry stays untouched in
// every case.
func runDropsResolvedStrategyParents(t *testing.T, factory Factory) {
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
			a := factory(map[string]string{
				"MyRR": "baml-roundrobin",
				"MyFb": "baml-fallback",
			})
			reg := &bamlutils.ClientRegistry{
				Clients: []*bamlutils.ClientProperty{tc.client},
			}
			if err := a.SetClientRegistry(reg); err != nil {
				t.Fatalf("SetClientRegistry: unexpected error: %v", err)
			}
			if got := a.UpstreamClientNamesSnapshot(); len(got) != 0 {
				t.Errorf("UpstreamClientNames(): got %v, want empty (strategy parent must be dropped)", got)
			}
			origClients := a.OriginalClientRegistry().Clients
			if len(origClients) != 1 || origClients[0] != tc.client {
				t.Errorf("OriginalClientRegistry mutated: got %+v, want %+v", origClients, []*bamlutils.ClientProperty{tc.client})
			}
		})
	}
}

// runPresenceOnlyRRRoundTrip is the presence-only round-trip: original
// registry has the entry, BAML-bound registry drops it, and the cache
// for primary still reflects the materialised+translated provider so
// ResolveProvider's shortcut returns the correct classification value.
// (BuildRequest dispatches with WithClient(leaf) after resolution, so
// BAML doesn't need the parent there.)
func runPresenceOnlyRRRoundTrip(t *testing.T, factory Factory) {
	primary := "MyRR"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "MyRR"},
		},
	}
	a := factory(map[string]string{
		"MyRR": "baml-roundrobin",
		"A":    "openai",
		"B":    "openai",
	})
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	got := a.OriginalClientRegistry()
	if got != reg || len(got.Clients) != 1 || got.Clients[0].Name != "MyRR" {
		t.Errorf("OriginalClientRegistry not preserved; got %+v", got)
	}
	// Both views must drop the inert presence-only RR parent (no
	// provider, no options.strategy). A regression that left the parent
	// in the legacy registry would slip past a BuildRequest-only
	// assertion, so check both views explicitly.
	if names := a.UpstreamClientNamesSnapshot(); len(names) != 0 {
		t.Errorf("UpstreamClientNames(): got %v, want empty for presence-only RR", names)
	}
	if names := a.LegacyUpstreamClientNamesSnapshot(); len(names) != 0 {
		t.Errorf("legacyUpstreamClientNamesSnapshot after presence-only RR: got %v, want empty", names)
	}
	if got := a.ClientRegistryProvider(); got != "baml-round-robin" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-round-robin", got)
	}
}

// runNonStrategyEntriesForwarded is the inverse regression guard: non-
// strategy entries (concrete providers, leaf clients) must continue to
// reach BAML's registry — only the strategy parents get dropped.
func runNonStrategyEntriesForwarded(t *testing.T, factory Factory) {
	reg := &bamlutils.ClientRegistry{
		Clients: []*bamlutils.ClientProperty{
			{Name: "ClientA", Provider: "openai"},
			{Name: "ClientB", Provider: "anthropic"},
			{Name: "MyRR", Provider: "baml-roundrobin", Options: map[string]any{"strategy": []any{"ClientA", "ClientB"}}},
		},
	}
	a := factory(emptyMap())
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	got := a.UpstreamClientNamesSnapshot()
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
	// parent ("MyRR") because legacy dispatch reaches into it directly.
	// Asserting exact equality pins both invariants: leaves flow
	// through AND the strategy parent is preserved (not dropped by the
	// legacy path).
	legacyGot := a.LegacyUpstreamClientNamesSnapshot()
	legacyWant := []string{"ClientA", "ClientB", "MyRR"}
	if !reflect.DeepEqual(legacyGot, legacyWant) {
		t.Errorf("legacyUpstreamClientNamesSnapshot: got %v, want %v (legacy view forwards non-strategy entries and retains strategy parents)", legacyGot, legacyWant)
	}
}

// runTranslatesCanonicalRoundRobinSpelling pins canonical round-robin
// spelling translation at the adapter seam. The operator-supplied
// "baml-roundrobin" spelling — the canonical baml-rest form — must be
// translated to "baml-round-robin" before forwarding to BAML's upstream
// CFFI, which rejects "baml-roundrobin" in ClientProvider::from_str.
func runTranslatesCanonicalRoundRobinSpelling(t *testing.T, factory Factory) {
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
	a := factory(emptyMap())
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "baml-round-robin" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-round-robin (operator baml-roundrobin → upstream-translated)", got)
	}
	// Original is preserved for the resolver — operator typed
	// "baml-roundrobin" and the resolver classification works on that
	// exact spelling via roundrobin.IsRoundRobinProvider, which accepts
	// all three forms.
	if a.OriginalClientRegistry().Clients[0].Provider != "baml-roundrobin" {
		t.Errorf("operator's Provider was mutated; original must be preserved verbatim")
	}
}

// runPresentEmptyProviderStaysEmpty covers the boundary between two
// adjacent cases: present-empty routes to legacy with
// PathReasonInvalidProviderOverride, while omitted-provider gets
// materialised from the introspected map. Present-empty is not the
// same as omitted — the operator explicitly cleared the provider, so
// we must NOT materialise; let BAML's CFFI emit its canonical "Invalid
// client provider:" error.
func runPresentEmptyProviderStaysEmpty(t *testing.T, factory Factory) {
	primary := "MyClient"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{Name: "MyClient", Provider: "", ProviderSet: true},
		},
	}
	a := factory(map[string]string{"MyClient": "openai"})
	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}
	if got := a.ClientRegistryProvider(); got != "" {
		t.Errorf("ClientRegistryProvider(): got %q, want empty (present-empty must not be materialised)", got)
	}
	provider, _, ok := a.BuildRequestClientEntrySnapshot("MyClient")
	if !ok {
		t.Fatalf("BuildRequest registry should retain the present-empty entry; got missing")
	}
	if provider != "" {
		t.Errorf("BuildRequest registry provider for MyClient: got %q, want \"\" (present-empty must not be materialised at the registry seam either)", provider)
	}
	legacyProvider, _, ok := a.LegacyClientEntrySnapshot("MyClient")
	if !ok {
		t.Fatalf("legacy registry should retain the present-empty entry; got missing")
	}
	if legacyProvider != "" {
		t.Errorf("legacy registry provider for MyClient: got %q, want \"\" (present-empty must be preserved verbatim in the legacy view)", legacyProvider)
	}
}

// runExplicitProviderPassesThrough is the regression guard for the
// common-case: a fully-formed runtime entry with a real provider must
// pass through unchanged (modulo upstream-spelling translation, which
// is a no-op for the four concrete provider strings tested here).
func runExplicitProviderPassesThrough(t *testing.T, factory Factory) {
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
			a := factory(emptyMap())
			if err := a.SetClientRegistry(reg); err != nil {
				t.Fatalf("SetClientRegistry: unexpected error: %v", err)
			}
			if got := a.ClientRegistryProvider(); got != p {
				t.Errorf("ClientRegistryProvider(): got %q, want %q (concrete provider must pass through unchanged)", got, p)
			}
		})
	}
}
