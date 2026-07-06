package debaml

import (
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// pairClassFieldSchema is Root{ p: Pair }, Pair{ a: int, b: string } — a
// MULTI-field all-required-flat-leaf class field, the ONLY class shape whose
// array input native array-to-singulars.
func pairClassFieldSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("p", &bamlutils.DynamicProperty{Ref: "Pair"})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Pair", &bamlutils.DynamicClass{
				Properties: props(kv("a", intProp()), kv("b", strProp())),
			}),
		),
	}
}

// singleFieldClassFieldSchema is Root{ s: Single }, Single{ only: string } — a
// SINGLE-field class, whose array-to-singular (implied-key-vs-array competition)
// native deliberately DECLINES.
func singleFieldClassFieldSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("s", &bamlutils.DynamicProperty{Ref: "Single"})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Single", &bamlutils.DynamicClass{
				Properties: props(kv("only", strProp())),
			}),
		),
	}
}

// unionFieldSchema wraps a single union property `u` with the given variant
// specs (each a DynamicTypeSpec), for list/map/class union arms.
func unionFieldSchema(variants ...*bamlutils.DynamicTypeSpec) *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{Type: "union", OneOf: variants})
}

// TestArrayToSingular_PrimitiveIntField pins int array-to-singular on a NON-union
// (class-field) target: FirstMatch scoring, proven-item exclusion, empty error.
func TestArrayToSingular_PrimitiveIntField(t *testing.T) {
	s := oneField(intProp())
	// Both items coerce; the FIRST (index 0) wins pick_best.
	mustParse(t, s, `{"u":[1,2]}`, `{"u":1}`)
	// A non-numeric string is a PROVEN coerce_int error (excluded), so 2 wins.
	mustParse(t, s, `{"u":["bad",2]}`, `{"u":2}`)
	// A numeric STRING item coerces (score 0) and, being first, wins.
	mustParse(t, s, `{"u":["7","8"]}`, `{"u":7}`)
	// Every item a proven error → BAML errors; native's provenError wraps the
	// fallback sentinel, so native DECLINES.
	requireUnsupported(t, s, `{"u":["bad","nope"]}`)
	// Empty array → error_unexpected_empty_array (provenError → native DECLINES).
	requireUnsupported(t, s, `{"u":[]}`)
}

// TestArrayToSingular_PrimitiveFloatBoolField pins float/bool array-to-singular.
func TestArrayToSingular_PrimitiveFloatBoolField(t *testing.T) {
	f := oneField(&bamlutils.DynamicProperty{Type: "float"})
	mustParse(t, f, `{"u":[1.5,2.5]}`, `{"u":1.5}`)
	mustParse(t, f, `{"u":["bad",2.5]}`, `{"u":2.5}`)

	b := oneField(boolProp())
	mustParse(t, b, `{"u":[true,false]}`, `{"u":true}`)
	// A NUMBER is a proven coerce_bool error (error_unexpected_type), so the bool wins.
	mustParse(t, b, `{"u":[1,true]}`, `{"u":true}`)
}

// TestArrayToSingular_ClassMultiField pins the multi-field all-required-flat-leaf
// class array-to-singular (length-one + multi-item first-best), and the
// single-field / scalar-item declines.
func TestArrayToSingular_ClassMultiField(t *testing.T) {
	s := pairClassFieldSchema()
	// Length-one array → the object coerced into the class.
	mustParse(t, s, `{"p":[{"a":1,"b":"x"}]}`, `{"p":{"a":1,"b":"x"}}`)
	// Multi-item → the FIRST fully-coercing object wins.
	mustParse(t, s, `{"p":[{"a":1,"b":"x"},{"a":2,"b":"y"}]}`, `{"p":{"a":1,"b":"x"}}`)
	// A partial object (missing required b) is a PROVEN class error → excluded, so
	// the fully-valid object wins.
	mustParse(t, s, `{"p":[{"a":1},{"a":2,"b":"y"}]}`, `{"p":{"a":2,"b":"y"}}`)
	// A SINGLE-field class array DECLINES (implied-key-vs-array competition deferred).
	requireUnsupported(t, singleFieldClassFieldSchema(), `{"s":["x"]}`)
}

