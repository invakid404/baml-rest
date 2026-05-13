// Package introspect parity tests.
//
// This file is the load-bearing safety mechanism for the BAML parser
// unification refactor: it feeds every fixture the existing line-based
// parser handles (plus a hand-built edge-case table) through BOTH the
// existing parser and the new shared bamlutils/bamlparser-based AST walker,
// then asserts the two produce byte-identical *bamlConfig values.
//
// The AST walker (`parityProcessFile` and friends below) is intentionally
// scoped to this test file — production code still calls parseBamlFile in
// main.go. The walker exists here so the parity assertion is meaningful
// today; a subsequent PR promotes it into production after this parity
// surface is green.
//
// The knownParityDivergences list is expected to remain EMPTY: any
// divergence surfaced by this test is either a correctness issue in the
// new parser/walker or a quirk of the old parser the new code is widening
// — both cases require explicit attention rather than silent acceptance.

package main

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
)

// knownParityDivergences enumerates fixtures whose old-parser and
// new-parser output are not expected to match. Each entry is a fixture
// name from the parity tables below. The list must remain empty: PR 1
// asserts the new parser is byte-for-byte equivalent on the documented
// surface, so any divergence is a regression to escalate.
var knownParityDivergences = map[string]string{}

// parityCase is a single fixture exercised against both parsers.
type parityCase struct {
	name string
	src  string
}

// runOldParser builds a fresh *bamlConfig and runs the in-tree line-based
// parser plus enrichShorthandClientProviders, matching parseBamlSourceDir's
// production pipeline (except for the FS walk, which is irrelevant for
// content-only fixtures).
func runOldParser(src string) *bamlConfig {
	cfg := newTestBamlConfig()
	parseBamlFile(cfg, src)
	enrichShorthandClientProviders(cfg)
	return cfg
}

// runNewParser builds a fresh *bamlConfig and runs the new AST-based
// walker (parityProcessFile) + enrichShorthandClientProviders. Output is
// structurally compatible with runOldParser.
func runNewParser(t *testing.T, src string) *bamlConfig {
	t.Helper()
	f, err := bamlparser.ParseString("parity.baml", src)
	if err != nil {
		t.Fatalf("new parser failed: %v", err)
	}
	cfg := newTestBamlConfig()
	parityProcessFile(cfg, f)
	enrichShorthandClientProviders(cfg)
	return cfg
}

// parityProcessFile dispatches over the new parser's top-level items in
// source order. Only client<llm>, function (with parenthesised signature),
// and retry_policy blocks contribute to *bamlConfig — every other top-level
// shape is intentionally ignored, mirroring parseBamlFile in main.go.
func parityProcessFile(cfg *bamlConfig, f *bamlparser.File) {
	for _, it := range f.Items {
		switch {
		case it.Client != nil && it.Client.TypeParam == "llm" && it.Client.Name != "":
			parityProcessClient(cfg, it.Client)
		case it.Function != nil && it.Function.Name != "":
			parityProcessFunction(cfg, it.Function)
		case it.RetryPolicy != nil && it.RetryPolicy.Name != "":
			parityProcessRetryPolicy(cfg, it.RetryPolicy)
		}
	}
}

// parityProcessClient walks a parsed client block, mirroring
// parseClientBlock's behavior: stale-state cleanup, ordered field
// processing, options-block dive for strategy / start / Bedrock keys.
func parityProcessClient(cfg *bamlConfig, c *bamlparser.ClientBlock) {
	name := c.Name
	delete(cfg.roundRobinStart, name)
	delete(cfg.fallbackChains, name)
	delete(cfg.clientProvider, name)
	delete(cfg.clientRetryPolicy, name)
	delete(cfg.bedrockClientOptions, name)

	for _, f := range c.Fields {
		switch f.Key {
		case "provider":
			if f.Value != nil {
				cfg.clientProvider[name] = canonicaliseProvider(asScalarString(f.Value))
			}
		case "retry_policy":
			if f.Value != nil {
				cfg.clientRetryPolicy[name] = asScalarString(f.Value)
			}
		case "options":
			if f.Block != nil {
				parityProcessOptions(cfg, name, f.Block)
			}
		}
	}
}

