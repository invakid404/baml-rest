package bamlparser

// File is the root of a parsed .baml document. Items appear in source order;
// callers walk Items to find the constructs they care about and ignore the
// rest (Other items capture unknown or unsupported top-level shapes so the
// parser tolerates the full upstream BAML surface without erroring).
type File struct {
	Items []*Item `@@*`
}

// Item is a single top-level declaration. Exactly one of the typed pointers
// is non-nil for any well-formed Item. Items appear in source order so
// duplicate-key semantics (later wins) are preserved end-to-end.
//
// Recognised semantic blocks (Generator, Client, Function, RetryPolicy)
// carry their parsed Fields and are consumed by the semantic walker.
// Tolerated declarations that baml-rest currently doesn't model are
// represented by metadata-only nodes: class/enum/test → TypeBlock, type
// aliases → TypeAlias, template_string → TemplateBlock, and anything else
// → Other. Those nodes carry the declaration keyword/name only; the
// brace body or expression right-hand-side is consumed during parsing
// and not retained on the AST.
//
// The alternative order is load-bearing: keyword-shaped declarations are
// tried first, with Other last as the catch-all so unknown punctuation /
// stray identifiers don't get swallowed by a more specific arm.
type Item struct {
	Generator   *GeneratorBlock   `  @@`
	Client      *ClientBlock      `| @@`
	Function    *FunctionBlock    `| @@`
	RetryPolicy *RetryPolicyBlock `| @@`
	Template    *TemplateBlock    `| @@`
	TypeBlock   *TypeBlock        `| @@`
	TypeAlias   *TypeAlias        `| @@`
	Other       *Other            `| @@`
}

// GeneratorBlock corresponds to `generator Name { key value ... }`.
//
// The closing `}` is optional in the grammar to match the prior parser's
// lenient handling of brace-missing inputs (an inline `// ... }` line
// comment elides the closing brace because the comment lexer rule
// consumes everything up to the next newline, including the trailing `}`).
type GeneratorBlock struct {
	Name   string   `"generator" @Ident`
	Fields []*Field `( "{" @@* "}"? )?`
}

// ClientBlock corresponds to `client[<type>] Name { key value ... }`.
// TypeParam carries the bare identifier between the angle brackets (typically
// "llm"); it is the empty string when the block is written without a type
// parameter. Preserving the raw form lets the introspect-side walker keep its
// current client<llm>-only gate without losing the ability to see plain
// `client Name { ... }` blocks for future widening.
//
// Grammar-tag form would force a public field reorder (the source order is
// `client <TypeParam>? Name`, struct order is `Name, TypeParam, Fields`),
// so the parsing is implemented via Parseable in bamlparser.go.
type ClientBlock struct {
	Name      string
	TypeParam string
	Fields    []*Field
}

// FunctionBlock corresponds to `function Name(...) -> Type { ... }`. Only the
// declared name is preserved; the signature between the parentheses and the
// return type are discarded by the parser because no downstream consumer in
// baml-rest depends on them.
//
// Parsing is implemented via Parseable in bamlparser.go because grammar
// tags cannot express the same-line `(` requirement that distinguishes
// signed functions from unsigned function declarations (the latter drop
// to Item.Other to match the introspect line-walker's posture).
type FunctionBlock struct {
	Name   string
	Fields []*Field
}

// RetryPolicyBlock corresponds to `retry_policy Name { ... }`. The closing
// `}` is optional for the same brace-eating-comment reason described on
// GeneratorBlock.
type RetryPolicyBlock struct {
	Name   string   `"retry_policy" @Ident`
	Fields []*Field `( "{" @@* "}"? )?`
}

// TemplateBlock corresponds to `template_string Name(...) #"..."#` and the
// `template_string Name { ... }` form. The body is intentionally not parsed;
// callers either ignore TemplateBlock entirely (introspect's posture today)
// or read Name for documentation/index purposes.
//
// Parsing is implemented via Parseable in bamlparser.go because the body
// is discarded — there is no AST field to hold the parsed parameter list
// or body — and the body alternates between a raw string token and a
// balanced brace block.
type TemplateBlock struct {
	Name string
}

// TypeBlock corresponds to `class Name { ... }`, `enum Name { ... }`, or
// `test Name { ... }` declarations — leading keyword + name + brace body
// shapes that baml-rest doesn't model semantically. Keyword carries the
// leading word (e.g. "class", "enum", "test") for callers that want to
// differentiate; Name carries the declaration name. The brace body is
// consumed during parsing and not retained; consumers that need a
// verbatim copy must read the source bytes directly.
//
// Parsing is implemented via Parseable in bamlparser.go because the body
// is discarded — there is no AST field to hold the captured fields, so
// declarative tags cannot express the metadata-only contract.
type TypeBlock struct {
	Keyword string
	Name    string
}

// TypeAlias corresponds to `type Name = expression`. Name carries the alias
// name; the right-hand-side expression is consumed during parsing and not
// retained on the AST.
//
// Parsing is implemented via Parseable in bamlparser.go: the right-hand
// side is line-position-terminated (continues until the next line begins
// with a top-level keyword), which grammar tags cannot express without
// keeping newline tokens in the grammar.
type TypeAlias struct {
	Name string
}

