// Package schemadescriptor is the public, dependency-free mirror of
// baml-rest's internal compact schema model (internal/schema.Bundle). It is
// the stable contract a BAML-side code generator emits and a de-BAML host
// lowers back into an internal/schema.Bundle via
// schema.FromStaticDescriptor.
//
// Why a separate package (and a separate module): a generated introspection
// package must be able to name these types without depending on baml-rest's
// root module or its unexported internals. internal/schema cannot be that
// contract — Go's internal-visibility rule forbids code outside the root
// module from importing it, and its Bundle carries unexported index maps.
// This package therefore RE-DECLARES the model as plain exported data with
// its own enum constants, importing nothing (in particular nothing from the
// root internal/* tree). The lowering translator lives on the internal side
// (schema.FromStaticDescriptor) so that this package stays a passive data
// contract with no knowledge of internal/schema.
//
// Fidelity contract: the field set, JSON tags, and enum string values here
// MUST stay byte-for-byte in lockstep with internal/schema. A drift is not a
// compile error (the two type sets are independent by design), so the
// lowering translator re-validates every enum string and structural
// invariant on the way in, failing closed on anything it does not
// recognise. When internal/schema gains or renames a TypeIR variant, this
// mirror and schema.FromStaticDescriptor must be updated together.
//
// The descriptor also carries envelope metadata the internal Bundle does not
// model ([Bundle.Method], [Bundle.Stream], [Bundle.Version]): the function a
// descriptor was generated for, whether it is the streaming variant, and the
// descriptor schema version. These let a host register descriptors by
// (method, stream) and reject an incompatible generator; the lowering
// translator validates Version and otherwise leaves the internal Bundle
// (which has no such fields) unaffected by them.
package schemadescriptor

// Version is the current descriptor schema version. A generated descriptor
// stamps [Bundle.Version] with the version it was produced for; the lowering
// translator rejects any other value so a host never silently mislowers a
// descriptor emitted by an incompatible generator.
const Version = 1

// Bundle is the public mirror of internal/schema.Bundle: a target type plus
// the enum/class/recursive-alias definitions it can reference, in BAML
// output-format order. Unlike the internal Bundle it also carries the
// envelope metadata a registry keys on (Version, Method, Stream); those
// three fields have no counterpart in the internal Bundle and are consumed
// by the host, not by type lowering.
//
// The slices are the ordered source of truth. Order is load-bearing: it is
// BAML's render_output_format hoist order (reverse-of-reference DFS), so the
// lowering translator preserves it verbatim rather than recomputing
// reachability.
type Bundle struct {
	// Version is the descriptor schema version; see [Version].
	Version int `json:"version"`
	// Method is the BAML function this descriptor was generated for. It is
	// envelope metadata for host-side registration and is not lowered into
	// the internal Bundle.
	Method string `json:"method,omitempty"`
	// Stream reports whether this descriptor is the streaming variant of
	// Method. Like Method it is envelope metadata, not lowered.
	Stream bool `json:"stream,omitempty"`

	Target Type `json:"target"`

	Enums   []EnumDef  `json:"enums,omitempty"`
	Classes []ClassDef `json:"classes,omitempty"`

	RecursiveClasses           []string            `json:"recursive_classes,omitempty"`
	StructuralRecursiveAliases []RecursiveAliasDef `json:"structural_recursive_aliases,omitempty"`
}

// Name carries a canonical name plus an optional rendered alias. A nil Alias
// means the rendered name equals the canonical name (BAML
// Name::rendered_name). The alias's nil-vs-present distinction is
// significant and preserved through lowering.
type Name struct {
	Name  string  `json:"name"`
	Alias *string `json:"alias,omitempty"`
}

// EnumDef is an enum definition: ordered values plus opaque constraints.
type EnumDef struct {
	Name        Name         `json:"name"`
	Values      []EnumValue  `json:"values"`
	Constraints []Constraint `json:"constraints,omitempty"`
}

// EnumValue is one enum member: canonical name (with optional alias) and an
// optional description. A nil Description is distinct from an empty one.
type EnumValue struct {
	Name        Name    `json:"name"`
	Description *string `json:"description,omitempty"`
}

// ClassDef is a class definition: ordered fields plus class-level streaming
// behavior and opaque constraints. It is identified by (Name.Name, Mode).
type ClassDef struct {
	Name        Name              `json:"name"`
	Description *string           `json:"description,omitempty"`
	Mode        StreamingMode     `json:"mode"`
	Fields      []ClassField      `json:"fields"`
	Constraints []Constraint      `json:"constraints,omitempty"`
	Stream      StreamingBehavior `json:"stream,omitzero"`
}

