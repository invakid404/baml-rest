package debaml

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// M3 slice b — SCALAR / LITERAL / ENUM (+ null) and flattened-scalar NESTED
// unions. checkSupportedUnionShape now admits any union whose arms are all
// fully-modeled non-composite leaves, and coerceScalarLeafUnion resolves them
// TWO-PHASE like BAML: a phase-1 try_cast pass (the first arm whose STRICT
// native-JSON-type cast matches wins at score 0, before any lenient conversion)
// and, only when no arm try_casts, a phase-2 lenient coerce pass (the M3a
// per-kind coercers + early first-score-0 + array_helper::pick_best). These tests
// pin the new claimed families, the observable try_cast / order-reversal
// behavior, enum arms (including alias/description forms the corpus IR cannot
// express), and the hard guards that STAY fallback (array-to-singular,
// non-finite float, no-match, list-element unions).

// primUnion is a terse builder for a `u` field that is a union of the given
// primitive type names (e.g. "string", "int", "float", "bool").
func primUnion(prims ...string) *bamlutils.DynamicOutputSchema {
	variants := make([]*bamlutils.DynamicTypeSpec, len(prims))
	for i, p := range prims {
		variants[i] = &bamlutils.DynamicTypeSpec{Type: p}
	}
	return unionSchema(variants...)
}

// TestScalarUnion_TryCastPrefersNativeType pins BAML's try_cast-first rule: an
// arm whose STRICT native-JSON-type cast matches the input wins at score 0 BEFORE
// any lenient conversion, regardless of declaration order. So a numeric STRING
// picks the string arm (the int arm's try_cast rejects a string) and a JSON
// number picks the int arm (the string arm's try_cast rejects a number) — in both
// orderings.
func TestScalarUnion_TryCastPrefersNativeType(t *testing.T) {
	si := primUnion("string", "int")
	is := primUnion("int", "string")
	// A numeric STRING stays a string under BOTH orderings — the int arm's
	// try_cast rejects a string, so the string arm's try_cast wins.
	mustParse(t, si, `{"u":"123"}`, `{"u":"123"}`)
	mustParse(t, is, `{"u":"123"}`, `{"u":"123"}`)
	// A JSON NUMBER coerces to int under BOTH orderings — the string arm's
	// try_cast rejects a number, so the int arm's try_cast wins.
	mustParse(t, si, `{"u":5}`, `{"u":5}`)
	mustParse(t, is, `{"u":5}`, `{"u":5}`)
	// A plain (non-numeric) string: the string arm try_casts under both orderings,
	// so the int arm's lenient parse never runs -> stays a string (CLAIMED).
	mustParse(t, si, `{"u":"hello"}`, `{"u":"hello"}`)
	mustParse(t, is, `{"u":"hello"}`, `{"u":"hello"}`)
}

// TestScalarUnion_IntFloat_Scored pins the two phases for int|float. A JSON
// number try_casts to whichever numeric arm is first (value-equal either way). A
// numeric STRING try_casts to neither (both need a JSON number), so the lenient
// coerce pass runs: the int arm rounds (FloatToInt score 1) while the float arm
// parses clean (score 0), so the float arm wins.
func TestScalarUnion_IntFloat_Scored(t *testing.T) {
	f := primUnion("int", "float")
	// JSON integer: int arm (index 0) try_casts -> int.
	mustParse(t, f, `{"u":5}`, `{"u":5}`)
	// JSON non-integer number: int arm try_cast rejects it (as_i64 None); float arm
	// try_casts -> 1.5.
	mustParse(t, f, `{"u":1.5}`, `{"u":1.5}`)
	// numeric STRING: neither arm try_casts -> lenient pass; float (score 0) beats
	// the rounding int arm (score 1) -> 1.5 (NOT rounded to 2).
	mustParse(t, f, `{"u":"1.5"}`, `{"u":1.5}`)
	// float | int reversal: the float arm (index 0) try_casts the JSON integer
	// first. Numerically 5 (differential compares numbers by value).
	rev := primUnion("float", "int")
	mustParse(t, rev, `{"u":5}`, `{"u":5}`)
}

