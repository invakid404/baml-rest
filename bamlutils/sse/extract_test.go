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
	if result1.RawDelta != "Hello world" {
		t.Errorf("expected raw delta 'Hello world', got %q", result1.RawDelta)
	}
	if result1.RawFull != "Hello world" {
		t.Errorf("expected raw full 'Hello world', got %q", result1.RawFull)
	}
	if result1.ParseableDelta != "Hello world" {
		t.Errorf("expected parseable delta 'Hello world', got %q", result1.ParseableDelta)
	}
	if result1.ParseableFull != "Hello world" {
		t.Errorf("expected parseable full 'Hello world', got %q", result1.ParseableFull)
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
	if result2.RawDelta != "!" {
		t.Errorf("expected raw delta '!', got %q", result2.RawDelta)
	}
	if result2.RawFull != "Hello world!" {
		t.Errorf("expected raw full 'Hello world!', got %q", result2.RawFull)
	}
	if result2.Reset {
		t.Error("expected Reset=false for incremental extraction")
	}

	// Third tick: no new chunks
	result3 := extractor.Extract(1, "openai", chunks2)
	if result3.RawDelta != "" {
		t.Errorf("expected empty raw delta, got %q", result3.RawDelta)
	}
	if result3.RawFull != "Hello world!" {
		t.Errorf("expected raw full 'Hello world!', got %q", result3.RawFull)
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
	if result1.RawFull != "First attempt partial" {
		t.Errorf("expected raw full 'First attempt partial', got %q", result1.RawFull)
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
	if result2.RawFull != "Retry success" {
		t.Errorf("expected raw full 'Retry success', got %q", result2.RawFull)
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
	if result3.RawDelta != "!" {
		t.Errorf("expected raw delta '!', got %q", result3.RawDelta)
	}
	if result3.RawFull != "Retry success!" {
		t.Errorf("expected raw full 'Retry success!', got %q", result3.RawFull)
	}
}

func TestIncrementalExtractor_RawAndParseableFull(t *testing.T) {
	extractor := NewIncrementalExtractor(false)

	// Before any extraction both accessors are empty.
	if got := extractor.RawFull(); got != "" {
		t.Errorf("expected empty RawFull() before extraction, got %q", got)
	}
	if got := extractor.ParseableFull(); got != "" {
		t.Errorf("expected empty ParseableFull() before extraction, got %q", got)
	}

	// After extraction (text-only provider) both accessors return identical content.
	chunks := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"test"}}]}`},
	}

	extractor.Extract(1, "openai", chunks)
	if got := extractor.RawFull(); got != "test" {
		t.Errorf("expected RawFull() 'test', got %q", got)
	}
	if got := extractor.ParseableFull(); got != "test" {
		t.Errorf("expected ParseableFull() 'test', got %q", got)
	}
}

func TestIncrementalExtractor_ResetWithEmptyDelta(t *testing.T) {
	extractor := NewIncrementalExtractor(false)

	// First tick: 1 call with content
	chunks1 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"Hello"}}]}`},
	}

	result1 := extractor.Extract(1, "openai", chunks1)
	if result1.RawFull != "Hello" {
		t.Errorf("expected raw full 'Hello', got %q", result1.RawFull)
	}

	// Second tick: retry occurred (callCount=2) but new call has 0 chunks yet
	// This is the critical case: Reset should be true even though Delta is empty
	emptyChunks := []SSEChunk{}

	result2 := extractor.Extract(2, "openai", emptyChunks)
	if !result2.Reset {
		t.Error("expected Reset=true when retry occurred, even with empty delta")
	}
	if result2.RawDelta != "" {
		t.Errorf("expected empty raw delta, got %q", result2.RawDelta)
	}
	if result2.RawFull != "" {
		t.Errorf("expected empty raw full (new call has no content yet), got %q", result2.RawFull)
	}

	// Third tick: new call gets content - Reset should now be false
	chunks3 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"World"}}]}`},
	}

	result3 := extractor.Extract(2, "openai", chunks3)
	if result3.Reset {
		t.Error("expected Reset=false for incremental update after retry")
	}
	if result3.RawDelta != "World" {
		t.Errorf("expected raw delta 'World', got %q", result3.RawDelta)
	}
	if result3.RawFull != "World" {
		t.Errorf("expected raw full 'World', got %q", result3.RawFull)
	}
}

func TestIncrementalExtractor_DifferentProviders(t *testing.T) {
	extractor := NewIncrementalExtractor(false)

	// First call with OpenAI
	chunks1 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"OpenAI"}}]}`},
	}

	result1 := extractor.Extract(1, "openai", chunks1)
	if result1.RawFull != "OpenAI" {
		t.Errorf("expected raw full 'OpenAI', got %q", result1.RawFull)
	}

	// Retry with Anthropic (different provider)
	chunks2 := []SSEChunk{
		mockChunk{`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Anthropic"}}`},
	}

	result2 := extractor.Extract(2, "anthropic", chunks2)
	if !result2.Reset {
		t.Error("expected Reset=true on retry")
	}
	if result2.RawFull != "Anthropic" {
		t.Errorf("expected raw full 'Anthropic', got %q", result2.RawFull)
	}
}

