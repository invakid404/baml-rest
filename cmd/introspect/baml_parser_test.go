package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestBamlConfig() *bamlConfig {
	return &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
		fallbackChains:    make(map[string][]string),
		roundRobinStart:   make(map[string]int),
	}
}

func TestParseBamlFile_ClientAndFunction(t *testing.T) {
	cfg := newTestBamlConfig()

	content := `
// Test client
client<llm> MyOpenAI {
    provider openai
    options {
        model "gpt-4o"
        api_key "test"
    }
}

client<llm> MyAnthropic {
    provider anthropic
    retry_policy MyRetryPolicy
    options {
        model "claude-3"
    }
}

function GetPerson(description: string) -> Person {
    client MyOpenAI
    prompt #"Extract: {{ description }}"#
}

function GetSummary(text: string) -> string {
    client MyAnthropic
    prompt #"Summarize: {{ text }}"#
}
`
	parseBamlFile(cfg, content)

	// Verify clients
	if cfg.clientProvider["MyOpenAI"] != "openai" {
		t.Errorf("expected MyOpenAI provider=openai, got %q", cfg.clientProvider["MyOpenAI"])
	}
	if cfg.clientProvider["MyAnthropic"] != "anthropic" {
		t.Errorf("expected MyAnthropic provider=anthropic, got %q", cfg.clientProvider["MyAnthropic"])
	}
	if cfg.clientRetryPolicy["MyAnthropic"] != "MyRetryPolicy" {
		t.Errorf("expected MyAnthropic retry_policy=MyRetryPolicy, got %q", cfg.clientRetryPolicy["MyAnthropic"])
	}

	// Verify functions
	if cfg.functionClient["GetPerson"] != "MyOpenAI" {
		t.Errorf("expected GetPerson client=MyOpenAI, got %q", cfg.functionClient["GetPerson"])
	}
	if cfg.functionClient["GetSummary"] != "MyAnthropic" {
		t.Errorf("expected GetSummary client=MyAnthropic, got %q", cfg.functionClient["GetSummary"])
	}
}

func TestParseBamlFile_RetryPolicy(t *testing.T) {
	cfg := newTestBamlConfig()

	content := `
retry_policy ConstantRetry {
    max_retries 3
    strategy {
        type constant_delay
        delay_ms 200
    }
}

retry_policy ExponentialRetry {
    max_retries 5
    strategy {
        type exponential_backoff
        delay_ms 100
        multiplier 2.0
        max_delay_ms 5000
    }
}
`
	parseBamlFile(cfg, content)

	// Verify constant retry policy
	cr, ok := cfg.retryPolicies["ConstantRetry"]
	if !ok {
		t.Fatal("ConstantRetry policy not found")
	}
	if cr.maxRetries != 3 {
		t.Errorf("expected maxRetries=3, got %d", cr.maxRetries)
	}
	if cr.strategy != "constant_delay" {
		t.Errorf("expected strategy=constant_delay, got %q", cr.strategy)
	}
	if cr.delayMs != 200 {
		t.Errorf("expected delayMs=200, got %d", cr.delayMs)
	}

	// Verify exponential retry policy
	er, ok := cfg.retryPolicies["ExponentialRetry"]
	if !ok {
		t.Fatal("ExponentialRetry policy not found")
	}
	if er.maxRetries != 5 {
		t.Errorf("expected maxRetries=5, got %d", er.maxRetries)
	}
	if er.strategy != "exponential_backoff" {
		t.Errorf("expected strategy=exponential_backoff, got %q", er.strategy)
	}
	if er.delayMs != 100 {
		t.Errorf("expected delayMs=100, got %d", er.delayMs)
	}
	if er.multiplier != 2.0 {
		t.Errorf("expected multiplier=2.0, got %f", er.multiplier)
	}
	if er.maxDelayMs != 5000 {
		t.Errorf("expected maxDelayMs=5000, got %d", er.maxDelayMs)
	}
}

func TestParseBamlSourceDir(t *testing.T) {
	// Create a temp directory with .baml files
	dir := t.TempDir()

	clientsContent := `
client<llm> GPT4 {
    provider openai
    options {
        model "gpt-4"
    }
}
`
	functionsContent := `
function Summarize(text: string) -> string {
    client GPT4
    prompt #"Summarize: {{ text }}"#
}
`
	if err := os.WriteFile(filepath.Join(dir, "clients.baml"), []byte(clientsContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "functions.baml"), []byte(functionsContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := parseBamlSourceDir(dir)

	if cfg.clientProvider["GPT4"] != "openai" {
		t.Errorf("expected GPT4 provider=openai, got %q", cfg.clientProvider["GPT4"])
	}
	if cfg.functionClient["Summarize"] != "GPT4" {
		t.Errorf("expected Summarize client=GPT4, got %q", cfg.functionClient["Summarize"])
	}
}

func TestParseBamlSourceDir_NonExistent(t *testing.T) {
	// Should return empty config without error
	cfg := parseBamlSourceDir("/nonexistent/path")
	if len(cfg.clientProvider) != 0 {
		t.Error("expected empty clientProvider for non-existent dir")
	}
	if len(cfg.clientRetryPolicy) != 0 {
		t.Error("expected empty clientRetryPolicy for non-existent dir")
	}
	if len(cfg.functionClient) != 0 {
		t.Error("expected empty functionClient for non-existent dir")
	}
	if len(cfg.retryPolicies) != 0 {
		t.Error("expected empty retryPolicies for non-existent dir")
	}
}

func TestParseBamlFile_IntegrationTestData(t *testing.T) {
	// Parse the actual integration test .baml files to verify real-world syntax
	dir := filepath.Join("..", "..", "integration", "testdata", "baml_src")
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			t.Skip("integration testdata not available")
		}
		t.Fatalf("os.Stat(%q): %v", dir, err)
	}
	cfg := parseBamlSourceDir(dir)

	// The test data has a TestClient with provider openai
	if cfg.clientProvider["TestClient"] != "openai" {
		t.Errorf("expected TestClient provider=openai, got %q", cfg.clientProvider["TestClient"])
	}

	// GetGreeting uses TestClient
	if cfg.functionClient["GetGreeting"] != "TestClient" {
		t.Errorf("expected GetGreeting client=TestClient, got %q", cfg.functionClient["GetGreeting"])
	}
}

