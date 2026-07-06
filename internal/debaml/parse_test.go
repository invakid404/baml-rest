package debaml

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// prop is a terse constructor for a DynamicProperty.
func strProp() *bamlutils.DynamicProperty  { return &bamlutils.DynamicProperty{Type: "string"} }
func intProp() *bamlutils.DynamicProperty  { return &bamlutils.DynamicProperty{Type: "int"} }
func boolProp() *bamlutils.DynamicProperty { return &bamlutils.DynamicProperty{Type: "bool"} }

func props(kv ...bamlutils.OrderedEntry[*bamlutils.DynamicProperty]) bamlutils.OrderedMap[*bamlutils.DynamicProperty] {
	return bamlutils.MustOrderedMap(kv...)
}

func kv(name string, p *bamlutils.DynamicProperty) bamlutils.OrderedEntry[*bamlutils.DynamicProperty] {
	return bamlutils.OrderedKV(name, p)
}

// parse drives the public callback for a final (non-stream) request.
func parse(t *testing.T, s *bamlutils.DynamicOutputSchema, raw string) (string, error) {
	t.Helper()
	res, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: raw, OutputSchema: s})
	if err != nil {
		return "", err
	}
	return string(res.JSON), nil
}

// mustParse asserts the parser succeeds and its output is SEMANTICALLY
// equal to want (decode both, compare values) — tolerant of harmless
// marshaling/key-order differences. Use mustParseExact when emitted byte
// order is the property under test.
func mustParse(t *testing.T, s *bamlutils.DynamicOutputSchema, raw, want string) {
	t.Helper()
	got, err := parse(t, s, raw)
	if err != nil {
		t.Fatalf("Parse(%q) unexpected error: %v", raw, err)
	}
	if !jsonValueEqual(t, got, want) {
		t.Errorf("Parse(%q):\n got %s\nwant %s", raw, got, want)
	}
}

// mustParseExact asserts the parser succeeds and its output matches want
// BYTE-FOR-BYTE. Reserved for tests that specifically assert emitted field
// order (the schema-order contract), where semantic equality would mask a
// reordering regression.
func mustParseExact(t *testing.T, s *bamlutils.DynamicOutputSchema, raw, want string) {
	t.Helper()
	got, err := parse(t, s, raw)
	if err != nil {
		t.Fatalf("Parse(%q) unexpected error: %v", raw, err)
	}
	if got != want {
		t.Errorf("Parse(%q): byte-exact mismatch\n got %s\nwant %s", raw, got, want)
	}
}

// jsonValueEqual reports whether a and b decode to structurally equal JSON
// values (object key order ignored; numbers compared as float64).
func jsonValueEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		t.Fatalf("decode got %q: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		t.Fatalf("decode want %q: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}

// requireUnsupported asserts the parser fell back (sentinel), not claimed.
func requireUnsupported(t *testing.T, s *bamlutils.DynamicOutputSchema, raw string) {
	t.Helper()
	_, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: raw, OutputSchema: s})
	if err == nil {
		t.Fatalf("Parse(%q): expected ErrDeBAMLParseUnsupported, got success", raw)
	}
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("Parse(%q): expected ErrDeBAMLParseUnsupported, got %v", raw, err)
	}
}

// requireClaimedError asserts a CLAIMED parse error (not the sentinel).
func requireClaimedError(t *testing.T, s *bamlutils.DynamicOutputSchema, raw string) {
	t.Helper()
	_, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: raw, OutputSchema: s})
	if err == nil {
		t.Fatalf("Parse(%q): expected a claimed parse error, got success", raw)
	}
	if errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("Parse(%q): expected a claimed parse error, got ErrDeBAMLParseUnsupported: %v", raw, err)
	}
}

func personSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("name", strProp()), kv("age", intProp())),
	}
}

func TestParse_StrictWholeInput(t *testing.T) {
	mustParse(t, personSchema(), `{"name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
}

func TestParse_MarkdownFence(t *testing.T) {
	mustParse(t, personSchema(), "```json\n{\"name\":\"Ada\",\"age\":36}\n```", `{"name":"Ada","age":36}`)
	// Bare fence with no info string.
	mustParse(t, personSchema(), "```\n{\"name\":\"Ada\",\"age\":36}\n```", `{"name":"Ada","age":36}`)
}

func TestParse_ProseExtraction(t *testing.T) {
	raw := "Sure! Here is the person:\n{\"name\":\"Ada\",\"age\":36}\nLet me know if you need more."
	mustParse(t, personSchema(), raw, `{"name":"Ada","age":36}`)
}

func TestParse_FieldOrderFollowsSchema(t *testing.T) {
	// Input order differs from schema order; output follows schema order.
	// Byte-exact on purpose: this is the schema-order emission contract.
	mustParseExact(t, personSchema(), `{"age":36,"name":"Ada"}`, `{"name":"Ada","age":36}`)
}

func TestParse_ProseExtractionSkipsQuotedBrace(t *testing.T) {
	// A quoted brace appears in prose before the real JSON object. The
	// balanced-span scanner must anchor on the real object (outside quotes),
	// not the quoted "{}", which would otherwise strict-parse to {} and then
	// fail coercion with a missing-required-field error — a parity break vs
	// BAML, which finds the real answer.
	raw := `The literal "{}" appears before the answer. {"name":"Ada","age":36}`
	mustParse(t, personSchema(), raw, `{"name":"Ada","age":36}`)
	// Also when the quoted brace is an opening one with no close.
	raw2 := `Use "{ to denote ... " then: {"name":"Ada","age":36}`
	mustParse(t, personSchema(), raw2, `{"name":"Ada","age":36}`)
}

func TestParse_PrimitivesAndBool(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("s", strProp()),
			kv("i", intProp()),
			kv("f", &bamlutils.DynamicProperty{Type: "float"}),
			kv("b", boolProp()),
		),
	}
	mustParse(t, s, `{"s":"x","i":7,"f":1.5,"b":true}`, `{"s":"x","i":7,"f":1.5,"b":true}`)
}

func TestParse_LenientPrimitiveCoercion(t *testing.T) {
	// Mcoerce-b: native now ports BAML's lenient int/bool/float/null coercers,
	// so numeric-string and float→int inputs to an int field CLAIM the coerced
	// value (byte-identical to BAML) instead of declining.
	//
	// String where an int is required: BAML parses "36"->36 (clean).
	mustParse(t, personSchema(), `{"name":"Ada","age":"36"}`, `{"name":"Ada","age":36}`)
	// Float where an int is required: BAML rounds 36.5->37 (FloatToInt). A
	// required (non-nullable) field claims regardless of the flag.
	mustParse(t, personSchema(), `{"name":"Ada","age":36.5}`, `{"name":"Ada","age":37}`)
	// Same via the fixing parser (single-quoted name forces the fix path): the
	// fix is CLAIMED and the lenient coercion now CLAIMS too.
	mustParse(t, personSchema(), `{name:'Ada', age:36.5}`, `{"name":"Ada","age":37}`)
	mustParse(t, personSchema(), `{name:'Ada', age:'36'}`, `{"name":"Ada","age":36}`)
	// Mcoerce-d PR 1: a number where a string is required now stringifies via
	// jsonish Value Display (JsonToString), so native CLAIMS "36".
	s := &bamlutils.DynamicOutputSchema{Properties: props(kv("v", strProp()))}
	mustParse(t, s, `{"v":36}`, `{"v":"36"}`)
}

