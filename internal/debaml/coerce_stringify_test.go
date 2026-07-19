package debaml

import (
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
)

// --- ordered-value builders local to the stringify tests (numV/strVv live in
// coerce_numeric_test.go). ---

func boolVal(b bool) value        { return value{kind: valBool, boolV: b} }
func nullVal() value              { return value{kind: valNull} }
func fld(k string, v value) field { return field{key: k, val: v} }
func objVal(fs ...field) value    { return value{kind: valObject, objV: fs} }
func arrVal(vs ...value) value    { return value{kind: valArray, arrV: vs} }

// TestDisplayValue pins displayValue against BAML's jsonish::Value Display impl
// (jsonish/value.rs:195) BYTE-FOR-BYTE. This is NOT JSON: keys and nested
// strings are UNQUOTED, entries use ", " and keys ": ". certain is true whenever
// native can reproduce BAML's serde_json Display — an INTEGER token prints its
// canonical decimal, and a NON-integer spelling is canonicalized via serde's f64
// Display (ryu shortest round-trip), which displayFloat64 reproduces byte-exact
// (Phase 7C, LIVE-CAPTURED): "5e0"/"5.00" -> "5.0", "-0" -> "-0.0", a past-u64
// integer -> its f64 form. All of these are CERTAIN.
func TestDisplayValue(t *testing.T) {
	cases := []struct {
		name        string
		in          value
		want        string
		wantCertain bool
	}{
		{"number-int", numV("5"), "5", true},
		{"number-negative", numV("-3"), "-3", true},
		{"number-zero", numV("0"), "0", true},
		{"number-large-in-i64", numV("9223372036854775807"), "9223372036854775807", true},
		{"number-large-in-u64", numV("18446744073709551615"), "18446744073709551615", true},
		// Non-integer spellings canonicalize via serde f64 Display (ryu), which
		// displayFloat64 reproduces byte-exact — CERTAIN (Phase 7C close).
		{"number-trailing-zero", numV("5.0"), "5.0", true},
		{"number-decimal", numV("3.14"), "3.14", true},
		{"number-exponent-lower", numV("5e0"), "5.0", true},
		{"number-exponent-upper", numV("5E0"), "5.0", true},
		{"number-redundant-decimal", numV("5.00"), "5.0", true},
		{"number-negative-zero", numV("-0"), "-0.0", true},
		{"number-past-u64", numV("18446744073709551616"), "1.8446744073709552e19", true},
		{"bool-true", boolVal(true), "true", true},
		{"bool-false", boolVal(false), "false", true},
		{"null", nullVal(), "null", true},
		{"string-plain", strVv("hi"), "hi", true}, // UNQUOTED
		{"string-empty", strVv(""), "", true},     // UNQUOTED empty
		{"string-with-spaces", strVv("a b"), "a b", true},
		{"empty-object", objVal(), "{}", true},
		{"empty-array", arrVal(), "[]", true},
		{"object-single", objVal(fld("a", numV("1"))), "{a: 1}", true},
		{"object-multi", objVal(fld("a", numV("1")), fld("b", strVv("x"))), "{a: 1, b: x}", true},
		{"array-mixed", arrVal(numV("1"), strVv("x"), boolVal(true)), "[1, x, true]", true},
		{"array-with-null", arrVal(numV("1"), nullVal()), "[1, null]", true},
		{"nested-array-in-object", objVal(fld("a", arrVal(numV("1"), numV("2")))), "{a: [1, 2]}", true},
		{"nested-object-in-array", arrVal(objVal(fld("k", strVv("v")))), "[{k: v}]", true},
		{"deep-nest", objVal(fld("a", arrVal(numV("1"), objVal(fld("b", numV("2")))))), "{a: [1, {b: 2}]}", true},
		// Input key ORDER is preserved (not sorted).
		{"object-order-preserved", objVal(fld("z", numV("1")), fld("a", numV("2"))), "{z: 1, a: 2}", true},
		// A non-integer number nested in the tree canonicalizes via f64 Display and
		// stays CERTAIN (Phase 7C close): 5.0 -> "5.0", 2e0 -> "2.0".
		{"object-nested-noninteger-number", objVal(fld("a", numV("5.0"))), "{a: 5.0}", true},
		{"array-nested-exponent-number", arrVal(numV("1"), numV("2e0")), "[1, 2.0]", true},
	}
	for _, c := range cases {
		got, certain := displayValue(c.in)
		if got != c.want {
			t.Errorf("displayValue(%s): got %q, want %q", c.name, got, c.want)
		}
		if certain != c.wantCertain {
			t.Errorf("displayValue(%s): certain = %v, want %v", c.name, certain, c.wantCertain)
		}
	}
}

