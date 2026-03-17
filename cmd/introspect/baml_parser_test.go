package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBamlFile_ClientAndFunction(t *testing.T) {
	cfg := &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
	}

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
	cfg := &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
	}

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
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skip("integration testdata not available")
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
	cfg := &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
	}

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
	cfg := &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
	}

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
	cfg := &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
	}

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
	cfg := &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
	}

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
	cfg := &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
	}

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
	cfg := &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
	}

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
	cfg := &bamlConfig{
		clientProvider:    make(map[string]string),
		clientRetryPolicy: make(map[string]string),
		functionClient:    make(map[string]string),
		retryPolicies:     make(map[string]parsedRetryPolicy),
	}

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
	}
	for _, tt := range tests {
		if got := stripInlineComment(tt.input); got != tt.expected {
			t.Errorf("stripInlineComment(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
