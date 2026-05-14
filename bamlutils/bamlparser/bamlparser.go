// Package bamlparser provides a shared parser for the .baml configuration
// surface that baml-rest consumes. It produces an AST from .baml source and
// is intentionally permissive about top-level constructs it doesn't model
// semantically: class/enum/test declarations are represented as TypeBlock,
// type aliases as TypeAlias, template_string declarations as TemplateBlock,
// and unknown leading-identifier or leading-punctuation forms as Other.
// These metadata-only nodes preserve source order while their bodies
// (brace bodies, expression right-hand-sides) are consumed and not
// retained, so the parser tolerates the full upstream BAML surface
// without erroring.
//
// The Value type encodes literal-vs-env.IDENT provenance at parse time as a
// first-class node. Downstream code reads Value.Literal vs Value.EnvRef to
// decide whether a field was declared inline or as an env-variable reference
// — preserving that distinction independently of runtime resolution
// (matching the IsSet() declared-presence semantics in cmd/introspect).
//
// The parser does NOT canonicalise identifiers (e.g. provider aliases) or
// unescape string literals. Quoted strings have their surrounding quotes
// stripped but no escape processing is applied, mirroring the existing
// introspect helpers; semantic consumers apply their own normalisation when
// converting AST values to concrete config.
//
// Implementation: a participle-built parser drives the top-level grammar
// declaratively via struct tags on the AST nodes; seven irregular shapes
// (ClientBlock, FunctionBlock, TemplateBlock, TypeBlock, TypeAlias, Other,
// Block) supply narrow Parseable hooks for behaviour that grammar tags
// cannot express cleanly (field-order mismatch, same-line `(` requirement
// after `function`, metadata-only body skipping, catch-all token
// consumption, garbage-token-skip-inside-block). Post-parse normalisation
// strips string / env-ref / raw-string delimiters so the AST contract
// matches the prior hand-rolled parser byte-for-byte.
package bamlparser

import (
	"io"
	"os"
	"strings"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// Token kinds emitted by the lexer. Comment and Whitespace tokens are
// produced for completeness and elided by the parser before grammar
// dispatch — keeping them in the lexer rules preserves position information
// for tokens that follow them.
const (
	tokComment   = "Comment"
	tokWS        = "Whitespace"
	tokRawString = "RawString"
	tokString    = "String"
	tokEnvRef    = "EnvRef"
	tokNumber    = "Number"
	tokIdent     = "Ident"
	tokArrow     = "Arrow"
	tokPunct     = "Punct"
	tokOther     = "Other"
)

// bamlLexer recognises the token shapes the BAML surface uses. Rule order
// matters: longer / more specific patterns (RawString, EnvRef, Arrow) come
// before their would-be Ident / Punct fall-throughs so the lexer picks the
// intended kind. The Other rule catches any single character that no other
// rule consumed; it lets the parser walk past unrecognised punctuation
// inside opaque Other blocks without aborting.
var bamlLexer = lexer.MustSimple([]lexer.SimpleRule{
	{Name: tokComment, Pattern: `//[^\n]*`},
	{Name: tokWS, Pattern: `\s+`},
	{Name: tokRawString, Pattern: `#"[\s\S]*?"#`},
	{Name: tokString, Pattern: `"(?:\\.|[^"\\])*"`},
	{Name: tokEnvRef, Pattern: `env\.[A-Za-z_][A-Za-z0-9_]*`},
	{Name: tokNumber, Pattern: `-?\d+(?:\.\d+)?`},
	{Name: tokArrow, Pattern: `->`},
	{Name: tokIdent, Pattern: `[A-Za-z_][A-Za-z0-9_\-/]*`},
	{Name: tokPunct, Pattern: `[{}()\[\]<>,.:?=|&!*@;]`},
	{Name: tokOther, Pattern: `.`},
})

// bamlParser is the participle-built parser that drives the File grammar.
// Grammar tags on File / Item / GeneratorBlock / RetryPolicyBlock / Block /
// Field / Value declare the parts of the surface participle can handle
// directly; the Parseable hooks defined below cover the rest.
var bamlParser = participle.MustBuild[File](
	participle.Lexer(bamlLexer),
	participle.Elide(tokComment, tokWS),
)

// blockSubParser parses just a Block body. The Parseable hooks for
// ClientBlock and FunctionBlock delegate brace-body parsing to this
// sub-parser, which in turn invokes Block.Parse (since Block is itself
// Parseable). The indirection keeps brace-body parsing routed through a
// single Block.Parse implementation regardless of whether the body is
// being parsed from a top-level keyword block or a nested Field.Block.
var blockSubParser = participle.MustBuild[Block](
	participle.Lexer(bamlLexer),
	participle.Elide(tokComment, tokWS),
)

// fieldSubParser parses a single Field. Block.Parse uses it to consume one
// Field at a time so the inside-block grammar (`Field = @Ident (Value |
// Block)?` on the AST type) stays declarative, while Block.Parse retains
// imperative control over what happens between fields — specifically, the
// "skip an un-Field-shaped token and keep going" behaviour that the prior
// hand-rolled parser provided and that the previous declarative `@@*`
// grammar lost.
var fieldSubParser = participle.MustBuild[Field](
	participle.Lexer(bamlLexer),
	participle.Elide(tokComment, tokWS),
)

// identType / punctType / rawStringType cache the token type IDs for the
// kinds the Parseable hooks inspect. Looked up once at init via the lexer's
// Symbols() table.
var (
	identType     = bamlLexer.Symbols()[tokIdent]
	punctType     = bamlLexer.Symbols()[tokPunct]
	rawStringType = bamlLexer.Symbols()[tokRawString]
)

// ParseBytes parses a .baml document from a byte slice. filename is used for
// error messages only — it does not need to refer to an existing file.
func ParseBytes(filename string, data []byte) (*File, error) {
	f, err := bamlParser.ParseBytes(filename, data)
	if err != nil {
		return nil, err
	}
	normalizeFile(f)
	return f, nil
}

// ParseString is a convenience wrapper around ParseBytes.
func ParseString(filename, content string) (*File, error) {
	return ParseBytes(filename, []byte(content))
}

// ParseReader parses a .baml document from an io.Reader. The reader is
// drained fully before tokenisation begins.
func ParseReader(filename string, r io.Reader) (*File, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return ParseBytes(filename, data)
}

// ParseFile reads path from disk and parses it.
func ParseFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseBytes(path, data)
}

