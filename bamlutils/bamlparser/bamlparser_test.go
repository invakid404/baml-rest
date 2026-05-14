package bamlparser

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *File {
	t.Helper()
	f, err := ParseString("test.baml", src)
	if err != nil {
		t.Fatalf("ParseString: %v", err)
	}
	return f
}

// findClient returns the first client block with the given name, or nil.
func findClient(f *File, name string) *ClientBlock {
	for _, it := range f.Items {
		if it.Client != nil && it.Client.Name == name {
			return it.Client
		}
	}
	return nil
}

func findFunction(f *File, name string) *FunctionBlock {
	for _, it := range f.Items {
		if it.Function != nil && it.Function.Name == name {
			return it.Function
		}
	}
	return nil
}

func findRetryPolicy(f *File, name string) *RetryPolicyBlock {
	for _, it := range f.Items {
		if it.RetryPolicy != nil && it.RetryPolicy.Name == name {
			return it.RetryPolicy
		}
	}
	return nil
}

func findGenerator(f *File, name string) *GeneratorBlock {
	for _, it := range f.Items {
		if it.Generator != nil && it.Generator.Name == name {
			return it.Generator
		}
	}
	return nil
}

func fieldByKey(fields []*Field, key string) *Field {
	for _, f := range fields {
		if f.Key == key {
			return f
		}
	}
	return nil
}

func TestParse_EmptyFile(t *testing.T) {
	f := mustParse(t, "")
	if len(f.Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(f.Items))
	}
}

func TestParse_OnlyComments(t *testing.T) {
	f := mustParse(t, "// just a comment\n// another line\n")
	if len(f.Items) != 0 {
		t.Errorf("expected 0 items, got %d: %+v", len(f.Items), f.Items)
	}
}

func TestParse_ClientWithLLMTypeParam(t *testing.T) {
	src := `
client<llm> MyClient {
    provider openai
    retry_policy Fast
}
`
	f := mustParse(t, src)
	c := findClient(f, "MyClient")
	if c == nil {
		t.Fatalf("client MyClient missing; items: %+v", f.Items)
	}
	if c.TypeParam != "llm" {
		t.Errorf("TypeParam = %q, want llm", c.TypeParam)
	}
	prov := fieldByKey(c.Fields, "provider")
	if prov == nil || prov.Value == nil || prov.Value.Ident == nil || *prov.Value.Ident != "openai" {
		t.Errorf("provider field missing/wrong: %+v", prov)
	}
	rp := fieldByKey(c.Fields, "retry_policy")
	if rp == nil || rp.Value == nil || rp.Value.Ident == nil || *rp.Value.Ident != "Fast" {
		t.Errorf("retry_policy field missing/wrong: %+v", rp)
	}
}

func TestParse_ClientWithSpacedTypeParam(t *testing.T) {
	src := `client <llm> SpacedClient { provider anthropic }`
	f := mustParse(t, src)
	c := findClient(f, "SpacedClient")
	if c == nil {
		t.Fatalf("client SpacedClient missing")
	}
	if c.TypeParam != "llm" {
		t.Errorf("TypeParam = %q, want llm", c.TypeParam)
	}
}

func TestParse_ClientWithoutTypeParam(t *testing.T) {
	// Upstream BAML grammar accepts plain `client Name { ... }`. The
	// parser must capture it; the introspect-side walker decides whether
	// to consume it (current posture: only client<llm> is consumed).
	src := `client Plain { provider openai }`
	f := mustParse(t, src)
	c := findClient(f, "Plain")
	if c == nil {
		t.Fatalf("plain client missing; items: %+v", f.Items)
	}
	if c.TypeParam != "" {
		t.Errorf("TypeParam = %q, want empty", c.TypeParam)
	}
}

func TestParse_FunctionSignatureIgnored(t *testing.T) {
	src := `
function GetPerson(description: string, ctx: Context) -> Person {
    client MyClient
    prompt #"Extract: {{ description }}"#
}
`
	f := mustParse(t, src)
	fn := findFunction(f, "GetPerson")
	if fn == nil {
		t.Fatalf("function GetPerson missing")
	}
	c := fieldByKey(fn.Fields, "client")
	if c == nil || c.Value == nil || c.Value.Ident == nil || *c.Value.Ident != "MyClient" {
		t.Errorf("client field missing/wrong: %+v", c)
	}
	// Prompt is captured as a Raw value.
	pr := fieldByKey(fn.Fields, "prompt")
	if pr == nil || pr.Value == nil || pr.Value.Raw == nil {
		t.Fatalf("prompt field should carry Raw value: %+v", pr)
	}
	if !strings.Contains(*pr.Value.Raw, "Extract:") {
		t.Errorf("Raw value content unexpected: %q", *pr.Value.Raw)
	}
}

