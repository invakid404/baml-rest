package buildrequest

import (
	"strings"
	"testing"
)

// These benchmarks measure OUR-SIDE extraction cost — the allocations the
// orchestrator incurs turning a response body into the parseable string it
// hands to parseFinal. They deliberately stop at that boundary: BAML's
// protobuf/FFI marshal of parseable happens downstream in parseFinal and is
// out of scope for #487 Stage 3.
//
// Each shape compares the Stage-2 borrow-lane extractor (ExtractResponseContent,
// which concatenates every kept segment through strings.Builder → always
// copies content) against the Stage-3 ExtractResponseContentBorrowed (which
// aliases unescaped single-segment content out of the buffer → zero copy). The
// []byte→string view inside the borrowed extractor is unsafe and allocation-
// free, so the B/op delta is exactly the content copy we removed.
//
// Expected reading (see PR description for measured numbers):
//   - OpenAI scalar: ~no delta. Scalar message.content already aliased at
//     Stage 2 (gjson.Get returns json[1:i] for the lone string), so there was
//     no content copy left to remove here.
//   - Anthropic/Gemini/Responses/Bedrock SINGLE block: large B/op drop — the
//     builder copy of the whole content disappears (single-segment fast-path).
//   - Multi-block (2+ segments): ~no delta — concatenation is data-mandated and
//     both paths copy.
//   - Escaped content: ~no delta — unescaping must allocate in both paths.

const benchContentSize = 64 << 10 // 64 KiB, large enough that the content copy dominates

// largeUnescaped returns n bytes of JSON-safe unescaped ASCII.
func largeUnescaped(n int) string {
	return strings.Repeat("abcdefghij", n/10)
}

// largeEscaped returns content of roughly n bytes that forces gjson to unescape
// (every 10th run carries an escape), so neither path can alias.
func largeEscaped(n int) string {
	return strings.Repeat("abcdefghi\\n", n/10)
}

func benchExtract(b *testing.B, provider, body string, borrowed bool) {
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	raw := []byte(body)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var p string
		var err error
		if borrowed {
			p, _, _, err = ExtractResponseContentBorrowed(provider, raw, false)
		} else {
			p, _, _, err = ExtractResponseContent(provider, body, false)
		}
		if err != nil {
			b.Fatal(err)
		}
		// Keep the result live so the compiler cannot elide the extraction.
		if len(p) == 0 {
			b.Fatal("empty parseable")
		}
	}
}

// --- OpenAI scalar: already aliased at Stage 2; expect ~no delta ---

func BenchmarkExtract_OpenAIScalar_Copying(b *testing.B) {
	body := `{"choices":[{"message":{"content":"` + largeUnescaped(benchContentSize) + `"}}]}`
	benchExtract(b, "openai", body, false)
}
func BenchmarkExtract_OpenAIScalar_Borrowed(b *testing.B) {
	body := `{"choices":[{"message":{"content":"` + largeUnescaped(benchContentSize) + `"}}]}`
	benchExtract(b, "openai", body, true)
}

// --- Anthropic single text block: builder copy removed by the fast-path ---

func BenchmarkExtract_AnthropicSingle_Copying(b *testing.B) {
	body := `{"content":[{"type":"text","text":"` + largeUnescaped(benchContentSize) + `"}]}`
	benchExtract(b, "anthropic", body, false)
}
func BenchmarkExtract_AnthropicSingle_Borrowed(b *testing.B) {
	body := `{"content":[{"type":"text","text":"` + largeUnescaped(benchContentSize) + `"}]}`
	benchExtract(b, "anthropic", body, true)
}

// --- Gemini single part: builder copy removed by the fast-path ---

func BenchmarkExtract_GeminiSingle_Copying(b *testing.B) {
	body := `{"candidates":[{"content":{"parts":[{"text":"` + largeUnescaped(benchContentSize) + `"}]}}]}`
	benchExtract(b, "google-ai", body, false)
}
func BenchmarkExtract_GeminiSingle_Borrowed(b *testing.B) {
	body := `{"candidates":[{"content":{"parts":[{"text":"` + largeUnescaped(benchContentSize) + `"}]}}]}`
	benchExtract(b, "google-ai", body, true)
}

// --- Anthropic multi-block: data-mandated concat; expect ~no delta ---

func BenchmarkExtract_AnthropicMulti_Copying(b *testing.B) {
	half := largeUnescaped(benchContentSize / 2)
	body := `{"content":[{"type":"text","text":"` + half + `"},{"type":"text","text":"` + half + `"}]}`
	benchExtract(b, "anthropic", body, false)
}
func BenchmarkExtract_AnthropicMulti_Borrowed(b *testing.B) {
	half := largeUnescaped(benchContentSize / 2)
	body := `{"content":[{"type":"text","text":"` + half + `"},{"type":"text","text":"` + half + `"}]}`
	benchExtract(b, "anthropic", body, true)
}

// --- Anthropic single ESCAPED block: unescape-copy in both; expect ~no delta ---

func BenchmarkExtract_AnthropicEscaped_Copying(b *testing.B) {
	body := `{"content":[{"type":"text","text":"` + largeEscaped(benchContentSize) + `"}]}`
	benchExtract(b, "anthropic", body, false)
}
func BenchmarkExtract_AnthropicEscaped_Borrowed(b *testing.B) {
	body := `{"content":[{"type":"text","text":"` + largeEscaped(benchContentSize) + `"}]}`
	benchExtract(b, "anthropic", body, true)
}
