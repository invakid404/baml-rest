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

func TestParse_ConservativeTypeMatch(t *testing.T) {
	// A JSON string where an int is required is a claimed coercion error,
	// not a silent string->int fix.
	requireClaimedError(t, personSchema(), `{"name":"Ada","age":"36"}`)
	// A float where an int is required is rejected too.
	requireClaimedError(t, personSchema(), `{"name":"Ada","age":36.5}`)
}

func TestParse_List(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("tags", &bamlutils.DynamicProperty{
			Type:  "list",
			Items: &bamlutils.DynamicTypeSpec{Type: "string"},
		})),
	}
	mustParse(t, s, `{"tags":["x","y","z"]}`, `{"tags":["x","y","z"]}`)
	// Wrong element type is a claimed error.
	requireClaimedError(t, s, `{"tags":["x",2]}`)
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

func TestParse_RequiredFieldMissing(t *testing.T) {
	requireClaimedError(t, personSchema(), `{"name":"Ada"}`)
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
	// Wrong literal value is a claimed error.
	requireClaimedError(t, s, `{"status":"inactive","version":2,"ok":true}`)
	requireClaimedError(t, s, `{"status":"active","version":3,"ok":true}`)
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
	// Unknown enum value is a claimed error.
	requireClaimedError(t, enumSchema(), `{"color":"MAUVE"}`)
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

func TestParse_UnsupportedMap(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("m", &bamlutils.DynamicProperty{
			Type:   "map",
			Keys:   &bamlutils.DynamicTypeSpec{Type: "string"},
			Values: &bamlutils.DynamicTypeSpec{Type: "int"},
		})),
	}
	requireUnsupported(t, s, `{"m":{"a":1}}`)
}

func TestParse_UnsupportedGeneralUnion(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("u", &bamlutils.DynamicProperty{
			Type: "union",
			OneOf: []*bamlutils.DynamicTypeSpec{
				{Type: "string"},
				{Type: "int"},
			},
		})),
	}
	requireUnsupported(t, s, `{"u":"x"}`)
}

func TestParse_UnsupportedFixingSyntax(t *testing.T) {
	// Trailing commas, unquoted keys, single quotes: a candidate exists but
	// is not strict JSON, so the parser falls back to BAML's fixing parser.
	requireUnsupported(t, personSchema(), `{"name":"Ada","age":36,}`)
	requireUnsupported(t, personSchema(), `{name:"Ada",age:36}`)
	requireUnsupported(t, personSchema(), `{'name':'Ada','age':36}`)
	// Fenced non-strict content also falls back.
	requireUnsupported(t, personSchema(), "```json\n{name:'Ada',age:36}\n```")
}

func TestParse_ClaimedErrorNoCandidate(t *testing.T) {
	// Truncated mid-value: no complete JSON value, so the parser CLAIMS a
	// parse error (BAML errors here too) rather than falling back.
	requireClaimedError(t, personSchema(), `{"name":"Ada","age":`)
	// Pure prose with no JSON at all.
	requireClaimedError(t, personSchema(), `I could not produce a record.`)
}

func TestParse_TopLevelArrayIsClaimedError(t *testing.T) {
	// The synthetic top-level is always an object; a top-level array fails
	// to coerce — a claimed error, not a fallback.
	requireClaimedError(t, personSchema(), `[1,2,3]`)
}