func TestParseBamlFile_QuotedValues(t *testing.T) {
	// Real builds inject dynamic.baml which uses `provider "openai"` (quoted).
	// The parser must strip surrounding quotes.
	cfg := newTestBamlConfig()

	content := `
client<llm> DynamicClient {
    provider "openai"
    options {
        model "gpt-4o-mini"
    }
}

client<llm> QuotedRetryClient {
    provider "anthropic"
    retry_policy "MyPolicy"
    options {
        model "claude-3"
    }
}

function DynamicCall(input: string) -> string {
    client DynamicClient
    prompt #"{{ input }}"#
}
`
	parseBamlFile(cfg, content)

	// Verify quotes are stripped from provider
	if cfg.clientProvider["DynamicClient"] != "openai" {
		t.Errorf("expected DynamicClient provider=openai (unquoted), got %q", cfg.clientProvider["DynamicClient"])
	}
	if cfg.clientProvider["QuotedRetryClient"] != "anthropic" {
		t.Errorf("expected QuotedRetryClient provider=anthropic (unquoted), got %q", cfg.clientProvider["QuotedRetryClient"])
	}

	// Verify quotes are stripped from retry_policy reference
	if cfg.clientRetryPolicy["QuotedRetryClient"] != "MyPolicy" {
		t.Errorf("expected retry_policy=MyPolicy (unquoted), got %q", cfg.clientRetryPolicy["QuotedRetryClient"])
	}

	// Verify function resolves correctly
	if cfg.functionClient["DynamicCall"] != "DynamicClient" {
		t.Errorf("expected DynamicCall client=DynamicClient, got %q", cfg.functionClient["DynamicCall"])
	}
}

