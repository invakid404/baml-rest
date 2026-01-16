package sse

import (
	"testing"
)

// mockChunk implements SSEChunk for testing
type mockChunk struct {
	text string
}

func (m mockChunk) Text() (string, error) { return m.text, nil }
func (m mockChunk) JSON() (any, error)    { return nil, nil }

func TestIncrementalExtractor_HappyPath(t *testing.T) {
	extractor := NewIncrementalExtractor()

	// First tick: 2 chunks
	chunks1 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"Hello"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":" world"}}]}`},
	}

	result1 := extractor.Extract(1, "openai", chunks1)
	if result1.Delta != "Hello world" {
		t.Errorf("expected delta 'Hello world', got %q", result1.Delta)
	}
	if result1.Full != "Hello world" {
		t.Errorf("expected full 'Hello world', got %q", result1.Full)
	}
	if result1.Reset {
		t.Error("expected Reset=false for first extraction (nothing to reset)")
	}

	// Second tick: same 2 chunks + 1 new chunk (incremental)
	chunks2 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"Hello"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":" world"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":"!"}}]}`},
	}

	result2 := extractor.Extract(1, "openai", chunks2)
	if result2.Delta != "!" {
		t.Errorf("expected delta '!', got %q", result2.Delta)
	}
	if result2.Full != "Hello world!" {
		t.Errorf("expected full 'Hello world!', got %q", result2.Full)
	}
	if result2.Reset {
		t.Error("expected Reset=false for incremental extraction")
	}

	// Third tick: no new chunks
	result3 := extractor.Extract(1, "openai", chunks2)
	if result3.Delta != "" {
		t.Errorf("expected empty delta, got %q", result3.Delta)
	}
	if result3.Full != "Hello world!" {
		t.Errorf("expected full 'Hello world!', got %q", result3.Full)
	}
	if result3.Reset {
		t.Error("expected Reset=false when no new chunks")
	}
}

func TestIncrementalExtractor_ResetOnRetry(t *testing.T) {
	extractor := NewIncrementalExtractor()

	// First tick: 1 call with content
	chunks1 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"First attempt partial"}}]}`},
	}

	result1 := extractor.Extract(1, "openai", chunks1)
	if result1.Reset {
		t.Error("expected Reset=false for first extraction")
	}
	if result1.Full != "First attempt partial" {
		t.Errorf("expected full 'First attempt partial', got %q", result1.Full)
	}

	// Second tick: call count increased (retry) - pass NEW call's chunks
	// The first call failed, so we're now on call 2 with fresh content
	chunks2 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"Retry success"}}]}`},
	}

	result2 := extractor.Extract(2, "openai", chunks2) // callCount=2 signals retry
	if !result2.Reset {
		t.Error("expected Reset=true when retry occurred (call count changed)")
	}
	// Should only contain content from the retry call
	if result2.Full != "Retry success" {
		t.Errorf("expected full 'Retry success', got %q", result2.Full)
	}

	// Third tick: more chunks on retry call - incremental, no reset
	chunks3 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"Retry success"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":"!"}}]}`},
	}

	result3 := extractor.Extract(2, "openai", chunks3)
	if result3.Reset {
		t.Error("expected Reset=false for incremental extraction after retry")
	}
	if result3.Delta != "!" {
		t.Errorf("expected delta '!', got %q", result3.Delta)
	}
	if result3.Full != "Retry success!" {
		t.Errorf("expected full 'Retry success!', got %q", result3.Full)
	}
}