// TestExtractDeltaPartsFromText_AnthropicThinking_Default verifies that with
// includeReasoning=false (the default, BAML-aligned behavior), Anthropic
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
// includeReasoning=true, Anthropic thinking_delta events populate the
// Reasoning channel while Parseable and Raw stay empty (raw is text-only
// by construction). text_delta behavior is unchanged.
func TestExtractDeltaPartsFromText_AnthropicThinking_OptIn(t *testing.T) {
	textDelta, err := ExtractDeltaPartsFromText("anthropic", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"Answer"}}`, true)
	if err != nil {
		t.Fatalf("text delta extraction failed: %v", err)
	}
	if textDelta.Parseable != "Answer" || textDelta.Raw != "Answer" || textDelta.Reasoning != "" {
		t.Fatalf("unexpected text delta: %+v", textDelta)
	}

	thinkingDelta, err := ExtractDeltaPartsFromText("anthropic", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Reasoning"}}`, true)
	if err != nil {
		t.Fatalf("thinking delta extraction failed: %v", err)
	}
	if thinkingDelta.Parseable != "" || thinkingDelta.Raw != "" || thinkingDelta.Reasoning != "Reasoning" {
		t.Fatalf("unexpected thinking delta under opt-in: %+v", thinkingDelta)
	}
}

// TestExtractDeltaPartsFromText_AnthropicThinking_ParseableInvariant codifies
// the structural guarantee that Parseable never includes thinking content,
// regardless of the includeReasoning flag value. Any future refactor that
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

// TestIncrementalExtractor_AnthropicThinking_ParseableInvariant exercises the
// extractor end-to-end with a thinking-then-text Anthropic stream and asserts
// the structural guarantee at the buffer level: regardless of the
// IncludeReasoning flag, the parseable and raw buffers are text-only. Under
// opt-in the reasoning buffer additionally carries thinking content; under
// default the reasoning buffer is empty.
//
// This test is the architectural counterpart to the per-event parseable
// invariant test above — it verifies the guarantee survives accumulation,
// which is what the legacy stream path relies on when feeding ParseStream.
func TestIncrementalExtractor_AnthropicThinking_ParseableInvariant(t *testing.T) {
	chunks := []SSEChunk{
		mockChunk{`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Let me reason..."}}`},
		mockChunk{`{"type":"content_block_delta","delta":{"type":"text_delta","text":"The answer "}}`},
		mockChunk{`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":" still thinking"}}`},
		mockChunk{`{"type":"content_block_delta","delta":{"type":"text_delta","text":"is 42"}}`},
	}

	// Default flag: parseable and raw text-only; reasoning empty.
	extOff := NewIncrementalExtractor(false)
	rOff := extOff.Extract(1, "anthropic", chunks)
	if rOff.ParseableFull != "The answer is 42" {
		t.Errorf("default: ParseableFull = %q, want %q", rOff.ParseableFull, "The answer is 42")
	}
	if rOff.RawFull != "The answer is 42" {
		t.Errorf("default: RawFull = %q, want %q (text-only)", rOff.RawFull, "The answer is 42")
	}
	if rOff.ReasoningFull != "" {
		t.Errorf("default: ReasoningFull = %q, want empty", rOff.ReasoningFull)
	}

	// Opt-in: parseable + raw still text-only; reasoning carries thinking.
	extOn := NewIncrementalExtractor(true)
	rOn := extOn.Extract(1, "anthropic", chunks)
	if rOn.ParseableFull != "The answer is 42" {
		t.Errorf("opt-in: ParseableFull = %q, want %q (parseable invariant violated!)", rOn.ParseableFull, "The answer is 42")
	}
	if rOn.RawFull != "The answer is 42" {
		t.Errorf("opt-in: RawFull = %q, want %q (raw is text-only by construction)", rOn.RawFull, "The answer is 42")
	}
	expectedReasoning := "Let me reason... still thinking"
	if rOn.ReasoningFull != expectedReasoning {
		t.Errorf("opt-in: ReasoningFull = %q, want %q", rOn.ReasoningFull, expectedReasoning)
	}

	// Cross-flag invariant: parseable and raw are byte-identical
	// regardless of flag.
	if rOff.ParseableFull != rOn.ParseableFull {
		t.Errorf("Parseable diverged across flag values: off=%q on=%q",
			rOff.ParseableFull, rOn.ParseableFull)
	}
	if rOff.RawFull != rOn.RawFull {
		t.Errorf("Raw diverged across flag values: off=%q on=%q",
			rOff.RawFull, rOn.RawFull)
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

// TestGetCurrentContent_OpenAIReasoningRawOnly verifies that
// GetCurrentContent returns only Raw text (never reasoning). Under both
// flag values, a reasoning-only delta yields an empty result because Raw
// is text-only by construction. Callers that need reasoning text must
// use ExtractDeltaPartsFromText directly and walk DeltaParts.Reasoning.
func TestGetCurrentContent_OpenAIReasoningRawOnly(t *testing.T) {
	for _, provider := range []string{"openai", "openai-generic", "azure-openai", "ollama", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			data := &StreamingData{
				Calls: []StreamingCall{
					{
						Provider: provider,
						Chunks: []SSEChunk{
							mockChunk{`{"choices":[{"delta":{"reasoning_content":"think"}}]}`},
						},
					},
				},
			}

			off, err := GetCurrentContent(data, false)
			if err != nil {
				t.Fatalf("flag=false: unexpected error: %v", err)
			}
			if off != "" {
				t.Errorf("flag=false: expected empty content (reasoning_content not in raw), got %q", off)
			}

			on, err := GetCurrentContent(data, true)
			if err != nil {
				t.Fatalf("flag=true: unexpected error: %v", err)
			}
			if on != "" {
				t.Errorf("flag=true: expected empty content (raw is text-only; reasoning surfaces via DeltaParts.Reasoning), got %q", on)
			}
		})
	}
}

