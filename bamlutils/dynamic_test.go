package bamlutils

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// TestDynamicOutputSchema_UnmarshalJSON_CapturesOrder pins #313: raw
// JSON callers of /call/_dynamic should get the wire-order of
// properties/classes/enums and nested class properties captured into
// the matching *Order slices, so the worker boundary can ship the
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
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, want := s.PropertiesOrder, []string{"delta", "alpha", "charlie", "bravo"}; !equalStrings(got, want) {
		t.Errorf("PropertiesOrder: got %v want %v", got, want)
	}
	if got, want := s.ClassesOrder, []string{"Profile", "Address"}; !equalStrings(got, want) {
		t.Errorf("ClassesOrder: got %v want %v", got, want)
	}
	if got, want := s.EnumsOrder, []string{"Status", "Preference"}; !equalStrings(got, want) {
		t.Errorf("EnumsOrder: got %v want %v", got, want)
	}
	if got, want := s.Classes["Profile"].PropertiesOrder, []string{"zulu", "alpha"}; !equalStrings(got, want) {
		t.Errorf("Profile.PropertiesOrder: got %v want %v", got, want)
	}
	if got, want := s.Classes["Address"].PropertiesOrder, []string{"zip", "city", "street"}; !equalStrings(got, want) {
		t.Errorf("Address.PropertiesOrder: got %v want %v", got, want)
	}
}

