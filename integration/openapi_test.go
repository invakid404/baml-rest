//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/invakid404/baml-rest/integration/testutil"
)

// schemaValidator holds the OpenAPI schema and router for validation.
type schemaValidator struct {
	doc    *openapi3.T
	router routers.Router
}

// fetchAndParseSchema fetches the OpenAPI schema from the server and prepares it for validation.
func fetchAndParseSchema(ctx context.Context, baseURL string) (*schemaValidator, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/openapi.json", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch schema: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read schema: %w", err)
	}

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse schema: %w", err)
	}

	// Validate the schema itself
	if err := doc.Validate(ctx); err != nil {
		return nil, fmt.Errorf("schema validation failed: %w", err)
	}

	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("failed to create router: %w", err)
	}

	return &schemaValidator{doc: doc, router: router}, nil
}

// validateResponse validates an HTTP response against the OpenAPI schema.
func (v *schemaValidator) validateResponse(
	ctx context.Context,
	method, path string,
	statusCode int,
	contentType string,
	body []byte,
) error {
	// Parse the URL
	parsedURL, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("failed to parse path: %w", err)
	}

	// Find the route
	route, pathParams, err := v.router.FindRoute(&http.Request{
		Method: method,
		URL:    parsedURL,
	})
	if err != nil {
		return fmt.Errorf("failed to find route for %s %s: %w", method, path, err)
	}

	// Create request validation input (needed for response validation)
	requestValidationInput := &openapi3filter.RequestValidationInput{
		Request: &http.Request{
			Method: method,
			URL:    parsedURL,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
		},
		PathParams: pathParams,
		Route:      route,
	}

	// Create response validation input
	responseValidationInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: requestValidationInput,
		Status:                 statusCode,
		Header: http.Header{
			"Content-Type": []string{contentType},
		},
		Body: io.NopCloser(bytes.NewReader(body)),
		Options: &openapi3filter.Options{
			IncludeResponseStatus: true,
		},
	}

	// Validate the response
	if err := openapi3filter.ValidateResponse(ctx, responseValidationInput); err != nil {
		return fmt.Errorf("response validation failed: %w", err)
	}

	return nil
}

// validateNDJSONEvent validates a single NDJSON event against the streaming schema.
// Uses direct schema validation since kin-openapi's ValidateResponse doesn't support NDJSON content type.
func (v *schemaValidator) validateNDJSONEvent(
	ctx context.Context,
	path string,
	eventJSON []byte,
) error {
	// Find the path in the schema
	pathItem := v.doc.Paths.Find(path)
	if pathItem == nil || pathItem.Post == nil {
		return fmt.Errorf("path %s not found in schema", path)
	}

	// Get the 200 response
	resp := pathItem.Post.Responses.Status(200)
	if resp == nil || resp.Value == nil {
		return fmt.Errorf("no 200 response defined for %s", path)
	}

	// Get the NDJSON content type schema
	ndjsonContent := resp.Value.Content.Get("application/x-ndjson")
	if ndjsonContent == nil || ndjsonContent.Schema == nil {
		return fmt.Errorf("no application/x-ndjson schema defined for %s", path)
	}

	// Parse the event JSON
	var eventData any
	if err := json.Unmarshal(eventJSON, &eventData); err != nil {
		return fmt.Errorf("failed to parse event JSON: %w", err)
	}

	// Validate against the schema
	if err := ndjsonContent.Schema.Value.VisitJSON(eventData); err != nil {
		return fmt.Errorf("schema validation failed: %w", err)
	}

	return nil
}

// getEventTypeSchema extracts the schema for a specific event type from the oneOf discriminated union.
// eventType should be "data" for partial events, "final" for final events, etc.
func (v *schemaValidator) getEventTypeSchema(path string, eventType string) (*openapi3.Schema, error) {
	pathItem := v.doc.Paths.Find(path)
	if pathItem == nil || pathItem.Post == nil {
		return nil, fmt.Errorf("path %s not found in schema", path)
	}

	resp := pathItem.Post.Responses.Status(200)
	if resp == nil || resp.Value == nil {
		return nil, fmt.Errorf("no 200 response defined for %s", path)
	}

	ndjsonContent := resp.Value.Content.Get("application/x-ndjson")
	if ndjsonContent == nil || ndjsonContent.Schema == nil || ndjsonContent.Schema.Value == nil {
		return nil, fmt.Errorf("no application/x-ndjson schema defined for %s", path)
	}

	// Find the variant with the matching type enum
	for _, variant := range ndjsonContent.Schema.Value.OneOf {
		if variant.Value == nil || variant.Value.Properties == nil {
			continue
		}
		typeSchema := variant.Value.Properties["type"]
		if typeSchema == nil || typeSchema.Value == nil {
			continue
		}
		if len(typeSchema.Value.Enum) > 0 && typeSchema.Value.Enum[0] == eventType {
			return variant.Value, nil
		}
	}

	return nil, fmt.Errorf("no schema found for event type %q in %s", eventType, path)
}

