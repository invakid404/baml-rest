package schema

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	desc "github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

var updateGolden = flag.Bool("update", false, "regenerate testdata/static_descriptor goldens from the lowered bundles")

// --- descriptor constructors (mirror the internal fixtures_test builders,
// but produce schemadescriptor values so the lowering path is exercised) ----

func descBundle(target desc.Type, enums []desc.EnumDef, classes []desc.ClassDef, recursive []string, aliases []desc.RecursiveAliasDef) desc.Bundle {
	return desc.Bundle{
		Version:                    desc.Version,
		Target:                     target,
		Enums:                      enums,
		Classes:                    classes,
		RecursiveClasses:           recursive,
		StructuralRecursiveAliases: aliases,
	}
}

func dString() desc.Type { return desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveString} }
func dInt() desc.Type    { return desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveInt} }
func dFloat() desc.Type  { return desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveFloat} }
func dBool() desc.Type   { return desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveBool} }
func dNull() desc.Type   { return desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveNull} }
func dTop() desc.Type    { return desc.Type{Kind: desc.TypeTop} }

func dMedia(m desc.MediaKind) desc.Type {
	return desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveMedia, Media: m}
}

func dEnumRef(name string) desc.Type { return desc.Type{Kind: desc.TypeEnum, Name: name} }
func dClassRef(name string) desc.Type {
	return desc.Type{Kind: desc.TypeClass, Name: name, Mode: desc.NonStreaming}
}
func dStreamClassRef(name string) desc.Type {
	return desc.Type{Kind: desc.TypeClass, Name: name, Mode: desc.Streaming}
}
func dAliasRef(name string) desc.Type {
	return desc.Type{Kind: desc.TypeRecursiveAlias, Name: name, Mode: desc.NonStreaming}
}

// typePtr returns a heap copy of t, for building hand-crafted descriptor Type
// nodes (including malformed ones) that need a *Type child.
func typePtr(t desc.Type) *desc.Type { return &t }

func dList(elem desc.Type) desc.Type { return desc.Type{Kind: desc.TypeList, Elem: &elem} }
func dMap(k, v desc.Type) desc.Type  { return desc.Type{Kind: desc.TypeMap, Key: &k, Value: &v} }

func dUnion(nullable bool, variants ...desc.Type) desc.Type {
	return desc.Type{Kind: desc.TypeUnion, Union: &desc.UnionType{Variants: variants, Nullable: nullable}}
}

func dLitStr(s string) desc.Type {
	return desc.Type{Kind: desc.TypeLiteral, Literal: &desc.LiteralValue{Kind: desc.LiteralString, String: s}}
}
func dLitInt(n int64) desc.Type {
	return desc.Type{Kind: desc.TypeLiteral, Literal: &desc.LiteralValue{Kind: desc.LiteralInt, Int: n}}
}
func dLitBool(b bool) desc.Type {
	return desc.Type{Kind: desc.TypeLiteral, Literal: &desc.LiteralValue{Kind: desc.LiteralBool, Bool: b}}
}

func dTuple(items ...desc.Type) desc.Type { return desc.Type{Kind: desc.TypeTuple, Items: items} }
func dArrow(ret desc.Type, params ...desc.Type) desc.Type {
	return desc.Type{Kind: desc.TypeArrow, Arrow: &desc.ArrowType{Params: params, Return: ret}}
}

// mustLower lowers a descriptor bundle and fails the test on any error.
func mustLower(t *testing.T, d desc.Bundle) *Bundle {
	t.Helper()
	b, err := FromStaticDescriptor(d)
	if err != nil {
		t.Fatalf("FromStaticDescriptor: %v", err)
	}
	return b
}

// mustErrContains asserts err is non-nil and its message contains sub.
func mustErrContains(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", sub)
	}
	if !strings.Contains(err.Error(), sub) {
		t.Fatalf("error %q does not contain %q", err.Error(), sub)
	}
}

// eqStrPtr reports whether two *string carry the same nil-vs-present state
// and, when present, the same value.
func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func kindsOf(ts []Type) []TypeKind {
	out := make([]TypeKind, len(ts))
	for i := range ts {
		out[i] = ts[i].Kind
	}
	return out
}