// TestUnmarshalOrderedObject_NestedRawMessage pins the goccy/go-json
// streaming-decoder contract that the order-capture path depends on:
// Token() must surface object delimiters and string keys, and a
// subsequent Decode(&json.RawMessage) on the value position must hand
// back the raw bytes of nested objects/arrays/primitives intact so the
// downstream goccy Unmarshal can decode them. Exercised through
// DynamicOutputSchema.UnmarshalJSON since unmarshalOrderedObject is
// package-private; if goccy ever diverges from stdlib here the order
// capture would silently break and this test should fail first.
func TestUnmarshalOrderedObject_NestedRawMessage(t *testing.T) {
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
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, want := s.PropertiesOrder, []string{"obj", "lit", "ref", "primitive"}; !equalStrings(got, want) {
		t.Errorf("PropertiesOrder: got %v want %v", got, want)
	}

	// Nested object value: items must round-trip through the goccy
	// Decode(&RawMessage) -> json.Unmarshal path.
	objProp := s.Properties["obj"]
	if objProp == nil || objProp.Type != "list" || objProp.Items == nil || objProp.Items.Type != "string" {
		t.Errorf("nested list property lost its items spec: %+v", objProp)
	}

	// Literal value (a JSON number) round-trips as expected. Goccy's
	// streaming decoder accepts UseNumber on the outer scan, but the
	// inner Decode(&RawMessage) -> Unmarshal must still surface the
	// value through the DynamicProperty.Value any field.
	litProp := s.Properties["lit"]
	if litProp == nil || litProp.Type != "literal_int" || litProp.Value == nil {
		t.Errorf("literal_int property missing value: %+v", litProp)
	}

	// Ref-only property: the inner Decode shouldn't strip the ref key.
	refProp := s.Properties["ref"]
	if refProp == nil || refProp.Ref != "Other" {
		t.Errorf("ref property dropped: %+v", refProp)
	}

	// Nested class round-trips with description + ordered properties.
	other := s.Classes["Other"]
	if other == nil || other.Description != "nested with array of values" {
		t.Errorf("nested class lost description: %+v", other)
	}
	if got, want := other.PropertiesOrder, []string{"a"}; !equalStrings(got, want) {
		t.Errorf("nested class PropertiesOrder: got %v want %v", got, want)
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
			err := json.Unmarshal([]byte(tc.body), &s)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
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
			err := json.Unmarshal([]byte(tc.body), &s)
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
// and leaves the corresponding map / order slice nil, so callers can
// explicitly opt out of classes or enums via `null` without tripping
// the non-object guard.
func TestDynamicOutputSchema_UnmarshalJSON_AcceptsNull(t *testing.T) {
	t.Parallel()
	body := []byte(`{
      "properties": null,
      "classes": null,
      "enums": null
    }`)
	var s DynamicOutputSchema
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if s.Properties != nil {
		t.Errorf("Properties: got %v want nil", s.Properties)
	}
	if s.Classes != nil {
		t.Errorf("Classes: got %v want nil", s.Classes)
	}
	if s.Enums != nil {
		t.Errorf("Enums: got %v want nil", s.Enums)
	}
	if len(s.PropertiesOrder) != 0 || len(s.ClassesOrder) != 0 || len(s.EnumsOrder) != 0 {
		t.Errorf("order slices should be empty; got props=%v classes=%v enums=%v", s.PropertiesOrder, s.ClassesOrder, s.EnumsOrder)
	}

	// Nested null on a class's properties is also accepted.
	nested := []byte(`{
      "properties": {"x": {"type": "string"}},
      "classes": {"C": {"properties": null}}
    }`)
	var s2 DynamicOutputSchema
	if err := json.Unmarshal(nested, &s2); err != nil {
		t.Fatalf("Unmarshal nested: %v", err)
	}
	if s2.Classes["C"].Properties != nil {
		t.Errorf("class C properties: got %v want nil", s2.Classes["C"].Properties)
	}
	if len(s2.Classes["C"].PropertiesOrder) != 0 {
		t.Errorf("class C PropertiesOrder: got %v want empty", s2.Classes["C"].PropertiesOrder)
	}
}

// TestDynamicOutputSchema_UnmarshalJSON_ResetsOnReuse pins that
// decoding into an already-populated receiver clears stale maps and
// *Order slices, instead of merging or retaining the previous values.
// json.Unmarshal into a non-zero target is valid Go usage; the prior
// implementation silently kept stale fields when the new payload
// omitted or nulled a section.
func TestDynamicOutputSchema_UnmarshalJSON_ResetsOnReuse(t *testing.T) {
	t.Parallel()
	first := []byte(`{
      "properties": {"a": {"type": "string"}, "b": {"type": "int"}},
      "classes": {"C": {"properties": {"p": {"type": "string"}, "q": {"type": "int"}}}},
      "enums": {"E": {"values": [{"name": "A"}]}}
    }`)
	var s DynamicOutputSchema
	if err := json.Unmarshal(first, &s); err != nil {
		t.Fatalf("first Unmarshal: %v", err)
	}
	if len(s.Properties) == 0 || len(s.Classes) == 0 || len(s.Enums) == 0 {
		t.Fatalf("first decode left maps empty: %+v", s)
	}

	// Empty object — every section absent. Reuse must clear all
	// previously populated fields.
	if err := json.Unmarshal([]byte(`{}`), &s); err != nil {
		t.Fatalf("reuse Unmarshal {}: %v", err)
	}
	if s.Properties != nil || s.Classes != nil || s.Enums != nil {
		t.Errorf("maps not cleared after empty reuse: %+v", s)
	}
	if len(s.PropertiesOrder) != 0 || len(s.ClassesOrder) != 0 || len(s.EnumsOrder) != 0 {
		t.Errorf("order slices not cleared after empty reuse: props=%v classes=%v enums=%v", s.PropertiesOrder, s.ClassesOrder, s.EnumsOrder)
	}

	// Re-populate then null every section. null is accepted as the
	// nil-map sentinel, so the same clearing contract applies.
	if err := json.Unmarshal(first, &s); err != nil {
		t.Fatalf("re-populate Unmarshal: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"properties":null,"classes":null,"enums":null}`), &s); err != nil {
		t.Fatalf("reuse Unmarshal null: %v", err)
	}
	if s.Properties != nil || s.Classes != nil || s.Enums != nil {
		t.Errorf("maps not cleared after null reuse: %+v", s)
	}
	if len(s.PropertiesOrder) != 0 || len(s.ClassesOrder) != 0 || len(s.EnumsOrder) != 0 {
		t.Errorf("order slices not cleared after null reuse: props=%v classes=%v enums=%v", s.PropertiesOrder, s.ClassesOrder, s.EnumsOrder)
	}
}

// TestDynamicClass_UnmarshalJSON_ResetsOnReuse mirrors the
// DynamicOutputSchema reuse-reset contract on the nested class type.
func TestDynamicClass_UnmarshalJSON_ResetsOnReuse(t *testing.T) {
	t.Parallel()
	first := []byte(`{"description": "old", "alias": "old-alias", "properties": {"p": {"type": "string"}, "q": {"type": "int"}}}`)
	var c DynamicClass
	if err := json.Unmarshal(first, &c); err != nil {
		t.Fatalf("first Unmarshal: %v", err)
	}
	if c.Description == "" || c.Alias == "" || len(c.Properties) == 0 || len(c.PropertiesOrder) == 0 {
		t.Fatalf("first decode left fields empty: %+v", c)
	}
	if err := json.Unmarshal([]byte(`{}`), &c); err != nil {
		t.Fatalf("reuse Unmarshal: %v", err)
	}
	if c.Description != "" || c.Alias != "" || c.Properties != nil || len(c.PropertiesOrder) != 0 {
		t.Errorf("fields not cleared after empty reuse: %+v", c)
	}
}

// TestDynamicInput_Validate_RejectsReservedClassName pins that a
// user class named Baml_Rest_DynamicOutput is rejected at validation
// rather than silently overwritten by buildWorkerClassMap. The name
// is the synthetic top-level class baml-rest writes into the worker
// payload.
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
			Properties: map[string]*DynamicProperty{"x": {Type: "string"}},
			Classes: map[string]*DynamicClass{
				"Baml_Rest_DynamicOutput": {
					Properties: map[string]*DynamicProperty{"sneaky": {Type: "string"}},
				},
			},
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
			Properties: map[string]*DynamicProperty{"x": {Type: "string"}},
			Classes: map[string]*DynamicClass{
				"Baml_Rest_DynamicOutput": {
					Properties: map[string]*DynamicProperty{"sneaky": {Type: "string"}},
				},
			},
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

func TestDynamicInput_Validate_RejectsPreserveOrderWithoutOrderMetadata(t *testing.T) {
	t.Parallel()
	prompt := "hi"
	primary := "TestClient"
	provider := "anthropic"

	mkInput := func() *DynamicInput {
		return &DynamicInput{
			Messages: []DynamicMessage{{Role: "user", TextContent: &prompt}},
			ClientRegistry: &ClientRegistry{
				Primary: &primary,
				Clients: []*ClientProperty{{Name: primary, Provider: provider, Options: map[string]any{"model": "x", "api_key": "k"}}},
			},
			OutputSchema: &DynamicOutputSchema{
				Properties: map[string]*DynamicProperty{
					"alpha": {Type: "string"},
					"bravo": {Type: "int"},
				},
				Classes: map[string]*DynamicClass{
					"C1": {Properties: map[string]*DynamicProperty{"p": {Type: "string"}}},
					"C2": {Properties: map[string]*DynamicProperty{"q": {Type: "string"}}},
				},
				Enums: map[string]*DynamicEnum{
					"E1": {Values: []*DynamicEnumValue{{Name: "A"}}},
					"E2": {Values: []*DynamicEnumValue{{Name: "B"}}},
				},
			},
			PreserveSchemaOrder: true,
		}
	}

	t.Run("missing properties order", func(t *testing.T) {
		in := mkInput()
		err := in.Validate()
		if err == nil || !strings.Contains(err.Error(), "output_schema.properties") {
			t.Fatalf("expected missing properties order, got %v", err)
		}
	})

	t.Run("missing classes order", func(t *testing.T) {
		in := mkInput()
		in.OutputSchema.PropertiesOrder = []string{"alpha", "bravo"}
		err := in.Validate()
		if err == nil || !strings.Contains(err.Error(), "output_schema.classes") {
			t.Fatalf("expected missing classes order, got %v", err)
		}
	})

	t.Run("missing enums order", func(t *testing.T) {
		in := mkInput()
		in.OutputSchema.PropertiesOrder = []string{"alpha", "bravo"}
		in.OutputSchema.ClassesOrder = []string{"C1", "C2"}
		err := in.Validate()
		if err == nil || !strings.Contains(err.Error(), "output_schema.enums") {
			t.Fatalf("expected missing enums order, got %v", err)
		}
	})

	t.Run("positive with all order set", func(t *testing.T) {
		in := mkInput()
		in.OutputSchema.PropertiesOrder = []string{"alpha", "bravo"}
		in.OutputSchema.ClassesOrder = []string{"C1", "C2"}
		in.OutputSchema.EnumsOrder = []string{"E1", "E2"}
		// Nested classes carry only 1 property — order required when len>=2,
		// so single-property classes need no order. Set anyway for clarity.
		in.OutputSchema.Classes["C1"].PropertiesOrder = []string{"p"}
		in.OutputSchema.Classes["C2"].PropertiesOrder = []string{"q"}
		if err := in.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("duplicate order entry rejected", func(t *testing.T) {
		in := mkInput()
		in.OutputSchema.PropertiesOrder = []string{"alpha", "alpha"}
		err := in.Validate()
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("expected duplicate error, got %v", err)
		}
	})

	t.Run("unknown order entry rejected", func(t *testing.T) {
		in := mkInput()
		in.OutputSchema.PropertiesOrder = []string{"alpha", "zzz"}
		err := in.Validate()
		if err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("expected unknown error, got %v", err)
		}
	})
}

func TestDynamicParseInput_Validate_RejectsPreserveOrderWithoutOrderMetadata(t *testing.T) {
	t.Parallel()
	in := &DynamicParseInput{
		Raw: "{\"alpha\":\"x\"}",
		OutputSchema: &DynamicOutputSchema{
			Properties: map[string]*DynamicProperty{
				"alpha": {Type: "string"},
				"bravo": {Type: "int"},
			},
		},
		PreserveSchemaOrder: true,
	}
	err := in.Validate()
	if err == nil || !strings.Contains(err.Error(), "output_schema.properties") {
		t.Fatalf("expected missing properties order error, got %v", err)
	}
	in.OutputSchema.PropertiesOrder = []string{"alpha", "bravo"}
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

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
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	data, err := in.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal worker payload: %v\n%s", err, data)
	}
	opts := decoded["__baml_options__"].(map[string]any)
	tb := opts["type_builder"].(map[string]any)
	dt := tb["dynamic_types"].(map[string]any)
	if got := dt["preserve_order"]; got != true {
		t.Errorf("preserve_order: got %v want true", got)
	}
	order := dt["order"].(map[string]any)
	classes := toStringSlice(t, order["classes"])
	wantClasses := []string{"Baml_Rest_DynamicOutput", "Profile", "Address"}
	if !equalStrings(classes, wantClasses) {
		t.Errorf("order.classes: got %v want %v", classes, wantClasses)
	}
	enums := toStringSlice(t, order["enums"])
	wantEnums := []string{"Status", "Preference"}
	if !equalStrings(enums, wantEnums) {
		t.Errorf("order.enums: got %v want %v", enums, wantEnums)
	}
	props := order["properties"].(map[string]any)
	dynOut := toStringSlice(t, props["Baml_Rest_DynamicOutput"])
	wantDynOut := []string{"delta", "alpha", "charlie"}
	if !equalStrings(dynOut, wantDynOut) {
		t.Errorf("order.properties.Baml_Rest_DynamicOutput: got %v want %v", dynOut, wantDynOut)
	}
	profile := toStringSlice(t, props["Profile"])
	wantProfile := []string{"zulu", "alpha"}
	if !equalStrings(profile, wantProfile) {
		t.Errorf("order.properties.Profile: got %v want %v", profile, wantProfile)
	}
	address := toStringSlice(t, props["Address"])
	wantAddress := []string{"zip", "city"}
	if !equalStrings(address, wantAddress) {
		t.Errorf("order.properties.Address: got %v want %v", address, wantAddress)
	}
}

// TestDynamicInput_ToWorkerInput_SingleClassPropertyOrder pins the
// reachable single-class case: validation legally allows empty
// ClassesOrder when there is exactly one class, so the worker-bound
// DynamicTypesOrder.Properties[<className>] must still carry that
// class's PropertiesOrder. Earlier code only copied per-class order
// for names listed in ClassesOrder, silently dropping nested order
// on single-class schemas.
func TestDynamicInput_ToWorkerInput_SingleClassPropertyOrder(t *testing.T) {
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
			Properties: map[string]*DynamicProperty{
				"only": {Ref: "Solo"},
			},
			PropertiesOrder: []string{"only"},
			Classes: map[string]*DynamicClass{
				"Solo": {
					Properties: map[string]*DynamicProperty{
						"zulu":  {Type: "string"},
						"alpha": {Type: "int"},
						"mike":  {Type: "bool"},
					},
					PropertiesOrder: []string{"zulu", "alpha", "mike"},
				},
			},
			// ClassesOrder intentionally empty — legal because Classes has one key.
		},
		PreserveSchemaOrder: true,
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	data, err := in.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal worker payload: %v\n%s", err, data)
	}
	opts := decoded["__baml_options__"].(map[string]any)
	tb := opts["type_builder"].(map[string]any)
	dt := tb["dynamic_types"].(map[string]any)
	order := dt["order"].(map[string]any)
	props := order["properties"].(map[string]any)
	got := toStringSlice(t, props["Solo"])
	want := []string{"zulu", "alpha", "mike"}
	if !equalStrings(got, want) {
		t.Errorf("order.properties.Solo: got %v want %v", got, want)
	}
	// Order.Classes must also cover the user class (single-class case
	// where ClassesOrder was legally empty). dt.Classes carries the
	// synthetic + user class on the worker side, so the worker-side
	// DynamicTypes.Validate requires both names in Order.Classes.
	classes := toStringSlice(t, order["classes"])
	wantClasses := []string{"Baml_Rest_DynamicOutput", "Solo"}
	if !equalStrings(classes, wantClasses) {
		t.Errorf("order.classes: got %v want %v", classes, wantClasses)
	}
}

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

