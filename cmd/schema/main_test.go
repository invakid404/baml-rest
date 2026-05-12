package main

import (
	"slices"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	baml_rest "github.com/invakid404/baml-rest"
)

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
