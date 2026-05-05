package bamlutils

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestClientProperty_ProviderPresenceFromJSON pins the JSON-decoded
// provider-presence semantics. The struct-tag-driven decoder
// collapses absent and
// present-empty into the same zero value; the custom UnmarshalJSON
// distinguishes them via ProviderSet so resolvers can route an
// explicit "provider":"" override to legacy where BAML emits its
// native invalid-provider error, rather than silently using the
// introspected fallback.
func TestClientProperty_ProviderPresenceFromJSON(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantPresent bool
		wantValue   string
	}{
		{
			name:        "provider key absent",
			raw:         `{"name":"MyClient"}`,
			wantPresent: false,
			wantValue:   "",
		},
		{
			name:        "provider key present non-empty",
			raw:         `{"name":"MyClient","provider":"openai"}`,
			wantPresent: true,
			wantValue:   "openai",
		},
		{
			name:        "provider key present empty",
			raw:         `{"name":"MyClient","provider":""}`,
			wantPresent: true,
			wantValue:   "",
		},
		{
			name:        "provider key present alongside options",
			raw:         `{"name":"MyClient","provider":"","options":{"strategy":["A"]}}`,
			wantPresent: true,
			wantValue:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c ClientProperty
			if err := json.Unmarshal([]byte(tc.raw), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if c.Provider != tc.wantValue {
				t.Errorf("Provider: got %q, want %q", c.Provider, tc.wantValue)
			}
			if got := c.IsProviderPresent(); got != tc.wantPresent {
				t.Errorf("IsProviderPresent(): got %v, want %v", got, tc.wantPresent)
			}
		})
	}
}

// TestClientProperty_ProviderPresenceFromStructLiteral guarantees that
// existing struct-literal test fixtures keep working without explicit
// ProviderSet wiring: a non-empty Provider value implies presence.
// Setting Provider:"" without ProviderSet means absent (the test-
// fixture default). To simulate present-empty in a struct literal,
// callers explicitly set ProviderSet:true.
func TestClientProperty_ProviderPresenceFromStructLiteral(t *testing.T) {
	cases := []struct {
		name        string
		client      ClientProperty
		wantPresent bool
	}{
		{
			name:        "non-empty Provider implies presence",
			client:      ClientProperty{Name: "MyClient", Provider: "openai"},
			wantPresent: true,
		},
		{
			name:        "empty Provider without ProviderSet is absent",
			client:      ClientProperty{Name: "MyClient"},
			wantPresent: false,
		},
		{
			name:        "explicit ProviderSet:true with empty Provider is present",
			client:      ClientProperty{Name: "MyClient", ProviderSet: true},
			wantPresent: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.client.IsProviderPresent(); got != tc.wantPresent {
				t.Errorf("IsProviderPresent(): got %v, want %v", got, tc.wantPresent)
			}
		})
	}
}

// TestClientProperty_NilSafe ensures the helper is nil-safe — resolver
// paths sometimes hold a *ClientProperty that may not yet have been
// resolved; treat nil as "no presence".
func TestClientProperty_NilSafe(t *testing.T) {
	var c *ClientProperty
	if c.IsProviderPresent() {
		t.Errorf("nil receiver: IsProviderPresent() = true, want false")
	}
}

