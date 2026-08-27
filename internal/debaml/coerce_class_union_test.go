package debaml

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// M3 slice c — CLASS + MIXED literal/enum/class union scoring.
// checkSupportedUnionShape now admits CLASS arms (constraint-free, required
// flat-leaf fields, single-field allowed, overlapping keys allowed) and any MIX of
// scalar/literal/enum/class. coerceUnionSafeMulti resolves them TWO-PHASE like BAML:
// a phase-1 try_cast pass (tryCastClass ports Class::try_cast — a STRICT exact-key
// object cast) and, only when NO arm try_casts, a phase-2 lenient coerce +
// array_helper::pick_best (with the class / scalar-vs-composite special ordering).
// These tests pin the class try_cast phase directly, the mixed families end-to-end,
// and the hard guards that STAY fallback.

// abClassUnionSchema builds Root{u: A | B} over two classes with the given field
// specs, for the class-union end-to-end tests.
func abClassUnionSchema(aFields, bFields bamlutils.OrderedMap[*bamlutils.DynamicProperty]) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "B"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: aFields}),
			bamlutils.OrderedKV("B", &bamlutils.DynamicClass{Properties: bFields}),
		),
	}
}

// TestTryCastClass_Strict pins Class::try_cast (tryCastClass): it matches ONLY an
// object whose keys EXACTLY equal a subset of the fields with NO extra key and every
// field present and value-type-matching, emitting in SCHEMA field order. Any other
// shape returns not-matched (the caller falls to the lenient phase).
func TestTryCastClass_Strict(t *testing.T) {
	s := abClassUnionSchema(
		props(kv("id", intProp()), kv("name", strProp())),
		props(kv("id", intProp()), kv("label", strProp())),
	)
	b, aT := classBundle(t, s, "A") // A{id int, name string}

	assertMatch := func(name string, in value, wantOut string, wantMatched bool) {
		out, kind, _, matched, err := tryCastClass(b, aT.Name, aT.Mode, in, nil)
		if err != nil {
			t.Fatalf("%s: unexpected err: %v", name, err)
		}
		if matched != wantMatched {
			t.Errorf("%s: matched=%v, want %v", name, matched, wantMatched)
			return
		}
		if kind != candClass {
			t.Errorf("%s: kind=%v, want candClass", name, kind)
		}
		if matched && string(out) != wantOut {
			t.Errorf("%s: out=%s, want %s", name, out, wantOut)
		}
	}

	// Exact full-field-set match -> score-0 cast, schema field order.
	assertMatch("exact", objVal(fld("id", numV("1")), fld("name", strVv("x"))), `{"id":1,"name":"x"}`, true)
	// Reordered input keys still emit in SCHEMA order (id before name).
	assertMatch("reordered", objVal(fld("name", strVv("x")), fld("id", numV("1"))), `{"id":1,"name":"x"}`, true)
	// An EXTRA key rejects the strict cast (BAML try_cast returns None on extras).
	assertMatch("extra key", objVal(fld("id", numV("1")), fld("name", strVv("x")), fld("z", numV("9"))), "", false)
	// A MISSING required field rejects the cast (no optional fills in the gate).
	assertMatch("missing field", objVal(fld("id", numV("1"))), "", false)
	// A field VALUE whose native JSON type mismatches (string into int) rejects the
	// cast — try_cast is strict, so a numeric STRING "1" does not cast to int.
	assertMatch("field type mismatch", objVal(fld("id", strVv("1")), fld("name", strVv("x"))), "", false)
	// A non-object never casts to a class (the lenient inferred-object path is not
	// try_cast).
	assertMatch("scalar input", numV("5"), "", false)
	assertMatch("array input", value{kind: valArray, arrV: []value{numV("1")}}, "", false)
	assertMatch("null input", nullVal(), "", false)
}

