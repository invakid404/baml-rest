package debaml

import (
	"context"
	"errors"
	"testing"

	"github.com/invakid404/baml-rest/bamlutils"
)

// M4b/M4c streaming (raw_is_done=false) native-parse coverage. Each claimed case
// here mirrors a LIVE-CAPTURED success prefix from the bamlfuzz parse-recovery
// corpus (adapters/common/codegen/testdata/bamlfuzz/parse_recovery/20..23 for M4b,
// 173..177 for M4c), so the
// asserted `want` is BAML v0.223.0 parse-stream's own output — the same value the
// Docker differential holds native to byte-exact. The declined cases pin the
// over-claim guards: native FALLS BACK (ErrDeBAMLParseUnsupported) so BAML
// parse-stream stays authoritative. These local tests prove value parity without
// BAML/Docker; the integration differential is the final byte-exact gate.

// parseStreamRaw drives the public callback for a streaming request.
func parseStreamRaw(t *testing.T, s *bamlutils.DynamicOutputSchema, raw string) (string, error) {
	t.Helper()
	res, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: raw, OutputSchema: s, Stream: true})
	if err != nil {
		return "", err
	}
	return string(res.JSON), nil
}

// mustStream asserts a CLAIMED stream parse whose output semantically equals want.
func mustStream(t *testing.T, s *bamlutils.DynamicOutputSchema, raw, want string) {
	t.Helper()
	got, err := parseStreamRaw(t, s, raw)
	if err != nil {
		t.Fatalf("stream Parse(%q): unexpected error: %v", raw, err)
	}
	if !jsonValueEqual(t, got, want) {
		t.Errorf("stream Parse(%q):\n got %s\nwant %s", raw, got, want)
	}
}

// requireStreamUnsupported asserts native FELL BACK (sentinel), not claimed.
func requireStreamUnsupported(t *testing.T, s *bamlutils.DynamicOutputSchema, raw string) {
	t.Helper()
	_, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: raw, OutputSchema: s, Stream: true})
	if err == nil {
		t.Fatalf("stream Parse(%q): expected ErrDeBAMLParseUnsupported, got success", raw)
	}
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("stream Parse(%q): expected ErrDeBAMLParseUnsupported, got %v", raw, err)
	}
}

// nameOnlySchema is Root{name:string} — the truncated_string corpus schema.
func nameOnlySchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{Properties: props(kv("name", strProp()))}
}

// nameTagsSchema is Root{name:string, tags:list<string>} — the trailing_comma
// corpus schema.
func nameTagsSchema() *bamlutils.DynamicOutputSchema {
	return &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("name", strProp()),
			kv("tags", &bamlutils.DynamicProperty{Type: "list", Items: &bamlutils.DynamicTypeSpec{Type: "string"}}),
		),
	}
}

// --- 20_streaming_growing_object (Root{name:string, age:int}) ---

