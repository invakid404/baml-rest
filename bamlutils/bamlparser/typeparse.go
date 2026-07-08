package bamlparser

import (
	"strings"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// This file holds the participle struct-tag grammar for BAML type
// expressions and the lowering pass that turns the raw grammar tree into
// the public TypeExpr tagged union.
//
// Per the de-BAML P3 philosophy (#586), the type grammar is expressed
// declaratively via participle struct tags — the ~99.99% that tags handle
// cleanly. The only hand-rolled piece here is Attribute.Parse, justified
// because attribute arguments carry arbitrary Jinja/constraint text
// (`@check(name, {{ this|length > 0 }})`) that cannot be a clean grammar and
// whose exact source bytes must be preserved.
//
// The grammar is intentionally structured for first-token dispatch (no
// backtracking): `(` -> group/tuple, a quoted string / number -> literal,
// an identifier -> named type (with optional `<...>` generics for map). This
// is slightly laxer than BAML's exact pest rule nesting (e.g. it accepts
// `<...>` generics after any identifier, and treats `?` as a per-element
// suffix rather than replicating BAML's union-then-optional precedence), but
// laxer-than-BAML is acceptable by design — BAML validates input before we
// see it. See #586 "Lax-vs-BAML decisions".
//
// DEFERRED SEMANTICS (slice 1 parses structure; later slices apply meaning):
//   - Attribute reassociation is NOT performed here. Attributes are attached
//     at their lexical position (trailing field attributes fold onto the
//     type via field_type_with_attr; enum-value attributes attach to the
//     member; `@@` to the block), and the Parenthesized marker is preserved,
//     so the builder slices can apply BAML's field<->type reassociation
//     (parse_field.rs reassociate_type_attributes) and last-union-variant
//     reassociation (parse_types.rs reassociate_union_attributes) faithfully.
//   - Doc comments (`///`) are not captured; the shared lexer elides all
//     `//`-prefixed comments. Descriptions/doc-comments are a slice-4
//     concern. Both are tracked in #586.

// -----------------------------------------------------------------------
// Raw grammar (unexported): the shape participle parses. Lowered to the
// public TypeExpr by lowerChain below.
// -----------------------------------------------------------------------

// rawChain mirrors BAML's field_type_chain: one or more field-types joined
// by "|". At the member/alias/return level this is the entry point.
type rawChain struct {
	Pos    lexer.Position
	EndPos lexer.Position

	First *rawFieldType   `@@`
	Rest  []*rawFieldType `( "|" @@ )*`
}

// rawFieldType mirrors BAML's field_type: a base type, any number of `[]`
// array suffixes, a trailing optional `?`, and trailing field attributes.
//
// LAX-VS-BAML: BAML nests these differently (union rule vs field_type vs
// array_notation) and applies a trailing `?` after a whole union to the
// union. We treat `?` as a per-element suffix and let lowerChain hoist
// optional single-variant unions, which reproduces BAML for the common
// cases (`Foo?`, `int | string?`, `(a|b)?`). The rare `X? | Y` mid-union
// ordering can differ; logged in #586. This never reaches us as invalid
// input, so the laxness only affects representation of edge shapes.
type rawFieldType struct {
	Pos    lexer.Position
	EndPos lexer.Position

	Base     *rawBase     `@@`
	Arrays   []string     `( @"[" "]" )*`
	Optional bool         `( @"?" )?`
	Attrs    []*Attribute `@@*`
}

// rawBase mirrors base_type, dispatched by first token: `(` -> group,
// string/number -> literal, identifier -> named.
type rawBase struct {
	Pos    lexer.Position
	EndPos lexer.Position

	Group   *rawGroup   `  @@`
	Literal *rawLiteral `| @@`
	Named   *rawNamed   `| @@`
}

// rawGroup unifies BAML's group, parenthesized_type, and tuple: one or more
// comma-separated inner chains inside parentheses, plus any group-level
// trailing attributes. lowerBase decides: 1 item -> Group (parenthesized
// single), 2+ items -> Tuple.
type rawGroup struct {
	Items []*rawChain  `"(" @@ ( "," @@ )*`
	Attrs []*Attribute `@@* ")"`
}

// rawLiteral is a quoted-string or numeric literal type. Bool literals
// (true/false) are identifiers and handled in lowerNamed.
type rawLiteral struct {
	Str *string `  @String`
	Num *string `| @Number`
}

// rawNamed is an identifier, optionally namespaced (`a::b`) or a path
// (`a.b`), optionally followed by `<...>` generics (only `map<K,V>` is
// meaningful; other generics are declined in lowering).
type rawNamed struct {
	Head     string        `@Ident`
	Segs     []*rawNameSeg `@@*`
	Generics *rawGenerics  `@@?`
}

// rawNameSeg is one qualified-name segment. Sep is "::" for namespaced names
// or "." for path identifiers. The lexer emits ":" as two tokens, so "::" is
// matched as two ":" literals.
type rawNameSeg struct {
	Sep  string `( @":" ":" | @"." )`
	Word string `@Ident`
}

type rawGenerics struct {
	Args []*rawChain `"<" @@ ( "," @@ )* ">"`
}

// typeChainParser parses a single rawChain (one type expression) from a
// shared lexer. The hand-rolled block hooks call ParseFromLexer against it
// so the type grammar stays declarative while member-boundary detection
// (which needs newline/line information the elided lexer hides) stays in the
// hooks. Comment/Whitespace are elided exactly as the top-level parser does.
var typeChainParser = participle.MustBuild[rawChain](
	participle.Lexer(bamlLexer),
	participle.Elide(tokComment, tokWS),
	participle.UseLookahead(2),
)

// parseTypeChainFromLexer parses one type expression from lex at its current
// position and lowers it to a public TypeExpr. It uses AllowTrailing so the
// greedy-but-bounded type grammar stops when it can no longer extend the
// type, leaving the remaining tokens (the next member, a closing brace, the
// next top-level item, ...) for the caller. Returns nil on parse failure so
// callers can fall back.
func parseTypeChainFromLexer(lex *lexer.PeekingLexer) *TypeExpr {
	raw, err := typeChainParser.ParseFromLexer(lex, participle.AllowTrailing(true))
	if err != nil || raw == nil {
		return nil
	}
	return lowerChain(raw)
}

// -----------------------------------------------------------------------
// Lowering: raw grammar tree -> public TypeExpr tagged union.
// -----------------------------------------------------------------------

func spanOf(pos, end lexer.Position) Span {
	return Span{Start: pos.Offset, End: end.Offset, Line: pos.Line, Col: pos.Column}
}

// lowerChain lowers a rawChain (field_type_chain). A single element lowers
// directly; multiple elements form a union. Optional single-variant unions
// produced by a "?" suffix are hoisted so `int | string?` becomes
// Union{int, string, Nullable} rather than a nested nullable union — this
// matches BAML's flattened union shape for the common cases.
func lowerChain(c *rawChain) *TypeExpr {
	if c == nil || c.First == nil {
		return &TypeExpr{Kind: KindUnsupported, Reason: "empty type expression"}
	}
	if len(c.Rest) == 0 {
		return lowerFieldType(c.First)
	}

	elems := make([]*rawFieldType, 0, 1+len(c.Rest))
	elems = append(elems, c.First)
	elems = append(elems, c.Rest...)

	variants := make([]*TypeExpr, 0, len(elems))
	nullable := false
	for _, e := range elems {
		lowered := lowerFieldType(e)
		// Hoist an optional single-variant union (from a bare `X?`
		// element) up to the enclosing union's Nullable flag.
		if lowered.Kind == KindUnion && lowered.Nullable && len(lowered.Variants) == 1 &&
			len(lowered.Attributes) == 0 {
			nullable = true
			variants = append(variants, lowered.Variants[0])
			continue
		}
		variants = append(variants, lowered)
	}

	return &TypeExpr{
		Kind:     KindUnion,
		Variants: variants,
		Nullable: nullable,
		Span:     spanOf(c.Pos, c.EndPos),
	}
}

// lowerFieldType lowers one field_type element: base, then array dimensions,
// then optional wrapping, then attaches trailing attributes to the outermost
// node.
func lowerFieldType(ft *rawFieldType) *TypeExpr {
	base := lowerBase(ft.Base)
	span := spanOf(ft.Pos, ft.EndPos)

	// Array suffixes: `T[]` and multi-dimensional `T[][]` collapse into a
	// single List with Dims = number of suffixes (matching BAML's
	// FieldType::List dims count).
	if dims := len(ft.Arrays); dims > 0 {
		base = &TypeExpr{Kind: KindList, Elem: base, Dims: dims, Span: span}
	}

	// Optional `?`: lower to a nullable union. If the target is already a
	// union, set Nullable in place; otherwise wrap it in a single-variant
	// nullable union (no separate optional kind, per the design). The
	// optional-array suffix (`T[]?`) therefore becomes a nullable union
	// around the whole list, not nullable elements.
	if ft.Optional {
		if base.Kind == KindUnion {
			base.Nullable = true
		} else {
			base = &TypeExpr{Kind: KindUnion, Variants: []*TypeExpr{base}, Nullable: true, Span: span}
		}
	}

	if len(ft.Attrs) > 0 {
		base.Attributes = append(base.Attributes, ft.Attrs...)
	}
	return base
}

func lowerBase(b *rawBase) *TypeExpr {
	span := spanOf(b.Pos, b.EndPos)
	switch {
	case b.Group != nil:
		return lowerGroup(b.Group, span)
	case b.Literal != nil:
		return lowerLiteral(b.Literal, span)
	case b.Named != nil:
		return lowerNamed(b.Named, span)
	}
	return &TypeExpr{Kind: KindUnsupported, Reason: "empty base type", Span: span}
}

// lowerGroup turns a parenthesised construct into a Tuple (2+ items) or a
// Group (single inner). A group-level trailing attribute, and any attribute
// on the inner element, are marked Parenthesized so BAML's union-attribute
// reassociation (parenthesised attributes stay on the inner type) can be
// applied by later slices.
func lowerGroup(g *rawGroup, span Span) *TypeExpr {
	items := make([]*TypeExpr, 0, len(g.Items))
	for _, it := range g.Items {
		items = append(items, lowerChain(it))
	}
	if len(items) >= 2 {
		// A tuple is always syntactically parenthesized: set the node's
		// Parenthesized bit and mark every attribute textually inside the
		// parentheses (items + group-level trailing attrs).
		t := &TypeExpr{Kind: KindTuple, Items: items, Parenthesized: true, Span: span}
		for _, it := range items {
			markParenthesizedAttrs(it)
		}
		markAttrsParenthesized(g.Attrs)
		t.Attributes = append(t.Attributes, g.Attrs...)
		return t
	}
	var inner *TypeExpr
	if len(items) == 1 {
		inner = items[0]
	} else {
		inner = &TypeExpr{Kind: KindUnsupported, Reason: "empty parenthesized type", Span: span}
	}
	// Every attribute textually inside the parentheses is marked
	// Parenthesized — recursively, so an attribute on a union variant or a
	// nested item (`(int | string @attr)`) is marked too, not just the
	// inner node's own attributes.
	markParenthesizedAttrs(inner)
	markAttrsParenthesized(g.Attrs)
	grp := &TypeExpr{Kind: KindGroup, Inner: inner, Parenthesized: true, Span: span}
	grp.Attributes = append(grp.Attributes, g.Attrs...)
	return grp
}

// markAttrsParenthesized flags a flat attribute slice as parenthesized.
func markAttrsParenthesized(attrs []*Attribute) {
	for _, a := range attrs {
		a.Parenthesized = true
	}
}

// markParenthesizedAttrs recursively flags every attribute within t (and its
// child types) as Parenthesized. It is applied to the contents of a
// parenthesized type/tuple so that BAML's union-attribute reassociation can
// be bounded correctly by a later slice.
//
// Granularity note (#586): this marker records that an attribute appeared
// textually inside a `( )`, which is coarser than BAML's parse-time
// `parenthesized` flag (BAML only sets it for the direct trailing attributes
// of a parenthesized_type/group, NOT for bare attributes on union variants
// or tuple items, which it parses with parenthesized=false and then hoists in
// reassociate_union_attributes). Slice 1 does not consume the marker, so this
// is harmless now; the future reassociation slice MUST reproduce BAML's
// last-union-variant hoisting using the Group/Tuple NODE structure
// (Kind==KindGroup/KindTuple, Parenthesized bit) rather than blindly treating
// every Parenthesized attribute as protected. LAX-VS-BAML logged in #586.
func markParenthesizedAttrs(t *TypeExpr) {
	if t == nil {
		return
	}
	markAttrsParenthesized(t.Attributes)
	markParenthesizedAttrs(t.Elem)
	markParenthesizedAttrs(t.Key)
	markParenthesizedAttrs(t.Value)
	markParenthesizedAttrs(t.Inner)
	for _, v := range t.Variants {
		markParenthesizedAttrs(v)
	}
	for _, it := range t.Items {
		markParenthesizedAttrs(it)
	}
}

func lowerLiteral(l *rawLiteral, span Span) *TypeExpr {
	if l.Str != nil {
		return &TypeExpr{
			Kind:         KindLiteral,
			LiteralKind:  "string",
			LiteralValue: stripStringQuotes(*l.Str),
			Span:         span,
		}
	}
	if l.Num != nil {
		kind := "int"
		if strings.Contains(*l.Num, ".") {
			// BAML v0.223 rejects float literal types; we retain the value
			// and let the builder decline. LAX-VS-BAML: accepted at parse
			// (#586).
			kind = "float"
		}
		return &TypeExpr{Kind: KindLiteral, LiteralKind: kind, LiteralValue: *l.Num, Span: span}
	}
	return &TypeExpr{Kind: KindUnsupported, Reason: "empty literal type", Span: span}
}

// primitiveNames maps the bare identifiers BAML treats as primitive scalar
// types. "null" is a primitive in BAML (Primitive(Optional, Null)); we keep
// it as a primitive node and let the builder decide null handling.
var primitiveNames = map[string]struct{}{
	"string": {}, "int": {}, "float": {}, "bool": {}, "null": {},
}

// mediaNames maps the bare identifiers BAML treats as media primitive types.
var mediaNames = map[string]struct{}{
	"image": {}, "audio": {}, "pdf": {}, "video": {},
}

func lowerNamed(n *rawNamed, span Span) *TypeExpr {
	// map<K, V> is the only meaningful generic. Other generics are declined.
	// LAX-VS-BAML: BAML only permits `<...>` on map; we accept generics after
	// any identifier at parse and decline non-map ones here (#586).
	if n.Generics != nil {
		if n.Head == "map" && len(n.Segs) == 0 && len(n.Generics.Args) == 2 {
			return &TypeExpr{
				Kind:  KindMap,
				Key:   lowerChain(n.Generics.Args[0]),
				Value: lowerChain(n.Generics.Args[1]),
				Span:  span,
			}
		}
		return &TypeExpr{
			Kind:   KindUnsupported,
			Reason: "generic type is not supported (only map<K,V>)",
			Span:   span,
		}
	}

	// Qualified names: namespaced (`a::b`) or path (`a.b`). BAML rejects path
	// identifiers in base types; we retain them as NameRef with the marker so
	// the builder declines. LAX-VS-BAML: accepted at parse (#586).
	if len(n.Segs) > 0 {
		var sb strings.Builder
		sb.WriteString(n.Head)
		namespaced, path := false, false
		for _, s := range n.Segs {
			if s.Sep == "." {
				sb.WriteString("." + s.Word)
				path = true
			} else {
				sb.WriteString("::" + s.Word)
				namespaced = true
			}
		}
		return &TypeExpr{
			Kind:       KindNameRef,
			Name:       sb.String(),
			Namespaced: namespaced,
			Path:       path,
			Span:       span,
		}
	}

	switch n.Head {
	case "true", "false":
		return &TypeExpr{Kind: KindLiteral, LiteralKind: "bool", LiteralValue: n.Head, Span: span}
	}
	if _, ok := primitiveNames[n.Head]; ok {
		return &TypeExpr{Kind: KindPrimitive, Primitive: n.Head, Span: span}
	}
	if _, ok := mediaNames[n.Head]; ok {
		return &TypeExpr{Kind: KindMedia, Media: n.Head, Span: span}
	}
	return &TypeExpr{Kind: KindNameRef, Name: n.Head, Span: span}
}