// validateAgainstEventTypeSchema validates data against a specific event type's schema.
func (v *schemaValidator) validateAgainstEventTypeSchema(path string, eventType string, data any) error {
	schema, err := v.getEventTypeSchema(path, eventType)
	if err != nil {
		return err
	}

	if err := schema.VisitJSON(data); err != nil {
		return fmt.Errorf("validation against %q schema failed: %w", eventType, err)
	}

	return nil
}

func TestOpenAPISchemaValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Fetch and parse the OpenAPI schema from the running server
	validator, err := fetchAndParseSchema(ctx, TestEnv.BAMLRestURL)
	if err != nil {
		t.Fatalf("Failed to fetch/parse OpenAPI schema: %v", err)
	}

	// Comprehensive test content that exercises multiple BAML features
	comprehensiveContent := `{
		"id": 42,
		"title": "Test Item",
		"description": "A test item with all features",
		"score": 3.14,
		"is_active": true,
		"metadata": {
			"created_by": "test-user",
			"priority": "HIGH",
			"tags": [
				{"name": "important", "value": "yes"},
				{"name": "category", "value": null}
			]
		},
		"related_ids": [1, 2, 3],
		"labels": ["alpha", "beta"]
	}`

	t.Run("call_endpoint", func(t *testing.T) {
		opts := setupNonStreamingScenario(t, "schema-test-call", comprehensiveContent)

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "GetComprehensive",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		// Validate response against schema
		err = validator.validateResponse(
			ctx, "POST", "/call/GetComprehensive",
			resp.StatusCode, "application/json", resp.Body,
		)
		if err != nil {
			t.Errorf("Schema validation failed for /call: %v", err)
		}
	})

	t.Run("call_with_raw_endpoint", func(t *testing.T) {
		opts := setupNonStreamingScenario(t, "schema-test-call-raw", comprehensiveContent)

		resp, err := BAMLClient.CallWithRaw(ctx, testutil.CallRequest{
			Method:  "GetComprehensive",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("CallWithRaw failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		// Reconstruct the full response body for validation
		fullResponse, _ := json.Marshal(map[string]any{
			"data": json.RawMessage(resp.Data),
			"raw":  resp.Raw,
		})

		err = validator.validateResponse(
			ctx, "POST", "/call-with-raw/GetComprehensive",
			resp.StatusCode, "application/json", fullResponse,
		)
		if err != nil {
			t.Errorf("Schema validation failed for /call-with-raw: %v", err)
		}
	})

	t.Run("stream_ndjson_endpoint", func(t *testing.T) {
		opts := setupScenario(t, "schema-test-stream", comprehensiveContent)

		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method:  "GetComprehensive",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})

		var eventCount int
		var partialCount int
		var finalEvent []byte

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++

				// Validate and track partial data events (type: "data")
				if event.IsPartialData() {
					partialCount++
					// Validate partial event against stream type schema
					partialJSON, _ := json.Marshal(map[string]any{
						"type": "data",
						"data": json.RawMessage(event.Data),
					})
					if err := validator.validateNDJSONEvent(ctx, "/stream/GetComprehensive", partialJSON); err != nil {
						t.Errorf("Partial event %d schema validation failed: %v", partialCount, err)
					}
				}

				// Capture the final event (type: "final") for validation
				if event.IsFinal() {
					// Reconstruct the NDJSON event for validation
					eventJSON, _ := json.Marshal(map[string]any{
						"type": "final",
						"data": json.RawMessage(event.Data),
					})
					finalEvent = eventJSON
				}

			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		if eventCount == 0 {
			t.Error("No events received")
		}

		if partialCount == 0 {
			t.Log("Warning: No partial events received (expected for short content)")
		}

		// Validate the final event against the strict schema
		if finalEvent == nil {
			t.Fatal("No final event received")
		}

		err := validator.validateNDJSONEvent(ctx, "/stream/GetComprehensive", finalEvent)
		if err != nil {
			t.Errorf("Final event schema validation failed: %v", err)
		}
	})

	t.Run("stream_with_raw_ndjson_endpoint", func(t *testing.T) {
		opts := setupScenario(t, "schema-test-stream-raw", comprehensiveContent)

		events, errs := BAMLClient.StreamWithRawNDJSON(ctx, testutil.CallRequest{
			Method:  "GetComprehensive",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})

		var eventCount int
		var partialCount int
		var finalEvent []byte
		var finalRaw string

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++

				// Validate and track partial data events (type: "data")
				if event.IsPartialData() {
					partialCount++
					// Validate partial event against stream type schema
					partialJSON, _ := json.Marshal(map[string]any{
						"type": "data",
						"data": json.RawMessage(event.Data),
						"raw":  event.Raw,
					})
					if err := validator.validateNDJSONEvent(ctx, "/stream-with-raw/GetComprehensive", partialJSON); err != nil {
						t.Errorf("Partial event %d schema validation failed: %v", partialCount, err)
					}
				}

				// Capture the final event (type: "final") for validation
				if event.IsFinal() {
					eventJSON, _ := json.Marshal(map[string]any{
						"type": "final",
						"data": json.RawMessage(event.Data),
						"raw":  event.Raw,
					})
					finalEvent = eventJSON
					finalRaw = event.Raw
				}

			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		if eventCount == 0 {
			t.Error("No events received")
		}

		if partialCount == 0 {
			t.Log("Warning: No partial events received (expected for short content)")
		}

		// Validate the final event against the strict schema
		if finalEvent == nil {
			t.Fatal("No final event received")
		}

		err := validator.validateNDJSONEvent(ctx, "/stream-with-raw/GetComprehensive", finalEvent)
		if err != nil {
			t.Errorf("Final event schema validation failed: %v", err)
		}

		// Verify that raw field was present in the final event
		if finalRaw == "" {
			t.Error("Expected raw field in final event")
		}
	})
}

