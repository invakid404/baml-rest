//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/testutil"
)

func TestMediaCallEndpoint(t *testing.T) {
	t.Run("image_from_url", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-media-img-url", "A photo of a cat sitting on a windowsill")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{
					"url": "https://example.com/cat.png",
				},
			},
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

		if result != "A photo of a cat sitting on a windowsill" {
			t.Errorf("Expected description, got '%s'", result)
		}
	})

	t.Run("image_from_url_with_media_type", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-media-img-url-mime", "A PNG image of a dog")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{
					"url":        "https://example.com/dog.png",
					"media_type": "image/png",
				},
			},
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

		if result != "A PNG image of a dog" {
			t.Errorf("Expected description, got '%s'", result)
		}
	})

	t.Run("image_from_base64", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-media-img-b64", "A base64-encoded image of a landscape")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{
					// Minimal valid PNG (1x1 transparent pixel)
					"base64":     "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
					"media_type": "image/png",
				},
			},
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

		if result != "A base64-encoded image of a landscape" {
			t.Errorf("Expected description, got '%s'", result)
		}
	})

	t.Run("image_with_text_param", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-media-img-prompt", "The image shows a sunset over the ocean")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "DescribeImageWithPrompt",
			Input: map[string]any{
				"img": map[string]any{
					"url": "https://example.com/sunset.jpg",
				},
				"prompt": "What do you see?",
			},
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

		if result != "The image shows a sunset over the ocean" {
			t.Errorf("Expected description, got '%s'", result)
		}
	})

	t.Run("audio_from_base64_wav", func(t *testing.T) {
		// Note: BAML pre-fetches audio URLs (unlike image URLs which are passed as references),
		// so we test audio via base64 with different mime types instead of a fake URL.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-media-audio-wav", "Hello, this is a test recording.")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "TranscribeAudio",
			Input: map[string]any{
				"audio": map[string]any{
					"base64":     "UklGRiQAAABXQVZFZm10IBAAAAABAAEAQB8AAIA+AAACABAAZGF0YQAAAAA=",
					"media_type": "audio/wav",
				},
			},
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

		if result != "Hello, this is a test recording." {
			t.Errorf("Expected transcription, got '%s'", result)
		}
	})

	t.Run("audio_from_base64", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-media-audio-b64", "Transcribed audio content")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "TranscribeAudio",
			Input: map[string]any{
				"audio": map[string]any{
					"base64":     "SGVsbG8gV29ybGQ=",
					"media_type": "audio/mp3",
				},
			},
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

		if result != "Transcribed audio content" {
			t.Errorf("Expected transcription, got '%s'", result)
		}
	})
}

func TestNestedMediaCallEndpoint(t *testing.T) {
	t.Run("class_with_image_field", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-nested-media-img", "A cat with the caption 'fluffy'")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "DescribeImageWithCaption",
			Input: map[string]any{
				"input": map[string]any{
					"img": map[string]any{
						"url": "https://example.com/cat.png",
					},
					"caption": "fluffy",
				},
			},
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

		if result != "A cat with the caption 'fluffy'" {
			t.Errorf("Expected description, got '%s'", result)
		}
	})

	t.Run("class_with_base64_image", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-nested-media-b64", "Image description from base64")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "DescribeImageWithCaption",
			Input: map[string]any{
				"input": map[string]any{
					"img": map[string]any{
						"base64":     "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
						"media_type": "image/png",
					},
					"caption": "test image",
				},
			},
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

		if result != "Image description from base64" {
			t.Errorf("Expected description, got '%s'", result)
		}
	})

	t.Run("class_with_optional_and_required_media", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-nested-media-doc", "Document processed successfully")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "ProcessDocument",
			Input: map[string]any{
				"bundle": map[string]any{
					"document": map[string]any{
						"base64":     "JVBERi0xLjQKMSAwIG9iago8PAovVHlwZSAvQ2F0YWxvZwo+PgplbmRvYmoK",
						"media_type": "application/pdf",
					},
					"title": "Test Document",
				},
			},
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

		if result != "Document processed successfully" {
			t.Errorf("Expected result, got '%s'", result)
		}
	})

	t.Run("class_with_optional_media_provided", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-nested-media-optional", "Document with cover processed")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "ProcessDocument",
			Input: map[string]any{
				"bundle": map[string]any{
					"cover": map[string]any{
						"url": "https://example.com/cover.png",
					},
					"document": map[string]any{
						"base64":     "JVBERi0xLjQKMSAwIG9iago8PAovVHlwZSAvQ2F0YWxvZwo+PgplbmRvYmoK",
						"media_type": "application/pdf",
					},
					"title": "Test Document With Cover",
				},
			},
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

		if result != "Document with cover processed" {
			t.Errorf("Expected result, got '%s'", result)
		}
	})
}