func equalKinds(a, b []TypeKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- preservation tests -----------------------------------------------------

func TestFromStaticDescriptorPreservesAliasesAndDescriptions(t *testing.T) {
	d := descBundle(
		dClassRef("Person"),
		[]desc.EnumDef{{
			Name: desc.Name{Name: "Status", Alias: ptr("StatusAlias")},
			Values: []desc.EnumValue{
				{Name: desc.Name{Name: "OK"}, Description: ptr("all good")},
				{Name: desc.Name{Name: "WARN", Alias: ptr("warn")}},
				// Alias present-but-empty must survive as a non-nil "" pointer,
				// distinct from the nil alias on OK/WARN's description.
				{Name: desc.Name{Name: "BLANK", Alias: ptr("")}, Description: ptr("")},
			},
		}},
		[]desc.ClassDef{{
			Name:        desc.Name{Name: "Person", Alias: ptr("PersonAlias")},
			Description: ptr("a person"),
			Mode:        desc.NonStreaming,
			Fields: []desc.ClassField{
				{Name: desc.Name{Name: "name", Alias: ptr("full_name")}, Type: dString(), Description: ptr("the name")},
				{Name: desc.Name{Name: "age"}, Type: dInt()},
				{Name: desc.Name{Name: "status"}, Type: dEnumRef("Status")},
			},
		}},
		nil, nil,
	)

	b := mustLower(t, d)

	cls, ok := b.FindClass("Person", NonStreaming)
	if !ok {
		t.Fatal("Person class not found")
	}
	if !eqStrPtr(cls.Name.Alias, ptr("PersonAlias")) {
		t.Errorf("class alias = %v, want PersonAlias", cls.Name.Alias)
	}
	if !eqStrPtr(cls.Description, ptr("a person")) {
		t.Errorf("class description = %v, want 'a person'", cls.Description)
	}

	name, _ := cls.Field("name")
	if !eqStrPtr(name.Name.Alias, ptr("full_name")) {
		t.Errorf("name alias = %v, want full_name", name.Name.Alias)
	}
	if !eqStrPtr(name.Description, ptr("the name")) {
		t.Errorf("name description = %v, want 'the name'", name.Description)
	}
	age, _ := cls.Field("age")
	if age.Name.Alias != nil {
		t.Errorf("age alias = %v, want nil", age.Name.Alias)
	}
	if age.Description != nil {
		t.Errorf("age description = %v, want nil", age.Description)
	}

	en, ok := b.FindEnum("Status")
	if !ok {
		t.Fatal("Status enum not found")
	}
	if !eqStrPtr(en.Name.Alias, ptr("StatusAlias")) {
		t.Errorf("enum alias = %v, want StatusAlias", en.Name.Alias)
	}
	ok1, _ := en.Value("OK")
	if !eqStrPtr(ok1.Description, ptr("all good")) {
		t.Errorf("OK description = %v, want 'all good'", ok1.Description)
	}
	if ok1.Name.Alias != nil {
		t.Errorf("OK alias = %v, want nil (absent)", ok1.Name.Alias)
	}
	blank, _ := en.Value("BLANK")
	if blank.Name.Alias == nil || *blank.Name.Alias != "" {
		t.Errorf("BLANK alias = %v, want non-nil empty string (nil-vs-empty must be preserved)", blank.Name.Alias)
	}
	if blank.Description == nil || *blank.Description != "" {
		t.Errorf("BLANK description = %v, want non-nil empty string", blank.Description)
	}
}

func TestFromStaticDescriptorDeepCopy(t *testing.T) {
	d := descBundle(
		dClassRef("C"),
		[]desc.EnumDef{{
			Name:        desc.Name{Name: "E"},
			Values:      []desc.EnumValue{{Name: desc.Name{Name: "A"}, Description: ptr("first")}},
			Constraints: []desc.Constraint{{Level: desc.ConstraintCheck, Expression: "enum expr", Label: ptr("enum label")}},
		}},
		[]desc.ClassDef{{
			Name:        desc.Name{Name: "C"},
			Description: ptr("orig desc"),
			Mode:        desc.NonStreaming,
			Fields: []desc.ClassField{{
				Name: desc.Name{Name: "e"},
				// Enum ref carrying a type-level (meta) constraint, so the
				// type-meta constraint deep-copy path is exercised too.
				Type: desc.Type{
					Kind: desc.TypeEnum,
					Name: "E",
					Meta: desc.TypeMeta{Constraints: []desc.Constraint{{Level: desc.ConstraintCheck, Expression: "field expr", Label: ptr("field label")}}},
				},
			}},
			Constraints: []desc.Constraint{{Level: desc.ConstraintAssert, Expression: "class expr", Label: ptr("class label")}},
		}},
		[]string{"C"}, nil,
	)

	b := mustLower(t, d)

	// Capture the source constraint label pointers before mutating, so the
	// lowered bundle's labels can be checked for pointer-identity isolation.
	origClassLabel := d.Classes[0].Constraints[0].Label
	origEnumLabel := d.Enums[0].Constraints[0].Label
	origFieldLabel := d.Classes[0].Fields[0].Type.Meta.Constraints[0].Label

	// Mutate every shared reference in the source descriptor after lowering,
	// including the constraint Expression strings and Label pointer targets.
	*d.Classes[0].Description = "MUTATED"
	*d.Enums[0].Values[0].Description = "MUTATED"
	d.Enums[0].Values[0].Name.Name = "MUTATED"
	d.RecursiveClasses[0] = "MUTATED"
	d.Classes[0].Constraints[0].Expression = "MUTATED"
	*d.Classes[0].Constraints[0].Label = "MUTATED"
	d.Enums[0].Constraints[0].Expression = "MUTATED"
	*d.Enums[0].Constraints[0].Label = "MUTATED"
	d.Classes[0].Fields[0].Type.Meta.Constraints[0].Expression = "MUTATED"
	*d.Classes[0].Fields[0].Type.Meta.Constraints[0].Label = "MUTATED"

	cls, _ := b.FindClass("C", NonStreaming)
	if cls.Description == nil || *cls.Description != "orig desc" {
		t.Errorf("class description = %v, deep copy did not isolate pointer", cls.Description)
	}
	en, _ := b.FindEnum("E")
	if en.Values[0].Name.Name != "A" {
		t.Errorf("enum value name = %q, deep copy did not isolate slice element", en.Values[0].Name.Name)
	}
	if en.Values[0].Description == nil || *en.Values[0].Description != "first" {
		t.Errorf("enum value description mutated through shared pointer: %v", en.Values[0].Description)
	}
	if b.RecursiveClasses[0] != "C" {
		t.Errorf("recursive class = %q, deep copy did not isolate slice", b.RecursiveClasses[0])
	}

	// Constraint payloads (opaque Expression string + Label pointer) must be
	// deep-copied on every carrier: class, enum, and type-level meta.
	if got := cls.Constraints[0]; got.Expression != "class expr" || !eqStrPtr(got.Label, ptr("class label")) {
		t.Errorf("class constraint mutated through shared payload: %+v", got)
	}
	if cls.Constraints[0].Label == origClassLabel {
		t.Errorf("class constraint label pointer was shared, not deep-copied")
	}
	if got := en.Constraints[0]; got.Expression != "enum expr" || !eqStrPtr(got.Label, ptr("enum label")) {
		t.Errorf("enum constraint mutated through shared payload: %+v", got)
	}
	if en.Constraints[0].Label == origEnumLabel {
		t.Errorf("enum constraint label pointer was shared, not deep-copied")
	}
	fe, _ := cls.Field("e")
	if got := fe.Type.Meta.Constraints[0]; got.Expression != "field expr" || !eqStrPtr(got.Label, ptr("field label")) {
		t.Errorf("field type constraint mutated through shared payload: %+v", got)
	}
	if fe.Type.Meta.Constraints[0].Label == origFieldLabel {
		t.Errorf("field type constraint label pointer was shared, not deep-copied")
	}
}

func TestFromStaticDescriptorConstraintsOpaque(t *testing.T) {
	d := descBundle(
		dClassRef("C"),
		[]desc.EnumDef{{
			Name:        desc.Name{Name: "E"},
			Values:      []desc.EnumValue{{Name: desc.Name{Name: "A"}}},
			Constraints: []desc.Constraint{{Level: desc.ConstraintCheck, Expression: "this|length > 0", Label: ptr("nonempty")}},
		}},
		[]desc.ClassDef{{
			Name:        desc.Name{Name: "C"},
			Mode:        desc.NonStreaming,
			Constraints: []desc.Constraint{{Level: desc.ConstraintAssert, Expression: "this.n > 0"}},
			Fields: []desc.ClassField{{
				Name: desc.Name{Name: "n"},
				Type: desc.Type{
					Kind:      desc.TypePrimitive,
					Primitive: desc.PrimitiveInt,
					Meta:      desc.TypeMeta{Constraints: []desc.Constraint{{Level: desc.ConstraintCheck, Expression: "{{ this < 100 }}", Label: ptr("small")}}},
				},
			}},
		}},
		nil, nil,
	)

	b := mustLower(t, d)

	en, _ := b.FindEnum("E")
	if len(en.Constraints) != 1 || en.Constraints[0].Level != ConstraintCheck ||
		en.Constraints[0].Expression != "this|length > 0" || !eqStrPtr(en.Constraints[0].Label, ptr("nonempty")) {
		t.Errorf("enum constraint not preserved opaquely: %+v", en.Constraints)
	}

	cls, _ := b.FindClass("C", NonStreaming)
	if len(cls.Constraints) != 1 || cls.Constraints[0].Level != ConstraintAssert ||
		cls.Constraints[0].Expression != "this.n > 0" || cls.Constraints[0].Label != nil {
		t.Errorf("class constraint not preserved opaquely: %+v", cls.Constraints)
	}

	n, _ := cls.Field("n")
	if len(n.Type.Meta.Constraints) != 1 || n.Type.Meta.Constraints[0].Level != ConstraintCheck ||
		n.Type.Meta.Constraints[0].Expression != "{{ this < 100 }}" || !eqStrPtr(n.Type.Meta.Constraints[0].Label, ptr("small")) {
		t.Errorf("type-level constraint not preserved opaquely: %+v", n.Type.Meta.Constraints)
	}
}

func TestFromStaticDescriptorMapValidKeys(t *testing.T) {
	cases := map[string]desc.Type{
		"string_key":  dMap(dString(), dInt()),
		"enum_key":    dMap(dEnumRef("K"), dInt()),
		"literal_key": dMap(dLitStr("only"), dInt()),
		"literal_union_key": dMap(
			dUnion(false, dLitStr("a"), dLitStr("b")),
			dInt(),
		),
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			d := descBundle(target,
				[]desc.EnumDef{{Name: desc.Name{Name: "K"}, Values: []desc.EnumValue{{Name: desc.Name{Name: "X"}}}}},
				nil, nil, nil)
			b := mustLower(t, d)
			if b.Target.Kind != TypeMap {
				t.Fatalf("target kind = %q, want map", b.Target.Kind)
			}
		})
	}
}

