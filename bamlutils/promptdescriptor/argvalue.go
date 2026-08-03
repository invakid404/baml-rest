package promptdescriptor

// This file is the de-BAML Slice 7.1b PROJECTED ARGUMENT carrier: the neutral,
// ordered value tree a GENERATED projector produces from a generated call's
// concrete typed Go arguments, and the only host-value input the native static
// binder accepts.
//
// It is deliberately a SECOND passive carrier, kept separate from
// [Function.InputValues]:
//
//   - the V3 universe describes what a value MEANS (source names, canonical
//     enum identity, class field order/aliases, list element types). It is a
//     build-time source fact and stays serializable.
//   - this carrier describes what a value IS for ONE call. It cannot live on
//     the descriptor: a descriptor would then need closures or a dependency on
//     the generated client's types package, and it would stop being a passive
//     per-project fact.
//
// The split is what removes runtime reflection from the seam. V3 alone cannot
// read a concrete generated `types.Palette`; a generated projector can, with
// exact type assertions and direct field selectors, and it writes the CANONICAL
// BAML names as literals. The binder then validates this tree against V3 and
// declines on any disagreement — so neither half is trusted alone.
//
// SENSITIVE: a projected value is real request input. Never log, %v-format,
// error-wrap, or metric-label an ArgumentValue or a StaticValue.

// StaticValueKind tags the shape of one projected [StaticValue] node. It
// mirrors [ValueKind] one-for-one so a projected node and its V3 type node can
// be compared without a translation table; the two enums are separate types
// because one is a per-call VALUE fact and the other a build-time TYPE fact.
type StaticValueKind string

const (
	// StaticString / StaticInt / StaticFloat / StaticBool carry a scalar in the
	// matching shape field.
	StaticString StaticValueKind = "string"
	StaticInt    StaticValueKind = "int"
	StaticFloat  StaticValueKind = "float"
	StaticBool   StaticValueKind = "bool"
	// StaticNull is an explicit absent value (a nil pointer/slice the projector
	// found on a nullable edge). It carries no payload.
	StaticNull StaticValueKind = "null"
	// StaticEnum is an enum member: TypeName is the SOURCE enum name and
	// Canonical the CANDIDATE canonical member name the projector read from the
	// generated Go value. Both are validated against V3 before binding — the
	// projector proposes, the binder disposes.
	StaticEnum StaticValueKind = "enum"
	// StaticClass is a class value: TypeName is the SOURCE class name and Fields
	// its values in the descriptor's canonical field order.
	StaticClass StaticValueKind = "class"
	// StaticList is a list value: Items in input index order.
	StaticList StaticValueKind = "list"
)

// StaticValue is one projected argument value node. Exactly one shape field is
// meaningful per Kind; every other field is its zero value.
//
// It carries CANONICAL BAML names only. A display alias never appears here: the
// projector writes the canonical field/member name as a literal and the binder
// resolves the alias from V3, so an alias can never become a second identity.
type StaticValue struct {
	Kind StaticValueKind
	// TypeName is the source enum name (StaticEnum) or source class name
	// (StaticClass). Empty for every other kind.
	TypeName string
	// String carries the payload for StaticString.
	String string
	// Int carries the payload for StaticInt.
	Int int64
	// Float carries the payload for StaticFloat.
	Float float64
	// Bool carries the payload for StaticBool.
	Bool bool
	// Canonical is the candidate canonical member name for StaticEnum.
	Canonical string
	// Fields are a StaticClass's field values in SOURCE field order.
	Fields []StaticFieldValue
	// Items are a StaticList's element values in INPUT INDEX order.
	Items []StaticValue
}

// StaticFieldValue is one projected class field: its canonical source name and
// its value. The canonical name is emitted as a literal by the generated
// projector (never derived from a Go field identifier, a struct tag read at
// runtime, or a display alias).
type StaticFieldValue struct {
	Canonical string
	Value     StaticValue
}

// ArgumentValue is one projected top-level argument: the declared argument name
// and its value. A projector returns these as an ORDERED slice in declared
// argument order; the binder proves that order and those names against
// [Function.Args] before it binds anything.
type ArgumentValue struct {
	Name  string
	Value StaticValue
}