func TestNestedMediaStreamEndpoint(t *testing.T) {
	t.Run("class_with_image_stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := "Streaming description of the captioned image"
		opts := setupScenario(t, "test-nested-media-stream", content)

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method: "DescribeImageWithCaption",
			Input: map[string]any{
				"input": map[string]any{
					"img": map[string]any{
						"url": "https://example.com/stream-test.png",
					},
					"caption": "streaming test",
				},
			},
			Options: opts,
		})

		var eventCount int
		var lastEvent testutil.StreamEvent

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto doneNestedStream
				}
				eventCount++
				lastEvent = event
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	doneNestedStream:

		if eventCount < 2 {
			t.Errorf("Expected multiple events, got %d", eventCount)
		}

		var result string
		if err := json.Unmarshal(lastEvent.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if result != content {
			t.Errorf("Expected '%s', got '%s'", content, result)
		}
	})
}

func TestNestedMediaOpenAPISchema(t *testing.T) {
	t.Run("nested_media_endpoints_exist", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		schema, err := BAMLClient.GetOpenAPISchema(ctx)
		if err != nil {
			t.Fatalf("Failed to get OpenAPI schema: %v", err)
		}

		if schema.Paths["/call/DescribeImageWithCaption"] == nil {
			t.Error("Missing /call/DescribeImageWithCaption path")
		}
		if schema.Paths["/call/ProcessDocument"] == nil {
			t.Error("Missing /call/ProcessDocument path")
		}
		if schema.Paths["/stream/DescribeImageWithCaption"] == nil {
			t.Error("Missing /stream/DescribeImageWithCaption path")
		}
	})

	t.Run("nested_media_schema_has_media_input_refs", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		schema, err := BAMLClient.GetOpenAPISchema(ctx)
		if err != nil {
			t.Fatalf("Failed to get OpenAPI schema: %v", err)
		}

		// MediaInput should exist as a component schema (reused by nested types)
		_, ok := schema.Components.Schemas["MediaInput"]
		if !ok {
			t.Error("Missing MediaInput component schema")
		}
	})
}

func TestMediaCallWithRawEndpoint(t *testing.T) {
	t.Run("image_with_raw", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := "A beautiful mountain landscape"
		opts := setupNonStreamingScenario(t, "test-media-raw", content)

		resp, err := BAMLClient.CallWithRaw(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{
					"url": "https://example.com/mountain.jpg",
				},
			},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		var data string
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			t.Fatalf("Failed to unmarshal data: %v", err)
		}

		if data != content {
			t.Errorf("Expected '%s', got '%s'", content, data)
		}

		if resp.Raw != content {
			t.Errorf("Expected raw '%s', got '%s'", content, resp.Raw)
		}
	})
}

func TestMediaStreamEndpoint(t *testing.T) {
	t.Run("image_sse_stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := "A detailed description of the image showing a city skyline at night"
		opts := setupScenario(t, "test-media-stream-sse", content)

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{
					"url": "https://example.com/skyline.jpg",
				},
			},
			Options: opts,
		})

		var eventCount int
		var lastEvent testutil.StreamEvent

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++
				lastEvent = event
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done:

		if eventCount < 2 {
			t.Errorf("Expected multiple events, got %d", eventCount)
		}

		var result string
		if err := json.Unmarshal(lastEvent.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if result != content {
			t.Errorf("Expected '%s', got '%s'", content, result)
		}
	})

	t.Run("image_ndjson_stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := "A colorful abstract painting with geometric shapes and vibrant colors"
		opts := setupScenario(t, "test-media-stream-ndjson", content)

		events, errs := BAMLClient.StreamNDJSON(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{
					"url": "https://example.com/painting.jpg",
				},
			},
			Options: opts,
		})

		var eventCount int
		var lastEvent testutil.StreamEvent
		var sawFinal bool

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done2
				}
				eventCount++
				lastEvent = event
				if event.IsFinal() {
					sawFinal = true
				}
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done2:

		if eventCount < 2 {
			t.Errorf("Expected multiple events, got %d", eventCount)
		}

		if !sawFinal {
			t.Error("Expected a final event")
		}

		var result string
		if err := json.Unmarshal(lastEvent.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if result != content {
			t.Errorf("Expected '%s', got '%s'", content, result)
		}
	})

	t.Run("image_with_text_param_stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := "The image contains a red sports car parked in front of a building"
		opts := setupScenario(t, "test-media-stream-mixed", content)

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method: "DescribeImageWithPrompt",
			Input: map[string]any{
				"img": map[string]any{
					"url": "https://example.com/car.jpg",
				},
				"prompt": "Describe in detail",
			},
			Options: opts,
		})

		var eventCount int
		var lastEvent testutil.StreamEvent

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done3
				}
				eventCount++
				lastEvent = event
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done3:

		if eventCount < 2 {
			t.Errorf("Expected multiple events, got %d", eventCount)
		}

		var result string
		if err := json.Unmarshal(lastEvent.Data, &result); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if result != content {
			t.Errorf("Expected '%s', got '%s'", content, result)
		}
	})

	t.Run("stream_with_raw_image", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := "A photograph of a snowy mountain peak"
		opts := setupScenario(t, "test-media-stream-raw", content)

		events, errs := BAMLClient.StreamWithRaw(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{
					"url": "https://example.com/mountain.jpg",
				},
			},
			Options: opts,
		})

		var eventCount int
		var lastEvent testutil.StreamEvent

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done4
				}
				eventCount++
				lastEvent = event
			case err := <-errs:
				if err != nil {
					t.Fatalf("Stream error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("Context cancelled")
			}
		}
	done4:

		if eventCount < 2 {
			t.Errorf("Expected multiple events, got %d", eventCount)
		}

		// Final event should have raw content
		if lastEvent.Raw == "" {
			t.Error("Expected raw content in stream-with-raw events")
		}
	})
}

