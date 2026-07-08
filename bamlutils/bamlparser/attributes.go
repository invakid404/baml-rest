package bamlparser

import (
	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// Attribute parsing is hand-rolled rather than expressed as participle
// struct tags. This is the one part of the type grammar that genuinely
// cannot be a clean tag grammar: attribute arguments carry arbitrary
// constraint/Jinja text (`@assert(ok, {{ this > 0 }})`, `@check(nonempty,
// {{ this|length > 0 }})`) whose exact source bytes must be preserved. The
// hook records the attribute name, a best-effort structured argument list,
// and — critically — the raw source span of the parenthesised arguments,
// resolved into Attribute.RawArgs once the source is available (see
// fillAttributeRawArgs). Justified per the de-BAML philosophy (#586):
// hand-roll only what tags cannot express.

// Parse implements participle.Parseable for Attribute as a FIELD attribute
// (`@name(args?)`). It refuses block attributes (`@@name`): on seeing a
// second `@` it restores the lexer and returns NextMatch so the enclosing
// grammar (or the class/enum member loop) handles the `@@` block attribute
// instead. This lets the type grammar reference `[]*Attribute` for trailing
// field attributes without swallowing a following `@@dynamic`.
func (a *Attribute) Parse(lex *lexer.PeekingLexer) error {
	if !peekPunct(lex, "@") {
		return participle.NextMatch
	}
	cp := lex.MakeCheckpoint()
	atTok := lex.Next() // consume "@"
	if peekPunct(lex, "@") {
		// This is a block attribute (`@@...`); not a field attribute.
		lex.LoadCheckpoint(cp)
		return participle.NextMatch
	}
	if lex.Peek().Type != identType {
		lex.LoadCheckpoint(cp)
		return participle.NextMatch
	}
	a.Block = false
	parseAttributeNameAndArgs(lex, a, atTok)
	return nil
}

// parseBlockAttribute parses a block attribute (`@@name(args?)`) from the
// current lexer position. The caller must have confirmed the next two
// tokens are `@` `@`. Returns nil if the shape is not a block attribute.
func parseBlockAttribute(lex *lexer.PeekingLexer) *Attribute {
	if !peekPunct(lex, "@") {
		return nil
	}
	cp := lex.MakeCheckpoint()
	atTok := lex.Next() // first "@"
	if !peekPunct(lex, "@") {
		lex.LoadCheckpoint(cp)
		return nil
	}
	lex.Next() // second "@"
	if lex.Peek().Type != identType {
		lex.LoadCheckpoint(cp)
		return nil
	}
	a := &Attribute{Block: true}
	parseAttributeNameAndArgs(lex, a, atTok)
	return a
}

// parseAttributeNameAndArgs consumes the dotted attribute name and, if
// present, a balanced parenthesised argument list. atTok is the leading `@`
// token, used to anchor the attribute's source span. On entry the lexer is
// positioned at the name identifier.
func parseAttributeNameAndArgs(lex *lexer.PeekingLexer, a *Attribute, atTok *lexer.Token) {
	nameTok := lex.Next() // identifier
	name := nameTok.Value
	lastEnd := tokenEnd(nameTok)
	// Dotted attribute names (e.g. stream.done, stream.with_state). The
	// lexer emits "." as its own Punct token, so a dotted name is
	// name ("." word)*.
	for peekPunct(lex, ".") {
		lex.Next() // "."
		if lex.Peek().Type == identType {
			w := lex.Next()
			name += "." + w.Value
			lastEnd = tokenEnd(w)
		}
	}
	a.Name = name

	if peekPunct(lex, "(") {
		a.HasParens = true
		openTok := lex.Next() // "("
		a.argStart = openTok.Pos.Offset + 1
		a.argEnd = a.argStart // default for empty args
		// depth counts ALL open brackets — parens (, list [, and brace { —
		// so that a top-level argument separator is a comma at depth 1 and a
		// list/Jinja/group body does not mis-segment arguments. (Brace tokens
		// cover Jinja `{{ ... }}`.) The arg list ends when a closing bracket
		// brings depth back to 0.
		depth := 1
		atArgStart := true // at the first token of a top-level argument
		for depth > 0 {
			t := lex.Peek()
			if t.EOF() {
				break
			}
			switch {
			case isPunctTok(t, "(") || isPunctTok(t, "[") || isPunctTok(t, "{"):
				depth++
				atArgStart = false
				lex.Next()
			case isPunctTok(t, ")") || isPunctTok(t, "]") || isPunctTok(t, "}"):
				depth--
				if depth == 0 {
					a.argEnd = t.Pos.Offset
					lastEnd = tokenEnd(t)
				}
				lex.Next()
			case depth == 1 && isPunctTok(t, ","):
				atArgStart = true
				lex.Next()
			default:
				// Best-effort structured capture: only the FIRST token of a
				// top-level argument, and only when it is a plain scalar. An
				// argument that starts with a complex/opening token (a Jinja
				// `{{...}}` block, a `[...]` list, a `(...)` group, or any
				// other non-scalar) is treated as COMPLEX — no structured
				// scalar is extracted from inside it. The `atArgStart` flag is
				// consumed on the first token regardless of scalar-ness, so a
				// scalar buried after leading braces/brackets cannot leak into
				// Args. Complex arguments survive verbatim via RawArgs, which
				// is the authoritative representation (#586: structured Args
				// are best-effort; RawArgs is exact).
				if depth == 1 && atArgStart {
					if v := scalarValueFromToken(t); v != nil {
						a.Args = append(a.Args, v)
					}
					atArgStart = false
				}
				lex.Next()
			}
		}
	}

	a.Span = Span{
		Start: atTok.Pos.Offset,
		End:   lastEnd,
		Line:  atTok.Pos.Line,
		Col:   atTok.Pos.Column,
	}
}

// scalarValueFromToken builds a best-effort *Value from a single lexer token
// for attribute-argument capture. Only the simple scalar shapes are
// represented; everything else returns nil (and survives via RawArgs). The
// returned Value carries the raw token text; delimiters are stripped by the
// normalisation pass (normalizeAttribute), matching the config Value
// contract.
func scalarValueFromToken(t *lexer.Token) *Value {
	switch t.Type {
	case stringType:
		s := t.Value
		return &Value{Literal: &s}
	case envRefType:
		s := t.Value
		return &Value{EnvRef: &s}
	case numberType:
		s := t.Value
		return &Value{Number: &s}
	case rawStringType:
		s := t.Value
		return &Value{Raw: &s}
	case identType:
		s := t.Value
		return &Value{Ident: &s}
	}
	return nil
}

// tokenEnd returns the byte offset just past the end of t.
func tokenEnd(t *lexer.Token) int {
	return t.Pos.Offset + len(t.Value)
}
