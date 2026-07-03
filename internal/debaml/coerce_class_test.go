package debaml

import (
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// classBundle builds an indexed bundle from a DynamicOutputSchema and returns a
// schema.Type referring to the named class, for direct coerceClass unit tests.
func classBundle(t *testing.T, s *bamlutils.DynamicOutputSchema, class string) (*schema.Bundle, schema.Type) {
	t.Helper()
	b, err := schema.FromDynamicOutputSchema(s, schema.BuildOptions{})
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	for i := range b.Classes {
		if b.Classes[i].Name.Name == class {
			return b, schema.Type{Kind: schema.TypeClass, Name: b.Classes[i].Name.Name, Mode: b.Classes[i].Mode}
		}
	}
	t.Fatalf("class %q not found in bundle", class)
	return nil, schema.Type{}
}

// nestedClassSchema builds Root{u: <ref C>} plus the named class C with the
// given properties, so an end-to-end mustParse of {"u": ...} exercises coerceClass
// on C.
func nestedClassSchema(cls string, props bamlutils.OrderedMap[*bamlutils.DynamicProperty]) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: bamlutils.MustOrderedMap(bamlutils.OrderedKV("u", &bamlutils.DynamicProperty{Ref: cls})),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV(cls, &bamlutils.DynamicClass{Properties: props})),
	}
}

func optProp(inner *bamlutils.DynamicTypeSpec) *bamlutils.DynamicProperty {
	return &bamlutils.DynamicProperty{Type: "optional", Inner: inner}
}
func listProp(inner *bamlutils.DynamicTypeSpec) *bamlutils.DynamicProperty {
	return &bamlutils.DynamicProperty{Type: "list", Items: inner}
}
func mapProp(val *bamlutils.DynamicTypeSpec) *bamlutils.DynamicProperty {
	return &bamlutils.DynamicProperty{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: val}
}
func nullProp() *bamlutils.DynamicProperty { return &bamlutils.DynamicProperty{Type: "null"} }

// TestCoerceClass_FieldAssignment pins BAML's object→class field assignment:
// input-key order, first-field match, first-key-wins for duplicate matches,
// unmatched keys omitted (ExtraKey), fields emitted in SCHEMA order.
func TestCoerceClass_FieldAssignment(t *testing.T) {
	// Root{a int, b string} with input keys out of schema order + an extra key.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("a", intProp()), kv("b", strProp())),
	}
	// Extra key ignored, fields re-emitted in schema order.
	mustParseExact(t, s, `{"b":"x","a":1,"extra":9}`, `{"a":1,"b":"x"}`)
	// Duplicate matching key: first value wins.
	mustParse(t, s, `{"a":1,"a":2,"b":"x"}`, `{"a":1,"b":"x"}`)

	// ExtraKey is score-bearing: a class with an extra key is not clean.
	b, ct := classBundle(t, s, "Baml_Rest_DynamicOutput")
	cf := &coerceFlags{}
	if _, err := coerceClass(b, ct.Name, ct.Mode, objVal(fld("a", numV("1")), fld("b", strVv("x")), fld("extra", numV("9"))), cf); err != nil {
		t.Fatalf("coerceClass extra: %v", err)
	}
	if !cf.isFlagged() {
		t.Errorf("an extra input key must flag cf (ExtraKey, score-bearing)")
	}
	// A clean object (no extras) must NOT flag.
	cfClean := &coerceFlags{}
	if _, err := coerceClass(b, ct.Name, ct.Mode, objVal(fld("a", numV("1")), fld("b", strVv("x"))), cfClean); err != nil {
		t.Fatalf("coerceClass clean: %v", err)
	}
	if cfClean.isFlagged() {
		t.Errorf("a clean all-matched object must NOT flag cf")
	}
}