// TestClientProperty_JSONRoundTrip pins MarshalJSON's preservation of
// the absent vs present-empty distinction across a full round-trip.
// Without a custom MarshalJSON, the default marshaler emits
// `"provider":""` for both states (Provider is a plain string), so an
// originally-absent value would re-decode as present-empty and route
// to legacy with PathReasonInvalidProviderOverride — silently
// changing routing behaviour for any caller that serialises and
// re-parses runtime registries (queue handoff, debug log, request
// re-issue).
func TestClientProperty_JSONRoundTrip(t *testing.T) {
	cases := []struct {
		name        string
		input       ClientProperty
		wantSubstr  string // a fragment that MUST appear in the marshalled output
		wantNoMatch string // a fragment that MUST NOT appear in the marshalled output
		wantPresent bool   // expected IsProviderPresent() after round-trip
		wantValue   string // expected Provider after round-trip
	}{
		{
			name:        "absent provider stays absent",
			input:       ClientProperty{Name: "MyClient"},
			wantNoMatch: `"provider"`,
			wantPresent: false,
			wantValue:   "",
		},
		{
			name:        "present-empty stays present-empty",
			input:       ClientProperty{Name: "MyClient", ProviderSet: true},
			wantSubstr:  `"provider":""`,
			wantPresent: true,
			wantValue:   "",
		},
		{
			name:        "present-nonempty round-trips verbatim",
			input:       ClientProperty{Name: "MyClient", Provider: "openai"},
			wantSubstr:  `"provider":"openai"`,
			wantPresent: true,
			wantValue:   "openai",
		},
		{
			name:        "present-nonempty with ProviderSet=true",
			input:       ClientProperty{Name: "MyClient", Provider: "openai", ProviderSet: true},
			wantSubstr:  `"provider":"openai"`,
			wantPresent: true,
			wantValue:   "openai",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marshalled, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			s := string(marshalled)
			if tc.wantSubstr != "" && !strings.Contains(s, tc.wantSubstr) {
				t.Errorf("marshalled %q must contain %q", s, tc.wantSubstr)
			}
			if tc.wantNoMatch != "" && strings.Contains(s, tc.wantNoMatch) {
				t.Errorf("marshalled %q must NOT contain %q (absent provider must drop the key entirely)", s, tc.wantNoMatch)
			}

			var decoded ClientProperty
			if err := json.Unmarshal(marshalled, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if decoded.Provider != tc.wantValue {
				t.Errorf("after round-trip Provider: got %q, want %q", decoded.Provider, tc.wantValue)
			}
			if got := decoded.IsProviderPresent(); got != tc.wantPresent {
				t.Errorf("after round-trip IsProviderPresent(): got %v, want %v", got, tc.wantPresent)
			}
		})
	}
}