// TestCoercePrimitiveString_Direct pins coercePrimitiveString: a JSON string is
// clean (no flag), a NON-null non-string flags JsonToString and emits the
// Display form as a JSON string, and a JSON null DECLINES (never stringified).
func TestCoercePrimitiveString_Direct(t *testing.T) {
	// String passthrough: emitted unchanged, CLEAN (no JsonToString).
	f := &coerceFlags{}
	out, err := coercePrimitiveString(strVv("hi"), f)
	if err != nil {
		t.Fatalf("string passthrough: unexpected error: %v", err)
	}
	if string(out) != `"hi"` {
		t.Errorf("string passthrough: got %s, want \"hi\"", out)
	}
	if f.isFlagged() {
		t.Errorf("string passthrough must be CLEAN (no JsonToString)")
	}

	// Non-string: JsonToString flag + Display-form JSON string.
	for _, c := range []struct {
		name string
		in   value
		want string
	}{
		{"number", numV("5"), `"5"`},
		{"bool", boolVal(true), `"true"`},
		{"object", objVal(fld("a", numV("1")), fld("b", strVv("x"))), `"{a: 1, b: x}"`},
		{"array", arrVal(numV("1"), strVv("x"), boolVal(true)), `"[1, x, true]"`},
	} {
		fl := &coerceFlags{}
		got, err := coercePrimitiveString(c.in, fl)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
		if !fl.isFlagged() {
			t.Errorf("%s: expected JsonToString flag (score-bearing)", c.name)
		}
	}

	// JSON null: DECLINE (BAML error_unexpected_null — null is never stringified).
	fn := &coerceFlags{}
	if _, err := coercePrimitiveString(nullVal(), fn); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("string<-null: expected ErrDeBAMLParseUnsupported, got %v", err)
	}
}

// TestCoercePrimitiveString_NonIntegerNumberClaims pins the Phase 7C number-display
// close: a NON-integer number spelling now stringifies via serde's f64 Display
// (ryu shortest round-trip, displayFloat64) byte-exact, so coercePrimitiveString
// CLAIMS it (JsonToString flag, NOT uncertain) — standalone and nested inside an
// object/array display. An INTEGER still prints its canonical decimal.
func TestCoercePrimitiveString_NonIntegerNumberClaims(t *testing.T) {
	for _, c := range []struct {
		name string
		in   value
		want string
	}{
		{"exponent", numV("5e0"), `"5.0"`},
		{"upper-exponent", numV("5E0"), `"5.0"`},
		{"decimal", numV("5.0"), `"5.0"`},
		{"redundant-decimal", numV("5.00"), `"5.0"`},
		{"negative-zero", numV("-0"), `"-0.0"`},
		{"past-u64", numV("18446744073709551616"), `"1.8446744073709552e19"`},
		{"object-with-noninteger", objVal(fld("a", numV("5.0"))), `"{a: 5.0}"`},
		{"array-with-exponent", arrVal(numV("1"), numV("2e0")), `"[1, 2.0]"`},
		{"integer", numV("5"), `"5"`},
		{"object-integers", objVal(fld("a", numV("1"))), `"{a: 1}"`},
	} {
		f := &coerceFlags{}
		got, err := coercePrimitiveString(c.in, f)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got (%s,%v), want (%s,nil)", c.name, got, err, c.want)
		}
		if f.isUncertain() {
			t.Errorf("%s: must NOT be uncertain (number display now reproduced byte-exact)", c.name)
		}
		if !f.isFlagged() {
			t.Errorf("%s: expected JsonToString flag (score-bearing)", c.name)
		}
	}
}

