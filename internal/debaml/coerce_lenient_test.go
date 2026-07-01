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

// TestCoercePrimitiveString_StaysStrict pins the b/d boundary: a non-string
// into a string field is JsonToString (Mcoerce-d), so native declines.
func TestCoercePrimitiveString_StaysStrict(t *testing.T) {
	s := oneField(strProp())
	mustParse(t, s, `{"u":"hi"}`, `{"u":"hi"}`)
	requireUnsupported(t, s, `{"u":5}`)
	requireUnsupported(t, s, `{"u":true}`)
}

// litIntField / litBoolField build a single literal-typed field.
func litIntField(v int64) *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{Type: "literal_int", Value: v})
}
func litBoolField(v bool) *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{Type: "literal_bool", Value: v})
}

// TestCoerceLiteralInt_Lenient pins int-literal coercion by primitive-coerce
// then value-compare, and the conservative fallback on mismatch / object input.
func TestCoerceLiteralInt_Lenient(t *testing.T) {
	s := litIntField(2)
	mustParse(t, s, `{"u":2}`, `{"u":2}`)
	mustParse(t, s, `{"u":"2"}`, `{"u":2}`)         // string→int match
	mustParse(t, s, `{"u":1.6}`, `{"u":2}`)         // round(1.6)=2 == literal
	requireUnsupported(t, s, `{"u":"5"}`)           // coerces to 5 != 2 -> fallback
	requireUnsupported(t, s, `{"u":3}`)             // 3 != 2 -> fallback
	requireUnsupported(t, s, `{"u":{"value":"2"}}`) // single-key object = Mcoerce-d
}

// TestCoerceLiteralBool_Lenient pins bool-literal coercion by primitive-coerce
// then value-compare, and the conservative fallback on mismatch / object input.
func TestCoerceLiteralBool_Lenient(t *testing.T) {
	s := litBoolField(true)
	mustParse(t, s, `{"u":true}`, `{"u":true}`)
	mustParse(t, s, `{"u":"true"}`, `{"u":true}`) // casefold match
	mustParse(t, s, `{"u":"TRUE"}`, `{"u":true}`)
	requireUnsupported(t, s, `{"u":"false"}`)          // coerces to false != true
	requireUnsupported(t, s, `{"u":false}`)            // false != true
	requireUnsupported(t, s, `{"u":{"value":"true"}}`) // single-key object = Mcoerce-d
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

// TestClassUnion_TwoLenientSuccesses_Fallback pins the over-claim guard: a
// numeric-string field (wheels="4") makes the SECOND arm (Car) newly succeed
// on an input that also strictly satisfies Book, so native declines the whole
// union (BAML pick_best = M3) rather than emitting the old strict Book arm.
func TestClassUnion_TwoLenientSuccesses_Fallback(t *testing.T) {
	s := classUnionSchema()
	// title,pages satisfy Book (brand,wheels extras); brand + wheels="4" satisfy
	// Car (title,pages extras) -> TWO lenient successes -> decline. Before
	// Mcoerce-b wheels="4" declined, so only Book succeeded (the over-claim).
	requireUnsupported(t, s, `{"u":{"title":"Go","pages":300,"brand":"Audi","wheels":"4"}}`)
}

// TestNullableOptionalInt_CleanOnly pins the nullable clean-only rule for an
// optional int: a clean direct string parse claims, but a FloatToInt round
// declines (the null arm competes by scoring → M3).
func TestNullableOptionalInt_CleanOnly(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "int"}})
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":"123"}`, `{"u":123}`) // clean direct parse -> claim
	requireUnsupported(t, s, `{"u":1.6}`)       // FloatToInt -> decline
}

// TestNullableOptionalBool_StringDeclines pins that an optional bool with a
// string bool (StringToBool) declines under the clean-only rule.
func TestNullableOptionalBool_StringDeclines(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "bool"}})
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":true}`, `{"u":true}`) // clean JSON bool -> claim
	requireUnsupported(t, s, `{"u":"true"}`)    // StringToBool -> decline
}

// TestNullableLiteralIntUnion_CleanOnly pins the clean-only rule inside a
// nullable homogeneous int-literal union.
func TestNullableLiteralIntUnion_CleanOnly(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{
		Type: "union",
		OneOf: []*bamlutils.DynamicTypeSpec{
			{Type: "literal_int", Value: int64(1)},
			{Type: "literal_int", Value: int64(2)},
			{Type: "null"},
		},
	})
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":"2"}`, `{"u":2}`) // clean string→int, arm 2 -> claim
	requireUnsupported(t, s, `{"u":1.6}`)   // arm 2 via FloatToInt -> decline
}