func TestParse_List(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("tags", &bamlutils.DynamicProperty{
			Type:  "list",
			Items: &bamlutils.DynamicTypeSpec{Type: "string"},
		})),
	}
	mustParse(t, s, `{"tags":["x","y","z"]}`, `{"tags":["x","y","z"]}`)
	// Mcoerce-d PR 1: a non-string element is stringified (JsonToString) and
	// KEPT, so the list claims 2->"2" instead of declining.
	mustParse(t, s, `{"tags":["x",2]}`, `{"tags":["x","2"]}`)
	// Mcoerce-c: a non-array STRING where list<string> is required is wrapped as
	// a single implied element (SingleToArray) and coerces cleanly -> claimed.
	mustParse(t, s, `{"tags":"x"}`, `{"tags":["x"]}`)
	// A non-array NUMBER singleton is now SingleToArray then JsonToString -> ["5"].
	mustParse(t, s, `{"tags":5}`, `{"tags":["5"]}`)
}

func TestParse_OptionalPresentAndAbsent(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("name", strProp()),
			kv("nick", &bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "string"}}),
		),
	}
	// Present.
	mustParse(t, s, `{"name":"Ada","nick":"Ace"}`, `{"name":"Ada","nick":"Ace"}`)
	// Explicit null.
	mustParse(t, s, `{"name":"Ada","nick":null}`, `{"name":"Ada","nick":null}`)
	// Absent: the parser omits it; the downstream InjectAbsentOptionals
	// pass (not the parser) inserts the null.
	mustParse(t, s, `{"name":"Ada"}`, `{"name":"Ada"}`)
}

func TestParse_RequiredFieldMissingDeclines(t *testing.T) {
	// A required field with no EXACT key match DECLINES (not a claimed
	// error): BAML matches field keys fuzzily, so native cannot tell whether
	// BAML would fuzzy-match some other key to the missing field or hard-fail
	// — so it falls back. (Here "age" is genuinely absent and BAML hard-fails
	// too, but native must decline because it cannot know that in general.)
	requireUnsupported(t, personSchema(), `{"name":"Ada"}`)
}

func TestParse_NestedClassRef(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("name", strProp()),
			kv("addr", &bamlutils.DynamicProperty{Ref: "Address"}),
		),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Address", &bamlutils.DynamicClass{
				Properties: props(kv("city", strProp()), kv("zip", strProp())),
			}),
		),
	}
	mustParse(t, s, `{"name":"Ada","addr":{"city":"Lon","zip":"E1"}}`, `{"name":"Ada","addr":{"city":"Lon","zip":"E1"}}`)
}

func TestParse_Literals(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("status", &bamlutils.DynamicProperty{Type: "literal_string", Value: "active"}),
			kv("version", &bamlutils.DynamicProperty{Type: "literal_int", Value: int64(2)}),
			kv("ok", &bamlutils.DynamicProperty{Type: "literal_bool", Value: true}),
		),
	}
	mustParse(t, s, `{"status":"active","version":2,"ok":true}`, `{"status":"active","version":2,"ok":true}`)
	// Mcoerce-a: a string literal now matches fuzzily via match_string. A
	// case variant matches and the CANONICAL literal is emitted (not the raw
	// input).
	mustParse(t, s, `{"status":"ACTIVE","version":2,"ok":true}`, `{"status":"active","version":2,"ok":true}`)
	// A string with no fuzzy match still DECLINES (no exact/fold/substring
	// hit): BAML errors in this required position, but native falls back.
	requireUnsupported(t, s, `{"status":"paused","version":2,"ok":true}`)
	// An int literal stays EXACT: BAML rounds/parses (string→int, float→int),
	// which is Mcoerce-b, so a non-equal int value DECLINES.
	requireUnsupported(t, s, `{"status":"active","version":3,"ok":true}`)
}

func enumSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("color", &bamlutils.DynamicProperty{Ref: "Color"})),
		Enums: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Color", &bamlutils.DynamicEnum{
				Values: []*bamlutils.DynamicEnumValue{{Name: "RED"}, {Name: "GREEN"}, {Name: "BLUE"}},
			}),
		),
	}
}

func TestParse_EnumByRenderedValue(t *testing.T) {
	mustParse(t, enumSchema(), `{"color":"GREEN"}`, `{"color":"GREEN"}`)
	// A value with no EXACT enum match DECLINES: BAML's enum coercion is
	// fuzzy (case/punctuation/substring/accent via match_string), so native
	// (exact) declines rather than claiming a mismatch BAML might still
	// match (e.g. "green" -> GREEN).
	requireUnsupported(t, enumSchema(), `{"color":"MAUVE"}`)
}

func TestParse_EnumByAlias(t *testing.T) {
	// The model is shown the alias; coercion matches it and emits the
	// canonical value name.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("color", &bamlutils.DynamicProperty{Ref: "Color"})),
		Enums: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Color", &bamlutils.DynamicEnum{
				Values: []*bamlutils.DynamicEnumValue{{Name: "GREEN", Alias: "Verde"}},
			}),
		),
	}
	mustParse(t, s, `{"color":"Verde"}`, `{"color":"GREEN"}`)
}

func TestParse_UnsupportedStream(t *testing.T) {
	_, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{
		Raw: `{"name":"Ada","age":36}`, OutputSchema: personSchema(), Stream: true,
	})
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("stream parse: expected ErrDeBAMLParseUnsupported, got %v", err)
	}
}

func TestParse_UnsupportedNilSchema(t *testing.T) {
	_, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: `{}`, OutputSchema: nil})
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("nil schema: expected ErrDeBAMLParseUnsupported, got %v", err)
	}
}

// mapStringIntSchema is a root class with one map<string,int> field.
func mapStringIntSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("scores", &bamlutils.DynamicProperty{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{Type: "int"},
		})),
	}
}

func TestParse_MapStringInt(t *testing.T) {
	// Clean map<string,int>: object input, string keys, in-scope int values.
	// Byte-exact on purpose — map output MUST preserve INPUT key order (the
	// keys here are deliberately out of lexical order), the M2b contract.
	mustParseExact(t, mapStringIntSchema(),
		`{"scores":{"z":1,"a":2,"m":3}}`,
		`{"scores":{"z":1,"a":2,"m":3}}`)
	// Empty object is a clean empty map.
	mustParseExact(t, mapStringIntSchema(), `{"scores":{}}`, `{"scores":{}}`)
}

func TestParse_MapNonObjectFieldDefaults(t *testing.T) {
	// Mcoerce-d PR 2: a required map class field with a non-object value is a
	// PROVEN coerce_map error_unexpected_type, so BAML fills the map default {}
	// (DefaultButHadUnparseableValue) and native now CLAIMS {"scores":{}}.
	mustParse(t, mapStringIntSchema(), `{"scores":[1,2,3]}`, `{"scores":{}}`)
	mustParse(t, mapStringIntSchema(), `{"scores":"x"}`, `{"scores":{}}`)
}

func TestParse_MapBadValuePartialSkip(t *testing.T) {
	// Mcoerce-c: a value that is a PROVEN BAML int parse error is SKIPPED as a
	// partial map entry (MapValueParseError), not a whole-map decline. "x" is not
	// a number in any form (no i64/u64/f64/fraction/extracted match), so
	// coerce_int errors and BAML skips just that entry, keeping the rest in input
	// order.
	mustParseExact(t, mapStringIntSchema(), `{"scores":{"a":1,"b":"x"}}`, `{"scores":{"a":1}}`)
	// All values bad -> {} (still a successful, empty map).
	mustParseExact(t, mapStringIntSchema(), `{"scores":{"a":"x","b":"y"}}`, `{"scores":{}}`)
	// A float map value is NOT "bad": Mcoerce-b rounds 2.5->3 (FloatToInt, a
	// successful coercion BAML keeps) — see TestParse_MapLenientValueClaimed.
}

