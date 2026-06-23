package buildrequest

import (
	"strings"
	"testing"
	"unsafe"
)

// stringAliasesBuffer reports whether s's backing array points INTO buf's
// backing array — i.e. s is a zero-copy view over (a slice of) buf rather than
// an owned copy. This is the teeth of the Stage-3 aliasing tests: the borrowed
// extractor must alias unescaped single-segment content (so our side copies
// nothing) and must NOT alias escaped or multi-segment content (those are
// data-mandated copies).
func stringAliasesBuffer(s string, buf []byte) bool {
	if len(s) == 0 || len(buf) == 0 {
		return false
	}
	sp := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	bp := uintptr(unsafe.Pointer(unsafe.SliceData(buf)))
	return sp >= bp && sp < bp+uintptr(len(buf))
}

// TestExtractResponseContentBorrowedMatchesString is the golden parity test for
// the aliasing borrowed extractor (the fasthttp borrow lane). For every
// fixture it asserts ExtractResponseContentBorrowed returns byte-for-byte
// identical parseable / raw / reasoning text and the same error shape as the
// owned string extractor ExtractResponseContent. The single-segment fast-path
// and the unsafe string view must never change the extracted VALUE — only
// whether it aliases — so this pins that the borrow lane and the string lane
// can never silently diverge.
func TestExtractResponseContentBorrowedMatchesString(t *testing.T) {
	cases := []struct {
		name             string
		provider         string
		body             string
		includeReasoning bool
	}{
		// OpenAI Chat Completions
		{"openai/scalar", "openai", `{"choices":[{"message":{"role":"assistant","content":"Hello world"}}]}`, false},
		{"openai/escaped", "openai", `{"choices":[{"message":{"content":"café — naïve 😀 \"quoted\"\n\ttabbed"}}]}`, false},
		{"openai/array-parts", "openai", `{"choices":[{"message":{"content":[{"type":"text","text":"First. "},{"type":"text","text":"Second."}]}}]}`, false},
		{"openai/array-single", "openai", `{"choices":[{"message":{"content":[{"type":"text","text":"only part"}]}}]}`, false},
		{"openai/null-content", "openai", `{"choices":[{"message":{"content":null}}]}`, false},
		{"openai/reasoning-on", "openai", `{"choices":[{"message":{"content":"answer","reasoning_content":"my hidden thoughts"}}]}`, true},
		{"openai/refusal", "openai", `{"choices":[{"message":{"refusal":"I can't help with that"}}]}`, false},
		{"openrouter/scalar", "openrouter", `{"choices":[{"message":{"content":"router text"}}]}`, false},

		// Anthropic Messages
		{"anthropic/text", "anthropic", `{"content":[{"type":"text","text":"Hello from Anthropic"}]}`, false},
		{"anthropic/multi-text", "anthropic", `{"content":[{"type":"text","text":"First part. "},{"type":"text","text":"Second part."}]}`, false},
		{"anthropic/thinking-on", "anthropic", `{"content":[{"type":"thinking","thinking":"deep thoughts"},{"type":"text","text":"final answer"}]}`, true},
		{"anthropic/escaped", "anthropic", `{"content":[{"type":"text","text":"line1\nline2 é \"q\""}]}`, true},
		{"anthropic/missing-content", "anthropic", `{"id":"msg_01"}`, false},

		// OpenAI Responses
		{"responses/message", "openai-responses", `{"output":[{"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"The response text."}]}]}`, false},
		{"responses/multi-part", "openai-responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"part A "},{"type":"output_text","text":"part B"}]}]}`, false},
		{"responses/reasoning-on", "openai-responses", `{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"summary one"}]},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`, true},
		{"responses/no-message", "openai-responses", `{"output":[{"type":"reasoning","summary":[]}]}`, false},

		// Google / Vertex (Gemini)
		{"google/text", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"Gemini answer"}]}}]}`, false},
		{"google/multi-part", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"a "},{"text":"b"}]}}]}`, false},
		{"google/thought-on", "google-ai", `{"candidates":[{"content":{"parts":[{"thought":true,"text":"thinking"},{"text":"answer"}]}}]}`, true},
		{"google/escaped", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"über \"x\"\n"}]}}]}`, true},

		// AWS Bedrock Converse
		{"bedrock/text", "aws-bedrock", `{"output":{"message":{"role":"assistant","content":[{"text":"bedrock answer"}]}}}`, false},
		{"bedrock/reasoning-on", "aws-bedrock", `{"output":{"message":{"content":[{"reasoningContent":{"reasoningText":{"text":"bedrock thinking","signature":"sig"}}},{"text":"answer"}]}}}`, true},
		{"bedrock/multi-text", "aws-bedrock", `{"output":{"message":{"content":[{"text":"one "},{"text":"two"}]}}}`, false},
		{"bedrock/missing-message", "aws-bedrock", `{"output":{}}`, false},

		// envelope / validation edge cases
		{"unsupported-provider", "totally-unknown", `{"x":1}`, false},
		{"invalid-json", "openai", `{"choices":[`, false},
		{"empty-body", "openai", ``, false},
		{"whitespace-only", "openai", "   \n\t  ", false},
		{"done-sentinel", "openai", `[DONE]`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sP, sRaw, sReason, sErr := ExtractResponseContent(tc.provider, tc.body, tc.includeReasoning)
			bP, bRaw, bReason, bErr := ExtractResponseContentBorrowed(tc.provider, []byte(tc.body), tc.includeReasoning)

			if (sErr == nil) != (bErr == nil) {
				t.Fatalf("error presence mismatch: string err=%v, borrowed err=%v", sErr, bErr)
			}
			if sErr != nil && bErr != nil && sErr.Error() != bErr.Error() {
				t.Fatalf("error text mismatch:\n string:   %q\n borrowed: %q", sErr.Error(), bErr.Error())
			}
			if sP != bP {
				t.Errorf("parseable mismatch:\n string:   %q\n borrowed: %q", sP, bP)
			}
			if sRaw != bRaw {
				t.Errorf("raw mismatch:\n string:   %q\n borrowed: %q", sRaw, bRaw)
			}
			if sReason != bReason {
				t.Errorf("reasoning mismatch:\n string:   %q\n borrowed: %q", sReason, bReason)
			}
			// raw must equal parseable for every provider (text-only by
			// construction) — guard that the borrow path preserves that too.
			if bErr == nil && bRaw != bP {
				t.Errorf("borrowed raw != parseable: raw=%q parseable=%q", bRaw, bP)
			}
		})
	}
}

// TestExtractResponseContentBorrowed_Aliasing pins the actual zero-copy win:
// unescaped single-segment content must come back as a view INTO the source
// buffer (no copy on our side), while escaped strings (unescape-copy) and
// genuine multi-segment responses (strings.Builder concat) must NOT alias —
// those copies are data-mandated.
func TestExtractResponseContentBorrowed_Aliasing(t *testing.T) {
	aliasing := []struct {
		name     string
		provider string
		body     string
		want     string
	}{
		{"openai/scalar", "openai", `{"choices":[{"message":{"content":"plain ascii answer"}}]}`, "plain ascii answer"},
		{"openai/array-single", "openai", `{"choices":[{"message":{"content":[{"type":"text","text":"single array part"}]}}]}`, "single array part"},
		{"anthropic/single-text", "anthropic", `{"content":[{"type":"text","text":"one anthropic block"}]}`, "one anthropic block"},
		// thinking precedes the single text block — still one parseable segment.
		{"anthropic/thinking-plus-one-text", "anthropic", `{"content":[{"type":"thinking","thinking":"deep"},{"type":"text","text":"the only answer"}]}`, "the only answer"},
		{"gemini/single-part", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"one gemini part"}]}}]}`, "one gemini part"},
		{"responses/single-message", "openai-responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"one responses block"}]}]}`, "one responses block"},
		{"bedrock/single-text", "aws-bedrock", `{"output":{"message":{"content":[{"text":"one bedrock block"}]}}}`, "one bedrock block"},
	}
	for _, tc := range aliasing {
		t.Run("aliases/"+tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			parseable, raw, _, err := ExtractResponseContentBorrowed(tc.provider, body, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parseable != tc.want {
				t.Fatalf("parseable = %q, want %q", parseable, tc.want)
			}
			if !stringAliasesBuffer(parseable, body) {
				t.Error("single-segment unescaped parseable did NOT alias the source buffer (lost the zero-copy fast-path)")
			}
			// raw == parseable, so it aliases too.
			if !stringAliasesBuffer(raw, body) {
				t.Error("raw did NOT alias the source buffer")
			}
		})
	}

	nonAliasing := []struct {
		name     string
		provider string
		body     string
		want     string
		reason   string
	}{
		{"openai/escaped", "openai", `{"choices":[{"message":{"content":"line\nwith\tescape"}}]}`, "line\nwith\tescape", "escaped string unescape-copies"},
		{"openai/array-multi", "openai", `{"choices":[{"message":{"content":[{"type":"text","text":"a "},{"type":"text","text":"b"}]}}]}`, "a b", "2 parts concat through builder"},
		{"anthropic/multi-text", "anthropic", `{"content":[{"type":"text","text":"x "},{"type":"text","text":"y"}]}`, "x y", "2 blocks concat"},
		{"gemini/multi-part", "google-ai", `{"candidates":[{"content":{"parts":[{"text":"p "},{"text":"q"}]}}]}`, "p q", "2 parts concat"},
		{"responses/multi-part", "openai-responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"m "},{"type":"output_text","text":"n"}]}]}`, "m n", "2 entries concat"},
		{"bedrock/multi-text", "aws-bedrock", `{"output":{"message":{"content":[{"text":"u "},{"text":"v"}]}}}`, "u v", "2 blocks concat"},
	}
	for _, tc := range nonAliasing {
		t.Run("owned/"+tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			parseable, _, _, err := ExtractResponseContentBorrowed(tc.provider, body, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parseable != tc.want {
				t.Fatalf("parseable = %q, want %q", parseable, tc.want)
			}
			if stringAliasesBuffer(parseable, body) {
				t.Errorf("parseable aliased the source buffer but should be owned (%s)", tc.reason)
			}
		})
	}
}

// TestExtractResponseContentBorrowed_ReasoningAliases pins the (easily-missed)
// fact that reasoning is NOT always owned: providers that return a scalar
// reasoning field directly — here OpenAI Chat's message.reasoning_content —
// hand back gjson's unescaped substring view, which aliases the borrowed
// buffer. This is why the orchestrator must clone reasoning before Release,
// and why the lifetime contract treats reasoning as a potential alias.
func TestExtractResponseContentBorrowed_ReasoningAliases(t *testing.T) {
	const wantReasoning = "unescaped reasoning telemetry"
	body := []byte(`{"choices":[{"message":{"content":"answer","reasoning_content":"unescaped reasoning telemetry"}}]}`)

	_, _, reasoning, err := ExtractResponseContentBorrowed("openai", body, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reasoning != wantReasoning {
		t.Fatalf("reasoning = %q, want %q", reasoning, wantReasoning)
	}
	if !stringAliasesBuffer(reasoning, body) {
		t.Error("OpenAI reasoning_content did NOT alias the borrowed buffer — the lifetime contract (reasoning MAY alias) would be wrong, or the orchestrator's clone-before-Release would be unnecessary; it is necessary")
	}
}

// TestExtractResponseContentBorrowed_AliasSafetyAfterPoison is the
// use-after-free guard for the aliasing extractor. It proves two things at
// once for an aliasing single-segment result:
//
//  1. the in-attempt view genuinely aliases the buffer — overwriting the
//     buffer (simulating fasthttp returning pooled storage to its pool, as
//     poisonReleasedBody does under -tags llmhttp_debug) corrupts the retained
//     view; and
//  2. strings.Clone taken BEFORE the overwrite yields owned bytes that survive
//     intact — which is exactly what Stage 2 does for every value that escapes
//     the attempt (raw/reasoning under NeedsRaw, parse-failure raw,
//     extraction-failure body).
//
// Together: aliasing is real (so retaining it past Release would be a
// use-after-free), and cloning-before-Release is a sufficient and correct fix.
func TestExtractResponseContentBorrowed_AliasSafetyAfterPoison(t *testing.T) {
	const want = "durable single segment content"
	body := []byte(`{"content":[{"type":"text","text":"durable single segment content"}]}`)

	parseable, raw, _, err := ExtractResponseContentBorrowed("anthropic", body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parseable != want {
		t.Fatalf("parseable = %q, want %q", parseable, want)
	}
	if !stringAliasesBuffer(parseable, body) {
		t.Fatal("precondition failed: parseable did not alias the buffer, cannot exercise the poison path")
	}

	// Clone BEFORE poisoning — this is the cloning-before-Release the
	// orchestrator performs for escaping values.
	owned := strings.Clone(raw)

	// Poison the backing buffer, mirroring poisonReleasedBody on Release.
	for i := range body {
		body[i] = 0xde
	}

	if owned != want {
		t.Errorf("cloned value corrupted after buffer poison: got %q, want %q", owned, want)
	}
	// Sanity: the retained alias DID observe the poison, confirming it shared
	// storage (i.e. retaining it past Release would have been a real bug).
	if parseable == want {
		t.Error("retained aliasing view survived buffer poison — it was not actually aliasing, test is not exercising the hazard")
	}
}
