package bamlparser

import (
	"os"
	"path/filepath"
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

func findTemplate(f *File, name string) *TemplateBlock {
	for _, it := range f.Items {
		if it.Template != nil && it.Template.Name == name {
			return it.Template
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
	// Prompt is captured as a Raw value in the ordered Fields ...
	pr := fieldByKey(fn.Fields, "prompt")
	if pr == nil || pr.Value == nil || pr.Value.Raw == nil {
		t.Fatalf("prompt field should carry Raw value: %+v", pr)
	}
	if !strings.Contains(*pr.Value.Raw, "Extract:") {
		t.Errorf("Raw value content unexpected: %q", *pr.Value.Raw)
	}
	// ... and projected onto the dedicated PromptRaw/HasPrompt fields.
	if !fn.HasPrompt {
		t.Errorf("HasPrompt = false, want true")
	}
	if fn.PromptRaw == nil {
		t.Fatalf("PromptRaw nil; want the projected raw prompt")
	}
	if *fn.PromptRaw != "Extract: {{ description }}" {
		t.Errorf("PromptRaw = %q, want %q", *fn.PromptRaw, "Extract: {{ description }}")
	}
	// The projection is exactly the final prompt Field's normalised Raw bytes.
	if fn.PromptRaw != pr.Value.Raw {
		t.Errorf("PromptRaw should alias the final prompt Field's Raw value")
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
	// The dedicated projection carries the identical braces/comments verbatim.
	if !fn.HasPrompt || fn.PromptRaw == nil {
		t.Fatalf("HasPrompt/PromptRaw not set: HasPrompt=%v PromptRaw=%v", fn.HasPrompt, fn.PromptRaw)
	}
	if *fn.PromptRaw != raw {
		t.Errorf("PromptRaw diverged from Field.Raw:\n got %q\nwant %q", *fn.PromptRaw, raw)
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
	// The template declaration retains keyword, name, typed args, and raw body.
	greeting := findTemplate(f, "Greeting")
	if greeting == nil {
		t.Fatalf("template Greeting not captured")
	}
	if greeting.Keyword != "template_string" {
		t.Errorf("Keyword = %q, want template_string", greeting.Keyword)
	}
	if len(greeting.Args) != 1 || greeting.Args[0].Name != "name" {
		t.Fatalf("args = %+v, want single arg name", greeting.Args)
	}
	if greeting.Args[0].Type == nil || renderType(greeting.Args[0].Type) != "string" {
		t.Errorf("arg type = %+v, want string", greeting.Args[0].Type)
	}
	if greeting.Body == nil || *greeting.Body != "Hello {{ name }}" {
		t.Errorf("Body = %v, want %q", greeting.Body, "Hello {{ name }}")
	}
	if greeting.HasUnsupportedBody {
		t.Errorf("HasUnsupportedBody = true for a valid raw body")
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

// TestParse_GeneratorMalformedNestedTokenAtStartSkipped pins the
// skip-garbage-token semantics for GeneratorBlock bodies. The prior
// declarative `( "{" @@* "}"? )?` grammar terminated the body at the
// leading `@` and leaked the rest to file scope. The fix routes the
// generator body through the same parseBlockFieldsUntilClose helper
// that Block / RetryPolicy / Client / Function bodies use.
func TestParse_GeneratorMalformedNestedTokenAtStartSkipped(t *testing.T) {
	src := `generator g { @ version "1.0.0" }`
	f := mustParse(t, src)
	g := findGenerator(f, "g")
	if g == nil {
		t.Fatalf("generator g missing; items: %+v", f.Items)
	}
	if len(g.Fields) != 1 {
		t.Fatalf("want 1 field, got %d: %+v", len(g.Fields), g.Fields)
	}
	ver := fieldByKey(g.Fields, "version")
	if ver == nil || ver.Value == nil || ver.Value.Literal == nil || *ver.Value.Literal != "1.0.0" {
		t.Errorf("version missing/wrong: %+v", ver)
	}
}

// TestParse_GeneratorMalformedNestedTokenBetweenFieldsSkipped pins that a
// garbage token between two well-formed generator fields does not
// terminate the body early.
func TestParse_GeneratorMalformedNestedTokenBetweenFieldsSkipped(t *testing.T) {
	src := `generator g { version "1.0.0" @ output_type "rest/go" }`
	f := mustParse(t, src)
	g := findGenerator(f, "g")
	if g == nil {
		t.Fatalf("generator g missing; items: %+v", f.Items)
	}
	if len(g.Fields) != 2 {
		t.Fatalf("want 2 fields, got %d: %+v", len(g.Fields), g.Fields)
	}
	ver := fieldByKey(g.Fields, "version")
	if ver == nil || ver.Value == nil || ver.Value.Literal == nil || *ver.Value.Literal != "1.0.0" {
		t.Errorf("version missing/wrong: %+v", ver)
	}
	ot := fieldByKey(g.Fields, "output_type")
	if ot == nil || ot.Value == nil || ot.Value.Literal == nil || *ot.Value.Literal != "rest/go" {
		t.Errorf("output_type missing/wrong: %+v", ot)
	}
}

// TestParse_RetryPolicyMalformedNestedTokenAtStartSkipped pins the
// skip-garbage-token semantics for RetryPolicyBlock bodies. Same rationale
// as TestParse_GeneratorMalformedNestedTokenAtStartSkipped.
func TestParse_RetryPolicyMalformedNestedTokenAtStartSkipped(t *testing.T) {
	src := `retry_policy R { @ max_retries 3 }`
	f := mustParse(t, src)
	rp := findRetryPolicy(f, "R")
	if rp == nil {
		t.Fatalf("retry_policy R missing; items: %+v", f.Items)
	}
	if len(rp.Fields) != 1 {
		t.Fatalf("want 1 field, got %d: %+v", len(rp.Fields), rp.Fields)
	}
	mr := fieldByKey(rp.Fields, "max_retries")
	if mr == nil || mr.Value == nil || mr.Value.Number == nil || *mr.Value.Number != "3" {
		t.Errorf("max_retries missing/wrong: %+v", mr)
	}
}

// TestParse_RetryPolicyMalformedNestedTokenBetweenFieldsSkipped pins that
// a garbage token between two well-formed retry_policy fields does not
// terminate the body early.
func TestParse_RetryPolicyMalformedNestedTokenBetweenFieldsSkipped(t *testing.T) {
	src := `retry_policy R { max_retries 3 @ delay_ms 100 }`
	f := mustParse(t, src)
	rp := findRetryPolicy(f, "R")
	if rp == nil {
		t.Fatalf("retry_policy R missing; items: %+v", f.Items)
	}
	if len(rp.Fields) != 2 {
		t.Fatalf("want 2 fields, got %d: %+v", len(rp.Fields), rp.Fields)
	}
	mr := fieldByKey(rp.Fields, "max_retries")
	if mr == nil || mr.Value == nil || mr.Value.Number == nil || *mr.Value.Number != "3" {
		t.Errorf("max_retries missing/wrong: %+v", mr)
	}
	dm := fieldByKey(rp.Fields, "delay_ms")
	if dm == nil || dm.Value == nil || dm.Value.Number == nil || *dm.Value.Number != "100" {
		t.Errorf("delay_ms missing/wrong: %+v", dm)
	}
}

// -----------------------------------------------------------------------
// P1 slice 1 (#586): function prompt projection (PromptRaw / HasPrompt).
// -----------------------------------------------------------------------

func TestParse_PromptLastFieldWins(t *testing.T) {
	// Two prompt fields: last-field-wins for the projection, while the ordered
	// Fields slice retains BOTH in source order (lossless).
	src := `
function F(x: string) -> string {
    client C
    prompt #"first"#
    prompt #"second"#
}
`
	f := mustParse(t, src)
	fn := findFunction(f, "F")
	if fn == nil {
		t.Fatalf("F missing")
	}
	var prompts []string
	for _, fld := range fn.Fields {
		if fld.Key == "prompt" && fld.Value != nil && fld.Value.Raw != nil {
			prompts = append(prompts, *fld.Value.Raw)
		}
	}
	if len(prompts) != 2 || prompts[0] != "first" || prompts[1] != "second" {
		t.Fatalf("ordered Fields lost a prompt: %v", prompts)
	}
	if !fn.HasPrompt || fn.PromptRaw == nil || *fn.PromptRaw != "second" {
		t.Errorf("projection = (%v, %v), want last-wins 'second'", fn.HasPrompt, fn.PromptRaw)
	}
}

func TestParse_PromptFinalNonRawDeclines(t *testing.T) {
	// An earlier raw prompt followed by a final NON-raw prompt: the projection
	// sets HasPrompt but leaves PromptRaw nil (the earlier raw prompt is never
	// resurrected), while Fields retains both source-order entries.
	src := `
function F(x: string) -> string {
    client C
    prompt #"earlier raw"#
    prompt "final literal"
}
`
	f := mustParse(t, src)
	fn := findFunction(f, "F")
	if fn == nil {
		t.Fatalf("F missing")
	}
	if !fn.HasPrompt {
		t.Errorf("HasPrompt = false; a final prompt field is present")
	}
	if fn.PromptRaw != nil {
		t.Errorf("PromptRaw = %q; a final non-raw prompt must not reuse an earlier raw prompt", *fn.PromptRaw)
	}
	var sawEarlierRaw bool
	for _, fld := range fn.Fields {
		if fld.Key == "prompt" && fld.Value != nil && fld.Value.Raw != nil && *fld.Value.Raw == "earlier raw" {
			sawEarlierRaw = true
		}
	}
	if !sawEarlierRaw {
		t.Errorf("ordered Fields lost the earlier raw prompt")
	}
}

func TestParse_PromptAbsent(t *testing.T) {
	src := `
function F(x: string) -> string {
    client C
}
`
	f := mustParse(t, src)
	fn := findFunction(f, "F")
	if fn == nil {
		t.Fatalf("F missing")
	}
	if fn.HasPrompt {
		t.Errorf("HasPrompt = true for a function with no prompt field")
	}
	if fn.PromptRaw != nil {
		t.Errorf("PromptRaw = %q, want nil", *fn.PromptRaw)
	}
}

func TestParse_PromptExactBodyNoTrimNoDedent(t *testing.T) {
	// A raw prompt with CRLF line endings, leading/trailing blank lines, tabs,
	// braces, URLs and `//` double-slashes. The parser retains the body
	// byte-for-byte after removing ONLY the raw-string delimiters: no
	// TrimSpace, dedent, unescape, or comment stripping.
	body := "\r\n" +
		"\t{ \"url\": \"https://example.com//v1\" } // not a comment\r\n" +
		"\t\tindented\r\n" +
		"\r\n"
	src := "function F(x: string) -> string {\n" +
		"    client C\n" +
		"    prompt #\"" + body + "\"#\n" +
		"}\n"
	f := mustParse(t, src)
	fn := findFunction(f, "F")
	if fn == nil {
		t.Fatalf("F missing")
	}
	if fn.PromptRaw == nil {
		t.Fatalf("PromptRaw nil")
	}
	if *fn.PromptRaw != body {
		t.Errorf("PromptRaw not byte-exact:\n got %q\nwant %q", *fn.PromptRaw, body)
	}
}

func TestParse_RealDynamicPromptExactRetention(t *testing.T) {
	// The real cmd/build/dynamic.baml prompt is retained byte-for-byte after
	// removing ONLY the raw-string delimiters — leading/trailing whitespace,
	// {%- / -%}, _.role, every media branch, ctx.output_format, and the replace
	// filter. Skipped when the file is not present (stripped-down checkout).
	path := filepath.Join("..", "..", "cmd", "build", "dynamic.baml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skip("dynamic.baml not available")
	}
	f, err := ParseBytes(path, data)
	if err != nil {
		t.Fatalf("parse dynamic.baml: %v", err)
	}
	fn := findFunction(f, "Baml_Rest_Dynamic")
	if fn == nil {
		t.Fatalf("Baml_Rest_Dynamic function missing")
	}
	if !fn.HasPrompt || fn.PromptRaw == nil {
		t.Fatalf("prompt not projected: HasPrompt=%v PromptRaw=%v", fn.HasPrompt, fn.PromptRaw)
	}
	// Reconstruct the expected body from source: the bytes between `prompt #"`
	// and the closing `"#` (single-hash; the real body contains no `"#`
	// sequence). This proves delimiter-removal-only with zero dedent/trim.
	s := string(data)
	const openMarker = "prompt #\""
	oi := strings.Index(s, openMarker)
	if oi < 0 {
		t.Fatalf("could not locate prompt open marker")
	}
	bodyStart := oi + len(openMarker)
	ci := strings.Index(s[bodyStart:], "\"#")
	if ci < 0 {
		t.Fatalf("could not locate prompt close marker")
	}
	want := s[bodyStart : bodyStart+ci]
	if *fn.PromptRaw != want {
		t.Errorf("PromptRaw not byte-exact:\n got %q\nwant %q", *fn.PromptRaw, want)
	}
	for _, needle := range []string{"{%-", "-%}", "_.role(", "ctx.output_format", `replace("{output_format}"`, "p.img", "p.aud", "p.doc", "p.vid"} {
		if !strings.Contains(*fn.PromptRaw, needle) {
			t.Errorf("PromptRaw lost %q", needle)
		}
	}
	if !strings.HasPrefix(*fn.PromptRaw, "\n") {
		t.Errorf("leading whitespace was trimmed from the prompt body")
	}
	if !strings.HasSuffix(*fn.PromptRaw, "\n  ") {
		t.Errorf("trailing whitespace was trimmed from the prompt body")
	}
}

// -----------------------------------------------------------------------
// P1 slice 1 (#586): template_string / string_template retention.
// -----------------------------------------------------------------------

func TestParse_TemplateStringAliasAndEquals(t *testing.T) {
	// Both keyword spellings, both with and without the optional `=` before the
	// body. All four retain keyword spelling, name, typed arg, and raw body.
	cases := []struct {
		name    string
		src     string
		keyword string
	}{
		{"template_string no equals", `template_string A(x: string) #"hi {{x}}"#`, "template_string"},
		{"template_string equals", `template_string B(x: string) = #"hi {{x}}"#`, "template_string"},
		{"string_template alias", `string_template C(x: string) #"hi {{x}}"#`, "string_template"},
		{"string_template alias equals", `string_template D(x: string) = #"hi {{x}}"#`, "string_template"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := mustParse(t, tc.src)
			var tb *TemplateBlock
			for _, it := range f.Items {
				if it.Template != nil {
					tb = it.Template
				}
			}
			if tb == nil {
				t.Fatalf("template not captured; items: %+v", f.Items)
			}
			if tb.Keyword != tc.keyword {
				t.Errorf("Keyword = %q, want %q", tb.Keyword, tc.keyword)
			}
			if tb.HasUnsupportedBody {
				t.Errorf("HasUnsupportedBody = true for a raw body")
			}
			if tb.Body == nil || *tb.Body != "hi {{x}}" {
				t.Errorf("Body = %v, want %q", tb.Body, "hi {{x}}")
			}
			if len(tb.Args) != 1 || tb.Args[0].Name != "x" || renderType(tb.Args[0].Type) != "string" {
				t.Errorf("Args = %+v, want [x: string]", tb.Args)
			}
		})
	}
}

func TestParse_TemplateBareAndTypedArgs(t *testing.T) {
	// Mixed bare (no `:type`) and typed named args; a bare arg has Type == nil.
	src := `template_string T(a, b: string, c) #"body"#`
	f := mustParse(t, src)
	tb := findTemplate(f, "T")
	if tb == nil {
		t.Fatalf("template T missing")
	}
	if len(tb.Args) != 3 {
		t.Fatalf("want 3 args, got %d: %+v", len(tb.Args), tb.Args)
	}
	if tb.Args[0].Name != "a" || tb.Args[0].Type != nil {
		t.Errorf("arg[0] = %+v, want bare 'a'", tb.Args[0])
	}
	if tb.Args[1].Name != "b" || renderType(tb.Args[1].Type) != "string" {
		t.Errorf("arg[1] = %+v, want 'b: string'", tb.Args[1])
	}
	if tb.Args[2].Name != "c" || tb.Args[2].Type != nil {
		t.Errorf("arg[2] = %+v, want bare 'c'", tb.Args[2])
	}
	if tb.Body == nil || *tb.Body != "body" {
		t.Errorf("Body = %v, want %q", tb.Body, "body")
	}
}

func TestParse_TemplateEmptyBody(t *testing.T) {
	// An empty raw body is a non-nil pointer to the empty string, distinct from
	// a nil (absent / brace-tolerated) body.
	src := `template_string Empty() #""#`
	f := mustParse(t, src)
	tb := findTemplate(f, "Empty")
	if tb == nil {
		t.Fatalf("template Empty missing")
	}
	if tb.Body == nil {
		t.Fatalf("Body nil; an empty raw body must be a non-nil empty string")
	}
	if *tb.Body != "" {
		t.Errorf("Body = %q, want empty string", *tb.Body)
	}
	if tb.HasUnsupportedBody {
		t.Errorf("HasUnsupportedBody = true for a valid (empty) raw body")
	}
}

func TestParse_TemplateMultiHashBody(t *testing.T) {
	// A multi-hash template body retains an embedded `"#` sequence verbatim,
	// with tabs preserved, and the following declaration still parses.
	src := "template_string M() ##\"line\t\"# still inside\"##\nclient<llm> After { provider openai }\n"
	f := mustParse(t, src)
	tb := findTemplate(f, "M")
	if tb == nil {
		t.Fatalf("template M missing")
	}
	if tb.Body == nil || *tb.Body != "line\t\"# still inside" {
		t.Errorf("Body = %v, want %q", tb.Body, "line\t\"# still inside")
	}
	if findClient(f, "After") == nil {
		t.Errorf("client After not found after multi-hash template")
	}
}

func TestParse_TemplateBraceBodyTolerated(t *testing.T) {
	// The historical brace-bodied template form is tolerated for parser
	// recovery but marked HasUnsupportedBody with a nil Body — never fabricated
	// into a string — and the following declaration still parses.
	src := `
template_string Braced(x: string) {
    this is { not } a raw body
}

client<llm> After { provider openai }
`
	f := mustParse(t, src)
	tb := findTemplate(f, "Braced")
	if tb == nil {
		t.Fatalf("template Braced missing")
	}
	if !tb.HasUnsupportedBody {
		t.Errorf("HasUnsupportedBody = false for a brace body")
	}
	if tb.Body != nil {
		t.Errorf("Body = %q; a brace body must not be fabricated into a string", *tb.Body)
	}
	if len(tb.Args) != 1 || tb.Args[0].Name != "x" {
		t.Errorf("Args = %+v, want [x: string]", tb.Args)
	}
	if findClient(f, "After") == nil {
		t.Errorf("client After not found after brace-bodied template")
	}
}

func TestParse_TemplateDollarIdentArg(t *testing.T) {
	// FIX-EVENTUALLY (#586): the widened Ident admits a leading `$`, so a
	// dollar-prefixed argument name lexes as a single Ident token rather than
	// splitting into `$` + name.
	src := `template_string D($input: string) #"{{ $input }}"#`
	f := mustParse(t, src)
	tb := findTemplate(f, "D")
	if tb == nil {
		t.Fatalf("template D missing")
	}
	if len(tb.Args) != 1 || tb.Args[0].Name != "$input" {
		t.Fatalf("Args = %+v, want single arg $input", tb.Args)
	}
}

func TestParse_TemplateSourceOrderAcrossFiles(t *testing.T) {
	// Multiple macros retain File.Items source order within a file; iterating
	// parsed files in order then File.Items in order yields a stable macro
	// sequence — the ordering the descriptor slice relies on, never lexical.
	fileA := `
template_string A1() #"a1"#
template_string A2() #"a2"#
`
	fileB := `
template_string B1() #"b1"#
`
	fa := mustParse(t, fileA)
	fb := mustParse(t, fileB)
	var order []string
	for _, src := range []*File{fa, fb} {
		for _, it := range src.Items {
			if it.Template != nil {
				order = append(order, it.Template.Name)
			}
		}
	}
	want := []string{"A1", "A2", "B1"}
	if len(order) != len(want) {
		t.Fatalf("macro order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("macro order = %v, want %v", order, want)
		}
	}
}