// TestParse_MapLenientValueClaimed pins the Mcoerce-b map-value flip: a float
// or numeric-string value coerces to int (a successful, entry-KEEPING coercion,
// not a MapValueParseError), so the whole clean map is CLAIMED byte-identical
// to BAML. The map's own score ignores the value's FloatToInt flag (score.rs),
// so this stays a claimable clean map.
func TestParse_MapLenientValueClaimed(t *testing.T) {
	mustParse(t, mapStringIntSchema(), `{"scores":{"a":1,"b":2.5}}`, `{"scores":{"a":1,"b":3}}`)
	mustParse(t, mapStringIntSchema(), `{"scores":{"a":1,"b":"7"}}`, `{"scores":{"a":1,"b":7}}`)
}

func TestParse_MapDuplicateKeyDeclines(t *testing.T) {
	// A duplicate input key would collapse to one output key; BAML's
	// duplicate insert/overwrite ordering is unproven here, so native
	// declines rather than claim it.
	requireUnsupported(t, mapStringIntSchema(), `{"scores":{"a":1,"a":2}}`)
}

// mapEnumKeySchema is a root class with one map<Key,string> field where Key
// is an enum {A,B}.
func mapEnumKeySchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("labels", &bamlutils.DynamicProperty{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Ref: "Key"},
			Values: &bamlutils.DynamicTypeSpec{Type: "string"},
		})),
		Enums: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Key", &bamlutils.DynamicEnum{
				Values: []*bamlutils.DynamicEnumValue{{Name: "A"}, {Name: "B"}},
			}),
		),
	}
}

func TestParse_MapEnumKeysExact(t *testing.T) {
	// Enum keys matched EXACTLY by rendered name; the emitted key is the
	// ORIGINAL input string (here identical to the canonical name), in input
	// key order.
	mustParseExact(t, mapEnumKeySchema(),
		`{"labels":{"A":"one","B":"two"}}`,
		`{"labels":{"A":"one","B":"two"}}`)
}

func TestParse_MapBadEnumKeyDeclines(t *testing.T) {
	// A key not exactly in the enum: BAML records MapKeyParseError and skips
	// it (partial map). Native declines the whole map.
	requireUnsupported(t, mapEnumKeySchema(), `{"labels":{"A":"one","C":"two"}}`)
}

func TestParse_MapFuzzyEnumKeyClaimed(t *testing.T) {
	// Mcoerce-a: a case/fuzzy variant ("a" for enum value A) now fuzzy-matches
	// via match_string and is CLAIMED. The emitted key is the ORIGINAL input
	// string "a" (maps insert the raw object key), not the canonical enum name.
	mustParseExact(t, mapEnumKeySchema(), `{"labels":{"a":"one"}}`, `{"labels":{"a":"one"}}`)
}

func TestParse_MapEnumKeyAlias(t *testing.T) {
	// The model is shown the alias; an exact alias match is accepted and the
	// emitted key is the ORIGINAL input string (the alias), NOT the canonical
	// enum name — maps insert the raw object key.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("labels", &bamlutils.DynamicProperty{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Ref: "Key"},
			Values: &bamlutils.DynamicTypeSpec{Type: "string"},
		})),
		Enums: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Key", &bamlutils.DynamicEnum{
				Values: []*bamlutils.DynamicEnumValue{{Name: "GREEN", Alias: "Verde"}},
			}),
		),
	}
	mustParseExact(t, s, `{"labels":{"Verde":"x"}}`, `{"labels":{"Verde":"x"}}`)
}

func TestParse_MapLiteralUnionKeysExact(t *testing.T) {
	// map<"A"|"B", string>: a non-nullable union of string literals as the
	// key, matched EXACTLY (exactly one flattened literal equal to the key).
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("m", &bamlutils.DynamicProperty{
			Type: "map",
			Keys: &bamlutils.DynamicTypeSpec{
				Type: "union",
				OneOf: []*bamlutils.DynamicTypeSpec{
					{Type: "literal_string", Value: "A"},
					{Type: "literal_string", Value: "B"},
				},
			},
			Values: &bamlutils.DynamicTypeSpec{Type: "string"},
		})),
	}
	mustParseExact(t, s, `{"m":{"A":"x","B":"y"}}`, `{"m":{"A":"x","B":"y"}}`)
	// A key matching NO string-literal-union arm is NOT a native partial skip: the
	// dynamic bridge KEEPS the non-matching key (live-captured full map), so
	// native declines the WHOLE map rather than skip an entry BAML keeps.
	requireUnsupported(t, s, `{"m":{"A":"x","C":"z"}}`)
}

// mapStringItemSchema is a root class with one map<string, Item> field; Item
// has fields {id, label} in that schema order.
func mapStringItemSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("by_id", &bamlutils.DynamicProperty{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{Ref: "Item"},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Item", &bamlutils.DynamicClass{
				Properties: props(kv("id", strProp()), kv("label", strProp())),
			}),
		),
	}
}

func TestParse_MapStringClassValues(t *testing.T) {
	// Map keys are out of lexical order AND each Item value has its fields out
	// of schema order in the input. The two ordering policies must coexist:
	// map keys stay in INPUT order while class fields are re-emitted in SCHEMA
	// order. Byte-exact on purpose — this is the dual-ordering contract.
	mustParseExact(t, mapStringItemSchema(),
		`{"by_id":{"z":{"label":"zed","id":"3"},"a":{"label":"ay","id":"1"}}}`,
		`{"by_id":{"z":{"id":"3","label":"zed"},"a":{"id":"1","label":"ay"}}}`)
	// A map-to-class entry missing a required field DECLINES the whole map:
	// BAML skips the bad entry (MapValueParseError), native cannot claim it.
	requireUnsupported(t, mapStringItemSchema(),
		`{"by_id":{"a":{"id":"1","label":"ay"},"b":{"id":"2"}}}`)
}

func TestParse_MapNestedMap(t *testing.T) {
	// map<string, map<string,int>>: nested maps under the same M2b rules,
	// both levels preserving input key order.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("grid", &bamlutils.DynamicProperty{
			Type: "map",
			Keys: &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{
				Type:   "map",
				Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
				Values: &bamlutils.DynamicTypeSpec{Type: "int"},
			},
		})),
	}
	mustParseExact(t, s,
		`{"grid":{"r2":{"c1":1,"c0":2},"r1":{"c9":3}}}`,
		`{"grid":{"r2":{"c1":1,"c0":2},"r1":{"c9":3}}}`)
}

func TestParse_MapIncompleteDeclines(t *testing.T) {
	// An unterminated map has no balanced span; native finds no
	// cleanly-claimable candidate and DECLINES (BAML closes/recovers at EOF,
	// which M2a defers) — never a claimed error.
	requireUnsupported(t, mapStringIntSchema(), `{"scores":{"a":1,"b":`)
	requireUnsupported(t, mapStringIntSchema(), `{"scores":{"a":1,"b":2`)
}