func TestFromStaticDescriptorUnionsOptionalLiterals(t *testing.T) {
	// optional(int): a single-variant nullable union that must NOT be
	// collapsed to a bare int — the descriptor is already normalised and
	// lowering preserves its shape verbatim.
	optional := mustLower(t, descBundle(dUnion(true, dInt()), nil, nil, nil, nil))
	if optional.Target.Kind != TypeUnion || optional.Target.Union == nil {
		t.Fatalf("optional target = %+v, want a union", optional.Target)
	}
	if !optional.Target.Union.Nullable || len(optional.Target.Union.Variants) != 1 {
		t.Errorf("optional union = %+v, want single-variant nullable", optional.Target.Union)
	}

	// Ordered, non-nullable union of >= 2 variants, preserving variant order.
	multi := mustLower(t, descBundle(dUnion(false, dInt(), dString(), dBool()), nil, nil, nil, nil))
	if multi.Target.Union.Nullable {
		t.Errorf("multi union unexpectedly nullable")
	}
	if got := kindsOf(multi.Target.Union.Variants); !equalKinds(got, []TypeKind{TypePrimitive, TypePrimitive, TypePrimitive}) {
		t.Errorf("multi union kinds = %v, want 3 primitives in order", got)
	}
	if multi.Target.Union.Variants[0].Primitive != PrimitiveInt ||
		multi.Target.Union.Variants[1].Primitive != PrimitiveString ||
		multi.Target.Union.Variants[2].Primitive != PrimitiveBool {
		t.Errorf("multi union variant order not preserved: %+v", multi.Target.Union.Variants)
	}

	// Multi-variant union that is ALSO nullable: variant order and the
	// Nullable marker must both survive (Nullable is not folded into the
	// variant list, and the variants keep their descriptor order).
	multiNull := mustLower(t, descBundle(dUnion(true, dString(), dInt()), nil, nil, nil, nil))
	if !multiNull.Target.Union.Nullable {
		t.Errorf("nullable multi union lost its Nullable marker")
	}
	if len(multiNull.Target.Union.Variants) != 2 ||
		multiNull.Target.Union.Variants[0].Primitive != PrimitiveString ||
		multiNull.Target.Union.Variants[1].Primitive != PrimitiveInt {
		t.Errorf("nullable multi union variants = %+v, want [string, int]", multiNull.Target.Union.Variants)
	}

	// Literal union carrying all three literal kinds.
	lits := mustLower(t, descBundle(dUnion(true, dLitStr("s"), dLitInt(7), dLitBool(true)), nil, nil, nil, nil))
	vs := lits.Target.Union.Variants
	if len(vs) != 3 {
		t.Fatalf("literal union variants = %d, want 3", len(vs))
	}
	if vs[0].Literal == nil || vs[0].Literal.Kind != LiteralString || vs[0].Literal.String != "s" {
		t.Errorf("variant 0 = %+v, want string literal 's'", vs[0].Literal)
	}
	if vs[1].Literal == nil || vs[1].Literal.Kind != LiteralInt || vs[1].Literal.Int != 7 {
		t.Errorf("variant 1 = %+v, want int literal 7", vs[1].Literal)
	}
	if vs[2].Literal == nil || vs[2].Literal.Kind != LiteralBool || vs[2].Literal.Bool != true {
		t.Errorf("variant 2 = %+v, want bool literal true", vs[2].Literal)
	}
	if !lits.Target.Union.Nullable {
		t.Errorf("literal union should stay nullable")
	}
}

