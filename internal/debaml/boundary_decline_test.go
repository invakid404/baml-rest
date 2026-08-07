package debaml

import (
	"errors"
	"strings"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// M5a FALLBACK-BOUNDARY GUARDS — unit level.
//
// These pin native's decline for the fallback families the bamlfuzz
// parse-recovery DIFFERENTIAL cannot reach: constraints (@assert/@check),
// media outputs, and recursive aliases/classes. Both differential legs lower
// through bamlfuzz.LowerToDynamicSchema, and the dynamic output_schema has NO
// channel for any of these (the #572 dynamic-schema ceiling — DynamicProperty
// carries no constraint/media fields, and self-ref/mutual-cycle schemas crash
// the cgo TypeBuilder, so LowerToDynamicSchema rejects them outright). There is
// therefore no "native declines + BAML handles" pair to capture, and native's
// FromDynamicOutputSchema never even produces these shapes today — the decline
// code exists for the future static-lowering path (M5b/M5c).
//
// So, mirroring the existing pattern (coerce_array_to_singular_test.go's
// constrained-key test, which notes "the dynamic bridge has no constraint
// channel, so this is exercised by constructing the schema.Type directly"), each
// guard constructs a schema.Bundle/Type directly and drives the SAME gate Parse
// runs — checkSupported (constraints/recursive) or Bundle.ValidateOutput (media,
// which Parse wraps into the ErrDeBAMLParseUnsupported sentinel at parse.go). A
// flip of any of these to "accepted" is exactly the accidental broadening M5a
// exists to catch.

// requireCheckSupportedDeclines asserts checkSupported(b) declined with the
// fallback sentinel — the gate Parse runs (parse.go), so a decline here is a
// runtime fallback to BAML CFFI.
func requireCheckSupportedDeclines(t *testing.T, b *schema.Bundle, what string) {
	t.Helper()
	err := checkSupported(b)
	if err == nil {
		t.Fatalf("%s: checkSupported accepted the schema; expected ErrDeBAMLParseUnsupported (fallback)", what)
	}
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("%s: checkSupported returned %v; expected ErrDeBAMLParseUnsupported", what, err)
	}
}

// declineBundle builds a minimal self-contained bundle whose target is the given
// root class, with optional enums. It is the fixture shape for the checkSupported
// guards — checkSupported walks b.Classes / b.Enums directly (no index needed for
// these cases), so a well-formed root class plus the field/constraint under test
// is enough.
func declineBundle(root schema.ClassDef, enums ...schema.EnumDef) *schema.Bundle {
	if root.Name.Name == "" {
		root.Name = schema.Name{Name: "Root"}
	}
	if root.Mode == "" {
		root.Mode = schema.NonStreaming
	}
	return &schema.Bundle{
		Target:  schema.Type{Kind: schema.TypeClass, Name: root.Name.Name, Mode: schema.NonStreaming},
		Classes: []schema.ClassDef{root},
		Enums:   enums,
	}
}

func scalarField(name string, t schema.Type) schema.ClassField {
	return schema.ClassField{Name: schema.Name{Name: name}, Type: t}
}

func stringType() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
}

func intType() schema.Type {
	return schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
}

func constrained(t schema.Type, level schema.ConstraintLevel, expr string) schema.Type {
	t.Meta.Constraints = append(t.Meta.Constraints, schema.Constraint{Level: level, Expression: expr})
	return t
}

