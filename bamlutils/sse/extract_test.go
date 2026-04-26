package sse

import (
	"strings"
	"testing"
)

// mockChunk implements SSEChunk for testing
type mockChunk struct {
	text string
}

func (m mockChunk) Text() (string, error) { return m.text, nil }
func (m mockChunk) JSON() (any, error)    { return nil, nil }

func TestIncrementalExtractor_HappyPath(t *testing.T) {
	extractor := NewIncrementalExtractor(false)

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
	extractor := NewIncrementalExtractor(false)

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
	extractor := NewIncrementalExtractor(false)

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
	extractor := NewIncrementalExtractor(false)

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
	extractor := NewIncrementalExtractor(false)

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

// TestExtractDeltaPartsFromText_AnthropicThinking_Default verifies that with
// includeThinking=false (the default, BAML-aligned behavior), Anthropic
// thinking_delta events contribute nothing to either Parseable or Raw, while
// text_delta events behave normally.
func TestExtractDeltaPartsFromText_AnthropicThinking_Default(t *testing.T) {
	textDelta, err := ExtractDeltaPartsFromText("anthropic", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"Answer"}}`, false)
	if err != nil {
		t.Fatalf("text delta extraction failed: %v", err)
	}
	if textDelta.Parseable != "Answer" || textDelta.Raw != "Answer" {
		t.Fatalf("unexpected text delta: %+v", textDelta)
	}

	thinkingDelta, err := ExtractDeltaPartsFromText("anthropic", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Reasoning"}}`, false)
	if err != nil {
		t.Fatalf("thinking delta extraction failed: %v", err)
	}
	if thinkingDelta.Parseable != "" || thinkingDelta.Raw != "" {
		t.Fatalf("expected thinking delta to be dropped under default flag, got: %+v", thinkingDelta)
	}
}

// TestExtractDeltaPartsFromText_AnthropicThinking_OptIn verifies that with
// includeThinking=true, Anthropic thinking_delta events accumulate into Raw
// while Parseable stays empty (so the BAML parser is never fed thinking
// content). text_delta behavior is unchanged.
func TestExtractDeltaPartsFromText_AnthropicThinking_OptIn(t *testing.T) {
	textDelta, err := ExtractDeltaPartsFromText("anthropic", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"Answer"}}`, true)
	if err != nil {
		t.Fatalf("text delta extraction failed: %v", err)
	}
	if textDelta.Parseable != "Answer" || textDelta.Raw != "Answer" {
		t.Fatalf("unexpected text delta: %+v", textDelta)
	}

	thinkingDelta, err := ExtractDeltaPartsFromText("anthropic", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Reasoning"}}`, true)
	if err != nil {
		t.Fatalf("thinking delta extraction failed: %v", err)
	}
	if thinkingDelta.Parseable != "" || thinkingDelta.Raw != "Reasoning" {
		t.Fatalf("unexpected thinking delta under opt-in: %+v", thinkingDelta)
	}
}

