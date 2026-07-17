package nativebody

import (
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/internal/nativeprompt"
)

// De-BAML Phase 7B: pure-Go (no BAML, no nanollm) unit proofs for the streaming
// canonical body. The BAML-oracle byte-parity proof (canonical stream body ==
// BAML's actual StreamRequest body) lives in the integration-gated
// stream_oracle_integration_test.go; this file proves the builder's shape, its
// exact suffix, its declines, and — critically — that adding the streaming path
// did NOT loosen the proven non-streaming builder.

func streamRendered() *nativeprompt.RenderedPrompt {
	return &nativeprompt.RenderedPrompt{
		Kind: nativeprompt.KindChat,
		Messages: []nativeprompt.RenderedMessage{
			{Role: "system", Parts: []nativeprompt.Part{{Text: sp("You are concise.")}}},
			{Role: "user", Parts: []nativeprompt.Part{{Text: sp("hi")}}},
		},
	}
}

func streamIntent(stream bool) ClientIntent {
	return ClientIntent{
		Provider:    ProviderOpenAI,
		TargetModel: "gpt-4o-mini",
		ModelAlias:  "nanollm://openai/x",
		Stream:      stream,
	}
}

// TestBuildOpenAIChatStreamExactBytes pins the exact streaming body bytes: the
// non-streaming body with BAML's `,"stream":true,"stream_options":{"include_usage":true}`
// suffix appended after the messages array, before the closing brace.
func TestBuildOpenAIChatStreamExactBytes(t *testing.T) {
	rendered := streamRendered()

	streamBody, err := BuildOpenAIChatStream(rendered, streamIntent(true))
	if err != nil {
		t.Fatalf("BuildOpenAIChatStream: %v", err)
	}
	const want = `{"model":"gpt-4o-mini","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"You are concise."}]},` +
		`{"role":"user","content":[{"type":"text","text":"hi"}]}` +
		`],"stream":true,"stream_options":{"include_usage":true}}`
	if got := string(streamBody.Bytes()); got != want {
		t.Fatalf("stream body mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestStreamBodyIsUnaryPlusSuffix proves the ONLY difference between the
// streaming and non-streaming bodies is BAML's trailing stream suffix: the
// streaming body is the unary body with its closing `}` replaced by
// `,"stream":true,"stream_options":{"include_usage":true}}`.
func TestStreamBodyIsUnaryPlusSuffix(t *testing.T) {
	rendered := streamRendered()

	unary, err := BuildOpenAIChat(rendered, streamIntent(false))
	if err != nil {
		t.Fatalf("BuildOpenAIChat: %v", err)
	}
	stream, err := BuildOpenAIChatStream(rendered, streamIntent(true))
	if err != nil {
		t.Fatalf("BuildOpenAIChatStream: %v", err)
	}
	u := string(unary.Bytes())
	s := string(stream.Bytes())
	if !strings.HasSuffix(u, "}") {
		t.Fatalf("unary body does not end in '}': %s", u)
	}
	want := u[:len(u)-1] + `,"stream":true,"stream_options":{"include_usage":true}}`
	if s != want {
		t.Fatalf("stream body is not unary+suffix\n got: %s\nwant: %s", s, want)
	}
	// The suffix carries BOTH fields in BAML's fixed order.
	if !strings.Contains(s, `"stream":true`) || !strings.Contains(s, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("stream body missing a required field: %s", s)
	}
	if idxStream, idxOpts := strings.Index(s, `"stream":true`), strings.Index(s, `"stream_options"`); idxStream >= idxOpts {
		t.Fatalf(`"stream" must precede "stream_options": %s`, s)
	}
}

// TestBuildOpenAIChatStreamRequiresStream proves the streaming builder DECLINES a
// non-streaming intent (client.Stream==false) — the mirror of the non-streaming
// builder's decline of a streaming intent — so neither surface can claim the
// other's bytes.
func TestBuildOpenAIChatStreamRequiresStream(t *testing.T) {
	body, err := BuildOpenAIChatStream(streamRendered(), streamIntent(false))
	if body != nil || err == nil {
		t.Fatalf("streaming builder must decline a non-streaming intent; got body=%v err=%v", body, err)
	}
	var d *Decline
	if !errors.As(err, &d) || d.Feature != FeatureStreaming {
		t.Fatalf("want FeatureStreaming decline, got %v", err)
	}
}

// TestNonStreamBuilderStillDeclinesStream is the I1 no-loosening guard: the
// proven non-streaming builder must STILL decline a streaming intent unchanged.
func TestNonStreamBuilderStillDeclinesStream(t *testing.T) {
	body, err := BuildOpenAIChat(streamRendered(), streamIntent(true))
	if body != nil || err == nil {
		t.Fatalf("non-streaming builder must decline a streaming intent; got body=%v err=%v", body, err)
	}
	var d *Decline
	if !errors.As(err, &d) || d.Feature != FeatureStreaming {
		t.Fatalf("want FeatureStreaming decline, got %v", err)
	}
}

// TestBuildOpenAIChatStreamSharesClientGates proves the streaming builder shares
// the mode-agnostic client gates: a non-openai provider and a body-affecting
// option each decline with the SAME concrete feature the non-streaming builder
// uses, so the streaming surface never admits a shape the unary surface rejects.
func TestBuildOpenAIChatStreamSharesClientGates(t *testing.T) {
	rendered := streamRendered()

	// Non-openai provider.
	badProvider := streamIntent(true)
	badProvider.Provider = "anthropic"
	if _, err := BuildOpenAIChatStream(rendered, badProvider); err == nil {
		t.Fatal("streaming builder admitted a non-openai provider")
	} else {
		var d *Decline
		if !errors.As(err, &d) || d.Feature != FeatureProvider {
			t.Fatalf("want FeatureProvider decline, got %v", err)
		}
	}

	// Body-affecting option (tools).
	withTools := streamIntent(true)
	withTools.BodyAffecting = []BodyAffectingOption{{Key: "tools", Feature: FeatureTools}}
	if _, err := BuildOpenAIChatStream(rendered, withTools); err == nil {
		t.Fatal("streaming builder admitted a body-affecting option")
	} else {
		var d *Decline
		if !errors.As(err, &d) || d.Feature != FeatureTools {
			t.Fatalf("want FeatureTools decline, got %v", err)
		}
	}
}

// TestBuildOpenAIChatStreamCompletion proves a completion prompt streams as the
// single-system-message body plus the suffix, exactly mirroring the unary
// completion realization.
func TestBuildOpenAIChatStreamCompletion(t *testing.T) {
	rendered := &nativeprompt.RenderedPrompt{Kind: nativeprompt.KindCompletion, Completion: "just do it"}
	stream, err := BuildOpenAIChatStream(rendered, streamIntent(true))
	if err != nil {
		t.Fatalf("BuildOpenAIChatStream(completion): %v", err)
	}
	const want = `{"model":"gpt-4o-mini","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"just do it"}]}` +
		`],"stream":true,"stream_options":{"include_usage":true}}`
	if got := string(stream.Bytes()); got != want {
		t.Fatalf("completion stream body mismatch\n got: %s\nwant: %s", got, want)
	}
}