func TestParse_UnsupportedMapIntKey(t *testing.T) {
	// A map key outside the legal set (int) is rejected upstream by schema
	// validation; the parser falls back rather than claiming.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("m", &bamlutils.DynamicProperty{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Type: "int"},
			Values: &bamlutils.DynamicTypeSpec{Type: "int"},
		})),
	}
	requireUnsupported(t, s, `{"m":{"1":1}}`)
}

func TestParse_MapScalarUnionValueClaimed(t *testing.T) {
	// M3b: a map VALUE that is a scalar-leaf union (string|int) is now in scope —
	// checkSupportedType's value recursion admits it, and coerceMapValueChild
	// routes the value through the try_cast-first scalar-union coercer. "x"
	// try_casts to the string arm; numeric value 5 skips string try_cast and
	// try_casts to int, so JsonToString phase-2 scoring is not reached.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("m", &bamlutils.DynamicProperty{
			Type: "map",
			Keys: &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{
				Type: "union",
				OneOf: []*bamlutils.DynamicTypeSpec{
					{Type: "string"},
					{Type: "int"},
				},
			},
		})),
	}
	mustParse(t, s, `{"m":{"a":"x"}}`, `{"m":{"a":"x"}}`)
	mustParse(t, s, `{"m":{"a":5}}`, `{"m":{"a":5}}`)
	// M3d: an ARRAY value now resolves to the INT arm. As a union arm the int's
	// array-to-singular target is the union, so Int 1 scores 1 (UnionMatch +
	// FirstMatch) and beats the string arm's JsonToString "[1, 2]" (score 2), so
	// the map keeps {"a":1}.
	mustParse(t, s, `{"m":{"a":[1,2]}}`, `{"m":{"a":1}}`)
}

// unionSchema wraps a single union property `u` with the given variants.
func unionSchema(variants ...*bamlutils.DynamicTypeSpec) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: variants,
		})),
	}
}

func litStr(v string) *bamlutils.DynamicTypeSpec {
	return &bamlutils.DynamicTypeSpec{Type: "literal_string", Value: v}
}

func TestParse_LiteralUnionStringExactClaimed(t *testing.T) {
	// Homogeneous string-literal union: an input that EXACTLY equals a literal
	// value try_casts to that arm in phase 1 and is CLAIMED. M3b no longer
	// requires literal values to be pairwise match-disjoint: exact inputs still
	// return from phase 1, while non-exact inputs that coerce against multiple
	// arms are resolved by phase-2 scoring / pick_best.
	s := unionSchema(litStr("small"), litStr("large"))
	mustParse(t, s, `{"u":"small"}`, `{"u":"small"}`)
	mustParse(t, s, `{"u":"large"}`, `{"u":"large"}`)
	// "medium" try_casts to neither literal and match_strings neither in phase 2,
	// so no arm succeeds → DECLINE (BAML also errors; native falls back).
	requireUnsupported(t, s, `{"u":"medium"}`)
	// A JSON number try_casts to neither string literal; the phase-2 stringify
	// (ObjectToString "5") matches neither value → no arm succeeds → DECLINE.
	requireUnsupported(t, s, `{"u":5}`)
}

func TestParse_LiteralUnionFuzzyStringScored(t *testing.T) {
	// M3b: a string-literal union whose values are NOT match-disjoint
	// ("on" | "only") is now CLAIMED by the try_cast-first scalar-union path
	// instead of declined at the gate. "on" exact-matches arm 0 in phase 1. "only"
	// skips arm 0's exact literal try_cast, then exact-matches arm 1 in phase 1;
	// substring scoring / pick_best is not reached. This pins the formerly
	// non-disjoint literal family now admitted by M3b (fixture 41).
	s := unionSchema(litStr("on"), litStr("only"))
	mustParse(t, s, `{"u":"on"}`, `{"u":"on"}`)
	mustParse(t, s, `{"u":"only"}`, `{"u":"only"}`)
	// Case/punctuation-only variants: "Yes" exact-matches arm 0 in phase-1 try_cast.
	s2 := unionSchema(litStr("Yes"), litStr("yes!"))
	mustParse(t, s2, `{"u":"Yes"}`, `{"u":"Yes"}`)
}

func TestParse_LiteralUnionBoolExactClaimed(t *testing.T) {
	s := unionSchema(
		&bamlutils.DynamicTypeSpec{Type: "literal_bool", Value: true},
		&bamlutils.DynamicTypeSpec{Type: "literal_bool", Value: false},
	)
	mustParse(t, s, `{"u":true}`, `{"u":true}`)
	mustParse(t, s, `{"u":false}`, `{"u":false}`)
	// Mcoerce-b: a string bool coerces (StringToBool) and matches exactly one
	// arm, so the union CLAIMS (a coerced bool equals at most one literal, so
	// the family stays safe). "true"→true.
	mustParse(t, s, `{"u":"true"}`, `{"u":true}`)
	mustParse(t, s, `{"u":"False"}`, `{"u":false}`)
	// A number is not a bool in any form (coerce_bool errors on a number), so
	// neither arm coerces → DECLINE.
	requireUnsupported(t, s, `{"u":1}`)
}

func TestParse_LiteralUnionIntExactClaimed(t *testing.T) {
	s := unionSchema(
		&bamlutils.DynamicTypeSpec{Type: "literal_int", Value: int64(1)},
		&bamlutils.DynamicTypeSpec{Type: "literal_int", Value: int64(2)},
	)
	mustParse(t, s, `{"u":1}`, `{"u":1}`)
	mustParse(t, s, `{"u":2}`, `{"u":2}`)
	// No literal equals the coerced integer → DECLINE (3 matches neither arm).
	requireUnsupported(t, s, `{"u":3}`)
	// Mcoerce-b: a JSON float rounds and a numeric string parses; the coerced
	// int equals at most one literal, so the union CLAIMS the lone match.
	mustParse(t, s, `{"u":2.0}`, `{"u":2}`) // 2.0→2 matches literal 2
	mustParse(t, s, `{"u":1.5}`, `{"u":2}`) // round(1.5)=2 matches literal 2
	mustParse(t, s, `{"u":"2"}`, `{"u":2}`) // "2"→2 matches literal 2
}

func TestParse_LiteralUnionMixedKindScored(t *testing.T) {
	// M3b: a mixed-literal-kind union (literal "1" | literal 1) is a scalar-leaf
	// family, now claimed via the try_cast-first pass rather than declined at the
	// gate. A JSON string "1" exact-matches the string literal (arm 0) in phase 1
	// -> "1". A JSON number 1 skips the string-literal try_cast, exact-matches the
	// int literal in phase 1, and emits 1; ObjectToString scoring is not reached.
	// The winner's own kind/value is emitted.
	s := unionSchema(litStr("1"), &bamlutils.DynamicTypeSpec{Type: "literal_int", Value: int64(1)})
	mustParse(t, s, `{"u":"1"}`, `{"u":"1"}`)
	mustParse(t, s, `{"u":1}`, `{"u":1}`)
}

// classUnionSchema wraps a single union property `u` over two flat classes
// Book{title,pages} and Car{brand,wheels} with disjoint field-name sets.
func classUnionSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Ref: "Book"},
				{Ref: "Car"},
			},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Book", &bamlutils.DynamicClass{
				Properties: props(kv("title", strProp()), kv("pages", intProp())),
			}),
			bamlutils.OrderedKV("Car", &bamlutils.DynamicClass{
				Properties: props(kv("brand", strProp()), kv("wheels", intProp())),
			}),
		),
	}
}

