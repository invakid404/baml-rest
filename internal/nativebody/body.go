package nativebody

import (
	"bytes"

	"github.com/invakid404/baml-rest/internal/nativeprompt"
)

// CanonicalBody is the immutable output of [BuildOpenAIChat]: the EXACT canonical
// OpenAI chat-completions request bytes BAML v0.223 emits for a given rendered
// prompt + client, plus the load-bearing model-separation metadata. Its bytes
// are handed downstream (Phase 4b nanollm Prepare, Phase 5 parity) WITHOUT
// re-marshaling — a round-trip through a map would destroy the request_body key
// order BAML emits, which is exactly what this phase proves.
//
// It is immutable: all fields are unexported and [CanonicalBody.Bytes] returns a
// copy, so a holder cannot mutate the proven bytes.
type CanonicalBody struct {
	raw         []byte
	targetModel string
	modelAlias  string
	provider    string
}

// Bytes returns a copy of the canonical request body bytes.
func (b *CanonicalBody) Bytes() []byte {
	out := make([]byte, len(b.raw))
	copy(out, b.raw)
	return out
}

// TargetModel is the BAML/OpenAI target model — the value serialized into the
// top-level JSON "model" field.
func (b *CanonicalBody) TargetModel() string { return b.targetModel }

// ModelAlias is the separate baml-rest/nanollm alias, carried as metadata only.
// It is NEVER the JSON "model" value; a later phase uses it as nanollm
// Request.Model.
func (b *CanonicalBody) ModelAlias() string { return b.modelAlias }

// Provider is the normalized provider the body was built for (always "openai" in
// Phase 4a).
func (b *CanonicalBody) Provider() string { return b.provider }

// bodyMessage is the flattened, admitted view of one output chat message: a
// standard role and its ordered text blocks. A completion is realized as a
// single system bodyMessage.
type bodyMessage struct {
	role  string
	texts []string
}

// BuildOpenAIChat serializes rendered + client into the exact canonical OpenAI
// chat-completions body BAML v0.223 emits, or returns a fail-closed *Decline
// (unwrapping to [ErrUnsupported]). It calls [SupportsOpenAIChat] first, so it
// never serializes an unproven shape.
//
// The observed BAML v0.223 openai order is reproduced exactly by a deterministic
// typed writer (NOT a map, which cannot preserve key order): top-level "model",
// then "messages", each message {"role","content"} with content an ARRAY of
// {"type":"text","text":...} blocks — even for a single text. The target model
// (never the nanollm alias) is the JSON "model" value. In Phase 4a there is no
// request_body suffix (a client carrying one declines), so the body is exactly
// model + messages.
func BuildOpenAIChat(rendered *nativeprompt.RenderedPrompt, client ClientIntent) (*CanonicalBody, error) {
	if err := SupportsOpenAIChat(rendered, client); err != nil {
		return nil, err
	}

	msgs := flattenMessages(rendered)

	var buf bytes.Buffer
	buf.WriteByte('{')
	buf.WriteString(`"model":`)
	writeJSONString(&buf, client.TargetModel)
	buf.WriteString(`,"messages":[`)
	for i, m := range msgs {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"role":`)
		writeJSONString(&buf, m.role)
		buf.WriteString(`,"content":[`)
		for j, text := range m.texts {
			if j > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(`{"type":"text","text":`)
			writeJSONString(&buf, text)
			buf.WriteByte('}')
		}
		buf.WriteString(`]}`)
	}
	buf.WriteString(`]}`)

	raw := make([]byte, buf.Len())
	copy(raw, buf.Bytes())
	return &CanonicalBody{
		raw:         raw,
		targetModel: client.TargetModel,
		modelAlias:  client.ModelAlias,
		provider:    client.Provider,
	}, nil
}

// flattenMessages projects an ADMITTED rendered prompt (SupportsOpenAIChat has
// already passed) into ordered bodyMessages. A completion becomes a single
// system message whose one text block is the completion string; a chat maps each
// message to its role and the text of each (text-only) part in order.
func flattenMessages(rp *nativeprompt.RenderedPrompt) []bodyMessage {
	if rp.Kind == nativeprompt.KindCompletion {
		return []bodyMessage{{role: "system", texts: []string{rp.Completion}}}
	}
	out := make([]bodyMessage, 0, len(rp.Messages))
	for i := range rp.Messages {
		m := &rp.Messages[i]
		texts := make([]string, 0, len(m.Parts))
		for j := range m.Parts {
			// Supports guaranteed every part is text (non-nil Text, no media).
			texts = append(texts, *m.Parts[j].Text)
		}
		out = append(out, bodyMessage{role: m.Role, texts: texts})
	}
	return out
}

const lowerHex = "0123456789abcdef"

// writeJSONString appends s to buf as a JSON string literal using EXACTLY BAML's
// (serde_json's) escaping, reproduced here byte-for-byte rather than delegated to
// encoding/json (whose defaults diverge). The differences that matter, all
// verified against BAML v0.223 output:
//
//   - HTML characters < > & are NOT escaped (encoding/json escapes them by
//     default);
//   - U+2028 / U+2029 are NOT escaped;
//   - 0x08 and 0x0c use the short escapes \b and \f;
//   - other control bytes < 0x20 use lowercase \u00XX;
//   - non-ASCII is emitted as raw UTF-8 (this loop is byte-oriented, so
//     multibyte sequences pass through unchanged).
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c == 0x08:
			buf.WriteString(`\b`)
		case c == 0x09:
			buf.WriteString(`\t`)
		case c == 0x0a:
			buf.WriteString(`\n`)
		case c == 0x0c:
			buf.WriteString(`\f`)
		case c == 0x0d:
			buf.WriteString(`\r`)
		case c < 0x20:
			buf.WriteString(`\u00`)
			buf.WriteByte(lowerHex[c>>4])
			buf.WriteByte(lowerHex[c&0xf])
		default:
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
}