func TestParse_FunctionQuotedShorthandClient(t *testing.T) {
	src := `
function Shorthand(input: string) -> string {
    client "openai/gpt-4o"
    prompt #"{{ input }}"#
}
`
	f := mustParse(t, src)
	fn := findFunction(f, "Shorthand")
	if fn == nil {
		t.Fatalf("function Shorthand missing")
	}
	c := fieldByKey(fn.Fields, "client")
	if c == nil || c.Value == nil || c.Value.Literal == nil {
		t.Fatalf("client should be Literal: %+v", c)
	}
	if *c.Value.Literal != "openai/gpt-4o" {
		t.Errorf("literal = %q, want openai/gpt-4o", *c.Value.Literal)
	}
}

func TestParse_RetryPolicyWithStrategyBlock(t *testing.T) {
	src := `
retry_policy ExpRetry {
    max_retries 5
    strategy {
        type exponential_backoff
        delay_ms 100
        multiplier 2.0
        max_delay_ms 10000
    }
}
`
	f := mustParse(t, src)
	rp := findRetryPolicy(f, "ExpRetry")
	if rp == nil {
		t.Fatalf("retry policy missing")
	}
	mr := fieldByKey(rp.Fields, "max_retries")
	if mr == nil || mr.Value == nil || mr.Value.Number == nil || *mr.Value.Number != "5" {
		t.Errorf("max_retries field wrong: %+v", mr)
	}
	st := fieldByKey(rp.Fields, "strategy")
	if st == nil || st.Block == nil {
		t.Fatalf("strategy block missing: %+v", st)
	}
	ty := fieldByKey(st.Block.Fields, "type")
	if ty == nil || ty.Value == nil || ty.Value.Ident == nil || *ty.Value.Ident != "exponential_backoff" {
		t.Errorf("strategy.type wrong: %+v", ty)
	}
	mult := fieldByKey(st.Block.Fields, "multiplier")
	if mult == nil || mult.Value == nil || mult.Value.Number == nil || *mult.Value.Number != "2.0" {
		t.Errorf("multiplier wrong: %+v", mult)
	}
}

func TestParse_GeneratorBlock(t *testing.T) {
	src := `
generator g {
    output_type "rest/go"
    output_dir "../"
    version "0.219.0"
    default_client_mode "sync"
}
`
	f := mustParse(t, src)
	g := findGenerator(f, "g")
	if g == nil {
		t.Fatalf("generator missing")
	}
	ver := fieldByKey(g.Fields, "version")
	if ver == nil || ver.Value == nil || ver.Value.Literal == nil || *ver.Value.Literal != "0.219.0" {
		t.Errorf("version field wrong: %+v", ver)
	}
}

func TestParse_OptionsListAndEnvRef(t *testing.T) {
	src := `
client<llm> Bedrock {
    provider aws-bedrock
    options {
        strategy [Fast, Slow]
        region env.AWS_REGION
        endpoint_url "http://localhost:9000"
    }
}
`
	f := mustParse(t, src)
	c := findClient(f, "Bedrock")
	if c == nil {
		t.Fatalf("client missing")
	}
	opts := fieldByKey(c.Fields, "options")
	if opts == nil || opts.Block == nil {
		t.Fatalf("options block missing: %+v", opts)
	}
	strat := fieldByKey(opts.Block.Fields, "strategy")
	if strat == nil || strat.Value == nil || strat.Value.List == nil {
		t.Fatalf("strategy not a list: %+v", strat)
	}
	if len(strat.Value.List) != 2 {
		t.Errorf("expected 2 strategy elements, got %d", len(strat.Value.List))
	}
	if got, _ := strat.Value.List[0].IdentValue(); got != "Fast" {
		t.Errorf("strategy[0] ident = %q, want Fast", got)
	}
	if got, _ := strat.Value.List[1].IdentValue(); got != "Slow" {
		t.Errorf("strategy[1] ident = %q, want Slow", got)
	}
	region := fieldByKey(opts.Block.Fields, "region")
	if region == nil || region.Value == nil {
		t.Fatalf("region missing")
	}
	if name, ok := region.Value.EnvName(); !ok || name != "AWS_REGION" {
		t.Errorf("region env name = (%q, %v); want (AWS_REGION, true)", name, ok)
	}
	endp := fieldByKey(opts.Block.Fields, "endpoint_url")
	if endp == nil || endp.Value == nil {
		t.Fatalf("endpoint_url missing")
	}
	if lit, ok := endp.Value.LiteralValue(); !ok || lit != "http://localhost:9000" {
		t.Errorf("endpoint_url literal = (%q, %v); want literal", lit, ok)
	}
}