func TestParse_ClassUnionFlatDisjointClaimed(t *testing.T) {
	s := classUnionSchema()
	// Input key set == exactly one variant's full field set → CLAIM, emitted
	// in that class's schema order.
	mustParse(t, s, `{"u":{"title":"Go","pages":300}}`, `{"u":{"title":"Go","pages":300}}`)
	mustParse(t, s, `{"u":{"brand":"Audi","wheels":4}}`, `{"u":{"brand":"Audi","wheels":4}}`)
	// Class fields re-emitted in SCHEMA order even when input is out of order.
	mustParse(t, s, `{"u":{"pages":300,"title":"Go"}}`, `{"u":{"title":"Go","pages":300}}`)
	// Mcoerce-a: an EXTRA key beyond a variant's full field set is ignored
	// (ExtraKey) and the arm still wins — exactly one variant succeeds, so it
	// is CLAIMED (the extra "x" does not fuzzy-match Car's disjoint fields).
	mustParse(t, s, `{"u":{"title":"Go","pages":300,"x":1}}`, `{"u":{"title":"Go","pages":300}}`)
	// Mcoerce-b: pages="300" now coerces via a CLEAN direct string→int parse
	// (BAML's s.parse::<i64>() adds no flag), so Book is still the lone clean
	// winner and is CLAIMED. (Before Mcoerce-b native declined this.)
	mustParse(t, s, `{"u":{"title":"Go","pages":"300"}}`, `{"u":{"title":"Go","pages":300}}`)
}

// TestParse_ClassUnionProvableLosingArmClaimed pins F-Codex / F-Codex2: when a
// LOSING arm has a PROVABLE required-field parse error, BAML errors just that arm
// and picks the other — native must EXCLUDE the losing arm (not decline the whole
// union). This covers a primitive int bad-string field, a required string ← null,
// and (F-Codex2) an int/bool LITERAL field value mismatch (which the leaf reports
// as a proven error but the type-shape whitelist alone does not recognize).
func TestParse_ClassUnionProvableLosingArmClaimed(t *testing.T) {
	s := classUnionSchema() // Book{title,pages} | Car{brand,wheels}
	// Book matches title,pages (brand,wheels extras -> score 2); Car matches
	// wheels="bad", which PROVABLY fails int coercion -> BAML errors Car -> Book.
	mustParse(t, s, `{"u":{"title":"Go","pages":300,"brand":"Audi","wheels":"bad"}}`, `{"u":{"title":"Go","pages":300}}`)
	// Symmetric: title=null provably errors Book (required string ← null), so Car
	// (a valid strict match) wins.
	mustParse(t, s, `{"u":{"title":null,"pages":300,"brand":"Audi","wheels":4}}`, `{"u":{"brand":"Audi","wheels":4}}`)

	// F-Codex2: a required int/bool LITERAL field whose value mismatches is a proven
	// BAML error, so BAML errors that arm and picks the other.
	// A{a int, b string} | B{c literal_int 5, d string}: c=7 (≠5) errors B -> A.
	litInt := litFieldClassUnionSchema(&bamlutils.DynamicTypeSpec{Type: "literal_int", Value: int64(5)})
	mustParse(t, litInt, `{"u":{"a":1,"b":"x","c":7,"d":"y"}}`, `{"u":{"a":1,"b":"x"}}`)
	// A{a int, b string} | B{c literal_bool true, d string}: c=false (≠true) errors B -> A.
	litBool := litFieldClassUnionSchema(&bamlutils.DynamicTypeSpec{Type: "literal_bool", Value: true})
	mustParse(t, litBool, `{"u":{"a":1,"b":"x","c":false,"d":"y"}}`, `{"u":{"a":1,"b":"x"}}`)
}

// litFieldClassUnionSchema builds A{a int, b string} | B{c <lit>, d string} — a
// flat disjoint-key class union whose B arm has a literal field, for the
// literal-field proven-error losing-arm tests.
func litFieldClassUnionSchema(lit *bamlutils.DynamicTypeSpec) *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "B"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{
				Properties: props(kv("a", intProp()), kv("b", strProp())),
			}),
			bamlutils.OrderedKV("B", &bamlutils.DynamicClass{
				Properties: props(
					kv("c", &bamlutils.DynamicProperty{Type: lit.Type, Value: lit.Value}),
					kv("d", strProp()),
				),
			}),
		),
	}
}

func TestParse_ClassUnionDeclines(t *testing.T) {
	s := classUnionSchema()
	// Missing a required field → DECLINE (BAML may fuzzy-match/fill).
	requireUnsupported(t, s, `{"u":{"title":"Go"}}`)
	// Key set matches no variant → DECLINE.
	requireUnsupported(t, s, `{"u":{"foo":1,"bar":2}}`)
	// Non-object input → DECLINE (BAML can infer/imply a class from a scalar).
	requireUnsupported(t, s, `{"u":5}`)
	requireUnsupported(t, s, `{"u":"Go"}`)
}

// nullableClassUnionSchema is classUnionSchema's nullable sibling: Book | Car |
// null. The null arm competes by scoring for non-null input (F1).
func nullableClassUnionSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Ref: "Book"},
				{Ref: "Car"},
				{Type: "null"},
			},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Book", &bamlutils.DynamicClass{
				Properties: props(kv("title", strProp()), kv("pages", intProp())),
			}),
			bamlutils.OrderedKV("Car", &bamlutils.DynamicClass{
				Properties: props(kv("brand", strProp()), kv("wheels", intProp())),
			}),
		),
	}
}

func TestParse_NullableClassUnionScored(t *testing.T) {
	// M3: for a NULLABLE union with non-null input, BAML scores the null arm too
	// (any non-null value -> null with DefaultButHadValue cost 110). Native now
	// reproduces the score model: the winning class arm is claimed whenever its
	// inherent score is < 110.
	s := nullableClassUnionSchema()
	// Null input -> null fast path.
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	// CLEAN winning arm (score 0) -> CLAIM immediately (early first-winner).
	mustParse(t, s, `{"u":{"title":"Go","pages":300}}`, `{"u":{"title":"Go","pages":300}}`)
	// ONE extra key (ExtraKey score 1) still scores well under null's 110, so the
	// Book arm wins and is CLAIMED (extra omitted). (The extra-key-heavy variant
	// that actually loses to null is exercised by the >110 corpus fixtures.)
	mustParse(t, s, `{"u":{"title":"Go","pages":300,"x":1}}`, `{"u":{"title":"Go","pages":300}}`)
}

func TestParse_NonASCIICaseFoldUnionDeclines(t *testing.T) {
	// P2: native's case fold (cases.Lower) is not byte-identical to Rust's
	// str::to_lowercase for every rune, so any match whose verdict hinges on
	// lowercasing a non-ASCII rune Go can't prove is lowercase-stable is
	// UNCERTAIN. A|B with A{a: literal "é"(U+00E9), aa, aaa} | B{b, bb}: arm A's
	// literal only matches the input "É"(U+00C9) via the case-fold attempt
	// (NFKD leaves the accent so it is not connected at the accent-fold stage),
	// and 'É' is non-ASCII and not IsLower → uncertain. Native must DECLINE THE
	// UNION rather than false-reject A and claim B as the lone winner.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type:  "union",
			OneOf: []*bamlutils.DynamicTypeSpec{{Ref: "A"}, {Ref: "B"}},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{
				Properties: props(
					kv("a", &bamlutils.DynamicProperty{Type: "literal_string", Value: "é"}),
					kv("aa", intProp()),
					kv("aaa", intProp()),
				),
			}),
			bamlutils.OrderedKV("B", &bamlutils.DynamicClass{
				Properties: props(kv("b", strProp()), kv("bb", intProp())),
			}),
		),
	}
	requireUnsupported(t, s, "{\"u\":{\"a\":\"É\",\"aa\":1,\"aaa\":1,\"b\":\"x\",\"bb\":2}}")
}

