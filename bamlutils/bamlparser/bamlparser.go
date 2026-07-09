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
// declaratively via struct tags on the AST nodes; nine irregular shapes
// (GeneratorBlock, ClientBlock, FunctionBlock, RetryPolicyBlock,
// TemplateBlock, TypeBlock, TypeAlias, Other, Block) supply narrow
// Parseable hooks for behaviour that grammar tags cannot express cleanly
// (field-order mismatch, same-line `(` requirement after `function`,
// metadata-only body skipping, catch-all token consumption, and the
// garbage-token-skip-inside-block loop that all four block-body-bearing
// keyword nodes share via parseBlockFieldsUntilClose). Post-parse
// normalisation strips string / env-ref / raw-string delimiters so the
// AST contract matches the prior hand-rolled parser byte-for-byte.
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
//
// The RawString rule matches BAML's 1-to-5-hash raw-string delimiters
// (`#"..."#` through `#####"..."#####`), tried longest-first so a shorter
// delimiter cannot truncate a longer one. Widening from the previous
// single-hash-only rule is needed because constraint expressions and prompt
// bodies use multi-hash delimiters to embed `"#` sequences verbatim (BAML's
// string-literal grammar, datamodel.pest:198+). RE2 has no backreferences,
// so the five hash counts are spelled out as ordered alternatives; the
// content class stays non-greedy so it stops at the first matching closer.
// The punctuation class already covers every character the type grammar
// needs (`<>`, `[]`, `.`, `?`, `|`, `@`, `,`, `:`), and dotted attribute
// names (`stream.done`) are assembled in-grammar from Ident + `.` tokens,
// so no additional punctuation rule is required for slice-1 type parsing.
var bamlLexer = lexer.MustSimple([]lexer.SimpleRule{
	{Name: tokComment, Pattern: `//[^\n]*`},
	{Name: tokWS, Pattern: `\s+`},
	{Name: tokRawString, Pattern: `#####"[\s\S]*?"#####|####"[\s\S]*?"####|###"[\s\S]*?"###|##"[\s\S]*?"##|#"[\s\S]*?"#`},
	{Name: tokString, Pattern: `"(?:\\.|[^"\\])*"`},
	{Name: tokEnvRef, Pattern: `env\.[A-Za-z_][A-Za-z0-9_]*`},
	{Name: tokNumber, Pattern: `-?\d+(?:\.\d+)?`},
	{Name: tokArrow, Pattern: `->`},
	{Name: tokIdent, Pattern: `[A-Za-z_][A-Za-z0-9_\-/]*`},
	{Name: tokPunct, Pattern: `[{}()\[\]<>,.:?=|&!*@;]`},
	{Name: tokOther, Pattern: `.`},
})

// bamlParser is the participle-built parser that drives the File grammar.
// Grammar tags on File, Item, Field, and Value declare the parts of the
// surface participle can handle directly. Body-bearing block types
// (Block, GeneratorBlock, RetryPolicyBlock, ClientBlock, FunctionBlock,
// TemplateBlock, TypeBlock, TypeAlias, Other) are Parseable hooks
// instead, because field-body iteration needs to leniently skip
// un-Field-shaped tokens and metadata-only nodes need balanced-skip
// over content the grammar otherwise can't express.
var bamlParser = participle.MustBuild[File](
	participle.Lexer(bamlLexer),
	participle.Elide(tokComment, tokWS),
)

// blockSubParser parses just a Block body. The Parseable hooks for
// ClientBlock and FunctionBlock delegate brace-body parsing to this
// sub-parser, which in turn invokes Block.Parse (since Block is itself
// Parseable). The indirection keeps brace-body parsing routed through a
// single Block.Parse implementation for client/function/nested-Field
// bodies. GeneratorBlock and RetryPolicyBlock call parseBlockFieldsUntilClose
// directly rather than going through blockSubParser — both routes converge
// on the same skip-garbage helper regardless.
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