// parityProcessOptions walks an options block. Strategy is captured at any
// depth (matching the old parser's depth-agnostic strategy extraction);
// start and Bedrock keys are only honoured at the top level of the
// options block (depth == 1), matching extractRoundRobinStart's and
// updateBedrockClientOption's guard.
func parityProcessOptions(cfg *bamlConfig, name string, opts *bamlparser.Block) {
	for _, f := range opts.Fields {
		switch f.Key {
		case "strategy":
			if f.Value != nil && f.Value.List != nil {
				chain := parityStrategyList(f.Value)
				if len(chain) > 0 {
					cfg.fallbackChains[name] = chain
				}
			}
		case "start":
			if f.Value != nil && f.Value.Number != nil {
				if n, err := strconv.ParseInt(*f.Value.Number, 10, 32); err == nil {
					cfg.roundRobinStart[name] = int(n)
				}
			}
		case "endpoint_url", "region",
			"access_key_id", "secret_access_key", "session_token", "profile":
			if val, ok := parityBedrockOptionValue(f.Value); ok {
				cur := cfg.bedrockClientOptions[name]
				switch f.Key {
				case "endpoint_url":
					cur.EndpointURL = val
				case "region":
					cur.Region = val
				case "access_key_id":
					cur.Credentials.AccessKeyID = val
				case "secret_access_key":
					cur.Credentials.SecretAccessKey = val
				case "session_token":
					cur.Credentials.SessionToken = val
				case "profile":
					cur.Credentials.Profile = val
				}
				cfg.bedrockClientOptions[name] = cur
			}
		}
		// Strategy at any nested depth: recurse into block-shaped option
		// fields and capture a strategy list if present. The old parser's
		// in-options strategy extraction is not depth-gated (see Codex's
		// scoping doc Q1) so the AST walker mirrors that posture.
		if f.Block != nil {
			parityProcessOptionsStrategyRecursive(cfg, name, f.Block)
		}
	}
}

func parityProcessOptionsStrategyRecursive(cfg *bamlConfig, name string, blk *bamlparser.Block) {
	for _, f := range blk.Fields {
		if f.Key == "strategy" && f.Value != nil && f.Value.List != nil {
			chain := parityStrategyList(f.Value)
			if len(chain) > 0 {
				cfg.fallbackChains[name] = chain
			}
		}
		if f.Block != nil {
			parityProcessOptionsStrategyRecursive(cfg, name, f.Block)
		}
	}
}