// TestStreamAndFinalSchemasDiffer verifies that partial (data) and final schemas are meaningfully different.
// This ensures that partial events with null fields fail final schema validation, proving the schemas
// aren't accidentally identical.
func TestStreamAndFinalSchemasDiffer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	validator, err := fetchAndParseSchema(ctx, TestEnv.BAMLRestURL)
	if err != nil {
		t.Fatalf("Failed to fetch/parse OpenAPI schema: %v", err)
	}

	// Use content that will generate partial events with null fields during streaming
	comprehensiveContent := `{
		"id": 42,
		"title": "Test Item",
		"description": "A test item with all features",
		"score": 3.14,
		"is_active": true,
		"metadata": {
			"created_by": "test-user",
			"priority": "HIGH",
			"tags": [
				{"name": "important", "value": "yes"},
				{"name": "category", "value": null}
			]
		},
		"related_ids": [1, 2, 3],
		"labels": ["alpha", "beta"]
	}`

	t.Run("partial_data_fails_final_schema", func(t *testing.T) {
		opts := setupScenario(t, "schema-differ-test", comprehensiveContent)

		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method:  "GetComprehensive",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})

		var firstPartialData []byte
		var finalData []byte

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}

				// Capture the first partial event
				if event.IsPartialData() && firstPartialData == nil {
					firstPartialData = []byte(event.Data)
				}

				// Capture the final event
				if event.IsFinal() {
					finalData = []byte(event.Data)
				}

			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		if firstPartialData == nil {
			t.Skip("No partial events received - cannot test schema difference")
		}
		if finalData == nil {
			t.Fatal("No final event received")
		}

		// Parse the partial data
		var partialParsed any
		if err := json.Unmarshal(firstPartialData, &partialParsed); err != nil {
			t.Fatalf("Failed to parse partial data: %v", err)
		}

		// Try to validate the first partial data against the final schema
		// This should FAIL because partial data may have null fields that the final schema requires
		partialAsFinal := map[string]any{
			"type": "final",
			"data": partialParsed,
		}

		err := validator.validateAgainstEventTypeSchema("/stream/GetComprehensive", "final", partialAsFinal)
		if err == nil {
			t.Error("Expected partial data to FAIL validation against final schema, but it passed. " +
				"This suggests the partial and final schemas might be identical.")
		} else {
			t.Logf("Correctly rejected partial data against final schema: %v", err)
		}

		// Verify the final data passes the final schema (sanity check)
		var finalParsed any
		if err := json.Unmarshal(finalData, &finalParsed); err != nil {
			t.Fatalf("Failed to parse final data: %v", err)
		}

		finalAsFinal := map[string]any{
			"type": "final",
			"data": finalParsed,
		}

		err = validator.validateAgainstEventTypeSchema("/stream/GetComprehensive", "final", finalAsFinal)
		if err != nil {
			t.Errorf("Final data should pass final schema validation: %v", err)
		}
	})

	t.Run("final_data_passes_partial_schema", func(t *testing.T) {
		opts := setupScenario(t, "schema-differ-test-2", comprehensiveContent)

		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method:  "GetComprehensive",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})

		var finalData []byte

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				if event.IsFinal() {
					finalData = []byte(event.Data)
				}

			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		if finalData == nil {
			t.Fatal("No final event received")
		}

		// Parse the final data
		var finalParsed any
		if err := json.Unmarshal(finalData, &finalParsed); err != nil {
			t.Fatalf("Failed to parse final data: %v", err)
		}

		// Final data should pass the partial schema (partial schema is more permissive)
		finalAsPartial := map[string]any{
			"type": "data",
			"data": finalParsed,
		}

		err := validator.validateAgainstEventTypeSchema("/stream/GetComprehensive", "data", finalAsPartial)
		if err != nil {
			t.Errorf("Final data should pass partial (data) schema validation since it's more permissive: %v", err)
		}
	})
}

