//go:build debug

package buildrequest

import (
	"testing"
)

func TestParseCallUnsupportedProvidersEnv(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  map[string]bool
	}{
		{"empty", "", nil},
		{"single", "openai", map[string]bool{"openai": true}},
		{
			"multi_comma",
			"openai,anthropic",
			map[string]bool{"openai": true, "anthropic": true},
		},
		{
			"whitespace_and_empty",
			"  openai , , anthropic  ",
			map[string]bool{"openai": true, "anthropic": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BAML_REST_CALL_UNSUPPORTED_PROVIDERS", tc.value)
			got := parseCallUnsupportedProvidersEnv()
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %d, want %d (got=%v)", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q: got %v, want %v", k, got[k], v)
				}
			}
		})
	}
}
