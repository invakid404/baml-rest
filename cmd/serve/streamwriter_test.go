package main

import (
	"io"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/gofiber/fiber/v3"
)

func TestEncodeNDJSONEvent(t *testing.T) {
	event := &NDJSONEvent{
		Type: NDJSONEventData,
		Data: json.RawMessage(`{"value":1}`),
		Raw:  "raw-value",
	}

	got, err := encodeNDJSONEvent(event)
	if err != nil {
		t.Fatalf("encodeNDJSONEvent: %v", err)
	}

	want := "{\"type\":\"data\",\"data\":{\"value\":1},\"raw\":\"raw-value\"}\n"
	if string(got) != want {
		t.Fatalf("encoded NDJSON = %q, want %q", string(got), want)
	}
}

func TestFormatSSEEvent(t *testing.T) {
	got := formatSSEEvent(sseEventFinal, "first\nsecond")
	want := "event: final\ndata: first\ndata: second\n\n"
	if got != want {
		t.Fatalf("formatted SSE event = %q, want %q", got, want)
	}
}

func TestFormatSSEComment(t *testing.T) {
	got := formatSSEComment("keepalive")
	want := ": keepalive\n\n"
	if got != want {
		t.Fatalf("formatted SSE comment = %q, want %q", got, want)
	}
}

// TestStreamAcceptNegotiation tests the stream handler Accept negotiation
// using the same c.Accepts() pattern as main.go's makeStreamHandler.
func TestStreamAcceptNegotiation(t *testing.T) {
	// Mirror the handler pattern from makeStreamHandler in main.go.
	handler := func(c fiber.Ctx) error {
		switch c.Accepts(contentTypeSSE, ContentTypeNDJSON) {
		case ContentTypeNDJSON:
			return c.Status(fiber.StatusOK).SendString(ContentTypeNDJSON)
		case contentTypeSSE:
			return c.Status(fiber.StatusOK).SendString(contentTypeSSE)
		default:
			return writeFiberJSONError(c, "Not Acceptable: server can produce text/event-stream or application/x-ndjson", fiber.StatusNotAcceptable)
		}
	}

	app := fiber.New()
	app.Post("/stream/test", handler)

	tests := []struct {
		name       string
		accept     string
		wantStatus int
		wantBody   string
	}{
		// Basic selection
		{
			name:       "defaults to SSE when accept is empty",
			accept:     "",
			wantStatus: http.StatusOK,
			wantBody:   contentTypeSSE,
		},
		{
			name:       "selects NDJSON when it is only supported type",
			accept:     ContentTypeNDJSON,
			wantStatus: http.StatusOK,
			wantBody:   ContentTypeNDJSON,
		},
		{
			name:       "selects SSE when it is only supported type",
			accept:     contentTypeSSE,
			wantStatus: http.StatusOK,
			wantBody:   contentTypeSSE,
		},
		// Quality values
		{
			name:       "prefers higher quality SSE over NDJSON",
			accept:     ContentTypeNDJSON + ";q=0.5, " + contentTypeSSE + ";q=0.9",
			wantStatus: http.StatusOK,
			wantBody:   contentTypeSSE,
		},
		{
			name:       "prefers higher quality NDJSON over SSE",
			accept:     contentTypeSSE + ";q=0.5, " + ContentTypeNDJSON + ";q=0.9",
			wantStatus: http.StatusOK,
			wantBody:   ContentTypeNDJSON,
		},
		{
			name:       "skips q=0 NDJSON and picks SSE",
			accept:     ContentTypeNDJSON + ";q=0, " + contentTypeSSE + ";q=0.1",
			wantStatus: http.StatusOK,
			wantBody:   contentTypeSSE,
		},
		// Wildcards
		{
			name:       "wildcard defaults to SSE",
			accept:     "*/*",
			wantStatus: http.StatusOK,
			wantBody:   contentTypeSSE,
		},
		{
			name:       "application wildcard selects NDJSON",
			accept:     "application/*",
			wantStatus: http.StatusOK,
			wantBody:   ContentTypeNDJSON,
		},
		{
			name:       "text wildcard selects SSE",
			accept:     "text/*",
			wantStatus: http.StatusOK,
			wantBody:   contentTypeSSE,
		},
		{
			name:       "prefers exact match over lower quality wildcard",
			accept:     "text/*;q=0.3, " + ContentTypeNDJSON + ";q=0.8",
			wantStatus: http.StatusOK,
			wantBody:   ContentTypeNDJSON,
		},
		// Wildcard specificity: exact match beats wildcard at same q
		{
			name:       "exact NDJSON beats wildcard at same quality",
			accept:     "*/*;q=0.8, " + ContentTypeNDJSON + ";q=0.8",
			wantStatus: http.StatusOK,
			wantBody:   ContentTypeNDJSON,
		},
		{
			name:       "exact SSE beats wildcard at same quality",
			accept:     "*/*;q=0.8, " + contentTypeSSE + ";q=0.8",
			wantStatus: http.StatusOK,
			wantBody:   contentTypeSSE,
		},
		// q=0 with type-level wildcard
		{
			name:       "application wildcard q=0 rejects NDJSON, picks SSE",
			accept:     "application/*;q=0, " + contentTypeSSE + ";q=0.5",
			wantStatus: http.StatusOK,
			wantBody:   contentTypeSSE,
		},
		// 406 Not Acceptable
		{
			name:       "406 when both formats explicitly q=0",
			accept:     contentTypeSSE + ";q=0, " + ContentTypeNDJSON + ";q=0",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:       "406 when wildcard is q=0",
			accept:     "*/*;q=0",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:       "406 when only unsupported types present",
			accept:     "text/plain, application/json",
			wantStatus: http.StatusNotAcceptable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "/stream/test", nil)
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}

			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, tt.wantStatus, body)
			}

			if tt.wantBody != "" {
				body, _ := io.ReadAll(resp.Body)
				if string(body) != tt.wantBody {
					t.Fatalf("body = %q, want %q", string(body), tt.wantBody)
				}
			}
		})
	}
}