// TestNativeDeclines_Constraints pins that ANY reachable @assert/@check
// constraint forces native fallback — native never evaluates constraints, so it
// declines regardless of level (check vs assert) or whether the predicate would
// pass or fail. Covers the scope's constraint sub-cases at the decline gate:
// scalar field, list (and list element), class-level, enum-level, and — since the
// b.Target walk landed — the RETURN TYPE itself and its list element.
func TestNativeDeclines_Constraints(t *testing.T) {
	label := func(s string) *string { return &s }

	// TARGET-LEVEL. `function F() -> T @assert(...)` declares its predicate on
	// b.Target, which no class or enum entry can report; before checkSupportedFields
	// walked the target these bundles were ADMITTED and native served a value BAML
	// would not. What BAML does instead varies by shape — a bare `int` return
	// rejects the parse, a bare `string` return skips the constraint entirely — and
	// that taxonomy is measured live against stock v0.223 by
	// [TestTargetLevelConstraintDeclineClassification] and profileoracle's
	// TestStockTargetLevelConstraintDispositionAndNativeDecline. The GATE does not
	// care which: it declines on the constraint alone, at either level.
	requireCheckSupportedDeclines(t, &schema.Bundle{
		Target: constrained(intType(), schema.ConstraintAssert, "false"),
	}, "@assert on the target type")
	requireCheckSupportedDeclines(t, &schema.Bundle{
		Target: constrained(stringType(), schema.ConstraintAssert, `this == "expected"`),
	}, "@assert on a bare-string target type")

	targetCheckType := stringType()
	targetCheckType.Meta.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck, Expression: `this == "expected"`, Label: label("expected")}}
	requireCheckSupportedDeclines(t, &schema.Bundle{Target: targetCheckType}, "@check on the target type")

	// A constrained ELEMENT of a target LIST — the same gap one structural step in,
	// so the walk is pinned as a tree walk rather than a single-node check.
	requireCheckSupportedDeclines(t, &schema.Bundle{Target: schema.Type{
		Kind: schema.TypeList,
		Elem: ptr(constrained(intType(), schema.ConstraintAssert, "this > 100")),
	}}, "@assert on a target list element")

	// @assert on a scalar field.
	requireCheckSupportedDeclines(t, declineBundle(schema.ClassDef{
		Fields: []schema.ClassField{scalarField("s", constrained(stringType(), schema.ConstraintAssert, "this|length > 0"))},
	}), "@assert on scalar field")

	// @check on a scalar field (check requires a label; native still declines).
	checkField := scalarField("s", stringType())
	checkField.Type.Meta.Constraints = []schema.Constraint{{Level: schema.ConstraintCheck, Expression: "this|length > 0", Label: label("non_empty")}}
	requireCheckSupportedDeclines(t, declineBundle(schema.ClassDef{
		Fields: []schema.ClassField{checkField},
	}), "@check on scalar field")

	// @check on a list VALUE type.
	listType := schema.Type{Kind: schema.TypeList, Elem: ptr(stringType())}
	requireCheckSupportedDeclines(t, declineBundle(schema.ClassDef{
		Fields: []schema.ClassField{scalarField("xs", constrained(listType, schema.ConstraintCheck, "this|length > 0"))},
	}), "@check on list type")

	// @assert on a list ELEMENT (constraint one level deeper than the list).
	listElemConstrained := schema.Type{Kind: schema.TypeList, Elem: ptr(constrained(stringType(), schema.ConstraintAssert, "this|length > 0"))}
	requireCheckSupportedDeclines(t, declineBundle(schema.ClassDef{
		Fields: []schema.ClassField{scalarField("xs", listElemConstrained)},
	}), "@assert on list element")

	// Class-level @check (the shape BAML serializes as {value, checks}).
	requireCheckSupportedDeclines(t, declineBundle(schema.ClassDef{
		Fields:      []schema.ClassField{scalarField("s", stringType())},
		Constraints: []schema.Constraint{{Level: schema.ConstraintCheck, Expression: "this.s|length > 0", Label: label("has_s")}},
	}), "class-level @check")

	// Class-level @assert.
	requireCheckSupportedDeclines(t, declineBundle(schema.ClassDef{
		Fields:      []schema.ClassField{scalarField("s", stringType())},
		Constraints: []schema.Constraint{{Level: schema.ConstraintAssert, Expression: "this.s|length > 0"}},
	}), "class-level @assert")

	// Enum-level constraint.
	requireCheckSupportedDeclines(t, declineBundle(
		schema.ClassDef{Fields: []schema.ClassField{scalarField("c", schema.Type{Kind: schema.TypeEnum, Name: "Color"})}},
		schema.EnumDef{
			Name:        schema.Name{Name: "Color"},
			Values:      []schema.EnumValue{{Name: schema.Name{Name: "RED"}}, {Name: schema.Name{Name: "GREEN"}}},
			Constraints: []schema.Constraint{{Level: schema.ConstraintCheck, Expression: "this != \"RED\"", Label: label("not_red")}},
		},
	), "enum-level @check")

	// Control: the SAME shapes without constraints are accepted, so the guards
	// prove constraint-specificity, not a blanket decline.
	if err := checkSupported(declineBundle(schema.ClassDef{
		Fields: []schema.ClassField{scalarField("s", stringType()), scalarField("xs", schema.Type{Kind: schema.TypeList, Elem: ptr(stringType())})},
	})); err != nil {
		t.Fatalf("constraint-free control: checkSupported unexpectedly declined: %v", err)
	}

	// TARGET-LEVEL CONTROL, and the reason the target walk is a decline-MORE change
	// rather than a blanket target reject. Each of these is the SAME target shape as
	// a decline above with the constraint REMOVED, plus the two target shapes the
	// static corpus actually serves (a bare string return, a class return). All must
	// still ADMIT — the target walk looks at Meta.Constraints and nothing else, so it
	// must not import checkSupportedType's coercion-scope declines into a position
	// that was never subject to them.
	unconstrained := []struct {
		name   string
		target schema.Type
	}{
		{"bare string return", stringType()},
		{"class return", schema.Type{Kind: schema.TypeClass, Name: "Root", Mode: schema.NonStreaming}},
		{"list of string return", schema.Type{Kind: schema.TypeList, Elem: ptr(stringType())}},
		{"list of int return", schema.Type{Kind: schema.TypeList, Elem: ptr(intType())}},
		{"map return", schema.Type{Kind: schema.TypeMap, Key: ptr(stringType()), Value: ptr(intType())}},
		{"optional string return", schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{stringType()}, Nullable: true}}},
	}
	if len(unconstrained) != 6 {
		t.Fatalf("unconstrained target controls = %d, want 6", len(unconstrained))
	}
	for _, u := range unconstrained {
		b := declineBundle(schema.ClassDef{Fields: []schema.ClassField{scalarField("s", stringType())}})
		b.Target = u.target
		if err := checkSupported(b); err != nil {
			t.Errorf("unconstrained target %q: checkSupported DECLINED (%v); the target walk must "+
				"reject constraints only, never a shape", u.name, err)
		}
	}
}

