package sseclient

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func collectEvents(t *testing.T, ctx context.Context, r io.Reader) ([]Event, error) {
	t.Helper()
	events, errc := Stream(ctx, r)
	var result []Event
	for ev := range events {
		result = append(result, ev)
	}
	return result, <-errc
}

func TestBasicSSEParsing(t *testing.T) {
	input := "data: hello world\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "hello world" {
		t.Errorf("expected data 'hello world', got %q", events[0].Data)
	}
	if events[0].Type != "" {
		t.Errorf("expected empty type, got %q", events[0].Type)
	}
}

func TestMultipleEvents(t *testing.T) {
	input := "data: first\n\ndata: second\n\ndata: third\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	expected := []string{"first", "second", "third"}
	for i, want := range expected {
		if events[i].Data != want {
			t.Errorf("event[%d]: expected data %q, got %q", i, want, events[i].Data)
		}
	}
}

func TestEventType(t *testing.T) {
	input := "event: content_block_delta\ndata: {\"type\":\"delta\"}\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "content_block_delta" {
		t.Errorf("expected type 'content_block_delta', got %q", events[0].Type)
	}
	if events[0].Data != `{"type":"delta"}` {
		t.Errorf("unexpected data: %q", events[0].Data)
	}
}

func TestEventID(t *testing.T) {
	input := "id: evt-123\ndata: hello\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ID != "evt-123" {
		t.Errorf("expected id 'evt-123', got %q", events[0].ID)
	}
}

func TestEventIDWithNull(t *testing.T) {
	// Per SSE spec, IDs containing null should be ignored
	input := "id: evt\x00bad\ndata: hello\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ID != "" {
		t.Errorf("expected empty id (null in id), got %q", events[0].ID)
	}
}

func TestMultipleDataLines(t *testing.T) {
	// Multiple data lines should be joined with newlines
	input := "data: line1\ndata: line2\ndata: line3\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "line1\nline2\nline3" {
		t.Errorf("expected joined data, got %q", events[0].Data)
	}
}

func TestCommentLines(t *testing.T) {
	// Lines starting with ':' are comments and should be ignored
	input := ": this is a comment\ndata: actual data\n: another comment\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "actual data" {
		t.Errorf("expected 'actual data', got %q", events[0].Data)
	}
}

func TestKeepaliveComments(t *testing.T) {
	// Keepalive-only input (no data events)
	input := ": keepalive\n\n: keepalive\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events from keepalives, got %d", len(events))
	}
}

func TestEmptyDataField(t *testing.T) {
	input := "data:\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "" {
		t.Errorf("expected empty data, got %q", events[0].Data)
	}
}

func TestDataWithoutSpace(t *testing.T) {
	// "data:value" (no space after colon) should work per SSE spec
	input := "data:no-space\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "no-space" {
		t.Errorf("expected 'no-space', got %q", events[0].Data)
	}
}

func TestDataWithExtraSpaces(t *testing.T) {
	// Only the first space after colon is stripped
	input := "data:  two spaces\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// First space stripped, second preserved
	if events[0].Data != " two spaces" {
		t.Errorf("expected ' two spaces', got %q", events[0].Data)
	}
}

func TestTrailingEventWithoutBlankLine(t *testing.T) {
	// Some providers close the connection without a final blank line
	input := "data: final"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event from trailing data, got %d", len(events))
	}
	if events[0].Data != "final" {
		t.Errorf("expected 'final', got %q", events[0].Data)
	}
}

func TestOpenAIFormat(t *testing.T) {
	// Real OpenAI SSE format
	input := `data: {"id":"chatcmpl-123","choices":[{"delta":{"content":"Hello"}}]}

data: {"id":"chatcmpl-123","choices":[{"delta":{"content":" world"}}]}

data: [DONE]

`

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[2].Data != "[DONE]" {
		t.Errorf("expected [DONE] sentinel, got %q", events[2].Data)
	}
}

func TestAnthropicFormat(t *testing.T) {
	// Real Anthropic SSE format with event types
	input := `event: message_start
data: {"type":"message_start","message":{"id":"msg-123"}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}

event: message_stop
data: {"type":"message_stop"}

`

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}
	if events[0].Type != "message_start" {
		t.Errorf("event[0]: expected type 'message_start', got %q", events[0].Type)
	}
	if events[1].Type != "content_block_delta" {
		t.Errorf("event[1]: expected type 'content_block_delta', got %q", events[1].Type)
	}
}

