//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// MockLLMURL is the mock LLM URL from TestEnv (used by dynamic endpoint tests)
var MockLLMURL string

func init() {
	// This will be set after TestEnv is initialized in TestMain
	// For now, tests access TestEnv.MockLLMInternal directly
}

// TestDynamicEndpoint tests the _dynamic endpoint functionality.
// Note: There is a known bug in BAML's streaming API that causes call/stream endpoints
// to fail with dynamically added classes. The parse endpoint should work correctly.
func TestDynamicEndpoint(t *testing.T) {
	// Dynamic endpoints require BAML >= 0.215.0
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}

	t.Run("parse_simple", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
			Raw: `{"answer": "4"}`,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"answer": {Type: "string"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicParse failed: %v", err)
		}

		t.Logf("Response status: %d", resp.StatusCode)
		if resp.Error != "" {
			t.Logf("Response error: %s", resp.Error)
		}
		if resp.Data != nil {
			t.Logf("Response data: %s", string(resp.Data))
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result map[string]any
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		t.Logf("Parsed result: %+v", result)

		// The parse endpoint returns DynamicProperties for dynamic classes
		if result["DynamicProperties"] != nil {
			props := result["DynamicProperties"].(map[string]any)
			if props["answer"] != "4" {
				t.Errorf("Expected answer '4', got %v", props["answer"])
			}
		} else if result["answer"] != "4" {
			t.Errorf("Expected answer '4', got %v", result["answer"])
		}
	})

	t.Run("parse_multiple_properties", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
			Raw: `{"name": "John", "age": 30, "active": true}`,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"name":   {Type: "string"},
					"age":    {Type: "int"},
					"active": {Type: "bool"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicParse failed: %v", err)
		}

		t.Logf("Response status: %d", resp.StatusCode)
		if resp.Error != "" {
			t.Logf("Response error: %s", resp.Error)
		}
		if resp.Data != nil {
			t.Logf("Response data: %s", string(resp.Data))
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result map[string]any
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		t.Logf("Parsed result: %+v", result)

		// Check properties (may be in DynamicProperties or top-level)
		props := result
		if result["DynamicProperties"] != nil {
			props = result["DynamicProperties"].(map[string]any)
		}

		if props["name"] != "John" {
			t.Errorf("Expected name 'John', got %v", props["name"])
		}
		// JSON unmarshals numbers as float64
		if age, ok := props["age"].(float64); !ok || age != 30 {
			t.Errorf("Expected age 30, got %v", props["age"])
		}
		if props["active"] != true {
			t.Errorf("Expected active true, got %v", props["active"])
		}
	})

	t.Run("parse_with_list_property", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
			Raw: `{"tags": ["go", "rust", "python"]}`,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"tags": {
						Type:  "list",
						Items: &testutil.DynamicTypeRef{Type: "string"},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicParse failed: %v", err)
		}

		t.Logf("Response status: %d", resp.StatusCode)
		if resp.Error != "" {
			t.Logf("Response error: %s", resp.Error)
		}
		if resp.Data != nil {
			t.Logf("Response data: %s", string(resp.Data))
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result map[string]any
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		t.Logf("Parsed result: %+v", result)

		// Check properties (may be in DynamicProperties or top-level)
		props := result
		if result["DynamicProperties"] != nil {
			props = result["DynamicProperties"].(map[string]any)
		}

		tags, ok := props["tags"].([]any)
		if !ok {
			t.Fatalf("Expected tags to be an array, got %T", props["tags"])
		}
		if len(tags) != 3 {
			t.Errorf("Expected 3 tags, got %d", len(tags))
		}
	})

	// Parse validation tests
	t.Run("validation_missing_raw", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
			Raw: "", // Empty raw
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"answer": {Type: "string"},
				},
			},
		})
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}

		if resp.StatusCode != 400 {
			t.Errorf("Expected status 400, got %d", resp.StatusCode)
		}
		if resp.Error == "" {
			t.Error("Expected error message for missing raw")
		}
	})

	t.Run("validation_missing_output_schema", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
			Raw:          `{"answer": "test"}`,
			OutputSchema: nil, // Missing output schema
		})
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}

		if resp.StatusCode != 400 {
			t.Errorf("Expected status 400, got %d", resp.StatusCode)
		}
	})

	t.Run("validation_empty_output_schema", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
			Raw: `{"answer": "test"}`,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{}, // Empty properties
			},
		})
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}

		if resp.StatusCode != 400 {
			t.Errorf("Expected status 400, got %d", resp.StatusCode)
		}
	})

	// Call/stream validation tests (skipped due to BAML bug)
	t.Run("validation_call_missing_messages", func(t *testing.T) {
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages:       []testutil.DynamicMessage{}, // Empty messages
			ClientRegistry: testutil.CreateTestClient(TestEnv.MockLLMInternal, "test"),
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"answer": {Type: "string"},
				},
			},
		})
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}

		if resp.StatusCode != 400 {
			t.Errorf("Expected status 400, got %d", resp.StatusCode)
		}
	})

	t.Run("validation_call_missing_client_registry", func(t *testing.T) {
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "user", Content: "Hello"},
			},
			ClientRegistry: nil, // Missing client registry
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"answer": {Type: "string"},
				},
			},
		})
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}

		if resp.StatusCode != 400 {
			t.Errorf("Expected status 400, got %d", resp.StatusCode)
		}
	})

	// The following tests are for call/stream endpoints which may fail due to known BAML bug.
	// Skipping by default - can be enabled once the bug is fixed.

	t.Run("call_simple", func(t *testing.T) {
		// Skip: Known BAML bug where streaming API doesn't propagate dynamic classes to parser
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Register scenario
		content := `{"answer": "4"}`
		opts := setupNonStreamingScenario(t, "test-dynamic-call", content)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "user", Content: "What is 2+2?"},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"answer": {Type: "string"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result map[string]any
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		// Check answer
		props := result
		if result["DynamicProperties"] != nil {
			props = result["DynamicProperties"].(map[string]any)
		}
		if props["answer"] != "4" {
			t.Errorf("Expected answer '4', got %v", props["answer"])
		}
	})

	t.Run("call_with_raw", func(t *testing.T) {
		// Skip: Known BAML bug
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Register scenario
		content := `{"answer": "4"}`
		opts := setupNonStreamingScenario(t, "test-dynamic-call-raw", content)

		resp, err := BAMLClient.DynamicCallWithRaw(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "user", Content: "What is 2+2?"},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"answer": {Type: "string"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicCallWithRaw failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		// Should have raw output
		if resp.Raw == "" {
			t.Error("Expected raw output to be non-empty")
		}
	})

	t.Run("stream_ndjson", func(t *testing.T) {
		// Skip: Known BAML bug
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Register streaming scenario
		opts := setupScenario(t, "test-dynamic-stream", `{"answer": "4"}`)

		events, errs := BAMLClient.DynamicStreamNDJSON(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "user", Content: "What is 2+2?"},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"answer": {Type: "string"},
				},
			},
		})

		var gotFinal bool
		for event := range events {
			if event.IsFinal() {
				gotFinal = true
			}
		}

		// Check for errors
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("Stream error: %v", err)
			}
		default:
		}

		if !gotFinal {
			t.Error("Expected to receive final event")
		}
	})
}

