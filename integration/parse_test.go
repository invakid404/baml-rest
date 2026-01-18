//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/testutil"
)

func TestParseEndpoint(t *testing.T) {
	t.Run("valid_json", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
			Method: "GetPerson",
			Raw:    `{"name": "John", "age": 30, "tags": ["developer"]}`,
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			Name string   `json:"name"`
			Age  int      `json:"age"`
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.Name != "John" || result.Age != 30 {
			t.Errorf("Expected John (30), got %s (%d)", result.Name, result.Age)
		}
		if len(result.Tags) != 1 || result.Tags[0] != "developer" {
			t.Errorf("Expected tags ['developer'], got %v", result.Tags)
		}
	})

	t.Run("json_with_preamble", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
			Method: "GetSimple",
			Raw:    "Here is the result:\n{\"message\": \"extracted\"}",
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.Message != "extracted" {
			t.Errorf("Expected message 'extracted', got '%s'", result.Message)
		}
	})

	t.Run("nested_json", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
			Method: "GetPersonWithAddress",
			Raw: `{
				"name": "Jane",
				"address": {
					"street": "123 Main",
					"city": "Boston",
					"zip": "02101"
				}
			}`,
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result struct {
			Name    string `json:"name"`
			Address struct {
				City string `json:"city"`
			} `json:"address"`
		}
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if result.Name != "Jane" {
			t.Errorf("Expected name 'Jane', got '%s'", result.Name)
		}
		if result.Address.City != "Boston" {
			t.Errorf("Expected city 'Boston', got '%s'", result.Address.City)
		}
	})

	t.Run("array_parse", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
			Method: "GetPeople",
			Raw:    `[{"name": "A", "age": 1, "tags": []}, {"name": "B", "age": 2, "tags": []}]`,
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if len(result) != 2 {
			t.Errorf("Expected 2 items, got %d", len(result))
		}
	})
}
