package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	baml_rest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
)

// SchemaPostRequiredStubInput is the input type wired into the stub
// StreamingMethod injected by TestPostRequestBodyRequired. It must have a
// stable, exported name because generateOpenAPISchema derives the component
// schema name via reflect.Type.Name().
type SchemaPostRequiredStubInput struct {
	Foo string `json:"foo"`
}

// SchemaPostRequiredStubOutput is the final-output type for the stub method.
type SchemaPostRequiredStubOutput struct {
	Bar string `json:"bar"`
}

// TestSchemaRequiredAndNullability pins the contract the schema customizer
// is expected to honour for `json:omitempty`, `json:"-"`, and pointer-slice
// elements.
//
// The bug fixed alongside this test: the customizer previously derived the
// per-struct `required` list from pointer-ness only, ignoring tag options.
// Fields like BamlOptions.IncludeReasoning (bool, omitempty) were emitted
// as required, ClientProperty.ProviderSet (json:"-") leaked into required
// despite having no property, and `[]*T` slice items lost their pointer
// nullability. Each assertion below maps to one of those failure modes.
func TestSchemaRequiredAndNullability(t *testing.T) {
	baml_rest.InitBamlRuntime()
	doc := generateOpenAPISchema()
	if doc == nil || doc.Components == nil {
		t.Fatalf("generated schema has no components")
	}
	schemas := doc.Components.Schemas

	type assertion struct {
		schema             string
		mustNotRequire     []string
		mustRequire        []string
		nullableProperties []string
	}
	cases := []assertion{
		{
			schema:             "BamlOptions",
			mustNotRequire:     []string{"include_reasoning", "client_registry", "retry", "type_builder"},
			nullableProperties: []string{"client_registry", "type_builder", "retry"},
		},
		{
			schema:      "ClientRegistry",
			mustRequire: []string{"clients"},
		},
		{
			schema:         "ClientProperty",
			mustNotRequire: []string{"provider", "options", "ProviderSet", "retry_policy"},
			mustRequire:    []string{"name"},
		},
		{
			schema:         "RetryConfig",
			mustNotRequire: []string{"strategy", "delay_ms", "multiplier", "max_delay_ms"},
			mustRequire:    []string{"max_retries"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.schema, func(t *testing.T) {
			ref, ok := schemas[tc.schema]
			if !ok || ref == nil || ref.Value == nil {
				t.Fatalf("schema %q not registered", tc.schema)
			}
			schema := ref.Value

			for _, name := range tc.mustNotRequire {
				if slices.Contains(schema.Required, name) {
					t.Errorf("%s.required unexpectedly contains %q (required=%v)", tc.schema, name, schema.Required)
				}
				if _, hasProperty := schema.Properties[name]; !hasProperty && name == "ProviderSet" {
					// json:"-" fields must be absent from properties entirely.
					continue
				}
			}
			for _, name := range tc.mustRequire {
				if !slices.Contains(schema.Required, name) {
					t.Errorf("%s.required missing %q (required=%v)", tc.schema, name, schema.Required)
				}
			}
			for _, name := range tc.nullableProperties {
				prop, ok := schema.Properties[name]
				if !ok || prop == nil {
					t.Errorf("%s.properties missing nullable property %q", tc.schema, name)
					continue
				}
				if !isNullable(prop) {
					t.Errorf("%s.%s expected nullable wrap", tc.schema, name)
				}
			}
		})
	}

	t.Run("ClientProperty.ProviderSet absent from properties", func(t *testing.T) {
		ref := schemas["ClientProperty"]
		if _, present := ref.Value.Properties["ProviderSet"]; present {
			t.Errorf("ClientProperty must skip json:\"-\" ProviderSet from properties")
		}
	})

	t.Run("ClientRegistry.clients items nullable", func(t *testing.T) {
		ref := schemas["ClientRegistry"]
		clients, ok := ref.Value.Properties["clients"]
		if !ok || clients == nil || clients.Value == nil || clients.Value.Items == nil {
			t.Fatalf("ClientRegistry.clients missing items schema")
		}
		if !isNullable(clients.Value.Items) {
			t.Errorf("ClientRegistry.clients.items expected nullable wrap (got %+v)", clients.Value.Items)
		}
	})
}

// TestPostRequestBodyRequired pins #252: every generated POST operation
// must emit requestBody.required = true. The OpenAPI 3.0 default is
// false, and an absent flag lets downstream client generators mark the
// body parameter as optional even though every endpoint here
// semantically requires a body. The test seeds baml_rest.Methods with
// a stub static method and the dynamic-method sentinel so both static
// and dynamic codepaths in generateOpenAPISchema fire.
func TestPostRequestBodyRequired(t *testing.T) {
	baml_rest.InitBamlRuntime()

	origMethods := baml_rest.Methods
	t.Cleanup(func() { baml_rest.Methods = origMethods })

	baml_rest.Methods = map[string]bamlutils.StreamingMethod{
		"StubMethod": {
			MakeInput:        func() any { return &SchemaPostRequiredStubInput{} },
			MakeOutput:       func() any { return &SchemaPostRequiredStubOutput{} },
			MakeStreamOutput: func() any { return &SchemaPostRequiredStubOutput{} },
		},
		bamlutils.DynamicMethodName: {
			MakeInput:        func() any { return &SchemaPostRequiredStubInput{} },
			MakeOutput:       func() any { return &SchemaPostRequiredStubOutput{} },
			MakeStreamOutput: func() any { return &SchemaPostRequiredStubOutput{} },
		},
	}

	doc := generateOpenAPISchema()
	if doc == nil || doc.Paths == nil {
		t.Fatalf("generated schema has no paths")
	}

	dynamicPathPrefixes := []string{
		"/call/_dynamic",
		"/call-with-raw/_dynamic",
		"/stream/_dynamic",
		"/stream-with-raw/_dynamic",
		"/parse/_dynamic",
	}
	seenDynamic := make(map[string]bool, len(dynamicPathPrefixes))

	for _, path := range doc.Paths.InMatchingOrder() {
		item := doc.Paths.Value(path)
		if item == nil || item.Post == nil {
			continue
		}
		op := item.Post
		if op.RequestBody == nil || op.RequestBody.Value == nil {
			t.Errorf("POST %s missing RequestBody value", path)
			continue
		}
		if !op.RequestBody.Value.Required {
			t.Errorf("POST %s requestBody.required = false (want true); downstream client generators will emit the body parameter as optional", path)
		}
		for _, prefix := range dynamicPathPrefixes {
			if strings.HasPrefix(path, prefix) {
				seenDynamic[prefix] = true
			}
		}
	}

	for _, prefix := range dynamicPathPrefixes {
		if !seenDynamic[prefix] {
			t.Errorf("dynamic operation %s missing from generated paths; test cannot cover the dynamic codepath", prefix)
		}
	}

	staticSpot := "/call/StubMethod"
	item := doc.Paths.Value(staticSpot)
	if item == nil || item.Post == nil {
		t.Fatalf("static spot-check %s not generated; static codepath uncovered", staticSpot)
	}
	if item.Post.RequestBody == nil || item.Post.RequestBody.Value == nil || !item.Post.RequestBody.Value.Required {
		t.Errorf("static spot-check %s requestBody.required = false (want true)", staticSpot)
	}
}

// TestErrorDetailsSchema pins #259: the apierror `details` field on
// both __ErrorResponse__ and __StreamErrorEvent__ must resolve to a
// named __ErrorDetails__ component declaring each known detail key as
// a typed optional property, while retaining additionalProperties:true
// for forward compatibility. Downstream codegen (Orval/etc.) relies on
// this shape to produce typed access on details.raw, details.body,
// details.status_code, ... instead of the prior {[k:string]:unknown}.
func TestErrorDetailsSchema(t *testing.T) {
	baml_rest.InitBamlRuntime()
	doc := generateOpenAPISchema()
	if doc == nil || doc.Components == nil {
		t.Fatalf("generated schema has no components")
	}
	schemas := doc.Components.Schemas

	ref, ok := schemas["__ErrorDetails__"]
	if !ok || ref == nil || ref.Value == nil {
		t.Fatalf("__ErrorDetails__ component not registered")
	}
	schema := ref.Value

	if schema.Type == nil || !schema.Type.Is(openapi3.TypeObject) {
		t.Errorf("__ErrorDetails__.type expected object, got %v", schema.Type)
	}
	if schema.AdditionalProperties.Has == nil || !*schema.AdditionalProperties.Has {
		t.Errorf("__ErrorDetails__.additionalProperties expected true (forward compatibility), got %+v", schema.AdditionalProperties)
	}
	if len(schema.Required) != 0 {
		t.Errorf("__ErrorDetails__.required expected empty (every detail field is optional), got %v", schema.Required)
	}

	expectedFields := map[string]string{
		"raw":               openapi3.TypeString,
		"body":              openapi3.TypeString,
		"status_code":       openapi3.TypeInteger,
		"client_name":       openapi3.TypeString,
		"error_code":        openapi3.TypeString,
		"error_message":     openapi3.TypeString,
		"exception_type":    openapi3.TypeString,
		"exception_message": openapi3.TypeString,
		"stacktrace":        openapi3.TypeString,
	}
	for name, wantType := range expectedFields {
		prop, ok := schema.Properties[name]
		if !ok || prop == nil || prop.Value == nil {
			t.Errorf("__ErrorDetails__.properties missing %q", name)
			continue
		}
		if prop.Value.Type == nil || !prop.Value.Type.Is(wantType) {
			t.Errorf("__ErrorDetails__.properties.%s expected type %s, got %v", name, wantType, prop.Value.Type)
		}
		if strings.TrimSpace(prop.Value.Description) == "" {
			t.Errorf("__ErrorDetails__.properties.%s expected non-empty description", name)
		}
	}

	const wantRef = "#/components/schemas/__ErrorDetails__"
	for _, parent := range []string{"__ErrorResponse__", "__StreamErrorEvent__"} {
		t.Run(parent+".details ref", func(t *testing.T) {
			parentRef, ok := schemas[parent]
			if !ok || parentRef == nil || parentRef.Value == nil {
				t.Fatalf("%s component not registered", parent)
			}
			details, ok := parentRef.Value.Properties["details"]
			if !ok || details == nil {
				t.Fatalf("%s.properties.details missing", parent)
			}
			if details.Ref != wantRef {
				t.Errorf("%s.properties.details expected $ref %q, got %q (value=%+v)", parent, wantRef, details.Ref, details.Value)
			}
		})
	}
}

// isNullable reports whether a SchemaRef carries explicit nullability —
// either inline Nullable=true on its value, or the allOf+nullable wrap
// emitted for $ref pointers.
func isNullable(ref *openapi3.SchemaRef) bool {
	if ref == nil {
		return false
	}
	if ref.Value != nil && ref.Value.Nullable {
		return true
	}
	return false
}

// TestSchemaPreserveOrderExposure pins #313: the public OpenAPI surface
// must surface the preserve-order opt-in on both __DynamicInput__ and
// __DynamicParseInput__, and the worker-bound TypeBuilder.dynamic_types
// shape must expose the matching preserve_order/order keys so generated
// clients can use the feature.
func TestSchemaPreserveOrderExposure(t *testing.T) {
	baml_rest.InitBamlRuntime()

	// The dynamic-input/parse schemas are only registered when Methods
	// carries the dynamic-method sentinel — mirror the seeding done by
	// TestPostRequestBodyRequired so this test exercises the dynamic
	// codepath.
	origMethods := baml_rest.Methods
	t.Cleanup(func() { baml_rest.Methods = origMethods })
	baml_rest.Methods = map[string]bamlutils.StreamingMethod{
		bamlutils.DynamicMethodName: {
			MakeInput:        func() any { return &SchemaPostRequiredStubInput{} },
			MakeOutput:       func() any { return &SchemaPostRequiredStubOutput{} },
			MakeStreamOutput: func() any { return &SchemaPostRequiredStubOutput{} },
		},
	}

	doc := generateOpenAPISchema()
	if doc == nil || doc.Components == nil {
		t.Fatalf("generated schema has no components")
	}
	schemas := doc.Components.Schemas

	for _, name := range []string{"__DynamicInput__", "__DynamicParseInput__"} {
		ref, ok := schemas[name]
		if !ok || ref == nil || ref.Value == nil {
			t.Fatalf("schema %q not registered", name)
		}
		prop, ok := ref.Value.Properties["preserve_schema_order"]
		if !ok || prop == nil || prop.Value == nil {
			t.Errorf("%s.properties missing preserve_schema_order", name)
			continue
		}
		if !prop.Value.Type.Is(openapi3.TypeBoolean) {
			t.Errorf("%s.preserve_schema_order: expected boolean, got %v", name, prop.Value.Type)
		}
		// Opt-in: must not be in required.
		if slices.Contains(ref.Value.Required, "preserve_schema_order") {
			t.Errorf("%s.required must not include preserve_schema_order (it's an opt-in)", name)
		}
		// Tri-state contract: JSON null is accepted as the
		// "inherit server default" sentinel, so the schema must mark
		// the field nullable. Without Nullable=true the OpenAPI surface
		// would lie about the wire shape.
		if !isNullable(prop) {
			t.Errorf("%s.preserve_schema_order: expected Nullable=true for tri-state inherit-default contract", name)
		}
	}

	tb, ok := schemas["TypeBuilder"]
	if !ok || tb == nil || tb.Value == nil {
		t.Fatalf("TypeBuilder schema not registered")
	}
	dt, ok := tb.Value.Properties["dynamic_types"]
	if !ok || dt == nil || dt.Value == nil {
		t.Fatalf("TypeBuilder.dynamic_types not present")
	}

	preserveOrder, ok := dt.Value.Properties["preserve_order"]
	if !ok || preserveOrder == nil || preserveOrder.Value == nil {
		t.Errorf("dynamic_types missing preserve_order")
	} else if !preserveOrder.Value.Type.Is(openapi3.TypeBoolean) {
		t.Errorf("dynamic_types.preserve_order expected boolean, got %v", preserveOrder.Value.Type)
	}

	// The legacy 'order' side-channel was removed when DynamicOutputSchema /
	// DynamicTypes migrated to ordered maps (#318). The wire shape now
	// carries order intrinsically via the JSON object key order.
	if _, present := dt.Value.Properties["order"]; present {
		t.Errorf("dynamic_types.order must be removed; OrderedMap carries order intrinsically")
	}
}
