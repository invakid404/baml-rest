//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// TestRecursiveTypes tests self-referential BAML types (recursive via type aliases).
// These verify that the codegen cycle detection works correctly at runtime and that
// recursive structures serialize/deserialize through the full pipeline.
func TestRecursiveTypes(t *testing.T) {
	t.Run("tree_node_flat", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Flat tree: single node with no children
		content := `{"value": "root", "children": null}`
		opts := setupNonStreamingScenario(t, "test-tree-flat", content)

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "ParseTree",
			Input:   map[string]any{"input": "just a root"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result map[string]any
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		if result["value"] != "root" {
			t.Errorf("Expected value 'root', got %v", result["value"])
		}
	})

	t.Run("tree_node_nested", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Nested tree: root with two children, one of which has a child
		content := `{
			"value": "root",
			"children": [
				{"value": "child1", "children": null},
				{"value": "child2", "children": [
					{"value": "grandchild", "children": null}
				]}
			]
		}`
		opts := setupNonStreamingScenario(t, "test-tree-nested", content)

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "ParseTree",
			Input:   map[string]any{"input": "a nested tree"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result map[string]any
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		if result["value"] != "root" {
			t.Errorf("Expected root value 'root', got %v", result["value"])
		}

		children, ok := result["children"].([]any)
		if !ok || len(children) != 2 {
			t.Fatalf("Expected 2 children, got %v", result["children"])
		}

		child2, ok := children[1].(map[string]any)
		if !ok {
			t.Fatal("Expected child2 to be a map")
		}
		if child2["value"] != "child2" {
			t.Errorf("Expected child2 value 'child2', got %v", child2["value"])
		}

		grandchildren, ok := child2["children"].([]any)
		if !ok || len(grandchildren) != 1 {
			t.Fatalf("Expected 1 grandchild, got %v", child2["children"])
		}
	})

	t.Run("tree_node_deeply_nested", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Deep chain: root -> child -> grandchild -> great-grandchild
		content := `{
			"value": "level0",
			"children": [{
				"value": "level1",
				"children": [{
					"value": "level2",
					"children": [{
						"value": "level3",
						"children": null
					}]
				}]
			}]
		}`
		opts := setupNonStreamingScenario(t, "test-tree-deep", content)

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "ParseTree",
			Input:   map[string]any{"input": "a deep tree"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		// Walk down 4 levels to verify deep nesting works
		var result map[string]any
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		current := result
		for level := 0; level <= 3; level++ {
			expected := "level" + string(rune('0'+level))
			if current["value"] != expected {
				t.Errorf("Level %d: expected %q, got %v", level, expected, current["value"])
			}
			if level < 3 {
				children, ok := current["children"].([]any)
				if !ok || len(children) != 1 {
					t.Fatalf("Level %d: expected 1 child, got %v", level, current["children"])
				}
				current, ok = children[0].(map[string]any)
				if !ok {
					t.Fatalf("Level %d: expected child to be a map", level)
				}
			}
		}
	})

	t.Run("tree_node_stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{
			"value": "root",
			"children": [
				{"value": "child", "children": null}
			]
		}`
		opts := setupScenario(t, "test-tree-stream", content)

		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method:  "ParseTree",
			Input:   map[string]any{"input": "a tree"},
			Options: opts,
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

	t.Run("tree_node_parse", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
			Method: "ParseTree",
			Raw:    `{"value": "parsed", "children": [{"value": "child", "children": null}]}`,
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result map[string]any
		if err := json.Unmarshal(resp.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}
		if result["value"] != "parsed" {
			t.Errorf("Expected value 'parsed', got %v", result["value"])
		}
	})
}

// TestRecursiveTypesWithMedia tests self-referential BAML types that also contain
// media fields (image). This verifies that the codegen handles both recursion and
// media conversion in the same struct correctly.
func TestRecursiveTypesWithMedia(t *testing.T) {
	// Skip: BAML runtime panics on recursive types with media fields.
	// The Rust tokio worker thread panics in fmt.rs with "a formatting trait
	// implementation returned an error when the underlying stream did not" —
	// a stack overflow in BAML's Display impl for recursive types containing
	// media (e.g. MediaTreeNode with image? + self-referential children).
	// Recursive types WITHOUT media (TreeNode) work fine.
	t.Skip("BAML runtime bug: Rust panic on recursive types with media fields")

	t.Run("media_tree_no_images", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// MediaTreeNode without any images set
		content := `{
			"label": "root",
			"thumbnail": null,
			"children": [
				{"label": "child", "thumbnail": null, "children": null}
			]
		}`
		opts := setupNonStreamingScenario(t, "test-media-tree-no-img", content)

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "ParseMediaTree",
			Input:   map[string]any{"input": "tree without images"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result map[string]any
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}
		if result["label"] != "root" {
			t.Errorf("Expected label 'root', got %v", result["label"])
		}
	})

	t.Run("media_tree_nested", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Nested MediaTreeNode (output only - images in output are just returned)
		content := `{
			"label": "gallery",
			"thumbnail": null,
			"children": [
				{"label": "photo1", "thumbnail": null, "children": null},
				{"label": "photo2", "thumbnail": null, "children": [
					{"label": "detail", "thumbnail": null, "children": null}
				]}
			]
		}`
		opts := setupNonStreamingScenario(t, "test-media-tree-nested", content)

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "ParseMediaTree",
			Input:   map[string]any{"input": "nested media tree"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var result map[string]any
		if err := json.Unmarshal(resp.Body, &result); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}
		if result["label"] != "gallery" {
			t.Errorf("Expected label 'gallery', got %v", result["label"])
		}

		children, ok := result["children"].([]any)
		if !ok || len(children) != 2 {
			t.Fatalf("Expected 2 children, got %v", result["children"])
		}
	})

	t.Run("media_tree_parse", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
			Method: "ParseMediaTree",
			Raw:    `{"label": "root", "thumbnail": null, "children": [{"label": "leaf", "thumbnail": null, "children": null}]}`,
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}
	})

	t.Run("media_tree_stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"label": "root", "thumbnail": null, "children": null}`
		opts := setupScenario(t, "test-media-tree-stream", content)

		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method:  "ParseMediaTree",
			Input:   map[string]any{"input": "media tree"},
			Options: opts,
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

// TestRecursiveJsonType tests the deeply recursive JSON-like union type
// (JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>).
func TestRecursiveJsonType(t *testing.T) {
	t.Run("simple_string_value", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"data": "hello"}`
		opts := setupNonStreamingScenario(t, "test-json-string", content)

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "ParseJson",
			Input:   map[string]any{"input": "a string value"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}
	})

	t.Run("nested_object_and_array", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Deeply nested JSON structure
		content := `{"data": {"name": "test", "items": [1, "two", true, null, {"nested": "value"}]}}`
		opts := setupNonStreamingScenario(t, "test-json-nested", content)

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "ParseJson",
			Input:   map[string]any{"input": "nested JSON"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}
	})

	t.Run("parse_json_value", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := BAMLClient.Parse(ctx, testutil.ParseRequest{
			Method: "ParseJson",
			Raw:    `{"data": [1, "hello", {"key": [true, null]}]}`,
		})
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}
	})
}
