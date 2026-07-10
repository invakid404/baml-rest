package nativeprompt

import (
	"errors"
	"strings"
	"testing"
)

// TestRenderCorpus is the pure-Go regression pin for the native renderer: it
// renders every seeded corpus case and asserts the structural RenderedPrompt.
// These expectations are the ones the integration oracle harness proved
// byte-exact against BAML v0.223, so a change that breaks parity breaks this
// test without needing CGO or the BAML runtime.
func TestRenderCorpus(t *testing.T) {
	byName := map[string]corpusCase{}
	for _, tc := range corpusCases() {
		byName[tc.name] = tc
	}

	render := func(t *testing.T, name string) *RenderedPrompt {
		t.Helper()
		tc, ok := byName[name]
		if !ok {
			t.Fatalf("corpus case %q not found", name)
		}
		rp, err := Render(tc.messages, tc.schema)
		if err != nil {
			t.Fatalf("Render(%s): %v", name, err)
		}
		if rp.Kind != KindChat {
			t.Fatalf("%s: kind = %q, want chat", name, rp.Kind)
		}
		return rp
	}

	t.Run("text_only", func(t *testing.T) {
		rp := render(t, "text_only")
		wantRoles := []string{"system", "user"}
		wantText := []string{"You are a helpful assistant.", "What is 2+2?"}
		if len(rp.Messages) != 2 {
			t.Fatalf("got %d messages, want 2", len(rp.Messages))
		}
		for i, m := range rp.Messages {
			if m.Role != wantRoles[i] {
				t.Errorf("msg[%d] role = %q, want %q", i, m.Role, wantRoles[i])
			}
			if len(m.Parts) != 1 || m.Parts[0].Text == nil || *m.Parts[0].Text != wantText[i] {
				t.Errorf("msg[%d] parts = %+v, want single text %q", i, m.Parts, wantText[i])
			}
			if m.Meta != nil {
				t.Errorf("msg[%d] unexpected meta %v", i, m.Meta)
			}
		}
	})

	t.Run("output_format_only", func(t *testing.T) {
		rp := render(t, "output_format_only")
		if len(rp.Messages) != 2 {
			t.Fatalf("got %d messages, want 2", len(rp.Messages))
		}
		// First message's single text part is the rendered ctx.output_format
		// block for {answer:string, count:int}.
		block := ""
		if len(rp.Messages[0].Parts) == 1 && rp.Messages[0].Parts[0].Text != nil {
			block = *rp.Messages[0].Parts[0].Text
		}
		for _, want := range []string{"answer: string", "count: int", "Answer in JSON"} {
			if !strings.Contains(block, want) {
				t.Errorf("output_format block missing %q; got:\n%s", want, block)
			}
		}
		if rp.Messages[1].Parts[0].Text == nil || *rp.Messages[1].Parts[0].Text != "Produce the structured output." {
			t.Errorf("msg[1] text = %+v", rp.Messages[1].Parts[0])
		}
	})

	t.Run("text_replace_output_format", func(t *testing.T) {
		rp := render(t, "text_replace_output_format")
		last := rp.Messages[len(rp.Messages)-1]
		if last.Parts[0].Text == nil {
			t.Fatalf("expected text part, got %+v", last.Parts[0])
		}
		got := *last.Parts[0].Text
		// The literal {output_format} token must be replaced by the rendered
		// block, and the surrounding text preserved.
		if strings.Contains(got, "{output_format}") {
			t.Errorf("literal {output_format} not replaced:\n%s", got)
		}
		if !strings.HasPrefix(got, "Answer strictly using ") || !strings.HasSuffix(got, " and nothing else.") {
			t.Errorf("surrounding text not preserved:\n%s", got)
		}
		if !strings.Contains(got, "answer: string") {
			t.Errorf("rendered block not substituted:\n%s", got)
		}
	})

	t.Run("role_metadata_cache_control", func(t *testing.T) {
		rp := render(t, "role_metadata_cache_control")
		if len(rp.Messages) != 4 {
			t.Fatalf("got %d messages, want 4", len(rp.Messages))
		}
		user := rp.Messages[1]
		if user.Role != "user" {
			t.Fatalf("msg[1] role = %q", user.Role)
		}
		cc, ok := user.Meta["cache_control"].(map[string]any)
		if !ok {
			t.Fatalf("msg[1] meta.cache_control missing/wrong: %#v", user.Meta)
		}
		if cc["type"] != "ephemeral" {
			t.Errorf("cache_control.type = %v, want ephemeral", cc["type"])
		}
		// The other three messages must carry NO metadata.
		for _, i := range []int{0, 2, 3} {
			if rp.Messages[i].Meta != nil {
				t.Errorf("msg[%d] unexpectedly has meta %v", i, rp.Messages[i].Meta)
			}
		}
	})

	t.Run("image_media", func(t *testing.T) {
		rp := render(t, "image_media")
		if len(rp.Messages) != 1 {
			t.Fatalf("got %d messages, want 1", len(rp.Messages))
		}
		parts := rp.Messages[0].Parts
		if len(parts) != 2 {
			t.Fatalf("got %d parts, want 2 (text + media): %+v", len(parts), parts)
		}
		if parts[0].Text == nil || *parts[0].Text != "Describe this image:" {
			t.Errorf("part[0] = %+v, want text", parts[0])
		}
		if parts[1].Media == nil {
			t.Fatalf("part[1] = %+v, want media", parts[1])
		}
		if parts[1].Media.Kind != "image" || parts[1].Media.URL != "https://example.com/cat.png" {
			t.Errorf("part[1] media = %+v", parts[1].Media)
		}
	})
}

