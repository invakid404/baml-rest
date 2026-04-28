package bamlutils

import (
	"encoding/json"
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
		{
			name:   "nil introspected map keeps omitted provider empty",
			client: &ClientProperty{Name: "StaticOpenAI"},
			want:   "openai", // introspected is non-nil in this test; nil case below
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