func TestStripBamlQuotes(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`"openai"`, "openai"},
		{`openai`, "openai"},
		{`"anthropic"`, "anthropic"},
		{`""`, ""},
		{`"a"`, "a"},
		{`a`, "a"},
		{``, ""},
	}
	for _, tt := range tests {
		if got := stripBamlQuotes(tt.input); got != tt.expected {
			t.Errorf("stripBamlQuotes(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestParseBamlSourceDir_NestedDirectories(t *testing.T) {
	// Create a temp directory tree with .baml files in subdirectories
	dir := t.TempDir()

	// Top-level file
	if err := os.WriteFile(filepath.Join(dir, "generators.baml"), []byte(`
// generator config (no client/function blocks)
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Subdirectory with clients
	clientsDir := filepath.Join(dir, "clients")
	if err := os.MkdirAll(clientsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientsDir, "openai.baml"), []byte(`
client<llm> MyOpenAI {
    provider openai
    options { model "gpt-4" }
}
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientsDir, "anthropic.baml"), []byte(`
client<llm> MyClaude {
    provider anthropic
    options { model "claude-3" }
}
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Nested subdirectory with functions
	funcsDir := filepath.Join(dir, "functions", "extraction")
	if err := os.MkdirAll(funcsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(funcsDir, "extract.baml"), []byte(`
function ExtractData(input: string) -> string {
    client MyOpenAI
    prompt #"{{ input }}"#
}
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Another nested directory with retry policies
	policiesDir := filepath.Join(dir, "policies")
	if err := os.MkdirAll(policiesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(policiesDir, "retry.baml"), []byte(`
retry_policy AggressiveRetry {
    max_retries 5
    strategy {
        type exponential_backoff
        delay_ms 100
        multiplier 2.0
        max_delay_ms 10000
    }
}
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := parseBamlSourceDir(dir)

	// Verify clients from subdirectory
	if cfg.clientProvider["MyOpenAI"] != "openai" {
		t.Errorf("expected MyOpenAI provider=openai, got %q", cfg.clientProvider["MyOpenAI"])
	}
	if cfg.clientProvider["MyClaude"] != "anthropic" {
		t.Errorf("expected MyClaude provider=anthropic, got %q", cfg.clientProvider["MyClaude"])
	}

	// Verify function from nested subdirectory
	if cfg.functionClient["ExtractData"] != "MyOpenAI" {
		t.Errorf("expected ExtractData client=MyOpenAI, got %q", cfg.functionClient["ExtractData"])
	}

	// Verify retry policy from subdirectory
	rp, ok := cfg.retryPolicies["AggressiveRetry"]
	if !ok {
		t.Fatal("AggressiveRetry policy not found")
	}
	if rp.maxRetries != 5 {
		t.Errorf("expected maxRetries=5, got %d", rp.maxRetries)
	}
	if rp.strategy != "exponential_backoff" {
		t.Errorf("expected strategy=exponential_backoff, got %q", rp.strategy)
	}
}

func TestParseBamlFile_PromptTextIgnored(t *testing.T) {
	// Prompt body can contain lines that look like config keys.
	// The parser must stop scanning for config once it hits the prompt block.
	cfg := newTestBamlConfig()

	content := `
client<llm> RealClient {
    provider openai
    options {
        model "gpt-4"
        provider "this should be ignored because it's inside options"
    }
}

function ExtractInfo(text: string) -> string {
    client RealClient
    prompt #"
        You are a helpful assistant.
        client OpenAI is the best LLM provider.
        provider anthropic is also good.
        retry_policy aggressive is recommended.
        Please extract info from: {{ text }}
    "#
}
`
	parseBamlFile(cfg, content)

	// The function should use RealClient, NOT "OpenAI" from the prompt text
	if cfg.functionClient["ExtractInfo"] != "RealClient" {
		t.Errorf("expected client=RealClient, got %q (prompt text leaked)", cfg.functionClient["ExtractInfo"])
	}

	// The client should have provider "openai", not overridden by options block text
	if cfg.clientProvider["RealClient"] != "openai" {
		t.Errorf("expected provider=openai, got %q (options block text leaked)", cfg.clientProvider["RealClient"])
	}
}

func TestParseBamlFile_SingleLineBlocks(t *testing.T) {
	// Some .baml files use compact single-line formatting.
	// The parser must handle blocks where opening and closing braces are on one line.
	cfg := newTestBamlConfig()

	content := `
client<llm> CompactClient { provider anthropic }

function CompactFunc(input: string) -> string { client CompactClient }

retry_policy CompactRetry { max_retries 2 }

client<llm> NormalClient {
    provider openai
    options { model "gpt-4" }
}

function NormalFunc(input: string) -> string {
    client NormalClient
    prompt #"{{ input }}"#
}
`
	parseBamlFile(cfg, content)

	// Single-line client block
	if cfg.clientProvider["CompactClient"] != "anthropic" {
		t.Errorf("expected CompactClient provider=anthropic, got %q", cfg.clientProvider["CompactClient"])
	}

	// Single-line function block
	if cfg.functionClient["CompactFunc"] != "CompactClient" {
		t.Errorf("expected CompactFunc client=CompactClient, got %q", cfg.functionClient["CompactFunc"])
	}

	// Single-line retry policy block
	rp, ok := cfg.retryPolicies["CompactRetry"]
	if !ok {
		t.Fatal("CompactRetry policy not found")
	}
	if rp.maxRetries != 2 {
		t.Errorf("expected CompactRetry maxRetries=2, got %d", rp.maxRetries)
	}

	// Normal multi-line blocks still work
	if cfg.clientProvider["NormalClient"] != "openai" {
		t.Errorf("expected NormalClient provider=openai, got %q", cfg.clientProvider["NormalClient"])
	}
	if cfg.functionClient["NormalFunc"] != "NormalClient" {
		t.Errorf("expected NormalFunc client=NormalClient, got %q", cfg.functionClient["NormalFunc"])
	}
}

func TestParseBamlFile_MultiEntryCompactBlocks(t *testing.T) {
	// Single-line blocks with multiple key-value entries must be split correctly.
	cfg := newTestBamlConfig()

	content := `
client<llm> Foo { provider openai retry_policy Fast }

retry_policy Fast { max_retries 2 type constant_delay delay_ms 100 }
`
	parseBamlFile(cfg, content)

	if cfg.clientProvider["Foo"] != "openai" {
		t.Errorf("expected Foo provider=openai, got %q", cfg.clientProvider["Foo"])
	}
	if cfg.clientRetryPolicy["Foo"] != "Fast" {
		t.Errorf("expected Foo retry_policy=Fast, got %q", cfg.clientRetryPolicy["Foo"])
	}

	rp, ok := cfg.retryPolicies["Fast"]
	if !ok {
		t.Fatal("Fast policy not found")
	}
	if rp.maxRetries != 2 {
		t.Errorf("expected maxRetries=2, got %d", rp.maxRetries)
	}
	if rp.strategy != "constant_delay" {
		t.Errorf("expected strategy=constant_delay, got %q", rp.strategy)
	}
	if rp.delayMs != 100 {
		t.Errorf("expected delayMs=100, got %d", rp.delayMs)
	}
}

func TestSplitInlineStatements(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"provider openai", []string{"provider openai"}},
		{"provider openai retry_policy Fast", []string{"provider openai", "retry_policy Fast"}},
		{"max_retries 2 type constant_delay delay_ms 100", []string{"max_retries 2", "type constant_delay", "delay_ms 100"}},
		{"client MyClient", []string{"client MyClient"}},
		{"", []string{""}},
	}

	for _, tt := range tests {
		got := splitInlineStatements(tt.input)
		if len(got) != len(tt.expected) {
			t.Errorf("splitInlineStatements(%q): got %d items %v, want %d items %v",
				tt.input, len(got), got, len(tt.expected), tt.expected)
			continue
		}
		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("splitInlineStatements(%q)[%d]: got %q, want %q",
					tt.input, i, got[i], tt.expected[i])
			}
		}
	}
}

func TestParseBamlFile_InlineComments(t *testing.T) {
	cfg := newTestBamlConfig()

	content := `
client<llm> TestClient {
    provider openai // default provider
    retry_policy Fast // production retry
    options {
        model "gpt-4o"
        base_url "http://localhost:11434"  // Placeholder, overridden in tests
        api_key "test-key"
    }
}

function GetGreeting(name: string) -> string {
    client TestClient // override in tests
    prompt #"Say hello to {{ name }}"#
}

retry_policy Fast {
    max_retries 3 // keep it reasonable
    strategy {
        type constant_delay // simple strategy
        delay_ms 200 // 200ms between retries
    }
}
`
	parseBamlFile(cfg, content)

	if cfg.clientProvider["TestClient"] != "openai" {
		t.Errorf("expected provider=openai, got %q (inline comment leaked)", cfg.clientProvider["TestClient"])
	}
	if cfg.clientRetryPolicy["TestClient"] != "Fast" {
		t.Errorf("expected retry_policy=Fast, got %q (inline comment leaked)", cfg.clientRetryPolicy["TestClient"])
	}
	if cfg.functionClient["GetGreeting"] != "TestClient" {
		t.Errorf("expected client=TestClient, got %q (inline comment leaked)", cfg.functionClient["GetGreeting"])
	}

	rp, ok := cfg.retryPolicies["Fast"]
	if !ok {
		t.Fatal("Fast policy not found")
	}
	if rp.maxRetries != 3 {
		t.Errorf("expected maxRetries=3, got %d (inline comment leaked)", rp.maxRetries)
	}
	if rp.strategy != "constant_delay" {
		t.Errorf("expected strategy=constant_delay, got %q (inline comment leaked)", rp.strategy)
	}
	if rp.delayMs != 200 {
		t.Errorf("expected delayMs=200, got %d (inline comment leaked)", rp.delayMs)
	}
}

func TestParseBamlFile_KeysAfterNestedSections(t *testing.T) {
	cfg := newTestBamlConfig()

	content := `
client<llm> MyClient {
    options {
        model "gpt-4"
        api_key "test"
    }
    provider openai
    retry_policy MyRetry
}

function MyFunc(input: string) -> string {
    prompt #"Process: {{ input }}"#
    client MyClient
}
`
	parseBamlFile(cfg, content)

	if cfg.clientProvider["MyClient"] != "openai" {
		t.Errorf("expected provider=openai after options block, got %q", cfg.clientProvider["MyClient"])
	}
	if cfg.clientRetryPolicy["MyClient"] != "MyRetry" {
		t.Errorf("expected retry_policy=MyRetry after options block, got %q", cfg.clientRetryPolicy["MyClient"])
	}
	if cfg.functionClient["MyFunc"] != "MyClient" {
		t.Errorf("expected client=MyClient after prompt block, got %q", cfg.functionClient["MyFunc"])
	}
}

func TestParseBamlFile_MultilineRawPromptBraces(t *testing.T) {
	cfg := newTestBamlConfig()

	content := `
function BraceFunc(input: string) -> string {
    client TestClient
    prompt #"
        Given the JSON object { "key": "value" },
        extract the { nested { data } } from it.
        Use format: { "result": "..." }
    "#
}

client<llm> TestClient {
    provider anthropic
}
`
	parseBamlFile(cfg, content)

	if cfg.functionClient["BraceFunc"] != "TestClient" {
		t.Errorf("expected client=TestClient, got %q", cfg.functionClient["BraceFunc"])
	}
	if cfg.clientProvider["TestClient"] != "anthropic" {
		t.Errorf("expected provider=anthropic, got %q (brace-heavy prompt perturbed block depth)", cfg.clientProvider["TestClient"])
	}
}

func TestStripInlineComment(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"openai // default", "openai"},
		{"openai", "openai"},
		{"Fast // production", "Fast"},
		{"3 // keep it reasonable", "3"},
		{"// full line comment", ""},
		{"no comment here", "no comment here"},
		{"", ""},
		// Escaped quotes: // inside escaped-quote context should not end the string
		{`"value with \" escaped // not a comment" // real comment`, `"value with \" escaped // not a comment"`},
		// Double-escaped backslash before quote: \\" means the quote IS real
		{`"val\\\\" // comment`, `"val\\\\"`},
		// URL inside quotes: // should not be treated as comment
		{`"https://example.com" // comment`, `"https://example.com"`},
	}
	for _, tt := range tests {
		if got := stripInlineComment(tt.input); got != tt.expected {
			t.Errorf("stripInlineComment(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestParseBamlFile_NestedSingleLineBlock(t *testing.T) {
	cfg := newTestBamlConfig()
	content := `
client<llm> MyClient {
    options { provider "should-be-ignored" }
    provider openai
}
`
	parseBamlFile(cfg, content)
	if cfg.clientProvider["MyClient"] != "openai" {
		t.Errorf("expected provider=openai, got %q (nested single-line options leaked)", cfg.clientProvider["MyClient"])
	}
}

func TestParseBamlFile_KeywordsInQuotedValues(t *testing.T) {
	// Keywords inside quoted values must not trigger splits
	cfg := newTestBamlConfig()

	content := `
client<llm> QuotedClient { provider "my client provider" }
`
	parseBamlFile(cfg, content)

	if cfg.clientProvider["QuotedClient"] != "my client provider" {
		t.Errorf("expected provider='my client provider', got %q (keyword in quoted value caused misparsing)", cfg.clientProvider["QuotedClient"])
	}
}

func TestParseBamlFile_NestedStrategyBlock(t *testing.T) {
	// Keywords inside nested strategy { ... } blocks must not be split
	// as top-level statements by splitInlineStatements
	cfg := newTestBamlConfig()

	// Multi-line retry_policy with strategy block (the normal form)
	content := `
retry_policy MyRetry {
    max_retries 3
    strategy {
        type constant_delay
        delay_ms 200
    }
}
`
	parseBamlFile(cfg, content)

	rp, ok := cfg.retryPolicies["MyRetry"]
	if !ok {
		t.Fatal("MyRetry policy not found")
	}
	if rp.maxRetries != 3 {
		t.Errorf("expected maxRetries=3, got %d", rp.maxRetries)
	}
	if rp.strategy != "constant_delay" {
		t.Errorf("expected strategy=constant_delay, got %q", rp.strategy)
	}
	if rp.delayMs != 200 {
		t.Errorf("expected delayMs=200, got %d", rp.delayMs)
	}
}

func TestMaskInlineContent(t *testing.T) {
	tests := []struct {
		input string
	}{
		{`provider openai`},
		{`provider "my client provider"`},
		{`strategy { type constant_delay }`},
		{`max_retries 3 strategy { type x }`},
		{`provider openai // retry_policy Fast`},
	}

	for _, tt := range tests {
		got := maskInlineContent(tt.input)
		if len(got) != len(tt.input) {
			t.Errorf("maskInlineContent(%q): length changed from %d to %d", tt.input, len(tt.input), len(got))
		}
		// Verify masked content doesn't contain keywords that were inside quotes/braces
		if tt.input == `provider "my client provider"` {
			if strings.Contains(got, "client") {
				t.Errorf("maskInlineContent(%q) still contains 'client': %q", tt.input, got)
			}
		}
		if tt.input == `strategy { type constant_delay }` {
			if strings.Contains(got, "type") {
				t.Errorf("maskInlineContent(%q) still contains 'type': %q", tt.input, got)
			}
		}
		if tt.input == `provider openai // retry_policy Fast` {
			if strings.Contains(got, "retry_policy") {
				t.Errorf("maskInlineContent(%q) still contains 'retry_policy' from comment: %q", tt.input, got)
			}
		}
	}
}

func TestParseBamlFile_MaxDelayMsCompactBlock(t *testing.T) {
	// max_delay_ms must not be split at the embedded delay_ms keyword
	cfg := newTestBamlConfig()

	content := `
retry_policy ExpRetry { max_retries 3 max_delay_ms 5000 delay_ms 100 }
`
	parseBamlFile(cfg, content)

	rp, ok := cfg.retryPolicies["ExpRetry"]
	if !ok {
		t.Fatal("ExpRetry policy not found")
	}
	if rp.maxRetries != 3 {
		t.Errorf("expected maxRetries=3, got %d", rp.maxRetries)
	}
	if rp.maxDelayMs != 5000 {
		t.Errorf("expected maxDelayMs=5000, got %d", rp.maxDelayMs)
	}
	if rp.delayMs != 100 {
		t.Errorf("expected delayMs=100, got %d", rp.delayMs)
	}
}

func TestParseBamlFile_CompactBlockWithInlineComment(t *testing.T) {
	// Keywords in inline comments must not trigger splits
	cfg := newTestBamlConfig()

	content := `
client<llm> CommentClient { provider openai // retry_policy Fast }
`
	parseBamlFile(cfg, content)

	if cfg.clientProvider["CommentClient"] != "openai" {
		t.Errorf("expected provider=openai, got %q", cfg.clientProvider["CommentClient"])
	}
	if cfg.clientRetryPolicy["CommentClient"] != "" {
		t.Errorf("expected no retry_policy (was in comment), got %q", cfg.clientRetryPolicy["CommentClient"])
	}
}

func TestParseBamlFile_URLInQuotedValueCompactBlock(t *testing.T) {
	// A URL containing // inside a quoted value must not be treated as a comment.
	// The // must not truncate later statements in the compact block.
	cfg := newTestBamlConfig()

	content := `
client<llm> URLClient { provider "https://api.example.com" retry_policy Fast }

retry_policy Fast {
    max_retries 2
}
`
	parseBamlFile(cfg, content)

	if cfg.clientProvider["URLClient"] != "https://api.example.com" {
		t.Errorf("expected provider='https://api.example.com', got %q (URL was truncated at //)", cfg.clientProvider["URLClient"])
	}
	if cfg.clientRetryPolicy["URLClient"] != "Fast" {
		t.Errorf("expected retry_policy=Fast, got %q (// in URL truncated remaining statements)", cfg.clientRetryPolicy["URLClient"])
	}
}

func TestParseBamlFile_CompactRetryPolicyWithStrategy(t *testing.T) {
	// A one-line retry_policy with an inline strategy { ... } block
	// must correctly parse the strategy contents.
	cfg := newTestBamlConfig()

	content := `
retry_policy Fast { max_retries 3 strategy { type constant_delay delay_ms 100 } }
`
	parseBamlFile(cfg, content)

	rp, ok := cfg.retryPolicies["Fast"]
	if !ok {
		t.Fatal("Fast policy not found")
	}
	if rp.maxRetries != 3 {
		t.Errorf("expected maxRetries=3, got %d", rp.maxRetries)
	}
	if rp.strategy != "constant_delay" {
		t.Errorf("expected strategy=constant_delay, got %q", rp.strategy)
	}
	if rp.delayMs != 100 {
		t.Errorf("expected delayMs=100, got %d", rp.delayMs)
	}
}

func TestParseBamlFile_CompactRetryPolicyExponential(t *testing.T) {
	cfg := newTestBamlConfig()

	content := `
retry_policy ExpRetry { max_retries 5 strategy { type exponential_backoff delay_ms 200 multiplier 2.0 max_delay_ms 10000 } }
`
	parseBamlFile(cfg, content)

	rp, ok := cfg.retryPolicies["ExpRetry"]
	if !ok {
		t.Fatal("ExpRetry policy not found")
	}
	if rp.maxRetries != 5 {
		t.Errorf("expected maxRetries=5, got %d", rp.maxRetries)
	}
	if rp.strategy != "exponential_backoff" {
		t.Errorf("expected strategy=exponential_backoff, got %q", rp.strategy)
	}
	if rp.delayMs != 200 {
		t.Errorf("expected delayMs=200, got %d", rp.delayMs)
	}
	if rp.multiplier != 2.0 {
		t.Errorf("expected multiplier=2.0, got %f", rp.multiplier)
	}
	if rp.maxDelayMs != 10000 {
		t.Errorf("expected maxDelayMs=10000, got %d", rp.maxDelayMs)
	}
}

func TestParseBamlFile_FallbackChain_MultiLine(t *testing.T) {
	cfg := newTestBamlConfig()
	content := `
client<llm> MyFallback {
    provider baml-fallback
    options {
        strategy [ClientA, ClientB, ClientC]
    }
}

client<llm> ClientA {
    provider openai
}

client<llm> ClientB {
    provider anthropic
}

client<llm> ClientC {
    provider google-ai
}
`
	parseBamlFile(cfg, content)

	chain, ok := cfg.fallbackChains["MyFallback"]
	if !ok {
		t.Fatal("expected fallback chain for MyFallback")
	}
	if len(chain) != 3 {
		t.Fatalf("expected 3 children, got %d: %v", len(chain), chain)
	}
	if chain[0] != "ClientA" || chain[1] != "ClientB" || chain[2] != "ClientC" {
		t.Errorf("unexpected chain: %v", chain)
	}
	if cfg.clientProvider["MyFallback"] != "baml-fallback" {
		t.Errorf("expected provider 'baml-fallback', got %q", cfg.clientProvider["MyFallback"])
	}
}

func TestParseBamlFile_FallbackChain_Inline(t *testing.T) {
	cfg := newTestBamlConfig()
	content := `
client<llm> InlineFallback {
    provider baml-fallback
    options { strategy [Fast, Slow] }
}
`
	parseBamlFile(cfg, content)

	chain, ok := cfg.fallbackChains["InlineFallback"]
	if !ok {
		t.Fatal("expected fallback chain for InlineFallback")
	}
	if len(chain) != 2 {
		t.Fatalf("expected 2 children, got %d: %v", len(chain), chain)
	}
	if chain[0] != "Fast" || chain[1] != "Slow" {
		t.Errorf("unexpected chain: %v", chain)
	}
}

func TestParseBamlFile_FallbackChain_InlineStrategyAfterModel(t *testing.T) {
	cfg := newTestBamlConfig()
	content := `
client<llm> InlineFallback {
    provider baml-fallback
    options { model "x" strategy [Fast, Slow] }
}
`
	parseBamlFile(cfg, content)

	chain, ok := cfg.fallbackChains["InlineFallback"]
	if !ok {
		t.Fatal("expected fallback chain for InlineFallback when strategy is not the first inline option")
	}
	if len(chain) != 2 {
		t.Fatalf("expected 2 children, got %d: %v", len(chain), chain)
	}
	if chain[0] != "Fast" || chain[1] != "Slow" {
		t.Errorf("unexpected chain: %v", chain)
	}
}

func TestParseBamlFile_NonFallbackClient_NoChain(t *testing.T) {
	cfg := newTestBamlConfig()
	content := `
client<llm> RegularClient {
    provider openai
    options {
        model "gpt-4"
    }
}
`
	parseBamlFile(cfg, content)

	if len(cfg.fallbackChains) != 0 {
		t.Errorf("expected no fallback chains for regular client, got %v", cfg.fallbackChains)
	}
}

func TestParseBamlFile_FallbackChain_StrategyOnClosingBraceLine(t *testing.T) {
	cfg := newTestBamlConfig()
	content := `
client<llm> ClosingBrace {
    provider baml-fallback
    options {
        strategy [ClientA, ClientB] }
}
`
	parseBamlFile(cfg, content)

	chain, ok := cfg.fallbackChains["ClosingBrace"]
	if !ok {
		t.Fatal("expected fallback chain for ClosingBrace (strategy on same line as closing })")
	}
	if len(chain) != 2 {
		t.Fatalf("expected 2 children, got %d: %v", len(chain), chain)
	}
	if chain[0] != "ClientA" || chain[1] != "ClientB" {
		t.Errorf("unexpected chain: %v", chain)
	}
}

func TestParseBamlFile_FallbackChain_MultiLineStrategy(t *testing.T) {
	cfg := newTestBamlConfig()
	content := `
client<llm> MultiLineStrategy {
    provider baml-fallback
    options {
        strategy [
            ClientA,
            ClientB,
            ClientC,
        ]
    }
}
`
	parseBamlFile(cfg, content)

	chain, ok := cfg.fallbackChains["MultiLineStrategy"]
	if !ok {
		t.Fatal("expected fallback chain for MultiLineStrategy (multi-line strategy list)")
	}
	if len(chain) != 3 {
		t.Fatalf("expected 3 children, got %d: %v", len(chain), chain)
	}
	if chain[0] != "ClientA" || chain[1] != "ClientB" || chain[2] != "ClientC" {
		t.Errorf("unexpected chain: %v", chain)
	}
}

func TestParseBamlFile_FallbackChain_MultiLineNoTrailingComma(t *testing.T) {
	cfg := newTestBamlConfig()
	content := `
client<llm> NoTrailingComma {
    provider baml-fallback
    options {
        strategy [
            Fast
            Slow
        ]
    }
}
`
	parseBamlFile(cfg, content)

	chain, ok := cfg.fallbackChains["NoTrailingComma"]
	if !ok {
		t.Fatal("expected fallback chain for NoTrailingComma")
	}
	if len(chain) != 2 {
		t.Fatalf("expected 2 children, got %d: %v", len(chain), chain)
	}
	if chain[0] != "Fast" || chain[1] != "Slow" {
		t.Errorf("unexpected chain: %v", chain)
	}
}

func TestParseStrategyList(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"strategy [A, B, C]", []string{"A", "B", "C"}},
		{"strategy [A B C]", []string{"A", "B", "C"}},
		{"strategy [SingleClient]", []string{"SingleClient"}},
		{"strategy []", nil},
		{"strategy", nil},
		{"not a strategy", nil},
		// Inline comments after closing bracket are outside the token range.
		{"strategy [ClientA, ClientB] // fallback pair", []string{"ClientA", "ClientB"}},
		{"strategy [A, B, C] // three-way chain", []string{"A", "B", "C"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseStrategyList(tt.input)
			if len(got) != len(tt.expected) {
				t.Fatalf("expected %v, got %v", tt.expected, got)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("index %d: expected %q, got %q", i, tt.expected[i], got[i])
				}
			}
		})
	}
}

func TestParseClientBlock_MultilineStrategyComments(t *testing.T) {
	// Inline comments in multi-line strategy lists must be stripped per-line
	// before joining. Without this, "ClientA, // primary ClientB," becomes
	// a flat string and ClientB is swallowed by the comment.
	cfg := newTestBamlConfig()
	block := []string{
		"provider baml-fallback",
		"options {",
		"    strategy [",
		"        ClientA, // primary",
		"        ClientB, // secondary",
		"    ]",
		"}",
	}
	parseClientBlock(cfg, "TestFB", block)
	got := cfg.fallbackChains["TestFB"]
	want := []string{"ClientA", "ClientB"}
	if len(got) != len(want) {
		t.Fatalf("expected chain %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: expected %q, got %q", i, want[i], got[i])
		}
	}
}

func TestParseClientBlock_MultilineStrategyOpeningLineComment(t *testing.T) {
	cfg := newTestBamlConfig()
	block := []string{
		"provider baml-fallback",
		"options {",
		"    strategy [ // opening comment",
		"        ClientA,",
		"        ClientB,",
		"    ]",
		"}",
	}
	parseClientBlock(cfg, "OpenLineComment", block)
	got := cfg.fallbackChains["OpenLineComment"]
	want := []string{"ClientA", "ClientB"}
	if len(got) != len(want) {
		t.Fatalf("expected chain %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: expected %q, got %q", i, want[i], got[i])
		}
	}
}

func TestParseClientBlock_OptionsOpeningLineStrategyComment(t *testing.T) {
	cfg := newTestBamlConfig()
	block := []string{
		"provider baml-fallback",
		"options { strategy [ // opening comment",
		"    ClientA,",
		"    ClientB,",
		"] }",
	}
	parseClientBlock(cfg, "InlineOptionsOpenLine", block)
	got := cfg.fallbackChains["InlineOptionsOpenLine"]
	want := []string{"ClientA", "ClientB"}
	if len(got) != len(want) {
		t.Fatalf("expected chain %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: expected %q, got %q", i, want[i], got[i])
		}
	}
}

func TestParseClientBlock_InlineOptionsStrategyAfterModel(t *testing.T) {
	cfg := newTestBamlConfig()
	block := []string{
		"provider baml-fallback",
		"options { model \"x\" strategy [Fast, Slow] }",
	}
	parseClientBlock(cfg, "InlineAfterModel", block)
	got := cfg.fallbackChains["InlineAfterModel"]
	want := []string{"Fast", "Slow"}
	if len(got) != len(want) {
		t.Fatalf("expected chain %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: expected %q, got %q", i, want[i], got[i])
		}
	}
}

func TestParseClientBlock_MultilineStrategyContinuationCommentWithClosingBracket(t *testing.T) {
	cfg := newTestBamlConfig()
	block := []string{
		"provider baml-fallback",
		"options {",
		"    strategy [",
		"        ClientA, // ] not done yet",
		"        ClientB,",
		"    ]",
		"}",
	}
	parseClientBlock(cfg, "ContinuationCommentBracket", block)
	got := cfg.fallbackChains["ContinuationCommentBracket"]
	want := []string{"ClientA", "ClientB"}
	if len(got) != len(want) {
		t.Fatalf("expected chain %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: expected %q, got %q", i, want[i], got[i])
		}
	}
}

