package debaml

import (
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// oneField builds a Root schema with a single field u of the given property.
func oneField(p *bamlutils.DynamicProperty) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{Properties: props(kv("u", p))}
}

// TestCoercePrimitiveInt_Lenient pins the end-to-end lenient int coercions a
// non-union int field claims: JSON float rounding, u64 wrap, numeric string,
// fraction, and extracted currency/sentence numbers.
func TestCoercePrimitiveInt_Lenient(t *testing.T) {
	s := oneField(intProp())
	mustParse(t, s, `{"u":1.6}`, `{"u":2}`)
	mustParse(t, s, `{"u":5.0}`, `{"u":5}`)
	mustParse(t, s, `{"u":18446744073709551615}`, `{"u":-1}`) // u64 wrap
	mustParse(t, s, `{"u":"123"}`, `{"u":123}`)
	mustParse(t, s, `{"u":" 123, "}`, `{"u":123}`)
	mustParse(t, s, `{"u":"3/2"}`, `{"u":2}`) // round(1.5)=2
	mustParse(t, s, `{"u":"$1,234"}`, `{"u":1234}`)
	mustParse(t, s, `{"u":"The answer is 10,000"}`, `{"u":10000}`)
	// Non-numeric string: BAML coerce_int errors / null-defaults; native declines.
	requireUnsupported(t, s, `{"u":"hello"}`)
}

// TestCoercePrimitiveFloat_Lenient pins lenient float coercions: numeric
// string, fraction, percent (50%→50.0, not 0.5), and extracted sentence number.
func TestCoercePrimitiveFloat_Lenient(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{Type: "float"})
	mustParse(t, s, `{"u":5}`, `{"u":5}`)
	mustParse(t, s, `{"u":"3.14"}`, `{"u":3.14}`)
	mustParse(t, s, `{"u":"3/2"}`, `{"u":1.5}`)
	mustParse(t, s, `{"u":"50%"}`, `{"u":50.0}`)
	mustParse(t, s, `{"u":"1,234.56"}`, `{"u":1234.56}`)
	requireUnsupported(t, s, `{"u":"nope"}`)
}

// TestCoercePrimitiveBool_Lenient pins lenient bool coercions: casefold and
// match_string substring.
func TestCoercePrimitiveBool_Lenient(t *testing.T) {
	s := oneField(boolProp())
	mustParse(t, s, `{"u":"TRUE"}`, `{"u":true}`)
	mustParse(t, s, `{"u":"False"}`, `{"u":false}`)
	mustParse(t, s, `{"u":"it is true."}`, `{"u":true}`)
	requireUnsupported(t, s, `{"u":"maybe"}`)
}

// TestCoercePrimitiveNull_EndToEnd pins that a non-union null field claims null
// for any non-null value (DefaultButHadValue), and for JSON null.
func TestCoercePrimitiveNull_EndToEnd(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{Type: "null"})
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":5}`, `{"u":null}`)
	mustParse(t, s, `{"u":"anything"}`, `{"u":null}`)
}

// TestCoercePrimitiveString_JsonToString pins Mcoerce-d PR 1: a NON-null
// non-string into a string field now stringifies via jsonish Value Display
// (JsonToString), while a JSON null still DECLINES (BAML error_unexpected_null,
// never stringified). Byte forms are exercised end-to-end (the emitted JSON
// string value must equal BAML's Display).
func TestCoercePrimitiveString_JsonToString(t *testing.T) {
	s := oneField(strProp())
	mustParse(t, s, `{"u":"hi"}`, `{"u":"hi"}`)     // string passthrough (clean)
	mustParse(t, s, `{"u":5}`, `{"u":"5"}`)         // number
	mustParse(t, s, `{"u":true}`, `{"u":"true"}`)   // bool
	mustParse(t, s, `{"u":false}`, `{"u":"false"}`) // bool
	// Object: UNQUOTED keys, ", " between entries, ": " kv separator, nested
	// string values UNQUOTED — the jsonish Value Display form.
	mustParse(t, s, `{"u":{"a":1,"b":"x"}}`, `{"u":"{a: 1, b: x}"}`)
	// Array: ", " between elements, nested string unquoted, nested null -> null.
	mustParse(t, s, `{"u":[1,"x",true,null]}`, `{"u":"[1, x, true, null]"}`)
	// Direct null is NOT stringified — BAML errors, native declines (fall back).
	requireUnsupported(t, s, `{"u":null}`)
}

