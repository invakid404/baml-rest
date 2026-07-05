package debaml

import (
	"encoding/json"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// listIntSchema is a root class with one required list<int> field u.
func listIntSchema() *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{
		Type:  "list",
		Items: &bamlutils.DynamicTypeSpec{Type: "int"},
	})
}

// listStrSchema is a root class with one required list<string> field u.
func listStrSchema() *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{
		Type:  "list",
		Items: &bamlutils.DynamicTypeSpec{Type: "string"},
	})
}

// optionalListIntSchema is a root class with one optional list<int> field u
// (a nullable single-arm union over list<int>).
func optionalListIntSchema() *bamlutils.DynamicOutputSchema {
	return oneField(&bamlutils.DynamicProperty{
		Type:  "optional",
		Inner: &bamlutils.DynamicTypeSpec{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "int"}},
	})
}

// listOfPairSchema is Root{ items: Pair[] }, Pair{ a: string, b: string } — a
// multi-field, all-required-flat-leaf class, so a SCALAR element is a proven
// missing-required-field error.
func listOfPairSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("items", &bamlutils.DynamicProperty{
			Type:  "list",
			Items: &bamlutils.DynamicTypeSpec{Ref: "Pair"},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Pair", &bamlutils.DynamicClass{
				Properties: props(kv("a", strProp()), kv("b", strProp())),
			}),
		),
	}
}

// TestCoerceList_ArrayPartialSkip pins BAML coerce_array's partial-list result:
// a PROVEN-parse-error element is dropped (ArrayItemParseError) while the list
// still succeeds, keeping accepted items in accepted input order.
func TestCoerceList_ArrayPartialSkip(t *testing.T) {
	s := listIntSchema()
	// "1"->1, 2.6->3 (round half-away), "bad" skipped, 4->4.
	mustParse(t, s, `{"u":["1",2.6,"bad",4]}`, `{"u":[1,3,4]}`)
	// All-bad array -> [].
	mustParse(t, s, `{"u":["bad","nope",""]}`, `{"u":[]}`)
	// Proven non-string item kinds BAML rejects with error_unexpected_type are
	// also skipped: null, bool, object.
	mustParse(t, s, `{"u":[1,null,2,true,3,{"x":1}]}`, `{"u":[1,2,3]}`)
}

// TestCoerceList_LenientElementsKept pins that Mcoerce-a/b leaf coercions run
// inside a list and KEEP the item (numeric-string, float->int round, fraction,
// extracted currency).
func TestCoerceList_LenientElementsKept(t *testing.T) {
	s := listIntSchema()
	mustParse(t, s, `{"u":["1",2.6,"3/2","$1,234"]}`, `{"u":[1,3,2,1234]}`)
}

// TestCoerceList_SingletonWrap pins SingleToArray: a non-array input becomes a
// one-element list on inner success, and an EMPTY list (not a parse failure) on
// a proven inner parse error.
func TestCoerceList_SingletonWrap(t *testing.T) {
	s := listIntSchema()
	mustParse(t, s, `{"u":"123"}`, `{"u":[123]}`) // inner success -> [123]
	mustParse(t, s, `{"u":2.6}`, `{"u":[3]}`)     // number singleton rounds
	mustParse(t, s, `{"u":"bad"}`, `{"u":[]}`)    // proven inner parse error -> []
	mustParse(t, s, `{"u":true}`, `{"u":[]}`)     // bool->int proven error -> []
}

// TestCoerceList_StringElementStringified pins the Mcoerce-d PR 1 flip: a
// list<string> non-null non-string element is now stringified (JsonToString) and
// KEPT, so the whole list claims instead of declining. A direct null element is
// a PROVEN skip. (Was TestCoerceList_StringElementDeferredDeclines.)
func TestCoerceList_StringElementStringified(t *testing.T) {
	s := listStrSchema()
	mustParse(t, s, `{"u":["a","b"]}`, `{"u":["a","b"]}`) // clean claim
	// Number element -> JsonToString "2", kept (fixture 96).
	mustParse(t, s, `{"u":["a",2]}`, `{"u":["a","2"]}`)
	// Number singleton -> SingleToArray then JsonToString -> ["5"].
	mustParse(t, s, `{"u":5}`, `{"u":["5"]}`)
	// String singleton coerces cleanly -> claim.
	mustParse(t, s, `{"u":"x"}`, `{"u":["x"]}`)
	// Direct null element is a proven error_unexpected_null skip; the rest kept.
	mustParse(t, s, `{"u":["a",null,"b"]}`, `{"u":["a","b"]}`)
}