// parityStrategyList converts a List Value to a slice of client names,
// matching parseStrategyList's whitespace/comma splitting and quote
// stripping.
func parityStrategyList(v *bamlparser.Value) []string {
	if v == nil || v.List == nil {
		return nil
	}
	var out []string
	for _, e := range v.List {
		s, ok := e.String()
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parityBedrockOptionValue mirrors extractBedrockOptionValue: a bare
// env.IDENT becomes Provenance=env (EnvVar=IDENT); anything else with a
// non-empty string representation becomes Provenance=literal (Literal=that
// string). An empty literal yields ok=false so the field is not recorded.
func parityBedrockOptionValue(v *bamlparser.Value) (bedrockOptionValue, bool) {
	if v == nil {
		return bedrockOptionValue{}, false
	}
	if name, ok := v.EnvName(); ok {
		if name == "" {
			return bedrockOptionValue{}, false
		}
		return bedrockOptionValue{EnvVar: name, Provenance: "env"}, true
	}
	s, ok := v.String()
	if !ok || s == "" {
		return bedrockOptionValue{}, false
	}
	return bedrockOptionValue{Literal: s, Provenance: "literal"}, true
}

// parityProcessFunction walks a function block, capturing the top-level
// `client VALUE` field. Matches parseFunctionBlock — every other field is
// ignored (prompt body is a Raw value or a nested block, options are
// ignored).
//
// Last-wins on duplicates: the production loop at
// cmd/introspect/main.go:2143-2174 keeps scanning every top-level line in
// the function body, so a later `client` field overwrites an earlier one.
// The walker iterates all matching fields rather than stopping at the
// first, mirroring that semantics exactly.
func parityProcessFunction(cfg *bamlConfig, fn *bamlparser.FunctionBlock) {
	for _, f := range fn.Fields {
		if f.Key != "client" || f.Value == nil {
			continue
		}
		cfg.functionClient[fn.Name] = asScalarString(f.Value)
	}
}

// parityProcessRetryPolicy walks a retry_policy block. Direct top-level
// fields (max_retries, type, delay_ms, multiplier, max_delay_ms) are
// applied first; nested `strategy { ... }` fields override them, matching
// the old expandStrategyBlock + allLines append precedence.
func parityProcessRetryPolicy(cfg *bamlConfig, r *bamlparser.RetryPolicyBlock) {
	p := parsedRetryPolicy{}
	apply := func(key string, v *bamlparser.Value) {
		switch key {
		case "max_retries":
			if n, ok := atoiValue(v); ok {
				p.maxRetries = n
			}
		case "type":
			p.strategy = asScalarString(v)
		case "delay_ms":
			if n, ok := atoiValue(v); ok {
				p.delayMs = n
			}
		case "multiplier":
			if fl, ok := parseFloatValue(v); ok {
				p.multiplier = fl
			}
		case "max_delay_ms":
			if n, ok := atoiValue(v); ok {
				p.maxDelayMs = n
			}
		}
	}
	// Pass 1: direct top-level fields (excluding nested blocks).
	for _, f := range r.Fields {
		if f.Block != nil {
			continue
		}
		apply(f.Key, f.Value)
	}
	// Pass 2: nested strategy block — overrides direct values, mirroring
	// the old parser's allLines = expanded + strategyLines precedence.
	for _, f := range r.Fields {
		if f.Key != "strategy" || f.Block == nil {
			continue
		}
		for _, sf := range f.Block.Fields {
			apply(sf.Key, sf.Value)
		}
	}
	cfg.retryPolicies[r.Name] = p
}

// asScalarString reconstructs the string the old parser would have seen
// after cleanBamlValue: quotes stripped from a Literal, env.X reassembled
// for an EnvRef so semantic consumers see the same "env.X" lexeme the
// line-based parser produced.
func asScalarString(v *bamlparser.Value) string {
	if v == nil {
		return ""
	}
	switch {
	case v.IsLiteral():
		s, _ := v.LiteralValue()
		return s
	case v.IsIdent():
		s, _ := v.IdentValue()
		return s
	case v.IsNumber():
		s, _ := v.NumberValue()
		return s
	case v.IsRaw():
		s, _ := v.RawValue()
		return s
	case v.IsEnvRef():
		name, _ := v.EnvName()
		return "env." + name
	}
	return ""
}

func atoiValue(v *bamlparser.Value) (int, bool) {
	s := asScalarString(v)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func parseFloatValue(v *bamlparser.Value) (float64, bool) {
	s := asScalarString(v)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// ----------------------------------------------------------------------------
// Parity fixtures: every fixture from baml_parser_test.go shaped into a
// content-only string, plus hand-built edge cases enumerated during
// scoping (Codex doc Q1). Each case feeds both parsers; outputs must be
// byte-identical post-normalisation.
// ----------------------------------------------------------------------------

func parityCases() []parityCase {
	return []parityCase{
		// ---- Mirrors of baml_parser_test.go fixtures ----
		{
			name: "ClientAndFunction",
			src: `
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
`,
		},
		{
			name: "RetryPolicyConstantAndExponential",
			src: `
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
`,
		},
		{
			name: "QuotedValues",
			src: `
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
`,
		},
		{
			name: "PromptTextIgnored",
			src: `
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
`,
		},
		{
			name: "SingleLineBlocks",
			src: `
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
`,
		},
		{
			name: "MultiEntryCompactBlocks",
			src: `
client<llm> Foo { provider openai retry_policy Fast }

retry_policy Fast { max_retries 2 type constant_delay delay_ms 100 }
`,
		},
		{
			name: "InlineComments",
			src: `
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
`,
		},
		{
			name: "KeysAfterNestedSections",
			src: `
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
`,
		},
		{
			name: "MultilineRawPromptBraces",
			src: `
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
`,
		},
		{
			name: "NestedSingleLineBlock",
			src: `
client<llm> MyClient {
    options { provider "should-be-ignored" }
    provider openai
}
`,
		},
		{
			name: "KeywordsInQuotedValues",
			src: `
client<llm> QuotedClient { provider "my client provider" }
`,
		},
		{
			name: "NestedStrategyBlock",
			src: `
retry_policy MyRetry {
    max_retries 3
    strategy {
        type constant_delay
        delay_ms 200
    }
}
`,
		},
		{
			name: "MaxDelayMsCompactBlock",
			src: `
retry_policy ExpRetry { max_retries 3 max_delay_ms 5000 delay_ms 100 }
`,
		},
		{
			name: "CompactBlockWithInlineComment",
			src: `
client<llm> CommentClient { provider openai // retry_policy Fast }
`,
		},
		{
			name: "URLInQuotedValueCompactBlock",
			src: `
client<llm> URLClient { provider "https://api.example.com" retry_policy Fast }

retry_policy Fast {
    max_retries 2
}
`,
		},
		{
			name: "CompactRetryPolicyWithStrategy",
			src: `
retry_policy Fast { max_retries 3 strategy { type constant_delay delay_ms 100 } }
`,
		},
		{
			name: "CompactRetryPolicyExponential",
			src: `
retry_policy ExpRetry { max_retries 5 strategy { type exponential_backoff delay_ms 200 multiplier 2.0 max_delay_ms 10000 } }
`,
		},
		{
			name: "FallbackChainMultiLine",
			src: `
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
`,
		},
		{
			name: "FallbackChainInline",
			src: `
client<llm> InlineFallback {
    provider baml-fallback
    options { strategy [Fast, Slow] }
}
`,
		},
		{
			name: "FallbackChainInlineStrategyAfterModel",
			src: `
client<llm> InlineFallback {
    provider baml-fallback
    options { model "x" strategy [Fast, Slow] }
}
`,
		},
		{
			name: "NonFallbackClientNoChain",
			src: `
client<llm> RegularClient {
    provider openai
    options {
        model "gpt-4"
    }
}
`,
		},
		{
			name: "FallbackChainStrategyOnClosingBraceLine",
			src: `
client<llm> ClosingBrace {
    provider baml-fallback
    options {
        strategy [ClientA, ClientB] }
}
`,
		},
		{
			name: "FallbackChainMultiLineStrategyTrailingCommas",
			src: `
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
`,
		},
		{
			name: "FallbackChainMultiLineNoTrailingComma",
			src: `
client<llm> NoTrailingComma {
    provider baml-fallback
    options {
        strategy [
            Fast
            Slow
        ]
    }
}
`,
		},
		{
			name: "BareFallbackAliasNormalised",
			src: `
client<llm> MyFallback {
    provider fallback
    options {
        strategy [A, B]
    }
}
`,
		},
		{
			name: "BareRoundRobinAliasNormalised",
			src: `
client<llm> MyRR {
    provider round-robin
    options {
        strategy [A, B]
    }
}
`,
		},
		{
			name: "RoundRobinStartMultilineOptions",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [ClientA, ClientB]
        start 1
    }
}
`,
		},
		{
			name: "RoundRobinStartInlineOptions",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options { strategy [A, B] start 2 }
}
`,
		},
		{
			name: "RoundRobinStartAbsentNoEntry",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
    }
}
`,
		},
		{
			name: "RoundRobinStartInvalidIgnored",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
        start oops
    }
}
`,
		},
		{
			name: "RoundRobinStartTrailingCloseBrace",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
        start 1 }
}
`,
		},
		{
			name: "RoundRobinStartQuotedRejected",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
        start "1"
    }
}
`,
		},
		{
			name: "RoundRobinStartOutOfI32",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
        start 2147483648
    }
}
`,
		},
		{
			name: "ShorthandFunctionClient",
			src: `