// TestArrayToSingular_UnionArmScoresUnionMatch pins THE union-arm scoring subtlety:
// a primitive array-to-singular AS A UNION ARM scores UnionMatch (0) + FirstMatch
// (1) = 1 (not FirstMatch twice = 2), so `string | int` on [1,2] picks the int (1)
// over the string arm's JsonToString "[1, 2]" (score 2).
func TestArrayToSingular_UnionArmScoresUnionMatch(t *testing.T) {
	mustParse(t, primUnion("string", "int"), `{"u":[1,2]}`, `{"u":1}`)
	mustParse(t, primUnion("int", "string"), `{"u":[1,2]}`, `{"u":1}`)
	// int | bool: the bool arm's array-to-singular is all-proven-errors (a NUMBER is
	// error_unexpected_type), excluded — the int arm wins.
	mustParse(t, primUnion("int", "bool"), `{"u":[1,2]}`, `{"u":1}`)
}

// TestUnion_ListArm pins list arms in a union: the string arm try_casts a scalar
// (list arm loses), the list arm's clean array wins at score 0, and a partial
// list beats a scalar array-to-singular (fixture 100 shape).
func TestUnion_ListArm(t *testing.T) {
	// list<string> | string with a scalar → the string arm try_casts (score 0).
	lsu := unionFieldSchema(
		&bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}},
		&bamlutils.DynamicTypeSpec{Type: "string"},
	)
	mustParse(t, lsu, `{"u":"hi"}`, `{"u":"hi"}`)
	// The same union with an ARRAY → the list arm coerces cleanly at score 0.
	mustParse(t, lsu, `{"u":["a","b"]}`, `{"u":["a","b"]}`)

	// int | list<int> with a partial array → the list arm ([1,2]) beats the int
	// arm's array-to-singular (Int 1, FirstMatch, devalued as a scalar-vs-composite).
	ilu := unionFieldSchema(
		&bamlutils.DynamicTypeSpec{Type: "int"},
		&bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "int"}},
	)
	mustParse(t, ilu, `{"u":[1,"bad",2]}`, `{"u":[1,2]}`)
}

// TestUnion_ListVsList pins the pick_best list-vs-list ordering: a list empty ONLY
// because of an ArrayItemParseError loses to a non-empty no-error list.
func TestUnion_ListVsList(t *testing.T) {
	// list<int> | list<string> with a scalar "hi": both SingleToArray-wrap; the int
	// list becomes [] (proven bad-int item) while the string list becomes ["hi"], so
	// the empty-error list is devalued and ["hi"] wins.
	llu := unionFieldSchema(
		&bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "int"}},
		&bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}},
	)
	mustParse(t, llu, `{"u":"hi"}`, `{"u":["hi"]}`)
}

// TestUnion_MapArm pins map arms in a union: tryCastMap (a clean map try_casts at
// ObjectToMap score 1), a partial-value map (fixture 45), and THE over-claim guard
// — a class arm with an extra key loses to the map (the class try_cast is strict).
func TestUnion_MapArm(t *testing.T) {
	// map<string,int> | string with a clean object → the map try_casts (score 1).
	msu := unionFieldSchema(
		&bamlutils.DynamicTypeSpec{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}},
		&bamlutils.DynamicTypeSpec{Type: "string"},
	)
	mustParse(t, msu, `{"u":{"a":1}}`, `{"u":{"a":1}}`)
	// A partial-value map: the bad value is skipped (MapValueParseError), the map arm
	// ({"a":1}) beats the string arm's stringification.
	mustParse(t, msu, `{"u":{"a":1,"b":"x"}}`, `{"u":{"a":1}}`)

	// OVER-CLAIM GUARD: C{a:int} | map<string,int> with an extra key → the MAP wins
	// (the class try_cast rejects the extra key; the map try_cast keeps it). Without
	// tryCastMap native would wrongly pick the lower-index class (ExtraKey).
	cmu := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Ref: "C"},
				{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}},
			},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: props(kv("a", intProp()))}),
		),
	}
	mustParse(t, cmu, `{"u":{"a":1,"extra":2}}`, `{"u":{"a":1,"extra":2}}`)
}

// TestUnion_MapArmNonStringKeyDeclines pins the gate: a map arm with an ENUM /
// literal key stays out of scope in a union (dynamic map-key keep is unproven).
func TestUnion_MapArmNonStringKeyDeclines(t *testing.T) {
	// map<"a"|"b", int> | string — a string-literal-union key is a legal map key but
	// not a plain string, so the map union arm declines at the gate.
	litKey := &bamlutils.DynamicTypeSpec{
		Type:  "union",
		OneOf: []*bamlutils.DynamicTypeSpec{{Type: "literal_string", Value: "a"}, {Type: "literal_string", Value: "b"}},
	}
	s := unionFieldSchema(
		&bamlutils.DynamicTypeSpec{Type: "map", Keys: litKey, Values: &bamlutils.DynamicTypeSpec{Type: "int"}},
		&bamlutils.DynamicTypeSpec{Type: "string"},
	)
	requireUnsupported(t, s, `{"u":{"a":1}}`)
}

