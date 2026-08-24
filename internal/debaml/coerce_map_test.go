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
	mustCoerceExact(t, s, `{"scores":{"a":1,"bad":"nope","c":"7"}}`, `{"scores":{"a":1,"c":7}}`)
	// All-bad values -> {}.
	mustCoerceExact(t, s, `{"scores":{"a":"x","b":"y"}}`, `{"scores":{}}`)
	// Proven non-string value kinds BAML rejects with error_unexpected_type are
	// also skipped: object, bool, null (a number is always coerced, kept).
	mustCoerceExact(t, s, `{"scores":{"a":1,"b":{"x":1},"c":true,"d":null,"e":2}}`, `{"scores":{"a":1,"e":2}}`)
}

// TestCoerceMap_LenientValuesKept pins that Mcoerce-a/b leaf coercions run
// inside a map VALUE and KEEP the entry (numeric-string, float->int round,
// fraction, extracted currency), in input key order.
func TestCoerceMap_LenientValuesKept(t *testing.T) {
	s := mapStringIntSchema()
	mustCoerceExact(t, s, `{"scores":{"a":"1","b":2.6,"c":"3/2","d":"$1,234"}}`, `{"scores":{"a":1,"b":3,"c":2,"d":1234}}`)
}

// TestCoerceMap_ClassValueScalarSkips pins that a multi-field flat class VALUE
// that is a genuine SCALAR is a proven missing-required-field error (skipped),
// while an object value is kept in schema order; an object MISSING a required
// field is a DEFERRED case (BAML may default/error) and declines the whole map.
func TestCoerceMap_ClassValueScalarSkips(t *testing.T) {
	s := mapStrPairSchema()
	// Scalar 5 skipped; the object value kept, its fields in SCHEMA order.
	mustCoerceExact(t, s, `{"items":{"k":5,"p":{"b":"y","a":"x"}}}`, `{"items":{"p":{"a":"x","b":"y"}}}`)
	// Scalar-only -> {}.
	mustCoerceExact(t, s, `{"items":{"k":5}}`, `{"items":{}}`)
	// An object value missing a required field is NOT a proven skip (BAML may
	// default-fill) -> decline the WHOLE map.
	requireCoerceUnsupported(t, s, `{"items":{"a":{"a":"x","b":"y"},"b":{"a":"x"}}}`)
}

// TestCoerceMap_KeyMissIsKeptNotSkipped pins the dynamic bridge's LENIENT
// string-literal-union map keys: a key matching NO arm is neither dropped nor
// canonicalized — it is inserted under its ORIGINAL string, in input order,
// alongside the matching entries (corpus fixtures 103/104, live-captured FULL
// maps). A fully-matching map, including a fuzzy case-variant, is unchanged.
func TestCoerceMap_KeyMissIsKeptNotSkipped(t *testing.T) {
	s := mapLitUnionStrSchema()
	// All keys match -> claim, keeping ORIGINAL strings in INPUT order.
	mustCoerceExact(t, s, `{"u":{"B":"y","A":"x"}}`, `{"u":{"B":"y","A":"x"}}`)
	// A fuzzy (case-variant) key still matches via match_string and is KEPT under
	// its ORIGINAL string, not the canonical literal.
	mustCoerceExact(t, s, `{"u":{"a":"x"}}`, `{"u":{"a":"x"}}`)
	// A key matching no arm is KEPT, between the two that do, in INPUT order
	// (fixture 104's shape: the matching keys are also out of lexical order).
	mustCoerceExact(t, s, `{"u":{"B":"y","C":"z","A":"x"}}`, `{"u":{"B":"y","C":"z","A":"x"}}`)
}

// TestCoerceMap_EnumKeyMissIsKeptNotSkipped pins the same leniency for an ENUM
// key: a non-member is KEPT under its original string rather than skipped
// (corpus fixtures 28/105 — map_bad_enum_key / the non-member live probe — are
// live-captured FULL maps).
func TestCoerceMap_EnumKeyMissIsKeptNotSkipped(t *testing.T) {
	s := mapEnumKeySchema()
	mustCoerceExact(t, s, `{"labels":{"A":"one","B":"two"}}`, `{"labels":{"A":"one","B":"two"}}`)
	mustCoerceExact(t, s, `{"labels":{"A":"one","C":"two"}}`, `{"labels":{"A":"one","C":"two"}}`)
}

