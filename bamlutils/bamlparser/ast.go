package bamlparser

// File is the root of a parsed .baml document. Items appear in source order;
// callers walk Items to find the constructs they care about and ignore the
// rest (Other items capture unknown or unsupported top-level shapes so the
// parser tolerates the full upstream BAML surface without erroring).
type File struct {
	Items []*Item
}

// Item is a single top-level declaration. Exactly one of the typed pointers
// is non-nil for any well-formed Item. Other is used for tolerated upstream
// constructs the package doesn't model (class, enum, type alias, test, raw
// top-level assignments, etc.) — its body is captured opaquely so it can be
// skipped without leaking into the recognised-block walkers.
type Item struct {
	Generator   *GeneratorBlock
	Client      *ClientBlock
	Function    *FunctionBlock
	RetryPolicy *RetryPolicyBlock
	Template    *TemplateBlock
	TypeBlock   *TypeBlock
	TypeAlias   *TypeAlias
	Other       *Other
}

// GeneratorBlock corresponds to `generator Name { key value ... }`.
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
type ClientBlock struct {
	Name      string
	TypeParam string
	Fields    []*Field
}

// FunctionBlock corresponds to `function Name(...) -> Type { ... }`. Only the
// declared name is preserved; the signature between the parentheses and the
// return type are discarded by the parser because no downstream consumer in
// baml-rest depends on them.
type FunctionBlock struct {
	Name   string
	Fields []*Field
}

// RetryPolicyBlock corresponds to `retry_policy Name { ... }`.
type RetryPolicyBlock struct {
	Name   string
	Fields []*Field
}

// TemplateBlock corresponds to `template_string Name(...) #"..."#` and the
// `template_string Name { ... }` form. The body is intentionally not parsed;
// callers either ignore TemplateBlock entirely (introspect's posture today)
// or read Name for documentation/index purposes.
type TemplateBlock struct {
	Name string
}

// TypeBlock corresponds to `class Name { ... }`, `enum Name { ... }`, and the
// `test Name { ... }` form — anything that has a leading keyword + name +
// block body but isn't one of the recognised value-expression blocks. The
// body is held opaquely so the parser can skip past it without erroring on
// constructs it doesn't model. Keyword carries the leading word (e.g.
// "class", "enum", "test") for callers that want to differentiate.
type TypeBlock struct {
	Keyword string
	Name    string
}

// TypeAlias corresponds to `type Name = expression`. The expression body is
// not parsed; only the alias name is captured.
type TypeAlias struct {
	Name string
}

// Other captures a top-level construct that doesn't match any recognised
// block shape. Keyword is the leading token's value (or empty when the item
// began with something other than an identifier). The parser uses Other to
// stay permissive: unknown top-level syntax is skipped rather than treated
// as an error, mirroring the line-by-line introspect parser's behaviour of
// ignoring any line that isn't a known prefix.
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
	Key   string
	Value *Value
	Block *Block
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
type Value struct {
	Literal *string
	EnvRef  *string
	Ident   *string
	Number  *string
	Raw     *string
	List    []*Value
}