// TestCoercePrimitive_GoOnlyFloatSpellingsDecline pins the CR-B1 fix
// end-to-end: float spellings Go's ParseFloat accepts but Rust rejects
// (underscores, hex floats) and non-finite results DECLINE for int, float, and
// int-literal targets, so native never claims a value BAML would decline nor
// emits an invalid JSON token.
func TestCoercePrimitive_GoOnlyFloatSpellingsDecline(t *testing.T) {
	ints := oneField(intProp())
	requireUnsupported(t, ints, `{"u":"0x1p4"}`)
	requireUnsupported(t, ints, `{"u":"1_000"}`)
	requireUnsupported(t, ints, `{"u":"0x1p4/2"}`)
	requireUnsupported(t, ints, `{"u":"inf"}`) // non-finite -> decline
	requireUnsupported(t, ints, `{"u":"nan"}`)

	floats := oneField(&bamlutils.DynamicProperty{Type: "float"})
	requireUnsupported(t, floats, `{"u":"0x1p4"}`)
	requireUnsupported(t, floats, `{"u":"1_000"}`)
	requireUnsupported(t, floats, `{"u":"NaN"}`) // non-finite -> no valid JSON
	requireUnsupported(t, floats, `{"u":"inf"}`)
	requireUnsupported(t, floats, `{"u":"NaN/2"}`)

	// Literal int inherits the primitive int reject: "0x1p4" would be 16 in Go
	// (== the literal) but Rust rejects it, so native declines (never claims 16).
	requireUnsupported(t, litIntField(16), `{"u":"0x1p4"}`)
}

// litIntField / litBoolField build a single literal-typed field.
func litIntField(v int64) *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{Type: "literal_int", Value: v})
}
func litBoolField(v bool) *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{Type: "literal_bool", Value: v})
}

// TestCoerceLiteralInt_Lenient pins int-literal coercion by primitive-coerce
// then value-compare, the conservative fallback on mismatch, and (Mcoerce-d PR
// 1) the single-key-object ObjectToPrimitive extraction.
func TestCoerceLiteralInt_Lenient(t *testing.T) {
	s := litIntField(2)
	mustParse(t, s, `{"u":2}`, `{"u":2}`)
	mustParse(t, s, `{"u":"2"}`, `{"u":2}`) // string→int match
	mustParse(t, s, `{"u":1.6}`, `{"u":2}`) // round(1.6)=2 == literal
	requireUnsupported(t, s, `{"u":"5"}`)   // coerces to 5 != 2 -> fallback
	requireUnsupported(t, s, `{"u":3}`)     // 3 != 2 -> fallback
	// Mcoerce-d: single-key object extracts the inner primitive (ObjectToPrimitive).
	mustParse(t, s, `{"u":{"value":"2"}}`, `{"u":2}`) // extract "2" -> 2 == literal
	mustParse(t, s, `{"u":{"any":2}}`, `{"u":2}`)     // key ignored, inner 2 == literal
	requireUnsupported(t, s, `{"u":{"value":"5"}}`)   // extract 5 != 2 -> fallback
}

// TestCoerceLiteralBool_Lenient pins bool-literal coercion by primitive-coerce
// then value-compare, the conservative fallback on mismatch, and (Mcoerce-d PR
// 1) the single-key-object ObjectToPrimitive extraction.
func TestCoerceLiteralBool_Lenient(t *testing.T) {
	s := litBoolField(true)
	mustParse(t, s, `{"u":true}`, `{"u":true}`)
	mustParse(t, s, `{"u":"true"}`, `{"u":true}`) // casefold match
	mustParse(t, s, `{"u":"TRUE"}`, `{"u":true}`)
	requireUnsupported(t, s, `{"u":"false"}`) // coerces to false != true
	requireUnsupported(t, s, `{"u":false}`)   // false != true
	// Mcoerce-d: single-key object extracts the inner primitive (ObjectToPrimitive).
	mustParse(t, s, `{"u":{"value":"true"}}`, `{"u":true}`) // extract "true" -> true
	requireUnsupported(t, s, `{"u":{"value":"false"}}`)     // extract false != true
}