// TestCoerceMap_KeptKeyMissCannotBeRanked is the BITING side of the leniency: the
// entry's BYTES are proven but BAML's MapKeyParseError weight is not, so a map
// carrying a kept non-matching key must never be RANKED against another candidate.
// An OPTIONAL map field scores its arm against the null arm, so it declines —
// while the same map in a plain (unranked) position above claims.
func TestCoerceMap_KeptKeyMissCannotBeRanked(t *testing.T) {
	optionalMap := oneField(optProp(&bamlutils.DynamicTypeSpec{
		Type: "map",
		Keys: &bamlutils.DynamicTypeSpec{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "literal_string", Value: "A"},
				{Type: "literal_string", Value: "B"},
			},
		},
		Values: &bamlutils.DynamicTypeSpec{Type: "string"},
	}))
	// Control: every key matches, the arm is rankable, the map claims.
	mustCoerceExact(t, optionalMap, `{"u":{"A":"x"}}`, `{"u":{"A":"x"}}`)
	// A kept non-matching key makes the arm unrankable -> decline the whole parse.
	requireCoerceUnsupported(t, optionalMap, `{"u":{"A":"x","C":"z"}}`)
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
	mustCoerceExact(t, s, `{"u":{"É":"zzz"}}`, `{"u":{}}`)
}

// TestCoerceMap_NonMatchingKeyAndLenientValueBothKept composes the two leniencies
// this map surface models, on the same entry: map<"A"|"B", string> with
// {"C": 5}. The VALUE is coerced first and stringifies to "5" (JsonToString,
// fixture 107's rule), then the KEY misses every arm and is kept under its
// original string (fixtures 103/104's rule). Neither is a skip, so the entry
// survives whole. The composition is not itself a corpus fixture; the deployed
// route's byte comparison is what holds it to BAML.
func TestCoerceMap_NonMatchingKeyAndLenientValueBothKept(t *testing.T) {
	mustCoerceExact(t, mapLitUnionStrSchema(), `{"u":{"C":5}}`, `{"u":{"C":"5"}}`)
}

// TestCoerceMap_StringStringNonStringValueStringified pins the Mcoerce-d PR 1
// flip: a map<string,string> non-null non-string VALUE is now stringified
// (JsonToString) and KEPT, so the map claims a partial-free result. A direct
// null value is a PROVEN MapValueParseError skip. (Was
// TestCoerceMap_StringStringNonStringValueDeclines.)
func TestCoerceMap_StringStringNonStringValueStringified(t *testing.T) {
	s := mapStrStrSchema()
	mustCoerceExact(t, s, `{"u":{"a":"x","b":"y"}}`, `{"u":{"a":"x","b":"y"}}`) // clean claim
	// Number value -> JsonToString "5", kept (fixture 107 shape).
	mustCoerceExact(t, s, `{"u":{"a":"x","b":5}}`, `{"u":{"a":"x","b":"5"}}`)
	mustCoerceExact(t, s, `{"u":{"a":true}}`, `{"u":{"a":"true"}}`)
	// Direct null value is a proven skip; the rest is kept.
	mustCoerceExact(t, s, `{"u":{"a":"x","b":null}}`, `{"u":{"a":"x"}}`)
}

// TestCoerceMap_DuplicateKeyDeclines pins that ANY duplicate original input key
// declines the whole map — even when the duplicate entry would itself SKIP (a
// skipped duplicate must not be reasoned to leave the rest safe).
func TestCoerceMap_DuplicateKeyDeclines(t *testing.T) {
	s := mapStringIntSchema()
	requireCoerceUnsupported(t, s, `{"scores":{"a":1,"a":2}}`)
	// Duplicate whose second entry has a (proven-bad) value still declines.
	requireCoerceUnsupported(t, s, `{"scores":{"a":1,"a":"bad"}}`)
}

// TestCoerceMap_NonObjectFieldDefaults pins the Mcoerce-d PR 2 flip: a REQUIRED
// map class field with a NON-object value is a PROVEN coerce_map
// error_unexpected_type, so BAML fills the map default {} with
// DefaultButHadUnparseableValue (score 2). Native now reproduces the default and
// CLAIMS {"scores":{}}. (A standalone/list/nullable map with a non-object still
// declines — only the class-field-default path is claimed here.)
func TestCoerceMap_NonObjectFieldDefaults(t *testing.T) {
	s := mapStringIntSchema()
	mustCoerce(t, s, `{"scores":[1,2,3]}`, `{"scores":{}}`)
	mustCoerce(t, s, `{"scores":"x"}`, `{"scores":{}}`)
	mustCoerce(t, s, `{"scores":5}`, `{"scores":{}}`)
}