function Extract(text: string) -> string {
    client "openai/gpt-4o"
    prompt #"Extract: {{ text }}"#
}

function Summarize(text: string) -> string {
    client "anthropic/claude-3-5-sonnet-20241022"
    prompt #"Summarize: {{ text }}"#
}
`,
		},
		{
			name: "BedrockEndpointURLLiteral",
			src: `
client<llm> CustomBedrock {
    provider aws-bedrock
    options {
        endpoint_url "http://localhost:9000"
        region "us-east-1"
    }
}
`,
		},
		{
			name: "BedrockEndpointURLEnvRef",
			src: `
client<llm> EnvBedrock {
    provider aws-bedrock
    options {
        endpoint_url env.BEDROCK_ENDPOINT
        region env.AWS_REGION_OVERRIDE
    }
}
`,
		},
		{
			name: "BedrockEndpointURLAbsent",
			src: `
client<llm> PlainBedrock {
    provider aws-bedrock
    options {
        model "anthropic.claude-3-sonnet-20240229-v1:0"
    }
}
`,
		},
		{
			name: "BedrockEndpointURLNonBedrockClient",
			src: `
client<llm> NotBedrock {
    provider openai
    options {
        endpoint_url "https://example.com/v1"
        region "ignored"
    }
}

client<llm> RealBedrock {
    provider aws-bedrock
    options {
        endpoint_url "http://localhost:9000"
    }
}
`,
		},
		{
			name: "BedrockEndpointURLInlineSingleLine",
			src:  `client<llm> InlineBedrock { provider aws-bedrock options { endpoint_url "http://h:1" region "us-east-1" } }`,
		},
		{
			name: "BedrockEndpointURLInlineCommentsAndQuotes",
			src: `
