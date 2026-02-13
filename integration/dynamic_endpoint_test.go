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

// TestDynamicEndpointMultiPartContent tests multi-part content support in dynamic endpoints.
func TestDynamicEndpointMultiPartContent(t *testing.T) {
	// Dynamic endpoints require BAML >= 0.215.0
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}
	if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
	}

	t.Run("text_parts_only", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"answer": "4"}`
		opts := setupNonStreamingScenario(t, "test-dynamic-parts-text", content)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("What is 2+2?")},
					},
				},
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

		props := result
		if result["DynamicProperties"] != nil {
			props = result["DynamicProperties"].(map[string]any)
		}
		if props["answer"] != "4" {
			t.Errorf("Expected answer '4', got %v", props["answer"])
		}
	})

	t.Run("output_format_part", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-dynamic-parts-output-format"
		content := `{"name": "Alice", "age": 30}`
		opts := setupNonStreamingScenario(t, scenarioID, content)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("Extract info from: Alice is 30.")},
						{Type: "output_format"},
					},
				},
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

		// Verify the output format was injected into the prompt
		capturedBody, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}
		capturedStr := string(capturedBody)
		t.Logf("Captured request body: %s", capturedStr)

		// The prompt should contain output schema field names
		if !strings.Contains(capturedStr, "name") || !strings.Contains(capturedStr, "age") {
			t.Error("Prompt sent to LLM does not contain output schema field names - output_format part was not rendered")
		}
	})

	t.Run("image_url_part", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-dynamic-parts-image-url"
		content := `{"description": "A green ogre"}`
		opts := setupNonStreamingScenario(t, scenarioID, content)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("Describe this image:")},
						{Type: "image", Image: &testutil.MediaInput{
							URL: strPtr("https://upload.wikimedia.org/wikipedia/en/4/4d/Shrek_%28character%29.png"),
						}},
						{Type: "output_format"},
					},
				},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"description": {Type: "string"},
				},
			},
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		// Verify the LLM received image content parts
		capturedBody, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}
		capturedStr := string(capturedBody)
		t.Logf("Captured request body: %s", capturedStr)

		// The request to the LLM should contain an image_url content part
		if !strings.Contains(capturedStr, "image_url") && !strings.Contains(capturedStr, "image") {
			t.Error("Request to LLM does not contain image content - image part was not rendered")
		}
	})

	t.Run("image_base64_part", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-dynamic-parts-image-b64"
		content := `{"description": "A tiny pixel"}`
		opts := setupNonStreamingScenario(t, scenarioID, content)

		// Minimal valid 1x1 PNG base64
		b64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMBAQApDs4AAAAASUVORK5CYII="

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("Describe this image:")},
						{Type: "image", Image: &testutil.MediaInput{
							Base64:    strPtr(b64),
							MediaType: strPtr("image/png"),
						}},
						{Type: "output_format"},
					},
				},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"description": {Type: "string"},
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

		props := result
		if result["DynamicProperties"] != nil {
			props = result["DynamicProperties"].(map[string]any)
		}
		if props["description"] != "A tiny pixel" {
			t.Errorf("Expected description 'A tiny pixel', got %v", props["description"])
		}
	})

	t.Run("mixed_text_and_string_messages", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"answer": "yes"}`
		opts := setupNonStreamingScenario(t, "test-dynamic-parts-mixed", content)

		// Mix of string content and parts content in different messages
		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "system", Content: "You are a helpful assistant."},
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("Is this a valid image?")},
						{Type: "image", Image: &testutil.MediaInput{
							URL: strPtr("https://upload.wikimedia.org/wikipedia/en/4/4d/Shrek_%28character%29.png"),
						}},
						{Type: "output_format"},
					},
				},
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
	})

	t.Run("stream_with_parts", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupScenario(t, "test-dynamic-parts-stream", `{"description": "An image"}`)

		events, errs := BAMLClient.DynamicStreamNDJSON(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("Describe:")},
						{Type: "image", Image: &testutil.MediaInput{
							URL: strPtr("https://upload.wikimedia.org/wikipedia/en/4/4d/Shrek_%28character%29.png"),
						}},
						{Type: "output_format"},
					},
				},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema: &testutil.DynamicOutputSchema{
				Properties: map[string]*testutil.DynamicProperty{
					"description": {Type: "string"},
				},
			},
		})

		var lastEvent *testutil.StreamEvent
		for event := range events {
			lastEvent = &event
		}

		if err := <-errs; err != nil {
			t.Fatalf("Stream error: %v", err)
		}

		if lastEvent == nil {
			t.Fatal("Expected at least one stream event")
		}
	})
}

