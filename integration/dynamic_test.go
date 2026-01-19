//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
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
	})
}