// TestCoerceList_ClassScalarSkips pins that a multi-field flat class element
// that is a genuine SCALAR is a proven missing-required-field error (skipped),
// while an object element is kept, and an ARRAY element (coerce_array_to_singular
// = pick_best, M3) makes native decline the whole list.
func TestCoerceList_ClassScalarSkips(t *testing.T) {
	s := listOfPairSchema()
	// Scalar 5 skipped; the object is kept in schema order.
	mustParse(t, s, `{"items":[5,{"a":"x","b":"y"}]}`, `{"items":[{"a":"x","b":"y"}]}`)
	// Scalar-only -> [].
	mustParse(t, s, `{"items":[5]}`, `{"items":[]}`)
	// An ARRAY element defers to coerce_array_to_singular (M3) -> decline whole list.
	requireUnsupported(t, s, `{"items":[["x","y"]]}`)
}

// TestCoerceList_NullableOptionalScored pins the M3 scored selection for lists:
// a list arm wins whenever its inherent score < 110 (the null arm), so a
// SingleToArray wrap (score 1), a kept FloatToInt element (score 1), or an
// ArrayItemParseError skip scored as 1+i (so an index-1 skip scores 2) all now
// CLAIM the list value.
func TestCoerceList_NullableOptionalScored(t *testing.T) {
	s := optionalListIntSchema()
	// Clean array -> clean list arm (score 0) beats null (110) -> claim.
	mustParse(t, s, `{"u":[1,2]}`, `{"u":[1,2]}`)
	// JSON null -> the null fast path claims null.
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	// Singleton -> SingleToArray (score 1 < 110) -> claim [123].
	mustParse(t, s, `{"u":"123"}`, `{"u":[123]}`)
	// A kept FloatToInt element (2.6 -> 3, score 1 < 110) -> claim [3].
	mustParse(t, s, `{"u":[2.6]}`, `{"u":[3]}`)
	// A partial skip (ArrayItemParseError(1) = score 2 < 110) -> claim [1].
	mustParse(t, s, `{"u":[1,"bad"]}`, `{"u":[1]}`)
}

