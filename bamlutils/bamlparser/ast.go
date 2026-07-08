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
// Parsing is implemented via Parseable in bamlparser.go so the body
// shares parseBlockFieldsUntilClose with Block — the prior declarative
// `"{" @@* "}"?` grammar terminated the body at the first non-Ident
// token, regressing the prior hand-rolled parser's "skip garbage,
// keep parsing later fields" semantics. The closing `}` is also
// optional to preserve lenient handling of brace-eating inline
// comments (an inline `// ... }` consumes the trailing `}` because
// the comment lexer rule runs to EOL/EOF).
type GeneratorBlock struct {
	Name   string
	Fields []*Field
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

// FunctionBlock corresponds to `function Name(...) -> Type { ... }`.
//
// Config extraction reads only Name and Fields (the brace-body key/value
// entries). Those are unchanged from the prior parser. As of de-BAML P3
// slice 1 (#586) the parser additionally retains the parsed signature:
// Params holds the named arguments and Return holds the parsed return type
// expression (nil when no `-> Type` is present). These type-system fields
// are not yet consumed downstream; they feed later descriptor-build slices.
//
// Parsing is implemented via Parseable in bamlparser.go because grammar
// tags cannot express the same-line `(` requirement that distinguishes
// signed functions from unsigned function declarations (the latter drop
// to Item.Other to match the introspect line-walker's posture).
type FunctionBlock struct {
	Name   string
	Fields []*Field

	// Params and Return are the parsed signature (P3 slice 1). Return is
	// nil when the declaration omits `-> Type`.
	Params []*Param
	Return *TypeExpr
}

// RetryPolicyBlock corresponds to `retry_policy Name { ... }`.
//
// Parsing is implemented via Parseable in bamlparser.go so the body
// shares parseBlockFieldsUntilClose with Block and GeneratorBlock for
// the skip-garbage-token loop. See GeneratorBlock for the rationale.
type RetryPolicyBlock struct {
	Name   string
	Fields []*Field
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
// `test Name { ... }` declarations. Keyword carries the leading word
// ("class", "enum", or "test") and also serves as the block kind; Name
// carries the declaration name.
//
// As of de-BAML P3 slice 1 (#586) class and enum bodies are parsed instead
// of discarded:
//   - Fields holds the members. For a class each member has a non-nil Type;
//     for an enum each value has Type == nil (matching BAML's grammar where
//     the field_type_chain is optional and enum values omit it).
//   - Args holds a named-argument list (`class Foo(x: int) { ... }`), parsed
//     but not yet used for schema build.
//   - Attributes holds block attributes (`@@dynamic`, `@@assert`, ...).
//   - Methods holds the names of any class methods (`expr_fn`,
//     `function name(...) -> ... { ... }`) declared in the body. BAML's
//     grammar prioritises expr_fn ahead of type_expression, so a method is
//     detected and DECLINED before it can be misread as a member: its body
//     is balanced-skipped and nothing is appended to Fields. Recording the
//     name (rather than a full stub) is enough for later builder slices to
//     decline the block with a useful diagnostic.
//   - HasUnsupportedContent is set when the body contained a shape the
//     slice-1 member parser could not model precisely — a method (see
//     Methods) or any other nested `{ }` block (type_builder, stray blocks),
//     which are balanced-skipped; later builder slices decline such blocks.
//
// `test` blocks are NOT static schema definitions; their body is
// balanced-skipped and Fields/Args/Attributes/Methods are left empty,
// preserving the prior metadata-only behaviour for tests.
//
// Parsing is implemented via Parseable in bamlparser.go because member
// boundaries are newline-terminated in BAML, but the shared lexer elides
// newlines; the hook uses token line positions to decide where a member's
// type ends and the next member begins — something struct tags cannot
// express. The type expressions themselves are parsed via the declarative
// participle type grammar (typeparse.go).
type TypeBlock struct {
	Keyword    string
	Name       string
	Args       []*Param
	Fields     []*TypeMember
	Attributes []*Attribute
	Methods    []string

	HasUnsupportedContent bool
}

// TypeAlias corresponds to `type Name = expression`. Name carries the alias
// name. As of de-BAML P3 slice 1 (#586) the right-hand side is parsed:
// Expr holds the parsed type expression and Attributes holds any attributes
// attached to the RHS (BAML permits `@check`/`@assert` on aliases). Expr is
// nil when the RHS could not be parsed as a type expression (the hook then
// falls back to the prior line-position-terminated scan so config parsing of
// surrounding items is unaffected).
//
// Parsing is implemented via Parseable in bamlparser.go: the leading
// `type Name =` prefix and the fall-back RHS scan (continue until the next
// line begins with a top-level keyword) are position-sensitive in ways
// grammar tags cannot express without retaining newline tokens; the RHS type
// expression itself is parsed via the declarative participle type grammar.
type TypeAlias struct {
	Name       string
	Expr       *TypeExpr
	Attributes []*Attribute
}

// Other captures metadata for a top-level construct that doesn't match any
// recognised declaration shape. Keyword is the leading token's value —
// either the identifier text when the item began with a word, or the raw
// token value when it began with punctuation. If the leading token is an
// identifier AND the very next token is `{`, the balanced block that
// follows is consumed and not retained. Punctuation-led tokens (a stray
// `}`, `]`, `)`, `@`, etc.) consume just the single token so an unrelated
// trailing block is not accidentally swallowed. Other is metadata-only so
// semantic walkers can skip unknown declarations while preserving source
// order, mirroring the line-by-line introspect parser's behaviour of
// ignoring any line that isn't a known prefix.
//
// Parsing is implemented via Parseable in bamlparser.go because Other is
// the catch-all alternative.
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
//     consumed during parsing and not preserved. This grammar is
//     deliberately looser than upstream BAML's, which permits only
//     commas and newlines as separators (see upstream BAML's grammar
//     `splitter` production). The looseness is safe to
//     keep because baml-rest invokes the real BAML compiler against
//     the same source as part of the build, and any bare-whitespace
//     list such as `[A B C]` is rejected upstream as a malformed
//     identifier reference before our walker would observe a
//     divergent element count.
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