// TestClassUnion_TryCastFirstWinner pins the phase-1 fast path for class unions: an
// input whose full field set is exactly one arm's fields returns that arm at score 0
// BEFORE the other arm (or any lenient scoring) is considered — even when the arms
// share a field name (fixture 39, formerly declined at the disjoint-key gate).
func TestClassUnion_TryCastFirstWinner(t *testing.T) {
	// A{id,name} | B{id,label} — overlapping `id`. Input == A's full field set.
	s := abClassUnionSchema(
		props(kv("id", intProp()), kv("name", strProp())),
		props(kv("id", intProp()), kv("label", strProp())),
	)
	mustParse(t, s, `{"u":{"id":1,"name":"x"}}`, `{"u":{"id":1,"name":"x"}}`)
	// Input == B's full field set -> B try_casts (A rejects the extra `label`).
	mustParse(t, s, `{"u":{"id":2,"label":"y"}}`, `{"u":{"id":2,"label":"y"}}`)
	// An input carrying BOTH `name` and `label` casts to NEITHER arm (each sees an
	// extra key) -> phase 2: A keeps id,name (label extra, ExtraKey 1), B keeps
	// id,label (name extra, ExtraKey 1); both score 1, so the lower-index arm A wins.
	mustParse(t, s, `{"u":{"id":1,"name":"x","label":"y"}}`, `{"u":{"id":1,"name":"x"}}`)
}

// TestMixedUnion_LiteralClass_TryCast pins fixture 42: a mixed literal-vs-class
// union ("active" | Status{tag}) with a single-key object. The literal arm's
// try_cast rejects an object; the class arm's try_cast matches Status{tag:"active"}
// at score 0, so it wins in phase 1 (the literal's object-to-primitive extraction —
// a phase-2 path — is never reached).
func TestMixedUnion_LiteralClass_TryCast(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "literal_string", Value: "active"},
				{Ref: "Status"},
			},
		})),
		Classes: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Status", &bamlutils.DynamicClass{
			Properties: props(kv("tag", strProp())),
		})),
	}
	mustParse(t, s, `{"u":{"tag":"active"}}`, `{"u":{"tag":"active"}}`)
	// A bare string "active" try_casts the LITERAL arm (index 0) instead.
	mustParse(t, s, `{"u":"active"}`, `{"u":"active"}`)
}

// TestMixedUnion_EnumClass_Scored pins fixture 43: an enum-vs-class union (Color |
// Detail{a,b}) with an object carrying both class fields AND an enum token as an
// EXTRA key. Neither arm try_casts (the enum needs a string; the class sees the
// extra `color` key), so phase 2 runs: the enum stringifies the object and
// substring-matches RED (ObjectToString 2 + SubstringMatch 2 = 4) while Detail keeps
// a,b and flags the extra `color` (ExtraKey 1). Detail's lower score wins.
func TestMixedUnion_EnumClass_Scored(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Ref: "Color"},
				{Ref: "Detail"},
			},
		})),
		Classes: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Detail", &bamlutils.DynamicClass{
			Properties: props(kv("a", intProp()), kv("b", intProp())),
		})),
		Enums: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Color", &bamlutils.DynamicEnum{
			Values: []*bamlutils.DynamicEnumValue{{Name: "RED"}, {Name: "GREEN"}, {Name: "BLUE"}},
		})),
	}
	mustParse(t, s, `{"u":{"a":1,"b":2,"color":"RED"}}`, `{"u":{"a":1,"b":2}}`)
}

// TestClassUnion_SingleFieldInferredScored pins fixture 40: a union of single-field
// classes A{val int} | B{num int} with a bare scalar. No arm try_casts (a scalar is
// not an object), so phase 2 runs: both absorb 5 via inferred-object (ImpliedKey
// score 2); classSingleImplied is FALSE for both (the implied field is an int, not a
// string), so no devalue fires and the lower-index arm A wins on (score, index).
func TestClassUnion_SingleFieldInferredScored(t *testing.T) {
	s := abClassUnionSchema(props(kv("val", intProp())), props(kv("num", intProp())))
	mustParse(t, s, `{"u":5}`, `{"u":{"val":5}}`)
}