func TestParse_QuotedEnvNameIsLiteralNotEnvRef(t *testing.T) {
	// `"env.NAME"` is a quoted string — upstream BAML treats it as a
	// regular Literal, NOT an env reference. Only the bare `env.NAME`
	// form is an env reference. This pin prevents accidental quoting
	// folds in the lexer that would conflate the two.
	src := `client<llm> X { provider aws-bedrock options { region "env.AWS_REGION" } }`
	f := mustParse(t, src)
	c := findClient(f, "X")
	opts := fieldByKey(c.Fields, "options")
	region := fieldByKey(opts.Block.Fields, "region")
	if !region.Value.IsLiteral() {
		t.Errorf("quoted env.X must be Literal, got %+v", region.Value)
	}
	if region.Value.IsEnvRef() {
		t.Errorf("quoted env.X must NOT be EnvRef, got %+v", region.Value)
	}
	if lit, _ := region.Value.LiteralValue(); lit != "env.AWS_REGION" {
		t.Errorf("literal = %q, want env.AWS_REGION", lit)
	}
}

func TestParse_RawStringPreservesBracesAndComments(t *testing.T) {
	// Raw strings must preserve braces and `//` sequences verbatim;
	// they're frequently used for prompt bodies that contain JSON
	// samples and inline URLs.
	src := `
function F(input: string) -> string {
    client X
    prompt #"
        sample: { "url": "https://example.com/v1" } // not a comment
        nested { braces { ok } }
    "#
}
`
	f := mustParse(t, src)
	fn := findFunction(f, "F")
	if fn == nil {
		t.Fatalf("function F missing")
	}
	pr := fieldByKey(fn.Fields, "prompt")
	if pr == nil || pr.Value == nil || pr.Value.Raw == nil {
		t.Fatalf("prompt raw missing: %+v", pr)
	}
	raw := *pr.Value.Raw
	if !strings.Contains(raw, `// not a comment`) {
		t.Errorf("raw string lost `//`: %q", raw)
	}
	if !strings.Contains(raw, `{ braces { ok } }`) {
		t.Errorf("raw string lost braces: %q", raw)
	}
}

func TestParse_TopLevelClassEnumTestTolerated(t *testing.T) {
	// class, enum, type-alias, test, template_string must not error.
	src := `
class SimpleOutput {
    message string
}

enum Category {
    A
    B
}

type JsonValue = int | float | bool

template_string Greeting(name: string) #"Hello {{ name }}"#

test BasicTest {
    functions [F]
    args { x "y" }
}

client<llm> Real {
    provider openai
}
`
	f, err := ParseString("test.baml", src)
	if err != nil {
		t.Fatalf("expected permissive parse, got error: %v", err)
	}
	// Real client should be recognised at the end.
	if findClient(f, "Real") == nil {
		t.Errorf("client Real not found after tolerated top-level constructs; items: %+v", f.Items)
	}
	// Confirm class is captured as TypeBlock and type alias as TypeAlias.
	var sawClass, sawEnum, sawTypeAlias, sawTemplate, sawTest bool
	for _, it := range f.Items {
		if it.TypeBlock != nil {
			switch it.TypeBlock.Keyword {
			case "class":
				sawClass = true
			case "enum":
				sawEnum = true
			case "test":
				sawTest = true
			}
		}
		if it.TypeAlias != nil {
			sawTypeAlias = true
		}
		if it.Template != nil {
			sawTemplate = true
		}
	}
	if !sawClass {
		t.Errorf("class not captured as TypeBlock")
	}
	if !sawEnum {
		t.Errorf("enum not captured as TypeBlock")
	}
	if !sawTypeAlias {
		t.Errorf("type alias not captured")
	}
	if !sawTemplate {
		t.Errorf("template_string not captured")
	}
	if !sawTest {
		t.Errorf("test block not captured as TypeBlock")
	}
}