// litIntUnion builds Root{u: v0 | v1} of int literals.
func litIntUnion(v0, v1 int64) *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{
		Type: "union",
		OneOf: []*bamlutils.DynamicTypeSpec{
			{Type: "literal_int", Value: v0},
			{Type: "literal_int", Value: v1},
		},
	})
}

// TestLiteralIntUnion_OneLenientSuccess pins the M2c revisit: a homogeneous
// int-literal union claims when EXACTLY one arm leniently coerces to its value.
func TestLiteralIntUnion_OneLenientSuccess(t *testing.T) {
	s := litIntUnion(1, 2)
	mustParse(t, s, `{"u":2}`, `{"u":2}`)
	mustParse(t, s, `{"u":"2"}`, `{"u":2}`) // "2"→2 matches only arm 2
	mustParse(t, s, `{"u":1.6}`, `{"u":2}`) // round→2 matches only arm 2
	// A value matching NO arm declines (BAML errors / null-defaults).
	requireUnsupported(t, s, `{"u":"7"}`)
}

// TestClassUnion_LenientLeaf_OneSuccess pins that the flat disjoint Book|Car
// class union claims when a numeric-string leaf lets exactly ONE arm succeed
// (Car's wheels="4"→4 is a clean direct string parse; Book misses its fields).
func TestClassUnion_LenientLeaf_OneSuccess(t *testing.T) {
	s := classUnionSchema() // Book{title,pages} | Car{brand,wheels}
	mustParse(t, s, `{"u":{"brand":"Audi","wheels":"4"}}`, `{"u":{"brand":"Audi","wheels":4}}`)
}

// TestClassUnion_TwoLenientSuccesses_Scored pins the M3 scored selection: a
// numeric-string field (wheels="4") makes Car succeed on an input that also
// strictly satisfies Book, so BOTH arms succeed and native runs pick_best. Book
// (2 extras) and Car (2 extras) TIE on score 2, so the lower index (Book, arm 0)
// wins — native emits Book's value directly instead of declining.
func TestClassUnion_TwoLenientSuccesses_Scored(t *testing.T) {
	s := classUnionSchema()
	// title,pages satisfy Book (brand,wheels extras => 2); brand + wheels="4"
	// satisfy Car (title,pages extras => 2) -> TWO successes, tie -> index 0 (Book).
	mustParse(t, s, `{"u":{"title":"Go","pages":300,"brand":"Audi","wheels":"4"}}`, `{"u":{"title":"Go","pages":300}}`)
}

// TestNullableOptionalInt_Scored pins the M3 scored rule for an optional int:
// the non-null arm wins when its score < 110 (the null arm's DefaultButHadValue),
// so a FloatToInt round (score 1) now CLAIMS 2 rather than declining.
func TestNullableOptionalInt_Scored(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "int"}})
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":"123"}`, `{"u":123}`) // clean direct parse (score 0) -> claim
	mustParse(t, s, `{"u":1.6}`, `{"u":2}`)     // FloatToInt score 1 < 110 -> claim 2
}

// TestNullableOptionalBool_Scored pins that an optional bool with a string bool
// (StringToBool, score 1 < 110) now CLAIMS the coerced bool.
func TestNullableOptionalBool_Scored(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "bool"}})
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":true}`, `{"u":true}`)   // clean JSON bool (score 0) -> claim
	mustParse(t, s, `{"u":"true"}`, `{"u":true}`) // StringToBool score 1 < 110 -> claim
}

// TestNullableLiteralIntUnion_Scored pins scored selection inside a nullable
// homogeneous int-literal union: arm 2 via FloatToInt (score 1) beats the null
// arm (110), so the union claims 2.
func TestNullableLiteralIntUnion_Scored(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{
		Type: "union",
		OneOf: []*bamlutils.DynamicTypeSpec{
			{Type: "literal_int", Value: int64(1)},
			{Type: "literal_int", Value: int64(2)},
			{Type: "null"},
		},
	})
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":"2"}`, `{"u":2}`) // clean string→int, arm 2 (score 0) -> claim
	mustParse(t, s, `{"u":1.6}`, `{"u":2}`) // arm 2 via FloatToInt score 1 < 110 -> claim
}