// TestClassUnion_ChildScoreDecides pins a class union resolved purely by child
// (field) scores in phase 2. A{a int, b string} | B{c int, d string} with an input
// whose keys hit both arms' field sets: neither try_casts (each sees the other arm's
// keys as extras), so phase 2 scores A (ExtraKey 2) vs B (ExtraKey 2); the tie breaks
// to the lower-index arm A (fixture 75 shape).
func TestClassUnion_ChildScoreDecides(t *testing.T) {
	s := abClassUnionSchema(
		props(kv("a", intProp()), kv("b", strProp())),
		props(kv("c", intProp()), kv("d", strProp())),
	)
	mustParse(t, s, `{"u":{"a":1,"b":"x","c":"5","d":"y"}}`, `{"u":{"a":1,"b":"x"}}`)
	// A JsonToString field score tips the winner: A's `b` takes a number, so A
	// scores 2 (JsonToString) + 2 (c,d extras) = 4 while B scores 2 (c=5 clean, a,b
	// extras ExtraKey 2) -> B wins.
	mustParse(t, s, `{"u":{"a":1,"b":2,"c":5,"d":"y"}}`, `{"u":{"c":5,"d":"y"}}`)
}

// TestClassUnion_ProvableLosingArmExcluded pins that a class arm whose REQUIRED field
// provably fails to coerce is EXCLUDED from scoring (not a whole-union decline), so
// the other arm claims — through the broadened gate. A{a int,b string} | B{c int,d
// string}: c="bad" provably fails int coercion -> B errors -> A wins.
func TestClassUnion_ProvableLosingArmExcluded(t *testing.T) {
	s := abClassUnionSchema(
		props(kv("a", intProp()), kv("b", strProp())),
		props(kv("c", intProp()), kv("d", strProp())),
	)
	mustParse(t, s, `{"u":{"a":1,"b":"x","c":"bad","d":"y"}}`, `{"u":{"a":1,"b":"x"}}`)
}

// TestClassUnion_HardGuardsStayFallback pins the load-bearing over-claim guards for
// M3c class/mixed unions.
func TestClassUnion_HardGuardsStayFallback(t *testing.T) {
	// (A class arm with a SINGLE-non-null OPTIONAL field is no longer a hard guard —
	// Batch 2 admits it and models its try_cast score; see TestClassUnion_OptionalField.)

	// A class arm with a required LIST or STRING-keyed MAP field is CLAIMED now:
	// tryCastArray / tryCastMap cover phase 1 (their scores summed into the class
	// try_cast score), coerceList / coerceMap cover phase 2, and an ABSENT one is
	// TypeIR::default_value-filled to [] / {} (DefaultFromNoValue, score 100). Both
	// are held byte-exact against live BAML by the parse-recovery differential.
	listField := abClassUnionSchema(
		props(kv("a", intProp()), kv("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}})),
		props(kv("c", intProp()), kv("d", strProp())),
	)
	mustParse(t, listField, `{"u":{"a":1,"tags":["x"]}}`, `{"u":{"a":1,"tags":["x"]}}`)
	mustParse(t, listField, `{"u":{"a":1}}`, `{"u":{"a":1,"tags":[]}}`)

	mapField := abClassUnionSchema(
		props(kv("a", intProp()), kv("m", &bamlutils.DynamicProperty{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}})),
		props(kv("c", intProp()), kv("d", strProp())),
	)
	mustParse(t, mapField, `{"u":{"a":1,"m":{"k":1}}}`, `{"u":{"a":1,"m":{"k":1}}}`)
	mustParse(t, mapField, `{"u":{"a":1}}`, `{"u":{"a":1,"m":{}}}`)

	// A class arm with a NON-STRING-keyed map field still declines: an enum/literal
	// key that MISSES is kept leniently with an unproven-weight MapKeyParseError,
	// and a union arm is always RANKED, so its score may not be guessed.
	enumKeyMapField := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "B"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{
				Properties: props(kv("a", intProp()), kv("m", &bamlutils.DynamicProperty{
					Type:   "map",
					Keys:   &bamlutils.DynamicTypeSpec{Ref: "Color"},
					Values: &bamlutils.DynamicTypeSpec{Type: "int"},
				})),
			}),
			bamlutils.OrderedKV("B", &bamlutils.DynamicClass{
				Properties: props(kv("c", intProp()), kv("d", strProp())),
			}),
		),
		Enums: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Color", &bamlutils.DynamicEnum{
			Values: []*bamlutils.DynamicEnumValue{{Name: "RED"}, {Name: "GREEN"}},
		})),
	}
	requireUnsupported(t, enumKeyMapField, `{"u":{"a":1,"m":{"RED":1}}}`)

	// M3d: ARRAY input to a class union of MULTI-field all-required-flat-leaf classes
	// now CLAIMS via class array-to-singular. The A arm ({a,b}) array-to-singulars
	// the lone object [{a:1,b:"x"}] into A{a:1,b:"x"} (+FirstMatch); the B arm
	// ({c,d}) errors on the item (keys a,b are extras, c,d missing → a proven
	// coerce_class error), so it is excluded — A wins.
	arr := abClassUnionSchema(
		props(kv("a", intProp()), kv("b", strProp())),
		props(kv("c", intProp()), kv("d", strProp())),
	)
	mustParse(t, arr, `{"u":[{"a":1,"b":"x"}]}`, `{"u":{"a":1,"b":"x"}}`)
}

