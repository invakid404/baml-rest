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