// TestCoerceClass_SingleFieldObjectImpliedKey pins the single-field implied-key
// path: when no input key matches the lone field, the WHOLE object is coerced
// into it (ImpliedKey). A string field stringifies the object (JsonToString); an
// int field with an object implied value FAILS (proven) so the class declines.
func TestCoerceClass_SingleFieldObjectImpliedKey(t *testing.T) {
	// Box{value string}: {foo:5} implied-key-coerces the whole object -> "{foo: 5}".
	strBox := nestedClassSchema("Box", props(kv("value", strProp())))
	mustParse(t, strBox, `{"u":{"foo":5}}`, `{"u":{"value":"{foo: 5}"}}`)

	// The implied path is score-bearing (ImpliedKey + JsonToString).
	b, ct := classBundle(t, strBox, "Box")
	cf := &coerceFlags{}
	if _, err := coerceClass(b, ct.Name, ct.Mode, objVal(fld("foo", numV("5"))), cf); err != nil {
		t.Fatalf("implied-key: %v", err)
	}
	if !cf.isFlagged() {
		t.Errorf("single-field implied-key must flag cf (ImpliedKey, score-bearing)")
	}

	// Box{value int}: {other:5} implied-coerces int<-object which is a proven
	// BAML error, and BAML then errors (missing required non-defaultable); native
	// cannot prove that from its own decline, so it DECLINES.
	intBox := nestedClassSchema("Box", props(kv("value", intProp())))
	requireUnsupported(t, intBox, `{"u":{"other":5}}`)

	// A matching key takes the normal path (no implied-key).
	mustParse(t, intBox, `{"u":{"value":5}}`, `{"u":{"value":5}}`)
}

// TestCoerceClass_SingleFieldScalarInferredObject pins the inferred-object path:
// a non-object non-array value is absorbed into the lone field (ImpliedKey +
// InferedObject). An int field takes the scalar directly; a string field
// stringifies it.
func TestCoerceClass_SingleFieldScalarInferredObject(t *testing.T) {
	intBox := nestedClassSchema("Box", props(kv("value", intProp())))
	mustParse(t, intBox, `{"u":5}`, `{"u":{"value":5}}`)

	strBox := nestedClassSchema("Box", props(kv("value", strProp())))
	mustParse(t, strBox, `{"u":true}`, `{"u":{"value":"true"}}`)
	mustParse(t, strBox, `{"u":5}`, `{"u":{"value":"5"}}`)

	// InferedObject is cost 0; the ONLY score-bearing flag is the child ImpliedKey.
	b, ct := classBundle(t, intBox, "Box")
	cf := &coerceFlags{}
	if _, err := coerceClass(b, ct.Name, ct.Mode, numV("5"), cf); err != nil {
		t.Fatalf("inferred-object: %v", err)
	}
	if !cf.isFlagged() {
		t.Errorf("inferred-object must flag cf (ImpliedKey, score-bearing)")
	}

	// A scalar that the lone field cannot coerce -> DECLINE (native cannot prove
	// BAML's outcome). "hello" -> int is a proven BAML error, but native declines.
	requireUnsupported(t, intBox, `{"u":"hello"}`)

	// A bare JSON null takes the SAME inferred-object path (non-object non-array):
	// coerceImplied(null) coerces null into the lone field. For an int/string
	// field, coerce_int/coerce_string on null is error_unexpected_null (a sentinel
	// decline, not a proven-map default), so the class is indeterminate and native
	// DECLINES (BAML errors; native falls back).
	requireUnsupported(t, intBox, `{"u":null}`)
	requireUnsupported(t, strBox, `{"u":null}`)
	// Directly: coerceClass on a bare null declines with the fallback sentinel.
	bn, cn := classBundle(t, strBox, "Box")
	if _, err := coerceClass(bn, cn.Name, cn.Mode, nullVal(), nil); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("single-field string class <- null: expected ErrDeBAMLParseUnsupported, got %v", err)
	}

	// When the lone field ACCEPTS null (an optional), the inferred-object path
	// SUCCEEDS on null: null is absorbed into the field as null (ImpliedKey).
	optBox := nestedClassSchema("Box", props(kv("value", optProp(&bamlutils.DynamicTypeSpec{Type: "int"}))))
	mustParse(t, optBox, `{"u":null}`, `{"u":{"value":null}}`)
}