func TestParse_NestedBracesInTypeBlockTolerated(t *testing.T) {
	// class with nested generic syntax `map<string, list<int>>` and
	// optional markers must not throw off block skipping.
	src := `
class Wrap {
    data map<string, list<int>>
    meta SomeType?
    children Wrap[]
}

client<llm> After { provider openai }
`
	f := mustParse(t, src)
	if findClient(f, "After") == nil {
		t.Errorf("client After not found; class nesting confused parser")
	}
}

func TestParse_CompactOneLineBlocks(t *testing.T) {
	src := `client<llm> Foo { provider openai retry_policy Fast }
retry_policy Fast { max_retries 2 type constant_delay delay_ms 100 }
function F(x: string) -> string { client Foo }`
	f := mustParse(t, src)
	foo := findClient(f, "Foo")
	if foo == nil {
		t.Fatalf("client Foo missing")
	}
	if fb := fieldByKey(foo.Fields, "provider"); fb == nil || *fb.Value.Ident != "openai" {
		t.Errorf("Foo.provider missing/wrong")
	}
	if fb := fieldByKey(foo.Fields, "retry_policy"); fb == nil || *fb.Value.Ident != "Fast" {
		t.Errorf("Foo.retry_policy missing/wrong")
	}
	rp := findRetryPolicy(f, "Fast")
	if rp == nil {
		t.Fatalf("Fast retry_policy missing")
	}
	if fb := fieldByKey(rp.Fields, "delay_ms"); fb == nil || *fb.Value.Number != "100" {
		t.Errorf("Fast.delay_ms missing/wrong: %+v", fb)
	}
}

func TestValue_HelpersRoundTrip(t *testing.T) {
	lit := "openai"
	env := "AWS_REGION"
	ident := "Fast"
	num := "3"
	raw := "prompt body"
	cases := []struct {
		name     string
		v        *Value
		isLit    bool
		isEnv    bool
		isIdent  bool
		isNum    bool
		isRaw    bool
		isList   bool
		expected string
	}{
		{"literal", &Value{Literal: &lit}, true, false, false, false, false, false, "openai"},
		{"env", &Value{EnvRef: &env}, false, true, false, false, false, false, "AWS_REGION"},
		{"ident", &Value{Ident: &ident}, false, false, true, false, false, false, "Fast"},
		{"number", &Value{Number: &num}, false, false, false, true, false, false, "3"},
		{"raw", &Value{Raw: &raw}, false, false, false, false, true, false, "prompt body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.v.IsLiteral() != tc.isLit {
				t.Errorf("IsLiteral")
			}
			if tc.v.IsEnvRef() != tc.isEnv {
				t.Errorf("IsEnvRef")
			}
			if tc.v.IsIdent() != tc.isIdent {
				t.Errorf("IsIdent")
			}
			if tc.v.IsNumber() != tc.isNum {
				t.Errorf("IsNumber")
			}
			if tc.v.IsRaw() != tc.isRaw {
				t.Errorf("IsRaw")
			}
			if tc.v.IsList() != tc.isList {
				t.Errorf("IsList")
			}
			s, ok := tc.v.String()
			if !ok || s != tc.expected {
				t.Errorf("String() = (%q, %v), want (%q, true)", s, ok, tc.expected)
			}
		})
	}
}

func TestValue_StringList_ListShape(t *testing.T) {
	a := "A"
	b := "B"
	v := &Value{List: []*Value{
		{Ident: &a},
		{Ident: &b},
	}}
	got, ok := v.StringList()
	if !ok {
		t.Fatalf("ok=false")
	}
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Errorf("got %v, want [A B]", got)
	}
}

func TestValue_StringList_EmptyList(t *testing.T) {
	v := &Value{List: []*Value{}}
	got, ok := v.StringList()
	if !ok {
		t.Fatalf("ok=false for empty list")
	}
	if len(got) != 0 {
		t.Errorf("got %v, want []", got)
	}
}

