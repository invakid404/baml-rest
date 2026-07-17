//go:build nanollm_integration

package admission

// canonreq oracle — re-proves that adopting nanollm v0.4.x's typed ChatRequest
// (canonreq.go) preserves the ABSOLUTE byte-parity vs BAML that the zero-nanollm
// root writer internal/nativebody.BuildOpenAIChat already proves. It anchors the
// SHIPPED marshaler (configured sonic + the backslash-parity-aware short-escape
// fixup) against that proven writer over the FULL P4/6c escaping corpus,
// INCLUDING the 0x08 and 0x0c bytes the raw sonic marshaler diverged on, and
// shows the fixup is what closes that gap.
//
// Every special character is built from explicit code points via u(...) so this
// source stays pure ASCII and no escape sequence can drift.

import (
	"bytes"
	"testing"

	"github.com/invakid404/baml-rest/internal/nativebody"
	"github.com/invakid404/baml-rest/internal/nativeprompt"
	nanollm "github.com/viktordanov/nanollm-ffi/go"
)

const oracleModel = "gpt-4o-mini"

func oracleIntent() nativebody.ClientIntent {
	return nativebody.ClientIntent{
		Provider:    nativebody.ProviderOpenAI,
		TargetModel: oracleModel,
		ModelAlias:  "openai/" + oracleModel,
	}
}

// u builds a string from Unicode code points, keeping this source pure ASCII.
func u(cps ...rune) string { return string(cps) }

func textPtr(s string) *string { return &s }

func completionPrompt(text string) *nativeprompt.RenderedPrompt {
	return &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindCompletion, Completion: text}
}

func chatPrompt(msgs ...nativeprompt.RenderedMessage) *nativeprompt.RenderedPrompt {
	return &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindChat, Messages: msgs}
}

func textMsg(role string, texts ...string) nativeprompt.RenderedMessage {
	parts := make([]nativeprompt.Part, 0, len(texts))
	for _, t := range texts {
		parts = append(parts, nativeprompt.Part{Text: textPtr(t)})
	}
	return nativeprompt.RenderedMessage{Role: role, Parts: parts}
}

