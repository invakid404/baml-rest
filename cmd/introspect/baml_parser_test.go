package main

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestBamlConfig() *bamlConfig {
	return &bamlConfig{
		clientProvider:       make(map[string]string),
		clientRetryPolicy:    make(map[string]string),
		functionClient:       make(map[string]string),
		retryPolicies:        make(map[string]parsedRetryPolicy),
		fallbackChains:       make(map[string][]string),
		roundRobinStart:      make(map[string]int),
		bedrockClientOptions: make(map[string]bedrockClientOptions),
	}
}

func TestParseBamlSourceDir(t *testing.T) {
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

// TestParseBamlSourceDir_IntegrationTestData exercises the production
// parser against the real integration .baml fixtures. Renamed from the
// pre-#265 TestParseBamlFile_IntegrationTestData; the test already used
// parseBamlSourceDir, the old name was a vestige.
func TestParseBamlSourceDir_IntegrationTestData(t *testing.T) {
	dir := filepath.Join("..", "..", "integration", "testdata", "baml_src")
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			t.Skip("integration testdata not available")
		}
		t.Fatalf("os.Stat(%q): %v", dir, err)
	}
	cfg := parseBamlSourceDir(dir)

	if cfg.clientProvider["TestClient"] != "openai" {
		t.Errorf("expected TestClient provider=openai, got %q", cfg.clientProvider["TestClient"])
	}

	if cfg.functionClient["GetGreeting"] != "TestClient" {
		t.Errorf("expected GetGreeting client=TestClient, got %q", cfg.functionClient["GetGreeting"])
	}
}

func TestParseBamlSourceDir_NestedDirectories(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "generators.baml"), []byte(`
// generator config (no client/function blocks)
`), 0644); err != nil {
		t.Fatal(err)
	}

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

	if cfg.clientProvider["MyOpenAI"] != "openai" {
		t.Errorf("expected MyOpenAI provider=openai, got %q", cfg.clientProvider["MyOpenAI"])
	}
	if cfg.clientProvider["MyClaude"] != "anthropic" {
		t.Errorf("expected MyClaude provider=anthropic, got %q", cfg.clientProvider["MyClaude"])
	}

	if cfg.functionClient["ExtractData"] != "MyOpenAI" {
		t.Errorf("expected ExtractData client=MyOpenAI, got %q", cfg.functionClient["ExtractData"])
	}

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