// ClassField is one field of a class. StreamingNeeded mirrors BAML's
// per-field streaming boolean, kept distinct from type-level and class-level
// streaming behavior.
type ClassField struct {
	Name            Name    `json:"name"`
	Type            Type    `json:"type"`
	Description     *string `json:"description,omitempty"`
	StreamingNeeded bool    `json:"streaming_needed,omitempty"`
}

// RecursiveAliasDef is one entry of BAML's structural_recursive_aliases map:
// a canonical alias name bound to a target type. References into it are by
// name via a [TypeRecursiveAlias] node.
type RecursiveAliasDef struct {
	Name   string `json:"name"`
	Target Type   `json:"target"`
}

// TypeKind mirrors internal/schema.TypeKind: the BAML TypeIR variant tags.
// Declared as independent constants, never aliased to the internal type.
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

// Type is one node of a TypeIR tree. Kind selects which payload fields are
// meaningful; the comments name the kind(s) each field belongs to. This
// mirrors internal/schema.Type field-for-field.
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

// PrimitiveKind mirrors internal/schema.PrimitiveKind.
type PrimitiveKind string

const (
	PrimitiveString PrimitiveKind = "string"
	PrimitiveInt    PrimitiveKind = "int"
	PrimitiveFloat  PrimitiveKind = "float"
	PrimitiveBool   PrimitiveKind = "bool"
	PrimitiveNull   PrimitiveKind = "null"
	PrimitiveMedia  PrimitiveKind = "media"
)

// MediaKind mirrors internal/schema.MediaKind: the media subtype carried by
// a primitive when Primitive == PrimitiveMedia.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaAudio MediaKind = "audio"
	MediaPDF   MediaKind = "pdf"
	MediaVideo MediaKind = "video"
)

// LiteralKind mirrors internal/schema.LiteralKind (string, int, bool only).
type LiteralKind string

const (
	LiteralString LiteralKind = "string"
	LiteralInt    LiteralKind = "int"
	LiteralBool   LiteralKind = "bool"
)

// LiteralValue is a literal type's value. Kind selects the meaningful scalar
// field.
type LiteralValue struct {
	Kind   LiteralKind `json:"kind"`
	String string      `json:"string,omitempty"`
	Int    int64       `json:"int,omitempty"`
	Bool   bool        `json:"bool,omitempty"`
}

// UnionType is a union's payload: non-null variants in BAML order plus a
// Nullable marker. Variants never contains a null primitive; nullability is
// expressed solely by Nullable, matching BAML's iter_include_null ordering.
type UnionType struct {
	Variants []Type `json:"variants"`
	Nullable bool   `json:"nullable,omitempty"`
}

// ArrowType is a function type (params -> return). Carried for TypeIR
// fidelity; rejected by the output-usable profile on the internal side.
type ArrowType struct {
	Params []Type `json:"params"`
	Return Type   `json:"return"`
}

// StreamingMode mirrors internal/schema.StreamingMode: the namespace BAML
// keys classes under.
type StreamingMode string

const (
	NonStreaming StreamingMode = "non_streaming"
	Streaming    StreamingMode = "streaming"
)

// TypeMeta is the per-type metadata BAML attaches to TypeIR: opaque
// constraints plus streaming behavior.
type TypeMeta struct {
	Constraints []Constraint      `json:"constraints,omitempty"`
	Stream      StreamingBehavior `json:"stream,omitzero"`
}

// IsZero reports whether the metadata carries nothing, so encoding/json's
// omitzero drops it for the common bare-type case.
func (m TypeMeta) IsZero() bool {
	return len(m.Constraints) == 0 && m.Stream.IsZero()
}

// StreamingBehavior mirrors BAML's {needed, done, state} streaming triple.
type StreamingBehavior struct {
	Needed bool `json:"needed,omitempty"`
	Done   bool `json:"done,omitempty"`
	State  bool `json:"state,omitempty"`
}

// IsZero reports whether all three streaming flags are false.
func (s StreamingBehavior) IsZero() bool {
	return !s.Needed && !s.Done && !s.State
}

// ConstraintLevel mirrors internal/schema.ConstraintLevel: a soft check
// versus a hard assert.
type ConstraintLevel string

const (
	ConstraintCheck  ConstraintLevel = "check"
	ConstraintAssert ConstraintLevel = "assert"
)

// Constraint is stored opaquely: the expression is never parsed or evaluated
// by this contract or by lowering.
type Constraint struct {
	Level      ConstraintLevel `json:"level"`
	Expression string          `json:"expression"`
	Label      *string         `json:"label,omitempty"`
}