// TestScalarUnion_BoolString_Scored pins that a bool-looking JSON string is
// claimed by the string arm in phase 1: bool try_cast rejects strings, then
// string try_cast succeeds, so StringToBool phase-2 scoring never runs.
func TestScalarUnion_BoolString_Scored(t *testing.T) {
	bs := primUnion("bool", "string")
	// JSON bool: bool try_cast accepts it in phase 1 -> true.
	mustParse(t, bs, `{"u":true}`, `{"u":true}`)
	// bool-looking STRING: bool try_cast rejects strings; string try_cast succeeds
	// in phase 1 -> stays "true" (no StringToBool scoring).
	mustParse(t, bs, `{"u":"true"}`, `{"u":"true"}`)
}

// colorStringUnion builds `Color | string` (enum arm first) with the given enum
// values, so an exact enum token try_casts (phase 1) ahead of the string arm.
func colorStringUnion(values []*bamlutils.DynamicEnumValue) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "Color"}, {Type: "string"}},
		})),
		Enums: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Color", &bamlutils.DynamicEnum{
			Values: values,
		})),
	}
}

// TestScalarUnion_EnumWithStringArm pins that an enum's try_cast is EXACT
// (rendered-name equality, no fuzz), so it only wins the try_cast pass on an
// exact token; every other string try_casts to the string arm before the enum's
// fuzzy coerce even runs. `Color | string`.
func TestScalarUnion_EnumWithStringArm(t *testing.T) {
	s := colorStringUnion([]*bamlutils.DynamicEnumValue{{Name: "RED"}, {Name: "GREEN"}, {Name: "BLUE"}})
	// exact token: enum arm (index 0) try_casts -> canonical name.
	mustParse(t, s, `{"u":"GREEN"}`, `{"u":"GREEN"}`)
	// lowercase "green": enum try_cast is case-SENSITIVE (miss), so the string arm
	// try_casts first -> stays "green" (the fuzzy enum coerce is never reached).
	mustParse(t, s, `{"u":"green"}`, `{"u":"green"}`)
	// a non-enum string: string arm try_casts -> whole string.
	mustParse(t, s, `{"u":"the color green please"}`, `{"u":"the color green please"}`)
}

// TestScalarUnion_EnumFuzzyInCoercePass pins the enum arm's FUZZY match forms
// (case fold, description, "rendered: description") — reachable only in the
// lenient coerce pass, i.e. when NO arm try_casts. `Color | literal_int 999`
// (the literal_int arm never try_casts a string, so the enum's coerce runs). The
// corpus FuzzEnum IR carries no alias/description, so this is unit-test only.
func TestScalarUnion_EnumFuzzyInCoercePass(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Ref: "Color"},
				{Type: "literal_int", Value: int64(999)},
			},
		})),
		Enums: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Color", &bamlutils.DynamicEnum{
			Values: []*bamlutils.DynamicEnumValue{
				{Name: "GREEN", Alias: "Verde"},
				{Name: "RED", Description: "the red one"},
			},
		})),
	}
	// "Verde" is GREEN's RENDERED name (alias), so the enum arm TRY_CASTS it
	// (exact rendered-name equality) -> canonical "GREEN".
	mustParse(t, s, `{"u":"Verde"}`, `{"u":"GREEN"}`)
	// "verde" (lowercase alias): try_cast is case-sensitive (miss), so the coerce
	// pass runs; the enum arm case-folds "verde" == "Verde" -> GREEN (score 0).
	// (The canonical "GREEN" is NOT a match candidate once an alias is set.)
	mustParse(t, s, `{"u":"verde"}`, `{"u":"GREEN"}`)
	// description and "rendered: description" forms match only in the coerce pass.
	mustParse(t, s, `{"u":"the red one"}`, `{"u":"RED"}`)
	mustParse(t, s, `{"u":"RED: the red one"}`, `{"u":"RED"}`)
}

// TestScalarUnion_NullableAllProvenErrorClaimsNull pins that when every non-null
// arm is a PROVEN BAML error, only the null arm (score 110) survives and native
// claims null. `1 | 2 | null` with 5 (coerces to int but equals neither literal).
func TestScalarUnion_NullableAllProvenErrorClaimsNull(t *testing.T) {
	s := unionSchema(
		&bamlutils.DynamicTypeSpec{Type: "literal_int", Value: int64(1)},
		&bamlutils.DynamicTypeSpec{Type: "literal_int", Value: int64(2)},
		&bamlutils.DynamicTypeSpec{Type: "null"},
	)
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":5}`, `{"u":null}`)
}