// TestDynamicEndpointOpenAPI tests that the OpenAPI schema includes the dynamic endpoints.
func TestDynamicEndpointOpenAPI(t *testing.T) {
	// Dynamic endpoints require BAML >= 0.215.0
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}

	t.Run("has_dynamic_endpoints", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		schema, err := BAMLClient.GetOpenAPISchema(ctx)
		if err != nil {
			t.Fatalf("Failed to get OpenAPI schema: %v", err)
		}

		// Check that _dynamic endpoints are present
		expectedPaths := []string{
			"/call/_dynamic",
			"/call-with-raw/_dynamic",
			"/stream/_dynamic",
			"/stream-with-raw/_dynamic",
			"/parse/_dynamic",
		}

		for _, path := range expectedPaths {
			if schema.Paths[path] == nil {
				t.Errorf("Expected path %s to exist in OpenAPI schema", path)
			}
		}
	})

	t.Run("has_dynamic_schemas", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		schema, err := BAMLClient.GetOpenAPISchema(ctx)
		if err != nil {
			t.Fatalf("Failed to get OpenAPI schema: %v", err)
		}

		// Check that dynamic schemas are present
		expectedSchemas := []string{
			"__DynamicInput__",
			"__DynamicOutput__",
			"__DynamicMessage__",
			"__DynamicOutputSchema__",
		}

		for _, schemaName := range expectedSchemas {
			if schema.Components.Schemas[schemaName] == nil {
				t.Errorf("Expected schema %s to exist in OpenAPI schema", schemaName)
			}
		}
	})
}