// TestCanonicalTypeMatchesRootWriter proves the SHIPPED marshaler
// (canonicalSonicMarshaler = configured sonic + fixShortEscapes) emits the EXACT
// canonical bytes the proven root writer emits, over the escaping corpus that
// pins serde_json's divergences from Go's encoding/json defaults — now including
// the 0x08 / 0x0c bytes and an adversarial escaped-backslash case.
func TestCanonicalTypeMatchesRootWriter(t *testing.T) {
	cases := []struct {
		name     string
		rendered *nativeprompt.RenderedPrompt
	}{
		{"completion_as_system_message", completionPrompt("Write a short, plain note about cats and dogs.")},
		{"single_user_message", chatPrompt(textMsg("user", "What is 2+2?"))},
		{"ordered_system_user_assistant", chatPrompt(
			textMsg("system", "You are a concise assistant."),
			textMsg("user", "Tell me about weather."),
			textMsg("assistant", "Here are 3 facts."),
		)},
		// HTML chars must NOT be escaped (encoding/json escapes them; EscapeHTML off matches BAML).
		{"html_chars_unescaped", chatPrompt(textMsg("user", "a<b>c&d </tag> a&&b"))},
		// tab/newline/carriage-return short escapes + a generic control byte -> lowercase u-escape.
		{"control_and_short_escapes", chatPrompt(textMsg("user", u('t', 'a', 'b', 0x09, 'v', 'a', 'l', 0x01, 'e', 'n', 'd', ' ', 'n', 'l', 0x0a, 'c', 'r', 0x0d)))},
		{"quote_and_backslash", chatPrompt(textMsg("user", `q"q\q`))},
		// U+2028 / U+2029 must pass through raw (unescaped) to match BAML.
		{"line_and_paragraph_separators", chatPrompt(textMsg("user", u('a', 0x2028, 'b', 0x2029, 'c')))},
		// DEL (0x7f) raw + non-ASCII raw UTF-8 passthrough.
		{"del_and_non_ascii", chatPrompt(textMsg("user", u('a', 0x7f, 'b', ' ', 'c', 'a', 'f', 0xe9, ' ', 0x2615, ' ', 0x1F600)))},
		{"multi_part_user_content_array", chatPrompt(textMsg("user", "first", "second", "third"))},
		// The two bytes raw sonic diverged on — now byte-exact via the fixup.
		{"backspace_byte", chatPrompt(textMsg("user", u('a', 0x08, 'b')))},
		{"form_feed_byte", chatPrompt(textMsg("user", u('a', 0x0c, 'b')))},
		{"both_short_escape_bytes", chatPrompt(textMsg("user", u('x', 0x08, 'y', 0x0c, 'z')))},
		// ADVERSARIAL: a real 0x08 / 0x0c next to the LITERAL letters u0008 preceded
		// by a real backslash (which sonic escapes to a backslash pair). The fixup
		// must rewrite the genuine u-escapes but leave the escaped-backslash literal
		// alone — exactly what the root writer does.
		{"short_bytes_beside_literal_u_escape", chatPrompt(textMsg("user", "A"+u(0x08)+"B"+`\`+"u0008C"+u(0x0c)+"D"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := nativebody.BuildOpenAIChat(tc.rendered, oracleIntent())
			if err != nil {
				t.Fatalf("root BuildOpenAIChat declined an admissible corpus case: %v", err)
			}
			want := root.Bytes()

			cr := chatRequestFromRendered(tc.rendered, oracleModel)

			// The SHIPPED marshaler reproduces the proven bytes exactly.
			shipped, err := cr.Build(canonicalSonicMarshaler)
			if err != nil {
				t.Fatalf("ChatRequest.Build(canonicalSonicMarshaler): %v", err)
			}
			if !bytes.Equal([]byte(shipped.Body), want) {
				t.Errorf("shipped body != root writer\n shipped: %s\n root:    %s", shipped.Body, want)
			}
			// It also wires the typed transport shape the way the previous hand-built
			// Request did.
			if shipped.Type != nanollm.ChatCompletion {
				t.Errorf("Request.Type = %v, want ChatCompletion", shipped.Type)
			}
			if shipped.Stream {
				t.Error("Request.Stream = true, want false for the non-streaming chat body")
			}
			if shipped.Model != oracleModel {
				t.Errorf("Build set Request.Model = %q, want the target %q (caller then overrides with the alias)", shipped.Model, oracleModel)
			}
		})
	}
}

// TestCanonicalTypeFixupClosesShortEscapeGap proves the fixup is load-bearing AND
// sufficient: on the 0x08 / 0x0c bytes the RAW sonic marshaler diverges from
// BAML, while the SHIPPED (fixed) marshaler is byte-exact.
func TestCanonicalTypeFixupClosesShortEscapeGap(t *testing.T) {
	rendered := chatPrompt(textMsg("user", u('x', 0x08, 'y', 0x0c, 'z')))

	root, err := nativebody.BuildOpenAIChat(rendered, oracleIntent())
	if err != nil {
		t.Fatalf("root BuildOpenAIChat: %v", err)
	}
	want := root.Bytes()
	// BAML emits the short escapes for these bytes.
	if !bytes.Contains(want, []byte{'\\', 'b'}) || !bytes.Contains(want, []byte{'\\', 'f'}) {
		t.Fatalf("root writer must emit the short backspace/form-feed escapes; got %s", want)
	}

	cr := chatRequestFromRendered(rendered, oracleModel)

	// RAW sonic diverges: it emits the six-byte long forms, so it does NOT match.
	raw, err := sonicCanonicalAPI.Marshal(cr)
	if err != nil {
		t.Fatalf("raw sonic Marshal: %v", err)
	}
	if bytes.Equal(raw, want) {
		t.Fatal("expected RAW sonic to diverge from BAML on 0x08/0x0c, but it matched — the fixup would be dead code")
	}
	if !bytes.Contains(raw, longEscBackspace) || !bytes.Contains(raw, longEscFormFeed) {
		t.Errorf("raw sonic should emit the long u-escape forms; got %s", raw)
	}

	// The SHIPPED (fixed) marshaler matches BAML byte-for-byte.
	shipped, err := cr.Build(canonicalSonicMarshaler)
	if err != nil {
		t.Fatalf("ChatRequest.Build(canonicalSonicMarshaler): %v", err)
	}
	if !bytes.Equal([]byte(shipped.Body), want) {
		t.Errorf("fixed sonic must equal the root writer on 0x08/0x0c\n shipped: %s\n root:    %s", shipped.Body, want)
	}
}
