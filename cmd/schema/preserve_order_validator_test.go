package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"

	baml_rest "github.com/invakid404/baml-rest"
	"github.com/invakid404/baml-rest/bamlutils"
)

// TestPreserveSchemaOrderNullValidates pins that kin-openapi's runtime
// validator accepts JSON `null` on the preserve_schema_order field of
// both __DynamicInput__ and __DynamicParseInput__. The Nullable=true
// flag asserted by TestSchemaPreserveOrderExposure is a static schema
// property; without this test a future regression could keep
// Nullable=true on the schema struct but break the actual validation
// machinery (e.g., if kin-openapi changes how nullable interacts with
// VisitJSON on primitive fields). Running the same JSON body through
// the validator catches that.
//
// Validation runs both at the field-schema level (the tightest check
// for the Nullable contract) and at the full-request-body level
// through openapi3filter.ValidateRequestBody, which is the same code
// path the production server would use if it ever wired up
// schema-driven request validation on the dynamic endpoints.
func TestPreserveSchemaOrderNullValidates(t *testing.T) {
	baml_rest.InitBamlRuntime()

	// Seed Methods with the dynamic sentinel so the dynamic input/
	// parse-input schemas are registered — mirrors the harness in
	// TestSchemaPreserveOrderExposure.
	origMethods := baml_rest.Methods
	t.Cleanup(func() { baml_rest.Methods = origMethods })
	baml_rest.Methods = map[string]bamlutils.StreamingMethod{
		bamlutils.DynamicMethodName: {
			MakeInput:        func() any { return &SchemaPostRequiredStubInput{} },
			MakeOutput:       func() any { return &SchemaPostRequiredStubOutput{} },
			MakeStreamOutput: func() any { return &SchemaPostRequiredStubOutput{} },
		},
	}

	// Marshal the in-memory schema and round-trip through the
	// kin-openapi Loader so $ref pointers in the generated doc get
	// resolved into populated SchemaRef.Value pointers — without this
	// VisitJSON would error on the first unresolved ref it walks into.
	doc := generateOpenAPISchema()
	if doc == nil || doc.Components == nil {
		t.Fatalf("generated schema has no components")
	}
	data, err := sonic.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal generated schema: %v", err)
	}
	loader := openapi3.NewLoader()
	loaded, err := loader.LoadFromData(data)
	if err != nil {
		t.Fatalf("LoadFromData: %v", err)
	}
	if err := loaded.Validate(loader.Context); err != nil {
		t.Fatalf("loaded schema failed validation: %v", err)
	}

	type bodyCase struct {
		name       string
		schemaName string
		path       string
		body       map[string]any
	}

	// Build minimal valid bodies with explicit JSON null on
	// preserve_schema_order. The non-null required fields are the
	// smallest shapes accepted by the generated schemas (one message
	// with a string content, one client with required options, one
	// property in output_schema). Anything beyond that risks tripping
	// unrelated schema constraints.
	callBody := map[string]any{
		"preserve_schema_order": nil,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"client_registry": map[string]any{
			"primary": "TestClient",
			"clients": []any{
				map[string]any{
					"name":     "TestClient",
					"provider": "openai",
					"options": map[string]any{
						"model":    "gpt",
						"api_key":  "k",
						"base_url": "http://x",
					},
				},
			},
		},
		"output_schema": map[string]any{
			"properties": map[string]any{
				"answer": map[string]any{"type": "string"},
			},
		},
	}
	parseBody := map[string]any{
		"preserve_schema_order": nil,
		"raw":                   `{"answer":"hi"}`,
		"output_schema": map[string]any{
			"properties": map[string]any{
				"answer": map[string]any{"type": "string"},
			},
		},
	}

	cases := []bodyCase{
		{name: "call", schemaName: "__DynamicInput__", path: "/call/" + bamlutils.DynamicEndpointName, body: callBody},
		{name: "parse", schemaName: "__DynamicParseInput__", path: "/parse/" + bamlutils.DynamicEndpointName, body: parseBody},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := loaded.Components.Schemas[tc.schemaName]
			if !ok || ref == nil || ref.Value == nil {
				t.Fatalf("schema %q not present in loaded doc", tc.schemaName)
			}

			// Field-level: confirm null validates against the
			// preserve_schema_order property schema directly. This is
			// the tightest possible check on the Nullable contract.
			prop, ok := ref.Value.Properties["preserve_schema_order"]
			if !ok || prop == nil || prop.Value == nil {
				t.Fatalf("%s missing preserve_schema_order", tc.schemaName)
			}
			if err := prop.Value.VisitJSON(nil); err != nil {
				t.Errorf("%s.preserve_schema_order: VisitJSON(nil) must accept null, got: %v", tc.schemaName, err)
			}

			// Full-body request validation through
			// openapi3filter.ValidateRequestBody. This is the same
			// code path a schema-driven HTTP middleware would run on
			// the wire body, so it catches Nullable-handling
			// regressions that VisitJSON-on-primitive might miss.
			bodyBytes, err := sonic.Marshal(tc.body)
			if err != nil {
				t.Fatalf("marshal request body: %v", err)
			}
			pathItem := loaded.Paths.Find(tc.path)
			if pathItem == nil || pathItem.Post == nil {
				t.Fatalf("path %q POST operation not present in loaded doc", tc.path)
			}
			reqBodyRef := pathItem.Post.RequestBody
			if reqBodyRef == nil || reqBodyRef.Value == nil {
				t.Fatalf("path %q POST has no request body schema", tc.path)
			}

			reqURL, err := url.Parse(tc.path)
			if err != nil {
				t.Fatalf("parse URL: %v", err)
			}
			req := &http.Request{
				Method: http.MethodPost,
				URL:    reqURL,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(bytes.NewReader(bodyBytes)),
			}
			input := &openapi3filter.RequestValidationInput{
				Request: req,
				Route: &routers.Route{
					Spec:      loaded,
					Path:      tc.path,
					PathItem:  pathItem,
					Method:    http.MethodPost,
					Operation: pathItem.Post,
				},
				Options: &openapi3filter.Options{},
			}
			if err := openapi3filter.ValidateRequestBody(context.Background(), input, reqBodyRef.Value); err != nil {
				t.Errorf("%s: ValidateRequestBody must accept body with preserve_schema_order=null, got: %v\nbody:\n%s", tc.schemaName, err, bodyBytes)
			}
		})
	}
}
