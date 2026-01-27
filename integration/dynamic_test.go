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

func TestDynamicTypes(t *testing.T) {
	t.Run("type_builder_adds_field", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Register scenario that returns a dynamic field
		content := `{"base_field": "base value", "extra_field": "extra value"}`
		opts := setupNonStreamingScenario(t, "test-dynamic", content)

		// Add type builder to inject the extra_field
		// Note: Use "dynamic class" prefix to add fields to dynamic classes
		opts.TypeBuilder = &testutil.TypeBuilder{
			BAMLSnippets: []string{
				`dynamic class DynamicOutput {
					extra_field string
				}`,
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		// Parse the response - should include both fields
		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		// Check base field
		if result.BaseField != "base value" {
			t.Errorf("Expected base_field 'base value', got %v", result.BaseField)
		}

		// Check dynamic extra field in DynamicProperties
		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}
		if extraField, ok := result.DynamicProperties["extra_field"].(string); !ok || extraField != "extra value" {
			t.Errorf("Expected extra_field 'extra value', got %v", result.DynamicProperties["extra_field"])
		}
	})

	t.Run("regression_31_dynamic_value_unwrapping", func(t *testing.T) {
		// This test verifies the fix for issue #31:
		// Dynamic values should be properly unwrapped, not returned as reflect.Value
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"base_field": "base", "dynamic_int": 42, "dynamic_bool": true}`
		opts := setupNonStreamingScenario(t, "test-dynamic-unwrap", content)

		opts.TypeBuilder = &testutil.TypeBuilder{
			BAMLSnippets: []string{
				`dynamic class DynamicOutput {
					dynamic_int int
					dynamic_bool bool
				}`,
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		// The int should be a proper number, not a string or reflect.Value representation
		if dynamicInt, ok := result.DynamicProperties["dynamic_int"].(float64); !ok || dynamicInt != 42 {
			t.Errorf("Expected dynamic_int 42, got %v (type %T)", result.DynamicProperties["dynamic_int"], result.DynamicProperties["dynamic_int"])
		}

		// The bool should be a proper boolean
		if dynamicBool, ok := result.DynamicProperties["dynamic_bool"].(bool); !ok || !dynamicBool {
			t.Errorf("Expected dynamic_bool true, got %v (type %T)", result.DynamicProperties["dynamic_bool"], result.DynamicProperties["dynamic_bool"])
		}
	})

	t.Run("union_type_in_dynamic_class", func(t *testing.T) {
		// Test that union types work correctly in dynamic classes
		// Uses static SuccessResult and ErrorResult classes as union variants

		t.Run("success_variant", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Return a SuccessResult variant
			content := `{"base_field": "test", "result": {"data": "operation completed"}}`
			opts := setupNonStreamingScenario(t, "test-union-success", content)

			opts.TypeBuilder = &testutil.TypeBuilder{
				BAMLSnippets: []string{
					`dynamic class DynamicOutput {
						result SuccessResult | ErrorResult
					}`,
				},
			}

			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetDynamic",
				Input:   map[string]any{"input": "test"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				BaseField         string         `json:"base_field"`
				DynamicProperties map[string]any `json:"DynamicProperties"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.BaseField != "test" {
				t.Errorf("Expected base_field 'test', got %v", result.BaseField)
			}

			if result.DynamicProperties == nil {
				t.Fatalf("Expected DynamicProperties to be present, got nil")
			}

			// The result field should be present in DynamicProperties
			resultField, ok := result.DynamicProperties["result"]
			if !ok {
				t.Fatalf("Expected 'result' field in DynamicProperties, got %v", result.DynamicProperties)
			}

			// Should be a SuccessResult with data field
			resultMap, ok := resultField.(map[string]any)
			if !ok {
				t.Fatalf("Expected result to be a map, got %T: %v", resultField, resultField)
			}

			if data, ok := resultMap["data"].(string); !ok || data != "operation completed" {
				t.Errorf("Expected data 'operation completed', got %v", resultMap["data"])
			}
		})

		t.Run("error_variant", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Return an ErrorResult variant
			content := `{"base_field": "test", "result": {"error": "something went wrong", "code": 500}}`
			opts := setupNonStreamingScenario(t, "test-union-error", content)

			opts.TypeBuilder = &testutil.TypeBuilder{
				BAMLSnippets: []string{
					`dynamic class DynamicOutput {
						result SuccessResult | ErrorResult
					}`,
				},
			}

			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetDynamic",
				Input:   map[string]any{"input": "test"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				BaseField         string         `json:"base_field"`
				DynamicProperties map[string]any `json:"DynamicProperties"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.BaseField != "test" {
				t.Errorf("Expected base_field 'test', got %v", result.BaseField)
			}

			if result.DynamicProperties == nil {
				t.Fatalf("Expected DynamicProperties to be present, got nil")
			}

			// The result field should be present in DynamicProperties
			resultField, ok := result.DynamicProperties["result"]
			if !ok {
				t.Fatalf("Expected 'result' field in DynamicProperties, got %v", result.DynamicProperties)
			}

			// Should be an ErrorResult with error and code fields
			resultMap, ok := resultField.(map[string]any)
			if !ok {
				t.Fatalf("Expected result to be a map, got %T: %v", resultField, resultField)
			}

			if errorMsg, ok := resultMap["error"].(string); !ok || errorMsg != "something went wrong" {
				t.Errorf("Expected error 'something went wrong', got %v", resultMap["error"])
			}

			// JSON unmarshals numbers as float64
			if code, ok := resultMap["code"].(float64); !ok || code != 500 {
				t.Errorf("Expected code 500, got %v (type %T)", resultMap["code"], resultMap["code"])
			}
		})
	})

	t.Run("dynamic_enum_values", func(t *testing.T) {
		// Test that dynamic enums can have values added at runtime
		// The DynamicCategory enum has DEFAULT defined in BAML,
		// but we can add new values like PREMIUM at runtime using TypeBuilder

		t.Run("base_enum_value", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Return a response with the base DEFAULT category
			content := `{"name": "Basic Plan", "category": "DEFAULT"}`
			opts := setupNonStreamingScenario(t, "test-dynamic-enum-base", content)

			// No type builder needed - DEFAULT is already defined
			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetDynamicEnum",
				Input:   map[string]any{"input": "categorize this item"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				Name     string `json:"name"`
				Category string `json:"category"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.Name != "Basic Plan" {
				t.Errorf("Expected name 'Basic Plan', got %v", result.Name)
			}

			if result.Category != "DEFAULT" {
				t.Errorf("Expected category 'DEFAULT', got %v", result.Category)
			}
		})

		t.Run("dynamic_enum_value", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Return a response with a dynamically added PREMIUM category
			content := `{"name": "Premium Plan", "category": "PREMIUM"}`
			opts := setupNonStreamingScenario(t, "test-dynamic-enum-dynamic", content)

			// Add PREMIUM value to the dynamic enum at runtime
			opts.TypeBuilder = &testutil.TypeBuilder{
				BAMLSnippets: []string{
					`dynamic enum DynamicCategory {
						PREMIUM
					}`,
				},
			}

			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetDynamicEnum",
				Input:   map[string]any{"input": "categorize this premium item"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				Name     string `json:"name"`
				Category string `json:"category"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.Name != "Premium Plan" {
				t.Errorf("Expected name 'Premium Plan', got %v", result.Name)
			}

			if result.Category != "PREMIUM" {
				t.Errorf("Expected category 'PREMIUM', got %v", result.Category)
			}
		})

		t.Run("multiple_dynamic_enum_values", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Return a response with a dynamically added ENTERPRISE category
			content := `{"name": "Enterprise Solution", "category": "ENTERPRISE"}`
			opts := setupNonStreamingScenario(t, "test-dynamic-enum-multiple", content)

			// Add multiple new values to the dynamic enum at runtime
			opts.TypeBuilder = &testutil.TypeBuilder{
				BAMLSnippets: []string{
					`dynamic enum DynamicCategory {
						PREMIUM
						ENTERPRISE
						VIP
					}`,
				},
			}

			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetDynamicEnum",
				Input:   map[string]any{"input": "categorize this enterprise item"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				Name     string `json:"name"`
				Category string `json:"category"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.Name != "Enterprise Solution" {
				t.Errorf("Expected name 'Enterprise Solution', got %v", result.Name)
			}

			if result.Category != "ENTERPRISE" {
				t.Errorf("Expected category 'ENTERPRISE', got %v", result.Category)
			}
		})

		t.Run("dynamic_enum_field_in_dynamic_class", func(t *testing.T) {
			// Test that a dynamic enum field added to a dynamic class
			// appears in DynamicProperties (not as a top-level field)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Return a response with base_field and a dynamically added category field
			content := `{"base_field": "test item", "category": "PREMIUM"}`
			opts := setupNonStreamingScenario(t, "test-dynamic-enum-in-class", content)

			// Add both:
			// 1. The PREMIUM value to the DynamicCategory enum
			// 2. A category field of type DynamicCategory to DynamicOutput
			opts.TypeBuilder = &testutil.TypeBuilder{
				BAMLSnippets: []string{
					`dynamic enum DynamicCategory {
						PREMIUM
					}`,
					`dynamic class DynamicOutput {
						category DynamicCategory
					}`,
				},
			}

			resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
				Method:  "GetDynamic",
				Input:   map[string]any{"input": "categorize this item"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				BaseField         string         `json:"base_field"`
				DynamicProperties map[string]any `json:"DynamicProperties"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			// Check base field
			if result.BaseField != "test item" {
				t.Errorf("Expected base_field 'test item', got %v", result.BaseField)
			}

			// Check that category is in DynamicProperties (not top-level)
			if result.DynamicProperties == nil {
				t.Fatalf("Expected DynamicProperties to be present, got nil")
			}

			category, ok := result.DynamicProperties["category"].(string)
			if !ok {
				t.Fatalf("Expected 'category' field in DynamicProperties, got %v", result.DynamicProperties)
			}

			if category != "PREMIUM" {
				t.Errorf("Expected category 'PREMIUM', got %v", category)
			}
		})
	})
}

// TestDynamicTypesImperative tests the new dynamic_types JSON schema-like API
// that uses the imperative TypeBuilder under the hood.
func TestDynamicTypesImperative(t *testing.T) {
	t.Run("add_class_with_primitive_properties", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Return a response with dynamically defined fields
		content := `{"base_field": "test", "name": "John", "age": 30, "active": true}`
		opts := setupNonStreamingScenario(t, "test-dynamic-imperative-primitives", content)

		// Use dynamic_types instead of baml_snippets
		opts.TypeBuilder = &testutil.TypeBuilder{
			DynamicTypes: &testutil.DynamicTypes{
				Classes: map[string]*testutil.DynamicClass{
					"DynamicOutput": {
						Properties: map[string]*testutil.DynamicProperty{
							"name":   {Type: "string"},
							"age":    {Type: "int"},
							"active": {Type: "bool"},
						},
					},
				},
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.BaseField != "test" {
			t.Errorf("Expected base_field 'test', got %v", result.BaseField)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		if name, ok := result.DynamicProperties["name"].(string); !ok || name != "John" {
			t.Errorf("Expected name 'John', got %v", result.DynamicProperties["name"])
		}

		if age, ok := result.DynamicProperties["age"].(float64); !ok || age != 30 {
			t.Errorf("Expected age 30, got %v", result.DynamicProperties["age"])
		}

		if active, ok := result.DynamicProperties["active"].(bool); !ok || !active {
			t.Errorf("Expected active true, got %v", result.DynamicProperties["active"])
		}
	})

	t.Run("add_enum_with_values", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Return a response with a dynamically added enum value
		content := `{"name": "Gold Member", "category": "GOLD"}`
		opts := setupNonStreamingScenario(t, "test-dynamic-imperative-enum", content)

		// Use dynamic_types to add GOLD value to DynamicCategory
		opts.TypeBuilder = &testutil.TypeBuilder{
			DynamicTypes: &testutil.DynamicTypes{
				Enums: map[string]*testutil.DynamicEnum{
					"DynamicCategory": {
						Values: []*testutil.DynamicEnumValue{
							{Name: "GOLD"},
							{Name: "SILVER"},
							{Name: "BRONZE"},
						},
					},
				},
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamicEnum",
			Input:   map[string]any{"input": "categorize gold member"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			Name     string `json:"name"`
			Category string `json:"category"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.Name != "Gold Member" {
			t.Errorf("Expected name 'Gold Member', got %v", result.Name)
		}

		if result.Category != "GOLD" {
			t.Errorf("Expected category 'GOLD', got %v", result.Category)
		}
	})

	t.Run("add_list_property", func(t *testing.T) {
		// Skip for BAML versions < 0.215.0 due to a bug with list types in dynamic classes
		// that was fixed in later versions
		if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
			t.Skip("Skipping: list types in dynamic classes require BAML >= 0.215.0")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Return a response with a list field
		content := `{"base_field": "test", "tags": ["go", "baml", "rest"]}`
		opts := setupNonStreamingScenario(t, "test-dynamic-imperative-list", content)

		opts.TypeBuilder = &testutil.TypeBuilder{
			DynamicTypes: &testutil.DynamicTypes{
				Classes: map[string]*testutil.DynamicClass{
					"DynamicOutput": {
						Properties: map[string]*testutil.DynamicProperty{
							"tags": {
								Type:  "list",
								Items: &testutil.DynamicTypeRef{Type: "string"},
							},
						},
					},
				},
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		tags, ok := result.DynamicProperties["tags"].([]any)
		if !ok {
			t.Fatalf("Expected tags to be an array, got %T: %v", result.DynamicProperties["tags"], result.DynamicProperties["tags"])
		}

		if len(tags) != 3 {
			t.Errorf("Expected 3 tags, got %d", len(tags))
		}
	})

	t.Run("add_optional_property", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Return a response with an optional field that's present
		content := `{"base_field": "test", "nickname": "Johnny"}`
		opts := setupNonStreamingScenario(t, "test-dynamic-imperative-optional", content)

		opts.TypeBuilder = &testutil.TypeBuilder{
			DynamicTypes: &testutil.DynamicTypes{
				Classes: map[string]*testutil.DynamicClass{
					"DynamicOutput": {
						Properties: map[string]*testutil.DynamicProperty{
							"nickname": {
								Type:  "optional",
								Inner: &testutil.DynamicTypeRef{Type: "string"},
							},
						},
					},
				},
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		if nickname, ok := result.DynamicProperties["nickname"].(string); !ok || nickname != "Johnny" {
			t.Errorf("Expected nickname 'Johnny', got %v", result.DynamicProperties["nickname"])
		}
	})

	t.Run("reference_existing_class", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Return a response referencing an existing SuccessResult class
		content := `{"base_field": "test", "result": {"data": "success!"}}`
		opts := setupNonStreamingScenario(t, "test-dynamic-imperative-ref", content)

		opts.TypeBuilder = &testutil.TypeBuilder{
			DynamicTypes: &testutil.DynamicTypes{
				Classes: map[string]*testutil.DynamicClass{
					"DynamicOutput": {
						Properties: map[string]*testutil.DynamicProperty{
							// Reference existing SuccessResult class from baml_src
							"result": {Ref: "SuccessResult"},
						},
					},
				},
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		resultField, ok := result.DynamicProperties["result"].(map[string]any)
		if !ok {
			t.Fatalf("Expected result to be a map, got %T: %v", result.DynamicProperties["result"], result.DynamicProperties["result"])
		}

		if data, ok := resultField["data"].(string); !ok || data != "success!" {
			t.Errorf("Expected data 'success!', got %v", resultField["data"])
		}
	})

	t.Run("combined_dynamic_types_and_snippets", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Test using both dynamic_types and baml_snippets together
		// dynamic_types should be processed first, then snippets can reference them
		content := `{"base_field": "test", "priority": "HIGH", "status": "complete"}`
		opts := setupNonStreamingScenario(t, "test-dynamic-combined", content)

		opts.TypeBuilder = &testutil.TypeBuilder{
			// dynamic_types creates the Priority enum
			DynamicTypes: &testutil.DynamicTypes{
				Enums: map[string]*testutil.DynamicEnum{
					"DynamicCategory": {
						Values: []*testutil.DynamicEnumValue{
							{Name: "HIGH"},
							{Name: "LOW"},
						},
					},
				},
			},
			// baml_snippets adds a field that uses the enum
			BAMLSnippets: []string{
				`dynamic class DynamicOutput {
					priority DynamicCategory
					status string
				}`,
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		if priority, ok := result.DynamicProperties["priority"].(string); !ok || priority != "HIGH" {
			t.Errorf("Expected priority 'HIGH', got %v", result.DynamicProperties["priority"])
		}

		if status, ok := result.DynamicProperties["status"].(string); !ok || status != "complete" {
			t.Errorf("Expected status 'complete', got %v", result.DynamicProperties["status"])
		}
	})

	t.Run("enum_value_with_alias", func(t *testing.T) {
		// Test that enum values with aliases work correctly
		// The alias allows the LLM to output "high priority" but it maps to "HIGH"
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// LLM returns the alias value, should be mapped to the enum name
		content := `{"name": "Urgent Task", "category": "high priority"}`
		opts := setupNonStreamingScenario(t, "test-enum-alias", content)

		opts.TypeBuilder = &testutil.TypeBuilder{
			DynamicTypes: &testutil.DynamicTypes{
				Enums: map[string]*testutil.DynamicEnum{
					"DynamicCategory": {
						Values: []*testutil.DynamicEnumValue{
							{Name: "HIGH", Alias: "high priority", Description: "High priority items"},
							{Name: "MEDIUM", Alias: "medium priority"},
							{Name: "LOW", Alias: "low priority"},
						},
					},
				},
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamicEnum",
			Input:   map[string]any{"input": "categorize urgent task"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			Name     string `json:"name"`
			Category string `json:"category"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		// The alias "high priority" should be mapped to "HIGH"
		if result.Category != "HIGH" {
			t.Errorf("Expected category 'HIGH' (mapped from alias 'high priority'), got %v", result.Category)
		}
	})

	t.Run("add_map_property", func(t *testing.T) {
		// Skip for BAML versions < 0.215.0 due to a bug with map types in dynamic classes
		if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
			t.Skip("Skipping: map types in dynamic classes require BAML >= 0.215.0")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Return a response with a map field
		content := `{"base_field": "test", "metadata": {"key1": "value1", "key2": "value2"}}`
		opts := setupNonStreamingScenario(t, "test-dynamic-imperative-map", content)

		opts.TypeBuilder = &testutil.TypeBuilder{
			DynamicTypes: &testutil.DynamicTypes{
				Classes: map[string]*testutil.DynamicClass{
					"DynamicOutput": {
						Properties: map[string]*testutil.DynamicProperty{
							"metadata": {
								Type:   "map",
								Keys:   &testutil.DynamicTypeRef{Type: "string"},
								Values: &testutil.DynamicTypeRef{Type: "string"},
							},
						},
					},
				},
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		metadata, ok := result.DynamicProperties["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("Expected metadata to be a map, got %T: %v", result.DynamicProperties["metadata"], result.DynamicProperties["metadata"])
		}

		if metadata["key1"] != "value1" || metadata["key2"] != "value2" {
			t.Errorf("Expected metadata {key1: value1, key2: value2}, got %v", metadata)
		}
	})

	t.Run("add_union_property", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Return a response with a union field (string variant)
		content := `{"base_field": "test", "value": "hello"}`
		opts := setupNonStreamingScenario(t, "test-dynamic-imperative-union", content)

		opts.TypeBuilder = &testutil.TypeBuilder{
			DynamicTypes: &testutil.DynamicTypes{
				Classes: map[string]*testutil.DynamicClass{
					"DynamicOutput": {
						Properties: map[string]*testutil.DynamicProperty{
							"value": {
								Type: "union",
								OneOf: []*testutil.DynamicTypeRef{
									{Type: "string"},
									{Type: "int"},
									{Type: "bool"},
								},
							},
						},
					},
				},
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		if value, ok := result.DynamicProperties["value"].(string); !ok || value != "hello" {
			t.Errorf("Expected value 'hello', got %v (%T)", result.DynamicProperties["value"], result.DynamicProperties["value"])
		}
	})

	t.Run("nested_new_classes", func(t *testing.T) {
		// Skip: BAML streaming API has a bug where dynamically added classes are not
		// visible to the parser. The sync API works fine, but we use streaming internally.
		// Bug reported to BAML team.
		t.Skip("BAML bug: streaming API doesn't propagate dynamic classes to parser")

		// Test creating a new class (Location) and referencing it from an existing
		// dynamic class (DynamicOutput). This tests the proper ordering of TypeBuilder
		// operations - we must not call Type() on existing class builders prematurely.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Return a response with nested dynamically created classes
		// Use unique class names to avoid conflicts with existing baml_src classes
		content := `{"base_field": "test", "location": {"street": "123 Main St", "city": "Springfield"}}`
		opts := setupNonStreamingScenario(t, "test-dynamic-imperative-nested", content)

		opts.TypeBuilder = &testutil.TypeBuilder{
			DynamicTypes: &testutil.DynamicTypes{
				Classes: map[string]*testutil.DynamicClass{
					// Create a new Location class (unique name to avoid conflicts)
					"Location": {
						Properties: map[string]*testutil.DynamicProperty{
							"street": {Type: "string"},
							"city":   {Type: "string"},
						},
					},
					// Then DynamicOutput references it
					"DynamicOutput": {
						Properties: map[string]*testutil.DynamicProperty{
							"location": {Ref: "Location"},
						},
					},
				},
			},
		}

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetDynamic",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		location, ok := result.DynamicProperties["location"].(map[string]any)
		if !ok {
			t.Fatalf("Expected location to be a map, got %T: %v", result.DynamicProperties["location"], result.DynamicProperties["location"])
		}

		if location["street"] != "123 Main St" {
			t.Errorf("Expected street '123 Main St', got %v", location["street"])
		}
		if location["city"] != "Springfield" {
			t.Errorf("Expected city 'Springfield', got %v", location["city"])
		}
	})

	t.Run("nested_new_classes_parse_api", func(t *testing.T) {
		// Skip for BAML versions < 0.215.0 due to a bug with nested dynamic classes
		if !bamlutils.IsVersionAtLeast(BAMLVersion, "0.215.0") {
			t.Skip("Skipping: nested dynamic classes require BAML >= 0.215.0")
		}

		// Test the same scenario as nested_new_classes but using the Parse API directly
		// instead of going through a full LLM call. This helps isolate whether the bug
		// is in the streaming API or also affects direct parsing.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := &testutil.BAMLOptions{
			TypeBuilder: &testutil.TypeBuilder{
				DynamicTypes: &testutil.DynamicTypes{
					Classes: map[string]*testutil.DynamicClass{
						// Create a new Location class
						"Location": {
							Properties: map[string]*testutil.DynamicProperty{
								"street": {Type: "string"},
								"city":   {Type: "string"},
							},
						},
						// DynamicOutput references it
						"DynamicOutput": {
							Properties: map[string]*testutil.DynamicProperty{
								"location": {Ref: "Location"},
							},
						},
					},
				},
			},
		}

		// Raw JSON that would be returned by an LLM
		rawResponse := `{"base_field": "test", "location": {"street": "123 Main St", "city": "Springfield"}}`

		resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
			Method:  "GetDynamic",
			Raw:     rawResponse,
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			BaseField         string         `json:"base_field"`
			DynamicProperties map[string]any `json:"DynamicProperties"`
		}
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.DynamicProperties == nil {
			t.Fatalf("Expected DynamicProperties to be present, got nil")
		}

		// Dynamic classes are unwrapped - fields are returned directly without Name/Fields wrapper
		location, ok := result.DynamicProperties["location"].(map[string]any)
		if !ok {
			t.Fatalf("Expected location to be a map, got %T: %v", result.DynamicProperties["location"], result.DynamicProperties["location"])
		}

		// Verify the unwrapped format - no Name/Fields wrapper
		if location["Name"] != nil || location["Fields"] != nil {
			t.Errorf("Dynamic class should be unwrapped, got internal format: %v", location)
		}

		if location["street"] != "123 Main St" {
			t.Errorf("Expected street '123 Main St', got %v", location["street"])
		}
		if location["city"] != "Springfield" {
			t.Errorf("Expected city 'Springfield', got %v", location["city"])
		}
	})
}
