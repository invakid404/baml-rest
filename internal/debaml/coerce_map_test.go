package debaml

import (
	"encoding/json"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// mapStrStrSchema is Root{ m: map<string,string> }.
func mapStrStrSchema() *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{
		Type:   "map",
		Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
		Values: &bamlutils.DynamicTypeSpec{Type: "string"},
	})
}

// optionalMapStrIntSchema is Root{ u: (map<string,int>)? } — an optional map,
// i.e. a nullable single-arm union over map<string,int>.
func optionalMapStrIntSchema() *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{
		Type: "optional",
		Inner: &bamlutils.DynamicTypeSpec{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{Type: "int"},
		},
	})
}

// mapLitUnionStrSchema is Root{ m: map<"A"|"B", string> } — a string-literal
// union key with string values.
func mapLitUnionStrSchema() *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{
		Type: "map",
		Keys: &bamlutils.DynamicTypeSpec{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "literal_string", Value: "A"},
				{Type: "literal_string", Value: "B"},
			},
		},
		Values: &bamlutils.DynamicTypeSpec{Type: "string"},
	})
}

// mapStrPairSchema is Root{ items: map<string, Pair> }, Pair{ a: string,
// b: string } — a multi-field, all-required-flat-leaf value class, so a SCALAR
// value is a proven missing-required-field error (a skippable entry).
func mapStrPairSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("items", &bamlutils.DynamicProperty{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{Ref: "Pair"},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Pair", &bamlutils.DynamicClass{
				Properties: props(kv("a", strProp()), kv("b", strProp())),
			}),
		),
	}
}

// TestCoerceMap_ValuePartialSkip pins BAML coerce_map's partial-map result: a
// PROVEN-parse-error VALUE is dropped (MapValueParseError) while the map still
// succeeds, keeping accepted entries in accepted INPUT key order.
func TestCoerceMap_ValuePartialSkip(t *testing.T) {
	s := mapStringIntSchema()
	// "a":1 kept, "b":"bad" skipped (proven int parse error), "c":"7" kept (7).
	mustParseExact(t, s, `{"scores":{"a":1,"bad":"nope","c":"7"}}`, `{"scores":{"a":1,"c":7}}`)
	// All-bad values -> {}.
	mustParseExact(t, s, `{"scores":{"a":"x","b":"y"}}`, `{"scores":{}}`)
	// Proven non-string value kinds BAML rejects with error_unexpected_type are
	// also skipped: object, bool, null (a number is always coerced, kept).
	mustParseExact(t, s, `{"scores":{"a":1,"b":{"x":1},"c":true,"d":null,"e":2}}`, `{"scores":{"a":1,"e":2}}`)
}

// TestCoerceMap_LenientValuesKept pins that Mcoerce-a/b leaf coercions run
// inside a map VALUE and KEEP the entry (numeric-string, float->int round,
// fraction, extracted currency), in input key order.
func TestCoerceMap_LenientValuesKept(t *testing.T) {
	s := mapStringIntSchema()
	mustParseExact(t, s, `{"scores":{"a":"1","b":2.6,"c":"3/2","d":"$1,234"}}`, `{"scores":{"a":1,"b":3,"c":2,"d":1234}}`)
}

// TestCoerceMap_ClassValueScalarSkips pins that a multi-field flat class VALUE
// that is a genuine SCALAR is a proven missing-required-field error (skipped),
// while an object value is kept in schema order; an object MISSING a required
// field is a DEFERRED case (BAML may default/error) and declines the whole map.
func TestCoerceMap_ClassValueScalarSkips(t *testing.T) {
	s := mapStrPairSchema()
	// Scalar 5 skipped; the object value kept, its fields in SCHEMA order.
	mustParseExact(t, s, `{"items":{"k":5,"p":{"b":"y","a":"x"}}}`, `{"items":{"p":{"a":"x","b":"y"}}}`)
	// Scalar-only -> {}.
	mustParseExact(t, s, `{"items":{"k":5}}`, `{"items":{}}`)
	// An object value missing a required field is NOT a proven skip (BAML may
	// default-fill) -> decline the WHOLE map.
	requireUnsupported(t, s, `{"items":{"a":{"a":"x","b":"y"},"b":{"a":"x"}}}`)
}