// TestProvenListItemError_Primitives pins the child-error classifier for
// primitive element targets — the load-bearing safe-skip vs must-decline split.
// The bundle is unused for non-class targets, so it is nil here.
func TestProvenListItemError_Primitives(t *testing.T) {
	intT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveInt}
	floatT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveFloat}
	boolT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveBool}
	strT := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	enumT := schema.Type{Kind: schema.TypeEnum, Name: "E"}
	litStrT := schema.Type{Kind: schema.TypeLiteral, Literal: &schema.LiteralValue{Kind: schema.LiteralString, String: "A"}}
	litIntT := schema.Type{Kind: schema.TypeLiteral, Literal: &schema.LiteralValue{Kind: schema.LiteralInt, Int: 1}}

	str := func(s string) value { return value{kind: valString, strV: s} }
	num := func(s string) value { return value{kind: valNumber, numV: json.Number(s)} }
	obj := value{kind: valObject}
	arr := value{kind: valArray}
	null := value{kind: valNull}
	boolV := value{kind: valBool, boolV: true}

	cases := []struct {
		name string
		elem schema.Type
		item value
		want bool
	}{
		// int/float + unparseable string -> proven.
		{"int-bad-string", intT, str("bad"), true},
		{"int-empty-string", intT, str(""), true},
		{"float-bad-string", floatT, str("nope"), true},
		// int/float + string BAML coerces (inf/overflow/fraction/comma) -> NOT proven.
		{"int-inf-string", intT, str("inf"), false},
		{"int-overflow-string", intT, str("1e400"), false},
		{"int-fraction-string", intT, str("3/2"), false},
		{"int-currency-string", intT, str("$1,234"), false},
		{"float-hex-string-proven", floatT, str("0x1p4"), true}, // Rust rejects hex; no number extracted
		// int/float + kinds BAML rejects with error_unexpected_type -> proven.
		{"int-object", intT, obj, true},
		{"int-bool", intT, boolV, true},
		{"int-null", intT, null, true},
		// int + array -> coerce_array_to_singular (M3) -> NOT proven.
		{"int-array", intT, arr, false},
		// int + number -> BAML always coerces -> NOT proven.
		{"int-number", intT, num("5"), false},
		// bool: a string is reached only after a certain match miss -> proven;
		// object/number/null error_unexpected_type -> proven; array M3 -> not.
		{"bool-string", boolT, str("nope"), true},
		{"bool-number", boolT, num("1"), true},
		{"bool-null", boolT, null, true},
		{"bool-array", boolT, arr, false},
		// string target: never proven (JsonToString / clean string success).
		{"string-number", strT, num("5"), false},
		{"string-object", strT, obj, false},
		// enum: only a string is a proven match miss; non-string stringifies.
		{"enum-string", enumT, str("zzz"), true},
		{"enum-object", enumT, obj, false},
		// literal: only a string literal from a string is proven.
		{"litstr-string", litStrT, str("zzz"), true},
		{"litstr-number", litStrT, num("5"), false},
		{"litint-string", litIntT, str("bad"), false}, // int literal defers to Mcoerce-d
	}
	for _, c := range cases {
		if got := provenListItemError(nil, c.elem, c.item); got != c.want {
			t.Errorf("%s: provenListItemError = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestProvenListItemError_Class pins the class branch: a genuine scalar into a
// multi-field flat class is proven, an object/array element is not (object may
// coerce/defer, array is coerce_array_to_singular = M3).
func TestProvenListItemError_Class(t *testing.T) {
	b, err := schema.FromDynamicOutputSchema(listOfPairSchema(), schema.BuildOptions{})
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	var pair schema.Type
	for i := range b.Classes {
		if len(b.Classes[i].Fields) == 2 { // Pair (Root's "items" field is len 1)
			pair = schema.Type{Kind: schema.TypeClass, Name: b.Classes[i].Name.Name, Mode: b.Classes[i].Mode}
		}
	}
	if pair.Name == "" {
		t.Fatal("Pair class not found in bundle")
	}
	cases := []struct {
		name string
		item value
		want bool
	}{
		{"scalar-number", value{kind: valNumber, numV: json.Number("5")}, true},
		{"scalar-string", value{kind: valString, strV: "x"}, true},
		{"scalar-bool", value{kind: valBool, boolV: true}, true},
		{"scalar-null", value{kind: valNull}, true},
		{"object", value{kind: valObject}, false}, // object coerces/defers, not proven here
		{"array", value{kind: valArray}, false},   // coerce_array_to_singular (M3)
	}
	for _, c := range cases {
		if got := provenListItemError(b, pair, c.item); got != c.want {
			t.Errorf("%s: provenListItemError(class) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBamlStringNumberFails pins the Rust-faithful numeric-string re-check: it is
// TRUE only when BAML's coerce_int/coerce_float string path would DEFINITELY
// error (no i64/u64/f64 incl. inf/nan/overflow, no fraction, no single extracted
// number), so a bad numeric string can be skipped without over-claiming the
// inf/overflow/hex cases BAML coerces.
func TestBamlStringNumberFails(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Genuine failures.
		{"bad", true},
		{"", true},
		{"   ", true},
		{"abc def", true},
		{"0x1p4", true},   // hex: Rust rejects f64; regex splits into 3 numbers
		{"1_000", true},   // underscore: Rust rejects f64; regex splits into 2 numbers
		{"3/0", true},     // fraction denom 0; regex 2 numbers
		{"1 and 2", true}, // 2 extracted numbers
		// BAML coerces these -> NOT a failure.
		{"123", false},
		{" 123, ", false}, // trim + trailing-comma trim
		{"inf", false},    // Rust f64 inf
		{"nan", false},    // Rust f64 nan
		{"1e400", false},  // overflow -> Rust Ok(inf)
		{"3/2", false},    // fraction
		{"$1,234", false}, // extracted currency
		{"50%", false},    // single extracted number
		{"The answer is 10,000", false},
		{"12,111,123,", false},
	}
	for _, c := range cases {
		if got := bamlStringNumberFails(c.in); got != c.want {
			t.Errorf("bamlStringNumberFails(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
