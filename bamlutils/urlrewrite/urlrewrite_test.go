package urlrewrite

import (
	"testing"
)

func TestParseRules(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Rule
	}{
		{"empty", "", nil},
		{"single rule", "https://llm.mandel.ai=http://litellm:4000", []Rule{{From: "https://llm.mandel.ai", To: "http://litellm:4000"}}},
		{"multiple rules", "https://a.com=http://b:1;https://c.com=http://d:2", []Rule{
			{From: "https://a.com", To: "http://b:1"},
			{From: "https://c.com", To: "http://d:2"},
		}},
		{"whitespace", " https://a.com = http://b:1 ; https://c.com = http://d:2 ", []Rule{
			{From: "https://a.com", To: "http://b:1"},
			{From: "https://c.com", To: "http://d:2"},
		}},
		{"trailing semicolon", "https://a.com=http://b:1;", []Rule{{From: "https://a.com", To: "http://b:1"}}},
		{"no equals", "invalid", nil},
		{"empty from", "=http://b:1", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseRules(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseRules(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ParseRules(%q)[%d] = %v, want %v", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestApply(t *testing.T) {
	rules := []Rule{
		{From: "https://llm.mandel.ai", To: "http://litellm:4000"},
	}

	input := `client<llm> Claude45Sonnet {
  provider openai
  options {
    base_url "https://llm.mandel.ai"
    model "bedrock-claude-4.5-sonnet"
  }
}`

	want := `client<llm> Claude45Sonnet {
  provider openai
  options {
    base_url "http://litellm:4000"
    model "bedrock-claude-4.5-sonnet"
  }
}`

	got := Apply(input, rules)
	if got != want {
		t.Errorf("Apply() =\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyToURL(t *testing.T) {
	rules := []Rule{
		{From: "https://llm.mandel.ai", To: "http://litellm:4000"},
	}

	tests := []struct {
		url  string
		want string
	}{
		{"https://llm.mandel.ai", "http://litellm:4000"},
		{"https://llm.mandel.ai/v1/chat/completions", "http://litellm:4000/v1/chat/completions"},
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := ApplyToURL(tt.url, rules)
			if got != tt.want {
				t.Errorf("ApplyToURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}