func TestStream_GrowingObject(t *testing.T) {
	s := personSchema()
	// No field value available yet → FALLBACK (open root object before ≥1 field).
	requireStreamUnsupported(t, s, `{`)
	requireStreamUnsupported(t, s, `{"name"`)
	requireStreamUnsupported(t, s, `{"name":`)
	// ≥1 field value available → CLAIMED, missing fields null-filled.
	mustStream(t, s, `{"name":"Ad`, `{"name":"Ad","age":null}`)
	mustStream(t, s, `{"name":"Ada"`, `{"name":"Ada","age":null}`)
	mustStream(t, s, `{"name":"Ada","age"`, `{"name":"Ada","age":null}`)
	mustStream(t, s, `{"name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
}

// --- 21_streaming_markdown_fence (Root{name:string, age:int}) ---

func TestStream_MarkdownFence(t *testing.T) {
	s := personSchema()
	// Bare prose / just-opened fence: no JSON content → FALLBACK.
	requireStreamUnsupported(t, s, "Here you go:\n")
	requireStreamUnsupported(t, s, "Here you go:\n```json\n")
	// Object emerging inside the (still-open) fence → CLAIMED.
	mustStream(t, s, "Here you go:\n```json\n{\"name\":\"Ada\"", `{"name":"Ada","age":null}`)
	mustStream(t, s, "Here you go:\n```json\n{\"name\":\"Ada\",\"age\":36}", `{"name":"Ada","age":36}`)
	mustStream(t, s, "Here you go:\n```json\n{\"name\":\"Ada\",\"age\":36}\n```", `{"name":"Ada","age":36}`)
}

// --- 22_streaming_truncated_string (Root{name:string}) ---

func TestStream_TruncatedString(t *testing.T) {
	s := nameOnlySchema()
	// An opened-but-empty string parses as "" (present, incomplete, kept — strings
	// are not done-required).
	mustStream(t, s, `{"name":"`, `{"name":""}`)
	mustStream(t, s, `{"name":"Ad`, `{"name":"Ad"}`)
	mustStream(t, s, `{"name":"Ada"`, `{"name":"Ada"}`)
	mustStream(t, s, `{"name":"Ada"}`, `{"name":"Ada"}`)
}

// --- 23_streaming_trailing_comma (Root{name:string, tags:list<string>}) ---

func TestStream_TrailingComma(t *testing.T) {
	s := nameTagsSchema()
	mustStream(t, s, `{"name":"Ada","tags":["x"`, `{"name":"Ada","tags":["x"]}`)
	mustStream(t, s, `{"name":"Ada","tags":["x","y",`, `{"name":"Ada","tags":["x","y"]}`)
	mustStream(t, s, `{"name":"Ada","tags":["x","y",]`, `{"name":"Ada","tags":["x","y"]}`)
	mustStream(t, s, `{"name":"Ada","tags":["x","y",]}`, `{"name":"Ada","tags":["x","y"]}`)
}

// --- Over-claim guards ---

// TestStream_IncompleteDoneRequiredScalarNulls pins the M4c semantic-streaming
// behavior for an INCOMPLETE done-required class field: an int that never
// terminates is DELETED by BAML (must_be_done && state != Complete →
// Err(IncompleteDoneValue)) and, as a CLASS field, REPLACED WITH NULL (deletion_nulls
// in semantic_streaming.rs process_node). Native now CLAIMS the null-replacement
// rather than falling back. (Captured live: see corpus 174_streaming_class_incomplete_scalar_field.)
func TestStream_IncompleteDoneRequiredScalarNulls(t *testing.T) {
	s := personSchema()
	// age=3 runs to EOF inside an unterminated object → Incomplete int → deleted →
	// the class field is null-replaced; the completed string sibling is kept.
	mustStream(t, s, `{"name":"Ada","age":3`, `{"name":"Ada","age":null}`)
	// A trailing comma after an UNQUOTED object value at EOF (`36,`) hits the
	// greedy-unquoted-object-value guard streamFix uses (BAML would consume the comma
	// and read greedily), so the streaming fixer DECLINES the candidate before
	// coercion — native over-declines it, safe, and not a corpus prefix.
	requireStreamUnsupported(t, s, `{"name":"Ada","age":36,`)
}

// TestStream_NumbersListDropsIncompleteTail pins the NUMBERS behavior: a partial
// integer-list prefix KEEPS completed elements and DROPS the incomplete
// done-required trailing element (semantic_streaming.rs List arm filter_map). int is
// done-required, list is not. (Captured live: see corpus 173_streaming_numbers_list_int.)
func TestStream_NumbersListDropsIncompleteTail(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("nums", listProp(&bamlutils.DynamicTypeSpec{Type: "int"}))),
	}
	// `[1,2` → 1 is comma-terminated (Complete, kept); 2 runs to EOF (Incomplete,
	// dropped). `[1` → the lone 1 is the incomplete trailing element → dropped → [].
	mustStream(t, s, `{"nums":[`, `{"nums":[]}`)
	mustStream(t, s, `{"nums":[1`, `{"nums":[]}`)
	mustStream(t, s, `{"nums":[1,`, `{"nums":[1]}`)
	mustStream(t, s, `{"nums":[1,2`, `{"nums":[1]}`)
	mustStream(t, s, `{"nums":[1,2,3`, `{"nums":[1,2]}`)
	mustStream(t, s, `{"nums":[1,2,3]`, `{"nums":[1,2,3]}`)
	mustStream(t, s, `{"nums":[1,2,3]}`, `{"nums":[1,2,3]}`)
}

// TestStream_ListOfClassPartialFieldNulls pins that a list of NON-done-required
// class elements KEEPS a partial trailing element (a class is not done-required)
// while NULL-REPLACING that element's incomplete done-required field. Both list
// elements survive; only the incomplete int becomes null. (Captured live: see corpus
// 175_streaming_list_class_partial_field.)
func TestStream_ListOfClassPartialFieldNulls(t *testing.T) {
	inner := bamlutils.MustOrderedMap(kv("a", intProp()))
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("items", listProp(&bamlutils.DynamicTypeSpec{Ref: "Inner"}))),
		Classes:    bamlutils.MustOrderedMap(bamlutils.OrderedKV("Inner", &bamlutils.DynamicClass{Properties: inner})),
	}
	// element 0 `{"a":1}` closed → complete → {a:1}; element 1 `{"a":2` incomplete
	// object with incomplete int a → class kept, a null-replaced → {a:null}.
	mustStream(t, s, `{"items":[{"a":1},{"a":2`, `{"items":[{"a":1},{"a":null}]}`)
}

