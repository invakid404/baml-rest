package adapter

import (
	"context"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// TestSetClientRegistry_MaterialisesOmittedProvider covers PR #192
// cold-review-3 finding 1 at the adapter seam. A runtime registry
// entry that omits the `provider` key (strategy-only or presence-only
// override) must have its provider materialised from the introspected
// map BEFORE AddLlmClient forwards it to BAML's CFFI; otherwise the
// upstream ClientProvider::from_str rejects "" and the request fails
// before WithClient(leaf) resolves anything.
//
// The test asserts the cache materialisation on the primary side
// (clientRegistryProvider) — this is the only field the adapter
// surfaces externally, and it's wired to the same materialise helper.
// The internal baml.ClientRegistry's per-entry providers are not
// observable from outside the baml package; we cover those via the
// helper-level tests in bamlutils/interfaces_test.go.
//
// Original-registry preservation: we also verify
// OriginalClientRegistry returns the operator's exact input so the
// resolver and metadata classifier still see verbatim presence.
func TestSetClientRegistry_MaterialisesOmittedProvider(t *testing.T) {
	primary := "MyRR"
	reg := &bamlutils.ClientRegistry{
		Primary: &primary,
		Clients: []*bamlutils.ClientProperty{
			{
				Name:    "MyRR",
				Options: map[string]any{"strategy": []any{"A", "B"}},
				// Provider deliberately omitted — strategy-only override.
			},
		},
	}
	a := &BamlAdapter{
		Context: context.Background(),
		IntrospectedClientProvider: map[string]string{
			"MyRR": "baml-roundrobin",
		},
	}

	if err := a.SetClientRegistry(reg); err != nil {
		t.Fatalf("SetClientRegistry: unexpected error: %v", err)
	}

	// Cache materialisation: the primary's provider must be the
	// upstream-translated form of the introspected provider, not the
	// operator's empty string. Without this, ResolveProvider's
	// shortcut would return "" and the metadata classifier's
	// downstream presence check would have to redundantly re-resolve
	// against the original registry.
	if got := a.ClientRegistryProvider(); got != "baml-round-robin" {
		t.Errorf("ClientRegistryProvider(): got %q, want baml-round-robin (introspected baml-roundrobin → upstream-translated)", got)
	}

	// Original-registry preservation: the resolver and metadata
	// classifier read this view; if SetClientRegistry mutated it,
	// every test that checks IsProviderPresent or the operator's raw
	// strategy options would silently regress.
	got := a.OriginalClientRegistry()
	if got != reg {
		t.Errorf("OriginalClientRegistry(): expected pointer-equal preservation, got different *ClientRegistry")
	}
	if len(got.Clients) != 1 {
		t.Fatalf("OriginalClientRegistry().Clients: len = %d, want 1", len(got.Clients))
	}
	if got.Clients[0].Provider != "" {
		t.Errorf("operator's omitted Provider was mutated to %q", got.Clients[0].Provider)
	}
	if got.Clients[0].IsProviderPresent() {
		t.Errorf("operator's omitted ProviderSet was mutated to true")
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
func TestSetClientRegistry_NilRegistryClearsState(t *testing.T) {
	a := &BamlAdapter{Context: context.Background()}
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
}
