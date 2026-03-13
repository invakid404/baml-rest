package main

import (
	"testing"

	"github.com/goccy/go-json"
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
		name   string
		accept string
		want   StreamFormat
	}{
		{
			name:   "defaults to SSE when accept is empty",
			accept: "",
			want:   StreamFormatSSE,
		},
		{
			name:   "selects NDJSON when it is only supported type",
			accept: ContentTypeNDJSON,
			want:   StreamFormatNDJSON,
		},
		{
			name:   "selects SSE when it is only supported type",
			accept: contentTypeSSE,
			want:   StreamFormatSSE,
		},
		{
			name:   "prefers higher quality SSE over NDJSON",
			accept: ContentTypeNDJSON + ";q=0.5, " + contentTypeSSE + ";q=0.9",
			want:   StreamFormatSSE,
		},
		{
			name:   "prefers higher quality NDJSON over SSE",
			accept: contentTypeSSE + ";q=0.5, " + ContentTypeNDJSON + ";q=0.9",
			want:   StreamFormatNDJSON,
		},
		{
			name:   "skips unacceptable NDJSON",
			accept: ContentTypeNDJSON + ";q=0, " + contentTypeSSE + ";q=0.1",
			want:   StreamFormatSSE,
		},
		{
			name:   "keeps first supported type when quality ties",
			accept: ContentTypeNDJSON + ";q=0.8, " + contentTypeSSE + ";q=0.8",
			want:   StreamFormatNDJSON,
		},
		{
			name:   "ignores unsupported types",
			accept: "text/plain, " + ContentTypeNDJSON,
			want:   StreamFormatNDJSON,
		},
		{
			name:   "skips invalid q-value",
			accept: ContentTypeNDJSON + ";q=invalid, " + contentTypeSSE,
			want:   StreamFormatSSE,
		},
		{
			name:   "skips out of range q-value",
			accept: ContentTypeNDJSON + ";q=1.5, " + contentTypeSSE,
			want:   StreamFormatSSE,
		},
		{
			name:   "defaults to SSE for wildcard",
			accept: "*/*",
			want:   StreamFormatSSE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NegotiateStreamFormatFromAccept(tt.accept)
			if got != tt.want {
				t.Fatalf("NegotiateStreamFormatFromAccept(%q) = %v, want %v", tt.accept, got, tt.want)
			}
		})
	}
}
