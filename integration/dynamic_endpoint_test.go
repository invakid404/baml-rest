//go:build integration

package integration

import (
	"context"
	"strings"
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

		// Response should be flattened (no DynamicProperties wrapper)
		if result["DynamicProperties"] != nil {
			t.Errorf("Response should be flattened, but found DynamicProperties wrapper")
		}
		if result["answer"] != "4" {
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

		// Response should be flattened (no DynamicProperties wrapper)
		if result["DynamicProperties"] != nil {
			t.Errorf("Response should be flattened, but found DynamicProperties wrapper")
		}

		if result["name"] != "John" {
			t.Errorf("Expected name 'John', got %v", result["name"])
		}
		// JSON unmarshals numbers as float64
		if age, ok := result["age"].(float64); !ok || age != 30 {
			t.Errorf("Expected age 30, got %v", result["age"])
		}
		if result["active"] != true {
			t.Errorf("Expected active true, got %v", result["active"])
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
						Items: &testutil.DynamicTypeSpec{Type: "string"},
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

		// Response should be flattened (no DynamicProperties wrapper)
		if result["DynamicProperties"] != nil {
			t.Errorf("Response should be flattened, but found DynamicProperties wrapper")
		}

		tags, ok := result["tags"].([]any)
		if !ok {
			t.Fatalf("Expected tags to be an array, got %T", result["tags"])
		}
		if len(tags) != 3 {
			t.Errorf("Expected 3 tags, got %d", len(tags))
		}
	})

	t.Run("parse_nested_class", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Use unique class name to avoid conflict with existing BAML Address class
		resp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
			Raw: `{"name": "John", "location": {"street": "123 Main St", "city": "Boston"}}`,
			OutputSchema: &testutil.DynamicOutputSchema{
				Classes: map[string]*testutil.DynamicClass{
					"DynamicLocation": {
						Properties: map[string]*testutil.DynamicProperty{
							"street": {Type: "string"},
							"city":   {Type: "string"},
						},
					},
				},
				Properties: map[string]*testutil.DynamicProperty{
					"name":     {Type: "string"},
					"location": {Ref: "DynamicLocation"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicParse failed: %v", err)
		}

		t.Logf("Response status: %d", resp.StatusCode)
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

		// Response should be flattened
		if result["DynamicProperties"] != nil {
			t.Errorf("Response should be flattened, but found DynamicProperties wrapper")
		}

		if result["name"] != "John" {
			t.Errorf("Expected name 'John', got %v", result["name"])
		}

		location, ok := result["location"].(map[string]any)
		if !ok {
			t.Fatalf("Expected location to be an object, got %T", result["location"])
		}

		// Dynamic classes should be unwrapped - no Name/Fields wrapper
		if location["Name"] != nil || location["Fields"] != nil {
			t.Errorf("Dynamic class should be unwrapped, got internal format: %v", location)
		}

		if location["street"] != "123 Main St" {
			t.Errorf("Expected street '123 Main St', got %v", location["street"])
		}
		if location["city"] != "Boston" {
			t.Errorf("Expected city 'Boston', got %v", location["city"])
		}
	})

	t.Run("parse_with_enum", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Use unique enum name to avoid conflicts
		resp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
			Raw: `{"name": "John", "status": "ACTIVE"}`,
			OutputSchema: &testutil.DynamicOutputSchema{
				Enums: map[string]*testutil.DynamicEnum{
					"DynamicStatus": {
						Values: []*testutil.DynamicEnumValue{
							{Name: "ACTIVE"},
							{Name: "INACTIVE"},
							{Name: "PENDING"},
						},
					},
				},
				Properties: map[string]*testutil.DynamicProperty{
					"name":   {Type: "string"},
					"status": {Ref: "DynamicStatus"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicParse failed: %v", err)
		}

		t.Logf("Response status: %d", resp.StatusCode)
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

		// Response should be flattened
		if result["DynamicProperties"] != nil {
			t.Errorf("Response should be flattened, but found DynamicProperties wrapper")
		}

		if result["name"] != "John" {
			t.Errorf("Expected name 'John', got %v", result["name"])
		}

		// Dynamic enums should be unwrapped to plain string values
		if result["status"] != "ACTIVE" {
			t.Errorf("Expected status 'ACTIVE', got %v (type: %T)", result["status"], result["status"])
		}

		// Verify it's NOT in the internal format
		if statusMap, isMap := result["status"].(map[string]any); isMap {
			t.Errorf("Dynamic enum should be unwrapped to string, got internal format: %v", statusMap)
		}
	})

	// Regression test: ensure deeply nested dynamic types are properly unwrapped
	// This tests that DynamicClass and DynamicEnum internal representations
	// ({"Name":"...", "Fields":{...}} and {"Name":"...", "Value":"..."}) are flattened
	t.Run("regression_nested_dynamic_types_unwrapped", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Use unique names to avoid conflicts with existing BAML types
		resp, err := BAMLClient.DynamicParse(ctx, testutil.DynamicParseRequest{
			Raw: `{"employee": {"name": "Alice", "role": "ADMIN"}, "tasks": [{"id": 1, "priority": "HIGH"}]}`,
			OutputSchema: &testutil.DynamicOutputSchema{
				Enums: map[string]*testutil.DynamicEnum{
					"DynRole": {
						Values: []*testutil.DynamicEnumValue{
							{Name: "ADMIN"},
							{Name: "USER"},
						},
					},
					"DynPriority": {
						Values: []*testutil.DynamicEnumValue{
							{Name: "HIGH"},
							{Name: "LOW"},
						},
					},
				},
				Classes: map[string]*testutil.DynamicClass{
					"DynEmployee": {
						Properties: map[string]*testutil.DynamicProperty{
							"name": {Type: "string"},
							"role": {Ref: "DynRole"},
						},
					},
					"DynTask": {
						Properties: map[string]*testutil.DynamicProperty{
							"id":       {Type: "int"},
							"priority": {Ref: "DynPriority"},
						},
					},
				},
				Properties: map[string]*testutil.DynamicProperty{
					"employee": {Ref: "DynEmployee"},
					"tasks": {
						Type:  "list",
						Items: &testutil.DynamicTypeSpec{Ref: "DynTask"},
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicParse failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		t.Logf("Response data: %s", string(resp.Data))

		var result map[string]any
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		// Verify no DynamicProperties wrapper
		if result["DynamicProperties"] != nil {
			t.Errorf("Response should be flattened, but found DynamicProperties wrapper")
		}

		// Verify nested class is unwrapped
		employee, ok := result["employee"].(map[string]any)
		if !ok {
			t.Fatalf("Expected employee to be an object, got %T", result["employee"])
		}
		if employee["Name"] != nil || employee["Fields"] != nil {
			t.Errorf("Nested class should be unwrapped, got internal format: %v", employee)
		}
		if employee["name"] != "Alice" {
			t.Errorf("Expected employee.name 'Alice', got %v", employee["name"])
		}

		// Verify nested enum is unwrapped
		if employee["role"] != "ADMIN" {
			t.Errorf("Expected employee.role 'ADMIN', got %v (type: %T)", employee["role"], employee["role"])
		}

		// Verify array of nested classes with enums
		tasks, ok := result["tasks"].([]any)
		if !ok {
			t.Fatalf("Expected tasks to be an array, got %T", result["tasks"])
		}
		if len(tasks) != 1 {
			t.Fatalf("Expected 1 task, got %d", len(tasks))
		}

		task, ok := tasks[0].(map[string]any)
		if !ok {
			t.Fatalf("Expected task to be an object, got %T", tasks[0])
		}
		t.Logf("Task content: %+v", task)
		if task["Name"] != nil || task["Fields"] != nil {
			t.Errorf("Nested class in array should be unwrapped, got internal format: %v", task)
		}
		// Check id to verify the class was parsed at all
		if task["id"] == nil {
			t.Errorf("Expected task.id to be present, task=%v", task)
		}
		if task["priority"] != "HIGH" {
			t.Errorf("Expected task.priority 'HIGH', got %v (type: %T)", task["priority"], task["priority"])
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
		if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
			t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
		}

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
		if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
			t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
		}

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
		if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
			t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
		}

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
		if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
			t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
		}

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
		if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
			t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
		}

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

	// Test that {output_format} placeholder in message content gets replaced
	// with BAML's output format instructions before being sent to the LLM
	t.Run("output_format_placeholder", func(t *testing.T) {
		if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
			t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-dynamic-output-format"
		content := `{"name": "Alice", "age": 30}`
		opts := setupNonStreamingScenario(t, scenarioID, content)

		// Send a request with {output_format} placeholder in message content
		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "user", Content: "Extract info from: Alice is 30.\n\n{output_format}"},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"name": {Type: "string"},
					"age":  {Type: "int"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		// Fetch the captured request from mock LLM to verify placeholder replacement
		capturedBody, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}

		capturedStr := string(capturedBody)
		t.Logf("Captured request body: %s", capturedStr)

		// The literal placeholder should NOT be in the prompt sent to the LLM
		if strings.Contains(capturedStr, "{output_format}") {
			t.Error("Prompt sent to LLM still contains literal {output_format} placeholder - replacement did not happen")
		}

		// The prompt should contain the output schema field names, which BAML
		// includes in its generated output format instructions
		if !strings.Contains(capturedStr, "name") || !strings.Contains(capturedStr, "age") {
			t.Error("Prompt sent to LLM does not contain output schema field names - output format was not injected")
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
			"__DynamicClass__",
			"__DynamicEnum__",
			"__DynamicEnumValue__",
		}

		for _, schemaName := range expectedSchemas {
			if schema.Components.Schemas[schemaName] == nil {
				t.Errorf("Expected schema %s to exist in OpenAPI schema", schemaName)
			}
		}
	})
}
