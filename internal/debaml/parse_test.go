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

func TestParse_ConservativeTypeMatchDeclines(t *testing.T) {
	// Native primitive matching is strict, but BAML's primitive coercers are
	// lenient (parse numeric strings, round float->int). A strict native
	// failure is exactly where BAML would still succeed, so these DECLINE
	// (fall back to BAML) rather than claim an error BAML would not produce.
	//
	// String where an int is required: BAML parses "36"->36.
	requireUnsupported(t, personSchema(), `{"name":"Ada","age":"36"}`)
	// Float where an int is required (strict JSON): BAML rounds 36.5->37.
	// (Latent M1 case — was a claimed error, must be a fallback.)
	requireUnsupported(t, personSchema(), `{"name":"Ada","age":36.5}`)
	// Same via the fixing parser (single-quoted name forces the fix path),
	// the new M2a exposure: native now CLAIMS the fix, so the non-integer
	// coercion must DECLINE, not propagate a claimed error.
	requireUnsupported(t, personSchema(), `{name:'Ada', age:36.5}`)
	requireUnsupported(t, personSchema(), `{name:'Ada', age:'36'}`)
	// Number where a string is required: BAML stringifies 36->"36".
	s := &bamlutils.DynamicOutputSchema{Properties: props(kv("v", strProp()))}
	requireUnsupported(t, s, `{"v":36}`)
}

func TestParse_List(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("tags", &bamlutils.DynamicProperty{
			Type:  "list",
			Items: &bamlutils.DynamicTypeSpec{Type: "string"},
		})),
	}
	mustParse(t, s, `{"tags":["x","y","z"]}`, `{"tags":["x","y","z"]}`)
	// Wrong element type DECLINES: BAML stringifies 2->"2" in a string list,
	// so native (strict) declines rather than claiming a mismatch.
	requireUnsupported(t, s, `{"tags":["x",2]}`)
	// A non-array where a list is required DECLINES: BAML wraps a singleton
	// into a one-element array, so native declines rather than claiming.
	requireUnsupported(t, s, `{"tags":"x"}`)
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
	// A non-exact literal value DECLINES: BAML's literal coercion is fuzzy
	// (case/punctuation/substring for strings, rounds/parses for ints), so
	// native (exact) declines rather than claiming a mismatch BAML might
	// still match.
	requireUnsupported(t, s, `{"status":"inactive","version":2,"ok":true}`)
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

func TestParse_FixingTrailingCommas(t *testing.T) {
	// Trailing comma in an object.
	mustParse(t, personSchema(), `{"name":"Ada","age":36,}`, `{"name":"Ada","age":36}`)
	// Trailing commas in a nested object and array, plus a top-level
	// trailing comma — all tolerated.
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("name", strProp()),
			kv("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}}),
		),
	}
	mustParse(t, s, `{"name":"Ada","tags":["x","y",],}`, `{"name":"Ada","tags":["x","y"]}`)
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

func TestParse_TopLevelArrayIsClaimedError(t *testing.T) {
	// The synthetic top-level is always an object; a top-level array fails
	// to coerce — a claimed error, not a fallback.
	requireClaimedError(t, personSchema(), `[1,2,3]`)
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
	// Alias present -> claimed; emitted under the canonical field name.
	mustParse(t, s, `{"full_name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
	// Canonical name present instead of the alias -> required rendered field
	// missing -> claimed coercion error (BAML errors here too, NOT a false
	// success).
	requireClaimedError(t, s, `{"name":"Ada","age":36}`)
}

func TestParse_FencedJSONWithInlineBackticks(t *testing.T) {
	// A ``` sequence inside the fenced JSON string body must NOT be treated
	// as the closing fence (fences are line-anchored), so the strict JSON is
	// claimed natively rather than truncated into a fallback.
	s := &bamlutils.DynamicOutputSchema{Properties: props(kv("msg", strProp()))}
	raw := "```json\n{\"msg\":\"contains ``` inside\"}\n```"
	mustParse(t, s, raw, "{\"msg\":\"contains ``` inside\"}")
}