func TestParseClientBlock_OptionsOpeningLineStrategyCommentWithClosingBracket(t *testing.T) {
	cfg := newTestBamlConfig()
	block := []string{
		"provider baml-fallback",
		"options { strategy [ // ] opening comment",
		"    ClientA,",
		"    ClientB,",
		"] }",
	}
	parseClientBlock(cfg, "InlineOptionsOpenLineBracket", block)
	got := cfg.fallbackChains["InlineOptionsOpenLineBracket"]
	want := []string{"ClientA", "ClientB"}
	if len(got) != len(want) {
		t.Fatalf("expected chain %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: expected %q, got %q", i, want[i], got[i])
		}
	}
}

func TestParseClientBlock_RoundRobinStart_MultilineOptions(t *testing.T) {
	cfg := newTestBamlConfig()
	block := []string{
		"provider baml-roundrobin",
		"options {",
		"    strategy [ClientA, ClientB]",
		"    start 1",
		"}",
	}
	parseClientBlock(cfg, "RR", block)
	if got, ok := cfg.roundRobinStart["RR"]; !ok || got != 1 {
		t.Fatalf("roundRobinStart[RR]: got (%d, %v), want (1, true)", got, ok)
	}
}

func TestParseClientBlock_RoundRobinStart_InlineOptions(t *testing.T) {
	cfg := newTestBamlConfig()
	block := []string{
		"provider baml-roundrobin",
		"options { strategy [A, B] start 2 }",
	}
	parseClientBlock(cfg, "RR", block)
	if got, ok := cfg.roundRobinStart["RR"]; !ok || got != 2 {
		t.Fatalf("roundRobinStart[RR]: got (%d, %v), want (2, true)", got, ok)
	}
}