// TestCoerceClass_MissingOptionalNull pins that an absent optional field is
// omitted (InjectAbsentOptionals adds the null downstream) yet flags cf
// (OptionalDefaultFromNoValue, score 1) for nullable-union cleanliness.
func TestCoerceClass_MissingOptionalNull(t *testing.T) {
	// Root{name string, nick string?} with nick absent.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("name", strProp()), kv("nick", optProp(&bamlutils.DynamicTypeSpec{Type: "string"}))),
	}
	// The native coercer omits the absent optional; equality is semantic here.
	mustParse(t, s, `{"name":"Ada"}`, `{"name":"Ada"}`)

	b, ct := classBundle(t, s, "Baml_Rest_DynamicOutput")
	cf := &coerceFlags{}
	out, err := coerceClass(b, ct.Name, ct.Mode, objVal(fld("name", strVv("Ada"))), cf)
	if err != nil {
		t.Fatalf("missing optional: %v", err)
	}
	if string(out) != `{"name":"Ada"}` {
		t.Errorf("absent optional must be OMITTED: got %s", out)
	}
	if !cf.isFlagged() {
		t.Errorf("absent optional must flag cf (OptionalDefaultFromNoValue, score-bearing)")
	}
}

// TestCoerceClass_RequiredDefaults pins TypeIR::default_value fills for a missing
// REQUIRED field: list→[], map→{}, primitive-null→null. Each flags cf
// (DefaultFromNoValue, score 100). A non-defaultable missing required field
// (int) DECLINES.
func TestCoerceClass_RequiredDefaults(t *testing.T) {
	// Required list field absent -> [].
	listS := nestedClassSchema("C", props(kv("items", listProp(&bamlutils.DynamicTypeSpec{Type: "int"}))))
	mustParse(t, listS, `{"u":{}}`, `{"u":{"items":[]}}`)

	// Required map field absent -> {}.
	mapS := nestedClassSchema("C", props(kv("m", mapProp(&bamlutils.DynamicTypeSpec{Type: "int"}))))
	mustParse(t, mapS, `{"u":{}}`, `{"u":{"m":{}}}`)

	// Required null-primitive field absent -> null.
	nullS := nestedClassSchema("C", props(kv("n", nullProp())))
	mustParse(t, nullS, `{"u":{}}`, `{"u":{"n":null}}`)

	// Each default flags cf.
	b, ct := classBundle(t, listS, "C")
	cf := &coerceFlags{}
	if _, err := coerceClass(b, ct.Name, ct.Mode, objVal(), cf); err != nil {
		t.Fatalf("list default: %v", err)
	}
	if !cf.isFlagged() {
		t.Errorf("DefaultFromNoValue must flag cf (score 100)")
	}

	// A non-defaultable required field (int) absent -> DECLINE (BAML errors,
	// native falls back).
	intS := nestedClassSchema("C", props(kv("x", intProp())))
	requireUnsupported(t, intS, `{"u":{}}`)
}

// TestCoerceClass_MapFieldDefaultButUnparseable pins DefaultButHadUnparseableValue:
// a present REQUIRED map field with a NON-object value is a proven coerce_map
// error, so BAML fills the map default {} and native CLAIMS it.
func TestCoerceClass_MapFieldDefaultButUnparseable(t *testing.T) {
	mapS := nestedClassSchema("C", props(kv("m", mapProp(&bamlutils.DynamicTypeSpec{Type: "int"}))))
	mustParse(t, mapS, `{"u":{"m":"bad"}}`, `{"u":{"m":{}}}`)
	mustParse(t, mapS, `{"u":{"m":[1,2]}}`, `{"u":{"m":{}}}`)

	b, ct := classBundle(t, mapS, "C")
	cf := &coerceFlags{}
	if _, err := coerceClass(b, ct.Name, ct.Mode, objVal(fld("m", strVv("bad"))), cf); err != nil {
		t.Fatalf("map default-but-unparseable: %v", err)
	}
	if !cf.isFlagged() {
		t.Errorf("DefaultButHadUnparseableValue must flag cf (score 2)")
	}
	// A map field with an OBJECT value coerces normally (partial map, not a default).
	mustParse(t, mapS, `{"u":{"m":{"a":1}}}`, `{"u":{"m":{"a":1}}}`)
}