// TestStream_MissingFieldDefaultFillers pins that a MISSING class field is filled
// with BAML's TypeIR::default_value (list→[], map→{}, optional→null), NOT a blanket
// null: BAML's streaming filler reuses the coercion-layer default fill. A missing
// required scalar (int) still fills null (no default_value). (Captured live: see
// corpus 177_streaming_missing_field_fillers.)
func TestStream_MissingFieldDefaultFillers(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(
			kv("a", intProp()),
			kv("tags", listProp(&bamlutils.DynamicTypeSpec{Type: "string"})),
			kv("scores", mapProp(&bamlutils.DynamicTypeSpec{Type: "int"})),
			kv("note", optProp(&bamlutils.DynamicTypeSpec{Type: "string"})),
		),
	}
	// `{"a":1}` is strict-complete: a=1 kept; tags/scores/note missing → default
	// fillers (list→[], map→{}, optional→null).
	mustStream(t, s, `{"a":1}`, `{"a":1,"tags":[],"scores":{},"note":null}`)
	// A missing required scalar has no default_value → null (b never streamed).
	s2 := &bamlutils.DynamicOutputSchema{Properties: props(kv("a", intProp()), kv("b", intProp()))}
	mustStream(t, s2, `{"a":1}`, `{"a":1,"b":null}`)
}

// TestStream_MapIntDropsIncompleteValue pins the map arm: an entry whose
// done-required value is incomplete drops the WHOLE key/value pair (never
// null-kept), keeping completed sibling entries in input key order. (Captured live:
// see corpus 176_streaming_map_int_partial.)
func TestStream_MapIntDropsIncompleteValue(t *testing.T) {
	s := &bamlutils.DynamicOutputSchema{
		Properties: props(kv("m", mapProp(&bamlutils.DynamicTypeSpec{Type: "int"}))),
	}
	// a=1 comma-terminated (kept); b=2 incomplete (dropped entry). The space after
	// the comma keeps the unquoted object value out of streamFix's greedy-comma
	// decline (an unquoted object value before a TIGHT comma declines).
	mustStream(t, s, `{"m":{"a":1, "b":2`, `{"m":{"a":1}}`)
}

// TestStream_NoStructureDeclines pins that raw text with no recoverable JSON
// structure falls back (BAML's allow_as_string→class path is not claimed by M4b).
func TestStream_NoStructureDeclines(t *testing.T) {
	requireStreamUnsupported(t, personSchema(), "just some prose, no json")
	requireStreamUnsupported(t, personSchema(), "")
}

// TestStream_ScalarToClassDeclines pins that a scalar/array into a class declines
// in stream mode (implied-key / inferred-object / array-to-singular deferred).
func TestStream_ScalarToClassDeclines(t *testing.T) {
	requireStreamUnsupported(t, personSchema(), `"hello"`)
	requireStreamUnsupported(t, personSchema(), `[1,2,3]`)
}

// TestStream_NilSchemaDeclines pins the nil-schema fallback.
func TestStream_NilSchemaDeclines(t *testing.T) {
	_, err := Parse(context.Background(), bamlutils.DeBAMLParseRequest{Raw: `{}`, OutputSchema: nil, Stream: true})
	if !errors.Is(err, bamlutils.ErrDeBAMLParseUnsupported) {
		t.Fatalf("nil schema stream: expected ErrDeBAMLParseUnsupported, got %v", err)
	}
}

// TestStream_FinalParseUnchanged is a belt-and-suspenders check that the M4b
// stream path did not alter final (non-stream) parse behavior for the same input.
func TestStream_FinalParseUnchanged(t *testing.T) {
	// Final parse of a truncated object still DECLINES (M2a defers unterminated
	// structures); only the STREAM path recovers it.
	requireUnsupported(t, personSchema(), `{"name":"Ada"`)
	// Final parse of a complete object is unchanged.
	mustParse(t, personSchema(), `{"name":"Ada","age":36}`, `{"name":"Ada","age":36}`)
}