func TestParseClientBlock_RoundRobinStart_Absent(t *testing.T) {
	// A RR client without `start N` must not leave a stale entry in the
	// map — absence is the signal the coordinator uses to fall back to
	// a random seed. Preseed a stale entry so the test fails if
	// parseClientBlock inherits prior state instead of resetting.
	cfg := newTestBamlConfig()
	cfg.roundRobinStart["RR"] = 99
	block := []string{
		"provider baml-roundrobin",
		"options {",
		"    strategy [A, B]",
		"}",
	}
	parseClientBlock(cfg, "RR", block)
	if v, ok := cfg.roundRobinStart["RR"]; ok {
		t.Fatalf("expected stale start to be cleared for RR, got %d", v)
	}
}

func TestParseClientBlock_RoundRobinStart_InvalidIgnored(t *testing.T) {
	// A malformed start value (non-integer) is silently ignored — codegen
	// must not panic; the coordinator simply picks a random seed. Same
	// preseed-and-cleared shape as the Absent test.
	cfg := newTestBamlConfig()
	cfg.roundRobinStart["RR"] = 99
	block := []string{
		"provider baml-roundrobin",
		"options {",
		"    strategy [A, B]",
		"    start oops",
		"}",
	}
	parseClientBlock(cfg, "RR", block)
	if v, ok := cfg.roundRobinStart["RR"]; ok {
		t.Fatalf("malformed start should clear stale entry, got %d", v)
	}
}

