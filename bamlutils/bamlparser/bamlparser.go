// Package bamlparser provides a shared parser for the .baml configuration
// surface that baml-rest consumes. It produces an AST from .baml source and
// is intentionally permissive about top-level constructs it doesn't model
// (class, enum, type alias, template_string, test, raw top-level
// assignments): such constructs are captured opaquely as Other items so the
// parser tolerates the full upstream BAML surface without erroring.
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
package bamlparser

import (
	"io"
	"os"

	"github.com/alecthomas/participle/v2/lexer"
)

// Token kinds emitted by the lexer. Comment and Whitespace tokens are
// produced for completeness and filtered out by the parser before grammar
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

// ParseBytes parses a .baml document from a byte slice. filename is used for
// error messages only — it does not need to refer to an existing file.
func ParseBytes(filename string, data []byte) (*File, error) {
	tokens, err := tokenize(filename, data)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	return p.parseFile()
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

// tokenize runs the lexer and returns a slice of significant tokens
// (comments and whitespace dropped). Position information is preserved on
// each token via the participle lexer.Token Pos field; the parser uses
// Pos.Line to detect line boundaries when skipping type aliases.
func tokenize(filename string, data []byte) ([]lexer.Token, error) {
	def, err := bamlLexer.LexString(filename, string(data))
	if err != nil {
		return nil, err
	}
	var out []lexer.Token
	for {
		tok, err := def.Next()
		if err != nil {
			return nil, err
		}
		if tok.EOF() {
			out = append(out, tok)
			break
		}
		switch tok.Type {
		case bamlLexer.Symbols()[tokComment],
			bamlLexer.Symbols()[tokWS]:
			continue
		}
		out = append(out, tok)
	}
	return out, nil
}

// parser is a hand-rolled walker over the token stream produced by the
// participle lexer. Hand-rolling the walker (rather than expressing the full
// grammar in participle struct tags) keeps the permissive-skip behaviour for
// unknown top-level constructs simple — participle's strict alternation
// would require modelling every upstream BAML shape to avoid parse errors.
type parser struct {
	tokens []lexer.Token
	pos    int
}

func (p *parser) peek() lexer.Token { return p.tokens[p.pos] }
func (p *parser) advance() lexer.Token {
	t := p.tokens[p.pos]
	if !t.EOF() {
		p.pos++
	}
	return t
}
func (p *parser) eof() bool { return p.peek().EOF() }

func (p *parser) typeOf(name string) lexer.TokenType {
	return bamlLexer.Symbols()[name]
}

func (p *parser) isPunct(t lexer.Token, ch string) bool {
	return t.Type == p.typeOf(tokPunct) && t.Value == ch
}

func (p *parser) parseFile() (*File, error) {
	file := &File{}
	for !p.eof() {
		item, err := p.parseTopLevelItem()
		if err != nil {
			return nil, err
		}
		if item != nil {
			file.Items = append(file.Items, item)
		}
	}
	return file, nil
}

// parseTopLevelItem dispatches based on the first identifier of a top-level
// declaration. Any non-identifier first token (a stray `}` from a malformed
// file, raw punctuation at file scope) is consumed as an Other so the
// parser never gets stuck.
func (p *parser) parseTopLevelItem() (*Item, error) {
	tok := p.peek()
	if tok.EOF() {
		return nil, nil
	}
	if tok.Type != p.typeOf(tokIdent) {
		// Unknown leading token at file scope: consume it and emit
		// Other so the walker advances. Real BAML never produces this
		// shape, but a future syntax extension might.
		p.advance()
		return &Item{Other: &Other{Keyword: tok.Value}}, nil
	}
	keyword := tok.Value
	switch keyword {
	case "generator":
		return p.parseGeneratorItem()
	case "client":
		return p.parseClientItem()
	case "function":
		return p.parseFunctionItem()
	case "retry_policy":
		return p.parseRetryPolicyItem()
	case "template_string":
		return p.parseTemplateItem()
	case "class", "enum", "test":
		return p.parseTypeBlockItem()
	case "type":
		return p.parseTypeAliasItem()
	default:
		// Permissive: unknown top-level identifier. Skip a balanced
		// block if one follows, otherwise scan to the next top-level
		// boundary so we don't loop.
		p.advance()
		p.skipBalancedBlockIfPresent()
		return &Item{Other: &Other{Keyword: keyword}}, nil
	}
}

func (p *parser) parseGeneratorItem() (*Item, error) {
	p.advance() // consume "generator"
	name := p.expectIdentValue()
	fields, err := p.parseBlockBody()
	if err != nil {
		return nil, err
	}
	return &Item{Generator: &GeneratorBlock{Name: name, Fields: fields}}, nil
}

func (p *parser) parseClientItem() (*Item, error) {
	p.advance() // consume "client"
	typeParam := ""
	// Optional type parameter: <ident>
	if p.isPunct(p.peek(), "<") {
		p.advance()
		if p.peek().Type == p.typeOf(tokIdent) {
			typeParam = p.advance().Value
		}
		if p.isPunct(p.peek(), ">") {
			p.advance()
		}
	}
	name := p.expectIdentValue()
	fields, err := p.parseBlockBody()
	if err != nil {
		return nil, err
	}
	return &Item{Client: &ClientBlock{Name: name, TypeParam: typeParam, Fields: fields}}, nil
}

// parseFunctionItem captures the function name and the body. The signature
// (parameter list and return type) between the function name and the body's
// opening brace is consumed without recording: no downstream consumer in
// baml-rest reads it.
//
// A function declaration must contain `(` on the same line as the leading
// `function` keyword for the block to be captured — declarations missing a
// parameter list are skipped (the body is consumed opaquely) so the parser
// stays bug-for-bug compatible with the introspect line-walker's
// extractFunctionName helper, which only recognises a function name when
// the parenthesis appears on the same source line.
func (p *parser) parseFunctionItem() (*Item, error) {
	functionTok := p.advance() // consume "function"
	name := p.expectIdentValue()
	sawParenSameLine := false
	for !p.eof() && !p.isPunct(p.peek(), "{") {
		t := p.peek()
		if t.Pos.Line == functionTok.Pos.Line && p.isPunct(t, "(") {
			sawParenSameLine = true
		}
		p.advance()
	}
	if name == "" || !sawParenSameLine {
		p.skipBalancedBlockIfPresent()
		return &Item{Other: &Other{Keyword: "function"}}, nil
	}
	fields, err := p.parseBlockBody()
	if err != nil {
		return nil, err
	}
	return &Item{Function: &FunctionBlock{Name: name, Fields: fields}}, nil
}

func (p *parser) parseRetryPolicyItem() (*Item, error) {
	p.advance() // consume "retry_policy"
	name := p.expectIdentValue()
	fields, err := p.parseBlockBody()
	if err != nil {
		return nil, err
	}
	return &Item{RetryPolicy: &RetryPolicyBlock{Name: name, Fields: fields}}, nil
}

// parseTemplateItem captures the template's name; its body is intentionally
// not parsed — the body is either a raw-string expression
// (`template_string Name(args) #"..."#`) or a block with arbitrary content
// that no introspect consumer reads.
func (p *parser) parseTemplateItem() (*Item, error) {
	p.advance() // consume "template_string"
	name := ""
	if p.peek().Type == p.typeOf(tokIdent) {
		name = p.advance().Value
	}
	// Skip the optional parameter list `(...)`.
	if p.isPunct(p.peek(), "(") {
		p.skipBalanced("(", ")")
	}
	// The body is either a raw string token (no braces) or a block.
	if p.peek().Type == p.typeOf(tokRawString) {
		p.advance()
	} else if p.isPunct(p.peek(), "{") {
		p.skipBalanced("{", "}")
	}
	return &Item{Template: &TemplateBlock{Name: name}}, nil
}

// parseTypeBlockItem captures `class Name { ... }`, `enum Name { ... }` and
// `test Name { ... }`. The body is skipped — no introspect-side consumer
// reads class/enum/test fields.
func (p *parser) parseTypeBlockItem() (*Item, error) {
	keyword := p.advance().Value
	name := ""
	if p.peek().Type == p.typeOf(tokIdent) {
		name = p.advance().Value
	}
	if p.isPunct(p.peek(), "{") {
		p.skipBalanced("{", "}")
	}
	return &Item{TypeBlock: &TypeBlock{Keyword: keyword, Name: name}}, nil
}

// parseTypeAliasItem captures `type Name = expression`. Type aliases have no
// trailing braces; they're newline-terminated by convention. Skip to the
// start of the next line that begins a top-level construct so we don't
// over-consume the rest of the file.
func (p *parser) parseTypeAliasItem() (*Item, error) {
	startLine := p.peek().Pos.Line
	p.advance() // consume "type"
	name := ""
	if p.peek().Type == p.typeOf(tokIdent) {
		name = p.advance().Value
	}
	// Consume the `=` and the expression that follows. The expression
	// continues until the next top-level identifier on a new line OR
	// end-of-file. Identifiers like `int`, `string`, `map`, etc. inside
	// the expression do not trigger a top-level boundary because they
	// stay on the same line as the `=`.
	for !p.eof() {
		t := p.peek()
		if t.Pos.Line != startLine && t.Type == p.typeOf(tokIdent) && isTopLevelKeyword(t.Value) {
			break
		}
		p.advance()
	}
	return &Item{TypeAlias: &TypeAlias{Name: name}}, nil
}

func isTopLevelKeyword(s string) bool {
	switch s {
	case "generator", "client", "function", "retry_policy",
		"template_string", "class", "enum", "test", "type":
		return true
	}
	return false
}

// expectIdentValue consumes the next token if it is an identifier and
// returns its value; otherwise it returns the empty string without
// advancing. The parser intentionally tolerates missing names so a malformed
// block doesn't abort parsing of the rest of the file.
func (p *parser) expectIdentValue() string {
	if p.peek().Type == p.typeOf(tokIdent) {
		return p.advance().Value
	}
	return ""
}

// parseBlockBody parses a `{ ... }` body. If the next token is not `{`, an
// empty slice is returned and no tokens are consumed — the caller is
// responsible for emitting the relevant AST node.
func (p *parser) parseBlockBody() ([]*Field, error) {
	if !p.isPunct(p.peek(), "{") {
		return nil, nil
	}
	p.advance() // consume "{"
	var fields []*Field
	for !p.eof() && !p.isPunct(p.peek(), "}") {
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		if f != nil {
			fields = append(fields, f)
		}
	}
	if p.isPunct(p.peek(), "}") {
		p.advance()
	}
	return fields, nil
}

// parseField parses one `key value` or `key { ... }` entry. Returns nil
// silently when the next token doesn't begin a recognisable field (e.g.
// stray punctuation), advancing one token so the loop makes progress.
func (p *parser) parseField() (*Field, error) {
	tok := p.peek()
	if tok.Type != p.typeOf(tokIdent) {
		// Unrecognised field-leading token; consume it to make
		// progress without erroring. Real .baml files don't produce
		// this shape inside a recognised block body.
		p.advance()
		return nil, nil
	}
	key := p.advance().Value
	// Block-shaped field: `key { ... }`
	if p.isPunct(p.peek(), "{") {
		nested, err := p.parseBlockBody()
		if err != nil {
			return nil, err
		}
		return &Field{Key: key, Block: &Block{Fields: nested}}, nil
	}
	// Value-shaped field: `key value`.
	v, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	return &Field{Key: key, Value: v}, nil
}

// parseValue parses a single right-hand-side value: literal string, raw
// string, env reference, identifier, number, or list. Returns nil when the
// next token doesn't begin a value (e.g. immediately followed by `}` for a
// keyword-only field like `prompt #"..."#` after a missing inner value);
// the caller decides whether nil is acceptable.
func (p *parser) parseValue() (*Value, error) {
	tok := p.peek()
	switch tok.Type {
	case p.typeOf(tokString):
		p.advance()
		lit := stripStringQuotes(tok.Value)
		return &Value{Literal: &lit}, nil
	case p.typeOf(tokRawString):
		p.advance()
		raw := stripRawString(tok.Value)
		return &Value{Raw: &raw}, nil
	case p.typeOf(tokEnvRef):
		p.advance()
		name := tok.Value[len("env."):]
		return &Value{EnvRef: &name}, nil
	case p.typeOf(tokNumber):
		p.advance()
		num := tok.Value
		return &Value{Number: &num}, nil
	case p.typeOf(tokIdent):
		p.advance()
		ident := tok.Value
		return &Value{Ident: &ident}, nil
	}
	if p.isPunct(tok, "[") {
		return p.parseList()
	}
	return nil, nil
}

// parseList parses a `[ ... ]` literal. Elements are separated by commas
// and/or whitespace (BAML accepts both; whitespace alone matches the
// introspect parser's whitespace-splitting strategy list semantics).
func (p *parser) parseList() (*Value, error) {
	p.advance() // consume "["
	var elems []*Value
	for !p.eof() && !p.isPunct(p.peek(), "]") {
		// Skip stray commas between elements.
		if p.isPunct(p.peek(), ",") {
			p.advance()
			continue
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		if v == nil {
			// Defensive: unrecognised token inside list. Advance
			// so the loop makes progress.
			p.advance()
			continue
		}
		elems = append(elems, v)
	}
	if p.isPunct(p.peek(), "]") {
		p.advance()
	}
	return &Value{List: elems}, nil
}

// skipBalanced consumes tokens until the brace / bracket / paren count
// returns to zero. Used to skip opaque block bodies in TypeBlock,
// TemplateBlock, and the catch-all Other path. Assumes the next token is
// the opener.
func (p *parser) skipBalanced(open, close string) {
	if !p.isPunct(p.peek(), open) {
		return
	}
	p.advance() // consume opener
	depth := 1
	for !p.eof() && depth > 0 {
		t := p.advance()
		if p.isPunctValue(t, open) {
			depth++
		} else if p.isPunctValue(t, close) {
			depth--
		}
	}
}

func (p *parser) isPunctValue(t lexer.Token, ch string) bool {
	return t.Type == p.typeOf(tokPunct) && t.Value == ch
}

// skipBalancedBlockIfPresent consumes a `{ ... }` block immediately
// following the current position. No-op when the next token isn't `{`.
func (p *parser) skipBalancedBlockIfPresent() {
	if p.isPunct(p.peek(), "{") {
		p.skipBalanced("{", "}")
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