// TestParseShorthandClient covers the BAML shorthand spec parser used
// by the introspector to recognize `<provider>/<model>` function-default
// clients. The provider allowlist must match upstream BAML v0.219.0's
// ClientProvider::from_str (clientspec.rs:119-144) — additions or
// removals in a future BAML release require lockstep updates here.
//
// The provider component is run through canonicaliseProvider, so the
// strategy aliases collapse onto the baml-rest canonical spellings.
func TestParseShorthandClient(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		wantProvider string
		wantModel    string
		wantOK       bool
	}{
		{"openai chat-style model", "openai/gpt-4o", "openai", "gpt-4o", true},
		{"anthropic versioned model", "anthropic/claude-3-5-sonnet-20241022", "anthropic", "claude-3-5-sonnet-20241022", true},
		{"openai-generic", "openai-generic/some-model", "openai-generic", "some-model", true},
		{"openai-responses", "openai-responses/gpt-4o", "openai-responses", "gpt-4o", true},
		{"azure-openai", "azure-openai/my-deployment", "azure-openai", "my-deployment", true},
		{"ollama", "ollama/llama3", "ollama", "llama3", true},
		{"openrouter", "openrouter/anthropic/claude-3", "openrouter", "anthropic/claude-3", true},
		{"google-ai", "google-ai/gemini-1.5", "google-ai", "gemini-1.5", true},
		{"vertex-ai", "vertex-ai/gemini-1.5", "vertex-ai", "gemini-1.5", true},
		{"aws-bedrock", "aws-bedrock/claude-3", "aws-bedrock", "claude-3", true},
		{"baml-openai-chat legacy alias", "baml-openai-chat/gpt-4", "baml-openai-chat", "gpt-4", true},
		{"baml-anthropic-chat legacy alias", "baml-anthropic-chat/claude-3", "baml-anthropic-chat", "claude-3", true},
		// Legacy alias: `baml-azure-chat` is accepted by upstream
		// BAML's ClientProvider::from_str (clientspec.rs:125, v0.219.0)
		// and passes through canonicaliseProvider unchanged — only the
		// strategy aliases (round-robin / fallback) get folded.
		{"baml-azure-chat legacy alias", "baml-azure-chat/my-deployment", "baml-azure-chat", "my-deployment", true},
		// Legacy alias: `baml-ollama-chat` is accepted by upstream
		// BAML's ClientProvider::from_str (clientspec.rs:129, v0.219.0)
		// and passes through canonicaliseProvider unchanged.
		{"baml-ollama-chat legacy alias", "baml-ollama-chat/llama3", "baml-ollama-chat", "llama3", true},
		{"fallback strategy shorthand canonicalised", "fallback/foo", "baml-fallback", "foo", true},
		// Strategy alias: `baml-fallback` is the already-canonical
		// spelling — upstream accepts it (clientspec.rs:140) and
		// canonicaliseProvider returns it as-is. Unlike round-robin,
		// the canonical-output and upstream-input forms agree here,
		// so this row complements the "fallback" row above without
		// the canonical-vs-input asymmetry that the no-hyphen
		// `baml-roundrobin/foo` negative row pins.
		{"baml-fallback strategy shorthand canonical", "baml-fallback/foo", "baml-fallback", "foo", true},
		{"round-robin strategy shorthand canonicalised", "round-robin/foo", "baml-roundrobin", "foo", true},
		// Strategy alias: `baml-round-robin` (hyphenated) IS accepted
		// by upstream BAML's ClientProvider::from_str
		// (clientspec.rs:142) and canonicaliseProvider folds it to
		// `baml-roundrobin` (no hyphen — baml-rest's emit-side
		// canonical). Contrast with the negative `baml-roundrobin/foo`
		// row below, where the no-hyphen form is explicitly rejected
		// because upstream does not accept it as shorthand input.
		{"baml-round-robin strategy shorthand canonicalised", "baml-round-robin/foo", "baml-roundrobin", "foo", true},
		// Negative: baml-rest's INTERNAL canonical spelling for the
		// round-robin strategy is `baml-roundrobin` (no hyphen between
		// "round" and "robin"). Upstream BAML's ClientProvider::from_str
		// (clientspec.rs:119-143, v0.219.0) only accepts the hyphenated
		// forms `round-robin` and `baml-round-robin` as shorthand input;
		// the no-hyphen form is baml-rest's emit-side canonical
		// (X-BAML-RoundRobin-* headers, downstream classification
		// switches) and was never an upstream-recognized provider input.
		//
		// Adding `baml-roundrobin` to isKnownShorthandProvider would
		// make the introspector recognize a shorthand that upstream
		// BAML would reject at request time — silent introspect-vs-
		// runtime divergence, the exact shape the upstream-parity
		// allowlist is designed to prevent. This row pins that
		// asymmetry so any future widening attempt (e.g. a code-review
		// suggestion to "round out the canonical aliases") fails
		// loudly with the explanation above.
		//
		// The fallback strategy is intentionally symmetric: upstream
		// accepts both `fallback` and `baml-fallback`, and
		// canonicaliseProvider folds them onto `baml-fallback` — so the
		// canonical-output spelling happens to also be a valid
		// upstream input. Only round-robin has the canonical-vs-input
		// asymmetry; not coincidentally, only round-robin's canonical
		// drops a hyphen that the upstream form keeps.
		{"no-hyphen baml-roundrobin is canonical-only, not shorthand input", "baml-roundrobin/foo", "", "", false},
		{"unknown provider rejected", "unknown/model", "", "", false},
		{"empty string rejected", "", "", "", false},
		{"no slash rejected", "openai", "", "", false},
		{"empty provider rejected", "/gpt-4o", "", "", false},
		{"empty model rejected", "openai/", "", "", false},
		{"named client with slash-looking name not recognised", "MyClient", "", "", false},
		{"case-sensitive provider", "OpenAI/gpt-4", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotProvider, gotModel, gotOK := parseShorthandClient(tc.in)
			if gotProvider != tc.wantProvider || gotModel != tc.wantModel || gotOK != tc.wantOK {
				t.Errorf("parseShorthandClient(%q) = (%q, %q, %t), want (%q, %q, %t)",
					tc.in, gotProvider, gotModel, gotOK, tc.wantProvider, tc.wantModel, tc.wantOK)
			}
		})
	}
}