client<llm> CommentedBedrock {
    provider aws-bedrock
    options {
        endpoint_url "http://localhost:9000"  // operator override for local dev
        region "us-east-1"  // production region
    }
}
`,
		},
		{
			name: "BedrockEndpointURLOnlyRegion",
			src: `
client<llm> RegionOnlyBedrock {
    provider aws-bedrock
    options {
        region "ap-southeast-2"
    }
}
`,
		},
		{
			name: "BedrockStaticCredsLiteral",
			src: `
client<llm> StaticCredsBedrock {
    provider aws-bedrock
    options {
        access_key_id     "STATIC_TEST_ACCESS_KEY"
        secret_access_key "STATIC_TEST_SECRET_KEY"
        session_token     "STATIC_TEST_SESSION_TOKEN"
        region            "us-east-1"
    }
}
`,
		},
		{
			name: "BedrockStaticCredsEnvRef",
			src: `
client<llm> EnvCredsBedrock {
    provider aws-bedrock
    options {
        access_key_id     env.AWS_ACCESS_KEY_ID
        secret_access_key env.AWS_SECRET_ACCESS_KEY
        session_token     env.AWS_SESSION_TOKEN
        profile           env.AWS_PROFILE
    }
}
`,
		},
		{
			name: "BedrockStaticCredsMixedLiteralAndEnv",
			src: `
