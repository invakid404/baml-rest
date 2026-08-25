package nativeschema

import (
	"sort"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
)

func strictFiles(t *testing.T, sources map[string]string) []SourceFile {
	t.Helper()
	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	sort.Strings(names)
	var files []SourceFile
	for _, n := range names {
		f, err := bamlparser.ParseBytes(n, []byte(sources[n]))
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		files = append(files, SourceFile{File: f, Path: n})
	}
	return files
}

// shorthand is a provider/model client spec (contains "/"), which resolves
// without a declared client block — so a clean fixture needs no client<llm>.
const shorthand = "openai/gpt-4o"

func TestCheckProjectIntegrityClean(t *testing.T) {
	files := strictFiles(t, map[string]string{
		"a.baml": "class C { x string }\n" +
			"type Alias = C\n" +
			"client<llm> GPT4 { provider openai retry_policy R }\n" +
			"retry_policy R { max_retries 1 }\n" +
			"client<llm> FB { provider baml-fallback options { strategy [GPT4] } }\n" +
			// A stray `strategy` on a NON-strategy provider (openai) produces no
			// descriptor Strategy, so its (here undeclared) child must NOT fail strict
			// — the strict check gates child resolution on the strategy provider,
			// matching BuildClientGraph's emit guard (CodeRabbit MAJOR).
			"client<llm> NonStrat { provider openai options { strategy [Nope] } }\n" +
			"function F(c: C) -> Alias { client GPT4 prompt #\"x\"# }\n" +
			"function G() -> string { client " + shorthand + " prompt #\"x\"# }\n",
	})
	if err := CheckProjectIntegrity(files); err != nil {
		t.Errorf("clean project reported integrity errors: %v", err)
	}
}

func TestCheckProjectIntegrityViolations(t *testing.T) {
	cases := []struct {
		name    string
		sources map[string]string
		want    string
	}{
		{
			name:    "duplicate class",
			sources: map[string]string{"a.baml": "class C { x string }\n", "b.baml": "class C { y string }\n"},
			want:    `type "C" is declared more than once`,
		},
		{
			name:    "duplicate function",
			sources: map[string]string{"a.baml": "function F() -> string { client " + shorthand + " prompt #\"a\"# }\nfunction F() -> string { client " + shorthand + " prompt #\"b\"# }\n"},
			want:    `function "F" is declared more than once`,
		},
		{
			name:    "duplicate retry_policy",
			sources: map[string]string{"a.baml": "retry_policy R { max_retries 1 }\nretry_policy R { max_retries 2 }\n"},
			want:    `retry_policy "R" is declared more than once`,
		},
		{
			name:    "duplicate client",
			sources: map[string]string{"a.baml": "client<llm> C { provider openai }\n", "b.baml": "client<llm> C { provider openai }\n"},
			want:    `client "C" is declared more than once`,
		},
		{
			name:    "duplicate template_string",
			sources: map[string]string{"a.baml": "template_string T(x: string) #\"a\"#\ntemplate_string T(y: string) #\"b\"#\n"},
			want:    `template_string "T" is declared more than once`,
		},
		{
			name:    "unresolved type reference",
			sources: map[string]string{"a.baml": "function F(x: Missing) -> string { client " + shorthand + " prompt #\"x\"# }\n"},
			want:    `type reference "Missing" resolves to no declared class, enum, or alias`,
		},
		{
			name:    "alias RHS to missing type",
			sources: map[string]string{"a.baml": "type T = Missing\nfunction H() -> T { client " + shorthand + " prompt #\"x\"# }\n"},
			want:    `type reference "Missing" resolves to no declared class, enum, or alias`,
		},
		{
			name:    "client references undeclared retry_policy",
			sources: map[string]string{"a.baml": "client<llm> C { provider openai retry_policy Missing }\n"},
			want:    `client "C" references undeclared retry_policy "Missing"`,
		},
		{
			name:    "function references undeclared client",
			sources: map[string]string{"a.baml": "function F() -> string { client Nope prompt #\"x\"# }\n"},
			want:    `function "F" references undeclared client "Nope"`,
		},
		{
			name:    "strategy references undeclared child client",
			sources: map[string]string{"a.baml": "client<llm> FB { provider baml-fallback options { strategy [Nope] } }\n"},
			want:    `strategy client "FB" references undeclared child client "Nope"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckProjectIntegrity(strictFiles(t, tc.sources))
			if err == nil {
				t.Fatalf("want integrity error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