// TestCoerceMap_KeyMissDeclines pins that a KEY matching NO string-literal-union
// arm is NOT a native partial skip: the dynamic bridge keeps non-matching keys
// leniently (a live-captured FULL map), so native declines the WHOLE map rather
// than skip an entry BAML would keep. A fully-matching map (incl. a fuzzy
// case-variant kept under its ORIGINAL string) still claims.
func TestCoerceMap_KeyMissDeclines(t *testing.T) {
	s := mapLitUnionStrSchema()
	// All keys match -> claim, keeping ORIGINAL strings in INPUT order.
	mustParseExact(t, s, `{"u":{"B":"y","A":"x"}}`, `{"u":{"B":"y","A":"x"}}`)
	// A fuzzy (case-variant) key still matches via match_string and is KEPT under
	// its ORIGINAL string, not the canonical literal.
	mustParseExact(t, s, `{"u":{"a":"x"}}`, `{"u":{"a":"x"}}`)
	// A key matching no arm -> decline the whole map (dynamic keeps it -> Mcoerce-d).
	requireUnsupported(t, s, `{"u":{"B":"y","C":"z","A":"x"}}`)
}

// TestCoerceMap_EnumKeyMissDeclines pins that a MISSED enum key is NOT a partial
// skip: the dynamic bridge keeps non-member enum keys (map_bad_enum_key -> full
// map), so native declines the whole map rather than skip an entry BAML keeps.
func TestCoerceMap_EnumKeyMissDeclines(t *testing.T) {
	s := mapEnumKeySchema()
	// All keys match -> claim; a missed key -> decline (not a partial skip).
	mustParseExact(t, s, `{"labels":{"A":"one","B":"two"}}`, `{"labels":{"A":"one","B":"two"}}`)
	requireUnsupported(t, s, `{"labels":{"A":"one","C":"two"}}`)
}

// TestCoerceMap_ValueThenKeyOrder pins BAML's VALUE-first order: an entry with a
// PROVEN-bad VALUE is SKIPPED without ever coercing the KEY, so a would-be
// case-fold-UNCERTAIN key (which, if evaluated, would decline the whole map)
// never fires — the entry is simply dropped and the map succeeds.
func TestCoerceMap_ValueThenKeyOrder(t *testing.T) {
	// map<"é"|"x", int>: key "É" vs literal "é" is case-fold UNCERTAIN (would
	// decline the map if the key were coerced), but the value "zzz" is a PROVEN
	// int parse error coerced FIRST, so the entry skips and the map is {}.
	s := oneField(&bamlutils.DynamicProperty{
		Type: "map",
		Keys: &bamlutils.DynamicTypeSpec{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "literal_string", Value: "é"},
				{Type: "literal_string", Value: "x"},
			},
		},
		Values: &bamlutils.DynamicTypeSpec{Type: "int"},
	})
	mustParseExact(t, s, `{"u":{"É":"zzz"}}`, `{"u":{}}`)
}

// TestCoerceMap_DeferredValuePreemptsKeySkip pins the other side of value-first:
// a value that is a DEFERRED Mcoerce-d success (JsonToString) declines the WHOLE
// map even when the key would otherwise be a proven skip — native must not emit
// a partial map that drops an entry BAML would keep via a lenient value path.
func TestCoerceMap_DeferredValuePreemptsKeySkip(t *testing.T) {
	// map<"A"|"B", string> with {"C": 5}: key "C" would be a proven MapKeyParseError
	// skip, but the value 5 -> string is JsonToString (Mcoerce-d, NOT proven), so
	// the whole map declines rather than skipping the entry.
	requireUnsupported(t, mapLitUnionStrSchema(), `{"u":{"C":5}}`)
}