// TestDynamicEndpointMultiPartValidation tests validation of multi-part content.
func TestDynamicEndpointMultiPartValidation(t *testing.T) {
	// Dynamic endpoints require BAML >= 0.215.0
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}
	if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
	}

	t.Run("empty_parts_array", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role:    "user",
					Content: []testutil.DynamicContentPart{},
				},
			},
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

	t.Run("invalid_part_type", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "invalid_type"},
					},
				},
			},
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

	t.Run("image_part_missing_payload", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "image"}, // Missing image payload
					},
				},
			},
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

	t.Run("image_part_both_url_and_base64", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "image", Image: &testutil.MediaInput{
							URL:    strPtr("https://example.com/img.png"),
							Base64: strPtr("abc123"),
						}},
					},
				},
			},
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

	t.Run("output_format_with_extra_text", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "output_format", Text: strPtr("ignored")},
					},
				},
			},
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
			t.Errorf("Expected status 400 for output_format with extra text field, got %d", resp.StatusCode)
		}
	})

	t.Run("image_part_with_extra_text", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{
							Type:  "image",
							Image: &testutil.MediaInput{URL: strPtr("https://example.com/img.png")},
							Text:  strPtr("extra text"),
						},
					},
				},
			},
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
			t.Errorf("Expected status 400 for image part with extra text field, got %d", resp.StatusCode)
		}
	})

	t.Run("text_part_with_extra_image", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{
							Type:  "text",
							Text:  strPtr("hello"),
							Image: &testutil.MediaInput{URL: strPtr("https://example.com/img.png")},
						},
					},
				},
			},
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
			t.Errorf("Expected status 400 for text part with extra image field, got %d", resp.StatusCode)
		}
	})
}

// strPtr is a helper to create a string pointer for tests.
func strPtr(s string) *string {
	return &s
}

// extractLLMMessageTexts parses a captured OpenAI chat completions request body
// and returns the text content of each message. For multi-part content (array),
// it concatenates text parts. For string content, it returns it directly.
func extractLLMMessageTexts(t *testing.T, capturedBody []byte) []struct {
	Role string
	Text string
} {
	t.Helper()

	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(capturedBody, &req); err != nil {
		t.Fatalf("Failed to parse captured request: %v", err)
	}

	var result []struct {
		Role string
		Text string
	}
	for _, m := range req.Messages {
		var text string

		// Try string first
		var str string
		if err := json.Unmarshal(m.Content, &str); err == nil {
			text = str
		} else {
			// Try array of content parts
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(m.Content, &parts); err == nil {
				var texts []string
				for _, p := range parts {
					if p.Type == "text" {
						texts = append(texts, p.Text)
					}
				}
				text = strings.Join(texts, "")
			}
		}

		result = append(result, struct {
			Role string
			Text string
		}{Role: m.Role, Text: text})
	}
	return result
}