func TestContextCancellation(t *testing.T) {
	// Create a reader that blocks until closed.
	// In production, context cancellation closes the HTTP response body,
	// which unblocks the scanner. We simulate this by closing the pipe
	// writer when the context is cancelled.
	pr, pw := io.Pipe()
	defer pr.Close()

	ctx, cancel := context.WithCancel(context.Background())

	events, errc := Stream(ctx, pr)

	// Cancel and close the writer to unblock the scanner
	cancel()
	pw.Close()

	// Drain events
	for range events {
	}

	err := <-errc
	// Either context.Canceled (if context check fires first) or nil
	// (if pipe close causes clean EOF first) is acceptable.
	if err != nil && err != context.Canceled {
		t.Errorf("expected nil or context.Canceled, got %v", err)
	}
}

func TestContextDeadline(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	events, errc := Stream(ctx, pr)

	// Close writer after context deadline to unblock the scanner
	go func() {
		<-ctx.Done()
		pw.Close()
	}()

	for range events {
	}

	err := <-errc
	// Either deadline exceeded or nil (clean pipe close) is acceptable
	if err != nil && err != context.DeadlineExceeded {
		t.Errorf("expected nil or context.DeadlineExceeded, got %v", err)
	}
}

func TestEmptyInput(t *testing.T) {
	events, err := collectEvents(t, context.Background(), strings.NewReader(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events from empty input, got %d", len(events))
	}
}

func TestRetryFieldIgnored(t *testing.T) {
	input := "retry: 3000\ndata: hello\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "hello" {
		t.Errorf("expected 'hello', got %q", events[0].Data)
	}
}

func TestBlankLinesOnlyBetweenEvents(t *testing.T) {
	// Multiple blank lines between events should not produce empty events
	input := "data: first\n\n\n\ndata: second\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

func TestFieldWithNoColon(t *testing.T) {
	// A line with no colon is treated as a field name with empty value.
	// Since "somefield" is not a recognized SSE field, it's ignored.
	input := "someunknownfield\ndata: hello\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != "hello" {
		t.Errorf("expected 'hello', got %q", events[0].Data)
	}
}

func TestMixedFieldOrder(t *testing.T) {
	// Fields can appear in any order
	input := "id: 42\ndata: payload\nevent: custom\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "custom" {
		t.Errorf("expected type 'custom', got %q", events[0].Type)
	}
	if events[0].Data != "payload" {
		t.Errorf("expected data 'payload', got %q", events[0].Data)
	}
	if events[0].ID != "42" {
		t.Errorf("expected id '42', got %q", events[0].ID)
	}
}

func TestParseSSELine(t *testing.T) {
	tests := []struct {
		line  string
		field string
		value string
	}{
		{"data: hello", "data", "hello"},
		{"data:hello", "data", "hello"},
		{"data:", "data", ""},
		{"data:  extra", "data", " extra"},
		{"event: custom", "event", "custom"},
		{"id: 123", "id", "123"},
		{"nocolon", "nocolon", ""},
		{"data: colon:in:value", "data", "colon:in:value"},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			field, value := parseSSELine(tt.line)
			if field != tt.field {
				t.Errorf("field: expected %q, got %q", tt.field, field)
			}
			if value != tt.value {
				t.Errorf("value: expected %q, got %q", tt.value, value)
			}
		})
	}
}

func TestLargeJSONPayload(t *testing.T) {
	// Simulate a large JSON payload (common with function calling)
	largeJSON := `{"choices":[{"delta":{"content":"` + strings.Repeat("x", 100000) + `"}}]}`
	input := "data: " + largeJSON + "\n\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != largeJSON {
		t.Errorf("data mismatch for large payload")
	}
}

func TestCRLFLineEndings(t *testing.T) {
	// HTTP/1.1 servers may deliver SSE with \r\n line endings.
	// The parser must handle this correctly.
	input := "data: hello\r\n\r\ndata: world\r\n\r\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events with CRLF, got %d", len(events))
	}
	if events[0].Data != "hello" {
		t.Errorf("expected 'hello', got %q", events[0].Data)
	}
	if events[1].Data != "world" {
		t.Errorf("expected 'world', got %q", events[1].Data)
	}
}

func TestCRLFWithEventType(t *testing.T) {
	input := "event: content_block_delta\r\ndata: {\"text\":\"hi\"}\r\n\r\n"

	events, err := collectEvents(t, context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "content_block_delta" {
		t.Errorf("expected type 'content_block_delta', got %q", events[0].Type)
	}
	if events[0].Data != `{"text":"hi"}` {
		t.Errorf("expected data, got %q", events[0].Data)
	}
}