// TestCoerceMap_StringStringNonStringValueDeclines pins the must-decline value
// path: map<string,string> with a NUMBER value is BAML JsonToString (Mcoerce-d),
// NOT a proven parse error, so native declines the whole map (never skips or
// stringifies).
func TestCoerceMap_StringStringNonStringValueDeclines(t *testing.T) {
	s := mapStrStrSchema()
	mustParseExact(t, s, `{"u":{"a":"x","b":"y"}}`, `{"u":{"a":"x","b":"y"}}`) // clean claim
	requireUnsupported(t, s, `{"u":{"a":"x","b":5}}`)
	requireUnsupported(t, s, `{"u":{"a":true}}`)
}

// TestCoerceMap_DuplicateKeyDeclines pins that ANY duplicate original input key
// declines the whole map — even when the duplicate entry would itself SKIP (a
// skipped duplicate must not be reasoned to leave the rest safe).
func TestCoerceMap_DuplicateKeyDeclines(t *testing.T) {
	s := mapStringIntSchema()
	requireUnsupported(t, s, `{"scores":{"a":1,"a":2}}`)
	// Duplicate whose second entry has a (proven-bad) value still declines.
	requireUnsupported(t, s, `{"scores":{"a":1,"a":"bad"}}`)
}

// TestCoerceMap_NonObjectDeclines pins the conservative non-object decline
// (unchanged): a non-object where a map is required is not a hard type error, so
// native falls back rather than claiming a mismatch BAML would not produce.
func TestCoerceMap_NonObjectDeclines(t *testing.T) {
	s := mapStringIntSchema()
	requireUnsupported(t, s, `{"scores":[1,2,3]}`)
	requireUnsupported(t, s, `{"scores":"x"}`)
	requireUnsupported(t, s, `{"scores":5}`)
}

// TestCoerceMap_NullableCleanOnlyDeclines pins the union revisit for maps: an
// object-input map arm ALWAYS carries ObjectToMap (score-bearing), so a nullable
// optional map declines against the scored null arm — while a JSON null claims
// the null fast path.
func TestCoerceMap_NullableCleanOnlyDeclines(t *testing.T) {
	s := optionalMapStrIntSchema()
	// JSON null -> the null fast path claims null.
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	// Object input -> ObjectToMap (score-bearing) -> decline (M3 scoring vs null),
	// even for a clean, fully-accepted map.
	requireUnsupported(t, s, `{"u":{"a":1}}`)
	requireUnsupported(t, s, `{"u":{}}`)
}

// TestProvenMapValueError pins the map-VALUE child classifier delegates to the
// shared provenListItemError whitelist (map values coerce exactly like list
// items): a proven int parse error / error_unexpected_type kind skips, while a
// deferred (string-target / object-class / array / number) value does not.
func TestProvenMapValueError(t *testing.T) {
	intT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
	strT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	str := func(s string) value { return value{kind: valString, strV: s} }
	num := func(s string) value { return value{kind: valNumber, numV: json.Number(s)} }

	cases := []struct {
		name string
		valT schema.Type
		val  value
		want bool
	}{
		{"int-bad-string", intT, str("bad"), true},
		{"int-object", intT, value{kind: valObject}, true},
		{"int-bool", intT, value{kind: valBool, boolV: true}, true},
		{"int-null", intT, value{kind: valNull}, true},
		{"int-number", intT, num("5"), false},            // BAML always coerces a number
		{"int-fraction-string", intT, str("3/2"), false}, // BAML coerces
		{"string-number", strT, num("5"), false},         // JsonToString (Mcoerce-d)
		{"string-object", strT, value{kind: valObject}, false},
	}
	for _, c := range cases {
		if got := provenMapValueError(nil, c.valT, c.val); got != c.want {
			t.Errorf("%s: provenMapValueError = %v, want %v", c.name, got, c.want)
		}
	}
}