func TestParse_MultilineStrategyList(t *testing.T) {
	src := `
client<llm> MultiLine {
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
	f := mustParse(t, src)
	c := findClient(f, "MultiLine")
	opts := fieldByKey(c.Fields, "options")
	strat := fieldByKey(opts.Block.Fields, "strategy")
	if strat.Value.List == nil {
		t.Fatalf("strategy not a list")
	}
	if len(strat.Value.List) != 3 {
		t.Errorf("got %d elems, want 3", len(strat.Value.List))
	}
}

func TestParse_StrategyListWhitespaceSeparated(t *testing.T) {
	src := `
client<llm> WS {
    provider baml-fallback
    options { strategy [A B C] }
}
`
	f := mustParse(t, src)
	c := findClient(f, "WS")
	opts := fieldByKey(c.Fields, "options")
	strat := fieldByKey(opts.Block.Fields, "strategy")
	if strat.Value.List == nil || len(strat.Value.List) != 3 {
		t.Fatalf("got %+v, want 3-element list", strat.Value)
	}
}

func TestParse_InlineCommentAfterValue(t *testing.T) {
	src := `client<llm> Cmt { provider openai // default
                          retry_policy Fast }`
	f := mustParse(t, src)
	c := findClient(f, "Cmt")
	if c == nil {
		t.Fatalf("client missing")
	}
	if v := fieldByKey(c.Fields, "provider"); v == nil || *v.Value.Ident != "openai" {
		t.Errorf("provider wrong: %+v", v)
	}
	if v := fieldByKey(c.Fields, "retry_policy"); v == nil || *v.Value.Ident != "Fast" {
		t.Errorf("retry_policy wrong: %+v", v)
	}
}

func TestParse_StringWithEmbeddedURL(t *testing.T) {
	src := `client<llm> URL { provider "https://example.com/v1" }`
	f := mustParse(t, src)
	c := findClient(f, "URL")
	p := fieldByKey(c.Fields, "provider")
	if !p.Value.IsLiteral() {
		t.Fatalf("expected literal")
	}
	got, _ := p.Value.LiteralValue()
	if got != "https://example.com/v1" {
		t.Errorf("got %q", got)
	}
}

func TestParse_RetryPolicyMissingFieldsZeroed(t *testing.T) {
	src := `retry_policy Empty { }`
	f := mustParse(t, src)
	rp := findRetryPolicy(f, "Empty")
	if rp == nil {
		t.Fatalf("Empty retry_policy missing")
	}
	if len(rp.Fields) != 0 {
		t.Errorf("expected 0 fields, got %+v", rp.Fields)
	}
}

func TestParse_FieldOrderPreserved(t *testing.T) {
	src := `
client<llm> Ordered {
    provider openai
    provider anthropic
    retry_policy A
}
`
	f := mustParse(t, src)
	c := findClient(f, "Ordered")
	if len(c.Fields) != 3 {
		t.Fatalf("want 3 fields, got %d: %+v", len(c.Fields), c.Fields)
	}
	if *c.Fields[0].Value.Ident != "openai" {
		t.Errorf("Fields[0] = %q, want openai", *c.Fields[0].Value.Ident)
	}
	if *c.Fields[1].Value.Ident != "anthropic" {
		t.Errorf("Fields[1] = %q, want anthropic", *c.Fields[1].Value.Ident)
	}
}

func TestParse_TopLevelGarbageDoesNotLoop(t *testing.T) {
	// Defensive: leading garbage tokens at file scope should not cause
	// an infinite loop. Real .baml files don't produce this shape but
	// the parser must remain robust.
	src := `} ] ) generator g { version "1.0.0" }`
	f := mustParse(t, src)
	if findGenerator(f, "g") == nil {
		t.Errorf("generator g lost after leading garbage; items: %+v", f.Items)
	}
}

func TestParse_HyphenAndSlashIdentifiers(t *testing.T) {
	src := `
