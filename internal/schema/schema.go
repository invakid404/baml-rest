// Package schema is baml-rest's own compact schema model, mirroring
// BAML's runtime spine: TypeIR (the recursive type enum) plus
// OutputFormatContent (the compact bundle of enum/class/recursive-alias
// definitions keyed for lookup).
//
// This is deliberately NOT a port of the BAML AST FieldType (parser
// syntax), the CFFI serde wrappers (Optional/Checked/StreamState), the
// parser-database, or the full IntermediateRepr. It is the minimal
// self-contained shape that both front-end output-format rendering and
// back-end jsonish parse-resolution need:
//
//   - jsonish::from_str consumes only (&OutputFormatContent, &TypeIR,
//     raw, raw_is_done) before coercion.
//   - ctx.output_format rendering wraps the same OutputFormatContent and
//     descends a TypeIR.
//
// Fidelity decisions worth calling out (see issue #524):
//
//   - There is no optional/nullable type kind. BAML TypeIR represents
//     optional as a union-with-null; this model follows that exactly via
//     [UnionType.Nullable]. [UnionType.Variants] never contains a null
//     primitive.
//   - Literals are first-class ([TypeLiteral]), not primitives carrying a
//     constraint.
//   - Ordered slices are the source of truth (BAML uses IndexMap/IndexSet
//     so serialized order is load-bearing for the renderer). The private
//     index maps are rebuilt from the slices and are never serialized.
//   - Cross-definition recursion is by canonical name, never Go pointer
//     cycles. Only local type nesting (Elem/Key/Value/Items/...) uses
//     pointers.
//
// This first cut owns the data model only. It is intentionally NOT wired
// into rendering (P3) or jsonish/parser replacement (P4/P5), and stores
// constraints opaque-but-unevaluated.
package schema

// Bundle is the compact schema bundle: a target type plus the
// definitions it can reference. It mirrors BAML's OutputFormatContent
// (enum map, class map keyed by (name, mode), recursive class set,
// structural recursive alias map, and target type).
//
// The exported slices are the canonical, ordered source of truth. The
// unexported index maps provide BAML-equivalent O(1) lookups and are
// (re)built by [Bundle.RebuildIndexes]; they are never serialized, so a
// Bundle decoded from JSON must have RebuildIndexes called before any
// Find* method is used. Constructors in this package
// ([FromDynamicOutputSchema]) build the indexes for you.
type Bundle struct {
	Target Type `json:"target"`

	Enums   []EnumDef  `json:"enums,omitempty"`
	Classes []ClassDef `json:"classes,omitempty"`

	RecursiveClasses           []string            `json:"recursive_classes,omitempty"`
	StructuralRecursiveAliases []RecursiveAliasDef `json:"structural_recursive_aliases,omitempty"`

	enumByName  map[string]int   `json:"-"`
	classByKey  map[ClassKey]int `json:"-"`
	aliasByName map[string]int   `json:"-"`
}

// ClassKey is the (canonical name, streaming mode) key BAML uses to index
// classes. The same class can exist twice under distinct modes; nothing
// else may collide.
type ClassKey struct {
	Name string
	Mode StreamingMode
}

// Name carries the canonical name plus an optional rendered alias. The
// canonical [Name.Name] is identity — global maps are keyed by it, never
// by the alias. A nil Alias means the rendered name equals the canonical
// name, matching BAML's Name::rendered_name().
type Name struct {
	Name  string  `json:"name"`
	Alias *string `json:"alias,omitempty"`
}

// RenderedName returns the alias when present, else the canonical name.
// This mirrors BAML's Name::rendered_name() and is the name used for
// parse matching and renderer output, never for identity/indexing.
func (n Name) RenderedName() string {
	if n.Alias != nil {
		return *n.Alias
	}
	return n.Name
}

// EnumDef is the parse/render shape of an enum: ordered values plus
// opaque constraints. valueByName / valueByRenderedName are convenience
// indexes rebuilt from Values.
type EnumDef struct {
	Name        Name         `json:"name"`
	Values      []EnumValue  `json:"values"`
	Constraints []Constraint `json:"constraints,omitempty"`

	valueByName         map[string]int `json:"-"`
	valueByRenderedName map[string]int `json:"-"`
}