// TestCoerceClass_ScalarMultiFieldAllDefaultable pins that a SCALAR into a
// multi-field class assigns nothing and fills every field's default (BAML's
// Some(x) arm is a no-op for >1 field); all-defaultable -> success.
func TestCoerceClass_ScalarMultiFieldAllDefaultable(t *testing.T) {
	// C{a int[], b map<string,int>}: a scalar fills both defaults.
	s := nestedClassSchema("C", props(
		kv("a", listProp(&bamlutils.DynamicTypeSpec{Type: "int"})),
		kv("b", mapProp(&bamlutils.DynamicTypeSpec{Type: "int"})),
	))
	mustParse(t, s, `{"u":5}`, `{"u":{"a":[],"b":{}}}`)
	// An empty object behaves the same (no keys -> all defaults).
	mustParse(t, s, `{"u":{}}`, `{"u":{"a":[],"b":{}}}`)
}

// TestCoerceClass_ArrayDeclines pins the HARD boundary: an ARRAY into a class is
// coerce_array_to_singular / pick_best (M3), so native DECLINES regardless of
// field count (single- and multi-field).
func TestCoerceClass_ArrayDeclines(t *testing.T) {
	intBox := nestedClassSchema("Box", props(kv("value", intProp())))
	requireUnsupported(t, intBox, `{"u":[1,2,3]}`)

	multi := nestedClassSchema("C", props(kv("a", intProp()), kv("b", strProp())))
	requireUnsupported(t, multi, `{"u":[1,2,3]}`)

	// Direct coerceClass with an array declines with the sentinel.
	b, ct := classBundle(t, intBox, "Box")
	if _, err := coerceClass(b, ct.Name, ct.Mode, arrVal(numV("1")), nil); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("array->class: expected ErrDeBAMLParseUnsupported, got %v", err)
	}
}

// TestDefaultValue pins the defaultValue helper (TypeIR::default_value) per kind.
func TestDefaultValue(t *testing.T) {
	listT := schema.Type{Kind: schema.TypeList, Elem: &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}}
	mapT := schema.Type{Kind: schema.TypeMap, Key: &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}, Value: &schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}}
	nullT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveNull}
	intT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
	strT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	enumT := schema.Type{Kind: schema.TypeEnum, Name: "E"}
	classT := schema.Type{Kind: schema.TypeClass, Name: "C"}
	litT := strLitType("x")

	defaultable := []struct {
		name string
		t    schema.Type
		want string
	}{
		{"list", listT, "[]"},
		{"map", mapT, "{}"},
		{"null", nullT, "null"},
		// Union: FIRST defaultable arm (list) wins in declaration order.
		{"union-list-first", schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{listT, strT}}}, "[]"},
		// Union: a non-defaultable arm is skipped; the map arm defaults.
		{"union-skip-nondefaultable", schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{strT, mapT}}}, "{}"},
		// Nullable union with no defaultable non-null arm -> the appended null arm.
		{"union-nullable-null", schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{intT}, Nullable: true}}, "null"},
		// Tuple: a single-element defaultable tuple (no comma path).
		{"tuple-single", schema.Type{Kind: schema.TypeTuple, Items: []schema.Type{nullT}}, "[null]"},
		// Tuple: every element defaultable -> the JOINED element defaults, in order.
		{"tuple-all-defaultable", schema.Type{Kind: schema.TypeTuple, Items: []schema.Type{listT, mapT}}, "[[],{}]"},
	}
	for _, c := range defaultable {
		got, ok := defaultValue(c.t)
		if !ok || string(got) != c.want {
			t.Errorf("defaultValue(%s) = (%s,%v), want (%s,true)", c.name, got, ok, c.want)
		}
	}

	nonDefaultable := []struct {
		name string
		t    schema.Type
	}{
		{"int", intT},
		{"string", strT},
		{"enum", enumT},
		{"class", classT},
		{"literal", litT},
		// A non-nullable union of non-defaultable arms (int | string).
		{"union-all-nondefaultable", schema.Type{Kind: schema.TypeUnion, Union: &schema.UnionType{Variants: []schema.Type{intT, strT}}}},
		// A tuple with ANY non-defaultable element (int[] , string) -> early return,
		// the whole tuple is non-defaultable.
		{"tuple-has-nondefaultable", schema.Type{Kind: schema.TypeTuple, Items: []schema.Type{listT, strT}}},
	}
	for _, c := range nonDefaultable {
		if _, ok := defaultValue(c.t); ok {
			t.Errorf("defaultValue(%s) must be non-defaultable (ok=false)", c.name)
		}
	}
}

