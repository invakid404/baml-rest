package buildrequest

import (
	"strings"
	"testing"
)

// TestExtractResponseContentBytesMatchesString is the golden parity test for
// the byte-oriented unary extractor. For every (provider, body,
// includeReasoning) fixture it asserts ExtractResponseContentBytes (the
// gjson.GetBytes / gjson.ValidBytes path used by the net/http unary lane)
// returns byte-for-byte identical parseable / raw / reasoning text and the
// same error shape as the string-based ExtractResponseContent. If these two
// ever diverge the net/http unary path would silently extract different
// content than the fasthttp path, so this is the contract that lets the
// orchestrator route BodyBytes through the byte extractor safely.
func TestExtractResponseContentBytesMatchesString(t *testing.T) {
	cases := []struct {
		name             string
		provider         string
		body             string
		includeReasoning bool
	}{
		// --- OpenAI Chat Completions ---
		{"openai/scalar", "openai", `{"choices":[{"message":{"role":"assistant","content":"Hello world"}}]}`, false},
		{"openai/unicode-escaped", "openai", `{"choices":[{"message":{"content":"café — naïve 😀 \"quoted\"\n\ttabbed"}}]}`, false},
		{"openai/unescaped-plain", "openai", `{"choices":[{"message":{"content":"plain ascii no escapes at all"}}]}`, false},
		{"openai/array-parts", "openai", `{"choices":[{"message":{"content":[{"type":"text","text":"First. "},{"type":"text","text":"Second."}]}}]}`, false},
		{"openai/null-content", "openai", `{"choices":[{"message":{"content":null}}]}`, false},
		{"openai/reasoning-off", "openai", `{"choices":[{"message":{"content":"answer","reasoning_content":"my hidden thoughts"}}]}`, false},
		{"openai/reasoning-on", "openai", `{"choices":[{"message":{"content":"answer","reasoning_content":"my hidden thoughts"}}]}`, true},
		{"openai/refusal", "openai", `{"choices":[{"message":{"refusal":"I can't help with that"}}]}`, false},
		{"openai/refusal-in-array", "openai", `{"choices":[{"message":{"content":[{"type":"refusal","refusal":"nope"}]}}]}`, false},
		{"openai/missing-content", "openai", `{"choices":[{"message":{}}]}`, false},
		{"openai/non-object-array-el", "openai", `{"choices":[{"message":{"content":["bare string"]}}]}`, false},

		// openai-compatible aliases
		{"openrouter/scalar", "openrouter", `{"choices":[{"message":{"content":"router text"}}]}`, false},
		{"ollama/scalar", "ollama", `{"choices":[{"message":{"content":"ollama text"}}]}`, false},

		// --- Anthropic Messages ---
		{"anthropic/text", "anthropic", `{"content":[{"type":"text","text":"Hello from Anthropic"}]}`, false},
		{"anthropic/multi-text", "anthropic", `{"content":[{"type":"text","text":"First part. "},{"type":"text","text":"Second part."}]}`, false},
		{"anthropic/thinking-off", "anthropic", `{"content":[{"type":"thinking","thinking":"deep thoughts"},{"type":"text","text":"final answer"}]}`, false},
		{"anthropic/thinking-on", "anthropic", `{"content":[{"type":"thinking","thinking":"deep thoughts"},{"type":"text","text":"final answer"}]}`, true},
		{"anthropic/escaped", "anthropic", `{"content":[{"type":"text","text":"line1\nline2 é \"q\""}]}`, true},
		{"anthropic/missing-content", "anthropic", `{"id":"msg_01"}`, false},
		{"anthropic/missing-block-type", "anthropic", `{"content":[{"text":"no type"}]}`, false},

		// --- OpenAI Responses ---
		{"responses/message", "openai-responses", `{"output":[{"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"The response text."}]}]}`, false},
		{"responses/multi-part", "openai-responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"part A "},{"type":"output_text","text":"part B"}]}]}`, false},
		{"responses/reasoning-on", "openai-responses", `{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"summary one"},{"type":"summary_text","text":" summary two"}]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`, true},
		{"responses/reasoning-off", "openai-responses", `{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"summary one"}]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`, false},
		{"responses/refusal", "openai-responses", `{"output":[{"type":"message","content":[{"type":"refusal","refusal":"declined"}]}]}`, false},
		{"responses/no-message", "openai-responses", `{"output":[{"type":"reasoning","summary":[]}]}`, false},
		{"responses/missing-output", "openai-responses", `{"id":"resp_1"}`, false},

		// --- Google / Vertex (Gemini) ---
		{"google/text", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"Gemini answer"}]}}]}`, false},
		{"vertex/text", "vertex-ai", `{"candidates":[{"content":{"parts":[{"text":"Vertex answer"}]}}]}`, false},
		{"google/multi-part", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"a "},{"text":"b"}]}}]}`, false},
		{"google/thought-off", "google-ai", `{"candidates":[{"content":{"parts":[{"thought":true,"text":"thinking"},{"text":"answer"}]}}]}`, false},
		{"google/thought-on", "google-ai", `{"candidates":[{"content":{"parts":[{"thought":true,"text":"thinking"},{"text":"answer"}]}}]}`, true},
		{"google/escaped", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"über \"x\"\n"}]}}]}`, true},
		{"google/missing-parts", "google-ai", `{"candidates":[{"content":{}}]}`, false},

		// --- AWS Bedrock Converse ---
		{"bedrock/text", "aws-bedrock", `{"output":{"message":{"role":"assistant","content":[{"text":"bedrock answer"}]}}}`, false},
		{"bedrock/reasoning-on", "aws-bedrock", `{"output":{"message":{"content":[{"reasoningContent":{"reasoningText":{"text":"bedrock thinking","signature":"sig"}}},{"text":"answer"}]}}}`, true},
		{"bedrock/reasoning-off", "aws-bedrock", `{"output":{"message":{"content":[{"reasoningContent":{"reasoningText":{"text":"bedrock thinking"}}},{"text":"answer"}]}}}`, false},
		{"bedrock/escaped", "aws-bedrock", `{"output":{"message":{"content":[{"text":"résumé \"q\"\t"}]}}}`, false},
		{"bedrock/no-blocks", "aws-bedrock", `{"output":{"message":{"content":[{"toolUse":{"name":"x"}}]}}}`, false},
		{"bedrock/missing-message", "aws-bedrock", `{"output":{}}`, false},

		// --- envelope / validation edge cases ---
		{"unsupported-provider", "totally-unknown", `{"x":1}`, false},
		{"invalid-json", "openai", `{"choices":[`, false},
		{"empty-body", "openai", ``, false},
		{"whitespace-only", "openai", "   \n\t  ", false},
		// A stream sentinel is not valid JSON for the unary path; both paths
		// must reject it identically.
		{"done-sentinel", "openai", `[DONE]`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sP, sRaw, sReason, sErr := ExtractResponseContent(tc.provider, tc.body, tc.includeReasoning)
			bP, bRaw, bReason, bErr := ExtractResponseContentBytes(tc.provider, []byte(tc.body), tc.includeReasoning)

			if (sErr == nil) != (bErr == nil) {
				t.Fatalf("error presence mismatch: string err=%v, bytes err=%v", sErr, bErr)
			}
			if sErr != nil && bErr != nil && sErr.Error() != bErr.Error() {
				t.Fatalf("error text mismatch:\n string: %q\n bytes:  %q", sErr.Error(), bErr.Error())
			}
			if sP != bP {
				t.Errorf("parseable mismatch:\n string: %q\n bytes:  %q", sP, bP)
			}
			if sRaw != bRaw {
				t.Errorf("raw mismatch:\n string: %q\n bytes:  %q", sRaw, bRaw)
			}
			if sReason != bReason {
				t.Errorf("reasoning mismatch:\n string: %q\n bytes:  %q", sReason, bReason)
			}
			// raw must equal parseable for every provider here (text-only by
			// construction) — guard that the byte path preserves that too.
			if bErr == nil && bRaw != bP {
				t.Errorf("byte path raw != parseable: raw=%q parseable=%q", bRaw, bP)
			}
		})
	}
}

// TestExtractResponseContentBytes_AliasingSafety guards the gjson.GetBytes
// ownership contract: the strings returned by the byte extractor must remain
// valid (and unchanged) after the source []byte is overwritten. getBytes
// copies matched Raw/Str out of the byte view before returning, so mutating
// the input afterward must not corrupt the extracted text. If this ever
// regressed (e.g. switching to an aliasing parser), the assertions below
// would observe garbage after the overwrite.
func TestExtractResponseContentBytes_AliasingSafety(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"durable content here"}}]}`)
	parseable, raw, _, err := ExtractResponseContentBytes("openai", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "durable content here"
	if parseable != want {
		t.Fatalf("parseable = %q, want %q", parseable, want)
	}
	// Scribble over the entire source buffer.
	for i := range body {
		body[i] = 'Z'
	}
	if parseable != want {
		t.Errorf("parseable corrupted after source overwrite: got %q, want %q", parseable, want)
	}
	if raw != want {
		t.Errorf("raw corrupted after source overwrite: got %q, want %q", raw, want)
	}
}

// BenchmarkUnaryBodyHandling_OldVsNew models the body-handling cost of the
// net/http unary path before and after this change, starting from the
// io.ReadAll buffer that every response already allocates:
//
//   - old: Execute did string(body) — a whole-body []byte->string copy
//     (B/op scales with the full body) — then string extraction aliased it.
//   - new: Execute exposes BodyBytes via a zero-copy view (bytesToBodyString,
//     0 allocs — see llmhttp.TestBytesToBodyStringZeroCopy) and extraction
//     runs straight off the bytes via gjson.GetBytes, copying out only the
//     extracted content rather than the whole body.
//
// The large non-content envelope (usage/ids/metadata) makes the dropped
// whole-body allocation visible: old/B/op tracks the full body, new/B/op
// tracks only the extracted content. Run with -benchmem.
func BenchmarkUnaryBodyHandling_OldVsNew(b *testing.B) {
	// Realistic-ish envelope: a short completion wrapped in a large metadata
	// body, so the whole-body copy is much bigger than the extracted content.
	envelope := strings.Repeat(`"k`+strings.Repeat("x", 40)+`":"v`+strings.Repeat("y", 40)+`",`, 64)
	body := []byte(`{"id":"chatcmpl-abc","object":"chat.completion","usage":{` + envelope +
		`"total":1},"choices":[{"index":0,"message":{"role":"assistant","content":"Hello world"}}]}`)

	b.Run("old/wholebody-copy+string-extract", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s := string(body) // the whole-body copy Execute used to do
			if _, _, _, err := ExtractResponseContent("openai", s, false); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("new/zerocopy+bytes-extract", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			// Execute's bytesToBodyString view is free; extraction reads
			// BodyBytes directly.
			if _, _, _, err := ExtractResponseContentBytes("openai", body, false); err != nil {
				b.Fatal(err)
			}
		}
	})
}