func TestParse_NonASCIICaseFoldStandaloneFallsBack(t *testing.T) {
	// Standalone (non-union) literal/enum under the same uncertainty: native
	// falls back rather than risk a claim that diverges from BAML.
	lit := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("k", &bamlutils.DynamicProperty{Type: "literal_string", Value: "é"})),
	}
	// "É"(U+00C9) only matches "é" via the uncertain case fold -> decline.
	requireUnsupported(t, lit, "{\"k\":\"É\"}")
	// Exact "é" needs no case fold -> certain -> claimed (the canonical literal).
	mustParse(t, lit, "{\"k\":\"é\"}", "{\"k\":\"é\"}")

	enum := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("c", &bamlutils.DynamicProperty{Ref: "Acc"})),
		Enums: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Acc", &bamlutils.DynamicEnum{
				Values: []*bamlutils.DynamicEnumValue{{Name: "É"}},
			}),
		),
	}
	requireUnsupported(t, enum, "{\"c\":\"é\"}")

	// ASCII case folding is UNAFFECTED — it stays certain and is claimed.
	mustParse(t, personSchema(), `{"Name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
}

func TestParse_OptionalArmScored(t *testing.T) {
	// M3 scored single-arm optional path: c is Color? (Color enum | null).
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("c", &bamlutils.DynamicProperty{
			Type:  "optional",
			Inner: &bamlutils.DynamicTypeSpec{Ref: "Color"},
		})),
		Enums: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Color", &bamlutils.DynamicEnum{
				Values: []*bamlutils.DynamicEnumValue{{Name: "RED"}, {Name: "GREEN"}},
			}),
		),
	}
	// Exact and case-fold enum matches are score 0 (clean) -> CLAIM immediately.
	mustParse(t, s, `{"c":"GREEN"}`, `{"c":"GREEN"}`)
	mustParse(t, s, `{"c":"green"}`, `{"c":"GREEN"}`)
	// A SUBSTRING match adds SubstringMatch (score 2), which is < 110, so the enum
	// arm beats the scored null arm and is now CLAIMED (M3 scoring).
	mustParse(t, s, `{"c":"the color green please"}`, `{"c":"GREEN"}`)
}

func TestParse_ClassUnionOverlappingKeysClaimed(t *testing.T) {
	// M3c: two classes sharing a field name (id) are NO LONGER declined at the gate
	// — pick_best now resolves overlapping-key arms. Here the input's full field set
	// is exactly A{id,name}, so A try_casts at score 0 in phase 1 and wins outright
	// (fixture 39). (The B arm's try_cast rejects the extra `name` key.)
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Ref: "A"},
				{Ref: "B"},
			},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{
				Properties: props(kv("id", intProp()), kv("name", strProp())),
			}),
			bamlutils.OrderedKV("B", &bamlutils.DynamicClass{
				Properties: props(kv("id", intProp()), kv("label", strProp())),
			}),
		),
	}
	mustParse(t, s, `{"u":{"id":1,"name":"x"}}`, `{"u":{"id":1,"name":"x"}}`)
}

func TestParse_ClassUnionSingleFieldClaimed(t *testing.T) {
	// M3c: a single-field class arm is NO LONGER declined at the gate — the
	// implied-key / inferred-object paths and the pick_best classSingleImplied
	// devalue are modeled. Here the input's key set is exactly A{only}, so A
	// try_casts at score 0 in phase 1 and wins (the B arm rejects the `only` key).
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Ref: "A"},
				{Ref: "B"},
			},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{
				Properties: props(kv("only", intProp())),
			}),
			bamlutils.OrderedKV("B", &bamlutils.DynamicClass{
				Properties: props(kv("solo", strProp())),
			}),
		),
	}
	mustParse(t, s, `{"u":{"only":1}}`, `{"u":{"only":1}}`)
}

func TestParse_ClassUnionNonFlatFieldDeclinedAtGate(t *testing.T) {
	// A class-union arm with a non-flat-leaf field (a list) is out of scope:
	// BAML's single-to-array leniency could make a second arm succeed.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Ref: "A"},
				{Ref: "B"},
			},
		})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("A", &bamlutils.DynamicClass{
				Properties: props(kv("title", strProp()), kv("tags", &bamlutils.DynamicProperty{
					Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"},
				})),
			}),
			bamlutils.OrderedKV("B", &bamlutils.DynamicClass{
				Properties: props(kv("brand", strProp()), kv("wheels", intProp())),
			}),
		),
	}
	requireUnsupported(t, s, `{"u":{"title":"Go","tags":["a"]}}`)
}

func TestParse_NullableScalarMultiUnionScored(t *testing.T) {
	// M3b: a nullable scalar-leaf multi-union (string | int | null) is now claimed
	// for non-null input too. The variants flatten to [string, int] (null hoisted
	// to Nullable). JSON null uses the null fast path. "x" try_casts to string; 5
	// skips string try_cast and try_casts to int, so both non-null cases return
	// before phase-2 scoring or null-arm competition.
	s := unionSchema(
		&bamlutils.DynamicTypeSpec{Type: "string"},
		&bamlutils.DynamicTypeSpec{Type: "int"},
		&bamlutils.DynamicTypeSpec{Type: "null"},
	)
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	mustParse(t, s, `{"u":"x"}`, `{"u":"x"}`)
	mustParse(t, s, `{"u":5}`, `{"u":5}`)
}

func TestParse_NullableSingleArmUnsupportedArmClaimsNull(t *testing.T) {
	// A nullable single-arm union T | null where T is UNSUPPORTED (here T is a map
	// whose value is a union with a list-of-MULTI-ARM-UNION arm — string |
	// list<int|string> — whose list element is a multi-arm union, so its array
	// union_variant_hint is out of scope and the value union declines). The null
	// fast path must CLAIM null regardless of the unsupported arm (mirroring BAML's
	// null arm), consistently with nullable MULTI unions. A NON-null input must
	// DECLINE: coerceUnionSafe delegates to coerce on the lone arm, which falls
	// back because the map value union is unsupported.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "optional",
			Inner: &bamlutils.DynamicTypeSpec{
				Type: "map",
				Keys: &bamlutils.DynamicTypeSpec{Type: "string"},
				Values: &bamlutils.DynamicTypeSpec{
					Type: "union",
					OneOf: []*bamlutils.DynamicTypeSpec{
						{Type: "string"},
						{Type: "list", Items: &bamlutils.DynamicTypeSpec{
							Type: "union",
							OneOf: []*bamlutils.DynamicTypeSpec{
								{Type: "int"},
								{Type: "string"},
							},
						}},
					},
				},
			},
		})),
	}
	// JSON null → CLAIM null (null fast path), even though the lone arm is
	// unsupported. Before the gate fix this DECLINED (the len==1 recursion
	// rejected the unsupported arm before the nullable check).
	mustParse(t, s, `{"u":null}`, `{"u":null}`)
	// Non-null → DECLINE (the lone map arm's value union has a list arm).
	requireUnsupported(t, s, `{"u":{"a":"x"}}`)
}

func TestParse_PrimitiveScalarUnionScored(t *testing.T) {
	// M3b: a bare-primitive scalar-leaf union (string | int) is now claimed via
	// the try_cast-first pass. A plain string try_casts to the string arm. A JSON
	// number 5 is selected in phase 1: string try_cast rejects numbers, int
	// try_cast accepts, so JsonToString scoring is skipped.
	s := unionSchema(
		&bamlutils.DynamicTypeSpec{Type: "string"},
		&bamlutils.DynamicTypeSpec{Type: "int"},
	)
	mustParse(t, s, `{"u":"x"}`, `{"u":"x"}`)
	mustParse(t, s, `{"u":5}`, `{"u":5}`)
	// A numeric STRING try_casts to the string arm in phase 1, before the int arm
	// is considered (mirrors fixture 37).
	mustParse(t, s, `{"u":"123"}`, `{"u":"123"}`)
}

func TestParse_NestedScalarUnionFlattenedScored(t *testing.T) {
	// A genuinely nested union ((string | int) | bool) flattens (simplifyUnion,
	// mirroring BAML's TypeIR::union) to the scalar-leaf set [string, int, bool].
	// M3b resolves it try_cast-first: for 123, the flattened string arm rejects
	// the number in try_cast, the int arm accepts in phase 1, and the value emits
	// as 123; JsonToString scoring is not reached. "a" try_casts to the string
	// arm. (Mirrors fixture 46.)
	s := unionSchema(
		&bamlutils.DynamicTypeSpec{Type: "union", OneOf: []*bamlutils.DynamicTypeSpec{
			{Type: "string"},
			{Type: "int"},
		}},
		&bamlutils.DynamicTypeSpec{Type: "bool"},
	)
	mustParse(t, s, `{"u":123}`, `{"u":123}`)
	mustParse(t, s, `{"u":"a"}`, `{"u":"a"}`)
	// A JSON bool try_casts to the bool arm (index 2) at score 0 — try_cast skips
	// the string/int arms (a bool is neither a JSON string nor a JSON number), so
	// it is CLAIMED (the int arm's lenient decline is never reached).
	mustParse(t, s, `{"u":true}`, `{"u":true}`)
}

func TestParse_SingleFieldClassImpliedKeyInferredObject(t *testing.T) {
	// A single-field class: BAML absorbs a scalar/non-object into the one field
	// via inferred-object, or an object whose keys miss the lone field via
	// implied-key. Mcoerce-d PR 2 ports both, so native now CLAIMS them when the
	// lone-field coercion succeeds and matches BAML byte-for-byte.
	oneField := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("value", intProp())),
	}
	// Scalar input -> inferred-object into the lone int field (ImpliedKey +
	// InferedObject): claimed {value: 42}.
	mustParse(t, oneField, `42`, `{"value":42}`)
	// Object with no matching key, lone field is int -> implied-key coerces the
	// WHOLE object into the int field, which FAILS (object->int is
	// error_unexpected_type), so the field is missing + non-defaultable and BAML
	// errors; native cannot prove that from its own decline, so it DECLINES.
	requireUnsupported(t, oneField, `{"other":5}`)
	// The lone field present -> normal claim (no implied-key needed).
	mustParse(t, oneField, `{"value":5}`, `{"value":5}`)

	// A single-field STRING class absorbs a non-matching object via implied-key +
	// JsonToString: {foo: 5} stringifies to "{foo: 5}" into the lone string field.
	strBox := &bamlutils.DynamicOutputSchema{Properties: props(kv("value", strProp()))}
	mustParse(t, strBox, `{"foo":5}`, `{"value":"{foo: 5}"}`)
	// A scalar into the lone string field: inferred-object + JsonToString.
	mustParse(t, strBox, `true`, `{"value":"true"}`)

	// A MULTI-field class with a NON-OBJECT input DECLINES: an ARRAY defers to
	// coerce_array_to_singular (M3) and a scalar leaves every required
	// non-defaultable field missing (BAML errors, native falls back).
	requireUnsupported(t, personSchema(), `[1,2,3]`)
	requireUnsupported(t, personSchema(), `42`)
}

func TestParse_MultiFieldFuzzyKey(t *testing.T) {
	// Mcoerce-a: a required field key now matches fuzzily via match_string
	// (no substring). A differently-cased key "Name" matches `name`, so the
	// class is CLAIMED with the CANONICAL field name emitted.
	mustParse(t, personSchema(), `{"Name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
	// All required fields matched by EXACT key -> CLAIM.
	mustParse(t, personSchema(), `{"name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
	// Extra/unknown keys are ignored on both sides when all required fields
	// are present -> still CLAIM.
	mustParse(t, personSchema(), `{"name":"Ada","age":36,"extra":true}`, `{"name":"Ada","age":36}`)
}

func TestParse_ClassFuzzyKeyFirstWins(t *testing.T) {
	// F2: when two input keys fuzzy-match the SAME field, BAML's update_map
	// keeps the FIRST matched value and ignores later duplicates
	// (coerce_class.rs:548 "DO NOTHING (keep first value)"). "name" and "Name"
	// both match field `name`; the FIRST ("Ada") wins, not the last.
	mustParse(t, personSchema(), `{"name":"Ada","Name":"Grace","age":36}`, `{"name":"Ada","age":36}`)
	// The first occurrence wins regardless of which case appears first.
	mustParse(t, personSchema(), `{"Name":"Grace","name":"Ada","age":36}`, `{"name":"Grace","age":36}`)
}

func TestParse_FixingTrailingCommas(t *testing.T) {
	// Trailing comma after a quoted string value and after an array value
	// (and a top-level trailing comma) — all parity-safe and claimed.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("name", strProp()),
			kv("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}}),
		),
	}
	mustParse(t, s, `{"name":"Ada","tags":["x","y",],}`, `{"name":"Ada","tags":["x","y"]}`)

	// A trailing comma right after an UNQUOTED NUMBER object value DECLINES:
	// BAML's fixing parser consumes the comma (the byte after it is '}', not
	// space/newline) and reads greedily, producing the string "36," — which
	// then fails int coercion, so BAML errors. Native must not claim a clean
	// 36 where BAML errors, so it declines.
	requireUnsupported(t, personSchema(), `{"name":"Ada","age":36,}`)
}

func TestParse_FixingLeadingAndRepeatedCommas(t *testing.T) {
	// Leading and repeated/stray commas in objects (BAML's object state
	// ignores stray commas while waiting for content).
	mustParse(t, personSchema(), `{,"name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
	mustParse(t, personSchema(), `{"name":"Ada",,"age":36}`, `{"name":"Ada","age":36}`)
	// Leading, repeated, and trailing commas in an array.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("nums", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "int"}})),
	}
	mustParse(t, s, `{"nums":[,1,,2,]}`, `{"nums":[1,2]}`)
}

func TestParse_FixingUnquotedKeys(t *testing.T) {
	mustParse(t, personSchema(), `{name: "Ada", age: 36}`, `{"name":"Ada","age":36}`)
	// Unquoted keys with bool / null / number values.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("flag", boolProp()),
			kv("count", intProp()),
			kv("maybe", &bamlutils.DynamicProperty{Type: "optional", Inner: &bamlutils.DynamicTypeSpec{Type: "string"}}),
		),
	}
	mustParse(t, s, `{flag: true, count: 5, maybe: null}`, `{"flag":true,"count":5,"maybe":null}`)
}