// EnumValue is a single enum member: canonical name (with optional
// rendered alias) and an optional description shown to the model.
type EnumValue struct {
	Name        Name    `json:"name"`
	Description *string `json:"description,omitempty"`
}

// ClassDef is the parse/render shape of a class: ordered fields plus
// class-level streaming behavior and opaque constraints. It is indexed by
// (Name.Name, Mode). fieldByName / fieldByRenderedName are convenience
// indexes rebuilt from Fields.
type ClassDef struct {
	Name        Name              `json:"name"`
	Description *string           `json:"description,omitempty"`
	Mode        StreamingMode     `json:"mode"`
	Fields      []ClassField      `json:"fields"`
	Constraints []Constraint      `json:"constraints,omitempty"`
	Stream      StreamingBehavior `json:"stream,omitzero"`

	fieldByName         map[string]int `json:"-"`
	fieldByRenderedName map[string]int `json:"-"`
}

// ClassField is one field of a class. StreamingNeeded is BAML's
// per-field tuple boolean (the fourth element in Class.fields), kept
// distinct from type-level [TypeMeta.Stream] and class-level
// [ClassDef.Stream] because BAML carries all three independently.
type ClassField struct {
	Name            Name    `json:"name"`
	Type            Type    `json:"type"`
	Description     *string `json:"description,omitempty"`
	StreamingNeeded bool    `json:"streaming_needed,omitempty"`
}

// RecursiveAliasDef is one entry of BAML's structural_recursive_aliases
// map: a canonical alias name bound to a target type. Edges into it are
// by [TypeRecursiveAlias] name, not pointer.
type RecursiveAliasDef struct {
	Name   string `json:"name"`
	Target Type   `json:"target"`
}

// TypeKind enumerates the BAML TypeIR variants. The list must be audited
// on BAML upgrades: a missing variant silently breaks parser/render
// parity.
type TypeKind string

const (
	TypeTop            TypeKind = "top"
	TypePrimitive      TypeKind = "primitive"
	TypeEnum           TypeKind = "enum"
	TypeLiteral        TypeKind = "literal"
	TypeClass          TypeKind = "class"
	TypeList           TypeKind = "list"
	TypeMap            TypeKind = "map"
	TypeRecursiveAlias TypeKind = "recursive_alias"
	TypeTuple          TypeKind = "tuple"
	TypeArrow          TypeKind = "arrow"
	TypeUnion          TypeKind = "union"
)

// Type is one node of a BAML TypeIR tree. Kind selects which of the
// payload fields are meaningful; the comments name the kind(s) each field
// belongs to. Meta carries the per-type constraints and streaming
// behavior.
type Type struct {
	Kind TypeKind `json:"kind"`
	Meta TypeMeta `json:"meta,omitzero"`

	Primitive PrimitiveKind `json:"primitive,omitempty"` // TypePrimitive
	Media     MediaKind     `json:"media,omitempty"`     // TypePrimitive when Primitive == PrimitiveMedia
	Name      string        `json:"name,omitempty"`      // TypeEnum, TypeClass, TypeRecursiveAlias
	Mode      StreamingMode `json:"mode,omitempty"`      // TypeClass, TypeRecursiveAlias
	Dynamic   bool          `json:"dynamic,omitempty"`   // TypeEnum, TypeClass
	Literal   *LiteralValue `json:"literal,omitempty"`   // TypeLiteral

	Elem  *Type `json:"elem,omitempty"`  // TypeList
	Key   *Type `json:"key,omitempty"`   // TypeMap
	Value *Type `json:"value,omitempty"` // TypeMap

	Items []Type     `json:"items,omitempty"` // TypeTuple
	Union *UnionType `json:"union,omitempty"` // TypeUnion
	Arrow *ArrowType `json:"arrow,omitempty"` // TypeArrow
}

// PrimitiveKind enumerates BAML's primitive values. Media is included for
// fidelity but is rejected by the output-usable profile (the renderer
// errors on it).
type PrimitiveKind string

