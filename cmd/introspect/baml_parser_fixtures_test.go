// BAML parser fixture corpus for the golden snapshot suite.
//
// This file holds the canonical fixture table for the BAML config
// surface — every fixture #265 PR 1 / PR 2 used to assert old/new
// parser parity, plus #265 PR 3's additions for the gaps Codex's
// scoping identified (comment-with-`]` variants, duplicate-client
// stale clearing, empty strategy list). The shapes pinned here cover
// every documented quirk from the migration sequence: TypeParam
// gating, function-without-parens, options depth asymmetry, retry
// precedence, env provenance, duplicate last-wins, malformed nested
// tokens, compact forms, and Bedrock credential provenance.
//
// Pre-#265 PR 3, the file fed each fixture through BOTH the old
// line-based parser AND the production AST walker and asserted byte-
// identical *bamlConfig output. Post-PR 3 the old parser is gone, so
// the fixtures are exercised by TestBamlConfigGoldens
// (baml_parser_golden_test.go), which compares the production
// walker's *bamlConfig snapshot against a stored expected snapshot in
// baml_parser_golden_data_test.go. Regenerate that data file with
// -update-baml-config-goldens after intentional fixture changes.

package main

// parityCase is a single fixture in the BAML config corpus. The name
// "parity" is preserved from the #265 PR 1 origin; the fixtures now
// drive a golden snapshot harness rather than an old/new parity
// comparison, but renaming would obscure the migration provenance for
// readers tracing back to the PR sequence.
type parityCase struct {
	name string
	src  string
}

// ----------------------------------------------------------------------------
// BAML config fixture corpus. Each case feeds the production walker via
// TestBamlConfigGoldens and is compared against a stored snapshot.
// ----------------------------------------------------------------------------

func parityCases() []parityCase {
	return []parityCase{
		// ---- Fixtures mirrored from baml_parser_test.go in #265 PR 1 ----
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

		// ---- Additional Q1 parity gaps ----
		//
		// Pin that a plain `client Name { ... }` block (no <llm> type
		// param) is parsed by the grammar but excluded by the walker's
		// TypeParam == "llm" gate — preserves introspect's `client<llm>`-only
		// consumption posture. #268 may widen this to consume plain
		// `client` blocks; until then, the gate is part of the contract.
		//
		// The Foo block deliberately contains only fields whose keys
		// are NOT top-level dispatch keywords (`client`, `function`,
		// `retry_policy`). Pre-PR-3 the line-based parser had a
		// greedy outer-scope re-scan quirk that would consume an inner
		// `retry_policy NAME` as a top-level declaration; that quirk
		// is gone with the line parser, but the fixture keeps the
		// minimal shape so it isolates the TypeParam gate cleanly.
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
		//     gated; the walker recurses into nested sub-blocks for the
		//     strategy key specifically, matching pre-PR-3 line-parser
		//     behaviour).
		//   - start      → captured ONLY at the immediate options depth.
		//   - Bedrock keys → captured ONLY at the immediate options depth.
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
		// NOT an env reference. Only bare `env.NAME` carries env
		// provenance; surrounding quotes make the value a string
		// literal at the lexer/grammar level, and the walker maps it
		// to bedrockOptionValue Literal provenance accordingly.
		// Mirrors the parser-standalone unit test
		// TestParse_QuotedEnvNameIsLiteralNotEnvRef but pins the
		// end-to-end bedrockOptionValue shape.
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
		// function block resolve last-wins. The production
		// processBAMLFunctionBlock walker iterates every top-level
		// `client` field and overwrites functionClient[name] on each
		// match, matching pre-PR-3 line-parser semantics.
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
		// Pin the "skip garbage tokens inside a block, keep parsing later
		// fields" semantics that the prior hand-rolled parser provided
		// via parseField's non-ident bail-and-advance fallback
		// (master:bamlutils/bamlparser/bamlparser.go:398-408 pre-refactor).
		// Caught by Codex's sign-off on PR #270: the declarative
		// `"{" @@* "}"?` Block grammar terminated the body at the first
		// non-Ident token and leaked the rest of the body to the outer
		// scope as `Other` items.
		//
		// Only the leading-garbage shape needed to live in the
		// regression corpus — both pre-PR-3 line parser and bamlparser
		// dropped the leading `@` cleanly. Between-fields-garbage
		// shapes (`provider openai @ retry_policy ...`) used to
		// diverge between the two parsers (the line parser's keyword-
		// boundary split captured the trailing `@` in the preceding
		// field's value, producing `provider="openai @"`; the
		// bamlparser drops the stray token). With the line parser
		// gone, the bamlparser side of that contract is pinned by the
		// standalone parser tests (TestParse_MalformedNestedTokenBetweenFieldsSkipped
		// / TestParse_MalformedNestedTokenSequenceSkipped in
		// bamlutils/bamlparser/bamlparser_test.go) instead of here.
		{
			name: "MalformedNestedTokenAtStart",
			src:  `client<llm> X { @ provider openai }`,
		},

		// ---- #265 PR 3 additions: coverage the direct old-helper tests
		// pinned and the original 66-case corpus did not. ----

		// Comment containing `]` mid-strategy-continuation. Pins that the
		// production walker (and bamlparser's lexer) treat `// ] not done
		// yet` as a comment and do not close the strategy list early.
		// Replaces the direct
		// TestParseClientBlock_MultilineStrategyContinuationCommentWithClosingBracket
		// test at baml_parser_test.go:1173.
		{
			name: "MultilineStrategyContinuationCommentWithClosingBracket",
			src: `
client<llm> ContinuationCommentBracket {
    provider baml-fallback
    options {
        strategy [
            ClientA, // ] not done yet
            ClientB,
        ]
    }
}
`,
		},
		// Comment with `]` on the inline `options { strategy [` opening
		// line. Pins the same comment-stripping invariant on the inline
		// shape. Replaces the direct
		// TestParseClientBlock_OptionsOpeningLineStrategyCommentWithClosingBracket
		// test at baml_parser_test.go:1197.
		{
			name: "OptionsOpeningLineStrategyCommentWithClosingBracket",
			src: `
client<llm> InlineOptionsOpenLineBracket {
    provider baml-fallback
    options { strategy [ // ] opening comment
        ClientA,
        ClientB,
    ] }
}
`,
		},
		// Post-`]` `start N }` suffix on the closing line of a multiline
		// strategy list. The existing RoundRobinStartTrailingCloseBrace
		// fixture covers `start 1 }` on a non-list-close line; this case
		// pins the harder shape `] start 1 }` where the close-bracket of
		// the strategy list and the start declaration co-occupy the same
		// line. Replaces a subcase of
		// TestParseClientBlock_RoundRobinStart_TrailingCloseBrace at
		// baml_parser_test.go:1292.
		{
			name: "RoundRobinStartTrailingCloseBraceSuffix",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [
            A,
            B,
        ] start 1 }
}
`,
		},
		// Duplicate-client stale clearing for round-robin start (absent).
		// First declaration populates start=5; second declaration omits
		// start. The stale entry must be cleared. Pins the parser's
		// per-block delete-then-rewrite contract that the coordinator's
		// "absent start ⇒ random seed" fallback depends on. Replaces
		// TestParseClientBlock_RoundRobinStart_Absent at
		// baml_parser_test.go:1246.
		{
			name: "DuplicateClientStaleClearing_RoundRobinStartAbsent",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
        start 5
    }
}

client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
    }
}
`,
		},
		// Duplicate-client stale clearing for round-robin start (invalid
		// second value). Replaces
		// TestParseClientBlock_RoundRobinStart_InvalidIgnored at
		// baml_parser_test.go:1265.
		{
			name: "DuplicateClientStaleClearing_RoundRobinStartInvalid",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
        start 5
    }
}