func TestParse_FixingSingleQuotes(t *testing.T) {
	mustParse(t, personSchema(), `{'name': 'Ada', 'age': 36}`, `{"name":"Ada","age":36}`)
	// Single-quoted keys/values inside a nested object.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("outer", &bamlutils.DynamicProperty{Ref: "Inner"})),
		Classes: bamlutils.MustOrderedMap(
			bamlutils.OrderedKV("Inner", &bamlutils.DynamicClass{Properties: props(kv("inner", strProp()))}),
		),
	}
	mustParse(t, s, `{'outer': {'inner': 'val'}}`, `{"outer":{"inner":"val"}}`)
}

func TestParse_SingleQuotedValueWithDelimiter(t *testing.T) {
	// Span detection is single-quote-BLIND (matching BAML's quote-blind
	// multi-json/prose grep). A structural '}' inside a single-quoted value
	// therefore terminates the span early: `{name:'Ada } Lovelace', age:36}`
	// slices to `{name:'Ada }`, which the fixing pass rejects as an
	// unterminated single-quoted string and DECLINES. That is parity-safe:
	// BAML greps the same prefix as one of several scored candidates, and
	// native must not claim the wider object on its own.
	requireUnsupported(t, personSchema(), `{name:'Ada } Lovelace', age:36}`)
	requireUnsupported(t, personSchema(), `Here: {name:'Ada } Lovelace', age:36} done.`)
	// When the brackets inside the single-quoted value happen to be balanced,
	// quote-blind slicing yields the whole object — exactly the span BAML's
	// (also quote-blind) grep produces — so native claims it and matches.
	s := &bamlutils.DynamicOutputSchema{Properties: props(kv("note", strProp()))}
	mustParse(t, s, `{note:'see [1] and {x}'}`, `{"note":"see [1] and {x}"}`)
}