// Other captures metadata for a top-level construct that doesn't match any
// recognised declaration shape. Keyword is the leading token's value —
// either the identifier text when the item began with a word, or the raw
// token value when it began with punctuation. Any following balanced block
// is consumed and not retained; Other is metadata-only so semantic walkers
// can skip unknown declarations while preserving source order, mirroring
// the line-by-line introspect parser's behaviour of ignoring any line
// that isn't a known prefix.
//
// Parsing is implemented via Parseable in bamlparser.go because Other is
// the catch-all alternative: it consumes one token plus, if immediately
// followed by `{`, a balanced brace block, with no further structure.
type Other struct {
	Keyword string
}

// Field is one entry inside a block body. Exactly one of Value and Block is
// non-nil:
//   - `key value`         → Value populated, Block nil
//   - `key { ... }`       → Block populated, Value nil
//   - `key [ a, b ]`      → Value populated with Value.List non-nil
//
// The parser does not look up Key against a schema; it captures every key it
// sees so the semantic walker can decide what to do per-block-type.
type Field struct {
	Key   string `@Ident`
	Value *Value `(   @@`
	Block *Block `  | @@ )?`
}

// Block is a brace-delimited body containing zero or more fields. Fields are
// preserved in source order — downstream walkers depend on the order for
// duplicate-key behaviour (later wins, except where the introspect walker
// explicitly clears stale state).
//
// Unknown nested content (raw punctuation between fields, malformed
// statements that don't match the Field shape) is skipped during parsing
// rather than retained on Block; the parser advances past unrecognised
// tokens defensively so a single malformed line cannot abort the whole
// parse. Consumers that need a verbatim copy of an unknown block body
// should read it directly from the source bytes; the AST is designed
// around the recognised-shape contract.
//
// Parsing is implemented via Parseable in bamlparser.go because the
// declarative `"{" @@* "}"?` form terminated the block early on the first
// non-Ident token and let the rest of the block leak to the outer scope,
// regressing the prior parser's "skip garbage, keep parsing later fields"
// semantics.
type Block struct {
	Fields []*Field
}

// Value carries a parsed right-hand-side. Exactly one of the typed pointers
// is non-nil for any well-formed Value:
//
//   - Literal: a quoted string. The pointed-to value carries the bytes
//     between the surrounding double quotes, with the quotes themselves
//     stripped but NO escape processing applied (mirrors the introspect
//     parser's stripBamlQuotes semantics).
//
//   - EnvRef:  a bare env.IDENT reference. The pointed-to value carries
//     just the IDENT portion (the "env." prefix is removed by the parser).
//     Quoted strings of the form "env.NAME" do NOT become EnvRef — only
//     the bare-identifier form does, matching the upstream BAML grammar
//     and preserving the literal-vs-env-reference distinction at parse
//     time.
//
//   - Ident:   an unquoted, non-env-reference identifier (e.g. provider
//     name `openai`, retry policy reference `Fast`, strategy alias
//     `round-robin`). Identifiers may contain hyphens and slashes so
//     shorthand specs like `openai/gpt-4o` and aliases like
//     `aws-bedrock` parse as a single Ident token.
//
//   - Number:  a numeric literal kept as its raw lexeme. The semantic
//     walker chooses int / float / i32 width per field; preserving the
//     raw string lets each consumer apply its own parse rules (in
//     particular extractRoundRobinStart's i32 contract).
//
//   - Raw:     a raw-string literal of the form `#"..."#`. The content
//     between the delimiters is preserved verbatim; raw strings are used
//     for prompts and templates and routinely contain braces and `//`
//     sequences that must not be treated as block / comment markers.
//
//   - List:    a `[ ... ]` literal containing zero or more values. The
//     surrounding brackets are consumed; List is the slice of element
//     values. Element separators (commas and/or whitespace) are
//     consumed during parsing and not preserved.
//
// Map-shape design choice. Upstream BAML's grammar allows a value to be a
// map literal (e.g. config-map expressions inside `options { ... }`).
// This package intentionally does NOT model maps as a distinct Value
// variant. Brace-delimited bodies are represented one level up — as
// Field.Block rather than Value.Map — because every map-shaped consumer
// in baml-rest treats the body as a named child block (options, strategy,
// nested credentials) and benefits from uniform Block iteration. Adding
// a Value.Map variant would force every walker to branch on
// Value.Map-vs-Field.Block for what is, in practice, the same shape.
// Future consumers that need value-position map literals (e.g. on the
// right-hand side of `=`) should extend Value rather than reshape Block.
//
// Grammar tags capture the raw token values (delimiters included); the
// surrounding quotes / raw delimiters / `env.` prefix are stripped by
// normalizeValue after participle returns so the AST contract matches
// the prior hand-rolled parser byte-for-byte.
type Value struct {
	Literal *string  `  @String`
	EnvRef  *string  `| @EnvRef`
	Ident   *string  `| @Ident`
	Number  *string  `| @Number`
	Raw     *string  `| @RawString`
	List    []*Value `| "[" ( @@ ","? )* "]"`
}