// TestParseClientBlock_RoundRobinStart_TrailingCloseBrace pins
// trailing-close-brace handling: BAML's grammar allows a
// config-map entry to be followed directly by the closing `}` of the
// options block (datamodel.pest:172-180), so a multiline options
// block can legally end with `start 1 }` on the same line.
// Pre-fix, ParseInt saw "1 }" verbatim and silently dropped a valid
// declaration. Post-fix, the trailing close-brace and whitespace are
// trimmed before parsing.
func TestParseClientBlock_RoundRobinStart_TrailingCloseBrace(t *testing.T) {
	t.Run("start on its own line ending with close-brace", func(t *testing.T) {
		cfg := newTestBamlConfig()
		block := []string{
			"provider baml-roundrobin",
			"options {",
			"    strategy [A, B]",
			"    start 1 }",
		}
		parseClientBlock(cfg, "RR", block)
		if got, ok := cfg.roundRobinStart["RR"]; !ok || got != 1 {
			t.Fatalf("roundRobinStart[RR]: got (%d, %v), want (1, true) — `start N }` final-line shape must parse", got, ok)
		}
	})

	t.Run("start on closing-bracket line after multiline strategy", func(t *testing.T) {
		// `] start 1 }` appears on the line that closes a multiline
		// strategy list. Both the strategy and the start must be
		// captured.
		cfg := newTestBamlConfig()
		block := []string{
			"provider baml-roundrobin",
			"options {",
			"    strategy [",
			"        A,",
			"        B,",
			"    ] start 1 }",
		}
		parseClientBlock(cfg, "RR", block)
		if got, ok := cfg.roundRobinStart["RR"]; !ok || got != 1 {
			t.Fatalf("roundRobinStart[RR]: got (%d, %v), want (1, true) — `] start N }` post-list-close suffix must parse", got, ok)
		}
		chain := cfg.fallbackChains["RR"]
		if len(chain) != 2 || chain[0] != "A" || chain[1] != "B" {
			t.Fatalf("fallbackChains[RR]: got %v, want [A B] — multiline strategy must still parse alongside the post-`]` start", chain)
		}
	})
}

