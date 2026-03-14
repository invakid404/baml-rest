package main

import (
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

func TestNegotiateStreamFormatFromAccept(t *testing.T) {
	tests := []struct {
		name       string
		accept     string
		want       StreamFormat
		wantOK     bool
	}{
		{
			name:   "defaults to SSE when accept is empty",
			accept: "",
			want:   StreamFormatSSE,
			wantOK: true,
		},
		{
			name:   "selects NDJSON when it is only supported type",
			accept: ContentTypeNDJSON,
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
		{
			name:   "selects SSE when it is only supported type",
			accept: contentTypeSSE,
			want:   StreamFormatSSE,
			wantOK: true,
		},
		{
			name:   "prefers higher quality SSE over NDJSON",
			accept: ContentTypeNDJSON + ";q=0.5, " + contentTypeSSE + ";q=0.9",
			want:   StreamFormatSSE,
			wantOK: true,
		},
		{
			name:   "prefers higher quality NDJSON over SSE",
			accept: contentTypeSSE + ";q=0.5, " + ContentTypeNDJSON + ";q=0.9",
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
		{
			name:   "skips unacceptable NDJSON",
			accept: ContentTypeNDJSON + ";q=0, " + contentTypeSSE + ";q=0.1",
			want:   StreamFormatSSE,
			wantOK: true,
		},
		{
			name:   "keeps first supported type when quality ties",
			accept: ContentTypeNDJSON + ";q=0.8, " + contentTypeSSE + ";q=0.8",
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
		{
			name:   "ignores unsupported types",
			accept: "text/plain, " + ContentTypeNDJSON,
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
		{
			name:   "skips invalid q-value",
			accept: ContentTypeNDJSON + ";q=invalid, " + contentTypeSSE,
			want:   StreamFormatSSE,
			wantOK: true,
		},
		{
			name:   "skips out of range q-value",
			accept: ContentTypeNDJSON + ";q=1.5, " + contentTypeSSE,
			want:   StreamFormatSSE,
			wantOK: true,
		},
		{
			name:   "defaults to SSE for wildcard",
			accept: "*/*",
			want:   StreamFormatSSE,
			wantOK: true,
		},
		{
			name:   "maps application wildcard to NDJSON",
			accept: "application/*",
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
		{
			name:   "maps text wildcard to SSE",
			accept: "text/*",
			want:   StreamFormatSSE,
			wantOK: true,
		},
		{
			name:   "considers wildcard quality values",
			accept: "text/*;q=0.9, " + ContentTypeNDJSON + ";q=0.5",
			want:   StreamFormatSSE,
			wantOK: true,
		},
		{
			name:   "prefers exact match over lower quality wildcard",
			accept: "text/*;q=0.3, " + ContentTypeNDJSON + ";q=0.8",
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
		{
			name:   "skips unacceptable wildcard",
			accept: "application/*;q=0, text/event-stream;q=0.5",
			want:   StreamFormatSSE,
			wantOK: true,
		},
		// Specificity: exact match beats wildcard at same q
		{
			name:   "exact NDJSON beats wildcard at same quality",
			accept: "*/*;q=0.8, " + ContentTypeNDJSON + ";q=0.8",
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
		{
			name:   "exact SSE beats wildcard at same quality",
			accept: "*/*;q=0.8, " + contentTypeSSE + ";q=0.8",
			want:   StreamFormatSSE,
			wantOK: true,
		},
		// Most-specific range determines effective q
		{
			name:   "specific range overrides higher-q wildcard for SSE",
			accept: "text/*;q=0.9, " + contentTypeSSE + ";q=0.1, " + ContentTypeNDJSON + ";q=0.5",
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
		{
			name:   "type wildcard overridden by exact match at lower q",
			accept: "application/*;q=0.9, " + ContentTypeNDJSON + ";q=0.1, " + contentTypeSSE + ";q=0.5",
			want:   StreamFormatSSE,
			wantOK: true,
		},
		// q=0 rejection (406 Not Acceptable)
		{
			name:   "rejects when both formats explicitly q=0",
			accept: contentTypeSSE + ";q=0, " + ContentTypeNDJSON + ";q=0",
			wantOK: false,
		},
		{
			name:   "rejects when wildcard is q=0",
			accept: "*/*;q=0",
			wantOK: false,
		},
		{
			name:   "rejects when only unsupported types present",
			accept: "text/plain, application/json",
			wantOK: false,
		},
		{
			name:   "rejects when wildcard q=0 overrides nothing else",
			accept: "*/*;q=0, text/plain;q=1",
			wantOK: false,
		},
		// Mixed wildcard/specific with q=0 specificity override
		{
			name:   "specific q=0 overrides wildcard for one format",
			accept: "*/*;q=0.8, " + contentTypeSSE + ";q=0",
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
		{
			name:   "specific q=0 overrides type wildcard",
			accept: "text/*;q=0.8, " + contentTypeSSE + ";q=0, " + ContentTypeNDJSON + ";q=0.5",
			want:   StreamFormatNDJSON,
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NegotiateStreamFormatFromAccept(tt.accept)
			if ok != tt.wantOK {
				t.Fatalf("NegotiateStreamFormatFromAccept(%q) ok = %v, want %v", tt.accept, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("NegotiateStreamFormatFromAccept(%q) = %v, want %v", tt.accept, got, tt.want)
			}
		})
	}
}

// TestStreamHandler406 verifies that the stream handler pattern used in main.go
// returns HTTP 406 when Accept negotiation rejects all formats.
func TestStreamHandler406(t *testing.T) {
	// Mirror the handler pattern from makeStreamHandler in main.go.
	handler := func(c fiber.Ctx) error {
		format, ok := NegotiateStreamFormatFromAccept(c.Get(fiber.HeaderAccept))
		if !ok {
			return writeFiberJSONError(c, "Not Acceptable: server can produce text/event-stream or application/x-ndjson", fiber.StatusNotAcceptable)
		}

		// Return the chosen format name instead of starting a real stream.
		if format == StreamFormatNDJSON {
			return c.Status(fiber.StatusOK).SendString(ContentTypeNDJSON)
		}
		return c.Status(fiber.StatusOK).SendString(contentTypeSSE)
	}

	app := fiber.New()
	app.Post("/stream/test", handler)

	tests := []struct {
		name       string
		accept     string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "406 when both formats rejected",
			accept:     contentTypeSSE + ";q=0, " + ContentTypeNDJSON + ";q=0",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:       "406 when wildcard q=0",
			accept:     "*/*;q=0",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:       "200 SSE for default accept",
			accept:     "*/*",
			wantStatus: http.StatusOK,
			wantBody:   contentTypeSSE,
		},
		{
			name:       "200 NDJSON when exact match beats wildcard",
			accept:     "*/*;q=0.8, " + ContentTypeNDJSON + ";q=0.8",
			wantStatus: http.StatusOK,
			wantBody:   ContentTypeNDJSON,
		},
		{
			name:       "200 NDJSON when SSE specifically rejected",
			accept:     "*/*;q=0.8, " + contentTypeSSE + ";q=0",
			wantStatus: http.StatusOK,
			wantBody:   ContentTypeNDJSON,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "/stream/test", nil)
			req.Header.Set("Accept", tt.accept)

			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}

			if tt.wantBody != "" {
				buf := make([]byte, 256)
				n, _ := resp.Body.Read(buf)
				body := string(buf[:n])
				if body != tt.wantBody {
					t.Fatalf("body = %q, want %q", body, tt.wantBody)
				}
			}
		})
	}
}
