//go:build integration

package mockllm

import (
	"strings"
	"testing"
)

// TestValidateAnthropicCacheControl_Invalid covers the bug-shape cases the
// mock must reject so integration tests catch regressions of #304: a
// content-block `cache_control` object that exists but is missing the
// `type` discriminator. The validator is path-aware (messages.N.content.M)
// so the error message points at the offending block.
func TestValidateAnthropicCacheControl_Invalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		body          string
		wantSubstring string
	}{
		{
			name: "empty cache_control object",
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
			name: "cache_control with empty type string",
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
			name: "cache_control with non-string type",
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
			name: "second message carries malformed cache_control",
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
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateAnthropicCacheControl([]byte(tc.body))
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

// TestValidateAnthropicCacheControl_Valid covers the shapes the validator
// must let through unchanged: well-formed cache_control, string content
// (no per-block metadata possible), and the absent-cache_control case.
// The bridge fix in #304 makes the production code emit exactly the
// "ephemeral" shape, so a regression in the validator that flagged it
// would mask the real fix.
func TestValidateAnthropicCacheControl_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{
			name: "well-formed ephemeral cache_control",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}]
				}]
			}`,
		},
		{
			name: "no cache_control on any block",
			body: `{
				"model": "m",
				"messages": [{
					"role": "user",
					"content": [{"type": "text", "text": "hi"}]
				}]
			}`,
		},
		{
			name: "string content - no block-level metadata possible",
			body: `{
				"model": "m",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
		},
		{
			name: "mixed messages: some carry valid cache_control, some none",
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
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validateAnthropicCacheControl([]byte(tc.body)); err != nil {
				t.Errorf("expected nil validation error; got %+v", err)
			}
		})
	}
}
