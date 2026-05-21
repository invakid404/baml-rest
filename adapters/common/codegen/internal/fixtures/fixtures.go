// Package fixtures declares the BAML-side type fixtures the
// codegen compile-matrix test feeds to ensureMirrorStruct /
// makePreambleWithArgs. The package is non-test (regular .go file)
// so the rendered output the matrix test writes to a temp module can
// import it for type references like `fixtures.Image` or
// `fixtures.MessageA`.
//
// The types intentionally have no behavior — only field shapes
// matter. `Image` stands in for BAML media types (the matrix
// configures pkgs.BamlPkg to point at this package so
// `isMediaReflectType` resolves Image as a media field). Every
// `*Image` field on a struct marks that struct as "contains media",
// which triggers the mirror-struct / convert-function emission path
// the matrix exercises.
package fixtures

// Image stands in for a BAML media type. The codegen detects it as
// media because (a) its declared name "Image" is in `mediaTypeNames`
// and (b) the matrix test sets pkgs.BamlPkg = this package's import
// path.
type Image struct{}

// ContentPartA is the value-element nested type used by MessageA's
// `*[]ContentPartA` field. Triggers the closure-context pool branch
// in nestedStructConversion.
type ContentPartA struct {
	Img *Image
}

// MessageA pairs with ContentPartA: a struct with a `*[]Struct`
// value-element field. Its converter pools the parts slice through
// the slice pool and takes the closure-context `ownedNested
// *[]func()` parameter.
type MessageA struct {
	Parts *[]ContentPartA
}

// ContentPartB is the pointer-element variant used by MessageB. The
// `*[]*ContentPartB` field falls through nestedStructConversion's
// pointer-element guard to the legacy `make` path; the converter
// does NOT take ownedNested.
type ContentPartB struct {
	Img *Image
}

// MessageB has a `*[]*Struct` pointer-element field — the F2-guard
// fallback shape. Its converter does NOT take ownedNested because
// nestedStructConversion skips the pool branch for pointer
// elements.
type MessageB struct {
	Parts *[]*ContentPartB
}

// ContentPartC + ToolPartC are the two distinct nested types used
// by MessageC to exercise the F2 multi-pooled-types shape.
type ContentPartC struct {
	Img *Image
}

type ToolPartC struct {
	Img *Image
}

// MessageC has two value-element pooled fields with DISTINCT inner
// types. The closure-context shape from F2's refactor lets both
// fields append release closures to a single `ownedNested
// *[]func()` slice.
type MessageC struct {
	Parts *[]ContentPartC
	Tools *[]ToolPartC
}

// Class<X> / Other<X> mirror Message<X> for the matrix's
// multi-param top-level shapes (e.g. `Method(messages []Message,
// classes []Class)` for two-pooled-params coverage). They duplicate
// the same field shapes under distinct names so each test cell can
// register an independent pool without sharing helper symbols.
type ClassA struct {
	Parts *[]ContentPartA
}

type ClassB struct {
	Parts *[]*ContentPartB
}

type ClassC struct {
	Parts *[]ContentPartC
	Tools *[]ToolPartC
}

type OtherA struct {
	Parts *[]ContentPartA
}

type OtherB struct {
	Parts *[]*ContentPartB
}

type OtherC struct {
	Parts *[]ContentPartC
	Tools *[]ToolPartC
}

// CycleA + CycleB form a mutually-recursive media-bearing pair used
// by the cycle regression test. CycleA directly pools (its `Parts`
// field is `*[]CyclePartWithMedia` with value elements). CycleB
// references *CycleA, and CycleA references *CycleB.
//
// Without the precompute pass, generation order is:
//   ensure(CycleA) -> mark generated, recurse into *CycleB
//     ensure(CycleB) -> mark generated, recurse into *CycleA
//       ensure(CycleA) -> already generated, return cached name
//     emit body of CycleB -> snapshot
//       convertNeedsOwnedNestedFor(CycleA) == false (A hasn't
//       finished its own analysis yet) so the call site for the
//       A field in B is 2-arg
//     CycleB body done, generateConversionFunc sets needs for B
//   emit body of CycleA -> sets needs for A
// The rendered Go has B's 2-arg call site against A's 3-arg
// signature -> compile error.
//
// With the precompute pass: both A and B are flagged BEFORE any
// body is emitted, so every call site sees the final transitive
// answer.
type CyclePartWithMedia struct {
	Img *Image
}

type CycleA struct {
	B     *CycleB
	Parts *[]CyclePartWithMedia
}

type CycleB struct {
	A *CycleA
}