// TestNativeDeclines_Media pins that a media output type forces native fallback.
// The dynamic output_schema cannot express media outputs, so this is exercised by
// constructing the Bundle directly and driving Bundle.ValidateOutput — the gate
// Parse runs at parse.go before checkSupported, whose error Parse wraps into the
// ErrDeBAMLParseUnsupported sentinel (unsupportedErr("validate schema", ...)).
func TestNativeDeclines_Media(t *testing.T) {
	for _, sub := range []schema.MediaKind{schema.MediaImage, schema.MediaAudio, schema.MediaPDF, schema.MediaVideo} {
		media := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveMedia, Media: sub}

		// Media as the output TARGET.
		targetBundle := &schema.Bundle{Target: media}
		requireMediaDeclines(t, targetBundle, "media target ("+string(sub)+")")

		// Media as a class FIELD.
		fieldBundle := declineBundle(schema.ClassDef{Fields: []schema.ClassField{scalarField("m", media)}})
		requireMediaDeclines(t, fieldBundle, "media field ("+string(sub)+")")
	}
}

// requireMediaDeclines asserts Bundle.ValidateOutput rejects the media type and
// that Parse's wrapping of that rejection is the fallback sentinel.
func requireMediaDeclines(t *testing.T, b *schema.Bundle, what string) {
	t.Helper()
	err := b.ValidateOutput()
	if err == nil {
		t.Fatalf("%s: ValidateOutput accepted media as an output type; expected rejection", what)
	}
	if !strings.Contains(err.Error(), "media") {
		t.Fatalf("%s: ValidateOutput rejected but not for media: %v", what, err)
	}
	// Parse converts this rejection into the fallback sentinel (parse.go:
	// unsupportedErr("validate schema", err)); confirm the wrap is the sentinel.
	if wrapped := unsupportedErr("validate schema", err); !errors.Is(wrapped, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("%s: Parse's validate-schema wrap is not ErrDeBAMLParseUnsupported: %v", what, wrapped)
	}
}

// TestNativeDeclines_RecursiveClass pins that a recursive class forces native
// fallback. The dynamic TypeBuilder cannot express a self-referential class
// (LowerToDynamicSchema rejects it — the cgo TypeBuilder crashes), so this is
// exercised by setting Bundle.RecursiveClasses directly, the flag checkSupported
// declines on.
func TestNativeDeclines_RecursiveClass(t *testing.T) {
	b := declineBundle(schema.ClassDef{
		Name:   schema.Name{Name: "Node"},
		Fields: []schema.ClassField{scalarField("next", schema.Type{Kind: schema.TypeClass, Name: "Node", Mode: schema.NonStreaming})},
	})
	b.RecursiveClasses = []string{"Node"}
	requireCheckSupportedDeclines(t, b, "recursive class")
}

// TestNativeDeclines_RecursiveAlias pins that a structural recursive alias forces
// native fallback, both when present in Bundle.StructuralRecursiveAliases and
// when a field is typed as a TypeRecursiveAlias reference. Recursive aliases have
// no dynamic output_schema representation at all, so both are constructed
// directly.
func TestNativeDeclines_RecursiveAlias(t *testing.T) {
	// Present in the bundle's structural-recursive-alias set.
	b := declineBundle(schema.ClassDef{Fields: []schema.ClassField{scalarField("s", stringType())}})
	b.StructuralRecursiveAliases = []schema.RecursiveAliasDef{{
		Name:   "JsonValue",
		Target: schema.Type{Kind: schema.TypeList, Elem: ptr(schema.Type{Kind: schema.TypeRecursiveAlias, Name: "JsonValue"})},
	}}
	requireCheckSupportedDeclines(t, b, "structural recursive alias (bundle set)")

	// A field typed directly as a recursive-alias reference.
	requireCheckSupportedDeclines(t, declineBundle(schema.ClassDef{
		Fields: []schema.ClassField{scalarField("a", schema.Type{Kind: schema.TypeRecursiveAlias, Name: "JsonValue"})},
	}), "recursive-alias-typed field")
}

// ptr returns a pointer to v, for the *Type fields (Elem/Key/Value).
func ptr(v schema.Type) *schema.Type { return &v }
