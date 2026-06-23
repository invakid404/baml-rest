package sse

import "testing"

// extractParityCase is one provider + event-JSON fixture run through both the
// string entry point (ExtractDeltaPartsFromText / gjson.Get) and the byte
// entry point (ExtractDeltaPartsFromBytes / gjson.GetBytes). Both must produce
// identical DeltaParts and identical error behaviour.
type extractParityCase struct {
	name     string
	provider string
	json     string
}

var extractParityCases = []extractParityCase{
	// OpenAI / openai-compat (Chat Completions)
	{"openai_text", "openai", `{"choices":[{"delta":{"content":"hello"}}]}`},
	{"openai_empty", "openai", `{"choices":[{"delta":{}}]}`},
	{"openai_reasoning", "openai", `{"choices":[{"delta":{"content":"hi","reasoning_content":"because"}}]}`},
	{"openai_escaped", "openai", `{"choices":[{"delta":{"content":"line1\nline2 \"q\" é"}}]}`},
	{"azure_text", "azure-openai", `{"choices":[{"delta":{"content":"az"}}]}`},
	{"ollama_text", "ollama", `{"choices":[{"delta":{"content":"ol"}}]}`},
	{"openrouter_text", "openrouter", `{"choices":[{"delta":{"content":"or"}}]}`},

	// OpenAI Responses
	{"responses_text", "openai-responses", `{"type":"response.output_text.delta","delta":"chunk"}`},
	{"responses_reasoning", "openai-responses", `{"type":"response.reasoning_summary_text.delta","delta":"think"}`},
	{"responses_other", "openai-responses", `{"type":"response.completed"}`},
	{"responses_nonstring_delta", "openai-responses", `{"type":"response.output_text.delta","delta":123}`},

	// Anthropic
	{"anthropic_text", "anthropic", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ab"}}`},
	{"anthropic_thinking", "anthropic", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}`},
	{"anthropic_other", "anthropic", `{"type":"message_start"}`},
	{"anthropic_escaped", "anthropic", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"a\tb☃"}}`},

	// Google / Vertex
	{"google_text", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"g1"},{"text":"g2"}]}}]}`},
	{"google_thought", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"think","thought":true},{"text":"answer"}]}}]}`},
	{"google_empty", "google-ai", `{"candidates":[{"content":{"parts":[]}}]}`},
	{"google_missing", "google-ai", `{"candidates":[]}`},
	{"vertex_text", "vertex-ai", `{"candidates":[{"content":{"parts":[{"text":"v"}]}}]}`},

	// AWS Bedrock (debug-string format)
	{"bedrock_text", "aws-bedrock", `{"debug":"ContentBlockDelta { delta: Some(Text(\"hi\")) }"}`},
	{"bedrock_nondelta", "aws-bedrock", `{"debug":"MessageStart { role: Assistant }"}`},
}

func TestExtractDeltaPartsFromBytesMatchesString(t *testing.T) {
	for _, includeReasoning := range []bool{false, true} {
		for _, tc := range extractParityCases {
			name := tc.name
			if includeReasoning {
				name += "_reasoning_on"
			}
			t.Run(name, func(t *testing.T) {
				strParts, strErr := ExtractDeltaPartsFromText(tc.provider, tc.json, includeReasoning)
				bytParts, bytErr := ExtractDeltaPartsFromBytes(tc.provider, []byte(tc.json), includeReasoning)

				if (strErr == nil) != (bytErr == nil) {
					t.Fatalf("error mismatch: string=%v bytes=%v", strErr, bytErr)
				}
				if strParts != bytParts {
					t.Errorf("DeltaParts mismatch (includeReasoning=%v):\n string=%#v\n  bytes=%#v",
						includeReasoning, strParts, bytParts)
				}
			})
		}
	}
}

// TestExtractDeltaPartsFromBytesUnsupportedProvider confirms the byte path
// errors identically to the string path for an unknown provider.
func TestExtractDeltaPartsFromBytesUnsupportedProvider(t *testing.T) {
	_, strErr := ExtractDeltaPartsFromText("made-up", `{}`, false)
	_, bytErr := ExtractDeltaPartsFromBytes("made-up", []byte(`{}`), false)
	if strErr == nil || bytErr == nil {
		t.Fatalf("expected errors for unsupported provider, got string=%v bytes=%v", strErr, bytErr)
	}
}
