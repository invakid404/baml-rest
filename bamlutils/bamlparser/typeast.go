package bamlparser

// This file defines the typed AST for the BAML *type system* — class/enum
// members, type aliases, function signatures, type expressions, and
// attributes. It is the "native front-end" surface described in de-BAML P3
// (issue #586): the parser now retains the .baml type system instead of
// discarding class/enum bodies and function signatures as metadata-only
// stubs.
//
// SCOPE (slice 1 — PARSE ONLY): these nodes are produced by the parser but
// are NOT yet consumed to build schema descriptors. Later slices lower them
// into bamlutils/schemadescriptor.Bundle. Every node can be marked
// Unsupported so later builder slices decline deterministically.
//
// The shapes follow BAML v0.223's grammar
// (engine/baml-lib/ast/src/parser/datamodel.pest + parse_types.rs) as
// closely as reasonable. Where we are intentionally laxer than BAML (BAML
// runs at build and rejects invalid .baml before our parser ever sees it,
// so we only parse BAML-valid input in production), the divergence is noted
// in a comment referencing #586 and logged in that issue's
// "Lax-vs-BAML decisions" section.

// Span is a source byte range plus the 1-based line/column of its start. It
// lets later slices produce deterministic decline diagnostics that point at
// the exact declaration.
type Span struct {
	Start int // byte offset of the first byte (inclusive)
	End   int // byte offset just past the last byte (exclusive)
	Line  int // 1-based line of Start
	Col   int // 1-based column of Start
}

// TypeKind tags the TypeExpr union. TypeExpr is a closed tagged union: for
// any well-formed node exactly the fields relevant to Kind are populated.
type TypeKind int

const (
	// KindUnsupported marks a syntactically-recognised but semantically
	// unrepresentable type (or a shape the slice-1 parser declines to model
	// precisely). Reason carries a stable human-readable explanation and
	// Span points at the source. Later builder slices decline any method
	// whose output graph reaches an Unsupported node.
	KindUnsupported TypeKind = iota
	// KindPrimitive is one of "string", "int", "float", "bool", "null".
	KindPrimitive
	// KindMedia is one of "image", "audio", "pdf", "video".
	KindMedia
	// KindNameRef is a reference to a named type (class/enum/alias). It may
	// be namespaced ("a::b") or a path ("a.b"); both are declined by later
	// slices (BAML rejects path identifiers in base types) but are retained
	// here so the builder can decline rather than the parser erroring.
	KindNameRef
	// KindList is an array type with an element and a dimension count
	// (>=1). Multi-dimensional arrays (string[][]) keep Dims=2 rather than
	// nesting, mirroring BAML's FieldType::List which carries a dims count.
	KindList
	// KindMap is map<Key, Value>.
	KindMap
	// KindUnion is a union of Variants. Nullable captures optionality
	// (the trailing "?" and explicit lowering of optionals) — there is no
	// separate optional kind, per the design.
	KindUnion
	// KindLiteral is a literal type: a quoted string, an integer, a float
	// (declined by BAML v0.223 validation), or a bool (true/false).
	KindLiteral
	// KindTuple is a parenthesized, comma-separated tuple of 2+ items.
	KindTuple
	// KindGroup is a parenthesized single inner type. It is not a runtime
	// node after semantic build, but is retained so the BAML union-attribute
	// reassociation rule (parenthesized attributes stay on the inner type)
	// can be applied by later slices.
	KindGroup
)

// TypeExpr is the parsed form of a BAML type expression. See TypeKind for
// which fields are meaningful per kind. Attributes, Parenthesized, and Span
// are common to all kinds.
type TypeExpr struct {
	Kind TypeKind

	// KindPrimitive: one of string/int/float/bool/null.
	Primitive string
	// KindMedia: one of image/audio/pdf/video.
	Media string

	// KindNameRef.
	Name       string // canonical dotted/coloned source name
	Namespaced bool   // used "::" separators (namespaced_identifier)
	Path       bool   // used "." separators (path_identifier)

	// KindList.
	Elem *TypeExpr
	Dims int

	// KindMap.
	Key   *TypeExpr
	Value *TypeExpr

	// KindUnion.
	Variants []*TypeExpr
	Nullable bool

	// KindLiteral.
	LiteralKind  string // "string", "int", "float", "bool"
	LiteralValue string // raw value ("hi" content, 42, true, ...)

	// KindTuple.
	Items []*TypeExpr

	// KindGroup.
	Inner *TypeExpr

	// KindUnsupported.
	Reason string

	// Common to all kinds.
	Attributes    []*Attribute
	Parenthesized bool // node was wrapped directly in ( ) — reassociation marker
	Span          Span
}

// IsUnsupported reports whether t (or, shallowly, t itself) is an
// Unsupported node. It does not recurse; callers that need reachability
// walk the children themselves.
func (t *TypeExpr) IsUnsupported() bool {
	return t != nil && t.Kind == KindUnsupported
}

// Attribute is a parsed BAML attribute: field attributes (`@name(args?)`),
// block attributes (`@@name(args?)`), and type attributes carry the same
// shape. Names may be dotted (e.g. "stream.done").
//
// Args holds a best-effort structured parse of the parenthesised arguments
// (simple string/number/ident/list values). RawArgs holds the exact source
// text between the outermost parentheses so constraint expressions and Jinja
// (`@check(name, {{ this|length > 0 }})`) survive verbatim even when the
// structured parse cannot represent them. Both are retained deliberately —
// see #586 / the design's "Attribute AST" section.
type Attribute struct {
	Name          string // dotted attribute name, e.g. "alias", "stream.done"
	Block         bool   // true for `@@...` (block attribute), false for `@...`
	Parenthesized bool   // attribute appeared inside a ( ) group/paren type
	HasParens     bool   // an argument list `( ... )` was present
	Args          []*Value
	RawArgs       string // exact source between the outermost ( and )
	Span          Span

	// argStart/argEnd are byte offsets of the RawArgs slice, captured during
	// parse and resolved into RawArgs by fillAttributeRawArgs once the full
	// source is available (the Parseable hook only sees the lexer).
	argStart int
	argEnd   int
}

// Param is one named argument of a function signature or a type-expression
// block's named-argument list. Type is nil when the argument has no `:type`
// annotation (BAML's named_argument allows a bare identifier).
type Param struct {
	Name string
	Type *TypeExpr
	Span Span
}

// TypeMember is one entry inside a class or enum body. Class fields carry a
// non-nil Type; enum values carry Type == nil (matching BAML, where
// type_expression's field_type_chain is optional and enum values omit it).
// Attributes holds the field attributes (`@...`) attached to the member;
// attributes that bind to the member's type are attached to Type instead.
type TypeMember struct {
	Name       string
	Type       *TypeExpr
	Attributes []*Attribute
	Span       Span
}
