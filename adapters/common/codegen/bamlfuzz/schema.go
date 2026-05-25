// Package bamlfuzz hosts the shared IR + rapid generators that
// back the property-style fuzz framework for the BAML-on-Go pipeline.
// It is intentionally implementation-agnostic: the IR describes a
// schema + value pair abstractly, and downstream emitters (dynamic
// TypeBuilder, static .baml source) lower it onto the two execution
// paths that need to agree.
//
// Naming convention for generated classes/enums is deterministic:
// classes are FuzzClass0, FuzzClass1, ...; enums are FuzzEnum0, FuzzEnum1, ...
// Property names are Fuzz_field_N. Stability across runs makes
// failure replay produce identical artifacts independent of map
// iteration order.
package bamlfuzz

// FuzzTypeKind enumerates every type shape the IR supports.
type FuzzTypeKind string

const (
	KindString   FuzzTypeKind = "string"
	KindInt      FuzzTypeKind = "int"
	KindFloat    FuzzTypeKind = "float"
	KindBool     FuzzTypeKind = "bool"
	KindNull     FuzzTypeKind = "null"
	KindLiteral  FuzzTypeKind = "literal"
	KindOptional FuzzTypeKind = "optional"
	KindList     FuzzTypeKind = "list"
	KindMap      FuzzTypeKind = "map"
	KindClassRef FuzzTypeKind = "class_ref"
	KindEnumRef  FuzzTypeKind = "enum_ref"
	// KindUnion represents an ordered sum of alternative types. The
	// type's Variants slice carries the alternatives in declaration
	// order; emitters render them with BAML pipe syntax (static) or
	// the upstream `union` / `oneOf` shape (dynamic). T | null is
	// expressed as a union with a KindNull variant — KindOptional
	// stays as the separate absent-key wrapper.
	KindUnion FuzzTypeKind = "union"
)

// FuzzLiteralKind narrows the literal payload to one of the BAML
// literal scalar kinds. Literals are stored inline on FuzzType so
// emitters can render the literal value without a second
// indirection through a separate node type.
type FuzzLiteralKind string

const (
	LiteralString FuzzLiteralKind = "string"
	LiteralInt    FuzzLiteralKind = "int"
	LiteralBool   FuzzLiteralKind = "bool"
)

// FuzzLiteral is the inline payload for KindLiteral. Exactly one of
// String / Int / Bool is meaningful, dispatched by Kind.
type FuzzLiteral struct {
	Kind   FuzzLiteralKind `json:"kind"`
	String string          `json:"string,omitempty"`
	Int    int64           `json:"int,omitempty"`
	Bool   bool            `json:"bool,omitempty"`
}

// FuzzType is the structural type spec. The discriminator is Kind;
// the other fields are populated based on Kind:
//   - KindOptional / KindList:    Inner
//   - KindMap:                    Key (always KindString in v1) +
//     Inner
//   - KindClassRef / KindEnumRef: Ref
//   - KindLiteral:                Literal
//   - KindUnion:                  Variants (>=1)
//   - primitives, KindNull:       nothing else.
//
// Inner / Key are pointers so the IR can hold recursive schemas:
// self-ref classes reach themselves only through a ClassRef edge,
// never through structural nesting, so the type spec itself stays
// finite. Variants is a slice so unions stay ordered: the chosen
// arm index in a FuzzValue refers to this declaration order.
type FuzzType struct {
	Kind     FuzzTypeKind `json:"kind"`
	Inner    *FuzzType    `json:"inner,omitempty"`
	Key      *FuzzType    `json:"key,omitempty"`
	Literal  *FuzzLiteral `json:"literal,omitempty"`
	Ref      string       `json:"ref,omitempty"`
	Variants []FuzzType   `json:"variants,omitempty"`
}

// FuzzProperty is one field inside a class declaration.
type FuzzProperty struct {
	Name string   `json:"name"`
	Type FuzzType `json:"type"`
}

// FuzzClass is a generated class declaration. Properties are
// declaration-ordered; emitters and the value walker preserve that
// order in their output.
type FuzzClass struct {
	Name       string         `json:"name"`
	Properties []FuzzProperty `json:"properties"`
}

// FuzzEnum is a generated enum declaration. Values are unique
// uppercase tokens (e.g. FuzzEnum0_V0).
type FuzzEnum struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// FuzzSchema is the top-level IR. Classes / Enums are declared in
// the order the generator emitted them; RootClass names the class
// the synthesized BAML function returns when the effective root is
// a class instance.
//
// RootType is the optional non-class effective root. When set it
// overrides RootClass for emission and value-walking: the
// synthesized function returns the spelled type (raw union /
// list / map / primitive), and the value walker drives the value
// as the corresponding kind. When RootType is nil the schema
// behaves exactly as v1: RootClass names the returned class. The
// fallback keeps existing corpus replay files (which serialize
// with no root_type) compatible.
//
// The Has* / Requires* flags are graph metadata derived from the
// class-ref reachability graph. They are not authoritative — call
// AnalyzeGraph to recompute. The fields exist so generators can
// stamp them once and downstream consumers don't have to recompute.
type FuzzSchema struct {
	Classes             []FuzzClass `json:"classes"`
	Enums               []FuzzEnum  `json:"enums"`
	RootClass           string      `json:"root_class"`
	RootType            *FuzzType   `json:"root_type,omitempty"`
	HasSelfRef          bool        `json:"has_self_ref"`
	HasMutualCycle      bool        `json:"has_mutual_cycle"`
	HasUnion            bool        `json:"has_union"`
	RequiresDynamicSkip bool        `json:"requires_dynamic_skip"`
}

// EffectiveRoot returns the FuzzType the schema returns from its
// synthesized function. When RootType is non-nil it is returned
// verbatim. Otherwise the result is a synthesized
// KindClassRef pointing at RootClass — the v1 default, kept so
// callers (emitters, walker, order checker) can always reason in
// terms of one return type.
func (s FuzzSchema) EffectiveRoot() FuzzType {
	if s.RootType != nil {
		return *s.RootType
	}
	return FuzzType{Kind: KindClassRef, Ref: s.RootClass}
}

// FindClass returns the named class declaration plus a found flag.
// Linear scan — class counts are bounded at 4 in v1, so a map is
// overkill.
func (s FuzzSchema) FindClass(name string) (FuzzClass, bool) {
	for _, cls := range s.Classes {
		if cls.Name == name {
			return cls, true
		}
	}
	return FuzzClass{}, false
}

// FindEnum returns the named enum declaration plus a found flag.
func (s FuzzSchema) FindEnum(name string) (FuzzEnum, bool) {
	for _, e := range s.Enums {
		if e.Name == name {
			return e, true
		}
	}
	return FuzzEnum{}, false
}
