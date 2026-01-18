//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/invakid404/baml-rest/integration/testutil"
)

func TestStreamEndpoint(t *testing.T) {
	t.Run("multiple_sse_events", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"name": "John Doe", "age": 30, "tags": []}`
		opts := setupScenario(t, "test-stream-person", content)

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPerson",
			Input:   map[string]any{"description": "John, 30 years old"},
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

		// Should have received multiple events (chunked streaming)
		if eventCount < 2 {
			t.Errorf("Expected multiple events, got %d", eventCount)
		}

		// Last event should have the complete data
		var person struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}
		if err := json.Unmarshal(lastEvent.Data, &person); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if person.Name != "John Doe" || person.Age != 30 {
			t.Errorf("Expected John Doe (30), got %s (%d)", person.Name, person.Age)
		}
	})

	t.Run("streaming_array", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `[{"name": "Alice", "age": 25, "tags": []}, {"name": "Bob", "age": 35, "tags": []}]`
		opts := setupScenario(t, "test-stream-array", content)

		events, errs := BAMLClient.Stream(ctx, testutil.CallRequest{
			Method:  "GetPeople",
			Input:   map[string]any{"descriptions": "Alice and Bob"},
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

		// Verify final result is complete array
		var people []struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}
		if err := json.Unmarshal(lastEvent.Data, &people); err != nil {
			t.Fatalf("Failed to unmarshal last event: %v", err)
		}

		if len(people) != 2 {
			t.Errorf("Expected 2 people, got %d", len(people))
		}
	})
}

func TestStreamWithRawEndpoint(t *testing.T) {
	t.Run("raw_accumulates", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		content := `{"message": "hello world"}`
		opts := setupScenario(t, "test-stream-raw", content)

		events, errs := BAMLClient.StreamWithRaw(ctx, testutil.CallRequest{
			Method:  "GetSimple",
			Input:   map[string]any{"input": "test"},
			Options: opts,
		})

		var eventCount int
		var lastRaw string

		for {
			select {
			case event, ok := <-events:
				if !ok {
					goto done
				}
				eventCount++
				if event.Raw != "" {
					lastRaw = event.Raw
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

		// Final raw should match the complete content
		if lastRaw != content {
			t.Errorf("Expected final raw '%s', got '%s'", content, lastRaw)
		}
	})
}
