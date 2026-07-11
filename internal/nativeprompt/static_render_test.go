package nativeprompt

import (
	"math"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	desc "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

// --- shared test builders (used by static_render_test and static_support_test) --

// primArg builds a declared primitive argument of the given BAML primitive.
func primArg(name, prim string) promptdescriptor.Argument {
	return promptdescriptor.Argument{
		Name: name,
		Type: &bamlparser.TypeExpr{Kind: bamlparser.KindPrimitive, Primitive: prim},
	}
}

// staticReturnBundle is the retained return descriptor used by the valid test
// functions: a flat two-field class {answer: string, count: int}. Its rendered
// ctx.output_format block is staticOutputFormatBlock.
func staticReturnBundle(method string) desc.Bundle {
	return desc.Bundle{
		Version: desc.Version,
		Method:  method,
		Target:  desc.Type{Kind: desc.TypeClass, Name: "Answer", Mode: desc.NonStreaming},
		Classes: []desc.ClassDef{{
			Name: desc.Name{Name: "Answer"},
			Mode: desc.NonStreaming,
			Fields: []desc.ClassField{
				{Name: desc.Name{Name: "answer"}, Type: desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveString}},
				{Name: desc.Name{Name: "count"}, Type: desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveInt}},
			},
		}},
	}
}

// staticOutputFormatBlock is the exact rendered ctx.output_format text for
// staticReturnBundle (pinned from outputformat.Render with default options).
const staticOutputFormatBlock = "Answer in JSON using this schema:\n{\n  answer: string,\n  count: int,\n}"

// mediaJSON is a well-formed media-marker body: wrapped in the media marker
// affixes it would lower to a real image MediaPart, so a rendered body composing
// it must decline rather than synthesize media.
const mediaJSON = `{"kind":"image","url":"https://example.test/i.png"}`

// returnBundleWithFieldDescription is a structurally-valid return bundle whose
// single field carries desc as its description. The description renders into the
// ctx.output_format block verbatim (as a `// ...` comment), which lets a test
// place a reserved BAML delimiter into the user-controlled rendered block.
func returnBundleWithFieldDescription(method, description string) desc.Bundle {
	d := description
	return desc.Bundle{
		Version: desc.Version,
		Method:  method,
		Target:  desc.Type{Kind: desc.TypeClass, Name: "Answer", Mode: desc.NonStreaming},
		Classes: []desc.ClassDef{{
			Name: desc.Name{Name: "Answer"},
			Mode: desc.NonStreaming,
			Fields: []desc.ClassField{
				{Name: desc.Name{Name: "answer"}, Type: desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveString}, Description: &d},
			},
		}},
	}
}

// staticFn builds a valid function descriptor envelope for method "F" with the
// given prompt and declared arguments.
func staticFn(prompt string, args ...promptdescriptor.Argument) promptdescriptor.Function {
	return promptdescriptor.Function{
		Version:  promptdescriptor.Version,
		Method:   "F",
		Prompt:   prompt,
		Args:     args,
		Client:   "MyClient",
		Provider: "openai",
		Return:   staticReturnBundle("F"),
	}
}

// mustRenderStatic renders and fails on any error.
func mustRenderStatic(t *testing.T, fn promptdescriptor.Function, args map[string]any) *RenderedPrompt {
	t.Helper()
	if err := SupportsStatic(fn, args); err != nil {
		t.Fatalf("SupportsStatic: %v", err)
	}
	rp, err := RenderStatic(fn, args)
	if err != nil {
		t.Fatalf("RenderStatic: %v", err)
	}
	if rp == nil {
		t.Fatal("RenderStatic returned nil prompt without error")
	}
	return rp
}

func wantText(t *testing.T, m RenderedMessage, role, text string) {
	t.Helper()
	if m.Role != role {
		t.Errorf("role = %q, want %q", m.Role, role)
	}
	if m.Meta != nil {
		t.Errorf("role %q unexpected meta %v", role, m.Meta)
	}
	if m.AllowDuplicateRole {
		t.Errorf("role %q unexpected allow_duplicate_role", role)
	}
	if len(m.Parts) != 1 || m.Parts[0].Text == nil {
		t.Fatalf("role %q parts = %+v, want single text part", role, m.Parts)
	}
	if *m.Parts[0].Text != text {
		t.Errorf("role %q text = %q, want %q", role, *m.Parts[0].Text, text)
	}
}

// TestRenderStaticCompletion covers literal text + direct string interpolation,
// dedent/trim, and the KindCompletion decision.
func TestRenderStaticCompletion(t *testing.T) {
	// Raw prompt carries BAML-style indentation and surrounding blank lines; the
	// renderer dedents by the minimum leading whitespace and trims the whole
	// template, exactly like the dynamic path.
	fn := staticFn("\n    You said: {{ msg }}\n    Done.\n  ", primArg("msg", "string"))
	rp := mustRenderStatic(t, fn, map[string]any{"msg": "hello"})
	if rp.Kind != KindCompletion {
		t.Fatalf("kind = %q, want completion", rp.Kind)
	}
	if rp.Completion != "You said: hello\nDone." {
		t.Fatalf("completion = %q", rp.Completion)
	}
}