func TestIncrementalExtractor_Full(t *testing.T) {
	extractor := NewIncrementalExtractor()

	// Before any extraction
	if got := extractor.Full(); got != "" {
		t.Errorf("expected empty Full() before extraction, got %q", got)
	}

	// After extraction
	chunks := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"test"}}]}`},
	}

	extractor.Extract(1, "openai", chunks)
	if got := extractor.Full(); got != "test" {
		t.Errorf("expected Full() 'test', got %q", got)
	}
}

func TestIncrementalExtractor_ResetWithEmptyDelta(t *testing.T) {
	extractor := NewIncrementalExtractor()

	// First tick: 1 call with content
	chunks1 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"Hello"}}]}`},
	}

	result1 := extractor.Extract(1, "openai", chunks1)
	if result1.Full != "Hello" {
		t.Errorf("expected full 'Hello', got %q", result1.Full)
	}

	// Second tick: retry occurred (callCount=2) but new call has 0 chunks yet
	// This is the critical case: Reset should be true even though Delta is empty
	emptyChunks := []SSEChunk{}

	result2 := extractor.Extract(2, "openai", emptyChunks)
	if !result2.Reset {
		t.Error("expected Reset=true when retry occurred, even with empty delta")
	}
	if result2.Delta != "" {
		t.Errorf("expected empty delta, got %q", result2.Delta)
	}
	if result2.Full != "" {
		t.Errorf("expected empty full (new call has no content yet), got %q", result2.Full)
	}

	// Third tick: new call gets content - Reset should now be false
	chunks3 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"World"}}]}`},
	}

	result3 := extractor.Extract(2, "openai", chunks3)
	if result3.Reset {
		t.Error("expected Reset=false for incremental update after retry")
	}
	if result3.Delta != "World" {
		t.Errorf("expected delta 'World', got %q", result3.Delta)
	}
	if result3.Full != "World" {
		t.Errorf("expected full 'World', got %q", result3.Full)
	}
}

func TestIncrementalExtractor_DifferentProviders(t *testing.T) {
	extractor := NewIncrementalExtractor()

	// First call with OpenAI
	chunks1 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"OpenAI"}}]}`},
	}

	result1 := extractor.Extract(1, "openai", chunks1)
	if result1.Full != "OpenAI" {
		t.Errorf("expected full 'OpenAI', got %q", result1.Full)
	}

	// Retry with Anthropic (different provider)
	chunks2 := []SSEChunk{
		mockChunk{`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Anthropic"}}`},
	}

	result2 := extractor.Extract(2, "anthropic", chunks2)
	if !result2.Reset {
		t.Error("expected Reset=true on retry")
	}
	if result2.Full != "Anthropic" {
		t.Errorf("expected full 'Anthropic', got %q", result2.Full)
	}
}

func TestGetCurrentContent_OnlyUsesLastCall(t *testing.T) {
	// GetCurrentContent should also only use the last call
	data := &StreamingData{
		Calls: []StreamingCall{
			{
				Provider: "openai",
				Chunks: []SSEChunk{
					mockChunk{`{"choices":[{"delta":{"content":"Failed"}}]}`},
				},
			},
			{
				Provider: "openai",
				Chunks: []SSEChunk{
					mockChunk{`{"choices":[{"delta":{"content":"Success"}}]}`},
				},
			},
		},
	}

	content, err := GetCurrentContent(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "Success" {
		t.Errorf("expected 'Success' (last call only), got %q", content)
	}
}

func TestIncrementalExtractor_ZeroCallCount(t *testing.T) {
	extractor := NewIncrementalExtractor()

	// callCount=0 should return empty result
	result := extractor.Extract(0, "openai", nil)
	if result.Delta != "" || result.Full != "" || result.Reset {
		t.Error("expected empty result for callCount=0")
	}
}

func TestIncrementalExtractor_ResetOnChunksDecreased(t *testing.T) {
	extractor := NewIncrementalExtractor()

	// First tick: 3 chunks
	chunks1 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"A"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":"B"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":"C"}}]}`},
	}

	result1 := extractor.Extract(1, "openai", chunks1)
	if result1.Full != "ABC" {
		t.Errorf("expected full 'ABC', got %q", result1.Full)
	}
	if result1.Reset {
		t.Error("expected Reset=false for first extraction")
	}

	// Second tick: only 2 chunks (decreased - could happen due to log truncation)
	// Same callCount, but chunks decreased - should trigger reset
	chunks2 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"X"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":"Y"}}]}`},
	}

	result2 := extractor.Extract(1, "openai", chunks2)
	if !result2.Reset {
		t.Error("expected Reset=true when chunks decreased (client state is invalid)")
	}
	if result2.Full != "XY" {
		t.Errorf("expected full 'XY', got %q", result2.Full)
	}
	// Delta should be the full content since we rebuilt
	if result2.Delta != "XY" {
		t.Errorf("expected delta 'XY' (full rebuild), got %q", result2.Delta)
	}
}