// TestRenderDeclinesUnprovenMedia pins the fail-closed contract at the render
// entry point: an unproven media kind is declined (ErrUnsupported), not
// rendered, so a caller falls back to BAML.
func TestRenderDeclinesUnprovenMedia(t *testing.T) {
	msgs := []Message{
		{Role: "user", Parts: []ContentPart{
			{Audio: &Media{Kind: MediaAudio, URL: "https://example.com/a.mp3"}},
		}},
	}
	_, err := Render(msgs, simpleSchema())
	if err == nil {
		t.Fatal("expected decline for audio media, got nil")
	}
	assertUnsupported(t, err)
}

// TestRenderDeclinesNilSchemaReachingOutputFormat pins Fix #1: when the input
// can reach ctx.output_format (an output_format part OR a {output_format}
// placeholder in content) but no output schema is supplied, Render fails closed
// with ErrUnsupported rather than rendering a weakened (empty) output_format.
func TestRenderDeclinesNilSchemaReachingOutputFormat(t *testing.T) {
	reaches := []struct {
		name string
		msgs []Message
	}{
		{
			name: "output_format_part",
			msgs: []Message{{Role: "system", Parts: []ContentPart{{OutputFormat: true}}}},
		},
		{
			name: "output_format_placeholder",
			msgs: []Message{{Role: "user", Content: strptr("answer using {output_format} here")}},
		},
	}
	for _, tc := range reaches {
		t.Run("decline_"+tc.name, func(t *testing.T) {
			_, err := Render(tc.msgs, nil)
			assertUnsupported(t, err)
			var d *Decline
			if !errors.As(err, &d) || d.Feature != FeatureNilOutputSchema {
				t.Fatalf("want nil_output_schema decline, got %v", err)
			}
			// With a schema, the same input renders (no decline).
			if _, err := Render(tc.msgs, simpleSchema()); err != nil {
				t.Fatalf("with schema should render, got: %v", err)
			}
		})
	}

	// A message that does NOT reach ctx.output_format renders fine with a nil
	// schema — the decline must be scoped to reachable inputs only.
	t.Run("text_only_nil_schema_ok", func(t *testing.T) {
		msgs := []Message{{Role: "user", Content: strptr("plain text, no placeholder")}}
		if _, err := Render(msgs, nil); err != nil {
			t.Fatalf("text-only with nil schema should render, got: %v", err)
		}
	})

	// When a message has parts, the content branch is never taken, so a
	// {output_format} placeholder in that message's content does NOT reach
	// ctx.output_format and must not force a decline under a nil schema.
	t.Run("placeholder_in_content_but_parts_win", func(t *testing.T) {
		msgs := []Message{{
			Role:    "user",
			Content: strptr("ignored {output_format}"),
			Parts:   []ContentPart{{Text: strptr("hi")}},
		}}
		if _, err := Render(msgs, nil); err != nil {
			t.Fatalf("parts-branch message should render with nil schema, got: %v", err)
		}
	})
}

// TestCompletionVsChat pins BAML's Completion-vs-Chat rule at the lowering
// layer: a rendered string with no role/media delimiter is a Completion.
func TestCompletionVsChat(t *testing.T) {
	comp, err := lower("just some completion text")
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if comp.Kind != KindCompletion || comp.Completion != "just some completion text" {
		t.Errorf("completion = %+v", comp)
	}

	chat, err := lower(emitRoleMarker("user", false, nil) + "hi")
	if err != nil {
		t.Fatalf("lower chat: %v", err)
	}
	if chat.Kind != KindChat || len(chat.Messages) != 1 || chat.Messages[0].Role != "user" {
		t.Errorf("chat = %+v", chat)
	}
}