func toStringSlice(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", v)
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, ok := x.(string)
		if !ok {
			t.Fatalf("expected string, got %T", x)
		}
		out = append(out, s)
	}
	return out
}

// TestDynamicInput_ToWorkerInput_CacheControlBridge pins the bridge from
// the public CacheControl{Type: ...} shape to the generated dynamic BAML
// input's `cache_control.cache_type` shape. The previous wire reused the
// public MessageMetadata struct directly, so the generated input dropped
// the `type` value and forwarded `cache_control: {}` to Anthropic
// once `allowed_role_metadata: ["cache_control"]` enabled the field
// (issue #304).
//
// Asserts:
//   - the tagged message's metadata contains `cache_control.cache_type:"ephemeral"`,
//   - no message carries the public `cache_control.type` JSON key,
//   - no message carries an empty `cache_control:{}` object,
//   - untagged messages emit no `metadata` key at all.
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
						"model":                "test-model",
						"api_key":              "test-key",
						"allowed_role_metadata": []any{"cache_control"},
					},
				},
			},
		},
		OutputSchema: &DynamicOutputSchema{
			Properties: map[string]*DynamicProperty{
				"answer": {Type: "string"},
			},
		},
	}

	if err := input.Validate(); err != nil {
		t.Fatalf("input.Validate: %v", err)
	}
	data, err := input.ToWorkerInput()
	if err != nil {
		t.Fatalf("ToWorkerInput: %v", err)
	}

	// The public `type` JSON key must never leak into the BAML-facing
	// payload; the generated input names the field `cache_type`.
	if bytes.Contains(data, []byte(`"cache_control":{"type":`)) {
		t.Errorf("worker payload still carries public cache_control.type shape:\n%s", data)
	}
	// An empty cache_control object is the exact bug shape from #304;
	// the bridge must never emit it.
	if bytes.Contains(data, []byte(`"cache_control":{}`)) {
		t.Errorf("worker payload carries empty cache_control object:\n%s", data)
	}
	// Spot the empty cache_type case too — the generated decoder treats
	// missing and empty alike, and both are invalid.
	if bytes.Contains(data, []byte(`"cache_type":""`)) {
		t.Errorf("worker payload carries empty cache_type string:\n%s", data)
	}

	var decoded struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal worker payload: %v\n%s", err, data)
	}
	if len(decoded.Messages) != 4 {
		t.Fatalf("expected 4 messages in worker payload, got %d\n%s", len(decoded.Messages), data)
	}

	// Index 0: system text — no metadata.
	if md, ok := decoded.Messages[0]["metadata"]; ok && md != nil {
		t.Errorf("system message must not carry metadata; got %v", md)
	}

	// Index 1: user parts with cache_control.
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

	// Index 2: assistant text — no metadata.
	if md, ok := decoded.Messages[2]["metadata"]; ok && md != nil {
		t.Errorf("assistant message must not carry metadata; got %v", md)
	}
	// Index 3: second user text — no metadata.
	if md, ok := decoded.Messages[3]["metadata"]; ok && md != nil {
		t.Errorf("follow-up user message must not carry metadata; got %v", md)
	}
}

