package buildrequest

import (
	"strings"
	"testing"
)

func TestExtractResponseContent_OpenAI(t *testing.T) {
	body := `{"id":"chatcmpl-abc","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hello world"},"finish_reason":"stop"}]}`
	text, _, err := ExtractResponseContent("openai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", text)
	}
}

func TestExtractResponseContent_OpenAICompatible(t *testing.T) {
	body := `{"choices":[{"message":{"content":"response text"}}]}`

	for _, provider := range []string{"openai", "openai-generic", "azure-openai", "ollama", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			text, _, err := ExtractResponseContent(provider, body, false)
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", provider, err)
			}
			if text != "response text" {
				t.Errorf("expected 'response text' for %s, got %q", provider, text)
			}
		})
	}
}

func TestExtractResponseContent_OpenAIEmptyContent(t *testing.T) {
	body := `{"choices":[{"message":{"content":""}}]}`
	text, _, err := ExtractResponseContent("openai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty string, got %q", text)
	}
}

func TestExtractResponseContent_OpenAINullContent(t *testing.T) {
	// Some providers return null content for function calls
	body := `{"choices":[{"message":{"content":null,"function_call":{"name":"foo"}}}]}`
	text, _, err := ExtractResponseContent("openai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty string for null content, got %q", text)
	}
}