// TestClassUnion_CollectionClassFieldHeldToUnionRules pins the cold-review fix: a
// class-valued LIST element / MAP value inside a class-union arm must be held to the
// SAME union-arm class rules the arm itself is, because checkSupportedType stops at a
// class REFERENCE (class definitions are validated once over the bundle's class
// slice, under the ORDINARY field rules). checkUnionCollectionClasses holds every
// class reachable through the collection to the SAME union-arm rules the arm itself
// obeys — so a `B` carrying a NESTED-CLASS or multi-arm-union field is still rejected
// at depth 1, exactly as at depth 0. (Batch 2 admits a SINGLE-non-null OPTIONAL field
// at both depths — BAML's Class::try_cast fills the missing optional at score 1, which
// tryCastClass now models — so a B with an optional field is now IN scope.)
//
// The restriction is a boundary, not a blanket: a collection of an IN-SCOPE class
// (all required flat leaves or single-non-null optionals), including a nested
// list-of-list, still CLAIMS.
func TestClassUnion_CollectionClassFieldHeldToUnionRules(t *testing.T) {
	// arm builds Root{u: A|C} with A carrying the given field and C{name string}.
	arm := func(field string, spec *bamlutils.DynamicProperty, inner bamlutils.OrderedMap[*bamlutils.DynamicProperty]) *bamlutils.DynamicOutputSchema {
		return &bamlutils.DynamicOutputSchema{
			Properties: props(kv("u", &bamlutils.DynamicProperty{
				Type:  "union",
				OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "C"}},
			})),
			Classes: bamlutils.MustOrderedMap(
				bamlutils.OrderedKV("A", &bamlutils.DynamicClass{Properties: props(kv(field, spec))}),
				bamlutils.OrderedKV("B", &bamlutils.DynamicClass{Properties: inner}),
				bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: props(kv("name", strProp()))}),
			),
		}
	}
	listOfB := &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Ref: "B"}}
	mapOfB := &bamlutils.DynamicProperty{
		Type: "map",
		Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Ref: "B"},
	}
	nestedListOfB := &bamlutils.DynamicProperty{
		Type:  "list",
		Items: &bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Ref: "B"}},
	}

	// Batch 2: B with a SINGLE-non-null OPTIONAL field is IN scope through a list, a
	// map, and a nested list — checkUnionCollectionClasses reaches checkUnionClassField,
	// which now admits the optional wrapper, and tryCastClass fills B's absent `y` with
	// null (OptionalDefaultFromNoValue) exactly as BAML does (the A arm's non-zero class
	// try_cast lands in try_cast_union's pick_best sub-path; C rejects the object).
	bOptional := props(kv("x", intProp()), kv("y", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})))
	mustParse(t, arm("items", listOfB, bOptional), `{"u":{"items":[{"x":1}]}}`, `{"u":{"items":[{"x":1,"y":null}]}}`)
	mustParse(t, arm("m", mapOfB, bOptional), `{"u":{"m":{"k":{"x":1}}}}`, `{"u":{"m":{"k":{"x":1,"y":null}}}}`)
	mustParse(t, arm("g", nestedListOfB, bOptional), `{"u":{"g":[[{"x":1}]]}}`, `{"u":{"g":[[{"x":1,"y":null}]]}}`)
	// An EMPTY collection claims (schema admitted; nothing to fill), and the C arm
	// still wins for its own shape under the B-optional schema.
	mustParse(t, arm("items", listOfB, bOptional), `{"u":{"items":[]}}`, `{"u":{"items":[]}}`)
	mustParse(t, arm("items", listOfB, bOptional), `{"u":{"name":"n"}}`, `{"u":{"name":"n"}}`)

	// B with a NESTED-CLASS field is STILL out of scope through the collection.
	bNested := props(kv("x", intProp()), kv("inner", &bamlutils.DynamicProperty{Ref: "D"}))
	nestedSchema := arm("items", listOfB, bNested)
	_ = nestedSchema.Classes.Set("D", &bamlutils.DynamicClass{Properties: props(kv("v", intProp()))})
	requireUnsupported(t, nestedSchema, `{"u":{"items":[{"x":1,"inner":{"v":2}}]}}`)

	// An IN-SCOPE B (all required flat leaves) still CLAIMS through a list, a map and
	// a nested list — the restriction must not swallow the family it was added to.
	bOK := props(kv("x", intProp()), kv("y", strProp()))
	mustParse(t, arm("items", listOfB, bOK), `{"u":{"items":[{"x":1,"y":"z"}]}}`, `{"u":{"items":[{"x":1,"y":"z"}]}}`)
	mustParse(t, arm("items", listOfB, bOK), `{"u":{"items":[]}}`, `{"u":{"items":[]}}`)
	mustParse(t, arm("m", mapOfB, bOK), `{"u":{"m":{"k":{"x":1,"y":"z"}}}}`, `{"u":{"m":{"k":{"x":1,"y":"z"}}}}`)
	mustParse(t, arm("g", nestedListOfB, bOK), `{"u":{"g":[[{"x":1,"y":"z"}]]}}`, `{"u":{"g":[[{"x":1,"y":"z"}]]}}`)
	// The OTHER arm still wins when the input is its shape.
	mustParse(t, arm("items", listOfB, bOK), `{"u":{"name":"n"}}`, `{"u":{"name":"n"}}`)
}

// TestClassUnion_CollectionClassCycleDeclines pins the walk's termination guard: a
// class-valued collection that leads back to a class already on the walk (A{items
// list<A>}) DECLINES instead of recursing forever. A recursive class inside a union
// arm is out of scope anyway; the guard is what makes that a decline rather than a
// hang.
func TestClassUnion_CollectionClassCycleDeclines(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "C"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{
				Properties: props(kv("items", &bamlutils.DynamicProperty{
					Type: "list", Items: &bamlutils.DynamicTypeSpec{Ref: "A"},
				})),
			}),
			bamlutils.OrderedKV("C", &bamlutils.DynamicClass{Properties: props(kv("name", strProp()))}),
		),
	}
	requireUnsupported(t, s, `{"u":{"items":[]}}`)
}