func TestParse_FixingProseJSONish(t *testing.T) {
	// Prose around a JSONish object (unquoted key + single-quoted value):
	// the balanced span is selected, then fixed.
	raw := "Here you go: {name: 'Ada', age: 36} — that's the record."
	mustParse(t, personSchema(), raw, `{"name":"Ada","age":36}`)
}

func TestParse_FixingFencedJSONish(t *testing.T) {
	// Fenced JSONish (unquoted key + single-quoted value): BAML recurses
	// into the fence with fixes enabled, and so does the native path.
	raw := "Here:\n```json\n{name: 'Ada', age: 36}\n```\nDone."
	mustParse(t, personSchema(), raw, `{"name":"Ada","age":36}`)
}

func TestParse_FixingDeferredFallsBack(t *testing.T) {
	// Repairs outside the conservative M2a subset must still fall back to
	// BAML (ErrDeBAMLParseUnsupported), preserving differential parity.
	//
	// Comments.
	requireUnsupported(t, personSchema(), `{"name":"Ada","age":36 /* note */}`)
	requireUnsupported(t, personSchema(), "{\"name\":\"Ada\", // note\n\"age\":36}")
	// Missing comma between fields.
	requireUnsupported(t, personSchema(), `{"name":"Ada" "age":36}`)
	// Escapes inside a double-quoted string (BAML's escape fixing deferred).
	requireUnsupported(t, personSchema(), `{name:"A\nda",age:36}`)
	// Bareword (non bool/null/number) unquoted value.
	requireUnsupported(t, personSchema(), `{name: Ada, age: 36}`)
	// Backtick-quoted value.
	s := &bamlutils.DynamicOutputSchema{Properties: props(kv("msg", strProp()))}
	requireUnsupported(t, s, "{msg: `hi`}")
}

func TestParse_NoCandidateDeclines(t *testing.T) {
	// "Couldn't find / complete a candidate" is a DECLINE, never a claim:
	// BAML may still recover any of these, so native falls back rather than
	// claiming a parse error that would diverge if BAML succeeds.
	//
	// Truncated mid-value (unterminated object): BAML's fixing parser closes
	// open collections at EOF and recovers a (partial) value.
	requireUnsupported(t, personSchema(), `{"name":"Ada","age":`)
	// Unterminated object with a complete prior field — still no closing
	// brace, so no balanced span; decline (M2a defers unterminated).
	requireUnsupported(t, personSchema(), `{"name":"Ada","age":36`)
	// Unterminated array.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}})),
	}
	requireUnsupported(t, s, `{"tags":["x","y"`)
	// Pure prose with no JSON candidate at all: BAML falls to a top-level
	// string, which then can't coerce to the object schema — but native
	// declines rather than claiming, since it found no candidate.
	requireUnsupported(t, personSchema(), `I could not produce a record.`)
}

func TestParse_MultipleTopLevelValuesDeclines(t *testing.T) {
	// Two top-level objects: BAML greps ALL balanced objects and scores them
	// (a later one can win), which M2a defers. Native must DECLINE rather
	// than claim the first span (which would propagate a spurious
	// missing-field error here).
	requireUnsupported(t, personSchema(), `{"name":"Ada"} {"name":"Bob","age":40}`)
	// A trailing bracketed structure after a valid object also declines.
	requireUnsupported(t, personSchema(), `{"name":"Ada","age":36} [1,2,3]`)
	// But a single object with trailing PROSE (no further brackets) is still
	// cleanly claimed — the strict whole-input fails on the trailing text,
	// and the balanced span has no second candidate after it.
	mustParse(t, personSchema(), `{"name":"Ada","age":36} that's all.`, `{"name":"Ada","age":36}`)
	// A quoted brace in the trailing prose is NOT a second candidate.
	mustParse(t, personSchema(), `{"name":"Ada","age":36} see "{}".`, `{"name":"Ada","age":36}`)
}

func TestParse_TopLevelArrayDeclines(t *testing.T) {
	// The synthetic top-level is always a class. Mcoerce-d PR 2: an ARRAY into a
	// class is coerce_array_to_singular / pick_best (M3) — BAML may coerce the
	// first matching item into the class (e.g. [{name,age}] succeeds) — so native
	// DECLINES (falls back) rather than claim a scored outcome it cannot model.
	requireUnsupported(t, personSchema(), `[1,2,3]`)
}

func TestParse_AliasedFieldMatchesRenderedNameOnly(t *testing.T) {
	// Field `name` is rendered as alias `full_name`. BAML's jsonish class
	// coercer matches the rendered (alias) key ONLY; the canonical key
	// `name` is an extra key and the rendered field is missing. Native must
	// agree to stay drift-free.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("name", &bamlutils.DynamicProperty{Type: "string", Alias: "full_name"}),
			kv("age", intProp()),
		),
	}
	// Alias present (exact) -> claimed; emitted under the canonical field name.
	mustParse(t, s, `{"full_name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
	// Canonical name present instead of the rendered alias -> the rendered
	// field `full_name` has no EXACT key match -> DECLINE. (BAML may even
	// fuzzy-match "name" as a substring of "full_name" and succeed, so native
	// must not claim a missing-required error.)
	requireUnsupported(t, s, `{"name":"Ada","age":36}`)
}

func TestParse_FencedJSONWithInlineBackticks(t *testing.T) {
	// A ``` sequence inside the fenced JSON string body must NOT be treated
	// as the closing fence (fences are line-anchored), so the strict JSON is
	// claimed natively rather than truncated into a fallback.
	s := &bamlutils.DynamicOutputSchema{Properties: props(kv("msg", strProp()))}
	raw := "```json\n{\"msg\":\"contains ``` inside\"}\n```"
	mustParse(t, s, raw, "{\"msg\":\"contains ``` inside\"}")
}
