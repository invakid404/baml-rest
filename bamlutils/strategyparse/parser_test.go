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
		{
			name:  "empty brackets",
			input: `[]`,
			want:  []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseStrategyOption(tt.input)
			if !reflect.DeepEqual(got, tt.want) && !(len(got) == 0 && len(tt.want) == 0) {
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
		{"empty list still accepted", "[]", nil},
	}
	for _, tt := range accepts {
		t.Run("accept/"+tt.name, func(t *testing.T) {
			got := ParseStrategyOption(tt.input)
			switch {
			case len(got) == 0 && len(tt.want) == 0:
				// empty-list shape — both sides empty, OK.
			case !reflect.DeepEqual(got, tt.want):
				t.Fatalf("ParseStrategyOption(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