func TestIncrementalExtractor_ZeroCallCount(t *testing.T) {
	extractor := NewIncrementalExtractor(false)

	// callCount=0 should return empty result
	result := extractor.Extract(0, "openai", nil)
	if result.RawDelta != "" || result.RawFull != "" || result.ParseableDelta != "" || result.ParseableFull != "" || result.Reset {
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

	if r1.RawDelta != r2.RawDelta {
		t.Errorf("RawDelta mismatch: Extract=%q, ExtractFrom=%q", r1.RawDelta, r2.RawDelta)
	}
	if r1.RawFull != r2.RawFull {
		t.Errorf("RawFull mismatch: Extract=%q, ExtractFrom=%q", r1.RawFull, r2.RawFull)
	}
	if r1.Reset != r2.Reset {
		t.Errorf("Reset mismatch: Extract=%v, ExtractFrom=%v", r1.Reset, r2.Reset)
	}

	// Second tick: add one chunk
	ifaceChunks = append(ifaceChunks, mockChunk{`{"choices":[{"delta":{"content":"!"}}]}`})
	concreteChunks = append(concreteChunks, concreteChunk{`{"choices":[{"delta":{"content":"!"}}]}`})

	r1 = ext1.Extract(1, "openai", ifaceChunks)
	r2 = ExtractFrom(ext2, 1, "openai", concreteChunks)

	if r1.RawDelta != r2.RawDelta {
		t.Errorf("RawDelta mismatch (tick 2): Extract=%q, ExtractFrom=%q", r1.RawDelta, r2.RawDelta)
	}
	if r1.RawFull != r2.RawFull {
		t.Errorf("RawFull mismatch (tick 2): Extract=%q, ExtractFrom=%q", r1.RawFull, r2.RawFull)
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
	if r2.RawFull != "Retry" {
		t.Errorf("expected RawFull 'Retry', got %q", r2.RawFull)
	}
}

func TestExtractFrom_ZeroCallCount(t *testing.T) {
	ext := NewIncrementalExtractor(false)

	result := ExtractFrom(ext, 0, "openai", []concreteChunk(nil))
	if result.RawDelta != "" || result.RawFull != "" || result.ParseableDelta != "" || result.ParseableFull != "" || result.Reset {
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
	if r.RawFull != "Hello world" {
		t.Fatalf("setup: expected 'Hello world', got %q", r.RawFull)
	}
	if ext.RawFull() != "Hello world" {
		t.Fatalf("setup: RawFull() should return accumulated content")
	}
	if ext.ParseableFull() != "Hello world" {
		t.Fatalf("setup: ParseableFull() should return accumulated content")
	}

	// Clear resets all internal state
	ext.Clear()

	if ext.RawFull() != "" {
		t.Errorf("after Clear: RawFull() should be empty, got %q", ext.RawFull())
	}
	if ext.ParseableFull() != "" {
		t.Errorf("after Clear: ParseableFull() should be empty, got %q", ext.ParseableFull())
	}

	// Next extraction should behave like a first extraction (Reset == false)
	chunks2 := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"content":"New"}}]}`},
	}
	r2 := ext.Extract(1, "openai", chunks2)
	if r2.Reset {
		t.Error("after Clear: first extraction should have Reset=false")
	}
	if r2.RawFull != "New" {
		t.Errorf("after Clear: expected RawFull='New', got %q", r2.RawFull)
	}
	if r2.RawDelta != "New" {
		t.Errorf("after Clear: expected RawDelta='New', got %q", r2.RawDelta)
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
	if result1.RawFull != "ABC" {
		t.Errorf("expected raw full 'ABC', got %q", result1.RawFull)
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
	if result2.RawFull != "XY" {
		t.Errorf("expected raw full 'XY', got %q", result2.RawFull)
	}
	// Delta should be the full content since we rebuilt
	if result2.RawDelta != "XY" {
		t.Errorf("expected raw delta 'XY' (full rebuild), got %q", result2.RawDelta)
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

// TestExtractDeltaPartsFromText_GeminiThoughtFiltering asserts that the
// Google AI / Vertex AI streaming branch iterates all parts, skips entries
// flagged thought:true, and concatenates the surviving text fields.
//
// Aligned with upstream BAML's text_content_part filter
// (engine/baml-runtime/src/internal/llm_client/primitive/google/response_handler.rs).
//
// Parseable invariance across includeReasoning is asserted; Raw equality
// across the flag is intentionally NOT asserted here — Phase 3 will diverge
// Gemini Raw under opt-in, and locking it now would block that.
func TestExtractDeltaPartsFromText_GeminiThoughtFiltering(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "single part",
			body: `{"candidates":[{"content":{"parts":[{"text":"Answer"}]}}]}`,
			want: "Answer",
		},
		{
			name: "multi part concatenation",
			body: `{"candidates":[{"content":{"parts":[{"text":"Hello "},{"text":"world"}]}}]}`,
			want: "Hello world",
		},
		{
			name: "thought at index 0 is skipped",
			body: `{"candidates":[{"content":{"parts":[{"text":"internal","thought":true},{"text":"Visible"}]}}]}`,
			want: "Visible",
		},
		{
			name: "thought-only yields empty",
			body: `{"candidates":[{"content":{"parts":[{"text":"internal","thought":true}]}}]}`,
			want: "",
		},
		{
			// Defensive guard: gjson stringifies non-string scalars via
			// String(), so an unguarded WriteString on a numeric text
			// field would emit "42Visible" instead of "Visible".
			name: "non-string text field is skipped",
			body: `{"candidates":[{"content":{"parts":[{"text":42},{"text":"Visible"}]}}]}`,
			want: "Visible",
		},
	}

	for _, provider := range []string{"google-ai", "vertex-ai"} {
		for _, tc := range cases {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				parts, err := ExtractDeltaPartsFromText(provider, tc.body, false)
				if err != nil {
					t.Fatalf("ExtractDeltaPartsFromText returned error: %v", err)
				}
				if parts.Parseable != tc.want {
					t.Errorf("Parseable = %q, want %q", parts.Parseable, tc.want)
				}
				if parts.Raw != tc.want {
					t.Errorf("Raw = %q, want %q (Phase 2: Raw == Parseable for Gemini)", parts.Raw, tc.want)
				}

				// Parseable must be byte-identical regardless of the
				// includeReasoning flag. Raw is intentionally not compared
				// across the flag — Phase 3 will diverge Gemini Raw under
				// opt-in, and locking it here would block that.
				partsThinking, err := ExtractDeltaPartsFromText(provider, tc.body, true)
				if err != nil {
					t.Fatalf("ExtractDeltaPartsFromText(includeReasoning=true) returned error: %v", err)
				}
				if partsThinking.Parseable != parts.Parseable {
					t.Errorf("Parseable diverged across includeReasoning: false=%q true=%q", parts.Parseable, partsThinking.Parseable)
				}
			})
		}
	}
}

// TestExtractDeltaPartsFromText_GeminiIncludeReasoning asserts the
// per-flag behavior for the Google AI / Vertex AI streaming branch:
//
//   - Parseable always excludes thought-tagged parts (text-only, fed to
//     the BAML parser via ParseStream).
//   - Raw mirrors Parseable regardless of flag — raw is text-only by
//     construction.
//   - Reasoning surfaces thought-tagged string text only when
//     includeReasoning is true.
//   - Non-string text on a thought part is silently skipped under both
//     flag values (thought parts are filtered, not validated).
//
// Parseable + raw invariance across the includeReasoning flag is
// asserted for every fixture; Reasoning is intentionally allowed to
// diverge.
func TestExtractDeltaPartsFromText_GeminiIncludeReasoning(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		parseable   string
		raw         string
		reasoningOn string
	}{
		{
			name:        "thought then visible",
			body:        `{"candidates":[{"content":{"parts":[{"text":"thought","thought":true},{"text":"Visible"}]}}]}`,
			parseable:   "Visible",
			raw:         "Visible",
			reasoningOn: "thought",
		},
		{
			name:        "interleaved thought preserves order",
			body:        `{"candidates":[{"content":{"parts":[{"text":"A"},{"text":"B","thought":true},{"text":"C"}]}}]}`,
			parseable:   "AC",
			raw:         "AC",
			reasoningOn: "B",
		},
		{
			name:        "thought only",
			body:        `{"candidates":[{"content":{"parts":[{"text":"thought","thought":true}]}}]}`,
			parseable:   "",
			raw:         "",
			reasoningOn: "thought",
		},
		{
			// Numeric text on a thought part: silently skipped under both
			// flag values.
			name:        "thought non-string text skipped under opt-in",
			body:        `{"candidates":[{"content":{"parts":[{"text":42,"thought":true},{"text":"Visible"}]}}]}`,
			parseable:   "Visible",
			raw:         "Visible",
			reasoningOn: "",
		},
	}

	for _, provider := range []string{"google-ai", "vertex-ai"} {
		for _, tc := range cases {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				off, err := ExtractDeltaPartsFromText(provider, tc.body, false)
				if err != nil {
					t.Fatalf("ExtractDeltaPartsFromText(includeReasoning=false) returned error: %v", err)
				}
				if off.Parseable != tc.parseable {
					t.Errorf("flag=false Parseable = %q, want %q", off.Parseable, tc.parseable)
				}
				if off.Raw != tc.raw {
					t.Errorf("flag=false Raw = %q, want %q", off.Raw, tc.raw)
				}
				if off.Reasoning != "" {
					t.Errorf("flag=false Reasoning = %q, want empty", off.Reasoning)
				}

				on, err := ExtractDeltaPartsFromText(provider, tc.body, true)
				if err != nil {
					t.Fatalf("ExtractDeltaPartsFromText(includeReasoning=true) returned error: %v", err)
				}
				if on.Parseable != tc.parseable {
					t.Errorf("flag=true Parseable = %q, want %q", on.Parseable, tc.parseable)
				}
				if on.Raw != tc.raw {
					t.Errorf("flag=true Raw = %q, want %q (raw is text-only regardless of flag)", on.Raw, tc.raw)
				}
				if on.Reasoning != tc.reasoningOn {
					t.Errorf("flag=true Reasoning = %q, want %q", on.Reasoning, tc.reasoningOn)
				}

				// Structural parseable + raw invariance: byte-identical
				// across both flag values for every fixture.
				if off.Parseable != on.Parseable {
					t.Errorf("Parseable diverged across includeReasoning: false=%q true=%q", off.Parseable, on.Parseable)
				}
				if off.Raw != on.Raw {
					t.Errorf("Raw diverged across includeReasoning: false=%q true=%q", off.Raw, on.Raw)
				}
			})
		}
	}
}

// TestExtractDeltaPartsFromText_OpenAIReasoningContent asserts the
// three-channel behavior for the OpenAI-compatible Chat Completions
// streaming branch (openai, openai-generic, azure-openai, ollama,
// openrouter):
//
//   - Parseable always reflects only delta.content (text-only, fed to the
//     BAML parser via ParseStream).
//   - Raw mirrors Parseable in all states — raw is text-only by
//     construction and never carries reasoning_content.
//   - Reasoning surfaces delta.reasoning_content only when
//     includeReasoning is true; otherwise empty.
//   - Non-string reasoning_content is silently skipped under both flag
//     values, matching the forgiving streaming semantics for optional
//     reasoning surfaces.
//
// Parseable and Raw invariance across the includeReasoning flag is
// asserted for every fixture × provider; Reasoning is intentionally
// allowed to diverge.
func TestExtractDeltaPartsFromText_OpenAIReasoningContent(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		parseable   string
		raw         string
		reasoningOn string
	}{
		{
			name:        "content only",
			body:        `{"choices":[{"delta":{"content":"Hello"}}]}`,
			parseable:   "Hello",
			raw:         "Hello",
			reasoningOn: "",
		},
		{
			name:        "reasoning only",
			body:        `{"choices":[{"delta":{"reasoning_content":"think"}}]}`,
			parseable:   "",
			raw:         "",
			reasoningOn: "think",
		},
		{
			name:        "content plus reasoning",
			body:        `{"choices":[{"delta":{"content":"Hello","reasoning_content":"think"}}]}`,
			parseable:   "Hello",
			raw:         "Hello",
			reasoningOn: "think",
		},
		{
			// Defensive: non-string reasoning_content is silently skipped
			// under opt-in, matching the gjson.String guard in the extractor.
			name:        "non-string reasoning ignored under opt-in",
			body:        `{"choices":[{"delta":{"content":"Hello","reasoning_content":42}}]}`,
			parseable:   "Hello",
			raw:         "Hello",
			reasoningOn: "",
		},
	}

	for _, provider := range []string{"openai", "openai-generic", "azure-openai", "ollama", "openrouter"} {
		for _, tc := range cases {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				off, err := ExtractDeltaPartsFromText(provider, tc.body, false)
				if err != nil {
					t.Fatalf("ExtractDeltaPartsFromText(includeReasoning=false) returned error: %v", err)
				}
				if off.Parseable != tc.parseable {
					t.Errorf("flag=false Parseable = %q, want %q", off.Parseable, tc.parseable)
				}
				if off.Raw != tc.raw {
					t.Errorf("flag=false Raw = %q, want %q", off.Raw, tc.raw)
				}
				if off.Reasoning != "" {
					t.Errorf("flag=false Reasoning = %q, want empty", off.Reasoning)
				}

				on, err := ExtractDeltaPartsFromText(provider, tc.body, true)
				if err != nil {
					t.Fatalf("ExtractDeltaPartsFromText(includeReasoning=true) returned error: %v", err)
				}
				if on.Parseable != tc.parseable {
					t.Errorf("flag=true Parseable = %q, want %q", on.Parseable, tc.parseable)
				}
				if on.Raw != tc.raw {
					t.Errorf("flag=true Raw = %q, want %q (raw is text-only regardless of flag)", on.Raw, tc.raw)
				}
				if on.Reasoning != tc.reasoningOn {
					t.Errorf("flag=true Reasoning = %q, want %q", on.Reasoning, tc.reasoningOn)
				}

				// Structural parseable + raw invariance: byte-identical
				// across both flag values for every fixture.
				if off.Parseable != on.Parseable {
					t.Errorf("Parseable diverged across includeReasoning: false=%q true=%q", off.Parseable, on.Parseable)
				}
				if off.Raw != on.Raw {
					t.Errorf("Raw diverged across includeReasoning: false=%q true=%q", off.Raw, on.Raw)
				}
			})
		}
	}
}

// TestIncrementalExtractor_OpenAIReasoning_StreamOrder verifies that the
// stored includeReasoning flag reaches ExtractDeltaPartsFromText through
// IncrementalExtractor.ExtractFrom, and that event order is preserved on
// the reasoning buffer: a reasoning-only event followed by a content-only
// event yields reasoning="think" + raw="Hello" under opt-in, and reasoning
// empty under default. Raw stays text-only regardless.
func TestIncrementalExtractor_OpenAIReasoning_StreamOrder(t *testing.T) {
	chunks := []SSEChunk{
		mockChunk{`{"choices":[{"delta":{"reasoning_content":"think"}}]}`},
		mockChunk{`{"choices":[{"delta":{"content":"Hello"}}]}`},
	}

	for _, provider := range []string{"openai", "openai-generic", "azure-openai", "ollama", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			off := NewIncrementalExtractor(false)
			resOff := off.Extract(1, provider, chunks)
			if resOff.ParseableFull != "Hello" {
				t.Errorf("flag=false ParseableFull = %q, want %q", resOff.ParseableFull, "Hello")
			}
			if resOff.RawFull != "Hello" {
				t.Errorf("flag=false RawFull = %q, want %q", resOff.RawFull, "Hello")
			}
			if resOff.ReasoningFull != "" {
				t.Errorf("flag=false ReasoningFull = %q, want empty", resOff.ReasoningFull)
			}

			on := NewIncrementalExtractor(true)
			resOn := on.Extract(1, provider, chunks)
			if resOn.ParseableFull != "Hello" {
				t.Errorf("flag=true ParseableFull = %q, want %q (parseable invariant violated!)", resOn.ParseableFull, "Hello")
			}
			if resOn.RawFull != "Hello" {
				t.Errorf("flag=true RawFull = %q, want %q (raw is text-only by construction)", resOn.RawFull, "Hello")
			}
			if resOn.ReasoningFull != "think" {
				t.Errorf("flag=true ReasoningFull = %q, want %q", resOn.ReasoningFull, "think")
			}

			// Structural parseable + raw invariance across the
			// IncrementalExtractor.
			if resOff.ParseableFull != resOn.ParseableFull {
				t.Errorf("ParseableFull diverged across includeReasoning: false=%q true=%q", resOff.ParseableFull, resOn.ParseableFull)
			}
			if resOff.RawFull != resOn.RawFull {
				t.Errorf("RawFull diverged across includeReasoning: false=%q true=%q", resOff.RawFull, resOn.RawFull)
			}
		})
	}
}

// TestIncrementalExtractor_GeminiThinking_ParseableInvariant exercises
// ExtractFrom on a Gemini SSE chunk through both flag-off and flag-on
// IncrementalExtractor instances, proving that the stored
// includeReasoning flag reaches ExtractDeltaPartsFromText in both the
// rebuild and incremental paths and that the parseable + raw buffers are
// byte-identical regardless of the flag.
func TestIncrementalExtractor_GeminiThinking_ParseableInvariant(t *testing.T) {
	chunks := []SSEChunk{
		mockChunk{`{"candidates":[{"content":{"parts":[{"text":"think","thought":true},{"text":"Hello"}]}}]}`},
		mockChunk{`{"candidates":[{"content":{"parts":[{"text":" more","thought":true},{"text":" world"}]}}]}`},
	}

	for _, provider := range []string{"google-ai", "vertex-ai"} {
		t.Run(provider, func(t *testing.T) {
			off := NewIncrementalExtractor(false)
			resOff := off.Extract(1, provider, chunks)

			on := NewIncrementalExtractor(true)
			resOn := on.Extract(1, provider, chunks)

			if resOff.ParseableFull != "Hello world" {
				t.Errorf("flag=false ParseableFull = %q, want %q", resOff.ParseableFull, "Hello world")
			}
			if resOff.RawFull != "Hello world" {
				t.Errorf("flag=false RawFull = %q, want %q", resOff.RawFull, "Hello world")
			}
			if resOff.ReasoningFull != "" {
				t.Errorf("flag=false ReasoningFull = %q, want empty", resOff.ReasoningFull)
			}
			if resOn.ParseableFull != "Hello world" {
				t.Errorf("flag=true ParseableFull = %q, want %q", resOn.ParseableFull, "Hello world")
			}
			if resOn.RawFull != "Hello world" {
				t.Errorf("flag=true RawFull = %q, want %q (raw is text-only by construction)", resOn.RawFull, "Hello world")
			}
			if resOn.ReasoningFull != "think more" {
				t.Errorf("flag=true ReasoningFull = %q, want %q", resOn.ReasoningFull, "think more")
			}

			// Structural parseable + raw invariance under the
			// IncrementalExtractor.
			if resOff.ParseableFull != resOn.ParseableFull {
				t.Errorf("ParseableFull diverged across includeReasoning: false=%q true=%q", resOff.ParseableFull, resOn.ParseableFull)
			}
			if resOff.RawFull != resOn.RawFull {
				t.Errorf("RawFull diverged across includeReasoning: false=%q true=%q", resOff.RawFull, resOn.RawFull)
			}
		})
	}
}

// TestExtractDeltaPartsFromText_OpenAIResponsesReasoningSummary asserts
// the three-channel behavior for the OpenAI Responses API
// (`/v1/responses`) streaming branch (provider key "openai-responses"):
//
//   - response.output_text.delta contributes to both Parseable and Raw
//     regardless of includeReasoning (text-only message stream).
//   - response.reasoning_summary_text.delta contributes to Reasoning only,
//     and only when includeReasoning is true. Parseable and Raw always
//     stay empty for reasoning events (raw is text-only by construction).
//   - Non-string delta on a reasoning_summary_text.delta event is silently
//     skipped under opt-in (defensive gjson.String guard).
//   - response.reasoning_summary_part.added is a bracket/setup event and
//     contributes nothing.
//   - response.reasoning_summary_part.done and response.reasoning_summary_text.done
//     deliberately contribute nothing — acting on them in this stateless
//     extractor would duplicate text already emitted by the deltas (mirrors
//     the existing response.output_text.done treatment).
//
// Parseable and Raw invariance across the includeReasoning flag is
// asserted for every fixture; Reasoning is intentionally allowed to
// diverge.
func TestExtractDeltaPartsFromText_OpenAIResponsesReasoningSummary(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		parseable   string
		raw         string
		reasoningOn string
	}{
		{
			name:        "output_text.delta",
			body:        `{"type":"response.output_text.delta","delta":"Hello"}`,
			parseable:   "Hello",
			raw:         "Hello",
			reasoningOn: "",
		},
		{
			// Defensive: non-string delta on output_text.delta is silently
			// skipped, matching the gjson.String guard in the extractor.
			name:        "non-string output_text delta is skipped",
			body:        `{"type":"response.output_text.delta","delta":42}`,
			parseable:   "",
			raw:         "",
			reasoningOn: "",
		},
		{
			name:        "reasoning_summary_text.delta opt-in surfaces summary",
			body:        `{"type":"response.reasoning_summary_text.delta","delta":"thought"}`,
			parseable:   "",
			raw:         "",
			reasoningOn: "thought",
		},
		{
			// Defensive: non-string delta on a reasoning summary event is
			// silently skipped under opt-in.
			name:        "reasoning_summary_text.delta non-string delta ignored",
			body:        `{"type":"response.reasoning_summary_text.delta","delta":42}`,
			parseable:   "",
			raw:         "",
			reasoningOn: "",
		},
		{
			name:        "reasoning_summary_part.added is a no-op",
			body:        `{"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}`,
			parseable:   "",
			raw:         "",
			reasoningOn: "",
		},
		{
			// Done event carries the full summary text but is intentionally
			// not extracted — acting on it would duplicate text from the
			// preceding *_text.delta events in this stateless extractor.
			name:        "reasoning_summary_part.done is a no-op (avoids duplicate text)",
			body:        `{"type":"response.reasoning_summary_part.done","part":{"type":"summary_text","text":"thought complete"}}`,
			parseable:   "",
			raw:         "",
			reasoningOn: "",
		},
		{
			name:        "reasoning_summary_text.done is a no-op (avoids duplicate text)",
			body:        `{"type":"response.reasoning_summary_text.done","text":"thought complete"}`,
			parseable:   "",
			raw:         "",
			reasoningOn: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			off, err := ExtractDeltaPartsFromText("openai-responses", tc.body, false)
			if err != nil {
				t.Fatalf("ExtractDeltaPartsFromText(includeReasoning=false) returned error: %v", err)
			}
			if off.Parseable != tc.parseable {
				t.Errorf("flag=false Parseable = %q, want %q", off.Parseable, tc.parseable)
			}
			if off.Raw != tc.raw {
				t.Errorf("flag=false Raw = %q, want %q", off.Raw, tc.raw)
			}
			if off.Reasoning != "" {
				t.Errorf("flag=false Reasoning = %q, want empty", off.Reasoning)
			}

			on, err := ExtractDeltaPartsFromText("openai-responses", tc.body, true)
			if err != nil {
				t.Fatalf("ExtractDeltaPartsFromText(includeReasoning=true) returned error: %v", err)
			}
			if on.Parseable != tc.parseable {
				t.Errorf("flag=true Parseable = %q, want %q", on.Parseable, tc.parseable)
			}
			if on.Raw != tc.raw {
				t.Errorf("flag=true Raw = %q, want %q (raw is text-only regardless of flag)", on.Raw, tc.raw)
			}
			if on.Reasoning != tc.reasoningOn {
				t.Errorf("flag=true Reasoning = %q, want %q", on.Reasoning, tc.reasoningOn)
			}

			// Structural parseable + raw invariance: byte-identical across
			// both flag values for every fixture.
			if off.Parseable != on.Parseable {
				t.Errorf("Parseable diverged across includeReasoning: false=%q true=%q", off.Parseable, on.Parseable)
			}
			if off.Raw != on.Raw {
				t.Errorf("Raw diverged across includeReasoning: false=%q true=%q", off.Raw, on.Raw)
			}
		})
	}
}