client<llm> MixedCredsBedrock {
    provider aws-bedrock
    options {
        access_key_id     "STATIC_TEST_ACCESS_KEY"
        secret_access_key env.AWS_SECRET_ACCESS_KEY
    }
}
`,
		},
		{
			name: "BedrockStaticCredsProfileOnly",
			src: `
client<llm> ProfileBedrock {
    provider aws-bedrock
    options {
        profile "ai-dev"
    }
}
`,
		},
		{
			name: "BedrockStaticCredsProfilePlusStatic",
			src: `
client<llm> BothCredsBedrock {
    provider aws-bedrock
    options {
        access_key_id     "STATIC_TEST_ACCESS_KEY"
        secret_access_key "STATIC_TEST_SECRET_KEY"
        profile           "ai-dev"
    }
}
`,
		},
		{
			name: "BedrockStaticCredsNonBedrockClientRecorded",
			src: `
client<llm> NotBedrockCreds {
    provider openai
    options {
        access_key_id     "should-not-emit"
        secret_access_key "should-not-emit"
    }
}
`,
		},
		{
			name: "BedrockStaticCredsInlineSingleLine",
			src:  `client<llm> InlineCreds { provider aws-bedrock options { access_key_id "STATIC_TEST_ACCESS_KEY" secret_access_key "STATIC_TEST_SECRET_KEY" } }`,
		},
		{
			name: "MultilineStrategyComments",
			src: `
client<llm> TestFB {
    provider baml-fallback
    options {
        strategy [
            ClientA, // primary
            ClientB, // secondary
        ]
    }
}
`,
		},
		{
			name: "MultilineStrategyOpeningLineComment",
			src: `
client<llm> OpenLineComment {
    provider baml-fallback
    options {
        strategy [ // opening comment
            ClientA,
            ClientB,
        ]
    }
}
`,
		},
		{
			name: "OptionsOpeningLineStrategyComment",
			src: `
client<llm> InlineOptionsOpenLine {
    provider baml-fallback
    options { strategy [ // opening comment
        ClientA,
        ClientB,
    ] }
}
`,
		},

		// ---- Hand-built edge cases from Codex scoping Q1 ----
		{
			name: "TopLevelClassEnumTypeIgnored",
			src: `
// Tolerated upstream constructs that introspect must ignore.
class SimpleOutput {
    message string
}

enum Category {
    A
    B
    C
}

type TreeNodeList = TreeNode[]
type JsonValue = int | float | bool | string | null | JsonValue[] | map<string, JsonValue>

template_string Greeting(name: string) #"Hello {{ name }}"#

test BasicTest {
    functions [F]
    args { x "y" }
}

client<llm> RealClient {
    provider openai
}

function RealFunc(x: string) -> string {
    client RealClient
    prompt #"{{ x }}"#
}
`,
		},
		{
			name: "TopLevelGeneratorIgnored",
			src: `
generator g {
    output_type "rest/go"
    output_dir "../"
    version "0.219.0"
}

client<llm> X {
    provider openai
}
`,
		},
		{
			name: "BedrockEmptyLiteralSkipped",
			src: `
client<llm> EmptyLit {
    provider aws-bedrock
    options {
        endpoint_url ""
        region "us-east-1"
    }
}
`,
		},
		{
			name: "StrategyListWhitespaceSeparated",
			src: `
client<llm> WS {
    provider baml-fallback
    options { strategy [A B C] }
}
`,
		},
		{
			name: "DuplicateProviderLastWins",
			src: `
client<llm> Dup {
    provider openai
    provider anthropic
}
`,
		},
		{
			name: "DuplicateRetryPolicyLastWins",
			src: `
client<llm> Dup {
    provider openai
    retry_policy A
    retry_policy B
}
`,
		},
		{
			name: "MissingRetryFieldsZeroed",
			src: `
retry_policy Empty { }
`,
		},
		{
			name: "RetryWithDirectFieldsAndNestedStrategyOverride",
			src: `