// -----------------------------------------------------------------------
// Parseable hooks for the irregular nodes.
// -----------------------------------------------------------------------

// Parse implements participle.Parseable for ClientBlock. The grammar order
// is `client <TypeParam>? Name { Fields }`, but the struct's field order is
// `Name, TypeParam, Fields`; pure grammar tags would force a public field
// reorder. The hook matches the literal `client` token, the optional
// `< Ident >` type parameter, the required name, and an optional brace
// body parsed via blockSubParser.
func (c *ClientBlock) Parse(lex *lexer.PeekingLexer) error {
	if !peekIdentLiteral(lex, "client") {
		return participle.NextMatch
	}
	lex.Next() // consume "client"

	if peekPunct(lex, "<") {
		lex.Next()
		if t := lex.Peek(); t.Type == identType {
			c.TypeParam = lex.Next().Value
		}
		if peekPunct(lex, ">") {
			lex.Next()
		}
	}

	if t := lex.Peek(); t.Type == identType {
		c.Name = lex.Next().Value
	}

	if peekPunct(lex, "{") {
		blk, err := blockSubParser.ParseFromLexer(lex, participle.AllowTrailing(true))
		if err != nil {
			return err
		}
		c.Fields = blk.Fields
	}
	return nil
}

// Parse implements participle.Parseable for FunctionBlock. Mirrors the prior
// hand-rolled parser's same-line `(` requirement: a function declaration
// captured as FunctionBlock must have `(` on the same source line as the
// `function` keyword. Declarations missing the parenthesised signature drop
// to Item.Other (this hook returns NextMatch so the catch-all consumes the
// keyword + balanced body), matching the introspect line-walker's posture.
func (fn *FunctionBlock) Parse(lex *lexer.PeekingLexer) error {
	if !peekIdentLiteral(lex, "function") {
		return participle.NextMatch
	}
	functionTok := lex.Next() // consume "function"

	name := ""
	if t := lex.Peek(); t.Type == identType {
		name = lex.Next().Value
	}

	// Scan tokens up to (but not consuming) the opening `{` or EOF,
	// recording whether `(` appeared on the same source line as the
	// `function` keyword.
	sawParenSameLine := false
	for {
		t := lex.Peek()
		if t.EOF() || isPunctTok(t, "{") {
			break
		}
		if isPunctTok(t, "(") && t.Pos.Line == functionTok.Pos.Line {
			sawParenSameLine = true
		}
		lex.Next()
	}
	if name == "" || !sawParenSameLine {
		// Unsigned function declaration: return NextMatch so Item.Other
		// catches the leading `function` token and any following balanced
		// body. The branch's lexer state is discarded by participle.
		return participle.NextMatch
	}

	if peekPunct(lex, "{") {
		blk, err := blockSubParser.ParseFromLexer(lex, participle.AllowTrailing(true))
		if err != nil {
			return err
		}
		fn.Fields = blk.Fields
	}
	fn.Name = name
	return nil
}

