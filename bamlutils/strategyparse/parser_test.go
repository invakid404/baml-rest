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