// TestCoerceClass_NullableCleanOnly pins the nullable-union clean-only rule for
// class arms: a class that fills any score-bearing flag (extra key, absent
// optional, default) inside a nullable single-arm union DECLINES against the
// scored null arm; only a clean class arm claims.
func TestCoerceClass_NullableCleanOnly(t *testing.T) {
	// Root{u: (C)?}, C{name string} — a clean class arm claims.
	clean := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", optProp(&bamlutils.DynamicTypeSpec{Ref: "C"}))),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: props(kv("name", strProp()))})),
	}
	mustParse(t, clean, `{"u":{"name":"x"}}`, `{"u":{"name":"x"}}`)
	// An EXTRA key makes the class arm non-clean -> declines against null (M3).
	requireUnsupported(t, clean, `{"u":{"name":"x","extra":1}}`)

	// Root{u: (C)?}, C{name string, nick string?} — an absent optional makes the
	// arm non-clean (OptionalDefaultFromNoValue) -> declines against null.
	withOpt := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", optProp(&bamlutils.DynamicTypeSpec{Ref: "C"}))),
		Classes: bamlutils.MustOrderedMap(bamlutils.OrderedKV("C", &bamlutils.DynamicClass{
			Properties: props(kv("name", strProp()), kv("nick", optProp(&bamlutils.DynamicTypeSpec{Type: "string"}))),
		})),
	}
	requireUnsupported(t, withOpt, `{"u":{"name":"x"}}`)
	// Both fields present -> clean -> claims.
	mustParse(t, withOpt, `{"u":{"name":"x","nick":"y"}}`, `{"u":{"name":"x","nick":"y"}}`)

	// A JSON null still takes the null fast path.
	mustParse(t, clean, `{"u":null}`, `{"u":null}`)
}

// TestCoerceClass_Fixture40ShapeStaysFallback is the LOAD-BEARING over-claim
// guard: a union of SINGLE-field classes (fixture 40 shape) with a scalar input
// must STAY fallback. It declines at the SCHEMA GATE (checkFlatClassUnion rejects
// a <2-field class-union variant), and — were the gate lifted — the lenient
// coerceClass makes BOTH single-field arms succeed via inferred-object, so the
// flat-class-union counter would see >=2 successes and decline. This must hold
// after coerceClass became lenient.
func TestCoerceClass_Fixture40ShapeStaysFallback(t *testing.T) {
	// Root{u: A | B}, A{val int}, B{num int}, input {"u":5}.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "B"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv("val", intProp()))}),
			bamlutils.OrderedKV("B", &bamlutils.DynamicClass{Properties: props(kv("num", intProp()))}),
		),
	}
	requireUnsupported(t, s, `{"u":5}`)

	// Both single-field classes DO absorb the scalar via inferred-object (which
	// is exactly why the union must stay fallback): each coerceClass succeeds.
	bA, aT := classBundle(t, s, "A")
	if _, err := coerceClass(bA, aT.Name, aT.Mode, numV("5"), nil); err != nil {
		t.Errorf("A{val int} must absorb scalar 5 via inferred-object: %v", err)
	}
	bB, bT := classBundle(t, s, "B")
	if _, err := coerceClass(bB, bT.Name, bT.Mode, numV("5"), nil); err != nil {
		t.Errorf("B{num int} must absorb scalar 5 via inferred-object: %v", err)
	}
}