func TestFromStaticDescriptorRecursive(t *testing.T) {
	// Recursive class: Node.next -> optional(Node), declared recursive.
	classesBundle := descBundle(
		dClassRef("Node"), nil,
		[]desc.ClassDef{{
			Name: desc.Name{Name: "Node"},
			Mode: desc.NonStreaming,
			Fields: []desc.ClassField{
				{Name: desc.Name{Name: "data"}, Type: dInt()},
				{Name: desc.Name{Name: "next"}, Type: dUnion(true, dClassRef("Node"))},
			},
		}},
		[]string{"Node"}, nil,
	)
	nodes := mustLower(t, classesBundle)
	if len(nodes.RecursiveClasses) != 1 || nodes.RecursiveClasses[0] != "Node" {
		t.Errorf("recursive classes = %v, want [Node]", nodes.RecursiveClasses)
	}

	// Structural recursive alias: JsonValue = string | JsonValue[] | map<string, JsonValue>.
	aliasBundle := descBundle(
		dAliasRef("JsonValue"), nil, nil, nil,
		[]desc.RecursiveAliasDef{{
			Name: "JsonValue",
			Target: dUnion(false,
				dString(),
				dList(dAliasRef("JsonValue")),
				dMap(dString(), dAliasRef("JsonValue")),
			),
		}},
	)
	aliases := mustLower(t, aliasBundle)
	if len(aliases.StructuralRecursiveAliases) != 1 {
		t.Fatalf("aliases = %d, want 1", len(aliases.StructuralRecursiveAliases))
	}
	if _, ok := aliases.FindRecursiveAlias("JsonValue"); !ok {
		t.Errorf("JsonValue alias not indexed")
	}
}

func TestFromStaticDescriptorStreaming(t *testing.T) {
	d := descBundle(
		dStreamClassRef("S"), nil,
		[]desc.ClassDef{{
			Name:   desc.Name{Name: "S"},
			Mode:   desc.Streaming,
			Stream: desc.StreamingBehavior{Needed: true, Done: true, State: true},
			Fields: []desc.ClassField{{
				Name: desc.Name{Name: "chunk"},
				Type: desc.Type{
					Kind:      desc.TypePrimitive,
					Primitive: desc.PrimitiveString,
					Meta:      desc.TypeMeta{Stream: desc.StreamingBehavior{Needed: true}},
				},
				StreamingNeeded: true,
			}},
		}},
		nil, nil,
	)

	b := mustLower(t, d)
	cls, ok := b.FindClass("S", Streaming)
	if !ok {
		t.Fatal("streaming class S not found")
	}
	if cls.Mode != Streaming {
		t.Errorf("class mode = %q, want streaming", cls.Mode)
	}
	if cls.Stream != (StreamingBehavior{Needed: true, Done: true, State: true}) {
		t.Errorf("class stream behavior = %+v, want all true", cls.Stream)
	}
	f, _ := cls.Field("chunk")
	if !f.StreamingNeeded {
		t.Errorf("field StreamingNeeded = false, want true")
	}
	if f.Type.Meta.Stream != (StreamingBehavior{Needed: true}) {
		t.Errorf("field type stream = %+v, want {Needed:true}", f.Type.Meta.Stream)
	}
}

// TestFromStaticDescriptorRejectsStrayMedia proves a Media subtype set on a
// NON-media primitive is REJECTED, not silently dropped: an incompatible
// payload field is a generator-drift signal the lowering must fail closed on.
func TestFromStaticDescriptorRejectsStrayMedia(t *testing.T) {
	stray := desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveString, Media: desc.MediaImage}
	_, err := FromStaticDescriptor(descBundle(stray, nil, nil, nil, nil))
	mustErrContains(t, err, `field "media" is not valid for kind "primitive"`)
}