// TestDynamicEndpointPromptWhitespace tests that the Jinja template doesn't
// inject spurious whitespace (blank lines, leading/trailing spaces) into the
// prompt sent to the LLM. Whitespace artifacts can significantly affect LLM
// output quality, so these tests act as a regression guard.
func TestDynamicEndpointPromptWhitespace(t *testing.T) {
	if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
		t.Skip("Skipping: dynamic endpoints require BAML >= 0.215.0")
	}
	if BAMLSourcePath == "" && !bamlutils.IsVersionAtLeast(BAMLVersion, "0.219.0") {
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")
	}

	simpleOutputSchema := &testutil.DynamicOutputSchema{
		Properties: map[string]*testutil.DynamicProperty{
			"answer": {Type: "string"},
		},
	}

	t.Run("single_string_message_no_leading_trailing_whitespace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-ws-single-string"
		opts := setupNonStreamingScenario(t, scenarioID, `{"answer": "ok"}`)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "user", Content: "What is 2+2? {output_format}"},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema:   simpleOutputSchema,
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		captured, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}

		messages := extractLLMMessageTexts(t, captured)
		for i, m := range messages {
			t.Logf("Message[%d] role=%s text=%q", i, m.Role, m.Text)

			if m.Text != strings.TrimSpace(m.Text) {
				t.Errorf("Message[%d] has leading/trailing whitespace: %q", i, m.Text)
			}
		}
	})

	t.Run("single_text_part_no_leading_trailing_whitespace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-ws-single-text-part"
		opts := setupNonStreamingScenario(t, scenarioID, `{"answer": "ok"}`)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("What is 2+2?")},
						{Type: "output_format"},
					},
				},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema:   simpleOutputSchema,
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		captured, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}

		messages := extractLLMMessageTexts(t, captured)
		for i, m := range messages {
			t.Logf("Message[%d] role=%s text=%q", i, m.Role, m.Text)

			if m.Text != strings.TrimSpace(m.Text) {
				t.Errorf("Message[%d] has leading/trailing whitespace: %q", i, m.Text)
			}
		}
	})

	t.Run("multiple_text_parts_no_blank_lines_between", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-ws-multi-text-parts"
		opts := setupNonStreamingScenario(t, scenarioID, `{"answer": "ok"}`)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("First line.")},
						{Type: "text", Text: strPtr("Second line.")},
						{Type: "text", Text: strPtr("Third line.")},
						{Type: "output_format"},
					},
				},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema:   simpleOutputSchema,
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		captured, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}

		messages := extractLLMMessageTexts(t, captured)
		for i, m := range messages {
			t.Logf("Message[%d] role=%s text=%q", i, m.Role, m.Text)

			// Check for consecutive blank lines (two or more newlines in a row
			// beyond what the user explicitly included)
			if strings.Contains(m.Text, "\n\n\n") {
				t.Errorf("Message[%d] has consecutive blank lines (3+ newlines): %q", i, m.Text)
			}

			if m.Text != strings.TrimSpace(m.Text) {
				t.Errorf("Message[%d] has leading/trailing whitespace: %q", i, m.Text)
			}
		}
	})

	t.Run("multiple_messages_no_spurious_whitespace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-ws-multi-messages"
		opts := setupNonStreamingScenario(t, scenarioID, `{"answer": "ok"}`)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "system", Content: "You are helpful."},
				{Role: "user", Content: "What is 2+2? {output_format}"},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema:   simpleOutputSchema,
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		captured, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}

		messages := extractLLMMessageTexts(t, captured)
		for i, m := range messages {
			t.Logf("Message[%d] role=%s text=%q", i, m.Role, m.Text)

			if m.Text != strings.TrimSpace(m.Text) {
				t.Errorf("Message[%d] has leading/trailing whitespace: %q", i, m.Text)
			}
		}
	})

	t.Run("mixed_string_and_parts_messages_no_spurious_whitespace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-ws-mixed-messages"
		opts := setupNonStreamingScenario(t, scenarioID, `{"answer": "ok"}`)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "system", Content: "You are helpful."},
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("Describe this image:")},
						{Type: "image", Image: &testutil.MediaInput{
							URL: strPtr("https://upload.wikimedia.org/wikipedia/en/4/4d/Shrek_%28character%29.png"),
						}},
						{Type: "output_format"},
					},
				},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema:   simpleOutputSchema,
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		captured, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}

		messages := extractLLMMessageTexts(t, captured)
		for i, m := range messages {
			t.Logf("Message[%d] role=%s text=%q", i, m.Role, m.Text)

			if m.Text != strings.TrimSpace(m.Text) {
				t.Errorf("Message[%d] has leading/trailing whitespace: %q", i, m.Text)
			}

			if strings.Contains(m.Text, "\n\n\n") {
				t.Errorf("Message[%d] has consecutive blank lines: %q", i, m.Text)
			}
		}
	})

	t.Run("parts_with_output_format_between_text_no_extra_whitespace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-ws-output-format-between"
		opts := setupNonStreamingScenario(t, scenarioID, `{"answer": "ok"}`)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{
					Role: "user",
					Content: []testutil.DynamicContentPart{
						{Type: "text", Text: strPtr("Instructions here.")},
						{Type: "output_format"},
						{Type: "text", Text: strPtr("Now answer.")},
					},
				},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema:   simpleOutputSchema,
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		captured, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}

		messages := extractLLMMessageTexts(t, captured)
		for i, m := range messages {
			t.Logf("Message[%d] role=%s text=%q", i, m.Role, m.Text)

			if strings.Contains(m.Text, "\n\n\n") {
				t.Errorf("Message[%d] has consecutive blank lines: %q", i, m.Text)
			}

			if m.Text != strings.TrimSpace(m.Text) {
				t.Errorf("Message[%d] has leading/trailing whitespace: %q", i, m.Text)
			}

			// Verify the output format was actually injected (not blank)
			if !strings.Contains(m.Text, "answer") {
				t.Errorf("Message[%d] doesn't contain output schema field 'answer' - output_format part may not have rendered: %q", i, m.Text)
			}
		}
	})

	t.Run("many_messages_no_accumulated_whitespace", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		scenarioID := "test-ws-many-messages"
		opts := setupNonStreamingScenario(t, scenarioID, `{"answer": "ok"}`)

		resp, err := BAMLClient.DynamicCall(ctx, testutil.DynamicRequest{
			Messages: []testutil.DynamicMessage{
				{Role: "system", Content: "You are a helpful assistant."},
				{Role: "user", Content: "First question."},
				{Role: "assistant", Content: "First answer."},
				{Role: "user", Content: "Second question."},
				{Role: "assistant", Content: "Second answer."},
				{Role: "user", Content: "Third question. {output_format}"},
			},
			ClientRegistry: opts.ClientRegistry,
			OutputSchema:   simpleOutputSchema,
		})
		if err != nil {
			t.Fatalf("DynamicCall failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		captured, err := MockClient.GetLastRequest(ctx, scenarioID)
		if err != nil {
			t.Fatalf("Failed to get last request: %v", err)
		}

		messages := extractLLMMessageTexts(t, captured)
		if len(messages) < 6 {
			t.Fatalf("Expected at least 6 messages, got %d", len(messages))
		}

		for i, m := range messages {
			t.Logf("Message[%d] role=%s text=%q", i, m.Role, m.Text)

			if m.Text != strings.TrimSpace(m.Text) {
				t.Errorf("Message[%d] has leading/trailing whitespace: %q", i, m.Text)
			}

			if strings.Contains(m.Text, "\n\n\n") {
				t.Errorf("Message[%d] has consecutive blank lines: %q", i, m.Text)
			}
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
			"__DynamicContentPart__",
			"__DynamicMediaInput__",
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