// TestNumberDisplayParity_EndToEnd pins the Phase 7C number-display close end-to-end:
// a non-integer number spelling now stringifies via serde f64 Display byte-exact, so
// native CLAIMS it — standalone string, inside a list<string>, and inside a map value
// — matching BAML (LIVE-CAPTURED). A string literal "5" still matches the numeric
// input by its match_string substring (5e0 -> "5.0" contains "5" -> "5"), as BAML does.
func TestNumberDisplayParity_EndToEnd(t *testing.T) {
	// Standalone string target: 5e0 -> "5.0" (BAML byte-exact).
	strS := oneField(strProp())
	mustParse(t, strS, `{"u":5e0}`, `{"u":"5.0"}`)
	mustParse(t, strS, `{"u":5}`, `{"u":"5"}`) // integer stays canonical

	// list<string> with a non-integer element -> element stringifies (not a decline).
	listS := oneField(&bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}})
	mustParse(t, listS, `{"u":["a",5.0]}`, `{"u":["a","5.0"]}`)

	// map<string,string> with a non-integer value -> value stringifies.
	mapS := oneField(&bamlutils.DynamicProperty{
		Type:   "map",
		Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
		Values: &bamlutils.DynamicTypeSpec{Type: "string"},
	})
	mustParse(t, mapS, `{"u":{"a":"x","b":5.0}}`, `{"u":{"a":"x","b":"5.0"}}`)

	// string literal "5": a numeric input matches via match_string (integer "5"
	// exact, 5e0 -> "5.0" substring), both -> "5", as BAML does.
	litS := oneField(&bamlutils.DynamicProperty{Type: "literal_string", Value: "5"})
	mustParse(t, litS, `{"u":5e0}`, `{"u":"5"}`)
	mustParse(t, litS, `{"u":5}`, `{"u":"5"}`)
}

// TestStringForMatch pins the shared match_string value→string prelude used by
// coerceEnum/coerceLiteral: string verbatim (clean), non-null non-string via
// Display + ObjectToString flag, JSON null declines.
func TestStringForMatch(t *testing.T) {
	f := &coerceFlags{}
	if s, err := stringForMatch(strVv("Red"), f); err != nil || s != "Red" {
		t.Errorf("string: got (%q,%v), want (\"Red\",nil)", s, err)
	}
	if f.isFlagged() {
		t.Errorf("string input must be CLEAN (no ObjectToString)")
	}

	fo := &coerceFlags{}
	if s, err := stringForMatch(objVal(fld("color", strVv("RED"))), fo); err != nil || s != "{color: RED}" {
		t.Errorf("object: got (%q,%v), want (\"{color: RED}\",nil)", s, err)
	}
	if !fo.isFlagged() {
		t.Errorf("object input must flag ObjectToString (score-bearing)")
	}

	fn := &coerceFlags{}
	if _, err := stringForMatch(nullVal(), fn); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("null: expected ErrDeBAMLParseUnsupported, got %v", err)
	}
}