// TestDynamicInput_ToWorkerInput_NoCacheControl_NoMetadata pins the
// omitempty contract: when no message carries metadata, the worker
// payload must not contain any `metadata` key at all.
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
			Properties: map[string]*DynamicProperty{"answer": {Type: "string"}},
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

// TestDynamicMessage_PublicJSON_StillUsesType locks the public API in.
// Callers of the dynamic endpoint pass `cache_control.type`; the bridge
// rewrites it internally. A regression that flipped the public shape
// would break every existing caller, so the public marshalled form is
// asserted here alongside the internal rewrite test above.
func TestDynamicMessage_PublicJSON_StillUsesType(t *testing.T) {
	t.Parallel()
	body := []byte(`{"role":"user","content":"hi","metadata":{"cache_control":{"type":"ephemeral"}}}`)
	var msg DynamicMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal public DynamicMessage: %v", err)
	}
	if msg.Metadata == nil || msg.Metadata.CacheControl == nil {
		t.Fatalf("metadata.CacheControl not populated from public JSON: %+v", msg)
	}
	if msg.Metadata.CacheControl.Type != "ephemeral" {
		t.Errorf("CacheControl.Type: got %q, want %q", msg.Metadata.CacheControl.Type, "ephemeral")
	}
	out, err := json.Marshal(&msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(out, []byte(`"cache_control":{"type":"ephemeral"}`)) {
		t.Errorf("public DynamicMessage marshal lost the type key; got:\n%s", out)
	}
}