// TestCoerceMap_NullableScored pins the M3 scored selection for maps: an
// object-input map arm ALWAYS carries ObjectToMap (score 1), which is < 110, so
// a nullable optional map now CLAIMS the (fully-accepted) map — while a JSON null
// still takes the null fast path.
func TestCoerceMap_NullableScored(t *testing.T) {
	s := optionalMapStrIntSchema()
	// JSON null -> the null fast path claims null.
	mustCoerce(t, s, `{"u":null}`, `{"u":null}`)
	// Object input -> ObjectToMap (score 1 < 110) -> claim the map, clean values.
	mustCoerce(t, s, `{"u":{"a":1}}`, `{"u":{"a":1}}`)
	// Empty object -> ObjectToMap (score 1 < 110) -> claim {}.
	mustCoerce(t, s, `{"u":{}}`, `{"u":{}}`)
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

// TestCoerceMap_CrossArmLiteralKeyTieDeclinesTheWholeMap is the map-level half of
// the literal-union key ambiguity boundary. A key that substring-matches TWO arms is
// BAML's StrMatchOneFromMany, which native has no live capture for on a map key — so
// it must decline the whole map rather than silently resolve the tie to whichever arm
// it happened to scan first. The control alongside it is a key that matches exactly
// one arm, which still claims: the decline is about the TIE, not about substrings.
func TestCoerceMap_CrossArmLiteralKeyTieDeclinesTheWholeMap(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{
		Type: "map",
		Keys: &bamlutils.DynamicTypeSpec{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "literal_string", Value: "cat"},
				{Type: "literal_string", Value: "dog"},
			},
		},
		Values: &bamlutils.DynamicTypeSpec{Type: "string"},
	})
	// Control: a substring hit on exactly ONE arm is a clean match and claims, with
	// the ORIGINAL key preserved.
	mustCoerceExact(t, s, `{"u":{"a cat":"x"}}`, `{"u":{"a cat":"x"}}`)
	// The tie: "cat and dog" substring-matches BOTH arms.
	requireCoerceUnsupported(t, s, `{"u":{"cat and dog":"x"}}`)
}

// TestTryCastMap_NonStringKeyIsNotMatched is the boundary that keeps the lenient
// map-key keep from ever being RANKED through the strict phase.
//
// try_cast_map does not look at keys, which is faithful to BAML and harmless for a
// string key. For an enum / string-literal key it would not be: coerceMapKey KEEPS a
// non-matching key and marks the map's score unproven so a scored position declines,
// and a candidate built by tryCastMap carries no coerceFlags at all — so if the
// strict phase could produce one for a non-string-keyed map, pickBest would rank it
// on a score native cannot prove. checkUnionMapVariant already refuses a non-string
// map key at the union gate (the case below therefore never reaches coercion in
// practice), but the gate lives in a different file from the code that would be
// wrong, so tryCastMap refuses it directly too.
func TestTryCastMap_NonStringKeyIsNotMatched(t *testing.T) {
	b, err := schema.FromDynamicOutputSchema(mapEnumKeySchema(), schema.BuildOptions{})
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	enumKey := schema.Type{Kind: schema.TypeEnum, Name: "Key"}
	strVal := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	// An input every VALUE of which try_casts cleanly, and whose keys include a
	// NON-MEMBER: the only thing standing between this and a rankable candidate is
	// the key-type refusal.
	in := objVal(fld("A", strVv("one")), fld("C", strVv("two")))

	if _, matched, err := tryCastMap(b, &enumKey, &strVal, in, &coerceCtx{}); err != nil {
		t.Fatalf("tryCastMap: %v", err)
	} else if matched {
		t.Error("tryCastMap matched an ENUM-keyed map; a non-string key must fall to the lenient pass, where the scoreUnknown decline lives")
	}
	// Control: the SAME entries under a STRING key do try_cast, so the refusal above
	// is about the key TYPE and not about the input.
	if _, matched, err := tryCastMap(b, &strVal, &strVal, in, &coerceCtx{}); err != nil {
		t.Fatalf("tryCastMap (string key): %v", err)
	} else if !matched {
		t.Error("tryCastMap declined a string-keyed map whose every value try_casts")
	}
}

// TestUnionMapArm_NonStringKeyDeclines is the end-to-end half: a multi-arm union
// carrying an ENUM-keyed map arm, fed an object with a retained non-member key, must
// DECLINE the whole parse rather than claim the kept map through a union position.
// Which layer refuses it (the gate today) is deliberately not asserted — the
// observable contract is that native does not claim.
func TestUnionMapArm_NonStringKeyDeclines(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "map", Keys: &bamlutils.DynamicTypeSpec{Ref: "Key"}, Values: &bamlutils.DynamicTypeSpec{Type: "string"}},
				{Type: "string"},
			},
		})),
		Enums: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Key", &bamlutils.DynamicEnum{
				Values: []*bamlutils.DynamicEnumValue{{Name: "A"}, {Name: "B"}},
			}),
		),
	}
	requireCoerceUnsupported(t, s, `{"u":{"A":"one","C":"two"}}`)
	// And with every key a member, so the decline above is the ARM SHAPE and not the
	// retained key: an enum-keyed map arm is outside the proven union family either way.
	requireCoerceUnsupported(t, s, `{"u":{"A":"one","B":"two"}}`)
}