// TestExtractDeltaPartsFromText_AnthropicThinking_ParseableInvariant codifies
// the structural guarantee that Parseable never includes thinking content,
// regardless of the includeThinking flag value. Any future refactor that
// breaks this invariant will fail this test.
func TestExtractDeltaPartsFromText_AnthropicThinking_ParseableInvariant(t *testing.T) {
	chunks := []string{
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Answer"}}`,
		`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Reasoning"}}`,
	}
	for _, chunk := range chunks {
		off, err := ExtractDeltaPartsFromText("anthropic", chunk, false)
		if err != nil {
			t.Fatalf("flag=false extraction failed: %v", err)
		}
		on, err := ExtractDeltaPartsFromText("anthropic", chunk, true)
		if err != nil {
			t.Fatalf("flag=true extraction failed: %v", err)
		}
		if off.Parseable != on.Parseable {
			t.Errorf("Parseable diverged across flag values for chunk %q: off=%q on=%q",
				chunk, off.Parseable, on.Parseable)
		}
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

	content, err := GetCurrentContent(data, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "Success" {
		t.Errorf("expected 'Success' (last call only), got %q", content)
	}
}

func TestIncrementalExtractor_ZeroCallCount(t *testing.T) {
	extractor := NewIncrementalExtractor(false)

	// callCount=0 should return empty result
	result := extractor.Extract(0, "openai", nil)
	if result.Delta != "" || result.Full != "" || result.Reset {
		t.Error("expected empty result for callCount=0")
	}
}

// concreteChunk is a distinct type (not mockChunk) to test ExtractFrom with
// a concrete non-interface slice, verifying the generic path avoids boxing.
type concreteChunk struct {
	text string
}

func (c concreteChunk) Text() (string, error) { return c.text, nil }
func (c concreteChunk) JSON() (any, error)    { return nil, nil }

func TestExtractFrom_MatchesExtract(t *testing.T) {
	// Run the same sequence through both Extract (interface slice) and
	// ExtractFrom (concrete slice) and verify identical results.

	ext1 := NewIncrementalExtractor(false)
	ext2 := NewIncrementalExtractor(false)

	ifaceChunks := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"Hello"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":" world"}}]}`},
	}
	concreteChunks := []concreteChunk{
		{`{"choices":[{"delta":{"content":"Hello"}}]}`},
		{`{"choices":[{"delta":{"content":" world"}}]}`},
	}

	r1 := ext1.Extract(1, "openai", ifaceChunks)
	r2 := ExtractFrom(ext2, 1, "openai", concreteChunks)

	if r1.Delta != r2.Delta {
		t.Errorf("Delta mismatch: Extract=%q, ExtractFrom=%q", r1.Delta, r2.Delta)
	}
	if r1.Full != r2.Full {
		t.Errorf("Full mismatch: Extract=%q, ExtractFrom=%q", r1.Full, r2.Full)
	}
	if r1.Reset != r2.Reset {
		t.Errorf("Reset mismatch: Extract=%v, ExtractFrom=%v", r1.Reset, r2.Reset)
	}

	// Second tick: add one chunk
	ifaceChunks = append(ifaceChunks, mockChunk{`{"choices":[{"delta":{"content":"!"}}]}`})
	concreteChunks = append(concreteChunks, concreteChunk{`{"choices":[{"delta":{"content":"!"}}]}`})

	r1 = ext1.Extract(1, "openai", ifaceChunks)
	r2 = ExtractFrom(ext2, 1, "openai", concreteChunks)

	if r1.Delta != r2.Delta {
		t.Errorf("Delta mismatch (tick 2): Extract=%q, ExtractFrom=%q", r1.Delta, r2.Delta)
	}
	if r1.Full != r2.Full {
		t.Errorf("Full mismatch (tick 2): Extract=%q, ExtractFrom=%q", r1.Full, r2.Full)
	}
	if r1.Reset != r2.Reset {
		t.Errorf("Reset mismatch (tick 2): Extract=%v, ExtractFrom=%v", r1.Reset, r2.Reset)
	}
}

func TestExtractFrom_RetryReset(t *testing.T) {
	ext := NewIncrementalExtractor(false)

	chunks1 := []concreteChunk{
		{`{"choices":[{"delta":{"content":"First"}}]}`},
	}
	r1 := ExtractFrom(ext, 1, "openai", chunks1)
	if r1.Reset {
		t.Error("expected Reset=false for first extraction")
	}

	// Retry: new call with different chunks
	chunks2 := []concreteChunk{
		{`{"choices":[{"delta":{"content":"Retry"}}]}`},
	}
	r2 := ExtractFrom(ext, 2, "openai", chunks2)
	if !r2.Reset {
		t.Error("expected Reset=true on retry")
	}
	if r2.Full != "Retry" {
		t.Errorf("expected Full 'Retry', got %q", r2.Full)
	}
}

func TestExtractFrom_ZeroCallCount(t *testing.T) {
	ext := NewIncrementalExtractor(false)

	result := ExtractFrom(ext, 0, "openai", []concreteChunk(nil))
	if result.Delta != "" || result.Full != "" || result.Reset {
		t.Error("expected empty result for callCount=0")
	}
}

func TestIncrementalExtractor_Clear(t *testing.T) {
	ext := NewIncrementalExtractor(false)

	// Populate state with two ticks
	chunks := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"Hello"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":" world"}}]}`},
	}
	r := ext.Extract(1, "openai", chunks)
	if r.Full != "Hello world" {
		t.Fatalf("setup: expected 'Hello world', got %q", r.Full)
	}
	if ext.Full() != "Hello world" {
		t.Fatalf("setup: Full() should return accumulated content")
	}

	// Clear resets all internal state
	ext.Clear()

	if ext.Full() != "" {
		t.Errorf("after Clear: Full() should be empty, got %q", ext.Full())
	}

	// Next extraction should behave like a first extraction (Reset == false)
	chunks2 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"New"}}]}`},
	}
	r2 := ext.Extract(1, "openai", chunks2)
	if r2.Reset {
		t.Error("after Clear: first extraction should have Reset=false")
	}
	if r2.Full != "New" {
		t.Errorf("after Clear: expected Full='New', got %q", r2.Full)
	}
	if r2.Delta != "New" {
		t.Errorf("after Clear: expected Delta='New', got %q", r2.Delta)
	}
}

func TestIncrementalExtractor_ResetOnChunksDecreased(t *testing.T) {
	extractor := NewIncrementalExtractor(false)

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

// TestDeltaProviderSupportMatchesExtractor enforces parity between
// IsDeltaProviderSupported and the provider switch inside
// ExtractDeltaPartsFromText. If a new provider is added to one without the
// other — or if an existing provider's format changes incompatibly — this
// test fails.
//
// For each provider reported as supported, we feed a provider-specific
// valid sample chunk and assert that extraction succeeds with the expected
// content ("test"). Empty input is too weak because several cases return
// ("", nil) without error even on unrecognized payloads.
//
// For meta-providers and unknown strings, we assert both that
// IsDeltaProviderSupported returns false AND that ExtractDeltaPartsFromText
// returns an "unsupported provider" error, so the two agree about which
// strings are rejected.
func TestDeltaProviderSupportMatchesExtractor(t *testing.T) {
	// Provider-specific valid sample chunks. Each must yield Raw == "test"
	// when passed through ExtractDeltaPartsFromText. aws-bedrock uses its
	// debug-string format, not a plain JSON chunk.
	samples := map[string]string{
		"openai":           `{"choices":[{"delta":{"content":"test"}}]}`,
		"openai-generic":   `{"choices":[{"delta":{"content":"test"}}]}`,
		"azure-openai":     `{"choices":[{"delta":{"content":"test"}}]}`,
		"ollama":           `{"choices":[{"delta":{"content":"test"}}]}`,
		"openrouter":       `{"choices":[{"delta":{"content":"test"}}]}`,
		"openai-responses": `{"type":"response.output_text.delta","delta":"test"}`,
		"anthropic":        `{"type":"content_block_delta","delta":{"type":"text_delta","text":"test"}}`,
		"google-ai":        `{"candidates":[{"content":{"parts":[{"text":"test"}]}}]}`,
		"vertex-ai":        `{"candidates":[{"content":{"parts":[{"text":"test"}]}}]}`,
		"aws-bedrock":      `{"debug":"ContentBlockDelta { delta: Some(Text(\"test\")) }"}`,
	}

	for provider, chunk := range samples {
		if !IsDeltaProviderSupported(provider) {
			t.Errorf("IsDeltaProviderSupported(%q) = false; provider has a sample chunk and must be supported", provider)
			continue
		}
		parts, err := ExtractDeltaPartsFromText(provider, chunk, false)
		if err != nil {
			t.Errorf("ExtractDeltaPartsFromText(%q) returned error: %v", provider, err)
			continue
		}
		if parts.Raw != "test" {
			t.Errorf("ExtractDeltaPartsFromText(%q).Raw = %q, want %q", provider, parts.Raw, "test")
		}
	}

	// Meta-providers and unknown strings must be rejected by BOTH
	// IsDeltaProviderSupported AND ExtractDeltaPartsFromText.
	for _, provider := range []string{"baml-fallback", "baml-roundrobin", "", "unknown"} {
		if IsDeltaProviderSupported(provider) {
			t.Errorf("IsDeltaProviderSupported(%q) = true, want false", provider)
		}
		if _, err := ExtractDeltaPartsFromText(provider, "{}", false); err == nil {
			t.Errorf("ExtractDeltaPartsFromText(%q, \"{}\") returned nil error; expected unsupported provider", provider)
		} else if !strings.Contains(err.Error(), "unsupported provider") {
			t.Errorf("ExtractDeltaPartsFromText(%q) error = %v, want to contain %q", provider, err, "unsupported provider")
		}
	}
}