client<llm> A {
    provider baml-fallback
}
function F(x: string) -> string {
    client "openai/gpt-4o"
}
`
	f := mustParse(t, src)
	a := findClient(f, "A")
	if *a.Fields[0].Value.Ident != "baml-fallback" {
		t.Errorf("expected baml-fallback ident, got %q", *a.Fields[0].Value.Ident)
	}
}

// TestParse_MalformedNestedTokenAtStartSkipped pins that an un-Field-shaped
// token at the start of a brace body is skipped and the subsequent fields
// parse normally — mirroring the prior hand-rolled parseField's
// non-ident-skip-and-continue behaviour. Caught by Codex's sign-off on
// PR #270: the previous declarative `"{" @@* "}"?` Block grammar
// terminated the block at `@` and leaked the rest of the body to the outer
// scope.
func TestParse_MalformedNestedTokenAtStartSkipped(t *testing.T) {
	src := `client<llm> X { @ provider openai }`
	f := mustParse(t, src)
	c := findClient(f, "X")
	if c == nil {
		t.Fatalf("client X missing; items: %+v", f.Items)
	}
	if len(c.Fields) != 1 {
		t.Fatalf("want 1 field, got %d: %+v", len(c.Fields), c.Fields)
	}
	prov := fieldByKey(c.Fields, "provider")
	if prov == nil || prov.Value == nil || prov.Value.Ident == nil || *prov.Value.Ident != "openai" {
		t.Errorf("provider missing/wrong: %+v", prov)
	}
}

// TestParse_MalformedNestedTokenBetweenFieldsSkipped pins that a garbage
// token between two well-formed fields does not terminate the block — the
// `@` is skipped and `retry_policy Fast` is captured as the second field.
//
// Note: this is intentionally a standalone parser test rather than a parity
// case because the production line-based parser's splitInlineStatements
// includes the trailing `@` in the preceding field's value (so its
// clientProvider["X"] is "openai @", not "openai"). The bamlparser
// AST walker drops stray non-Ident tokens, matching the prior PR #269
// standalone bamlparser. That divergence between line-based and AST
// parsers on garbage input pre-dates PR 1.5 and is out of scope for
// the parity invariant.
func TestParse_MalformedNestedTokenBetweenFieldsSkipped(t *testing.T) {
	src := `client<llm> X { provider openai @ retry_policy Fast }`
	f := mustParse(t, src)
	c := findClient(f, "X")
	if c == nil {
		t.Fatalf("client X missing; items: %+v", f.Items)
	}
	if len(c.Fields) != 2 {
		t.Fatalf("want 2 fields, got %d: %+v", len(c.Fields), c.Fields)
	}
	prov := fieldByKey(c.Fields, "provider")
	if prov == nil || prov.Value == nil || prov.Value.Ident == nil || *prov.Value.Ident != "openai" {
		t.Errorf("provider missing/wrong: %+v", prov)
	}
	rp := fieldByKey(c.Fields, "retry_policy")
	if rp == nil || rp.Value == nil || rp.Value.Ident == nil || *rp.Value.Ident != "Fast" {
		t.Errorf("retry_policy missing/wrong: %+v", rp)
	}
	// Crucially: nothing leaks to top level. The previous declarative
	// `@@* "}"?` Block grammar produced a spurious top-level Item from
	// the leaked `retry_policy Fast` tokens.
	for _, it := range f.Items {
		if it.RetryPolicy != nil {
			t.Errorf("retry_policy leaked to top level: %+v", it.RetryPolicy)
		}
	}
}

// TestParse_MalformedNestedTokenSequenceSkipped pins that consecutive
// garbage tokens (`@ !`) between two fields are all skipped — the loop's
// "advance one token and re-try Field" invariant must hold even when the
// stray-token run is longer than one.
func TestParse_MalformedNestedTokenSequenceSkipped(t *testing.T) {
	src := `client<llm> X { provider openai @ ! retry_policy Fast }`
	f := mustParse(t, src)
	c := findClient(f, "X")
	if c == nil {
		t.Fatalf("client X missing; items: %+v", f.Items)
	}
	if len(c.Fields) != 2 {
		t.Fatalf("want 2 fields, got %d: %+v", len(c.Fields), c.Fields)
	}
	prov := fieldByKey(c.Fields, "provider")
	if prov == nil || prov.Value == nil || prov.Value.Ident == nil || *prov.Value.Ident != "openai" {
		t.Errorf("provider missing/wrong: %+v", prov)
	}
	rp := fieldByKey(c.Fields, "retry_policy")
	if rp == nil || rp.Value == nil || rp.Value.Ident == nil || *rp.Value.Ident != "Fast" {
		t.Errorf("retry_policy missing/wrong: %+v", rp)
	}
}