retry_policy Direct {
    max_retries 3
    type constant_delay
    delay_ms 100
    strategy {
        type exponential_backoff
        delay_ms 500
        multiplier 2.0
    }
}
`,
		},
		{
			name: "FunctionWithoutSignatureIgnored",
			src: `
function NoSig { client X }

client<llm> X { provider openai }
`,
		},
		{
			name: "ProviderWithHyphensAndSlashes",
			src: `
client<llm> AwsBR {
    provider aws-bedrock
}

function Shortcut(x: string) -> string {
    client "openai/gpt-4o"
}
`,
		},

		// ---- Codex sign-off Section 4 fills ----
		//
		// Pin that a plain `client Name { ... }` block (no <llm> type
		// param) is parsed by the new grammar but excluded by the
		// walker's TypeParam == "llm" gate — matches the old parser's
		// `client<llm>`-only consumption posture at
		// cmd/introspect/main.go:1324-1325. PR 4 / #268 may widen this
		// to consume plain `client` blocks; until then, the gate is
		// part of the contract.
		//
		// The Foo block deliberately contains only fields whose keys
		// are NOT top-level dispatch prefixes in the old parser
		// (`client<llm>`, `function `, `retry_policy `). Embedding a
		// `retry_policy NAME` inner field would trigger the old
		// line-based parser's greedy outer-scope re-scan of Foo's
		// body and produce a spurious top-level retry policy entry —
		// an orthogonal quirk that this case is not pinning. The
		// shape used here isolates the TypeParam gate cleanly.
		{
			name: "PlainClientWithoutLLMIgnored",
			src: `
client Foo {
    provider openai
}

client<llm> Bar {
    provider anthropic
    retry_policy Slow
}
`,
		},
		// Pin the depth-gating contract for options-block fields:
		//   - strategy   → captured at ANY depth inside options { } (not
		//     gated; the old parser's extractStrategyStatement runs on
		//     every line while inOptions is true — see
		//     cmd/introspect/main.go:1778-1794).
		//   - start      → captured ONLY at the immediate options depth
		//     (optionsDepth == 1; cmd/introspect/main.go:1804-1807).
		//   - Bedrock keys → captured ONLY at the immediate options
		//     depth (cmd/introspect/main.go:1808-1815).
		//
		// The fixture nests a `custom_subblock` inside options. The
		// nested `start 99` and `endpoint_url "x"` must NOT leak; the
		// nested `strategy [C, D]` SHOULD override the outer
		// `strategy [A, B]` (last-write-wins). `region "us-east-1"`
		// sits at the top options depth so it is captured normally.
		{
			name: "OptionsNestedDepthGating",
			src: `
client<llm> NestedDepth {
    provider baml-fallback
    options {
        strategy [A, B]
        start 1
        custom_subblock {
            start 99
            endpoint_url "x"
            strategy [C, D]
        }
        region "us-east-1"
    }
}
`,
		},
		// Pin that a quoted "env.X" string is a Literal Bedrock value,
		// NOT an env reference. The old parser's
		// extractBedrockOptionValue (cmd/introspect/main.go:2068-2108)
		// only treats bare `env.NAME` as env provenance; the surrounding
		// quotes are stripped by stripBamlQuotes and the result becomes
		// Literal provenance. Mirrors the parser-standalone unit test
		// at bamlutils/bamlparser/bamlparser_test.go:TestParse_QuotedEnvNameIsLiteralNotEnvRef
		// but pins the end-to-end bedrockOptionValue shape.
		{
			name: "BedrockQuotedEnvIsLiteral",
			src: `
