package admission

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
	"github.com/invakid404/baml-rest/internal/schema"
)

// TestAdmittedStaticReturnShape pins the NARROWED proven-decoder return-shape set
// (review P2.1): admit exactly what the BAML v0.223 differential covers — a top-level
// string scalar and a flat class of string|int fields — and decline every shape whose
// mapper is not differential-proven. This is the runtime backstop that must stay in
// lockstep with the codegen serve-seam emission gate.
func TestAdmittedStaticReturnShape(t *testing.T) {
	prim := func(p schemadescriptor.PrimitiveKind) schemadescriptor.Type {
		return schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: p}
	}
	classBundle := func(fields ...schemadescriptor.ClassField) schemadescriptor.Bundle {
		return schemadescriptor.Bundle{
			Version: schemadescriptor.Version,
			Method:  "M",
			Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "C", Mode: schemadescriptor.NonStreaming},
			Classes: []schemadescriptor.ClassDef{{Name: schemadescriptor.Name{Name: "C"}, Mode: schemadescriptor.NonStreaming, Fields: fields}},
		}
	}
	field := func(name string, p schemadescriptor.PrimitiveKind) schemadescriptor.ClassField {
		return schemadescriptor.ClassField{Name: schemadescriptor.Name{Name: name}, Type: prim(p)}
	}

	cases := []struct {
		name string
		desc schemadescriptor.Bundle
		want bool
	}{
		{"top_level_string_ADMIT", schemadescriptor.Bundle{Version: schemadescriptor.Version, Method: "M", Target: prim(schemadescriptor.PrimitiveString)}, true},
		{"top_level_int_DECLINE", schemadescriptor.Bundle{Version: schemadescriptor.Version, Method: "M", Target: prim(schemadescriptor.PrimitiveInt)}, false},
		{"top_level_float_DECLINE", schemadescriptor.Bundle{Version: schemadescriptor.Version, Method: "M", Target: prim(schemadescriptor.PrimitiveFloat)}, false},
		{"top_level_bool_DECLINE", schemadescriptor.Bundle{Version: schemadescriptor.Version, Method: "M", Target: prim(schemadescriptor.PrimitiveBool)}, false},
		// EXACTLY StaticAnswer{answer:string, confidence:int} admits.
		{"static_answer_exact_ADMIT", classBundle(field("answer", schemadescriptor.PrimitiveString), field("confidence", schemadescriptor.PrimitiveInt)), true},
		{"flat_class_with_float_field_DECLINE", classBundle(field("answer", schemadescriptor.PrimitiveString), field("ratio", schemadescriptor.PrimitiveFloat)), false},
		{"flat_class_with_bool_field_DECLINE", classBundle(field("answer", schemadescriptor.PrimitiveString), field("flag", schemadescriptor.PrimitiveBool)), false},
		{"empty_class_DECLINE", classBundle(), false},
		// Narrowing (review P1.3): a structurally-different UNTESTED class must NOT claim.
		{"different_field_names_DECLINE", classBundle(field("count", schemadescriptor.PrimitiveInt), field("title", schemadescriptor.PrimitiveString)), false},
		{"wrong_field_order_DECLINE", classBundle(field("confidence", schemadescriptor.PrimitiveInt), field("answer", schemadescriptor.PrimitiveString)), false},
		{"three_fields_DECLINE", classBundle(field("answer", schemadescriptor.PrimitiveString), field("confidence", schemadescriptor.PrimitiveInt), field("extra", schemadescriptor.PrimitiveString)), false},
		{"one_field_answer_DECLINE", classBundle(field("answer", schemadescriptor.PrimitiveString)), false},
		{"confidence_string_type_DECLINE", classBundle(field("answer", schemadescriptor.PrimitiveString), field("confidence", schemadescriptor.PrimitiveString)), false},
		{"answer_int_type_DECLINE", classBundle(field("answer", schemadescriptor.PrimitiveInt), field("confidence", schemadescriptor.PrimitiveInt)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := schema.FromStaticDescriptor(tc.desc)
			if err != nil {
				t.Fatalf("FromStaticDescriptor: %v", err)
			}
			if got := admittedStaticReturnShape(b); got != tc.want {
				t.Errorf("admittedStaticReturnShape = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAdmittedStaticReturnShape_RecursionEnumAliasDeclines pins the FIRST guard in
// admittedStaticReturnShape: an OTHERWISE-ADMISSIBLE StaticAnswer class target that
// ALSO carries a non-empty RecursiveClasses, StructuralRecursiveAliases, or Enums
// collection declines via the early return BEFORE the shape switch — the exact
// "recursive/enum/alias declines before claim" guarantee.
//
// The base bundle is a FULLY-ADMISSIBLE StaticAnswer{answer:string, confidence:int}
// (the same shape TestAdmittedStaticReturnShape's static_answer_exact_ADMIT case
// accepts). This is load-bearing: a guard-sanity assertion first proves the base
// admits ON ITS OWN, so the decline in each subtest is attributable SOLELY to the
// injected collection — not to some other predicate (e.g. len(Classes) != 1) that
// would also fire if the early-return guard were deleted.
func TestAdmittedStaticReturnShape_RecursionEnumAliasDeclines(t *testing.T) {
	// admissibleBase builds a FRESH, validated, fully-admissible StaticAnswer bundle
	// per call so a subtest's mutation cannot leak into the others.
	admissibleBase := func(t *testing.T) *schema.Bundle {
		t.Helper()
		desc := schemadescriptor.Bundle{
			Version: schemadescriptor.Version,
			Method:  "M",
			Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "StaticAnswer", Mode: schemadescriptor.NonStreaming},
			Classes: []schemadescriptor.ClassDef{{
				Name: schemadescriptor.Name{Name: "StaticAnswer"},
				Mode: schemadescriptor.NonStreaming,
				Fields: []schemadescriptor.ClassField{
					{Name: schemadescriptor.Name{Name: "answer"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
					{Name: schemadescriptor.Name{Name: "confidence"}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveInt}},
				},
			}},
		}
		b, err := schema.FromStaticDescriptor(desc)
		if err != nil {
			t.Fatalf("FromStaticDescriptor: %v", err)
		}
		return b
	}

	// Guard sanity: the base MUST admit unaltered — otherwise a decline below could
	// stem from the shape switch rather than the early-return guard under test.
	if !admittedStaticReturnShape(admissibleBase(t)) {
		t.Fatalf("base StaticAnswer bundle must admit unaltered; the guard proof is only meaningful when the injected collection is the SOLE cause of a decline")
	}

	inject := map[string]func(*schema.Bundle){
		"recursive_classes":            func(b *schema.Bundle) { b.RecursiveClasses = []string{"Node"} },
		"structural_recursive_aliases": func(b *schema.Bundle) { b.StructuralRecursiveAliases = []schema.RecursiveAliasDef{{Name: "JsonValue"}} },
		"enums":                        func(b *schema.Bundle) { b.Enums = []schema.EnumDef{{Name: schema.Name{Name: "E"}}} },
	}
	for name, attach := range inject {
		t.Run(name, func(t *testing.T) {
			b := admissibleBase(t)
			attach(b)
			if admittedStaticReturnShape(b) {
				t.Errorf("an OTHERWISE-ADMISSIBLE StaticAnswer bundle with non-empty %s must decline via the early-return guard", name)
			}
		})
	}
}

// TestAdmittedStaticReturnShape_FieldAliasDeclines proves a class field @alias
// declines (the JSON key would diverge from the generated struct tag).
func TestAdmittedStaticReturnShape_FieldAliasDeclines(t *testing.T) {
	alias := "renamed"
	desc := schemadescriptor.Bundle{
		Version: schemadescriptor.Version,
		Method:  "M",
		Target:  schemadescriptor.Type{Kind: schemadescriptor.TypeClass, Name: "C", Mode: schemadescriptor.NonStreaming},
		Classes: []schemadescriptor.ClassDef{{
			Name: schemadescriptor.Name{Name: "C"},
			Mode: schemadescriptor.NonStreaming,
			Fields: []schemadescriptor.ClassField{
				{Name: schemadescriptor.Name{Name: "answer", Alias: &alias}, Type: schemadescriptor.Type{Kind: schemadescriptor.TypePrimitive, Primitive: schemadescriptor.PrimitiveString}},
			},
		}},
	}
	b, err := schema.FromStaticDescriptor(desc)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	if admittedStaticReturnShape(b) {
		t.Errorf("class with a field @alias must decline (unproven decoder shape)")
	}
}
