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

// TestIncrementalExtractor_AnthropicThinking_ParseableInvariant exercises the
// extractor end-to-end with a thinking-then-text Anthropic stream and asserts
// the structural guarantee at the buffer level: regardless of the
// IncludeThinkingInRaw flag, the parseable buffer is text-only. Under opt-in
// the raw buffer additionally carries thinking content; under default both
// buffers are equal.
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

	// Default flag: both buffers carry text only.
	extOff := NewIncrementalExtractor(false)
	rOff := extOff.Extract(1, "anthropic", chunks)
	if rOff.ParseableFull != "The answer is 42" {
		t.Errorf("default: ParseableFull = %q, want %q", rOff.ParseableFull, "The answer is 42")
	}
	if rOff.RawFull != "The answer is 42" {
		t.Errorf("default: RawFull = %q, want %q (text-only)", rOff.RawFull, "The answer is 42")
	}

	// Opt-in: parseable still text-only, raw carries thinking + text.
	extOn := NewIncrementalExtractor(true)
	rOn := extOn.Extract(1, "anthropic", chunks)
	if rOn.ParseableFull != "The answer is 42" {
		t.Errorf("opt-in: ParseableFull = %q, want %q (parseable invariant violated!)", rOn.ParseableFull, "The answer is 42")
	}
	expectedRaw := "Let me reason...The answer  still thinkingis 42"
	if rOn.RawFull != expectedRaw {
		t.Errorf("opt-in: RawFull = %q, want %q", rOn.RawFull, expectedRaw)
	}

	// Cross-flag invariant: parseable is byte-identical regardless of flag.
	if rOff.ParseableFull != rOn.ParseableFull {
		t.Errorf("Parseable diverged across flag values: off=%q on=%q",
			rOff.ParseableFull, rOn.ParseableFull)
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