func TestExtractResponseContent_OpenAINumericContent(t *testing.T) {
	// content is a number — unexpected type, should error
	body := `{"choices":[{"message":{"content":123}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for numeric content")
	}
}

func TestExtractResponseContent_OpenAIObjectContent(t *testing.T) {
	// content is an object (not array) — unexpected type, should error
	body := `{"choices":[{"message":{"content":{"foo":"bar"}}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for object content")
	}
}

func TestExtractResponseContent_OpenAIBoolContent(t *testing.T) {
	body := `{"choices":[{"message":{"content":true}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for boolean content")
	}
}

func TestExtractResponseContent_OpenAIRefusal(t *testing.T) {
	// message.refusal field present → error surfacing the refusal text
	body := `{"choices":[{"message":{"role":"assistant","refusal":"I cannot help with that","content":null}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for refusal response")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
}

func TestExtractResponseContent_OpenAIRefusalEmptyTopLevel(t *testing.T) {
	// message.refusal is an empty string with null content — still a refusal
	body := `{"choices":[{"message":{"refusal":"","content":null}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for empty top-level refusal")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
}

func TestExtractResponseContent_OpenAIRefusalNullFallsThrough(t *testing.T) {
	// Explicit null refusal with valid content — not a refusal, normal extraction
	body := `{"choices":[{"message":{"refusal":null,"content":"Hello world"}}]}`
	text, _, err := ExtractResponseContent("openai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", text)
	}
}

func TestExtractResponseContent_OpenAIRefusalNonString(t *testing.T) {
	// message.refusal is a non-string type — still a refusal
	body := `{"choices":[{"message":{"refusal":true,"content":null}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for non-string top-level refusal")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
}

func TestExtractResponseContent_OpenAIRefusalInContentArray(t *testing.T) {
	// {"type":"refusal"} part in content array → error
	body := `{"choices":[{"message":{"content":[{"type":"refusal","refusal":"Not allowed"}]}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for refusal content part")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
}

func TestExtractResponseContent_OpenAIRefusalEmptyText(t *testing.T) {
	// type=="refusal" with empty refusal field — still an error
	body := `{"choices":[{"message":{"content":[{"type":"refusal","refusal":""}]}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for refusal part with empty text")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
}

func TestExtractResponseContent_OpenAIRefusalMissingField(t *testing.T) {
	// type=="refusal" with no refusal field at all — still an error
	body := `{"choices":[{"message":{"content":[{"type":"refusal"}]}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for refusal part with missing field")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
}

func TestExtractResponseContent_OpenAIArrayNonObjectElement(t *testing.T) {
	// content array with a non-object element → error
	body := `{"choices":[{"message":{"content":[123]}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for non-object element in content array")
	}
}

func TestExtractResponseContent_AnthropicNonObjectElement(t *testing.T) {
	body := `{"content":["not an object"]}`
	_, _, err := ExtractResponseContent("anthropic", body, false)
	if err == nil {
		t.Fatal("expected error for non-object element in Anthropic content array")
	}
}

func TestExtractResponseContent_GeminiNonObjectElement(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":["not an object"]}}]}`
	_, _, err := ExtractResponseContent("google-ai", body, false)
	if err == nil {
		t.Fatal("expected error for non-object element in Gemini parts array")
	}
}

// ======== OpenAI Responses API tests ========

func TestExtractResponseContent_OpenAIResponses(t *testing.T) {
	body := `{"output":[{"type":"message","status":"completed","content":[{"type":"output_text","text":"Hello from Responses API"}],"role":"assistant"}]}`
	text, _, err := ExtractResponseContent("openai-responses", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello from Responses API" {
		t.Errorf("expected 'Hello from Responses API', got %q", text)
	}
}

func TestExtractResponseContent_OpenAIResponsesMultiPart(t *testing.T) {
	body := `{"output":[{"type":"message","content":[
		{"type":"output_text","text":"Part one. "},
		{"type":"output_text","text":"Part two."}
	],"role":"assistant"}]}`
	text, _, err := ExtractResponseContent("openai-responses", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Part one. Part two." {
		t.Errorf("expected 'Part one. Part two.', got %q", text)
	}
}

func TestExtractResponseContent_OpenAIResponsesWithReasoning(t *testing.T) {
	// Reasoning items should be skipped; only message items are extracted
	body := `{"output":[
		{"type":"reasoning","content":[],"summary":[]},
		{"type":"message","status":"completed","content":[
			{"type":"output_text","text":"The answer is 42"}
		],"role":"assistant"}
	]}`
	text, _, err := ExtractResponseContent("openai-responses", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "The answer is 42" {
		t.Errorf("expected 'The answer is 42', got %q", text)
	}
}

func TestExtractResponseContent_OpenAIResponsesMultipleMessageItems(t *testing.T) {
	body := `{"output":[
		{"type":"message","content":[{"type":"output_text","text":"First. "}],"role":"assistant"},
		{"type":"reasoning","content":[],"summary":[]},
		{"type":"message","content":[{"type":"output_text","text":"Second."}],"role":"assistant"}
	]}`
	text, _, err := ExtractResponseContent("openai-responses", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "First. Second." {
		t.Errorf("expected 'First. Second.', got %q", text)
	}
}

func TestExtractResponseContent_OpenAIResponsesMultipleMessageItemsMultiPart(t *testing.T) {
	body := `{"output":[
		{"type":"message","content":[
			{"type":"output_text","text":"A"},
			{"type":"output_text","text":"B"}
		],"role":"assistant"},
		{"type":"message","content":[
			{"type":"output_text","text":"C"},
			{"type":"output_text","text":"D"}
		],"role":"assistant"}
	]}`
	text, _, err := ExtractResponseContent("openai-responses", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "ABCD" {
		t.Errorf("expected 'ABCD', got %q", text)
	}
}

func TestExtractResponseContent_OpenAIResponsesNoMessageItem(t *testing.T) {
	// No message item in output → error
	body := `{"output":[{"type":"reasoning","content":[],"summary":[]}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for missing message item")
	}
}

func TestExtractResponseContent_OpenAIResponsesEmptyContent(t *testing.T) {
	// Message item with empty content array → empty string (valid)
	body := `{"output":[{"type":"message","content":[],"role":"assistant"}]}`
	text, _, err := ExtractResponseContent("openai-responses", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty string, got %q", text)
	}
}

func TestExtractResponseContent_OpenAIResponsesNonObjectOutputElement(t *testing.T) {
	// Non-object element in output array → error (not silently skipped)
	body := `{"output":[123,{"type":"message","content":[{"type":"output_text","text":"ok"}],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for non-object element in output array")
	}
}

func TestExtractResponseContent_OpenAIResponsesOutputMissingType(t *testing.T) {
	// Output item with no type field before valid message → error
	body := `{"output":[{"foo":"bar"},{"type":"message","content":[{"type":"output_text","text":"ok"}],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for output item missing type field")
	}
}

func TestExtractResponseContent_OpenAIResponsesOutputNonStringType(t *testing.T) {
	// Output item with non-string type before valid message → error
	body := `{"output":[{"type":true},{"type":"message","content":[{"type":"output_text","text":"ok"}],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for output item with non-string type field")
	}
}

func TestExtractResponseContent_OpenAIResponsesTrailingNonObjectOutputElement(t *testing.T) {
	// Non-object element AFTER a valid message item → still an error
	body := `{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}],"role":"assistant"},123]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for trailing non-object element in output array")
	}
}

func TestExtractResponseContent_OpenAIResponsesMissingOutput(t *testing.T) {
	body := `{"id":"resp_01"}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for missing output array")
	}
}

func TestExtractResponseContent_OpenAIResponsesMalformedTextField(t *testing.T) {
	// output_text with non-string text → error
	body := `{"output":[{"type":"message","content":[{"type":"output_text","text":123}],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for non-string text field")
	}
}

func TestExtractResponseContent_OpenAIResponsesNonObjectContent(t *testing.T) {
	// Non-object element in content array → error
	body := `{"output":[{"type":"message","content":["not an object"],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for non-object content element")
	}
}

func TestExtractResponseContent_OpenAIResponsesMissingContentArray(t *testing.T) {
	// Message item with no content field → error
	body := `{"output":[{"type":"message","role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for missing content array in message item")
	}
}

func TestExtractResponseContent_OpenAIResponsesRefusal(t *testing.T) {
	// Refusal-only response → error with refusal text
	body := `{"output":[{"type":"message","content":[{"type":"refusal","refusal":"I cannot help with that"}],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for refusal content part")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "I cannot help with that") {
		t.Errorf("expected refusal text in error, got: %v", err)
	}
}

func TestExtractResponseContent_OpenAIResponsesRefusalBeforeText(t *testing.T) {
	// Refusal part before output_text — refusal takes priority
	body := `{"output":[{"type":"message","content":[
		{"type":"refusal","refusal":"Not allowed"},
		{"type":"output_text","text":"Should not reach this"}
	],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for refusal part even with output_text present")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
}

func TestExtractResponseContent_OpenAIResponsesRefusalEmptyText(t *testing.T) {
	// Refusal part with empty refusal field — still an error
	body := `{"output":[{"type":"message","content":[{"type":"refusal","refusal":""}],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for refusal part with empty text")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
}

func TestExtractResponseContent_OpenAIResponsesRefusalMissingField(t *testing.T) {
	// Refusal part with no refusal field — still an error
	body := `{"output":[{"type":"message","content":[{"type":"refusal"}],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for refusal part with missing field")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected refusal error, got: %v", err)
	}
}

// ======== OpenAI Chat Completions array content tests ========

func TestExtractResponseContent_OpenAIArrayContent(t *testing.T) {
	// Multimodal / structured content: content is an array of typed parts
	body := `{"choices":[{"message":{"content":[
		{"type":"text","text":"Hello"},
		{"type":"text","text":" world"}
	]}}]}`
	text, _, err := ExtractResponseContent("openai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", text)
	}
}

func TestExtractResponseContent_OpenAIArrayContentMixed(t *testing.T) {
	// Array with non-text parts (image_url) — only text parts extracted
	body := `{"choices":[{"message":{"content":[
		{"type":"text","text":"Before image. "},
		{"type":"image_url","image_url":{"url":"https://example.com/img.png"}},
		{"type":"text","text":"After image."}
	]}}]}`
	text, _, err := ExtractResponseContent("openai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Before image. After image." {
		t.Errorf("expected 'Before image. After image.', got %q", text)
	}
}

func TestExtractResponseContent_OpenAIArrayContentMalformedTextField(t *testing.T) {
	// text field is a number instead of string — should error
	body := `{"choices":[{"message":{"content":[{"type":"text","text":123}]}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for numeric text field in content part")
	}
}

func TestExtractResponseContent_OpenAIArrayContentSingleText(t *testing.T) {
	body := `{"choices":[{"message":{"content":[{"type":"text","text":"only text"}]}}]}`
	text, _, err := ExtractResponseContent("openai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "only text" {
		t.Errorf("expected 'only text', got %q", text)
	}
}

func TestExtractResponseContent_OpenAIArrayContentMissingType(t *testing.T) {
	// Content part with no type field → error (not silently skipped)
	body := `{"choices":[{"message":{"content":[{"text":"ok"}]}}]}`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for content part missing type field")
	}
}

func TestExtractResponseContent_AnthropicMissingBlockType(t *testing.T) {
	body := `{"content":[{"text":"ok"}]}`
	_, _, err := ExtractResponseContent("anthropic", body, false)
	if err == nil {
		t.Fatal("expected error for Anthropic content block missing type field")
	}
}

func TestExtractResponseContent_OpenAIResponsesMissingContentType(t *testing.T) {
	body := `{"output":[{"type":"message","content":[{"text":"ok"}],"role":"assistant"}]}`
	_, _, err := ExtractResponseContent("openai-responses", body, false)
	if err == nil {
		t.Fatal("expected error for Responses API content entry missing type field")
	}
}

func TestExtractResponseContent_OpenAIArrayContentEmpty(t *testing.T) {
	body := `{"choices":[{"message":{"content":[]}}]}`
	text, _, err := ExtractResponseContent("openai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty string for empty content array, got %q", text)
	}
}

func TestExtractResponseContent_Anthropic(t *testing.T) {
	body := `{
		"id": "msg_01",
		"type": "message",
		"role": "assistant",
		"content": [
			{"type": "text", "text": "Hello from Anthropic"}
		],
		"stop_reason": "end_turn"
	}`
	text, _, err := ExtractResponseContent("anthropic", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello from Anthropic" {
		t.Errorf("expected 'Hello from Anthropic', got %q", text)
	}
}

func TestExtractResponseContent_AnthropicMultipleTextBlocks(t *testing.T) {
	body := `{
		"content": [
			{"type": "text", "text": "First part. "},
			{"type": "text", "text": "Second part."}
		]
	}`
	text, _, err := ExtractResponseContent("anthropic", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "First part. Second part." {
		t.Errorf("expected concatenated text, got %q", text)
	}
}

func TestExtractResponseContent_AnthropicWithThinking_Default(t *testing.T) {
	// Default (includeThinking=false, BAML-aligned): parseable AND raw
	// contain only the text block. Thinking blocks are dropped from raw.
	body := `{
		"content": [
			{"type": "thinking", "thinking": "Let me think about this..."},
			{"type": "text", "text": "The answer is 42"}
		]
	}`
	parseable, raw, err := ExtractResponseContent("anthropic", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parseable != "The answer is 42" {
		t.Errorf("expected parseable 'The answer is 42', got %q", parseable)
	}
	if raw != "The answer is 42" {
		t.Errorf("expected raw 'The answer is 42' (thinking dropped), got %q", raw)
	}
}

func TestExtractResponseContent_AnthropicWithThinking_OptIn(t *testing.T) {
	// Opt-in (includeThinking=true): parseable contains only text (the BAML
	// parser must never see thinking content), raw contains thinking + text.
	body := `{
		"content": [
			{"type": "thinking", "thinking": "Let me think about this..."},
			{"type": "text", "text": "The answer is 42"}
		]
	}`
	parseable, raw, err := ExtractResponseContent("anthropic", body, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parseable != "The answer is 42" {
		t.Errorf("expected parseable 'The answer is 42', got %q", parseable)
	}
	if raw != "Let me think about this...The answer is 42" {
		t.Errorf("expected raw 'Let me think about this...The answer is 42', got %q", raw)
	}
}

func TestExtractResponseContent_AnthropicWithThinking_ParseableInvariant(t *testing.T) {
	// Parseable must never include thinking content, regardless of the
	// includeThinking flag value. Codifies the structural guarantee.
	body := `{
		"content": [
			{"type": "thinking", "thinking": "Let me think about this..."},
			{"type": "text", "text": "The answer is 42"}
		]
	}`
	parseableOff, _, err := ExtractResponseContent("anthropic", body, false)
	if err != nil {
		t.Fatalf("flag=false extraction failed: %v", err)
	}
	parseableOn, _, err := ExtractResponseContent("anthropic", body, true)
	if err != nil {
		t.Fatalf("flag=true extraction failed: %v", err)
	}
	if parseableOff != parseableOn {
		t.Errorf("parseable diverged across flag values: off=%q on=%q",
			parseableOff, parseableOn)
	}
	if parseableOff != "The answer is 42" {
		t.Errorf("expected parseable 'The answer is 42', got %q", parseableOff)
	}
}

func TestExtractResponseContent_AnthropicThinkingOnly_Default(t *testing.T) {
	// Default: thinking-only response is empty in both parseable and raw.
	body := `{
		"content": [
			{"type": "thinking", "thinking": "Reasoning without output"}
		]
	}`
	parseable, raw, err := ExtractResponseContent("anthropic", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parseable != "" {
		t.Errorf("expected empty parseable, got %q", parseable)
	}
	if raw != "" {
		t.Errorf("expected empty raw under default flag, got %q", raw)
	}
}

func TestExtractResponseContent_AnthropicThinkingOnly_OptIn(t *testing.T) {
	// Opt-in: thinking-only response has empty parseable and the thinking in raw.
	body := `{
		"content": [
			{"type": "thinking", "thinking": "Reasoning without output"}
		]
	}`
	parseable, raw, err := ExtractResponseContent("anthropic", body, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parseable != "" {
		t.Errorf("expected empty parseable, got %q", parseable)
	}
	if raw != "Reasoning without output" {
		t.Errorf("expected raw thinking content under opt-in, got %q", raw)
	}
}

func TestExtractResponseContent_AnthropicMalformedTextField(t *testing.T) {
	body := `{"content":[{"type":"text","text":123}]}`
	_, _, err := ExtractResponseContent("anthropic", body, false)
	if err == nil {
		t.Fatal("expected error for numeric text field in Anthropic block")
	}
}

func TestExtractResponseContent_AnthropicMalformedThinkingField(t *testing.T) {
	body := `{"content":[{"type":"thinking","thinking":true}]}`
	_, _, err := ExtractResponseContent("anthropic", body, false)
	if err == nil {
		t.Fatal("expected error for boolean thinking field in Anthropic block")
	}
}

func TestExtractResponseContent_AnthropicEmptyContent(t *testing.T) {
	body := `{"content": []}`
	text, _, err := ExtractResponseContent("anthropic", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty string, got %q", text)
	}
}

func TestExtractResponseContent_AnthropicMissingContent(t *testing.T) {
	// Non-empty body but no content array → error (malformed Anthropic response)
	body := `{"id": "msg_01"}`
	_, _, err := ExtractResponseContent("anthropic", body, false)
	if err == nil {
		t.Fatal("expected error for missing content array in non-empty body")
	}
}

func TestExtractResponseContent_GoogleAI(t *testing.T) {
	body := `{
		"candidates": [
			{
				"content": {
					"parts": [{"text": "Hello from Gemini"}],
					"role": "model"
				},
				"finishReason": "STOP"
			}
		]
	}`
	text, _, err := ExtractResponseContent("google-ai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello from Gemini" {
		t.Errorf("expected 'Hello from Gemini', got %q", text)
	}
}

func TestExtractResponseContent_VertexAI(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"Vertex response"}],"role":"model"}}]}`
	text, _, err := ExtractResponseContent("vertex-ai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Vertex response" {
		t.Errorf("expected 'Vertex response', got %q", text)
	}
}

func TestExtractResponseContent_GeminiMultipleParts(t *testing.T) {
	// Gemini can return multiple text parts in a single response
	body := `{"candidates":[{"content":{"parts":[
		{"text":"First paragraph. "},
		{"text":"Second paragraph."}
	],"role":"model"}}]}`
	text, _, err := ExtractResponseContent("google-ai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "First paragraph. Second paragraph." {
		t.Errorf("expected concatenated text, got %q", text)
	}
}

func TestExtractResponseContent_GeminiMixedParts(t *testing.T) {
	// Parts may include non-text entries (functionCall, executableCode)
	body := `{"candidates":[{"content":{"parts":[
		{"text":"Here is the result: "},
		{"functionCall":{"name":"get_weather","args":{"city":"NYC"}}},
		{"text":"Done."}
	],"role":"model"}}]}`
	text, _, err := ExtractResponseContent("google-ai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Here is the result: Done." {
		t.Errorf("expected 'Here is the result: Done.', got %q", text)
	}
}

func TestExtractResponseContent_GeminiMalformedTextField(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":123}]}}]}`
	_, _, err := ExtractResponseContent("google-ai", body, false)
	if err == nil {
		t.Fatal("expected error for numeric text field in Gemini part")
	}
}

func TestExtractResponseContent_GeminiObjectTextField(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":{"a":1}}]}}]}`
	_, _, err := ExtractResponseContent("google-ai", body, false)
	if err == nil {
		t.Fatal("expected error for object text field in Gemini part")
	}
}

func TestExtractResponseContent_GeminiEmptyParts(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[],"role":"model"}}]}`
	text, _, err := ExtractResponseContent("google-ai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty string for empty parts, got %q", text)
	}
}

func TestExtractResponseContent_GeminiMissingParts(t *testing.T) {
	// Non-empty body but parts array is missing → error
	body := `{"candidates":[{"content":{"role":"model"}}]}`
	_, _, err := ExtractResponseContent("google-ai", body, false)
	if err == nil {
		t.Fatal("expected error for missing parts in non-empty body")
	}
}

func TestExtractResponseContent_VertexMultipleParts(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[
		{"text":"Part A"},
		{"text":"Part B"}
	],"role":"model"}}]}`
	text, _, err := ExtractResponseContent("vertex-ai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Part APart B" {
		t.Errorf("expected 'Part APart B', got %q", text)
	}
}

func TestExtractResponseContent_UnsupportedProvider(t *testing.T) {
	_, _, err := ExtractResponseContent("aws-bedrock", `{}`, false)
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
}

func TestExtractResponseContent_UnknownProvider(t *testing.T) {
	_, _, err := ExtractResponseContent("some-unknown-provider", `{}`, false)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestExtractResponseContent_MalformedJSON(t *testing.T) {
	// Non-empty body that isn't valid JSON → error (can't extract content)
	for _, provider := range []string{"openai", "anthropic", "google-ai"} {
		t.Run(provider, func(t *testing.T) {
			_, _, err := ExtractResponseContent(provider, `{invalid json}`, false)
			if err == nil {
				t.Fatalf("expected error for malformed JSON with provider %s", provider)
			}
		})
	}
}

func TestExtractResponseContent_TrailingGarbage(t *testing.T) {
	// Valid JSON followed by trailing garbage — gjson would accept this
	// but gjson.Valid rejects it. Should error, not false-success.
	body := `{"choices":[{"message":{"content":"ok"}}]} garbage`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for JSON with trailing garbage")
	}
}

func TestExtractResponseContent_TruncatedJSON(t *testing.T) {
	// Truncated JSON that gjson can still partially query — should error.
	body := `{"choices":[{"message":{"content":"partial`
	_, _, err := ExtractResponseContent("openai", body, false)
	if err == nil {
		t.Fatal("expected error for truncated JSON")
	}
}

func TestExtractResponseContent_MissingExpectedFields(t *testing.T) {
	// Non-empty valid JSON but missing the provider's expected fields → error
	tests := []struct {
		provider string
		body     string
	}{
		{"openai", `{"id":"chatcmpl-abc","object":"chat.completion"}`},
		{"anthropic", `{"id":"msg_01","type":"message"}`},
		{"google-ai", `{"usageMetadata":{"totalTokenCount":10}}`},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			_, _, err := ExtractResponseContent(tt.provider, tt.body, false)
			if err == nil {
				t.Fatalf("expected error for %s with missing fields", tt.provider)
			}
		})
	}
}

func TestExtractResponseContent_EmptyBody(t *testing.T) {
	// An LLM provider should never return a valid 200 with an empty body.
	for _, provider := range []string{"openai", "anthropic", "google-ai"} {
		t.Run(provider, func(t *testing.T) {
			_, _, err := ExtractResponseContent(provider, "", false)
			if err == nil {
				t.Fatalf("expected error for empty body with provider %s", provider)
			}
		})
	}
}

func TestExtractResponseContent_WhitespaceOnlyBody(t *testing.T) {
	_, _, err := ExtractResponseContent("openai", "   \n\t  ", false)
	if err == nil {
		t.Fatal("expected error for whitespace-only body")
	}
}

// TestCallProviderWhitelistMatchesExtractor verifies that every provider
// in callSupportedProviders has a corresponding case in
// ExtractResponseContent. This prevents desynchronization.
func TestCallProviderWhitelistMatchesExtractor(t *testing.T) {
	// Use a realistic response body that works for all OpenAI-compatible providers
	testBodies := map[string]string{
		"openai":           `{"choices":[{"message":{"content":"test"}}]}`,
		"openai-generic":   `{"choices":[{"message":{"content":"test"}}]}`,
		"azure-openai":     `{"choices":[{"message":{"content":"test"}}]}`,
		"ollama":           `{"choices":[{"message":{"content":"test"}}]}`,
		"openrouter":       `{"choices":[{"message":{"content":"test"}}]}`,
		"openai-responses": `{"output":[{"type":"message","content":[{"type":"output_text","text":"test"}],"role":"assistant"}]}`,
		"anthropic":        `{"content":[{"type":"text","text":"test"}]}`,
		"google-ai":        `{"candidates":[{"content":{"parts":[{"text":"test"}]}}]}`,
		"vertex-ai":        `{"candidates":[{"content":{"parts":[{"text":"test"}]}}]}`,
	}

	for provider := range callSupportedProviders {
		t.Run(provider, func(t *testing.T) {
			body, ok := testBodies[provider]
			if !ok {
				t.Fatalf("provider %q in callSupportedProviders but no test body defined", provider)
			}
			parseable, _, err := ExtractResponseContent(provider, body, false)
			if err != nil {
				t.Fatalf("ExtractResponseContent(%q) returned error: %v", provider, err)
			}
			if parseable != "test" {
				t.Errorf("ExtractResponseContent(%q) parseable = %q, expected 'test'", provider, parseable)
			}
		})
	}
}
