package strategyparse

import (
	"reflect"
	"testing"
)

func TestParseStrategyOption_BracketedString_StripsQuotes(t *testing.T) {
	// Verifies the regression fix for CodeRabbit round-robin cold review,
	// finding 4: a runtime-registered strategy override passed as a raw
	// string ("strategy [\"A\", \"B\"]") must parse down to unquoted
	// names so downstream resolution matches introspected client keys.
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "double-quoted with strategy prefix",
			input: `strategy ["ClientA", "ClientB"]`,
			want:  []string{"ClientA", "ClientB"},
		},
		{
			name:  "single-quoted without strategy prefix",
			input: `['ClientA', 'ClientB']`,
			want:  []string{"ClientA", "ClientB"},
		},
		{
			name:  "unquoted tokens",
			input: `strategy [A, B, C]`,
			want:  []string{"A", "B", "C"},
		},
		{
			name:  "whitespace-only separators",
			input: `[A B C]`,
			want:  []string{"A", "B", "C"},
		},
		{
			name:  "mixed quotes",
			input: `["A", 'B', C]`,
			want:  []string{"A", "B", "C"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseStrategyOption(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseStrategyOption(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseStrategyOption_StringSlice(t *testing.T) {
	got := ParseStrategyOption([]string{` "ClientA" `, "ClientB", ""})
	want := []string{"ClientA", "ClientB"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseStrategyOption_AnySlice(t *testing.T) {
	got := ParseStrategyOption([]any{`"ClientA"`, "ClientB"})
	want := []string{"ClientA", "ClientB"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseStrategyOption_AnySlice_NonStringInvalidates(t *testing.T) {
	// Heterogeneous []any with a non-string element is treated as invalid
	// (returns nil) so the caller falls back to the introspected chain.
	got := ParseStrategyOption([]any{"ClientA", 42})
	if got != nil {
		t.Fatalf("expected nil for heterogeneous slice, got %v", got)
	}
}

func TestParseStrategyOption_UnknownShape(t *testing.T) {
	if got := ParseStrategyOption(42); got != nil {
		t.Fatalf("expected nil for int input, got %v", got)
	}
	if got := ParseStrategyOption(nil); got != nil {
		t.Fatalf("expected nil for nil input, got %v", got)
	}
}

// TestParseStrategyOption_BracketedString_RequiresBrackets is the
// regression for CodeRabbit finding B. BAML upstream's ensure_array
// rejects non-list strategy values; we mirror that by refusing any
// string form that isn't explicitly bracketed. A previous revision
// happily accepted bare tokens — a client_registry entry with
// options.strategy = "ClientA" would silently collapse the strategy
// to a one-element chain instead of falling back to the introspected
// configuration.
func TestParseStrategyOption_BracketedString_RequiresBrackets(t *testing.T) {
	// Each of these should produce nil (parser rejection), causing the
	// caller to fall back to the introspected chain.
	rejects := []struct {
		name  string
		input string
	}{
		{"bare token", "ClientA"},
		{"bare token with strategy prefix", "strategy ClientA"},
		{"missing closing bracket", "strategy [ClientA"},
		{"missing opening bracket", "strategy ClientA]"},
		{"empty string", ""},
		{"whitespace only", "   "},
		{"strategy prefix only", "strategy "},
		{"token adjacent to brackets on wrong side", "ClientA ["},
		{"bracket in middle only", "Client[A]"},
		// Empty strategy arrays: BAML upstream's ensure_strategy
		// errors with "strategy must not be empty" — mirror that by
		// treating empty arrays as non-overrides rather than
		// accepted-but-empty chains.
		{"empty brackets", "[]"},
		{"empty brackets with prefix", "strategy []"},
		{"whitespace-only brackets", "[  ]"},
	}
	for _, tt := range rejects {
		t.Run("reject/"+tt.name, func(t *testing.T) {
			if got := ParseStrategyOption(tt.input); got != nil {
				t.Fatalf("ParseStrategyOption(%q) = %v, want nil (non-list input must be rejected)", tt.input, got)
			}
		})
	}

	// Valid bracketed forms must still be accepted, including the
	// pre-existing shapes the parser already handled.
	accepts := []struct {
		name  string
		input string
		want  []string
	}{
		{"strategy prefix + brackets", "strategy [ClientA]", []string{"ClientA"}},
		{"brackets only", "[ClientA]", []string{"ClientA"}},
		{"multi-element with commas", "[A, B, C]", []string{"A", "B", "C"}},
		{"whitespace around brackets", "  strategy [A, B]  ", []string{"A", "B"}},
	}
	for _, tt := range accepts {
		t.Run("accept/"+tt.name, func(t *testing.T) {
			got := ParseStrategyOption(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseStrategyOption(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseStrategyOption_EmptyListsCollapseToNil is the regression
// for the CodeRabbit follow-up on finding B. BAML upstream's
// ensure_strategy rejects empty strategy arrays ("strategy must not
// be empty" in baml-lib/llm-client/src/clients/helpers.rs:790-797);
// the parser mirrors that by returning nil for every shape that ends
// up with zero tokens.
//
// Caller contract (PR #192 cold-review-2 finding 1): the parser's
// nil/empty signal is the input to a *three-state* decision in the
// orchestrator's inspectStrategyOverride helper. Callers distinguish
//
//   - absent (no `strategy` key in Options): use introspected chain.
//   - valid (parser returned a non-empty chain): honour the override.
//   - present-but-invalid (key present, parser returned nil/empty):
//     route the request to legacy with PathReasonInvalidStrategyOverride
//     so BAML upstream's runtime emits the canonical
//     "strategy must be an array" / "strategy must not be empty"
//     error. A previous revision silently used the introspected chain
//     here, masking operator typos and diverging from BAML upstream.
func TestParseStrategyOption_EmptyListsCollapseToNil(t *testing.T) {
	cases := []struct {
		name  string
		input any
	}{
		{"empty []string", []string{}},
		{"empty []any", []any{}},
		{"string with empty brackets", "[]"},
		{"string with whitespace brackets", "[  ]"},
		{"string with prefix + empty brackets", "strategy []"},
		{"[]string of only blanks", []string{"", "  ", "\t"}},
		{"[]any of only blanks", []any{"", "  "}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseStrategyOption(tt.input); got != nil {
				t.Fatalf("ParseStrategyOption(%v) = %v, want nil (empty overrides must not clobber the introspected chain)", tt.input, got)
			}
		})
	}
}