// TestScalarUnion_NoMatchAndArrayDecline pins the hard guards. A no-match scalar
// union declines the whole union; ARRAY input to a union with an int/float/bool
// arm declines (array-to-singular is M3d).
func TestScalarUnion_NoMatchAndArrayDecline(t *testing.T) {
	// int | bool with a non-numeric non-bool string: both arms declineCoerce
	// (native-can't-prove) -> whole union declines.
	requireUnsupported(t, primUnion("int", "bool"), `{"u":"hello"}`)
	// ARRAY input to string | int: the string arm stringifies (JsonToString) but
	// the int arm's array-to-singular is M3d and declines -> whole union declines.
	requireUnsupported(t, primUnion("string", "int"), `{"u":[1,2]}`)
}

// TestScalarUnion_NonFiniteFloatDeclines pins the non-finite-float fallback. A
// float|int union with an "inf"/"nan" STRING: no arm try_casts (both need a JSON
// number), so the coerce pass runs; the float arm parses a non-finite value with
// no JSON spelling (declines) and the int arm declines too -> whole union falls
// back. (A `float | string` union would instead let the string arm try_cast the
// literal "inf" to a string, so it is deliberately not used here.)
func TestScalarUnion_NonFiniteFloatDeclines(t *testing.T) {
	fi := primUnion("float", "int")
	requireUnsupported(t, fi, `{"u":"inf"}`)
	requireUnsupported(t, fi, `{"u":"nan"}`)
}

// TestScalarUnion_ListElementUnionDeclines pins the M3b hint guard: a MULTI-ARM
// union as a LIST ELEMENT declines (BAML threads ctx.union_variant_hint between
// array elements; native has no hint, so per-element arm selection can diverge —
// array hints are M3d). A single-non-null-arm optional element, a TOP-LEVEL
// scalar union, and a MAP VALUE union all stay CLAIMED (map values / class fields
// reset the hint, and an optional's only hint is its one arm).
func TestScalarUnion_ListElementUnionDeclines(t *testing.T) {
	// list<int | string> — multi-arm union element -> DECLINE (hint gap).
	listUnion := oneField(&bamlutils.DynamicProperty{
		Type: "list",
		Items: &bamlutils.DynamicTypeSpec{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Type: "int"}, {Type: "string"}},
		},
	})
	requireUnsupported(t, listUnion, `{"u":["a",1]}`)
	// list<Color | string> — the enum|string shape the reviewer flagged -> DECLINE.
	listEnumUnion := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "list",
			Items: &bamlutils.DynamicTypeSpec{
				Type:  "union",
				OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "Color"}, {Type: "string"}},
			},
		})),
		Enums: bamlutils.MustOrderedMap(bamlutils.OrderedKV("Color", &bamlutils.DynamicEnum{
			Values: []*bamlutils.DynamicEnumValue{{Name: "GREEN"}, {Name: "RED"}},
		})),
	}
	requireUnsupported(t, listEnumUnion, `{"u":["other","GREEN"]}`)

	// list<int?> — a single-non-null-arm optional element is hint-safe -> CLAIMED.
	listOptional := oneField(&bamlutils.DynamicProperty{
		Type:  "list",
		Items: &bamlutils.DynamicTypeSpec{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "int"}},
	})
	mustParse(t, listOptional, `{"u":[1,2]}`, `{"u":[1,2]}`)

	// TOP-LEVEL scalar union still claims (the gate change is list-element-only).
	mustParse(t, primUnion("string", "int"), `{"u":"x"}`, `{"u":"x"}`)

	// MAP value union still claims — coerce_map resets the hint (enter_scope), so
	// map<_, union> has no hint gap.
	mapUnion := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("m", &bamlutils.DynamicProperty{
			Type: "map",
			Keys: &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{
				Type:  "union",
				OneOf: []*bamlutils.DynamicTypeSpec{{Type: "string"}, {Type: "int"}},
			},
		})),
	}
	mustParse(t, mapUnion, `{"m":{"a":"x"}}`, `{"m":{"a":"x"}}`)
}

// TestScalarUnion_LiteralStringSubstringTie_PickFirst pins the pick_best index
// tiebreak for a scalar union: an ARRAY whose Display substring-matches BOTH
// disjoint string-literal arms scores 4 on each (ObjectToString 2 + SubstringMatch
// 2), so the lower-index arm wins. (Same as the M3a two-substring fixture, now
// routed through the broadened coerceScalarLeafUnion.)
func TestScalarUnion_LiteralStringSubstringTie_PickFirst(t *testing.T) {
	s := unionSchema(litStr("foo"), litStr("bar"))
	mustParse(t, s, `{"u":["foo","bar"]}`, `{"u":"foo"}`)
}