// TestEnrichShorthandClientProviders_FunctionDefault verifies that a
// function defaulting to a shorthand client lands in cfg.clientProvider
// with the parsed provider after the enrichment pass. Without this
// entry the FunctionProvider materialization in generateBamlConfigVars
// silently drops the function and the BuildRequest gate falls to
// legacy with PathReasonEmptyProvider for every call.
func TestEnrichShorthandClientProviders_FunctionDefault(t *testing.T) {
	cfg := newTestBamlConfig()
	cfg.functionClient["MyFunc"] = "openai/gpt-4o"

	enrichShorthandClientProviders(cfg)

	if got := cfg.clientProvider["openai/gpt-4o"]; got != "openai" {
		t.Errorf("clientProvider[openai/gpt-4o] = %q, want %q", got, "openai")
	}
}

// TestEnrichShorthandClientProviders_FallbackChild covers strategy-
// wrapper children referencing a shorthand spec. Fallback-chain
// resolution looks up cfg.clientProvider[child] for each entry; a
// shorthand child without enrichment would classify as empty-provider
// inside the chain.
func TestEnrichShorthandClientProviders_FallbackChild(t *testing.T) {
	cfg := newTestBamlConfig()
	cfg.clientProvider["MyFallback"] = "baml-fallback"
	cfg.fallbackChains["MyFallback"] = []string{"openai/gpt-4o", "anthropic/claude-3-5-sonnet"}

	enrichShorthandClientProviders(cfg)

	if got := cfg.clientProvider["openai/gpt-4o"]; got != "openai" {
		t.Errorf("clientProvider[openai/gpt-4o] = %q, want %q", got, "openai")
	}
	if got := cfg.clientProvider["anthropic/claude-3-5-sonnet"]; got != "anthropic" {
		t.Errorf("clientProvider[anthropic/claude-3-5-sonnet] = %q, want %q", got, "anthropic")
	}
}

// TestEnrichShorthandClientProviders_DoesNotOverrideExisting pins that
// a named client whose own provider declaration would happen to share
// the shorthand syntax (theoretically possible — a `client<llm>` block
// can be named anything) keeps its declared provider. The enrichment
// pass only fills holes; it never overwrites a parsed provider entry.
func TestEnrichShorthandClientProviders_DoesNotOverrideExisting(t *testing.T) {
	cfg := newTestBamlConfig()
	cfg.clientProvider["openai/gpt-4o"] = "anthropic"
	cfg.functionClient["MyFunc"] = "openai/gpt-4o"

	enrichShorthandClientProviders(cfg)

	if got := cfg.clientProvider["openai/gpt-4o"]; got != "anthropic" {
		t.Errorf("clientProvider[openai/gpt-4o] = %q, want preserved %q", got, "anthropic")
	}
}

