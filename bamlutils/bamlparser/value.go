package bamlparser

import "strings"

// IsLiteral reports whether v carries a quoted-string literal.
func (v *Value) IsLiteral() bool {
	return v != nil && v.Literal != nil
}

// IsEnvRef reports whether v carries a bare env.IDENT reference.
func (v *Value) IsEnvRef() bool {
	return v != nil && v.EnvRef != nil
}

// IsIdent reports whether v carries an unquoted, non-env identifier.
func (v *Value) IsIdent() bool {
	return v != nil && v.Ident != nil
}

// IsNumber reports whether v carries a numeric literal.
func (v *Value) IsNumber() bool {
	return v != nil && v.Number != nil
}

// IsRaw reports whether v carries a raw-string (`#"..."#`) literal.
func (v *Value) IsRaw() bool {
	return v != nil && v.Raw != nil
}

// IsList reports whether v carries a `[ ... ]` literal.
func (v *Value) IsList() bool {
	return v != nil && v.List != nil
}

// String returns v's string representation for scalar shapes:
//
//   - Literal -> the quoted-string content with quotes stripped (no
//     escape processing — matches the introspect parser's stripBamlQuotes).
//   - EnvRef  -> the bare env-var name (no leading "env.").
//   - Ident   -> the identifier value.
//   - Number  -> the raw numeric lexeme.
//   - Raw     -> the raw-string content between #" and "#.
//
// For List values String returns "" and ok=false; callers wanting list
// semantics use Value.Strings or the per-element Value accessors directly.
func (v *Value) String() (string, bool) {
	if v == nil {
		return "", false
	}
	switch {
	case v.Literal != nil:
		return *v.Literal, true
	case v.EnvRef != nil:
		return *v.EnvRef, true
	case v.Ident != nil:
		return *v.Ident, true
	case v.Number != nil:
		return *v.Number, true
	case v.Raw != nil:
		return *v.Raw, true
	}
	return "", false
}

// AsString returns the string form of v as the introspect parser sees it
// post-cleanBamlValue: surrounding quotes stripped, no escape processing,
// env-ref provenance ignored (env refs return their env-var name verbatim).
// Use this when emulating a code path that called cleanBamlValue on the
// raw token; for env-vs-literal-aware consumers, prefer EnvName + Literal
// branching.
func (v *Value) AsString() string {
	s, _ := v.String()
	return s
}

// EnvName returns the env-var name when v is an EnvRef. ok=false for any
// other shape — including a quoted `"env.NAME"` Literal, which the
// upstream BAML grammar treats as a regular string, not an env reference.
func (v *Value) EnvName() (string, bool) {
	if v == nil || v.EnvRef == nil {
		return "", false
	}
	return *v.EnvRef, true
}

// LiteralValue returns the literal-string content when v is a Literal.
// ok=false for any other shape. The returned string is the raw bytes
// between the surrounding double quotes; no escape processing is applied.
func (v *Value) LiteralValue() (string, bool) {
	if v == nil || v.Literal == nil {
		return "", false
	}
	return *v.Literal, true
}

// IdentValue returns the identifier content when v is an Ident.
// ok=false for any other shape.
func (v *Value) IdentValue() (string, bool) {
	if v == nil || v.Ident == nil {
		return "", false
	}
	return *v.Ident, true
}

// NumberValue returns the raw numeric lexeme when v is a Number.
// ok=false for any other shape. The lexeme is returned verbatim so the
// caller can pick the appropriate strconv parser (Atoi vs ParseInt with
// a specific bit width vs ParseFloat).
func (v *Value) NumberValue() (string, bool) {
	if v == nil || v.Number == nil {
		return "", false
	}
	return *v.Number, true
}

// RawValue returns the raw-string content (between `#"` and `"#`) when v
// is a Raw value. ok=false for any other shape.
func (v *Value) RawValue() (string, bool) {
	if v == nil || v.Raw == nil {
		return "", false
	}
	return *v.Raw, true
}

// Strings flattens a list-shaped value into a slice of strings, applying
// the scalar String conversion to each element. Elements without a string
// representation (e.g. nested lists) are skipped.
//
// ok reports whether v was a list at all; an empty list returns
// (nil, true), distinguishing "no list" from "empty list" for callers that
// care (e.g. fallback-chain emission, where an empty list and an absent
// strategy field are both inactive but produced by different shapes).
func (v *Value) Strings() ([]string, bool) {
	if v == nil || v.List == nil {
		return nil, false
	}
	var out []string
	for _, e := range v.List {
		if s, ok := e.String(); ok {
			out = append(out, s)
		}
	}
	return out, true
}

// StringList interprets v as a list of strings, matching the introspect
// parser's parseStrategyList semantics:
//
//   - When v is already a List value, elements are converted via Strings.
//   - When v is a scalar string-shaped value, the lexeme is split on
//     commas / whitespace and each non-empty token is returned (this
//     mirrors the line-based parser's behaviour of accepting
//     `strategy ClientA ClientB` even though strictly it should require
//     brackets).
//
// Returns (nil, false) when v has no list-or-string interpretation.
func (v *Value) StringList() ([]string, bool) {
	if v == nil {
		return nil, false
	}
	if v.List != nil {
		out, _ := v.Strings()
		return out, true
	}
	if s, ok := v.String(); ok {
		tokens := splitStrategyTokens(s)
		if len(tokens) == 0 {
			return nil, false
		}
		return tokens, true
	}
	return nil, false
}

// splitStrategyTokens applies the introspect parser's strategy-token rule:
// split on comma / space / tab / newline, strip surrounding double quotes
// from each token, drop empties.
func splitStrategyTokens(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		t := strings.TrimSpace(part)
		t = stripSurroundingQuotes(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// stripSurroundingQuotes mirrors the introspect parser's stripBamlQuotes
// (cmd/introspect/main.go:1480-1488): remove a single pair of surrounding
// double quotes, do not touch anything else. Used by StringList so that
// `strategy ["A", "B"]` flattens to ["A", "B"] regardless of whether the
// upstream form put the quotes on the list elements.
func stripSurroundingQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