// TestDynamicInputValidate_RejectsEmptyCacheControlType pins the contract
// chosen by the #304 CR verdict: callers that intend "no cache control"
// must leave Metadata.CacheControl nil rather than zeroing Type. Without
// this guard, the bridge would pass `cache_type:""` to BAML, which
// already round-trips through the alias-bypass template in dynamic.baml
// and lands on the wire as `cache_control:{"type":""}` — accepted by
// the Go boundary but rejected by Anthropic with the same
// `cache_control.type: Field required` error from issue #304. The
// rejection here makes the failure obvious at the API boundary instead
// of buried in the provider response.
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
			Properties: map[string]*DynamicProperty{"answer": {Type: "string"}},
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

	// Whitespace-only counts as empty.
	input.Messages[1].Metadata.CacheControl.Type = "   "
	if err := input.Validate(); err == nil {
		t.Error("expected Validate() to reject whitespace-only cache_control.type; got nil")
	}

	// A nil Metadata.CacheControl on the same message must pass — that is
	// the "no cache control" sentinel callers should use.
	input.Messages[1].Metadata.CacheControl = nil
	if err := input.Validate(); err != nil {
		t.Errorf("Validate() rejected nil CacheControl: %v", err)
	}

	// A nil Metadata altogether must also pass.
	input.Messages[1].Metadata = nil
	if err := input.Validate(); err != nil {
		t.Errorf("Validate() rejected nil Metadata: %v", err)
	}
}
