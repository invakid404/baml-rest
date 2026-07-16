package admission

import (
	"bytes"

	"github.com/bytedance/sonic"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

// canonicalTextBlock is one OpenAI content block — {"type":"text","text":...}.
// It is a fixed-field struct rather than nanollm's map-based TextBlock helper on
// purpose: a map[string]any lets the Marshaler order its keys ("text" sorts
// ahead of "type"), which would break byte-parity with BAML's fixed
// type-then-text order. A struct always serializes in declaration order under
// any Marshaler.
type canonicalTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// chatRequestFromRendered maps an ADMITTED rendered prompt and its resolved
// target model onto nanollm v0.4.x's typed [nanollm.ChatRequest] — the FFI
// package's own canonical OpenAI chat-completions request type. It mirrors the
// zero-nanollm root writer nativebody.BuildOpenAIChat: a completion is realized
// as a single system message; a chat maps each message's role and ordered text
// parts to a content array of text blocks.
//
// Model is the TARGET model — the top-level JSON "model" value, never the
// nanollm alias. ChatRequest.Build copies it to Request.Model, which the caller
// overrides with the alias, exactly as the previous hand-built [nanollm.Request]
// did. The typed model is proven byte-faithful to the root writer's canonical
// bytes, under [canonicalSonicMarshaler], by the gated canonreq oracle.
func chatRequestFromRendered(rendered *nativeprompt.RenderedPrompt, targetModel string) nanollm.ChatRequest {
	var messages []nanollm.ChatMessage
	if rendered.Kind == nativeprompt.KindCompletion {
		messages = []nanollm.ChatMessage{{
			Role:    "system",
			Content: []canonicalTextBlock{{Type: "text", Text: rendered.Completion}},
		}}
	} else {
		messages = make([]nanollm.ChatMessage, 0, len(rendered.Messages))
		for i := range rendered.Messages {
			m := &rendered.Messages[i]
			blocks := make([]canonicalTextBlock, 0, len(m.Parts))
			for j := range m.Parts {
				// SupportsOpenAIChat guaranteed every admitted part is text.
				blocks = append(blocks, canonicalTextBlock{Type: "text", Text: *m.Parts[j].Text})
			}
			messages = append(messages, nanollm.ChatMessage{Role: m.Role, Content: blocks})
		}
	}
	return nanollm.ChatRequest{Model: targetModel, Messages: messages}
}

// sonicCanonicalAPI is the configured sonic encoder for the canonical OpenAI
// body: EscapeHTML off to match serde_json/BAML (which does not escape < > &).
// With the fixed-field structs above it reproduces BAML byte-for-byte on every
// admitted input EXCEPT bytes 0x08 and 0x0c, which sonic emits as the six-byte
// u-escape (backslash u 0 0 0 8 / backslash u 0 0 0 c) where serde_json/BAML
// emit the short backspace / form-feed escapes. fixShortEscapes closes that one
// gap; canonicalSonicMarshaler is the two composed.
var sonicCanonicalAPI = sonic.Config{EscapeHTML: false}.Froze()

// longEscBackspace and longEscFormFeed are the exact six-byte sequences sonic
// emits for 0x08 and 0x0c (a backslash, the letter u, then four lowercase hex
// digits). Declared as explicit bytes so this source file carries no JSON escape
// sequences of its own.
var (
	longEscBackspace = []byte{'\\', 'u', '0', '0', '0', '8'}
	longEscFormFeed  = []byte{'\\', 'u', '0', '0', '0', 'c'}
)

// canonicalSonicMarshaler is the SHIPPED canonical-body Marshaler: sonic
// (EscapeHTML off) followed by fixShortEscapes. The result is byte-exact vs BAML
// on every admitted input — proven by the gated canonreq oracle, including the
// 0x08 / 0x0c cases — so ChatRequest.Build uses it directly at the admission
// site. The root writer's canonical bytes remain the runtime parity anchor
// (validatePreparedBody), so a hypothetical mismatch fails closed to BAML rather
// than emitting a non-parity body.
var canonicalSonicMarshaler nanollm.Marshaler = func(v any) ([]byte, error) {
	body, err := sonicCanonicalAPI.Marshal(v)
	if err != nil {
		return nil, err
	}
	return fixShortEscapes(body), nil
}

// fixShortEscapes rewrites sonic's two divergent JSON string escapes to the
// short forms serde_json/BAML emit: the six-byte u-escape for 0x08 becomes the
// short backspace escape (backslash b) and the one for 0x0c becomes the short
// form-feed escape (backslash f). Every other byte sonic emits already matches
// BAML, so nothing else is touched.
//
// It is backslash-parity-aware, which is the whole point: a backslash only
// introduces an escape when it is NOT itself the payload of a preceding
// escaped-backslash pair. The scan consumes each escape as a UNIT — a two-byte
// short escape (including an escaped backslash) or a six-byte u-escape — so it
// advances past the payload and can never re-read it as a new introducer. Thus
// the letters u0008 inside a literal escaped-backslash-then-u0008 sequence are
// copied verbatim, never rewritten, while a genuine u-escape for a real 0x08
// byte IS rewritten. Bytes that are not escape introducers are copied unchanged.
func fixShortEscapes(b []byte) []byte {
	// Nothing to rewrite unless a divergent long form is present at all; return
	// the input untouched (no allocation) in the common case.
	if !bytes.Contains(b, longEscBackspace) && !bytes.Contains(b, longEscFormFeed) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] != '\\' {
			out = append(out, b[i])
			i++
			continue
		}
		// b[i] is a backslash — an escape introducer. It needs at least one payload
		// byte; a trailing lone backslash (never emitted by sonic) is copied as-is.
		if i+1 >= len(b) {
			out = append(out, b[i])
			i++
			continue
		}
		// A genuine u-escape whose hex is one of the two sonic diverges on.
		if b[i+1] == 'u' && i+6 <= len(b) &&
			b[i+2] == '0' && b[i+3] == '0' && b[i+4] == '0' {
			switch b[i+5] {
			case '8': // 0x08 -> short backspace escape
				out = append(out, '\\', 'b')
				i += 6
				continue
			case 'c': // 0x0c -> short form-feed escape
				out = append(out, '\\', 'f')
				i += 6
				continue
			}
		}
		// Any other escape — an escaped backslash, an escaped quote, a different
		// u-escape, or another short escape — is copied as a unit (introducer plus
		// its next byte), so the payload of an escaped-backslash pair is consumed
		// here and never treated as a new introducer. This is what makes the scan
		// parity-aware.
		out = append(out, b[i], b[i+1])
		i += 2
	}
	return out
}