// TestFromStaticDescriptorRejectsInlinePointerCycles proves a descriptor whose
// inline type pointers (Elem/Key/Value/Union/Arrow) form a cycle fails closed
// instead of recursing to a stack overflow. Legitimate recursion must go
// through a named ref, which is covered by TestFromStaticDescriptorRecursive.
func TestFromStaticDescriptorRejectsInlinePointerCycles(t *testing.T) {
	t.Run("list_elem_self", func(t *testing.T) {
		node := &desc.Type{Kind: desc.TypeList}
		node.Elem = node
		_, err := FromStaticDescriptor(descBundle(*node, nil, nil, nil, nil))
		mustErrContains(t, err, "inline type pointer cycle")
	})

	t.Run("map_value_self", func(t *testing.T) {
		node := &desc.Type{Kind: desc.TypeMap, Key: &desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveString}}
		node.Value = node
		_, err := FromStaticDescriptor(descBundle(*node, nil, nil, nil, nil))
		mustErrContains(t, err, "inline type pointer cycle")
	})

	t.Run("union_self", func(t *testing.T) {
		u := &desc.UnionType{}
		u.Variants = []desc.Type{{Kind: desc.TypeUnion, Union: u}}
		_, err := FromStaticDescriptor(descBundle(desc.Type{Kind: desc.TypeUnion, Union: u}, nil, nil, nil, nil))
		mustErrContains(t, err, "inline union pointer cycle")
	})

	t.Run("arrow_self", func(t *testing.T) {
		a := &desc.ArrowType{}
		a.Params = []desc.Type{{Kind: desc.TypeArrow, Arrow: a}}
		_, err := FromStaticDescriptor(descBundle(desc.Type{Kind: desc.TypeArrow, Arrow: a}, nil, nil, nil, nil))
		mustErrContains(t, err, "inline arrow pointer cycle")
	})

	t.Run("deep_cycle_through_field", func(t *testing.T) {
		// Cycle reached only after descending target -> class field -> list.
		node := &desc.Type{Kind: desc.TypeList}
		node.Elem = node
		d := descBundle(dClassRef("C"), nil,
			[]desc.ClassDef{{
				Name:   desc.Name{Name: "C"},
				Mode:   desc.NonStreaming,
				Fields: []desc.ClassField{{Name: desc.Name{Name: "f"}, Type: *node}},
			}}, nil, nil)
		_, err := FromStaticDescriptor(d)
		mustErrContains(t, err, "inline type pointer cycle")
	})
}

// TestFromStaticDescriptorRejectsInlineSliceCycles proves a descriptor whose
// []Type VALUE slices (tuple Items / arrow Params) alias an ancestor's backing
// array fails closed. These cycles never repeat a tracked *Type/*UnionType/
// *ArrowType pointer, so only the backing-array guard catches them.
func TestFromStaticDescriptorRejectsInlineSliceCycles(t *testing.T) {
	t.Run("tuple_items_self", func(t *testing.T) {
		items := make([]desc.Type, 1)
		items[0] = desc.Type{Kind: desc.TypeTuple, Items: items} // Items aliases its own backing array
		_, err := FromStaticDescriptor(descBundle(desc.Type{Kind: desc.TypeTuple, Items: items}, nil, nil, nil, nil))
		mustErrContains(t, err, "slice cycle")
	})

	t.Run("arrow_params_self", func(t *testing.T) {
		// Two DISTINCT *ArrowType pointers sharing one Params backing array, so
		// the *ArrowType guard never fires and only the slice guard catches it.
		params := make([]desc.Type, 1)
		params[0] = desc.Type{Kind: desc.TypeArrow, Arrow: &desc.ArrowType{Params: params, Return: dInt()}}
		outer := desc.Type{Kind: desc.TypeArrow, Arrow: &desc.ArrowType{Params: params, Return: dInt()}}
		_, err := FromStaticDescriptor(descBundle(outer, nil, nil, nil, nil))
		mustErrContains(t, err, "slice cycle")
	})

	t.Run("tuple_items_via_arrow_params", func(t *testing.T) {
		// A tuple whose Items IS an arrow's Params backing array: the cycle
		// crosses slice carriers and must still be caught.
		shared := make([]desc.Type, 1)
		shared[0] = desc.Type{Kind: desc.TypeTuple, Items: shared}
		outer := desc.Type{Kind: desc.TypeArrow, Arrow: &desc.ArrowType{Params: shared, Return: dInt()}}
		_, err := FromStaticDescriptor(descBundle(outer, nil, nil, nil, nil))
		mustErrContains(t, err, "slice cycle")
	})

	t.Run("same_range_full_reentry", func(t *testing.T) {
		// A multi-element slice whose inner tuple Items is the FULL outer slice
		// (same backing head AND same length): the (head, len) range key must
		// still catch this genuine re-entry, proving the narrowed key did not
		// weaken real-cycle detection.
		shared := make([]desc.Type, 2)
		shared[0] = desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveInt}
		shared[1] = desc.Type{Kind: desc.TypeTuple, Items: shared} // full slice, len 2
		_, err := FromStaticDescriptor(descBundle(desc.Type{Kind: desc.TypeTuple, Items: shared}, nil, nil, nil, nil))
		mustErrContains(t, err, "slice cycle")
	})
}