// TestIncrementalExtractor_OpenAIResponsesReasoningSummary_StreamOrder
// verifies that the stored includeReasoning flag reaches
// ExtractDeltaPartsFromText through IncrementalExtractor.ExtractFrom for
// the openai-responses provider, and that event order is preserved on
// the reasoning buffer: interleaved summary/text deltas yield reasoning
// "think1 think2 " in order under opt-in, while parseable and raw stay
// text-only and flag-invariant.
func TestIncrementalExtractor_OpenAIResponsesReasoningSummary_StreamOrder(t *testing.T) {
	chunks := []SSEChunk{
		mockChunk{`{"type":"response.reasoning_summary_text.delta","delta":"think1 "}`},
		mockChunk{`{"type":"response.output_text.delta","delta":"Hello "}`},
		mockChunk{`{"type":"response.reasoning_summary_text.delta","delta":"think2 "}`},
		mockChunk{`{"type":"response.output_text.delta","delta":"world"}`},
	}

	off := NewIncrementalExtractor(false)
	resOff := off.Extract(1, "openai-responses", chunks)
	if resOff.ParseableFull != "Hello world" {
		t.Errorf("flag=false ParseableFull = %q, want %q", resOff.ParseableFull, "Hello world")
	}
	if resOff.RawFull != "Hello world" {
		t.Errorf("flag=false RawFull = %q, want %q (must mirror ParseableFull when flag is off)", resOff.RawFull, "Hello world")
	}
	if resOff.ReasoningFull != "" {
		t.Errorf("flag=false ReasoningFull = %q, want empty", resOff.ReasoningFull)
	}

	on := NewIncrementalExtractor(true)
	resOn := on.Extract(1, "openai-responses", chunks)
	if resOn.ParseableFull != "Hello world" {
		t.Errorf("flag=true ParseableFull = %q, want %q (parseable invariant violated!)", resOn.ParseableFull, "Hello world")
	}
	if resOn.RawFull != "Hello world" {
		t.Errorf("flag=true RawFull = %q, want %q (raw is text-only by construction)", resOn.RawFull, "Hello world")
	}
	if resOn.ReasoningFull != "think1 think2 " {
		t.Errorf("flag=true ReasoningFull = %q, want %q (event order must be preserved)", resOn.ReasoningFull, "think1 think2 ")
	}

	// Structural parseable + raw invariance across the IncrementalExtractor.
	if resOff.ParseableFull != resOn.ParseableFull {
		t.Errorf("ParseableFull diverged across includeReasoning: false=%q true=%q", resOff.ParseableFull, resOn.ParseableFull)
	}
	if resOff.RawFull != resOn.RawFull {
		t.Errorf("RawFull diverged across includeReasoning: false=%q true=%q", resOff.RawFull, resOn.RawFull)
	}
}

// TestGetCurrentContent_OpenAIResponsesReasoningSummaryRawOnly verifies
// that GetCurrentContent returns only Raw text. A summary-only chunk
// yields empty content under both flag values because Raw is text-only
// by construction; reasoning surfaces via DeltaParts.Reasoning, not via
// the Raw-returning helper.
func TestGetCurrentContent_OpenAIResponsesReasoningSummaryRawOnly(t *testing.T) {
	data := &StreamingData{
		Calls: []StreamingCall{
			{
				Provider: "openai-responses",
				Chunks: []SSEChunk{
					mockChunk{`{"type":"response.reasoning_summary_text.delta","delta":"thought"}`},
				},
			},
		},
	}

	off, err := GetCurrentContent(data, false)
	if err != nil {
		t.Fatalf("flag=false: unexpected error: %v", err)
	}
	if off != "" {
		t.Errorf("flag=false: expected empty content (reasoning summary not in raw), got %q", off)
	}

	on, err := GetCurrentContent(data, true)
	if err != nil {
		t.Fatalf("flag=true: unexpected error: %v", err)
	}
	if on != "" {
		t.Errorf("flag=true: expected empty content (raw is text-only; reasoning surfaces via DeltaParts.Reasoning), got %q", on)
	}
}