const (
	PrimitiveString PrimitiveKind = "string"
	PrimitiveInt    PrimitiveKind = "int"
	PrimitiveFloat  PrimitiveKind = "float"
	PrimitiveBool   PrimitiveKind = "bool"
	PrimitiveNull   PrimitiveKind = "null"
	PrimitiveMedia  PrimitiveKind = "media"
)

// MediaKind enumerates BAML's media subtypes (BamlMediaType). It is the
// payload of [Type.Media] when Primitive == PrimitiveMedia, preserving the
// distinction Rust's TypeValue::Media(BamlMediaType) carries rather than
// collapsing all media into one value. The current dynamic-schema path
// never produces media (and ValidateOutput rejects it), but a media
// primitive must still name a valid subtype to pass structural validation.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaAudio MediaKind = "audio"
	MediaPDF   MediaKind = "pdf"
	MediaVideo MediaKind = "video"
)

// LiteralKind enumerates the BAML literal value kinds (string, int, bool
// only — there are no float/null literals in TypeIR).
type LiteralKind string

const (
	LiteralString LiteralKind = "string"
	LiteralInt    LiteralKind = "int"
	LiteralBool   LiteralKind = "bool"
)

// LiteralValue is a literal type's value. Kind selects which scalar field
// is meaningful.
type LiteralValue struct {
	Kind   LiteralKind `json:"kind"`
	String string      `json:"string,omitempty"`
	Int    int64       `json:"int,omitempty"`
	Bool   bool        `json:"bool,omitempty"`
}

// UnionType is a union's payload. Variants holds the non-null variants in
// BAML order; null is represented solely by Nullable. This preserves
// BAML's effective iter_include_null order: non-null variants first,
// canonical null appended last when the union is optional.
type UnionType struct {
	Variants []Type `json:"variants"`
	Nullable bool   `json:"nullable,omitempty"`
}

// ArrowType is a function type (params -> return). Carried for TypeIR
// fidelity; rejected by the output-usable profile.
type ArrowType struct {
	Params []Type `json:"params"`
	Return Type   `json:"return"`
}

// StreamingMode is the namespace BAML keys classes under: a class can
// exist as both its non-streaming and streaming variant.
type StreamingMode string

const (
	NonStreaming StreamingMode = "non_streaming"
	Streaming    StreamingMode = "streaming"
)

// TypeMeta is the per-type metadata BAML attaches to TypeIR: constraints
// plus streaming behavior.
type TypeMeta struct {
	Constraints []Constraint      `json:"constraints,omitempty"`
	Stream      StreamingBehavior `json:"stream,omitzero"`
}

// IsZero reports whether the metadata carries nothing, so encoding/json's
// omitzero drops it from output for the common bare-type case.
func (m TypeMeta) IsZero() bool {
	return len(m.Constraints) == 0 && m.Stream.IsZero()
}

// StreamingBehavior is BAML's {needed, done, state} streaming triple:
// not-null (needed), complete-before-visible (done), and {value,
// streaming_state} client display (state).
type StreamingBehavior struct {
	Needed bool `json:"needed,omitempty"`
	Done   bool `json:"done,omitempty"`
	State  bool `json:"state,omitempty"`
}

// IsZero reports whether all three streaming flags are false.
func (s StreamingBehavior) IsZero() bool {
	return !s.Needed && !s.Done && !s.State
}

// ConstraintLevel distinguishes a soft check from a hard assert.
type ConstraintLevel string

const (
	ConstraintCheck  ConstraintLevel = "check"
	ConstraintAssert ConstraintLevel = "assert"
)

// Constraint is stored opaquely: the expression is not parsed or
// evaluated in this first cut.
type Constraint struct {
	Level      ConstraintLevel `json:"level"`
	Expression string          `json:"expression"`
	Label      *string         `json:"label,omitempty"`
}

// strPtr returns a pointer to s, or nil when s is empty. Used to lower
// the dynamic schema's plain-string optional fields (description/alias)
// into the model's pointer-as-presence shape.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