// TestRenderStaticPrimitiveArgs pins exact scalar display for every primitive
// and representative edge values, in a completion (completion body is NOT
// trimmed, so leading/trailing spaces survive).
func TestRenderStaticPrimitiveArgs(t *testing.T) {
	cases := []struct {
		name string
		prim string
		val  any
		want string
	}{
		{"int_zero", "int", int64(0), "0"},
		{"int_neg", "int", int64(-5), "-5"},
		{"int_max", "int", int64(math.MaxInt64), "9223372036854775807"},
		{"int_min", "int", int64(math.MinInt64), "-9223372036854775808"},
		{"float_frac", "float", 3.14, "3.14"},
		{"float_zero", "float", 0.0, "0.0"},
		{"float_neg_zero", "float", math.Copysign(0, -1), "-0.0"},
		{"float_neg", "float", -1.5, "-1.5"},
		{"bool_true", "bool", true, "true"},
		{"bool_false", "bool", false, "false"},
		{"str_ascii", "string", "hello world", "hello world"},
		{"str_unicode", "string", "héllo 日本語 🙂", "héllo 日本語 🙂"},
		{"str_quotes_backslash", "string", `a"b\c`, `a"b\c`},
		{"str_newline", "string", "line1\nline2", "line1\nline2"},
		{"str_edge_spaces", "string", "  padded  ", "  padded  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := staticFn("{{ v }}", primArg("v", tc.prim))
			rp := mustRenderStatic(t, fn, map[string]any{"v": tc.val})
			if rp.Kind != KindCompletion {
				t.Fatalf("kind = %q, want completion", rp.Kind)
			}
			if rp.Completion != tc.want {
				t.Fatalf("completion = %q, want %q", rp.Completion, tc.want)
			}
		})
	}
}

// TestRenderStaticRoleChat covers text-only chat: positional _.role, role= kwarg
// form, _.chat positional, ordered roles, and a primitive interpolation. Chat
// message text IS trimmed.
func TestRenderStaticRoleChat(t *testing.T) {
	fn := staticFn(
		`{{ _.role("system") }}You are a helpful assistant.{{ _.role(role="user") }}{{ question }}{{ _.chat("assistant") }}Sure!`,
		primArg("question", "string"),
	)
	rp := mustRenderStatic(t, fn, map[string]any{"question": "What is 2+2?"})
	if rp.Kind != KindChat {
		t.Fatalf("kind = %q, want chat", rp.Kind)
	}
	if len(rp.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(rp.Messages))
	}
	wantText(t, rp.Messages[0], "system", "You are a helpful assistant.")
	wantText(t, rp.Messages[1], "user", "What is 2+2?")
	wantText(t, rp.Messages[2], "assistant", "Sure!")
}

// TestRenderStaticChatKwargRoleAll proves both _.role(role=) and _.chat(role=)
// kwarg spellings and all three standard literal roles are accepted.
func TestRenderStaticChatKwargRoleAll(t *testing.T) {
	fn := staticFn(
		`{{ _.chat(role="system") }}s{{ _.chat(role="user") }}u{{ _.role(role="assistant") }}a`,
	)
	rp := mustRenderStatic(t, fn, map[string]any{})
	if rp.Kind != KindChat || len(rp.Messages) != 3 {
		t.Fatalf("got %+v", rp)
	}
	wantText(t, rp.Messages[0], "system", "s")
	wantText(t, rp.Messages[1], "user", "u")
	wantText(t, rp.Messages[2], "assistant", "a")
}

// TestRenderStaticOutputFormatChat covers bare ctx.output_format in a chat
// message plus a primitive arg in the next message.
func TestRenderStaticOutputFormatChat(t *testing.T) {
	fn := staticFn(
		`{{ _.role("system") }}{{ ctx.output_format }}{{ _.role("user") }}{{ topic }}`,
		primArg("topic", "string"),
	)
	rp := mustRenderStatic(t, fn, map[string]any{"topic": "cats"})
	if rp.Kind != KindChat || len(rp.Messages) != 2 {
		t.Fatalf("got %+v", rp)
	}
	wantText(t, rp.Messages[0], "system", staticOutputFormatBlock)
	wantText(t, rp.Messages[1], "user", "cats")
}

// TestRenderStaticCompletionOutputFormat covers bare ctx.output_format in a
// KindCompletion body (no role calls).
func TestRenderStaticCompletionOutputFormat(t *testing.T) {
	fn := staticFn(`{{ ctx.output_format }}`)
	rp := mustRenderStatic(t, fn, map[string]any{})
	if rp.Kind != KindCompletion {
		t.Fatalf("kind = %q, want completion", rp.Kind)
	}
	if rp.Completion != staticOutputFormatBlock {
		t.Fatalf("completion = %q, want %q", rp.Completion, staticOutputFormatBlock)
	}
}

// TestRenderStaticCommentsAndWhitespaceControl proves MiniJinja comments and
// {{- -}} whitespace-control variants are accepted around the allowlisted
// forms. A bare string-literal expression is NOT in the allowlist, so only
// argument interpolation appears here.
func TestRenderStaticCommentsAndWhitespaceControl(t *testing.T) {
	// {{- n }} strips the whitespace (the newline) immediately preceding it, so
	// "Head\n" collapses to "Head" before the interpolated value.
	fn := staticFn("{# a comment #}Head\n{{- n }}", primArg("n", "int"))
	rp := mustRenderStatic(t, fn, map[string]any{"n": int64(7)})
	if rp.Kind != KindCompletion {
		t.Fatalf("kind = %q, want completion", rp.Kind)
	}
	if rp.Completion != "Head7" {
		t.Fatalf("completion = %q, want %q", rp.Completion, "Head7")
	}
}
