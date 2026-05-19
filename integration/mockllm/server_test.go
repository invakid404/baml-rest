//go:build integration

package mockllm

import (
	"strings"
	"testing"
)

// TestValidateCacheControl_Invalid covers the bug-shape cases the mock
// must reject so integration tests catch regressions of #304: any
// `cache_control` object that exists but is missing the wire-required
// `type` discriminator. Coverage spans the Anthropic content-block
// shape (where #304 was first reported) and the OpenAI/LiteLLM-shaped
// path that callers can use to route Anthropic metadata through
// openai-generic providers. The validator is body-shape gated and
// path-aware, so the error message points at the offending object
// regardless of endpoint.
func TestValidateCacheControl_Invalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		body          string
		wantSubstring string
	}{
		{
			name: "anthropic: empty cache_control object",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "hi", "cache_control": {}}]
				}]
			}`,
			wantSubstring: "messages.0.content.0.text.cache_control.type: Field required",
		},
		{
			name: "anthropic: cache_control with empty type string",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "hi", "cache_control": {"type": ""}}]
				}]
			}`,
			wantSubstring: "messages.0.content.0.text.cache_control.type: Field required",
		},
		{
			name: "anthropic: cache_control with non-string type",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "hi", "cache_control": {"type": 42}}]
				}]
			}`,
			wantSubstring: "messages.0.content.0.text.cache_control.type",
		},
		{
			name: "anthropic: second message carries malformed cache_control",
			body: `{
				"model": "m",
				"messages": [
					{"role": "system", "content": "ignore me"},
					{"role": "user", "content": [
						{"type": "text", "text": "first"},
						{"type": "text", "text": "second", "cache_control": {}}
					]}
				]
			}`,
			wantSubstring: "messages.1.content.1",
		},
		{
			name: "openai chat-completions: content-block cache_control leaks cache_type",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "hi", "cache_control": {"cache_type": "ephemeral"}}]
				}]
			}`,
			wantSubstring: "cache_control.type: Field required",
		},
		{
			name: "openai chat-completions: top-level cache_control with empty type",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": "hi",
					"cache_control": {"type": ""}
				}]
			}`,
			wantSubstring: "cache_control.type: Field required",
		},
		{
			name: "openai chat-completions: cache_control object replaced by string",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": "hi",
					"cache_control": "ephemeral"
				}]
			}`,
			wantSubstring: "cache_control: Input should be a valid dictionary",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateCacheControlObjectsRecursive([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected validation error; got nil")
			}
			if err.Type != "error" {
				t.Errorf("Type: got %q, want %q", err.Type, "error")
			}
			if err.Error.Type != "invalid_request_error" {
				t.Errorf("Error.Type: got %q, want %q", err.Error.Type, "invalid_request_error")
			}
			if !strings.Contains(err.Error.Message, tc.wantSubstring) {
				t.Errorf("Error.Message %q does not contain %q", err.Error.Message, tc.wantSubstring)
			}
		})
	}
}

// TestValidateCacheControl_Valid covers the shapes the validator must
// let through unchanged: well-formed cache_control across Anthropic and
// OpenAI body shapes, string content (no per-block metadata possible),
// and the absent-cache_control case. The bridge fix in #304 makes the
// production code emit exactly the "ephemeral" shape on both endpoints
// after the class-free literal template change in 5c223a637, so a
// regression in the validator that flagged it would mask the real fix.
func TestValidateCacheControl_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{
			name: "anthropic: well-formed ephemeral cache_control",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}]
				}]
			}`,
		},
		{
			name: "anthropic: no cache_control on any block",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "hi"}]
				}]
			}`,
		},
		{
			name: "anthropic: string content - no block-level metadata possible",
			body: `{
				"model": "m",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
		},
		{
			name: "anthropic: mixed messages: some carry valid cache_control, some none",
			body: `{
				"model": "m",
				"messages": [
					{"role": "system", "content": "ignore"},
					{"role": "user", "content": [
						{"type": "text", "text": "a"},
						{"type": "text", "text": "b", "cache_control": {"type": "ephemeral"}}
					]},
					{"role": "assistant", "content": "ok"}
				]
			}`,
		},
		{
			name: "openai chat-completions: well-formed top-level cache_control",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": "hi",
					"cache_control": {"type": "ephemeral"}
				}]
			}`,
		},
		{
			name: "openai chat-completions: well-formed content-block cache_control",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}]
				}]
			}`,
		},
		{
			name: "openai chat-completions: no cache_control anywhere",
			body: `{
				"model": "m",
				"messages": [{"role": "user", "content": "hi"}]
			}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validateCacheControlObjectsRecursive([]byte(tc.body)); err != nil {
				t.Errorf("expected nil validation error; got %+v", err)
			}
		})
	}
}