client<llm> QuotedEnvBedrock {
    provider aws-bedrock
    options {
        endpoint_url env.MY_HOST
        region       "env.US_EAST_1"
    }
}
`,
		},
		// Pin that duplicate top-level `client` fields inside a
		// function block resolve last-wins. The old parser's
		// parseFunctionBlock (cmd/introspect/main.go:2143-2174) keeps
		// scanning every line and overwrites functionClient[name] on
		// each `client VALUE` match. The walker fix at
		// parityProcessFunction (this file, above) removed an early
		// `break` to mirror that semantics.
		{
			name: "DuplicateFunctionClientLastWins",
			src: `
client<llm> First { provider openai }
client<llm> Second { provider anthropic }

function Foo(x: string) -> string {
    client First
    prompt #"{{ x }}"#
    client Second
}
`,
		},
	}
}

func TestParity_OldVsNewParser_Fixtures(t *testing.T) {
	for _, tc := range parityCases() {
		t.Run(tc.name, func(t *testing.T) {
			if reason, divergent := knownParityDivergences[tc.name]; divergent {
				t.Skipf("known divergence: %s", reason)
			}
			oldCfg := runOldParser(tc.src)
			newCfg := runNewParser(t, tc.src)
			assertBamlConfigEqual(t, oldCfg, newCfg)
		})
	}
}

// assertBamlConfigEqual deep-compares two bamlConfig values, reporting the
// specific map that diverged so failures point at the precise data shape
// out of alignment rather than dumping the entire config.
func assertBamlConfigEqual(t *testing.T, want, got *bamlConfig) {
	t.Helper()
	if !reflect.DeepEqual(normaliseStringMap(want.clientProvider), normaliseStringMap(got.clientProvider)) {
		t.Errorf("clientProvider mismatch:\nold: %#v\nnew: %#v",
			normaliseStringMap(want.clientProvider), normaliseStringMap(got.clientProvider))
	}
	if !reflect.DeepEqual(normaliseStringMap(want.clientRetryPolicy), normaliseStringMap(got.clientRetryPolicy)) {
		t.Errorf("clientRetryPolicy mismatch:\nold: %#v\nnew: %#v",
			normaliseStringMap(want.clientRetryPolicy), normaliseStringMap(got.clientRetryPolicy))
	}
	if !reflect.DeepEqual(normaliseStringMap(want.functionClient), normaliseStringMap(got.functionClient)) {
		t.Errorf("functionClient mismatch:\nold: %#v\nnew: %#v",
			normaliseStringMap(want.functionClient), normaliseStringMap(got.functionClient))
	}
	if !reflect.DeepEqual(want.retryPolicies, got.retryPolicies) {
		t.Errorf("retryPolicies mismatch:\nold: %#v\nnew: %#v", want.retryPolicies, got.retryPolicies)
	}
	if !reflect.DeepEqual(normaliseStringSliceMap(want.fallbackChains), normaliseStringSliceMap(got.fallbackChains)) {
		t.Errorf("fallbackChains mismatch:\nold: %#v\nnew: %#v",
			normaliseStringSliceMap(want.fallbackChains), normaliseStringSliceMap(got.fallbackChains))
	}
	if !reflect.DeepEqual(want.roundRobinStart, got.roundRobinStart) {
		t.Errorf("roundRobinStart mismatch:\nold: %#v\nnew: %#v", want.roundRobinStart, got.roundRobinStart)
	}
	if !reflect.DeepEqual(want.bedrockClientOptions, got.bedrockClientOptions) {
		t.Errorf("bedrockClientOptions mismatch:\nold: %#v\nnew: %#v", want.bedrockClientOptions, got.bedrockClientOptions)
	}
}

// normaliseStringMap replaces a nil map with an empty one so reflect.DeepEqual
// reports parity between configs that left a map untouched vs. those that
// created one with no entries — both are semantically equivalent.
func normaliseStringMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func normaliseStringSliceMap(m map[string][]string) map[string][]string {
	if m == nil {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Copy slice to avoid aliasing surprises in DeepEqual.
		out[k] = append([]string(nil), m[k]...)
	}
	return out
}