// TestOpenAPISchemaStructure validates the schema itself has expected structure.
func TestOpenAPISchemaStructure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	validator, err := fetchAndParseSchema(ctx, TestEnv.BAMLRestURL)
	if err != nil {
		t.Fatalf("Failed to fetch/parse OpenAPI schema: %v", err)
	}

	t.Run("has_streaming_endpoints", func(t *testing.T) {
		// Check that streaming endpoints exist
		streamPath := validator.doc.Paths.Find("/stream/GetComprehensive")
		if streamPath == nil {
			t.Error("Missing /stream/GetComprehensive path")
		} else if streamPath.Post == nil {
			t.Error("Missing POST operation for /stream/GetComprehensive")
		}

		streamWithRawPath := validator.doc.Paths.Find("/stream-with-raw/GetComprehensive")
		if streamWithRawPath == nil {
			t.Error("Missing /stream-with-raw/GetComprehensive path")
		} else if streamWithRawPath.Post == nil {
			t.Error("Missing POST operation for /stream-with-raw/GetComprehensive")
		}
	})

	t.Run("has_ndjson_content_type", func(t *testing.T) {
		streamPath := validator.doc.Paths.Find("/stream/GetComprehensive")
		if streamPath == nil || streamPath.Post == nil {
			t.Skip("Stream path not found")
		}

		resp := streamPath.Post.Responses.Status(200)
		if resp == nil {
			t.Fatal("Missing 200 response")
		}

		ndjsonContent := resp.Value.Content.Get("application/x-ndjson")
		if ndjsonContent == nil {
			t.Error("Missing application/x-ndjson content type")
		}

		sseContent := resp.Value.Content.Get("text/event-stream")
		if sseContent == nil {
			t.Error("Missing text/event-stream content type")
		}
	})

	t.Run("has_discriminated_union", func(t *testing.T) {
		streamPath := validator.doc.Paths.Find("/stream/GetComprehensive")
		if streamPath == nil || streamPath.Post == nil {
			t.Skip("Stream path not found")
		}

		resp := streamPath.Post.Responses.Status(200)
		if resp == nil {
			t.Fatal("Missing 200 response")
		}

		ndjsonContent := resp.Value.Content.Get("application/x-ndjson")
		if ndjsonContent == nil || ndjsonContent.Schema == nil {
			t.Fatal("Missing NDJSON schema")
		}

		schema := ndjsonContent.Schema.Value
		if schema == nil {
			t.Fatal("Schema value is nil")
		}

		// Should have 4 variants: partial data, final data, reset, error
		if len(schema.OneOf) != 4 {
			t.Errorf("Expected 4 oneOf variants (partial, final, reset, error), got %d", len(schema.OneOf))
		}

		if schema.Discriminator == nil {
			t.Error("Expected discriminator")
		} else if schema.Discriminator.PropertyName != "type" {
			t.Errorf("Expected discriminator on 'type', got '%s'", schema.Discriminator.PropertyName)
		}

		// Verify we have separate partial ("data") and final ("final") event types
		eventTypes := make(map[string]bool)
		for _, variant := range schema.OneOf {
			if variant.Value == nil || variant.Value.Properties == nil {
				continue
			}
			typeSchema := variant.Value.Properties["type"]
			if typeSchema == nil || typeSchema.Value == nil {
				continue
			}
			if len(typeSchema.Value.Enum) > 0 {
				eventTypes[typeSchema.Value.Enum[0].(string)] = true
			}
		}

		if !eventTypes["data"] {
			t.Error("Missing 'data' event type for partial events")
		}
		if !eventTypes["final"] {
			t.Error("Missing 'final' event type for final events")
		}
		if !eventTypes["reset"] {
			t.Error("Missing 'reset' event type")
		}
		if !eventTypes["error"] {
			t.Error("Missing 'error' event type")
		}
	})

	t.Run("has_global_event_schemas", func(t *testing.T) {
		resetSchema := validator.doc.Components.Schemas["__StreamResetEvent__"]
		if resetSchema == nil {
			t.Error("Missing __StreamResetEvent__ component schema")
		}

		errorSchema := validator.doc.Components.Schemas["__StreamErrorEvent__"]
		if errorSchema == nil {
			t.Error("Missing __StreamErrorEvent__ component schema")
		}
	})
}