// identType / punctType / rawStringType / etc. cache the token type IDs for
// the kinds the Parseable hooks and type-grammar helpers inspect. Looked up
// once at init via the lexer's Symbols() table.
var (
	identType     = bamlLexer.Symbols()[tokIdent]
	punctType     = bamlLexer.Symbols()[tokPunct]
	rawStringType = bamlLexer.Symbols()[tokRawString]
	stringType    = bamlLexer.Symbols()[tokString]
	numberType    = bamlLexer.Symbols()[tokNumber]
	envRefType    = bamlLexer.Symbols()[tokEnvRef]
	arrowType     = bamlLexer.Symbols()[tokArrow]
)

// ParseBytes parses a .baml document from a byte slice. filename is used for
// error messages only — it does not need to refer to an existing file.
func ParseBytes(filename string, data []byte) (*File, error) {
	f, err := bamlParser.ParseBytes(filename, data)
	if err != nil {
		return nil, err
	}
	normalizeFile(f, data)
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

// Parse implements participle.Parseable for GeneratorBlock. Matches the
// literal `generator` token, the required name (Ident), and an optional
// brace body whose fields are iterated via parseBlockFieldsUntilClose —
// the same helper Block.Parse uses, so a top-level generator body
// tolerates un-Field-shaped tokens with the same skip-and-continue
// semantics. A pure `( "{" @@* "}"? )?` grammar terminated the body at
// the first non-Ident token and leaked the rest to file scope.
func (g *GeneratorBlock) Parse(lex *lexer.PeekingLexer) error {
	if !peekIdentLiteral(lex, "generator") {
		return participle.NextMatch
	}
	lex.Next() // consume "generator"

	if t := lex.Peek(); t.Type == identType {
		g.Name = lex.Next().Value
	}
	if peekPunct(lex, "{") {
		lex.Next()
		g.Fields = parseBlockFieldsUntilClose(lex)
	}
	return nil
}

// Parse implements participle.Parseable for RetryPolicyBlock. Mirrors
// GeneratorBlock.Parse — body iteration goes through
// parseBlockFieldsUntilClose so the skip-garbage-token semantics match
// the prior parser exactly.
func (r *RetryPolicyBlock) Parse(lex *lexer.PeekingLexer) error {
	if !peekIdentLiteral(lex, "retry_policy") {
		return participle.NextMatch
	}
	lex.Next() // consume "retry_policy"

	if t := lex.Peek(); t.Type == identType {
		r.Name = lex.Next().Value
	}
	if peekPunct(lex, "{") {
		lex.Next()
		r.Fields = parseBlockFieldsUntilClose(lex)
	}
	return nil
}

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

	// Gate (unchanged config-parity contract): a FunctionBlock requires a
	// `(` on the same source line as the `function` keyword before the body
	// `{`. Declarations without it drop to Item.Other, matching the
	// introspect line-walker's posture. Determine the gate by scanning to
	// `{`/EOF from a checkpoint, then rewind to parse the signature precisely
	// so Params/Return can be captured without changing the gate behaviour.
	cp := lex.MakeCheckpoint()
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
	lex.LoadCheckpoint(cp)

	// Parse the signature: `( named-args )` then optional `-> return-type`.
	// The named-argument list and return type feed the P3 type-system AST;
	// the brace body's Fields (consumed below) are unchanged. Any tokens
	// between the signature and the body that we don't recognise are skipped
	// defensively so the body still parses (laxer-than-BAML; #586).
	if peekPunct(lex, "(") {
		fn.Params = parseParamList(lex)
	}
	if lex.Peek().Type == arrowType {
		lex.Next() // consume "->"
		fn.Return = parseTypeChainFromLexer(lex)
	}
	for {
		t := lex.Peek()
		if t.EOF() || isPunctTok(t, "{") {
			break
		}
		lex.Next()
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
// `(class | enum | test) Name? named_args? { ... }`. For class/enum the body
// is parsed into members + block attributes; for test the body is
// balanced-skipped (tests are not schema definitions), preserving the prior
// metadata-only behaviour. In all cases the block is consumed up to and
// including its matching `}`, so the token stream after the block is
// identical to the prior skip-only parser — keeping config-golden parsing of
// surrounding items unchanged.
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
	kwTok := lex.Next()
	tb.Keyword = kwTok.Value

	if t := lex.Peek(); t.Type == identType {
		tb.Name = lex.Next().Value
	}
	// Optional named-argument list: `class Foo(x: int) { ... }`. Parsed for
	// class/enum only (tests have no arg list); retained but not yet
	// consumed for schema build.
	if tb.Keyword != "test" && peekPunct(lex, "(") {
		tb.Args = parseParamList(lex)
	}
	if !peekPunct(lex, "{") {
		return nil
	}
	if tb.Keyword == "test" {
		skipBalanced(lex, "{", "}")
		return nil
	}
	lex.Next() // consume "{"
	parseTypeMembersUntilClose(lex, tb)
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

	// `type Name = <field_type_with_attr>`. When the `=` is present, parse
	// the RHS as a type expression; on parse failure fall back to the prior
	// line-position-terminated scan so a RHS the type grammar cannot model
	// does not derail parsing of the items that follow.
	if peekPunct(lex, "=") {
		lex.Next() // consume "="
		cp := lex.MakeCheckpoint()
		if te := parseTypeChainFromLexer(lex); te != nil && lex.RawCursor() != cp.RawCursor() {
			ta.Expr = te
			// Alias-level attributes (BAML permits `@check`/`@assert` on
			// aliases). Trailing attributes fold onto the RHS type via the
			// type grammar; any remaining field attributes attach here.
			for isPunctTok(lex.Peek(), "@") && !peekDoubleAt(lex) {
				a := &Attribute{}
				if err := a.Parse(lex); err != nil {
					break
				}
				ta.Attributes = append(ta.Attributes, a)
			}
			return nil
		}
		lex.LoadCheckpoint(cp)
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

// Parse implements participle.Parseable for Block. Matches the opening
// `{`, delegates body iteration to parseBlockFieldsUntilClose, and returns
// NextMatch when the next token isn't `{` so the outer alternation can try
// other arms. Block.Parse itself is intentionally thin: all the
// skip-garbage-token semantics live in the shared helper so GeneratorBlock
// and RetryPolicyBlock get the same behaviour for their top-level bodies.
func (b *Block) Parse(lex *lexer.PeekingLexer) error {
	if !peekPunct(lex, "{") {
		return participle.NextMatch
	}
	lex.Next() // consume "{"
	b.Fields = parseBlockFieldsUntilClose(lex)
	return nil
}

// parseBlockFieldsUntilClose iterates field declarations from inside an
// already-opened brace body until it reaches the matching `}` or EOF.
// Mirrors the prior hand-rolled parseField's "advance one token, keep
// looping" tolerance for un-Field-shaped tokens — a purely declarative
// `@@* "}"?` grammar terminated the block at the first non-Ident token
// and leaked the rest of the body to the outer scope (caught by Codex
// across both rounds of PR #270 sign-off).
//
// Behaviour invariants preserved from the prior parser:
//
//   - At the matching `}`: consume it and return.
//   - At EOF before `}`: return cleanly without erroring. The explicit
//     brace-eating-comment fixture and the bare-`{ key value` case both
//     rely on this.
//   - Each iteration delegates Field parsing to fieldSubParser so the
//     `@Ident (Value | Block)?` grammar stays declarative on the Field
//     type itself. On Field-NextMatch (un-Field-shaped position), the
//     lexer is rolled back to the pre-attempt checkpoint (so partial
//     consumption doesn't compound with the one-token advance) and the
//     loop advances one token before retrying.
//
// The caller is expected to have already consumed the opening `{`.
func parseBlockFieldsUntilClose(lex *lexer.PeekingLexer) []*Field {
	var fields []*Field
	for {
		t := lex.Peek()
		if t.EOF() {
			return fields
		}
		if isPunctTok(t, "}") {
			lex.Next()
			return fields
		}
		cp := lex.MakeCheckpoint()
		f, err := fieldSubParser.ParseFromLexer(lex, participle.AllowTrailing(true))
		if err != nil || f == nil {
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
		fields = append(fields, f)
	}
}

// -----------------------------------------------------------------------
// Type-system body parsing (P3 slice 1, #586).
// -----------------------------------------------------------------------

// parseParamList parses a BAML named-argument list `( name (: Type)? , ... )`
// from the current lexer position (which must be at `(`). Newlines are
// elided by the lexer, so commas alone delimit parameters; the closing `)`
// is consumed. Each parameter's type is parsed via the declarative type
// grammar. Unrecognised tokens inside the list are skipped defensively.
func parseParamList(lex *lexer.PeekingLexer) []*Param {
	if !peekPunct(lex, "(") {
		return nil
	}
	lex.Next() // consume "("
	var params []*Param
	for {
		t := lex.Peek()
		if t.EOF() {
			return params
		}
		if isPunctTok(t, ")") {
			lex.Next()
			return params
		}
		if isPunctTok(t, ",") {
			lex.Next()
			continue
		}
		if t.Type == identType {
			nameTok := lex.Next()
			p := &Param{Name: nameTok.Value, Span: tokSpan(nameTok)}
			if peekPunct(lex, ":") {
				lex.Next() // consume ":"
				p.Type = parseTypeChainFromLexer(lex)
			}
			params = append(params, p)
			continue
		}
		// Unrecognised token inside the arg list; skip defensively.
		lex.Next()
	}
}

// parseTypeMembersUntilClose iterates class/enum members from inside an
// already-opened brace body until the matching `}` (which it consumes) or
// EOF. It mirrors skipBalanced's end position exactly — every iteration
// consumes a member, a block attribute, a balanced nested `{...}`, or a
// single stray token — so the token stream after the block is identical to
// the prior skip-only behaviour.
func parseTypeMembersUntilClose(lex *lexer.PeekingLexer, tb *TypeBlock) {
	for {
		t := lex.Peek()
		if t.EOF() {
			return
		}
		if isPunctTok(t, "}") {
			lex.Next()
			return
		}
		if isPunctTok(t, "@") {
			// Block attribute `@@name(...)`. A lone field attribute `@...`
			// with no preceding member is skipped defensively.
			if a := parseBlockAttribute(lex); a != nil {
				tb.Attributes = append(tb.Attributes, a)
				continue
			}
			lex.Next()
			continue
		}
		if isPunctTok(t, "{") {
			// Nested brace block that is not a leading-`function` method
			// (e.g. a type_builder block or a stray block). Balanced-skip and
			// flag the block unsupported so later builder slices decline it.
			// LAX-VS-BAML: such blocks are not modelled (#586).
			tb.HasUnsupportedContent = true
			skipBalanced(lex, "{", "}")
			continue
		}
		if t.Type == identType {
			// expr_fn / class method. BAML v0.223's type_expression_contents
			// prioritises expr_fn ahead of type_expression, and expr_fn is
			// spelled with a leading `function` keyword. Detect and DECLINE it
			// BEFORE parseTypeMember runs, so the method header
			// (`function double() -> int`) is never misread into Fields. The
			// body is balanced-skipped and the method name is recorded on
			// Methods; nothing is appended to Fields.
			// LAX-VS-BAML: methods are declined, not modelled (#586).
			if t.Value == "function" {
				declineMethod(lex, tb)
				continue
			}
			cp := lex.MakeCheckpoint()
			m := parseTypeMember(lex)
			if m != nil && lex.RawCursor() != cp.RawCursor() {
				// Fail-closed on a typeless CLASS field. Class fields always
				// carry a type; only enum values omit it (Type == nil). A
				// class member with no parsed type is malformed/unsupported
				// (BAML rejects it at validation) — mark it Unsupported rather
				// than emitting a silent enum-like nil-type field, so later
				// builder slices decline it. Enum values keep Type == nil.
				// LAX-VS-BAML: accepted at parse then declined (#586).
				if tb.Keyword == "class" && m.Type == nil {
					m.Type = &TypeExpr{
						Kind:   KindUnsupported,
						Reason: "class field has no type",
						Span:   m.Span,
					}
					tb.HasUnsupportedContent = true
				}
				tb.Fields = append(tb.Fields, m)
				continue
			}
			// Defensive against a degenerate no-progress parse.
			lex.LoadCheckpoint(cp)
			lex.Next()
			continue
		}
		// Stray punctuation / other token; skip defensively.
		lex.Next()
	}
}

// declineMethod consumes a class/enum method (`expr_fn`) — `function NAME(
// args ) -> ret { body }` — from the current lexer position (which must be
// at the `function` keyword) WITHOUT appending anything to Fields. It records
// the method name on tb.Methods, sets tb.HasUnsupportedContent, and
// balanced-skips the method body. A type never contains `{`, so the first
// `{` after the signature is unambiguously the body opener; if no body is
// found before the block's closing `}` or EOF (malformed input BAML would
// reject), it stops without consuming that `}`. The end position matches the
// prior parser's, so surrounding items still parse identically.
func declineMethod(lex *lexer.PeekingLexer, tb *TypeBlock) {
	lex.Next() // consume "function"
	name := ""
	if t := lex.Peek(); t.Type == identType {
		name = lex.Next().Value
	}
	for {
		t := lex.Peek()
		if t.EOF() || isPunctTok(t, "}") {
			break
		}
		if isPunctTok(t, "{") {
			skipBalanced(lex, "{", "}")
			break
		}
		lex.Next()
	}
	tb.HasUnsupportedContent = true
	if name != "" {
		tb.Methods = append(tb.Methods, name)
	}
}

// parseTypeMember parses one class field or enum value. The caller ensures
// the next token is an identifier (the member name). A type expression is
// parsed only when the next token is on the SAME source line as the name and
// can begin a type — mirroring BAML, where a type_expression's field_type is
// newline-terminated, so an enum value on its own line gets no type while a
// class field (`age int`) does. Field attributes (`@...`, not `@@...`) after
// the name/type attach to the member; a `@@` block attribute is left for the
// enclosing member loop.
func parseTypeMember(lex *lexer.PeekingLexer) *TypeMember {
	nameTok := lex.Next() // identifier (caller-guaranteed)
	m := &TypeMember{Name: nameTok.Value, Span: tokSpan(nameTok)}

	if nt := lex.Peek(); !nt.EOF() && nt.Pos.Line == nameTok.Pos.Line && canStartType(nt) {
		cp := lex.MakeCheckpoint()
		if te := parseTypeChainFromLexer(lex); te != nil && lex.RawCursor() != cp.RawCursor() {
			m.Type = te
		} else {
			lex.LoadCheckpoint(cp)
		}
	}

	for isPunctTok(lex.Peek(), "@") && !peekDoubleAt(lex) {
		a := &Attribute{}
		if err := a.Parse(lex); err != nil {
			break
		}
		m.Attributes = append(m.Attributes, a)
	}
	return m
}

// canStartType reports whether t can begin a type expression: an identifier
// (named type / primitive / map), a quoted string or numeric literal type,
// or a `(` opening a group/tuple/parenthesized type.
func canStartType(t *lexer.Token) bool {
	if t.EOF() {
		return false
	}
	switch t.Type {
	case identType, stringType, numberType:
		return true
	}
	return isPunctTok(t, "(")
}

// peekDoubleAt reports whether the next two tokens are `@` `@` (a block
// attribute), without consuming anything.
func peekDoubleAt(lex *lexer.PeekingLexer) bool {
	cp := lex.MakeCheckpoint()
	if !peekPunct(lex, "@") {
		return false
	}
	lex.Next()
	res := peekPunct(lex, "@")
	lex.LoadCheckpoint(cp)
	return res
}

// tokSpan returns the source Span covering a single token.
func tokSpan(t *lexer.Token) Span {
	return Span{Start: t.Pos.Offset, End: tokenEnd(t), Line: t.Pos.Line, Col: t.Pos.Column}
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
func normalizeFile(f *File, data []byte) {
	for _, item := range f.Items {
		normalizeItem(item, data)
	}
}

func normalizeItem(it *Item, data []byte) {
	switch {
	case it.Generator != nil:
		normalizeFields(it.Generator.Fields)
	case it.Client != nil:
		normalizeFields(it.Client.Fields)
	case it.Function != nil:
		normalizeFields(it.Function.Fields)
		for _, p := range it.Function.Params {
			normalizeTypeExpr(p.Type, data)
		}
		normalizeTypeExpr(it.Function.Return, data)
	case it.RetryPolicy != nil:
		normalizeFields(it.RetryPolicy.Fields)
	case it.TypeBlock != nil:
		for _, m := range it.TypeBlock.Fields {
			normalizeTypeExpr(m.Type, data)
			normalizeAttributes(m.Attributes, data)
		}
		for _, p := range it.TypeBlock.Args {
			normalizeTypeExpr(p.Type, data)
		}
		normalizeAttributes(it.TypeBlock.Attributes, data)
	case it.TypeAlias != nil:
		normalizeTypeExpr(it.TypeAlias.Expr, data)
		normalizeAttributes(it.TypeAlias.Attributes, data)
	}
}

// normalizeTypeExpr recursively fills attribute raw-argument slices and
// normalises structured attribute-argument Values across a type expression
// and all its children.
func normalizeTypeExpr(t *TypeExpr, data []byte) {
	if t == nil {
		return
	}
	normalizeAttributes(t.Attributes, data)
	normalizeTypeExpr(t.Elem, data)
	normalizeTypeExpr(t.Key, data)
	normalizeTypeExpr(t.Value, data)
	normalizeTypeExpr(t.Inner, data)
	for _, v := range t.Variants {
		normalizeTypeExpr(v, data)
	}
	for _, i := range t.Items {
		normalizeTypeExpr(i, data)
	}
}

// normalizeAttributes resolves each attribute's raw-argument source slice
// (captured as byte offsets during parse — the Parseable hook only sees the
// lexer, not the source) and normalises its best-effort structured Values so
// they follow the same delimiter-stripped contract as config Values.
func normalizeAttributes(attrs []*Attribute, data []byte) {
	for _, a := range attrs {
		if a.HasParens && a.argStart >= 0 && a.argEnd >= a.argStart && a.argEnd <= len(data) {
			a.RawArgs = string(data[a.argStart:a.argEnd])
		}
		for _, v := range a.Args {
			normalizeValue(v)
		}
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

// stripRawString removes the raw-string delimiters, returning the content
// verbatim. It handles BAML's 1-to-5-hash delimiters: the opener is N `#`
// then `"`, the closer is `"` then N `#`, so N+1 bytes are trimmed from
// each end. Falls back to returning s unchanged if the shape is unexpected.
func stripRawString(s string) string {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n >= len(s) || s[n] != '"' {
		return s
	}
	// Trim N hashes + 1 quote from the front and 1 quote + N hashes from the
	// back. Guard against a token too short to contain both delimiters.
	if len(s) >= 2*(n+1) {
		return s[n+1 : len(s)-(n+1)]
	}
	return s
}
