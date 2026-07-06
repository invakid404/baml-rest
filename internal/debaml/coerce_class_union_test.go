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
		out, kind, matched, err := tryCastClass(b, aT.Name, aT.Mode, in)
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
	// A class arm with an OPTIONAL field declines at the gate (its non-zero try_cast
	// / default scoring in a union is M3d).
	optField := abClassUnionSchema(
		props(kv("a", intProp()), kv("b", optProp(&bamlutils.DynamicTypeSpec{Type: "string"}))),
		props(kv("c", intProp()), kv("d", strProp())),
	)
	requireUnsupported(t, optField, `{"u":{"a":1,"b":"x"}}`)

	// A class arm with a LIST field declines at the gate (single-to-array is M3d).
	listField := abClassUnionSchema(
		props(kv("a", intProp()), kv("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}})),
		props(kv("c", intProp()), kv("d", strProp())),
	)
	requireUnsupported(t, listField, `{"u":{"a":1,"tags":["x"]}}`)

	// A class arm with a MAP field declines at the gate.
	mapField := abClassUnionSchema(
		props(kv("a", intProp()), kv("m", &bamlutils.DynamicProperty{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Type: "string"}, Values: &bamlutils.DynamicTypeSpec{Type: "int"}})),
		props(kv("c", intProp()), kv("d", strProp())),
	)
	requireUnsupported(t, mapField, `{"u":{"a":1,"m":{"k":1}}}`)

	// ARRAY input to a class union declines: BAML runs coerce_array_to_singular /
	// pick_best over the items (array-to-singular is M3d), which native does not
	// model, so every class arm declines the array and the union falls back.
	arr := abClassUnionSchema(
		props(kv("a", intProp()), kv("b", strProp())),
		props(kv("c", intProp()), kv("d", strProp())),
	)
	requireUnsupported(t, arr, `{"u":[{"a":1,"b":"x"}]}`)
}