// TestCoerceLiteral_ObjectToPrimitive pins the single-key-object extraction
// prelude: a single-key object whose inner value is a number/bool/string is
// unwrapped, the literal is matched against the inner value, and ObjectToPrimitive
// flags. An inner object/array/null gets NO extraction (falls through).
func TestCoerceLiteral_ObjectToPrimitive(t *testing.T) {
	litInt := &schema.LiteralValue{Kind: schema.LiteralInt, Int: 1}
	litBool := &schema.LiteralValue{Kind: schema.LiteralBool, Bool: true}
	litStr := &schema.LiteralValue{Kind: schema.LiteralString, String: "active"}

	// {value:"1"} -> literal int 1, ObjectToPrimitive.
	f := &coerceFlags{}
	out, err := coerceLiteral(litInt, objVal(fld("value", strVv("1"))), f)
	if err != nil || string(out) != "1" {
		t.Fatalf("int extraction: got (%s,%v), want (1,nil)", out, err)
	}
	if !f.isFlagged() {
		t.Errorf("int extraction: expected ObjectToPrimitive flag")
	}

	// {k:true} -> literal bool true, ObjectToPrimitive. (Key name is ignored.)
	fb := &coerceFlags{}
	out, err = coerceLiteral(litBool, objVal(fld("k", boolVal(true))), fb)
	if err != nil || string(out) != "true" {
		t.Fatalf("bool extraction: got (%s,%v), want (true,nil)", out, err)
	}
	if !fb.isFlagged() {
		t.Errorf("bool extraction: expected ObjectToPrimitive flag")
	}

	// {tag:"active"} -> literal string "active", ObjectToPrimitive.
	fs := &coerceFlags{}
	out, err = coerceLiteral(litStr, objVal(fld("tag", strVv("active"))), fs)
	if err != nil || string(out) != `"active"` {
		t.Fatalf("string extraction: got (%s,%v), want (\"active\",nil)", out, err)
	}
	if !fs.isFlagged() {
		t.Errorf("string extraction: expected ObjectToPrimitive flag")
	}

	// Inner value MISMATCH propagates the error (BAML does NOT fall back to
	// matching the whole object): {value:"2"} vs literal int 1 declines.
	fm := &coerceFlags{}
	if _, err := coerceLiteral(litInt, objVal(fld("value", strVv("2"))), fm); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("int mismatch: expected ErrDeBAMLParseUnsupported, got %v", err)
	}

	// Inner value is an OBJECT (not number/bool/string): NO extraction, falls
	// through to the int path which declines the object.
	fno := &coerceFlags{}
	if _, err := coerceLiteral(litInt, objVal(fld("value", objVal())), fno); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("nested-object inner: expected decline, got %v", err)
	}

	// A MULTI-key object gets no extraction and declines for int literals.
	fmk := &coerceFlags{}
	if _, err := coerceLiteral(litInt, objVal(fld("a", numV("1")), fld("b", numV("2"))), fmk); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("multi-key object: expected decline, got %v", err)
	}
}

// TestCoerceLiteralString_ObjectToString pins that a non-string string-literal
// input is stringified (ObjectToString) then matched: a MULTI-key object is
// Display-stringified whole (no single-key extraction) and can substring-match.
func TestCoerceLiteralString_ObjectToString(t *testing.T) {
	lit := &schema.LiteralValue{Kind: schema.LiteralString, String: "active"}
	// A number that stringifies to exactly the literal.
	litNum := &schema.LiteralValue{Kind: schema.LiteralString, String: "5"}
	f := &coerceFlags{}
	out, err := coerceLiteral(litNum, numV("5"), f)
	if err != nil || string(out) != `"5"` {
		t.Fatalf("number->literal string: got (%s,%v), want (\"5\",nil)", out, err)
	}
	if !f.isFlagged() {
		t.Errorf("number->literal string: expected ObjectToString flag")
	}
	// A JSON null still declines.
	fn := &coerceFlags{}
	if _, err := coerceLiteral(lit, nullVal(), fn); !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Errorf("literal string <- null: expected decline, got %v", err)
	}
}