client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
        start oops
    }
}
`,
		},
		// Duplicate-client stale clearing for round-robin start (out-of-i32
		// second value). Pins that the i32 ParseInt width clamps even
		// when an earlier valid value already lives in the map. Replaces
		// TestParseClientBlock_RoundRobinStart_OutOfI32Ignored at
		// baml_parser_test.go:1360.
		{
			name: "DuplicateClientStaleClearing_RoundRobinStartOutOfI32",
			src: `
client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
        start 5
    }
}

client<llm> RR {
    provider baml-roundrobin
    options {
        strategy [A, B]
        start 2147483648
    }
}
`,
		},
		// Duplicate-client stale clearing for Bedrock endpoint_url. First
		// declaration sets endpoint_url; second declaration omits it and
		// the bedrockClientOptions entry must be cleared rather than left
		// stale. Replaces
		// TestParseClientBlock_BedrockEndpointURL_StaleEntryCleared at
		// baml_parser_test.go:1996.
		{
			name: "DuplicateClientStaleClearing_BedrockEndpoint",
			src: `
client<llm> Foo {
    provider aws-bedrock
    options {
        endpoint_url "http://h"
    }
}

client<llm> Foo {
    provider aws-bedrock
    options {
        model "x"
    }
}
`,
		},
		// Duplicate-client stale clearing for Bedrock static credentials.
		// First declaration populates access_key_id / secret_access_key;
		// second declaration omits both and the bedrockClientOptions
		// entry must be cleared. Replaces
		// TestParseClientBlock_BedrockStaticCreds_StaleEntryCleared at
		// baml_parser_test.go:2357.
		{
			name: "DuplicateClientStaleClearing_BedrockCreds",
			src: `
client<llm> CredsBedrock {
    provider aws-bedrock
    options {
        access_key_id     "STATIC_TEST_ACCESS_KEY"
        secret_access_key "STATIC_TEST_SECRET_KEY"
    }
}

client<llm> CredsBedrock {
    provider aws-bedrock
    options {
        model "anthropic.claude-3-sonnet-20240229-v1:0"
    }
}
`,
		},
		// Empty strategy list produces no fallback chain entry. Pins the
		// processBAMLOptionsBlock `if len(chain) > 0` guard so a
		// fallback-provider client with an empty `strategy []` does not
		// leave a zero-length chain in the map (the runtime classifier
		// distinguishes "no chain" from "empty chain"). Replaces the
		// pre-PR-3 direct `parseStrategyList("strategy [])` row in the
		// deleted helper-internal TestParseStrategyList.
		{
			name: "EmptyStrategyListNoFallbackChain",
			src: `
client<llm> EmptyStrategy {
    provider baml-fallback
    options {
        strategy []
    }
}
`,
		},
	}
}