// Parse implements participle.Parseable for TemplateBlock. Matches
// `template_string Name? (params)? ( #"..."# | { ... } )`. The body is
// metadata-only: only Name is retained.
func (tb *TemplateBlock) Parse(lex *lexer.PeekingLexer) error {
	if !peekIdentLiteral(lex, "template_string") {
		return participle.NextMatch
	}
	lex.Next() // consume "template_string"

	if t := lex.Peek(); t.Type == identType {
		tb.Name = lex.Next().Value
	}
	if peekPunct(lex, "(") {
		skipBalanced(lex, "(", ")")
	}
	if t := lex.Peek(); t.Type == rawStringType {
		lex.Next()
	} else if peekPunct(lex, "{") {
		skipBalanced(lex, "{", "}")
	}
	return nil
}

// Parse implements participle.Parseable for TypeBlock. Matches
// `(class | enum | test) Name? { ... }`. The body is discarded.
func (tb *TypeBlock) Parse(lex *lexer.PeekingLexer) error {
	t := lex.Peek()
	if t.EOF() || t.Type != identType {
		return participle.NextMatch
	}
	switch t.Value {
	case "class", "enum", "test":
	default:
		return participle.NextMatch
	}
	tb.Keyword = lex.Next().Value

	if t := lex.Peek(); t.Type == identType {
		tb.Name = lex.Next().Value
	}
	if peekPunct(lex, "{") {
		skipBalanced(lex, "{", "}")
	}
	return nil
}

// Parse implements participle.Parseable for TypeAlias. Matches
// `type Name? <rhs...>` where the right-hand side runs until EOF or the
// first top-level keyword on a later line — mirroring the prior parser's
// line-position-terminated alias scan exactly.
func (ta *TypeAlias) Parse(lex *lexer.PeekingLexer) error {
	if !peekIdentLiteral(lex, "type") {
		return participle.NextMatch
	}
	startTok := lex.Next() // consume "type"
	startLine := startTok.Pos.Line

	if t := lex.Peek(); t.Type == identType {
		ta.Name = lex.Next().Value
	}
	for {
		t := lex.Peek()
		if t.EOF() {
			break
		}
		if t.Pos.Line != startLine && t.Type == identType && isTopLevelKeyword(t.Value) {
			break
		}
		lex.Next()
	}
	return nil
}

// Parse implements participle.Parseable for Other, the catch-all alternative.
// Consumes exactly one token (capturing its value as Keyword) and, if the
// leading token is an identifier and is immediately followed by `{`, the
// balanced block body that follows. Punctuation-led Others consume just the
// single token — matching the prior parser's behaviour so a stray `}` does
// not gobble a subsequent valid block. Returns NextMatch at EOF so the
// outer Items repetition terminates cleanly.
func (o *Other) Parse(lex *lexer.PeekingLexer) error {
	t := lex.Peek()
	if t.EOF() {
		return participle.NextMatch
	}
	tok := lex.Next()
	o.Keyword = tok.Value
	if tok.Type == identType && peekPunct(lex, "{") {
		skipBalanced(lex, "{", "}")
	}
	return nil
}

// Parse implements participle.Parseable for Block. The block body is parsed
// imperatively so the loop between fields can skip un-Field-shaped tokens
// (matching the prior hand-rolled parseField's "advance one token, keep
// looping" semantics). A purely declarative `"{" @@* "}"?` grammar would
// terminate the block at the first non-Ident token and leak the rest of
// the body to the outer scope — caught by Codex's sign-off on PR #270.
//
// Individual Field parsing stays declarative: each iteration delegates to
// fieldSubParser, so the `@Ident (Value | Block)?` grammar lives on the
// Field type itself. The loop's only responsibility is brace-bounded
// iteration plus garbage-token skip.
//
// Missing-close-brace handling matches the prior parser: reaching EOF
// before `}` is not an error (the explicit brace-eating-comment fixture
// and bare-`{ key value` cases both depend on this).
func (b *Block) Parse(lex *lexer.PeekingLexer) error {
	if !peekPunct(lex, "{") {
		return participle.NextMatch
	}
	lex.Next() // consume "{"
	for {
		t := lex.Peek()
		if t.EOF() {
			return nil
		}
		if isPunctTok(t, "}") {
			lex.Next()
			return nil
		}
		cp := lex.MakeCheckpoint()
		f, err := fieldSubParser.ParseFromLexer(lex, participle.AllowTrailing(true))
		if err != nil || f == nil {
			// Field didn't match here; restore lex to its pre-attempt
			// position so the sub-parser's partial consumption (if any)
			// doesn't compound with the one-token advance below.
			lex.LoadCheckpoint(cp)
			lex.Next()
			continue
		}
		// Defensive against a degenerate sub-parser success that
		// consumed nothing — would otherwise infinite-loop.
		if lex.RawCursor() == cp.RawCursor() {
			lex.Next()
			continue
		}
		b.Fields = append(b.Fields, f)
	}
}