// TestFromStaticDescriptorAcceptsNonCyclicSharedSlices proves the slice guard
// is path-scoped, not a global visited set: a valid empty tuple lowers, and a
// backing array reused across SIBLING fields (a finite DAG, not a cycle) is not
// falsely rejected.
func TestFromStaticDescriptorAcceptsNonCyclicSharedSlices(t *testing.T) {
	t.Run("empty_tuple_lowers", func(t *testing.T) {
		b := mustLower(t, descBundle(desc.Type{Kind: desc.TypeTuple, Items: []desc.Type{}}, nil, nil, nil, nil))
		if b.Target.Kind != TypeTuple {
			t.Fatalf("target kind = %q, want tuple", b.Target.Kind)
		}
		if len(b.Target.Items) != 0 {
			t.Errorf("empty tuple lowered to %d items, want 0", len(b.Target.Items))
		}
	})

	t.Run("sibling_shared_backing_array", func(t *testing.T) {
		// Both fields' tuple Items point at the same backing array. Because the
		// guard backtracks after each field, the second field is not a cycle.
		shared := []desc.Type{dInt(), dString()}
		d := descBundle(dClassRef("C"), nil,
			[]desc.ClassDef{{
				Name: desc.Name{Name: "C"},
				Mode: desc.NonStreaming,
				Fields: []desc.ClassField{
					{Name: desc.Name{Name: "a"}, Type: desc.Type{Kind: desc.TypeTuple, Items: shared}},
					{Name: desc.Name{Name: "b"}, Type: desc.Type{Kind: desc.TypeTuple, Items: shared}},
				},
			}}, nil, nil)
		b := mustLower(t, d)
		cls, _ := b.FindClass("C", NonStreaming)
		if len(cls.Fields) != 2 {
			t.Fatalf("class fields = %d, want 2", len(cls.Fields))
		}
	})

	t.Run("nested_prefix_shared_slice", func(t *testing.T) {
		// A finite, acyclic DAG: the outer tuple's Items is `shared` (len 2) and
		// its second element is a tuple whose Items is `shared[:1]` (len 1) — a
		// shorter PREFIX of the same backing array, containing only the leaf int.
		// There is no path back to any active range, so it must lower. Keying the
		// guard on the backing head alone conflates these two ranges and would
		// falsely report a cycle; the (head, len) range key keeps them distinct.
		shared := make([]desc.Type, 2)
		shared[0] = desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveInt}
		shared[1] = desc.Type{Kind: desc.TypeTuple, Items: shared[:1]}
		b := mustLower(t, descBundle(desc.Type{Kind: desc.TypeTuple, Items: shared}, nil, nil, nil, nil))
		if b.Target.Kind != TypeTuple || len(b.Target.Items) != 2 {
			t.Fatalf("target = %+v, want a 2-item tuple", b.Target)
		}
		if b.Target.Items[1].Kind != TypeTuple || len(b.Target.Items[1].Items) != 1 {
			t.Errorf("nested prefix tuple = %+v, want a 1-item tuple", b.Target.Items[1])
		}
	})
}

// TestFromStaticDescriptorOutputIllegalKindsLowerButFailValidateOutput proves
// tuple/arrow/top/media are structurally valid (they lower and pass the
// default Validate that FromStaticDescriptor runs) yet are rejected by the
// stricter output profile.
func TestFromStaticDescriptorOutputIllegalKindsLowerButFailValidateOutput(t *testing.T) {
	cases := map[string]desc.Type{
		"tuple": dTuple(dInt(), dString()),
		"arrow": dArrow(dInt(), dString()),
		"top":   dTop(),
		"media": dMedia(desc.MediaImage),
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := FromStaticDescriptor(descBundle(target, nil, nil, nil, nil))
			if err != nil {
				t.Fatalf("FromStaticDescriptor should lower %s structurally: %v", name, err)
			}
			if err := b.ValidateOutput(); err == nil {
				t.Errorf("ValidateOutput accepted %s, want rejection", name)
			}
		})
	}
}

// --- fail-closed tests ------------------------------------------------------