func TestMediaValidation(t *testing.T) {
	t.Run("missing_url_and_base64", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-media-invalid-empty", "should not reach here")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{},
			},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		// Should get a 500 (the conversion error happens server-side during BAML call)
		if resp.StatusCode == 200 {
			t.Fatalf("Expected error status, got 200")
		}
	})

	t.Run("both_url_and_base64", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-media-invalid-both", "should not reach here")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{
					"url":    "https://example.com/image.png",
					"base64": "iVBORw0KGgo=",
				},
			},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		// Should get a 500 (the conversion error happens server-side during BAML call)
		if resp.StatusCode == 200 {
			t.Fatalf("Expected error status, got 200")
		}
	})

	t.Run("missing_media_input_entirely", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		opts := setupNonStreamingScenario(t, "test-media-invalid-missing", "should not reach here")

		// Send request without the img field at all
		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method:  "DescribeImage",
			Input:   map[string]any{},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		// Should get an error (either 400 for missing field or 500 for nil media input)
		if resp.StatusCode == 200 {
			t.Fatalf("Expected error status, got 200")
		}
	})
}

func TestMediaOpenAPISchema(t *testing.T) {
	t.Run("schema_has_media_input_type", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		schema, err := BAMLClient.GetOpenAPISchema(ctx)
		if err != nil {
			t.Fatalf("Failed to get OpenAPI schema: %v", err)
		}

		mediaInput, ok := schema.Components.Schemas["MediaInput"].(map[string]any)
		if !ok {
			t.Fatal("Missing MediaInput schema in components")
		}

		// Should have oneOf with url and base64 variants
		oneOf, ok := mediaInput["oneOf"].([]any)
		if !ok {
			t.Fatal("MediaInput schema should have oneOf")
		}
		if len(oneOf) != 2 {
			t.Fatalf("Expected 2 oneOf variants, got %d", len(oneOf))
		}
	})

	t.Run("describe_image_endpoint_exists", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		schema, err := BAMLClient.GetOpenAPISchema(ctx)
		if err != nil {
			t.Fatalf("Failed to get OpenAPI schema: %v", err)
		}

		// Check /call/DescribeImage exists
		if schema.Paths["/call/DescribeImage"] == nil {
			t.Error("Missing /call/DescribeImage path")
		}

		// Check /stream/DescribeImage exists
		if schema.Paths["/stream/DescribeImage"] == nil {
			t.Error("Missing /stream/DescribeImage path")
		}

		// Check /call/DescribeImageWithPrompt exists
		if schema.Paths["/call/DescribeImageWithPrompt"] == nil {
			t.Error("Missing /call/DescribeImageWithPrompt path")
		}

		// Check /call/TranscribeAudio exists
		if schema.Paths["/call/TranscribeAudio"] == nil {
			t.Error("Missing /call/TranscribeAudio path")
		}
	})

	t.Run("media_response_validates_against_schema", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		validator, err := fetchAndParseSchema(ctx, TestEnv.BAMLRestURL)
		if err != nil {
			t.Fatalf("Failed to fetch schema: %v", err)
		}

		// Make an actual call and validate the response against the schema
		opts := setupNonStreamingScenario(t, "test-media-schema-validate", "A test image description")

		resp, err := BAMLClient.Call(ctx, testutil.CallRequest{
			Method: "DescribeImage",
			Input: map[string]any{
				"img": map[string]any{
					"url": "https://example.com/test.png",
				},
			},
			Options: opts,
		})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}

		if resp.StatusCode != 200 {
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, resp.Error)
		}

		// Validate response against OpenAPI schema
		if err := validator.validateResponse(ctx, "POST", "/call/DescribeImage", 200, "application/json", resp.Body); err != nil {
			t.Errorf("Response failed schema validation: %v", err)
		}
	})
}
