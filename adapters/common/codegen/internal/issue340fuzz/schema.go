// Package issue340fuzz hosts the shared IR + rapid generators that
// back the property-style fuzz framework for the BAML-on-Go pipeline.
// It is intentionally implementation-agnostic: the IR describes a
// schema + value pair abstractly, and downstream emitters (dynamic
// TypeBuilder, static .baml source) lower it onto the two execution
// paths that need to agree.
//
// Naming convention for generated classes/enums is deterministic:
// classes are F340_C0, F340_C1, ...; enums are F340_E0, F340_E1, ...
// Property names are F340_field_N. Stability across runs makes
// failure replay produce identical artifacts independent of map
// iteration order.
package issue340fuzz

// FuzzTypeKind enumerates every type shape the IR supports. Unions
// are intentionally excluded from v1; see issue #340 v1.1.
type FuzzTypeKind string

const (
	KindString   FuzzTypeKind = "string"
	KindInt      FuzzTypeKind = "int"
	KindFloat    FuzzTypeKind = "float"
	KindBool     FuzzTypeKind = "bool"
	KindLiteral  FuzzTypeKind = "literal"
	KindOptional FuzzTypeKind = "optional"
	KindList     FuzzTypeKind = "list"
	KindMap      FuzzTypeKind = "map"
	KindClassRef FuzzTypeKind = "class_ref"
	KindEnumRef  FuzzTypeKind = "enum_ref"
)

// FuzzLiteralKind narrows the literal payload to one of the BAML
// literal scalar kinds. Literals are stored inline on FuzzType
// rather than as a separate node so emitters can render them
// without a second indirection.
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
//   - KindOptional / KindList:  Inner
//   - KindMap:                  Key (always KindString in v1) + Inner
//   - KindClassRef / KindEnumRef: Ref
//   - KindLiteral:              Literal
//   - primitives:               nothing else.
//
// Storing Inner / Key as pointers (rather than value-embedded
// FuzzType) lets the IR hold recursive schemas (self-ref classes
// reach themselves only through a ClassRef, not through structural
// nesting, so the type itself stays finite).
type FuzzType struct {
	Kind    FuzzTypeKind `json:"kind"`
	Inner   *FuzzType    `json:"inner,omitempty"`
	Key     *FuzzType    `json:"key,omitempty"`
	Literal *FuzzLiteral `json:"literal,omitempty"`
	Ref     string       `json:"ref,omitempty"`
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
// uppercase tokens (e.g. F340_E0_V0).
type FuzzEnum struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// FuzzSchema is the top-level IR. Classes / Enums are declared in
// the order the generator emitted them; RootClass names the class
// the synthesized BAML function returns.
//
// The Has* / Requires* flags are graph metadata derived from the
// class-ref reachability graph. They are not authoritative — call
// AnalyzeGraph to recompute. The fields exist so generators can
// stamp them once and downstream consumers don't have to recompute.
type FuzzSchema struct {
	Classes             []FuzzClass `json:"classes"`
	Enums               []FuzzEnum  `json:"enums"`
	RootClass           string      `json:"root_class"`
	HasSelfRef          bool        `json:"has_self_ref"`
	HasMutualCycle      bool        `json:"has_mutual_cycle"`
	HasUnion            bool        `json:"has_union"`
	RequiresDynamicSkip bool        `json:"requires_dynamic_skip"`
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