// TestClientRegistry_Validate pins the runtime-registry invariants:
//
//   - Empty Names are operator typos that BAML's AddLlmClient would
//     index under "" while every baml-rest lookup keys on Name. The
//     entry becomes a nameless ghost the resolver / classifier can't
//     reach but BAML may still execute. OriginalClientRegistry is
//     captured at the top of every adapter's SetClientRegistry, so
//     even a forward-loop skip in the BAML-bound views would let
//     generated scoped-child registries re-introduce the entry from
//     the original — only the centralized validator closes both
//     vectors.
//   - Duplicate Names create a divergence between BAML's AddLlmClient
//     (map-backed, last-wins) and every baml-rest lookup (slice-
//     forward, first-match). The two halves classify against
//     different definitions and silently desync route classification,
//     support gating, retry policy, RR inspection, and X-BAML-Path-
//     Reason from what BAML actually executes.
//
// Empty-Name is checked BEFORE duplicate, so an entry violating both
// invariants surfaces as ErrEmptyClientName (the more fundamental
// shape).
func TestClientRegistry_Validate(t *testing.T) {
	cases := []struct {
		name string
		reg  *ClientRegistry
		// wantSentinel is the expected sentinel match. nil means the
		// fixture should validate cleanly (no error).
		wantSentinel error
		// wantSubstr is a substring expected in the wrapped error
		// message for diagnostics. Empty when wantSentinel is nil.
		wantSubstr string
	}{
		{
			name: "nil registry",
			reg:  nil,
		},
		{
			name: "empty clients",
			reg:  &ClientRegistry{},
		},
		{
			name: "single client",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{{Name: "MyClient", Provider: "openai"}},
			},
		},
		{
			name: "valid distinct clients",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					{Name: "ClientA", Provider: "openai"},
					{Name: "ClientB", Provider: "anthropic"},
					{Name: "ClientC", Provider: "google-ai"},
				},
			},
		},
		{
			name: "duplicate with different providers",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					{Name: "Tenant", Provider: "openai"},
					{Name: "Tenant", Provider: "anthropic"},
				},
			},
			wantSentinel: ErrDuplicateClientName,
			wantSubstr:   "Tenant",
		},
		{
			name: "duplicate with same provider — still reject",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					{Name: "Tenant", Provider: "openai"},
					{Name: "Tenant", Provider: "openai"},
				},
			},
			wantSentinel: ErrDuplicateClientName,
			wantSubstr:   "Tenant",
		},
		{
			name: "duplicate where second is present-empty (the load-bearing case)",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					{Name: "Tenant", Provider: "openai"},
					{Name: "Tenant", Provider: "", ProviderSet: true},
				},
			},
			wantSentinel: ErrDuplicateClientName,
			wantSubstr:   "Tenant",
		},
		{
			name: "valid then duplicate later in slice",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					{Name: "ClientA", Provider: "openai"},
					{Name: "ClientB", Provider: "anthropic"},
					{Name: "ClientA", Provider: "google-ai"},
				},
			},
			wantSentinel: ErrDuplicateClientName,
			wantSubstr:   "ClientA",
		},
		{
			name: "nil entries skipped (operator-tolerable absence, distinct from empty-Name typo)",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					nil,
					{Name: "ClientA", Provider: "openai"},
					nil,
				},
			},
		},
		{
			name: "empty-Name first entry rejects",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					{Name: "", Provider: "openai"},
				},
			},
			wantSentinel: ErrEmptyClientName,
			// Index 0 — the message must point at the offending slot.
			wantSubstr: "index 0",
		},
		{
			name: "empty-Name later in slice rejects (validation scans whole slice)",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					{Name: "GoodClient", Provider: "openai"},
					{Name: "", Provider: "anthropic"},
				},
			},
			wantSentinel: ErrEmptyClientName,
			wantSubstr:   "index 1",
		},
		{
			name: "two empty-Name entries rejects (still ErrEmptyClientName, not duplicate)",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					{Name: "", Provider: "openai"},
					{Name: "", Provider: "anthropic"},
				},
			},
			wantSentinel: ErrEmptyClientName,
			wantSubstr:   "index 0",
		},
		{
			name: "empty-Name fires before duplicate when both invariants violated",
			reg: &ClientRegistry{
				Clients: []*ClientProperty{
					{Name: "Dup", Provider: "openai"},
					{Name: "", Provider: "anthropic"},
					{Name: "Dup", Provider: "google-ai"},
				},
			},
			wantSentinel: ErrEmptyClientName,
			wantSubstr:   "index 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.reg.Validate()
			if tc.wantSentinel == nil {
				if err != nil {
					t.Errorf("Validate(): want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(): want error wrapping %v, got nil", tc.wantSentinel)
			}
			if !errors.Is(err, tc.wantSentinel) {
				t.Errorf("Validate(): want errors.Is(err, %v) true, got err=%v", tc.wantSentinel, err)
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("Validate() error %q must contain diagnostic substring %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestTranslateUpstreamProvider pins the upstream-registry seam
// translation. baml-rest's canonical "baml-roundrobin" spelling is
// rejected by BAML
// upstream's ClientProvider::from_str (clientspec.rs:119-144); the
// translation helper rewrites it to "baml-round-robin" only at the
// upstream registry seam. All other strings — including the operator-
// supplied alternates — pass through unchanged so we never silently
// mutate operator input.
func TestTranslateUpstreamProvider(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// The single translation case: our canonical spelling becomes
		// the BAML-upstream-accepted form before AddLlmClient.
		{"baml-roundrobin", "baml-round-robin"},
		// Both other RR spellings are upstream-accepted verbatim, so
		// we preserve operator input.
		{"baml-round-robin", "baml-round-robin"},
		{"round-robin", "round-robin"},
		// Both fallback spellings are upstream-accepted verbatim.
		{"baml-fallback", "baml-fallback"},
		{"fallback", "fallback"},
		// Concrete providers pass through.
		{"openai", "openai"},
		{"anthropic", "anthropic"},
		// Empty stays empty so present-empty entries can still surface
		// BAML's canonical "Invalid client provider:" error at CFFI.
		{"", ""},
		// Unknown strings pass through unchanged; BAML's parser rejects
		// them — that's the intended observable behaviour.
		{"not-a-real-provider", "not-a-real-provider"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := TranslateUpstreamProvider(tc.in); got != tc.want {
				t.Errorf("TranslateUpstreamProvider(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsResolvedStrategyParent pins the strategy-parent
// classification: baml-rest-resolved RR / fallback strategy parent
// entries must be classifiable from either the operator-supplied
// provider or the introspected fallback, across all four RR
// spellings and both fallback spellings. Non-strategy entries must
// never classify here, regardless of presence state.
func TestIsResolvedStrategyParent(t *testing.T) {
	introspected := map[string]string{
		"StaticRR":       "baml-roundrobin",
		"StaticRRDash":   "baml-round-robin",
		"StaticRRBare":   "round-robin",
		"StaticFb":       "baml-fallback",
		"StaticFbBare":   "fallback",
		"StaticOpenAI":   "openai",
	}
	cases := []struct {
		name   string
		client *ClientProperty
		want   bool
	}{
		// Presence-only — classified via introspected map.
		{"presence-only RR (canonical introspected)", &ClientProperty{Name: "StaticRR"}, true},
		{"presence-only RR (hyphenated introspected)", &ClientProperty{Name: "StaticRRDash"}, true},
		{"presence-only RR (bare introspected)", &ClientProperty{Name: "StaticRRBare"}, true},
		{"presence-only fallback (canonical)", &ClientProperty{Name: "StaticFb"}, true},
		{"presence-only fallback (bare)", &ClientProperty{Name: "StaticFbBare"}, true},
		{"presence-only openai (not strategy)", &ClientProperty{Name: "StaticOpenAI"}, false},
		// Explicit provider — classified directly, ignoring introspected.
		{"explicit baml-roundrobin", &ClientProperty{Name: "AnyName", Provider: "baml-roundrobin"}, true},
		{"explicit baml-round-robin", &ClientProperty{Name: "AnyName", Provider: "baml-round-robin"}, true},
		{"explicit round-robin", &ClientProperty{Name: "AnyName", Provider: "round-robin"}, true},
		{"explicit baml-fallback", &ClientProperty{Name: "AnyName", Provider: "baml-fallback"}, true},
		{"explicit fallback", &ClientProperty{Name: "AnyName", Provider: "fallback"}, true},
		{"explicit openai", &ClientProperty{Name: "AnyName", Provider: "openai"}, false},
		// Present-empty (operator typo'd) — classified as not-strategy
		// because the empty string isn't an RR/fallback spelling and
		// presence overrides the introspected lookup.
		{"present-empty on RR-introspected name (operator override wins)", &ClientProperty{Name: "StaticRR", ProviderSet: true}, false},
		// Unknown name with omitted provider — no introspected entry
		// → empty string → not strategy.
		{"unknown name with omitted provider", &ClientProperty{Name: "Unknown"}, false},
		// Nil safety.
		{"nil client", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsResolvedStrategyParent(tc.client, introspected); got != tc.want {
				t.Errorf("IsResolvedStrategyParent: got %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("nil introspected map with omitted provider", func(t *testing.T) {
		if IsResolvedStrategyParent(&ClientProperty{Name: "StaticRR"}, nil) {
			t.Errorf("nil introspected map: presence-only entry should classify as not-strategy")
		}
	})
}

// TestUpstreamClientRegistryProvider pins the four shapes the helper
// must honour at the BAML CFFI seam: materialise omitted-provider
// entries from the introspected
// map so strategy-only / presence-only RR overrides actually reach
// upstream as valid registry entries; preserve explicit present-empty
// entries so BAML emits its canonical invalid-provider error;
// translate the canonical "baml-roundrobin" spelling at the seam so
// CFFI's parser accepts it; leave unknown-name omitted-provider
// entries as "" so the request fails with BAML's own error rather than
// us synthesising one.
func TestUpstreamClientRegistryProvider(t *testing.T) {
	introspected := map[string]string{
		"StaticOpenAI": "openai",
		"StaticRR":     "baml-roundrobin",
		"StaticFb":     "baml-fallback",
	}
	cases := []struct {
		name   string
		client *ClientProperty
		want   string
	}{
		{
			name:   "nil client returns empty",
			client: nil,
			want:   "",
		},
		{
			name:   "explicit provider passes through",
			client: &ClientProperty{Name: "Anywhere", Provider: "anthropic"},
			want:   "anthropic",
		},
		{
			name:   "explicit baml-roundrobin gets translated",
			client: &ClientProperty{Name: "Anywhere", Provider: "baml-roundrobin"},
			want:   "baml-round-robin",
		},
		{
			name:   "explicit baml-round-robin passes through",
			client: &ClientProperty{Name: "Anywhere", Provider: "baml-round-robin"},
			want:   "baml-round-robin",
		},
		{
			name:   "explicit round-robin passes through",
			client: &ClientProperty{Name: "Anywhere", Provider: "round-robin"},
			want:   "round-robin",
		},
		{
			name:   "present-empty stays empty (CFFI emits canonical error)",
			client: &ClientProperty{Name: "StaticOpenAI", ProviderSet: true},
			want:   "",
		},
		{
			name:   "omitted provider materialised from introspected map",
			client: &ClientProperty{Name: "StaticOpenAI"},
			want:   "openai",
		},
		{
			name: "omitted provider on RR client materialises and translates",
			client: &ClientProperty{
				Name:    "StaticRR",
				Options: map[string]any{"strategy": []any{"A", "B"}},
			},
			want: "baml-round-robin",
		},
		{
			name:   "omitted provider on fallback client materialises (no translation)",
			client: &ClientProperty{Name: "StaticFb"},
			want:   "baml-fallback",
		},
		{
			name:   "unknown name with omitted provider stays empty",
			client: &ClientProperty{Name: "UnknownClient"},
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UpstreamClientRegistryProvider(tc.client, introspected)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// Nil-introspected case verified separately — the helper must not
	// dereference the map when it's nil.
	t.Run("nil introspected map with omitted provider returns empty", func(t *testing.T) {
		got := UpstreamClientRegistryProvider(&ClientProperty{Name: "StaticOpenAI"}, nil)
		if got != "" {
			t.Errorf("got %q, want empty when introspected map is nil", got)
		}
	})

	// Non-mutation invariant: calling the helper does not change the
	// input ClientProperty. The resolver and metadata classifier read
	// the original after the adapter sanitises the BAML-bound copy, so
	// any mutation here would silently change downstream behaviour.
	t.Run("does not mutate input client", func(t *testing.T) {
		client := &ClientProperty{Name: "StaticOpenAI"}
		_ = UpstreamClientRegistryProvider(client, introspected)
		if client.Provider != "" || client.ProviderSet {
			t.Errorf("input mutated: Provider=%q, ProviderSet=%v", client.Provider, client.ProviderSet)
		}
	})
}

func TestDynamicTypes_Validate(t *testing.T) {
	tests := []struct {
		name    string
		dt      *DynamicTypes
		wantErr string // empty means no error expected
	}{
		{
			name:    "nil DynamicTypes is valid",
			dt:      nil,
			wantErr: "",
		},
		{
			name:    "empty DynamicTypes is valid",
			dt:      &DynamicTypes{},
			wantErr: "",
		},
		{
			name: "empty class is valid",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"EmptyClass": {},
				},
			},
			wantErr: "",
		},
		{
			name: "empty enum is valid",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"EmptyEnum": {},
				},
			},
			wantErr: "",
		},
		{
			name: "nil class definition",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"NilClass": nil,
				},
			},
			wantErr: `class "NilClass": definition is nil`,
		},
		{
			name: "nil enum definition",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"NilEnum": nil,
				},
			},
			wantErr: `enum "NilEnum": definition is nil`,
		},
		{
			name: "nil property",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"nilProp": nil,
						},
					},
				},
			},
			wantErr: `class "TestClass": property "nilProp" is nil`,
		},
		{
			name: "nil enum value",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"TestEnum": {
						Values: []*DynamicEnumValue{nil},
					},
				},
			},
			wantErr: `enum "TestEnum": value at index 0 is nil`,
		},
		{
			name: "enum value with empty name",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"TestEnum": {
						Values: []*DynamicEnumValue{
							{Name: ""},
						},
					},
				},
			},
			wantErr: `enum "TestEnum": value at index 0 has empty name`,
		},
		{
			name: "property missing type and ref",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"badProp": {},
						},
					},
				},
			},
			wantErr: `must have 'type' or 'ref'`,
		},
		{
			name: "property with both type and ref",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"badProp": {
								Type: "string",
								Ref:  "OtherClass",
							},
						},
					},
				},
			},
			wantErr: `cannot have both 'type' and 'ref'`,
		},
		{
			name: "valid primitive types",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"strProp":   {Type: "string"},
							"intProp":   {Type: "int"},
							"floatProp": {Type: "float"},
							"boolProp":  {Type: "bool"},
							"nullProp":  {Type: "null"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "unknown type",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"badProp": {Type: "unknown_type"},
						},
					},
				},
			},
			wantErr: `unknown type "unknown_type"`,
		},
		{
			name: "list without items",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"listProp": {Type: "list"},
						},
					},
				},
			},
			wantErr: `'list' type requires 'items'`,
		},
		{
			name: "valid list",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"listProp": {
								Type:  "list",
								Items: &DynamicTypeSpec{Type: "string"},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "optional without inner",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"optProp": {Type: "optional"},
						},
					},
				},
			},
			wantErr: `'optional' type requires 'inner'`,
		},
		{
			name: "valid optional",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"optProp": {
								Type:  "optional",
								Inner: &DynamicTypeSpec{Type: "string"},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "map without keys",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"mapProp": {
								Type:   "map",
								Values: &DynamicTypeSpec{Type: "string"},
							},
						},
					},
				},
			},
			wantErr: `'map' type requires 'keys' and 'values'`,
		},
		{
			name: "map without values",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"mapProp": {
								Type: "map",
								Keys: &DynamicTypeSpec{Type: "string"},
							},
						},
					},
				},
			},
			wantErr: `'map' type requires 'keys' and 'values'`,
		},
		{
			name: "valid map",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"mapProp": {
								Type:   "map",
								Keys:   &DynamicTypeSpec{Type: "string"},
								Values: &DynamicTypeSpec{Type: "int"},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "union without oneOf",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"unionProp": {Type: "union"},
						},
					},
				},
			},
			wantErr: `'union' type requires 'oneOf' with at least one type`,
		},
		{
			name: "union with nil variant",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"unionProp": {
								Type:  "union",
								OneOf: []*DynamicTypeSpec{nil},
							},
						},
					},
				},
			},
			wantErr: `'oneOf[0]' is nil`,
		},
		{
			name: "valid union",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"unionProp": {
								Type: "union",
								OneOf: []*DynamicTypeSpec{
									{Type: "string"},
									{Type: "int"},
								},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "literal_string without value",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_string"},
						},
					},
				},
			},
			wantErr: `'literal_string' type requires 'value'`,
		},
		{
			name: "literal_string with wrong value type",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_string", Value: 123},
						},
					},
				},
			},
			wantErr: `'literal_string' value must be a string`,
		},
		{
			name: "valid literal_string",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_string", Value: "hello"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "literal_int without value",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_int"},
						},
					},
				},
			},
			wantErr: `'literal_int' type requires 'value'`,
		},
		{
			name: "literal_int with wrong value type",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_int", Value: "not an int"},
						},
					},
				},
			},
			wantErr: `'literal_int' value must be an integer`,
		},
		{
			name: "valid literal_int with int",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_int", Value: 42},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "valid literal_int with float64 (JSON unmarshaling)",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_int", Value: float64(42)},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "literal_bool without value",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_bool"},
						},
					},
				},
			},
			wantErr: `'literal_bool' type requires 'value'`,
		},
		{
			name: "literal_bool with wrong value type",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_bool", Value: "true"},
						},
					},
				},
			},
			wantErr: `'literal_bool' value must be a boolean`,
		},
		{
			name: "valid literal_bool",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"litProp": {Type: "literal_bool", Value: true},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "valid ref to class",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"refProp": {Ref: "OtherClass"},
						},
					},
					"OtherClass": {},
				},
			},
			wantErr: "",
		},
		{
			name: "valid ref to enum",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"refProp": {Ref: "Status"},
						},
					},
				},
				Enums: map[string]*DynamicEnum{
					"Status": {
						Values: []*DynamicEnumValue{
							{Name: "ACTIVE"},
							{Name: "INACTIVE"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "ref to undefined type is allowed (may exist in baml_src)",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"refProp": {Ref: "ExternalType"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "deeply nested valid types",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"deepProp": {
								Type: "optional",
								Inner: &DynamicTypeSpec{
									Type: "list",
									Items: &DynamicTypeSpec{
										Type: "map",
										Keys: &DynamicTypeSpec{Type: "string"},
										Values: &DynamicTypeSpec{
											Type: "union",
											OneOf: []*DynamicTypeSpec{
												{Type: "string"},
												{Type: "int"},
												{Ref: "OtherClass"},
											},
										},
									},
								},
							},
						},
					},
					"OtherClass": {},
				},
			},
			wantErr: "",
		},
		{
			name: "nested type with error",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TestClass": {
						Properties: map[string]*DynamicProperty{
							"deepProp": {
								Type: "list",
								Items: &DynamicTypeSpec{
									Type: "optional",
									// Missing Inner
								},
							},
						},
					},
				},
			},
			wantErr: `'optional' type requires 'inner'`,
		},
		{
			name: "enum with all features",
			dt: &DynamicTypes{
				Enums: map[string]*DynamicEnum{
					"Status": {
						Description: "Status enum",
						Alias:       "UserStatus",
						Values: []*DynamicEnumValue{
							{Name: "ACTIVE", Description: "Active user", Alias: "active"},
							{Name: "INACTIVE", Skip: true},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "class with all features",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"User": {
						Description: "User class",
						Alias:       "UserModel",
						Properties: map[string]*DynamicProperty{
							"name": {Type: "string", Description: "User name", Alias: "userName"},
							"age":  {Type: "int"},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "self-referential class",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"TreeNode": {
						Properties: map[string]*DynamicProperty{
							"value": {Type: "string"},
							"children": {
								Type: "list",
								Items: &DynamicTypeSpec{
									Ref: "TreeNode",
								},
							},
						},
					},
				},
			},
			wantErr: "",
		},
		{
			name: "mutually referential classes",
			dt: &DynamicTypes{
				Classes: map[string]*DynamicClass{
					"ClassA": {
						Properties: map[string]*DynamicProperty{
							"b": {Ref: "ClassB"},
						},
					},
					"ClassB": {
						Properties: map[string]*DynamicProperty{
							"a": {Ref: "ClassA"},
						},
					},
				},
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.dt.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("Validate() expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Validate() error = %q, want error containing %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestDynamicTypes_Validate_MaxDepth(t *testing.T) {
	// Build a deeply nested type that exceeds maxTypeDepth
	var buildDeepType func(depth int) *DynamicTypeSpec
	buildDeepType = func(depth int) *DynamicTypeSpec {
		if depth <= 0 {
			return &DynamicTypeSpec{Type: "string"}
		}
		return &DynamicTypeSpec{
			Type:  "optional",
			Inner: buildDeepType(depth - 1),
		}
	}

	t.Run("type at max depth is valid", func(t *testing.T) {
		dt := &DynamicTypes{
			Classes: map[string]*DynamicClass{
				"TestClass": {
					Properties: map[string]*DynamicProperty{
						"deepProp": {
							Type:  "optional",
							Inner: buildDeepType(maxTypeDepth - 2), // -2 because: property itself is depth 0, outer optional is depth 1
						},
					},
				},
			},
		}
		if err := dt.Validate(); err != nil {
			t.Errorf("Validate() at max depth should not error, got: %v", err)
		}
	})

	t.Run("type exceeding max depth returns error", func(t *testing.T) {
		dt := &DynamicTypes{
			Classes: map[string]*DynamicClass{
				"TestClass": {
					Properties: map[string]*DynamicProperty{
						"tooDeepProp": {
							Type:  "optional",
							Inner: buildDeepType(maxTypeDepth + 10),
						},
					},
				},
			},
		}
		err := dt.Validate()
		if err == nil {
			t.Error("Validate() expected error for exceeding max depth, got nil")
		} else if !strings.Contains(err.Error(), "exceeds maximum depth") {
			t.Errorf("Validate() error = %q, want error about exceeding max depth", err.Error())
		}
	})
}

// TestShouldDropStrategyParentForTopLevelLegacy pins the inert-parent
// drop predicate. The helper reports true (drop) only for inert
// presence-only strategy parents — entries that classify as strategy
// parents (via introspected provider) but carry no explicit provider,
// no `options.strategy`, and no `options.start`. Forwarding such
// entries into BAML's upstream registry retriggers BAML's
// missing-strategy CFFI rejection (round_robin.rs:73-83 /
// helpers.rs:790-829).
//
// Every other shape — explicit provider override, runtime strategy,
// runtime start, present-empty (operator-typo'd), unknown name,
// non-strategy entries — must be kept. An explicit declaration is the
// operator's signal to drive the parent at runtime, even if BAML's
// parser would later reject it; baml-rest's contract is to forward
// the operator's intent verbatim and let BAML emit its canonical
// validation error.
func TestShouldDropStrategyParentForTopLevelLegacy(t *testing.T) {
	introspected := map[string]string{
		"StaticRR":     "baml-roundrobin",
		"StaticFb":     "baml-fallback",
		"StaticOpenAI": "openai",
	}
	cases := []struct {
		name   string
		client *ClientProperty
		want   bool
	}{
		// Inert presence-only strategy parents — drop.
		{"inert presence-only RR (introspected)", &ClientProperty{Name: "StaticRR"}, true},
		{"inert presence-only fallback (introspected)", &ClientProperty{Name: "StaticFb"}, true},

		// Presence-only RR with runtime override → keep. The
		// operator's runtime declaration is the signal to forward.
		{"presence-only RR with options.strategy", &ClientProperty{
			Name:    "StaticRR",
			Options: map[string]any{"strategy": []any{"A", "B"}},
		}, false},
		{"presence-only RR with options.start", &ClientProperty{
			Name:    "StaticRR",
			Options: map[string]any{"start": 1},
		}, false},
		{"presence-only RR with both strategy and start", &ClientProperty{
			Name:    "StaticRR",
			Options: map[string]any{"strategy": []any{"A", "B"}, "start": 1},
		}, false},

		// Explicit provider RR aliases — keep. Each alias is the
		// operator's explicit declaration.
		{"explicit baml-roundrobin", &ClientProperty{Name: "MyRR", Provider: "baml-roundrobin", ProviderSet: true}, false},
		{"explicit baml-round-robin", &ClientProperty{Name: "MyRR", Provider: "baml-round-robin", ProviderSet: true}, false},
		{"explicit round-robin", &ClientProperty{Name: "MyRR", Provider: "round-robin", ProviderSet: true}, false},

		// Explicit provider fallback aliases — keep.
		{"explicit baml-fallback", &ClientProperty{Name: "MyFb", Provider: "baml-fallback", ProviderSet: true}, false},
		{"explicit fallback", &ClientProperty{Name: "MyFb", Provider: "fallback", ProviderSet: true}, false},

		// Present-empty (operator typo'd) on a name introspected as
		// strategy parent — keep. Present-empty is treated as
		// not-strategy by IsResolvedStrategyParent (the empty string
		// isn't an RR/fallback spelling), so the predicate returns
		// false at the first guard. BAML emits its canonical
		// invalid-provider error against the empty string.
		{"present-empty on RR-introspected name", &ClientProperty{
			Name:        "StaticRR",
			Provider:    "",
			ProviderSet: true,
		}, false},

		// Unknown name with omitted provider — keep (not a strategy
		// parent in the introspected map).
		{"unknown name with omitted provider", &ClientProperty{Name: "Unknown"}, false},

		// Non-strategy entry (introspected as openai) — keep.
		{"presence-only openai", &ClientProperty{Name: "StaticOpenAI"}, false},

		// Explicit non-strategy provider — keep.
		{"explicit openai", &ClientProperty{Name: "Anything", Provider: "openai", ProviderSet: true}, false},

		// Nil client — predicate must not panic; safe-default = keep.
		{"nil client", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldDropStrategyParentForTopLevelLegacy(tc.client, introspected); got != tc.want {
				t.Errorf("ShouldDropStrategyParentForTopLevelLegacy: got %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("nil introspected map with presence-only RR-named entry", func(t *testing.T) {
		// Without an introspected map, the entry doesn't classify
		// as a strategy parent (IsResolvedStrategyParent returns
		// false for omitted-provider with no introspected entry),
		// so the predicate's first guard returns false (keep). The
		// upstream CFFI seam will see an entry with empty provider
		// and emit its own error if applicable.
		if ShouldDropStrategyParentForTopLevelLegacy(&ClientProperty{Name: "StaticRR"}, nil) {
			t.Errorf("nil introspected map: presence-only entry should NOT be dropped (not classified as strategy parent without introspected entry)")
		}
	})
}