// TestProvenListItemError_StringNull pins the ONLY leaf classifier change in
// Mcoerce-d PR 1: a string list element that is a direct NULL is a PROVEN
// error_unexpected_null skip, while a non-null non-string SUCCEEDS at the leaf
// (JsonToString) and so never reaches provenListItemError.
func TestProvenListItemError_StringNull(t *testing.T) {
	b := &schema.Bundle{}
	strElem := schema.Type{Kind: schema.TypePrimitive, Primitive: schema.PrimitiveString}
	// Null: proven skip.
	if !provenListItemError(b, strElem, nullVal()) {
		t.Errorf("string<-null must be a proven parse error (skip)")
	}
	// provenMapValueError delegates to the same whitelist.
	if !provenMapValueError(b, strElem, nullVal()) {
		t.Errorf("map string value <- null must be a proven parse error (skip)")
	}
	// A non-null non-string is NOT classified here (it succeeds at the leaf via
	// JsonToString); the classifier must NOT claim it as a proven error.
	for _, c := range []struct {
		name string
		item value
	}{
		{"number", numV("5")},
		{"bool", boolVal(true)},
		{"object", objVal(fld("a", numV("1")))},
		{"array", arrVal(numV("1"))},
	} {
		if provenListItemError(b, strElem, c.item) {
			t.Errorf("string<-%s must NOT be classified proven (it succeeds via JsonToString)", c.name)
		}
	}
}

// TestParse_EnumObjectToString_EndToEnd pins the enum ObjectToString claim: an
// object input whose Display form substring-matches exactly one enum value
// coerces to that value's canonical name.
func TestParse_EnumObjectToString_EndToEnd(t *testing.T) {
	// enumSchema is Root{ color: Color }, Color{RED,GREEN,BLUE}.
	// {color:{k:"RED"}} -> Display "{k: RED}" -> substring match RED (only).
	mustParse(t, enumSchema(), `{"color":{"k":"RED"}}`, `{"color":"RED"}`)
}

// TestParse_LiteralObjectToPrimitive_EndToEnd pins the single-key-object
// ObjectToPrimitive extraction end-to-end for int / bool / string literals.
func TestParse_LiteralObjectToPrimitive_EndToEnd(t *testing.T) {
	si := oneField(&bamlutils.DynamicProperty{Type: "literal_int", Value: int64(1)})
	mustParse(t, si, `{"u":{"value":"1"}}`, `{"u":1}`)

	sb := oneField(&bamlutils.DynamicProperty{Type: "literal_bool", Value: true})
	mustParse(t, sb, `{"u":{"x":"true"}}`, `{"u":true}`)

	ss := oneField(&bamlutils.DynamicProperty{Type: "literal_string", Value: "active"})
	mustParse(t, ss, `{"u":{"tag":"active"}}`, `{"u":"active"}`)

	// Inner mismatch: {"value":"2"} vs literal int 1 -> BAML errors, native
	// declines (falls back), not a claim of the wrong value.
	requireUnsupported(t, si, `{"u":{"value":"2"}}`)
}

// TestParse_ListStringStringify_EndToEnd pins the list leaf classifier flips:
// non-null non-string elements are stringified + KEPT (JsonToString), and a
// direct null element is SKIPPED (proven error_unexpected_null).
func TestParse_ListStringStringify_EndToEnd(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}})
	mustParse(t, s, `{"u":["a",2]}`, `{"u":["a","2"]}`) // fixture 96
	mustParse(t, s, `{"u":[true,{"a":1},[1,2]]}`, `{"u":["true","{a: 1}","[1, 2]"]}`)
	// null element skipped (proven), the rest kept.
	mustParse(t, s, `{"u":["a",null,"b"]}`, `{"u":["a","b"]}`)
}

// TestParse_MapStringStringify_EndToEnd pins the map value leaf classifier
// flips: a non-null non-string value is stringified + KEPT, and a direct null
// value is SKIPPED (proven MapValueParseError).
func TestParse_MapStringStringify_EndToEnd(t *testing.T) {
	s := oneField(&bamlutils.DynamicProperty{
		Type:   "map",
		Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
		Values: &bamlutils.DynamicTypeSpec{Type: "string"},
	})
	mustCoerce(t, s, `{"u":{"a":"x","b":5}}`, `{"u":{"a":"x","b":"5"}}`) // fixture 107 shape
	// null value skipped (proven), the rest kept.
	mustCoerce(t, s, `{"u":{"a":"x","b":null}}`, `{"u":{"a":"x"}}`)
}