// TestUnion_ListArmTryCast pins THE list-arm phase-1 try_cast (the over-claim the
// cold review caught): a list<int> arm try_cast REJECTS a numeric-string element
// while its lenient coerce would accept it, so BAML's try_cast_union picks the
// list<string> arm — native must reproduce this, not lenient-early-return [1].
func TestUnion_ListArmTryCast(t *testing.T) {
	llu := unionFieldSchema(
		&bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "int"}},
		&bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}},
	)
	// list<int> try_cast fails on "1" (int try_cast rejects a string); list<string>
	// try_casts at score 0 → wins with ["1"].
	mustParse(t, llu, `{"u":["1"]}`, `{"u":["1"]}`)
	// A JSON number array: list<int> try_casts at score 0 (arm 0) → [1,2].
	mustParse(t, llu, `{"u":[1,2]}`, `{"u":[1,2]}`)
	// A single-non-null-arm optional element (list<int?>) as an arm is safe: its
	// only union hint is the one arm, so it try_casts each element independently.
	optElem := unionFieldSchema(
		&bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "int"}}},
		&bamlutils.DynamicTypeSpec{Type: "string"},
	)
	mustParse(t, optElem, `{"u":[1,2]}`, `{"u":[1,2]}`)
}

// TestUnion_MapValueTryCast pins the map-VALUE phase-1 try_cast for list- and
// union-valued maps (the same over-claim root, reachable through the map-arm gate).
func TestUnion_MapValueTryCast(t *testing.T) {
	// map<string,list<int>> | map<string,list<string>> with {"a":["1"]}: the first
	// map's value try_cast fails (list<int> rejects "1"), the second wins.
	mlu := unionFieldSchema(
		&bamlutils.DynamicTypeSpec{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "int"}}},
		&bamlutils.DynamicTypeSpec{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}}},
	)
	mustParse(t, mlu, `{"u":{"a":["1"]}}`, `{"u":{"a":["1"]}}`)

	// map<string,int> | map<string,int|string> with {"a":"1"}: the first map's value
	// try_cast fails (int rejects "1"), the second's inner int|string try_cast picks
	// the string arm → {"a":"1"}.
	muu := unionFieldSchema(
		&bamlutils.DynamicTypeSpec{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}},
		&bamlutils.DynamicTypeSpec{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Type: "int"}, {Type: "string"}},
		}},
	)
	mustParse(t, muu, `{"u":{"a":"1"}}`, `{"u":{"a":"1"}}`)
}

// TestCheckUnionMapVariant_ConstrainedKeyDeclines pins FIX B: a union map arm with
// a CONSTRAINED string key declines (native does not model key constraints). The
// dynamic bridge has no constraint channel, so this is exercised by constructing the
// schema.Type directly.
func TestCheckUnionMapVariant_ConstrainedKeyDeclines(t *testing.T) {
	constrainedKey := schema.Type{
		Kind:      schema.TypePrimitive,
		Primitive: schema.PrimitiveString,
		Meta:      schema.TypeMeta{Constraints: []schema.Constraint{{Level: schema.ConstraintAssert, Expression: "this|length > 0"}}},
	}
	valT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
	mapArm := schema.Type{Kind: schema.TypeMap, Key: &constrainedKey, Value: &valT}
	if err := checkUnionMapVariant(nil, mapArm); err == nil || !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("constrained-key map union arm: expected ErrDeBAMLParseUnsupported, got %v", err)
	}
	// A plain string key is admitted.
	plainKey := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	if err := checkUnionMapVariant(nil, schema.Type{Kind: schema.TypeMap, Key: &plainKey, Value: &valT}); err != nil {
		t.Fatalf("plain string-key map union arm: unexpected error %v", err)
	}
}

// TestList_MultiArmUnionElementDeclines pins the list<union> guard: an array whose
// element is a MULTI-ARM union declines (the array union_variant_hint is deferred).
func TestList_MultiArmUnionElementDeclines(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{
		Type: "list",
		Items: &bamlutils.DynamicTypeSpec{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Type: "int"}, {Type: "string"}},
		},
	})
	requireUnsupported(t, s, `{"u":[1,"x"]}`)
	// A single-non-null-arm optional element (list<int?>) is NOT multi-arm → in scope.
	opt := oneField(&bamlutils.DynamicProperty{
		Type:  "list",
		Items: &bamlutils.DynamicTypeSpec{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "int"}},
	})
	mustParse(t, opt, `{"u":[1,2]}`, `{"u":[1,2]}`)
}