// -----------------------------------------------------------------------
// Helpers shared by Parseable hooks.
// -----------------------------------------------------------------------

// peekIdentLiteral reports whether the next non-elided token is an Ident
// with the given value. Returns false at EOF.
func peekIdentLiteral(lex *lexer.PeekingLexer, value string) bool {
	t := lex.Peek()
	if t.EOF() {
		return false
	}
	return t.Type == identType && t.Value == value
}

// peekPunct reports whether the next non-elided token is a Punct with the
// given value. Returns false at EOF.
func peekPunct(lex *lexer.PeekingLexer, value string) bool {
	t := lex.Peek()
	if t.EOF() {
		return false
	}
	return isPunctTok(t, value)
}

// isPunctTok reports whether t is a Punct token with the given value.
func isPunctTok(t *lexer.Token, value string) bool {
	return t.Type == punctType && t.Value == value
}

// skipBalanced consumes the opener token at the lexer's current position
// (which must match open) and then advances until the matching close token
// is consumed, tracking brace depth. No-op when the next token is not the
// opener.
func skipBalanced(lex *lexer.PeekingLexer, open, close string) {
	if !peekPunct(lex, open) {
		return
	}
	lex.Next() // consume opener
	depth := 1
	for depth > 0 {
		t := lex.Next()
		if t.EOF() {
			return
		}
		if isPunctTok(t, open) {
			depth++
		} else if isPunctTok(t, close) {
			depth--
		}
	}
}

// isTopLevelKeyword reports whether s is one of the keywords that introduces
// a top-level declaration. Used by TypeAlias.Parse to terminate its RHS
// scan when the next line begins with a known top-level construct.
func isTopLevelKeyword(s string) bool {
	switch s {
	case "generator", "client", "function", "retry_policy",
		"template_string", "class", "enum", "test", "type":
		return true
	}
	return false
}

// -----------------------------------------------------------------------
// Post-parse normalisation.
// -----------------------------------------------------------------------

// normalizeFile strips token delimiters from captured scalar values so the
// public AST contract matches the prior hand-rolled parser byte-for-byte:
// String literals retain only the bytes between the quotes, EnvRef values
// drop the leading `env.` prefix, RawString values drop the `#"` / `"#`
// delimiters. No escape processing is applied.
func normalizeFile(f *File) {
	for _, item := range f.Items {
		normalizeItem(item)
	}
}

func normalizeItem(it *Item) {
	switch {
	case it.Generator != nil:
		normalizeFields(it.Generator.Fields)
	case it.Client != nil:
		normalizeFields(it.Client.Fields)
	case it.Function != nil:
		normalizeFields(it.Function.Fields)
	case it.RetryPolicy != nil:
		normalizeFields(it.RetryPolicy.Fields)
	}
}

func normalizeFields(fields []*Field) {
	for _, f := range fields {
		if f.Value != nil {
			normalizeValue(f.Value)
		}
		if f.Block != nil {
			normalizeFields(f.Block.Fields)
		}
	}
}

func normalizeValue(v *Value) {
	if v == nil {
		return
	}
	if v.Literal != nil {
		s := stripStringQuotes(*v.Literal)
		v.Literal = &s
	}
	if v.EnvRef != nil {
		s := strings.TrimPrefix(*v.EnvRef, "env.")
		v.EnvRef = &s
	}
	if v.Raw != nil {
		s := stripRawString(*v.Raw)
		v.Raw = &s
	}
	for _, e := range v.List {
		normalizeValue(e)
	}
}

// stripStringQuotes removes the surrounding double quotes from a String
// token. The lexer guarantees the token starts and ends with `"`. No escape
// processing is applied — the introspect parser's stripBamlQuotes does the
// same, so semantic consumers see byte-identical literal values.
func stripStringQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// stripRawString removes the `#"` opener and `"#` closer from a RawString
// token, returning the content verbatim.
func stripRawString(s string) string {
	if len(s) >= 4 && s[0] == '#' && s[1] == '"' && s[len(s)-2] == '"' && s[len(s)-1] == '#' {
		return s[2 : len(s)-2]
	}
	return s
}