// TestEnrichShorthandClientProviders_UnknownProviderIgnored verifies
// that a function defaulting to a `provider/model` literal with an
// unrecognised provider does not pollute cfg.clientProvider. Upstream
// would also reject this at ClientSpec::new_from_id parse time; the
// introspector must agree so downstream behaviour stays unchanged
// (falls to legacy as PathReasonEmptyProvider, just like before).
func TestEnrichShorthandClientProviders_UnknownProviderIgnored(t *testing.T) {
	cfg := newTestBamlConfig()
	cfg.functionClient["MyFunc"] = "made-up-provider/some-model"

	enrichShorthandClientProviders(cfg)

	if _, exists := cfg.clientProvider["made-up-provider/some-model"]; exists {
		t.Error("clientProvider should not contain entry for unknown shorthand provider")
	}
}

// TestParseBamlSourceDir_ShorthandFunctionClient exercises the full
// parse pipeline: a .baml fixture with a function defaulting to a
// shorthand client should produce a populated ClientProvider entry for
// that shorthand string, so the FunctionProvider materialization in
// generateBamlConfigVars sees a non-empty provider and the BuildRequest
// dispatch gate engages instead of falling through to legacy.
func TestParseBamlSourceDir_ShorthandFunctionClient(t *testing.T) {
	dir := t.TempDir()
	content := `
function Extract(text: string) -> string {
    client "openai/gpt-4o"
    prompt #"Extract: {{ text }}"#
}

function Summarize(text: string) -> string {
    client "anthropic/claude-3-5-sonnet-20241022"
    prompt #"Summarize: {{ text }}"#
}
`
	if err := os.WriteFile(filepath.Join(dir, "functions.baml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := parseBamlSourceDir(dir)

	if got := cfg.functionClient["Extract"]; got != "openai/gpt-4o" {
		t.Errorf("functionClient[Extract] = %q, want %q", got, "openai/gpt-4o")
	}
	if got := cfg.clientProvider["openai/gpt-4o"]; got != "openai" {
		t.Errorf("clientProvider[openai/gpt-4o] = %q, want %q — shorthand-defaulted function would fall to legacy with empty provider", got, "openai")
	}
	if got := cfg.clientProvider["anthropic/claude-3-5-sonnet-20241022"]; got != "anthropic" {
		t.Errorf("clientProvider[anthropic/claude-3-5-sonnet-20241022] = %q, want %q", got, "anthropic")
	}
}

// TestBedrockOptionValue_Resolve_RuntimeBranches exercises the
// generated Resolve() method indirectly by calling its Go equivalent
// here on the parser-side type — the generator emits the same switch
// shape verbatim. Pinning both branches catches regressions in either
// the parser-side struct fields or the generated method's switch
// arms.
func TestBedrockOptionValue_Resolve_RuntimeBranches(t *testing.T) {
	t.Run("literal returns value with ok=true", func(t *testing.T) {
		v := bedrockOptionValue{Literal: "x", Provenance: "literal"}
		got, ok := resolveBedrockOptionValueForTest(v)
		if got != "x" || !ok {
			t.Errorf("literal resolve = (%q, %v), want (\"x\", true)", got, ok)
		}
	})
	t.Run("env returns os.LookupEnv result", func(t *testing.T) {
		t.Setenv("PARSER_RESOLVE_TEST", "from-env")
		v := bedrockOptionValue{EnvVar: "PARSER_RESOLVE_TEST", Provenance: "env"}
		got, ok := resolveBedrockOptionValueForTest(v)
		if got != "from-env" || !ok {
			t.Errorf("env resolve = (%q, %v), want (\"from-env\", true)", got, ok)
		}
	})
	t.Run("env unset returns empty/false", func(t *testing.T) {
		v := bedrockOptionValue{EnvVar: "DEFINITELY_NOT_SET_PARSER_TEST", Provenance: "env"}
		got, ok := resolveBedrockOptionValueForTest(v)
		if got != "" || ok {
			t.Errorf("unset env resolve = (%q, %v), want (\"\", false)", got, ok)
		}
	})
	t.Run("unset provenance returns empty/false", func(t *testing.T) {
		v := bedrockOptionValue{}
		got, ok := resolveBedrockOptionValueForTest(v)
		if got != "" || ok {
			t.Errorf("unset provenance resolve = (%q, %v), want (\"\", false)", got, ok)
		}
	})
}

// resolveBedrockOptionValueForTest mirrors the runtime Resolve() method
// shape that emitBedrockOptionTypes generates. Lives in the test file
// so the parser-side type retains a unit-testable resolver without the
// generator having to expose its shape via a callable hook.
func resolveBedrockOptionValueForTest(v bedrockOptionValue) (string, bool) {
	switch v.Provenance {
	case "literal":
		return v.Literal, true
	case "env":
		return os.LookupEnv(v.EnvVar)
	}
	return "", false
}

// isSetBedrockOptionValueForTest mirrors the runtime IsSet() method
// shape emitBedrockOptionTypes generates: presence is decided by
// Provenance alone, NOT by Resolve()'s second return. This pins the
// declared-vs-resolved separation that the codegen relies on for
// declared-but-env-unset credentials to enter the resolver's static
// branch (and error there) rather than silently fall through to the
// default AWS credential chain.
func isSetBedrockOptionValueForTest(v bedrockOptionValue) bool {
	return v.Provenance != ""
}

// TestBedrockOptionValue_IsSet pins the declared-vs-resolved boundary.
// A declared env.X reference whose env var is unset at runtime must
// still report IsSet()=true so the codegen-emitted Present flag enters
// the resolver's static-branch validation. If a future refactor were
// to fold IsSet into Resolve()'s ok, this test fails immediately.
func TestBedrockOptionValue_IsSet(t *testing.T) {
	cases := []struct {
		name string
		v    bedrockOptionValue
		want bool
	}{
		{
			name: "literal is declared",
			v:    bedrockOptionValue{Literal: "x", Provenance: "literal"},
			want: true,
		},
		{
			name: "env ref is declared even when env var is unset",
			v:    bedrockOptionValue{EnvVar: "NEVER_SET_DEFINITELY", Provenance: "env"},
			want: true,
		},
		{
			name: "empty literal is still declared (provenance != \"\")",
			v:    bedrockOptionValue{Provenance: "literal"},
			want: true,
		},
		{
			name: "zero value is not declared",
			v:    bedrockOptionValue{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSetBedrockOptionValueForTest(tc.v); got != tc.want {
				t.Errorf("IsSet(%+v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// TestBedrockOptionValue_IsSetVsResolve_EnvUnset is the load-bearing
// pin for the #263 sign-off fix. A declared env.X reference whose
// env var is unset at runtime must report:
//   - IsSet() == true   (the field was declared in .baml)
//   - Resolve() == ("", false)  (the value did not resolve)
//
// Codegen drives Present from IsSet(), so the resolver's static branch
// is entered (Present=true, Value=""); the existing "resolved to empty"
// error then fires. Driving Present from Resolve()'s ok would collapse
// declared-but-env-unset into never-declared and silently fall through
// to the default AWS credential chain — a security-significant
// regression. This test pins both halves of that distinction.
func TestBedrockOptionValue_IsSetVsResolve_EnvUnset(t *testing.T) {
	v := bedrockOptionValue{EnvVar: "BAML_REST_TEST_NEVER_SET_VAR", Provenance: "env"}

	if !isSetBedrockOptionValueForTest(v) {
		t.Error("IsSet() must be true for a declared env.X reference, regardless of env-var resolution")
	}
	value, ok := resolveBedrockOptionValueForTest(v)
	if ok {
		t.Errorf("Resolve() ok = true for unset env.X ref; want false (got value=%q)", value)
	}
	if value != "" {
		t.Errorf("Resolve() value = %q for unset env.X ref; want empty", value)
	}
}
