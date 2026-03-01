//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/testutil"
)

func TestCallEndpoint(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		t.Run("simple_string_return", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			opts := setupNonStreamingScenario(t, "test-greeting", "Hello, World!")

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method:  "GetGreeting",
				Input:   map[string]any{"name": "World"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result string
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result != "Hello, World!" {
				t.Errorf("Expected 'Hello, World!', got '%s'", result)
			}
		})

		t.Run("simple_object_return", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			opts := setupNonStreamingScenario(t, "test-simple", `{"message": "test message"}`)

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method:  "GetSimple",
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
				Message string `json:"message"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.Message != "test message" {
				t.Errorf("Expected 'test message', got '%s'", result.Message)
			}
		})

		t.Run("complex_object", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			opts := setupNonStreamingScenario(t, "test-person", `{
			"name": "John Doe",
			"age": 30,
			"email": "john@example.com",
			"tags": ["developer", "gopher"]
		}`)

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": "John Doe, 30, developer"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				Name  string   `json:"name"`
				Age   int      `json:"age"`
				Email *string  `json:"email"`
				Tags  []string `json:"tags"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.Name != "John Doe" {
				t.Errorf("Expected name 'John Doe', got '%s'", result.Name)
			}
			if result.Age != 30 {
				t.Errorf("Expected age 30, got %d", result.Age)
			}
			if result.Email == nil || *result.Email != "john@example.com" {
				t.Errorf("Expected email 'john@example.com', got %v", result.Email)
			}
			if len(result.Tags) != 2 || result.Tags[0] != "developer" {
				t.Errorf("Expected tags ['developer', 'gopher'], got %v", result.Tags)
			}
		})

		t.Run("nested_object", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			opts := setupNonStreamingScenario(t, "test-person-address", `{
			"name": "Jane Doe",
			"address": {
				"street": "123 Main St",
				"city": "San Francisco",
				"zip": "94102"
			}
		}`)

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method:  "GetPersonWithAddress",
				Input:   map[string]any{"description": "Jane in SF"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				Name    string `json:"name"`
				Address struct {
					Street string `json:"street"`
					City   string `json:"city"`
					Zip    string `json:"zip"`
				} `json:"address"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.Name != "Jane Doe" {
				t.Errorf("Expected name 'Jane Doe', got '%s'", result.Name)
			}
			if result.Address.City != "San Francisco" {
				t.Errorf("Expected city 'San Francisco', got '%s'", result.Address.City)
			}
		})

		t.Run("array_return", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			opts := setupNonStreamingScenario(t, "test-people", `[
			{"name": "Alice", "age": 25, "tags": []},
			{"name": "Bob", "age": 35, "tags": ["manager"]}
		]`)

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method:  "GetPeople",
				Input:   map[string]any{"descriptions": "Alice (25) and Bob (35, manager)"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result []struct {
				Name string `json:"name"`
				Age  int    `json:"age"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if len(result) != 2 {
				t.Fatalf("Expected 2 people, got %d", len(result))
			}
			if result[0].Name != "Alice" || result[0].Age != 25 {
				t.Errorf("Expected first person Alice (25), got %s (%d)", result[0].Name, result[0].Age)
			}
			if result[1].Name != "Bob" || result[1].Age != 35 {
				t.Errorf("Expected second person Bob (35), got %s (%d)", result[1].Name, result[1].Age)
			}
		})

		t.Run("optional_field_present", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			opts := setupNonStreamingScenario(t, "test-person-email", `{
			"name": "John",
			"age": 25,
			"email": "john@test.com",
			"tags": []
		}`)

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": "John with email"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				Email *string `json:"email"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.Email == nil {
				t.Error("Expected email to be present")
			} else if *result.Email != "john@test.com" {
				t.Errorf("Expected email 'john@test.com', got '%s'", *result.Email)
			}
		})

		t.Run("optional_field_absent", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			opts := setupNonStreamingScenario(t, "test-person-no-email", `{
			"name": "Jane",
			"age": 30,
			"tags": []
		}`)

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method:  "GetPerson",
				Input:   map[string]any{"description": "Jane without email"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			var result struct {
				Email *string `json:"email"`
			}
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if result.Email != nil {
				t.Errorf("Expected email to be nil, got '%s'", *result.Email)
			}
		})

		t.Run("enum_return", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			opts := setupNonStreamingScenario(t, "test-category", `{
			"name": "iPhone",
			"category": "TECH"
		}`)

			resp, err := client.Call(ctx, testutil.CallRequest{
				Method:  "GetCategory",
				Input:   map[string]any{"item": "iPhone 15 Pro"},
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

			if result.Category != "TECH" {
				t.Errorf("Expected category 'TECH', got '%s'", result.Category)
			}
		})
	})
}

func TestCallWithRawEndpoint(t *testing.T) {
	forEachUnaryClient(t, func(t *testing.T, client *testutil.BAMLRestClient) {
		t.Run("basic_with_raw", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			content := `{"message": "test"}`
			opts := setupNonStreamingScenario(t, "test-raw-simple", content)

			resp, err := client.CallWithRaw(ctx, testutil.CallRequest{
				Method:  "GetSimple",
				Input:   map[string]any{"input": "test"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			// Check data is present
			var data struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(resp.Data, &data); err != nil {
				t.Fatalf("Failed to unmarshal data: %v", err)
			}
			if data.Message != "test" {
				t.Errorf("Expected message 'test', got '%s'", data.Message)
			}

			// Check raw is present and matches
			if resp.Raw != content {
				t.Errorf("Expected raw '%s', got '%s'", content, resp.Raw)
			}
		})

		t.Run("raw_includes_full_content", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Content with preamble that might be stripped from parsed output
			content := `Here's the JSON:
{"message": "extracted"}`
			opts := setupNonStreamingScenario(t, "test-raw-preamble", content)

			resp, err := client.CallWithRaw(ctx, testutil.CallRequest{
				Method:  "GetSimple",
				Input:   map[string]any{"input": "test"},
				Options: opts,
			})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			if resp.StatusCode != 200 {
				t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
			}

			// Data should have the parsed message
			var data struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(resp.Data, &data); err != nil {
				t.Fatalf("Failed to unmarshal data: %v", err)
			}
			if data.Message != "extracted" {
				t.Errorf("Expected message 'extracted', got '%s'", data.Message)
			}

			// Raw should include the full content
			if resp.Raw != content {
				t.Errorf("Expected raw to include preamble, got '%s'", resp.Raw)
			}
		})
	})
}