// TestCoerceClass_ClaimedChildErrorPropagates pins that a CLAIMED field error (an
// enum/string-literal substring TIE, StrMatchOneFromMany) is PROPAGATED as the
// class error — BAML errors that non-defaultable field and thus the whole class,
// so native claims the same error rather than swallowing it into a fallback
// (fixture 51 shape as a class field).
func TestCoerceClass_ClaimedChildErrorPropagates(t *testing.T) {
	// Root{animal: Animal}, Animal{CAT,DOG}; "cat dog" substring-ties both.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("animal", &bamlutils.DynamicProperty{Ref: "Animal"})),
		Enums: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Animal", &bamlutils.DynamicEnum{
			Values: []*bamlutils.DynamicEnumValue{{Name: "CAT"}, {Name: "DOG"}},
		})),
	}
	requireClaimedError(t, s, `{"animal":"cat dog"}`)

	// The same tie via a single-field implied-key path also propagates: Box{a
	// enum} with a non-matching object whose Display substring-ties both values.
	b, ct := classBundle(t, s, "Baml_Rest_DynamicOutput")
	if _, err := coerceClass(b, ct.Name, ct.Mode, objVal(fld("animal", strVv("cat dog"))), nil); err == nil {
		t.Errorf("substring-tie field must claim an error, got nil")
	} else if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("substring-tie field must be a CLAIMED error, got fallback sentinel: %v", err)
	}
}

// TestCoerceClass_CollectionChildFlips pins that class children which now succeed
// through Mcoerce-d structural coercion are KEPT in a list/map (not skipped, not
// a whole-collection decline).
func TestCoerceClass_CollectionChildFlips(t *testing.T) {
	// list<Box> where Box{value int}: scalar elements infer-object into each Box.
	listBox := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", listProp(&bamlutils.DynamicTypeSpec{Ref: "Box"}))),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV("Box", &bamlutils.DynamicClass{Properties: props(kv("value", intProp()))})),
	}
	mustParse(t, listBox, `{"u":[5,6]}`, `{"u":[{"value":5},{"value":6}]}`)

	// map<string, Box>: scalar values infer-object into each Box.
	mapBox := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", mapProp(&bamlutils.DynamicTypeSpec{Ref: "Box"}))),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV("Box", &bamlutils.DynamicClass{Properties: props(kv("value", intProp()))})),
	}
	mustParse(t, mapBox, `{"u":{"a":5,"b":6}}`, `{"u":{"a":{"value":5},"b":{"value":6}}}`)

	// list<C> where C{items int[]}: an empty-object element fills the list default.
	listDefault := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", listProp(&bamlutils.DynamicTypeSpec{Ref: "C"}))),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: props(kv("items", listProp(&bamlutils.DynamicTypeSpec{Type: "int"})))})),
	}
	mustParse(t, listDefault, `{"u":[{}]}`, `{"u":[{"items":[]}]}`)

	// map<string, C> where C{items int[]}: an empty-object value fills the default.
	mapDefault := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", mapProp(&bamlutils.DynamicTypeSpec{Ref: "C"}))),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: props(kv("items", listProp(&bamlutils.DynamicTypeSpec{Type: "int"})))})),
	}
	mustParse(t, mapDefault, `{"u":{"k":{}}}`, `{"u":{"k":{"items":[]}}}`)
}