// TestParseClientBlock_RoundRobinStart_OutOfI32Ignored pins the i32
// contract: extractRoundRobinStart parses with bit
// width 32 so values that fit in a Go int on a 64-bit platform but not
// in BAML's i32 contract are rejected by the introspector — same
// behaviour as the runtime override path's inspectStartOverride. Pre-
// fix `strconv.Atoi` accepted these on 64-bit hosts and codegen would
// emit them; BAML's `start as usize` cast then wrapped them silently.
func TestParseClientBlock_RoundRobinStart_OutOfI32Ignored(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"one above MaxInt32", "2147483648"},
		{"one below MinInt32", "-2147483649"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Preseed-and-cleared: out-of-i32 must be ignored AND
			// must clear any stale entry from a prior parse.
			cfg := newTestBamlConfig()
			cfg.roundRobinStart["RR"] = 99
			block := []string{
				"provider baml-roundrobin",
				"options {",
				"    strategy [A, B]",
				"    start " + tc.raw,
				"}",
			}
			parseClientBlock(cfg, "RR", block)
			if v, ok := cfg.roundRobinStart["RR"]; ok {
				t.Fatalf("out-of-i32 start %q should clear stale entry, got %d", tc.raw, v)
			}
		})
	}
}

// TestCanonicaliseProvider_FoldsAliases verifies the introspector
// folds BAML's strategy aliases — both round-robin and fallback —
// onto canonical baml-rest spellings. The BAML CLI init template
// uses `provider fallback`, so config
// files written from the docs hit the bare alias path. Without the
// fold the runtime classifier would treat such clients as unsupported
// single-providers.
func TestCanonicaliseProvider_FoldsAliases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"baml-roundrobin", "baml-roundrobin"},
		{"baml-round-robin", "baml-roundrobin"},
		{"round-robin", "baml-roundrobin"},
		{"baml-fallback", "baml-fallback"},
		{"fallback", "baml-fallback"},
		{"openai", "openai"},
		{"anthropic", "anthropic"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := canonicaliseProvider(tc.in); got != tc.want {
				t.Errorf("canonicaliseProvider(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseClientBlock_BareFallbackAliasNormalised verifies that the
// `provider fallback` spelling — the form BAML's CLI init template
// emits — lands in the introspected ClientProvider map as the
// canonical "baml-fallback" string. Cold-review-2 finding 2.
//
// Also asserts the fallback chain is captured under the bare alias:
// the runtime classifier and
// chain resolution both depend on cfg.fallbackChains[name] being
// populated regardless of provider spelling, and a regression that
// silently returned an empty chain for the bare alias would still
// pass the provider-only assertion.
func TestParseClientBlock_BareFallbackAliasNormalised(t *testing.T) {
	cfg := newTestBamlConfig()
	block := []string{
		"provider fallback",
		"options {",
		"    strategy [A, B]",
		"}",
	}
	parseClientBlock(cfg, "MyFallback", block)
	if got := cfg.clientProvider["MyFallback"]; got != "baml-fallback" {
		t.Errorf("clientProvider[MyFallback]: got %q, want baml-fallback", got)
	}
	chain := cfg.fallbackChains["MyFallback"]
	if len(chain) != 2 || chain[0] != "A" || chain[1] != "B" {
		t.Errorf("fallbackChains[MyFallback]: got %v, want [A B] — chain must be captured under the bare alias too", chain)
	}
}

// TestParseClientBlock_BareRoundRobinAliasNormalised is the round-robin
// sibling of the fallback test above.
// `provider round-robin` is the bare alias BAML accepts alongside the
// canonical `baml-roundrobin` form; canonicaliseProvider folds it at the
// raw-token layer, but the parse-block-level test pins the end-to-end
// shape — provider canonicalised AND the strategy chain captured under
// the alias — so a regression that broke either half would surface here
// rather than slipping through on the existing canonicaliser-only test.
func TestParseClientBlock_BareRoundRobinAliasNormalised(t *testing.T) {
	cfg := newTestBamlConfig()
	block := []string{
		"provider round-robin",
		"options {",
		"    strategy [A, B]",
		"}",
	}
	parseClientBlock(cfg, "MyRR", block)
	if got := cfg.clientProvider["MyRR"]; got != "baml-roundrobin" {
		t.Errorf("clientProvider[MyRR]: got %q, want baml-roundrobin", got)
	}
	chain := cfg.fallbackChains["MyRR"]
	if len(chain) != 2 || chain[0] != "A" || chain[1] != "B" {
		t.Errorf("fallbackChains[MyRR]: got %v, want [A B] — chain must be captured under the bare alias too", chain)
	}
}
