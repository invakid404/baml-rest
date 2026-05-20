package bamlutils

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

// TestDynamicOutputSchema_UnmarshalJSON_CapturesOrder pins #313: raw
// JSON callers of /call/_dynamic get the wire-order of
// properties/classes/enums and nested class properties captured into
// the OrderedMap insertion order, so the worker boundary can ship the
// order across to TypeBuilder population without a round-trip through
// Go map iteration.
func TestDynamicOutputSchema_UnmarshalJSON_CapturesOrder(t *testing.T) {
	t.Parallel()
	body := []byte(`{
      "properties": {
        "delta": {"type": "string"},
        "alpha": {"type": "int"},
        "charlie": {"ref": "Address"},
        "bravo": {"type": "string"}
      },
      "classes": {
        "Profile": {"properties": {"zulu": {"type": "string"}, "alpha": {"type": "int"}}},
        "Address": {"properties": {"zip": {"type": "string"}, "city": {"type": "string"}, "street": {"type": "string"}}}
      },
      "enums": {
        "Status": {"values": [{"name": "PENDING"}, {"name": "ACTIVE"}]},
        "Preference": {"values": [{"name": "LOW"}, {"name": "HIGH"}]}
      }
    }`)
	var s DynamicOutputSchema
	if err := sonic.Unmarshal(body, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, want := s.Properties.Keys(), []string{"delta", "alpha", "charlie", "bravo"}; !equalStrings(got, want) {
		t.Errorf("Properties order: got %v want %v", got, want)
	}
	if got, want := s.Classes.Keys(), []string{"Profile", "Address"}; !equalStrings(got, want) {
		t.Errorf("Classes order: got %v want %v", got, want)
	}
	if got, want := s.Enums.Keys(), []string{"Status", "Preference"}; !equalStrings(got, want) {
		t.Errorf("Enums order: got %v want %v", got, want)
	}
	profile, _ := s.Classes.Get("Profile")
	if got, want := profile.Properties.Keys(), []string{"zulu", "alpha"}; !equalStrings(got, want) {
		t.Errorf("Profile.Properties order: got %v want %v", got, want)
	}
	address, _ := s.Classes.Get("Address")
	if got, want := address.Properties.Keys(), []string{"zip", "city", "street"}; !equalStrings(got, want) {
		t.Errorf("Address.Properties order: got %v want %v", got, want)
	}
}

// TestUnmarshalOrderedMap_NestedRawMessage pins the goccy/go-json
// streaming-decoder contract that the order-capture path depends on:
// Token() must surface object delimiters and string keys, and a
// subsequent Decode(&json.RawMessage) on the value position must hand
// back the raw bytes of nested objects/arrays/primitives intact so the
// downstream goccy Unmarshal can decode them.
func TestUnmarshalOrderedMap_NestedRawMessage(t *testing.T) {
	t.Parallel()
	body := []byte(`{
      "properties": {
        "obj": {"type": "list", "items": {"type": "string"}},
        "lit": {"type": "literal_int", "value": 42},
        "ref": {"ref": "Other"},
        "primitive": {"type": "string"}
      },
      "classes": {
        "Other": {"description": "nested with array of values", "properties": {"a": {"type": "string"}}}
      }
    }`)
	var s DynamicOutputSchema
	if err := sonic.Unmarshal(body, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, want := s.Properties.Keys(), []string{"obj", "lit", "ref", "primitive"}; !equalStrings(got, want) {
		t.Errorf("Properties order: got %v want %v", got, want)
	}

	objProp, _ := s.Properties.Get("obj")
	if objProp == nil || objProp.Type != "list" || objProp.Items == nil || objProp.Items.Type != "string" {
		t.Errorf("nested list property lost its items spec: %+v", objProp)
	}

	litProp, _ := s.Properties.Get("lit")
	if litProp == nil || litProp.Type != "literal_int" || litProp.Value == nil {
		t.Errorf("literal_int property missing value: %+v", litProp)
	}

	refProp, _ := s.Properties.Get("ref")
	if refProp == nil || refProp.Ref != "Other" {
		t.Errorf("ref property dropped: %+v", refProp)
	}

	other, _ := s.Classes.Get("Other")
	if other == nil || other.Description != "nested with array of values" {
		t.Errorf("nested class lost description: %+v", other)
	}
	if got, want := other.Properties.Keys(), []string{"a"}; !equalStrings(got, want) {
		t.Errorf("nested class Properties order: got %v want %v", got, want)
	}
}

func TestDynamicOutputSchema_UnmarshalJSON_RejectsDuplicateKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "duplicate property",
			body: `{"properties":{"a":{"type":"string"},"a":{"type":"int"}}}`,
			want: `output_schema.properties: duplicate key "a"`,
		},
		{
			name: "duplicate class",
			body: `{"properties":{"x":{"type":"string"}},"classes":{"C":{"properties":{}},"C":{"properties":{}}}}`,
			want: `output_schema.classes: duplicate key "C"`,
		},
		{
			name: "duplicate enum",
			body: `{"properties":{"x":{"type":"string"}},"enums":{"E":{"values":[{"name":"A"}]},"E":{"values":[{"name":"B"}]}}}`,
			want: `output_schema.enums: duplicate key "E"`,
		},
		{
			name: "duplicate nested class property",
			body: `{"properties":{"x":{"type":"string"}},"classes":{"C":{"properties":{"a":{"type":"string"},"a":{"type":"int"}}}}}`,
			want: `class.properties: duplicate key "a"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s DynamicOutputSchema
			err := sonic.Unmarshal([]byte(tc.body), &s)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestDynamicOutputSchema_UnmarshalJSON_RejectsDuplicateTopLevelKey
// pins the strict-duplicate rule at the OUTER level of the schema
// payload. The struct-tag decode on the outer object would silently
// accept last-wins, so the pre-check helper restores symmetry.
func TestDynamicOutputSchema_UnmarshalJSON_RejectsDuplicateTopLevelKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "duplicate outer properties",
			body: `{"properties":{"a":{"type":"string"}},"properties":{"b":{"type":"int"}}}`,
			want: `output_schema: duplicate key "properties"`,
		},
		{
			name: "duplicate outer classes",
			body: `{"properties":{"x":{"type":"string"}},"classes":{"A":{"properties":{}}},"classes":{"B":{"properties":{}}}}`,
			want: `output_schema: duplicate key "classes"`,
		},
		{
			name: "duplicate outer enums",
			body: `{"properties":{"x":{"type":"string"}},"enums":{"E1":{"values":[{"name":"A"}]}},"enums":{"E2":{"values":[{"name":"B"}]}}}`,
			want: `output_schema: duplicate key "enums"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s DynamicOutputSchema
			err := sonic.Unmarshal([]byte(tc.body), &s)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestDynamicClass_UnmarshalJSON_RejectsDuplicateTopLevelKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "duplicate properties",
			body: `{"properties":{"a":{"type":"string"}},"properties":{"b":{"type":"int"}}}`,
			want: `class: duplicate key "properties"`,
		},
		{
			name: "duplicate alias",
			body: `{"alias":"first","alias":"second","properties":{"a":{"type":"string"}}}`,
			want: `class: duplicate key "alias"`,
		},
		{
			name: "duplicate description",
			body: `{"description":"first","description":"second","properties":{"a":{"type":"string"}}}`,
			want: `class: duplicate key "description"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c DynamicClass
			err := sonic.Unmarshal([]byte(tc.body), &c)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}

	body := []byte(`{"description":"only","alias":"once","properties":{"p":{"type":"string"}}}`)
	var c DynamicClass
	if err := sonic.Unmarshal(body, &c); err != nil {
		t.Errorf("well-formed class rejected: %v", err)
	}
	if c.Description != "only" || c.Alias != "once" || c.Properties.Len() != 1 {
		t.Errorf("well-formed class decoded incorrectly: %+v", c)
	}
}

func TestDynamicOutputSchema_UnmarshalJSON_RejectsNonObject(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "properties as array", body: `{"properties":[]}`, want: `output_schema.properties: must be a JSON object`},
		{name: "properties as string", body: `{"properties":"x"}`, want: `output_schema.properties: must be a JSON object`},
		{name: "properties as number", body: `{"properties":123}`, want: `output_schema.properties: must be a JSON object`},
		{name: "properties as bool", body: `{"properties":true}`, want: `output_schema.properties: must be a JSON object`},
		{name: "classes as array", body: `{"properties":{"x":{"type":"string"}},"classes":[]}`, want: `output_schema.classes: must be a JSON object`},
		{name: "classes as string", body: `{"properties":{"x":{"type":"string"}},"classes":"x"}`, want: `output_schema.classes: must be a JSON object`},
		{name: "classes as number", body: `{"properties":{"x":{"type":"string"}},"classes":42}`, want: `output_schema.classes: must be a JSON object`},
		{name: "classes as bool", body: `{"properties":{"x":{"type":"string"}},"classes":true}`, want: `output_schema.classes: must be a JSON object`},
		{name: "enums as array", body: `{"properties":{"x":{"type":"string"}},"enums":[]}`, want: `output_schema.enums: must be a JSON object`},
		{name: "enums as string", body: `{"properties":{"x":{"type":"string"}},"enums":"x"}`, want: `output_schema.enums: must be a JSON object`},
		{name: "enums as number", body: `{"properties":{"x":{"type":"string"}},"enums":7}`, want: `output_schema.enums: must be a JSON object`},
		{name: "enums as bool", body: `{"properties":{"x":{"type":"string"}},"enums":false}`, want: `output_schema.enums: must be a JSON object`},
		{name: "class properties as array", body: `{"properties":{"x":{"type":"string"}},"classes":{"C":{"properties":[]}}}`, want: `class.properties: must be a JSON object`},
		{name: "class properties as string", body: `{"properties":{"x":{"type":"string"}},"classes":{"C":{"properties":"x"}}}`, want: `class.properties: must be a JSON object`},
		{name: "class properties as number", body: `{"properties":{"x":{"type":"string"}},"classes":{"C":{"properties":9}}}`, want: `class.properties: must be a JSON object`},
		{name: "class properties as bool", body: `{"properties":{"x":{"type":"string"}},"classes":{"C":{"properties":false}}}`, want: `class.properties: must be a JSON object`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s DynamicOutputSchema
			err := sonic.Unmarshal([]byte(tc.body), &s)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestDynamicOutputSchema_UnmarshalJSON_AcceptsNull pins the
// nil-map convention: null on any schema-object field is accepted
// and leaves the corresponding OrderedMap at its zero value, so
// callers can opt out of classes or enums via `null`.
func TestDynamicOutputSchema_UnmarshalJSON_AcceptsNull(t *testing.T) {
	t.Parallel()
	body := []byte(`{
      "properties": null,
      "classes": null,
      "enums": null
    }`)
	var s DynamicOutputSchema
	if err := sonic.Unmarshal(body, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !s.Properties.IsZero() {
		t.Errorf("Properties: got %v want zero", s.Properties.Keys())
	}
	if !s.Classes.IsZero() {
		t.Errorf("Classes: got %v want zero", s.Classes.Keys())
	}
	if !s.Enums.IsZero() {
		t.Errorf("Enums: got %v want zero", s.Enums.Keys())
	}

	nested := []byte(`{
      "properties": {"x": {"type": "string"}},
      "classes": {"C": {"properties": null}}
    }`)
	var s2 DynamicOutputSchema
	if err := sonic.Unmarshal(nested, &s2); err != nil {
		t.Fatalf("Unmarshal nested: %v", err)
	}
	cls, _ := s2.Classes.Get("C")
	if !cls.Properties.IsZero() {
		t.Errorf("class C properties: got %v want zero", cls.Properties.Keys())
	}
}

// TestDynamicOutputSchema_UnmarshalJSON_ResetsOnReuse pins that
// decoding into an already-populated receiver clears stale maps.
func TestDynamicOutputSchema_UnmarshalJSON_ResetsOnReuse(t *testing.T) {
	t.Parallel()
	first := []byte(`{
      "properties": {"a": {"type": "string"}, "b": {"type": "int"}},
      "classes": {"C": {"properties": {"p": {"type": "string"}, "q": {"type": "int"}}}},
      "enums": {"E": {"values": [{"name": "A"}]}}
    }`)
	var s DynamicOutputSchema
	if err := sonic.Unmarshal(first, &s); err != nil {
		t.Fatalf("first Unmarshal: %v", err)
	}
	if s.Properties.Len() == 0 || s.Classes.Len() == 0 || s.Enums.Len() == 0 {
		t.Fatalf("first decode left maps empty: %+v", s)
	}

	if err := sonic.Unmarshal([]byte(`{}`), &s); err != nil {
		t.Fatalf("reuse Unmarshal {}: %v", err)
	}
	if !s.Properties.IsZero() || !s.Classes.IsZero() || !s.Enums.IsZero() {
		t.Errorf("maps not cleared after empty reuse: %+v", s)
	}

	if err := sonic.Unmarshal(first, &s); err != nil {
		t.Fatalf("re-populate Unmarshal: %v", err)
	}
	if err := sonic.Unmarshal([]byte(`{"properties":null,"classes":null,"enums":null}`), &s); err != nil {
		t.Fatalf("reuse Unmarshal null: %v", err)
	}
	if !s.Properties.IsZero() || !s.Classes.IsZero() || !s.Enums.IsZero() {
		t.Errorf("maps not cleared after null reuse: %+v", s)
	}
}

func TestDynamicClass_UnmarshalJSON_ResetsOnReuse(t *testing.T) {
	t.Parallel()
	first := []byte(`{"description": "old", "alias": "old-alias", "properties": {"p": {"type": "string"}, "q": {"type": "int"}}}`)
	var c DynamicClass
	if err := sonic.Unmarshal(first, &c); err != nil {
		t.Fatalf("first Unmarshal: %v", err)
	}
	if c.Description == "" || c.Alias == "" || c.Properties.Len() == 0 {
		t.Fatalf("first decode left fields empty: %+v", c)
	}
	if err := sonic.Unmarshal([]byte(`{}`), &c); err != nil {
		t.Fatalf("reuse Unmarshal: %v", err)
	}
	if c.Description != "" || c.Alias != "" || !c.Properties.IsZero() {
		t.Errorf("fields not cleared after empty reuse: %+v", c)
	}
}

// TestDynamicInput_Validate_RejectsReservedClassName pins that a
// user class named Baml_Rest_DynamicOutput is rejected at validation
// rather than silently overwritten by buildWorkerClassMap.
func TestDynamicInput_Validate_RejectsReservedClassName(t *testing.T) {
	t.Parallel()
	prompt := "hi"
	primary := "TestClient"
	provider := "anthropic"
	in := &DynamicInput{
		Messages: []DynamicMessage{{Role: "user", TextContent: &prompt}},
		ClientRegistry: &ClientRegistry{
			Primary: &primary,
			Clients: []*ClientProperty{{Name: primary, Provider: provider, Options: map[string]any{"model": "m", "api_key": "k"}}},
		},
		OutputSchema: &DynamicOutputSchema{
			Properties: MustOrderedMap(OrderedKV("x", &DynamicProperty{Type: "string"})),
			Classes: MustOrderedMap(
				OrderedKV("Baml_Rest_DynamicOutput", &DynamicClass{
					Properties: MustOrderedMap(OrderedKV("sneaky", &DynamicProperty{Type: "string"})),
				}),
			),
		},
	}
	err := in.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error %q should mention 'reserved'", err.Error())
	}
	if !strings.Contains(err.Error(), "Baml_Rest_DynamicOutput") {
		t.Errorf("error %q should name the reserved class", err.Error())
	}
}

// TestDynamicParseInput_Validate_RejectsReservedClassName mirrors
// the reserved-name validation on the parse-input path.
func TestDynamicParseInput_Validate_RejectsReservedClassName(t *testing.T) {
	t.Parallel()
	in := &DynamicParseInput{
		Raw: `{"x":"y"}`,
		OutputSchema: &DynamicOutputSchema{
			Properties: MustOrderedMap(OrderedKV("x", &DynamicProperty{Type: "string"})),
			Classes: MustOrderedMap(
				OrderedKV("Baml_Rest_DynamicOutput", &DynamicClass{
					Properties: MustOrderedMap(OrderedKV("sneaky", &DynamicProperty{Type: "string"})),
				}),
			),
		},
	}
	err := in.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error %q should mention 'reserved'", err.Error())
	}
	if !strings.Contains(err.Error(), "Baml_Rest_DynamicOutput") {
		t.Errorf("error %q should name the reserved class", err.Error())
	}
}

// TestDynamicInput_Validate_PreserveSchemaOrderTriState pins the *bool
// contract. With OrderedMap end-to-end, preserve-order no longer
// requires order-metadata validation, so the tri-state only governs
// whether the worker payload sets dynamic_types.preserve_order=true.
// nil/JSON-null inherit the server default; *true and *false win
// outright.
func TestDynamicInput_Validate_PreserveSchemaOrderTriState(t *testing.T) {
	t.Parallel()
	prompt := "hi"
	primary := "TestClient"
	provider := "anthropic"

	mkInput := func(preserve *bool) *DynamicInput {
		return &DynamicInput{
			Messages: []DynamicMessage{{Role: "user", TextContent: &prompt}},
			ClientRegistry: &ClientRegistry{
				Primary: &primary,
				Clients: []*ClientProperty{{Name: primary, Provider: provider, Options: map[string]any{"model": "m", "api_key": "k"}}},
			},
			OutputSchema: &DynamicOutputSchema{
				Properties: MustOrderedMap(
					OrderedKV("alpha", &DynamicProperty{Type: "string"}),
					OrderedKV("bravo", &DynamicProperty{Type: "int"}),
				),
			},
			PreserveSchemaOrder: preserve,
		}
	}

	for _, p := range []*bool{nil, boolPtr(false), boolPtr(true)} {
		if err := mkInput(p).Validate(); err != nil {
			t.Errorf("Validate(%v): %v", p, err)
		}
	}

	t.Run("JSON null decodes to nil and inherits default", func(t *testing.T) {
		body := []byte(`{
          "preserve_schema_order": null,
          "messages": [{"role": "user", "content": "hi"}],
          "client_registry": {"primary": "TestClient", "clients": [{"name": "TestClient", "provider": "anthropic", "options": {"model": "m", "api_key": "k"}}]},
          "output_schema": {"properties": {"alpha": {"type": "string"}, "bravo": {"type": "int"}}}
        }`)
		var in DynamicInput
		if err := sonic.Unmarshal(body, &in); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if in.PreserveSchemaOrder != nil {
			t.Errorf("JSON null must decode *bool to nil; got %v", *in.PreserveSchemaOrder)
		}
		if err := in.Validate(); err != nil {
			t.Errorf("null/absent must inherit absence: %v", err)
		}
	})
}

// TestDynamicParseInput_Validate_PreserveSchemaOrderTriState mirrors the
// tri-state contract on the parse-input path.
func TestDynamicParseInput_Validate_PreserveSchemaOrderTriState(t *testing.T) {
	t.Parallel()

	mkInput := func(preserve *bool) *DynamicParseInput {
		return &DynamicParseInput{
			Raw: `{"alpha":"x"}`,
			OutputSchema: &DynamicOutputSchema{
				Properties: MustOrderedMap(
					OrderedKV("alpha", &DynamicProperty{Type: "string"}),
					OrderedKV("bravo", &DynamicProperty{Type: "int"}),
				),
			},
			PreserveSchemaOrder: preserve,
		}
	}

	for _, p := range []*bool{nil, boolPtr(false), boolPtr(true)} {
		if err := mkInput(p).Validate(); err != nil {
			t.Errorf("Validate(%v): %v", p, err)
		}
	}

	t.Run("JSON null decodes to nil and inherits default", func(t *testing.T) {
		body := []byte(`{
          "preserve_schema_order": null,
          "raw": "{\"alpha\":\"x\"}",
          "output_schema": {"properties": {"alpha": {"type": "string"}, "bravo": {"type": "int"}}}
        }`)
		var in DynamicParseInput
		if err := sonic.Unmarshal(body, &in); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if in.PreserveSchemaOrder != nil {
			t.Errorf("JSON null must decode *bool to nil; got %v", *in.PreserveSchemaOrder)
		}
		if err := in.Validate(); err != nil {
			t.Errorf("null/absent must inherit absence: %v", err)
		}
	})
}

// TestDynamicInput_ToWorkerInput_PropagatesOrder pins that
// preserve_schema_order=true on the public input lands on the worker
// boundary as dynamic_types.preserve_order=true, with the OrderedMap
// classes/enums carrying the wire order intrinsically — no side-channel
// "order" key required.
func TestDynamicInput_ToWorkerInput_PropagatesOrder(t *testing.T) {
	t.Parallel()
	body := []byte(`{
      "preserve_schema_order": true,
      "messages": [{"role": "user", "content": "go"}],
      "client_registry": {"primary": "X", "clients": [{"name": "X", "provider": "anthropic", "retry_policy": null, "options": {"model": "m", "api_key": "k"}}]},
      "output_schema": {
        "properties": {
          "delta": {"type": "string"},
          "alpha": {"type": "int"},
          "charlie": {"ref": "Address"}
        },
        "classes": {
          "Profile": {"properties": {"zulu": {"type": "string"}, "alpha": {"type": "int"}}},
          "Address": {"properties": {"zip": {"type": "string"}, "city": {"type": "string"}}}
        },
        "enums": {
          "Status": {"values": [{"name": "PENDING"}, {"name": "ACTIVE"}]},
          "Preference": {"values": [{"name": "LOW"}, {"name": "HIGH"}]}
        }
      }
    }`)
	var in DynamicInput
	if err := sonic.Unmarshal(body, &in); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	data, err := in.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}

	// Parse the worker payload preserving key order so we can verify the
	// classes/enums objects survived as ordered objects on the wire.
	type dyn struct {
		PreserveOrder bool                         `json:"preserve_order"`
		Classes       OrderedMap[*DynamicClass]    `json:"classes"`
		Enums         OrderedMap[*DynamicEnum]     `json:"enums"`
	}
	type tb struct {
		DynamicTypes dyn `json:"dynamic_types"`
	}
	type opts struct {
		TypeBuilder tb `json:"type_builder"`
	}
	type payload struct {
		Opts opts `json:"__baml_options__"`
	}
	var decoded payload
	if err := sonic.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal worker payload: %v\n%s", err, data)
	}
	if !decoded.Opts.TypeBuilder.DynamicTypes.PreserveOrder {
		t.Errorf("preserve_order: got false want true")
	}
	gotClasses := decoded.Opts.TypeBuilder.DynamicTypes.Classes.Keys()
	wantClasses := []string{"Baml_Rest_DynamicOutput", "Profile", "Address"}
	if !equalStrings(gotClasses, wantClasses) {
		t.Errorf("classes order: got %v want %v", gotClasses, wantClasses)
	}
	gotEnums := decoded.Opts.TypeBuilder.DynamicTypes.Enums.Keys()
	wantEnums := []string{"Status", "Preference"}
	if !equalStrings(gotEnums, wantEnums) {
		t.Errorf("enums order: got %v want %v", gotEnums, wantEnums)
	}
	dynOut, _ := decoded.Opts.TypeBuilder.DynamicTypes.Classes.Get("Baml_Rest_DynamicOutput")
	if got, want := dynOut.Properties.Keys(), []string{"delta", "alpha", "charlie"}; !equalStrings(got, want) {
		t.Errorf("Baml_Rest_DynamicOutput.Properties order: got %v want %v", got, want)
	}
	profile, _ := decoded.Opts.TypeBuilder.DynamicTypes.Classes.Get("Profile")
	if got, want := profile.Properties.Keys(), []string{"zulu", "alpha"}; !equalStrings(got, want) {
		t.Errorf("Profile.Properties order: got %v want %v", got, want)
	}
	address, _ := decoded.Opts.TypeBuilder.DynamicTypes.Classes.Get("Address")
	if got, want := address.Properties.Keys(), []string{"zip", "city"}; !equalStrings(got, want) {
		t.Errorf("Address.Properties order: got %v want %v", got, want)
	}
}

// TestDynamicInput_ToWorkerInput_SyntheticClassFirst pins that
// the synthetic Baml_Rest_DynamicOutput class is always emitted at the
// front of the worker-bound Classes regardless of input order.
func TestDynamicInput_ToWorkerInput_SyntheticClassFirst(t *testing.T) {
	t.Parallel()
	primary := "TestClient"
	provider := "anthropic"
	greeting := "go"
	in := &DynamicInput{
		Messages: []DynamicMessage{{Role: "user", TextContent: &greeting}},
		ClientRegistry: &ClientRegistry{
			Primary: &primary,
			Clients: []*ClientProperty{{Name: primary, Provider: provider, Options: map[string]any{"model": "m", "api_key": "k"}}},
		},
		OutputSchema: &DynamicOutputSchema{
			Properties: MustOrderedMap(OrderedKV("only", &DynamicProperty{Ref: "Solo"})),
			Classes: MustOrderedMap(
				OrderedKV("Solo", &DynamicClass{
					Properties: MustOrderedMap(
						OrderedKV("zulu", &DynamicProperty{Type: "string"}),
						OrderedKV("alpha", &DynamicProperty{Type: "int"}),
						OrderedKV("mike", &DynamicProperty{Type: "bool"}),
					),
				}),
			),
		},
		PreserveSchemaOrder: boolPtr(true),
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	data, err := in.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}

	type dyn struct {
		Classes OrderedMap[*DynamicClass] `json:"classes"`
	}
	type tb struct {
		DynamicTypes dyn `json:"dynamic_types"`
	}
	type opts struct {
		TypeBuilder tb `json:"type_builder"`
	}
	type payload struct {
		Opts opts `json:"__baml_options__"`
	}
	var decoded payload
	if err := sonic.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal worker payload: %v\n%s", err, data)
	}
	gotClasses := decoded.Opts.TypeBuilder.DynamicTypes.Classes.Keys()
	wantClasses := []string{"Baml_Rest_DynamicOutput", "Solo"}
	if !equalStrings(gotClasses, wantClasses) {
		t.Errorf("classes order: got %v want %v", gotClasses, wantClasses)
	}
	solo, _ := decoded.Opts.TypeBuilder.DynamicTypes.Classes.Get("Solo")
	if got, want := solo.Properties.Keys(), []string{"zulu", "alpha", "mike"}; !equalStrings(got, want) {
		t.Errorf("Solo.Properties order: got %v want %v", got, want)
	}
}

// boolPtr returns a pointer to v. Used to construct the *bool tri-state
// PreserveSchemaOrder field in tests where the Go literal `true`/`false`
// would not be addressable inline.
func boolPtr(v bool) *bool { return &v }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDynamicInput_ToWorkerInput_CacheControlBridge pins the bridge from
// the public CacheControl{Type: ...} shape to the generated dynamic BAML
// input's `cache_control.cache_type` shape (issue #304).
func TestDynamicInput_ToWorkerInput_CacheControlBridge(t *testing.T) {
	t.Parallel()

	systemText := "you are a helpful assistant"
	userText := "tell me a joke"
	assistantText := "sure: why did the chicken cross the road?"
	followupText := "i don't know, why?"

	primary := "TestClient"
	provider := "anthropic"

	input := &DynamicInput{
		Messages: []DynamicMessage{
			{Role: "system", TextContent: &systemText},
			{
				Role: "user",
				PartsContent: []DynamicContentPart{
					{Type: "text", Text: &userText},
				},
				Metadata: &MessageMetadata{
					CacheControl: &CacheControl{Type: "ephemeral"},
				},
			},
			{Role: "assistant", TextContent: &assistantText},
			{Role: "user", TextContent: &followupText},
		},
		ClientRegistry: &ClientRegistry{
			Primary: &primary,
			Clients: []*ClientProperty{
				{
					Name:     primary,
					Provider: provider,
					Options: map[string]any{
						"model":                 "test-model",
						"api_key":               "test-key",
						"allowed_role_metadata": []any{"cache_control"},
					},
				},
			},
		},
		OutputSchema: &DynamicOutputSchema{
			Properties: MustOrderedMap(OrderedKV("answer", &DynamicProperty{Type: "string"})),
		},
	}

	if err := input.Validate(); err != nil {
		t.Fatalf("input.Validate: %v", err)
	}
	data, err := input.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}

	if bytes.Contains(data, []byte(`"cache_control":{"type":`)) {
		t.Errorf("worker payload still carries public cache_control.type shape:\n%s", data)
	}
	if bytes.Contains(data, []byte(`"cache_control":{}`)) {
		t.Errorf("worker payload carries empty cache_control object:\n%s", data)
	}
	if bytes.Contains(data, []byte(`"cache_type":""`)) {
		t.Errorf("worker payload carries empty cache_type string:\n%s", data)
	}

	var decoded struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := sonic.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal worker payload: %v\n%s", err, data)
	}
	if len(decoded.Messages) != 4 {
		t.Fatalf("expected 4 messages in worker payload, got %d\n%s", len(decoded.Messages), data)
	}

	if md, ok := decoded.Messages[0]["metadata"]; ok && md != nil {
		t.Errorf("system message must not carry metadata; got %v", md)
	}

	msg := decoded.Messages[1]
	mdRaw, ok := msg["metadata"]
	if !ok {
		t.Fatalf("tagged user message missing metadata key: %v", msg)
	}
	md, ok := mdRaw.(map[string]any)
	if !ok {
		t.Fatalf("tagged metadata is not an object: %T %v", mdRaw, mdRaw)
	}
	ccRaw, ok := md["cache_control"]
	if !ok {
		t.Fatalf("tagged metadata missing cache_control: %v", md)
	}
	cc, ok := ccRaw.(map[string]any)
	if !ok {
		t.Fatalf("cache_control is not an object: %T %v", ccRaw, ccRaw)
	}
	if got, want := cc["cache_type"], "ephemeral"; got != want {
		t.Errorf("cache_type: got %v, want %q", got, want)
	}
	if _, present := cc["type"]; present {
		t.Errorf("cache_control still carries public `type` key: %v", cc)
	}

	if md, ok := decoded.Messages[2]["metadata"]; ok && md != nil {
		t.Errorf("assistant message must not carry metadata; got %v", md)
	}
	if md, ok := decoded.Messages[3]["metadata"]; ok && md != nil {
		t.Errorf("follow-up user message must not carry metadata; got %v", md)
	}
}

func TestDynamicInput_ToWorkerInput_NoCacheControl_NoMetadata(t *testing.T) {
	t.Parallel()

	prompt := "hello"
	primary := "TestClient"
	provider := "anthropic"
	input := &DynamicInput{
		Messages: []DynamicMessage{
			{Role: "user", TextContent: &prompt},
		},
		ClientRegistry: &ClientRegistry{
			Primary: &primary,
			Clients: []*ClientProperty{
				{Name: primary, Provider: provider, Options: map[string]any{"model": "test-model", "api_key": "test-key"}},
			},
		},
		OutputSchema: &DynamicOutputSchema{
			Properties: MustOrderedMap(OrderedKV("answer", &DynamicProperty{Type: "string"})),
		},
	}

	data, err := input.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}
	if bytes.Contains(data, []byte(`"metadata"`)) {
		t.Errorf("worker payload should omit metadata when no message carries it; got:\n%s", data)
	}
}

func TestDynamicMessage_PublicJSON_StillUsesType(t *testing.T) {
	t.Parallel()
	body := []byte(`{"role":"user","content":"hi","metadata":{"cache_control":{"type":"ephemeral"}}}`)
	var msg DynamicMessage
	if err := sonic.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal public DynamicMessage: %v", err)
	}
	if msg.Metadata == nil || msg.Metadata.CacheControl == nil {
		t.Fatalf("metadata.CacheControl not populated from public JSON: %+v", msg)
	}
	if msg.Metadata.CacheControl.Type != "ephemeral" {
		t.Errorf("CacheControl.Type: got %q, want %q", msg.Metadata.CacheControl.Type, "ephemeral")
	}
	out, err := sonic.Marshal(&msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(out, []byte(`"cache_control":{"type":"ephemeral"}`)) {
		t.Errorf("public DynamicMessage marshal lost the type key; got:\n%s", out)
	}
}

func TestDynamicInputValidate_RejectsEmptyCacheControlType(t *testing.T) {
	t.Parallel()

	prompt := "hi"
	primary := "TestClient"
	provider := "anthropic"
	input := &DynamicInput{
		Messages: []DynamicMessage{
			{Role: "system", TextContent: &prompt},
			{
				Role:        "user",
				TextContent: &prompt,
				Metadata: &MessageMetadata{
					CacheControl: &CacheControl{Type: ""},
				},
			},
		},
		ClientRegistry: &ClientRegistry{
			Primary: &primary,
			Clients: []*ClientProperty{
				{Name: primary, Provider: provider, Options: map[string]any{"model": "test-model", "api_key": "test-key"}},
			},
		},
		OutputSchema: &DynamicOutputSchema{
			Properties: MustOrderedMap(OrderedKV("answer", &DynamicProperty{Type: "string"})),
		},
	}

	err := input.Validate()
	if err == nil {
		t.Fatal("expected Validate() to reject empty cache_control.type; got nil")
	}
	const want = "messages[1].metadata.cache_control.type is required"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("Validate() error %q does not contain %q", err.Error(), want)
	}

	input.Messages[1].Metadata.CacheControl.Type = "   "
	if err := input.Validate(); err == nil {
		t.Error("expected Validate() to reject whitespace-only cache_control.type; got nil")
	}

	input.Messages[1].Metadata.CacheControl = nil
	if err := input.Validate(); err != nil {
		t.Errorf("Validate() rejected nil CacheControl: %v", err)
	}

	input.Messages[1].Metadata = nil
	if err := input.Validate(); err != nil {
		t.Errorf("Validate() rejected nil Metadata: %v", err)
	}
}