func TestFromStaticDescriptorFailClosed(t *testing.T) {
	cases := []struct {
		name string
		in   desc.Bundle
		want string
	}{
		{
			name: "unknown_version",
			in:   desc.Bundle{Version: 99, Target: dInt()},
			want: "unsupported static descriptor version",
		},
		{
			name: "zero_version",
			in:   desc.Bundle{Version: 0, Target: dInt()},
			want: "unsupported static descriptor version",
		},
		{
			name: "unknown_type_kind",
			in:   descBundle(desc.Type{Kind: "bogus"}, nil, nil, nil, nil),
			want: "unknown type kind",
		},
		{
			name: "unknown_primitive",
			in:   descBundle(desc.Type{Kind: desc.TypePrimitive, Primitive: "bogus"}, nil, nil, nil, nil),
			want: "unknown primitive",
		},
		{
			name: "unknown_literal_kind",
			in:   descBundle(desc.Type{Kind: desc.TypeLiteral, Literal: &desc.LiteralValue{Kind: "bogus"}}, nil, nil, nil, nil),
			want: "unknown literal kind",
		},
		{
			name: "media_missing_subtype",
			in:   descBundle(desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveMedia}, nil, nil, nil, nil),
			want: "media primitive has invalid subtype",
		},
		{
			name: "media_bad_subtype",
			in:   descBundle(dMedia("gif"), nil, nil, nil, nil),
			want: "media primitive has invalid subtype",
		},
		{
			name: "unknown_mode_on_class_ref",
			in:   descBundle(desc.Type{Kind: desc.TypeClass, Name: "C", Mode: "bogus"}, nil, nil, nil, nil),
			want: "invalid streaming mode",
		},
		{
			name: "unknown_mode_on_class_def",
			in: descBundle(dClassRef("C"), nil,
				[]desc.ClassDef{{Name: desc.Name{Name: "C"}, Mode: "bogus"}}, nil, nil),
			want: "invalid streaming mode",
		},
		{
			name: "unknown_constraint_level",
			in: descBundle(dClassRef("C"), nil,
				[]desc.ClassDef{{
					Name:        desc.Name{Name: "C"},
					Mode:        desc.NonStreaming,
					Constraints: []desc.Constraint{{Level: "warn", Expression: "x"}},
					Fields:      []desc.ClassField{{Name: desc.Name{Name: "n"}, Type: dInt()}},
				}}, nil, nil),
			want: "unknown constraint level",
		},
		{
			name: "list_missing_element",
			in:   descBundle(desc.Type{Kind: desc.TypeList}, nil, nil, nil, nil),
			want: "list type missing element",
		},
		{
			name: "map_missing_key_value",
			in:   descBundle(desc.Type{Kind: desc.TypeMap}, nil, nil, nil, nil),
			want: "map type requires key and value",
		},
		{
			name: "union_missing_payload",
			in:   descBundle(desc.Type{Kind: desc.TypeUnion}, nil, nil, nil, nil),
			want: "union type missing payload",
		},
		{
			name: "arrow_missing_payload",
			in:   descBundle(desc.Type{Kind: desc.TypeArrow}, nil, nil, nil, nil),
			want: "arrow type missing payload",
		},
		{
			name: "literal_missing_value",
			in:   descBundle(desc.Type{Kind: desc.TypeLiteral}, nil, nil, nil, nil),
			want: "literal type missing value",
		},
		{
			name: "null_primitive_in_union",
			in:   descBundle(dUnion(false, dInt(), dNull()), nil, nil, nil, nil),
			want: "null primitive",
		},
		{
			name: "empty_enum_ref_name",
			in:   descBundle(desc.Type{Kind: desc.TypeEnum, Name: ""}, nil, nil, nil, nil),
			want: "enum reference has empty name",
		},
		{
			name: "empty_class_ref_name",
			in:   descBundle(desc.Type{Kind: desc.TypeClass, Name: "", Mode: desc.NonStreaming}, nil, nil, nil, nil),
			want: "class reference has empty name",
		},
		{
			name: "empty_alias_ref_name",
			in:   descBundle(desc.Type{Kind: desc.TypeRecursiveAlias, Name: "", Mode: desc.NonStreaming}, nil, nil, nil, nil),
			want: "recursive alias reference has empty name",
		},
		{
			name: "dangling_class_ref",
			in:   descBundle(dClassRef("Missing"), nil, nil, nil, nil),
			want: "unresolved class reference",
		},
		{
			name: "dangling_enum_ref",
			in:   descBundle(dEnumRef("Missing"), nil, nil, nil, nil),
			want: "unresolved enum reference",
		},
		{
			name: "dangling_alias_ref",
			in:   descBundle(dAliasRef("Missing"), nil, nil, nil, nil),
			want: "unresolved recursive alias reference",
		},
		{
			name: "dangling_recursive_class",
			in: descBundle(dClassRef("C"), nil,
				[]desc.ClassDef{{Name: desc.Name{Name: "C"}, Mode: desc.NonStreaming, Fields: []desc.ClassField{{Name: desc.Name{Name: "n"}, Type: dInt()}}}},
				[]string{"Ghost"}, nil),
			want: "unresolved class",
		},
		{
			name: "duplicate_rendered_enum_value",
			in: descBundle(dEnumRef("E"),
				[]desc.EnumDef{{Name: desc.Name{Name: "E"}, Values: []desc.EnumValue{
					{Name: desc.Name{Name: "A"}},
					{Name: desc.Name{Name: "B", Alias: ptr("A")}},
				}}}, nil, nil, nil),
			want: "duplicate rendered value name",
		},
		{
			name: "duplicate_rendered_class_field",
			in: descBundle(dClassRef("C"), nil,
				[]desc.ClassDef{{Name: desc.Name{Name: "C"}, Mode: desc.NonStreaming, Fields: []desc.ClassField{
					{Name: desc.Name{Name: "a"}, Type: dInt()},
					{Name: desc.Name{Name: "b", Alias: ptr("a")}, Type: dInt()},
				}}}, nil, nil),
			want: "duplicate rendered field name",
		},
		{
			name: "invalid_map_key",
			in:   descBundle(dMap(dInt(), dString()), nil, nil, nil, nil),
			want: "map key must be",
		},

		// Stray / incompatible payload fields for the resolved kind.
		{
			name: "enum_ref_with_mode",
			in:   descBundle(desc.Type{Kind: desc.TypeEnum, Name: "E", Mode: desc.NonStreaming}, nil, nil, nil, nil),
			want: `field "mode" is not valid for kind "enum"`,
		},
		{
			name: "list_with_dynamic",
			in:   descBundle(desc.Type{Kind: desc.TypeList, Elem: typePtr(dInt()), Dynamic: true}, nil, nil, nil, nil),
			want: `field "dynamic" is not valid for kind "list"`,
		},
		{
			name: "primitive_with_elem",
			in:   descBundle(desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveString, Elem: typePtr(dInt())}, nil, nil, nil, nil),
			want: `field "elem" is not valid for kind "primitive"`,
		},
		{
			name: "top_with_name",
			in:   descBundle(desc.Type{Kind: desc.TypeTop, Name: "x"}, nil, nil, nil, nil),
			want: `field "name" is not valid for kind "top"`,
		},
		{
			name: "union_with_stray_literal",
			in:   descBundle(desc.Type{Kind: desc.TypeUnion, Union: &desc.UnionType{Variants: []desc.Type{dInt(), dString()}}, Literal: &desc.LiteralValue{Kind: desc.LiteralString, String: "x"}}, nil, nil, nil, nil),
			want: `field "literal" is not valid for kind "union"`,
		},
		{
			// Non-nil but EMPTY Items on a non-tuple kind must be rejected, not
			// silently dropped (len-based presence would have missed this).
			name: "primitive_with_empty_items",
			in:   descBundle(desc.Type{Kind: desc.TypePrimitive, Primitive: desc.PrimitiveString, Items: []desc.Type{}}, nil, nil, nil, nil),
			want: `field "items" is not valid for kind "primitive"`,
		},

		// Mismatched literal scalar fields.
		{
			name: "literal_string_with_int",
			in:   descBundle(desc.Type{Kind: desc.TypeLiteral, Literal: &desc.LiteralValue{Kind: desc.LiteralString, String: "s", Int: 5}}, nil, nil, nil, nil),
			want: "string literal carries a non-string scalar",
		},
		{
			name: "literal_int_with_string",
			in:   descBundle(desc.Type{Kind: desc.TypeLiteral, Literal: &desc.LiteralValue{Kind: desc.LiteralInt, Int: 1, String: "x"}}, nil, nil, nil, nil),
			want: "int literal carries a non-int scalar",
		},
		{
			name: "literal_bool_with_int",
			in:   descBundle(desc.Type{Kind: desc.TypeLiteral, Literal: &desc.LiteralValue{Kind: desc.LiteralBool, Bool: true, Int: 2}}, nil, nil, nil, nil),
			want: "bool literal carries a non-bool scalar",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromStaticDescriptor(tc.in)
			mustErrContains(t, err, tc.want)
		})
	}
}

