package debaml

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// M3 slice a — SCORE MODEL + safe-family pick_best. The safe-family union
// coercers (coerceScalarLeafUnion / coerceFlatClassUnion) now score every arm with
// the types.rs inherent model, apply BAML's early first-score-0 winner rule, and
// otherwise run a faithful array_helper::pick_best over the successful candidates
// plus (when nullable) the null arm (DefaultButHadValue, score 110). No gate
// broadening — only the existing literal/class safe families are scored. These
// tests pin the flips from the pre-M3 clean-only rule to scored selection.

// TestLiteralUnion_ObjectToPrimitive_OneSuccess pins that a homogeneous
// int-literal union claims when a single-key-object extraction (ObjectToPrimitive)
// makes EXACTLY one arm coerce to its value: 1|2 with {"value":1} -> 1.
func TestLiteralUnion_ObjectToPrimitive_OneSuccess(t *testing.T) {
	s := litIntUnion(1, 2)
	mustParse(t, s, `{"u":{"value":1}}`, `{"u":1}`) // extract 1 -> matches arm 1 only
	mustParse(t, s, `{"u":{"any":2}}`, `{"u":2}`)   // key ignored, extract 2 -> arm 2 only
	// Extracted inner value matches NO arm -> both arms are proven BAML errors,
	// so the (non-nullable) union has no success and declines.
	requireUnsupported(t, s, `{"u":{"value":7}}`)
}

// TestLiteralUnion_ObjectToString_OneSuccess pins that a homogeneous
// string-literal union claims when stringification (ObjectToString) makes
// EXACTLY one arm match: "5"|"6" with 5 -> "5".
func TestLiteralUnion_ObjectToString_OneSuccess(t *testing.T) {
	s := unionSchema(litStr("5"), litStr("6"))
	mustParse(t, s, `{"u":5}`, `{"u":"5"}`) // Display "5" matches literal "5" only
	mustParse(t, s, `{"u":6}`, `{"u":"6"}`)
	// Display matches neither literal -> both proven errors -> decline.
	requireUnsupported(t, s, `{"u":7}`)
}

// TestLiteralUnion_TwoSubstring_Scored pins the M3 pick_best selection: a value
// that substring-matches BOTH disjoint literal arms is two successes with equal
// score, so the lower-index arm ("foo", arm 0) wins instead of declining. The
// array Display "[foo, bar]" substring-matches both "foo" and "bar".
func TestLiteralUnion_TwoSubstring_Scored(t *testing.T) {
	s := unionSchema(litStr("foo"), litStr("bar"))
	// Each arm: ObjectToString (2) + SubstringMatch (2) = score 4; tie -> arm 0.
	mustParse(t, s, `{"u":["foo","bar"]}`, `{"u":"foo"}`)
}

// TestLiteralUnion_NullableFlaggedArm_Scored pins that a non-null arm won only
// via ObjectToPrimitive / ObjectToString (score 2) now BEATS the scored null arm
// (110) and is claimed, while the JSON-null fast path still claims null and a
// clean exact match wins immediately.
func TestLiteralUnion_NullableFlaggedArm_Scored(t *testing.T) {
	sInt := oneField(&bamlutils.DynamicProperty{
		Type: "union",
		OneOf: []*bamlutils.DynamicTypeSpec{
			{Type: "literal_int", Value: int64(1)},
			{Type: "literal_int", Value: int64(2)},
			{Type: "null"},
		},
	})
	mustParse(t, sInt, `{"u":null}`, `{"u":null}`)     // null fast path
	mustParse(t, sInt, `{"u":{"value":1}}`, `{"u":1}`) // ObjectToPrimitive score 2 < 110

	sStr := unionSchema(litStr("5"), litStr("6"), &bamlutils.DynamicTypeSpec{Type: "null"})
	mustParse(t, sStr, `{"u":null}`, `{"u":null}`)
	mustParse(t, sStr, `{"u":"5"}`, `{"u":"5"}`) // phase-1 try_cast exact match, no phase-2 scoring
	mustParse(t, sStr, `{"u":5}`, `{"u":"5"}`)   // number: phase-2 ObjectToString score 2 < 110 -> claim
}

// TestClassUnion_Stringification_OneSuccess pins that a flat disjoint-key class
// union claims when field stringification (JsonToString) inside EXACTLY one arm
// makes it succeed: Book{title,pages}|Car{brand,wheels}, title <- 5 -> "5".
func TestClassUnion_Stringification_OneSuccess(t *testing.T) {
	s := classUnionSchema()
	mustParse(t, s, `{"u":{"title":5,"pages":300}}`, `{"u":{"title":"5","pages":300}}`)
}

// TestClassUnion_Stringification_TwoSuccesses_Scored pins the M3 pick_best flip:
// an input carrying BOTH arms' full field sets makes both succeed, and pick_best
// picks the lower-score class. Book carries JsonToString (2) + 2 extras = 4; Car
// carries only 2 extras = 2, so Car wins.
func TestClassUnion_Stringification_TwoSuccesses_Scored(t *testing.T) {
	s := classUnionSchema()
	mustParse(t, s, `{"u":{"title":5,"pages":300,"brand":"Audi","wheels":4}}`, `{"u":{"brand":"Audi","wheels":4}}`)
}

// TestClassUnion_NullableStringified_Scored pins that a JsonToString-flagged
// winning class arm (score 2 < 110) now beats the scored null arm and is claimed,
// while the JSON-null fast path still claims null.
func TestClassUnion_NullableStringified_Scored(t *testing.T) {
	s := nullableClassUnionSchema()
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":{"title":5,"pages":300}}`, `{"u":{"title":"5","pages":300}}`) // JsonToString score 2 < 110
}

// TestNullableOptionalString_JsonToString_Scored pins that an optional string
// with a NON-string input coerces via JsonToString (score 2 < 110) and is now
// CLAIMED, while a clean string passthrough wins immediately.
func TestNullableOptionalString_JsonToString_Scored(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "string"}})
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":"hi"}`, `{"u":"hi"}`) // clean passthrough (score 0)
	mustParse(t, s, `{"u":5}`, `{"u":"5"}`)     // JsonToString score 2 < 110 -> claim
}

// TestNullableOptionalClassDefault_Scored pins that an optional class whose lone
// arm fills a required-field DEFAULT (DefaultFromNoValue, score 100) still beats
// the null arm (110) and is CLAIMED (C{items int[]} with {} -> {items:[]}).
func TestNullableOptionalClassDefault_Scored(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", optProp(&bamlutils.DynamicTypeSpec{Ref: "C"}))),
		Classes: bamlutils.MustOrderedMap(bamlutils.OrderedKV("C", &bamlutils.DynamicClass{
			Properties: props(kv("items", listProp(&bamlutils.DynamicTypeSpec{Type: "int"}))),
		})),
	}
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":{}}`, `{"u":{"items":[]}}`) // DefaultFromNoValue 100 < 110 -> claim
}