func TestStaticBundlesFromDescriptors(t *testing.T) {
	good := descBundle(dInt(), nil, nil, nil, nil)
	bad := desc.Bundle{Version: 99, Target: dInt()}

	bundles, errs := StaticBundlesFromDescriptors(map[string]func() desc.Bundle{
		"good":     func() desc.Bundle { return good },
		"bad":      func() desc.Bundle { return bad },
		"nil_ctor": nil,
	})

	if bundles == nil || errs == nil {
		t.Fatal("both result maps must be non-nil")
	}
	if _, ok := bundles["good"]; !ok {
		t.Errorf("good descriptor missing from bundles")
	}
	if _, ok := bundles["bad"]; ok {
		t.Errorf("bad descriptor should not be in bundles")
	}
	if err, ok := errs["bad"]; !ok {
		t.Errorf("bad descriptor missing from errs")
	} else {
		mustErrContains(t, err, "unsupported static descriptor version")
	}
	if err, ok := errs["nil_ctor"]; !ok {
		t.Errorf("nil ctor missing from errs")
	} else {
		mustErrContains(t, err, "nil descriptor constructor")
	}
	if _, ok := errs["good"]; ok {
		t.Errorf("good descriptor should not have an error")
	}
}

// --- golden tests -----------------------------------------------------------

func goldenBundle() desc.Bundle {
	// A wide bundle: ordered enum + class, all three alias states (absent,
	// present-nonempty, present-empty), descriptions, an enum/type/class
	// constraint, a map, an optional union, a literal union, a recursive
	// self-reference, and a type-level stream+constraint on a field. It
	// passes the default (structural) Validate.
	return descBundle(
		dClassRef("Root"),
		[]desc.EnumDef{{
			Name: desc.Name{Name: "Status", Alias: ptr("StatusAlias")},
			Values: []desc.EnumValue{
				{Name: desc.Name{Name: "OK"}, Description: ptr("okay")},
				{Name: desc.Name{Name: "WARN", Alias: ptr("warn")}},
				{Name: desc.Name{Name: "BLANK", Alias: ptr("")}},
			},
			Constraints: []desc.Constraint{{Level: desc.ConstraintCheck, Expression: `this != "X"`}},
		}},
		[]desc.ClassDef{{
			Name:        desc.Name{Name: "Root"},
			Description: ptr("root object"),
			Mode:        desc.NonStreaming,
			Constraints: []desc.Constraint{{Level: desc.ConstraintAssert, Expression: "this.id|length > 0", Label: ptr("nonempty")}},
			Fields: []desc.ClassField{
				{Name: desc.Name{Name: "id"}, Type: dString()},
				{Name: desc.Name{Name: "label", Alias: ptr("label_alias")}, Type: dString(), Description: ptr("the label")},
				{Name: desc.Name{Name: "note", Alias: ptr("")}, Type: dString()},
				{Name: desc.Name{Name: "status"}, Type: dEnumRef("Status")},
				{Name: desc.Name{Name: "tags"}, Type: dList(dString())},
				{Name: desc.Name{Name: "meta"}, Type: dMap(dString(), dInt())},
				{Name: desc.Name{Name: "maybe"}, Type: dUnion(true, dInt())},
				{Name: desc.Name{Name: "kind"}, Type: dUnion(false, dLitStr("a"), dLitStr("b"))},
				{
					Name: desc.Name{Name: "score"},
					Type: desc.Type{
						Kind:      desc.TypePrimitive,
						Primitive: desc.PrimitiveInt,
						Meta: desc.TypeMeta{
							Constraints: []desc.Constraint{{Level: desc.ConstraintCheck, Expression: "this > 0"}},
							Stream:      desc.StreamingBehavior{Needed: true},
						},
					},
				},
				{Name: desc.Name{Name: "child"}, Type: dUnion(true, dClassRef("Root"))},
			},
		}},
		[]string{"Root"}, nil,
	)
}

func goldenAliasBundle() desc.Bundle {
	return descBundle(
		dAliasRef("Json"), nil, nil, nil,
		[]desc.RecursiveAliasDef{{
			Name: "Json",
			Target: dUnion(false,
				dString(),
				dBool(),
				dFloat(),
				dList(dAliasRef("Json")),
				dMap(dString(), dAliasRef("Json")),
			),
		}},
	)
}

func TestFromStaticDescriptorGolden(t *testing.T) {
	cases := map[string]func() desc.Bundle{
		"kitchen_sink":    goldenBundle,
		"recursive_alias": goldenAliasBundle,
	}
	for name, ctor := range cases {
		t.Run(name, func(t *testing.T) {
			b := mustLower(t, ctor())
			checkGolden(t, name, b)
		})
	}
}

func checkGolden(t *testing.T, name string, b *Bundle) {
	t.Helper()
	got, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatalf("marshal lowered bundle: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", "static_descriptor", name, "bundle.json")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %q (regenerate with `go test ./internal/schema -run Golden -update`): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden %q mismatch (run -update to regenerate):\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}
